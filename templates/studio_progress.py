"""Small, credential-free progress records shared by deployment subprocesses."""
import json, pathlib, threading, time

class ProgressReporter:
    def __init__(self, root, total_files=0, total_bytes=0):
        self.path = pathlib.Path(root) / 'progress.json'
        self.lock = threading.RLock()
        self.value = {'phase':'preparing','fileIndex':0,'totalFiles':total_files,'totalBytes':total_bytes,'completedBytes':0,'currentBytes':0,'fileBytes':0,'speedBytes':0,'startedAt':time.time()}
        if total_files == 0 and total_bytes == 0 and self.path.exists():
            try:
                existing = json.loads(self.path.read_text(encoding='utf-8'))
                if isinstance(existing, dict):
                    valid_int = lambda x: isinstance(x, int) and not isinstance(x, bool) and x >= 0
                    tf = existing.get('totalFiles')
                    tb = existing.get('totalBytes')
                    fi = existing.get('fileIndex', 0)
                    vf = existing.get('verifiedFiles')
                    cb = existing.get('completedBytes', 0)
                    if valid_int(tf) and tf <= 1000 and valid_int(tb) and valid_int(fi) and fi <= tf and valid_int(cb) and cb <= tb:
                        self.value.update(totalFiles=tf, totalBytes=tb, fileIndex=fi, completedBytes=cb)
                        if valid_int(vf) and vf <= tf:
                            self.value['verifiedFiles'] = vf
                    self.value.update(currentFile='', currentBytes=0, fileBytes=0, speedBytes=0, etaSeconds=None)
            except Exception:
                pass
        self.last_write = 0
        self.sample_time = time.monotonic()
        self.sample_bytes = 0
    def set(self, force=True, **fields):
        with self.lock:
            if fields.get('phase') and fields['phase'] != self.value['phase']:
                self.value['startedAt'] = time.time()
            self.value.update(fields)
            now = time.time()
            if not force and now-self.last_write < 1:
                return
            self.value['updatedAt'] = now
            # Unique temporary file avoids collision with a prior crashed process.
            tmp = self.path.with_name('progress.%s.tmp' % threading.get_ident())
            tmp.write_text(json.dumps(self.value), encoding='utf-8')
            tmp.replace(self.path)
            self.last_write = now
    def begin_file(self, index, filename, size, completed):
        self.sample_time = time.monotonic()
        self.sample_bytes = 0
        self.set(phase='downloading',fileIndex=index,currentFile=filename,fileBytes=size,currentBytes=0,completedBytes=completed,speedBytes=0,etaSeconds=None)
    def advance(self, current):
        with self.lock:
            current = max(0,min(int(current),self.value['fileBytes']))
            now = time.monotonic()
            elapsed = now-self.sample_time
            fields = {'currentBytes':current}
            if elapsed >= 1:
                speed = max(0,(current-self.sample_bytes)/elapsed)
                self.sample_time,self.sample_bytes = now,current
                remaining = self.value['fileBytes']-current
                fields.update(speedBytes=speed,etaSeconds=remaining/speed if speed>0 else None)
            self.set(force=False,**fields)

def download_tqdm(base, reporter):
    class DownloadProgress(base):
        def __init__(self,*args,**kwargs):
            kwargs.pop('name',None)
            kwargs['disable'] = False
            super().__init__(*args,**kwargs)
            reporter.advance(self.n)
            reporter.sample_bytes = self.n
            reporter.sample_time = time.monotonic()
        def update(self,n=1):
            result=super().update(n)
            reporter.advance(self.n)
            return result
        def close(self):
            if hasattr(self,'n'):
                reporter.advance(self.n)
            return super().close()
    return DownloadProgress
