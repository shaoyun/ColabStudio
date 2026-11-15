// Measured samples only. Missing values and gaps remain visible; no invented history.
const resourceTargets=new Map();
const diskHistories=new Map();
let resourceCall=call;
let resourceTimer=null,resourcePolling=false,resourceEpoch=0,resourceDelay=15000;
function clearResourceMonitoring(){resourceEpoch++;clearTimeout(resourceTimer);resourceTargets.clear();diskHistories.clear();$('serviceResourcePanel')?.replaceChildren();}
function chartPaths(samples,key,now){
 const paths=[];let path='',last=0;
 for(const s of samples){const value=s[key];if(!numeric(s.time)||s.time<now-900||s.time>now+10)continue;
  if(!numeric(value)||value<0||value>100){if(path)paths.push(path);path='';last=0;continue}
  if(last&&s.time-last>40){if(path)paths.push(path);path=''}
  const x=Math.max(0,Math.min(400,(s.time-(now-900))/900*400)),y=62-value/100*54;
  path+=(path?'L':'M')+x.toFixed(2)+','+y.toFixed(2);last=s.time;
 }
 if(path)paths.push(path);return paths;
}
function resourceChart(samples,key,label,detail,now){
 const box=document.createElement('div');box.className='telemetry-chart';
 const title=document.createElement('div');title.className='telemetry-title';
 const name=document.createElement('span'),value=document.createElement('strong');name.textContent=label;
 const latest=samples.at(-1),valid=latest&&now-latest.time<45&&numeric(latest[key])&&latest[key]>=0&&latest[key]<=100;value.textContent=valid?latest[key].toFixed(1)+'%':'—';title.append(name,value);
 const ns='http://www.w3.org/2000/svg',svg=document.createElementNS(ns,'svg');svg.setAttribute('viewBox','0 0 400 70');svg.setAttribute('preserveAspectRatio','none');svg.setAttribute('role','img');svg.setAttribute('aria-label',label+'，最近 15 分钟的利用率，纵轴 0–100%');
 for(const y of [8,35,62]){const line=document.createElementNS(ns,'line');for(const [k,v] of Object.entries({x1:0,x2:400,y1:y,y2:y}))line.setAttribute(k,v);line.setAttribute('class','telemetry-gridline');svg.append(line)}
 for(const x of [0,400/3,800/3,400]){const line=document.createElementNS(ns,'line');for(const [k,v] of Object.entries({x1:x,x2:x,y1:8,y2:62}))line.setAttribute(k,v);line.setAttribute('class','telemetry-gridline');svg.append(line)}
 for(const d of chartPaths(samples,key,now)){const path=document.createElementNS(ns,'path');path.setAttribute('d',d);path.setAttribute('class','telemetry-line');svg.append(path)}
 if(valid){const dot=document.createElementNS(ns,'circle');dot.setAttribute('cx',Math.max(0,Math.min(400,(latest.time-(now-900))/900*400)));dot.setAttribute('cy',62-latest[key]/100*54);dot.setAttribute('r','2.5');dot.setAttribute('class','telemetry-dot');svg.append(dot)}
 const plot=document.createElement('div'),yAxis=document.createElement('div'),xAxis=document.createElement('div');plot.className='telemetry-plot';yAxis.className='telemetry-y-axis';xAxis.className='telemetry-x-axis';
 for(const text of ['100%','50%','0%']){const tick=document.createElement('span');tick.textContent=text;yAxis.append(tick)}
 for(const offset of [900,600,300,0]){const tick=document.createElement('time');const date=new Date((now-offset)*1000);tick.dateTime=date.toISOString();tick.textContent=date.toLocaleTimeString('zh-CN',{hour:'2-digit',minute:'2-digit',hour12:false});xAxis.append(tick)}
 plot.append(yAxis,svg);
 const foot=document.createElement('p');foot.className='resource-message';foot.textContent=detail||'等待真实采样';box.append(title,plot,xAxis,foot);return box;
}
function drawResourceHistory(target,h){
 target.history.replaceChildren();const samples=h.samples||[],latest=samples.at(-1),now=Date.now()/1000;
 const capacity=(used,total)=>numeric(used)&&numeric(total)?(used/1073741824).toFixed(1)+' / '+(total/1073741824).toFixed(1)+' GiB':'';
 for(const [key,label,detail] of [['cpu','CPU',latest?.cpuModel?(latest.cpuModel+' · '+latest.cores+' 核'):null],['gpu','GPU 利用率',latest?.gpuName],['ram','内存 RAM',capacity(latest?.ramUsed,latest?.ramTotal)],['vram','GPU 显存',capacity(latest?.vramUsed,latest?.vramTotal)]])target.history.append(resourceChart(samples,key,label,detail,now));
 const disks=diskHistories.get(target.name);
 if(disks?.size){for(const [label,history] of disks){const disk=history.at(-1);target.history.append(resourceChart(history,'disk','磁盘空间'+(label?' · '+label:''),(capacity(disk?.diskUsed,disk?.diskTotal)||'容量未提供')+' · 本机连接后记录',now))}}
 else target.history.append(resourceChart([],'disk','磁盘空间','等待磁盘数据',now));
 const stale=latest&&now-latest.time>=45;
 target.message.textContent=stale?'连接或采样中断，保留历史；下次自动重试。':h.message||('每 15 秒采样 · 最近 15 分钟 · '+samples.length+' 个真实采样点'+(latest?' · '+new Date(latest.time*1000).toLocaleTimeString('zh-CN'):' · 等待首个样本'));
 if(h.managed)target.panel.parentElement?.querySelector('.cpu-block')?.setAttribute('hidden','');
}
function recordDiskResources(name,disks,now){
 let histories=diskHistories.get(name);if(!histories){histories=new Map();diskHistories.set(name,histories)}
 const active=new Set();
 disks.forEach((disk,index)=>{const f=disk.filesystem||{},label=String(f.label||'磁盘 '+(index+1));active.add(label);
  const samples=histories.get(label)||[],valid=numeric(f.totalBytes)&&numeric(f.usedBytes)&&f.totalBytes>0&&f.usedBytes>=0&&f.usedBytes<=f.totalBytes;
  samples.push({time:now,disk:valid?100*f.usedBytes/f.totalBytes:null,diskUsed:valid?f.usedBytes:null,diskTotal:valid?f.totalBytes:null});
  histories.set(label,samples.filter(s=>now-s.time<900).slice(-60));
 });
 for(const [label,samples] of histories)if(!active.has(label)){samples.push({time:now,disk:null,diskUsed:null,diskTotal:null});histories.set(label,samples.filter(s=>now-s.time<900).slice(-60))}
}
function registerResourceTarget(name,panel){
 let target=resourceTargets.get(panel);if(target)return target;
 const history=document.createElement('div'),message=document.createElement('p'),frame=document.createElement('section');history.className='telemetry-grid';message.className='resource-message';frame.className='telemetry-history';frame.append(history,message);panel.after(frame);
 target={name,panel,history,message,samples:[],pending:false};resourceTargets.set(panel,target);drawResourceHistory(target,{});return target;
}
async function updateResourceTarget(target){
 if(target.pending||!target.panel.isConnected||!connected)return true;
 target.pending=true;const epoch=resourceEpoch;
 try{
  const r=await resourceCall('Resources',target.name);
  if(epoch!==resourceEpoch||!target.panel.isConnected||!connected)return true;
  renderResources(r,target.panel);
  updateModelMemoryEvidence(target.name,r);
  recordDiskResources(target.name,r.disks||[],Date.now()/1000);
  const h=await resourceCall('ResourceHistory',target.name);
  if(epoch!==resourceEpoch||!target.panel.isConnected||!connected)return true;
  if(h.managed){drawResourceHistory(target,h)}else{
   const m=r.memory,g=r.gpus?.[0],now=Date.now()/1000;
   target.samples.push({time:now,cpu:null,gpu:numeric(g?.gpuUtilization)?g.gpuUtilization*100:null,ram:numeric(m?.totalBytes)&&numeric(m?.freeBytes)&&m.totalBytes>0?100*(m.totalBytes-m.freeBytes)/m.totalBytes:null,vram:numeric(g?.memoryTotalBytes)&&g.memoryTotalBytes>0?100*g.memoryUsedBytes/g.memoryTotalBytes:null,ramUsed:numeric(m?.totalBytes)&&numeric(m?.freeBytes)?m.totalBytes-m.freeBytes:null,ramTotal:m?.totalBytes,vramUsed:g?.memoryUsedBytes,vramTotal:g?.memoryTotalBytes,gpuName:g?.name});
   target.samples=target.samples.filter(s=>now-s.time<900).slice(-60);drawResourceHistory(target,{samples:target.samples,message:h.message});
  }
  return true;
 }catch(e){if(epoch===resourceEpoch&&target.panel.isConnected)target.message.textContent='资源刷新暂不可用：'+String(e.message||e)+'。保留历史，稍后重试。';return false}
 finally{target.pending=false}
}
function ensureServiceResources(){
 const host=$('serviceResourcePanel'),name=serviceSnapshot?.runtime;
 if(!host)return;
 if(!name||!connected){host.replaceChildren();delete host.dataset.runtime;return}
 if(host.dataset.runtime===name)return;
 host.replaceChildren();host.dataset.runtime=name;
 const heading=document.createElement('h2'),panel=document.createElement('div');heading.textContent='机器资源';panel.className='resource-grid';host.append(heading,panel);registerResourceTarget(name,panel);
}
async function pollResourceTargets(){
 clearTimeout(resourceTimer);
 if(resourcePolling){scheduleResourceRefresh();return}
 if(document.hidden||!connected||!['overview','services'].includes(currentView)){scheduleResourceRefresh();return}
 resourcePolling=true;
 try{
  if(currentView==='services')ensureServiceResources();
  const results=[];
  for(const [panel,target] of resourceTargets){if(!panel.isConnected){resourceTargets.delete(panel);continue}if(panel.closest('[data-workspace-view]')?.hidden)continue;results.push(await updateResourceTarget(target))}
  resourceDelay=results.includes(false)?Math.min(60000,resourceDelay*2):15000;
 }finally{resourcePolling=false;scheduleResourceRefresh()}
}
function scheduleResourceRefresh(){clearTimeout(resourceTimer);resourceTimer=setTimeout(pollResourceTargets,resourceDelay)}
document.addEventListener('visibilitychange',()=>{clearTimeout(resourceTimer);if(!document.hidden)scheduleResourceRefresh()});
scheduleResourceRefresh();
