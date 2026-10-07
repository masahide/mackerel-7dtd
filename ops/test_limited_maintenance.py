from contextlib import nullcontext
import copy
import contextlib
import io
import json
import socket
import unittest
from unittest.mock import patch
import xml.etree.ElementTree as ET

from limited_maintenance import LimitedMaintenance, SHUTDOWN, TARGET, safe_result
from suzume_update import Blocked


class MemoryReservation:
    def __init__(self):self.state=None;self.history=[];self.completed=None
    def locked(self):return nullcontext()
    def require_free(self):
        if self.state:raise Blocked('MAINTENANCE_RESERVED')
    def claim(self, job, target):
        self.require_free();self.state={'jobId':job,'targetVersion':target,'recoveryRequired':True};return self.state
    def save(self, state, phase):
        state['phase']=phase;self.state=copy.deepcopy(state);self.history.append(copy.deepcopy(state))
    def read(self):return copy.deepcopy(self.state)
    def release(self, state):self.completed=copy.deepcopy(state);self.state=None


class Backend:
    def __init__(self):self.calls=[];self.failure=None;self.deny_shutdown=False;self.latest_nonzero=False
    def call(self, name):
        self.calls.append(name)
        if self.failure==name:raise Blocked('MOCK_'+name.upper()+'_FAILED')
    def release_lock(self):return nullcontext()
    def preflight(self):self.call('preflight');return {'cron':[{'pid':48,'startToken':'123','state':'S'}]}
    def pause_cron(self, state):self.call('pause_cron')
    def inspect_pane(self):self.call('inspect_pane');return {'paneId':'%0','pid':234,'originalRemainOnExit':'off'}
    def arm(self, pane):self.call('arm')
    def probe(self, zero=False):
        self.call('probe')
        if zero and self.latest_nonzero:raise Blocked('PLAYERS_NOT_VERIFIED_ZERO')
        return {'gameVersion':TARGET,'onlinePlayers':0}
    def shutdown(self):
        self.call('shutdown')
        return {'shutdownSent':False,'errorCode':'PLAYERS_NOT_VERIFIED_ZERO'} if self.deny_shutdown else {'shutdownSent':True,'telnetEOF':True}
    def wait_clean_exit(self, state):self.call('wait_clean_exit');return {'gameExitCode':0,'writerAbsenceVerified':True}
    def copy(self):self.call('copy');return {'quiescentCopySha256':'a'*64,'sourceQuiescent':True,'verified':False,'fullWorldBackup':False}
    def recover(self, state):self.call('recover');return {'gameVersion':TARGET,'onlinePlayers':0}
    def resume_cron(self, state):self.call('resume_cron')


class LimitedMaintenanceTest(unittest.TestCase):
    def setUp(self):
        self.reservation=MemoryReservation();self.backend=Backend();self.job='a'*32
        self.workflow=LimitedMaintenance(self.reservation,self.backend)

    def test_success_returns_game_and_cron_before_releasing_reservation(self):
        result=self.workflow.run(self.job)
        self.assertEqual(result['phase'],'completed');self.assertFalse(result['recoveryRequired'])
        self.assertTrue(result['gameReturned']);self.assertFalse(result['verified'])
        self.assertFalse(result['fullWorldBackup']);self.assertFalse(result['runtimeRestored'])
        self.assertLess(self.backend.calls.index('recover'),self.backend.calls.index('resume_cron'))
        pending=next(s for s in self.reservation.history if s['phase']=='arming_exit_observation')
        self.assertEqual(pending['pane']['originalRemainOnExit'],'off')
        self.assertTrue(pending['cronPauseAttempted'])
        self.assertIsNone(self.reservation.state)

    def test_latest_nonzero_or_console_race_never_copies_world(self):
        for mode in ['latest','console']:
            with self.subTest(mode=mode):
                backend=Backend();backend.latest_nonzero=mode=='latest';backend.deny_shutdown=mode=='console'
                reservation=MemoryReservation();result=LimitedMaintenance(reservation,backend).run(self.job)
                self.assertNotIn('copy',backend.calls);self.assertFalse(result['shutdownAttempted'])
                if mode=='latest':self.assertNotIn('shutdown',backend.calls)
                self.assertEqual(result['errorCode'],'PLAYERS_NOT_VERIFIED_ZERO')
                self.assertTrue(result['gameReturned']);self.assertFalse(result['recoveryRequired'])

    def test_each_failure_keeps_exact_phase_and_attempts_original_return(self):
        for stage in ['pause_cron','inspect_pane','arm','shutdown','wait_clean_exit','copy']:
            with self.subTest(stage=stage):
                backend=Backend();backend.failure=stage;reservation=MemoryReservation()
                result=LimitedMaintenance(reservation,backend).run(self.job)
                self.assertEqual(result['errorCode'],'MOCK_'+stage.upper()+'_FAILED')
                self.assertIn('failedPhase',result);self.assertTrue(result['gameReturned'])
                self.assertFalse(result['recoveryRequired']);self.assertIn('resume_cron',backend.calls)
                if stage!='copy':self.assertNotIn('copy',backend.calls)

    def test_failed_return_or_cron_resume_retains_reservation_and_blocks_duplicate(self):
        for stage in ['recover','resume_cron']:
            with self.subTest(stage=stage):
                backend=Backend();backend.failure=stage;reservation=MemoryReservation()
                workflow=LimitedMaintenance(reservation,backend);result=workflow.run(self.job)
                self.assertTrue(result['recoveryRequired']);self.assertEqual(result['phase'],'operator_recovery_required')
                self.assertIsNotNone(reservation.state)
                before=list(backend.calls)
                with self.assertRaisesRegex(Blocked,'MAINTENANCE_RESERVED'):workflow.run('b'*32)
                self.assertEqual(backend.calls,before)
                backend.failure=None
                recovered=workflow.recover(self.job)
                self.assertFalse(recovered['recoveryRequired']);self.assertIsNone(reservation.state)

    def test_unknown_preflight_never_claims_or_mutates(self):
        self.backend.failure='preflight'
        with self.assertRaises(Blocked):self.workflow.run(self.job)
        self.assertEqual(self.backend.calls,['preflight']);self.assertIsNone(self.reservation.state)

    def test_recovery_wrong_owner_cannot_start_or_clear_and_public_result_omits_private_state(self):
        self.reservation.state={'jobId':'b'*32,'phase':'stopping','recoveryRequired':True}
        with self.assertRaisesRegex(Blocked,'OWNER_MISMATCH'):self.workflow.recover(self.job)
        self.assertEqual(self.backend.calls,[])
        self.assertNotIn('cron',safe_result({'cron':{'pid':48},'probePassword':'private','verified':False}))
        self.assertNotIn('probePassword',safe_result({'probePassword':'private','verified':False}))

    def test_console_rechecks_players_and_does_not_send_shutdown_when_nonzero(self):
        password='fixture-private-auth'
        root=ET.fromstring('<ServerSettings><property name="TelnetEnabled" value="true"/><property name="TelnetPort" value="8081"/><property name="TelnetPassword" value="'+password+'"/></ServerSettings>')
        class FakeSocket:
            def __init__(self, count):self.count=count;self.replies=[b'password:'];self.sent=[]
            def __enter__(self):return self
            def __exit__(self,*_):pass
            def settimeout(self,_):pass
            def recv(self,_):return self.replies.pop(0) if self.replies else b''
            def sendall(self,data):
                self.sent.append(data)
                if data==password.encode()+b'\r\n':self.replies=[b'Connected to session.\r\n']
                elif data==b'version\r\n':self.replies=[TARGET.encode()+b'\r\n']
                elif data==b'lp\r\n':self.replies=[('Total of '+str(self.count)+' in the game\r\n').encode()]
        for count in [0,1]:
            with self.subTest(count=count):
                fake=FakeSocket(count);output=io.StringIO()
                with patch('socket.create_connection',return_value=fake),patch('xml.etree.ElementTree.parse',return_value=ET.ElementTree(root)),contextlib.redirect_stdout(output):
                    exec(compile(SHUTDOWN,'fixed-test-shutdown','exec'),{})
                result=json.loads(output.getvalue())
                self.assertEqual(result['shutdownSent'],count==0)
                self.assertEqual(b'shutdown\r\n' in fake.sent,count==0)
                self.assertNotIn(password,output.getvalue())


if __name__=='__main__':unittest.main()
