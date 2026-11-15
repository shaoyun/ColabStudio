"""Offline sampler and pinned localization checks. No credentials or cloud calls."""
import importlib.util, pathlib, tempfile, unittest, json, hashlib, shutil, sys
from types import SimpleNamespace
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]/'templates'))
import studio_metrics as metrics
from toybox_zh_cn import localize
ROOT=pathlib.Path(__file__).resolve().parents[1]
class TelemetryTests(unittest.TestCase):
    def test_cpu_delta_memory_gpu_units_and_no_first_cpu(self):
        with tempfile.TemporaryDirectory() as d:
            proc=pathlib.Path(d)
            (proc/'stat').write_text('cpu  10 0 10 60 20 0 0 0\n')
            (proc/'cpuinfo').write_text('model name : Test CPU\n')
            (proc/'meminfo').write_text('MemTotal: 1000 kB\nMemAvailable: 250 kB\n')
            calls=[]
            def gpu(*a,**k): calls.append((a,k));return SimpleNamespace(stdout='NVIDIA L4, 40, 1024, 4096\n')
            first,previous=metrics.collect(proc,gpu_run=gpu)
            self.assertIsNone(first['cpu']);self.assertEqual(first['ram'],75)
            (proc/'stat').write_text('cpu  40 0 20 100 40 0 0 0\n')
            sample,_=metrics.collect(proc,previous,gpu)
            self.assertAlmostEqual(sample['cpu'],40)
            self.assertEqual(sample['gpu'],40);self.assertEqual(sample['vram'],25)
            self.assertEqual(sample['vramUsed'],1073741824)
            self.assertEqual(calls[0][1]['timeout'],3)
    def test_missing_gpu_and_missing_proc_are_unknown(self):
        def missing(*a,**k): raise FileNotFoundError()
        sample,previous=metrics.collect(pathlib.Path('/nonexistent-colabstudio-proc'),gpu_run=missing)
        self.assertIsNone(previous)
        for key in ['cpu','gpu','ram','vram']:self.assertIsNone(sample[key])
    def test_localization_refuses_unknown_content_without_partial_write(self):
        with tempfile.TemporaryDirectory() as d:
            root=pathlib.Path(d);(root/'a').write_text('設定');(root/'b').write_text('custom')
            sha=lambda s:hashlib.sha256(s.encode()).hexdigest()
            patch={'files':{n:{'source':sha('設定'),'target':sha('设置'),'lines':{'設定':'设置'}} for n in ['a','b']}}
            (root/'patch.json').write_text(json.dumps(patch))
            with self.assertRaises(RuntimeError): localize(root,root/'patch.json')
            self.assertEqual((root/'a').read_text(),'設定')
    def test_localization_idempotent(self):
        with tempfile.TemporaryDirectory() as d:
            root=pathlib.Path(d);(root/'a').write_text('設定')
            sha=lambda s:hashlib.sha256(s.encode()).hexdigest()
            (root/'patch.json').write_text(json.dumps({'files':{'a':{'source':sha('設定'),'target':sha('设置'),'lines':{'設定':'设置'}}}}))
            self.assertEqual(localize(root,root/'patch.json'),1)
            self.assertEqual(localize(root,root/'patch.json'),0)
if __name__=='__main__':unittest.main()
