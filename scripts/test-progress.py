"""Real local files and progress callbacks, with no network or model downloads."""
import hashlib, importlib.util, json, pathlib, sys, tempfile, unittest
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]/'templates'))
from studio_progress import ProgressReporter, download_tqdm
from toybox_download import download_models
class Counter:
    def __init__(self,*args,**kw): self.n=kw.get('initial',0);self.disabled=kw.get('disable')
    def update(self,n): self.n+=n
    def close(self): pass
class ProgressTests(unittest.TestCase):
    def test_callback_reports_real_bytes_speed_and_eta(self):
        with tempfile.TemporaryDirectory() as d:
            p=ProgressReporter(d,2,20);p.begin_file(1,'model.bin',10,0)
            c=download_tqdm(Counter,p)(total=10,name='huggingface_hub.http_get',disable=True)
            p.sample_time-=2;p.last_write=0;c.update(4)
            value=json.loads((pathlib.Path(d)/'progress.json').read_text())
            self.assertFalse(c.disabled);self.assertEqual(value['currentBytes'],4)
            self.assertGreater(value['speedBytes'],0);self.assertGreater(value['etaSeconds'],0)
            c.update(6);self.assertEqual(p.value['currentBytes'],10)
    def test_initial_cached_bytes_do_not_inflate_speed(self):
        with tempfile.TemporaryDirectory() as d:
            p=ProgressReporter(d,1,10);p.begin_file(1,'model.bin',10,0)
            c=download_tqdm(Counter,p)(total=10,initial=4)
            p.sample_time-=2;p.last_write=0;c.update(2)
            self.assertEqual(p.value['currentBytes'],6)
            self.assertGreater(p.value['speedBytes'],0.5)
            self.assertLess(p.value['speedBytes'],1.5)
    def setup_download(self,root,bad=False):
        payload=b'model!';lock={'models':[{'repo':'test/model','revision':'pinned','file':'model.bin','folder':'weights','size':len(payload),'sha256':hashlib.sha256(b'wrong' if bad else payload).hexdigest()}],'guidesRevision':'pinned'}
        (root/'lock.json').write_text(json.dumps(lock));(root/'minimax-h3-colab/toybox').mkdir(parents=True)
        def download(repo,name,**kw):
            p=root/('download-'+pathlib.Path(name).name);p.write_bytes(payload)
            if 'tqdm_class' in kw:
                c=kw['tqdm_class'](total=len(payload),initial=0);c.update(len(payload));c.close()
            return p
        return download,len(payload)
    def test_download_verification_and_cache_counters(self):
        with tempfile.TemporaryDirectory() as d:
            root=pathlib.Path(d);download,size=self.setup_download(root)
            download_models(root,download,Counter)
            result=json.loads((root/'progress.json').read_text());self.assertEqual(result['phase'],'download_complete');self.assertEqual(result['completedBytes'],size);self.assertEqual(result['verifiedFiles'],1)
            def cached(repo,name,**kw):
                if not name.startswith('docs/'):self.fail('cached model downloaded again')
                return download(repo,name,**kw)
            download_models(root,cached,Counter)
            self.assertEqual(json.loads((root/'progress.json').read_text())['completedBytes'],size)
    def test_bad_checksum_never_marks_verified(self):
        with tempfile.TemporaryDirectory() as d:
            root=pathlib.Path(d);download,_=self.setup_download(root,True)
            with self.assertRaises(RuntimeError):download_models(root,download,Counter)
            result=json.loads((root/'progress.json').read_text());self.assertEqual(result['phase'],'error');self.assertEqual(result['completedBytes'],0);self.assertEqual(result.get('verifiedFiles',0),0)
if __name__=='__main__': unittest.main()
