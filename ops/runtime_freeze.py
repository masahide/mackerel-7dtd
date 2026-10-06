"""Prepare fixed recovery assets without stopping or starting the live game.

The CLI has only a preparation ID. It does not accept host, path, shell, version,
or image parameters. No backup verified receipt or production enablement occurs.
"""
from __future__ import annotations
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import time

from suzume_update import ArchiveStore, Blocked, FrozenTarget, JOB, atomic_json, digest

ROOT = Path("/home/masahide/work/7dtd")
CONTAINER = "f043019c7471ff9d7c66eeb67676e4b94e856b25664ce5e77302ce247f6eafd3"
IMAGE = "sha256:13ee1d0fd0047f5b53e274339ce500420cf82b554ff5f0190f60723c042da371"
COMPOSE = "e2721c806c7a39f97eeeb0f3818e0e8fbcdbc7c5b094ade9e1742991ef6e0568"
BUILD = "25661908"
FIXED_ENV = {"START_MODE": "1", "UPDATE_MODS": "NO", "CPM_UPDATE": "NO",
             "ALLOC_FIXES_UPDATE": "NO", "MONITOR": "NO", "BACKUP": "NO",
             "TEST_ALERT": "NO", "CHANGE_CONFIG_DIR_OWNERSHIP": "NO"}

# The same code is used for the live and frozen runtime. Only static launcher,
# module and script bytes are read; no game console or network is contacted.
CHECKPOINT = r'''
import hashlib,json,pathlib
base=pathlib.Path('/home/sdtdserver')
files=[base/x for x in ['sdtdserver','linuxgsm.sh','user.sh','install.sh','openvpn.sh']]
for root in [base/'scripts',base/'lgsm/modules']:
 if not root.is_dir():raise RuntimeError('static runtime scope absent')
 files+=sorted(p for p in root.rglob('*') if p.is_file())
extra=pathlib.Path('/etc/openvpn/update-resolv-conf.sh')
files.append(extra)
out={str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in files}
print(json.dumps(out))
'''


class DockerRunner:
    def run(self, args, timeout=1800):
        try:
            p = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
            if p.returncode != 0:
                raise Blocked("PREPARATION_COMMAND_FAILED")
            return p.stdout
        except (OSError, subprocess.TimeoutExpired):
            raise Blocked("PREPARATION_COMMAND_FAILED") from None


def mod_inventory(root):
    mods = root / "ServerFiles/Mods"
    if not mods.is_dir() or mods.is_symlink():
        raise Blocked("MOD_SCOPE_UNAVAILABLE")
    result = {}
    for p in sorted(mods.rglob("*")):
        if p.is_symlink():
            raise Blocked("MOD_LINK_UNSUPPORTED")
        if p.is_file():
            result[p.relative_to(mods).as_posix()] = digest(p)
    if not result:
        raise Blocked("MOD_SCOPE_UNAVAILABLE")
    return result


class RuntimePreparer:
    def __init__(self, runner, root=ROOT, emit=lambda _: None):
        # A test runner may use a generated fixture root. The real runner is
        # restricted to the observed host/root and administrator execution.
        if isinstance(runner, DockerRunner):
            if (sys.platform != "linux" or os.geteuid() != 0 or socket.gethostname() != "7dtd01"
                    or root != ROOT or root.is_symlink() or root.resolve() != ROOT):
                raise Blocked("PRODUCTION_CONTEXT_MISMATCH")
        self.runner, self.root, self.emit = runner, root, emit

    def inspect_live(self):
        cf = self.root / "docker-compose.yml"
        if cf.is_symlink() or digest(cf) != COMPOSE:
            raise Blocked("COMPOSE_CHANGED")
        cid = self.runner.run(["docker", "compose", "-f", str(cf), "ps", "-q", "7dtdserver"], 30).strip()
        if cid != CONTAINER:
            raise Blocked("CONTAINER_CHANGED")
        values = json.loads(self.runner.run(["docker", "inspect", cid], 30))
        if len(values) != 1:
            raise Blocked("CONTAINER_CHANGED")
        ins = values[0]
        if (ins["Image"] != IMAGE or ins["State"]["Status"] != "running"
                or ins["Config"]["Entrypoint"] != ["/home/sdtdserver/openvpn.sh"]):
            raise Blocked("RUNTIME_CHANGED")
        python = self.runner.run(["docker", "exec", cid, "/bin/sh", "-c", "command -v python3"], 30).strip()
        if python != "/usr/bin/python3":
            raise Blocked("CONTAINER_PYTHON_PATH_UNCONFIRMED")
        expected = {"ServerFiles": "/home/sdtdserver/serverfiles", "7DaysToDie": "/home/sdtdserver/.local/share/7DaysToDie",
                    "LGSM-Config": "/home/sdtdserver/lgsm/config-lgsm/sdtdserver", "log": "/home/sdtdserver/log",
                    "backups": "/home/sdtdserver/lgsm/backup"}
        for name, destination in expected.items():
            matches = [m for m in ins["Mounts"] if m["Destination"] == destination]
            if (len(matches) != 1 or matches[0]["Type"] != "bind"
                    or matches[0]["Source"] != str(self.root / name)):
                raise Blocked("MOUNT_CHANGED")
        manifest = self.root / "ServerFiles/steamapps/appmanifest_294420.acf"
        if re.findall(r'"buildid"\s*"([0-9]+)"', manifest.read_text()) != [BUILD]:
            raise Blocked("BUILD_CHANGED")
        return ins

    def checkpoint(self, frozen_image=None):
        if frozen_image is None:
            args = ["docker", "exec", CONTAINER, "/usr/bin/python3", "-c", CHECKPOINT]
        else:
            # No ports, VPN bind, host namespace, extra capability or game start.
            args = ["docker", "run", "--rm", "--network", "none", "--cap-drop", "ALL", "--read-only",
                    "--entrypoint", "/usr/bin/python3", frozen_image, "-c", CHECKPOINT]
        result = json.loads(self.runner.run(args, 120))
        if not result or any(not re.fullmatch(r"[0-9a-f]{64}", v) for v in result.values()):
            raise Blocked("STATIC_RUNTIME_UNAVAILABLE")
        return result

    @contextmanager
    def release_lock(self):
        import fcntl
        path = self.root / "ServerFiles/Mods/TrailwatchBridge/TrailwatchBridge.dll.release.lock"
        if path.is_symlink() or not path.is_file():
            raise Blocked("RELEASE_LOCK_UNAVAILABLE")
        fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            yield
        except BlockingIOError:
            raise Blocked("MOD_RELEASE_BUSY") from None
        finally:
            os.close(fd)

    def prepare(self, preparation_id):
        if not isinstance(preparation_id, str) or not JOB.fullmatch(preparation_id):
            raise Blocked("INVALID_PREPARATION_ID")
        base = self.root / "upgrade-backups"
        if base.is_symlink():
            raise Blocked("UNSAFE_PREPARATION_DIRECTORY")
        folder = base / ("adapter-preparation-" + preparation_id)
        try:
            folder.mkdir(mode=0o700)  # Never retry a possibly interrupted operation.
        except FileExistsError:
            raise Blocked("PREPARATION_EXISTS_INSPECT_STATE_FIRST") from None
        state = {"preparationId": preparation_id, "phase": "checking", "createdAt": time.time(),
                 "verified": False, "runtimeRestored": False, "productionEnabled": False}
        def phase(value):
            state["phase"] = value
            state["updatedAt"] = time.time()
            atomic_json(folder / "state.json", state)
            self.emit({"preparationId": preparation_id, "phase": value})
        try:
            phase("checking")
            with self.release_lock():
                before = self.inspect_live()
                static = self.checkpoint()
                mods = mod_inventory(self.root)
                # A full private inspect is recovery input; never expose Env.
                atomic_json(folder / "container.private.json", before)
                phase("freezing_runtime")
                args = ["docker", "commit", "--pause=false", "--change", "LABEL org.suzume.preparation=" + preparation_id]
                for key, value in FIXED_ENV.items():
                    args += ["--change", "ENV " + key + "=" + value]
                args.append(CONTAINER)
                frozen = self.runner.run(args, 600).strip()
                if not re.fullmatch(r"sha256:[0-9a-f]{64}", frozen):
                    raise Blocked("FROZEN_IMAGE_ID_UNAVAILABLE")
                state["frozenImageId"] = frozen
                phase("checking_frozen_runtime")
                if self.checkpoint(frozen) != static or self.checkpoint() != static:
                    raise Blocked("STATIC_RUNTIME_CHANGED")
                image_info = json.loads(self.runner.run(["docker", "image", "inspect", frozen], 30))[0]
                image_env = dict(x.split("=", 1) for x in image_info["Config"]["Env"] if "=" in x)
                if any(image_env.get(k) != v for k, v in FIXED_ENV.items()):
                    raise Blocked("FROZEN_START_FLAGS_MISMATCH")
                phase("saving_runtime_image")
                image = folder / "runtime-image.tar"
                self.runner.run(["docker", "image", "save", "--output", str(image), frozen], 1200)
                os.chmod(image, 0o600)
                ArchiveStore.validate_image(image, frozen)
                state.update(imageSha256=digest(image), imageBytes=image.stat().st_size)
                phase("freezing_serverfiles")
                payload = folder / "target-serverfiles.tar"
                self.runner.run(["tar", "--acls", "--xattrs", "--numeric-owner", "-cpf", str(payload), "-C", str(self.root), "ServerFiles"], 1800)
                os.chmod(payload, 0o600)
                self.runner.run(["tar", "--acls", "--xattrs", "--compare", "--file", str(payload), "-C", str(self.root)], 1800)
                target_sha = digest(payload)
                FrozenTarget(payload, target_sha, BUILD, mods).verify()
                if mod_inventory(self.root) != mods or self.checkpoint() != static:
                    raise Blocked("PREPARATION_SOURCE_CHANGED")
                self.inspect_live()
                state.update(payloadSha256=target_sha, payloadBytes=payload.stat().st_size,
                             steamBuild=BUILD, approvedModHashes=mods, staticRuntimeHashes=static,
                             fixedStartupEnvironment=FIXED_ENV,
                             targetVersion="Game version: V 3.3.0 (b18) Compatibility Version: V 3.3.0")
                phase("prepared")
                return {key: state[key] for key in ["preparationId", "phase", "frozenImageId", "imageSha256", "imageBytes",
                                                   "payloadSha256", "payloadBytes", "steamBuild", "verified", "runtimeRestored", "productionEnabled"]}
        except Exception as error:
            state["errorCode"] = str(error) if isinstance(error, Blocked) else "PREPARATION_FAILED"
            phase("failed")
            raise Blocked(state["errorCode"]) from None


if __name__ == "__main__":
    try:
        if len(sys.argv) != 3 or sys.argv[1] != "prepare":
            raise Blocked("USE_PREPARE_WITH_ID")
        output = RuntimePreparer(DockerRunner(), emit=lambda data: print(json.dumps(data), flush=True)).prepare(sys.argv[2])
        print(json.dumps(output), flush=True)
    except Blocked as error:
        print(json.dumps({"errorCode": str(error)}), flush=True)
        sys.exit(1)
