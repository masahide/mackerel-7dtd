"""An OCI config-only derivative for isolated trials, with identical layers.

Keep the original recovery archive unchanged. Remove VOLUME/EXPOSE/Compose
metadata so only the five explicitly verified private bind mounts can exist.
No external fetch, layer extraction, production replacement or backup proof.
"""
import hashlib
import io
import json
import os
import sys
import tarfile
import time

from runtime_freeze import DockerRunner, FIXED_ENV, RuntimePreparer
from suzume_update import ArchiveStore, Blocked, JOB, atomic_json, digest


def encode(value):return json.dumps(value,sort_keys=True,separators=(',',':')).encode()
def sha(data):return 'sha256:'+hashlib.sha256(data).hexdigest()


def derivative_archive(source, source_id, job, destination):
    if not isinstance(job,str) or not JOB.fullmatch(job):raise Blocked('INVALID_PREPARATION_ID')
    ArchiveStore.validate_image(source,source_id)
    with tarfile.open(source,'r:') as original:
        try:
            index=json.load(original.extractfile('index.json'))
            descriptor=index['manifests'][0]
            manifest=json.load(original.extractfile('blobs/sha256/'+descriptor['digest'].split(':')[1]))
            config=json.load(original.extractfile('blobs/sha256/'+manifest['config']['digest'].split(':')[1]))
        except (KeyError,TypeError):raise Blocked('TRIAL_SOURCE_MUST_BE_OCI') from None
        settings=config['config']
        env=dict(item.split('=',1) for item in settings['Env'] if '=' in item)
        if any(env.get(key)!=value for key,value in FIXED_ENV.items()):raise Blocked('TRIAL_SOURCE_FLAGS_CHANGED')
        # These declarations create volumes beyond the explicit bind whitelist.
        settings.pop('Volumes',None);settings.pop('ExposedPorts',None)
        labels=settings.get('Labels') or {}
        settings['Labels']={key:value for key,value in labels.items()
            if not key.startswith(('com.docker.compose.','desktop.docker.io.')) and key!='org.suzume.preparation'}
        settings['Labels'].update({'org.suzume.runtime-trial':job,'org.suzume.source-image':source_id})
        settings.update(Entrypoint=['/bin/sleep'],Cmd=['infinity'])
        config_data=encode(config);config_id=sha(config_data)
        manifest['config']={**manifest['config'],'digest':config_id,'size':len(config_data)}
        manifest_data=encode(manifest);image_id=sha(manifest_data)
        index['manifests']=[{'mediaType':'application/vnd.oci.image.manifest.v1+json',
                            'digest':image_id,'size':len(manifest_data),'platform':{'os':'linux','architecture':'amd64'}}]
        replacement={'index.json':encode(index),'blobs/sha256/'+config_id.split(':')[1]:config_data,
                     'blobs/sha256/'+image_id.split(':')[1]:manifest_data}
        fd=os.open(destination,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
        with os.fdopen(fd,'wb') as stream,tarfile.open(fileobj=stream,mode='w:') as output:
            for member in original.getmembers():
                if member.name in ['manifest.json','repositories','index.json'] or member.name in replacement:continue
                output.addfile(member,original.extractfile(member) if member.isfile() else None)
            for name,data in replacement.items():
                member=tarfile.TarInfo(name);member.size=len(data);member.mode=0o600
                output.addfile(member,io.BytesIO(data))
    ArchiveStore.validate_image(destination,image_id)
    return image_id


def prepare(preparer,job):
    if not isinstance(job,str) or not JOB.fullmatch(job):raise Blocked('INVALID_PREPARATION_ID')
    folder=preparer.root/'upgrade-backups'/('adapter-preparation-'+job)
    state=json.loads((folder/'state.json').read_text())
    receipt_path=folder/'isolated-image.json'
    if receipt_path.exists():raise Blocked('ISOLATED_IMAGE_EXISTS_INSPECT_FIRST')
    with preparer.release_lock():
        preparer.inspect_live()
        if preparer.checkpoint()!=state['staticRuntimeHashes']:raise Blocked('STATIC_RUNTIME_CHANGED')
        source=folder/'runtime-image.tar'
        if digest(source)!=state['imageSha256']:raise Blocked('SOURCE_ARCHIVE_CHANGED')
        receipt={'preparationId':job,'sourceImageId':state['frozenImageId'],'phase':'preparing_isolated_image',
                 'createdAt':time.time(),'verified':False,'runtimeRestored':False,'productionEnabled':False}
        atomic_json(receipt_path,receipt)
        try:
            destination=folder/'isolated-trial-image.tar'
            image_id=derivative_archive(source,state['frozenImageId'],job,destination)
            receipt.update(trialImageId=image_id,archiveSha256=digest(destination),phase='loading_isolated_image')
            atomic_json(receipt_path,receipt)
            preparer.runner.run(['docker','image','load','--input',str(destination)],120)
            image=json.loads(preparer.runner.run(['docker','image','inspect',image_id],30))[0]
            original=json.loads(preparer.runner.run(['docker','image','inspect',state['frozenImageId']],30))[0]
            cfg=image['Config']
            if (image['Id']!=image_id or image['RootFS']!=original['RootFS'] or cfg.get('Volumes') or cfg.get('ExposedPorts')
                    or cfg.get('Labels',{}).get('org.suzume.runtime-trial')!=job
                    or any(key.startswith(('com.docker.compose.','desktop.docker.io.')) for key in cfg.get('Labels',{}))):
                raise Blocked('ISOLATED_IMAGE_METADATA_MISMATCH')
            if preparer.checkpoint(image_id)!=state['staticRuntimeHashes']:raise Blocked('ISOLATED_RUNTIME_CHANGED')
            preparer.inspect_live()
            receipt.update(phase='isolated_image_prepared',layersIdentical=True,staticRuntimeIdentical=True)
        except Exception as error:
            receipt.update(phase='failed',errorCode=str(error) if isinstance(error,Blocked) else 'ISOLATED_IMAGE_FAILED')
        atomic_json(receipt_path,receipt)
        if receipt.get('errorCode'):raise Blocked(receipt['errorCode'])
        return receipt


if __name__=='__main__':
    try:
        if len(sys.argv)!=3 or sys.argv[1]!='prepare':raise Blocked('USE_PREPARE_WITH_PREPARATION_ID')
        print(json.dumps(prepare(RuntimePreparer(DockerRunner()),sys.argv[2])))
    except Exception as error:
        print(json.dumps({'errorCode':str(error) if isinstance(error,Blocked) else 'ISOLATED_IMAGE_FAILED'}));sys.exit(1)
