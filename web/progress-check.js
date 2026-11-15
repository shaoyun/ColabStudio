(()=>{
 setView('services');
 const sample={transfer:{phase:'downloading',currentFile:'model.safetensors',fileIndex:3,totalFiles:16,verifiedFiles:2,currentBytes:50,fileBytes:100,completedBytes:200,totalBytes:1000,speedBytes:10,etaSeconds:5,startedAt:Date.now()/1000-20,updatedAt:Date.now()/1000},model:{phase:'loading',requested:'H3Model'}};
 renderDeploymentProgress(sample);
 if(document.getElementById('fileProgress').value!==50||document.getElementById('totalProgress').value!==25)throw new Error('下载字节百分比错误');
 if(!document.getElementById('progressFile').textContent.includes('3 / 16')||!document.getElementById('progressSpeed').textContent.includes('预计剩余'))throw new Error('文件序号、速度或预计时间未显示');
 sample.transfer.phase='verifying';renderDeploymentProgress(sample);
 if(document.getElementById('totalProgress').value!==30||!document.getElementById('progressPhase').textContent.includes('SHA-256'))throw new Error('下载和校验阶段混淆');
 sample.transfer.currentFile='<img src=x onerror=alert(1)>';renderDeploymentProgress(sample);if(document.querySelector('#progressFile img'))throw new Error('文件名未按文本渲染');
 renderDeploymentProgress({legacy:true,message:'旧部署仅日志',model:{phase:'partial'}});
 if(!document.getElementById('progressMeters').hidden||!document.getElementById('modelLoadState').textContent.includes('部分装载'))throw new Error('旧版进度或部分装载被伪造百分比');
 renderDeploymentProgress({});
 if(!document.getElementById('modelLoadState').textContent.includes('首次任务'))throw new Error('在线被误认为模型已经装载');
 return {passed:true,downloadBytes:true,verificationSeparate:true,legacyHonest:true,modelOnDemand:true,filenameAsText:true,autoLogs:document.getElementById('serviceAutoLogs').checked};
})()
