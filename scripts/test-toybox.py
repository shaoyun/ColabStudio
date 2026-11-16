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
    def test_named_and_quick_tunnel_modes(self):
        spawned = []
        stopped = []

        class FakeProc:
            def __init__(self, alive=True):
                self.alive = alive
                self.pid = 424242
            def poll(self):
                return None if self.alive else 0

        def fake_spawn(name, args, cwd=None, env=None):
            spawned.append((name, list(args), dict(env) if env else None))
            self.m.PROCS[name] = FakeProc(alive=True)

        self.m.spawn = fake_spawn
        self.m.stop_proc = lambda name: (stopped.append(name), self.m.PROCS.pop(name, None))

        # Default (no tunnel.json) -> Quick Tunnel
        self.assertEqual(self.m.load_tunnel_config(), {'mode': 'quick'})
        self.m.start_tunnel()
        self.assertEqual(self.m.STATE['tunnel_mode'], 'quick')
        self.assertEqual(spawned[-1][1][1:], ['tunnel', '--url', 'http://127.0.0.1:8190'])
        self.assertIsNone(spawned[-1][2])
        (self.root / 'tunnel.log').write_text('INF | https://quick-demo.trycloudflare.com\n', encoding='utf-8')
        self.m.check_tunnel_url()
        self.assertEqual(self.m.STATE['url'], 'https://quick-demo.trycloudflare.com')

        # Switch to Named Tunnel via tunnel.json
        token = 'eyJ' + 'A' * 40
        (self.root / 'tunnel.json').write_text(json.dumps({'mode': 'named', 'url': 'https://toybox.example.com', 'token': token}), encoding='utf-8')
        self.m.start_tunnel(force_restart=True)
        self.assertIn('tunnel', stopped)
        self.assertEqual(self.m.STATE['tunnel_mode'], 'named')
        self.assertEqual(self.m.STATE['url'], '')
        last_args, last_env = spawned[-1][1], spawned[-1][2]
        self.assertEqual(last_args[1:], ['tunnel', '--no-autoupdate', '--metrics', '127.0.0.1:20241', 'run'])
        self.assertNotIn(token, ' '.join(last_args))
        self.assertEqual(last_env.get('TUNNEL_TOKEN'), token)
        self.assertEqual((self.root / 'tunnel.json').stat().st_mode & 0o777, 0o600)

        # Stale quick log was truncated; URL stays empty until metrics / connection ready
        self.m.tunnel_metrics_ready = lambda: False
        self.m.check_tunnel_url()
        self.assertEqual(self.m.STATE['url'], '')
        self.m.tunnel_metrics_ready = lambda: True
        self.m.check_tunnel_url()
        self.assertEqual(self.m.STATE['url'], 'https://toybox.example.com')

        # Drive snapshot never archives tunnel.json or tunnel token
        (self.m.DATA / 'outputs').mkdir(exist_ok=True)
        (self.m.DATA / 'outputs' / 'clip.mp4').write_bytes(b'ok')
        self.m.STATE['stage'] = 'failed'
        self.m.snapshot('tunnel-sec')
        with tarfile.open(self.root / 'backup-tunnel-sec.tar.gz') as t:
            self.assertNotIn('tunnel.json', t.getnames())
            self.assertEqual(set(t.getnames()), {'outputs/clip.mp4', 'config.json'})
    def test_is_busy_ignores_stale_orphaned_jobs_and_agents(self):
        import time
        now = time.time()
        started = now - 60

        def responses(job, agent, queue=None):
            return lambda url: {
                'http://127.0.0.1:8188/queue': queue or {'queue_running': [], 'queue_pending': []},
                'http://127.0.0.1:8190/api/jobs': job,
                'http://127.0.0.1:8190/api/agent/runs': agent,
            }[url]

        # Old orphan job (>120s) absent from ComfyUI queue does not lock supervisor.
        self.m.proc_start_epoch = lambda name: None
        self.m.fetch = responses([{'id': 'orphan-q', 'status': 'queued', 'created': now - 300}], [])
        self.assertFalse(self.m.is_busy())

        # Newly created job (<120s), queued ComfyUI job, or schemaless job stays busy.
        self.m.fetch = responses([{'id': 'fresh-q', 'status': 'queued', 'created': now - 30}], [])
        self.assertTrue(self.m.is_busy())
        self.m.fetch = responses(
            [{'id': 'in-comfy', 'status': 'running', 'created': now - 600}],
            [],
            {'queue_running': [[0, 'in-comfy']], 'queue_pending': []},
        )
        self.assertTrue(self.m.is_busy())
        self.m.fetch = responses([{'status': 'queued'}], [])
        self.assertTrue(self.m.is_busy())

        # Joining jobs & agents older than current toybox start time are orphans.
        self.m.proc_start_epoch = lambda name: started if name == 'toybox' else None
        self.m.fetch = responses([{'status': 'joining', 'finished': started - 30}], [])
        self.assertFalse(self.m.is_busy())
        self.m.fetch = responses([{'status': 'joining', 'finished': started + 10}], [])
        self.assertTrue(self.m.is_busy())
        self.m.fetch = responses([], [{'status': 'generating', 'created': started - 30, 'log': [[started - 20, 'step']]}])
        self.assertFalse(self.m.is_busy())
        self.m.fetch = responses([], [{'status': 'generating', 'created': started - 30, 'log': [[started + 5, 'step']]}])
        self.assertTrue(self.m.is_busy())
        self.m.fetch = responses([], [{'status': 'generating'}])
        self.assertTrue(self.m.is_busy())
    def test_deploy_keeps_comfyui_and_toybox_running_for_tunnel_only_switch(self):
        import hashlib
        (self.root / 'ComfyUI').mkdir(parents=True, exist_ok=True)
        cf = self.root / 'cloudflared'
        cf.write_bytes(b'fake-cloudflared')
        lock_data = {
            'toybox': 'test',
            'pip': {},
            'repos': {
                'Comfy-Org/ComfyUI': 'c1',
                'T8mars/comfyui-minimax-h3-audio-T8': 'c2',
                'facok/comfyui-SelfLift': 'c3',
                'NikoDemon80/ComfyUI-H3-Motion-Context': 'c4',
                'T8mars/comfyui-minimax-h3-prompt-enhancer-T8': 'c5',
                'shaoyun/minimax-h3-colab': 'c6',
            },
            'cloudflared': {'url': 'https://example.invalid/cf', 'sha256': hashlib.sha256(b'fake-cloudflared').hexdigest()},
        }
        (self.root / 'lock.json').write_text(json.dumps(lock_data), encoding='utf-8')
        (self.root / 'installed.json').write_text(json.dumps({'version': 'test'}), encoding='utf-8')
        (self.root / 'tunnel.json').write_text(json.dumps({'mode': 'named', 'url': 'https://toybox.example.com', 'token': 'eyJ' + 'A' * 40}), encoding='utf-8')
        spec = importlib.util.spec_from_file_location('supervisor_tunnel_switch', self.root / 'supervisor.py')
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)

        class FakeProc:
            def __init__(self, alive=True):
                self.alive = alive
                self.pid = 777777
                self.terminated = False
            def poll(self):
                return None if self.alive else 0
            def terminate(self):
                self.terminated = True
                self.alive = False

        comfy_proc = FakeProc(True)
        toybox_proc = FakeProc(True)
        tunnel_proc = FakeProc(True)
        mod.PROCS['comfyui'] = comfy_proc
        mod.PROCS['toybox'] = toybox_proc
        mod.PROCS['tunnel'] = tunnel_proc
        stopped = []
        mod.repo_matches = lambda dest, commit: True
        mod.restore = lambda: None
        mod.wait_http = lambda url: None
        mod.persist_processes = lambda: None
        mod.stop_proc = lambda name: (stopped.append(name), mod.PROCS[name].terminate(), mod.PROCS.pop(name, None))
        mod.spawn = lambda name, args, cwd=None, env=None: mod.PROCS.setdefault(name, FakeProc(True))
        mod.deploy()
        self.assertEqual(stopped, ['tunnel'])
        self.assertFalse(comfy_proc.terminated)
        self.assertFalse(toybox_proc.terminated)
        self.assertTrue(tunnel_proc.terminated)
        self.assertTrue((self.root / 'web.sig').exists())
        self.assertEqual(mod.STATE['stage'], 'online')

    def test_comfy_env_sets_expandable_segments(self):
        import os
        orig = os.environ.get('PYTORCH_CUDA_ALLOC_CONF')
        try:
            os.environ.pop('PYTORCH_CUDA_ALLOC_CONF', None)
            self.assertEqual(self.m.comfy_env()['PYTORCH_CUDA_ALLOC_CONF'], 'expandable_segments:True')
            os.environ['PYTORCH_CUDA_ALLOC_CONF'] = 'max_split_size_mb:128'
            self.assertEqual(self.m.comfy_env()['PYTORCH_CUDA_ALLOC_CONF'], 'max_split_size_mb:128,expandable_segments:True')
            os.environ['PYTORCH_CUDA_ALLOC_CONF'] = 'expandable_segments:True,garbage_collection_threshold:0.8'
            self.assertEqual(self.m.comfy_env()['PYTORCH_CUDA_ALLOC_CONF'], 'expandable_segments:True,garbage_collection_threshold:0.8')
        finally:
            if orig is None:
                os.environ.pop('PYTORCH_CUDA_ALLOC_CONF', None)
            else:
                os.environ['PYTORCH_CUDA_ALLOC_CONF'] = orig

    def test_ensure_chunk_ffn_node_and_math_equivalence(self):
        import torch
        import torch.nn as nn

        self.assertFalse(self.m.ensure_chunk_ffn_node())
        comfy_dir = self.root / 'ComfyUI'
        comfy_dir.mkdir(parents=True, exist_ok=True)
        (comfy_dir / 'main.py').write_text('# stub\n', encoding='utf-8')
        self.assertTrue(self.m.ensure_chunk_ffn_node())
        node_init = comfy_dir / 'custom_nodes' / 'h3_chunk_ffn' / '__init__.py'
        self.assertTrue(node_init.is_file())
        self.assertFalse(self.m.ensure_chunk_ffn_node())

        spec = importlib.util.spec_from_file_location('h3_chunk_ffn_test', node_init)
        node_mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(node_mod)
        self.assertIn('MiniMaxChunkFeedForward', node_mod.NODE_CLASS_MAPPINGS)

        observed_lens = []

        class ToyMLP(nn.Module):
            def __init__(self, dim=16, ffn=32):
                super().__init__()
                self.fc1 = nn.Linear(dim, ffn * 2, bias=False)
                self.fc2 = nn.Linear(ffn, dim, bias=False)

            def forward(self, x):
                observed_lens.append(x.shape[-2])
                a, b = self.fc1(x).chunk(2, dim=-1)
                return self.fc2(torch.nn.functional.silu(a) * b)

        class ToyBlock(nn.Module):
            def __init__(self):
                super().__init__()
                self.mlp = ToyMLP()

        class FakeModelPatcher:
            def __init__(self, dm):
                self.diffusion_model = dm
                self.patches = {}

            def clone(self):
                return FakeModelPatcher(self.diffusion_model)

            def get_model_object(self, name):
                assert name == 'diffusion_model'
                return self.diffusion_model

            def add_object_patch(self, name, obj):
                self.patches[name] = obj

        dm = nn.Module()
        dm.blocks = nn.ModuleList([ToyBlock()])
        patcher = FakeModelPatcher(dm)
        node = node_mod.NODE_CLASS_MAPPINGS['MiniMaxChunkFeedForward']()
        (patched,) = node.patch(patcher, chunks=4, seq_threshold=4096)
        patched_fn = patched.patches['diffusion_model.blocks.0.mlp.forward']

        torch.manual_seed(0)
        x_long = torch.randn(1, 5000, 16)
        with torch.no_grad():
            ref_long = dm.blocks[0].mlp.forward(x_long)
            observed_lens.clear()
            got_long = patched_fn(x_long)
        self.assertTrue(torch.allclose(ref_long, got_long, atol=1e-6))
        self.assertGreater(len(observed_lens), 1)
        self.assertTrue(all(n <= 1280 for n in observed_lens))

        x_short = torch.randn(1, 2048, 16)
        with torch.no_grad():
            ref_short = dm.blocks[0].mlp.forward(x_short)
            observed_lens.clear()
            got_short = patched_fn(x_short)
        self.assertEqual(observed_lens, [2048])
        self.assertTrue(torch.equal(ref_short, got_short))

        # High-VRAM GPUs (e.g. A100 40 GiB) bypass chunking cleanly without cloning/patching
        class Props:
            def __init__(self, gib):
                self.total_memory = int(gib * (1024 ** 3))

        orig_avail = torch.cuda.is_available
        orig_props = torch.cuda.get_device_properties
        try:
            torch.cuda.is_available = lambda: True
            torch.cuda.get_device_properties = lambda idx=0: Props(40.0)
            (a100_out,) = node.patch(patcher, chunks=4)
            self.assertIs(a100_out, patcher)
            self.assertEqual(a100_out.patches, {})

            torch.cuda.get_device_properties = lambda idx=0: Props(22.5)
            (l4_out,) = node.patch(patcher, chunks=4)
            self.assertIsNot(l4_out, patcher)
            self.assertIn('diffusion_model.blocks.0.mlp.forward', l4_out.patches)
        finally:
            torch.cuda.is_available = orig_avail
            torch.cuda.get_device_properties = orig_props
if __name__=='__main__':unittest.main()
