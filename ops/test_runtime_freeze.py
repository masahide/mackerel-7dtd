from contextlib import nullcontext
import hashlib
import json
from pathlib import Path
import shutil
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import runtime_freeze as freeze
from suzume_update import Blocked
from test_suzume_update import make_image


class FixtureRunner:
    def __init__(self, root, image, frozen_id):
        self.root, self.image, self.frozen_id = root, image, frozen_id
        self.calls = []
        self.static_changed = False
        self.change_mod = False
        self.image_env = dict(freeze.FIXED_ENV)
        destinations = {"ServerFiles": "/home/sdtdserver/serverfiles", "7DaysToDie": "/home/sdtdserver/.local/share/7DaysToDie",
                        "LGSM-Config": "/home/sdtdserver/lgsm/config-lgsm/sdtdserver", "log": "/home/sdtdserver/log",
                        "backups": "/home/sdtdserver/lgsm/backup"}
        self.ins = {"Image": freeze.IMAGE, "State": {"Status": "running"},
                    "Config": {"Entrypoint": ["/home/sdtdserver/openvpn.sh"], "Env": ["API_SECRET=fixture-private"]},
                    "Mounts": [{"Destination": dest, "Type": "bind", "Source": str(root / name)} for name, dest in destinations.items()]}

    def run(self, args, timeout=1800):
        self.calls.append(args)
        if args[:2] == ["docker", "compose"]: return freeze.CONTAINER
        if args[:2] == ["docker", "inspect"]: return json.dumps([self.ins])
        if args[:2] == ["docker", "commit"]: return self.frozen_id
        if args[:3] == ["docker", "image", "inspect"]:
            return json.dumps([{"Config": {"Env": [k + "=" + v for k, v in self.image_env.items()]}}])
        if args[:3] == ["docker", "image", "save"]:
            shutil.copyfile(self.image, args[args.index("--output") + 1])
            return ""
        if args[:2] in [["docker", "exec"], ["docker", "run"]]:
            if args[-1] == "command -v python3": return "/usr/bin/python3"
            changed = self.static_changed and args[:2] == ["docker", "run"]
            return json.dumps({"/home/sdtdserver/user.sh": hashlib.sha256(b"changed" if changed else b"fixture").hexdigest()})
        if args[0] == "tar":
            if "-cpf" in args:
                path = Path(args[args.index("-cpf") + 1])
                with tarfile.open(path, "w") as tar: tar.add(self.root / "ServerFiles", arcname="ServerFiles")
            elif self.change_mod:
                (self.root / "ServerFiles/Mods/TrailwatchBridge/Fixture.dll").write_bytes(b"changed after freeze")
            return ""
        raise AssertionError("Unexpected fixture command")


class FreezeTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.root = self.base / "game"
        self.root.mkdir()
        (self.root / "upgrade-backups").mkdir()
        cf = self.root / "docker-compose.yml"
        cf.write_text("fixture compose")
        self.compose_patch = patch.object(freeze, "COMPOSE", hashlib.sha256(cf.read_bytes()).hexdigest())
        self.compose_patch.start()
        self.addCleanup(self.compose_patch.stop)
        server = self.root / "ServerFiles"
        (server / "steamapps").mkdir(parents=True)
        (server / "steamapps/appmanifest_294420.acf").write_text('"buildid" "25661908"')
        mods = server / "Mods/TrailwatchBridge"
        mods.mkdir(parents=True)
        (mods / "Fixture.dll").write_bytes(b"fixture")
        (mods / "TrailwatchBridge.dll.release.lock").write_bytes(b"")
        image = self.base / "image.tar"
        frozen_id = make_image(image)
        self.runner = FixtureRunner(self.root, image, frozen_id)
        self.preparer = freeze.RuntimePreparer(self.runner, self.root)
        self.preparer.release_lock = lambda: nullcontext()
        self.job = "1234567890abcdef1234567890abcdef"

    def test_freeze_never_stops_starts_downloads_or_enables_production(self):
        result = self.preparer.prepare(self.job)
        self.assertEqual(result["phase"], "prepared")
        self.assertFalse(result["verified"])
        self.assertFalse(result["runtimeRestored"])
        self.assertFalse(result["productionEnabled"])
        commands = self.runner.calls
        self.assertFalse(any("stop" in c or "start" in c or "pull" in c or "steamcmd" in c for c in commands))
        commit = next(c for c in commands if c[:2] == ["docker", "commit"])
        self.assertIn("--pause=false", commit)
        inspector = next(c for c in commands if c[:2] == ["docker", "run"])
        self.assertEqual(inspector[inspector.index("--network") + 1], "none")
        self.assertIn("--read-only", inspector)
        self.assertNotIn("--publish", inspector)
        self.assertNotIn("--privileged", inspector)
        self.assertNotIn("fixture-private", json.dumps(result))

    def test_uncertain_or_completed_attempt_never_reexecutes(self):
        self.preparer.prepare(self.job)
        count = len(self.runner.calls)
        with self.assertRaisesRegex(Blocked, "INSPECT_STATE_FIRST"):
            self.preparer.prepare(self.job)
        self.assertEqual(count, len(self.runner.calls))

    def test_changed_runtime_or_start_flags_prevent_prepared_receipt(self):
        for kind in ["static", "env", "mods"]:
            with self.subTest(kind=kind):
                self.runner.static_changed = kind == "static"
                self.runner.change_mod = kind == "mods"
                self.runner.image_env = dict(freeze.FIXED_ENV)
                if kind == "env": self.runner.image_env["START_MODE"] = "3"
                job = hashlib.md5(kind.encode(), usedforsecurity=False).hexdigest()
                with self.assertRaises(Blocked): self.preparer.prepare(job)
                state = json.loads((self.root / "upgrade-backups" / ("adapter-preparation-" + job) / "state.json").read_text())
                self.assertEqual(state["phase"], "failed")
                self.assertFalse(state["verified"])

    def test_changed_image_mount_or_nonrunning_state_prevent_commit(self):
        for kind in ["image", "mount", "state"]:
            with self.subTest(kind=kind):
                original = json.loads(json.dumps(self.runner.ins))
                if kind == "image": self.runner.ins["Image"] = "sha256:" + "f" * 64
                if kind == "mount": self.runner.ins["Mounts"][0]["Source"] = "/wrong/path"
                if kind == "state": self.runner.ins["State"]["Status"] = "exited"
                before = len(self.runner.calls)
                job = hashlib.md5(kind.encode(), usedforsecurity=False).hexdigest()
                with self.assertRaises(Blocked): self.preparer.prepare(job)
                self.assertFalse(any(c[:2] == ["docker", "commit"] for c in self.runner.calls[before:]))
                self.runner.ins = original

    def test_request_cannot_choose_path_command_or_target(self):
        for value in ["../escape", "; stop", "latest", None]:
            with self.subTest(value=value), self.assertRaisesRegex(Blocked, "INVALID_PREPARATION_ID"):
                self.preparer.prepare(value)
        self.assertEqual(self.runner.calls, [])


if __name__ == "__main__": unittest.main()
