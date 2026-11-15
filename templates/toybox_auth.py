"""Toybox access password and signed sessions. No credentials in URLs or logs."""
import base64, hashlib, hmac, http.cookies, json, pathlib, re, secrets, threading, time, urllib.parse
LOGIN='/__colabstudio/login'
COOKIE='__Host-colabstudio_session'

class AccessGuard:
    def __init__(self,root):
        self.root=pathlib.Path(root)
        self.mutex=threading.RLock()
        self.attempts={}
        self.last_marker=0
        self.config=None
        self.load()
    def load(self):
        with self.mutex:
            c=json.loads((self.root/'web-auth.json').read_text())
            if not c.get('owner') or not re.fullmatch('[a-f0-9]{32}',c.get('salt','')) or not re.fullmatch('[a-f0-9]{64}',c.get('hash','')) or not re.fullmatch('[a-f0-9]{64}',c.get('session_key','')) or not 100000<=c.get('iterations',0)<=1000000:
                raise ValueError('Invalid access configuration')
            changed=self.config is None or self.config['revision']!=c['revision']
            self.config=c
            now=time.time()
            if changed:
                (self.root/'web-auth.json').chmod(0o600)
            if changed or now-self.last_marker>=3:
                marker={'owner':c['owner'],'revision':c['revision'],'protected':True,'updated':now}
                tmp=self.root/('auth-status.%d.tmp'%threading.get_ident());tmp.write_text(json.dumps(marker));tmp.replace(self.root/'auth-status.json');self.last_marker=now
            return c
    def token(self,c):
        payload=base64.urlsafe_b64encode(json.dumps({'exp':int(time.time()+8*3600),'nonce':secrets.token_hex(16),'revision':c['revision']},separators=(',',':')).encode()).decode().rstrip('=')
        signature=hmac.new(bytes.fromhex(c['session_key']),payload.encode(),hashlib.sha256).hexdigest()
        return payload+'.'+signature
    def valid(self,header,c):
        try:
            jar=http.cookies.SimpleCookie();jar.load(header or '')
            token=jar[COOKIE].value
            if len(token)>2048:return False
            payload,signature=token.rsplit('.',1)
            expected=hmac.new(bytes.fromhex(c['session_key']),payload.encode(),hashlib.sha256).hexdigest()
            if not hmac.compare_digest(signature,expected):return False
            v=json.loads(base64.urlsafe_b64decode(payload+'='*(-len(payload)%4)))
            return v['revision']==c['revision'] and time.time()<v['exp']<=time.time()+8*3600+5
        except Exception:return False
    def send(self,h,status,body,html=False,location=None,cookie=None):
        h.send_response(status);h.send_header('Content-Type','text/html; charset=utf-8' if html else 'application/json; charset=utf-8');h.send_header('Cache-Control','no-store');h.send_header('Referrer-Policy','same-origin');h.send_header('X-Content-Type-Options','nosniff');h.send_header('X-Frame-Options','DENY');h.send_header('Strict-Transport-Security','max-age=31536000')
        if html:h.send_header('Content-Security-Policy',"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
        if location:h.send_header('Location',location)
        if cookie:h.send_header('Set-Cookie',cookie)
        data=body.encode();h.send_header('Content-Length',str(len(data)));h.end_headers()
        if h.command!='HEAD':h.wfile.write(data)
    def page(self,message='请输入 Colab Studio 中的访问密码。'):
        # Messages are fixed constants, never reflect submitted passwords or request paths.
        return '<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Toybox 登录</title><style>body{font:16px system-ui;max-width:380px;margin:12vh auto;padding:24px;color:#20362a}input,button{font:inherit;padding:12px;box-sizing:border-box;width:100%;margin:8px 0}button{background:#287553;color:white;border:0;border-radius:6px}p{line-height:1.7}</style><h1>H3 Toybox</h1><p>'+message+'</p><form method="post" action="'+LOGIN+'"><label for="password">访问密码</label><input id="password" name="password" type="password" autocomplete="current-password" required maxlength="128"><button>登录</button></form><p>仅拿到网页链接不能使用服务。</p></html>'
    def private_read(self,h,path):
        # Old supervisors require three read-only health/queue APIs. Tunnel requests
        # preserve their public Host and add forwarding headers, so cannot use this.
        host=h.headers.get('Host','')
        forwarded=any(h.headers.get(k) for k in ['CF-Connecting-IP','X-Forwarded-For','X-Forwarded-Proto','Forwarded'])
        return h.command=='GET' and path in {'/api/config','/api/jobs','/api/agent/runs'} and host in {'127.0.0.1:8190','localhost:8190'} and h.client_address[0] in {'127.0.0.1','::1'} and not forwarded
    def check(self,h):
        try:c=self.load()
        except Exception:self.send(h,503,'{"error":"Access configuration unavailable"}');return False
        path=urllib.parse.urlsplit(h.path).path
        if self.private_read(h,path):return True
        if h.headers.get('X-Forwarded-Proto','').lower()=='http':
            self.send(h,403,'{"error":"HTTPS required"}');return False
        if h.command=='GET' and path==LOGIN:
            self.send(h,200,self.page(),True);return False
        if h.command=='POST':
            origin=urllib.parse.urlsplit(h.headers.get('Origin',''))
            if origin.scheme not in {'http','https'} or origin.netloc!=h.headers.get('Host',''):
                self.send(h,403,'{"error":"Origin rejected"}');return False
        if h.command=='POST' and path==LOGIN:
            try:length=int(h.headers.get('Content-Length','0'))
            except ValueError:length=0
            if length<=0 or length>2048:self.send(h,400,'{"error":"Invalid request"}');return False
            ip=h.headers.get('CF-Connecting-IP') or h.client_address[0]
            with self.mutex:
                now=time.time();self.attempts={k:[t for t in v if now-t<60] for k,v in self.attempts.items() if any(now-t<60 for t in v)}
                attempts=self.attempts.setdefault(ip,[])
                if len(attempts)>=5:self.send(h,429,self.page('尝试次数过多，请一分钟后重试。'),True);return False
                attempts.append(now)
            form=urllib.parse.parse_qs(h.rfile.read(length).decode('utf-8','replace'));password=form.get('password',[''])[0]
            actual=hashlib.pbkdf2_hmac('sha256',password.encode(),bytes.fromhex(c['salt']),c['iterations']).hex()
            if not hmac.compare_digest(actual,c['hash']):self.send(h,401,self.page('访问密码错误，请在 Colab Studio 中查看或复制。'),True);return False
            self.send(h,303,'',location='/',cookie=COOKIE+'='+self.token(c)+'; Path=/; Max-Age=28800; HttpOnly; Secure; SameSite=Strict');return False
        if not self.valid(h.headers.get('Cookie'),c):
            if path.startswith('/api/') or h.command!='GET':self.send(h,401,'{"error":"Authentication required"}')
            else:self.send(h,303,'',location=LOGIN)
            return False
        return True
