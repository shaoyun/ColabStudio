"""Load pinned Toybox without forking its UI; add a shutdown admission gate."""
import importlib.util, pathlib, sys, urllib.parse, fcntl
ROOT = pathlib.Path(__file__).resolve().parent
APP = ROOT / 'minimax-h3-colab' / 'toybox'
from toybox_zh_cn import localize
localize(APP)
sys.path.insert(0, str(APP))
spec = importlib.util.spec_from_file_location('toybox', APP / 'server.py')
m = importlib.util.module_from_spec(spec)
sys.modules['toybox'] = m
spec.loader.exec_module(m)
from toybox_auth import AccessGuard
access=AccessGuard(ROOT)
original_get=m.Handler.do_GET
original = m.Handler.do_POST

def guarded_get(self):
    if access.check(self):
        return original_get(self)

def guarded(self):
    if not access.check(self):
        return
    with open(ROOT / 'admission.lock', 'a') as admission:
        fcntl.flock(admission, fcntl.LOCK_SH)
        return guarded_locked(self)

def guarded_locked(self):
    path = urllib.parse.urlparse(self.path).path
    if (ROOT / 'frozen').exists() or ((ROOT / 'draining').exists() and path in {'/api/generate','/api/agent/start','/api/agent/revise'}):
        return self.send_json({'error': 'Service is shutting down; new tasks are disabled.'}, 409)
    return original(self)

m.Handler.do_GET = guarded_get
m.Handler.do_POST = guarded
m.LIBRARY.mkdir(parents=True, exist_ok=True)
m.OUTPUTS.mkdir(parents=True, exist_ok=True)
m.restore_inputs(m.load_config())
m.MONITOR.start()
m.ThreadingHTTPServer(('127.0.0.1', m.PORT), m.Handler).serve_forever()
