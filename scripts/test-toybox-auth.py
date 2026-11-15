"""Local HTTP security tests. No Colab, Google OAuth, model or real passwords."""
import base64, hashlib, hmac, http.client, http.server, json, pathlib, secrets, sys, tempfile, threading, time, unittest, urllib.parse
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]/'templates'))
from toybox_auth import AccessGuard
class AccessTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.root=pathlib.Path(self.tmp.name);self.password='fixture-password'
  self.config={'owner':'owned','revision':'first','salt':'11'*16,'hash':hashlib.pbkdf2_hmac('sha256',self.password.encode(),bytes.fromhex('11'*16),100000).hex(),'session_key':'22'*32,'iterations':100000}
  self.write_config();self.guard=AccessGuard(self.root);self.generated=0;outer=self
  class Handler(http.server.BaseHTTPRequestHandler):
   def do_GET(self):
    if outer.guard.check(self):self.send_response(200);self.end_headers();self.wfile.write(b'PRIVATE TOYBOX CONTENT')
   def do_POST(self):
    if outer.guard.check(self):outer.generated+=1;self.send_response(200);self.end_headers();self.wfile.write(b'generated')
   def log_message(self,*args):pass
  self.server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler);self.thread=threading.Thread(target=self.server.serve_forever,daemon=True);self.thread.start()
 def tearDown(self):self.server.shutdown();self.server.server_close();self.thread.join();self.tmp.cleanup()
 def write_config(self):(self.root/'web-auth.json').write_text(json.dumps(self.config))
 def request(self,method,path,body=None,headers=None):
  h={'Host':'private.trycloudflare.com','CF-Connecting-IP':'192.0.2.1'};h.update(headers or {});c=http.client.HTTPConnection('127.0.0.1',self.server.server_port,timeout=3);c.request(method,path,body,h);r=c.getresponse();status=r.status;response=dict(r.getheaders());data=r.read();c.close();return status,response,data
 def login(self,password=None):
  status,headers,_=self.request('POST','/__colabstudio/login',urllib.parse.urlencode({'password':password or self.password}),{'Origin':'https://private.trycloudflare.com','Content-Type':'application/x-www-form-urlencoded'})
  return status,headers
 def test_link_alone_cannot_generate_or_read_outputs(self):
  self.assertEqual(self.request('GET','/')[0],303)
  self.assertEqual(self.request('GET','/outputs/video.mp4')[0],303)
  self.assertEqual(self.request('GET','/api/config')[0],401)
  self.assertEqual(self.request('POST','/api/generate','{}',{'Origin':'https://private.trycloudflare.com'})[0],401)
  self.assertEqual(self.generated,0)
 def test_correct_password_gets_protected_cookie_and_access(self):
  status,headers=self.login();self.assertEqual(status,303);cookie=headers['Set-Cookie']
  for flag in ['HttpOnly','Secure','SameSite=Strict','Max-Age=28800']:self.assertIn(flag,cookie)
  self.assertNotIn(self.password,cookie);value=cookie.split(';',1)[0]
  self.assertEqual(self.request('GET','/',headers={'Cookie':value})[0],200)
  self.assertEqual(self.request('POST','/api/generate','{}',{'Cookie':value,'Origin':'https://private.trycloudflare.com'})[0],200)
  self.assertEqual(self.generated,1)
  self.assertEqual(self.request('POST','/api/generate','{}',{'Cookie':value,'Origin':'https://evil.example'})[0],403)
  self.assertEqual(self.generated,1)
 def test_login_page_keeps_browser_origin(self):
  # Browsers send "Origin: null" for form POSTs under no-referrer, which the guard rejects.
  self.assertEqual(self.request('GET','/__colabstudio/login')[1].get('Referrer-Policy'),'same-origin')
 def test_wrong_password_and_rate_limit(self):
  for _ in range(5):self.assertEqual(self.login('wrong-secret')[0],401)
  self.assertEqual(self.login('wrong-secret')[0],429)
  self.assertEqual(self.generated,0)
 def test_rotation_and_tamper_reject_old_cookie(self):
  _,headers=self.login();cookie=headers['Set-Cookie'].split(';',1)[0]
  self.assertEqual(self.request('GET','/',headers={'Cookie':cookie+'bad'})[0],303)
  self.config['revision']='second';self.config['session_key']='33'*32;self.write_config()
  self.assertEqual(self.request('GET','/',headers={'Cookie':cookie})[0],303)
 def test_private_health_cannot_be_spoofed_through_tunnel(self):
  self.assertEqual(self.request('GET','/api/config',headers={'Host':'127.0.0.1:8190'})[0],401)
  c=http.client.HTTPConnection('127.0.0.1',self.server.server_port);c.request('GET','/api/config',headers={'Host':'127.0.0.1:8190'});r=c.getresponse();self.assertEqual(r.status,200);r.read();c.close()
  self.assertEqual(self.request('POST','/api/generate','{}',{'Host':'127.0.0.1:8190','Origin':'http://127.0.0.1:8190'})[0],401)
 def test_expired_session_and_plain_http_rejected(self):
  token=self.guard.token(self.config);payload,signature=token.rsplit('.',1)
  value=json.loads(base64.urlsafe_b64decode(payload+'='*(-len(payload)%4)));value['exp']=int(time.time()-10)
  payload=base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip('=');signature=hmac.new(bytes.fromhex(self.config['session_key']),payload.encode(),hashlib.sha256).hexdigest()
  self.assertEqual(self.request('GET','/',headers={'Cookie':'__Host-colabstudio_session='+payload+'.'+signature})[0],303)
  self.assertEqual(self.request('GET','/',headers={'X-Forwarded-Proto':'http'})[0],403)
 def test_missing_configuration_fails_closed(self):
  (self.root/'web-auth.json').unlink();self.assertEqual(self.request('GET','/')[0],503)
if __name__=='__main__':unittest.main()
