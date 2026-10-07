"""Bounded offline boot of prepared assets, with copies and no published ports.

This is an administrator CLI, not an update hook. The live game is never stopped,
started, recreated, or mounted into the trial. Online Save copies do not establish
a verified quiescent world backup, even when the isolated game starts correctly.
"""
import json
import os
import re
import shutil
import sys
import time

from runtime_freeze import BUILD, CONTAINER, DockerRunner, ROOT, RuntimePreparer, mod_inventory
from suzume_update import ArchiveStore, Blocked, FrozenTarget, JOB, archive_members, atomic_json, digest

TARGET = "Game version: V 3.3.0 (b18) Compatibility Version: V 3.3.0"
MEMORY_PROBE = "import json,pathlib; m=dict(line.split(':',1) for line in pathlib.Path('/proc/meminfo').read_text().splitlines()); print(json.dumps({'memoryAvailable':int(m['MemAvailable'].split()[0])*1024}))"
PROBE = r'''
import json,re,socket,time,xml.etree.ElementTree as ET
properties={p.attrib.get('name'):p.attrib.get('value') for p in ET.parse('/home/sdtdserver/serverfiles/sdtdserver.xml').getroot().iter('property')}
if properties.get('TelnetEnabled')!='true' or properties.get('TelnetPort')!='8081':raise SystemExit(1)
password=properties.get('TelnetPassword') or ''
if '\r' in password or '\n' in password:raise SystemExit(1)
try:
 with socket.create_connection(('127.0.0.1',8081),timeout=3) as sock:
  sock.settimeout(2);data=b'';end=time.monotonic()+5
  while time.monotonic()<end and b'password:' not in data.lower() and b'session.' not in data.lower():
   try:data+=sock.recv(8192)
   except socket.timeout:pass
   if len(data)>65536:raise SystemExit(1)
  if password:
   if b'password:' not in data.lower():raise SystemExit(1)
   sock.sendall(password.encode()+b'\r\n');data=b'';end=time.monotonic()+5
   while time.monotonic()<end and b'session.' not in data.lower():
    try:data+=sock.recv(8192)
    except socket.timeout:pass
    if len(data)>65536:raise SystemExit(1)
  if b'session.' not in data.lower():raise SystemExit(1)
  sock.sendall(b'version\r\n');data=b'';end=time.monotonic()+5
  while time.monotonic()<end:
   try:data+=sock.recv(8192)
   except socket.timeout:pass
   if len(data)>65536:raise SystemExit(1)
   lines=re.findall(r'Game version:[^\r\n]*[\r\n]',data.decode('utf-8','replace'))
   if len(lines)==1:
    print(json.dumps({'gameVersion':' '.join(lines[0].split())}));raise SystemExit(0)
except OSError:pass
raise SystemExit(1)
'''


class OfflineBootRehearsal:
    def __init__(self, preparer, emit=lambda _: None):
        self.preparer, self.runner, self.emit = preparer, preparer.runner, emit

    def require_resource_capacity(self):
        info = json.loads(self.runner.run(["docker", "info", "--format", "{{json .}}"], 30))
        memory = json.loads(self.runner.run(["docker", "exec", CONTAINER, "/usr/bin/python3", "-c", MEMORY_PROBE], 30))
        if (type(info.get("MemTotal")) is not int or info["MemTotal"] < 14 * 1024**3
                or type(info.get("NCPU")) is not int or info["NCPU"] < 4
                or type(memory.get("memoryAvailable")) is not int or memory["memoryAvailable"] < 8 * 1024**3):
            raise Blocked("TRIAL_RESOURCE_CAPACITY_UNAVAILABLE")

    def inspect_trial(self, name, preparation_id, image):
        values = json.loads(self.runner.run(["docker", "inspect", name], 30))
        if len(values) != 1:
            raise Blocked("TRIAL_IDENTITY_UNAVAILABLE")
        ins = values[0]
        cid = ins.get("Id", "")
        host = ins.get("HostConfig") or {}
        if (not re.fullmatch(r"[0-9a-f]{64}", cid) or cid == CONTAINER or ins.get("Image") != image
                or (ins.get("Config", {}).get("Labels") or {}).get("org.suzume.rehearsal") != preparation_id
                or host.get("NetworkMode") != "none" or host.get("PortBindings")
                or host.get("Privileged") or host.get("RestartPolicy", {}).get("Name") != "no"):
            raise Blocked("TRIAL_IDENTITY_MISMATCH")
        return cid, ins

    def run(self, preparation_id, mode="online"):
        if not isinstance(preparation_id, str) or not JOB.fullmatch(preparation_id):
            raise Blocked("INVALID_PREPARATION_ID")
        if mode not in ["online", "check-start"]:
            raise Blocked("INVALID_TRIAL_MODE")
        p = self.preparer
        folder = p.root / "upgrade-backups" / ("adapter-preparation-" + preparation_id)
        if folder.is_symlink() or not folder.is_dir():
            raise Blocked("PREPARATION_UNAVAILABLE")
        state = json.loads((folder / "state.json").read_text())
        copied = json.loads((folder / "copy-rehearsal.json").read_text())
        if (state.get("phase") != "prepared" or state.get("steamBuild") != BUILD
                or copied.get("phase") != "target_copy_restored" or copied.get("filesystemCopyRestored") is not True
                or state.get("verified") is not False or copied.get("verified") is not False):
            raise Blocked("TRIAL_ASSETS_NOT_PREPARED")
        trial_root = folder / ("offline-runtime-trial" if mode == "online" else "fixed-start-readiness-trial")
        try:
            trial_root.mkdir(mode=0o700)  # Never retry/reuse an uncertain trial.
        except FileExistsError:
            raise Blocked("TRIAL_EXISTS_INSPECT_STATE_FIRST") from None
        receipt = {"preparationId": preparation_id, "phase": "checking", "createdAt": time.time(),
                   "offlineBootVerified": False, "fullWorldBackup": False, "verified": False,
                   "runtimeRestored": False, "productionEnabled": False,
                   "startAttempted": False, "cleanupRequired": False,
                   "purpose": "world_copy_trial" if mode == "online" else "fixed_start_only",
                   "sourceQuiescent": False}
        def phase(value):
            receipt.update(phase=value, updatedAt=time.time())
            atomic_json(trial_root / "receipt.json", receipt)
            self.emit({"preparationId": preparation_id, "phase": value})
        name = ("suzume-offline-check-" if mode == "online" else "suzume-start-check-") + preparation_id
        cid, start_attempted = None, False
        with p.release_lock():
            try:
                phase("checking")
                p.inspect_live()
                if p.checkpoint() != state["staticRuntimeHashes"] or mod_inventory(p.root) != state["approvedModHashes"]:
                    raise Blocked("PREPARATION_SOURCE_CHANGED")
                # Capped at 6GiB/2 CPUs, with 2GiB of available memory reserved.
                self.require_resource_capacity()
                if shutil.disk_usage(folder).free < 40 * 1024**3:
                    raise Blocked("TRIAL_SPACE_UNAVAILABLE")
                if p.resolve_frozen_image(preparation_id) != state["frozenImageId"]:
                    raise Blocked("PREPARATION_IMAGE_CHANGED")
                image = folder / "runtime-image.tar"
                if digest(image) != state["imageSha256"]:
                    raise Blocked("PREPARATION_ARCHIVE_CHANGED")
                ArchiveStore.validate_image(image, state["frozenImageId"])
                # ServerFiles already restored and compared. The online Save and
                # config copy is NOT labelled a quiescent or complete backup.
                server = folder / "restore-target-copy/ServerFiles"
                payload = folder / "target-serverfiles.tar"
                FrozenTarget(payload, state["payloadSha256"], BUILD, state["approvedModHashes"]).verify()
                self.runner.run(["tar", "--acls", "--xattrs", "--compare", "--file", str(payload), "-C", str(server.parent)], 300)
                if (server.is_symlink() or mod_inventory(server.parent) != state["approvedModHashes"]
                        or digest(server / "7DaysToDieServer.x86_64") != digest(p.root / "ServerFiles/7DaysToDieServer.x86_64")):
                    raise Blocked("TRIAL_TARGET_COPY_CHANGED")
                if mode == "online":
                    phase("copying_online_world_for_trial")
                    archive = trial_root / "online-world-config.tar"
                    self.runner.run(["tar", "--acls", "--xattrs", "--numeric-owner", "-cpf", str(archive),
                                     "-C", str(p.root), "7DaysToDie", "LGSM-Config"], 300)
                    archive.chmod(0o600)
                    phase("comparing_online_source")
                    try:
                        self.runner.run(["tar", "--acls", "--xattrs", "--compare", "--file", str(archive), "-C", str(p.root)], 300)
                    except Blocked:
                        raise Blocked("ONLINE_COPY_NOT_MATCHED") from None
                    receipt["onlineSourceCompared"] = True
                else:
                    # Explicit runtime-only rehearsal: an unverified online
                    # snapshot is never converted into a recovery receipt.
                    archive = folder / "offline-runtime-trial/online-world-config.tar"
                    if archive.is_symlink() or not archive.is_file():
                        raise Blocked("UNVERIFIED_TRIAL_COPY_UNAVAILABLE")
                    receipt["onlineSourceCompared"] = False
                    phase("reading_unverified_snapshot_for_start_check")
                phase("restoring_online_copy")
                archive_members(archive, allowed=("7DaysToDie", "LGSM-Config"))
                self.runner.run(["tar", "--acls", "--xattrs", "--numeric-owner", "--same-owner", "--same-permissions",
                                 "-xpf", str(archive), "-C", str(trial_root)], 300)
                self.runner.run(["tar", "--acls", "--xattrs", "--compare", "--file", str(archive), "-C", str(trial_root)], 300)
                receipt["onlineCopySha256"] = digest(archive)
                for directory in ["log", "backups"]:
                    (trial_root / directory).mkdir(mode=0o755)
                    # Match live volume ownership without changing live files.
                for directory in ["log", "backups"]:
                    source_stat = (p.root / directory).stat()
                    os.chown(trial_root / directory, source_stat.st_uid, source_stat.st_gid)
                args = ["docker", "run", "--detach", "--name", name, "--label", "org.suzume.rehearsal=" + preparation_id,
                        "--network", "none", "--restart", "no", "--cpus", "2", "--memory", "6g", "--memory-swap", "6g",
                        "--pids-limit", "512", "--cap-drop", "NET_RAW", "--entrypoint",
                        "/home/sdtdserver/user.sh" if mode == "online" else "/bin/sleep"]
                mounts = [(server, "/home/sdtdserver/serverfiles"),
                          (trial_root / "7DaysToDie", "/home/sdtdserver/.local/share/7DaysToDie"),
                          (trial_root / "LGSM-Config", "/home/sdtdserver/lgsm/config-lgsm/sdtdserver"),
                          (trial_root / "log", "/home/sdtdserver/log"), (trial_root / "backups", "/home/sdtdserver/lgsm/backup")]
                for source, target in mounts:
                    args += ["--mount", "type=bind,source=" + str(source) + ",target=" + target]
                args.append(state["frozenImageId"])
                if mode == "check-start":
                    args.append("infinity")
                self.require_resource_capacity()
                receipt.update(startAttempted=True, cleanupRequired=True)
                phase("starting_offline_trial")
                start_attempted = True
                self.runner.run(args, 60)
                cid, _ = self.inspect_trial(name, preparation_id, state["frozenImageId"])
                receipt["trialContainerId"] = cid
                if mode == "check-start":
                    phase("checking_fixed_gsm_start")
                    self.runner.run(["docker", "exec", "--user", "sdtdserver", "--workdir", "/home/sdtdserver",
                                     cid, "./sdtdserver", "start"], 60)
                phase("waiting_for_offline_game")
                deadline = time.monotonic() + 180
                while time.monotonic() < deadline:
                    _, ins = self.inspect_trial(name, preparation_id, state["frozenImageId"])
                    if ins["State"].get("Status") != "running" or ins["State"].get("OOMKilled"):
                        raise Blocked("TRIAL_EXITED_OR_OOM")
                    try:
                        probe = json.loads(self.runner.run(["docker", "exec", cid, "/usr/bin/python3", "-c", PROBE], 20))
                    except Blocked:
                        time.sleep(2)
                        continue
                    if probe.get("gameVersion") != TARGET:
                        raise Blocked("TRIAL_VERSION_MISMATCH")
                    receipt.update(offlineBootVerified=True, gameVersion=TARGET)
                    break
                if not receipt["offlineBootVerified"]:
                    raise Blocked("TRIAL_BOOT_TIMEOUT")
                p.inspect_live()
                if p.checkpoint() != state["staticRuntimeHashes"] or mod_inventory(p.root) != state["approvedModHashes"]:
                    raise Blocked("PREPARATION_SOURCE_CHANGED")
                phase("offline_game_verified")
            except Exception as error:
                receipt["failedPhase"] = receipt["phase"]
                receipt["errorCode"] = str(error) if isinstance(error, Blocked) else "TRIAL_FAILED"
                phase("failed")
            finally:
                # Mutators only act on the newly created, fully identified trial.
                # No rm -rf, live mount changes, or commands to the live game.
                if start_attempted and cid is None:
                    try:
                        cid, _ = self.inspect_trial(name, preparation_id, state["frozenImageId"])
                    except Blocked:
                        receipt["cleanupRequired"] = True
                        atomic_json(trial_root / "receipt.json", receipt)
                if cid:
                    try:
                        self.inspect_trial(name, preparation_id, state["frozenImageId"])
                        try:
                            logs = self.runner.run(["docker", "logs", "--tail", "200", cid], 30)
                            log_path = trial_root / "container-log.private.txt"
                            fd = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                            with os.fdopen(fd, "w", encoding="utf-8") as f:
                                f.write(logs)
                            receipt["privateLogsSaved"] = True
                        except (Blocked, OSError):
                            receipt["privateLogsSaved"] = False
                        self.runner.run(["docker", "stop", "--time", "90", cid], 120)
                        _, stopped = self.inspect_trial(name, preparation_id, state["frozenImageId"])
                        if stopped["State"].get("Status") != "exited":
                            raise Blocked("TRIAL_CLEANUP_NOT_VERIFIED")
                        self.runner.run(["docker", "container", "rm", cid], 30)
                        receipt.update(trialRemoved=True, cleanupRequired=False)
                    except Exception as error:
                        receipt["cleanupErrorCode"] = str(error) if isinstance(error, Blocked) else "TRIAL_CLEANUP_FAILED"
                        receipt.setdefault("errorCode", "TRIAL_CLEANUP_FAILED")
                        receipt["phase"] = "failed"
                    atomic_json(trial_root / "receipt.json", receipt)
        if receipt.get("cleanupErrorCode"):
            raise Blocked("TRIAL_CLEANUP_FAILED")
        if receipt.get("errorCode"):
            raise Blocked(receipt["errorCode"])
        return receipt


if __name__ == "__main__":
    try:
        if len(sys.argv) != 3 or sys.argv[1] not in ["rehearse", "check-start"]:
            raise Blocked("USE_REHEARSE_WITH_PREPARATION_ID")
        output = OfflineBootRehearsal(RuntimePreparer(DockerRunner()), emit=lambda item: print(json.dumps(item), flush=True)).run(sys.argv[2], "online" if sys.argv[1] == "rehearse" else "check-start")
        print(json.dumps(output), flush=True)
    except Blocked as error:
        print(json.dumps({"errorCode": str(error)}), flush=True)
        sys.exit(1)
