import contextlib
import io
import json
import os
from pathlib import Path
import socket
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import xml.etree.ElementTree as ET

from runtime_freeze import CONTAINER
from runtime_rehearsal import MEMORY_PROBE, OBSERVED_PLATFORM_LINES, OFFLINE_PLATFORM, OfflineBootRehearsal, PROBE, TARGET
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

    def test_platform_override_requires_exact_private_readonly_mount(self):
        job,image="a"*32,"sha256:"+"b"*64
        target='/home/sdtdserver/serverfiles/platform.cfg'
        mount={'Destination':target,'Type':'bind','Source':'/private/trial/platform.cfg','RW':False}
        info={'Id':'c'*64,'Image':image,'Config':{'Labels':{'org.suzume.rehearsal':job}},
              'HostConfig':{'NetworkMode':'none','PortBindings':None,'RestartPolicy':{'Name':'no'}},'Mounts':[mount]}
        runner=SimpleNamespace(run=lambda *_:json.dumps([info]));trial=OfflineBootRehearsal(SimpleNamespace(runner=runner))
        trial.expected_mounts={target:('bind',mount['Source'])};trial.readonly_targets={target}
        trial.inspect_trial('fixed',job,image)
        mount['RW']=True
        with self.assertRaisesRegex(Blocked,'TRIAL_PLATFORM_OVERRIDE_NOT_READONLY'):trial.inspect_trial('fixed',job,image)
        mount['RW']=False;info['Mounts'].append(dict(mount))
        with self.assertRaisesRegex(Blocked,'TRIAL_MOUNTS_NOT_ISOLATED'):trial.inspect_trial('fixed',job,image)

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
        (f.root / "ServerFiles/platform.cfg").write_text('\n'.join(OBSERVED_PLATFORM_LINES)+'\n')
        f.preparer.prepare(f.job)
        f.preparer.stage_recovery_copy(f.job)
        space = patch("runtime_rehearsal.shutil.disk_usage", return_value=SimpleNamespace(free=1024**4))
        space.start()
        self.addCleanup(space.stop)
        self.calls, self.status, self.probe_version = [], "running", TARGET
        self.mem_total, self.mem_available = 16 * 1024**3, 12 * 1024**3
        self.trial_id = "e" * 64
        self.start_failure = False
        self.change_online_world = False
        self.cleanup_failure = None
        self.logs_failure = False
        self.wrong_mount = False
        original_run = f.runner.run
        def run(args, timeout=1800):
            self.calls.append(args)
            if args[:2] == ["docker", "info"]: return json.dumps({"MemTotal": self.mem_total, "NCPU": 8})
            if args[-1] == MEMORY_PROBE: return json.dumps({"memoryAvailable": self.mem_available})
            if args[:2] == ["docker", "create"]:
                if self.start_failure: raise Blocked("MOCK_START_REPLY_LOST")
                return self.trial_id
            if args[:2] == ["docker", "start"]:
                self.status = "running"
                return self.trial_id
            if args[:2] == ["docker", "inspect"] and args[2] != CONTAINER:
                return json.dumps([{"Id": self.trial_id, "Image": f.runner.frozen_id,
                                   "Config": {"Labels": {"org.suzume.rehearsal": f.job}},
                                   "HostConfig": {"NetworkMode": "none", "RestartPolicy": {"Name": "no"}},
                                   "Mounts": [{"Destination": target, "Type": value[0], "Source": "/live" if self.wrong_mount else value[1], "RW":target not in getattr(self.trial,"readonly_targets",set())} for target, value in getattr(self.trial, "expected_mounts", {}).items()],
                                   "State": {"Status": self.status, "OOMKilled": False}}])
            if args[:3] == ["docker", "exec", self.trial_id] and args[-1] == PROBE: return json.dumps({"gameVersion": self.probe_version})
            if args[:2] == ["docker", "logs"]:
                if self.logs_failure: raise Blocked("MOCK_LOG_READ_FAILED")
                return "fixture-private-log"
            if args[:2] == ["docker", "stop"]:
                self.assertEqual(args[-1], self.trial_id)
                if self.cleanup_failure == "stop": raise Blocked("MOCK_STOP_REPLY_LOST")
                self.status = "exited"
                return ""
            if args[:3] == ["docker", "container", "rm"]:
                self.assertEqual(args[-1], self.trial_id)
                if self.cleanup_failure == "remove": raise Blocked("MOCK_REMOVE_REPLY_LOST")
                return ""
            if args[0] == "tar":
                if (self.change_online_world and "--compare" in args and args[-1] == str(f.root)
                        and any(value.endswith("online-world-config.tar") for value in args)):
                    (f.root / "7DaysToDie/fixture.txt").write_text("world changed while zero players")
                if subprocess.run(args, capture_output=True).returncode:
                    raise Blocked("PREPARATION_COMMAND_FAILED")
                return ""
            return original_run(args, timeout)
        f.runner.run = run
        self.trial = OfflineBootRehearsal(f.preparer)

    def test_success_only_uses_copies_no_ports_and_never_claims_world_backup(self):
        result = self.trial.run(self.fixture.job)
        self.assertTrue(result["offlineBootVerified"])
        self.assertTrue(result["trialRemoved"])
        self.assertFalse(result["cleanupRequired"])
        self.assertTrue(result["privateLogsSaved"])
        for key in ["verified", "fullWorldBackup", "runtimeRestored", "productionEnabled"]: self.assertFalse(result[key])
        start = next(c for c in self.calls if c[:2] == ["docker", "create"])
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
        self.assertFalse(any(c[:2] == ["docker", "create"] for c in self.calls))

    def receipt(self):
        folder = self.fixture.root / "upgrade-backups" / ("adapter-preparation-" + self.fixture.job) / "offline-runtime-trial"
        return json.loads((folder / "receipt.json").read_text())

    def test_online_world_change_stops_before_boot_and_preserves_failure_phase(self):
        self.change_online_world = True
        with self.assertRaisesRegex(Blocked, "ONLINE_COPY_NOT_MATCHED"): self.trial.run(self.fixture.job)
        self.assertFalse(any(c[:2] == ["docker", "create"] for c in self.calls))
        result = self.receipt()
        self.assertEqual(result["failedPhase"], "comparing_online_source")
        self.assertFalse(result["startAttempted"])
        self.assertFalse(result["cleanupRequired"])
        self.assertFalse(result["verified"])

    def test_fixed_start_on_unverified_snapshot_cannot_claim_world_recovery(self):
        self.change_online_world = True
        with self.assertRaisesRegex(Blocked, "ONLINE_COPY_NOT_MATCHED"): self.trial.run(self.fixture.job)
        self.change_online_world = False
        with patch.object(self.trial, "resolve_trial_image", return_value=self.fixture.runner.frozen_id):
            result = self.trial.run(self.fixture.job, mode="check-start")
        self.assertTrue(result["offlineBootVerified"])
        self.assertEqual(result["purpose"], "fixed_start_only")
        self.assertFalse(result["onlineSourceCompared"])
        self.assertFalse(result["sourceQuiescent"])
        self.assertFalse(result["verified"])
        self.assertFalse(result["runtimeRestored"])
        self.assertEqual(result['platformMode'],'LOCAL_LAN_without_EOS')
        cfg=self.fixture.root/'upgrade-backups'/('adapter-preparation-'+self.fixture.job)/'fixed-start-readiness-trial/platform.cfg'
        self.assertEqual(cfg.read_bytes(),OFFLINE_PLATFORM)
        fixed=self.fixture.root/'upgrade-backups'/('adapter-preparation-'+self.fixture.job)/'restore-target-copy/ServerFiles/platform.cfg'
        self.assertEqual(fixed.read_text().splitlines(),OBSERVED_PLATFORM_LINES)
        self.assertTrue(result["trialRemoved"])
        command = ["docker", "exec", "--user", "sdtdserver", "--workdir", "/home/sdtdserver", self.trial_id, "./sdtdserver", "start"]
        self.assertIn(command, self.calls)
        start = next(c for c in self.calls if c[:2] == ["docker", "create"])
        self.assertIn("/bin/sleep", start)
        self.assertIn("infinity", start)
        self.assertNotIn(CONTAINER, start)

    def test_unknown_platform_configuration_cannot_be_overridden_or_started(self):
        source=self.fixture.root/'ServerFiles/platform.cfg'
        source.write_text('platform=unknown\ncrossplatform=EOS\n')
        trial_root=self.fixture.root/'new-platform-trial';trial_root.mkdir()
        with self.assertRaisesRegex(Blocked,'PLATFORM_CONFIGURATION_NOT_OBSERVED'):
            self.trial.offline_platform_copy(source.parent,trial_root)
        self.assertFalse((trial_root/'platform.cfg').exists())

    def test_offline_platform_cannot_overwrite_existing_copy(self):
        source=self.fixture.root/'ServerFiles/platform.cfg'
        original=source.read_bytes();trial_root=self.fixture.root/'new-platform-trial';trial_root.mkdir()
        self.trial.offline_platform_copy(source.parent,trial_root)
        (trial_root/'platform.cfg').write_text('retained evidence')
        with self.assertRaises(FileExistsError):self.trial.offline_platform_copy(source.parent,trial_root)
        self.assertEqual((trial_root/'platform.cfg').read_text(),'retained evidence')
        self.assertEqual(source.read_bytes(),original)

    def test_wrong_mount_is_rejected_before_container_start(self):
        self.wrong_mount = True
        with self.assertRaisesRegex(Blocked, "TRIAL_MOUNTS_NOT_ISOLATED"): self.trial.run(self.fixture.job)
        self.assertFalse(any(c[:2] == ["docker", "start"] for c in self.calls))
        self.assertFalse(any(c[:2] == ["docker", "stop"] for c in self.calls))
        self.assertTrue(self.receipt()["cleanupRequired"])

    def assert_cleanup_failure(self, failure):
        self.cleanup_failure = failure
        with self.assertRaisesRegex(Blocked, "TRIAL_CLEANUP_FAILED"): self.trial.run(self.fixture.job)
        result = self.receipt()
        self.assertTrue(result["offlineBootVerified"])
        self.assertTrue(result["cleanupRequired"])
        self.assertIn("cleanupErrorCode", result)
        self.assertEqual(result["phase"], "failed")
        for call in self.calls:
            if call[:2] == ["docker", "stop"] or call[:3] == ["docker", "container", "rm"]:
                self.assertNotEqual(call[-1], CONTAINER)

    def test_failed_stop_is_durable_and_never_targets_live(self):
        self.assert_cleanup_failure("stop")

    def test_failed_removal_is_durable_and_never_targets_live(self):
        self.assert_cleanup_failure("remove")

    def test_log_failure_does_not_prevent_trial_cleanup(self):
        self.logs_failure = True
        result = self.trial.run(self.fixture.job)
        self.assertFalse(result["privateLogsSaved"])
        self.assertTrue(result["trialRemoved"])
        self.assertFalse(result["cleanupRequired"])

    def test_original_failure_is_retained_when_cleanup_also_fails(self):
        self.probe_version = "Game version: unknown"
        self.cleanup_failure = "stop"
        with self.assertRaisesRegex(Blocked, "TRIAL_CLEANUP_FAILED"): self.trial.run(self.fixture.job)
        result = self.receipt()
        self.assertEqual(result["errorCode"], "TRIAL_VERSION_MISMATCH")
        self.assertEqual(result["cleanupErrorCode"], "MOCK_STOP_REPLY_LOST")
        self.assertTrue(result["cleanupRequired"])

    def test_lost_start_reply_cleans_up_only_identified_trial(self):
        self.start_failure = True
        with self.assertRaisesRegex(Blocked, "MOCK_START_REPLY_LOST"): self.trial.run(self.fixture.job)
        self.assertEqual(self.status, "exited")
        self.assertTrue(any(c[:3] == ["docker", "container", "rm"] for c in self.calls))

    def test_ambiguous_live_identity_retains_recovery_without_stop(self):
        self.trial_id = CONTAINER
        with self.assertRaisesRegex(Blocked, "TRIAL_IDENTITY_MISMATCH"): self.trial.run(self.fixture.job)
        self.assertFalse(any(c[:2] == ["docker", "stop"] for c in self.calls))
        folder = self.fixture.root / "upgrade-backups" / ("adapter-preparation-" + self.fixture.job) / "offline-runtime-trial"
        self.assertTrue(json.loads((folder / "receipt.json").read_text())["cleanupRequired"])


if __name__ == "__main__": unittest.main()
