import contextlib
import io
import json
import unittest
from unittest.mock import patch

import receiver_maintenance as receiver


class ReceiverMaintenanceTest(unittest.TestCase):
    def invoke(self,action,status=None,launch_error=None):
        calls=[];output=io.StringIO()
        def remote(code,payload=None,arguments=(),timeout=180):
            if code==receiver.EXECUTE:
                calls.append('launch')
                if launch_error:raise RuntimeError(launch_error)
                return [{'phase':'worker_launched','pid':123}]
            if code==receiver.STATUS:
                calls.append('status');return [status]
            if code==receiver.STAGE:
                calls.append('stage')
                self.assertEqual(payload['probe'],{'user':'private-user','secret':'private-secret'})
                return [{'phase':'ready_for_limited_maintenance','verified':False}]
            self.fail('Unexpected fixed worker')
        def run(args,**kwargs):
            self.assertEqual(args,['systemctl','stop','apiserver7dtd.service']);calls.append('api-stop')
        with patch('sys.stdin',io.StringIO(json.dumps({'operation':action,'modules':{}}))),patch.object(receiver,'configuration',return_value={'user':'private-user','secret':'private-secret'}),patch.object(receiver,'remote',side_effect=remote),patch.object(receiver.subprocess,'run',side_effect=run),patch.object(receiver,'api_start',side_effect=lambda:calls.append('api-start')),contextlib.redirect_stdout(output):
            result=receiver.main()
        self.assertNotIn('private-secret',output.getvalue());self.assertNotIn('private-user',output.getvalue())
        return result,calls,output.getvalue()

    def test_stage_verifies_without_stopping_any_service(self):
        result,calls,_=self.invoke('stage')
        self.assertEqual(result,0);self.assertEqual(calls,['stage'])

    def test_api_resumes_only_after_game_and_cron_return(self):
        status={'phase':'completed','gameReturned':True,'recoveryRequired':False,'workerPresent':False}
        result,calls,_=self.invoke('collect',status)
        self.assertEqual(result,0);self.assertEqual(calls,['api-stop','launch','status','api-start'])

    def test_unknown_worker_or_failed_recovery_keeps_mutators_inhibited(self):
        status={'phase':'operator_recovery_required','recoveryRequired':True,'workerPresent':False}
        result,calls,out=self.invoke('collect',status)
        self.assertEqual(result,1);self.assertNotIn('api-start',calls)
        self.assertIn('OPERATOR_RECOVERY_REQUIRED_API_REMAINS_INHIBITED',out)

    def test_failed_preflight_before_claim_restores_api_without_game_mutation(self):
        status={'phase':'not_reserved','workerPresent':False,'startupErrorCodes':['VERSION_OR_PLAYERS_NOT_VERIFIED']}
        result,calls,out=self.invoke('collect',status)
        self.assertEqual(result,1);self.assertEqual(calls,['api-stop','launch','status','api-start'])
        self.assertIn('MAINTENANCE_PREFLIGHT_FAILED_NO_GAME_CHANGE',out)

    def test_lost_launch_reply_never_replays_or_resumes_mutators(self):
        result,calls,_=self.invoke('collect',launch_error='REMOTE_REPLY_LOST')
        self.assertEqual(result,1);self.assertEqual(calls,['api-stop','launch'])

    def test_explicit_recovery_does_not_issue_another_api_stop(self):
        status={'phase':'operator_recovered','gameReturned':True,'recoveryRequired':False,'workerPresent':False}
        result,calls,_=self.invoke('recover',status)
        self.assertEqual(result,0);self.assertEqual(calls,['launch','status','api-start'])


if __name__=='__main__':unittest.main()
