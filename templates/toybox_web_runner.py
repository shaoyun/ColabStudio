"""Load pinned Toybox UI from our fork (zh-cn localization baked in); add shutdown gating and isolated OpenAI API auth."""
import fcntl, importlib.util, pathlib, sys, urllib.parse
ROOT = pathlib.Path(__file__).resolve().parent
APP = ROOT / 'minimax-h3-colab' / 'toybox'
sys.path.insert(0, str(APP))
spec = importlib.util.spec_from_file_location('toybox', APP / 'server.py')
m = importlib.util.module_from_spec(spec)
sys.modules['toybox'] = m
spec.loader.exec_module(m)
from toybox_auth import AccessGuard
from toybox_openai import OpenAIGateway, is_inference_path, redact_requestline

access = AccessGuard(ROOT)
gateway = OpenAIGateway(ROOT, m, access)
original_get = m.Handler.do_GET
original_post = m.Handler.do_POST
original_head = getattr(m.Handler, 'do_HEAD', None)
original_log = getattr(m.Handler, 'log_message', None)
original_send_json = m.Handler.send_json

def safe_send_json(self, data, code=200):
    """Avoid cascading BrokenPipeError when handler error paths write after client disconnect."""
    try:
        return original_send_json(self, data, code)
    except (BrokenPipeError, ConnectionResetError):
        return None

def guarded_log_message(self, fmt, *args):
    if original_log is None or original_log is object:
        return
    safe_args = tuple(redact_requestline(a) if isinstance(a, str) else a for a in args)
    return original_log(self, redact_requestline(fmt), *safe_args)

def guarded_get(self):
    try:
        if is_inference_path(self.path):
            return gateway.handle(self)
        if access.check(self):
            return original_get(self)
    except (BrokenPipeError, ConnectionResetError):
        return None

def guarded_head(self):
    try:
        if is_inference_path(self.path):
            return gateway.handle(self)
        if access.check(self):
            if original_head:
                return original_head(self)
            return self.send_error(405)
    except (BrokenPipeError, ConnectionResetError):
        return None

def guarded_post(self):
    if is_inference_path(self.path):
        return gateway.handle(self)
    if not access.check(self):
        return
    with open(ROOT / 'admission.lock', 'a') as admission:
        fcntl.flock(admission, fcntl.LOCK_SH)
        return guarded_locked(self)

def guarded_delete(self):
    if is_inference_path(self.path):
        return gateway.handle(self)
    if access.check(self):
        return self.send_json({'error': 'Method not allowed'}, 405)

def guarded_options(self):
    if is_inference_path(self.path):
        return gateway.handle(self)
    if access.check(self):
        self.send_response(204)
        self.send_header('Content-Length', '0')
        self.end_headers()

def guarded_locked(self):
    path = urllib.parse.urlparse(self.path).path
    if (ROOT / 'frozen').exists() or ((ROOT / 'draining').exists() and path in {'/api/generate','/api/agent/start','/api/agent/revise'}):
        return self.send_json({'error': 'Service is shutting down; new tasks are disabled.'}, 409)
    return original_post(self)

def cleanup_orphaned_tasks():
    """Fail in-memory-only joining/agent records left over from a previous web process."""
    try:
        if m.JOBS_PATH.exists():
            with m.lock:
                jobs = m.load_json(m.JOBS_PATH, [])
                if isinstance(jobs, list):
                    changed = False
                    for job in jobs:
                        if isinstance(job, dict) and job.get('status') == 'joining':
                            job['status'] = 'error'
                            job['join_error'] = '服务重启前视频拼接未完成'
                            changed = True
                    if changed:
                        m.save_json(m.JOBS_PATH, jobs)
    except Exception:
        pass
    try:
        if m.AGENT_PATH.exists():
            with m.AGENT_LOCK:
                runs = m.load_json(m.AGENT_PATH, [])
                if isinstance(runs, list):
                    changed = False
                    for run in runs:
                        if isinstance(run, dict) and run.get('status') not in {'done', 'error', 'stopped', 'cancelled'}:
                            run['status'] = 'error'
                            run['error'] = '服务重启前短片任务未完成，已中断'
                            changed = True
                    if changed:
                        m.save_json(m.AGENT_PATH, runs)
    except Exception:
        pass

class QuietThreadingHTTPServer(m.ThreadingHTTPServer):
    def handle_error(self, request, client_address):
        if isinstance(sys.exc_info()[1], (BrokenPipeError, ConnectionResetError)):
            return
        super().handle_error(request, client_address)

def install_chunk_ffn_fallback():
    """Keep MiniMaxChunkFeedForward (401) active even when Toybox repo is pinned to an older commit."""
    if hasattr(m, 'has_chunk_ffn'):
        return
    state = {'ok': False}

    def has_chunk_ffn(cfg):
        if state['ok']:
            return True
        try:
            info = m.comfy(cfg, '/object_info/MiniMaxChunkFeedForward', timeout=5)
            if 'MiniMaxChunkFeedForward' in (info or {}):
                state['ok'] = True
        except Exception:
            pass
        return state['ok']

    orig_build_generate = m.build_generate
    chunk_id = getattr(m, 'N_CHUNK', '401')

    def patched_build_generate(cfg, final_text, chosen, task, req, size, src_job=None):
        wf, seed = orig_build_generate(cfg, final_text, chosen, task, req, size, src_job)
        width, height = size
        if chunk_id not in wf and m.N_SHIFT in wf and min(width, height) >= 1000 and has_chunk_ffn(cfg):
            wf[chunk_id] = {
                'class_type': 'MiniMaxChunkFeedForward',
                'inputs': {
                    'chunks': 4,
                    'seq_threshold': 24576,
                    'model': [m.N_SHIFT, 0],
                },
            }
            for node in wf.values():
                if node is wf[chunk_id]:
                    continue
                for k, v in node.get('inputs', {}).items():
                    if v == [m.N_SHIFT, 0]:
                        node['inputs'][k] = [chunk_id, 0]
        return wf, seed

    m.has_chunk_ffn = has_chunk_ffn
    m.build_generate = patched_build_generate


install_chunk_ffn_fallback()
m.Handler.send_json = safe_send_json
m.Handler.log_message = guarded_log_message
m.Handler.do_GET = guarded_get
m.Handler.do_HEAD = guarded_head
m.Handler.do_POST = guarded_post
m.Handler.do_DELETE = guarded_delete
m.Handler.do_OPTIONS = guarded_options
m.LIBRARY.mkdir(parents=True, exist_ok=True)
m.OUTPUTS.mkdir(parents=True, exist_ok=True)
cleanup_orphaned_tasks()
m.restore_inputs(m.load_config())
m.MONITOR.start()
QuietThreadingHTTPServer(('127.0.0.1', m.PORT), m.Handler).serve_forever()
