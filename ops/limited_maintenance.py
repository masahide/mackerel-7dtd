"""One attended, fixed-version stop/copy/return; never an update API hook.

The existing container/VPN and live files are kept. No force kill, Docker restart,
SteamCMD, download, network rule, payload application or verified backup receipt.
Installed privately before use; recover uses the durable reservation after a crash.
"""
from contextlib import nullcontext
import json
import os
from pathlib import Path
import re
import sys
import time
import urllib.request

from runtime_freeze import CONTAINER, DockerRunner, RuntimePreparer, mod_inventory
from runtime_rehearsal import PROBE, TARGET
from suzume_update import Blocked, JOB, Reservation, archive_members, atomic_json, digest

CONFIG_HASHES = {
    "_default.cfg": "b15d00e9974da327f1d947297c8fca59ed8caa2a369cc48a58c8e57643f5d2ec",
    "common.cfg": "bb36108ed4ac465616e150e6656d006065b277d1e963074b92e931877f4de0aa",
    "sdtdserver.cfg": "7e2dc14d7bb7166da1ad9bc4705e3a027eabd7f01dc7237b69525eabf9fe26fa",
    "secrets-common.cfg": "b8645630306ae6a79839e82d96d40b54a3ef8cb12d3575d9f9bf387418b79225",
    "secrets-sdtdserver.cfg": "648eebade1c9c80cf8b5461a10528d415a60ea79056c6dfd401eda4789a17616",
}
CRONTAB_SHA = "64b42edbd5f1ef1f339c5684754b3d61c792cdc48b665e154b127ccca6214d9a"

CONTAINER_CONTROL = r'''
import hashlib,json,os,pathlib,re,signal,subprocess,sys
action=sys.argv[1];expected=json.loads(sys.argv[2])
base=pathlib.Path('/home/sdtdserver')
def scan():
 out={'game':[],'cron':[],'vpn':[],'conflicts':[]}
 for p in pathlib.Path('/proc').iterdir():
  if not p.name.isdigit():continue
  try:
   comm=(p/'comm').read_text().strip();stat=(p/'stat').read_text().rsplit(')',1)[1].split()
   raw=(p/'cmdline').read_bytes().replace(b'\0',b' ')
   obj={'pid':int(p.name),'startToken':stat[19],'state':stat[0]}
   if comm.startswith('7DaysToDie'):out['game'].append(obj)
   elif comm=='cron':out['cron'].append(obj)
   elif comm=='openvpn':out['vpn'].append(obj)
   elif ('steamcmd' in comm or (comm in ['bash','sh','tar','gzip'] and b'sdtdserver' in raw and any(word in raw for word in [b' monitor',b' backup',b' update',b' start',b' stop']))):out['conflicts'].append(obj)
  except FileNotFoundError:continue
  except (OSError,ValueError,IndexError):raise SystemExit(1)
 return out
def tmux(args):
 uid=(base/'lgsm/data/sdtdserver.uid').read_text().strip()
 if not re.fullmatch('[0-9a-f]{8}',uid):raise SystemExit(1)
 command=['/usr/bin/tmux','-L','sdtdserver-'+uid]+args
 p=subprocess.run(command,capture_output=True,text=True,timeout=10)
 if p.returncode:raise SystemExit(1)
 return p.stdout.strip()
def pane():
 text=tmux(['list-panes','-t','sdtdserver','-F','#{pane_id} #{pane_pid} #{pane_dead} #{pane_dead_status}'])
 lines=text.splitlines()
 if len(lines)!=1:raise SystemExit(1)
 parts=lines[0].split()
 if len(parts) not in [3,4] or not re.fullmatch('%[0-9]+',parts[0]):raise SystemExit(1)
 return {'paneId':parts[0],'pid':int(parts[1]),'dead':int(parts[2]),'exitCode':int(parts[3]) if len(parts)==4 else None}
current=scan()
if action=='inspect':
 current['crontabSha256']=hashlib.sha256(pathlib.Path('/var/spool/cron/crontabs/sdtdserver').read_bytes()).hexdigest()
 print(json.dumps(current))
elif action in ['pause-cron','resume-cron']:
 cron=current['cron']
 if len(cron)!=1 or any(cron[0][k]!=expected[k] for k in ['pid','startToken']):raise SystemExit(1)
 if action=='pause-cron':
  if current['conflicts'] or cron[0]['state'] not in ['S','R']:raise SystemExit(1)
  os.kill(expected['pid'],signal.SIGSTOP)
 else:
  if cron[0]['state'] in ['T','t']:os.kill(expected['pid'],signal.SIGCONT)
 print(json.dumps({'done':True}))
elif action in ['inspect-pane','arm-pane']:
 p=pane()
 if p['dead'] or len(current['game'])!=1 or p['pid']!=current['game'][0]['pid']:raise SystemExit(1)
 original=tmux(['show-options','-v','-t','sdtdserver','remain-on-exit'])
 if original not in ['on','off']:raise SystemExit(1)
 if action=='arm-pane':
  if p['paneId']!=expected['paneId'] or p['pid']!=expected['pid'] or original!=expected['originalRemainOnExit']:raise SystemExit(1)
  tmux(['set-option','-t','sdtdserver','remain-on-exit','on'])
 print(json.dumps({**p,'originalRemainOnExit':original}))
elif action in ['pane-status','disarm-pane','remove-dead-pane']:
 p=pane()
 if p['paneId']!=expected['paneId'] or p['pid']!=expected['pid']:raise SystemExit(1)
 if action=='disarm-pane':
  original=expected['originalRemainOnExit']
  if original not in ['on','off']:raise SystemExit(1)
  tmux(['set-option','-t','sdtdserver','remain-on-exit',original])
 elif action=='remove-dead-pane':
  if current['game'] or p['dead']!=1:raise SystemExit(1)
  tmux(['kill-session','-t','sdtdserver']) # Only a proven dead terminal; no game is killed.
 print(json.dumps(p))
else:raise SystemExit(1)
'''

# Authentication stays inside the original container. Only fixed version/lp/
# shutdown commands are sent. Raw replies/passwords never reach stdout.
SHUTDOWN = r'''
import json,re,socket,time,xml.etree.ElementTree as ET
properties={p.attrib.get('name'):p.attrib.get('value') for p in ET.parse('/home/sdtdserver/serverfiles/sdtdserver.xml').getroot().iter('property')}
password=properties.get('TelnetPassword') or ''
if properties.get('TelnetEnabled')!='true' or properties.get('TelnetPort')!='8081' or '\r' in password or '\n' in password:raise SystemExit(1)
def receive(sock,pattern,seconds=10):
 data=b'';end=time.monotonic()+seconds
 while time.monotonic()<end:
  try:chunk=sock.recv(8192)
  except socket.timeout:continue
  if not chunk:raise SystemExit(1)
  data+=chunk
  if len(data)>65536:raise SystemExit(1)
  text=data.decode('utf-8','replace')
  if re.search(pattern,text,re.I):return text
 raise SystemExit(1)
sent=False
def main():
 global sent
 with socket.create_connection(('127.0.0.1',8081),timeout=3) as sock:
  sock.settimeout(2)
  receive(sock,'password:' if password else r'session\.')
  if password:
   sock.sendall(password.encode()+b'\r\n');receive(sock,r'session\.')
  sock.sendall(b'version\r\n')
  text=receive(sock,r'Game version:[^\r\n]*[\r\n]')
  lines=re.findall(r'Game version:[^\r\n]*[\r\n]',text)
  if len(lines)!=1 or ' '.join(lines[0].split())!='Game version: V 3.3.0 (b18) Compatibility Version: V 3.3.0':raise SystemExit(1)
  sock.sendall(b'lp\r\n')
  text=receive(sock,r'Total of [0-9]+ in the game[\r\n]')
  counts=re.findall(r'Total of ([0-9]+) in the game[\r\n]',text)
  if counts!=['0']:return {'shutdownSent':False,'errorCode':'PLAYERS_NOT_VERIFIED_ZERO'}
  sent=True # Treat even a lost send reply as possibly issued, never replay it.
  sock.sendall(b'shutdown\r\n')
  end=time.monotonic()+90;eof=False
  while time.monotonic()<end:
   try:chunk=sock.recv(8192)
   except socket.timeout:continue
   if not chunk:eof=True;break
  return {'shutdownSent':True,'telnetEOF':eof}
try:result=main()
except (Exception,SystemExit):result={'shutdownSent':sent,'errorCode':'SHUTDOWN_REPLY_NOT_VERIFIED'}
print(json.dumps(result))
'''


class LiveBackend:
    def __init__(self, preparer, preparation_id):
        if not isinstance(preparation_id, str) or not JOB.fullmatch(preparation_id):
            raise Blocked("INVALID_PREPARATION_ID")
        self.p, self.runner, self.job = preparer, preparer.runner, preparation_id
        self.folder = preparer.root / "upgrade-backups" / ("adapter-preparation-" + preparation_id)
        self.state = json.loads((self.folder / "state.json").read_text())
        self.directory = self.folder / "limited-maintenance"

    def control(self, action, expected=None):
        user = "sdtdserver" if "pane" in action else "root"
        args = ["docker", "exec", "--user", user, CONTAINER, "/usr/bin/python3", "-c", CONTAINER_CONTROL, action, json.dumps(expected or {})]
        return json.loads(self.runner.run(args, 30))

    def require_assets(self):
        self.p.inspect_live()
        if self.p.checkpoint() != self.state["staticRuntimeHashes"] or mod_inventory(self.p.root) != self.state["approvedModHashes"]:
            raise Blocked("FIXED_RUNTIME_CHANGED")
        config = self.p.root / "LGSM-Config"
        if any((config / name).is_symlink() or digest(config / name) != value for name, value in CONFIG_HASHES.items()):
            raise Blocked("GSM_CONFIGURATION_CHANGED")
        saved = self.folder / "restore-target-copy/ServerFiles"
        for name in ["7DaysToDieServer.x86_64", "sdtdserver.xml", "platform.cfg"]:
            if digest(saved / name) != digest(self.p.root / "ServerFiles" / name):
                raise Blocked("FIXED_GAME_OR_XML_CHANGED")

    def probe(self, zero=False):
        private = self.directory / "probe.private.json"
        if private.is_symlink() or private.stat().st_mode & 0o077:
            raise Blocked("PRIVATE_PROBE_CONFIGURATION_UNAVAILABLE")
        auth = json.loads(private.read_text())
        def api(path, body=None):
            headers = {'X-SDTD-API-TOKENNAME': auth['user'], 'X-SDTD-API-SECRET': auth['secret']}
            if body is not None:headers['Content-Type']='application/json'
            request=urllib.request.Request('http://127.0.0.1:8080/api'+path,headers=headers,
                data=json.dumps(body).encode() if body is not None else None)
            with urllib.request.urlopen(request, timeout=10) as response:return json.load(response)['data']
        try:
            version=api('/command',{'command':'version'})['result']
            lines=[' '.join(line.split()) for line in version.splitlines() if line.strip().startswith('Game version:')]
            count=api('/serverstats')['players'];players=api('/player')['players']
            if (lines != [TARGET] or type(count) is not int or count < 0 or not isinstance(players,list)
                    or any(not isinstance(p,dict) or type(p.get('online')) is not bool for p in players)
                    or sum(p['online'] for p in players)!=count or (zero and count != 0)):
                raise Blocked('VERSION_OR_PLAYERS_NOT_VERIFIED')
            return {'gameVersion':TARGET,'onlinePlayers':count}
        except Blocked:raise
        except Exception:raise Blocked('LOCAL_API_PROBE_FAILED') from None

    def preflight(self):
        self.require_assets()
        proof=json.loads((self.folder / 'fixed-start-readiness-trial/receipt.json').read_text())
        if (proof.get('purpose')!='fixed_start_only' or proof.get('offlineBootVerified') is not True
                or proof.get('trialRemoved') is not True or proof.get('gameVersion')!=TARGET):
            raise Blocked('FIXED_START_NOT_REHEARSED')
        result=self.control('inspect')
        if (len(result['game'])!=1 or len(result['cron'])!=1 or len(result['vpn'])!=1
                or result['conflicts'] or result['crontabSha256']!=CRONTAB_SHA):
            raise Blocked('EXISTING_OPERATIONS_NOT_QUIESCENT')
        self.probe(zero=True)
        self.inspect_pane() # Prove the exit-observation/recovery command before stopping.
        return result

    def release_lock(self):return self.p.release_lock()

    def pause_cron(self, receipt):
        self.control('pause-cron',receipt['cron'])
        status=self.control('inspect')
        if status['cron'][0]['state'] not in ['T','t'] or status['conflicts']:
            raise Blocked('CRON_PAUSE_NOT_VERIFIED')

    def inspect_pane(self):return self.control('inspect-pane')
    def arm(self, pane):return self.control('arm-pane',pane)

    def shutdown(self):
        return json.loads(self.runner.run(['docker','exec',CONTAINER,'/usr/bin/python3','-c',SHUTDOWN],100))

    def wait_clean_exit(self, receipt):
        end=time.monotonic()+90
        while time.monotonic()<end:
            status=self.control('inspect');pane=self.control('pane-status',receipt['pane'])
            if not status['game'] and pane['dead']==1:
                if pane['exitCode']!=0:raise Blocked('GAME_EXIT_NOT_CLEAN')
                self.p.inspect_live()
                return {'gameExitCode':0,'writerAbsenceVerified':True}
            time.sleep(1)
        raise Blocked('GRACEFUL_SHUTDOWN_NOT_VERIFIED')

    def copy(self):
        status=self.control('inspect')
        if status['game'] or status['conflicts'] or status['cron'][0]['state'] not in ['T','t']:
            raise Blocked('WORLD_NOT_QUIESCENT')
        archive=self.directory/'quiescent-world-config.tar'
        if archive.exists() or archive.is_symlink():raise Blocked('QUIESCENT_COPY_EXISTS_INSPECT_FIRST')
        self.runner.run(['tar','--acls','--xattrs','--numeric-owner','-cpf',str(archive),'-C',str(self.p.root),'7DaysToDie','LGSM-Config'],180)
        archive.chmod(0o600)
        self.runner.run(['tar','--acls','--xattrs','--compare','--file',str(archive),'-C',str(self.p.root)],90)
        archive_members(archive,allowed=('7DaysToDie','LGSM-Config'))
        status=self.control('inspect')
        if status['game'] or status['conflicts']:raise Blocked('WORLD_WRITER_RETURNED')
        return {'quiescentCopySha256':digest(archive),'quiescentCopyBytes':archive.stat().st_size,
                'sourceQuiescent':True,'fullWorldBackup':False,'verified':False}

    def recover(self, receipt):
        self.require_assets()
        status=self.control('inspect')
        if receipt.get('shutdownAttempted') and status['game']:
            # Never race a still-pending graceful shutdown with start/cron.
            end=time.monotonic()+90
            while status['game'] and time.monotonic()<end:
                time.sleep(1);status=self.control('inspect')
            if status['game']:raise Blocked('SHUTDOWN_STILL_PENDING_OPERATOR_REQUIRED')
        if not status['game']:
            if receipt.get('pane'):self.control('remove-dead-pane',receipt['pane'])
            self.runner.run(['docker','exec','--user','sdtdserver','--workdir','/home/sdtdserver',CONTAINER,'./sdtdserver','start'],60)
        elif receipt.get('pane'):
            self.control('disarm-pane',receipt['pane'])
        end=time.monotonic()+240
        while time.monotonic()<end:
            try:
                result=self.probe();self.require_assets()
                return result
            except Blocked:time.sleep(2)
        raise Blocked('FIXED_RETURN_NOT_VERIFIED')

    def resume_cron(self, receipt):
        self.control('resume-cron',receipt['cron'])
        status=self.control('inspect')
        if (len(status['cron'])!=1 or status['cron'][0]['state'] in ['T','t']
                or status['crontabSha256']!=CRONTAB_SHA or len(status['vpn'])!=1):
            raise Blocked('CRON_OR_VPN_RETURN_NOT_VERIFIED')


class LimitedMaintenance:
    def __init__(self, reservation, backend, emit=lambda _:None):
        self.reservation,self.backend,self.emit=reservation,backend,emit

    def run(self, job):
        with self.reservation.locked(),self.backend.release_lock():
            self.reservation.require_free()
            readiness=self.backend.preflight()
            state=self.reservation.claim(job,TARGET)
            state.update(cron=readiness['cron'][0],shutdownAttempted=False,cronPauseAttempted=False,
                         fullWorldBackup=False,verified=False,runtimeRestored=False)
            def phase(value):
                self.reservation.save(state,value)
                self.emit({'preparationId':job,'phase':value})
            try:
                state['cronPauseAttempted']=True;phase('pausing_cron')
                self.backend.pause_cron(state)
                state['pane']=self.backend.inspect_pane()
                phase('arming_exit_observation')
                self.backend.arm(state['pane'])
                phase('checking_latest_players')
                self.backend.probe(zero=True)
                state['shutdownAttempted']=True;phase('stopping_game')
                result=self.backend.shutdown()
                state.update(result)
                if result.get('shutdownSent') is not True:
                    state['shutdownAttempted']=False
                    raise Blocked(result.get('errorCode','SHUTDOWN_NOT_SENT'))
                phase('checking_clean_exit')
                state.update(self.backend.wait_clean_exit(state))
                phase('copying_quiescent_world')
                state.update(self.backend.copy())
            except Exception as error:
                state['failedPhase']=state['phase']
                state['errorCode']=str(error) if isinstance(error,Blocked) else 'LIMITED_MAINTENANCE_FAILED'
            finally:
                phase('returning_original_game')
                try:
                    state.update(self.backend.recover(state))
                    state['gameReturned']=True
                    phase('resuming_cron')
                    if state['cronPauseAttempted']:self.backend.resume_cron(state)
                    state['recoveryRequired']=False
                    phase('returned_with_error' if state.get('errorCode') else 'completed')
                    self.reservation.release(state)
                except Exception as error:
                    state['recoveryErrorCode']=str(error) if isinstance(error,Blocked) else 'FIXED_RETURN_FAILED'
                    state['recoveryRequired']=True
                    phase('operator_recovery_required')
            return state

    def recover(self, job):
        with self.reservation.locked(),self.backend.release_lock():
            state=self.reservation.read()
            if state is None or state['jobId']!=job:raise Blocked('RESERVATION_OWNER_MISMATCH')
            state.update(self.backend.recover(state));state['gameReturned']=True
            if state.get('cronPauseAttempted'):self.backend.resume_cron(state)
            state['recoveryRequired']=False
            self.reservation.save(state,'operator_recovered');self.reservation.release(state)
            return state


def safe_result(state):
    allowed=['jobId','phase','recoveryRequired','errorCode','failedPhase','recoveryErrorCode','gameReturned',
             'gameVersion','onlinePlayers','gameExitCode','writerAbsenceVerified','shutdownSent','telnetEOF',
             'quiescentCopySha256','quiescentCopyBytes','sourceQuiescent','fullWorldBackup','verified','runtimeRestored']
    return {k:state[k] for k in allowed if k in state}


if __name__=='__main__':
    try:
        if len(sys.argv)!=3 or sys.argv[1] not in ['verify','collect','recover']:
            raise Blocked('USE_FIXED_MAINTENANCE_ACTION_AND_PREPARATION_ID')
        backend=LiveBackend(RuntimePreparer(DockerRunner()),sys.argv[2])
        reservation=Reservation(backend.p.root/'upgrade-backups/limited-maintenance-reservation')
        workflow=LimitedMaintenance(reservation,backend,lambda data:print(json.dumps(data),flush=True))
        if sys.argv[1]=='verify':
            with reservation.locked(),backend.release_lock():
                reservation.require_free();backend.preflight()
            print(json.dumps({'phase':'ready_for_limited_maintenance','verified':False}),flush=True)
        else:
            result=workflow.run(sys.argv[2]) if sys.argv[1]=='collect' else workflow.recover(sys.argv[2])
            print(json.dumps(safe_result(result)),flush=True)
            if result.get('errorCode') or result.get('recoveryRequired'):sys.exit(1)
    except Exception as error:
        code=str(error) if isinstance(error,Blocked) else 'FIXED_MAINTENANCE_FAILED'
        print(json.dumps({'errorCode':code}),flush=True);sys.exit(1)
