# 交接文档

给接手这个项目的人（或者下一个 AI 会话）看的：项目现在是什么样、为什么这样设计、做到哪儿了、接下来做什么、有哪些坑。
用户说明见 [README](../README.md)，内核和网关之间的协议见 [protocol.md](protocol.md)。

最后更新：2026-10-21，中断 / 撤回、电脑配对接入阶段完成。

## 一句话

nano-agent 是作者自己的轻量 bot 智能体：**C 内核**（agent 循环、权限、沙箱里执行工具，没有状态）+ **Go 网关**（上下文、模型、聊天平台、网页界面，唯一有状态的中心）。网关可以同时管理好几个内核（节点），网页能管理 SSH 服务器并自动部署内核；旧内核侧 SSH 执行器模式仍可使用。

## 目录

```
kernel/                C 内核和执行器
  src/main.c           内核：poll 主循环，连网关、派发工具、列目录
  src/core.c/h         循环状态机（sans-I/O）：预算、权限模式、审批
  src/runner.c/h       子进程任务（执行器、ssh、上传）
  src/remote.c/h       SSH 远程执行：准备服务器（检查/上传/现编）、拼 ssh 命令
  src/skills.c/h       技能目录扫描、SKILL.md 解析、工具定义和按需加载
  src/conf.c/h         kernel.conf 解析（含 remote = …）
  src/nexec_*.c        执行器 nano-exec：每次工具调用启动一次，套上 Landlock + seccomp 再执行
  src/sandbox.c        Landlock / seccomp
  tests/               test_core.c（状态机）、test_exec.py（执行器）、test_host.py（假网关驱动真内核，含假 ssh）
gateway/               Go 网关
  cmd/nano-gateway     入口；chat.go 是 `nano-gateway chat` 终端客户端
  internal/app         调度：会话、窗口（endpoints）、节点（nodes）、补聊天记录（recap）、命令
    compact.go         超限后异步生成摘要，取消、失败裁剪和用量统计
    undo.go            撤回（/undo、「撤回」、平台撤回事件）和不带斜杠的「停」
  internal/channel     聊天渠道：cli（unix socket）、web、telegram、feishu（含扫码创建）
    stream.go          平台流式显示：后台合并、限频、结束补齐、失败时发送完整文本
  internal/kernel      内核连接（hub，多节点），Attach 可接入 SSH 通道（不查口令）
    pair.go            电脑配对：待确认请求、专用口令（只存 SHA-256）、移除
  internal/device      启动包、SSH 部署和连接管理，密码只写私有文件
    assets/            启动脚本、可重建的源码与接入技能包（make 自动更新）
  internal/session     历史（JSONL 只追加）、会话设置、裁剪
  internal/llm         OpenAI 兼容接口（流式）、mock
  internal/config      配置：gateway.toml + data/config.json（网页改的），schema 驱动设置页
  internal/web         网页接口 + 嵌入的前端（dist/）
  web/                 前端源码：Svelte 5 + Vite（make web 构建到 internal/web/dist）
scripts/               run-local.sh（make run）、e2e.sh
skills/device-connections/  接入技能与完整操作 / API 指南，启动包会携带
docs/                  protocol.md、本文件
```

## 怎么跑、怎么测

```sh
make run          # 编译，后台起内核、前台起网关（Ctrl-C 停）；打印网页登录链接
make chat         # 另一个终端：终端聊天（nano-gateway chat），随时进出
make test         # C 测试（ASan）+ Go 测试 + e2e（mock 模型，真实内核/执行器/网关）
make web          # 改了前端后重新构建（npm run check && build），产物要提交
cd gateway && go test -race ./...
```

- e2e（scripts/e2e.sh）现在 47 项：终端聊天、工具、审批、模式、沙箱、发「停」和 Ctrl-C 停下任务、撤回（含历史文件截断）、会话切换、两个内核替补、SSH 服务器（假 ssh）、终端重连、本机与远程技能，以及超限摘要、保存和继续用工具。
- 配对的完整流程（真实内核从源码构建 → 请求配对 → 打印确认码 → 允许 → 存口令重连）在 device 包的 TestLinuxPackageBuildsFromSourceAndConnects 里；真实 OpenSSH 测试顺带检查服务器上的配置里没有口令。
- 设备模块还有真实 OpenSSH（临时 sshd + 密钥）、模拟密码 askpass、源码构建启动包和取消测试；本轮 Windows PowerShell 启动器实测通过 WSL 连入并在 Windows 盘读写。
- 网页的界面检查用 headless Chromium + CDP 脚本截图（在之前会话的 scratchpad 里，不在仓库里）。演示环境用 mock 模型，别连真模型、别动用户的 data/。
- 本机是 Fedora 43（WSL），glibc 2.42，**不能静态链接**（没装 glibc-static），所以 `nano-exec-static` 不会生成。

## 已经完成的（按提交）

| 提交 | 内容 |
|---|---|
| b924520 | 初版：C 内核 + Go 网关 + 协议 |
| b97b56c | 内置工具 Read/Write/Edit/Grep/Shell；沙箱执行器（Landlock + seccomp）；权限模式 auto/ask/readonly/none |
| 2bab4a0 | 流式输出、思考强度（只传 OpenAI 风格的 reasoning_effort）、前缀缓存优先的上下文 |
| 1741bed | 网页管理界面：拓扑图、点节点改设置、Ctrl+K、审批、人格 |
| 60d2c8a | 目录浏览、飞书扫码创建机器人、没有工作目录就只聊天、默认代理、只支持私聊、拖动节点 |
| 2c8dfa0 / 8883a05 | 会话和聊天窗口分开：所有窗口能接入同一个会话（主会话 main），/session |
| a8bf7b6 | 不是管理员时说明原因；网页里一键设为管理员 |
| f08e92d | 切换会话/回来时补上没看到的聊天记录（/history）；网关和终端聊天分开（make chat） |
| 131e2e2 | 多个内核（节点）：会话绑定机器，掉线时替补只聊天，/node |
| b3c1929 | SSH 远程执行：服务器作为节点（经由某个内核），自动上传/现编执行器 |
| fb07909 | SKILL.md 按需加载、技能热更新、网页列表、SSH 附带文件上传与只读访问 |
| 8c2e48f | Telegram 发送后编辑、飞书 CardKit 流式卡片，含思考/工具/用量、长回复分段和失败降级 |
| 77f4944 | 超限时合并旧摘要和旧对话，完整保留近期工具链与当前轮；支持取消、失败裁剪、重启恢复和统计 |
| c10c4d4 | 跨端自动同步、默认折叠思考与工具、完整原始历史查询，修复 WSL DrvFS Permission denied |
| 4bc5110 | Windows WSL2 / Linux 启动包、网页 SSH 密码 / 密钥 / config 接入与断开、自动部署、stdio 通道、接入技能 |
| 中断撤回与配对 | 各端都能停（「停」、Ctrl-C、Telegram 按钮、网页输入框旁）；撤回最近一轮（/undo、「撤回」、网页按钮放回输入框、飞书撤回事件）；电脑启动包不带口令，首次连接配对；SSH 部署不留口令 |

## 关键设计（改之前先理解）

1. **前缀缓存优先**：系统提示词和工具定义固定不变；历史只追加；时间、来源（`[2026-10-21 周三 15:08 · Telegram]`）、环境变化（`[系统] 在 X 上工作。工作目录：…`、替补说明）都追加在用户消息前面，**变了才追加**（Settings.EnvNote 记着上次说的）。历史太长时一次裁掉一大段（滞回），之后又只追加。任何会改动前面字节的改动都是缓存回归。
2. **网关是唯一有状态的中心，内核无状态**。会话 = 一段对话（上下文、工作目录、模式、思考强度、人格、机器）；窗口（endpoint）= 终端、某个私聊，接在会话上（data/endpoints.json）。管理员的窗口默认接主会话；别人各自一个会话。
3. **节点**：每个内核是一个节点（按 hello 的 node 区分）；内核能 SSH 到的服务器也是节点（经由那个内核）。每个会话在一台机器上工作（Settings.Node → workspace.node → 和网关同一台机器的内核）。每一轮 `pick()`：会话的机器能到就用；到不了就替补（本机优先，其次最近连上的），**只聊天**并说明原因。默认工作目录只属于默认机器。
4. **沙箱**：执行器每次调用启动一次，Landlock（工作目录读写、系统目录只读、HOME 其他地方不可见）+ seccomp（禁 AF_UNIX、io_uring 等）+ 清理环境变量。工作目录不能包含 nano-agent 自己（内核、网关、执行器各自把关）。没有沙箱的机器，自动模式下 Shell 也要审批（按每一轮实际执行的机器算）。
5. **旧的内核侧 SSH 远程**：服务器上不跑内核。内核后台准备执行器（检查 → 上传 → 现编），工具调用是 `ssh … 目标 sh -c 'exec 执行器'`，ControlMaster 复用，关 stdin 取消。执行器放在服务器 `~/.cache/nano-agent/nano-exec-<指纹>`。
6. **安全**：网页只听本机、密码登录、HttpOnly SameSite=Strict、写操作要 X-Nano 头、本机时查 Host；终端聊天走数据目录里只有自己能连的 unix socket（0600 + SO_PEERCRED）。内核口令三种：共用 token（本机、手写 kernel.conf 的）；配对得到的专用口令（绑定节点名，冒充不了别的节点，网关只存 SHA-256，网页移除即作废）；网关自己 SSH 启动的 stdio 内核不查口令（Hub.Attach），服务器上不留网关口令。TCP 本身不加密，跨网络使用私有网络或隧道。
7. **技能**：由内核管理，默认目录是 kernel.conf 旁的 skills/（skills 配置可改）。名字和简介放在 Skill 工具定义里，正文调用时读。下一轮开始和空闲每 30 秒扫描；元数据变化用 tools 消息同步网关和网页。正文、附带文件更新不改变工具定义。SSH 调用 Skill 时把所选技能打包上传到服务器缓存，后续 Read/Shell 在沙箱里只读访问。沿用 runner 的异步执行和取消，不另起技能服务。
8. **聊天平台流式显示**：RunView 只追加到内存，streamRun 的后台协程合并更新，Close 不阻塞调度锁，但会补齐尾部并关闭卡片。Telegram 每秒编辑消息，飞书每半秒用 CardKit Content 更新卡片，每张卡片的 sequence 严格递增。长回复分段，错误只记录一次，结束时发送完整文本。无正文增量的模型最终结果也会交给 RunView。飞书需要 cardkit:card:write，扫码注册已加入；平台接口测试用 httptest 模拟，无真实凭据的渠道仍需使用时验证。
9. **上下文压缩**：历史超限且有完整旧轮次可切时，额外用当前模型生成摘要。已有 Summary 和被切掉的历史作为 JSON 数据传入，不传工具、人格和推理记录；输出 2048 token，支持 none 时关闭思考，60 秒超时。最近消息和当前轮留在原位置；摘要连同 EnvNote 存在 Settings.Summary，只在压缩时替换，JSONL 不改写。生成失败沿用裁剪，已有摘要不丢；取消或迟到结果不提交，保存失败不推进 Base。额外调用和 token 都计入本轮及全局统计。

10. **平台同步与卡片**：Telegram / 飞书实现 Watcher.OpenMirror，其他窗口开始一轮时同步输入和 RunView。过程、思考、正文分开保存，飞书过程卡用 collapsible_panel（expanded=false），回复卡只放正文；Telegram 独立过程消息用 expandable blockquote，正文按 UTF-16 分段。过程预览有长度限制，网页保留完整记录。/sync 是 /history 的入口，历史查看用 Archive，压缩不再导致旧消息无法查看。
11. **WSL DrvFS**：9p 可能在 dentry 释放后重建 inode，刚加过的 Landlock 规则就失效。add_rule 成功后保留 O_PATH | O_CLOEXEC 引用直到本次执行器退出，不关闭沙箱，不额外放行父目录。test_exec 增加 Windows 盘上的目录、读写、只读和越界回归。

12. **网页管理 SSH**：internal/device.Manager 保存 data/connections.json，写入后才更新内存，列表仅返回 has_password。OpenSSH 处理别名、密钥、跳板机；密码经 SSH_ASKPASS 和 0600 文件传递，不进参数。先上传安装包并检查 / 本机编译，再启动 gateway=stdio 的内核；net.Pipe + Hub.Attach(local=false) 接入同一协议。断开时取消 SSH、关闭 stdin，内核停止工具后退出。自动连接可在启动和掉线后重试，修改 / 删除前先断开。
13. **启动包**：设备页面可下载 Windows ZIP / Linux tar.gz，只带用户填写的网关地址（不带口令）、构建机二进制、完整 C 源码和接入技能。内核没有口令时 hello 带 pair，网关登记请求（网页设备页置顶卡片 + 全局提示，6 位确认码两边对照），允许后回 paired + 专用口令，内核存 paired.token 后断开重连；拒绝回 error(denied) 内核退出；口令失效回 error(auth) 内核删 paired.token 重新申请。同名已配对或在线时拒绝申请。Windows PowerShell 界面选择 WSL2、填写信息，安装到 WSL 的 ~/.local/share/nano-agent/client，Linux 直接 start.sh；不兼容时 ensure-engine.sh 用目标机 cc/make 构建。新增 kernel.device_listen 是可热更新的额外 TCP listener，保留原有本机 Unix socket。

14. **中断和撤回**（app/undo.go）：任务进行中整条消息是「停」「停止」「stop」等就当 /stop；「撤回」任何时候当 /undo。Message.Action 区分界面操作：ActionStop（终端 Ctrl-C 发 ChatStop 控制行、Telegram 按钮回调；没任务时不回话）、ActionRecall（飞书 im.message.recalled_v1，按 MsgID 找，只撤最近一轮，更早的只说明）。撤回 = Store.Undo 截掉最后一条 user 及之后（JSONL 用 os.Truncate，只动末尾，前缀缓存不受影响），不能越过 Compact 的 Base；排队中的直接从队列删；正在跑的设 undoAfter、cancel，finish/KernelDown 里由 afterRun 统一处理（和 resetAfter 一起）。撤回后：EnvNote 如果在被删的消息里就清空（下一条重新说明环境），各窗口 Seen 截到新末尾，发 undo 事件让网页重读历史。网页撤回按钮只在最后一条用户消息上，撤回的文字放回输入框。

## 用户的偏好和约定（重要）

- 中文交流；界面、日志、文档都是中文。用户喜欢“有创意、好看、好用”的网页，不要填表式的设置；拓扑图节点能拖动。
- 先把项目主体功能搭好，每完成一个阶段提交；实现保持直接，做合适的错误处理，不堆防御性代码，体验优化留到实际使用中推进。
- 思考强度只用 OpenAI 风格的 `reasoning_effort`（用户的模型接受 none/low/high/xhigh/max，不接受 medium/minimal/off）。切换思考强度导致缓存失效是 vLLM 的问题，**不要提示**。
- 目前只支持私聊，群聊以后做。
- 提交：分支 main，作者 `tofu-zyc <286995380+tofu-zyc@users.noreply.github.com>`，每完成一个阶段提交一次，Co-Authored-By 记录实际参与者。用户说过“提交，然后开始下一步即可”。
- **密钥**：用户的 API key 只在本地 gateway.toml（gitignore，权限 600）。提交前用 `git grep --cached` 查一遍。不要删用户的 data/。工作目录不能包含 nano-agent 自己。
- 用户的模型：自部署的 OpenAI 兼容接口（deepseek-v4.1-flash，1M 上下文），地址在 gateway.toml。
- 杀进程前确认 /proc/PID/cwd 是演示目录，用户自己可能开着网关（cwd 是项目目录）。

## 接下来要做的（用户要求先把主体功能做齐）

1. 用户继续使用中反馈平台卡片与跨端同步。此轮未向真实飞书 / Telegram 发送测试消息，平台接口通过模拟服务验证。用户现有的飞书机器人是之前扫码建的：流式卡片要 cardkit:card:write、撤回同步要订阅 im.message.recalled_v1，需在开放平台后台补上（新扫码的自动带）。飞书卡片上的“停止”按钮需要卡片回调（card.action.trigger，长连接），还没做，目前飞书靠发「停」。
2. 将已文档化的设备管理 API 接为经过授权的 agent 管理工具；当前接入 skill 提供操作知识，不提供网关凭据。
3. 长期记忆、群聊、每台机器单独 token。Windows 当前是 WSL2 启动器，原生 Win32 内核尚未做。移动端暂不做，将来要求在手机本机执行工具。

## 坑

- Svelte 5：`(this.live[sid] ??= []).push(x)` 推进的是原始数组，界面看不到，要用 `liveOf()`。
- 拓扑图拖节点时不能捕获指针，否则节点点不开。
- 飞书注册接口返回的是 `expires_in`，SDK 里写的是 `expire_in`，两个都认。
- unix socket 路径最长 107 字节：终端聊天的 socket 路径太长时改放 `$XDG_RUNTIME_DIR`（CLISocket）。
- 测试内核都通过 unix socket 连上来，都算“和网关同一台机器”，替补会选最近连上的。
- 内核日志用 `%.*s` + `utf8_cut` 截断，别用 `%.200s`（会切坏中文）。
- `make` 每次都会尝试静态链接执行器，失败只提示一次（build/.static-tried）。
