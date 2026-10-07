import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from runtime_freeze import FIXED_ENV
from suzume_update import ArchiveStore, Blocked, digest
from trial_image import derivative_archive
from test_image_archive import blob_name,oci_entries,write_archive


class TrialImageTest(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.source=Path(self.temp.name)/'original.tar';self.output=Path(self.temp.name)/'trial.tar'
        self.job='a'*32

    def fixture(self,wrong_flags=False):
        def settings(config):
            flags=dict(FIXED_ENV)
            if wrong_flags:flags['START_MODE']='3'
            config['config']={'Volumes':{'/game/':{}},'ExposedPorts':{'26900/tcp':{}},
                'Env':[k+'='+v for k,v in flags.items()]+['API_SECRET=fixture-private'],
                'Labels':{'com.docker.compose.project':'production','desktop.docker.io/wsl-distro':'fixture',
                    'org.suzume.preparation':self.job,'keep':'metadata'},'Entrypoint':['/vpn.sh']}
        entries,self.image_id,_,self.layer_id=oci_entries(change_config=settings)
        write_archive(self.source,entries)
        self.entries=entries

    def test_only_config_changes_layers_and_original_archive_remain_identical(self):
        self.fixture();before=digest(self.source)
        new_id=derivative_archive(self.source,self.image_id,self.job,self.output)
        self.assertNotEqual(new_id,self.image_id);self.assertEqual(before,digest(self.source))
        ArchiveStore.validate_image(self.output,new_id)
        with tarfile.open(self.output,'r:') as tar:
            index=json.load(tar.extractfile('index.json'))
            manifest=json.load(tar.extractfile(blob_name(index['manifests'][0]['digest'])))
            config=json.load(tar.extractfile(blob_name(manifest['config']['digest'])))
            self.assertEqual(tar.extractfile(blob_name(self.layer_id)).read(),self.entries[blob_name(self.layer_id)])
            self.assertNotIn('manifest.json',tar.getnames())
        settings=config['config']
        self.assertNotIn('Volumes',settings);self.assertNotIn('ExposedPorts',settings)
        self.assertEqual(settings['Entrypoint'],['/bin/sleep']);self.assertEqual(settings['Cmd'],['infinity'])
        self.assertEqual(settings['Labels']['org.suzume.source-image'],self.image_id)
        self.assertFalse(any(k.startswith(('com.docker.compose.','desktop.docker.io.')) for k in settings['Labels']))
        self.assertNotIn('org.suzume.preparation',settings['Labels'])
        self.assertIn('API_SECRET=fixture-private',settings['Env'])

    def test_changed_source_flags_or_digest_cannot_produce_trial(self):
        self.fixture(wrong_flags=True)
        with self.assertRaisesRegex(Blocked,'FLAGS_CHANGED'):derivative_archive(self.source,self.image_id,self.job,self.output)
        self.assertFalse(self.output.exists())
        with self.assertRaises(Blocked):derivative_archive(self.source,'sha256:'+'0'*64,self.job,self.output)
        self.assertFalse(self.output.exists())

    def test_existing_output_cannot_be_overwritten(self):
        self.fixture();self.output.write_bytes(b'uncertain retained attempt')
        with self.assertRaises(FileExistsError):derivative_archive(self.source,self.image_id,self.job,self.output)
        self.assertEqual(self.output.read_bytes(),b'uncertain retained attempt')


if __name__=='__main__':unittest.main()
