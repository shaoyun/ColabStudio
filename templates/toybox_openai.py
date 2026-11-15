"""OpenAI-compatible HTTP gateway layered cleanly on top of H3 Toybox without duplicating auth."""
import base64
import copy
import email.parser
import email.policy
import fcntl
import io
import json
import os
import pathlib
import re
import threading
import time
import urllib.parse
import uuid

from toybox_auth import OPENAI_CORS_HEADERS

MAX_BODY_BYTES = 256 * 1024 * 1024
MAX_WAIT_SECONDS = 1800

MODEL_H3 = 'MiniMaxAI/MiniMax-H3'
MODEL_REF2VA = 'MiniMaxAI/MiniMax-H3-Ref2VA'
MODEL_IMAGE = 'MiniMaxAI/MiniMax-H3-Image'
MODEL_REWRITER = 'MiniMaxAI/MiniMax-H3-Rewriter-9B'

_EXACT_PATHS = frozenset({
    '/v1/models', '/models',
    '/v1/chat/completions', '/chat/completions',
    '/v1/images/generations', '/images/generations',
    '/v1/images/edits', '/images/edits',
    '/v1/videos', '/videos',
})
_MODEL_DETAIL_RE = re.compile(r'^/(?:v1/)?models/[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)?$')
_VIDEO_ITEM_RE = re.compile(r'^/(?:v1/)?videos/[A-Za-z0-9_-]+(?:/content)?$')
_IMAGE_FILE_RE = re.compile(r'^/(?:v1/)?images/files/[A-Za-z0-9._-]+$')
_MEDIA_DOWNLOAD_RE = re.compile(
    r'^/(?:v1/)?(?:videos/[A-Za-z0-9_-]+/content|images/files/[A-Za-z0-9._-]+)$'
)
_SAFE_FILENAME_RE = re.compile(r'^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$')
_SIZE_RE = re.compile(r'^\s*(\d{2,4})\s*[x×*]\s*(\d{2,4})\s*$', re.I)
_DATA_URI_RE = re.compile(r'^data:([A-Za-z0-9.+/-]+)(?:;[^,]*)?;base64,(.+)$', re.S)
_TAG_REF_RE = re.compile(r'<\s*(Picture|Image|Video|Audio)\s+(\d+)\s*>', re.I)
_MENTION_RE = re.compile(r'@(圖片|图片|Image|影片|视频|Video|音訊|音频|Audio)\s?(\d+)', re.I)
_SECRET_QUERY_RE = re.compile(r'([?&](?:api_key|token|key)=)[^&\s#]+', re.I)
_TOYBOX_TOKEN_RE = re.compile(r'\bsk-toybox-[A-Za-z0-9_-]+\b')

_IMAGE_EXTS = {'.png', '.jpg', '.jpeg', '.webp', '.bmp'}
_VIDEO_EXTS = {'.mp4', '.mov', '.webm', '.mkv', '.avi'}
_AUDIO_EXTS = {'.wav', '.mp3', '.flac', '.ogg', '.m4a', '.aac'}
_MIME_TO_EXT = {
    'image/png': '.png',
    'image/jpeg': '.jpg',
    'image/jpg': '.jpg',
    'image/webp': '.webp',
    'image/bmp': '.bmp',
    'video/mp4': '.mp4',
    'video/quicktime': '.mov',
    'video/webm': '.webm',
    'video/x-matroska': '.mkv',
    'audio/wav': '.wav',
    'audio/x-wav': '.wav',
    'audio/mpeg': '.mp3',
    'audio/flac': '.flac',
    'audio/ogg': '.ogg',
    'audio/mp4': '.m4a',
    'audio/aac': '.aac',
}

_MODEL_ALIASES = {
    'minimaxai/minimax-h3': MODEL_H3,
    'minimax-h3': MODEL_H3,
    'minimax_h3': MODEL_H3,
    'minimax-h3-fl2va': MODEL_H3,
    'h3': MODEL_H3,
    'sora-2': MODEL_H3,
    'sora-2-pro': MODEL_H3,
    'minimaxai/minimax-h3-ref2va': MODEL_REF2VA,
    'minimax-h3-ref2va': MODEL_REF2VA,
    'minimax_h3_ref2va': MODEL_REF2VA,
    'ref2va': MODEL_REF2VA,
    'h3-ref2va': MODEL_REF2VA,
    'minimaxai/minimax-h3-image': MODEL_IMAGE,
    'minimax-h3-image': MODEL_IMAGE,
    'minimax_h3_image': MODEL_IMAGE,
    'p-h3-image': MODEL_IMAGE,
    'ph3-image': MODEL_IMAGE,
    'qwen-image': MODEL_IMAGE,
    'qwen-image-2.1': MODEL_IMAGE,
    'gpt-image-1': MODEL_IMAGE,
    'dall-e-3': MODEL_IMAGE,
    'minimaxai/minimax-h3-rewriter-9b': MODEL_REWRITER,
    'minimax-h3-rewriter-9b': MODEL_REWRITER,
    'minimax-h3-rewriter': MODEL_REWRITER,
    'qwen3.5-9b': MODEL_REWRITER,
    'qwen3.5-9b-rewriter': MODEL_REWRITER,
    'rewriter': MODEL_REWRITER,
}


def redact_requestline(text):
    text = _TOYBOX_TOKEN_RE.sub('[REDACTED]', str(text or ''))
    return _SECRET_QUERY_RE.sub(r'\1[REDACTED]', text)


def is_inference_path(raw_target):
    """Strict path check: inference routes bypass web login/CSRF and use single-pass API token auth."""
    raw_path = (raw_target or '').split('?', 1)[0]
    if not raw_path.startswith('/') or '\\' in raw_path:
        return False
    lowered = raw_path.lower()
    if '..' in raw_path or '%2e' in lowered or '%2f' in lowered or '%5c' in lowered:
        return False
    if raw_path in _EXACT_PATHS:
        return True
    return bool(
        _MODEL_DETAIL_RE.fullmatch(raw_path)
        or _VIDEO_ITEM_RE.fullmatch(raw_path)
        or _IMAGE_FILE_RE.fullmatch(raw_path)
    )


def _strip_v1(path):
    return path[3:] if path.startswith('/v1/') else path


def _is_media_download_request(h, path):
    return h.command in ('GET', 'HEAD') and bool(_MEDIA_DOWNLOAD_RE.fullmatch(path))


def _resolve_model(raw_model, default_model=None):
    raw = str(raw_model or '').strip()
    if not raw:
        return default_model
    low = raw.lower()
    if low in _MODEL_ALIASES:
        return _MODEL_ALIASES[low]
    if 'ref2va' in low:
        return MODEL_REF2VA
    if 'image' in low:
        return MODEL_IMAGE
    if 'rewriter' in low or 'qwen' in low:
        return MODEL_REWRITER
    if 'minimax' in low or 'h3' in low:
        return MODEL_H3
    return None


def _model_modality(model_id):
    if model_id == MODEL_IMAGE:
        return 'image'
    if model_id in (MODEL_H3, MODEL_REF2VA):
        return 'video'
    if model_id == MODEL_REWRITER:
        return 'chat'
    return None


def _build_models(cfg):
    now_ts = int(time.time())
    img_installed = bool((cfg.get('image') or {}).get('installed', True))
    rw = cfg.get('rewriter') or {}
    return [
        {
            'id': MODEL_H3,
            'object': 'model',
            'created': now_ts,
            'owned_by': 'minimax',
            'loaded': True,
            'modality': 'video',
            'supported_endpoints': ['/v1/videos', '/v1/chat/completions'],
            'task_modes': ['t2va', 'i2va', 'fl2va', 'l2va', 'ref2va'],
        },
        {
            'id': MODEL_REF2VA,
            'object': 'model',
            'created': now_ts,
            'owned_by': 'minimax',
            'loaded': True,
            'modality': 'video',
            'supported_endpoints': ['/v1/videos', '/v1/chat/completions'],
            'task_modes': ['ref2va'],
            'max_ref_images': 9,
            'max_ref_videos': 3,
            'max_ref_audios': 3,
        },
        {
            'id': MODEL_IMAGE,
            'object': 'model',
            'created': now_ts,
            'owned_by': 'minimax',
            'loaded': img_installed,
            'modality': 'image',
            'engine': (cfg.get('image') or {}).get('engine', 'ph3'),
            'supported_endpoints': ['/v1/images/generations', '/v1/images/edits', '/v1/chat/completions'],
        },
        {
            'id': MODEL_REWRITER,
            'object': 'model',
            'created': now_ts,
            'owned_by': 'minimax',
            'loaded': True,
            'modality': 'chat',
            'provider': rw.get('provider', 'local'),
            'supported_endpoints': ['/v1/chat/completions'],
        },
    ]


def _closest_aspect(w, h, aspects):
    target = float(w) / max(1.0, float(h))
    return min(aspects.keys(), key=lambda k: abs((aspects[k][0] / aspects[k][1]) - target))


def _parse_image_size(size_str, aspect_str, image_sizes, aspects, default_aspect='1:1', default_size='1.3K'):
    img_size = default_size if default_size in image_sizes else '1.3K'
    aspect = default_aspect
    raw_size = str(size_str or '').strip()
    if raw_size:
        low = raw_size.lower()
        if low in ('1k', '1.3k', '1.6k', '2k', '2.7k'):
            img_size = {'1k': '1K', '1.3k': '1.3K', '1.6k': '1.6K', '2k': '2K', '2.7k': '2.7K'}[low]
        elif raw_size in aspects:
            aspect = raw_size
        else:
            m = _SIZE_RE.fullmatch(raw_size)
            if m:
                w, h = max(64, int(m.group(1))), max(64, int(m.group(2)))
                aspect = _closest_aspect(w, h, aspects)
                long_side = max(w, h)
                img_size = min(image_sizes.keys(), key=lambda k: abs(image_sizes[k] - long_side))
    raw_aspect = str(aspect_str or '').strip()
    if raw_aspect and (raw_aspect == 'auto' or raw_aspect in aspects):
        aspect = raw_aspect
    return aspect, img_size


def _parse_video_size(size_str, aspect_str, res_str, aspects, has_refs=False):
    aspect = 'auto' if has_refs else '16:9'
    resolution = '720p'
    raw_size = str(size_str or '').strip()
    if raw_size:
        low = raw_size.lower()
        if low in ('480p', '720p', '1080p'):
            resolution = low
        elif raw_size in aspects:
            aspect = raw_size
        else:
            m = _SIZE_RE.fullmatch(raw_size)
            if m:
                w, h = max(64, int(m.group(1))), max(64, int(m.group(2)))
                aspect = _closest_aspect(w, h, aspects)
                short_side = min(w, h)
                resolution = '480p' if short_side <= 560 else ('720p' if short_side <= 860 else '1080p')
    raw_aspect = str(aspect_str or '').strip()
    if raw_aspect and (raw_aspect == 'auto' or raw_aspect in aspects):
        aspect = raw_aspect
    raw_res = str(res_str or '').strip().lower()
    if raw_res in ('480p', '720p', '1080p'):
        resolution = raw_res
    return aspect, resolution


def _parse_seconds(val):
    if val in (None, ''):
        return 6.0
    try:
        secs = float(str(val).strip().rstrip('sS'))
    except ValueError:
        raise ValueError('seconds must be a number between 4 and 15')
    if secs <= 0:
        raise ValueError('seconds must be positive')
    return float(max(4.0, min(15.0, secs)))


def _parse_bool(val, default=False):
    if val is None:
        return default
    if isinstance(val, bool):
        return val
    s = str(val).strip().lower()
    if not s:
        return default
    return s in ('1', 'true', 'yes', 'on')


def _coerce_media_bytes(item, kind='image', default_name='upload'):
    allowed = _IMAGE_EXTS if kind == 'image' else (_VIDEO_EXTS if kind == 'video' else _AUDIO_EXTS)
    default_ext = '.png' if kind == 'image' else ('.mp4' if kind == 'video' else '.wav')
    if isinstance(item, dict):
        item = item.get('b64_json') or item.get('url') or item.get('data') or ''
    if isinstance(item, tuple) and len(item) == 3:
        filename, ctype, raw_bytes = item
        if not raw_bytes:
            return None
        name = pathlib.Path(filename or (default_name + default_ext)).name
        ext = pathlib.Path(name).suffix.lower()
        if ext not in allowed:
            ext = _MIME_TO_EXT.get((ctype or '').split(';', 1)[0].strip().lower(), default_ext)
            if ext not in allowed:
                raise ValueError(f'Unsupported {kind} file type')
            name = (pathlib.Path(name).stem or default_name) + ext
        return name, raw_bytes
    if isinstance(item, str):
        raw = item.strip()
        if not raw:
            return None
        if raw.startswith(('http://', 'https://', 'file://')):
            raise ValueError('External URLs are disabled for media inputs; send base64 data URI or multipart upload')
        m = _DATA_URI_RE.match(raw)
        if m:
            mime = m.group(1).lower()
            b64 = re.sub(r'\s+', '', m.group(2))
            ext = _MIME_TO_EXT.get(mime, default_ext)
            if ext not in allowed:
                raise ValueError(f'Unsupported {kind} MIME type: {mime}')
            try:
                data = base64.b64decode(b64, validate=False)
            except Exception:
                raise ValueError('Invalid base64 media payload')
            return f'{default_name}{ext}', data
        try:
            data = base64.b64decode(re.sub(r'\s+', '', raw), validate=True)
        except Exception:
            raise ValueError('Invalid base64 media payload')
        if not data:
            return None
        return f'{default_name}{default_ext}', data
    raise ValueError(f'Invalid {kind} input')


def _bridge_ref_tags(prompt, n_img, n_vid, n_aud):
    """Bridge <Picture N>/<Image N>/<Video N>/<Audio N> to Toybox @ mentions and ensure all uploads are referenced."""
    counts = {'image': n_img, 'video': n_vid, 'audio': n_aud}

    def replace_angle(match):
        raw_kind = match.group(1).lower()
        idx = int(match.group(2))
        kind = 'image' if raw_kind in ('picture', 'image') else raw_kind
        if 1 <= idx <= counts[kind]:
            prefix = {'image': 'Image', 'video': 'Video', 'audio': 'Audio'}[kind]
            return f'@{prefix}{idx}'
        return match.group(0)

    bridged = _TAG_REF_RE.sub(replace_angle, prompt)
    mentioned = {'image': set(), 'video': set(), 'audio': set()}
    kind_map = {
        '圖片': 'image', '图片': 'image', 'image': 'image',
        '影片': 'video', '视频': 'video', 'video': 'video',
        '音訊': 'audio', '音频': 'audio', 'audio': 'audio',
    }
    for m in _MENTION_RE.finditer(bridged):
        k = kind_map.get(m.group(1).lower(), kind_map.get(m.group(1)))
        if k:
            mentioned[k].add(int(m.group(2)))
    extras = []
    for k, label in (('image', 'Image'), ('video', 'Video'), ('audio', 'Audio')):
        for i in range(1, counts[k] + 1):
            if i not in mentioned[k]:
                extras.append(f'@{label}{i}')
    if extras:
        bridged = bridged.rstrip() + '\n[References: ' + ' '.join(extras) + ']'
    return bridged


def _public_base_url(h):
    host = (h.headers.get('X-Forwarded-Host') or h.headers.get('Host') or '').split(',', 1)[0].strip()
    if not host:
        return ''
    proto = (h.headers.get('X-Forwarded-Proto') or '').split(',', 1)[0].strip().lower()
    if proto not in ('http', 'https'):
        hostname = host.split(':', 1)[0].lower()
        proto = 'https' if hostname.endswith('.trycloudflare.com') else 'http'
    return f'{proto}://{host}'


def _parse_chat_messages(messages):
    if not isinstance(messages, list) or not messages:
        raise ValueError('messages must be a non-empty list')
    sys_parts = []
    convo_parts = []
    images = []
    last_user_text = []

    def add_content(role, content, is_last_user=False):
        if isinstance(content, str):
            text = content.strip()
            if text:
                if role == 'system':
                    sys_parts.append(text)
                else:
                    convo_parts.append((role, 'text', text))
                    if is_last_user:
                        last_user_text.append(text)
        elif isinstance(content, list):
            for block in content:
                if not isinstance(block, dict):
                    continue
                btype = block.get('type')
                if btype in ('text', 'input_text'):
                    t = str(block.get('text') or '').strip()
                    if t:
                        if role == 'system':
                            sys_parts.append(t)
                        else:
                            convo_parts.append((role, 'text', t))
                            if is_last_user:
                                last_user_text.append(t)
                elif btype == 'image_url' and role != 'system':
                    iu = block.get('image_url')
                    src = iu.get('url') if isinstance(iu, dict) else iu
                    if src:
                        coerced = _coerce_media_bytes(src, kind='image', default_name=f'chat_img_{len(images)+1}')
                        if coerced:
                            images.append(coerced)
                            convo_parts.append((role, 'image', coerced))

    last_user_idx = -1
    for idx, msg in enumerate(messages):
        if isinstance(msg, dict) and msg.get('role') == 'user':
            last_user_idx = idx
    for idx, msg in enumerate(messages):
        if not isinstance(msg, dict):
            continue
        role = str(msg.get('role') or 'user').lower()
        add_content(role, msg.get('content'), is_last_user=(idx == last_user_idx))

    system_text = '\n\n'.join(sys_parts).strip()
    media_prompt = '\n'.join(last_user_text).strip()
    if not media_prompt:
        media_prompt = '\n'.join(val for r, k, val in convo_parts if k == 'text' and r == 'user').strip()
    # Build ordered parts for the rewriter LLM preserving prior turns cleanly.
    user_roles = [r for r, _, _ in convo_parts if r != 'system']
    has_assistant = any(r == 'assistant' for r in user_roles)
    rw_parts = []
    for r, k, val in convo_parts:
        if k == 'image':
            rw_parts.append(('image', val))
        else:
            text_val = f'{r.capitalize()}: {val}\n' if has_assistant else val
            rw_parts.append(('text', text_val))
    return system_text, media_prompt, images, rw_parts


def _split_thinking(raw):
    raw = str(raw or '').strip()
    m = re.match(r'^<think>(.*?)</think>\s*(.*)$', raw, flags=re.S)
    if m:
        return m.group(1).strip(), m.group(2).strip()
    if '</think>' in raw and '<think>' not in raw:
        think, rest = raw.split('</think>', 1)
        return think.strip(), rest.strip()
    cleaned = re.sub(r'<think>.*?</think>', '', raw, flags=re.S).strip()
    return '', cleaned


class OpenAIGateway:
    """Handles OpenAI-compatible inference endpoints cleanly separated from Toybox web auth."""

    def __init__(self, root, toybox_module, access_guard):
        self.root = pathlib.Path(root)
        self.m = toybox_module
        self.access = access_guard
        self.cache_lock = threading.Lock()

    def _send_bytes(self, h, status, content_type, data, headers=None):
        h.send_response(status)
        h.send_header('Content-Type', content_type)
        h.send_header('Cache-Control', 'no-store')
        h.send_header('X-Content-Type-Options', 'nosniff')
        for k, v in OPENAI_CORS_HEADERS.items():
            h.send_header(k, v)
        if headers:
            for k, v in headers.items():
                h.send_header(k, v)
        h.send_header('Content-Length', str(len(data)))
        h.end_headers()
        if h.command != 'HEAD':
            h.wfile.write(data)

    def _send_json(self, h, data, status=200):
        body = json.dumps(data, ensure_ascii=False).encode('utf-8')
        self._send_bytes(h, status, 'application/json; charset=utf-8', body)

    def _send_error(self, h, status, message, err_type='invalid_request_error', code=None):
        self.access.send_openai_error(h, status, message, err_type=err_type, code=code)

    def _start_sse(self, h):
        h.send_response(200)
        h.send_header('Content-Type', 'text/event-stream; charset=utf-8')
        h.send_header('Cache-Control', 'no-cache')
        h.send_header('Connection', 'close')
        h.send_header('X-Accel-Buffering', 'no')
        h.send_header('X-Content-Type-Options', 'nosniff')
        for k, v in OPENAI_CORS_HEADERS.items():
            h.send_header(k, v)
        h.end_headers()
        h.close_connection = True

    def _write_sse_json(self, h, payload):
        line = ('data: ' + json.dumps(payload, ensure_ascii=False) + '\n\n').encode('utf-8')
        h.wfile.write(line)
        h.wfile.flush()

    def _write_sse_raw(self, h, text):
        h.wfile.write(text.encode('utf-8'))
        h.wfile.flush()

    def _read_body_bytes(self, h):
        if h.headers.get('Transfer-Encoding', '').lower() == 'chunked':
            raise ValueError('Chunked transfer encoding is not supported')
        raw_len = h.headers.get('Content-Length', '').strip()
        if not raw_len:
            return b''
        try:
            length = int(raw_len)
        except ValueError:
            raise ValueError('Invalid Content-Length')
        if length < 0 or length > MAX_BODY_BYTES:
            raise ValueError('Request body too large')
        return h.rfile.read(length) if length else b''

    def _parse_json_or_multipart(self, h):
        raw = self._read_body_bytes(h)
        ctype = h.headers.get('Content-Type') or ''
        low = ctype.lower()
        if 'multipart/form-data' in low:
            head = b'Content-Type: ' + ctype.encode('latin-1', 'replace') + b'\r\nMIME-Version: 1.0\r\n\r\n'
            msg = email.parser.BytesParser(policy=email.policy.default).parsebytes(head + raw)
            if not msg.is_multipart():
                raise ValueError('Invalid multipart payload')
            fields = {}
            files = {}
            for part in msg.iter_parts():
                name = part.get_param('name', header='content-disposition')
                if not name:
                    continue
                if name.endswith('[]'):
                    name = name[:-2]
                payload = part.get_payload(decode=True) or b''
                filename = part.get_filename()
                if filename is not None:
                    files.setdefault(name, []).append((filename, part.get_content_type(), payload))
                else:
                    fields[name] = payload.decode('utf-8', 'replace')
            return fields, files
        if not raw:
            return {}, {}
        try:
            data = json.loads(raw.decode('utf-8'))
        except ValueError:
            raise ValueError('Invalid JSON request body')
        if not isinstance(data, dict):
            raise ValueError('Request JSON must be an object')
        return data, {}

    def _with_admission(self, fn):
        with open(self.root / 'admission.lock', 'a') as admission:
            fcntl.flock(admission, fcntl.LOCK_SH)
            if (self.root / 'frozen').exists() or (self.root / 'draining').exists():
                raise PermissionError('Service is shutting down or backing up; new tasks are disabled.')
            return fn()

    def handle(self, h):
        parsed = urllib.parse.urlsplit(h.path)
        path = parsed.path
        if h.command == 'OPTIONS':
            if h.headers.get('X-Forwarded-Proto', '').lower() == 'http':
                return self._send_error(h, 403, 'HTTPS required', code='https_required')
            h.send_response(204)
            for k, v in OPENAI_CORS_HEADERS.items():
                h.send_header(k, v)
            h.send_header('Content-Length', '0')
            h.end_headers()
            return

        allow_query = _is_media_download_request(h, path)
        ok, tok = self.access.check_api(h, allow_query=allow_query)
        if not ok:
            return

        rel = _strip_v1(path)
        try:
            if h.command in ('GET', 'HEAD'):
                if rel == '/models':
                    return self._handle_list_models(h)
                if rel.startswith('/models/'):
                    return self._handle_get_model(h, rel[len('/models/'):])
                if rel == '/videos':
                    return self._handle_list_videos(h, parsed.query, tok)
                if rel.startswith('/videos/') and rel.endswith('/content'):
                    vid = rel[len('/videos/'):-len('/content')]
                    return self._handle_video_content(h, vid)
                if rel.startswith('/videos/'):
                    return self._handle_get_video(h, rel[len('/videos/'):], tok)
                if rel.startswith('/images/files/'):
                    return self._handle_get_image_file(h, rel[len('/images/files/'):])
                return self._send_error(h, 405, 'Method not allowed')

            if h.command == 'POST':
                if rel == '/images/generations':
                    return self._with_admission(lambda: self._handle_images(h, tok, is_edit=False))
                if rel == '/images/edits':
                    return self._with_admission(lambda: self._handle_images(h, tok, is_edit=True))
                if rel == '/videos':
                    return self._with_admission(lambda: self._handle_create_video(h, tok))
                if rel == '/chat/completions':
                    return self._with_admission(lambda: self._handle_chat_completions(h, tok))
                return self._send_error(h, 405, 'Method not allowed')

            if h.command == 'DELETE':
                if rel.startswith('/videos/') and not rel.endswith('/content'):
                    return self._with_admission(lambda: self._handle_delete_video(h, rel[len('/videos/'):]))
                return self._send_error(h, 405, 'Method not allowed')

            return self._send_error(h, 405, 'Method not allowed')
        except PermissionError as exc:
            return self._send_error(h, 409, str(exc), err_type='conflict_error', code='service_draining')
        except FileNotFoundError as exc:
            return self._send_error(h, 404, str(exc), code='not_found')
        except ValueError as exc:
            return self._send_error(h, 400, str(exc))
        except Exception as exc:
            return self._send_error(h, 500, f'Server error: {type(exc).__name__}: {exc}', err_type='server_error')

    def _handle_list_models(self, h):
        cfg = self.m.load_config()
        return self._send_json(h, {'object': 'list', 'data': _build_models(cfg)})

    def _handle_get_model(self, h, raw_model):
        cfg = self.m.load_config()
        clean = urllib.parse.unquote(raw_model or '').strip().strip('/')
        entries = _build_models(cfg)
        for item in entries:
            if item['id'].lower() == clean.lower():
                return self._send_json(h, item)
        resolved = _MODEL_ALIASES.get(clean.lower())
        if resolved:
            for item in entries:
                if item['id'] == resolved:
                    copied = dict(item)
                    copied['id'] = clean
                    return self._send_json(h, copied)
        return self._send_error(h, 404, f"Model '{clean}' not found", code='model_not_found')

    def _run_image_job(self, cfg, prompt, ref_items, aspect, image_size, seed):
        tray = []
        for idx, (fname, data) in enumerate(ref_items[:8], start=1):
            item = self.m.add_asset(cfg, fname, data)
            tray.append({'id': item['id']})
        req = {
            'mode': 'image',
            'prompt': prompt,
            'aspect': aspect,
            'image_size': image_size,
            'tray': tray,
            'seed': seed,
        }
        job = self.m.generate_image(cfg, req)
        entry = self.m.wait_prompt(cfg, job['id'])
        self.m.refresh_jobs(cfg)
        if entry.get('status', {}).get('status_str') == 'error':
            raise RuntimeError(self.m.execution_error(entry))
        miswired = self.m.check_image_wiring(entry, [{'comfy_name': n} for n in job.get('refs', [])])
        if miswired:
            raise RuntimeError(miswired)
        info = self.m.saved_image(entry)
        if not info:
            raise RuntimeError('Image output missing from engine')
        self.m.OUTPUTS.mkdir(parents=True, exist_ok=True)
        out_name = f"openai_img_{job['id']}.png"
        out_path = self.m.OUTPUTS / out_name
        tmp_path = self.m.OUTPUTS / f".{out_name}.tmp"
        self.m.comfy_download(cfg, info, tmp_path)
        tmp_path.replace(out_path)
        return job, out_name, out_path

    def _handle_images(self, h, tok, is_edit):
        fields, files = self._parse_json_or_multipart(h)
        raw_model = str(fields.get('model') or '').strip()
        resolved = _resolve_model(raw_model, MODEL_IMAGE)
        if resolved is None:
            return self._send_error(h, 400, f"Model '{raw_model}' not found", code='model_not_found')
        if _model_modality(resolved) != 'image':
            return self._send_error(h, 400, f"Model '{raw_model}' does not support image generation", code='model_modality_mismatch')

        prompt = str(fields.get('prompt') or '').strip()
        if not prompt:
            raise ValueError('prompt is required')

        raw_imgs = []
        for k in ('image', 'images', 'ref_images'):
            if k in files:
                raw_imgs.extend(files[k])
            val = fields.get(k)
            if isinstance(val, list):
                raw_imgs.extend(val)
            elif val:
                raw_imgs.append(val)

        ref_items = []
        for idx, item in enumerate(raw_imgs[:8], start=1):
            coerced = _coerce_media_bytes(item, kind='image', default_name=f'img_ref_{idx}')
            if coerced:
                ref_items.append(coerced)
        if is_edit and not ref_items:
            raise ValueError('At least one reference image is required for /v1/images/edits')

        cfg = self.m.load_config()
        aspect, image_size = _parse_image_size(
            fields.get('size'),
            fields.get('aspect') or fields.get('aspect_ratio'),
            self.m.IMAGE_SIZES,
            self.m.ASPECTS,
            default_aspect='auto' if ref_items else '1:1',
            default_size=str(fields.get('image_size') or fields.get('resolution') or '1.3K'),
        )
        try:
            n = max(1, min(4, int(fields.get('n') or 1)))
        except (TypeError, ValueError):
            n = 1
        seed_raw = fields.get('seed')
        base_seed = None
        if seed_raw not in (None, ''):
            try:
                base_seed = int(seed_raw)
            except (TypeError, ValueError):
                raise ValueError('seed must be an integer')

        response_format = str(fields.get('response_format') or 'b64_json').strip().lower()
        base_url = _public_base_url(h)
        tok_qs = f"?api_key={urllib.parse.quote(tok, safe='')}" if tok else ''

        data_out = []
        for idx in range(n):
            cur_seed = (base_seed + idx) if base_seed is not None else None
            job, out_name, out_path = self._run_image_job(cfg, prompt, ref_items, aspect, image_size, cur_seed)
            file_url = f'{base_url}/v1/images/files/{out_name}{tok_qs}' if base_url else f'/v1/images/files/{out_name}{tok_qs}'
            if response_format == 'url':
                data_out.append({'url': file_url, 'revised_prompt': job.get('final_prompt') or prompt})
            else:
                b64 = base64.b64encode(out_path.read_bytes()).decode('ascii')
                data_out.append({'b64_json': b64, 'url': file_url, 'revised_prompt': job.get('final_prompt') or prompt})

        return self._send_json(h, {'created': int(time.time()), 'data': data_out})

    def _handle_get_image_file(self, h, raw_filename):
        name = urllib.parse.unquote(raw_filename or '')
        if not _SAFE_FILENAME_RE.fullmatch(name):
            raise FileNotFoundError('Image file not found')
        outputs_dir = self.m.OUTPUTS.resolve()
        candidate = (outputs_dir / name).resolve()
        if candidate.parent != outputs_dir or candidate.is_symlink() or not candidate.is_file():
            raise FileNotFoundError('Image file not found')
        ext = candidate.suffix.lower()
        if ext not in _IMAGE_EXTS:
            raise FileNotFoundError('Image file not found')
        mime = 'image/jpeg' if ext in ('.jpg', '.jpeg') else ('image/webp' if ext == '.webp' else 'image/png')
        self._send_bytes(
            h,
            200,
            mime,
            candidate.read_bytes(),
            headers={'Content-Disposition': f'inline; filename="{name}"'},
        )

    def _video_id(self, job_id):
        s = str(job_id)
        return s if s.startswith('video_') else f'video_{s}'

    def _find_video_job(self, cfg, video_id):
        clean = str(video_id or '').strip()
        stripped = clean[len('video_'):] if clean.startswith('video_') else clean
        jobs = self.m.refresh_jobs(cfg)
        for job in jobs:
            if job.get('mode') == 'image':
                continue
            jid = str(job.get('id') or '')
            if jid in (clean, stripped) or self._video_id(jid) == clean:
                return job
        return None

    def _format_video_object(self, job, base_url='', tok=''):
        raw_status = str(job.get('status') or 'queued')
        status_map = {
            'queued': 'queued',
            'running': 'in_progress',
            'joining': 'in_progress',
            'done': 'completed',
            'error': 'failed',
            'cancelled': 'cancelled',
        }
        status = status_map.get(raw_status, 'queued')
        vid_id = self._video_id(job['id'])
        if status == 'completed':
            progress = 100
        elif status == 'queued':
            progress = 0
        else:
            raw_pct = (job.get('progress') or {}).get('percent', 0)
            progress = max(0, min(99, int(round(float(raw_pct or 0)))))
        tok_qs = f"?api_key={urllib.parse.quote(tok, safe='')}" if tok else ''
        content_rel = f'/v1/videos/{vid_id}/content{tok_qs}'
        content_url = f'{base_url}{content_rel}' if base_url else content_rel
        err_obj = None
        if status == 'failed':
            err_obj = {
                'code': 'video_generation_failed',
                'message': str(job.get('error') or job.get('join_error') or 'Video generation failed'),
            }
        created_at = int(job.get('created') or time.time())
        completed_at = int(job['finished']) if (status == 'completed' and job.get('finished')) else (int(time.time()) if status == 'completed' else None)
        model_name = job.get('openai_model') or (MODEL_REF2VA if job.get('task') == 'Ref2VA' else MODEL_H3)
        return {
            'id': vid_id,
            'object': 'video',
            'created_at': created_at,
            'completed_at': completed_at,
            'expires_at': None,
            'status': status,
            'model': model_name,
            'prompt': job.get('prompt') or '',
            'progress': progress,
            'seconds': str(job.get('seconds') or 6),
            'size': f"{job.get('width', 1280)}x{job.get('height', 720)}",
            'task_mode': str(job.get('task') or '').lower(),
            'seed': job.get('seed'),
            'url': content_url if status == 'completed' else None,
            'error': err_obj,
        }

    def _submit_video_job(self, cfg, params, files=None, default_model=MODEL_H3):
        files = files or {}
        prompt = str(params.get('prompt') or params.get('visual_prompt') or '').strip()
        if not prompt:
            raise ValueError('prompt is required')
        raw_model = str(params.get('model') or '').strip()
        resolved = _resolve_model(raw_model, default_model)
        if resolved is None:
            raise ValueError(f"Model '{raw_model}' not found")
        if _model_modality(resolved) != 'video':
            raise ValueError(f"Model '{raw_model}' does not support video generation")

        seconds = _parse_seconds(params.get('seconds') or params.get('duration'))

        def collect_bucket(keys):
            items = []
            for k in keys:
                if k in files:
                    items.extend(files[k])
                v = params.get(k)
                if isinstance(v, list):
                    items.extend(v)
                elif v:
                    items.append(v)
            return items

        first_raw = collect_bucket(('input_reference', 'first_frame', 'first_frame_base64'))
        last_raw = collect_bucket(('last_frame', 'last_frame_base64'))
        ref_img_raw = collect_bucket(('ref_images', 'ref_images_base64', 'input_images', 'images'))
        ref_vid_raw = collect_bucket(('ref_videos', 'ref_videos_base64', 'videos'))
        ref_aud_raw = collect_bucket(('ref_audios', 'ref_audios_base64', 'audios'))

        first_item = _coerce_media_bytes(first_raw[0], 'image', 'first_frame') if first_raw else None
        last_item = _coerce_media_bytes(last_raw[0], 'image', 'last_frame') if last_raw else None
        ref_imgs = [c for c in (_coerce_media_bytes(x, 'image', f'ref_img_{i}') for i, x in enumerate(ref_img_raw[:9], 1)) if c]
        ref_vids = [c for c in (_coerce_media_bytes(x, 'video', f'ref_vid_{i}') for i, x in enumerate(ref_vid_raw[:3], 1)) if c]
        ref_auds = [c for c in (_coerce_media_bytes(x, 'audio', f'ref_aud_{i}') for i, x in enumerate(ref_aud_raw[:3], 1)) if c]

        task_hint = str(params.get('task_mode') or '').strip().lower()
        use_ref = bool(ref_imgs or ref_vids or ref_auds or task_hint == 'ref2va' or resolved == MODEL_REF2VA)
        if first_item and not (ref_imgs or ref_vids or ref_auds) and task_hint != 'ref2va':
            use_ref = False

        if use_ref:
            if first_item and len(ref_imgs) < 9:
                ref_imgs = [first_item] + ref_imgs
            if last_item and len(ref_imgs) < 9:
                ref_imgs = ref_imgs + [last_item]
            tray = []
            for fname, data in ref_imgs:
                tray.append({'id': self.m.add_asset(cfg, fname, data)['id'], 'use_audio': True, 'extend': False})
            for fname, data in ref_vids:
                tray.append({'id': self.m.add_asset(cfg, fname, data)['id'], 'use_audio': True, 'extend': False})
            for fname, data in ref_auds:
                tray.append({'id': self.m.add_asset(cfg, fname, data)['id'], 'use_audio': True, 'extend': False})
            mode = 'ref' if tray else 'text'
            prompt_for_job = _bridge_ref_tags(prompt, len(ref_imgs), len(ref_vids), len(ref_auds)) if tray else prompt
            first_id = None
            last_id = None
            has_refs = bool(tray)
        elif first_item or last_item:
            mode = 'frames'
            tray = []
            first_id = self.m.add_asset(cfg, first_item[0], first_item[1])['id'] if first_item else None
            last_id = self.m.add_asset(cfg, last_item[0], last_item[1])['id'] if last_item else None
            prompt_for_job = prompt
            has_refs = True
        else:
            mode = 'text'
            tray = []
            first_id = None
            last_id = None
            prompt_for_job = prompt
            has_refs = False

        aspect, resolution = _parse_video_size(
            params.get('size'),
            params.get('aspect') or params.get('aspect_ratio'),
            params.get('resolution'),
            self.m.ASPECTS,
            has_refs=has_refs,
        )
        seed_val = params.get('seed')
        if seed_val in (None, ''):
            seed = None
        else:
            try:
                seed = int(seed_val)
            except (TypeError, ValueError):
                raise ValueError('seed must be an integer')

        req = {
            'mode': mode,
            'prompt': prompt_for_job,
            'tray': tray,
            'first_id': first_id,
            'last_id': last_id,
            'seconds': seconds,
            'aspect': aspect,
            'resolution': resolution,
            'intensity': str(params.get('intensity') or 'off'),
            'skill': str(params.get('skill') or ''),
            'fast': _parse_bool(params.get('fast'), default=True),
            'seed': seed,
        }
        if _parse_bool(params.get('rewrite'), default=False):
            req['final_prompt'] = self.m.rewrite(cfg, req)['prompt']
        job = self.m.generate(cfg, req)
        job['openai_model'] = raw_model or resolved
        return job

    def _handle_create_video(self, h, tok):
        params, files = self._parse_json_or_multipart(h)
        cfg = self.m.load_config()
        job = self._submit_video_job(cfg, params, files=files, default_model=MODEL_H3)
        return self._send_json(h, self._format_video_object(job, base_url=_public_base_url(h), tok=tok))

    def _handle_list_videos(self, h, query, tok):
        qs = urllib.parse.parse_qs(query)
        try:
            limit = max(1, min(100, int((qs.get('limit') or ['20'])[0])))
        except ValueError:
            limit = 20
        cfg = self.m.load_config()
        base_url = _public_base_url(h)
        jobs = [j for j in self.m.refresh_jobs(cfg) if j.get('mode') != 'image']
        items = [self._format_video_object(j, base_url=base_url, tok=tok) for j in jobs[:limit]]
        return self._send_json(h, {'object': 'list', 'data': items, 'has_more': len(jobs) > len(items)})

    def _handle_get_video(self, h, raw_video_id, tok):
        clean = urllib.parse.unquote(raw_video_id or '').strip().strip('/')
        cfg = self.m.load_config()
        job = self._find_video_job(cfg, clean)
        if not job:
            return self._send_error(h, 404, f"Video '{clean}' not found", code='video_not_found')
        return self._send_json(h, self._format_video_object(job, base_url=_public_base_url(h), tok=tok))

    def _ensure_cached_video_path(self, cfg, job):
        self.m.OUTPUTS.mkdir(parents=True, exist_ok=True)
        outputs_dir = self.m.OUTPUTS.resolve()
        if job.get('full_video'):
            full = (outputs_dir / pathlib.Path(job['full_video']).name).resolve()
            if full.parent == outputs_dir and full.is_file():
                return full
        video_info = job.get('video')
        if not isinstance(video_info, dict) or not video_info.get('filename'):
            raise RuntimeError('Video output missing from engine')
        cached = (outputs_dir / f"openai_vid_{job['id']}.mp4").resolve()
        with self.cache_lock:
            if not cached.is_file():
                tmp = outputs_dir / f".openai_vid_{job['id']}.{threading.get_ident()}.tmp"
                self.m.comfy_download(cfg, video_info, tmp)
                tmp.replace(cached)
        return cached

    def _handle_video_content(self, h, raw_video_id):
        clean = urllib.parse.unquote(raw_video_id or '').strip().strip('/')
        cfg = self.m.load_config()
        job = self._find_video_job(cfg, clean)
        if not job:
            return self._send_error(h, 404, f"Video '{clean}' not found", code='video_not_found')
        if job.get('status') != 'done':
            return self._send_error(
                h,
                409,
                f"Video '{clean}' is not ready yet (status={job.get('status')})",
                err_type='conflict_error',
                code='video_not_ready',
            )
        path = self._ensure_cached_video_path(cfg, job)
        data = path.read_bytes()
        vid_id = self._video_id(job['id'])
        self._send_bytes(
            h,
            200,
            'video/mp4',
            data,
            headers={'Content-Disposition': f'inline; filename="{vid_id}.mp4"'},
        )

    def _handle_delete_video(self, h, raw_video_id):
        clean = urllib.parse.unquote(raw_video_id or '').strip().strip('/')
        cfg = self.m.load_config()
        job = self._find_video_job(cfg, clean)
        if not job:
            return self._send_error(h, 404, f"Video '{clean}' not found", code='video_not_found')
        if job.get('status') in ('queued', 'running', 'joining'):
            return self._send_error(
                h,
                409,
                'Cannot delete an in-progress video job',
                err_type='conflict_error',
                code='job_in_progress',
            )
        jid = job['id']
        with self.m.lock:
            jobs = self.m.load_json(self.m.JOBS_PATH, [])
            save_list = [j for j in jobs if j.get('id') != jid]
            self.m.save_json(self.m.JOBS_PATH, save_list)
        cached = self.m.OUTPUTS / f'openai_vid_{jid}.mp4'
        cached.unlink(missing_ok=True)
        return self._send_json(h, {'id': self._video_id(jid), 'object': 'video.deleted', 'deleted': True})

    def _handle_chat_completions(self, h, tok):
        body, _ = self._parse_json_or_multipart(h)
        raw_model = str(body.get('model') or MODEL_REWRITER).strip()
        resolved = _resolve_model(raw_model, MODEL_REWRITER)
        if resolved is None:
            return self._send_error(h, 404, f"Model '{raw_model}' not found", code='model_not_found')

        modality = _model_modality(resolved)
        stream = _parse_bool(body.get('stream'), default=False)
        system_text, media_prompt, images, rw_parts = _parse_chat_messages(body.get('messages'))
        cmpl_id = f'chatcmpl-{uuid.uuid4().hex[:20]}'
        created_ts = int(time.time())
        base_url = _public_base_url(h)
        tok_qs = f"?api_key={urllib.parse.quote(tok, safe='')}" if tok else ''
        cfg = self.m.load_config()

        if modality == 'image':
            if not media_prompt:
                raise ValueError('Prompt message cannot be empty')
            aspect, image_size = _parse_image_size(
                body.get('size'),
                body.get('aspect') or body.get('aspect_ratio'),
                self.m.IMAGE_SIZES,
                self.m.ASPECTS,
                default_aspect='auto' if images else '1:1',
                default_size=str(body.get('image_size') or body.get('resolution') or '1.3K'),
            )
            _, out_name, _ = self._run_image_job(cfg, media_prompt, images[:8], aspect, image_size, body.get('seed'))
            img_url = f'{base_url}/v1/images/files/{out_name}{tok_qs}' if base_url else f'/v1/images/files/{out_name}{tok_qs}'
            answer = f'![Generated Image]({img_url})\n\n下载地址：{img_url}'
            return self._finish_chat_answer(h, cmpl_id, created_ts, raw_model, answer, '', media_prompt, stream)

        if modality == 'video':
            if not media_prompt:
                raise ValueError('Prompt message cannot be empty')
            params = {
                'model': raw_model,
                'prompt': media_prompt,
                'seconds': body.get('seconds') or body.get('duration'),
                'size': body.get('size'),
                'aspect': body.get('aspect') or body.get('aspect_ratio'),
                'resolution': body.get('resolution'),
                'seed': body.get('seed'),
                'fast': body.get('fast', True),
                'rewrite': body.get('rewrite', False),
            }
            if images:
                if resolved == MODEL_REF2VA or len(images) > 2:
                    params['ref_images'] = images[:9]
                else:
                    params['first_frame'] = images[0]
                    if len(images) >= 2:
                        params['last_frame'] = images[1]
            job = self._submit_video_job(cfg, params, default_model=resolved)
            return self._stream_or_wait_video_chat(h, cfg, job['id'], cmpl_id, created_ts, raw_model, media_prompt, base_url, tok_qs, stream)

        return self._handle_rewriter_chat(h, cfg, raw_model, system_text, rw_parts, body, cmpl_id, created_ts, stream)

    def _finish_chat_answer(self, h, cmpl_id, created_ts, model, content, reasoning, prompt_text, stream):
        if not stream:
            msg = {'role': 'assistant', 'content': content}
            if reasoning:
                msg['reasoning_content'] = reasoning
            p_tok = max(1, len(prompt_text) // 4)
            c_tok = max(1, (len(content) + len(reasoning)) // 4)
            return self._send_json(h, {
                'id': cmpl_id,
                'object': 'chat.completion',
                'created': created_ts,
                'model': model,
                'choices': [{'index': 0, 'message': msg, 'finish_reason': 'stop'}],
                'usage': {'prompt_tokens': p_tok, 'completion_tokens': c_tok, 'total_tokens': p_tok + c_tok},
            })

        self._start_sse(h)
        self._write_sse_json(h, {
            'id': cmpl_id,
            'object': 'chat.completion.chunk',
            'created': created_ts,
            'model': model,
            'choices': [{'index': 0, 'delta': {'role': 'assistant', 'content': ''}, 'finish_reason': None}],
        })
        if reasoning:
            self._write_sse_json(h, {
                'id': cmpl_id,
                'object': 'chat.completion.chunk',
                'created': created_ts,
                'model': model,
                'choices': [{'index': 0, 'delta': {'reasoning_content': reasoning}, 'finish_reason': None}],
            })
        if content:
            self._write_sse_json(h, {
                'id': cmpl_id,
                'object': 'chat.completion.chunk',
                'created': created_ts,
                'model': model,
                'choices': [{'index': 0, 'delta': {'content': content}, 'finish_reason': None}],
            })
        self._write_sse_json(h, {
            'id': cmpl_id,
            'object': 'chat.completion.chunk',
            'created': created_ts,
            'model': model,
            'choices': [{'index': 0, 'delta': {}, 'finish_reason': 'stop'}],
        })
        self._write_sse_raw(h, 'data: [DONE]\n\n')

    def _stream_or_wait_video_chat(self, h, cfg, job_id, cmpl_id, created_ts, model, prompt_text, base_url, tok_qs, stream):
        vid_id = self._video_id(job_id)
        if not stream:
            deadline = time.time() + MAX_WAIT_SECONDS
            while time.time() < deadline:
                job = self._find_video_job(cfg, job_id)
                if not job:
                    raise RuntimeError('Video job disappeared')
                if job.get('status') == 'error':
                    return self._send_error(h, 500, str(job.get('error') or 'Video generation failed'), err_type='server_error')
                if job.get('status') == 'done':
                    self._ensure_cached_video_path(cfg, job)
                    v_url = f'{base_url}/v1/videos/{vid_id}/content{tok_qs}' if base_url else f'/v1/videos/{vid_id}/content{tok_qs}'
                    answer = f"视频生成完成（Seed: `{job.get('seed')}`）。\n\n[下载 / 播放 MP4 视频]({v_url})"
                    return self._finish_chat_answer(h, cmpl_id, created_ts, model, answer, '', prompt_text, False)
                time.sleep(1.0)
            return self._send_error(h, 504, 'Video generation timed out', err_type='server_error')

        self._start_sse(h)
        self._write_sse_json(h, {
            'id': cmpl_id,
            'object': 'chat.completion.chunk',
            'created': created_ts,
            'model': model,
            'choices': [{'index': 0, 'delta': {'role': 'assistant', 'content': ''}, 'finish_reason': None}],
        })
        last_line = ''
        deadline = time.time() + MAX_WAIT_SECONDS
        final_job = None
        while time.time() < deadline:
            final_job = self._find_video_job(cfg, job_id)
            if not final_job:
                break
            status = final_job.get('status')
            if status in ('done', 'error'):
                break
            prog = final_job.get('progress') or {}
            line = f"> 状态：{status} · {prog.get('percent', 0)}%\n"
            if line != last_line:
                last_line = line
                self._write_sse_json(h, {
                    'id': cmpl_id,
                    'object': 'chat.completion.chunk',
                    'created': created_ts,
                    'model': model,
                    'choices': [{'index': 0, 'delta': {'content': line}, 'finish_reason': None}],
                })
            else:
                self._write_sse_raw(h, ': keepalive\n\n')
            time.sleep(0.5)

        if final_job and final_job.get('status') == 'done':
            self._ensure_cached_video_path(cfg, final_job)
            v_url = f'{base_url}/v1/videos/{vid_id}/content{tok_qs}' if base_url else f'/v1/videos/{vid_id}/content{tok_qs}'
            fin_txt = f"\n视频生成完成（Seed: `{final_job.get('seed')}`）。\n\n[下载 / 播放 MP4 视频]({v_url})\n"
        else:
            err_msg = (final_job or {}).get('error') or 'Video generation failed or timed out'
            fin_txt = f'\n视频生成失败：{err_msg}\n'
        self._write_sse_json(h, {
            'id': cmpl_id,
            'object': 'chat.completion.chunk',
            'created': created_ts,
            'model': model,
            'choices': [{'index': 0, 'delta': {'content': fin_txt}, 'finish_reason': None}],
        })
        self._write_sse_json(h, {
            'id': cmpl_id,
            'object': 'chat.completion.chunk',
            'created': created_ts,
            'model': model,
            'choices': [{'index': 0, 'delta': {}, 'finish_reason': 'stop'}],
        })
        self._write_sse_raw(h, 'data: [DONE]\n\n')

    def _handle_rewriter_chat(self, h, cfg, raw_model, system_text, rw_parts, body, cmpl_id, created_ts, stream):
        if not rw_parts:
            raise ValueError('Prompt message cannot be empty')
        cfg_run = copy.deepcopy(cfg)
        rw = cfg_run.setdefault('rewriter', {})
        if body.get('temperature') is not None:
            rw['temperature'] = float(body['temperature'])
        if body.get('top_p') is not None:
            rw['top_p'] = float(body['top_p'])
        max_tokens = body.get('max_completion_tokens') or body.get('max_tokens')
        if max_tokens is not None:
            rw['max_length'] = max(16, int(max_tokens))
        if 'enable_thinking' in body:
            rw['thinking'] = bool(body['enable_thinking'])

        side = int(rw.get('image_side') or 768)
        prepared_parts = []
        prompt_chars = len(system_text)
        for kind, val in rw_parts:
            if kind == 'text':
                prompt_chars += len(val)
                prepared_parts.append(('text', val))
            else:
                from PIL import Image
                with Image.open(io.BytesIO(val[1])) as im:
                    prepared_parts.append(('image', self.m.pad_square(im, side)))

        provider = rw.get('provider', 'local')
        if provider == 'gemini':
            raw_out = self.m.rewrite_gemini(rw['gemini'], rw, system_text, prepared_parts, max_tokens=max_tokens)
        elif provider == 'openai':
            raw_out = self.m.rewrite_openai(rw['openai'], rw, system_text, prepared_parts, max_tokens=max_tokens)
        else:
            raw_out = self.m.rewrite_local(cfg_run, system_text, prepared_parts, uuid.uuid4().hex[:8])

        reasoning, content = _split_thinking(raw_out)
        if not rw.get('thinking', False):
            reasoning = ''
        return self._finish_chat_answer(h, cmpl_id, created_ts, raw_model, content, reasoning, ' ' * prompt_chars, stream)
