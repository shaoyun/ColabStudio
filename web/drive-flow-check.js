(async()=>{
 setView('services');
 if(!document.getElementById('cancelDriveLogin'))throw new Error('缺少取消本次授权按钮');
 for(let i=0;i<100&&(busy||servicePollBusy);i++)await new Promise(r=>setTimeout(r,20));
 await beginDriveAuth();const first=driveAuthID;
 if(!driveAuthPending||document.getElementById('cancelDriveLogin').hidden||document.getElementById('connectDrive').textContent!=='重新发起授权')throw new Error('授权等待时无法取消或重试');
 if(document.getElementById('driveCodeArea').hidden)throw new Error('远程授权码输入未显示');
 document.getElementById('driveCode').value='stale-code';await cancelDriveAuth();
 if((await call('DriveStatus')).pending||!document.getElementById('driveCodeArea').hidden||document.getElementById('driveCode').value)throw new Error('取消后状态或旧授权码未清理');
 await beginDriveAuth();const second=driveAuthID;if(first===second||!driveAuthPending)throw new Error('取消后不能立即发起新授权');
 await beginDriveAuth();const third=driveAuthID;if(second===third||!driveAuthPending)throw new Error('重新发起按钮未替换旧流程');
 await cancelDriveAuth();return {passed:true,pendingVisible:true,cancelAvailable:true,oldCodeCleared:true,retryImmediate:true,restartReplacesFlow:true,noRealBrowserOpened:true};
})()
