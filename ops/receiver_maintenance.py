"""Private receiver coordinator for the authorized fixed, attended rehearsal.

Existing credentials are sent only over SSH stdin to a private root-owned file.
Stage/verify do not change services. Collect inhibits only the management API,
launches a detached fixed worker, and resumes the API only after game/cron return.
"""
import base64,hashlib,json,pathlib,re,shlex,subprocess,sys,time,urllib.parse,urllib.request

JOB='b2684acae8b0495d8afa700c4cd28b2c'
ROOT='/home/masahide/work/7dtd/upgrade-backups/adapter-preparation-'+JOB
PROGRAM=ROOT+'/maintenance-program'

def unit(name):
    return subprocess.check_output(['systemctl','show','apiserver7dtd.service','--value','-p',name],text=True).strip()

def configuration():
    pid=unit('MainPID')
    if not pid.isdigit() or int(pid)<=0:raise RuntimeError('MANAGEMENT_API_NOT_RUNNING')
    env=dict(x.decode().split('=',1) for x in pathlib.Path('/proc/'+pid+'/environ').read_bytes().split(b'\0') if b'=' in x)
    def setting(key):return env.get('OPSA_'+key,env.get(key,''))
    parts=urllib.parse.urlsplit(setting('API_BASE_URL'))
    if (parts.scheme!='http' or parts.port!=8080 or parts.path.rstrip('/')!='/api' or parts.username or parts.query
            or not setting('API_USER') or not setting('API_SECRET')):raise RuntimeError('EXISTING_PROBE_CONFIGURATION_CHANGED')
    if any(unit(k) for k in ['ExecStartPre','ExecStartPost','ExecStop','ExecStopPost']):raise RuntimeError('SERVICE_SIDE_EFFECTS_NOT_REVIEWED')
    for prop in ['Requires','PartOf','BindsTo','TriggeredBy','Wants']:
        if re.search(r'\b(?:7dtd|7dtdserver)\.service\b',unit(prop)):raise RuntimeError('GAME_SERVICE_DEPENDENCY')
    binary=pathlib.Path('/usr/local/bin/apiserver7dtd')
    if hashlib.sha256(binary.read_bytes()).hexdigest()!='5240934142a625efaa30995359af0d7eee021cfbe381a20f1e8fad0945a9f2b5':raise RuntimeError('MANAGEMENT_BINARY_CHANGED')
    return {'user':setting('API_USER'),'secret':setting('API_SECRET')}

STAGE=r'''
import hashlib,json,os,pathlib,subprocess,sys
request=json.load(sys.stdin);job='b2684acae8b0495d8afa700c4cd28b2c'
folder=pathlib.Path('/home/masahide/work/7dtd/upgrade-backups')/('adapter-preparation-'+job)
if pathlib.Path('/home/masahide/work/7dtd/upgrade-backups/limited-maintenance-reservation/reservation.json').exists():raise SystemExit('MAINTENANCE_ALREADY_RESERVED')
program=folder/'maintenance-program';private=folder/'limited-maintenance'
for path in [program,private]:
 path.mkdir(mode=0o700,exist_ok=True)
 if path.is_symlink() or path.resolve().parent!=folder.resolve() or path.stat().st_uid!=0 or path.stat().st_mode & 0o077:raise SystemExit('PRIVATE_PROGRAM_DIRECTORY_NOT_SAFE')
hashes={}
if set(request['modules'])!={'suzume_update','runtime_freeze','runtime_rehearsal','limited_maintenance'}:raise SystemExit('INVALID_SOURCE_BUNDLE')
for name,item in request['modules'].items():
 data=item['code'].encode();value=hashlib.sha256(data).hexdigest()
 if value!=item['sha256']:raise SystemExit('SOURCE_HASH_MISMATCH')
 path=program/(name+'.py');hashes[name+'.py']=value
 if path.exists():
  if path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest()!=value:raise SystemExit('EXISTING_PROGRAM_DIFFERS')
 else:
  fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
  with os.fdopen(fd,'wb') as stream:stream.write(data)
manifest=program/'source-hashes.json'
if not manifest.exists():
 fd=os.open(manifest,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'w') as stream:json.dump(hashes,stream)
elif json.loads(manifest.read_text())!=hashes:raise SystemExit('EXISTING_PROGRAM_MANIFEST_DIFFERS')
probe=private/'probe.private.json'
if probe.exists():
 if probe.is_symlink() or json.loads(probe.read_text())!=request['probe']:raise SystemExit('EXISTING_PRIVATE_PROBE_DIFFERS')
else:
 fd=os.open(probe,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'w') as stream:json.dump(request['probe'],stream)
p=subprocess.run(['python3',str(program/'limited_maintenance.py'),'verify',job],capture_output=True,text=True,timeout=150)
for line in p.stdout.splitlines():
 item=json.loads(line)
 if not isinstance(item,dict) or not set(item)<={'phase','verified','errorCode'}:raise SystemExit('UNSAFE_VERIFICATION_OUTPUT')
 print(json.dumps(item),flush=True)
raise SystemExit(p.returncode)
'''

EXECUTE=r'''
import hashlib,json,os,pathlib,subprocess,sys
action=sys.argv[1];job='b2684acae8b0495d8afa700c4cd28b2c'
folder=pathlib.Path('/home/masahide/work/7dtd/upgrade-backups')/('adapter-preparation-'+job)
program=folder/'maintenance-program';private=folder/'limited-maintenance'
hashes=json.loads((program/'source-hashes.json').read_text())
for name,value in hashes.items():
 p=program/name
 if p.is_symlink() or p.stat().st_uid!=0 or p.stat().st_mode & 0o077 or hashlib.sha256(p.read_bytes()).hexdigest()!=value:raise SystemExit('STAGED_SOURCE_CHANGED')
if action not in ['collect','recover']:raise SystemExit('INVALID_MAINTENANCE_ACTION')
log=private/('worker.private.log' if action=='collect' else 'recovery.private.log')
fd=os.open(log,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,'wb') as output:
 p=subprocess.Popen(['python3',str(program/'limited_maintenance.py'),action,job],stdin=subprocess.DEVNULL,
   stdout=output,stderr=output,start_new_session=True)
(private/'worker-pid.json').write_text(json.dumps({'pid':p.pid,'action':action})+'\n')
(private/'worker-pid.json').chmod(0o600)
print(json.dumps({'phase':'worker_launched','pid':p.pid,'action':action}),flush=True)
'''

STATUS=r'''
import json,pathlib,os
job='b2684acae8b0495d8afa700c4cd28b2c';root=pathlib.Path('/home/masahide/work/7dtd/upgrade-backups')
state=root/'limited-maintenance-reservation/reservation.json';completed=root/'limited-maintenance-reservation'/(job+'.completed.json')
private=root/('adapter-preparation-'+job)/'limited-maintenance'
allowed=['jobId','phase','recoveryRequired','errorCode','failedPhase','recoveryErrorCode','gameReturned','gameVersion','onlinePlayers','gameExitCode','writerAbsenceVerified','shutdownSent','telnetEOF','quiescentCopySha256','quiescentCopyBytes','sourceQuiescent','fullWorldBackup','verified','runtimeRestored']
data=json.loads(state.read_text()) if state.exists() else json.loads(completed.read_text()) if completed.exists() else {'phase':'not_reserved'}
out={k:data[k] for k in allowed if k in data}
pid=json.loads((private/'worker-pid.json').read_text())['pid'] if (private/'worker-pid.json').exists() else None
out['workerPresent']=pid is not None and pathlib.Path('/proc',str(pid)).exists()
if out.get('phase')=='not_reserved' and not out['workerPresent']:
 logs=[]
 for name in ['worker.private.log','recovery.private.log']:
  p=private/name
  if p.exists():
   for line in p.read_text().splitlines():
    try:item=json.loads(line)
    except ValueError:continue
    if isinstance(item,dict) and 'errorCode' in item:logs.append(item['errorCode'])
 out['startupErrorCodes']=logs
print(json.dumps(out))
'''

def remote(code,payload=None,arguments=(),timeout=180):
    command=shlex.join(['sudo','-n','python3','-c',code,*arguments])
    args=['ssh','-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o','ConnectTimeout=10',
        '-o','ServerAliveInterval=15','-o','ServerAliveCountMax=8','-o','IdentityAgent=none','-o','IdentitiesOnly=yes',
        '-o','UserKnownHostsFile=/var/lib/7dtd-trailwatch/.ssh/known_hosts','-i','/var/lib/7dtd-trailwatch/id_ed25519',
        '-p','2022','masahide@localhost',command]
    p=subprocess.run(args,input=json.dumps(payload) if payload is not None else None,capture_output=True,text=True,timeout=timeout)
    items=[json.loads(line) for line in p.stdout.splitlines() if line.strip()]
    if p.returncode:raise RuntimeError(items[-1].get('errorCode','REMOTE_MAINTENANCE_FAILED') if items else 'REMOTE_MAINTENANCE_FAILED')
    return items

def api_start():
    subprocess.run(['systemctl','start','apiserver7dtd.service'],check=True,capture_output=True,timeout=45)
    if unit('ActiveState')!='active':raise RuntimeError('MANAGEMENT_API_RETURN_NOT_VERIFIED')
    configuration()


def main():
    try:
        request=json.load(sys.stdin)
        action=request.get('operation')
        if action not in ['stage','collect','recover','status']:raise RuntimeError('INVALID_FIXED_MAINTENANCE_REQUEST')
        if action=='stage':
            data={'modules':request['modules'],'probe':configuration()}
            for item in remote(STAGE,data):print(json.dumps(item),flush=True)
        elif action=='status':
            for item in remote(STATUS):print(json.dumps(item),flush=True)
        else:
            if action=='collect':
                configuration()
                # The fixed game CLI performs two authenticated local API player
                # checks, then version/lp immediately before issuing shutdown.
                subprocess.run(['systemctl','stop','apiserver7dtd.service'],check=True,capture_output=True,timeout=45)
                print(json.dumps({'phase':'management_mutators_inhibited'}),flush=True)
            for item in remote(EXECUTE,arguments=(action,)):print(json.dumps(item),flush=True)
            previous=None;end=time.monotonic()+900
            while time.monotonic()<end:
                status=remote(STATUS,timeout=45)[0]
                if status.get('phase')!=previous:
                    print(json.dumps(status),flush=True);previous=status.get('phase')
                if status.get('gameReturned') and status.get('recoveryRequired') is False:
                    api_start();print(json.dumps({'phase':'management_api_returned','gameReturned':True}),flush=True)
                    if status.get('errorCode'):raise RuntimeError('GAME_RETURNED_WITH_MAINTENANCE_ERROR')
                    break
                if status.get('phase')=='operator_recovery_required' or not status.get('workerPresent'):
                    if status.get('phase')=='not_reserved' and not status.get('workerPresent'):
                        # Preflight failed before the durable claim/mutators. Do not
                        # leave a healthy management service inhibited unnecessarily.
                        api_start();print(json.dumps({'phase':'management_api_returned_before_game_mutation'}),flush=True)
                        raise RuntimeError('MAINTENANCE_PREFLIGHT_FAILED_NO_GAME_CHANGE')
                    raise RuntimeError('OPERATOR_RECOVERY_REQUIRED_API_REMAINS_INHIBITED')
                time.sleep(2)
            else:raise RuntimeError('WORKER_STATUS_UNKNOWN_API_REMAINS_INHIBITED')
    except Exception as error:
        code=str(error) if isinstance(error,RuntimeError) else 'FIXED_COORDINATOR_FAILED'
        print(json.dumps({'errorCode':code}),flush=True);return 1

    return 0


if __name__=='__main__':sys.exit(main())
