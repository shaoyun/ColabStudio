"""Tests run on local temporary files only. No GPU, cloud, token or Keychain access."""
import importlib.util, json, pathlib, shutil, tarfile, tempfile, unittest, sys
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]/"templates"))
SOURCE = pathlib.Path(__file__).resolve().parents[1]
class SupervisorTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tmp.name)
        shutil.copy(SOURCE/'templates/toybox_supervisor.py',self.root/'supervisor.py')
        (self.root/'deployment.json').write_text(json.dumps({'id':'test','binding':'test'}))
        (self.root/'lock.json').write_text(json.dumps({'toybox':'test'}))
        spec = importlib.util.spec_from_file_location('supervisor',self.root/'supervisor.py')
        self.m = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.m)
    def tearDown(self): self.tmp.cleanup()
    def test_active_jobs_and_agents_block_shutdown(self):
        def responses(job,agent,queue=None):
            return lambda url: {'http://127.0.0.1:8188/queue': queue or {'queue_running':[],'queue_pending':[]},'http://127.0.0.1:8190/api/jobs':job,'http://127.0.0.1:8190/api/agent/runs':agent}[url]
        for status in ['queued','running','joining']:
            self.m.fetch=responses([{'status':status}],[])
            self.assertTrue(self.m.is_busy())
        for status in ['planning','generating','unknown']:
            self.m.fetch=responses([], [{'status':status}])
            self.assertTrue(self.m.is_busy())
        self.m.fetch=responses([{'status':'done'}],[{'status':'done'}])
        self.assertFalse(self.m.is_busy())
        self.m.fetch=responses([],[],{'queue_running':[]})
        with self.assertRaises(RuntimeError): self.m.is_busy()
    def test_snapshot_excludes_secrets_and_models(self):
        data=self.m.DATA
        (data/'outputs').mkdir()
        (data/'outputs'/'test.mp4').write_bytes(b'video')
        (data/'secrets.json').write_text('secret')
        (data/'config.json').write_text(json.dumps({'provider':'gemini','nested':{'api_key':'secret','model':'model'}}))
        (data/'outputs'/'linked').symlink_to(data/'secrets.json')
        self.m.STATE['stage']='failed'
        self.m.snapshot('unit-test')
        with tarfile.open(self.root/'backup-unit-test.tar.gz') as t:
            self.assertEqual(set(t.getnames()),{'outputs/test.mp4','config.json'})
            cfg=json.load(t.extractfile('config.json'))
            self.assertNotIn('api_key',cfg['nested'])
        self.assertTrue((self.root/'frozen').exists())
        self.assertEqual(self.m.STATE['backup']['nonce'],'unit-test')
    def test_queue_race_refuses_snapshot(self):
        self.m.is_busy=lambda: True
        with self.assertRaises(RuntimeError): self.m.snapshot('race')
        self.assertFalse((self.root/'backup-race.tar.gz').exists())
    def test_restore_rejects_path_escape(self):
        import io, hashlib
        p=self.root/'restore.part000000'
        with tarfile.open(p,'w:gz') as t:
            entry=tarfile.TarInfo('../escape');entry.size=3;t.addfile(entry,io.BytesIO(b'bad'))
        (self.root/'restore.json').write_text(json.dumps({'parts':1,'md5':hashlib.md5(p.read_bytes()).hexdigest()}))
        with self.assertRaises(RuntimeError):self.m.restore()
        self.assertFalse((self.root.parent/'escape').exists())
if __name__=='__main__':unittest.main()
