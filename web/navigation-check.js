(async()=>{
 const visibleViews=()=>Array.from(document.querySelectorAll('[data-workspace-view]')).filter(el=>el.getClientRects().length>0).map(el=>el.dataset.workspaceView);
 const checks=[];
 function expectView(view,title){const visible=visibleViews();if(visible.length!==1||visible[0]!==view)throw new Error('视图没有互斥显示：'+view+' / '+visible.join(','));if(document.querySelector('.page-header h1').textContent!==title)throw new Error('标题未同步');const active=document.querySelectorAll('aside [aria-current="page"]');if(active.length!==1)throw new Error('侧栏选中态未互斥');checks.push({view,title,visibleCount:visible.length,overviewVisible:!document.getElementById('overviewPanel').hidden})}
 document.getElementById('servicesNav').click();expectView('services','服务');
 document.getElementById('automationNav').click();expectView('automation','自动化');
 document.getElementById('profileNav').click();expectView('profile','账号');
 if(!document.querySelector('#profilePanel #accountName')||!document.querySelector('#profilePanel #accountEmail')||!document.querySelector('#profilePanel #logout'))throw new Error('账号信息未移入独立页面');
 if(document.querySelector('aside .side-account,aside .foot,.allocation,#create,#specs'))throw new Error('旧侧栏内容或无效申请入口仍存在');
 if(!document.querySelector('#servicesNav svg')||!document.querySelector('#automationNav svg'))throw new Error('菜单图标缺失');
 document.getElementById('accountSettingsNav').click();expectView('account','连接设置');
 document.getElementById('accountSettingsNav').click();expectView('account','连接设置');
 sidebarPanels[0].nav.click();expectView('connection-panel-0','连接说明');
 sidebarPanels[1].nav.click();expectView('connection-panel-1','高级设置');
 // A late data refresh may update children, but cannot reopen the overview container.
 document.getElementById('backendInfo').hidden=false;
 document.querySelector('.account-overview').hidden=false;
 await new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r)));
 expectView('connection-panel-1','高级设置');
 sidebarPanels[1].panel.querySelector('.panel-close').click();expectView('overview','实例总览');
 setView('logs');expectView('logs','实例日志');
 logTimer=setTimeout(()=>{},60000);
 document.getElementById('overviewNav').click();expectView('overview','实例总览');
 if(logTimer!==null)throw new Error('离开日志后定时器未清理');
 document.getElementById('accountSettingsNav').click();document.getElementById('closeAccount').click();expectView('overview','实例总览');
 const main=document.querySelector('main'),aside=document.querySelector('aside'),marker=document.createElement('div');marker.style.height='1800px';document.getElementById('overviewPanel').append(marker);const top=aside.getBoundingClientRect().top;main.scrollTop=240;
 if(main.scrollTop===0||aside.getBoundingClientRect().top!==top||document.scrollingElement.scrollTop!==0)throw new Error('独立滚动回归');
 marker.remove();main.scrollTop=0;
 // Leave a real selected settings view for the screenshot.
 document.getElementById('accountSettingsNav').click();expectView('account','连接设置');
 return {passed:true,checks,logTimerStopped:logTimer===null,independentScroll:true};
})()
