// 本地 UI 预览服务器：在普通浏览器里预览桌面应用界面，不启动桌面窗口、
// 不访问 Colab、不部署任何云端资源。数据来自下方 mockBridgeJS 里的假数据桥。
//
// 用法：
//
//	go run ./scripts/preview
//	# 浏览器打开 http://127.0.0.1:8899
//
// 预览不同服务状态（H3 Toybox 服务卡）：
//
//	http://127.0.0.1:8899/?state=installing   部署中（进度条会动，约 15 秒后自动变为在线）
//	http://127.0.0.1:8899/?state=online       在线（默认）
//	http://127.0.0.1:8899/?state=stopped      未启动
//	http://127.0.0.1:8899/?state=failed       部署失败
//	http://127.0.0.1:8899/?state=disconnected 连接异常
//	http://127.0.0.1:8899/?state=lost         实例已结束
//
// 修改 web/ 下文件（index.html / style.css / app.js）后，浏览器会在 1 秒多内
// 自动刷新；也可以手动 Cmd+R。仅用于界面开发，验收仍需 go run . 跑真实应用。
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

var webDir string

func main() {
	addr := flag.String("addr", "127.0.0.1:8899", "预览服务监听地址")
	dir := flag.String("web", "", "web 目录位置，默认自动定位到仓库根目录的 web/")
	flag.Parse()

	webDir = *dir
	if webDir == "" {
		if wd, err := os.Getwd(); err == nil {
			webDir = filepath.Join(wd, "web")
		}
	}
	if _, err := os.Stat(filepath.Join(webDir, "index.html")); err != nil {
		// go run ./scripts/preview 时工作目录可能是 scripts/preview 或仓库根
		for _, cand := range []string{filepath.Join("..", "..", "web"), filepath.Join("..", "web")} {
			if _, err := os.Stat(filepath.Join(cand, "index.html")); err == nil {
				webDir, _ = filepath.Abs(cand)
				break
			}
		}
	}
	if _, err := os.Stat(filepath.Join(webDir, "index.html")); err != nil {
		log.Fatalf("找不到 web 目录（-web 可指定）：web/index.html 不存在于 %s", webDir)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page, err := composePage(true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	http.HandleFunc("/_/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, assetVersion())
	})

	log.Printf("预览地址：http://%s  （web 目录：%s）", *addr, webDir)
	log.Printf("状态预览示例：http://%s/?state=installing", *addr)
	log.Printf("假数据在 scripts/preview/main.go 的 mockBridgeJS 中修改。")
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func mustRead(name string) []byte {
	b, err := os.ReadFile(filepath.Join(webDir, name))
	if err != nil {
		log.Fatalf("读取 web/%s 失败：%v", name, err)
	}
	return b
}

func assetVersion() string {
	h := sha256.New()
	for _, name := range []string{"index.html", "style.css", "app.js", "telemetry.js", "icon.png"} {
		fmt.Fprintf(h, "%s:%d;", name, len(mustRead(name)))
		h.Write(mustRead(name))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// composePage 与 main.go 的组装方式保持一致：同样的占位符、同样的 JS 拼接顺序，
// 另外注入假数据桥（mockBridgeJS）和自动刷新脚本。
func composePage(live bool) ([]byte, error) {
	page := string(mustRead("index.html"))
	css := string(mustRead("style.css"))
	app := string(mustRead("app.js"))
	telemetry := string(mustRead("telemetry.js"))
	icon := base64.StdEncoding.EncodeToString(mustRead("icon.png"))

	// 与 main.go 相同：app.js 之后拼接 telemetry.js；预览时假数据桥必须在前，
	// 这样 app.js 调 mygo.call 时 window.mygo 已存在。
	js := mockBridgeJS + "\n" + app + "\n" + telemetry
	if live {
		js += "\n" + reloadScript(assetVersion())
		// 自动刷新用 XMLHttpRequest 轮询 /_/version；预览页 CSP 放开 connect-src 'self'。
		// 桌面应用不受影响（它由 main.go 原样组装，CSP 不变）。
		page = strings.Replace(page, "img-src data:", "img-src data:; connect-src 'self'", 1)
	}

	page = strings.NewReplacer(
		"/*__APP_CSS__*/", css,
		"/*__APP_JS__*/", js,
		"__APP_ICON__", "data:image/png;base64,"+icon,
	).Replace(page)
	return []byte(page), nil
}

func reloadScript(version string) string {
	return "(function(){var v='" + version + "';setInterval(function(){var x=new XMLHttpRequest();x.open('GET','/_/version?'+Date.now(),true);" +
		"x.onload=function(){if(x.responseText&&x.responseText!==v)location.reload()};x.onerror=function(){};x.send()},1200)})();"
}

// mockBridgeJS：页面内的假数据桥，替换桌面应用提供的 window.mygo。
// 方法名对应 Studio 上的绑定方法；返回值字段名与 Go 结构体的 JSON 标签一致。
// 想改预览数据（余额、账号、日志文本等）直接改这里的字面量。
const mockBridgeJS = `(function(){
'use strict';
if(!window.mygo){window.mygo={}}
console.log('ColabStudio 预览模式：使用本地假数据，不会连接 Colab。');

var VALID=['stopped','uncertain','allocating','allocated','installing','starting','online','draining','backing_up','backup_ready','releasing','failed','disconnected','lost'];
var state='online';
try{
 var q=new URLSearchParams(location.search).get('state');
 if(q&&VALID.indexOf(q)>=0)state=q;
}catch(e){}
var pageStart=Date.now();
var machineType='L4';
var URL_ONLINE='https://preview-toybox.trycloudflare.com';
var PASSWORD='toybox-preview-pass';
var API_TOKEN='sk-preview-demo-token';

if(state==='installing'){setTimeout(function(){state='online'},15000)}

function iso(msAgo){return new Date(Date.now()-msAgo).toISOString()}
function nowSec(){return Math.floor(Date.now()/1000)}
function deployed(){return ['allocated','installing','starting','online','draining','backing_up','backup_ready','releasing','failed','disconnected'].indexOf(state)>=0}
function onlineish(){return ['online','draining','backing_up','backup_ready'].indexOf(state)>=0}

var MESSAGES={
 stopped:'可先选择机器类型并唤醒，再连接 Drive 并部署服务。机器唤醒后即开始计费。',
 uncertain:'上一次申请结果尚未确认，已阻止重复创建；请在原实例继续部署。',
 allocating:'正在申请 L4 实例，请稍候…',
 allocated:'机器已唤醒，尚未部署。部署并启动后开始下载约 60 GB 模型。',
 installing:'正在部署：下载模型、安装 ComfyUI 与 Toybox，完成后自动上线。',
 starting:'正在启动远端服务，等待健康检查。',
 online:'服务在线。生成、素材与工作流请在 Toybox 网页中操作。',
 draining:'定时结束但任务仍在运行，等任务完成后再释放。',
 backing_up:'正在打包作品并上传 Google Drive…',
 backup_ready:'作品已保存到 Drive，等待释放确认。',
 releasing:'正在释放实例…',
 failed:'部署失败：磁盘可用空间不足（需要至少 72 GiB）。机器可能仍在计费。',
 disconnected:'与实例的连接暂时中断，正在按指数退避重连。',
 lost:'实例已结束（超时或被平台回收）。'
};

function serviceRecord(){
 var rec={
  id:'preview-instance',account:'preview@example.com',binding:'toybox',
  runtime:deployed()?'preview-runtime-01':'',
  operation:'preview-operation',postSent:false,
  state:state,message:MESSAGES[state]||'',
  url:onlineish()?URL_ONLINE:'',
  updated:iso(30000),
  heartbeat:onlineish()?iso(8000):(deployed()?iso(1800000):null),
  rule:{enabled:false,days:[1,2,3,4,5],start:'09:00',end:'18:00',zone:'Asia/Shanghai'},
  runningWindow:'',lastWindow:'',suppressed:'',
  stopRequested:['draining','backing_up','backup_ready','releasing'].indexOf(state)>=0,
  folder:'previewFolderID',archive:deployed()?'H3-Toybox/Toybox-预览.tar.gz':'',
  backupTime:onlineish()?iso(3600000*5):null,
  remoteRoot:deployed()?'/content/colabstudio':'',
  backupNonce:'',accelerator:machineType
 };
 if(state==='failed'){rec.startupError='安装脚本退出码 1：磁盘可用空间不足（需要至少 72 GiB）';rec.startupLog='[安装器] 第 21/24 个模型校验失败：SHA-256 不匹配';}
 else if(state==='installing'){rec.startupPhase='done'}
 return rec;
}

// installing 状态下进度随时间推进，方便调进度条样式。
function transferProgress(){
 var t=(Date.now()-pageStart)/1000;
 var totalBytes=64*1024*1024*1024;
 var phase=t<9?'downloading':'verifying';
 var fileIdx=Math.min(24,3+Math.floor(t/12));
 var fileBytes=2.6*1024*1024*1024;
 var frac=(t%12)/12;
 var currentBytes=Math.floor(fileBytes*(phase==='verifying'?0.85:0.55+0.4*frac));
 var doneFiles=Math.max(0,fileIdx-1);
 var verified=t<9?doneFiles:doneFiles+Math.floor((t-9)/2);
 return{
  phase:phase,
  currentFile:'h3-weights-'+String(fileIdx).padStart(4,'0')+'-of-0024.safetensors',
  fileIndex:fileIdx,totalFiles:24,
  currentBytes:currentBytes,fileBytes:fileBytes,
  completedBytes:Math.floor(totalBytes*0.42),totalBytes:totalBytes,
  speedBytes:phase==='verifying'?900*1024*1024:176*1024*1024,
  etaSeconds:Math.floor((fileBytes-currentBytes)/(176*1024*1024)),
  startedAt:nowSec()-Math.floor(t),updatedAt:nowSec(),
  verifiedFiles:Math.min(24,verified)
 };
}

function deploymentProgress(){
 var rec=serviceRecord();
 var result={message:'',legacy:false,weightsReady:onlineish(),weightsEvidence:'installation',
  startupError:rec.startupError||'',startupPhase:rec.startupPhase||''};
 if(state==='installing'){result.transfer=transferProgress()}
 else if(onlineish()){result.transfer={phase:'ready',totalFiles:24,verifiedFiles:24,currentFile:'',fileIndex:24,currentBytes:0,fileBytes:0,completedBytes:64*1024*1024*1024,totalBytes:64*1024*1024*1024,speedBytes:0,updatedAt:nowSec()}}
 else if(state==='failed'){result.transfer={phase:'error',totalFiles:24,verifiedFiles:21,currentFile:'',fileIndex:21,currentBytes:0,fileBytes:0,completedBytes:0,totalBytes:0,speedBytes:0,updatedAt:nowSec()};result.message='下载或校验未完成，请查看部署日志。'}
 else{result.transfer=null}
 result.model={phase:onlineish()?'on_demand':'',requested:'',loadedMiB:onlineish()?34567:0};
 if(onlineish()){result.remote={fresh:true,health:true,stage:'online',heartbeat:nowSec(),url:URL_ONLINE}}
 else if(state==='failed'){result.remote={fresh:true,health:false,stage:'failed',heartbeat:nowSec(),url:''}}
 else{result.remote=null}
 return result;
}

function runtimeRow(){
 return{
  name:'preview-runtime-01',displayName:deployed()?'H3 Toybox · '+machineType:'H3 Toybox',
  backend:'',version:'',
  runtimeSpec:{variant:'default',accelerator:machineType,shape:'standard'},
  connectionInfo:{url:'',expireTime:''}
 };
}

var SAMPLE_LOGS={
 deployment:'[安装器] 2026-11-15 09:12:01 检查 CUDA 驱动：13.4，符合要求\n[安装器] 2026-11-15 09:12:03 磁盘可用 118 GiB，满足 72 GiB 要求\n[安装器] 2026-11-15 09:12:10 开始下载模型（24 个文件，约 60 GiB）\n[安装器] 2026-11-15 09:31:47 文件 21/24 校验失败，重试中',
 toybox:'[toybox] listening on 127.0.0.1:8190\n[toybox] agent ready, 0 running tasks\n[toybox] GET /api/jobs 200',
 comfyui:'ComfyUI version 0.3.x\nRequested to load model h3-int8 (device cuda:0)\nLoaded graph in 1.8s · VRAM 34.6 GiB / 45.6 GiB',
 tunnel:'cloudflared: starting quick tunnel\ncloudflared: https://preview-toybox.trycloudflare.com\ncloudflared: allowed-mail=preview@example.com'
};

var handlers={
 'Studio.Status':function(){return{connected:true,remote:false,clientID:'preview-client.apps.googleusercontent.com'}},
 'Studio.Profile':function(){return{name:'预览账号',email:'preview@example.com'}},
 'Studio.Backend':function(){return{compatible:false}},
 'Studio.Usage':function(){return{currentBalance:42.5,consumptionRateHourly:onlineish()?2.36:0,assignmentsCount:deployed()?1:0}},
 'Studio.Runtimes':function(){return deployed()?[runtimeRow()]:[]},
 'Studio.ServiceStatus':function(){
  var rec=serviceRecord();
  rec.accelerator=machineType;
  return Object.assign({},rec,{operating:false,manager:true,drive:true,driveAccount:'preview@example.com',
   next:rec.rule.enabled?'下次启动：周一 09:00（Asia/Shanghai）':'未启用',version:'preview'});
 },
 'Studio.DeploymentProgress':deploymentProgress,
 'Studio.ToyboxAccessStatus':function(){
  return{protected:onlineish(),apiProtected:onlineish(),apiBaseURL:onlineish()?URL_ONLINE+'/v1':'',runtime:deployed()?'preview-runtime-01':'',
   message:onlineish()?'网页密码与 OpenAI API Token 保护均已启用，两者独立鉴权互不冲突。':'新部署默认启用网页与 OpenAI API 鉴权；可在任务空闲时更新。'};
 },
 'Studio.ToyboxPassword':function(){return PASSWORD},
 'Studio.ToyboxAPIToken':function(){return API_TOKEN},
 'Studio.ToyboxLogs':function(kind){return SAMPLE_LOGS[kind]||SAMPLE_LOGS.deployment},
 'Studio.DriveStatus':function(){
  return{connected:true,status:'ok',folder:'previewFolderID',pending:false,id:'',
   message:'Drive 已连接；作品保存目录已就绪。',
   client:'导入的独立 Drive client',expectedEmail:'preview@example.com',
   email:'preview@example.com',attemptEmail:''};
 },
 'Studio.Logs':function(name,lines){return{path:'/content/easycolab_server.log',
  text:'2026-11-15 09:12:01 easycolab_server started\n2026-11-15 09:12:04 kernel ready\n2026-11-15 09:12:09 health check ok',
  lines:3,truncated:false,missing:false,message:'',updated:new Date().toISOString(),bytes:512}},
 'Studio.CPU':function(name){return{model:'Intel(R) Xeon(R) CPU（预览数据）',cores:12,utilization:14.2}},

 'Studio.WakeToybox':function(confirmed){if(confirmed&&state==='stopped'){setTimeout(function(){state='allocated'},1200)}},
 'Studio.StartToybox':function(confirmed){if(confirmed&&deployed()){state='installing';pageStart=Date.now();setTimeout(function(){state='online'},15000)}},
 'Studio.RetryToybox':function(confirmed){if(confirmed&&deployed()){state='installing';pageStart=Date.now();setTimeout(function(){state='online'},15000)}},
 'Studio.StopToybox':function(confirmed){if(confirmed){setTimeout(function(){state='stopped'},800)}},
 'Studio.SaveMachineType':function(value){machineType=value||'L4'},
 'Studio.SaveServiceRule':function(){},
 'Studio.SaveConfig':function(){},
 'Studio.ApplyToyboxAccess':function(){},
 'Studio.Release':function(){},

 'Studio.Login':function(){}, 'Studio.Disconnect':function(){},
 'Studio.BeginRemoteLogin':function(){}, 'Studio.SubmitRemoteCode':function(){},
 'Studio.ReuseExistingLogin':function(){}, 'Studio.ImportEasyColabConfig':function(){},
 'Studio.BeginDriveLogin':function(){return false},
 'Studio.CancelDriveLogin':function(){}, 'Studio.SubmitDriveCode':function(){},
 'Studio.UseEasyColabDrive':function(){}, 'Studio.ImportDriveClient':function(){},
 'Studio.Open':function(){}, 'Studio.OpenToybox':function(){},
 'Studio.OpenDriveFolder':function(){},
 'Studio.CopyToyboxURL':function(){}, 'Studio.CopyToyboxPassword':function(){},
 'Studio.CopyToyboxAPIToken':function(){}, 'Studio.CopyToyboxAPIBaseURL':function(){},
 'Studio.RotateToyboxAPIToken':function(confirmed){if(confirmed)API_TOKEN='sk-preview-rotated-'+nowSec()},
 'Studio.RefreshService':function(){}
};

window.mygo.call=function(method){
 var args=Array.prototype.slice.call(arguments,1);
 var handler=handlers[method];
 if(!handler){return Promise.reject(new Error('预览模式未模拟的方法：'+method))}
 try{var out=handler.apply(null,args);return Promise.resolve(out===undefined?null:out)}
 catch(err){return Promise.reject(err instanceof Error?err:new Error(String(err)))}
};
})();`
