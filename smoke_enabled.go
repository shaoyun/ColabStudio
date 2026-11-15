//go:build smoke

package main

import (
	"encoding/json"
	"github.com/egoist/mygo"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Build-tag-only desktop verification: real WKWebView, real MyGo RPC, no API mutations.
func startSmoke(w *mygo.Window) {
	w.Page().OnDidFinishLoad(func() {
		go func() {
			time.Sleep(time.Second)
			dir := os.Getenv("COLABSTUDIO_SMOKE_DIR")
			if dir == "" {
				mygo.App.Quit()
				return
			}
			os.MkdirAll(dir, 0755)
			if os.Getenv("COLABSTUDIO_VERIFY_READINESS") == "1" {
				source, _ := assets.ReadFile("web/readiness-check.js")
				result, e := w.Page().Eval(string(source))
				report := map[string]any{"result": result}
				if e != nil {
					report["error"] = e.Error()
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "readiness-check.json"), b, 0644)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "service-readiness-demo.png"), png, 0644)
				}
				mygo.App.Quit()
				return
			}
			if os.Getenv("COLABSTUDIO_VERIFY_STARTUP") == "1" {
				result, e := w.Page().Eval(`(async()=>{
                    setView('services');clearTimeout(resourceTimer);
                    const native=serviceRead;
                    const base={runtime:'compat/demo',accelerator:'L4',manager:true,drive:true,driveAccount:'demo@example.invalid',rule:{days:[],start:'09:00',end:'18:00',zone:'Asia/Shanghai'},updated:new Date().toISOString()};
                    let fixture={...base,state:'allocated',operating:true,startupPhase:'checking_drive',startupLog:'Demo · 本地测试\n12:00:00 检查 Drive 授权与 API',message:'正在检查 Drive'};
                    let progressReads=0,logReads=0,releaseAuth;
                    serviceRead=async(method)=>{
                        if(method==='ServiceStatus')return {...fixture};
                        if(method==='DriveStatus')return {connected:true,email:'demo@example.invalid',expectedEmail:'demo@example.invalid'};
                        if(method==='ToyboxAccessStatus')return new Promise(resolve=>{releaseAuth=resolve});
                        if(method==='DeploymentProgress'){progressReads++;return {transfer:{phase:'dependencies'},model:{phase:'on_demand'}}}
                        if(method==='ToyboxLogs'){logReads++;return fixture.startupLog+'\nDemo：安装依赖日志持续更新'}
                        throw new Error('Unexpected fixture read '+method);
                    };
                    await serviceStatus();await new Promise(r=>setTimeout(r,30));
                    if(!$('progressPhase').textContent.includes('检查 Drive')||!$('serviceLogText').textContent.includes('检查 Drive')||!$('startToybox').disabled)throw new Error('Pre-directory phase or log missing');
                    fixture={...base,state:'allocated',startupPhase:'failed',startupError:'Demo：模拟 Drive HTTP 403',startupLog:'Demo · 本地回归测试，非真实 API 错误\n12:00:00 检查 Drive 授权与 API\n12:00:01 启动失败：Demo：模拟 Drive HTTP 403',message:'机器已唤醒，尚未部署服务'};
                    await serviceStatus();await new Promise(r=>setTimeout(r,30));
                    if(!$('serviceMessage').textContent.includes('模拟 Drive HTTP 403')||!$('progressError').textContent.includes('模拟 Drive HTTP 403')||!$('serviceLogText').textContent.includes('启动失败'))throw new Error('Startup failure masked by machine state');
                    const failed={...fixture};fixture={...base,state:'installing',remoteRoot:'content/demo',startupPhase:'done',startupLog:'Demo · 已提交远端安装',message:'正在安装'};
                    await serviceStatus();await new Promise(r=>setTimeout(r,30));
                    if(progressReads!==1||logReads!==1||!$('progressPhase').textContent.includes('安装 Python')||!$('serviceLogText').textContent.includes('安装依赖日志持续更新'))throw new Error('Slow auth blocked independent progress/logs');
                    fixture=failed;await serviceStatus();await new Promise(r=>setTimeout(r,30));
                    if(releaseAuth)releaseAuth({protected:false,message:'Demo'});
                    $('progressPhase').closest('.deployment-progress').scrollIntoView({block:'center'});
                    serviceRead=native;clearTimeout(resourceTimer);
                    return {passed:true,demo:true,preDirectoryPhase:true,preDirectoryLogs:true,persistentFailureVisible:true,slowAuthDoesNotBlockProgress:true,slowAuthDoesNotBlockLogs:true,duplicateStartDisabled:true};
                })()`)
				report := map[string]any{"result": result}
				if e != nil {
					report["error"] = e.Error()
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "startup-check.json"), b, 0644)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "startup-diagnostics-demo.png"), png, 0644)
				}
				w.Page().Eval(`document.getElementById('serviceLogText').scrollIntoView({block:'center'})`)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "startup-log-demo.png"), png, 0644)
				}
				mygo.App.Quit()
				return
			}
			if os.Getenv("COLABSTUDIO_VERIFY_TELEMETRY") == "1" {
				source, _ := assets.ReadFile("web/telemetry-check.js")
				result, e := w.Page().Eval(string(source))
				report := map[string]any{"result": result}
				if e != nil {
					report["error"] = e.Error()
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "telemetry-check.json"), b, 0644)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "telemetry-demo.png"), png, 0644)
					w.Page().Eval(`document.querySelector('#serviceResourcePanel .telemetry-chart:last-child').scrollIntoView({block:'center'})`)
					if diskPNG, diskErr := w.CapturePage(); diskErr == nil {
						os.WriteFile(filepath.Join(dir, "telemetry-disk-demo.png"), diskPNG, 0644)
					}
				}
				mygo.App.Quit()
				return
			}
			if os.Getenv("COLABSTUDIO_VERIFY_SECURITY_LAYOUT") == "1" {
				w.Page().Eval(`(async()=>{await serviceStatus();setView('services');document.getElementById('webSecurityPanel').scrollIntoView({block:'center'});})()`)
				time.Sleep(200 * time.Millisecond)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "drive-security-layout.png"), png, 0644)
				}
				mygo.App.Quit()
				return
			}
			if os.Getenv("COLABSTUDIO_VERIFY_DRIVE_STATUS") == "1" {
				source, _ := assets.ReadFile("web/drive-status-check.js")
				result, e := w.Page().Eval(string(source))
				report := map[string]any{"result": result}
				if e != nil {
					report["error"] = e.Error()
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "drive-status-check.json"), b, 0644)
				mygo.App.Quit()
				return
			}
			if os.Getenv("COLABSTUDIO_VERIFY_DRIVE_FLOW") == "1" {
				source, _ := assets.ReadFile("web/drive-flow-check.js")
				result, e := w.Page().Eval(string(source))
				report := map[string]any{"result": result}
				if e != nil {
					report["error"] = e.Error()
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "drive-flow-check.json"), b, 0644)
				mygo.App.Quit()
				return
			}
			if os.Getenv("COLABSTUDIO_VERIFY_PROGRESS") == "1" {
				source, _ := assets.ReadFile("web/progress-check.js")
				result, e := w.Page().Eval(string(source))
				report := map[string]any{"result": result}
				if e != nil {
					report["error"] = e.Error()
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "progress-check.json"), b, 0644)
				mygo.App.Quit()
				return
			}
			if os.Getenv("COLABSTUDIO_VERIFY_WAKE") == "1" {
				source, _ := assets.ReadFile("web/wake-check.js")
				result, e := w.Page().Eval(string(source))
				report := map[string]any{"result": result, "allocationPosts": smokeWakePosts.Load()}
				if e != nil {
					report["error"] = e.Error()
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "wake-check.json"), b, 0644)
				mygo.App.Quit()
				return
			}
			if os.Getenv("COLABSTUDIO_VERIFY_TOYBOX") == "1" {
				result, e := w.Page().Eval(`(async()=>{await serviceStatus();setView('services');const visible=()=>Array.from(document.querySelectorAll('[data-workspace-view]')).filter(e=>!e.hidden).map(e=>e.dataset.workspaceView);const service=visible();setView('automation');const automation=visible();setView('account');const account=visible();setView('services');const m=document.querySelector('main'),a=document.querySelector('aside');return {service,automation,account,isolated:service.length===1&&service[0]==='services'&&automation.length===1&&automation[0]==='automation'&&account.length===1&&account[0]==='account',mainOverflow:getComputedStyle(m).overflowY,asideOverflow:getComputedStyle(a).overflowY,startDisabled:document.getElementById('startToybox').disabled,ruleDays:document.querySelectorAll('[name=weekday]').length,ruleDefaultEnabled:document.getElementById('ruleEnabled').checked}})()`)
				report := map[string]any{"result": result}
				if e != nil {
					report["error"] = e.Error()
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "toybox-ui.json"), b, 0644)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "toybox-services.png"), png, 0644)
				}
				w.Page().Eval(`setView('automation')`)
				time.Sleep(200 * time.Millisecond)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "toybox-automation.png"), png, 0644)
				}
				mygo.App.Quit()
				return
			}
			if os.Getenv("COLABSTUDIO_VERIFY_NAVIGATION") == "1" {
				source, _ := assets.ReadFile("web/navigation-check.js")
				result, e := w.Page().Eval(string(source))
				report := map[string]any{"result": result}
				if e != nil {
					report["error"] = e.Error()
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "navigation-check.json"), b, 0644)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "navigation-settings.png"), png, 0644)
				}
				w.Page().Eval(`setView('profile')`)
				time.Sleep(200 * time.Millisecond)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "account-menu.png"), png, 0644)
				}
				mygo.App.Quit()
				return
			}

			if os.Getenv("COLABSTUDIO_VERIFY_TITLEBAR") == "1" {
				result, e := w.Page().Eval(`(()=>{const root=getComputedStyle(document.documentElement),aside=document.querySelector('aside'),header=document.querySelector('.page-header'),brand=document.querySelector('.brand');return {pageHeight:innerHeight,asideTop:aside.getBoundingClientRect().top,headerTop:header.getBoundingClientRect().top,brandTop:brand.getBoundingClientRect().top,controlsBottom:root.getPropertyValue('--mygo-titlebar-controls-bottom'),controlsLeft:root.getPropertyValue('--mygo-titlebar-inset-left'),headerDraggable:getComputedStyle(header).getPropertyValue('--app-region').trim()==='drag',refreshNoDrag:getComputedStyle(document.getElementById('refresh')).getPropertyValue('--app-region').trim()==='no-drag',asideOverflow:getComputedStyle(aside).overflowY,mainOverflow:getComputedStyle(document.querySelector('main')).overflowY}})()`)
				report := map[string]any{"result": result, "windowBounds": w.Bounds(), "contentBounds": w.ContentBounds()}
				if e != nil {
					report["error"] = "标题栏布局验证失败"
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "titlebar-check.json"), b, 0644)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "titlebar-layout.png"), png, 0644)
				}
				mygo.App.Quit()
				return
			}

			if os.Getenv("COLABSTUDIO_VERIFY_SCROLL") == "1" {
				result, e := w.Page().Eval(`(async()=>{
  const main=document.querySelector('main'),aside=document.querySelector('aside');
  const marker=document.createElement('div');marker.style.height='1800px';marker.setAttribute('aria-hidden','true');main.append(marker);
  const settingsButton=document.querySelectorAll('.sidebar-link')[1];settingsButton.click();const settingsInMain=!!document.querySelector('main #client')&&!!document.querySelector('main #secret');settingsButton.click();
  const before=aside.getBoundingClientRect().top;
  main.scrollTop=420;aside.scrollTop=120;window.scrollTo(0,100);
  await new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r)));
  const result={settingsInMain,mainOverflow:getComputedStyle(main).overflowY,mainScrolled:main.scrollTop>0,mainScrollTop:main.scrollTop,asideOverflow:getComputedStyle(aside).overflowY,asideScrollTop:aside.scrollTop,asideFixed:Math.abs(aside.getBoundingClientRect().top-before)<1,documentScrollTop:document.scrollingElement.scrollTop};
  marker.remove();main.scrollTop=0;return result;
 })()`)
				report := map[string]any{"result": result}
				if e != nil {
					report["error"] = "滚动验证失败"
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "scroll-layout.json"), b, 0644)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "scroll-layout.png"), png, 0644)
				}
				mygo.App.Quit()
				return
			}

			if os.Getenv("COLABSTUDIO_VERIFY_REUSE") == "1" {
				if os.Getenv("COLABSTUDIO_VERIFY_LOGS") == "1" {
					w.Page().Eval(`window.verifyLogs=true`)
				}
				if os.Getenv("COLABSTUDIO_VERIFY_MONITOR") == "1" {
					w.Page().Eval(`window.verifyMonitor=true`)
				}
				result, e := w.Page().Eval(`(async()=>{
  document.getElementById('reuseLogin').click();
  for(let i=0;i<100&&busy;i++)await new Promise(r=>setTimeout(r,1000));
  if(document.querySelector('.cpu-detect')&&window.verifyMonitor){document.querySelector('.cpu-detect').click();for(let i=0;i<75&&busy;i++)await new Promise(r=>setTimeout(r,1000));}
  let logCheck=null;
  if(window.verifyLogs&&document.querySelector('.view-logs')){document.querySelector('.view-logs').click();for(let i=0;i<45&&logPending;i++)await new Promise(r=>setTimeout(r,1000));logCheck={visible:!document.getElementById('logViewer').hidden,hasText:document.getElementById('logText').textContent.length>0,status:document.getElementById('logStatus').textContent,autoDefault:document.getElementById('autoLogs').checked};const first=document.getElementById('logStatus').textContent;document.getElementById('autoLogs').checked=true;document.getElementById('autoLogs').dispatchEvent(new Event('change'));await new Promise(r=>setTimeout(r,6500));for(let i=0;i<20&&logPending;i++)await new Promise(r=>setTimeout(r,500));logCheck.autoRefreshed=document.getElementById('logStatus').textContent!==first;document.getElementById('autoLogs').checked=false;document.getElementById('autoLogs').dispatchEvent(new Event('change'));logCheck.timerStopped=logTimer===null;logCheck.followLatest=document.getElementById('followLogs').checked;}
  const row=document.querySelector('.runtime');
  let releaseCancelled=false;
  if(row){row.querySelector('.danger').click();await new Promise(r=>setTimeout(r,100));const shown=document.getElementById('confirm').open;document.getElementById('cancel').click();releaseCancelled=shown&&!document.getElementById('confirm').open;}
  return {connected:document.getElementById('status').textContent.includes('已连接'),busy,notice:document.getElementById('notice').textContent,runtimeCount:document.getElementById('count').textContent,compatible,allocationRemoved:!document.querySelector('.allocation'),releaseCancelled,logCheck,accountNamePresent:!!document.getElementById('accountName')?.textContent&&document.getElementById('accountName')?.textContent!=='Google Colab',accountEmailPresent:document.getElementById('accountEmail')?.textContent.includes('@'),balance:document.getElementById('balance')?.textContent,burnRate:document.getElementById('burnRate')?.textContent,resourceText:document.querySelector('.resource-grid')?.textContent,cpuFields:Array.from(document.querySelectorAll('.cpu-fields dd')).map(x=>x.textContent),cpuStatus:document.querySelector('.cpu-block .resource-message')?.textContent};
 })()`)
				report := map[string]any{"result": result}
				if e != nil {
					report["error"] = "WebView 验证失败"
				}
				b, _ := json.MarshalIndent(report, "", "  ")
				os.WriteFile(filepath.Join(dir, "existing-login.json"), b, 0644)
				if png, e := w.CapturePage(); e == nil {
					os.WriteFile(filepath.Join(dir, "connected-window.png"), png, 0644)
					if os.Getenv("COLABSTUDIO_VERIFY_MONITOR") == "1" {
						w.Page().Eval(`document.querySelector('.cpu-fields')?.scrollIntoView({block:'center'})`)
						time.Sleep(100 * time.Millisecond)
						if img, e := w.CapturePage(); e == nil {
							os.WriteFile(filepath.Join(dir, "resource-monitor.png"), img, 0644)
						}
					}
				}

				if os.Getenv("COLABSTUDIO_VERIFY_DESIGN") == "1" {
					checks, e := w.Page().Eval(`(()=>{document.getElementById('accountSettingsNav').click();const accountSettingsVisible=!document.getElementById('accountPanel').hidden;document.getElementById('overviewNav').click();const main=document.querySelector('main'),aside=document.querySelector('aside');const top=aside.getBoundingClientRect().top;main.scrollTop=250;aside.scrollTop=120;const result={accountSettingsVisible,asideFixed:aside.getBoundingClientRect().top===top,asideScrollTop:aside.scrollTop,mainScrollTop:main.scrollTop,documentScrollTop:document.scrollingElement.scrollTop};main.scrollTop=0;return result})()`)
					if e == nil {
						b, _ := json.MarshalIndent(checks, "", "  ")
						os.WriteFile(filepath.Join(dir, "design-checks.json"), b, 0644)
					}
					w.Page().Eval(`(()=>{const rules=Array.from(document.styleSheets[0].cssRules);const dark=rules.filter(r=>r.type===4&&r.conditionText.includes('prefers-color-scheme: dark')).flatMap(r=>Array.from(r.cssRules).map(x=>x.cssText)).join('\n');const style=document.createElement('style');style.id='qa-dark';style.textContent=dark;document.head.append(style)})()`)
					time.Sleep(150 * time.Millisecond)
					if png, e := w.CapturePage(); e == nil {
						os.WriteFile(filepath.Join(dir, "design-dark.png"), png, 0644)
					}
					w.Page().Eval(`document.getElementById('qa-dark')?.remove();confirmAction('释放此实例？','解除分配后，未保存的运行数据可能丢失。此处为确认界面检查，不提交释放请求。');true`)
					if png, e := w.CapturePage(); e == nil {
						os.WriteFile(filepath.Join(dir, "design-confirmation.png"), png, 0644)
					}
					w.Page().Eval(`document.getElementById('cancel').click();true`)
					w.SetSize(375, 780)
					time.Sleep(200 * time.Millisecond)
					if png, e := w.CapturePage(); e == nil {
						os.WriteFile(filepath.Join(dir, "design-narrow.png"), png, 0644)
					}
				}
				mygo.App.Quit()
				return
			}
			result, e := w.Page().Eval(`(async()=>{
   if(!document.getElementById('status').textContent.includes('未连接'))return {skipped:'已有连接，跳过截图和未连接测试'};
   const initial={title:document.title,lang:document.documentElement.lang,allocationRemoved:!document.querySelector('.allocation')};
   document.getElementById('refresh').click();await new Promise(r=>setTimeout(r,700));
   initial.refreshError=document.getElementById('notice').textContent;
   const promise=confirmAction('释放此实例？','测试确认框，不提交任何 API 请求');
   initial.confirmVisible=document.getElementById('confirm').open;
   document.getElementById('cancel').click();initial.cancelResult=await promise;
   initial.ready=typeof mygo.call==='function';return initial;
  })()`)
			report := map[string]any{"result": result}
			if e != nil {
				report["error"] = e.Error()
			}
			b, _ := json.MarshalIndent(report, "", "  ")
			os.WriteFile(filepath.Join(dir, "ui-smoke.json"), b, 0644)
			if png, e := w.CapturePage(); e == nil {
				os.WriteFile(filepath.Join(dir, "app-window.png"), png, 0644)
			}
			mygo.App.Quit()
		}()
	})
}

// Verification processes must never ask for access to the user's Keychain.
// Each gets an isolated encrypted vault and an ephemeral in-memory key backend.
type smokeKeyStore struct{ key []byte }

func (s *smokeKeyStore) Get(string) ([]byte, error)   { return s.key, nil }
func (s *smokeKeyStore) Set(_ string, b []byte) error { s.key = append([]byte(nil), b...); return nil }
func init() {
	if os.Getenv("COLABSTUDIO_VERIFY_MONITOR") == "1" {
		cpuTrace = func(s string) {
			if dir := os.Getenv("COLABSTUDIO_SMOKE_DIR"); dir != "" {
				f, e := os.OpenFile(filepath.Join(dir, "cpu-diagnostic-trace.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
				if e == nil {
					f.WriteString(s + "\n")
					f.Close()
				}
			}
		}
	}
	if os.Getenv("COLABSTUDIO_SMOKE_VAULT") == "1" {
		if dir, e := os.MkdirTemp("", "colabstudio-smoke-vault-"); e == nil {
			store = newCredentialVault(filepath.Join(dir, "credentials.enc"), &smokeKeyStore{})
		}
	}
}

var smokeWakePosts atomic.Int32
var smokeWakeAssigned atomic.Bool

type smokeHTTPTransport struct{}

func (smokeHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := "{}"
	switch {
	case strings.HasSuffix(r.URL.Path, "assignments"):
		body = `{"assignments":[]}`
		if smokeWakeAssigned.Load() {
			body = `{"assignments":[{"endpoint":"smoke-machine","runtimeProxyInfo":{"url":"https://smoke.r.colab.dev","token":"smoke-proxy"}}]}`
		}
	case strings.HasSuffix(r.URL.Path, "assign"):
		if r.Method == "POST" {
			smokeWakePosts.Add(1)
			smokeWakeAssigned.Store(true)
			encoded, _ := json.Marshal(map[string]any{"endpoint": "smoke-machine", "variant": 1, "accelerator": r.URL.Query().Get("accelerator")})
			body = string(encoded)
		} else {
			body = `{"token":"smoke-challenge","variant":"GPU","acc":"L4"}`
		}
	case strings.HasSuffix(r.URL.Path, "runtimes"):
		body = `{"runtimes":[]}`
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}
func init() {
	if (os.Getenv("COLABSTUDIO_VERIFY_WAKE") == "1" || os.Getenv("COLABSTUDIO_VERIFY_DRIVE_FLOW") == "1") && os.Getenv("COLABSTUDIO_SMOKE_VAULT") == "1" {
		token, _ := json.Marshal(Token{Access: "smoke-access", Until: time.Now().Add(time.Hour)})
		profile, _ := json.Marshal(cachedProfile{Binding: credentialBinding([]byte("smoke-refresh")), Fetched: time.Now(), Profile: AccountProfile{Name: "测试账号", Email: "test@example.invalid"}})
		_ = store.Set("access-token", token)
		_ = store.Set("refresh-token", []byte("smoke-refresh"))
		_ = store.Set("account-profile", profile)
		httpClient = &http.Client{Transport: smokeHTTPTransport{}}
	}
}

func init() {
	if os.Getenv("COLABSTUDIO_VERIFY_DRIVE_FLOW") == "1" && os.Getenv("COLABSTUDIO_SMOKE_VAULT") == "1" {
		driveAuthConfig = func() (Config, error) {
			return Config{ClientID: "test.apps.googleusercontent.com", ClientSecret: "test-secret", Remote: true}, nil
		}
		driveAuthOpen = func(string) error { return nil }
	}
}
