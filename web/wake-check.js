(async()=>{
 const wake=document.getElementById('wakeToybox');
 if(!wake)throw new Error('缺少独立唤醒机器入口；Drive 未连接时无法申请机器');
 for(let i=0;i<100&&(servicePollBusy||busy);i++)await new Promise(r=>setTimeout(r,30));
 await serviceStatus();setView('services');
 if(wake.disabled)throw new Error('未连接 Drive 时唤醒入口被错误禁用');
 const machine=document.getElementById('machineType');if(machine.value!=='L4'||machine.disabled)throw new Error('机器默认类型或可编辑状态错误');machine.value='A100';machine.dispatchEvent(new Event('change'));for(let i=0;i<150&&(busy||serviceSnapshot?.accelerator!=='A100');i++)await new Promise(r=>setTimeout(r,30));if(serviceSnapshot?.accelerator!=='A100')throw new Error('机器类型未保存');
 if(!document.getElementById('startToybox').disabled)throw new Error('部署前应要求作品保存授权');
 wake.click();await new Promise(r=>setTimeout(r,20));
 if(!document.getElementById('confirm').open)throw new Error('唤醒缺少计费确认');if(!document.getElementById('confirmText').textContent.includes('A100'))throw new Error('计费确认未显示所选类型');
 document.getElementById('approve').click();
 for(let i=0;i<150;i++){await new Promise(r=>setTimeout(r,30));await serviceStatus();if(serviceSnapshot?.state==='allocated')break}
 if(serviceSnapshot?.state!=='allocated')throw new Error('唤醒 RPC 未返回机器分配状态：'+JSON.stringify(serviceSnapshot));
 if(document.getElementById('serviceState').textContent!=='机器已唤醒')throw new Error('机器状态未显示');
 if(document.getElementById('stopToybox').textContent!=='释放机器')throw new Error('空机器没有释放入口');
 if(!wake.disabled)throw new Error('已有机器时唤醒按钮仍可重复申请');if(!machine.disabled||machine.value!=='A100')throw new Error('唤醒后机器类型未锁定');
 serviceSnapshot.drive=true;applyServiceControls();const deploymentEnabled=!document.getElementById('startToybox').disabled;serviceSnapshot.drive=false;applyServiceControls();
 if(!deploymentEnabled)throw new Error('机器已唤醒且 Drive 已连接时无法部署');
 return {passed:true,defaultL4:true,selectedA100:true,typeLockedAfterWake:true,wakeWithoutDrive:true,billingConfirmed:true,actualBridgeRPC:true,duplicateDisabled:true,emptyMachineRelease:true,deploymentEnabled:true,backend:'local HTTP fixture; no real GPU allocated'};
})()
