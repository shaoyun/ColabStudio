"""Apply reviewed text-only localization for the pinned upstream version, offline."""
import ast
import hashlib
import json
import os
import pathlib

def localize(app, manifest=None):
    manifest = pathlib.Path(manifest or pathlib.Path(__file__).with_name('toybox-zh-cn.json'))
    patch = json.loads(manifest.read_text(encoding='utf-8'))
    pending = []
    for name, entry in patch['files'].items():
        path = pathlib.Path(app) / name
        raw = path.read_bytes()
        digest = hashlib.sha256(raw).hexdigest()
        if digest == entry['target']:
            continue
        if digest != entry['source']:
            raise RuntimeError('Toybox localization source mismatch: ' + name)
        text = ''.join(entry['lines'].get(line, line) for line in raw.decode('utf-8').splitlines(keepends=True))
        if hashlib.sha256(text.encode()).hexdigest() != entry['target']:
            raise RuntimeError('Toybox localization checksum mismatch: ' + name)
        if name.endswith('.py'):
            ast.parse(text, filename=name)
        pending.append((path, text))
    for path, text in pending:
        tmp = path.with_name(path.name + '.zh-cn.tmp')
        tmp.write_text(text, encoding='utf-8')
        os.replace(tmp, path)
    return len(pending)
