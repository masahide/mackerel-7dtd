import contextlib
import io
import json
import os
import socket
import subprocess
import unittest
from unittest.mock import patch
import xml.etree.ElementTree as ET

from runtime_freeze import CONTAINER
from runtime_rehearsal import MEMORY_PROBE, OfflineBootRehearsal, PROBE, TARGET
from suzume_update import Blocked
import test_runtime_freeze as fixtures


class TrialIdentityTest(unittest.TestCase):
    def test_live_wrong_label_or_published_container_can_never_be_mutated_as_trial(self):
        job, image = "a" * 32, "sha256:" + "b" * 64
        info = {"Id": "c" * 64, "Image": image, "Config": {"Labels": {"org.suzume.rehearsal": job}},
                "HostConfig": {"NetworkMode": "none", "PortBindings": None, "RestartPolicy": {"Name": "no"}}}
        class Runner:
            def run(self, args, timeout): return json.dumps([info])
        class Preparer:
            runner = Runner()
        trial = OfflineBootRehearsal(Preparer())
        self.assertEqual(trial.inspect_trial("fixed-name", job, image)[0], "c" * 64)
        for field, value in [("live", CONTAINER), ("label", "wrong"), ("network", "host"),
                             ("ports", {"26900/tcp": [{}]}), ("privileged", True), ("restart", "always")]:
            with self.subTest(field=field):
                original = json.loads(json.dumps(info))
                if field == "live": info["Id"] = value
                if field == "label": info["Config"]["Labels"]["org.suzume.rehearsal"] = value
                if field == "network": info["HostConfig"]["NetworkMode"] = value
                if field == "ports": info["HostConfig"]["PortBindings"] = value
                if field == "privileged": info["HostConfig"]["Privileged"] = value
                if field == "restart": info["HostConfig"]["RestartPolicy"]["Name"] = value
                with self.assertRaisesRegex(Blocked, "TRIAL_IDENTITY_MISMATCH"):
                    trial.inspect_trial("fixed-name", job, image)
                info.clear(); info.update(original)

    def test_probe_authentication_and_fragmented_reply_use_crlf_without_exposing_secret(self):
        password = "fixture-private-password"
        root = ET.fromstring('<ServerSettings><property name="TelnetEnabled" value="true"/><property name="TelnetPort" value="8081"/><property name="TelnetPassword" value="' + password + '"/></ServerSettings>')
        class FakeSocket:
            def __init__(self): self.replies, self.sent = [b"Please enter password:"], []
            def __enter__(self): return self
            def __exit__(self, *_): pass
            def settimeout(self, _): pass
            def recv(self, _):
                if self.replies: return self.replies.pop(0)
                raise socket.timeout()
            def sendall(self, data):
                self.sent.append(data)
                if data == password.encode() + b"\r\n": self.replies = [b"Connected to session.\r\n"]
                if data == b"version\r\n": self.replies = [TARGET[:30].encode(), TARGET[30:].encode() + b"\r\n"]
        fake, output = FakeSocket(), io.StringIO()
        with patch("socket.create_connection", return_value=fake), patch("xml.etree.ElementTree.parse", return_value=ET.ElementTree(root)), contextlib.redirect_stdout(output):
            with self.assertRaises(SystemExit) as exited: exec(compile(PROBE, "trusted-test-probe", "exec"), {})
        self.assertEqual(exited.exception.code, 0)
        self.assertEqual(json.loads(output.getvalue())["gameVersion"], TARGET)
        self.assertNotIn(password, output.getvalue())
        self.assertEqual(fake.sent, [password.encode() + b"\r\n", b"version\r\n"])


@unittest.skipUnless(os.name == "posix", "GNU tar copy rehearsal runs on Linux CI")
class OfflineRehearsalTest(unittest.TestCase):
    def setUp(self):
        self.fixture = fixtures.FreezeTest()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        f = self.fixture
        for name in ["7DaysToDie", "LGSM-Config", "log", "backups"]:
            (f.root / name).mkdir()
            (f.root / name / "fixture.txt").write_text("private fixture")
        (f.root / "ServerFiles/7DaysToDieServer.x86_64").write_bytes(b"fixture game executable")
        f.preparer.prepare(f.job)
        f.preparer.stage_recovery_copy(f.job)
        self.calls, self.status, self.probe_version = [], "running", TARGET
        self.mem_total, self.mem_available = 16 * 1024**3, 12 * 1024**3
        self.trial_id = "e" * 64
        original_run = f.runner.run
        def run(args, timeout=1800):
            self.calls.append(args)
            if args[:2] == ["docker", "info"]: return json.dumps({"MemTotal": self.mem_total, "NCPU": 8})
            if args[-1] == MEMORY_PROBE: return json.dumps({"memoryAvailable": self.mem_available})
            if args[:3] == ["docker", "run", "--detach"]: return self.trial_id
            if args[:2] == ["docker", "inspect"] and args[2] != CONTAINER:
                return json.dumps([{"Id": self.trial_id, "Image": f.runner.frozen_id,
                                   "Config": {"Labels": {"org.suzume.rehearsal": f.job}},
                                   "HostConfig": {"NetworkMode": "none", "RestartPolicy": {"Name": "no"}},
                                   "State": {"Status": self.status, "OOMKilled": False}}])
            if args[:3] == ["docker", "exec", self.trial_id]: return json.dumps({"gameVersion": self.probe_version})
            if args[:2] == ["docker", "stop"]:
                self.assertEqual(args[-1], self.trial_id)
                self.status = "exited"
                return ""
            if args[:3] == ["docker", "container", "rm"]:
                self.assertEqual(args[-1], self.trial_id)
                return ""
            if args[0] == "tar":
                subprocess.run(args, check=True, capture_output=True)
                return ""
            return original_run(args, timeout)
        f.runner.run = run
        self.trial = OfflineBootRehearsal(f.preparer)

    def test_success_only_uses_copies_no_ports_and_never_claims_world_backup(self):
        result = self.trial.run(self.fixture.job)
        self.assertTrue(result["offlineBootVerified"])
        self.assertTrue(result["trialRemoved"])
        for key in ["verified", "fullWorldBackup", "runtimeRestored", "productionEnabled"]: self.assertFalse(result[key])
        start = next(c for c in self.calls if c[:3] == ["docker", "run", "--detach"])
        self.assertIn("none", start)
        self.assertIn("6g", start)
        self.assertNotIn("--publish", start)
        self.assertNotIn("--privileged", start)
        for value in start:
            if value.startswith("type=bind,"): self.assertIn("adapter-preparation-" + self.fixture.job, value)
        self.assertNotIn("private fixture", json.dumps(result))

    def test_wrong_version_cleanup_cannot_stop_live_game(self):
        self.probe_version = "Game version: unknown"
        with self.assertRaisesRegex(Blocked, "TRIAL_VERSION_MISMATCH"): self.trial.run(self.fixture.job)
        self.assertEqual(self.status, "exited")
        self.assertTrue(any(c[:3] == ["docker", "container", "rm"] for c in self.calls))

    def test_insufficient_current_capacity_cannot_start_trial(self):
        self.mem_available = 3 * 1024**3
        with self.assertRaisesRegex(Blocked, "TRIAL_RESOURCE_CAPACITY_UNAVAILABLE"): self.trial.run(self.fixture.job)
        self.assertFalse(any(c[:3] == ["docker", "run", "--detach"] for c in self.calls))


if __name__ == "__main__": unittest.main()
