# 交接文档

给接手这个项目的人（或者下一个 AI 会话）看的：项目现在是什么样、为什么这样设计、做到哪儿了、接下来做什么、有哪些坑。
用户说明见 [README](../README.md)，内核和网关之间的协议见 [protocol.md](protocol.md)。

最后更新：2026-10-22，图片输入（网页 / Telegram / 飞书，模型能力决定发不发）、多模型（网页管理接口和模型、默认模型、每个会话单独选）、多工作目录挂载、技能移到网关、网关启动本机内核、拓扑内核节点和右键菜单、命令提示完成。设计说明见 [ARCHITECTURE.md](ARCHITECTURE.md)。

## 一句话

nano-agent 是作者自己的轻量 bot 智能体：**C 内核**（agent 循环、权限、沙箱里执行工具，没有状态）+ **Go 网关**（上下文、模型、聊天平台、网页界面，唯一有状态的中心）。工作目录 = 机器 + 目录，机器是网关所在的本机、网关自己 SSH 连的服务器（只放执行器）、装了内核的电脑；不管内核跑在哪，都能在任何挂在网关上的工作目录里干活（同机由内核本地执行，异机由网关执行）。

## 目录

```
kernel/                C 内核和执行器
  src/main.c           内核：poll 主循环，连网关、按 workspace 参数派发工具（本机执行 / 交给网关）、代网关执行（run）、放技能文件（skill_put）、列目录
  src/core.c/h         循环状态机（sans-I/O）：预算、权限模式、审批
  src/runner.c/h       执行器子进程的管理（超时、取消、清理）
  src/conf.c/h         kernel.conf 解析（旧的 remote = / ssh = 给出提示，skills = 忽略并提示）
  src/nexec_*.c        执行器 nano-exec：每次工具调用启动一次，套上 Landlock + seccomp 再执行
  src/sandbox.c        Landlock / seccomp
  tests/               test_core.c（状态机）、test_exec.py（执行器）、test_host.py（假网关驱动真内核，含多工作目录、代执行）
gateway/               Go 网关
  cmd/nano-gateway     入口；chat.go 是 `nano-gateway chat` 终端客户端；localkernel.go 启动和看着本机内核
  internal/app         调度：会话、窗口（endpoints）、命令、补聊天记录（recap）
    workspaces.go      工作目录列表（data/workspaces.json）、旧设置迁移、/dir
    nodes.go           内核和机器、route()：这一轮由哪个内核跑、每个挂载的工作目录怎么执行、hostFor()
    exec.go            内核交来的 exec：在工作目录那台机器上执行；Skill（读说明、把附带文件放到挂载的机器上）
    kernelhost.go      把装了内核的电脑当作 host：run / ls / skill_put，等 run_result
    workspace.go       会话挂载的工作目录和环境说明（EnvNote）
    compact.go         超限后异步生成摘要，取消、失败裁剪和用量统计
    undo.go            撤回（/undo、「撤回」、平台撤回事件）和不带斜杠的「停」
    models.go          可用的模型：/model、按会话选模型、按模型建客户端、思考强度和历史上限
    images.go          图片输入：模型能力、把历史里的图片读成 data URL
  internal/host        网关能直接执行工具的机器：本机（local.go）、SSH 服务器（ssh.go：主连接、
                       探测、上传 / 现编执行器、重连）、连接管理（manager.go，data/connections.json）
  internal/channel     聊天渠道：cli（unix socket）、web、telegram、feishu（含扫码创建）
    stream.go          平台流式显示：后台合并、限频、结束补齐、失败时发送完整文本
  internal/kernel      内核连接（hub，多内核；AllowToken 给网关启动的本机内核）
    pair.go            电脑配对：待确认请求、专用口令（只存 SHA-256）、移除
  internal/skills      技能（放在网关上）：扫描、Skill 工具定义、加载说明和附带文件
  internal/session     历史（JSONL 只追加）、会话设置、裁剪
  internal/llm         OpenAI 兼容接口（流式）、mock
  internal/models      模型列表：接口、模型、默认模型（data/models.json，0600）
  internal/media       图片存储（data/uploads，按内容哈希去重，权限 0600）
  internal/config      配置：gateway.toml + data/config.json（网页改的），schema 驱动设置页
  internal/web         网页接口 + 嵌入的前端（dist/）
  web/                 前端源码：Svelte 5 + Vite（make web 构建到 internal/web/dist）
scripts/               install-kernel.sh（内核安装，make installer 打成单文件）、run-local.sh（make run）、e2e.sh
skills/device-connections/  接入和排查的技能（给聊天 agent 看的说明）
docs/                  ARCHITECTURE.md、protocol.md、本文件
```

## 怎么跑、怎么测

```sh
make run          # 编译，后台起内核、前台起网关（Ctrl-C 停）；打印网页登录链接
make chat         # 另一个终端：终端聊天（nano-gateway chat），随时进出
make test         # C 测试（ASan）+ Go 测试 + e2e（mock 模型，真实内核/执行器/网关）
make web          # 改了前端后重新构建（npm run check && build），产物要提交
cd gateway && go test -race ./...
```

- e2e（scripts/e2e.sh）现在 50 项（测试里 `[kernel] local = false`，自己起 pc、phone 两个内核）：终端聊天、工具、审批、模式、沙箱、发「停」和 Ctrl-C 停下任务、撤回（含历史文件截断）、会话切换、本机内核掉线后别的内核顶上（照样能用工具）、同时挂本机和 SSH 服务器上的目录（假 ssh：没写 workspace 报错、写文件、服务器上只有执行器、技能文件两台机器都放好、同一轮用两边的目录）、终端重连，以及超限摘要、保存和继续用工具。
- host 包：假 ssh 测部署 / 执行 / 断线 / 架构不同时现编，有 sshd 时起一个临时的真 OpenSSH 测一遍。kernel 包测配对。
- 安装脚本没有自动测试，改了之后手动测：临时 HOME + PATH 里放一个总是失败的假 systemctl（走后台进程分支，不碰真的 systemd），装 → 配对 → 再装（升级）→ --uninstall。
- 网页的界面检查用 headless Chromium + CDP 脚本截图（在会话的 scratchpad 里，不在仓库里）。演示环境用 mock 模型，别连真模型、别动用户的 data/。
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
| 33bdff6 | 各端都能停（「停」、Ctrl-C、Telegram 按钮、网页输入框旁）；撤回最近一轮（/undo、「撤回」、网页按钮放回输入框、飞书撤回事件）；电脑首次连接配对 |
| 094effa、c0b8c07 | 重写：SSH 服务器由网关自己连、只放执行器（去掉内核侧 remote 和服务器上的内核、启动包）；工作目录 = 机器 + 目录的列表；同机 / 异机两种执行模式（exec / skill_files 协议）；网页工作目录选择器、拓扑按机器画；内核安装脚本和单文件安装包；协议 v4 |
| 多工作目录 | 一个会话挂好几个工作目录（可跨机器），工具参数 workspace；技能移到网关（Skill 由网关处理，附带文件放到各机器的 ~/.cache/nano-agent/skills）；网关代执行别的电脑上的工具（run）；网关启动本机内核；拓扑加内核节点、右键菜单；输入框命令提示；协议 v5 |
| 多模型 | 网页上管理多个 OpenAI 兼容接口和模型：默认模型、会话单独选（/model、会话设置）、每个模型自己的上下文窗口、历史上限、思考强度；接口只回传“有没有设置 key” |
| 图片输入 | 网页粘贴/拖拽/上传、Telegram 照片、飞书图片消息；模型能力（modalities）决定发不发给模型；图片存 data/uploads，历史里只存引用，支持图片的模型收到 OpenAI content parts |

## 关键设计（改之前先理解）

1. **前缀缓存优先**：系统提示词和工具定义固定不变；历史只追加；时间、来源（`[2026-10-21 周三 15:08 · Telegram]`）、环境变化（`[系统] 在 X 上工作。工作目录：…`、替补说明）都追加在用户消息前面，**变了才追加**（Settings.EnvNote 记着上次说的）。历史太长时一次裁掉一大段（滞回），之后又只追加。任何会改动前面字节的改动都是缓存回归。
2. **网关是唯一有状态的中心，内核无状态**。会话 = 一段对话（上下文、工作目录、模式、思考强度、人格、机器）；窗口（endpoint）= 终端、某个私聊，接在会话上（data/endpoints.json）。管理员的窗口默认接主会话；别人各自一个会话。
3. **机器、工作目录和执行**（详见 ARCHITECTURE.md）：机器 = 本机（host.LocalName「本机」）、SSH 服务器（host.Manager）、内核所在的电脑（非本机内核，机器名 = 内核名；本机内核都算本机）。工作目录 `{id, host, path, default}` 存 data/workspaces.json；会话的 Settings.Workspaces / WorkspacesSet（挂载的 ID 列表；没设过就在第一轮落到默认挂载的那些，空列表且 WorkspacesSet 表示只聊天；旧的 Workspace 字段启动时迁移）。每一轮 `route()`：挑内核（第一个挂载的机器上的 → 别的挂载机器上的 → preferred），每个工作目录：在内核的机器上 local=true，否则 hostFor() 能用就挂上（网关执行），用不了的进 down。input.workspaces 由 route.spec() 生成（同一台机器上挂的目录一起放进 dirs，ro 带技能缓存目录）。内核按工具参数 workspace 找工作目录（全名 / 序号 / 只写机器 / 只写路径），预检的错误通过 na_call.error 直接作为结果，沙箱按调用（na_call.sandbox）。
4. **沙箱**：执行器每次调用启动一次，Landlock（工作目录读写、系统目录只读、HOME 其他地方不可见）+ seccomp（禁 AF_UNIX、io_uring 等）+ 清理环境变量。工作目录不能包含 nano-agent 自己（内核、网关、执行器各自把关）。没有沙箱的机器，自动模式下 Shell 也要审批（按每一轮实际执行的机器算）。
5. **SSH 服务器**（internal/host/ssh.go）：网关起一个 `ssh -M -N` 主连接（ControlPersist=no，控制路径在 $XDG_RUNTIME_DIR/nano-ssh 或 /tmp/nano-agent-<uid>/nano-ssh），之后的命令都 `-o ControlMaster=no -o BatchMode=yes` 复用它。连上后探测（uname、有没有执行器）→ 架构相同就上传（有 nano-exec-static 优先）→ 否则传 nano-exec-src.tar.gz 用 cc 现编。执行器在服务器 `~/.cache/nano-agent/nano-exec-<sha256 前 8 位>`。每 5 分钟复查，掉线 5s→60s 退避重连；ssh 退出码 255 视为连接断了。工具调用 stdin 保持打开，关 stdin 就是取消（3 秒后杀）。测试用 NANO_SSH_PROGRAM 换成假 ssh。
6. **安全**：网页只听本机、密码登录、HttpOnly SameSite=Strict、写操作要 X-Nano 头、本机时查 Host；终端聊天走数据目录里只有自己能连的 unix socket（0600 + SO_PEERCRED）。内核口令两种：共用 token（本机、手写 kernel.conf 的）；配对得到的专用口令（绑定节点名，冒充不了别的节点，网关只存 SHA-256，网页移除即作废）。Hub.Attach 不查口令，只给测试用。TCP 本身不加密，跨网络使用私有网络或隧道。SSH 密码只在 data/connections.json（0600）和 askpass 临时文件里。
7. **技能**：放在网关上（internal/skills，`[skills] dir` 默认 gateway.toml 旁的 skills/），每一轮开始时 Scan()，名字或简介变了才发 skills 事件；Skill 工具定义由网关加进模型请求（toolsFor，和内核的工具一起按名字排序，内核报的 Skill 忽略）。调用 Skill 时内核用 exec（没有 workspace）交给网关，网关读说明、把附带文件 PutFiles 到这一轮挂载的每台机器的 ~/.cache/nano-agent/skills/<名字>/（本机、SSH 自己放，别的电脑用 skill_put，内核只肯写这个目录），说明里给的就是这个路径，每台机器一样。
8. **聊天平台流式显示**：RunView 只追加到内存，streamRun 的后台协程合并更新，Close 不阻塞调度锁，但会补齐尾部并关闭卡片。Telegram 每秒编辑消息，飞书每半秒用 CardKit Content 更新卡片，每张卡片的 sequence 严格递增。长回复分段，错误只记录一次，结束时发送完整文本。无正文增量的模型最终结果也会交给 RunView。飞书需要 cardkit:card:write，扫码注册已加入；平台接口测试用 httptest 模拟，无真实凭据的渠道仍需使用时验证。
9. **上下文压缩**：历史超限且有完整旧轮次可切时，额外用当前模型生成摘要。已有 Summary 和被切掉的历史作为 JSON 数据传入，不传工具、人格和推理记录；输出 2048 token，支持 none 时关闭思考，60 秒超时。最近消息和当前轮留在原位置；摘要连同 EnvNote 存在 Settings.Summary，只在压缩时替换，JSONL 不改写。生成失败沿用裁剪，已有摘要不丢；取消或迟到结果不提交，保存失败不推进 Base。额外调用和 token 都计入本轮及全局统计。

10. **平台同步与卡片**：Telegram / 飞书实现 Watcher.OpenMirror，其他窗口开始一轮时同步输入和 RunView。streamRun 把过程存成结构化的（思考、工具调用列表 toolEntry、备注），版本号变了才发请求。Telegram：回复正文用 tgHTML 把 Markdown 转成 Telegram 的 HTML 子集（tgmarkdown.go，没闭合的标记保持成对，代码块跨段时 tgMarkdownChunks 补开头），HTML 发不出去（can't parse entities）就退回纯文本；过程是单独一条消息（状态行 + expandable blockquote）。飞书：一张 CardKit 卡片（src 来源、proc 折叠面板里 think / tools、reply、foot 状态、stop 按钮），长回复续卡只放正文，结束时删 stop，没有过程时删 proc；卡片约 30 KB 上限，正文每卡 3500 字、工具只留最近 12 次。审批：渠道实现 channel.Approver（SendApproval / ResolveApproval），app/approvals.go 发审批卡片、记 ref，在哪儿批了（按钮、命令、网页）都 resolveApproval 改成结果；按钮回来是 Message.Action = ActionApprove / ActionDeny（Telegram callback_data ap:编号:1/0，飞书卡片回调 card.action.trigger，value {a, n}），走 decide 的管理员检查。/sync 是 /history 的入口，历史查看用 Archive。
11. **WSL DrvFS**：9p 可能在 dentry 释放后重建 inode，刚加过的 Landlock 规则就失效。add_rule 成功后保留 O_PATH | O_CLOEXEC 引用直到本次执行器退出，不关闭沙箱，不额外放行父目录。test_exec 增加 Windows 盘上的目录、读写、只读和越界回归。

12. **网页管理 SSH**：host.Manager 保存 data/connections.json，写入后才更新内存，列表只返回 has_password。OpenSSH 处理别名、密钥、跳板机；密码经 SSH_ASKPASS 和 0600 文件传递，不进参数。自动连接在启动和掉线后重试，修改 / 删除前先断开。状态变化 OnChange → app.HostsChanged 推 machines / workspaces 事件。
13. **本机内核**（cmd/nano-gateway/localkernel.go）：`[kernel] local` 默认开，网关找到 nano-exec 旁边的 nano-kernel，生成 data/local-kernel.conf（随机口令，hub.AllowToken 接受它），起子进程，stderr 追加到 data/kernel.log，退出了退避重启（1s→30s，跑满一分钟重置），网关退出时 SIGTERM 等 4 秒。名字默认 gateway（不用主机名：同一台机器上 WSL 里装的电脑内核多半叫主机名，同名会互相顶掉）。make run 只起网关。
14. **内核安装**（scripts/install-kernel.sh，POSIX sh）：源码来自脚本后面的 base64 payload（make installer 生成 dist/nano-kernel-install.sh），没有就用脚本所在的源码目录（拷到临时目录编译，不弄脏源码树）。缺 cc/make 时按包管理器装（sudo 前确认）；装到 ~/.local/share/nano-agent/kernel/，升级只改 gateway / node 两行；systemd 用户服务（StandardError=append:kernel.log），没有就 nano-kernel-ctl 后台跑 + 可选 ~/.profile 一行（带标记，卸载时删）。之后读 kernel.log：确认码、网关已接受连接、网关报错（名字冲突之类，直接告诉用户）。内核的重连退避在 welcome / pairing 之后才重置，被拒时不会每秒重连。
15. **中断和撤回**（app/undo.go）：任务进行中整条消息是「停」「停止」「stop」等就当 /stop；「撤回」任何时候当 /undo。Message.Action 区分界面操作：ActionStop（终端 Ctrl-C 发 ChatStop 控制行、Telegram 按钮回调；没任务时不回话）、ActionRecall（飞书 im.message.recalled_v1，按 MsgID 找，只撤最近一轮，更早的只说明）。撤回 = Store.Undo 截掉最后一条 user 及之后（JSONL 用 os.Truncate，只动末尾，前缀缓存不受影响），不能越过 Compact 的 Base；排队中的直接从队列删；正在跑的设 undoAfter、cancel，finish/KernelDown 里由 afterRun 统一处理（和 resetAfter 一起）。撤回后：EnvNote 如果在被删的消息里就清空（下一条重新说明环境），各窗口 Seen 截到新末尾，发 undo 事件让网页重读历史。网页撤回按钮只在最后一条用户消息上，撤回的文字放回输入框。

16. **多模型**（internal/models、app/models.go）：配置文件里的 `[llm]` 是内置端点 `config`，网页里加的接口和模型存 `data/models.json`（0600）。会话的 `Settings.Model` 是模型 ID（端点/模型名），空表示默认模型。`clientFor` 找到模型后，config 端点直接用 `a.llm`（配置热更新由 SetLLM 换），别的端点按需建客户端并按配置指纹（reflect.DeepEqual）缓存；`levelsOf`/`historyLimit` 跟着模型走（历史上限不填时用上下文窗口的一半）。`ModelsChanged` 在模型列表变化后把指向已删模型的会话改回默认，并刷新所有会话（默认模型可能变了）。网页接口只返回 `has_key`，不回传 API key。

17. **图片输入**（internal/media、app/images.go）：入口是网页上传（`POST /api/uploads`，multipart）、Telegram（getFile 下载照片或图片文件）、飞书（image 消息和 post 里的 img，走 im/v1/messages/{id}/resources）。图片按 sha256 存 `data/uploads/<id>`（0600，去重），`channel.Message.Images` / `session.Msg.Images` 只存引用。请求模型时 `session.BuildMessages` 看这一轮模型的 `Modalities`：有 image 就把图片读成 `data:` URL，和文字一起做成 OpenAI 的 content 数组（`llm.Message.Parts`，`MarshalJSON` 只在有 Parts 时发数组，纯文字还是字符串）；没有 image 就保持字符串内容，后面加一句 `[图片×N]`，并给用户发一条提醒。图片随历史保留（压缩摘要时只留文字说明），不逐轮改写旧消息，前缀缓存不受影响。能力来源：配置文件 `[llm] modalities` 或每个模型自己的 `modalities`。设置页的输入类型是配置 schema 的 `multi` 类型（固定选项 + `Fixed` 锁定 text），不要改回手填的 `list`；`store.addModel()` 让设置页和 ⌘K 能直接打开模型面板的“添加模型”流程。

## 用户的偏好和约定（重要）

- 中文交流；界面、日志、文档都是中文。用户喜欢“有创意、好看、好用”的网页，不要填表式的设置；拓扑图节点能拖动。
- 先把项目主体功能搭好，每完成一个阶段提交；实现保持直接，做合适的错误处理，不堆防御性代码，体验优化留到实际使用中推进。
- 思考强度只用 OpenAI 风格的 `reasoning_effort`（用户的模型接受 none/low/high/xhigh/max，不接受 medium/minimal/off）。切换思考强度导致缓存失效是 vLLM 的问题，**不要提示**。
- 目前只支持私聊，群聊以后做。
- 提交：分支 main，作者 `tofu-zyc <286995380+tofu-zyc@users.noreply.github.com>`，每完成一个阶段提交一次，Co-Authored-By 记录实际参与者。用户说过“提交，然后开始下一步即可”。
- **密钥**：用户的 API key 只在本地 gateway.toml（gitignore，权限 600）。提交前用 `git grep --cached` 查一遍。不要删用户的 data/。工作目录不能包含 nano-agent 自己。
- 用户的模型：自部署的 OpenAI 兼容接口（deepseek-v4.1-flash，1M 上下文），地址在 gateway.toml。
- 杀进程前确认 /proc/PID/cwd 是演示目录，用户自己可能开着网关（cwd 是项目目录）。

## 正在设计

- 模型配置交互重做：撤掉独立模型页；模型只有 `models.json` 一个来源，没有“配置文件模型”、没有等级；没有模型就是空状态，由用户自己添加接口和模型；模型和接口全部回到拓扑的模型节点面板。设计稿见 [DESIGN-model-node.md](DESIGN-model-node.md)，评审确认后实施。

## 接下来要做的（用户要求先把主体功能做齐）

1. 卡片重新设计、审批按钮都只用模拟服务测过，没往真实的飞书 / Telegram 发过，等用户实际用了再调样式。用户现有的飞书机器人是之前扫码建的：流式卡片要 cardkit:card:write、撤回同步要订阅 im.message.recalled_v1、按钮要订阅回调 card.action.trigger，需在开放平台后台补上。
2. 聊天里批准配对（/pair），现在只能在网页上点。让 AI 改网关配置（自举，要先定设计）是用户提的下一批。飞书卡片按钮要用户在开放平台订阅 card.action.trigger（扫码注册的 addons 里没有回调，没敢猜格式）。
3. 图片的清理策略（不用了的图片一直占着 uploads）；音视频输入用户说以后再说（支持音视频的模型还少，也有别的办法）。
3. 同名冲突：远程内核和 SSH 服务器同名时 route 优先按 SSH 服务器算，没有拦；内核起名叫「本机」也没拦（安装脚本拦了）。
4. 长期记忆、群聊。Windows 原生内核没做（在 WSL2 里装）。移动端暂不做，将来要求在手机本机执行工具。

## 坑

- Svelte 5：`(this.live[sid] ??= []).push(x)` 推进的是原始数组，界面看不到，要用 `liveOf()`。
- 拓扑图拖节点时不能捕获指针，否则节点点不开。
- 飞书注册接口返回的是 `expires_in`，SDK 里写的是 `expire_in`，两个都认。
- unix socket 路径最长 107 字节：终端聊天的 socket 路径太长时改放 `$XDG_RUNTIME_DIR`（CLISocket）。
- 测试内核都通过 unix socket 连上来，都算本机；要模拟别的电脑上的内核用 Hub.Attach(local=false)（nodes_test 的 dialRemote）。从 127.0.0.1 用 TCP 连上来的也算本机。
- SSH 服务器上 `~` 不能当工作目录（执行器在 ~/.cache 里，执行器自己拒绝），要用子目录。
- 起演示网关时别用 `cd X && cmd &` 记 `$!`：那是子 shell 的 pid，杀了它网关还在跑。
- 协议 v4（异机模式）。旧启动包装的 v3 内核连上来会被拒（错误里说要重装），否则它会无视 exec=gateway、拿网关机器上的路径在自己机器上执行。以前 SSH 服务器上部署的内核文件（~/.local/share/nano-agent/managed/）不再使用，可以删。
- 内核日志用 `%.*s` + `utf8_cut` 截断，别用 `%.200s`（会切坏中文）。
- 模型列表在网关内存里也有一份（models.Store）：直接改 `data/models.json` 要重启网关才生效，平时用网页或 `/model`。
- 模型 ID 是「端点/模型名」，带斜杠：网页里删除模型用查询参数 `/api/models/items?id=…`，别改成路径参数（`{id}` 匹配不了斜杠）。
- 网页改名/admins 等配置变化会触发 `ModelsChanged`（配置文件里的端点/模型可能变了）；它会给所有会话发 session 事件，别以为是重复推送。
- 图片会一直留在历史里（模型每轮都会重看一遍，token 会涨）；`data/uploads` 按内容哈希存、不会自动清理，暂时没有清理策略，删会话也不会删图片。
- 非多模态模型收到图片时只是发 `[图片×N]` + 提醒，这是“先用着”的行为，正式的降级/换模型策略以后单独设计。
- 图片只在网关这一侧处理，内核和执行器完全不知道图片的存在（工具结果仍然是文字）。
- `make` 每次都会尝试静态链接执行器，失败只提示一次（build/.static-tried）。
