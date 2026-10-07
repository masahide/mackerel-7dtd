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
import xml.etree.ElementTree as ET

from runtime_freeze import BUILD, CONTAINER, DockerRunner, ROOT, RuntimePreparer, mod_inventory
from suzume_update import ArchiveStore, Blocked, FrozenTarget, JOB, archive_members, atomic_json, digest

TARGET = "Game version: V 3.3.0 (b18) Compatibility Version: V 3.3.0"
OBSERVED_WORLD_DAY = 848 # Saved-world checkpoint from the attended return, not a new world.
OBSERVED_PLATFORM_LINES = ["platform=Steam", "crossplatform=EOS", "serverplatforms=Steam,XBL,PSN,LAN,"]
OFFLINE_PLATFORM = b"platform=Local\ncrossplatform=None\nserverplatforms=LAN\n"
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
    out={'gameVersion':' '.join(lines[0].split())}
    if globals().get('worldcheck',False):
     for command,pattern,key in [('gt',r'^Day ([0-9]+), ([0-9]{1,2}):([0-9]{2})\r?$', 'gameTime'),('lp',r'^Total of ([0-9]+) in the game\r?$', 'onlinePlayers')]:
      sock.sendall(command.encode()+b'\r\n');reply=b'';limit=time.monotonic()+5;matches=[]
      while time.monotonic()<limit:
       try:chunk=sock.recv(8192)
       except socket.timeout:continue
       if not chunk:raise SystemExit(1)
       reply+=chunk
       if len(reply)>65536:raise SystemExit(1)
       matches=re.findall(pattern,reply.decode('utf-8','replace'),re.M)
       if matches:break
      if len(matches)!=1:raise SystemExit(1)
      if key=='gameTime':
       day,hour,minute=map(int,matches[0])
       if day<1 or hour>23 or minute>59:raise SystemExit(1)
       out[key]={'days':day,'hours':hour,'minutes':minute}
      else:out[key]=int(matches[0])
    print(json.dumps(out));raise SystemExit(0)
except OSError:pass
raise SystemExit(1)
'''
WORLD_PROBE = "worldcheck=True\n" + PROBE


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

    def resolve_trial_image(self, folder, state, preparation_id):
        receipt = json.loads((folder / "isolated-image.json").read_text())
        image = receipt.get("trialImageId", "")
        if (receipt.get("phase") != "isolated_image_prepared" or receipt.get("sourceImageId") != state["frozenImageId"]
                or receipt.get("layersIdentical") is not True or receipt.get("staticRuntimeIdentical") is not True
                or not re.fullmatch(r"sha256:[0-9a-f]{64}", image)):
            raise Blocked("ISOLATED_IMAGE_NOT_PREPARED")
        archive = folder / "isolated-trial-image.tar"
        if digest(archive) != receipt.get("archiveSha256"):
            raise Blocked("ISOLATED_IMAGE_ARCHIVE_CHANGED")
        ArchiveStore.validate_image(archive, image)
        metadata = json.loads(self.runner.run(["docker", "image", "inspect", image], 30))[0]
        cfg = metadata.get("Config") or {}
        labels = cfg.get("Labels") or {}
        if (metadata.get("Id") != image or cfg.get("Volumes") or cfg.get("ExposedPorts")
                or labels.get("org.suzume.runtime-trial") != preparation_id
                or any(key.startswith(("com.docker.compose.", "desktop.docker.io/", "desktop.docker.io.")) for key in labels)):
            raise Blocked("ISOLATED_IMAGE_METADATA_CHANGED")
        if self.preparer.checkpoint(image) != state["staticRuntimeHashes"]:
            raise Blocked("ISOLATED_RUNTIME_CHANGED")
        return image

    def offline_platform_copy(self, server, trial_root):
        # V3.3.0 b18 metadata confirms Local as native and LAN as server-only;
        # None skips crossplatform initialization. This read-only trial file
        # the fixed ServerFiles copy, original archive and live files stay intact.
        source = server / "platform.cfg"
        if source.is_symlink() or not source.is_file() or source.read_text().splitlines() != OBSERVED_PLATFORM_LINES:
            raise Blocked("PLATFORM_CONFIGURATION_NOT_OBSERVED")
        destination = trial_root / "platform.cfg"
        fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as stream:stream.write(OFFLINE_PLATFORM)
        original = source.stat()
        os.chown(destination, original.st_uid, original.st_gid)
        destination.chmod(0o644) # Contains only public platform names, no secrets.
        return destination, digest(source), digest(destination)

    def quiescent_archive(self, folder, preparation_id):
        records = self.preparer.root / "upgrade-backups/limited-maintenance-reservation"
        record = records / (preparation_id + ".completed.json")
        directory = folder / "limited-maintenance"
        archive = directory / "quiescent-world-config.tar"
        if (records.is_symlink() or record.is_symlink() or not record.is_file()
                or (records / "reservation.json").exists() or (records / "reservation.json").is_symlink()
                or directory.is_symlink() or archive.is_symlink() or not archive.is_file()):
            raise Blocked("QUIESCENT_RETURN_RECEIPT_UNAVAILABLE")
        try:
            state = json.loads(record.read_text())
        except (ValueError, OSError):
            raise Blocked("QUIESCENT_RETURN_RECEIPT_UNAVAILABLE") from None
        if (state.get("jobId") != preparation_id or state.get("phase") != "completed"
                or state.get("gameVersion") != TARGET or type(state.get("gameExitCode")) is not int
                or state["gameExitCode"] != 0 or any(state.get(k) is not True for k in
                ["gameReturned", "writerAbsenceVerified", "shutdownSent", "telnetEOF", "sourceQuiescent"])
                or any(state.get(k) is not False for k in
                ["recoveryRequired", "fullWorldBackup", "verified", "runtimeRestored"])
                or state.get("errorCode") or state.get("recoveryErrorCode")):
            raise Blocked("QUIESCENT_RETURN_NOT_PROVEN")
        if (type(state.get("quiescentCopyBytes")) is not int or state["quiescentCopyBytes"] <= 0
                or not re.fullmatch(r"[0-9a-f]{64}", str(state.get("quiescentCopySha256", "")))
                or archive.stat().st_size != state["quiescentCopyBytes"]
                or digest(archive) != state["quiescentCopySha256"]):
            raise Blocked("QUIESCENT_COPY_CHANGED")
        archive_members(archive, allowed=("7DaysToDie", "LGSM-Config"))
        return archive, state

    def restored_world(self, trial_root):
        try:
            properties = {}
            for node in ET.parse(trial_root / "ServerFiles/sdtdserver.xml").getroot().iter("property"):
                key = node.attrib.get("name")
                if key in properties:raise ValueError()
                properties[key] = node.attrib.get("value")
            name, kind = properties["GameName"], properties["GameWorld"]
            if (not name or name in [".", ".."] or any(c in name for c in "/\\\r\n*?[]")
                    or properties.get("UserDataFolder") or properties.get("SaveGameFolder")):
                raise ValueError()
            saves = list((trial_root / "7DaysToDie/Saves").glob("*/" + name + "/main.ttw"))
            if len(saves) != 1 or not saves[0].is_file() or saves[0].is_symlink():raise ValueError()
            world = saves[0].parent.parent.name
            if kind != "RWG" and world != kind:raise ValueError()
            generated = trial_root / "7DaysToDie/GeneratedWorlds" / world
            if kind == "RWG" and (generated.is_symlink() or not (generated / "map_info.xml").is_file()):raise ValueError()
            cfg = trial_root / "LGSM-Config"
            hashes = {p.relative_to(cfg).as_posix(): digest(p) for p in cfg.rglob("*") if p.is_file()}
            if not hashes:raise ValueError()
            return {"gameName": name, "gameWorld": kind, "savedWorld": world,
                    "saveMainSha256BeforeBoot": digest(saves[0]), "restoredConfigHashes": hashes}
        except (KeyError, ValueError, OSError, ET.ParseError):
            raise Blocked("RESTORED_WORLD_CONFIGURATION_UNAVAILABLE") from None

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
        expected_mounts = getattr(self, "expected_mounts", None)
        if expected_mounts is not None:
            mounts = ins.get("Mounts", [])
            actual = {m.get("Destination"): (m.get("Type"), m.get("Source")) for m in mounts}
            if actual != expected_mounts or len(mounts) != len(expected_mounts):
                raise Blocked("TRIAL_MOUNTS_NOT_ISOLATED")
            if any(m.get("RW") is not False for m in mounts if m.get("Destination") in getattr(self, "readonly_targets", set())):
                raise Blocked("TRIAL_PLATFORM_OVERRIDE_NOT_READONLY")
        return cid, ins

    def run(self, preparation_id, mode="online"):
        if not isinstance(preparation_id, str) or not JOB.fullmatch(preparation_id):
            raise Blocked("INVALID_PREPARATION_ID")
        if mode not in ["online", "check-start", "quiescent"]:
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
        trial_root = folder / {"online":"offline-runtime-trial", "check-start":"fixed-start-readiness-trial", "quiescent":"quiescent-recovery-trial"}[mode]
        try:
            trial_root.mkdir(mode=0o700)  # Never retry/reuse an uncertain trial.
        except FileExistsError:
            raise Blocked("TRIAL_EXISTS_INSPECT_STATE_FIRST") from None
        receipt = {"preparationId": preparation_id, "phase": "checking", "createdAt": time.time(),
                   "offlineBootVerified": False, "fullWorldBackup": False, "verified": False,
                   "runtimeRestored": False, "productionEnabled": False,
                   "startAttempted": False, "cleanupRequired": False,
                   "purpose": {"online":"world_copy_trial", "check-start":"fixed_start_only", "quiescent":"quiescent_world_recovery"}[mode],
                   "sourceQuiescent": False}
        def phase(value):
            receipt.update(phase=value, updatedAt=time.time())
            atomic_json(trial_root / "receipt.json", receipt)
            self.emit({"preparationId": preparation_id, "phase": value})
        name = {"online":"suzume-offline-check-", "check-start":"suzume-start-check-", "quiescent":"suzume-quiescent-check-"}[mode] + preparation_id
        cid, start_attempted = None, False
        runtime_image = state["frozenImageId"]
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
                if mode != "online":
                    runtime_image = self.resolve_trial_image(folder, state, preparation_id)
                    receipt["trialImageId"] = runtime_image
                # ServerFiles already restored and compared. The online Save and
                # config copy is NOT labelled a quiescent or complete backup.
                server = folder / "restore-target-copy/ServerFiles"
                payload = folder / "target-serverfiles.tar"
                FrozenTarget(payload, state["payloadSha256"], BUILD, state["approvedModHashes"]).verify()
                if mode == "quiescent":
                    archive, returned = self.quiescent_archive(folder, preparation_id)
                    receipt.update(sourceQuiescent=True, quiescentCopySha256=returned["quiescentCopySha256"],
                                   quiescentCopyBytes=returned["quiescentCopyBytes"], cleanExitVerified=True)
                    phase("restoring_fresh_serverfiles")
                    self.runner.run(["tar", "--acls", "--xattrs", "--numeric-owner", "--same-owner", "--same-permissions",
                                     "-xpf", str(payload), "-C", str(trial_root)], 600)
                    server = trial_root / "ServerFiles"
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
                elif mode == "check-start":
                    # Explicit runtime-only rehearsal: an unverified online
                    # snapshot is never converted into a recovery receipt.
                    archive = folder / "offline-runtime-trial/online-world-config.tar"
                    if archive.is_symlink() or not archive.is_file():
                        raise Blocked("UNVERIFIED_TRIAL_COPY_UNAVAILABLE")
                    receipt["onlineSourceCompared"] = False
                    phase("reading_unverified_snapshot_for_start_check")
                phase("restoring_quiescent_copy" if mode == "quiescent" else "restoring_online_copy")
                archive_members(archive, allowed=("7DaysToDie", "LGSM-Config"))
                self.runner.run(["tar", "--acls", "--xattrs", "--numeric-owner", "--same-owner", "--same-permissions",
                                 "-xpf", str(archive), "-C", str(trial_root)], 300)
                self.runner.run(["tar", "--acls", "--xattrs", "--compare", "--file", str(archive), "-C", str(trial_root)], 300)
                if mode == "quiescent":
                    self.quiescent_archive(folder, preparation_id) # Recheck receipt/content after restore.
                    receipt.update(self.restored_world(trial_root), worldConfigCopyRestored=True,
                                   serverFilesCopyRestored=True, modsHashMatched=True)
                else:receipt["onlineCopySha256"] = digest(archive)
                for directory in ["log", "backups"]:
                    (trial_root / directory).mkdir(mode=0o755)
                    # Match live volume ownership without changing live files.
                for directory in ["log", "backups"]:
                    source_stat = (p.root / directory).stat()
                    os.chown(trial_root / directory, source_stat.st_uid, source_stat.st_gid)
                args = ["docker", "create", "--name", name, "--label", "org.suzume.rehearsal=" + preparation_id,
                        "--label", "com.docker.compose.project=suzume-isolated-check",
                        "--label", "com.docker.compose.service=rehearsal", "--label", "com.docker.compose.oneoff=True",
                        "--network", "none", "--restart", "no", "--cpus", "2", "--memory", "6g", "--memory-swap", "6g",
                        "--pids-limit", "512", "--cap-drop", "NET_RAW", "--entrypoint",
                        "/home/sdtdserver/user.sh" if mode == "online" else "/bin/sleep"]
                mounts = [(server, "/home/sdtdserver/serverfiles"),
                          (trial_root / "7DaysToDie", "/home/sdtdserver/.local/share/7DaysToDie"),
                          (trial_root / "LGSM-Config", "/home/sdtdserver/lgsm/config-lgsm/sdtdserver"),
                          (trial_root / "log", "/home/sdtdserver/log"), (trial_root / "backups", "/home/sdtdserver/lgsm/backup")]
                for source, target in mounts:
                    args += ["--mount", "type=bind,source=" + str(source) + ",target=" + target]
                self.expected_mounts = {target: ("bind", str(source)) for source, target in mounts}
                if mode != "online":
                    platform, source_hash, trial_hash = self.offline_platform_copy(server, trial_root)
                    platform_target = "/home/sdtdserver/serverfiles/platform.cfg"
                    args += ["--mount", "type=bind,source=" + str(platform) + ",target=" + platform_target + ",readonly"]
                    self.expected_mounts[platform_target] = ("bind", str(platform))
                    self.readonly_targets = {platform_target}
                    receipt.update(platformMode="LOCAL_LAN_without_EOS", sourcePlatformSha256=source_hash, platformOverrideSha256=trial_hash)
                args.append(runtime_image)
                if mode != "online":
                    args.append("infinity")
                atomic_json(trial_root / "creation-input.private.json", {"dockerArgs": args, "expectedMounts": self.expected_mounts})
                self.require_resource_capacity()
                receipt.update(startAttempted=True, cleanupRequired=True)
                phase("starting_offline_trial")
                start_attempted = True
                self.runner.run(args, 60)
                cid, _ = self.inspect_trial(name, preparation_id, runtime_image)
                receipt["trialContainerId"] = cid
                # Verify all mounts/ports before any entrypoint or game executes.
                self.runner.run(["docker", "start", cid], 60)
                if mode != "online":
                    phase("checking_fixed_gsm_start")
                    self.runner.run(["docker", "exec", "--user", "sdtdserver", "--workdir", "/home/sdtdserver",
                                     cid, "./sdtdserver", "start"], 60)
                phase("waiting_for_offline_game")
                deadline = time.monotonic() + 180
                while time.monotonic() < deadline:
                    _, ins = self.inspect_trial(name, preparation_id, runtime_image)
                    if ins["State"].get("Status") != "running" or ins["State"].get("OOMKilled"):
                        raise Blocked("TRIAL_EXITED_OR_OOM")
                    try:
                        probe = json.loads(self.runner.run(["docker", "exec", cid, "/usr/bin/python3", "-c", WORLD_PROBE if mode == "quiescent" else PROBE], 25))
                    except Blocked:
                        time.sleep(2)
                        continue
                    if probe.get("gameVersion") != TARGET:
                        raise Blocked("TRIAL_VERSION_MISMATCH")
                    if mode == "quiescent":
                        if probe.get("gameTime", {}).get("days") != OBSERVED_WORLD_DAY or probe.get("onlinePlayers") != 0:
                            time.sleep(2) # A listening telnet socket alone does not prove a loaded save.
                            continue
                        receipt.update(gameTime=probe["gameTime"], onlinePlayers=0, savedWorldBootVerified=True)
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
                        cid, _ = self.inspect_trial(name, preparation_id, runtime_image)
                    except Blocked:
                        receipt["cleanupRequired"] = True
                        atomic_json(trial_root / "receipt.json", receipt)
                if cid:
                    try:
                        self.inspect_trial(name, preparation_id, runtime_image)
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
                        _, stopped = self.inspect_trial(name, preparation_id, runtime_image)
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
        if len(sys.argv) != 3 or sys.argv[1] not in ["rehearse", "check-start", "rehearse-quiescent"]:
            raise Blocked("USE_REHEARSE_WITH_PREPARATION_ID")
        output = OfflineBootRehearsal(RuntimePreparer(DockerRunner()), emit=lambda item: print(json.dumps(item), flush=True)).run(sys.argv[2], {"rehearse":"online", "check-start":"check-start", "rehearse-quiescent":"quiescent"}[sys.argv[1]])
        print(json.dumps(output), flush=True)
    except Blocked as error:
        print(json.dumps({"errorCode": str(error)}), flush=True)
        sys.exit(1)
