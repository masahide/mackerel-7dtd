"""Update hook core. No live SSH, Docker, SteamCMD or firewall adapter is enabled.

The backend must implement the observed deployment and a validated maintenance
fence. This module can be exercised with an isolated fake backend without game
access. The archive implementation requires GNU tar and a private Linux root.
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import tarfile
import tempfile
import stat
from contextlib import contextmanager


JOB = re.compile(r"^[0-9a-f]{32}$")
SHA = re.compile(r"^[0-9a-f]{64}$")
GAME_ROOT = "/home/masahide/work/7dtd"
ENTRIES = ("ServerFiles", "7DaysToDie", "LGSM-Config", "log", "backups",
           "docker-compose.yml", "Dockerfile", ".env", "docker-compose.override.yml")
REQUIRED = ("ServerFiles", "7DaysToDie", "LGSM-Config", "log", "docker-compose.yml", "Dockerfile")


class Blocked(RuntimeError):
    """A stable error code, with no subprocess output or credentials."""


def digest(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(4 * 1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def private_directory(path: Path):
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    if (not stat.S_ISDIR(info.st_mode)
            or (os.name == "posix" and (info.st_uid != os.geteuid() or info.st_mode & 0o077))):
        raise Blocked("UNSAFE_PRIVATE_DIRECTORY")


def atomic_json(path: Path, value: dict) -> None:
    fd, name = tempfile.mkstemp(prefix=".state-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            os.chmod(name, 0o600)
            json.dump(value, f, sort_keys=True)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
        if os.name == "posix":
            directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


class Reservation:
    """Durable, fail-closed reservation, retained across process deaths.

    Every external mutator must check this same record under action.lock. This
    cannot constrain operators or legacy scripts that do not join the protocol.
    No stale timeout, unlink or automatic recovery is implemented.
    """
    def __init__(self, directory: Path):
        self.directory = directory
        private_directory(directory)
        self.path = directory / "reservation.json"

    @contextmanager
    def locked(self):
        import fcntl
        flags = os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW
        fd = os.open(self.directory / "action.lock", flags, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            yield
        except BlockingIOError:
            raise Blocked("EXTERNAL_OPERATION_BUSY") from None
        finally:
            os.close(fd)

    def read(self):
        if self.path.is_symlink():
            raise Blocked("UNSAFE_STATE_FILE")
        if not self.path.exists():
            return None
        try:
            data = json.loads(self.path.read_text(encoding="utf-8"))
            if not JOB.fullmatch(data["jobId"]) or not isinstance(data["phase"], str):
                raise ValueError()
            return data
        except (ValueError, KeyError, TypeError):
            raise Blocked("STATE_REPAIR_REQUIRED") from None

    def require_free(self):
        if self.read() is not None:
            raise Blocked("MAINTENANCE_RESERVED")

    def claim(self, job_id: str, target: str):
        if not JOB.fullmatch(job_id):
            raise Blocked("INVALID_JOB_ID")
        self.require_free()
        state = {"jobId": job_id, "targetVersion": target, "phase": "reserved", "recoveryRequired": True}
        atomic_json(self.path, state)
        return state

    def owned(self, job_id: str, phase: str):
        state = self.read()
        if state is None or state.get("jobId") != job_id:
            raise Blocked("RESERVATION_OWNER_MISMATCH")
        if state["phase"] != phase:
            raise Blocked("PHASE_OR_RECOVERY_MISMATCH")
        return state

    def save(self, state, phase):
        state["phase"] = phase
        atomic_json(self.path, state)

    def release(self, state):
        # Keep the receipt for audit; deleting reservation alone is never a
        # supported way to clear an interrupted job.
        atomic_json(self.directory / (state["jobId"] + ".completed.json"), state)
        self.path.unlink()
        if os.name == "posix":
            fd = os.open(self.directory, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)


def archive_members(archive: Path, allowed=ENTRIES):
    """Reject traversal, links outside the bundle, devices and link parents."""
    members = {}
    links = set()
    total = 0
    try:
        with tarfile.open(archive, "r:") as tar:
            for member in tar:
                name = member.name.rstrip("/")
                p = PurePosixPath(name)
                if (not name or p.is_absolute() or ".." in p.parts or "\\" in name
                        or str(p) != name or p.parts[0] not in allowed or name in members):
                    raise Blocked("UNSAFE_ARCHIVE_MEMBER")
                if not (member.isfile() or member.isdir() or member.issym() or member.islnk()):
                    raise Blocked("UNSAFE_ARCHIVE_TYPE")
                if member.issym() or member.islnk():
                    target = PurePosixPath(member.linkname)
                    if target.is_absolute() or "\\" in member.linkname:
                        raise Blocked("UNSAFE_ARCHIVE_LINK")
                    parts = list(p.parent.parts) if member.issym() else []
                    for part in target.parts:
                        if part == "..":
                            if not parts:
                                raise Blocked("UNSAFE_ARCHIVE_LINK")
                            parts.pop()
                        elif part != ".":
                            parts.append(part)
                    if not parts or parts[0] not in allowed:
                        raise Blocked("UNSAFE_ARCHIVE_LINK")
                    links.add(name)
                if member.size < 0:
                    raise Blocked("UNSAFE_ARCHIVE_SIZE")
                total += member.size
                if len(members) >= 1_000_000 or total > 2 * 1024**4:
                    raise Blocked("ARCHIVE_CAPACITY_EXCEEDED")
                members[name] = member
    except (tarfile.TarError, OSError):
        raise Blocked("ARCHIVE_UNREADABLE") from None
    for name in members:
        if any(str(parent) in links for parent in PurePosixPath(name).parents):
            raise Blocked("ARCHIVE_LINK_PARENT")
    return members


class ArchiveStore:
    """Quiescent game-root backup plus isolated file restore rehearsal.

    GNU tar verifies file contents, uid/gid, modes, ACLs and xattrs. A passing
    rehearsal proves filesystem recovery only, not gameplay or mod compatibility.
    A frozen image export is mandatory. External VPN files and container writable
    state are outside this scope: this class NEVER emits the API verified receipt.
    """
    def __init__(self, directory: Path, run=None):
        self.directory = directory
        private_directory(directory)
        self.run = run or self._run

    @staticmethod
    def _run(args):
        try:
            p = subprocess.run(args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=1800)
            if p.returncode:
                raise Blocked("ARCHIVE_COMMAND_FAILED")
        except (OSError, subprocess.TimeoutExpired):
            raise Blocked("ARCHIVE_COMMAND_FAILED") from None

    def create(self, root: Path, job_id: str, export_image, image_id: str):
        if not JOB.fullmatch(job_id) or not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
            raise Blocked("INVALID_BACKUP_IDENTITY")
        if root.is_symlink() or not root.is_dir():
            raise Blocked("UNSAFE_GAME_ROOT")
        for name in REQUIRED:
            if not (root / name).exists() or (root / name).is_symlink():
                raise Blocked("BACKUP_SCOPE_INCOMPLETE")
        folder = self.directory / ("backup_" + job_id)
        folder.mkdir(mode=0o700)  # Existing/uncertain attempts cannot overwrite.
        archive = folder / "rollback.tar"
        entries = [name for name in ENTRIES if (root / name).exists()]
        self.run(["tar", "--acls", "--xattrs", "--numeric-owner", "-cpf", str(archive), "-C", str(root), *entries])
        os.chmod(archive, 0o600)
        self.run(["tar", "--acls", "--xattrs", "--compare", "--file", str(archive), "-C", str(root)])
        sha = digest(archive)
        image = folder / "image.tar"
        export_image(image, image_id)
        if image.is_symlink() or not image.is_file():
            raise Blocked("UNSAFE_IMAGE_EXPORT")
        os.chmod(image, 0o600)
        self.validate_image(image, image_id)
        evidence = {"backupId": "backup_" + job_id, "archiveSha256": sha, "archiveBytes": archive.stat().st_size,
                    "imageId": image_id, "imageSha256": digest(image), "imageBytes": image.stat().st_size,
                    "entries": entries, "verified": False, "runtimeRestored": False}
        atomic_json(folder / "evidence.json", evidence)
        self.rehearse(folder)
        evidence.update(filesystemRestored=True)
        atomic_json(folder / "evidence.json", evidence)
        return {"backupId": evidence["backupId"], "filesystemRestored": True, "runtimeRestored": False}

    @staticmethod
    def validate_image(image: Path, image_id: str):
        # Verify the Docker-save image identity and referenced layer bytes.
        # OCI/layout variants are deliberately not accepted without a validator.
        try:
            with tarfile.open(image, "r:") as tar:
                members = {}
                for m in tar:
                    p = PurePosixPath(m.name.rstrip("/"))
                    if (p.is_absolute() or ".." in p.parts or "\\" in m.name or str(p) in members
                            or not (m.isfile() or m.isdir())):
                        raise Blocked("IMAGE_ARCHIVE_INVALID")
                    members[str(p)] = m
                manifest_member = members.get("manifest.json")
                if manifest_member is None or manifest_member.size > 1024 * 1024:
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                manifest = json.load(tar.extractfile(manifest_member))
                if not isinstance(manifest, list) or len(manifest) != 1:
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                config = members[manifest[0]["Config"]]
                if config.size > 1024 * 1024:
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                data = tar.extractfile(config).read()
                if "sha256:" + hashlib.sha256(data).hexdigest() != image_id:
                    raise Blocked("IMAGE_ID_MISMATCH")
                diff_ids = json.loads(data)["rootfs"]["diff_ids"]
                layers = manifest[0]["Layers"]
                if not layers or len(layers) != len(diff_ids):
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                for name, expected in zip(layers, diff_ids):
                    if not re.fullmatch(r"sha256:[0-9a-f]{64}", expected) or not members[name].isfile():
                        raise Blocked("IMAGE_ARCHIVE_INVALID")
                    h = hashlib.sha256()
                    with tar.extractfile(members[name]) as f:
                        for chunk in iter(lambda: f.read(4 * 1024 * 1024), b""):
                            h.update(chunk)
                    if "sha256:" + h.hexdigest() != expected:
                        raise Blocked("IMAGE_LAYER_MISMATCH")
                    # Docker diff_ids identify uncompressed tar layers. Digest
                    # equality alone does not prove the saved layer is readable.
                    with tar.extractfile(members[name]) as f:
                        with tarfile.open(fileobj=f, mode="r:") as layer:
                            for _ in layer:
                                pass
        except (KeyError, TypeError, ValueError, tarfile.TarError, OSError, AttributeError):
            raise Blocked("IMAGE_ARCHIVE_INVALID") from None

    def rehearse(self, folder: Path):
        # Restores into a NEW private directory only. Never takes a live restore
        # destination, extracts over a mounted world, or starts recovered games.
        if folder.parent.resolve() != self.directory.resolve() or folder.is_symlink():
            raise Blocked("UNSAFE_BACKUP_LOCATION")
        evidence = json.loads((folder / "evidence.json").read_text(encoding="utf-8"))
        archive = folder / "rollback.tar"
        image = folder / "image.tar"
        if (archive.is_symlink() or image.is_symlink() or archive.stat().st_size != evidence["archiveBytes"]
                or not SHA.fullmatch(evidence["archiveSha256"]) or digest(archive) != evidence["archiveSha256"]
                or image.stat().st_size != evidence["imageBytes"] or digest(image) != evidence["imageSha256"]):
            raise Blocked("BACKUP_HASH_MISMATCH")
        members = archive_members(archive)
        if (any(name not in members for name in REQUIRED)
                or any(not members[name].isdir() for name in REQUIRED[:4])
                or any(not members[name].isfile() for name in REQUIRED[4:])):
            raise Blocked("BACKUP_SCOPE_INCOMPLETE")
        self.validate_image(image, evidence["imageId"])
        restored = folder / "restore-copy"
        restored.mkdir(mode=0o700)
        self.run(["tar", "--acls", "--xattrs", "--numeric-owner", "--same-owner", "--same-permissions",
                  "-xpf", str(archive), "-C", str(restored)])
        self.run(["tar", "--acls", "--xattrs", "--compare", "--file", str(archive), "-C", str(restored)])
        return restored


class FrozenTarget:
    """Validate a pre-fetched payload; never resolve or download latest stable.

    Expected hashes/build/mod inventory come from an operator-reviewed profile.
    Checking bytes and version metadata cannot establish runtime compatibility.
    """
    def __init__(self, archive: Path, archive_sha256: str, steam_build: str, mod_hashes: dict):
        self.archive, self.sha, self.build, self.mods = archive, archive_sha256, steam_build, mod_hashes

    def verify(self):
        if (self.archive.is_symlink() or not isinstance(self.sha, str) or not SHA.fullmatch(self.sha)
                or not isinstance(self.build, str) or not re.fullmatch(r"[0-9]{1,20}", self.build)
                or digest(self.archive) != self.sha):
            raise Blocked("FROZEN_TARGET_HASH_MISMATCH")
        members = archive_members(self.archive, allowed=("ServerFiles",))
        manifest_name = "ServerFiles/steamapps/appmanifest_294420.acf"
        manifest = members.get(manifest_name)
        if manifest is None or not manifest.isfile() or manifest.size > 64 * 1024:
            raise Blocked("TARGET_BUILD_UNAVAILABLE")
        found_mods = {}
        with tarfile.open(self.archive, "r:") as tar:
            text = tar.extractfile(manifest).read().decode("utf-8")
            if re.findall(r'"buildid"\s*"([0-9]+)"', text) != [self.build]:
                raise Blocked("TARGET_BUILD_MISMATCH")
            for name, member in members.items():
                if name.startswith("ServerFiles/Mods/") and not member.isdir():
                    if not member.isfile():
                        raise Blocked("TARGET_MOD_LINK_UNSUPPORTED")
                    h = hashlib.sha256()
                    with tar.extractfile(member) as f:
                        for chunk in iter(lambda: f.read(4 * 1024 * 1024), b""):
                            h.update(chunk)
                    found_mods[name.removeprefix("ServerFiles/Mods/")] = h.hexdigest()
        if not self.mods or found_mods != self.mods:
            raise Blocked("TARGET_MOD_INVENTORY_MISMATCH")
        return members

    def stage_copy(self, directory: Path):
        self.verify()
        directory.mkdir(mode=0o700)  # Always NEW; a live destination is unsupported.
        ArchiveStore._run(["tar", "--acls", "--xattrs", "--numeric-owner", "--same-owner", "--same-permissions",
                           "-xpf", str(self.archive), "-C", str(directory)])
        ArchiveStore._run(["tar", "--acls", "--xattrs", "--compare", "--file", str(self.archive), "-C", str(directory)])
        return directory / "ServerFiles"


class Workflow:
    """Six operation hooks, plus finish AFTER the management API verifies.

    backend must keep admission closed across Docker recreation; inhibit cron,
    external deploy/restart and legacy mutators; use a pre-fetched, hash-pinned
    target; prove no game/save writers; and prove runtime and mod compatibility.
    No production adapter with these properties has yet been validated.
    """
    def __init__(self, reservation: Reservation, backend, target_version: str):
        self.reservation, self.backend, self.target = reservation, backend, target_version

    def preflight(self):
        with self.reservation.locked():
            self.reservation.require_free()
            self.backend.preflight(self.target)  # Read-only, incl all readiness proofs.

    def stop(self, job_id, current_version):
        with self.reservation.locked():
            self.backend.preflight(self.target)
            state = self.reservation.claim(job_id, self.target)
            self.reservation.save(state, "fencing")
            self.backend.close_admission_and_external_operations(job_id)
            version, players = self.backend.probe()
            if type(players) is not int or players != 0:
                raise Blocked("PLAYERS_NOT_VERIFIED_ZERO")
            if version != current_version:
                raise Blocked("VERSION_CHANGED")
            self.reservation.save(state, "stopping")
            self.backend.stop_cleanly()
            self.reservation.save(state, "stopped")

    def _phase(self, job_id, expected, pending, completed, operation):
        with self.reservation.locked():
            state = self.reservation.owned(job_id, expected)
            if state["targetVersion"] != self.target:
                raise Blocked("RESERVED_TARGET_CHANGED")
            self.backend.assert_fence_and_exclusion(job_id)
            self.reservation.save(state, pending)
            result = operation(state)
            self.reservation.save(state, completed)
            return result

    def check_stopped(self, job_id):
        return self._phase(job_id, "stopped", "checking_stopped", "quiescent",
                           lambda _: self.backend.assert_no_game_or_save_writers())

    def backup(self, job_id):
        def save(state):
            self.backend.assert_no_game_or_save_writers()
            result = self.backend.backup_and_rehearse(job_id)
            if (set(result) != {"backupId", "verified"} or result["verified"] is not True
                    or not re.fullmatch(r"[A-Za-z0-9_-]{16,128}", result["backupId"])):
                raise Blocked("BACKUP_NOT_VERIFIED")
            state["backupId"] = result["backupId"]
            return result
        return self._phase(job_id, "quiescent", "backing_up", "backed_up", save)

    def apply(self, job_id):
        def apply(state):
            self.backend.assert_no_game_or_save_writers()
            self.backend.verify_backup(state["backupId"])
            self.backend.apply_frozen_target(self.target)
            self.backend.verify_installed_target_and_mods(self.target)
        return self._phase(job_id, "backed_up", "applying", "applied", apply)

    def start(self, job_id):
        return self._phase(job_id, "applied", "starting", "started",
                           lambda _: self.backend.start_with_admission_closed())

    def finish(self, job_id):
        def finish(state):
            # API already verified; recheck under the same external reservation
            # before releasing admission. Do not release on any unknown state.
            version, players = self.backend.probe()
            if version != self.target or type(players) is not int or players != 0:
                raise Blocked("FINISH_NOT_VERIFIED")
            self.backend.verify_runtime_and_mods(self.target)
            self.backend.open_admission_and_external_operations(job_id)
            state["recoveryRequired"] = False
        self._phase(job_id, "started", "releasing", "completed", finish)
        with self.reservation.locked():
            state = self.reservation.owned(job_id, "completed")
            self.reservation.release(state)
