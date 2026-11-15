"""Local HTTP security tests. No Colab, Google OAuth, model or real passwords."""
import base64, hashlib, hmac, http.client, http.server, json, pathlib, secrets, sys, tempfile, threading, time, unittest, urllib.parse
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]/'templates'))
from toybox_auth import AccessGuard
from toybox_openai import OpenAIGateway, is_inference_path
class AccessTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.root=pathlib.Path(self.tmp.name);self.password='fixture-password'
  self.config={'owner':'owned','revision':'first','salt':'11'*16,'hash':hashlib.pbkdf2_hmac('sha256',self.password.encode(),bytes.fromhex('11'*16),100000).hex(),'session_key':'22'*32,'iterations':100000}
  self.write_config();self.guard=AccessGuard(self.root);self.generated=0;outer=self
  class Handler(http.server.BaseHTTPRequestHandler):
   def do_GET(self):
    if outer.guard.check(self):body=b'PRIVATE TOYBOX CONTENT';self.send_response(200);self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
   def do_POST(self):
    if outer.guard.check(self):
     n=int(self.headers.get('Content-Length','0') or 0)
     if n>0:self.rfile.read(n)
     outer.generated+=1;body=b'generated';self.send_response(200);self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
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


class OpenAIIsolatedAuthTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.root=pathlib.Path(self.tmp.name)
  self.password='fixture-password';self.api_token='sk-toybox-test-secret-01'
  self.config={
   'owner':'owned',
   'revision':'first',
   'salt':'11'*16,
   'hash':hashlib.pbkdf2_hmac('sha256',self.password.encode(),bytes.fromhex('11'*16),100000).hex(),
   'session_key':'22'*32,
   'iterations':100000,
   'api_token_hash':hashlib.sha256(self.api_token.encode()).hexdigest(),
  }
  self.write_config()
  self.guard=AccessGuard(self.root)
  self.web_calls=0;self.api_calls=0
  outer=self
  class FakeToybox:
   @staticmethod
   def load_config():
    return {'image':{'installed':True,'engine':'ph3'},'rewriter':{'installed':True,'running':True,'model':'MiniMax-H3-Rewriter-9B'}}
   @staticmethod
   def refresh_jobs(cfg):
    return []
   @staticmethod
   def rewrite_local(cfg,system,parts,job_id=None):
    outer.api_calls+=1
    return 'rewritten-ok'
  self.gateway=OpenAIGateway(self.root,FakeToybox(),self.guard)
  class Handler(http.server.BaseHTTPRequestHandler):
   def do_GET(self):
    if is_inference_path(self.path):return outer.gateway.handle(self)
    if outer.guard.check(self):outer.web_calls+=1;body=b'WEB_OK';self.send_response(200);self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
   def do_HEAD(self):
    if is_inference_path(self.path):return outer.gateway.handle(self)
    if outer.guard.check(self):self.send_response(200);self.send_header('Content-Length','0');self.end_headers()
   def do_POST(self):
    if is_inference_path(self.path):return outer.gateway.handle(self)
    if outer.guard.check(self):
     n=int(self.headers.get('Content-Length','0') or 0)
     if n>0:self.rfile.read(n)
     outer.web_calls+=1;body=b'WEB_POST_OK';self.send_response(200);self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
   def do_DELETE(self):
    if is_inference_path(self.path):return outer.gateway.handle(self)
    if outer.guard.check(self):self.send_response(200);self.send_header('Content-Length','0');self.end_headers()
   def do_OPTIONS(self):
    if is_inference_path(self.path):return outer.gateway.handle(self)
    if outer.guard.check(self):self.send_response(204);self.send_header('Content-Length','0');self.end_headers()
   def log_message(self,*args):pass
  self.server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler)
  self.thread=threading.Thread(target=self.server.serve_forever,daemon=True);self.thread.start()
 def tearDown(self):self.server.shutdown();self.server.server_close();self.thread.join();self.tmp.cleanup()
 def write_config(self):(self.root/'web-auth.json').write_text(json.dumps(self.config))
 def request(self,method,path,body=None,headers=None):
  h={'Host':'private.trycloudflare.com','CF-Connecting-IP':'192.0.2.1'}
  if headers:h.update(headers)
  c=http.client.HTTPConnection('127.0.0.1',self.server.server_port,timeout=3)
  c.request(method,path,body,h)
  r=c.getresponse();status=r.status;resp=dict(r.getheaders());data=r.read();c.close()
  return status,resp,data
 def login_cookie(self):
  st,hdrs,_=self.request('POST','/__colabstudio/login',urllib.parse.urlencode({'password':self.password}),{'Origin':'https://private.trycloudflare.com','Content-Type':'application/x-www-form-urlencoded'})
  self.assertEqual(st,303)
  return hdrs['Set-Cookie'].split(';',1)[0]

 def test_unauthenticated_openai_returns_json_401_no_redirect_or_www_auth(self):
  st,hdrs,data=self.request('GET','/v1/models')
  self.assertEqual(st,401)
  self.assertNotIn('Location',hdrs)
  self.assertNotIn('WWW-Authenticate',hdrs)
  self.assertEqual(hdrs.get('Access-Control-Allow-Origin'),'*')
  payload=json.loads(data.decode())
  self.assertEqual(payload['error']['code'],'invalid_api_key')

 def test_web_cookie_cannot_authenticate_openai_routes(self):
  cookie=self.login_cookie()
  st_models,_,_=self.request('GET','/v1/models',headers={'Cookie':cookie})
  self.assertEqual(st_models,401)
  st_chat,_,_=self.request('POST','/v1/chat/completions',json.dumps({'model':'rewriter','messages':[{'role':'user','content':'hi'}]}),{'Cookie':cookie,'Content-Type':'application/json'})
  self.assertEqual(st_chat,401)
  self.assertEqual(self.api_calls,0)

 def test_api_token_cannot_authenticate_web_or_native_api(self):
  st_web,hdrs,_=self.request('GET','/',headers={'Authorization':f'Bearer {self.api_token}'})
  self.assertEqual(st_web,303)
  self.assertEqual(hdrs.get('Location'),'/__colabstudio/login')
  st_api,_,_=self.request('GET','/api/config',headers={'X-API-Key':self.api_token})
  self.assertEqual(st_api,401)
  self.assertEqual(self.web_calls,0)

 def test_valid_api_token_with_cookie_basic_and_cross_origin_passes_once(self):
  cookie=self.login_cookie()
  body=json.dumps({'model':'MiniMaxAI/MiniMax-H3-Rewriter-9B','messages':[{'role':'user','content':'hello'}]})
  # Case 1: X-API-Key + web Cookie + Authorization Basic + cross-site Origin (bypass web Origin/Cookie/Basic checks)
  st1,hdrs1,data1=self.request('POST','/v1/chat/completions',body,{
   'X-API-Key':self.api_token,
   'Authorization':'Basic dXNlcjpwYXNz',
   'Cookie':cookie,
   'Origin':'https://external-client.example',
   'Content-Type':'application/json',
  })
  self.assertEqual(st1,200)
  self.assertEqual(hdrs1.get('Access-Control-Allow-Origin'),'*')
  self.assertEqual(json.loads(data1.decode())['choices'][0]['message']['content'],'rewritten-ok')
  self.assertEqual(self.api_calls,1)
  # Case 2: Bearer token + invalid Cookie + cross-site Origin
  st2,_,_=self.request('POST','/v1/chat/completions',body,{
   'Authorization':f'Bearer {self.api_token}',
   'Cookie':'__Host-colabstudio_session=tampered',
   'Origin':'https://external-client.example',
   'Content-Type':'application/json',
  })
  self.assertEqual(st2,200)
  self.assertEqual(self.api_calls,2)
  # Case 3: x-goog-api-key header + non-/v1 alias
  st3,_,_=self.request('GET','/models',headers={'x-goog-api-key':self.api_token})
  self.assertEqual(st3,200)
  self.assertEqual(self.web_calls,0)

 def test_options_cors_http_downgrade_traversal_and_unconfigured_token(self):
  st_opt,hdrs,_=self.request('OPTIONS','/v1/chat/completions',headers={'Origin':'https://external-client.example'})
  self.assertEqual(st_opt,204)
  self.assertEqual(hdrs.get('Access-Control-Allow-Origin'),'*')
  st_opt_http,_,_=self.request('OPTIONS','/v1/chat/completions',headers={'X-Forwarded-Proto':'http'})
  self.assertEqual(st_opt_http,403)
  st_get_http,_,_=self.request('GET','/v1/models',headers={'X-Forwarded-Proto':'http','Authorization':f'Bearer {self.api_token}'})
  self.assertEqual(st_get_http,403)
  # Path traversal out of /v1 must NOT match is_inference_path; falls back to web AccessGuard -> 303 redirect on GET / 401 on POST
  self.assertFalse(is_inference_path('/v1/models/../../api/config'))
  self.assertFalse(is_inference_path('/v1/models/%2e%2e/api/config'))
  st_trav,hdrs_trav,_=self.request('GET','/v1/models/../../api/config',headers={'Authorization':f'Bearer {self.api_token}'})
  self.assertEqual(st_trav,303)
  self.assertEqual(hdrs_trav.get('Location'),'/__colabstudio/login')
  st_trav_post,_,_=self.request('POST','/v1/models/../../api/generate','{}',headers={'Origin':'https://private.trycloudflare.com','Authorization':f'Bearer {self.api_token}'})
  self.assertEqual(st_trav_post,401)
  self.assertEqual(self.web_calls,0)
  # Legacy deployment without api_token_hash fails closed with explicit JSON error code
  del self.config['api_token_hash'];self.write_config()
  st_missing,_,data=self.request('GET','/v1/models',headers={'Authorization':f'Bearer {self.api_token}'})
  self.assertEqual(st_missing,401)
  self.assertEqual(json.loads(data.decode())['error']['code'],'api_token_not_configured')

if __name__=='__main__':unittest.main()
