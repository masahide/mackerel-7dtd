"""Update hook core. No live SSH, Docker, SteamCMD or firewall adapter is enabled.

The backend must implement the observed deployment and a validated maintenance
fence. This module can be exercised with an isolated fake backend without game
access. The archive implementation requires GNU tar and a private Linux root.
"""
from __future__ import annotations

import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import tarfile
import tempfile
import stat
import zlib
from contextlib import contextmanager


JOB = re.compile(r"^[0-9a-f]{32}$")
SHA = re.compile(r"^[0-9a-f]{64}$")
GAME_ROOT = "/home/masahide/work/7dtd"
ENTRIES = ("ServerFiles", "7DaysToDie", "LGSM-Config", "log", "backups",
           "docker-compose.yml", "Dockerfile", ".env", "docker-compose.override.yml")
REQUIRED = ("ServerFiles", "7DaysToDie", "LGSM-Config", "log", "docker-compose.yml", "Dockerfile")


class Blocked(RuntimeError):
    """A stable error code, with no subprocess output or credentials."""


OCI_INDEX = "application/vnd.oci.image.index.v1+json"
OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
OCI_CONFIG = "application/vnd.oci.image.config.v1+json"
OCI_TAR = "application/vnd.oci.image.layer.v1.tar"
OCI_GZIP = OCI_TAR + "+gzip"
MAX_IMAGE_JSON = 1024 * 1024
MAX_LAYER_EXPANDED = 8 * 1024**3


def image_json(tar, member):
    if not member.isfile() or member.size < 0 or member.size > MAX_IMAGE_JSON:
        raise Blocked("IMAGE_ARCHIVE_INVALID")
    # Duplicate keys could be interpreted differently by Docker and Python.
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise Blocked("IMAGE_ARCHIVE_INVALID")
            result[key] = value
        return result
    with tar.extractfile(member) as f:
        return json.load(f, object_pairs_hook=unique)


def image_blob(tar, members, descriptor, allowed_types):
    if (not isinstance(descriptor, dict) or descriptor.get("mediaType") not in allowed_types
            or not isinstance(descriptor.get("digest"), str)
            or not re.fullmatch(r"sha256:[0-9a-f]{64}", descriptor["digest"])
            or type(descriptor.get("size")) is not int or descriptor["size"] < 0
            or "urls" in descriptor or "data" in descriptor):
        raise Blocked("IMAGE_DESCRIPTOR_INVALID")
    name = "blobs/sha256/" + descriptor["digest"].split(":", 1)[1]
    member = members[name]
    if not member.isfile() or member.size != descriptor["size"]:
        raise Blocked("IMAGE_BLOB_SIZE_MISMATCH")
    h = hashlib.sha256()
    with tar.extractfile(member) as f:
        for chunk in iter(lambda: f.read(4 * 1024 * 1024), b""):
            h.update(chunk)
    if "sha256:" + h.hexdigest() != descriptor["digest"]:
        raise Blocked("IMAGE_BLOB_DIGEST_MISMATCH")
    return member


def image_layer(tar, member, expected_diff_id, compressed):
    if not member.isfile() or not isinstance(expected_diff_id, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", expected_diff_id):
        raise Blocked("IMAGE_ARCHIVE_INVALID")
    # Manifest digest hashes stored bytes. DiffID hashes the ENTIRE decoded tar
    # stream, including padding. Reading to EOF also verifies gzip CRC/trailer.
    h, length = hashlib.sha256(), 0
    with tar.extractfile(member) as f:
        stream = gzip.GzipFile(fileobj=f, mode="rb") if compressed else f
        try:
            for chunk in iter(lambda: stream.read(4 * 1024 * 1024), b""):
                length += len(chunk)
                if length > MAX_LAYER_EXPANDED:
                    raise Blocked("IMAGE_LAYER_TOO_LARGE")
                h.update(chunk)
        finally:
            if compressed:
                stream.close()
    if "sha256:" + h.hexdigest() != expected_diff_id:
        raise Blocked("IMAGE_LAYER_MISMATCH")
    # Hash equality alone does not establish that the decoded bytes are a tar.
    # Parse without extracting, and read regular payloads to catch truncation.
    with tar.extractfile(member) as f:
        stream = gzip.GzipFile(fileobj=f, mode="rb") if compressed else f
        try:
            with tarfile.open(fileobj=stream, mode="r|") as layer:
                for item in layer:
                    if item.isfile():
                        with layer.extractfile(item) as contents:
                            for _ in iter(lambda: contents.read(4 * 1024 * 1024), b""):
                                pass
        finally:
            if compressed:
                stream.close()


def validate_image_archive(image, image_id):
    """Validate legacy Docker-save or a single-manifest OCI image layout.

    Only local SHA256 blobs, ordinary/gzip OCI layers, and a single runnable
    manifest are accepted. Multi-platform indexes, zstd, external descriptors,
    and unknown media types require their own validator; they fail closed here.
    Nothing is extracted, downloaded, loaded, or executed.
    """
    try:
        if image.is_symlink() or not isinstance(image_id, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
            raise Blocked("IMAGE_ARCHIVE_INVALID")
        with tarfile.open(image, "r:") as tar:
            members = {}
            for m in tar:
                p = PurePosixPath(m.name.rstrip("/"))
                if (p.is_absolute() or ".." in p.parts or "\\" in m.name or str(p) in members
                        or not (m.isfile() or m.isdir())):
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                members[str(p)] = m
            docker_manifest = image_json(tar, members["manifest.json"]) if "manifest.json" in members else None
            if docker_manifest is not None and (not isinstance(docker_manifest, list) or len(docker_manifest) != 1
                                                or not isinstance(docker_manifest[0], dict)):
                raise Blocked("IMAGE_ARCHIVE_INVALID")
            is_oci = "index.json" in members or "oci-layout" in members
            if is_oci:
                layout = image_json(tar, members["oci-layout"])
                index = image_json(tar, members["index.json"])
                if (layout != {"imageLayoutVersion": "1.0.0"} or not isinstance(index, dict)
                        or type(index.get("schemaVersion")) is not int or index["schemaVersion"] != 2
                        or index.get("mediaType", OCI_INDEX) != OCI_INDEX
                        or not isinstance(index.get("manifests"), list) or len(index["manifests"]) != 1):
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                descriptor = index["manifests"][0]
                manifest_member = image_blob(tar, members, descriptor, {OCI_MANIFEST})
                if descriptor["digest"] != image_id:
                    raise Blocked("IMAGE_ID_MISMATCH")
                manifest = image_json(tar, manifest_member)
                if (not isinstance(manifest, dict) or type(manifest.get("schemaVersion")) is not int
                        or manifest["schemaVersion"] != 2 or manifest.get("mediaType") != OCI_MANIFEST):
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                config_member = image_blob(tar, members, manifest["config"], {OCI_CONFIG})
                config = image_json(tar, config_member)
                if (not isinstance(config, dict) or config.get("os") != "linux" or config.get("architecture") != "amd64"
                        or not isinstance(config.get("rootfs"), dict) or config["rootfs"].get("type") != "layers"):
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                diff_ids, layers = config["rootfs"]["diff_ids"], manifest["layers"]
                if not isinstance(layers, list) or not layers or not isinstance(diff_ids, list) or len(layers) != len(diff_ids):
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                names = []
                for descriptor, diff_id in zip(layers, diff_ids):
                    member = image_blob(tar, members, descriptor, {OCI_TAR, OCI_GZIP})
                    names.append(member.name)
                    image_layer(tar, member, diff_id, descriptor["mediaType"] == OCI_GZIP)
                # Docker-save can include both OCI and legacy entrypoints. Both
                # must point at the exact same config and ordered layer blobs.
                if docker_manifest is not None and (docker_manifest[0].get("Config") != config_member.name
                                                     or docker_manifest[0].get("Layers") != names):
                    raise Blocked("IMAGE_MANIFEST_MISMATCH")
            else:
                if docker_manifest is None:
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                config_member = members[docker_manifest[0]["Config"]]
                if not config_member.isfile() or config_member.size > MAX_IMAGE_JSON:
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                with tar.extractfile(config_member) as f:
                    data = f.read()
                if "sha256:" + hashlib.sha256(data).hexdigest() != image_id:
                    raise Blocked("IMAGE_ID_MISMATCH")
                config = image_json(tar, config_member)
                diff_ids, layers = config["rootfs"]["diff_ids"], docker_manifest[0]["Layers"]
                if not isinstance(layers, list) or not layers or not isinstance(diff_ids, list) or len(layers) != len(diff_ids):
                    raise Blocked("IMAGE_ARCHIVE_INVALID")
                for name, diff_id in zip(layers, diff_ids):
                    image_layer(tar, members[name], diff_id, False)
    except (KeyError, TypeError, ValueError, tarfile.TarError, OSError, AttributeError, EOFError, zlib.error):
        raise Blocked("IMAGE_ARCHIVE_INVALID") from None


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
        validate_image_archive(image, image_id)

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
