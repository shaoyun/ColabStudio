"""ColabStudio-owned supervisor. Executed only inside an explicitly allocated Colab VM."""
import fcntl, hashlib, importlib.util, json, os, pathlib, re, shutil, subprocess, sys, tarfile, threading, time, urllib.request
from studio_progress import ProgressReporter
ROOT = pathlib.Path(__file__).resolve().parent
CFG = json.loads((ROOT / 'deployment.json').read_text())
LOCK = json.loads((ROOT / 'lock.json').read_text())
DATA = ROOT / 'data'
DATA.mkdir(exist_ok=True)
STATE = {'owner': CFG['id'], 'binding': CFG['binding'], 'version': LOCK['toybox'], 'stage': 'installing', 'message': 'Preparing environment', 'busy': True}
MUTEX = threading.RLock()
PROCS = {}
CANCEL = threading.Event()
DRAIN = ROOT / 'draining'
COMFY = ROOT / 'ComfyUI'
APP = ROOT / 'minimax-h3-colab' / 'toybox'
DEPLOYING = True
PROGRESS=ProgressReporter(ROOT)

def update(**kw):
    with MUTEX:
        STATE.update(kw)

def atomic(path, value):
    tmp = path.with_suffix('.tmp')
    tmp.write_text(json.dumps(value), encoding='utf-8')
    tmp.replace(path)

def process_signature(pid):
    try:
        stat = pathlib.Path('/proc/%d/stat' % pid).read_text().rsplit(')', 1)[1].split()
        if stat[0] == 'Z':
            return None
        command = pathlib.Path('/proc/%d/cmdline' % pid).read_bytes().hex()
        return {'start': stat[19], 'command': command}
    except (OSError, IndexError):
        return None

class AdoptedProcess:
    def __init__(self, pid, signature):
        self.pid, self.signature = pid, signature
    def poll(self):
        return None if process_signature(self.pid) == self.signature else 0
    def terminate(self):
        if self.poll() is None:
            import signal
            os.kill(self.pid, signal.SIGTERM)

def persist_processes():
    records = {}
    for name, proc in list(PROCS.items()):
        signature = process_signature(proc.pid)
        if signature:
            records[name] = {'pid': proc.pid, 'signature': signature}
    atomic(ROOT / 'processes.json', records)

def adopt_processes():
    path = ROOT / 'processes.json'
    if not path.exists():
        return
    for name, entry in json.loads(path.read_text()).items():
        if name in {'installer','comfyui','toybox','tunnel'} and process_signature(entry['pid']) == entry['signature']:
            PROCS[name] = AdoptedProcess(entry['pid'], entry['signature'])

def fetch(url):
    with urllib.request.urlopen(url, timeout=8) as r:
        return json.load(r)

def run(args, cwd=None):
    if CANCEL.is_set():
        raise RuntimeError('Deployment cancelled for shutdown')
    p = subprocess.Popen(args, cwd=cwd)
    PROCS['installer'] = p
    persist_processes()
    result = p.wait()
    PROCS.pop('installer', None)
    persist_processes()
    if result:
        raise RuntimeError('Installation command failed (exit %d)' % result)
    if CANCEL.is_set():
        raise RuntimeError('Deployment cancelled for shutdown')

def clone(repo, dest):
    dest = pathlib.Path(dest)
    if not (dest / '.git').is_dir():
        run(['git', 'init', str(dest)])
        run(['git', '-C', str(dest), 'remote', 'add', 'origin', 'https://github.com/' + repo + '.git'])
    run(['git', '-C', str(dest), 'fetch', '--depth', '1', 'origin', LOCK['repos'][repo]])
    run(['git', '-C', str(dest), 'checkout', '--detach', LOCK['repos'][repo]])

def spawn(name, args, cwd=None, env=None):
    old = PROCS.get(name)
    if old and old.poll() is None:
        return
    PROCS[name] = subprocess.Popen(args, cwd=cwd, env=env, stdout=open(ROOT / (name + '.log'), 'a'), stderr=subprocess.STDOUT, start_new_session=True)
    persist_processes()

def wait_http(url):
    for _ in range(180):
        if CANCEL.is_set():
            raise RuntimeError('Deployment cancelled for shutdown')
        try:
            fetch(url)
            return
        except Exception:
            time.sleep(2)
    raise RuntimeError('Service readiness timed out')

def digest(path, kind='sha256'):
    h = hashlib.new(kind)
    with open(path, 'rb') as f:
        for chunk in iter(lambda: f.read(4 * 1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()

def restore():
    meta = ROOT / 'restore.json'
    if not meta.exists():
        return
    record = json.loads(meta.read_text())
    archive = ROOT / 'restore.tar.gz'
    with archive.open('wb') as out:
        for i in range(record['parts']):
            part = ROOT / ('restore.part%06d' % i)
            with part.open('rb') as f:
                shutil.copyfileobj(f, out)
    if digest(archive, 'md5') != record['md5']:
        raise RuntimeError('Restore checksum mismatch')
    with tarfile.open(archive, 'r:gz') as t:
        for member in t.getmembers():
            target = (DATA / member.name).resolve()
            if DATA.resolve() not in target.parents or not (member.isfile() or member.isdir()):
                raise RuntimeError('Unsafe archive member')
        t.extractall(DATA, filter='data')
    archive.unlink()
    for i in range(record['parts']):
        (ROOT / ('restore.part%06d' % i)).unlink(missing_ok=True)
    meta.unlink()

def deploy():
    global DEPLOYING
    try:
        PROGRESS.set(phase='preparing')
        previous = PROCS.get('installer')
        while previous and previous.poll() is None:
            if CANCEL.is_set():
                raise RuntimeError('Cancelled while recovering installer')
            time.sleep(2)
        # Probe in a child so installing a new torch does not leave stale modules loaded.
        check = "import torch,shutil,subprocess; assert torch.cuda.is_available(),'GPU missing'; assert torch.cuda.get_device_capability(0)>=(8,0),'Unsupported GPU'; assert int(subprocess.check_output(['nvidia-smi','--query-gpu=driver_version','--format=csv,noheader']).decode().split('.')[0])>=580,'Driver needs 580+'; assert shutil.disk_usage('/content').free>=72*2**30,'Need 72 GiB free'"
        if not (ROOT / 'installed.json').exists():
            run([sys.executable, '-c', check])
            PROGRESS.set(phase='repositories')
            update(message='Installing pinned ComfyUI and extensions')
            clone('Comfy-Org/ComfyUI', COMFY)
            for repo, name in [('T8mars/comfyui-minimax-h3-audio-T8', 'minimax-h3-audio-t8'), ('facok/comfyui-SelfLift', 'comfyui-SelfLift'), ('NikoDemon80/ComfyUI-H3-Motion-Context', 'ComfyUI-H3-Motion-Context')]:
                clone(repo, COMFY / 'custom_nodes' / name)
            clone('T8mars/comfyui-minimax-h3-prompt-enhancer-T8', ROOT / 'enhancer')
            clone('dseditor/minimax-h3-colab', APP.parent)
            PROGRESS.set(phase='dependencies')
            pip = LOCK['pip']
            run([sys.executable, '-m', 'pip', 'install', 'torch==' + pip['torch'], 'torchvision==' + pip['torchvision'], 'torchaudio==' + pip['torchaudio'], '--index-url', 'https://download.pytorch.org/whl/cu130'])
            constraints = ROOT / 'constraints.txt'
            constraints.write_text('\n'.join(k + '==' + v for k, v in pip.items()) + '\n')
            run([sys.executable, '-m', 'pip', 'install', '-c', str(constraints), '-r', str(COMFY / 'requirements.txt'), 'huggingface-hub==' + pip['huggingface-hub'], 'hf-xet==' + pip['hf-xet']])
            # Downloads happen in a separate process after pip, with immutable HF revisions.
            downloader = ROOT / 'download.py'
            update(message='Downloading and verifying 16 model files; see deployment log')
            run([sys.executable, '-u', str(downloader)])
            PROGRESS.value.update(json.loads((ROOT/'progress.json').read_text()))
            PROGRESS.set(phase='tunnel_setup')
            cf = ROOT / 'cloudflared'
            urllib.request.urlretrieve(LOCK['cloudflared']['url'], cf)
            if not LOCK['cloudflared'].get('sha256') or digest(cf) != LOCK['cloudflared']['sha256']:
                raise RuntimeError('cloudflared checksum mismatch')
            cf.chmod(0o700)
            atomic(ROOT / 'installed.json', {'version': LOCK['toybox']})
        PROGRESS.set(phase='restoring')
        restore()
        (DATA / 'output' / 'H3Toybox').mkdir(parents=True, exist_ok=True)
        (COMFY / 'input').mkdir(exist_ok=True)
        cfg_path = DATA / 'config.json'
        cfg = json.loads(cfg_path.read_text()) if cfg_path.exists() else {}
        cfg.update(comfy_url='http://127.0.0.1:8188', comfy_input_dir=str(COMFY / 'input'), skills_dir=str(ROOT / 'enhancer/directional_skills'), free_after_rewrite=True)
        cfg['image'] = dict(cfg.get('image') or {}, engine='ph3', installed=True)
        cfg['agent'] = dict(cfg.get('agent') or {}, provider=(cfg.get('agent') or {}).get('provider', 'gemini'))
        atomic(cfg_path, cfg)
        PROGRESS.set(phase='starting_engine')
        update(stage='starting', message='Starting ComfyUI')
        spawn('comfyui', [sys.executable, 'main.py', '--listen', '127.0.0.1', '--port', '8188', '--output-directory', str(DATA / 'output')], cwd=COMFY)
        wait_http('http://127.0.0.1:8188/system_stats')
        PROGRESS.set(phase='starting_web')
        spawn('toybox', [sys.executable, str(ROOT / 'web_runner.py')], cwd=APP, env=dict(os.environ, H3TOYBOX_PORT='8190', H3TOYBOX_DATA=str(DATA), H3TOYBOX_SECRETS=str(ROOT / 'secrets.json')))
        wait_http('http://127.0.0.1:8190/api/config')
        spawn('tunnel', [str(ROOT / 'cloudflared'), 'tunnel', '--url', 'http://127.0.0.1:8190'])
        PROGRESS.set(phase='ready')
        update(stage='online', message='Services are ready')
    except Exception as exc:
        # Do not include exception strings with potentially sensitive URLs or credentials.
        PROGRESS.set(phase='error')
        print('Deployment failed:', type(exc).__name__, flush=True)
        update(stage='failed', message='Deployment failed; inspect deployment log')
    finally:
        DEPLOYING = False

def is_busy():
    q = fetch('http://127.0.0.1:8188/queue')
    jobs = fetch('http://127.0.0.1:8190/api/jobs')
    agents = fetch('http://127.0.0.1:8190/api/agent/runs')
    if not isinstance(q.get('queue_running'), list) or not isinstance(q.get('queue_pending'), list) or not isinstance(jobs, list) or not isinstance(agents, list):
        raise RuntimeError('Unknown queue schema')
    busy_states = {'queued', 'running', 'joining', 'planning', 'rewriting', 'generating', 'revising'}
    # Only known terminal Agent states may be treated as idle.
    return bool(q['queue_running'] or q['queue_pending'] or any(j.get('status') not in {'done','error','cancelled','stopped'} for j in jobs) or any(a.get('status') not in {'done','error','stopped','cancelled'} for a in agents))

def clean_config(v):
    if isinstance(v, dict):
        return {k: clean_config(val) for k, val in v.items() if not re.search(r'(api.?key|token|secret|password)', k, re.I)}
    if isinstance(v, list):
        return [clean_config(x) for x in v]
    return v

def snapshot(nonce):
    (ROOT / 'frozen').touch()
    with open(ROOT / 'admission.lock', 'a') as admission:
        fcntl.flock(admission, fcntl.LOCK_EX)
        if STATE['stage'] != 'failed' and is_busy():
            raise RuntimeError('Queue changed during shutdown; keep waiting')
        return snapshot_locked(nonce)

def snapshot_locked(nonce):
    path = ROOT / ('backup-' + nonce + '.tar.gz')
    update(stage='backing_up', message='Creating consistent backup')
    # The web wrapper blocks all mutation while snapshotting, queue was checked idle.
    (ROOT / 'frozen').touch()
    tmp_config = ROOT / 'safe-config.json'
    cfg = json.loads((DATA / 'config.json').read_text()) if (DATA / 'config.json').exists() else {}
    atomic(tmp_config, clean_config(cfg))
    with tarfile.open(path.with_suffix('.partial'), 'w:gz') as tar:
        for p in sorted(DATA.rglob('*')):
            rel = p.relative_to(DATA)
            if p.is_symlink() or not p.is_file() or any(re.search(r'(secret|token|password|api.?key)', x, re.I) for x in rel.parts) or str(rel) == 'config.json':
                continue
            tar.add(p, arcname=str(rel), recursive=False)
        tar.add(tmp_config, arcname='config.json')
    path.with_suffix('.partial').replace(path)
    update(stage='backup_ready', backup={'file': path.name, 'size': path.stat().st_size, 'md5': digest(path, 'md5'), 'nonce': nonce}, message='Waiting for verified Google Drive upload')

if __name__ == '__main__':
    # An advisory lock prevents duplicate supervisors if a bootstrap response is lost.
    import fcntl
    lockfile = open(ROOT / 'supervisor.lock', 'a')
    try:
        fcntl.flock(lockfile, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        sys.exit(0)
    adopt_processes()
    threading.Thread(target=deploy, daemon=True).start()
    last_command = ''
    snapshot_nonce = ''
    while True:
        try:
            command_file = ROOT / 'command.json'
            cmd = json.loads(command_file.read_text()) if command_file.exists() else {}
            if cmd.get('nonce') != last_command:
                last_command = cmd.get('nonce')
                if cmd.get('action') == 'drain':
                    DRAIN.touch()
                    if DEPLOYING:
                        CANCEL.set()
                        p = PROCS.get('installer')
                        if p and p.poll() is None:
                            p.terminate()
                elif cmd.get('action') == 'resume':
                    if CANCEL.is_set():
                        raise RuntimeError('Cancelled deployment cannot resume automatically')
                    DRAIN.unlink(missing_ok=True)
                    (ROOT / 'frozen').unlink(missing_ok=True)
                    snapshot_nonce = ''
                    update(backup=None)
            if not DEPLOYING:
                if STATE['stage'] == 'failed':
                    # Failed partial installs can contain outputs after restore. Never assume idle.
                    busy = any(p.poll() is None for k,p in PROCS.items() if k in {'comfyui','toybox'})
                    update(busy=busy)
                else:
                    try:
                        busy = is_busy()
                        update(busy=busy, health=True)
                    except Exception:
                        busy = True
                        update(busy=True, health=False, message='Service health unknown; shutdown blocked')
                if DRAIN.exists() and not busy:
                    if STATE['stage'] != 'backup_ready':
                        update(stage='draining')
                    if cmd.get('action') == 'backup' and cmd.get('nonce') != snapshot_nonce:
                        snapshot(cmd['nonce'])
                        snapshot_nonce = cmd['nonce']
                elif not DRAIN.exists() and STATE['stage'] not in {'failed'}:
                    # Only restart owned processes when a healthy, installed deployment exists.
                    if not CANCEL.is_set():
                        if PROCS.get('comfyui') and PROCS['comfyui'].poll() is not None:
                            spawn('comfyui', [sys.executable, 'main.py', '--listen', '127.0.0.1', '--port', '8188', '--output-directory', str(DATA/'output')], cwd=COMFY)
                        if PROCS.get('toybox') and PROCS['toybox'].poll() is not None:
                            spawn('toybox', [sys.executable, str(ROOT / 'web_runner.py')], cwd=APP, env=dict(os.environ,H3TOYBOX_PORT='8190',H3TOYBOX_DATA=str(DATA),H3TOYBOX_SECRETS=str(ROOT/'secrets.json')))
                        if PROCS.get('tunnel') and PROCS['tunnel'].poll() is not None:
                            spawn('tunnel',[str(ROOT/'cloudflared'),'tunnel','--url','http://127.0.0.1:8190'])
            tunnel_log = ROOT / 'tunnel.log'
            if tunnel_log.exists():
                with tunnel_log.open('rb') as f:
                    f.seek(max(0, tunnel_log.stat().st_size-16384))
                    urls = re.findall(r'https://[a-z0-9-]+\.trycloudflare\.com', f.read().decode('utf-8','replace'))
                if urls:
                    update(url=urls[-1])
        except Exception as exc:
            print('Supervisor operation failed:', type(exc).__name__, flush=True)
            update(message='Supervisor operation failed; shutdown blocked', busy=True)
        update(heartbeat=time.time())
        with MUTEX:
            atomic(ROOT / 'state.json', dict(STATE))
        time.sleep(5)
