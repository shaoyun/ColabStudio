(()=>{
 setView('services');
 applyDriveStatus({connected:true,email:'owner@example.invalid',expectedEmail:'owner@example.invalid',status:'connected',folder:'saved-folder'});
 if(document.getElementById('connectDrive').textContent!=='重新授权'||!document.getElementById('driveBadge').textContent.includes('Drive 已连接'))throw new Error('已连接仍显示连接 Drive');
 if(document.getElementById('openDriveFolder').disabled||document.getElementById('openDriveFolder').textContent!=='打开保存目录')throw new Error('已连接目录不可打开');
 applyDriveStatus({connected:false,email:'',expectedEmail:'owner@example.invalid',attemptEmail:'other@example.invalid',status:'account_mismatch'});
 if(document.getElementById('connectDrive').textContent!=='重新选择 Drive 账号'||document.getElementById('driveAttemptRow').hidden)throw new Error('账号不匹配没有说明');
 if(!document.getElementById('driveExpectedAccount').textContent.includes('owner@')||!document.getElementById('driveAttemptAccount').textContent.includes('other@'))throw new Error('没有显示需要和实际选择的账号');
 if(!document.querySelector('.drive-location').textContent.includes('ColabStudio / H3-Toybox'))throw new Error('未明确保存目录');
 return {passed:true,connectedLabel:true,expectedAndAttemptAccount:true,explicitFolder:true,folderAction:true};
})()
