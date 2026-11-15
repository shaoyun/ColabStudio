# ColabStudio：Toybox 服务管理方案（已按用户选择实施）

## 产品结果

保留 MyGo + Go macOS 应用，首页改为“服务”，以 H3 Toybox 为第一个内置部署模板。本机只管理服务生命周期、算力、日志、作品备份和启动规则；生成、素材、工作流、Agent 等操作使用上游 Toybox Web UI。账号继续复用现有加密 vault，不新增 Keychain item 或要求再次 Google 授权。

侧栏为“服务 / 自动化 / 实例 / 连接设置”。各页互斥，左侧固定、右侧独立滚动，保留现有图标、无标题栏布局与系统深色。实例页保留真实余额、CPU/GPU、内存和手动释放功能；连接帮助并入设置，不增加常驻导航。

服务卡展示状态、部署步骤、GPU、CU/h、最后心跳、下次计划；主操作为“启动”“打开网页”“停止并释放”。详情集中提供部署日志、ComfyUI 日志、Toybox 日志、作品备份状态。

## 已核实的实现依据

- 上游：https://github.com/dseditor/minimax-h3-colab ，本次读取 commit `44708d4aa17e30fc8b56be375387ba2106936a9c`，以其中 H3_Toybox.ipynb 为安装清单依据。2026-11-15 起 UI 来源改为用户 fork https://github.com/shaoyun/minimax-h3-colab ：fork 在基线 `44708d4` 之上内置中文本地化并承载界面调整，部署锁定 fork commit（见 `templates/toybox-lock.json`），不再运行 localize。
- ComfyUI 监听 127.0.0.1:8188，Toybox 监听 127.0.0.1:8190。GPU 首选 L4；部署前检查 compute capability、驱动 >=580、磁盘 >=72 GiB。没有 L4 不自动升级到更贵配置，T4 不作为该 int8 模板的后备。
- 默认包含图片模型与本地改写模型；下载体积和初始化进度在启动前说明。模型、插件和安装依赖通过版本清单固定，不把上游源码复制成一份长期分叉。
- Notebook 的 Google Drive mount 和 serve_kernel_port_as_window 依赖交互界面，不能直接照搬到无头部署。Google 官方输出工具也注明新窗口端口代理存在浏览器限制。
- 云端创建沿用 Google Colab CLI 的 assign challenge/POST 协议；兼容模式目前只有列表和释放，需新增该能力，不依赖当前返回 SERVICE_DISABLED 的 public create 接口。
- Cloudflare Quick Tunnel 官方源码支持 --allowed-mail：浏览器以指定邮箱一次性验证码认证，不要求 Cloudflare 账号。个人首版采用受保护临时网址；不启动公开无认证的隧道。该服务无可用性保证，网址随隧道重建改变。

参考：https://github.com/googlecolab/google-colab-cli ，https://github.com/cloudflare/cloudflared/blob/master/cmd/cloudflared/tunnel/quick_tunnel.go ，https://developers.cloudflare.com/tunnel/get-started/quick-tunnels/ ，https://research.google.com/colaboratory/faq.html 。

## 调用关系

```text
ColabStudio（本机 MyGo UI + Go 管理器）
  ├─ OAuth / Colab API ── 分配、对账、释放自己管理的实例
  └─ 已认证 Jupyter 通道 ── 安装、日志、心跳、队列及备份
                                │
                     Colab：ComfyUI + Toybox
                                │
                   受邮箱验证保护的 Cloudflare 隧道
                                │
                         用户系统浏览器
```

本机不新增 Python 运行依赖；Python 安装与监督脚本只在已经提供 Python 的 Colab 内执行。Google refresh/access token 留在本机；仅 Jupyter proxy token用于本机到云端的连接，不上传主 Google 凭据。可选 HF token仅在受保护设置中录入并通过加密通道提供给部署过程；不用聊天收集 token。Gemini 等生成 API key继续由 Toybox Web 设置管理，非启动必需。

## 生命周期与防重

1. 以账号身份和服务类型 toybox 建立持久服务记录，部署版本与配置 hash作为记录属性，配置变化不会自动产生第二个同类实例。记录存储不含明文凭据，秘密留在现有 vault。
2. 启动使用跨进程文件锁与持久 operation UUID；先列出账号真实 assignments，再核对本地 endpoint和远端 ColabStudio marker。运行中的同服务直接复用；已有部署恢复观察进度。
3. 不把“同一 GPU”视为“同一服务”。未知实例仅展示，不安装、不停止、不抢占。远端 marker与本地绑定不一致时停止自动操作并提示处理。
4. assign使用固定 notebook hash；请求超时或返回不明确先标记“正在核对”，不自动换 UUID重试创建。无法确认是否已分配时阻止再次申请。
5. 上传并异步运行安装器；步骤、PID、版本、错误和进度写入自有目录。下载可恢复，已验证文件跳过。重启本机应用后通过 marker、进程与端口健康检查恢复状态，不重复安装或再申请。
6. 状态为：未启动、核对中、申请中、安装中、启动中、在线、连接异常、等待任务结束、备份中、释放中、已停止、失败。健康失败与实例消失分开显示；无网络绝不推断实例已释放。
7. 手动启动显示账号、L4、预期下载规模与计费提示，由用户点击。手动释放必须确认；自动规则启用时明确授权所管服务的创建与释放。安装失败是否释放仅在启动确认里授权，且只适用于该次明确创建的实例。

## 心跳与恢复

管理器每30秒检查自有服务状态、进程和健康接口，维护正常认证连接，凭据到期刷新并指数退避重连。仅恢复自己启动的服务进程和隧道，不杀未知进程，不因健康请求失败新建付费实例。

不伪造鼠标活动、不持续运行空转代码；Colab 的最大寿命、容量和平台回收不能通过心跳消除。Google FAQ明确免费且无正 CU 余额的运行时限制以外部 Web UI 为主的交互。启动页展示真实余额与适用说明，不承诺永久在线。

Mac 睡眠、关机或断网期间本机无法保证执行计划或维持连接；恢复后先对账，不补开错过的启动任务。是否增加退出 App 后仍运行的本机后台服务，等待用户选择。推荐关闭窗口留菜单栏，明确退出才停止管理器；退出不默默声称云端也已释放。

## 自动化规则

首版按周几、开始/结束时间和显式时区配置运行时段，支持跨午夜；展示接下来一次启动和停止。默认规则关闭，启用时显示“会消耗 CU”并要求用户确认一次。规则只操作已绑定账号下的 toybox 服务，切换账号不会继承执行。

同一规则触发有持久执行键，应用重启、时钟跳变和多窗口不重复执行。错过的启动跳过；唤醒时若已超过结束时间，只对账并处理原有自管实例的停止流程。手动停止后抑制本时段再次启动，下一个独立时段才恢复规则。启动失败不高频重试、不换 GPU、不启动第二台。

忙碌判定同时读取 Toybox jobs、Agent runs和 ComfyUI queue，覆盖 queued/running/joining及 Agent任务；接口失败视为状态未知。停机时先阻止新任务进入，再按用户选定的忙碌策略收尾。数据保存成功后才能进入普通释放流程；备份失败保留实例并显著提示仍可能计费，不能把失败视为备份完成。

## 用户已确认的行为

2026-11-15 用户已逐项确认：

1. 已确认：定时结束遇到任务时，等任务完成后再关闭，不到点强制中断。
2. 已确认：作品保存到 Google Drive，用户接受额外授权。通过本机独立最小权限授权上传到应用管理目录，不上传主 Google refresh token到 Colab；备份范围包括输出、素材、作业/Agent记录和非秘密配置，排除模型缓存与 secrets。校验上传完成后才释放；上传失败保留实例并提示仍在计费。
3. 已确认：App 在菜单栏后台运行时执行。关闭窗口保留管理器；明确退出后不再执行。不安装 LaunchAgent，Mac 关机或睡眠期间不能保证执行。

Google Drive路线需要用户主动授权；不能将当前拥有广权限的 Google refresh token上传 Colab。无论选择哪条路径，云端临时目录都不标记为永久存储。

## 代码边界和交付

这是超过8个文件的功能扩展，包含一个新增云端部署/监督组件。后端沿用 Studio、现有 vault、compatible、monitoring和 logs，新增服务注册/对账、远程部署、调度及模板版本清单；前端增加互斥服务/自动化页面；主程序增加菜单栏生命周期；README同步连接、备份和规则说明。

不移植 EasyColab 的聊天、模型管理、生成面板、多端同步或通用服务器控制；不把 Cindy 的调度器当成本应用后台；不新增本机 Python、Node或另一个桌面框架。

验收：

- 本地 fake Colab/Jupyter与部署测试覆盖正常启动、失败、超时不明、重复点击、并发进程、重启恢复、未知实例保护、账号切换与 token脱敏。
- 调度测试覆盖跨午夜、时区、重启重复触发、Mac错过时段、手动停止、忙碌、备份失败和释放失败。
- Web入口验证未认证被拒绝、允许邮箱登录、Toybox资源上传/下载与页面路由；心跳/日志失败不创建新实例。
- go test -race ./...；真实 MyGo WebView检查页面互斥、滚动、部署状态、停止确认与计划预览。所有 smoke构建使用 COLABSTUDIO_SMOKE_VAULT=1，不碰真实 Keychain。
- ./scripts/build.sh 产出 build/darwin-arm64/ColabStudio.app和DMG，并完成签名检查及实际开窗。
- 未经用户明确授权，不为验收新开 L4、下载模型或释放任何现有云端实例。没有真实GPU部署验证的部分如实列明。

回退时禁用规则、停止本地管理器，保留账号与作品备份；远端实例不随软件回退自动销毁。正在计费的自管实例明确列出，由用户手动确认处理。
