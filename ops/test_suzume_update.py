import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

from suzume_update import ArchiveStore, Blocked, FrozenTarget, Reservation, Workflow, archive_members, digest, REQUIRED

ID = "1234567890abcdef1234567890abcdef"
OTHER = "abcdef1234567890abcdef1234567890"
OLD = "Game version: V 3.2.0 (b10) Compatibility Version: V 3.2.0"
TARGET = "Game version: V 3.3.0 (b18) Compatibility Version: V 3.3.0"


def make_image(path, wrong_layer=False):
    layer_io = io.BytesIO()
    with tarfile.open(fileobj=layer_io, mode="w") as layer_tar:
        member = tarfile.TarInfo("fixture.txt")
        member.size = 7
        layer_tar.addfile(member, io.BytesIO(b"fixture"))
    layer = layer_io.getvalue()
    config = json.dumps({"rootfs": {"diff_ids": ["sha256:" + hashlib.sha256(layer).hexdigest()]}}).encode()
    image_id = "sha256:" + hashlib.sha256(config).hexdigest()
    with tarfile.open(path, "w") as tar:
        entries = {"config.json": config, "layer.tar": layer if not wrong_layer else b"corrupt layer",
                   "manifest.json": json.dumps([{"Config": "config.json", "Layers": ["layer.tar"]}]).encode()}
        for name, data in entries.items():
            info = tarfile.TarInfo(name)
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))
    return image_id


class FakeBackend:
    def __init__(self):
        self.events = []
        self.version, self.players = OLD, 0
        self.fenced = False
        self.fail = ""
        self.receipt = {"backupId": "backup_" + ID, "verified": True}

    def event(self, name):
        self.events.append(name)
        if self.fail == name:
            raise Blocked("MOCK_FAILURE")

    def preflight(self, target): self.event("preflight")
    def close_admission_and_external_operations(self, job):
        self.event("fence")
        self.fenced = True
    def assert_fence_and_exclusion(self, job):
        self.event("assert_fence")
        if not self.fenced: raise Blocked("FENCE_LOST")
    def probe(self):
        self.event("probe")
        return self.version, self.players
    def stop_cleanly(self): self.event("stop")
    def assert_no_game_or_save_writers(self): self.event("quiescent")
    def backup_and_rehearse(self, job):
        self.event("backup")
        return self.receipt
    def verify_backup(self, backup): self.event("verify_backup")
    def apply_frozen_target(self, target):
        self.event("apply")
        self.version = target
    def verify_installed_target_and_mods(self, target): self.event("verify_installed")
    def start_with_admission_closed(self): self.event("start")
    def verify_runtime_and_mods(self, target): self.event("verify_runtime")
    def open_admission_and_external_operations(self, job):
        self.event("release")
        self.fenced = False


@unittest.skipUnless(os.name == "posix", "POSIX flock is required; Linux CI exercises reservation")
class WorkflowTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.reservation = Reservation(Path(self.temp.name) / "state")
        self.backend = FakeBackend()
        self.workflow = Workflow(self.reservation, self.backend, TARGET)

    def through(self, last="finish"):
        operations = [("stop", lambda: self.workflow.stop(ID, OLD)),
                      ("check", lambda: self.workflow.check_stopped(ID)),
                      ("backup", lambda: self.workflow.backup(ID)),
                      ("apply", lambda: self.workflow.apply(ID)),
                      ("start", lambda: self.workflow.start(ID)),
                      ("finish", lambda: self.workflow.finish(ID))]
        for name, operation in operations:
            operation()
            if name == last: break

    def test_success_releases_only_after_runtime_verification(self):
        self.workflow.preflight()
        self.through()
        self.assertIsNone(self.reservation.read())
        self.assertFalse(self.backend.fenced)
        self.assertLess(self.backend.events.index("verify_runtime"), self.backend.events.index("release"))
        receipt = json.loads((self.reservation.directory / (ID + ".completed.json")).read_text())
        self.assertFalse(receipt["recoveryRequired"])

    def test_player_race_or_unknown_never_stops(self):
        for count in [1, None, True, -1]:
            with self.subTest(count=count), tempfile.TemporaryDirectory() as tmp:
                reservation = Reservation(Path(tmp))
                backend = FakeBackend()
                backend.players = count
                workflow = Workflow(reservation, backend, TARGET)
                with self.assertRaisesRegex(Blocked, "PLAYERS_NOT_VERIFIED_ZERO"):
                    workflow.stop(ID, OLD)
                self.assertNotIn("stop", backend.events)
                self.assertTrue(backend.fenced)
                self.assertTrue(reservation.read()["recoveryRequired"])

    def test_version_race_never_stops(self):
        self.backend.version = TARGET
        with self.assertRaisesRegex(Blocked, "VERSION_CHANGED"):
            self.workflow.stop(ID, OLD)
        self.assertNotIn("stop", self.backend.events)

    def test_failed_fence_does_not_stop_or_clear_reservation(self):
        self.backend.fail = "fence"
        with self.assertRaises(Blocked):
            self.workflow.stop(ID, OLD)
        self.assertEqual(self.reservation.read()["phase"], "fencing")
        self.assertTrue(self.reservation.read()["recoveryRequired"])
        self.assertNotIn("stop", self.backend.events)
        self.assertNotIn("release", self.backend.events)

    def test_changed_target_cannot_continue_reserved_job(self):
        self.through("stop")
        changed = Workflow(self.reservation, self.backend, OLD)
        with self.assertRaisesRegex(Blocked, "RESERVED_TARGET_CHANGED"):
            changed.check_stopped(ID)
        self.assertEqual(self.reservation.read()["phase"], "stopped")

    def test_restart_stale_and_duplicate_requests_cannot_resume(self):
        self.through("stop")
        other = Workflow(Reservation(self.reservation.directory), self.backend, TARGET)
        for job in [ID, OTHER]:
            with self.assertRaisesRegex(Blocked, "MAINTENANCE_RESERVED"):
                other.stop(job, OLD)
        with self.assertRaisesRegex(Blocked, "MAINTENANCE_RESERVED"):
            other.preflight()
        with self.assertRaisesRegex(Blocked, "RESERVATION_OWNER_MISMATCH"):
            other.check_stopped(OTHER)
        self.assertEqual(self.backend.events.count("stop"), 1)

    def test_parallel_external_action_blocks(self):
        other = Reservation(self.reservation.directory)
        with self.reservation.locked():
            with self.assertRaisesRegex(Blocked, "EXTERNAL_OPERATION_BUSY"):
                with other.locked(): pass

    def test_bad_backup_never_applies(self):
        self.through("check")
        self.backend.receipt["verified"] = False
        with self.assertRaisesRegex(Blocked, "BACKUP_NOT_VERIFIED"):
            self.workflow.backup(ID)
        with self.assertRaisesRegex(Blocked, "PHASE_OR_RECOVERY_MISMATCH"):
            self.workflow.apply(ID)
        self.assertNotIn("apply", self.backend.events)

    def test_each_interrupted_phase_retains_fence_and_recovery(self):
        for failure in ["stop", "quiescent", "backup", "verify_backup", "apply", "verify_installed", "start", "verify_runtime", "release"]:
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                backend = FakeBackend()
                backend.fail = failure
                reservation = Reservation(Path(tmp))
                workflow = Workflow(reservation, backend, TARGET)
                with self.assertRaises(Blocked):
                    workflow.stop(ID, OLD)
                    workflow.check_stopped(ID)
                    workflow.backup(ID)
                    workflow.apply(ID)
                    workflow.start(ID)
                    workflow.finish(ID)
                self.assertTrue(reservation.read()["recoveryRequired"])
                self.assertTrue(backend.fenced)
                with self.assertRaisesRegex(Blocked, "MAINTENANCE_RESERVED"):
                    workflow.preflight()

    def test_fence_loss_and_wrong_final_version_do_not_release(self):
        self.through("stop")
        self.backend.fenced = False
        with self.assertRaisesRegex(Blocked, "FENCE_LOST"):
            self.workflow.check_stopped(ID)
        self.backend.fenced = True
        self.workflow.check_stopped(ID)
        self.workflow.backup(ID)
        self.workflow.apply(ID)
        self.workflow.start(ID)
        self.backend.version = OLD
        with self.assertRaisesRegex(Blocked, "FINISH_NOT_VERIFIED"):
            self.workflow.finish(ID)
        self.assertTrue(self.backend.fenced)
        self.assertNotIn("release", self.backend.events)


class ArchiveTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)

    def test_image_layers_and_id_verified(self):
        image = self.base / "image.tar"
        image_id = make_image(image)
        ArchiveStore.validate_image(image, image_id)
        with self.assertRaisesRegex(Blocked, "IMAGE_ID_MISMATCH"):
            ArchiveStore.validate_image(image, "sha256:" + "0" * 64)
        make_image(image, wrong_layer=True)
        with self.assertRaisesRegex(Blocked, "IMAGE_LAYER_MISMATCH"):
            ArchiveStore.validate_image(image, image_id)

    def test_tar_rejects_escape_links_devices_and_duplicates(self):
        cases = [("../outside", "file", ""), ("/outside", "file", ""),
                 ("ServerFiles/a", "symlink", "../../outside"),
                 ("ServerFiles/a", "symlink", "/outside"),
                 ("ServerFiles/device", "device", ""), ("unapproved", "file", "")]
        for name, kind, target in cases:
            with self.subTest(name=name, kind=kind):
                archive = self.base / "bad.tar"
                with tarfile.open(archive, "w") as tar:
                    m = tarfile.TarInfo(name)
                    if kind == "symlink": m.type, m.linkname = tarfile.SYMTYPE, target
                    if kind == "device": m.type = tarfile.CHRTYPE
                    tar.addfile(m)
                with self.assertRaises(Blocked): archive_members(archive)
        for mode in ["duplicate", "link-parent"]:
            archive = self.base / "bad.tar"
            with tarfile.open(archive, "w") as tar:
                m = tarfile.TarInfo("ServerFiles/a")
                if mode == "link-parent": m.type, m.linkname = tarfile.SYMTYPE, "../7DaysToDie"
                tar.addfile(m)
                tar.addfile(tarfile.TarInfo("ServerFiles/a" if mode == "duplicate" else "ServerFiles/a/file"))
            with self.assertRaises(Blocked): archive_members(archive)

    @unittest.skipUnless(os.name == "posix", "GNU tar restore/owner/ACL checks run on Linux CI")
    def test_complete_restore_matches_sources_and_metadata(self):
        root = self.base / "game"
        root.mkdir()
        for name in REQUIRED:
            p = root / name
            if name in REQUIRED[4:]: p.write_text("isolated configuration " + name)
            else:
                p.mkdir()
                (p / "fixture.bin").write_bytes(b"world/config/mod/executable fixture\0\xff")
        (root / "backups").mkdir()
        (root / "ServerFiles" / "run").write_text("executable fixture")
        (root / "ServerFiles" / "run").chmod(0o751)
        os.link(root / "ServerFiles" / "run", root / "ServerFiles" / "run-hardlink")
        os.symlink("run", root / "ServerFiles" / "run-symlink")
        os.setxattr(root / "ServerFiles" / "run", "user.fixture", b"restored-xattr")
        # If ACL tools are installed, exercise an actual extended ACL too.
        import shutil
        acl = shutil.which("setfacl")
        if acl:
            subprocess.run([acl, "-m", "u:12345:r--", str(root / "ServerFiles" / "run")], check=True)
        image = self.base / "fixture-image.tar"
        image_id = make_image(image)
        store = ArchiveStore(self.base / "backups")
        receipt = store.create(root, ID, lambda destination, _: shutil.copyfile(image, destination), image_id)
        self.assertTrue(receipt["filesystemRestored"])
        self.assertNotIn("verified", receipt)  # Must NOT be forwarded to the API.
        folder = store.directory / receipt["backupId"]
        restored = folder / "restore-copy"
        self.assertEqual(digest(root / "ServerFiles" / "run"), digest(restored / "ServerFiles" / "run"))
        self.assertEqual((restored / "ServerFiles" / "run").stat().st_mode & 0o777, 0o751 if not acl else (root / "ServerFiles" / "run").stat().st_mode & 0o777)
        self.assertEqual(os.readlink(restored / "ServerFiles" / "run-symlink"), "run")
        self.assertEqual(os.stat(restored / "ServerFiles" / "run").st_ino, os.stat(restored / "ServerFiles" / "run-hardlink").st_ino)
        self.assertEqual(os.getxattr(restored / "ServerFiles" / "run", "user.fixture"), b"restored-xattr")
        if acl:
            before = subprocess.check_output(["getfacl", "-cp", str(root / "ServerFiles" / "run")])
            after = subprocess.check_output(["getfacl", "-cp", str(restored / "ServerFiles" / "run")])
            self.assertEqual(before, after)
        evidence = json.loads((folder / "evidence.json").read_text())
        self.assertTrue(evidence["filesystemRestored"])
        self.assertFalse(evidence["verified"])
        self.assertFalse(evidence["runtimeRestored"])
        self.assertIn("backups", evidence["entries"])
        with self.assertRaises(FileExistsError):
            store.create(root, ID, lambda destination, _: shutil.copyfile(image, destination), image_id)

    def test_failed_creation_cannot_emit_verified_receipt(self):
        root = self.base / "game"
        root.mkdir()
        store = ArchiveStore(self.base / "backups", run=lambda _: None)
        with self.assertRaisesRegex(Blocked, "BACKUP_SCOPE_INCOMPLETE"):
            store.create(root, ID, lambda *_: None, "sha256:" + "a" * 64)

    def test_frozen_target_requires_exact_hash_build_and_mod_inventory(self):
        payload = self.base / "target.tar"
        with tarfile.open(payload, "w") as tar:
            for name, data in {"ServerFiles/steamapps/appmanifest_294420.acf": b'"buildid" "25661908"',
                               "ServerFiles/Mods/Example/Example.dll": b"fixed-mod"}.items():
                member = tarfile.TarInfo(name)
                member.size = len(data)
                tar.addfile(member, io.BytesIO(data))
        mods = {"Example/Example.dll": hashlib.sha256(b"fixed-mod").hexdigest()}
        FrozenTarget(payload, digest(payload), "25661908", mods).verify()
        for sha, build, inventory, code in [
                ("0" * 64, "25661908", mods, "HASH_MISMATCH"),
                (digest(payload), "24994542", mods, "BUILD_MISMATCH"),
                (digest(payload), "25661908", {}, "INVENTORY_MISMATCH"),
                (digest(payload), "25661908", {"Example/Example.dll": "0" * 64}, "INVENTORY_MISMATCH")]:
            with self.subTest(code=code), self.assertRaisesRegex(Blocked, code):
                FrozenTarget(payload, sha, build, inventory).verify()

    @unittest.skipUnless(os.name == "posix", "GNU tar failure checks run on Linux CI")
    def test_compare_or_restore_failure_never_marks_verified(self):
        import shutil
        for failure in ["source-compare", "restore", "corrupt-image"]:
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                base = Path(tmp)
                root = base / "game"
                root.mkdir()
                for name in REQUIRED:
                    p = root / name
                    if name in REQUIRED[4:]: p.write_text("isolated")
                    else:
                        p.mkdir()
                        (p / "fixture").write_text("isolated")
                image = base / "image.tar"
                image_id = make_image(image, wrong_layer=failure == "corrupt-image")
                def run(args):
                    if ((failure == "source-compare" and "--compare" in args)
                            or (failure == "restore" and "-xpf" in args)):
                        raise Blocked("INJECTED_FAILURE")
                    ArchiveStore._run(args)
                store = ArchiveStore(base / "store", run=run)
                with self.assertRaises(Blocked):
                    store.create(root, ID, lambda dest, _: shutil.copyfile(image, dest), image_id)
                evidence = store.directory / ("backup_" + ID) / "evidence.json"
                if evidence.exists():
                    self.assertFalse(json.loads(evidence.read_text())["verified"])
                    self.assertNotIn("filesystemRestored", json.loads(evidence.read_text()))


if __name__ == "__main__":
    unittest.main()
