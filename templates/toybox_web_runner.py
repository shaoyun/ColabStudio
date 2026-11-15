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

def guarded_log_message(self, fmt, *args):
    if original_log is None or original_log is object:
        return
    safe_args = tuple(redact_requestline(a) if isinstance(a, str) else a for a in args)
    return original_log(self, redact_requestline(fmt), *safe_args)

def guarded_get(self):
    if is_inference_path(self.path):
        return gateway.handle(self)
    if access.check(self):
        return original_get(self)

def guarded_head(self):
    if is_inference_path(self.path):
        return gateway.handle(self)
    if access.check(self):
        if original_head:
            return original_head(self)
        return self.send_error(405)

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

m.Handler.log_message = guarded_log_message
m.Handler.do_GET = guarded_get
m.Handler.do_HEAD = guarded_head
m.Handler.do_POST = guarded_post
m.Handler.do_DELETE = guarded_delete
m.Handler.do_OPTIONS = guarded_options
m.LIBRARY.mkdir(parents=True, exist_ok=True)
m.OUTPUTS.mkdir(parents=True, exist_ok=True)
m.restore_inputs(m.load_config())
m.MONITOR.start()
m.ThreadingHTTPServer(('127.0.0.1', m.PORT), m.Handler).serve_forever()
