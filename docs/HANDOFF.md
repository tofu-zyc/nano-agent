# 交接文档

给接手这个项目的人（或者下一个 AI 会话）看的：项目现在是什么样、为什么这样设计、做到哪儿了、接下来做什么、有哪些坑。
用户说明见 [README](../README.md)，内核和网关之间的协议见 [protocol.md](protocol.md)。

最后更新：2026-10-22，跨平台阶段 1/4 完成（平台层 + Windows 执行器 nano-exec.exe，WSL 里用 zig 交叉编译、通过 interop 在真实 Windows 上测）；之前阶段 4/4 完成（Gateway / GatewayAdmin 让 AI 在聊天里配置模型、SSH、工作目录和配对）；阶段 3 完成（工作目录读写 + 外面只读 + elevated 提权）；阶段 2 完成（密钥加密、可选脱敏）；阶段 1 完成：所有 nano-agent 文件统一到 `~/.nano-agent`（`NANO_HOME` 可改），网关装成后台服务，内核/SSH 执行器/技能缓存迁移到同一目录，协议升到 v7（网关管理工具）。之前完成：模型管理重做、图片输入、多工作目录、技能移到网关、本机内核、拓扑和命令提示；权限模型是“管理员读全盘；工作目录读写，外面只读；写外面要 elevated 审批；没有工作目录时在 HOME 下工作”。界面细节见 [DESIGN-ui-interactions.md](DESIGN-ui-interactions.md)，设计说明见 [ARCHITECTURE.md](ARCHITECTURE.md)，模型交互见 [DESIGN-model-node.md](DESIGN-model-node.md)，下一步见 [PLAN-next.md](PLAN-next.md) 和 [PLAN-cross-platform.md](PLAN-cross-platform.md)。

## 一句话

nano-agent 是作者自己的轻量 bot 智能体：**C 内核**（agent 循环、权限、沙箱里执行工具，没有状态）+ **Go 网关**（上下文、模型、聊天平台、网页界面，唯一有状态的中心）。工作目录 = 机器 + 目录，机器是网关所在的本机、网关自己 SSH 连的服务器（只放执行器）、装了内核的电脑；不管内核跑在哪，都能在任何挂在网关上的工作目录里干活（同机由内核本地执行，异机由网关执行）。

所有自己产生的文件都在 **`~/.nano-agent`**（`NANO_HOME` 可改）：`bin/` 程序、`lib/` 内核和执行器、`gateway.toml`、`personas/`、`skills/`、`data/`、`run/` 以及 `secret.key`。删掉这个目录就清理干净；只有 systemd 服务文件和 shell 的 PATH 行在目录外，`--uninstall` 会一起删。


## 目录

```
kernel/                C 内核和执行器
  src/main.c           内核：poll 主循环，连网关、按 workspace 参数派发工具（本机执行 / 交给网关）、代网关执行（run）、放技能文件（skill_put）、列目录
  src/core.c/h         循环状态机（sans-I/O）：预算、权限模式、审批
  src/runner.c/h       执行器子进程的管理（超时、取消、清理）
  src/conf.c/h         kernel.conf 解析（旧的 remote = / ssh = 给出提示，skills = 忽略并提示）
  src/plat.h           平台层的接口（路径、文件、自己、子进程、参数）
  src/plat_posix.c     平台层的 POSIX 实现（Linux 和 macOS 共用）
  src/plat_win.c       平台层的 Windows 实现（宽字符 API、UTF-8 边界、创建进程、Job Object）
  src/nexec_*.c        执行器 nano-exec：每次工具调用启动一次，套上 Landlock + seccomp 再执行
  src/sandbox.c        Landlock / seccomp（Linux）
  src/sandbox_win.c    Windows 上沙箱为空（landlock=0，走无沙箱规则）
  tests/               test_core.c（状态机）、test_exec.py（执行器）、test_exec_win.py（Windows 执行器，WSL interop）、test_host.py（假网关驱动真内核，含多工作目录、代执行）
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
    gateway_tools.go   Gateway / GatewayAdmin：查询和修改网关，管理动作走审批
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
  internal/models      模型列表：接口、模型、默认模型（data/models.json，0600，API key 加密）
  internal/media       图片存储（data/uploads，按内容哈希去重，权限 0600）
  internal/config      配置：gateway.toml + data/config.json（网页改的，密钥字段加密），schema 驱动设置页
  internal/web         网页接口 + 嵌入的前端（dist/）
  web/                 前端源码：Svelte 5 + Vite（make web 构建到 internal/web/dist）
scripts/               install-gateway.sh（安装到用户目录）、install-kernel.sh（内核安装，make installer 打成单文件）、run-local.sh（make run）、e2e.sh、fetch-zig.sh、fetch-rg.sh
skills/device-connections/  接入和排查的技能（给聊天 agent 看的说明）
docs/                  ARCHITECTURE.md、protocol.md、本文件、DESIGN-agent-gateway-admin.md
```

## 怎么跑、怎么测

```sh
make install      # 日常使用：全部装到 ~/.nano-agent（NANO_HOME 可改），注册后台服务
make run          # 开发：编译，后台起内核、前台起网关（Ctrl-C 停）；打印网页地址，带密码的链接在 data/web.url
make chat         # 另一个终端：终端聊天（nano-gateway chat），随时进出
make test         # C 测试（ASan）+ Go 测试 + e2e（mock 模型，真实内核/执行器/网关）；装了 zig 和 build/tools/rg.exe 时还跑 Windows 执行器测试
make web          # 改了前端后重新构建（npm run check && build），产物要提交
cd gateway && go test -race ./...
scripts/fetch-zig.sh   # 下载固定版本的 zig 到 build/tools/zig（只用它的 cc，不需要 sudo）
scripts/fetch-rg.sh    # 下载固定版本的 rg.exe 到 build/tools/（Windows 测试和发布包用）
make -C kernel windows-exec   # 只交叉编译 Windows 执行器（build/nano-exec.exe）
python3 kernel/tests/test_exec_win.py kernel/build/nano-exec.exe build/tools/rg.exe   # 单独跑 Windows 执行器测试
```

- e2e（scripts/e2e.sh）现在 67 项（测试里 `[kernel] local = false`，自己起 pc、phone 两个内核）：终端聊天、工具、审批、模式、沙箱、发「停」和 Ctrl-C 停下任务、撤回（含历史文件截断）、会话切换、本机内核掉线后别的内核顶上（照样能用工具）、同时挂本机和 SSH 服务器上的目录（假 ssh：没写 workspace 报错、写文件、服务器上只有执行器、技能文件两台机器都放好、同一轮用两边的目录）、终端重连，Gateway 查询 / GatewayAdmin 审批添加工作目录，以及超限摘要、保存和继续用工具。
- Windows 执行器（test_exec_win.py）现在 13 项，在 WSL 里通过 interop 跑 `nano-exec.exe`，测试目录和 USERPROFILE 都放在 Windows 临时目录：probe、中文路径和内容、Read/Write/Edit、CRLF 保留、工作目录外拒绝、ro=/ 读所有盘、PowerShell 中文输出和退出码、密钥不泄漏、超时连同子进程杀掉、取消、自带 rg、根目录盘符和 `--ls`、错误请求。没有 zig 时 `make test` 会跳过这一组并提示。
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
| 多工作目录 | 一个会话挂好几个工作目录（可跨机器），工具参数 workspace；技能移到网关（Skill 由网关处理，附带文件放到各机器的 ~/.nano-agent/skills）；网关代执行别的电脑上的工具（run）；网关启动本机内核；拓扑加内核节点、右键菜单；输入框命令提示；协议 v5 |
| 多模型 | 网页上管理多个 OpenAI 兼容接口和模型：默认模型、会话单独选（/model、会话设置）、每个模型自己的上下文窗口、历史上限、思考强度；接口只回传“有没有设置 key” |
| 图片输入 | 网页粘贴/拖拽/上传、Telegram 照片、飞书图片消息；模型能力（modalities）决定发不发给模型；图片存 data/uploads，历史里只存引用，支持图片的模型收到 OpenAI content parts |
| 3048b68 | 跨平台阶段 1：平台层（POSIX 一份、Windows 一份）+ Windows 执行器 `nano-exec.exe`；宽字符路径、符号链接/junction 解析、CRLF Edit、PowerShell UTF-8、Job Object 超时/取消、自带 rg；zig 交叉编译，WSL interop 实测 |

## 关键设计（改之前先理解）

1. **前缀缓存优先**：系统提示词和工具定义固定不变；历史只追加；时间、来源（`[2026-10-21 周三 15:08 · Telegram]`）、环境变化（`[系统] 在 X 上工作。工作目录：…`、替补说明）都追加在用户消息前面，**变了才追加**（Settings.EnvNote 记着上次说的）。历史太长时一次裁掉一大段（滞回），之后又只追加。任何会改动前面字节的改动都是缓存回归。
2. **网关是唯一有状态的中心，内核无状态**。会话 = 一段对话（上下文、工作目录、模式、思考强度、人格、机器）；窗口（endpoint）= 终端、某个私聊，接在会话上（data/endpoints.json）。管理员的窗口默认接主会话；别人各自一个会话。
3. **机器、工作目录和执行**（详见 ARCHITECTURE.md）：机器 = 本机（host.LocalName「本机」）、SSH 服务器（host.Manager）、内核所在的电脑（非本机内核，机器名 = 内核名；本机内核都算本机）。工作目录 `{id, host, path, default}` 存 data/workspaces.json；会话的 Settings.Workspaces / WorkspacesSet（挂载的 ID 列表；没设过就在第一轮落到默认挂载的那些，空列表且 WorkspacesSet 表示没有工作目录（在 HOME 下工作）；旧的 Workspace 字段启动时迁移）。每一轮 `route()`：挑内核（第一个挂载的机器上的 → 别的挂载机器上的 → preferred），每个工作目录：在内核的机器上 local=true，否则 hostFor() 能用就挂上（网关执行），用不了的进 down。input.workspaces 由 route.spec() 生成（同一台机器上挂的目录一起放进 dirs；管理员 ro 为 /，访客 ro 只有技能缓存目录）。没有工作目录的管理员拿到一个 HOME 工作目录（dirs 空、home true、ro /），Shell 在 HOME 下运行，写只到临时目录；带 elevated 的调用在 auto 下也要审批，批准后这一次调用 rw 加 /。内核按工具参数 workspace 找工作目录（全名 / 序号 / 只写机器 / 只写路径），预检的错误通过 na_call.error 直接作为结果，沙箱按调用（na_call.sandbox）。
4. **提权**：工作目录默认可写，工作目录外只读；`Write` / `Edit` / `Shell` 带 `elevated: true` 时在任何模式（包括 auto）都要审批，批准后这一次调用的 rw 放开到 `/`；readonly/none 和访客直接拒绝。没有工作目录的管理员用 HOME 模式（dirs 空、home true），Shell 的 cwd 是 HOME，默认只能写临时目录。审批卡片标题带“提权”。

5. **沙箱**：执行器每次调用启动一次，Landlock（管理员读整个文件系统、只写工作目录和临时/缓存目录；访客仍然只读工作目录）+ seccomp（禁 AF_UNIX、io_uring 等）+ 清理环境变量。没有工作目录的管理员在 HOME 下工作（Read/Grep/Shell），写外面要 elevated；访客没有工具、不能提权。工作目录不能包含网关的运行时、配置和数据目录（用 `make install` 后它们在用户目录）。没有沙箱的机器，自动模式下 Shell 也要审批（按每一轮实际执行的机器算）。
6. **密钥和脱敏**：`secret.Init` 在 main/chat 启动时读 `~/.nano-agent/secret.key`（没有就生成 32 字节）；models/host/config/web 在读写时加解密，旧明文启动时重写。`[security] redact`（off/public/always，默认 public）只对工具结果脱敏：模型请求按接口 BaseURL 是否为内网判断，Telegram/飞书过程卡片总是脱敏，网页和终端保留原文；同一段内容每次结果一致。

7. **SSH 服务器**（internal/host/ssh.go）：网关起一个 `ssh -M -N` 主连接（ControlPersist=no，控制路径在 `$NANO_HOME/run/nano-ssh`，路径太长时退回 $XDG_RUNTIME_DIR 或 /tmp），之后的命令都 `-o ControlMaster=no -o BatchMode=yes` 复用它。连上后探测（uname、有没有执行器）→ 架构相同就上传（有 nano-exec-static 优先）→ 否则传 nano-exec-src.tar.gz 用 cc 现编。执行器在服务器 `~/.nano-agent/nano-exec-<sha256 前 8 位>`；部署成功后删旧的 `~/.cache/nano-agent`。每 5 分钟复查，掉线 5s→60s 退避重连；ssh 退出码 255 视为连接断了。工具调用 stdin 保持打开，关 stdin 就是取消（3 秒后杀）。测试用 NANO_SSH_PROGRAM 换成假 ssh。
8. **安全**：网页只听本机、密码登录、HttpOnly SameSite=Strict、写操作要 X-Nano 头、本机时查 Host；终端聊天走数据目录里只有自己能连的 unix socket（0600 + SO_PEERCRED）。内核口令两种：共用 token（本机、手写 kernel.conf 的）；配对得到的专用口令（绑定节点名，冒充不了别的节点，网关只存 SHA-256，网页移除即作废）。Hub.Attach 不查口令，只给测试用。TCP 本身不加密，跨网络使用私有网络或隧道。SSH 密码在 data/connections.json（0600，加密）和 askpass 临时文件里。
9. **技能**：放在网关上（internal/skills，`[skills] dir` 默认 gateway.toml 旁的 skills/），每一轮开始时 Scan()，名字或简介变了才发 skills 事件；Skill 工具定义由网关加进模型请求（toolsFor，和内核的工具一起按名字排序，内核报的 Skill 忽略）。调用 Skill 时内核用 exec（没有 workspace）交给网关，网关读说明、把附带文件 PutFiles 到这一轮挂载的每台机器的 ~/.nano-agent/skills/<名字>/（本机、SSH 自己放，别的电脑用 skill_put，内核只肯写这个目录），说明里给的就是这个路径，每台机器一样。
10. **聊天平台流式显示**：RunView 只追加到内存，streamRun 的后台协程合并更新，Close 不阻塞调度锁，但会补齐尾部并关闭卡片。Telegram 每秒编辑消息，飞书每半秒用 CardKit Content 更新卡片，每张卡片的 sequence 严格递增。长回复分段，错误只记录一次，结束时发送完整文本。无正文增量的模型最终结果也会交给 RunView。飞书需要 cardkit:card:write，扫码注册已加入；平台接口测试用 httptest 模拟，无真实凭据的渠道仍需使用时验证。
11. **上下文压缩**：历史超限且有完整旧轮次可切时，额外用当前模型生成摘要。已有 Summary 和被切掉的历史作为 JSON 数据传入，不传工具、人格和推理记录；输出 2048 token，支持 none 时关闭思考，60 秒超时。最近消息和当前轮留在原位置；摘要连同 EnvNote 存在 Settings.Summary，只在压缩时替换，JSONL 不改写。生成失败沿用裁剪，已有摘要不丢；取消或迟到结果不提交，保存失败不推进 Base。额外调用和 token 都计入本轮及全局统计。

12. **平台同步与卡片**：Telegram / 飞书实现 Watcher.OpenMirror，其他窗口开始一轮时同步输入和 RunView。streamRun 把过程存成结构化的（思考、工具调用列表 toolEntry、备注），版本号变了才发请求。Telegram：回复正文用 tgHTML 把 Markdown 转成 Telegram 的 HTML 子集（tgmarkdown.go，没闭合的标记保持成对，代码块跨段时 tgMarkdownChunks 补开头），HTML 发不出去（can't parse entities）就退回纯文本；过程是单独一条消息（状态行 + expandable blockquote）。飞书：一张 CardKit 卡片（src 来源、proc 折叠面板里 think / tools、reply、foot 状态、stop 按钮），长回复续卡只放正文，结束时删 stop，没有过程时删 proc；卡片约 30 KB 上限，正文每卡 3500 字、工具只留最近 12 次。审批：渠道实现 channel.Approver（SendApproval / ResolveApproval），app/approvals.go 发审批卡片、记 ref，在哪儿批了（按钮、命令、网页）都 resolveApproval 改成结果；按钮回来是 Message.Action = ActionApprove / ActionDeny（Telegram callback_data ap:编号:1/0，飞书卡片回调 card.action.trigger，value {a, n}），走 decide 的管理员检查。/sync 是 /history 的入口，历史查看用 Archive。
13. **WSL DrvFS**：9p 可能在 dentry 释放后重建 inode，刚加过的 Landlock 规则就失效。add_rule 成功后保留 O_PATH | O_CLOEXEC 引用直到本次执行器退出，不关闭沙箱，不额外放行父目录。test_exec 增加 Windows 盘上的目录、读写、只读和越界回归。

14. **网页管理 SSH**：host.Manager 保存 data/connections.json，密码加密，写入后才更新内存，列表只返回 has_password。OpenSSH 处理别名、密钥、跳板机；密码经 SSH_ASKPASS 和 0600 文件传递，不进参数。自动连接在启动和掉线后重试，修改 / 删除前先断开。状态变化 OnChange → app.HostsChanged 推 machines / workspaces 事件。
15. **本机内核**（cmd/nano-gateway/localkernel.go）：`[kernel] local` 默认开，网关找到 nano-exec 旁边的 nano-kernel，生成 data/local-kernel.conf（随机口令，hub.AllowToken 接受它），起子进程，stderr 追加到 data/kernel.log，退出了退避重启（1s→30s，跑满一分钟重置），网关退出时 SIGTERM 等 4 秒。名字默认 gateway（不用主机名：同一台机器上 WSL 里装的电脑内核多半叫主机名，同名会互相顶掉）。make run 只起网关。
16. **内核安装**（scripts/install-kernel.sh，POSIX sh）：源码来自脚本后面的 base64 payload（make installer 生成 dist/nano-kernel-install.sh），没有就用脚本所在的源码目录（拷到临时目录编译，不弄脏源码树）。缺 cc/make 时按包管理器装（sudo 前确认）；装到 ~/.nano-agent/kernel/，升级只改 gateway / node 两行；从旧 `~/.local/share/nano-agent/kernel` 升级时把 kernel.conf 和 paired.token 搬过来，不用重新配对；systemd 用户服务（StandardError=append:kernel.log，UMask=0077），没有就 nano-kernel-ctl 后台跑 + 可选 ~/.profile 一行（带标记，卸载时删；从旧目录迁移时把旧的启动行路径换成新路径）。之后读 kernel.log：确认码、网关已接受连接、网关报错（名字冲突之类，直接告诉用户）。内核的重连退避在 welcome / pairing 之后才重置，被拒时不会每秒重连。
17. **中断和撤回**（app/undo.go）：任务进行中整条消息是「停」「停止」「stop」等就当 /stop；「撤回」任何时候当 /undo。Message.Action 区分界面操作：ActionStop（终端 Ctrl-C 发 ChatStop 控制行、Telegram 按钮回调；没任务时不回话）、ActionRecall（飞书 im.message.recalled_v1，按 MsgID 找，只撤最近一轮，更早的只说明）。撤回 = Store.Undo 截掉最后一条 user 及之后（JSONL 用 os.Truncate，只动末尾，前缀缓存不受影响），不能越过 Compact 的 Base；排队中的直接从队列删；正在跑的设 undoAfter、cancel，finish/KernelDown 里由 afterRun 统一处理（和 resetAfter 一起）。撤回后：EnvNote 如果在被删的消息里就清空（下一条重新说明环境），各窗口 Seen 截到新末尾，发 undo 事件让网页重读历史。网页撤回按钮只在最后一条用户消息上，撤回的文字放回输入框。

18. **模型管理**（internal/models、app/models.go）：接口和模型只存在 `data/models.json`（0600，API key 加密；网页只返回 `has_key`）。旧版 `[llm]` 的单模型在第一次启动时由 `models.Open` 一次性导入成普通接口 + 模型（接口 ID 仍是 `config`，只为兼容已有会话的 `config/模型名` 引用，没有特殊行为），并写 `seeded: true`；删掉后重启不会复活。没有模型时网关正常启动，聊天直接提示去模型节点添加，不启动 run，也不隐式兜底。会话的 `Settings.Model` 是模型 ID，空表示默认模型；默认模型只来自 models.json。`clientFor` 按模型建客户端并按 `LLMConfig` 指纹缓存；`levelsOf`/`historyLimit` 跟着模型走（历史上限不填按上下文窗口一半，窗口没填兜底 128000）。`[llm]` 只保留 timeout / temperature / extra / send_reasoning / trim_to 这些全局默认，模型相关字段不再从网页设置里改。

19. **图片输入**（internal/media、app/images.go）：入口是网页上传（`POST /api/uploads`，multipart）、Telegram（getFile 下载照片或图片文件）、飞书（image 消息和 post 里的 img，走 im/v1/messages/{id}/resources）。图片按 sha256 存 `data/uploads/<id>`（0600，去重），`channel.Message.Images` / `session.Msg.Images` 只存引用。请求模型时 `session.BuildMessages` 看这一轮模型的 `Modalities`：有 image 就把图片读成 `data:` URL，和文字一起做成 OpenAI 的 content 数组（`llm.Message.Parts`，`MarshalJSON` 只在有 Parts 时发数组，纯文字还是字符串）；没有 image 就保持字符串内容，后面加一句 `[图片×N]`，并给用户发一条提醒。图片随历史保留（压缩摘要时只留文字说明），不逐轮改写旧消息，前缀缓存不受影响。能力来源只有模型自己的 `modalities`；在模型节点面板里勾“图片”。`store.openModel(intent)` 让 ⌘K、右键菜单能直接打开模型面板并进入添加模型/接口。

20. **网关管理工具**（app/gateway_tools.go）：`Gateway` 查询状态/模型/SSH/工作目录/配对/ssh 别名，只给管理员、不审批、不返回密钥；`GatewayAdmin` 复用 models.Store、host.Manager、AddWorkspace、UpdateSession 和配对 API，任何模式都审批（NA_TOOL_ADMIN），结果和审计日志不回显密钥。改完立即生效，不重启；监听地址等重启项不开放。协议 v7。

## 用户的偏好和约定（重要）

- 中文交流；界面、日志、文档都是中文。用户喜欢“有创意、好看、好用”的网页，不要填表式的设置；拓扑图节点能拖动。
- 先把项目主体功能搭好，每完成一个阶段提交；实现保持直接，做合适的错误处理，不堆防御性代码，体验优化留到实际使用中推进。
- 思考强度只用 OpenAI 风格的 `reasoning_effort`（用户的模型接受 none/low/high/xhigh/max，不接受 medium/minimal/off）。切换思考强度导致缓存失效是 vLLM 的问题，**不要提示**。
- 目前只支持私聊，群聊以后做。
- 提交：分支 main，作者 `tofu-zyc <286995380+tofu-zyc@users.noreply.github.com>`，每完成一个阶段提交一次，Co-Authored-By 记录实际参与者。用户说过“提交，然后开始下一步即可”。
- **密钥**：API key、SSH 密码、机器人 token、网页密码在 models.json / connections.json / config.json / web.secret 里是 `enc:v1:`（AES-256-GCM），密钥文件是 `~/.nano-agent/secret.key`（0600）；gateway.toml 里旧的明文会在启动时搬进 data/config.json 加密并清空那一行。不要删用户的 data/ 和 secret.key。工作目录不能包含 ~/.nano-agent；用 `make install` 后它就在 ~/.nano-agent。
- 用户的模型：自部署的 OpenAI 兼容接口（deepseek-v4.1-flash，1M 上下文），迁移后地址/模型保存在 data/models.json。
- 杀进程前确认 /proc/PID/cwd 是演示目录，用户自己可能开着网关（cwd 是项目目录）。


## 接下来要做的（用户要求先把主体功能做齐）

1. 卡片重新设计、审批按钮都只用模拟服务测过，没往真实的飞书 / Telegram 发过，等用户实际用了再调样式。用户现有的飞书机器人是之前扫码建的：流式卡片要 cardkit:card:write、撤回同步要订阅 im.message.recalled_v1、按钮要订阅回调 card.action.trigger，需在开放平台后台补上。
2. 飞书卡片按钮要用户在开放平台订阅 card.action.trigger（扫码注册的 addons 里没有回调，没敢猜格式）。聊天里配置网关已经做了，见 [DESIGN-agent-gateway-admin.md](DESIGN-agent-gateway-admin.md)。
3. 图片的清理策略（不用了的图片一直占着 uploads）；音视频输入用户说以后再说（支持音视频的模型还少，也有别的办法）。
4. 同名冲突：远程内核和 SSH 服务器同名时 route 优先按 SSH 服务器算，没有拦；内核起名叫「本机」也没拦（安装脚本拦了）。
5. 长期记忆、群聊。跨平台按 [PLAN-cross-platform.md](PLAN-cross-platform.md) 做：阶段 1 Windows 执行器已完成；阶段 2 Windows 内核 + 网关路径/回环修复；阶段 3 本地离线安装包；阶段 4 macOS。移动端暂不做，将来要求在手机本机执行工具。

## 坑

- Svelte 5：`(this.live[sid] ??= []).push(x)` 推进的是原始数组，界面看不到，要用 `liveOf()`。
- 拓扑图拖节点时不能捕获指针，否则节点点不开。
- 飞书注册接口返回的是 `expires_in`，SDK 里写的是 `expire_in`，两个都认。
- unix socket 路径最长 107 字节：终端聊天和 SSH 控制 socket 平时在 `$NANO_HOME/run`，路径太长时退回 `$XDG_RUNTIME_DIR` 或 `/tmp/nano-agent-<uid>`（paths.Socket / paths.ShortRunDir）。
- 测试内核都通过 unix socket 连上来，都算本机；要模拟别的电脑上的内核用 Hub.Attach(local=false)（nodes_test 的 dialRemote）。从 127.0.0.1 用 TCP 连上来的也算本机。
- `~` 本身不能当工作目录（里面有 `~/.nano-agent`，改坏了网关自己的配置/数据/执行器）；`~` 下面的子目录都可以。SSH 服务器同理。
- 起演示网关时别用 `cd X && cmd &` 记 `$!`：那是子 shell 的 pid，杀了它网关还在跑。
- 协议 v7（新增 Gateway / GatewayAdmin 管理工具）。旧内核会被拒，错误里说要重新运行安装脚本；升级内核安装脚本会把旧 kernel.conf / paired.token 搬到 ~/.nano-agent/kernel，不需要重新配对。
- 内核日志用 `%.*s` + `utf8_cut` 截断，别用 `%.200s`（会切坏中文）。
- 模型列表在网关内存里也有一份（models.Store）：直接改 `data/models.json` 要重启网关才生效，平时用网页或 `/model`。
- 模型 ID 是「接口/模型名」，带斜杠：网页里删除模型用查询参数 `/api/models/items?id=…`，别改成路径参数（`{id}` 匹配不了斜杠）。
- 迁移只在 `seeded` 为假时跑一次；别在每次启动时重新读 `[llm]`，否则用户删掉的模型会复活。
- 没有模型时网关必须能启动：不要恢复“启动时创建默认客户端 / 从 [llm] 合成模型”的老逻辑；聊天在 startRun 里直接提示并清空队列。
- 图片会一直留在历史里（模型每轮都会重看一遍，token 会涨）；`data/uploads` 按内容哈希存、不会自动清理，暂时没有清理策略，删会话也不会删图片。
- 非多模态模型收到图片时只是发 `[图片×N]` + 提醒，这是“先用着”的行为，正式的降级/换模型策略以后单独设计。
- 图片只在网关这一侧处理，内核和执行器完全不知道图片的存在（工具结果仍然是文字）。
- `make` 每次都会尝试静态链接执行器，失败只提示一次（build/.static-tried）。
- Windows 执行器：`argv` 是 ANSI 的，中文路径要用 `plat_args()`（内部走 `GetCommandLineW` + `CommandLineToArgvW`），不要直接用 `argv`。`plat_win.c` 全程用宽字符 API，只在边界上转 UTF-8。
- 在 WSL 里用 interop 测 Windows 程序时，WSL 会把 `USERPROFILE` 换回真实的 Windows 用户目录，直接 `Popen` 设环境变量没用；test_exec_win.py 写一个临时 `run.cmd` 用 `set "K=V"`（一定要带引号，否则 `&&` 前的空格会进到值里）设好再启动 exe。测试目录放在 Windows 临时目录，`wslpath -w` 转路径。
- Windows 上 `powershell.exe -Command` 的输出用 `[Console]::OutputEncoding`/`$OutputEncoding` 强制 UTF-8；超时/取消用 Job Object（`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`），创建进程要先 `CREATE_SUSPENDED`、加入 Job、再 `ResumeThread`（传的是线程句柄，别传进程句柄）。
- 交叉编译 Windows 执行器：`zig cc -target x86_64-windows-gnu … -lws2_32 -lshell32`。`build/tools/` 里的 zig/rg 不提交，先跑 `scripts/fetch-zig.sh`、`scripts/fetch-rg.sh`。
- 在 WSL 的 9p 上覆盖一个马上要运行的 `.exe` 偶尔会拿到旧内容；测试每次复制到新的临时目录，不要复用同一个 exe 路径。
