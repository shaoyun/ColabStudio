(()=>{
 setView('services');clearTimeout(resourceTimer);
 const now=Date.now()/1000;
 const base={runtime:'compat/demo',remoteRoot:'content/demo',manager:true,drive:true,accelerator:'L4',state:'online',startupPhase:'done',message:'Demo · 本地状态验收，非真实云端运行结果',rule:{days:[],start:'09:00',end:'18:00',zone:'Asia/Shanghai'}};
 const ready={weightsReady:true,weightsEvidence:'sha256',transfer:{phase:'ready',totalFiles:16,verifiedFiles:16,totalBytes:1000,completedBytes:1000},model:{phase:'on_demand'},remote:{stage:'online',health:true,fresh:true,heartbeat:now,url:'https://demo-colabstudio.trycloudflare.com'}};
 serviceSnapshot={...base,runtime:'',remoteRoot:''};renderServiceReadiness(serviceSnapshot);if(!$('copyToyboxURL').disabled||$('toyboxURL').value)throw new Error('Unallocated address exposed');
 serviceSnapshot={...base,runtime:'',remoteRoot:'',state:'lost'};renderServiceReadiness(serviceSnapshot);if(!$('readinessState').textContent.includes('实例已结束')||$('toyboxURL').value)throw new Error('Ended VM retained usable address');
 serviceSnapshot={...base,remoteRoot:'',state:'allocated'};renderServiceReadiness(serviceSnapshot);if(!$('readinessState').textContent.includes('尚未部署'))throw new Error('Awake shown as deployed');
 serviceSnapshot={...base};webAccessReady=false;
 renderServiceReadiness(serviceSnapshot,{...ready,weightsReady:false,transfer:{phase:'verifying',totalFiles:16,verifiedFiles:15,totalBytes:1000,completedBytes:1000}});
 if($('weightsState').dataset.state==='ready'||!$('weightsState').textContent.includes('校验'))throw new Error('100% download falsely completed verification');
 renderServiceReadiness(serviceSnapshot,ready);if(!$('weightsState').textContent.includes('已下载并校验')||!$('modelReadyState').textContent.includes('尚无装载事件')||!$('openToybox').disabled||$('copyToyboxURL').disabled)throw new Error('Weights/model/auth independence lost');
 webAccessReady=true;renderServiceReadiness(serviceSnapshot,ready);if($('openToybox').disabled||!$('readinessState').textContent.includes('可打开网页')||$('toyboxURL').value!==ready.remote.url||!$('toyboxURL').readOnly)throw new Error('Ready URL unavailable');
 serviceSnapshot={...base,startupError:'以前的启动请求返回中断'};renderServiceReadiness(serviceSnapshot,ready);if($('openToybox').disabled||!$('readinessState').textContent.includes('可打开网页'))throw new Error('Old startup error overrode current healthy service');serviceSnapshot={...base};
 renderServiceReadiness(serviceSnapshot,{...ready,remote:{...ready.remote,url:''}});if(!$('openToybox').disabled||$('toyboxURL').value||!$('readinessState').textContent.includes('等待访问地址'))throw new Error('Ready before tunnel URL');
 for(const [phase,text] of [['loading','正在装载'],['partial','部分装载'],['loaded','装载完成'],['error','显存不足']]){renderServiceReadiness(serviceSnapshot,{...ready,model:{phase,requested:'H3Model',loadedMiB:8192}});if(!$('modelReadyState').textContent.includes(text)||!$('modelReadyDetail').textContent.includes('H3Model'))throw new Error('Missing model state '+phase)}
 renderServiceReadiness(serviceSnapshot,{...ready,remote:{...ready.remote,fresh:false,heartbeat:now-120}});if(!$('openToybox').disabled||!$('copyToyboxURL').disabled||!$('readinessState').textContent.includes('重新确认'))throw new Error('Stale service shown ready');
 renderServiceReadiness(serviceSnapshot,{...ready,remote:{...ready.remote,url:'https://evil.example/?token=secret'}});if($('toyboxURL').value||!$('copyToyboxURL').disabled)throw new Error('Untrusted URL shown');
 serviceSnapshot={...base,runtime:'compat/next',remoteRoot:''};renderServiceReadiness(serviceSnapshot);if($('toyboxURL').value||$('weightsState').dataset.state==='ready')throw new Error('New machine retained prior readiness');
 serviceSnapshot={...base};renderServiceReadiness(serviceSnapshot,{...ready,weightsEvidence:'installation'});if(!$('weightsDetail').textContent.includes('安装记录'))throw new Error('Installed evidence not distinguished');
 renderDeploymentProgress({...ready,model:{phase:'loaded',requested:'H3Model',loadedMiB:8192}});applyServiceControls();
 updateModelMemoryEvidence(base.runtime,{gpus:[{memoryUsedBytes:12*1073741824,memoryTotalBytes:24*1073741824}]});if(!$('modelMemoryEvidence').textContent.includes('12.0 / 24.0'))throw new Error('Current VRAM missing');
 $('serviceState').textContent='Demo · 服务就绪';$('serviceMessage').textContent=base.message;$('weightsState').closest('.service-facts').scrollIntoView({block:'center'});
 return {passed:true,demo:true,weightsVerifiedSeparateFromDownload:true,onDemandHonest:true,modelEvents:true,currentVRAM:true,serviceHealthAndAuth:true,currentHealthOverridesOldRequestError:true,waitingForURL:true,copyAndOpenControls:true,staleRejected:true,unsafeURLRejected:true,newMachineClearsState:true,installedEvidence:true};
})()
