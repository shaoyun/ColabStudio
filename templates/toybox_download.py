"""Pinned HF downloads with real byte counters and streaming hash verification."""
import hashlib, json, pathlib, shutil
from studio_progress import ProgressReporter, download_tqdm

def download_models(root, downloader, tqdm_base):
    root=pathlib.Path(root)
    lock=json.loads((root/'lock.json').read_text())
    files=lock['models']
    reporter=ProgressReporter(root,len(files),sum(f['size'] for f in files))
    completed=0
    try:
        for index,f in enumerate(files,1):
            target=root/'ComfyUI/models'/f['folder']/pathlib.Path(f['file']).name
            target.parent.mkdir(parents=True,exist_ok=True)
            reporter.begin_file(index,target.name,f['size'],completed)
            print('MODEL',index,len(files),target.name,flush=True)
            if not target.exists():
                got=downloader(f['repo'],f['file'],revision=f['revision'],local_dir=str(root/'hf'),tqdm_class=download_tqdm(tqdm_base,reporter))
                shutil.move(str(got),target)
            if target.stat().st_size!=f['size']:
                raise RuntimeError('Model size mismatch')
            reporter.set(phase='verifying',currentBytes=0,speedBytes=0,etaSeconds=None)
            reporter.sample_time=__import__('time').monotonic()
            reporter.sample_bytes=0
            h=hashlib.sha256();count=0
            with target.open('rb') as fd:
                for block in iter(lambda:fd.read(4*1024*1024),b''):
                    h.update(block);count+=len(block);reporter.advance(count)
            if f.get('sha256') and h.hexdigest()!=f['sha256']:
                raise RuntimeError('Model checksum mismatch')
            completed+=f['size']
            reporter.set(currentBytes=f['size'],completedBytes=completed,verifiedFiles=index,speedBytes=0,etaSeconds=None)
        reporter.set(phase='guides',currentFile='',currentBytes=0,fileBytes=0)
        guides=root/'minimax-h3-colab/toybox/guides';guides.mkdir(exist_ok=True)
        for name in ['VIDEO_PROMPT_WRITING_GUIDE_ref_en.md','VIDEO_PROMPT_WRITING_GUIDE_base_en.md']:
            shutil.copy(downloader('MiniMaxAI/MiniMax-H3','docs/'+name,revision=lock['guidesRevision'],local_dir=str(root/'hf')),guides/name)
        reporter.set(phase='download_complete',speedBytes=0,etaSeconds=None)
    except Exception:
        reporter.set(phase='error',speedBytes=0,etaSeconds=None)
        raise

if __name__=='__main__':
    from huggingface_hub import hf_hub_download
    from tqdm.auto import tqdm
    download_models(pathlib.Path(__file__).resolve().parent,hf_hub_download,tqdm)
