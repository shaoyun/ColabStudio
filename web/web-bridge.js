(() => {
  'use strict';

  document.documentElement.dataset.mode = 'web';

  const styleEl = document.createElement('style');
  styleEl.textContent = `
    [data-mode="web"] .window-drag-pad { display: none !important; }
    .web-side-tools {
      margin-top: auto;
      padding: 10px 12px 14px;
      border-top: 1px solid rgba(128, 128, 128, 0.2);
      display: flex;
      gap: 8px;
      flex-wrap: wrap;
    }
    .web-side-tools button {
      flex: 1 1 auto;
      font-size: 12px;
      padding: 6px 10px;
    }
    dialog.web-dialog {
      max-width: 520px;
      width: calc(100vw - 32px);
      border: 1px solid rgba(128, 128, 128, 0.28);
      border-radius: 12px;
      padding: 20px;
      box-sizing: border-box;
    }
    dialog.web-dialog::backdrop {
      background: rgba(15, 23, 42, 0.55);
      backdrop-filter: blur(2px);
    }
    .web-dialog h2 { margin: 0 0 8px; font-size: 17px; }
    .web-dialog p { margin: 6px 0 12px; font-size: 13px; line-height: 1.5; }
    .web-dialog input[type="password"],
    .web-dialog input[type="text"] {
      width: 100%;
      box-sizing: border-box;
      margin: 6px 0 10px;
    }
    .web-dialog pre.web-api-code {
      background: rgba(15, 23, 42, 0.08);
      border: 1px solid rgba(128, 128, 128, 0.25);
      border-radius: 8px;
      padding: 10px 12px;
      font-size: 12px;
      line-height: 1.45;
      overflow-x: auto;
      white-space: pre-wrap;
      word-break: break-all;
      margin: 6px 0 12px;
    }
    .web-dialog-error {
      color: #dc2626;
      font-size: 12px;
      min-height: 18px;
      margin-bottom: 8px;
    }
    .web-oauth-link-box {
      margin: 8px 0 10px;
      padding: 8px 10px;
      border-radius: 8px;
      background: rgba(59, 130, 246, 0.08);
      border: 1px solid rgba(59, 130, 246, 0.25);
      font-size: 12px;
      line-height: 1.45;
      word-break: break-all;
    }
    .web-oauth-link-box a {
      font-weight: 600;
      text-decoration: underline;
    }
  `;
  document.head.appendChild(styleEl);

  let unlockPromise = null;

  function ensureAuthDialog() {
    let dlg = document.getElementById('webAuthDialog');
    if (dlg) return dlg;
    dlg = document.createElement('dialog');
    dlg.id = 'webAuthDialog';
    dlg.className = 'web-dialog';
    dlg.innerHTML = `
      <h2>解锁 ColabStudio Web 控制台</h2>
      <p>请输入服务端 API Token（可在使用服务器终端运行 <code>colabstudio-server -print-token</code> 查看）。认证后将签发安全的 HttpOnly 会话 Cookie。</p>
      <form id="webAuthForm">
        <label for="webAuthTokenInput">API Token</label>
        <input id="webAuthTokenInput" type="password" autocomplete="current-password" placeholder="粘贴服务端 API Token" required>
        <div id="webAuthError" class="web-dialog-error" role="alert"></div>
        <div class="actions">
          <button type="submit" class="primary" id="webAuthSubmitBtn">解锁进入</button>
        </div>
      </form>
    `;
    dlg.addEventListener('cancel', (ev) => ev.preventDefault());
    document.body.appendChild(dlg);
    return dlg;
  }

  function requestTokenUnlock() {
    if (unlockPromise) return unlockPromise;
    unlockPromise = (async () => {
      if (document.readyState === 'loading') {
        await new Promise((r) => document.addEventListener('DOMContentLoaded', r, { once: true }));
      }
      const dlg = ensureAuthDialog();
      const form = document.getElementById('webAuthForm');
      const input = document.getElementById('webAuthTokenInput');
      const errBox = document.getElementById('webAuthError');
      const submitBtn = document.getElementById('webAuthSubmitBtn');

      errBox.textContent = '';
      input.value = '';
      if (!dlg.open) dlg.showModal();
      setTimeout(() => input.focus(), 20);

      await new Promise((resolve) => {
        form.onsubmit = async (ev) => {
          ev.preventDefault();
          const token = input.value.trim();
          if (!token) {
            errBox.textContent = '请输入 API Token';
            return;
          }
          submitBtn.disabled = true;
          errBox.textContent = '';
          try {
            const res = await fetch('/api/auth/login', {
              method: 'POST',
              credentials: 'same-origin',
              headers: {
                'Content-Type': 'application/json',
                'X-Requested-With': 'ColabStudio',
              },
              body: JSON.stringify({ token }),
            });
            const data = await res.json().catch(() => ({}));
            if (!res.ok || !data.ok) {
              errBox.textContent = data.error || 'Token 验证失败 (HTTP ' + res.status + ')';
              return;
            }
            input.value = '';
            dlg.close();
            resolve();
          } catch (err) {
            errBox.textContent = '网络请求失败：' + String(err && err.message ? err.message : err);
          } finally {
            submitBtn.disabled = false;
          }
        };
      });
    })().finally(() => {
      unlockPromise = null;
    });
    return unlockPromise;
  }

  async function rawCall(method, ...args) {
    for (let attempt = 0; attempt < 2; attempt++) {
      const res = await fetch('/api/rpc/' + encodeURIComponent(method), {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
          'Content-Type': 'application/json',
          'X-Requested-With': 'ColabStudio',
        },
        body: JSON.stringify({ args }),
      });
      if (res.status === 401) {
        await requestTokenUnlock();
        continue;
      }
      const data = await res.json().catch(() => ({}));
      if (!res.ok || !data.ok) {
        throw new Error(data.error || '请求失败 (HTTP ' + res.status + ')');
      }
      return data.result;
    }
    throw new Error('会话鉴权失败，请重新输入 Token');
  }

  async function copyTextToBrowser(text) {
    const str = String(text || '');
    if (!str) throw new Error('无可复制内容');
    if (navigator.clipboard && window.isSecureContext) {
      try {
        await navigator.clipboard.writeText(str);
        return;
      } catch (_) {}
    }
    const ta = document.createElement('textarea');
    ta.value = str;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-9999px';
    ta.style.left = '-9999px';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try {
      ok = document.execCommand('copy');
    } catch (_) {
      ok = false;
    }
    ta.remove();
    if (!ok) {
      throw new Error('浏览器未授予剪贴板权限，请手动从输入框复制');
    }
  }

  function openExternalURL(url, label) {
    if (!url) throw new Error('地址尚未就绪');
    const win = window.open(url, '_blank', 'noopener,noreferrer');
    if (!win) {
      throw new Error('浏览器拦截了新窗口，请允许弹窗或复制链接手动打开 ' + (label || '') + '：' + url);
    }
  }

  function pickLocalFileText(accept) {
    return new Promise((resolve) => {
      const input = document.createElement('input');
      input.type = 'file';
      if (accept) input.accept = accept;
      input.style.display = 'none';
      document.body.appendChild(input);

      let settled = false;
      const done = (val) => {
        if (settled) return;
        settled = true;
        window.removeEventListener('focus', onFocus);
        input.remove();
        resolve(val);
      };
      const onFocus = () => {
        setTimeout(() => {
          if (!input.files || !input.files.length) done(null);
        }, 400);
      };
      input.addEventListener('cancel', () => done(null));
      input.addEventListener('change', async () => {
        const file = input.files && input.files[0];
        if (!file) {
          done(null);
          return;
        }
        try {
          done(await file.text());
        } catch (_) {
          done(null);
        }
      });
      window.addEventListener('focus', onFocus, { once: true });
      input.click();
    });
  }

  function askWebChoice(title, message, primaryLabel) {
    return new Promise((resolve) => {
      let dlg = document.getElementById('webChoiceDialog');
      if (!dlg) {
        dlg = document.createElement('dialog');
        dlg.id = 'webChoiceDialog';
        dlg.className = 'web-dialog';
        dlg.innerHTML = `
          <h2 id="webChoiceTitle"></h2>
          <p id="webChoiceMsg"></p>
          <div class="actions">
            <button type="button" id="webChoiceCancel">取消</button>
            <button type="button" class="primary" id="webChoiceOk"></button>
          </div>
        `;
        document.body.appendChild(dlg);
      }
      document.getElementById('webChoiceTitle').textContent = title;
      document.getElementById('webChoiceMsg').textContent = message;
      document.getElementById('webChoiceOk').textContent = primaryLabel || '选择文件';
      const finish = (v) => {
        if (dlg.open) dlg.close();
        resolve(v);
      };
      document.getElementById('webChoiceCancel').onclick = () => finish(false);
      document.getElementById('webChoiceOk').onclick = () => finish(true);
      dlg.oncancel = () => finish(false);
      dlg.showModal();
    });
  }

  function showOAuthHintBox(containerId, boxId, authURL, tipText) {
    const container = document.getElementById(containerId);
    if (!container) return;
    let box = document.getElementById(boxId);
    if (!box) {
      box = document.createElement('div');
      box.id = boxId;
      box.className = 'web-oauth-link-box';
      container.prepend(box);
    }
    box.replaceChildren();
    const tip = document.createElement('div');
    tip.textContent = tipText + ' ';
    const link = document.createElement('a');
    link.href = authURL;
    link.target = '_blank';
    link.rel = 'noopener noreferrer';
    link.textContent = '点此打开 Google 授权页';
    tip.appendChild(link);
    box.appendChild(tip);
    box.hidden = false;
  }

  function clearOAuthHintBox(boxId) {
    const box = document.getElementById(boxId);
    if (box) box.hidden = true;
  }

  function showDesktopClientOAuthDialog(authURL) {
    return new Promise((resolve, reject) => {
      let dlg = document.getElementById('webOAuthDialog');
      if (!dlg) {
        dlg = document.createElement('dialog');
        dlg.id = 'webOAuthDialog';
        dlg.className = 'web-dialog';
        dlg.innerHTML = `
          <h2>自建 OAuth Client 远程授权</h2>
          <p>请在新打开的标签页中登录 Google 账号。授权后浏览器会跳转到 <code>http://127.0.0.1:53682/oauth2callback?code=...</code>（远端访问页面提示“无法访问此网站”属正常现象）。请完整复制地址栏 URL 并粘贴到下方：</p>
          <div class="web-oauth-link-box"><a id="webOAuthOpenLink" href="#" target="_blank" rel="noopener noreferrer">重新打开 Google 授权页面</a></div>
          <form id="webOAuthForm">
            <input id="webOAuthCallbackInput" type="text" autocomplete="off" placeholder="粘贴完整回调 URL（或授权码）" required>
            <div id="webOAuthError" class="web-dialog-error"></div>
            <div class="actions">
              <button type="button" id="webOAuthCancelBtn">取消</button>
              <button type="submit" class="primary" id="webOAuthSubmitBtn">确认完成授权</button>
            </div>
          </form>
        `;
        document.body.appendChild(dlg);
      }
      const link = document.getElementById('webOAuthOpenLink');
      const form = document.getElementById('webOAuthForm');
      const input = document.getElementById('webOAuthCallbackInput');
      const errBox = document.getElementById('webOAuthError');
      const cancelBtn = document.getElementById('webOAuthCancelBtn');
      const submitBtn = document.getElementById('webOAuthSubmitBtn');

      link.href = authURL;
      input.value = '';
      errBox.textContent = '';
      dlg.showModal();

      let done = false;
      const fail = (err) => {
        if (done) return;
        done = true;
        if (dlg.open) dlg.close();
        reject(err);
      };
      cancelBtn.onclick = () => fail(new Error('已取消远程授权'));
      dlg.oncancel = (ev) => {
        ev.preventDefault();
        fail(new Error('已取消远程授权'));
      };
      form.onsubmit = async (ev) => {
        ev.preventDefault();
        const codeInput = input.value.trim();
        if (!codeInput) {
          errBox.textContent = '请粘贴授权回调地址或授权码';
          return;
        }
        submitBtn.disabled = true;
        errBox.textContent = '';
        try {
          await rawCall('SubmitRemoteCode', codeInput);
          done = true;
          if (dlg.open) dlg.close();
          resolve();
        } catch (e) {
          errBox.textContent = String(e && e.message ? e.message : e);
        } finally {
          submitBtn.disabled = false;
        }
      };
    });
  }

  window.mygo = {
    async call(fullName, ...args) {
      switch (fullName) {
        case 'Studio.CopyToyboxURL': {
          const url = await rawCall('ToyboxVerifiedURL');
          await copyTextToBrowser(url);
          return;
        }
        case 'Studio.CopyToyboxPassword': {
          const pw = await rawCall('ToyboxPassword');
          await copyTextToBrowser(pw);
          return;
        }
        case 'Studio.CopyToyboxAPIToken': {
          const tok = await rawCall('ToyboxAPIToken');
          await copyTextToBrowser(tok);
          return;
        }
        case 'Studio.CopyToyboxAPIBaseURL': {
          const base = await rawCall('ToyboxAPIBaseURL');
          await copyTextToBrowser(base);
          return;
        }
        case 'Studio.OpenToybox': {
          const url = await rawCall('OpenToyboxURL');
          openExternalURL(url, 'Toybox');
          return;
        }
        case 'Studio.Open': {
          const url = await rawCall('RuntimeOpenURL', args[0]);
          openExternalURL(url, 'Colab 实例');
          return;
        }
        case 'Studio.OpenDriveFolder': {
          const url = await rawCall('EnsureDriveFolderURL');
          openExternalURL(url, 'Google Drive 保存目录');
          return;
        }
        case 'Studio.OpenColab': {
          const url = await rawCall('CompatibleURL');
          openExternalURL(url, 'Google Colab');
          return;
        }
        case 'Studio.ImportEasyColabConfig': {
          const found = await rawCall('TryImportLocalEasyColabConfig');
          if (found) return;
          const text = await pickLocalFileText('.json,application/json');
          if (!text) {
            throw new Error('已取消导入；可上传 EasyColab / Colab CLI 的 oauth_config.json');
          }
          await rawCall('ImportEasyColabConfigJSON', text);
          return;
        }
        case 'Studio.ReuseExistingLogin': {
          try {
            return await rawCall('ReuseExistingLogin');
          } catch (err) {
            const msg = String(err && err.message ? err.message : err);
            if (!/未找到|不存在|不支持|钥匙串/.test(msg)) throw err;
            const upload = await askWebChoice(
              '上传本地 Colab 凭据 JSON？',
              '服务器本机未找到可直接复用的登录凭据（' + msg + '）。您也可以从当前电脑选择 EasyColab / Colab CLI 导出的凭据 JSON 文件上传使用。',
              '选择凭据文件'
            );
            if (!upload) throw err;
            const text = await pickLocalFileText('.json,application/json');
            if (!text) throw err;
            await rawCall('ImportExistingCredentialJSON', text);
            return;
          }
        }
        case 'Studio.ImportDriveClient': {
          const text = await pickLocalFileText('.json,application/json');
          if (!text) throw new Error('已取消导入 Drive 配置');
          await rawCall('ImportDriveClientJSON', text);
          return;
        }
        case 'Studio.UseEasyColabDrive': {
          try {
            return await rawCall('UseEasyColabDrive');
          } catch (err) {
            const msg = String(err && err.message ? err.message : err);
            if (!msg.includes('未找到')) throw err;
            const upload = await askWebChoice(
              '上传 EasyColab Drive 配置？',
              '服务器未找到 ~/.config/easycolab/drive_rclone.conf。是否从当前电脑上传 drive_rclone.conf？',
              '上传 drive_rclone.conf'
            );
            if (!upload) throw err;
            const text = await pickLocalFileText('.conf,text/plain');
            if (!text) throw err;
            await rawCall('ImportRcloneDriveConf', text);
            return;
          }
        }
        case 'Studio.BeginRemoteLogin': {
          const info = await rawCall('BeginWebLogin');
          const tip = info.remote
            ? '若浏览器未自动弹出授权窗口，请手动打开并完成授权后粘贴结果页授权码：'
            : '请打开下方授权页；授权后若跳转 127.0.0.1 无法连接，请完整复制地址栏 URL 粘贴到下方：';
          showOAuthHintBox('codeArea', 'webColabAuthHint', info.authURL, tip);
          window.open(info.authURL, '_blank', 'noopener,noreferrer');
          return;
        }
        case 'Studio.Login': {
          const info = await rawCall('BeginWebLogin');
          window.open(info.authURL, '_blank', 'noopener,noreferrer');
          await showDesktopClientOAuthDialog(info.authURL);
          return;
        }
        case 'Studio.SubmitRemoteCode': {
          clearOAuthHintBox('webColabAuthHint');
          return await rawCall('SubmitRemoteCode', args[0]);
        }
        case 'Studio.BeginDriveLogin': {
          const info = await rawCall('BeginWebDriveLogin', args[0]);
          const tip = info.desktopCallback
            ? '完成授权后页面若跳转 127.0.0.1 显示无法连接，请完整复制地址栏 URL 粘贴到下方：'
            : '请完成授权后将页面显示的授权码粘贴到下方：';
          showOAuthHintBox('driveCodeArea', 'webDriveAuthHint', info.authURL, tip);
          const input = document.getElementById('driveCode');
          if (input && info.desktopCallback) {
            input.placeholder = '粘贴授权码或 http://127.0.0.1:.../callback?code=... 完整地址';
          }
          window.open(info.authURL, '_blank', 'noopener,noreferrer');
          return true;
        }
        case 'Studio.CancelDriveLogin': {
          clearOAuthHintBox('webDriveAuthHint');
          return await rawCall('CancelDriveLogin', args[0]);
        }
        case 'Studio.SubmitDriveCode': {
          clearOAuthHintBox('webDriveAuthHint');
          return await rawCall('SubmitDriveCode', args[0], args[1]);
        }
        default:
          return await rawCall(fullName.replace(/^Studio\./, ''), ...args);
      }
    },
  };

  function openAPIGuideDialog() {
    let dlg = document.getElementById('webAPIGuideDialog');
    if (!dlg) {
      dlg = document.createElement('dialog');
      dlg.id = 'webAPIGuideDialog';
      dlg.className = 'web-dialog';
      dlg.innerHTML = `
        <h2>远程控制 REST API（Token 认证）</h2>
        <p>所有 <code>/api/v1/*</code> 接口支持使用 <code>Authorization: Bearer &lt;TOKEN&gt;</code> 或 <code>X-API-Token</code> 请求头调用。</p>
        <pre id="webAPICurlExamples" class="web-api-code"></pre>
        <div class="actions">
          <button type="button" id="webAPICopyBtn">复制示例命令</button>
          <button type="button" class="primary" id="webAPICloseBtn">关闭</button>
        </div>
      `;
      document.body.appendChild(dlg);
      document.getElementById('webAPICloseBtn').onclick = () => dlg.close();
      document.getElementById('webAPICopyBtn').onclick = async () => {
        const txt = document.getElementById('webAPICurlExamples').textContent;
        await copyTextToBrowser(txt).catch(() => {});
      };
    }
    const base = window.location.origin;
    document.getElementById('webAPICurlExamples').textContent = [
      '# 1. 查看 CU 算力消耗与剩余估算',
      `curl -fsS -H "Authorization: Bearer YOUR_TOKEN" "${base}/api/v1/cu"`,
      '',
      '# 2. 一键开启 Toybox（支持 accelerator、wait:true）',
      `curl -fsS -X POST -H "Authorization: Bearer YOUR_TOKEN" -H "Content-Type: application/json" -d '{"accelerator":"L4"}' "${base}/api/v1/toybox/start"`,
      '',
      '# 3. 一键安全保存并停止释放 Toybox（传 force:true 可跳过备份强制释放）',
      `curl -fsS -X POST -H "Authorization: Bearer YOUR_TOKEN" -H "Content-Type: application/json" -d '{"force":false}' "${base}/api/v1/toybox/stop"`,
      '',
      '# 4. 查看运行实例与一键释放指定或全部非托管实例',
      `curl -fsS -H "Authorization: Bearer YOUR_TOKEN" "${base}/api/v1/runtimes"`,
      `curl -fsS -X POST -H "Authorization: Bearer YOUR_TOKEN" -H "Content-Type: application/json" -d '{"all":true}' "${base}/api/v1/runtimes/release"`,
      '',
      '# 5. 获取已部署 Toybox 网页密码与 OpenAI API Token',
      `curl -fsS -H "Authorization: Bearer YOUR_TOKEN" "${base}/api/v1/toybox/credentials"`,
    ].join('\n');
    dlg.showModal();
  }

  function applyWebUICustomizations() {
    const authCode = document.getElementById('authCode');
    if (authCode) {
      authCode.placeholder = '粘贴 Google 授权码或完整回调 URL';
    }
    const secretInput = document.getElementById('secret');
    if (secretInput) {
      secretInput.placeholder = '加密保存于服务端凭据库';
    }
    const aside = document.querySelector('aside');
    if (aside && !document.getElementById('webSideTools')) {
      const box = document.createElement('div');
      box.id = 'webSideTools';
      box.className = 'web-side-tools';

      const apiBtn = document.createElement('button');
      apiBtn.type = 'button';
      apiBtn.textContent = '远程 API';
      apiBtn.onclick = openAPIGuideDialog;

      const lockBtn = document.createElement('button');
      lockBtn.type = 'button';
      lockBtn.textContent = '锁定会话';
      lockBtn.onclick = async () => {
        await fetch('/api/auth/logout', {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'X-Requested-With': 'ColabStudio' },
        }).catch(() => {});
        await requestTokenUnlock();
        window.location.reload();
      };

      box.append(apiBtn, lockBtn);
      aside.appendChild(box);
    }

    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    const replacements = [
      ['本应用 Keychain', '服务端加密凭据库'],
      ['macOS Keychain', '服务端加密凭据库'],
      ['Keychain', '服务端加密凭据库'],
      ['退出 App 或 Mac 睡眠、断网会暂停管理', '退出服务或服务器断网会暂停管理'],
      ['仅 App 在菜单栏运行且 Mac 可联网时执行', '仅 ColabStudio Web 服务运行且服务器可联网时执行'],
      ['Mac 持续开机', '服务器保持运行'],
      ['Mac 恢复运行后先核对原实例。', '服务重启恢复后先核对原实例。'],
      ['本地回调授权', '远程浏览器授权'],
    ];
    let node;
    while ((node = walker.nextNode())) {
      if (!node.nodeValue) continue;
      let val = node.nodeValue;
      let changed = false;
      for (const [from, to] of replacements) {
        if (val.includes(from)) {
          val = val.split(from).join(to);
          changed = true;
        }
      }
      if (changed) node.nodeValue = val;
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', applyWebUICustomizations, { once: true });
  } else {
    applyWebUICustomizations();
  }
})();
