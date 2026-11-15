"""Local HTTP protocol tests for Toybox OpenAI-compatible gateway. No Colab, GPU or network."""
import base64
import hashlib
import http.client
import json
import pathlib
import sys
import tempfile
import threading
import unittest
import urllib.parse
import uuid

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / 'templates'))
from toybox_auth import AccessGuard
from toybox_openai import (
    MODEL_H3,
    MODEL_IMAGE,
    MODEL_REF2VA,
    MODEL_REWRITER,
    OpenAIGateway,
    is_inference_path,
    redact_requestline,
)

TINY_PNG = base64.b64decode(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII='
)
FAKE_MP4 = b'\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isomTOYBOXTEST'


def _encode_multipart(fields=None, files=None):
    boundary = '----ToyboxBoundary' + uuid.uuid4().hex[:12]
    chunks = []
    for key, val in (fields or {}).items():
        chunks.append(f'--{boundary}\r\n'.encode())
        chunks.append(f'Content-Disposition: form-data; name="{key}"\r\n\r\n'.encode())
        chunks.append(str(val).encode('utf-8') + b'\r\n')
    for field_name, filename, content_type, data in (files or []):
        chunks.append(f'--{boundary}\r\n'.encode())
        chunks.append(
            (
                f'Content-Disposition: form-data; name="{field_name}"; '
                f'filename="{filename}"\r\nContent-Type: {content_type}\r\n\r\n'
            ).encode()
        )
        chunks.append(data + b'\r\n')
    chunks.append(f'--{boundary}--\r\n'.encode())
    return b''.join(chunks), f'multipart/form-data; boundary={boundary}'


class FakeToyboxModule:
    IMAGE_SIZES = {'1K': 1024, '1.3K': 1344, '1.6K': 1664, '2K': 2048, '2.7K': 2720}
    ASPECTS = {'16:9': (16, 9), '9:16': (9, 16), '1:1': (1, 1), '4:3': (4, 3), '3:4': (3, 4), '21:9': (21, 9)}

    def __init__(self, root):
        self.root = pathlib.Path(root)
        self.OUTPUTS = self.root / 'outputs'
        self.JOBS_PATH = self.root / 'jobs.json'
        self.lock = threading.Lock()
        self.assets = []
        self.jobs = []
        self.last_image_req = None
        self.last_video_req = None
        self.next_id = 0
        self.video_status_sequence = {}

    def load_config(self):
        return {
            'image': {'installed': True, 'engine': 'ph3'},
            'rewriter': {'installed': True, 'running': True, 'model': 'MiniMax-H3-Rewriter-9B', 'thinking': True},
        }

    def load_json(self, path, default=None):
        p = pathlib.Path(path)
        if not p.is_file():
            return default
        return json.loads(p.read_text())

    def save_json(self, path, data):
        pathlib.Path(path).write_text(json.dumps(data))
        if pathlib.Path(path) == self.JOBS_PATH:
            self.jobs = list(data)

    def add_asset(self, cfg, filename, body, folder=None):
        self.next_id += 1
        item = {'id': f'asset_{self.next_id}_{filename}', 'name': filename, 'size': len(body)}
        self.assets.append(item)
        return item

    def generate_image(self, cfg, req):
        self.last_image_req = req
        self.next_id += 1
        jid = f'imgjob_{self.next_id}'
        job = {
            'id': jid,
            'mode': 'image',
            'prompt': req['prompt'],
            'final_prompt': req['prompt'],
            'refs': [t['id'] for t in req.get('tray', [])],
            'status': 'done',
        }
        with self.lock:
            self.jobs.insert(0, job)
            self.save_json(self.JOBS_PATH, self.jobs)
        return job

    def wait_prompt(self, cfg, prompt_id):
        return {'status': {'status_str': 'success'}}

    def execution_error(self, entry):
        return 'unknown error'

    def check_image_wiring(self, entry, refs):
        return None

    def saved_image(self, entry):
        return {'filename': 'comfy_out.png', 'subfolder': '', 'type': 'output'}

    def generate(self, cfg, req):
        self.last_video_req = req
        self.next_id += 1
        jid = f'vidjob_{self.next_id}'
        task_name = {'ref': 'Ref2VA', 'frames': 'FL2VA'}.get(req.get('mode'), 'T2VA')
        job = {
            'id': jid,
            'mode': req.get('mode', 'text'),
            'task': task_name,
            'prompt': req['prompt'],
            'seconds': req.get('seconds', 6),
            'width': 1280 if req.get('aspect') != '9:16' else 720,
            'height': 720 if req.get('aspect') != '9:16' else 1280,
            'seed': req.get('seed') if req.get('seed') is not None else 42,
            'status': 'queued',
            'progress': {'percent': 0},
            'created': 1700000000,
        }
        with self.lock:
            self.jobs.insert(0, job)
            self.save_json(self.JOBS_PATH, self.jobs)
        return job

    def refresh_jobs(self, cfg):
        with self.lock:
            for job in self.jobs:
                seq = self.video_status_sequence.get(job['id'])
                if seq:
                    next_state = seq.pop(0)
                    job.update(next_state)
            self.save_json(self.JOBS_PATH, self.jobs)
            return list(self.jobs)

    def comfy_download(self, cfg, fileinfo, dest):
        fname = fileinfo.get('filename', '')
        data = FAKE_MP4 if fname.endswith('.mp4') else TINY_PNG
        pathlib.Path(dest).write_bytes(data)

    def rewrite(self, cfg, req):
        return {'prompt': req['prompt'] + ' [rewritten]'}

    def rewrite_local(self, cfg, system, parts, job_id=None):
        user_texts = [val for kind, val in parts if kind == 'text']
        return f'<think>thinking-step-1</think>\ncinematic: {" ".join(user_texts)} (sys={system or "-"})'


class OpenAIGatewayTests(unittest.TestCase):
    def setUp(self):
        import http.server

        self.tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tmp.name)
        self.token = 'sk-toybox-local-gateway-test-01'
        cfg = {
            'owner': 'owned',
            'revision': 'first',
            'salt': '11' * 16,
            'hash': '22' * 32,
            'session_key': '33' * 32,
            'iterations': 100000,
            'api_token_hash': hashlib.sha256(self.token.encode()).hexdigest(),
        }
        (self.root / 'web-auth.json').write_text(json.dumps(cfg))
        self.guard = AccessGuard(self.root)
        self.fake = FakeToyboxModule(self.root)
        self.gateway = OpenAIGateway(self.root, self.fake, self.guard)
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                assert is_inference_path(self.path)
                return outer.gateway.handle(self)

            def do_HEAD(self):
                assert is_inference_path(self.path)
                return outer.gateway.handle(self)

            def do_POST(self):
                assert is_inference_path(self.path)
                return outer.gateway.handle(self)

            def do_DELETE(self):
                assert is_inference_path(self.path)
                return outer.gateway.handle(self)

            def do_OPTIONS(self):
                assert is_inference_path(self.path)
                return outer.gateway.handle(self)

            def log_message(self, *args):
                pass

        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.tmp.cleanup()

    def request(self, method, path, body=None, headers=None, auth=True):
        h = {'Host': 'demo.trycloudflare.com', 'X-Forwarded-Proto': 'https'}
        if auth:
            h['Authorization'] = f'Bearer {self.token}'
        if headers:
            h.update(headers)
        conn = http.client.HTTPConnection('127.0.0.1', self.server.server_port, timeout=5)
        conn.request(method, path, body, h)
        resp = conn.getresponse()
        st = resp.status
        rh = dict(resp.getheaders())
        data = resp.read()
        conn.close()
        return st, rh, data

    def test_models_list_detail_aliases_and_not_found(self):
        st, _, data = self.request('GET', '/v1/models')
        self.assertEqual(st, 200)
        models = json.loads(data.decode())
        self.assertEqual(models['object'], 'list')
        ids = {m['id'] for m in models['data']}
        self.assertEqual(ids, {MODEL_H3, MODEL_REF2VA, MODEL_IMAGE, MODEL_REWRITER})

        st_detail, _, detail_raw = self.request('GET', '/v1/models/MiniMaxAI/MiniMax-H3-Image')
        self.assertEqual(st_detail, 200)
        self.assertEqual(json.loads(detail_raw.decode())['modality'], 'image')

        st_alias, _, alias_raw = self.request('GET', '/models/sora-2')
        self.assertEqual(st_alias, 200)
        self.assertEqual(json.loads(alias_raw.decode())['id'], 'sora-2')

        st_404, _, _ = self.request('GET', '/v1/models/nonexistent-model')
        self.assertEqual(st_404, 404)

    def test_images_generation_edit_and_file_download(self):
        payload = json.dumps({
            'model': 'MiniMaxAI/MiniMax-H3-Image',
            'prompt': 'sunset lantern',
            'size': '1024x1024',
            'response_format': 'b64_json',
            'seed': 123,
        })
        st, _, data = self.request('POST', '/v1/images/generations', payload, {'Content-Type': 'application/json'})
        self.assertEqual(st, 200)
        parsed = json.loads(data.decode())
        item = parsed['data'][0]
        self.assertEqual(base64.b64decode(item['b64_json']), TINY_PNG)
        self.assertIn('/v1/images/files/openai_img_', item['url'])
        self.assertIn('api_key=', item['url'])

        parsed_url = urllib.parse.urlsplit(item['url'])
        file_target = f'{parsed_url.path}?{parsed_url.query}'
        # Unauthenticated media download fails; signed api_key query succeeds without Authorization header
        st_unauth, _, _ = self.request('GET', parsed_url.path, auth=False)
        self.assertEqual(st_unauth, 401)
        st_dl, hdrs_dl, body_dl = self.request('GET', file_target, auth=False)
        self.assertEqual(st_dl, 200)
        self.assertEqual(hdrs_dl.get('Content-Type'), 'image/png')
        self.assertEqual(body_dl, TINY_PNG)
        st_head, _, body_head = self.request('HEAD', file_target, auth=False)
        self.assertEqual(st_head, 200)
        self.assertEqual(body_head, b'')

        # Image edit multipart upload
        body_mp, ctype = _encode_multipart(
            fields={'model': 'gpt-image-1', 'prompt': 'add snow', 'response_format': 'url'},
            files=[('image', 'src.png', 'image/png', TINY_PNG)],
        )
        st_ed, _, data_ed = self.request('POST', '/v1/images/edits', body_mp, {'Content-Type': ctype})
        self.assertEqual(st_ed, 200)
        ed_json = json.loads(data_ed.decode())
        self.assertNotIn('b64_json', ed_json['data'][0])
        self.assertEqual(len(self.fake.last_image_req['tray']), 1)

        # Edit without reference image -> 400
        body_empty, ctype_empty = _encode_multipart(fields={'prompt': 'no image'})
        st_bad, _, _ = self.request('POST', '/v1/images/edits', body_empty, {'Content-Type': ctype_empty})
        self.assertEqual(st_bad, 400)

    def test_videos_t2va_fl2va_ref2va_lifecycle_and_delete(self):
        # 1. T2VA JSON create
        st, _, raw = self.request(
            'POST',
            '/v1/videos',
            json.dumps({'model': 'sora-2', 'prompt': 'ocean waves', 'seconds': 6, 'size': '1280x720'}),
            {'Content-Type': 'application/json'},
        )
        self.assertEqual(st, 200)
        v1 = json.loads(raw.decode())
        vid = v1['id']
        self.assertTrue(vid.startswith('video_'))
        self.assertEqual(v1['status'], 'queued')
        self.assertEqual(self.fake.last_video_req['mode'], 'text')

        # Content before completion -> 409; DELETE while queued -> 409
        st_unready, _, _ = self.request('GET', f'/v1/videos/{vid}/content')
        self.assertEqual(st_unready, 409)
        st_del_active, _, _ = self.request('DELETE', f'/v1/videos/{vid}')
        self.assertEqual(st_del_active, 409)

        # Advance job to done
        raw_jid = vid[len('video_'):]
        self.fake.video_status_sequence[raw_jid] = [
            {'status': 'done', 'finished': 1700000100, 'video': {'filename': 'final.mp4'}}
        ]
        st_get, _, raw_get = self.request('GET', f'/v1/videos/{vid}')
        self.assertEqual(st_get, 200)
        v_done = json.loads(raw_get.decode())
        self.assertEqual(v_done['status'], 'completed')
        self.assertEqual(v_done['progress'], 100)
        self.assertIn(f'/v1/videos/{vid}/content?api_key=', v_done['url'])

        content_url = urllib.parse.urlsplit(v_done['url'])
        st_mp4, hdrs_mp4, body_mp4 = self.request('GET', f'{content_url.path}?{content_url.query}', auth=False)
        self.assertEqual(st_mp4, 200)
        self.assertEqual(hdrs_mp4.get('Content-Type'), 'video/mp4')
        self.assertEqual(body_mp4, FAKE_MP4)
        self.assertTrue((self.fake.OUTPUTS / f'openai_vid_{raw_jid}.mp4').is_file())

        # List videos
        st_list, _, raw_list = self.request('GET', '/v1/videos')
        self.assertEqual(st_list, 200)
        self.assertEqual(json.loads(raw_list.decode())['data'][0]['id'], vid)

        # Delete completed video
        st_del, _, raw_del = self.request('DELETE', f'/v1/videos/{vid}')
        self.assertEqual(st_del, 200)
        self.assertTrue(json.loads(raw_del.decode())['deleted'])

        # 2. FL2VA multipart
        fl_body, fl_ct = _encode_multipart(
            fields={'model': 'MiniMaxAI/MiniMax-H3', 'prompt': 'morph smoothly', 'seconds': '8'},
            files=[
                ('first_frame', 'f.png', 'image/png', TINY_PNG),
                ('last_frame', 'l.png', 'image/png', TINY_PNG),
            ],
        )
        st_fl, _, raw_fl = self.request('POST', '/v1/videos', fl_body, {'Content-Type': fl_ct})
        self.assertEqual(st_fl, 200)
        self.assertEqual(json.loads(raw_fl.decode())['task_mode'], 'fl2va')
        self.assertEqual(self.fake.last_video_req['mode'], 'frames')
        self.assertEqual(self.fake.last_video_req['seconds'], 8)
        self.assertIsNotNone(self.fake.last_video_req['first_id'])
        self.assertIsNotNone(self.fake.last_video_req['last_id'])

        # 3. Ref2VA multipart with <Picture 1> tag bridging and extra audio fallback
        ref_body, ref_ct = _encode_multipart(
            fields={'model': MODEL_REF2VA, 'prompt': '<Picture 1> walks into frame'},
            files=[
                ('ref_images', 'p1.png', 'image/png', TINY_PNG),
                ('ref_images', 'p2.png', 'image/png', TINY_PNG),
                ('ref_audios', 'a1.wav', 'audio/wav', b'RIFF0000WAVEfmt '),
            ],
        )
        st_ref, _, raw_ref = self.request('POST', '/v1/videos', ref_body, {'Content-Type': ref_ct})
        self.assertEqual(st_ref, 200)
        self.assertEqual(json.loads(raw_ref.decode())['task_mode'], 'ref2va')
        bridged = self.fake.last_video_req['prompt']
        self.assertIn('@Image1', bridged)
        self.assertIn('@Image2', bridged)
        self.assertIn('@Audio1', bridged)
        self.assertEqual(len(self.fake.last_video_req['tray']), 3)

    def test_chat_completions_rewriter_image_and_video_streaming(self):
        # 1. Rewriter non-stream & SSE
        st_rw, _, raw_rw = self.request(
            'POST',
            '/v1/chat/completions',
            json.dumps({
                'model': MODEL_REWRITER,
                'messages': [
                    {'role': 'system', 'content': 'film style'},
                    {'role': 'user', 'content': 'cat jumps'},
                ],
                'stream': False,
            }),
            {'Content-Type': 'application/json'},
        )
        self.assertEqual(st_rw, 200)
        msg = json.loads(raw_rw.decode())['choices'][0]['message']
        self.assertIn('cinematic: cat jumps (sys=film style)', msg['content'])
        self.assertEqual(msg['reasoning_content'], 'thinking-step-1')

        st_sse, hdrs_sse, raw_sse = self.request(
            'POST',
            '/v1/chat/completions',
            json.dumps({
                'model': 'rewriter',
                'messages': [{'role': 'user', 'content': 'stream me'}],
                'stream': True,
            }),
            {'Content-Type': 'application/json'},
        )
        self.assertEqual(st_sse, 200)
        self.assertIn('text/event-stream', hdrs_sse.get('Content-Type', ''))
        sse_text = raw_sse.decode()
        self.assertIn('thinking-step-1', sse_text)
        self.assertIn('stream me', sse_text)
        self.assertIn('data: [DONE]', sse_text)

        # 2. Image chat with vision data URL
        img_uri = 'data:image/png;base64,' + base64.b64encode(TINY_PNG).decode()
        st_ic, _, raw_ic = self.request(
            'POST',
            '/v1/chat/completions',
            json.dumps({
                'model': MODEL_IMAGE,
                'messages': [{
                    'role': 'user',
                    'content': [
                        {'type': 'text', 'text': 'make it snowy'},
                        {'type': 'image_url', 'image_url': {'url': img_uri}},
                    ],
                }],
            }),
            {'Content-Type': 'application/json'},
        )
        self.assertEqual(st_ic, 200)
        ic_content = json.loads(raw_ic.decode())['choices'][0]['message']['content']
        self.assertIn('/v1/images/files/openai_img_', ic_content)

        # 3. Video chat SSE
        # Pre-seed the next video id to progress queued -> running -> done across refresh_jobs polling
        next_jid = f'vidjob_{self.fake.next_id + 1}'
        self.fake.video_status_sequence[next_jid] = [
            {'status': 'running', 'progress': {'percent': 35}},
            {'status': 'done', 'finished': 1700000200, 'video': {'filename': 'chat_out.mp4'}},
        ]
        st_vc, _, raw_vc = self.request(
            'POST',
            '/v1/chat/completions',
            json.dumps({
                'model': MODEL_H3,
                'messages': [{'role': 'user', 'content': 'drone shot'}],
                'stream': True,
            }),
            {'Content-Type': 'application/json'},
        )
        self.assertEqual(st_vc, 200)
        vc_text = raw_vc.decode()
        self.assertIn('running · 35%', vc_text)
        self.assertIn(f'/v1/videos/video_{next_jid}/content?api_key=', vc_text)
        self.assertIn('data: [DONE]', vc_text)

    def test_draining_and_frozen_admission_gate_and_redaction(self):
        (self.root / 'draining').write_text('draining')
        # Read endpoints continue working while draining
        self.assertEqual(self.request('GET', '/v1/models')[0], 200)
        self.assertEqual(self.request('GET', '/v1/videos')[0], 200)
        # Mutating inference endpoints return 409
        st_post, _, data_post = self.request(
            'POST',
            '/v1/images/generations',
            json.dumps({'prompt': 'blocked'}),
            {'Content-Type': 'application/json'},
        )
        self.assertEqual(st_post, 409)
        self.assertEqual(json.loads(data_post.decode())['error']['code'], 'service_draining')
        self.assertEqual(
            self.request(
                'POST',
                '/v1/videos',
                json.dumps({'prompt': 'blocked'}),
                {'Content-Type': 'application/json'},
            )[0],
            409,
        )
        (self.root / 'draining').unlink()

        line = (
            f'GET /v1/images/files/a.png?api_key={self.token}&ok=1 '
            f'bearer={self.token}'
        )
        redacted = redact_requestline(line)
        self.assertNotIn(self.token, redacted)
        self.assertIn('/v1/images/files/a.png?api_key=[REDACTED]&ok=1', redacted)


if __name__ == '__main__':
    unittest.main()
