(async()=>{
 clearTimeout(resourceTimer);connected=true;setView('services');
 serviceSnapshot={runtime:'compat/demo',state:'allocated'};ensureServiceResources();
 const target=[...resourceTargets.values()].find(t=>t.name==='compat/demo');
 const now=Date.now()/1000;
 for(let i=59;i>=1;i--)recordDiskResources('compat/demo',[{filesystem:{label:'/content',usedBytes:(30+(59-i)/5)*1073741824,totalBytes:100*1073741824}}],now-i*15);
 const samples=Array.from({length:60},(_,i)=>({time:now-(59-i)*15,cpu:18+Math.sin(i/4)*8,gpu:45+Math.sin(i/7)*28,ram:42+i/6,vram:62+Math.sin(i/8)*5,cpuModel:'Demo CPU',cores:8,gpuName:'Demo NVIDIA L4',ramUsed:12*1073741824,ramTotal:24*1073741824,vramUsed:15*1073741824,vramTotal:24*1073741824}));
 const native=resourceCall,requests=[];
 resourceCall=async(method,...args)=>{requests.push(method);if(method==='Resources')return {memory:{totalBytes:24*1073741824,freeBytes:12*1073741824},disks:[{filesystem:{label:'/content',usedBytes:42*1073741824,totalBytes:100*1073741824}}],gpus:[{name:'Demo NVIDIA L4',gpuUtilization:.45,memoryUsedBytes:15*1073741824,memoryTotalBytes:24*1073741824}]};if(method==='ResourceHistory')return {managed:true,samples};throw new Error('unexpected fixture call: '+method)};
 await updateResourceTarget(target);
 if(target.history.children.length!==5||target.history.querySelectorAll('path').length!==5)throw new Error('Missing five history charts: '+JSON.stringify({children:target.history.children.length,paths:target.history.querySelectorAll('path').length,message:target.message.textContent,requests,connected,pending:target.pending,attached:target.panel.isConnected}));
 if(target.history.querySelectorAll('.telemetry-y-axis').length!==5||target.history.querySelectorAll('.telemetry-x-axis time').length!==20)throw new Error('Missing axes');
 for(const chart of target.history.children){if(chart.querySelector('.telemetry-y-axis').textContent!=='100%50%0%')throw new Error('Wrong percent scale');const ticks=[...chart.querySelectorAll('time')].map(t=>Date.parse(t.dateTime));if(ticks[3]-ticks[0]!==900000||ticks[1]-ticks[0]!==300000)throw new Error('Wrong time scale')}
 const diskChart=target.history.lastElementChild;if(diskChart.querySelector('strong').textContent!=='42.0%'||!diskChart.textContent.includes('42.0 / 100.0 GiB'))throw new Error('Incorrect disk percentage or capacity');
 recordDiskResources('missing-test',[{filesystem:{label:'test',usedBytes:1,totalBytes:2}}],now-15);recordDiskResources('missing-test',[],now);if(diskHistories.get('missing-test').get('test').at(-1).disk!==null)throw new Error('Missing disk became zero');
 for(let i=0;i<65;i++)recordDiskResources('limit-test',[{filesystem:{usedBytes:1,totalBytes:2}}],now-65+i);if(diskHistories.get('limit-test').values().next().value.length!==60)throw new Error('Unbounded disk samples');
 if(!target.message.textContent.includes('60 个真实采样点'))throw new Error('Sample count not shown');
 if(chartPaths([{time:now-75,cpu:20},{time:now,cpu:30}], 'cpu',now).length!==2)throw new Error('Gap incorrectly connected');
 if(chartPaths([{time:now-15,cpu:null},{time:now,cpu:20}], 'cpu',now)[0].includes('L'))throw new Error('Missing values fabricated');
 const first=resourceChart([{time:now,cpu:null}],'cpu','CPU','',now);if(first.querySelector('strong').textContent!=='—')throw new Error('Unknown CPU shown as zero');
 const compact=resourceChart(samples,'ram','RAM','',now);compact.style.width='180px';document.body.append(compact);
 const compactTicks=[...compact.querySelectorAll('.telemetry-x-axis time')].map(t=>t.getBoundingClientRect());for(let i=1;i<compactTicks.length;i++)if(compactTicks[i-1].right>compactTicks[i].left)throw new Error('Compact time labels overlap');
 const svgRect=compact.querySelector('svg').getBoundingClientRect(),yTicks=[...compact.querySelectorAll('.telemetry-y-axis span')];for(const [i,fraction] of [8/70,.5,62/70].entries()){const rect=yTicks[i].getBoundingClientRect();if(Math.abs((rect.top+rect.bottom)/2-(svgRect.top+svgRect.height*fraction))>1)throw new Error('Percent label misaligned with grid')};compact.remove();
 const previous=requests.length;setView('profile');await pollResourceTargets();if(requests.length!==previous)throw new Error('Hidden view polled');
 setView('services');drawResourceHistory(target,{managed:true,samples:[{time:now-60,cpu:50}]});if(!target.message.textContent.includes('中断'))throw new Error('Stale history not marked');
 resourceCall=async()=>{throw new Error('fixture offline')};await updateResourceTarget(target);if(!target.message.textContent.includes('fixture offline'))throw new Error('Failure not visible');
 drawResourceHistory(target,{managed:true,samples,message:'Demo · 本地测试数据，仅用于窗口验收 · 每 15 秒采样，最近 15 分钟'});
 document.getElementById('serviceResourcePanel').scrollIntoView({block:'start'});
 clearTimeout(resourceTimer);resourceCall=native;connected=false;
 return {passed:true,fiveCharts:true,diskCapacityAndPercent:true,percentAxes:true,timeAxes:true,narrowAxesFit:true,axesAligned:true,missingDiskUnknown:true,intervalMs:15000,historyLimit:60,missingCPUUnknown:true,gapsDisconnected:true,hiddenViewPaused:true,staleMarked:true,errorsPreserveHistory:true,demo:true};
})()
