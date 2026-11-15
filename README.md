# ColabStudio

使用 **MyGo v0.8.3 + macOS WKWebView + Go** 的中文 Colab 实例管理应用。页面内嵌到二进制，不需要 Node、Bun、Python 或本地 Web 服务运行。Python 不是本应用运行依赖。

## 运行

本次交付为 Apple Silicon 构建：

```sh
open build/darwin-arm64/ColabStudio.app
```

macOS 最低版本由 MyGo 打包设定为 12.0。只经过当前 Apple Silicon 主机验证，未测试 Intel。应用采用本机 ad-hoc 签名，未进行 Developer ID 签名或 Apple notarization，不代表已满足公开分发条件。

## 免重新授权：复用已有登录

点击 **复用已有登录**。优先读取 EasyColab 的 `~/.config/colab-cli/token.json`；文件不存在时读取 Colab CLI 的 Keychain 项 `google-colab-cli / oauth_token`。需要完整 refresh token、匹配的 client 信息和 Colab scope。应用先向 Google 验证 refresh grant，成功后复制到自己的加密 vault，不更改或删除原凭据。已有权限不会因为复制而缩小；应用只调用 Colab API。

已在本机实测：refresh token 刷新成功；公开 API 返回 **HTTP 403 / SERVICE_DISABLED**，但同一份凭据调用 EasyColab / Google Colab CLI 的 `GET https://colab.research.google.com/tun/m/assignments?authuser=0` 返回 HTTP 200，能读取 1 个实际分配的实例。应用现在仅在公开 API 返回 SERVICE_DISABLED 时自动切换为 EasyColab 兼容接口。登录复用和真实界面结果见 `evidence/existing-login.json`、`evidence/connected-window.png`。

兼容模式支持实例列表、详情、浏览器打开，以及经过 UI 确认的释放（遵循 Google Colab CLI 的 GET 获取 XSRF token → POST unassign 流程）。未对真实实例执行释放，仅用本地测试服务器验证该协议。

**兼容模式不提供 runtimeSpecs 资格查询，实例总览的通用配置申请保持禁用，提供“前往 Colab 网页申请”。新增“服务”页的 Toybox 使用独立的 Colab CLI assign 协议申请 L4，不虚构 eligible。**公开 API 模式的资格查询与创建代码仍保留；要使用它们，需要 OAuth 所属项目启用 Colab API。原项目不可管理时，需使用自己的项目与 client 授权；原 refresh token 不能直接搭配新 client。

## 账号连接：参考 EasyColab

已参照 `../easycolab/colab_bridge.py` 的 `cmd_auth_url`、`cmd_auth_code` 和 macOS 的授权码回填交互实现：

1. 点击 **导入 EasyColab 配置**。优先查找 `~/.colab-cli-oauth-config.json`、`~/Library/Application Support/EasyColab/venv/lib/python*/site-packages/colab_cli/oauth_config.json`、uv tools 下的 Colab CLI 配置。
2. 找不到时，系统文件选择器允许选择已安装 `google-colab-cli` 包中的 `oauth_config.json`。需要该包使用的 Cloud SDK OAuth client；不能将任意自建 client 搭配 SDK redirect。
3. 点击 **浏览器授权**，在系统浏览器选择 Google 账号并允许访问。
4. 将 Google HTTPS 结果页展示的**一次性授权码**粘贴到本应用，点击 **确认授权码**。不需要在聊天里发送授权码、token 或 Client secret。
5. 授权完成后自动刷新账号实际分配的实例。

**导入 EasyColab 配置**按钮只导入 OAuth client 配置；**复用已有登录**按钮则明确读取已有用户凭据。两者均不更改 EasyColab 文件。本应用不内置或伪造 client 凭据。没有本机配置时，可从官方 `google-colab-cli` 安装包取得配置，或按下节创建自己的 Desktop client。

此流程使用 SDK 注册的 HTTPS 结果页、`token_usage=remote`、`prompt=select_account consent`、PKCE S256。请求 `https://www.googleapis.com/auth/colaboratory` 与 `openid email profile`，以显示当前账号邮箱和名称；不申请 EasyColab 其他功能使用的 Drive 或 Cloud scope。授权码和 PKCE verifier 仅在内存中保存，授权流程 10 分钟有效，提交后清除；失败请重新开始授权。

### 自建 OAuth client

展开 **高级设置：自建 OAuth client**：

1. 在 Google Cloud Console 选择或创建项目，启用 **Colab API**（`colaboratory.googleapis.com`）。
2. 在 Google Auth Platform 配置 Branding / Audience / Data Access；开发测试时把自己的 Google 账号列入 test users，并配置 Colab scope。
3. 创建类型为 **Desktop app** 的 OAuth client，将下载文件内的 `client_id`、`client_secret` 填入应用并保存。
4. 点击高级设置里的 **本地回调授权**。使用系统浏览器和随机端口 `http://127.0.0.1:<port>/callback`，带 state 校验及 PKCE；3 分钟超时。

自建 client 使用本地回调，EasyColab 配置使用 HTTPS 授权码回填，两种回调不能混用。Google 侧同意页面、账号资格及 API 开通情况仍由用户和 Google 项目决定；应用不能代注册或绕过限制。

### 存储与退出

Client secret、refresh token、access token 保存在 AES-256-GCM 加密的 `~/Library/Application Support/ColabStudio/credentials.enc`，权限为 0600；钥匙串只保存一把 32-byte 加密密钥（service `dev.colabstudio.desktop`，account `credential-vault-key-v1`）。运行期间缓存密钥与凭据；一次启动最多读或写一次钥匙串密钥，实例刷新、token 更新与断开连接不再逐条访问钥匙串。拒绝访问会在本进程内记住，避免重复弹窗。不会降级为明文 token 文件。

从早期三条目版本升级时，若本机仍有之前已授权且 client 匹配的 EasyColab 登录文件，会验证 refresh grant 并直接恢复到新加密存储；不读取或自动删除旧的三个钥匙串条目。没有该本机文件时可点击“复用已有登录”或浏览器授权。非秘密 Client ID 与授权模式仍在 config.json。

在系统钥匙串提示中选择“始终允许”，同一应用版本后续可复用系统授权。当前本机没有可用 codesigning identity，应用使用 ad-hoc 签名；重新构建改变代码身份后，macOS 仍可能再确认一次，不能保证跨更新永久免弹窗。正式稳定签名后才能稳定延续应用身份。测试构建使用临时加密文件和内存密钥替身，不访问真实钥匙串。

**断开连接**清除本应用本机 token，不释放实例，也不撤销 Google 侧授权；如需撤销，请到 Google 账号的第三方访问管理中操作。更改配置会清除旧账号 token。

## 已实现

- 账号连接与刷新 token；未连接、加载、空列表与错误状态。
- 列出账号已分配实例；显示实例名称、CPU/GPU/TPU、accelerator、内存 shape 和镜像版本。API 没有返回 GB 容量时不会估算。
- 查询 `runtimeSpecs`，展示是否 eligible；创建前在后端再次校验资格。
- 用户确认后创建，使用 UUID4 requestId；不会自动重试创建。
- 创建返回异步 Operation；点击 **检查申请结果** 使用官方 v1 operations.get 查询，完成后刷新列表。申请记录仅保存在当前窗口，关闭应用后可以刷新实例确认分配结果。
- 释放前显示实例名称和数据丢失提示；取消不发请求，后端要求明确确认与合法 resource name。
- 打开实例时重新读取连接信息，在默认浏览器打开 Google HTTPS 地址；不将 proxy token 发送到前端或添加到 URL。

`eligible` 表示账号资格，不保证实时容量。创建可能消耗计算单元；本次验证没有创建或释放任何真实实例。

## 构建与测试

需要 Go 1.27.3（`go.mod` 声明并可通过 Go toolchain 自动下载）、macOS 和系统打包工具。依赖已固定在 `go.mod` / `go.sum`。

```sh
go mod download
go test ./...
./scripts/build.sh
open build/darwin-arm64/ColabStudio.app
```

构建脚本通过固定依赖中的 MyGo CLI 打包 `.app` 和 `.dmg`，跳过 notarization。只需要 `.app` 时：

```sh
./scripts/build.sh -skip-dmg
```

真实 WebView 检查（仅在未连接状态运行，不提交 Google API 写入）：

```sh
./scripts/test-ui.sh
```

`smoke` build tag 才包含测试钩子；常规 `.app` 不含截图或 JavaScript 测试逻辑。验证产物见 `evidence/ui-smoke.json` 与 `evidence/app-window.png`。

## 验证范围与限制

2026-11-14 已实际构建并运行 MyGo 应用，通过 macOS 窗口 API 确认 `ColabStudio` 可见窗口尺寸 1120×780；通过 MyGo `CapturePage` 捕获真实 WKWebView 页面，并通过 `Eval` 检查真实 RPC 的未授权提示、禁用创建按钮、确认框显示和取消行为。

单元测试使用本地 HTTP 测试服务器验证 Authorization header、列表解析、401/403/429/503 脱敏提示、非法 JSON、资源名与释放确认校验；另验证 EasyColab OAuth client 配置与无活动 PKCE 流程拒绝授权码。

**已验证真实已有登录可刷新并复制到 Keychain**，随后以只读 GET 验证公开接口返回 SERVICE_DISABLED、兼容接口返回 HTTP 200 并展示 1 个实例；没有执行云端创建或删除，没有 Demo 或伪造实例。测试不代表 Google 实时容量、账号资格或所有网络条件已验证。浏览器打开使用 API 的 authenticated URL；如果 Google 端要求额外代理请求头，应用不提供 Jupyter 代理，仍需使用 Colab 网页或兼容客户端连接。

## 官方参考

- [MyGo 入门](https://mygo.egoist.dev/docs/getting-started)、[构建](https://mygo.egoist.dev/docs/distribution)；文档站已重定向至 mygo.run，本次也核对了官方源码仓库中的 docs。
- [Google Colab CLI](https://github.com/googlecolab/google-colab-cli)：远程授权流程参考；没有运行 CLI 代替桌面应用。
- [Desktop OAuth](https://developers.google.com/identity/protocols/oauth2/native-app)
- [runtimes.list](https://developers.google.com/colab/api/reference/rest/v1beta/runtimes/list)、[runtimespecs.list](https://developers.google.com/colab/api/reference/rest/v1beta/runtimespecs/list)
- [runtimes.create](https://developers.google.com/colab/api/reference/rest/v1beta/runtimes/create)、[runtimes.delete](https://developers.google.com/colab/api/reference/rest/v1beta/runtimes/delete)
- [operations.get](https://developers.google.com/colab/api/reference/rest/v1/operations/get)

## 应用图标

正式图标为 `resources/icon.png`（1024×1024、透明外边距）；生成原图和设计说明见 `design/`。`mygo.json` 显式引用图标，MyGo 打包时生成多尺寸 `.icns` 并写入应用包图标配置。修改图标后重新运行 `./scripts/build.sh`。

## 算力与资源监控

- 账号区域展示真实算力余额（CU）、每小时消耗（CU/h）与活跃实例数，使用 EasyColab / Google Colab CLI 的 `GET /tun/m/ccu-info`。
- 实例展示内存已用/总量、GPU 型号与显存、GPU 占用、磁盘已用/总量。资源采用 Google 官方 VS Code 扩展使用的 `GET /api/colab/resources`，容量按 GiB 展示，缺失值显示“未提供”，真实 0 不会被当成缺失。
- “刷新实例”同时更新余额与资源；CPU 按需检测，不持续启动 kernel，不将采样结果推算成实时数据。CPU 型号、逻辑核数与占用率会标注采样时间，并在当前窗口内保留到下次检测。
- CPU 诊断先列出 kernel：已有空闲 kernel 时以独立 globals 执行只读系统采样，不保存历史、不插入 notebook cell；现有 kernel 忙碌或未就绪时不提交检测。没有 kernel 时才创建临时 Python kernel，结束时只清理本次创建的 kernel。采样读取 `/proc/cpuinfo`、`/proc/stat` 与 `os.cpu_count()`，不会创建、重启或释放 Colab runtime。
- CPU 请求使用 Colab 官方客户端的 WebSocket 代理认证与 session 参数；token 仅在 Go 内部使用，诊断日志只记录阶段与消息类型，不输出 token、URL 或 notebook 内容。
- 算力或资源接口单独失败时各区域分别显示错误，不隐藏已成功读取的实例列表；不会用假数据填补。

当前版本已在真实账号与实例验证余额、RAM、GPU、磁盘，以及 CPU 型号/逻辑核数/占用率。界面证据：`evidence/connected-window.png`、`evidence/resource-monitor.png`；`evidence/existing-login.json` 保存脱敏验证结果。验证使用隔离密钥存储，没有读取真实 Keychain。

监控接口参考：[Colab CLI consumption](https://github.com/googlecolab/google-colab-cli/blob/main/src/colab_cli/consumption.py)、[Google Colab VS Code resources](https://github.com/googlecolab/colab-vscode/blob/main/src/colab/client/v1/index.ts)。

## 界面布局

界面采用固定深绿侧栏与独立滚动工作区：实例总览展示算力与资源，登录操作集中在“连接设置”，较长说明与高级设置在右侧展开。运行实例的打开/释放操作位于标题旁，新实例申请默认收起。支持系统浅色/深色主题与窄窗口。

`web/style.css`、`web/app.js` 和品牌图标由 Go 内嵌后注入页面，不依赖外部字体、CDN、前端构建服务或 Node。常规刷新成功以时间更新表达，错误和需要继续处理的授权/申请提示保留。

## 当前账号邮箱和名称

左侧“当前账号”通过当前 OAuth access token 读取 Google userinfo，显示实际名称和邮箱，不使用浏览器默认账号推断身份。复用 EasyColab 已有授权时，如果已有 email/profile 权限，可直接显示，无需重新授权。

身份信息缓存 24 小时，并以当前 refresh token 的 SHA-256 绑定，保存在现有加密 vault 内，不新增 Keychain item。切换账号不会读取旧账号缓存，断开连接或更改客户端配置会清除本机身份缓存。userinfo 暂时不可用或缺少权限时，仅在邮箱位置提示，不影响实例管理，不自动发起授权。

新的浏览器登录请求增加 `openid email profile`，以便用户在正常授权时一并允许读取邮箱和名称。

## 实例日志查看

在实例标题旁点击 **查看日志**，右侧会展开当前实例的 `/content/easycolab_server.log`。可选尾部 100 / 300 / 1000 行、手动刷新、每 5 秒自动刷新（默认关闭），并选择是否跟随最新输出。

读取使用 Jupyter Contents 元数据接口与文件 GET / Range，不依赖 notebook kernel 是否空闲，不创建 kernel、不运行 notebook 代码、不启动 EasyColab 服务。最多读取尾部 64 KiB；如果服务端不支持范围读取且文件过大，会停止下载并显示说明，不将文件开头冒充尾部。日志不存在、为空、轮转或读取失败均有独立提示；读取失败时标明保留的是上次内容。

日志在 Go 中移除 ANSI 控制序列并遮盖常见 token/password/API key/Authorization 字段及当前连接凭据，再以纯文本显示。日志正文不写入本地文件，也不写入应用诊断日志；自定义格式的秘密可能无法完全识别，分享日志前仍应检查。关闭面板、切换账号或释放该实例会停止自动读取；应用在后台时暂停定时读取。

Jupyter 路径根目录差异通过已知的两个文件路径候选处理。此入口查看的是 EasyColab 服务文件日志，不将其冒充任意 notebook 的 cell 历史。

侧栏切换使用互斥页面：实例总览、连接设置、连接说明、高级设置与实例日志仅显示当前选择的内容。异步总览刷新不会把隐藏的总览重新显示；离开日志会停止自动刷新。导航回归检查为 `web/navigation-check.js`，原生验证结果见 `evidence/navigation-check.json`。

## H3 Toybox 服务管理（2026-11-15）

首页“服务”管理 H3 Toybox：启动计费 L4、后台部署 ComfyUI 和模型、打开原有 Toybox Web UI、查看部署/ComfyUI/Toybox/网页连接日志，以及等待任务完成后保存并释放。没有在桌面内重做生成界面。“自动化”配置每周运行时段，实例总览与连接设置仍为独立页面。

### 首次使用

1. 在“连接设置”复用现有 Colab 登录，确认左侧邮箱是需要使用的账号。
2. 在“服务 → 作品保存”点击“连接 Google Drive”。使用同一个账号授予 `drive.file` 和基本身份权限。EasyColab/Cloud SDK client 使用浏览器授权码回填，自建 Desktop client 使用本机随机端口回调。**这是独立 Drive 授权，不覆盖 Colab token；不把 Google OAuth token 上传到运行时。**凭据仍使用现有加密 vault，不新增 Keychain item。
3. 若 OAuth 所属项目未启用 Drive API，展开“Drive API 未启用或需要独立 client”：在自己可管理的项目启用 Google Drive API、配置 OAuth consent / test users，创建 Desktop app，导入下载的 client JSON，再连接 Drive。这个独立 client 不改变当前 Colab 登录。
4. 点击“启动 Toybox”，确认 L4 会消耗 CU。分配前验证 Drive API 可访问，再核对已有实例的 ColabStudio 标记。首次下载约 60 GB，实际时间取决于 Colab 和 Hugging Face 网络；安装日志可手动读取。
5. 在线后点击“打开网页”。采用 Cloudflare 受指定邮箱保护的临时入口，按网页提示收取一次性验证码。默认不公开无认证的生成服务，也不暴露 ComfyUI 管理端口。Quick Tunnel 无 SLA，重连网址可能变化；不支持 SSE，但当前固定版本 Toybox 浏览器使用 HTTP 轮询。
6. “停止并释放”先禁止新生成任务进入，等待 Toybox jobs、Agent runs 和 ComfyUI queue 完成，生成一致备份，传至 Drive 并核对大小和 MD5 后才释放。失败时保留实例并显示错误，仍可能计费。Drive 备份为独立 `.tar.gz` 版本，保留输出、素材、记录和非秘密配置，不包含模型或 API key。下次新建实例会恢复上次已确认备份；它不是持续挂载的 Drive，意外平台回收前尚未备份的新作品仍可能丢失。

保存目前由本机转发：临时下载包权限 0600，再以 8 MiB 分块上传 Drive，最大备份 50 GiB。上传或校验失败不释放；已完成上传在释放重试时按服务和快照标记查询复用，避免重复备份。正常结束删除本机临时文件；异常终止留下的 `ColabStudio-backup-*.tar.gz` 可由用户清理。Drive 容量不足同样阻止普通释放。

### 菜单栏与自动化

- 关闭窗口保留菜单栏管理器；菜单可重新打开窗口或 Toybox 网页。明确“退出”停止本机管理，不默默释放云端实例。未安装 LaunchAgent 或其他后台服务。
- 心跳每 30 秒刷新 Colab 正常连接并读取云端监督状态，云端监督每 5 秒写状态。维护真实连接与服务健康，不执行空转代码或伪造浏览器操作。监督进程持久保存自己启动的进程身份，重启时核对 PID、启动时间和命令后恢复观察；不杀未知进程。
- 心跳不绕过 Colab 最大寿命、账号资格、实时容量或平台回收；免费且无正 CU 余额的 Colab 对外部 Web UI 使用另有限制。Mac 睡眠、关机或断网期间不能保证维护和定时操作。
- 每周规则支持指定日期、时区与跨午夜。默认关闭，启用需确认自动计费和释放授权。保存规则不补开当前时段；应用重启或睡眠错过的启动跳过。手动停止后，本时段不重新启动。定时结束等任务完成和 Drive 保存，不强制中断任务。
- 服务记录为 `~/Library/Application Support/ColabStudio/services.json`，权限 0600；跨进程文件锁防止多份 App 同时调度。申请前同步持久记录 notebook hash / operation，结果不明仅查询同一次申请，不换 UUID 或重复 POST。不认识的实例不安装、不抢占、不自动释放。
- 现有记录绑定账号邮箱。切换账号后原服务规则暂停，不把旧服务转给新账号；申请、备份或释放执行期间拒绝切换账号。服务记录损坏时暂停自动操作，不能通过删除记录来“重试”申请。
- 部署失败仍可能持有计费实例。先看日志；监督进程未确认在线时可在原实例“继续部署”，不另开 VM。紧急情况下可到“实例总览”明确确认释放，但该通用操作不会执行服务备份，未保存数据可能丢失。

### 固定版本与验证边界

上游 Notebook：[H3_Toybox.ipynb](https://github.com/dseditor/minimax-h3-colab/blob/44708d4aa17e30fc8b56be375387ba2106936a9c/H3_Toybox.ipynb)。Toybox UI 部署来源为 fork [shaoyun/minimax-h3-colab](https://github.com/shaoyun/minimax-h3-colab)（基线 `44708d4` + 中文本地化 + 界面调整，提交记录见 fork）。`templates/toybox-lock.json` 固定 Toybox fork、ComfyUI 和插件的 Git commits、16 个模型的 HF revision/大小/SHA-256、cloudflared 2026.11.0 及 SHA-256、直接 pip 依赖。PyTorch 使用官方 cu130 索引中相互匹配的 torch 2.11.0 / torchvision 0.26.0 / torchaudio 2.11.0。上游 requirements 的传递依赖未完整锁定，安装冲突会报错，不自动换版本或另申请实例。

远端安装和监督脚本在 `templates/`，仅在 Colab 执行；本机应用仍只需 Go/MyGo。模板预检 GPU capability >=8.0、驱动 >=580、空闲磁盘 >=72 GiB；默认申请 L4，按用户所选类型提交，不自动改型。HF 匿名下载，遇限流在部署日志报错；当前未提供 HF token 设置。

```sh
go test -race ./...
python3 scripts/test-toybox.py
./scripts/build.sh
open build/darwin-arm64/ColabStudio.app
```

测试覆盖真实停机调用链的替身请求，确认 Drive 校验失败不发送释放、成功才释放；覆盖固定 assignment hash、跨午夜/时区规则、跨进程锁、备份秘密排除、队列变化与不安全恢复路径。真实 WKWebView 截图及页面互斥、按钮保护和滚动结果见 `evidence/toybox-services.png`、`evidence/toybox-automation.png`、`evidence/toybox-ui.json`。验证使用隔离 vault，不读取真实 Keychain。

**本次未新建 GPU、未下载真实大模型、未向用户 Drive 写文件、未释放现有实例。**部署、Cloudflare 邮箱验证及 Drive 授权/上传尚需用户在应用中实际完成；本地测试通过不代表真实 GPU 全流程已成功。

## 菜单整理（2026-11-15）

服务和自动化使用统一线性图标。当前账号姓名、邮箱、连接状态和断开连接操作移入独立“账号”页面，侧栏仅保留导航；移除侧栏底部文字。实例总览删除通用“申请新实例”区域及前端调用入口，Toybox 仍通过“服务”启动。

## 唤醒机器与部署服务修复

“唤醒机器”现在独立于 Drive 授权：确认计费后直接走 Colab assign challenge → POST 分配 L4，显示“机器已唤醒”和实例名称；这一步不安装模型或启动 Toybox。已有机器只核对并复用，不再次申请。接着连接 Drive，点击“部署并启动”；Drive 已连接时也可直接“唤醒并启动”。

修复了已有机器尚无 supervisor 时，启动先检查服务状态而无法进入部署的问题。未部署的机器可点击“释放机器”明确确认释放，不要求不存在的服务先完成备份。已部署服务仍沿用等待任务、保存 Drive、校验后释放的流程。机器唤醒后即计费，不用时请释放。

回归证据：`evidence/wake-check-before.json` 记录旧版缺少入口的失败；`evidence/wake-check.json` 通过真实 MyGo RPC 和本地 HTTP 替身确认不连接 Drive 也能申请机器，POST 仅一次。`wake_test.go` 验证无 Drive 唤醒、重复复用、分配结果不明不重复 POST、已有机器先部署再健康检查，以及空机器释放。均未创建或删除真实云端实例。

## 下载、校验和模型装载进度

服务页每 5 秒尝试更新进度，并默认自动刷新当前选择的部署/ComfyUI/Toybox日志，可关闭自动更新或跟随最新。离开服务页或应用处于后台时暂停前端轮询。机器及日志切换后丢弃迟到的旧内容；更新失败保留上次进度并标明失败。

新部署通过 Hugging Face 官方 `tqdm_class` 回调报告真实下载字节：当前文件名、序号、当前文件和总下载量、速度与当前文件预计剩余时间。哈希校验独立显示读取进度，不将下载完成冒充校验成功。依赖安装、作品恢复、引擎/网页启动显示实际阶段和已用时间，不生成虚构百分比。

模型在任务执行时按需装载。读取 ComfyUI 实际日志中的 `Requested to load`、`loaded completely`、`loaded partially` 和显存错误，显示最近装载事件；部分装载不表示全部权重进入显存。引擎未提供加载字节比例，因此只显示状态和观察到该状态的时间，不显示推算百分比。

现有实例没有 `progress.json` 时兼容为实时日志和已有文件序号，不重启机器、不重下模型，也不凭空补历史百分比。下一次新部署启用结构化下载进度。验证：`python3 scripts/test-progress.py`、`python3 scripts/test-toybox.py`、`go test -race ./...`；真实 WebView 检查见 `evidence/progress-check.json`。验证不下载实际大模型，不操作真实 GPU。

## 启动前选择机器类型

服务页提供 L4 / A100 / H100 选择框，默认 L4，旧记录自动按 L4 处理。选择会持久保存，手动唤醒、唤醒并启动和定时启动均使用该类型；确认框显示具体 GPU。选项不代表账号已有资格或有即时容量，失败不会自动换型。Toybox 的 GPU、驱动与磁盘预检仍会执行。

机器已分配或申请结果待确认时，前端和后端均锁定类型，不会通过切换类型再开一台。已启用定时规则时，改型需要确认后续计费配置。选择和保存本身不分配实例。回归测试 `machine_type_test.go` 与 `evidence/wake-check.json` 验证默认 L4、选择 A100、计费确认、类型锁定及三种 GPU 的 GET/POST 参数。

同时修复 Colab assign challenge 的 `variant: "GPU"` 字符串与最终分配响应整数枚举的兼容解析；此前只按整数解析会导致有效申请令牌响应被误判格式错误。相关协议以官方 Google Colab CLI client 为依据。测试使用本地替身，未验证真实硬件部署成功。

## Drive 授权取消与重试

授权准备、等待浏览器或验证授权码时，可以“取消本次授权”，也可直接“重新发起授权”。取消会关闭本机回调监听、清空本次授权码和 PKCE 状态；不删除已有 Drive token，不调用 Google 撤销接口。重新发起使用全新的 state/PKCE 与独立请求标识，不必等待旧流程 5 分钟超时。

旧流程的取消、超时、授权码和迟到回调仅影响其自身。即使取消发生在 token exchange 过程中，迟到的成功结果也不会保存或覆盖新流程。Drive 授权状态使用独立锁，等待网络或打开浏览器时不持有该锁；前端取消/重试不被全局加载按钮阻塞。导入另一个 Drive client 同样会清理旧监听。

`drive_flow_test.go` 覆盖立即重试、现有凭据保留、取消时关闭 loopback、旧回调隔离、准备/验证中的取消；`evidence/drive-flow-check.json` 为真实 WebView 的按钮及状态回归结果，使用隔离凭据和浏览器替身，不发起真实 Google 授权。

## Drive 账号与明确保存位置

浏览器完成 Google 页面不等于应用已保存授权：Drive 账号必须与服务绑定的 Colab 账号一致。界面分别显示需要的邮箱、已保存的 Drive 邮箱和本次选择的邮箱。选错时说明两者并显示“本次授权未保存 / 重新选择 Drive 账号”；成功匹配时显示“Drive 已连接 / 重新授权”。不会静默接受错误账号或删除此前有效凭据。

默认位置是 **Google Drive → 我的云端硬盘 → ColabStudio / H3-Toybox → Toybox-YYYYMMDD-HHMMSS.tar.gz**。连接后可点击“创建并打开保存目录”，不必先启动机器或做第一次备份；只有用户点击或开始备份时才创建目录。已有目录直接打开。此功能在停止时打包、上传并校验，非实时挂载。用户手动移动或重命名目录后，以打开的 Drive 目录为准。

## Toybox 网页双层鉴权

新部署默认启用网页自身的访问密码，外层仍使用 Cloudflare 指定服务账号邮箱的验证码。获得链接本身不能读作品或调用生成接口。访问密码由本机随机生成，在既有加密 vault 中保存；点击“查看密码 / 复制密码”才提供给用户，不在 URL、部署日志、服务记录或作品备份中保存明文。远端只保存 PBKDF2-SHA256 verifier 与会话签名密钥，文件权限 0600。

登录成功设置 8 小时签名会话，Cookie 为 HttpOnly、Secure、SameSite=Strict；写请求检查 Origin，错误密码限速，鉴权配置缺失时拒绝访问。只给本机监督进程保留三个私有只读健康/队列接口，经过隧道的请求不能使用该通道。会话过期后刷新网页重新登录。每次应用鉴权配置会更换会话密钥并使旧会话失效。

现有旧实例不会被静默重启：在“Toybox 网页鉴权”点击“启用 / 更新网页鉴权”，确认后仅在队列及 Agent 全部空闲时更新，必要时只重启受本应用管理的 Toybox 网页进程；不重建 VM 或重启 ComfyUI。进程身份不符、忙碌或无法确认状态时拒绝操作。原生“打开网页”必须收到匹配且近期的密码保护确认后才打开，不能把配置计划冒充保护已生效。

本地安全验证：`python3 scripts/test-toybox-auth.py` 覆盖链接无密码不能生成/读作品、正确密码、Cookie 属性、Origin、尝试限速、旧会话失效、私有接口防冒用及配置丢失时拒绝访问。`go test -race ./...` 验证密码不进入远端明文配置、保护确认与 Drive 账号/目录逻辑；`evidence/drive-status-check.json` 记录真实 WebView 状态显示检查。本次没有在真实实例执行网页进程更新，也没有向用户 Drive 创建目录；这些操作由用户在 App 中明确点击。

### 简体界面与资源历史（2026-11-15）

- 当前锁定的 H3 Toybox 上游界面原为繁体。本应用内置 `toybox-zh-cn.json` 文案补丁，新部署在启动网页前自动转为简体，使用 EasyColab 的大陆界面用语，保留繁体、简体和英文 `@` 素材标签兼容。补丁由 `zhconv==1.4.3` 生成，绑定上游 SHA-256，运行时仅使用 Python 标准库；已转换文件不会重复修改，未知版本拒绝覆盖。既有服务可在任务空闲时点击“更新简体界面”，只重启自有 Toybox 网页进程，机器和 ComfyUI 不重启，浏览器需重新登录。
- 实例总览与服务页每 15 秒自动刷新资源。对本应用管理的机器自动安装独立轻量采样器：读取 `/proc/stat`、`/proc/meminfo` 和 `nvidia-smi`，不加载模型或 CUDA context。后续 App 只读取小型采样文件，不反复运行 Notebook 单元格。CPU、GPU 利用率、RAM 与显存曲线保存最近 60 个真实样本（约 15 分钟）；CPU 首次采样需一个间隔，无数据不填零，断线显示中断。采样器有文件锁，不重复常驻；其目录与业务部署分离，尚未部署服务的机器仍可直接释放。
- 未由本应用管理的实例仅通过现有只读 Colab 资源接口采样 GPU/RAM/显存，不擅自安装进程，CPU 保持未知，可手动诊断。页面隐藏或切到其他菜单时暂停本地资源轮询，失败逐步退避到最多 60 秒，重新连接后恢复；远端采样器继续低频记录，Mac 睡眠期间无法展示实时状态。
- EasyColab 的后台状态同步间隔约 2.5 秒，生成中约 15 秒同步全局状态。本应用统一采用 15 秒，减少请求与界面刷新。
- 当前心跳是 App 每 30 秒维护连接和检查运行计划，云端监督进程每 5 秒检查自有服务并尝试恢复。App 必须运行（可关闭窗口留在菜单栏），Mac 联网且未睡眠。它不模拟用户操作，也不能防止 Colab 的闲置回收、最大运行时长、余额耗尽或平台中断；一直开着 Mac 并不等于云端永久在线。依据：[Google Colab FAQ](https://research.google.com/colaboratory/faq.html)。

离线验证：`python3 scripts/test-telemetry.py`。真实 MyGo 资源曲线截图验证使用 smoke 构建、隔离 vault 和明确 Demo 样本；不创建计费实例，不写真实 Drive，也不重启用户当前服务。

资源曲线补充：磁盘空间沿用现有只读 Colab 资源请求，每 15 秒记录已用/总容量和使用率，不增加云端执行或额外请求。各磁盘分开绘图，历史从本次连接开始，保留最近 60 点（最多 15 分钟）；手动刷新页面保留记录，断开账号清除，缺失指标留空。所有曲线均显示 0%、50%、100% 纵轴及每 5 分钟一个实际时间刻度。

### 服务就绪与访问地址

服务页顶部独立显示权重下载/校验、模型最近装载事件和服务健康状态，默认每 5 秒刷新。下载字节到 100% 并不等于校验完成；只有全部文件的字节和校验计数一致，或同版本安装记录存在时，才确认权重下载与校验已完成。旧安装记录会明确标注。模型按任务装载，界面显示模型名称、装载/部分装载/完成/显存不足事件及最近显存采样，不宣称所有模型当前都常驻 GPU。

ComfyUI 与 Toybox 的健康检查通过、心跳有效、网页地址生成且访问密码确认后，显示“服务就绪，可打开网页”。地址栏可以选择文字，并提供复制地址、打开网页按钮；两项操作读取归属匹配的远端最新地址，不依赖可能滞后的本机缓存。地址不包含密码、proxy token 或 API key；重建机器后可能改变。状态过期时保留地址并标注未确认，停止宣称服务可用。打开网页仍保留密码保护检查。

就绪区窗口验收见 `evidence/readiness-check.json` 和 `evidence/service-readiness-demo.png`（明确 Demo 数据）；后端使用隔离 HTTP 与模拟剪贴板测试验证，没有新建或重新部署计费实例。
