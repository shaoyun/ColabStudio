"""Low overhead Linux resource history. No GPU allocation, CUDA context or keepalive."""
import fcntl
import json
import os
import pathlib
import subprocess
import sys
import time

INTERVAL = 15
LIMIT = 60

def cpu_counters(proc):
    values = list(map(int, (proc / 'stat').read_text().splitlines()[0].split()[1:9]))
    return sum(values), values[3] + values[4]

def collect(proc=pathlib.Path('/proc'), previous=None, gpu_run=subprocess.run):
    sample = {'time': time.time(), 'cpu': None, 'gpu': None, 'ram': None, 'vram': None}
    counters = None
    try:
        counters = cpu_counters(proc)
        if previous and counters[0] > previous[0] and counters[1] >= previous[1]:
            sample['cpu'] = max(0, min(100, 100 * (1 - (counters[1]-previous[1])/(counters[0]-previous[0]))))
        model = next((s.split(':',1)[1].strip() for s in (proc/'cpuinfo').read_text().splitlines() if s.startswith('model name')), '')
        sample.update(cpuModel=model, cores=os.cpu_count())
    except (OSError, ValueError, IndexError):
        pass
    try:
        mem = {s.split(':')[0]: int(s.split()[1])*1024 for s in (proc/'meminfo').read_text().splitlines() if ':' in s}
        total, available = mem['MemTotal'], mem['MemAvailable']
        if total > 0 and 0 <= available <= total:
            sample.update(ram=100*(total-available)/total, ramUsed=total-available, ramTotal=total)
    except (OSError, ValueError, KeyError):
        pass
    try:
        result = gpu_run(['nvidia-smi','--query-gpu=name,utilization.gpu,memory.used,memory.total','--format=csv,noheader,nounits'], capture_output=True, text=True, timeout=3, check=True)
        name, util, used, total = [v.strip() for v in result.stdout.splitlines()[0].split(',')]
        sample['gpuName'] = name
        try:
            utilization = float(util)
            if 0 <= utilization <= 100: sample['gpu'] = utilization
        except ValueError: pass
        try:
            used, total = float(used)*1048576, float(total)*1048576
            if total > 0 and 0 <= used <= total: sample.update(vram=100*used/total, vramUsed=used, vramTotal=total)
        except ValueError: pass
    except (OSError, ValueError, IndexError, subprocess.SubprocessError):
        pass
    return sample, counters

def atomic(path, value):
    tmp=path.with_suffix('.tmp')
    fd=os.open(tmp,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600)
    with os.fdopen(fd,'w') as f: json.dump(value,f,allow_nan=False)
    os.replace(tmp,path)

def main(root, owner, binding):
    root=pathlib.Path(root);root.mkdir(mode=0o700,parents=True,exist_ok=True)
    with open(root/'sampler.lock','a') as lock:
        try: fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError: return
        target=root/'metrics.json';history=[];previous=None
        try:
            data=json.loads(target.read_text())
            if data['owner']==owner and data['binding']==binding:
                history=[s for s in data['samples'] if 0 <= time.time()-s['time'] <= 900][-LIMIT:]
        except (OSError,ValueError,KeyError,TypeError): pass
        while True:
            started=time.monotonic()
            sample,previous=collect(previous=previous)
            history=[s for s in history if 0 <= sample['time']-s['time'] < 900]
            history.append(sample);history=history[-LIMIT:]
            atomic(target,{'owner':owner,'binding':binding,'samples':history})
            time.sleep(max(0.1,INTERVAL-(time.monotonic()-started)))

if __name__=='__main__': main(*sys.argv[1:])
