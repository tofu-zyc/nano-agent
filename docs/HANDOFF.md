# 交接文档

给接手这个项目的人（或者下一个 AI 会话）看的：项目现在是什么样、为什么这样设计、做到哪儿了、接下来做什么、有哪些坑。
用户说明见 [README](../README.md)，内核和网关之间的协议见 [protocol.md](protocol.md)。

## v0.8.0：自主执行、桌面闭环与可复用技能（2026-10-26）

此版本在 v0.7.6 上补自主执行、任务验收/检查点、外部事件、整轮桌面资源协调和群聊界面。所有实测使用临时配置，正式 linux-pc 网关的运行版本未替换；发布状态和安装包以 GitHub v0.8.0 Release 为准。除确定性回归，文件自主续做与 Windows 桌面操作都用已配置的真实模型完成验收。

- 自主日程可使用文件、Shell、Skill、MCP、桌面工具，保留创建时目录和权限上限；访客不能创建执行计划。审批回到创建它的管理员窗口。
- `Agenda` 新增 `criteria`、`progress/checkpoint`、`done/evidence`；完成先暂存，正常结束才提交。只有绑定本轮真实成功回执的验收才算完成；MCP 明确未确认效果不能充当证据。取消、挪动或手动完成优先于迟到的收尾。
- 真实模型测试发现兼容接口不一定向模型显示 `tool_call_id`：它能正确做完文件任务，却一直猜验收 ID。现在只在当前模型请求视图的有效工具结果开头加 `[本轮验收凭据] {"id":...,"tool":...}`；`Agenda list` 也能查本轮可用 ID。失败、未确认、元操作不发凭据，旧轮次/重复 ID 的旧结果不标记；原始工具输出和历史不变，工具快照版本升为 6。
- 定时任务结果、相关机器上下线会进入心智事件并提前检查；认识和反思的调整进入后续决策。
- 桌面按整轮保留，等待者可取消，结束/断线释放，避免两个任务在观察和点击之间插入；跨设备互等会明确拒绝并要求留检查点后重排。
- 内部群聊有成员头像、发言人身份、@ 筛选与键盘补全、实时消息及暂停/恢复；旧的 `[名字]` 历史兼容，新发言另存 `Msg.Agent`。
- 新增 `scripts/e2e-autonomy.py`（已接入 make test）及 `scripts/e2e-desktop.py`；文件自主闭环在开发机和 linux-pc 通过。桌面驱动 0.7.0 / 0.37.0 在 linux-pc 隔离 X11 通过非空截图、`set_value` 完整中英文和应用输出核对；0.37.0 下真网关 + C 内核 + 到点自主日程 + MCP 审批/操作 + 应用输出验收的组合也通过。详见 `PLAN-computer-use.md`。
- 完整隔离回归已通过：C 154 项、Go 全量 race、e2e 78 项、control 12 项、pipe/MCP 透传、安装/更新/回滚、macOS 交叉编译；网页新增群聊验收，0 failures。后续变更按影响范围补跑。
- Windows 实机已通过 Linux 网关 → 独立 Windows C 内核 → Driver 0.37.0 → 自有 Win32 窗口的自主闭环，完整中英文、截图、点击和真实输出验收均通过；截图实际可见，原有内核及 Driver 进程保留。`scripts/e2e-desktop-win.py` 从 WSL 编译自有夹具，经 SSH 临时转发连接 Linux 测试网关；测试配置、程序副本与工作目录隔离，按精确路径/PID 清理。
- `scripts/e2e-autonomy-real.py` 显式选择已有模型做有界验收：linux-pc 上 `deepseek-v4.1-flash` 自行排期、读取未知随机文本、审批写入、保存检查点、网关重启后续做且不重写第一轮文件，再关联真实 Read 凭据完成验收。仅临时复制所需的加密模型端点和密钥，结束删除副本；不接正式聊天/机器人/MCP。该用例会消费 API 用量，不加入默认测试。
- Windows 用例加 `--real-model-home` 后，同一真实模型已通过自行排期、观察含截图的界面、选择最新元素、输入中英文、点击、回读和验收。独立核对应用输出和非空截图；临时凭据已删除。额外用合成凭据验证 SIGTERM 中断也会退出临时网关、删除副本并保留失败报告。
- 新增可发现的 `autonomous-tasks` 技能；重整 `computer-use`，日常任务复用现有服务，安装和 Linux 排障资料按需读取。life / MCP / 定时任务技能相互指路。新增技能、改简介、删技能会刷新旧会话的工具目录，不用 /reset；正文在调用时重新读。
- 技能源文件与执行缓存分开：`skill-cache/<name>-<内容哈希>/`，不同 agent/版本不会覆盖全局技能源文件；Skill 回执返回各机器的实际完整路径，支持自定义 NANO_HOME。内核自动只读开放该缓存，不放进可写的通用 cache。真实模型加载新技能后，两轮文件续做验收通过。
- 原生 Wayland 补测：Driver 0.37.0 + Sway 1.11 / wlroots 0.19，在独立无 XWayland 会话通过发现、截图、完整中英文、点击及真网关自主日程回读验收。Sway 树确认窗口是 xdg_shell、window=null；测试脚本新增 --isolated-wayland，并在中断时收尾私有会话。没有改正式 GNOME 设置。

后续扩展/评估：真实模型跨多日与复杂桌面任务的行为评估；GNOME（当前无显示输出）、其他 Wayland 合成器的单独验收；macOS 无实机，用户已确认先完成 Windows/Linux；Windows 网关自动更新、macOS 网关发布包；附件回收、音视频输入/转码等。旧交接记录中的“长期记忆/群聊没做”和“Windows CUA 未验证”是历史信息，以本节及代码为准。

历史发布记录：2026-10-26，**v0.7.6 已发布**（dev/v0.7.6 合并回 main）：修网页会话历史因重复附件 key 整段空白。之前 **v0.7.5 已发布**（dev/v0.7.5 合并回 main）：修 MCP 工具在多工作目录会话里被内核当成执行器工具（不经过沙箱的 MCP 被 workspace 预检挡住）。之前 **v0.7.4 已发布**（dev/v0.7.4 合并回 main）：修 Agenda/Idea 多工作目录、旧会话工具快照、life 会话留言；补心智状态 UI 和 Mind 工具；computer use 安装流程进技能；新增 Windows 网关包。之前 **v0.7.3 已发布**（dev/v0.7.3 合并回 main）：已有会话切换 agent、拟人型自主活动（Agenda/Idea、UI 和心智状态基础）、computer use（Cua Driver，Linux 真机握手/窗口树；Windows/macOS 未验证）。见下面「v0.7.3 的改动和交接」。之前 **v0.7.2 已发布**（dev/v0.7.2 合并回 main）：MCP 服务挪到拓扑图上单独的节点、AI 能在聊天里加改删测 MCP（技能 mcp），修了 AI 配模型接口时地址和 key 一直存不进去；定时任务回 `[不回复]` 不再往飞书推、编辑任务改成点选时间（含“只运行一次”）。见下面「v0.7.2 的改动和交接」。之前 **v0.7.1 已发布**（dev/v0.7.1 合并回 main）：MCP 服务——网关本机起进程、连远程地址，或者挂到任何一台接入的电脑上跑（协议 v10 管道透传，Linux / macOS / Windows 内核都行），网页里加改删测，审批三档；还有 v0.7.0 之后的几处修复。见下面「v0.7.0 发布后的修复」和「MCP（2026-10-25）」，**改 MCP 之前先读那一节的「锁的规矩」**。之前 **v0.7.0 已发布**（dev/v0.7 合并回 main）：QQ 和微信机器人（扫码绑定）、“放行”权限模式（拟人型默认）、飞书拟人型回复渲染 Markdown 和“正在回复”表情、`/state` 看 agent 在干嘛、调工具前说的话当场发、看图和发文件（SendFile）。**设计、调研结论、做了什么、没做什么都在 [DESIGN-v0.7-channels.md](DESIGN-v0.7-channels.md)，接手先读它。** 之前 **v0.6.0 已发布**（dev/v0.6 合并回 main）：两种 agent（工具型 / 拟人型）、私聊和群聊统一的“活的”聊天循环、拟人型的内心活动（作息里自己醒来冲浪、找人聊天）、长期记忆（知识库、人物画像、自我认知）和睡觉做梦整理记忆。**设计、调研结论、分阶段进度都在 [DESIGN-v0.6-living-agent.md](DESIGN-v0.6-living-agent.md)，接手先读它。** 再之前 **v0.5.1 已发布**（main）：修 v0.5.0 发布后用户报的问题——agent 的人格编辑器里看不见自带人格、找不到添加人格文件的地方、删 agent 死路、同名 agent、配置项和工具名用英文、入口直连 agent 的虚线绕得太复杂（已改回直连）。之前 **v0.5.0 已发布**（dev/v0.5 合并回 main，阶段 6、7、8 都已完成）：多 agent——每个 agent 是图上一个节点，有自己的名字、人格、模型和会话，可以绑自己的机器人；每个 agent 能覆盖全局设置，没覆盖的跟随全局；外部群聊能听会回；网关内部有群（房间）和两个 agent 的私聊；**专门给群聊做的界面（成员头像、@ 补全）还没做**，群聊现在就用普通的会话页（每一条前面带 `[谁说的]`）。验收提的问题都修了（覆盖人格、Message 权限、拓扑分列、判断费用、中文名字的 ID 等），见下面「v0.5 验收修复」。之前 **v0.4.1 已发布**（main）：网关换了版本，开着的网页自己刷新；工具调用轮数上限改名、好找（见「v0.4.1 发布后的修复」）。之前 **v0.4.0 已发布**（dev/v0.4 合并回 main）：拓扑图就是控制台，去掉设置页、设备页、定时任务页、人格页（见下面「v0.4 开发进度」）；同一个版本里还有 Windows 内核配置被写坏的修复、每条消息的上限改成网关设置、检查更新走代理（见「v0.3.2 发布后的修复」，原来准备单独发 v0.3.3，用户要求一起发）。之前 **v0.3.2 已发布**：设置页数字项改了不保存修好；显示档位单独成「消息与显示」组，Telegram、飞书、命令行节点面板里也有（见「v0.3.1 发布后的修复」）。之前 **v0.3.1 已发布**：修飞书回复出现两次、Telegram 工具记录被删（显示档位）；设置页和拓扑图补上「上下文与记忆」「网络搜索」「定时任务」；v0.3 功能的技能（web-search、scheduled-tasks、context-and-history、gateway-settings）和聊天里改设置（`settings_set`）、查对话轮次（`turns`）；默认开的开关在设置页显示对了；见下面「v0.3.0 发布后的修复」。之前 **v0.3.0 已发布**（dev/v0.3 合并回 main）：上下文真实用量计量和 `/context`、分层上下文整理（工具结果占位 + 分块滚动摘要，请求前/结束后/空闲/`/compact`）、`/reset` 归档和删除历史（整段、几轮、归档）、聊天显示档位（`/show`）、DeepSeek 网络搜索（WebSearch/WebFetch）、定时任务（`Schedule`、`/cron`、网页「定时任务」页），见下面「v0.3 开发进度」；之前 **v0.2.2 已发布**：自带人格「小纳」「大肥鱼」（网页「人格」页套用、聊天 `/persona`，见「自带人格」）；Windows 开机自启修好（普通用户建不了“任何用户登录时”的任务：改成只对当前用户，建不了退到「启动」文件夹，`nano-kernel start` 在任务不存在时直接拉起）、`nano-gateway help` / `--help` / `-h`、`nano-kernel help` 和各命令 `--help`、去掉 `nano-kernel-ctl`（见「内核管理命令」）；更新和卸载做完（`nano-gateway update` / `uninstall`、`/update`、网页更新按钮、GatewayAdmin `update`、内核自动跟网关同版本、`nano-kernel-ctl update|uninstall`，见下面「更新和卸载」）；2026-10-22，**v0.1.1 已发布**（v0.1.1 修了安装后 PATH、卸载不停后台网关、长 socket 路径、飞书卡片体积；v0.1.0 Linux/Windows 一行安装用真实 Release 实测过；CI 在 Linux/Windows/macOS 真机全绿，macOS 第一次真正跑通）；发布准备完成（公开仓库 https://github.com/tofu-zyc/nano-agent ，Apache-2.0；GitHub Actions 在 Linux/Windows/macOS 真机上测试、推 `v*` 标签自动发布；一行安装 `install.sh` / `install.ps1`；内核自我更新（协议 v9 的 `update`，失败自动退回）；`nano-gateway export` / `import` 搬家）；网关查看和控制完成（`status/start/stop/restart/logs/url/doctor/version`、配置检查后自重启、网页重启与版本/运行时长、聊天 `/status` `/restart`、`Gateway` 日志和 `GatewayAdmin restart`、后台看护进程、WSL 启动文件夹保活；`scripts/e2e-control.py` 12 项）；跨平台阶段 4/4 完成（macOS 平台层补齐：`_NSGetExecutablePath`、无沙箱 stub、无 PDEATHSIG 的进程组清理；zig 交叉编译 aarch64/x86_64 macOS，`make release` 出 macOS 包；`install-kernel.sh` 加 launchd 用户 agent。**本机没有 Mac，macOS 只做了交叉编译检查，没有在真机上跑过**）；阶段 3/4 完成（本地交叉编译、离线安装包、Linux/Windows 安装脚本和离线安装测试）；阶段 2/4 完成（Windows 内核 nano-kernel.exe、TCP/Winsock、网关协议 v8、路径归属和 127.0.0.1 误判修复，WSL 网关 + Windows 内核实测）；阶段 1/4 完成（平台层 + Windows 执行器 nano-exec.exe，WSL 里用 zig 交叉编译、通过 interop 在真实 Windows 上测）；之前阶段 4/4 完成（Gateway / GatewayAdmin 让 AI 在聊天里配置模型、SSH、工作目录和配对）；阶段 3 完成（工作目录读写 + 外面只读 + elevated 提权）；阶段 2 完成（密钥加密、可选脱敏）；阶段 1 完成：所有 nano-agent 文件统一到 `~/.nano-agent`（`NANO_HOME` 可改），网关装成后台服务，内核/SSH 执行器/技能缓存迁移到同一目录，协议升到 v7（网关管理工具）。之前完成：模型管理重做、图片输入、多工作目录、技能移到网关、本机内核、拓扑和命令提示；权限模型是“管理员读全盘；工作目录读写，外面只读；写外面要 elevated 审批；没有工作目录时在 HOME 下工作”。界面细节见 [DESIGN-ui-interactions.md](DESIGN-ui-interactions.md)，设计说明见 [ARCHITECTURE.md](ARCHITECTURE.md)，模型交互见 [DESIGN-model-node.md](DESIGN-model-node.md)，下一个大版本（v0.3 – v0.6：上下文压缩、显示档位、搜索、定时任务、多 agent 和群聊、知识库和做梦、MCP 和 computer use）见 [PLAN-roadmap.md](PLAN-roadmap.md)；更早的计划见 [PLAN-next.md](PLAN-next.md) 和 [PLAN-cross-platform.md](PLAN-cross-platform.md)。

## v0.7.6 的改动和交接（2026-10-26 发布）

**修网页会话历史整段空白（Svelte `each_key_duplicate`）**：

- 工具结果里同一个附件会同时出现在 `images`（交给模型看的图）和 `sent`（真正发出去的图）里；`ToolCard` 用 `(m.id)` 做 keyed each，重复 id 让 Svelte 抛 `each_key_duplicate`，整个历史列表渲染中断——所以旧消息全空，新消息走实时路径还能显示。控制台能直接看到这个错。
- 修复：`buildBlocks` 对工具媒体按附件 id 去重；`ToolCard`、用户图片、工作目录的 keyed each 统一用 `id:index`，以后即使再有重复附件也不会炸。
- `scripts/check-web-ui.py` 增加一个 `images`/`sent` 同 id 的工具会话回归；UI 检查 0 failures，`svelte-check` 0 errors。
- 纯前端修复，无后端/数据迁移；会话 JSONL 没动过，历史数据完好。

## v0.7.5 的改动和交接（2026-10-26 发布）

**修 MCP 工具在多工作目录会话里报“用 workspace 参数”**：

- 内核的审批预检（`main.c` 的 `calls[i]` 循环）和执行入口（`exec_tool`）都只认写死的 `gateway_tool()` 名单。MCP 工具名是 `<服务>__<工具>`，不在名单里，于是被当成要在本机执行器里跑的工具：会话挂了多个工作目录就先要求 `workspace`，可 MCP 工具的 schema 里根本没有这个参数，模型没法传。
- v0.7.5 增加 `mcp_tool()`：名字里带 `__` 的工具在两处都直接交给网关，不再挑工作目录。MCP 进程由网关注册/内核 `pipe_open` 直接 `exec`，**不经过 `nano-exec` 的 Landlock/seccomp 沙箱**；Shell 里的 sandbox 报错和 MCP 是两回事。
- 网关侧也补了明确错误：`<服务>__<工具>` 但服务没配置时返回“没有这个 MCP 服务：<服务>”，不再掉进“这一轮没有挂载工作目录”分支。
- e2e 增加 `fake__echo` 的多工作目录检查；现在是 78 项全过。

**关于“放行”和沙箱**：权限模式的“放行/自动”只表示**不逐步审批**，不表示关掉执行器的 OS 沙箱。`Shell` 仍然在 Landlock 里跑；沙箱不允许的 Unix socket/路径不会因为放行就打开。MCP/CUA 走的是另一条执行链（网关注册的 MCP 进程，不在 `nano-exec` 里），所以不应该被 Shell 的沙箱拦。要“无沙箱执行 Shell”需要另做一个明确的危险开关和审批，不能和“放行”混为一谈。

**验证**：`./scripts/e2e.sh` 78 passed；内核 `make test`、Go 全量/race 仍按 v0.7.4 的套件跑；在 linux-pc 上真实 `cua-driver 0.6.8` 的 `doctor` 显示 X11/AT-SPI 正常、`list_windows` 能看到窗口；CUA 的 MCP 工具链路在修路由后由网关执行。

## v0.7.4 的改动和交接（2026-10-26 发布）

**已经做完（v0.7.4 已发布）**：

1. **修 Agenda / Idea 一调就报错（v0.7.3 的回归）**：内核的 `gateway_tool()` 白名单和 `kernel/src/tools.c` 只认识旧的网关工具，`Agenda`、`Idea`、`Mind` 被当成要工作目录的执行器工具；会话挂了多个目录时先要求 `workspace`，加了又报“未知工具”。现在内核直接转给网关，e2e 增加三条多工作目录调用检查（77 项全过）。
2. **修旧会话工具快照不刷新**：v0.7.3 加了 Agenda/Idea 但 `prefixVer` 仍是 3，`@.../life` 等旧会话一直用旧工具快照（所以 life 里根本看不到 Agenda/Idea）。v0.7.4 把 `prefixVer` 升到 4，升级后第一次请求重建一次快照；新工具 `Mind` 一并进入。
3. **心智状态可见、可更新**：agent 节点把「拟人」和「自主活动」合并成一张卡，去掉重复的状态/叫醒按钮/token；新增「心智状态（长期兴趣、认知、反思）」展开区。新增 `Mind` 工具（`view` / `interest_add` / `interest_remove` / `belief` / `reflect`），兴趣和认识必须写来源；API `autonomy` 增加 `self`。life 技能和内心说明同步。
4. **life 会话能留言**：管理员在 life 会话里可以直接输入（以前只能看），会叫醒一轮；`Message` 的 `chat:<自己的 life 会话>` 不再报“没有这个私聊”，而是把话放进 life 历史并顺手叫醒。dream 仍然只读。`messageChat` 对 life 走单独的留言路径；UI 顶部/底部说明同步。
5. **computer use 安装写进技能**：`skills/computer-use/SKILL.md` 增加 AI 自助安装/配置流程（Linux/macOS/Windows 的官方安装、依赖、图形会话、daemon/socket、权限边界、nano-agent MCP 配置、诊断）；`skills/mcp/SKILL.md` 加指针。AI 可以自己装、自己配 MCP，但安装第三方软件前必须取得用户同意；macOS 系统权限和 Windows UAC 仍要用户/管理员确认，Linux 图形会话如果 AI 的 Shell 在后台服务里则需要用户在桌面终端启动一次 daemon。
6. **Windows 网关（x86_64）**：新增 `nano-gateway-<版本>-windows-x86_64.zip`，含 `nano-gateway.exe`、`lib/nano-kernel.exe`/`nano-exec.exe`/`rg.exe`、示例配置、内置 agent/skills 和 `install-gateway.ps1`（安装到 `%USERPROFILE%\.nano-agent`，默认 TCP 7700、网页 7788，任务计划/启动文件夹自启，`nano-gateway start|stop|status|url`）。Windows 自动更新暂不支持，升级重跑安装包（配置和数据保留）。补了 Windows 进程/路径/权限位兼容和本机内核 stop 清理。

**本版验证（发版前）**：

- `go build ./...`、`go vet ./...`、`go test ./...`；`go test -race ./internal/app ./internal/autonomy ./internal/mcp ./internal/session ./internal/web ./internal/rooms ./internal/host` 全过。
- `./scripts/e2e.sh` 77 passed（新增 Agenda/Idea/Mind 多工作目录）；`make -C kernel test` 全过。
- `python3 scripts/check-web-ui.py` 0 failures，新增“拟人/自主活动合并、叫醒按钮只有一处、心智状态展开、life 会话可留言”。
- `scripts/test-isolated.sh` 全过：core 154、exec/host/pipe/pipe-win/mcp-remote 0 failures、e2e 77、control 12、install-linux 6、oneliner/update 0 failures、macOS 交叉编译通过。隔离环境没有 playwright，网页检查是单独在正常 HOME 下跑的（0 failures）。
- Windows 网关在本机 Windows 真实运行：`nano-gateway.exe version` = v0.7.4；安装包安装后启动，`status` 显示 web/cli online、本机内核 windows v0.7.4 在线，`Invoke-WebRequest http://127.0.0.1:17789/` 返回 200；`stop` 后本机内核一起退出。任务计划被系统拒绝（没有管理员权限）时自动退回启动文件夹，安装/卸载都验证过。**Windows 自动更新未实现**；Windows 上 CUA Driver 未验证。
- Linux 真机 CUA 验证同 v0.7.3（握手/窗口树）；截图/输入仍受 WSLg 限制。

**还没做/要注意**：

- Windows 网关的自动更新（`nano-gateway update` 会明确提示手动下载安装包）；`restart` 在 Windows 上仍不支持（`reexec` 返回错误）。
- Windows 网关的系统级服务/多用户 session 没有专门支持，目前按当前用户后台进程 + 登录自启。
- v0.7.3 的会话切换、自主活动主体、CUA 主体实现不变；本版只修上面这些。

## v0.7.3 的改动和交接（2026-10-26）

这个版本做三件事：已有会话切换 agent、拟人型 agent 的自主活动控制层、computer use 接入。分三条独立的开发线，互不依赖，但共用“会话归属”和“工具/内心执行链”。

### 1. 已有会话能切换 agent

- 数据：`session.Settings.Agent` / `AgentSegments` / `Handoff`；`session.Msg.Agent` 记每条 assistant/tool 消息的来源。
- 身份：新增 `gateway/internal/app/identity.go`。**执行身份和路由命名空间分开**：`execAgentID`/`agentOf` 决定人格、模型、工具、限制、记忆、会话计数；`routeAgentID` 只决定窗口接线、`mainOf`、`@<agent>/life`、`@<agent>/cron`、房间成员。改这一块之前先读这个文件的注释，不要全局替换 `agents.SessionAgent`。
- PATCH：`SessionPatch.Agent`，整份先验证再写盘；运行/排队/整理中拒绝；切换时清工具快照、环境说明、记忆/近况注入和 recalled，写交接说明，清旧 agent 的 FollowUp/Awaiting/群触发，结束旧 Driver session。
- 删除 agent：显式选过它的会话（含 main）检查忙碌后一次性交给默认 agent；没有显式选择的 `@old/...` 会话保持旧行为。
- UI：`AgentSwitcher`（聊天顶部 + 会话设置）；`SessionInfo` 多了 `route_agent` / `agent_fixed` / `can_switch_agent` / `switch_blocked`。
- 注意：外部平台群聊可以切换单一回复者；多人房间成员和公共记录固定/无单一 agent。`attach` 的比较仍然用路由前缀，但会跳过“用户显式选过 agent”的会话，否则下一条机器人消息会把选择改回去。

### 2. 自主活动（BDI + Generative Agents 的规划/反思）

- 新包 `gateway/internal/autonomy`：心智状态、事件队列（去重）、意图生命周期、议程、想法池、执行记录、反思、每日预算、重启恢复；纯规则选择到期事项。
- 接入 `app/autonomy.go`：聊天事件 10 分钟内合并；约定优先、不受作息限制；探索看作息和每日次数；资源占用单独判断；预算按天；暂停立即打断当前轮并保留下一步；重启不重放未知执行。
- 工具：`Agenda`（list/add/move/cancel/done）和 `Idea`（list/add/schedule/drop/defer）。`Agenda done` 必须写真实结果；只有真的调了工具且这一轮正常结束才把日程标成完成，否则往后挪。旧 `FollowUp` 仍是聊天回访的唯一执行者。
- UI：agent 节点「自主活动」卡；`/autonomy [pause|resume|wake]`；`GET/POST /api/agents/{id}/autonomy`。
- 重要：旧的 20 分钟 `lifeQuiet` 已经不在 `lifeBlocked`/启动判断里。不要把它加回去；如果要做“刚聊过天别打扰”，只能影响主动联系（`Message` 自己的规则），不能阻止约定和日程。
- 数据：`data/autonomy.json`（按 agent 存心智状态）。旧 `data/life.json` 仍保留近况、Awaiting、FollowUps；首次加载会把近况导入成认识，不会重复导入（`Migrated` 位）。

### 3. computer use（Cua Driver）

- 接入走现有 MCP：Driver 在目标机器图形会话里跑，网关通过本机 stdio / HTTP / 远端内核 pipe 连过去；不改 C 内核协议。
- `mcp.ResultText` 现在同时保留文字和 `structuredContent`（以前有文字就提前返回）。
- `gateway/internal/app/mcp_desktop.go`：稳定 Driver session（`na-<hash>`，工具 schema 有 `session` 才注入）、同机桌面调用串行、会话结束清理、initialize instructions 进系统提示、截图预算（模型只看最近两个带图工具结果）。
- 诊断：`MCPInfo` / MCP 节点面板显示驱动版本、目标机器在线、instructions、最近错误；“设备在线不等于桌面可操作”。
- 真机验证：Linux WSL Fedora 43 + cua-driver 0.7.0 的 MCP 握手、`list_windows`、`start_session`/`end_session`、`health_report`、启动 gedit 后 `get_window_state` 读到 76 个 AT-SPI 元素，均通过。**WSLg 的截图（X11 GetImage BadMatch）和输入（X MPX 权限、文本未在树里出现）没有通过；Windows / macOS / 原生 Wayland 未实机验证。** 细节和平台矩阵见 `docs/PLAN-computer-use.md`。
- 真机测试默认跳过：`NANO_CUA_REAL=1 NANO_CUA_BIN=/path/to/cua-driver go test ./internal/mcp -run TestRealCuaDriver -v`。

### 本版验证（发版前）

- `go build ./...`、`go vet ./...`、`go test ./...`；`go test -race ./internal/app ./internal/autonomy ./internal/mcp ./internal/session ./internal/web ./internal/rooms` 全过。
- `cd gateway/web && npm run check && npm run build` 0 错误。
- `python3 scripts/check-web-ui.py` 0 failures，新增：会话顶部/设置切换 agent、刷新持久化、自主活动卡和接口。
- `NANO_CUA_REAL=1 ... go test ./internal/mcp -run TestRealCuaDriver` 在本机 Linux 通过（输出见上）。
- 没有跑全量 `scripts/test-isolated.sh`（需要完整构建和 Windows interop 环境）；发布前如果时间允许应补跑。

### 还没做 / 坑

- Windows/macOS 的桌面驱动实机验证；WSLg 的截图和输入限制不是网关能绕过的。
- 自主活动的长期行为评估：测试只能证明状态机、真实工具结果和恢复逻辑，不证明“像人”。
- 会话切换后真实 Telegram/飞书/QQ/微信机器人的入口行为没有实机回归（假渠道模拟了 attach）。
- 内核协议没有变化；这一版全在网关和前端（C 代码没动）。

## v0.7 开发进度（dev/v0.7，已合并 main，v0.7.0 发布）

见 [DESIGN-v0.7-channels.md](DESIGN-v0.7-channels.md)。已完成：放行模式、飞书拟人型回复和状态、`/state`、QQ、微信、调工具前说的话当场发、看图（Read 读到图片交给模型）和 SendFile 发图片 / 文件（都有测试）。**2026-10-25 验证补完**：`scripts/test-isolated.sh` 全过（e2e 74 项，含 SendFile 那一步）、网页界面检查 0 failures（要单独跑，隔离脚本里会被跳过）、Windows 执行器 14 项（补了 `t_fetch`）、`e2e-win.py` 17 项；顺手修了 Windows 上文件不存在却报“No error”的问题（`plat_win.c` 没设 errno）。d307b8c 的 CI 也是 success。**已合并 main、发 v0.7.0**；剩下音频视频格式转换。**QQ 和微信都没在真的平台上试过**，发版后第一次扫码时留意一下；飞书发图片要老应用在开放平台补开 `im:resource` 再发一版。

## v0.7.1 发布后的修复（dev/v0.7.2）

- **定时任务回了 `[不回复]` 也推到飞书**：用户的 agent 在 v0.7.0 时建了一个每 3 分钟查 B 站私信的任务，那时新任务默认投递到创建它的窗口（飞书私聊）；
  没新私信时模型老实回 `[不回复]`，但 `completeSchedule` 不看内容，每 3 分钟往飞书推一条「[定时任务 B站私信值守] [不回复]」。
  修法（`app/schedules.go`）：回复以 `[不回复]` 开头或结尾就只记在任务页、不推送（正文中间提到这几个字不算，`scheduleSilent`）；
  和上一次一模一样的失败只推第一次；每次运行时在 prompt 前加一行说明，告诉模型结果会推到哪、没事就只回 `[不回复]`
  （工具型 agent 原来不知道这个约定）。技能 `scheduled-tasks` 里“deliver 不写就是现在这个窗口”是 v0.7.0 的旧说法，一并改了。
  已有任务的 `deliver` 不动（分不清是用户选的还是当年默认的），有了上面两条，没事的那几轮不会再推。
- **编辑任务的弹窗**：运行时间改成点选（每隔 N 分钟/小时、每天几点、每周几、只运行一次、自定义；`web/src/lib/when.ts` 负责和
  cron / every / at 互相转换，认不出来的落到「自定义」原样保留）。「只运行一次」就是 `at …`，后端本来就会在跑完后关掉任务，
  只是界面上没有入口。分组框叠在一起：弹窗是限高的纵向 flex，子元素被压扁、选项溢出来盖住下面——改成子元素不压缩、
  选项在框里滚动。投递列表不再列别的定时任务、内心、做梦、网关群的会话（以前只认 `cron:` 开头，别的 agent 的会话前面有
  `@<agent>/`），聊天窗口显示成「飞书 · 名字」；新任务默认不推送。`scripts/check-web-ui.py` 补了点选、只运行一次、重新打开认得出、
  1280 和 390 宽不重叠的检查。

### v0.7.2 的改动和交接（2026-10-25 发布）

**已经做完（v0.7.2 已发布）**：

1. 3d0ac02（CI 三平台全绿）：定时任务 `[不回复]` 不推送、同样的失败只推一次、编辑任务弹窗（见上一节）。
2. 另一个提交（MCP 和 AI 配置）：
   - **MCP 节点**：拓扑图「能力和内核」一列加了「MCP 服务」节点（在技能和模型之间，上面三个节点各上移一格，画布最小高度 900→1000）。
     面板 `web/src/components/panels/MCPPanel.svelte`（从网关面板整块挪过来，网关面板里已删掉），补了：在哪台机器、那台机器在不在线、
     展开看工具、只开放工具（白名单）字段、不在线的机器在下拉里也保留。挂在别的电脑上的服务从 MCP 节点连线到那台机器；
     机器面板里列「这台机器上跑的 MCP 服务」。⌘K 能搜到。颜色 `--c-mcp`，图标 `plug`。
   - **AI 能配 MCP**：`Gateway {"what":"mcp"}`（配置、在线、上次列到的工具和说明、能挂服务的在线电脑）；`GatewayAdmin` 的
     `mcp_save`（没写的字段保持原样；改名用 `id` 写旧名，密钥跟着走；env 值写空表示沿用）、`mcp_delete`、`mcp_test`。
     代码在 `app/mcp_admin.go`。审批卡上 env 的值打星；`knownSecrets` 加了 MCP 的 env 值（工具结果、日志里出现就脱敏）。
     技能 `skills/mcp/SKILL.md`。README 加了「MCP 服务」一节。工具定义变了，`prefixVer` 2→3（升级后会话的工具快照重建一次）。
   - **顺手修的老 bug**：`gatewayArgs` 里 `BaseURL`、`APIKey`、`IdentityFile`、`ContextWindow`、`MaxTokens`、`MaxHistory`、`AutoConnect`、
     `ThinkingLevels` 没写 json 标签，参数表里是 `base_url` 这种带下划线的名字，Go 对不上——**AI 在聊天里配模型接口时地址和 key
     一直是丢的**。全部字段写明了标签，`TestGatewayAdminSchemaFields` 保证参数表里每个参数都能收到。
   - MCP 连上、断开时 `Manager.OnChange` → 发 `mcp` 事件 → 网页重新读概况（节点上的在线数跟着变）。`MCPInfo` 多了 `tool_names`。
   - 测试：`app/mcp_admin_test.go`（参数表对齐、模型接口地址和 key 存上、AI 加查测改改名删 MCP 且不回显密钥）；
     `check-web-ui.py` 改成点 MCP 节点。已跑：`go test -race ./internal/app ./internal/mcp`、`npm run check`、`check-web-ui.py` 0 failures。
     **发版前跑完的**：`scripts/test-isolated.sh` 全过（C 154 项；exec、host、pipe、pipe-win、mcp-remote 全 0 failures；e2e 74、control 12、install-linux 6、oneliner、update；macOS 交叉编译通过）、
     `python3 scripts/check-web-ui.py` 0 failures（66 项）、`make test-win`（Windows 执行器 14 项、`e2e-win.py` 17 项）全过，CI 三平台 success。

**用户的原则**（2026-10-25 明确说的）：**网页上用户能配置的，AI 在聊天里也要能辅助配置**（工具 + 技能）。反过来 AI 能配的，网页也该能配。

**接下来要做**：

1. **核对“网页能配的 AI 也能配”**。网页所有写接口见 `web/server.go` 里 POST/PATCH/PUT/DELETE 的路由，逐个对照 AI 工具
   （`Gateway` / `GatewayAdmin` / `Schedule` / `Message` / `Knowledge` / 聊天命令）。已经有的：模型、SSH、工作目录、配对、设置项、
   agent（`agent_save` / `agent_delete` / `bot_bind`）、定时任务、MCP、历史删除。**很可能缺的，要逐个确认**：群（房间）的建、改、删
   （`/api/rooms`）；人格文件（`/api/personas`、`/api/agents/{id}/persona`——看 `agent_save` 能不能改人格）；窗口设为管理员、
   接到哪个会话（`/api/endpoints/{key}`、`/api/endpoints/{key}/admin`）；会话的新建、改名、改模式/模型、删除、重置
   （`/api/sessions`，部分有聊天命令）；机器人的改名、删除（`/api/bots`；扫码创建 AI 做不了，可以告诉用户去网页）；
   电脑接入地址（`PUT /api/devices/listener`）；长期记忆条目的改删（`/api/agents/{id}/memory/*`，agent 自己有 `Knowledge`）；
   叫醒、做梦（有 `/wake`、`/sleep` 命令）。缺的加进 `GatewayAdmin`（参数加进 `gatewayAdminSchema` 时 json 标签一定写，测试会查），
   再在对应技能里写怎么用。
2. **用户那台网关**（`linux-pc`，0.7.1）：任务「B站私信值守」每 3 分钟往飞书推一条「[不回复]」，升到 0.7.2 才停；
   着急的话让用户在网页的定时任务里把飞书那个投递窗口取消勾选。
   连那台机器：非交互 shell 里 `ssh` 被 `.bashrc` 的包装函数劫持（`_linux_pc_ssh` 找不到），用 `command ssh linux-pc …`。
   **只读查看**，用户没说不要改他的环境。
3. 没做、等用户定的：
   - 麦麦那种数值：建议先做“熟悉度”（按聊天次数、最近一次算）和“精力”（按时间和预算算），心情只做显示；用户还没回复要不要做。
   - SendFile 发音频：用户说已经支持了，先不管。
   - MCP 连远程地址（HTTP）这条路**没有测试**；也没接过真正的 computer use 服务。

## v0.7.0 发布后的修复

- **新 agent 的窗口「接到主会话」接到了默认 agent 上**：用户新建了一个 agent、把 Telegram / 飞书 / QQ / 微信都绑给它，结果每个会话只挂着一个平台。两半原因：① 窗口接哪个会话，看说话的人**是不是管理员**（不是管理员就各自一个会话，会话 ID 就是窗口 ID）；② 网页里的「设为管理员」和「接到主会话」把目标**写死成全局 `main`**（`WelcomeAdmin`、渠道面板），于是新 agent 的窗口被搬到默认 agent 上，下一条消息又被搬回来 —— 看起来就是“怎么接都接不到一起”。
- 修法：按窗口所在会话的 agent 算主会话 —— 默认 agent 是 `main`，别的 agent 是 `@<id>/main`（`app/endpoints.go` 的 `mainOf`）。`/api/sessions` 里每个窗口多一个 `main` 字段，渠道面板用它，不再写死。测试 `TestWelcomeAdminOnOtherAgent` 锁住“新 agent 的访客窗口设为管理员后进 `@bob/main`”。
- 仍然成立的行为：绑定机器人 ≠ 你在那个平台上就是管理员。管理员名单（`access.admins`）是按平台身份记的（`tg:<id>`、`feishu:<openid>`、`qq.<机器人>:<openid>`）；不是管理员的窗口各自一个会话，要在会话/渠道面板里点「设为管理员」才会并进主会话。
## v0.6 开发进度（dev/v0.6，已合并 main，v0.6.0 发布）

设计和进度清单见 [DESIGN-v0.6-living-agent.md](DESIGN-v0.6-living-agent.md)（每完成一个阶段在那里打勾、写坑）。

## v0.4.1 发布后的修复（2026-10-23，v0.4.1 发布）

- **升级后在开着的网页上找不到新设置**：网关更新以后，一直开着的标签页还在跑旧的页面代码；旧页面不认识新的 `agent` 组，工具调用上限在哪个面板里都不显示（用户说“还是没看到”，刷新后才找到）。现在页面记下加载时网关的版本，之后读概况时发现版本变了就自己刷新一次，刷新后提示“网关已经是 x，页面已刷新成新版本”（`state.svelte.ts` 的 `refresh`）。
- 工具调用上限不好找：设置项改叫「工具调用轮数上限」（用户的说法），搜索词加上“工具调用、调用次数、轮数”；内核面板里的说明直接链到 Agent 节点的这一项；到了上限的回复里也写“工具调用轮数上限”。
- 测试：`check-web-ui.py` 加了“搜‘工具调用上限’能找到”和“网关换了版本，开着的页面自己刷新、版本不变不刷新”（拦截概况接口改版本号）。

## v0.5.0 发布后的修复（2026-10-24，v0.5.1 发布）

用户升级到 v0.5.0 以后说「默认人格不是有两个吗？只看见了一个」「怎么添加人格文件？」，查下来是 v0.5 把人格搬到每个 agent 之后，**agent 的人格编辑器把左边整栏藏了**（`class:hidden={!!agentId}`），自带人格（小纳、大肥鱼）和新建人格文件的入口一起被藏掉；顺手做了一轮易用性检查：

- **自带人格**：agent 的人格编辑器里也显示「自带人格」两个（小纳、大肥鱼），点一下填进编辑框，保存后生效；下面加一句「再要一个人格？」说明每个人格就是一个 agent。
- **添加人格文件**：三条路都写明白了——① 拓扑里「＋ 添加 agent」，新建时可以直接选自带人格（小纳 / 大肥鱼 / 自己写），选了就写进新 agent 的人格文件；② 会话面板「人格」→「新建一个只给这个会话用的人格」（文件在 `~/.nano-agent/personas/`，只在这个会话生效）；③ agent 面板「编辑人格」里改当前这个。
- **说不清楚的英文**：agent 覆盖设置里的枚举（权限模式、群聊积极度、思考强度）和工具白名单以前直接显示 `auto/ask/readonly`、`low/normal/high`、`Read/Write/Shell`；现在显示中文（自动 / 每次问 / 只读；不主动说 / 少插话 / 正常 / 多插话；读文件 / 写文件 / 执行命令…），英文原名放在鼠标提示里。全局设置面板也统一用同一张中文名表（`web/src/lib/labels.ts`）。
- **删 agent 死路**：以前绑着机器人的 agent 删不掉（要先一个个换绑），现在删的时候自动把机器人交给默认 agent，确认框里会列出是哪几个；同时把它从所有群的成员/禁言名单里去掉（群空了就连群一起删），它的会话不再出现在列表里（历史还在磁盘上）。
- **名字不能重复**：以前能建两个都叫「小鲍」的 agent，按名字 @ 会有歧义；现在建和改名都会拒绝，提示「已经有叫「小鲍」的 agent 了（id），换个名字」。
- **措辞**：机器人面板「这个机器人是谁」→「接到哪个 agent」；「渠道」→「渠道 ID」；群面板「记录」→「聊天记录（就是会话）」；预算写成「每天最多用多少 token（AI 的用量单位…）」；建群面板的工具说明改成「agent 之间怎么传话」。
- 测试：`check-web-ui.py` 加了「agent 的人格编辑器里能看到两个人格」「点大肥鱼会填进编辑框」「新建 agent 时选自带人格会写进人格文件」「群聊积极度选项是中文」「会话里能新建人格文件」；Go 测试加了 `TestAgentNameUnique`、`TestDeleteAgentCleansUp`（删 agent 后会清群成员、会话不再列出），并把「绑着机器人不能删」的旧测试改成「机器人交给默认 agent」。

## v0.5 开发进度（dev/v0.5 合并回 main，v0.5.0 发布，2026-10-24）

v0.5 是多 agent、机器人绑定、群聊（[PLAN-roadmap.md](PLAN-roadmap.md) 阶段 6–8）。大版本在新分支 `dev/v0.5` 上做，每个阶段 `make test` 全过后单独提交；做完、用户同意再合并回 main 发布。

### 阶段 6/7/8 都做完了

- 阶段 6：多 agent、机器人绑定、每个 agent 覆盖设置（见下）。
- 阶段 7：外部群聊（Telegram、飞书）能听会回、点名才回、安静了按积极度插话、发短消息、可回 `[不回复]`。
- 阶段 8：网关内部的群（房间）和两个 agent 的私聊、防刷屏（连续 8 条 / 来回 6 轮自动暂停）、`Message` 工具、网页上的群节点和面板。
- **还没做**：专门给群聊做的界面——成员头像、@ 补全、多人气泡（现在群里就是普通会话页，每一条前面带 `[名字]`），下一版再做。

### v0.5 验收修复（2026-10-24，用户验收提的问题）

- **升级盖掉用户改过的人格**：`nano-gateway update` 的 `installShipped` 会先写包里自带的 `agents/default/persona.md`，网关启动时 `MigrateAgents` 看到目标已存在就不再迁移，用户改过的 `personas/default.md` 被忽略。现在：老布局（有 `personas/default.md`）的机器升级时 `installShipped` 不写这个文件，由迁移决定；迁移改成**复制**、原文件留着（更新失败退回旧版本还要用它）。`personaFor` 也不再兜底回 `personas/default.md`（一处能改），只有没有 agent 列表的老模式才用它。测试：`scripts/test-update.py` 加了「从 v0.4 布局升级（用户改过人格）」的用例，用单独的 HOME 装 0.4.0（包里只有 `personas/default.md`）、改名、删掉 `agents/`（真实 v0.4 机器就是这样）、升级到 0.5.0，确认用的还是用户的人格、原文件还在、没有 `.new`。
- **`Message` 只给管理员**：`exec.go` 里执行前和 Gateway/GatewayAdmin 一样查 `isAdmin(r.from)`；测试 `TestMessageGuestRejected`（访客 readonly 时模型调 Message 被拒，内核收到 OK=false）。
- **拓扑图**：群（房间）单独一列（`群` 在会话和 Agent 之间），群的聊天记录连到群节点而不是默认 agent；两个 agent 的私聊不再当普通群，画成一个虚线小节点；每个机器人（含停用的）都是入口列一个节点，并画出到它绑的 agent 的连线；入口直接连 agent 的线（没有会话的机器人、定时任务）还是直连（和 v0.4 一样）：线画在卡片下面，被挡住的段落看不见；中间试过“绕画布底部 + 走空隙”，用户说原来的更好，已改回直连。停用的机器人在后端 `Channels()` 里也要出现（原来只在 `a.runs` 里有）。agent 在群里旁听的会话（`@<agent>/room:<id>`）不再当成普通会话显示。网页检查用「3 个 agent、2 个群、1 个私聊、1 个停用的机器人」的场景截图并断言连线（`topology-groups.png`）。
- **群聊插话的判断也花模型调用**：以前只有说出口的才记额度，每次「要不要说话」的判断都是白花的模型调用。现在除了每小时说几次，还限判断次数（积极度 low/normal/high → 每小时最多 6/18/45 次，两次判断至少隔 1 分钟），写进了 agent 面板「群里插话积极度」的说明；`TestGroupThoughtBudget`。
- **小问题**：中文名字的 agent 以前 ID 会变成 `agent`、`agent-2`，现在取不出拉丁字母时用短的随机 ID（`a-1f2e3d`），面板上仍然显示名字；时长不再显示 `30m0s` 而是 `30m`（`durText`）；agent 面板里关于 `personas/default.md` 的说明按修完后的真实行为改了；README 里升级那句话也改了。
- **第二轮验收发现：内置 Telegram 在图上出现两次**（一个一直是停用的）。“停用的机器人也上图”那段按机器人的消息渠道名找，内置 Telegram 的消息渠道名是 `tg`，而 runs 和图上它叫 `telegram`；改成用 `botRunKey`。只影响显示，路由一直是对的（消息带的渠道名就是 `tg`）。`TestChannelsIncludeDisabledBots` 检查渠道列表正好是 cli、web、telegram、feishu 加另外的机器人；`check-web-ui.py` 检查入口列没有重名的节点。
- 还可以再好看点（没改）：没有会话的机器人到 agent 的虚线从画布最下面绕过去，再从下往上接到 agent 卡片的底边，会穿过下面几个 agent；能看清，但不够干净。

### 阶段 6：多 agent 和机器人绑定

- 用户的要求：多 agent 要在拓扑图上有节点，agent 之间的群聊也用节点和连线（阶段 8）；每个 agent 可以覆盖全局设置（模型、思考强度、每条消息上限、权限模式、上下文整理），没覆盖的跟随全局；定时任务可以指定由哪个 agent 跑。这两点做前问过用户，都同意。
- **数据**：`data/agents.json`（名字、头像、模型、思考强度、覆盖的设置、工具白名单、群聊白名单）、`agents/<id>/persona.md`（人格）、`data/bots.json`（机器人：种类、凭据加密、绑定的 agent、显示档位、是否内置）。会话 ID：默认 agent 不变（`main`、`tg:123`），别的 agent 加前缀 `@<id>/main`、`@<id>/tg:123`（`agents.SessionFor` / `SessionAgent`）。
- **迁移**：第一次启动建默认 agent「小纳」，把 `personas/default.md` 挪到 `agents/default/persona.md`（挪完删原文件；目标文件已存在就不再挪）；内置 Telegram、飞书当成 `tg`、`feishu` 两个机器人绑到默认 agent。现有的会话、窗口、历史都不动。
- **覆盖**：`agents.Agent` 里的指针字段为 nil 表示跟随全局。优先级：会话自己设的（`/model`、`/mode`、`/think`）> 所属 agent 的 > 全局（`config.toml` 的 `agent`、`workspace.mode`、`context` 组）。实现是 `limitsOf`、`agentModeDefault`、`contextConfigFor`、`modelFor`、`thinkingOf`、`filterAgentTools` 这几个，调用方都持有 `a.mu`。
- **机器人**：内置 tg/feishu 的凭据还是 `gateway.toml` 里的 `[telegram]`、`[feishu]`（设置只有一个家：Telegram、飞书节点），另外加的机器人凭据存在 `bots.json` 里（`secret.Encrypt`，网页只回传有没有）；渠道名 `tg.<id>`、`feishu.<id>`（`channel.SetName`，Telegram/Feishu 的 `Name()` 改成读字段）。`ReconcileChannels` 按机器人列表启动、停止、重启；窗口接哪个会话由 `agentForChannel` 决定，机器人换绑后窗口跟着换到新 agent 的会话。
- **网页**：Agent 列里每个 agent 一个节点（名字、头像、会话数、机器人数），下面是「Agent 默认值」（`agent` 组全体设置的家，`FieldGroup`）和「添加 agent」；入口列多了「添加机器人」；内置 Telegram / 飞书节点里能换绑 agent。面板：`AgentPanel`（人格、模型、思考强度、覆盖设置、工具白名单、加机器人）、`AgentDefaultsPanel`、`AgentsPanel`、`BotPanel`、`BotsPanel`。人格编辑器支持编辑 agent 自己的 `agents/<id>/persona.md`（`store.persona.agent`）。
- **聊天里**：`Gateway {"what":"agents"}` 查 agent 和机器人；`GatewayAdmin` 加 `agent_save`、`agent_delete`、`bot_bind`。技能 `skills/agents/SKILL.md`。
- **定时任务**：任务记创建时所在的 agent（`schedule.Task.Agent`），会话 ID 是 `@<agent>/cron:<id>`（`cronSID`），任务面板能选「由谁执行」。
- **测试**：`agents` 包单测（ID 校验、默认 agent 不能删、持久化、会话 ID 前缀）；`app` 里测迁移、人格查找顺序、覆盖优先级（会话 > agent > 全局）、工具白名单、机器人路由和换绑、agent 增删改；`check-web-ui.py` 加：每个 agent 都有节点、Agent 默认值 / 添加 agent / 添加机器人节点在图上、agent 面板能存覆盖设置、Telegram 节点能换绑。
- **坑**：面板里 `onclick={() => store.open({ type: 'bots' })}` 手写时少一个 `}` 会让 svelte-check 报「Expected token }」；改成在 script 里写一个函数再 `onclick={openBots}` 最稳。Agent 列节点变多以后列会变长，拓扑图会自动缩放适配；拖过节点的偏移量还记在 `nano.layout2`。

### 阶段 7：群聊行为（2026-10-24）

- 用户的要求：群聊像真人——不在群里展示思考和工具过程、不长篇大论、不是每句都回、只能用少量工具。
- **会话**：每个外部群在每个 agent 下一个会话（默认 agent 是 `tg:<群ID>`，别的是 `@bob/tg:<群ID>`），会话设置里标 `group`。群聊默认不挂工作目录，工具只有 agent 的群聊白名单（默认 WebSearch、WebFetch、Skill），权限按访客算（`access.guest`）。
- **听着**：群里每条消息都记进历史，格式 `[15:08][张三] 内容`，不触发模型（`listenGroup`）。
- **什么时候说话**：被 @、回复了机器人（渠道给的 `Mentioned`）、或者消息里叫到 agent 的名字 → 等 2 秒（`groupReply` 的定时器）把这期间的几条一起交给模型；这一轮不再追加用户消息（`queued.trigger`）。
- **插话**：`groupTick` 每秒看一遍——群里安静够久（默认 20s）、有新消息、最短间隔（10s）和每小时额度（off/low/normal/high = 0/2/6/15）都允许时，让模型决定要不要说；不想说就回 `[不回复]`（这一轮只在请求里补一句提醒，不改历史）。
- **怎么说话**：系统提示 = 人格 + 群聊守则（`agents/<id>/group.md`，没有就用内置的）；回复按空行拆成最多 3 条、每条最多 300 字，之间停 0.3–3 秒；不发思考、工具过程和用量（群聊这一轮不开流式窗口）。
- **设置**：插话积极度、安静多久、最短间隔、一条多长、最多几条，都在 agent 自己节点的「覆盖的设置」里（`group_*`，存 agents.json）。
- **平台前提**：Telegram 要关掉 privacy mode 或者当群管理员；飞书要开「获取群组中所有消息」。写进了 Telegram / 飞书节点的接入说明。
- **测试**：`group_test.go`——只听不回、被 @ 才回（回复里没有用量）、`[不回复]` 什么都不发、拆条和长度上限、安静期 / 最短间隔 / 积极度 off、群聊工具白名单和群聊守则、群聊默认不挂工作目录。
- **坑**：以前群消息会被当成管理员的窗口接进主会话；现在 `attach` 对群消息一律用这个群自己的会话。
- **坑**：拓扑图第一次打开时，节点有 0.45 秒淡入，截图/自动化容易截到“只有连线没有卡片”；检查脚本现在等节点 opacity 到 1 再截图，并且 `prefers-reduced-motion` 下不做淡入。

### 阶段 8：网关内部群聊和 agent 私聊（2026-10-24，后端 + 节点，聊天界面还没做）

- 数据：`data/rooms.json`（id、名字、成员 agent、点名/自由、暂停、禁言、是不是私聊）。群的聊天记录直接用网关的会话 `room:<id>`（网页的会话页能看、能发言），每个成员 agent 在群里还有自己的会话 `@<agent>/room:<id>`。
- 说话：人在 `room:<id>` 里发言 → 转给所有成员（`[张三] 内容`），被 @ 或者叫到名字的进「攒 2 秒再回」那一套（沿用阶段 7 的群聊引擎：短、口语、不发思考、工具只有群聊白名单）；自由模式按积极度插话。agent 的回复由 `roomAgentSay` 记进群的记录并转给其他成员。
- 防乱：连续 8 条都是 agent 说的、或者两个 agent 来回 6 轮（中间没有人说话）→ 自动暂停并在群里说一声；手动暂停 / 恢复、禁言某个成员、点名 / 自由在房间节点里改。
- `Message` 工具（网关执行，内核登记成 NA_TOOL_READ）：`{to:"room:群名", text}` 发到群、`{to:"agent:名字", text, wait:true}` 私聊并等对方回话（最多 5 分钟）；两个 agent 的私聊就是两个成员的房间，第一次发消息时自动建。
- 网页：拓扑图 Agent 列上面是群节点（连着成员 agent），下面是「创建群」；房间面板能改成员、禁言、点名/自由、暂停/恢复、打开聊天、删除。聊天就是用会话页——发到 `room:<id>` 的话就是群里的话。
- **收尾补上的**：建群/改名时把群记录会话的标题设成群名（会话列表里不再是 `room:abc123`）；`/room` 命令（列出、`/room <群名> pause|resume`，只有管理员）；群每天 token 预算（房间面板里填，0 不限，超了自动暂停并说明）；`Message` 发给整个群时也按点名/自由判断，不会把所有人叫起来；`/room` 的命令权限补了 `isAdmin` 检查。
- **还没做**：多人头像和 @ 补全这类专门的群聊界面（现在就是普通的会话页，能看到每一条和谁说的）。下一轮补。
- 测试：`rooms` 包的增删改查；`TestRoomFanout`（人说话转给成员、被叫到的回、记录里能看到）。

## v0.4 开发进度（dev/v0.4，已合并 main，v0.4.0 发布）

v0.4 是“拓扑图就是控制台”（原来 v0.4 的多 agent 顺延到 v0.5，见 [PLAN-roadmap.md](PLAN-roadmap.md)）。设计见 [DESIGN-topology-config.md](DESIGN-topology-config.md)。

### 阶段 0：配置和节点合在一起（2026-10-23）

- 起因：用户说“既能用节点配置，又能在设置页配置，只保留一个，最好基于拓扑节点……起码保证项目能够稳定运行，而不是到处漏风”。逐项查过 v0.3.2：模型请求参数只在设置页；默认权限模式只在“没有工作目录”时的占位节点上；`kernel` 组（网关自己的监听地址）挂在每个内核节点上；每条消息的上限在每台电脑的 kernel.conf 里（同一个版本里已改成网关设置）；“检查新版本”开关改了不生效、检查更新不走代理（同一个版本里已修）。
- **只有两个页面**：拓扑（所有对象的状态、操作、设置）和会话（聊天）。设置页、设备页、定时任务页、人格页删掉；旧链接 `#/settings[?focus=键]`、`#/devices`、`#/schedules`、`#/personas[/文件]` 跳到对应的节点（`state.svelte.ts` 的 `parseHash` / `openLegacy`）。
- **每个配置项只有一个家**：`config.Field.Group` 就是节点（`config.Nodes`：agent、gateway、web、cli、telegram、feishu、model、memory、search、skills、local、machines）；`TestEveryFieldHasAHome` 保证每项的 group 是已知节点、key 对应真实配置、每个节点至少一项。网页 `lib/nodes.ts` 的 `HOMES` 是 group → 节点；不认识的 group 显示在网关节点的「其他设置」里。`Gateway {"what":"settings"}` 的 group 就是节点（`TestSettingsGroupsMatchNodes`），每项带 `node`；聊天里改不了的项说明去哪个节点改。
- **新节点**：Agent（人格、每条消息的上限、默认权限、管理员和访客、显示档位、消息时间、时区）、技能、「＋ 接入机器」（配对请求、安装命令、电脑接入地址、添加 SSH 服务器）、「＋ 添加工作目录」。布局从左到右：入口 → 会话 → Agent → 网关 → 能力（记忆、搜索、技能、模型）/ 内核 → 机器 → 工作目录，列上方有列名。节点上有标记（渠道报错、搜索没 key、没有模型、没有内核、机器用不了、待重启、新版本、等配对）。
- **面板**拆成每个节点一个组件（`components/panels/*`），`Inspector.svelte` 只管外壳（标题、宽度可拖、按对象切换）；共用排版在 `panel.css`（类名带 `p-` 前缀，避免和设置项组件的类名互相影响）。人格编辑器、SSH 表单、定时任务表单是盖在页面上的对话框（`lib/portal.ts` 挪到 body 下）。
- **测试**：`scripts/check-web-ui.py`（`make test-ui`，`make test` 里有 playwright 就跑；CI 的 Linux 任务装 chromium-headless-shell 后单独跑）：起一次性网关，逐个点开所有节点，确认 55 个配置项每个恰好在一个节点里；改数字、文本、开关、列表后刷新还在，不合法的值不存；网关节点的改过列表能跳转；旧链接跳对；⌘K 搜设置跳到节点并高亮；人格编辑器保存；定时任务新建删除；没有 JS 报错（4xx 网络请求不算）。截图在 `build/ui-check/`。
- 坑：Esc 关面板是在下一轮事件里做的（先让弹窗处理 Esc），测试里 Esc 后马上按 Ctrl+K 会把刚打开的命令面板关掉，测试里中间等了 200ms；用户手速碰不到。加宽的模型面板会盖住图右边的节点，测试点下一个节点前先关面板。

## v0.3 开发进度（dev/v0.3，已合并 main，v0.3.0 发布）

### 阶段 1：上下文计量和 /context（2026-10-23）

- 保存每次正常模型请求的真实输入、缓存、输出用量和请求前的绝对历史位置；摘要调用只计累计用量，不覆盖正常请求的校准。上下文使用真实输入加新增历史估算，分项等比缩放并用对话项吸收整数舍入误差。
- `/context`、网页会话里的可展开进度条、`Gateway {"what":"session"}` 共用同一份计量；窗口跟随所选模型。重启保留计量；reset 清掉计量。
- 选择：模型、人格、工具定义或摘要变了，旧校准失效，恢复估算直到下一次真实请求，避免把另一套请求的用量套进来。图片暂无独立 token 估算，仍由真实接口用量校准。
- 测试：`scripts/test-isolated.sh` 运行 `make test`，临时 HOME/NANO_HOME/USERPROFILE/XDG 目录；mock 模型、本地假服务。`scripts/test-isolated.sh web` 运行 `make web`（检查和构建），提交 dist。新增计量组合、分项总和、初始估算、校准失效、窗口及重启测试。
- 坑：原测试子进程换 HOME 后继承外层 NANO_HOME，技能路径落错；已让宿主测试和 e2e 同步设各自临时 NANO_HOME。新增 `NANO_TEST_NO_WINDOWS=1`，跳过 Windows interop、安装和 DrvFS 测试；第一轮漏掉 kernel 的带引号判断，触发了原有临时 Windows 执行器测试，已补齐，未动用户 Windows 配置/服务。后续统一通过隔离入口测试。

### 阶段 2：上下文整理、归档与删除（2026-10-23）

- 新增 context 配置和设置页字段：75% 整理到 40%、保留最近 4 轮、空闲 30m/30%、归档 90 天（0 永久）。模型/旧 llm 的显式历史上限继续生效。
- 两层整理：旧工具结果超过 2KB 存绝对下标占位表，原 JSONL 不动；摘要按窗口 1/3 预算分块滚动，单条工具输出截短，六项固定结构，带入已有摘要和 `/compact` 重点。失败保留第一层，仍超限才省略；当前轮和最近 keep_turns 不丢。近期内容本身太大时允许超过目标，不悄悄牺牲保留区。
- 请求前、结束后 80% 阈值、空闲和手动整理；后台整理时新消息排队，取消/reset 的迟到结果不提交。假时钟通过 Maintain 驱动测试，日常每分钟检查空闲、每天清过期归档。
- 人格和工具定义在首次请求持久化快照，reset/整理后刷新，人格文本变了也刷新（见下面「验收修正」）；技能文件内容仍按需读取，增删技能定义等到固定时机刷新。调整了原来“下一轮立即改工具定义”的旧测试。
- `/reset` 归档到 data/archive/<会话ID>/；`/reset 删除` 管理员直接删除当前历史。网页可选连续轮次永久删除、清摘要、删单份归档、两次确认删除全部历史及归档。GatewayAdmin history_delete 继续任何模式审批，支持全部或按轮（0 起算）。删除重算 Base/占位表并使计量失效；当前 run 不能删自身的某几轮，全部删除会等本轮结束完成。阶段 10 尚未实现 Diary，因此不存在可继续读取的第二份副本。
- 测试：隔离入口下 make test、make web；新增分块大小、工具占位和原始文件不变、删除位置重算、归档/保留期、手动重点、假时钟空闲和排队、摘要失败保留第一层。现有取消、重启恢复、前缀一致性和整套端到端测试保留。
- 坑：mock 原来固定报告 100 输入 token，真实用量校准后永远不触发长对话整理；命令行 mock 改为按请求估算报告用量，固定统计的单元测试仍可用原模式。配置中的显式 0（永久保留/关闭空闲）在解析前设默认值，避免被默认值覆盖。

### 阶段 3：聊天显示档位（2026-10-23）

- 顺带修复阶段 2 CI 暴露的旧 e2e 偶发失败：expect 在整个输出里搜索，HOME Shell 的 secret-profile 和 elevated 会命中前面请求，测试连续发两条 !call 被正常批处理合并成非法 JSON。现在 send 记输出字节位置，expect 只检查这次发送之后的内容；累计次数断言继续单独用 expect_n。

- `channels.show` 默认“过程”；telegram.show/feishu.show 机器人覆盖；`/show 详细|过程|简洁|default` 窗口覆盖存在 endpoints.json。网页固定完整显示，终端也使用窗口档位。
- RunView 包装器过滤思考/工具/结果/备注，不改变历史和模型请求。简洁只保留正文，Telegram 临时状态结束后删除，飞书移除状态组件，终端在支持颜色的交互窗口用可擦除单行状态（非交互输出省略状态，避免留下控制字符）。后台整理完成提示在简洁窗口省略，网页照常显示。
- 测试：隔离 make test；三档假渠道事件、临时状态清除、配置优先级和窗口持久化；平台 httptest 流式测试继续覆盖。设置页由 schema 自动生成，本阶段没有改前端源码。

### 阶段 4：DeepSeek 网络搜索（2026-10-23）

- 新增 internal/search Provider、DeepSeek 官方 Search/Fetch，WebSearch 默认 6 条/最多 10 条，带编号、来源链接和摘要；WebFetch 按 Unicode 字符每页 20000 字，提示下一页 offset。
- search 配置由 schema 提供设置页字段；密钥加密并只回传是否设置。显式 search.api_key 优先，否则只从 models.json 中主机名恰好为 api.deepseek.com 的接口回退；关闭或无 key 时不提供工具。工具定义仍在新会话/reset/整理时刷新。
- 408/429/5xx 最多重试两次，支持取消；401/402/429 中文提示，错误不回显远端响应体。重定向不转发凭据。非空成功次数按网关时区记入 data/search-usage.json，重启保留，/status 显示当天统计。
- 选择：Fetch 缓存 10 分钟（最多 32 页），同一网页按 offset 续读复用内容，避免每页重复付费；空结果不计成功次数。网络工具在所有模式（含访客 none）可用，独立的公开只读策略不放宽 Read/Write 等权限。
- 测试：隔离 make test；全部搜索测试只用 httptest 或内存 Provider，从未调用真实 DeepSeek 接口。覆盖鉴权/参数、结果格式、中文分页与缓存、错误和重试、取消、空结果、严格主机名回退、访客工具及前缀稳定、每日统计和重启、配置加密与网页隐藏值；C 状态机验证四种模式均直接执行网络只读工具。本阶段没有改前端源码。
- 坑：新增网关工具也要在内核登记私有执行策略，并跳过工作目录预检，否则访客会在到达网关前被拒绝；工具执行本身仍在网关，不向执行器传搜索密钥。

### 阶段 5：定时任务（2026-10-23）

- data/schedules.json 原子保存任务、下次时间、运行中标记、last 和最近十次结果；cron 用 robfig/cron/v3 v3.0.1 五段解析器，另支持 every 时长、at 当地时间。时区与 session.timezone 相同，默认系统时区。新增 /cron 列表与 run/on/off/rm，管理员 Schedule 工具，网页“定时任务”页（新建、编辑、开关、立即运行、结果、任务会话）。
- 每次执行在 cron:<id> 先归档/reset，再设置工作目录和模式并正常调用模型/工具；只把最终结果投递到 deliver，多窗口审批也送到 deliver，可在原窗口 /approve 或 /deny，或网页审批。任务不把结果追加进投递窗口原会话的模型历史；网页通知是实时消息，持久结果在任务页和独立任务历史里。
- 权限：访客不能管理任务；模型 create/update/delete 在 ask 审批、auto 直接、readonly 拒绝，list/run_now 按只读处理。创建时记录模式上限，更新不能提高，也不能高于当前操作会话；立即运行也检查当前模式。任务运行使用保存的模式，创建者不再是管理员时不运行。工作目录可选已登记的目录，不另加目录黑名单。
- 选择：网页明确点击、用户直接输入 /cron 是用户自己执行管理动作，不再让模型审批；Schedule 工具修改仍由真实内核审批。阶段 6 前 agent 固定 default。timeout 可按任务设置，默认总时限 30m（包含等待审批）；审批时限也是 30m，普通聊天原配置不变。
- 补跑：先持久化“本次已领取”和未来 Next，再启动。相同任务不重叠；周期任务停机错过多次，只补最近一次且错过不超过一小时，catch_up=false 不补；过期一次性任务只记录“错过”并关闭。运行中再到点则跳过那次。崩溃后原先 running 的任务记“已中断”，不重放可能已产生副作用的任务；已经推进的 Next 保留。数据损坏时停用调度并在网页报错，绝不覆盖成空列表。
- 任务下次运行会归档并清空，因此不做结束后/空闲的后台摘要；运行中需要的同步上下文整理照常。停止和断线、无模型/内核、超时都记录失败并通知，迟到内核消息不重复完成。启动断线避免重复记录/投递。
- 顺带修复阶段 2 的一处遗漏：GatewayAdmin history_delete 在 readonly 下现在也会进入审批；其他 GatewayAdmin 管理动作保持既有权限。C 状态机和宿主测试覆盖。
- 测试：最终隔离 make test 全套（C ASan/UBSan、Go race、67 项 e2e、控制与离线安装、假 Release 升级/回滚、macOS 交叉编译）；make web 检查和构建通过并提交 dist。新增时区/春季跳时/秋季重复小时、非法 cron 与 DST 一次性时间、间隔跳过、补跑/关闭补跑/超一小时、运行领取和重启恢复、损坏文件、结果保留、mock 执行和归档、原会话不变、投递窗口审批、超时/离线失败、权限上限和访客拒绝、认证及防跨站接口测试。真实内核宿主用假网关验证 Schedule action 策略和网络工具访客权限；没有调用真实模型/DeepSeek/Telegram/飞书。
- 坑：现有审批命令原本只接受当前会话的审批单；定时任务的审批单属于 cron 会话，所以增加“该任务的投递窗口”匹配，仍需管理员。内核默认聊天总时限只有 10m，因此协议 v9 增加可选的单轮时限覆盖字段；不改普通聊天默认时限。cron 模块下载的默认代理 EOF，使用临时 GOPROXY 下载固定版本，没有改全局环境。

五个阶段已按顺序分别提交到 dev/v0.3；验收修正后用户同意，`--no-ff` 合并回 main，在 main 上打 v0.3.0 发布。v0.4（阶段 6–8）从 main 新开 `dev/v0.4`。

### v0.3.2 发布后的修复（2026-10-23，和 v0.4 一起在 v0.4.0 发布）

- **Windows 内核重启后连 127.0.0.1:7700、改了地址也起不来**（用户的真实 kernel.conf 逐行看过）。两个 bug 叠在一起：
  - 安装脚本升级时用 `Get-Content` 读 kernel.conf，PowerShell 5.1 对没有 BOM 的文件按系统编码（GBK）解码，第一行中文说明成了乱码，乱码吃掉了换行，`gateway = …` 进了注释行；内核读不到网关地址，用了 Windows 的默认值 `tcp:127.0.0.1:7700`。
  - 安装脚本写的模板最后一行没有换行；`nano-kernel connect` 找不到 `gateway` 行就追加到最后，变成 `max_output = 65536gateway = …`，内核直接起不来。
  - 修法：安装脚本按 UTF-8 读写，升级时修好乱码说明行（救回粘在后面的配置项）、拆开粘在一起的行，每行以换行结尾；`conf_set` 追加前补换行、拆开粘住的行（`nano-kernel connect` 就能修好坏文件）；内核读配置时跳过 UTF-8 BOM（老记事本会加），粘住的行报“好像是两行粘在了一起”并给出修法。sh 安装脚本也补了“最后一行没换行”和“没有 gateway 行”的处理。测试：`test_host.py` 的 `conf_repair`、`test-install-win.py` 用用户那种坏文件不带 -Gateway 升级。
- **每条消息最多请求 16 次模型、最长 10 分钟，网页改不了**：这些写在每台电脑的 kernel.conf 里。现在是网关的设置 `[agent]`（v0.4 起在拓扑图的 Agent 节点里）：`max_steps` 默认 200（0 不限）、`run_timeout` 默认不限、`approval_timeout` 默认 30m（0 一直等）、`max_output_kb` 默认 64。网关每一轮在 input 里带上 `max_steps`、`run_timeout_ms`、`approval_timeout_ms`（大于 0 是上限，小于 0 不限）和 `max_output`；kernel.conf 的同名项只在连着旧网关时兜底，安装脚本不再写它们（升级时去掉以前写的默认值）。到了上限，回复说明“发「继续」接着做、在哪里改”。测试：`test_gateway_limits`（core）、`t_gateway_limits`（host）、`TestAgentLimits`、`TestInputCarriesAgentLimits`。
- **检查更新不走代理**：发布包下载用的是只认环境变量的默认 HTTP 客户端，而网关作为 systemd 服务运行时，终端里的 `HTTPS_PROXY` 传不进去（用户的网关进程里确实没有代理变量）。现在检查更新、下载新版本走「默认代理」`network.proxy`，失败时错误里写明走的是什么（直连 / 环境变量里的代理 / 哪个代理）；默认代理在网页上显示网关进程实际看到的环境变量代理。
- 「检查新版本」开关只在启动时读一次，改了不生效：现在每次到点都看当前设置。
- 拓扑节点漏掉的设置：模型节点加上「请求参数」（llm 组，以前只在设置页），工作目录节点加上默认权限模式（以前只有没有工作目录时的占位节点上有）；⌘K 里的模型请求、消息与显示直接打开对应节点。

### v0.3.1 发布后的修复（2026-10-23，v0.3.2 发布）

- 设置页的数字项（上下文整理阈值、保留轮数、超时、温度……）改了不保存：数字输入框 `bind:value` 给的是数字，`commit()` 里对它调 `.trim()` 抛异常，没有请求也没有报错。现在先统一成文本再处理；用浏览器验证回车、点别处都会保存，刷新后还在。
- 显示档位的全局设置埋在「网关」组里找不到：新设置组「消息与显示」（`chat`：channels.show、session.timestamps、session.timezone），Telegram、飞书、命令行节点的面板里也显示这一组；机器人自己的显示档位空选项显示成“跟随默认”。设置页和面板上写明“改完按回车或点到别处就自动保存”。

### v0.3.0 发布后的修复（2026-10-23，v0.3.1 发布）

- 飞书回复出现两次、卡片停在“正在回复”、面板里是“（没有思考内容）”；Telegram 结束后工具记录被删掉。原因：默认的过程档结束时 `filteredView.Close` 也调了 `Status("")`，而 `streamRun.Status` 会把这一轮标成简洁档；飞书于是把状态行更新成空字符串，这次更新失败（从现象看飞书不接受空内容）→ 流式失败 → 结束后补发正文，Telegram 按简洁档删掉了过程消息。
- 修法：档位在打开时由 `FilterView` 通过 `SetShow` 告诉流式窗口（快照里的 `Show`），不再从状态行推断；只有简洁档才清状态行。飞书卡片按档位排版：详细档“思考和工具调用”，过程档只有“工具调用”，简洁档没有面板；简洁档结束时删掉状态行，不再更新成空。转发到别的窗口（mirror）先不发，等档位定下再建卡片。
- 测试：飞书假接口拒绝空内容更新；新增过程档和简洁档经过 `FilterView` 的飞书、Telegram 测试。在 v0.3.0 代码上用同样的假接口能复现截图里的现象（卡片停在“正在回复”，再补发一条“正文 + 用量”）。
- 设置和拓扑补齐（用户反馈“记忆、搜索都找不到设置，希望有节点”）：设置页拆出「上下文与记忆」（`memory` 组：context.* 和高级的 llm.trim_to）、「网络搜索」（`search` 组），剩下的请求参数叫「模型请求」（`llm` 组）。拓扑图右边一列从上到下是记忆、搜索、模型（网关用的服务），左边渠道下面是定时任务（连到 `cron:` 会话）；搜索工具调用时 网关→搜索 的连线有光点。三个节点各有面板（搜索可以“测试搜索”，真的计费一次）。概况接口多了 `search`（是否启用、key 来源 settings / endpoint:<接口名>、今天次数，不含 key）、`schedules`（任务数、下次时间）、`memory`（整理过的会话、归档份数）；`search`、`schedules` 事件带这些数据，网页直接更新。
- 技能和聊天里的设置：v0.3 的功能当时没写技能，聊天里也改不了搜索、上下文这些设置。新增技能 `web-search`、`scheduled-tasks`、`context-and-history`、`gateway-settings`；`Gateway` 加 `turns`（删历史前查轮次编号，从 0 起，标出正在执行的一轮和已整理进摘要的）、`search`、`settings`（可按组，密钥只给 is_set，改不了的标 changeable=false 和原因）；`GatewayAdmin` 加 `settings_set`（和网页设置页同一条路：`config.Store.Set`，发 config 事件），要重启才生效的、只读的、除搜索 key 以外的密钥不开放；审批卡片上密钥类设置的值打星。测试 `settings_tool_test.go`。
- 设置页里“没写就默认开”的开关（search.enabled、session.timestamps、update.check、cli.* 等 *bool）以前显示成关着；现在没写时报告为开。

### 验收修正（2026-10-23）

- 人格切换不生效：提示快照冻结了系统提示，`/persona`、网页「人格」页保存、直接改人格文件，在会话第一次请求之后都不生效（`/persona` 却说“下一条消息开始生效”）。现在人格文本和快照不一样时就重新取快照（工具定义一起刷新）：人格变是用户有意的，系统提示一变缓存本来就全部失效。人格没变时工具定义照旧冻结（`TestGatewaySkills`）。测试：`TestPersonaCommand` 改回默认、`TestPersonaEditRefreshesPrefix`。
- `/context` 按方案的格式：`41.2k / 128k（32%）`、阈值带百分比、摘要带整理时间和整理掉的条数、没请求过时说明是估算、`idle_compact = "0"` 时写“空闲不自动整理”（`TestContextText`）。
- 网页：定时任务页“立即运行”的图标（原来引用了不存在的 `play`，显示成 ×）、删除按钮改成红色、投递位置显示窗口名字；删轮次的列表去掉 `[系统]` 说明和时间戳，只显示用户说的话。`HistoryManager.svelte` 按普通格式重写。
- 代码风格：新加的 Go 注释改成中文；内核新加的几行按原来的缩进和空格重排，网关执行的工具名单收成 `gateway_tool()`。

## 一句话

nano-agent 是作者自己的轻量 bot 智能体：**C 内核**（agent 循环、权限、沙箱里执行工具，没有状态）+ **Go 网关**（上下文、模型、聊天平台、网页界面，唯一有状态的中心）。工作目录 = 机器 + 目录，机器是网关所在的本机、网关自己 SSH 连的服务器（只放执行器）、装了内核的电脑；不管内核跑在哪，都能在任何挂在网关上的工作目录里干活（同机由内核本地执行，异机由网关执行）。

所有自己产生的文件都在 **`~/.nano-agent`**（`NANO_HOME` 可改）：`bin/` 程序、`lib/` 内核和执行器、`gateway.toml`、`personas/`、`skills/`、`data/`、`run/` 以及 `secret.key`。删掉这个目录就清理干净；只有 systemd 服务文件和 shell 的 PATH 行在目录外，`--uninstall` 会一起删。


## 目录

```
kernel/                C 内核和执行器
  src/main.c           内核：poll 主循环，连网关、按 workspace 参数派发工具（本机执行 / 交给网关）、代网关执行（run）、放技能文件（skill_put）、列目录
  src/core.c/h         循环状态机（sans-I/O）：预算、权限模式、审批
  src/runner.c/h       执行器子进程的管理（超时、取消、清理；POSIX）
  src/runner_win.c     Windows 上的执行器子进程：管道 + 读线程 + 回环 socket 唤醒 + Job Object
  src/conf.c/h         kernel.conf 解析（旧的 remote = / ssh = 给出提示，skills = 忽略并提示）
  src/plat.h           平台层的接口（路径、文件、自己、子进程、参数、socket/poll）
  src/plat_posix.c     平台层的 POSIX 实现（Linux 和 macOS 共用）
  src/plat_win.c       平台层的 Windows 实现（宽字符 API、UTF-8 边界、创建进程、Job Object、Winsock）
  src/nexec_*.c        执行器 nano-exec：每次工具调用启动一次，套上 Landlock + seccomp 再执行
  src/sandbox.c        Landlock / seccomp（Linux）
  src/sandbox_win.c    Windows 上沙箱为空（landlock=0，走无沙箱规则）
  tests/               test_core.c（状态机）、test_exec.py（执行器）、test_exec_win.py（Windows 执行器，WSL interop）、test_host.py（假网关驱动真内核，含多工作目录、代执行）
gateway/               Go 网关
  cmd/nano-gateway     入口；chat.go 是 `nano-gateway chat` 终端客户端；command.go 是 status/start/stop/restart/logs/url/doctor；service.go 后台看护进程和 pid；restart.go 重启 marker；localkernel.go 启动和看着本机内核
  internal/app         调度：会话、窗口（endpoints）、命令、补聊天记录（recap）
    workspaces.go      工作目录列表（data/workspaces.json）、旧设置迁移、/dir
    nodes.go           内核和机器、route()：这一轮由哪个内核跑、每个挂载的工作目录怎么执行、hostFor()
    exec.go            内核交来的 exec：在工作目录那台机器上执行；Skill（读说明、把附带文件放到挂载的机器上）
    kernelhost.go      把装了内核的电脑当作 host：run / ls / skill_put，等 run_result
    workspace.go       会话挂载的工作目录和环境说明（EnvNote）
    compact.go         超限后异步生成摘要，取消、失败裁剪和用量统计
    undo.go            撤回（/undo、「撤回」、平台撤回事件）和不带斜杠的「停」
    models.go          可用的模型：/model、按会话选模型、按模型建客户端、思考强度和历史上限
    persona_cmd.go     /persona：查看、给会话换上自带人格（写会话专属人格文件）、改回默认
    gateway_tools.go   Gateway / GatewayAdmin：查询和修改网关，管理动作走审批
    images.go          图片输入：模型能力、把历史里的图片读成 data URL
  internal/host        网关能直接执行工具的机器：本机（local.go）、SSH 服务器（ssh.go：主连接、
                       探测、上传 / 现编执行器、重连）、连接管理（manager.go，data/connections.json）
  internal/channel     聊天渠道：cli（unix socket）、web、telegram、feishu（含扫码创建）
    stream.go          平台流式显示：后台合并、限频、结束补齐、失败时发送完整文本
  internal/kernel      内核连接（hub，多内核；AllowToken 给网关启动的本机内核）
    pair.go            电脑配对：待确认请求、专用口令（只存 SHA-256）、移除
  internal/skills      技能（放在网关上）：扫描、Skill 工具定义、加载说明和附带文件
  internal/persona     人格文件热加载；presets/*.md 是自带人格（小纳、大肥鱼），go:embed 进程序，开头 --- 包着 name / desc
  internal/session     历史（JSONL 只追加）、会话设置、裁剪
  internal/llm         OpenAI 兼容接口（流式）、mock
  internal/models      模型列表：接口、模型、默认模型（data/models.json，0600，API key 加密）
  internal/media       图片存储（data/uploads，按内容哈希去重，权限 0600）
  internal/config      配置：gateway.toml + data/config.json（网页改的，密钥字段加密），schema 驱动设置页；Check() 给重启前严格校验
  internal/control     本机控制 socket（run/control.sock，0600）：status 查询
  internal/version     编译时版本号（make 用 -ldflags 写入）
  internal/web         网页接口 + 嵌入的前端（dist/）
  web/                 前端源码：Svelte 5 + Vite（make web 构建到 internal/web/dist）
scripts/               install-gateway.sh（安装到用户目录，支持源码和离线包）、install-kernel.sh（Linux/macOS 内核安装，离线包优先，make installer 打成单文件）、install-kernel.ps1（Windows 离线安装）、run-local.sh（make run）、e2e.sh、e2e-win.py（Windows 内核端到端）、test-install-linux.py / test-install-win.py（离线安装测试）、zipdir.py、fetch-zig.sh、fetch-rg.sh
skills/device-connections/  接入和排查的技能（给聊天 agent 看的说明）；同级还有 model-setup、web-search、scheduled-tasks、context-and-history、gateway-settings
docs/                  ARCHITECTURE.md、protocol.md、本文件、DESIGN-agent-gateway-admin.md
```

## 怎么跑、怎么测

```sh
make install      # 日常使用：全部装到 ~/.nano-agent（NANO_HOME 可改），注册后台服务
make run          # 开发：编译，后台起内核、前台起网关（Ctrl-C 停）；打印网页地址，带密码的链接在 data/web.url
make chat         # 另一个终端：终端聊天（nano-gateway chat），随时进出
make test         # C 测试（ASan）+ Go 测试 + e2e（mock 模型，真实内核/执行器/网关）+ 离线安装测试 + macOS 交叉编译检查；有 Windows 内核和 WSL interop 时还跑 Windows 内核 e2e；装了 zig 和 build/tools/rg.exe 时还跑 Windows 执行器测试
make test-win     # 只跑 Windows 执行器测试和 Windows 内核 e2e
make test-install # 离线安装测试和一行安装测试（先 make release VERSION=test；临时 HOME / USERPROFILE；Linux 和 Windows）
make release VERSION=x.y.z  # 本地交叉编译到 dist/：Linux/Windows 内核包、Linux 网关包、SHA256SUMS（不联网发布）
make web          # 改了前端后重新构建（npm run check && build），产物要提交
cd gateway && go test -race ./...
scripts/fetch-zig.sh   # 下载固定版本的 zig 到 build/tools/zig（只用它的 cc，不需要 sudo）
scripts/fetch-rg.sh    # 下载 Windows 的 rg.exe（WSL 里测 Windows 执行器用）
scripts/fetch-rg.sh --all  # 发布包要的 Linux/macOS/Windows rg
make -C kernel windows-exec   # 只交叉编译 Windows 执行器（build/nano-exec.exe）
make -C kernel windows-kernel # 只交叉编译 Windows 内核（build/nano-kernel.exe）
make -C kernel cross-linux    # 交叉编译 Linux 静态内核/执行器（x86_64、arm64）
make -C kernel cross-macos    # 交叉编译 macOS 内核/执行器（aarch64、x86_64；本机不能运行）
make test-macos               # 只做 macOS 交叉编译检查（没有 Mac，运行测试交给 macOS runner）
python3 kernel/tests/test_exec_win.py kernel/build/nano-exec.exe build/tools/rg.exe   # 单独跑 Windows 执行器测试
python3 scripts/e2e-win.py    # 单独跑 Windows 内核端到端（需要网关、Windows 内核、WSL interop）
```

- e2e（scripts/e2e.sh）现在 67 项（测试里 `[kernel] local = false`，自己起 pc、phone 两个内核）：终端聊天、工具、审批、模式、沙箱、发「停」和 Ctrl-C 停下任务、撤回（含历史文件截断）、会话切换、本机内核掉线后别的内核顶上（照样能用工具）、同时挂本机和 SSH 服务器上的目录（假 ssh：没写 workspace 报错、写文件、服务器上只有执行器、技能文件两台机器都放好、同一轮用两边的目录）、终端重连，Gateway 查询 / GatewayAdmin 审批添加工作目录，以及超限摘要、保存和继续用工具。SSH 工作目录的 `~/proj` 现在会存成服务器给的规范绝对路径。
- Windows 内核 e2e（scripts/e2e-win.py）现在 17 项：WSL 里的网关监听 TCP，网关自己启动的本机内核（AllowToken，local）和通过 interop 启动的 Windows 内核（127.0.0.1 TCP，必须是远程）同时在线；添加 `C:\…\工作 目录` 后路径按 Windows 内核 `--ls` 的规范形式保存；Linux 内核这一轮通过 `run` 在 Windows 内核上执行 Read；只挂 Windows 目录时由 Windows 内核本地执行 Read、Shell（无沙箱要审批）、CRLF Edit、用 `/` 的 Write、Grep、Skill 文件下发；中文路径、空格、PowerShell 输出和工作目录都覆盖。临时 `USERPROFILE`、临时 Windows 目录，结束后清理。
- Windows 执行器（test_exec_win.py）现在 13 项，在 WSL 里通过 interop 跑 `nano-exec.exe`，测试目录和 USERPROFILE 都放在 Windows 临时目录：probe、中文路径和内容、Read/Write/Edit、CRLF 保留、工作目录外拒绝、ro=/ 读所有盘、PowerShell 中文输出和退出码、密钥不泄漏、超时连同子进程杀掉、取消、自带 rg、根目录盘符和 `--ls`、错误请求。没有 zig 时 `make test` 会跳过这一组并提示。
- 离线安装（scripts/test-install-linux.py / test-install-win.py）各 4 项：Linux 用 `make release` 的包在临时 HOME 里装、启动后配对、升级保留 `paired.token`、卸载；Windows 用临时 USERPROFILE 和真实 `nano-kernel.exe` 做同样四步（`-NoService`）。Windows 的“注册任务计划程序”在 WSL interop 下被系统拒绝（Access is denied），安装脚本有 `schtasks` 兜底，但这一步没有在真实桌面会话里实测；`-NoService` 的安装/配对/升级/卸载是实测的。
- macOS：`make -C kernel cross-macos` 交叉编译出 aarch64/x86_64 的 `nano-kernel` 和 `nano-exec`（Mach-O），`make release` 出两个 macOS 包；**没有在真机上运行过**。macOS 上第一版没有沙箱（landlock=0，Shell 在自动模式下要审批）；daemon 用 launchd 用户 agent（`~/Library/LaunchAgents/com.nano-agent.kernel.plist`），卸载时删除。运行测试需要在 macOS 机器或 GitHub Actions 的 macos runner 上跑（这次不建仓库、不推送，所以没跑）。
- host 包：假 ssh 测部署 / 执行 / 断线 / 架构不同时现编，有 sshd 时起一个临时的真 OpenSSH 测一遍。kernel 包测配对。
- 安装脚本没有自动测试，改了之后手动测：临时 HOME + PATH 里放一个总是失败的假 systemctl（走后台进程分支，不碰真的 systemd），装 → 配对 → 再装（升级）→ --uninstall。
- 网页的界面检查用 headless Chromium + CDP 脚本截图（在会话的 scratchpad 里，不在仓库里）。演示环境用 mock 模型，别连真模型、别动用户的 data/。
- 本机是 Fedora 43（WSL），glibc 2.42，**不能静态链接**（没装 glibc-static），所以 `nano-exec-static` 不会生成。

## 网关查看和控制（2026-10-22）

- `nano-gateway` 子命令：`chat`、`status`、`start`、`stop`、`restart`、`logs [-f] [-n N]`、`url`、`doctor`、`version`；不带子命令仍然前台运行。`status` 通过 `run/control.sock`（0600，只允许同用户）查询正在运行的网关，CLI 渠道关掉也能用；协议是请求 `{"op":"status"}`、响应 `{"ok":true,"status":...}`。
- 终端/网页/聊天的重启都先过 `config.Check(path)`：按启动顺序重新读 `gateway.toml`、`data/config.json`（含解密）并完整校验；坏配置直接报错，不写 marker、不停网关。成功后写 `data/restart.json`（0600，含发起人 SID/渠道/时间），主循环收到 `restartCh` 后优雅退出并 `syscall.Exec` 自己；新进程启动后读 marker，删除并给发起会话回“网关已重启（用时 N 秒）”。终端客户端会自动重连，CLI 没连上时消息先放在 App 里，等终端重新连上再发。
- 后台模式：`start` 起一个 `supervise` 看护进程（`run/gateway.supervisor.pid`），看护进程起子进程（`run/gateway.pid`），异常退出 3 秒后拉起；`stop` 给看护进程 SIGTERM，由它转发给子进程。self-reexec 的 pid 不变，不会被当成异常退出。有 systemd unit（`NANO_GATEWAY_MODE=systemd`）时 start/stop/restart 走 `systemctl --user`，没有 unit 才用后台模式；旧 `nano-gateway-ctl` 已从安装脚本移除并在升级/卸载时清理。systemd 只用临时 `XDG_CONFIG_HOME`、fake `systemctl` 和单元生成检查过，没有对真实 `nano-gateway.service` 做启停 e2e。
- 网页：`GET /api/overview` 带 `runtime`（版本/启动时间/运行时长/运行方式/待重启数/最近错误），设置页待重启横幅有“重启网关”按钮，重启时显示“重启中”并轮询重连；`POST /api/restart` 先检查配置，失败 400。登录令牌持久化在 `data/web.sessions`（0600），网关重启后网页不用重新登录。
- 聊天：管理员 `/status`（版本、运行时长、运行方式、内核、机器、渠道、默认模型、待重启、最近错误）和 `/restart`（不再额外弹审批；聊天命令本身已有管理员判断）；`/help` 自动列出。
- AI：`Gateway {"what":"status"}` 返回和 CLI `status` 同一套运行概况（版本/模式/运行时长/内核/机器/渠道/待重启/最近错误，另带工作目录）；`Gateway {"what":"logs","lines":N}` 返回最近 N 行脱敏日志（默认 50，最多 500，用 `knownSecrets` 打星）；`GatewayAdmin {"action":"restart"}` 走审批后重启。
- `doctor` 检查配置（严格）、`secret.key`/数据目录权限、网页/内核/电脑接入端口、本机执行器和内核（有 `VERSION` 文件时比版本）、systemd enabled/linger 或后台 pid、WSL 保活；有 `[问题]` 时退出码 1。
- WSL 保活：当时 Windows 任务计划程序在 WSL interop 下 `schtasks /Create` 和 PowerShell `Register-ScheduledTask` 都返回“拒绝访问”（**后来查清：是触发器没限定用户——`/SC ONLOGON`、`New-ScheduledTaskTrigger -AtLogOn` 不带 `-User` 是“任何用户登录时”，要管理员；带 `-User 当前用户` 普通用户就能建，见「内核管理命令」**），所以改用 **Windows 启动文件夹里的隐藏 VBS**：`%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\nano-agent-wsl-keepalive.vbs`，内容用 `wsl.exe -d <发行版> --exec /bin/sh -c "... start --quiet ...; exec /bin/sleep 2147483647"`。在真实 Windows 上从启动文件夹运行过、写 marker 验证过，随后删除；安装脚本在 WSL 里询问或 `--wsl-keepalive`，卸载时删除。不改 `.wslconfig`。
- 测试：`scripts/e2e-control.py` 12 项（临时 `NANO_HOME`/`XDG_CONFIG_HOME`、mock 模型、后台模式）：version/start/status/logs/url/restart/stop、聊天 `/status` `/help` `/restart`（含重启后回话）、网页重启后同一 Cookie 仍有效、坏配置拒绝重启且网关保持运行、`kill -9` 后看护进程拉起。Go 新增 config.Check、pid 文件/进程判断、Gateway status 字段、Gateway logs 脱敏、GatewayAdmin restart 的测试。

## 自带人格（2026-10-23）

- `gateway/internal/persona/presets/`：`xiaona.md`（和 `personas/default.md` 正文一样，测试会比对，改一份要同步另一份）、`dafeiyu.md`（大肥鱼：网友给 DeepSeek 画的萌化形象，蓝色鲸鱼女仆，爱吃白饭、嘴上想摸鱼、带点傲娇）。编进程序跟着程序更新，不放进 `$NANO_HOME/personas`（安装脚本只在人格目录为空时才拷，老用户拿不到新文件）。
- 套用 = 把预设正文写进 `default.md` 或会话专属人格文件，之后照常编辑。网页「人格」页左边「自带人格」点一下填进编辑框（保存才生效）；`GET /api/persona-presets`。聊天 `/persona [编号 | 名字 | default]`（切换要管理员），写 / 删 `<dir>/<会话ID>.md`。`/persona` 和网页标题会说明当前人格原样是哪个预设（`MatchPreset`）。
- 每个预设都要带「消息格式」和「工具」两节（时间戳、群聊昵称、[系统] 行、不编造工具结果），`presets_test.go` 检查。大肥鱼的人设里专门写了“偷吃可以、偷工不行”：梗里的摸鱼、半成品交付、“我去吃饭了你测完告诉我”、偷偷写小游戏都只当口头玩笑，干活规矩不能破；被问底层模型时不确定就说不确定。
- 资料来源：机核《对 DeepSeek 大肥鱼现象的终极解释》、腾讯新闻（2026-08-11、2026-08-21）关于“蓝色大肥鱼”的报道。

## 内核管理命令（2026-10-23）

- `nano-kernel` 自己有子命令（`kernel/src/ctl.c`）：`status`、`start`、`stop`、`restart`、`connect 地址 [--name]`、`logs [-f] [-n]`、`version`、`update`、`uninstall`、`help`；`-c` 指定配置，默认是程序旁边的 kernel.conf。不带参数且当前目录没有 kernel.conf 时显示用法（不会误启动第二个内核）。
- 运行中的内核在配置文件旁边写 `<配置名>.pid` 和 `<配置名>.status.json`（kernel.conf → kernel.pid；同一个目录可以有好几份配置）（connecting / connected / pairing / denied / rejected，带确认码、出错原因、版本、沙箱），退出时删掉；同一份配置已经有内核在跑时拒绝启动。Windows 上 `stop` 写 `kernel.stop` 文件、内核每秒看一次、正常退出（退出码 0，run-kernel.ps1 不再拉起）；POSIX 用 SIGTERM；启动时清掉残留的 stop 文件。
- 安装脚本写 `service`（systemd / launchd / task:任务名 / startup:名字 / none），`nano-kernel` 按它决定 start / stop 怎么做（旧安装没有这个文件时按服务文件猜）。`$NANO_HOME/bin/nano-kernel` 链接到内核。`nano-kernel-ctl` 已去掉（0.2.1 时还是转给 `nano-kernel` 的壳）：安装脚本删掉旧的壳和链接，把 `.profile` 里带标记的旧启动行换成 `nano-kernel`。Windows 把内核目录加进当前用户 PATH（`-NoPath` 不加，测试用），卸载时去掉。
- 帮助：`nano-kernel help [命令]`、`nano-kernel 命令 --help`（不读配置、不执行）；status/start/stop/restart/uninstall 遇到多余参数报错（以前 `start --help` 会真的启动）；`update` 只接受 `--version`。`nano-gateway help [命令]`、`--help`、`-h`、`nano-gateway 命令 --help`，命令表在 `cmd/nano-gateway/help.go`，`help_test.go` 检查 main 里每个子命令都在帮助里（supervise、check-config 是内部用的）。
- **Windows 开机自启（v0.2.1 及以前在普通用户下注册不了）**：`New-ScheduledTaskTrigger -AtLogOn` 不带 `-User`、`schtasks /SC ONLOGON` 是“任何用户登录时”，非管理员一律“拒绝访问”；CI 的 Windows runner 是管理员，本机测试又一直 `-NoService`，所以没测出来。用户那台 Windows 就是这样：任务没建成，安装脚本却说“内核会在后台一直重试”，其实内核根本没启动。现在：触发器和 principal 都限定当前用户（`WindowsIdentity.GetCurrent().Name`，Interactive、Limited）；还建不了就在「启动」文件夹放快捷方式（`<任务名>.lnk`，powershell 最小化 + `-WindowStyle Hidden` 跑 run-kernel.ps1；`-NoTask` 直接用这种；`NANO_STARTUP_DIR` 给测试换目录）；启动一律 `nano-kernel start --quiet`（任务计划就 `schtasks /Run`，失败或启动文件夹模式直接拉起 run-kernel.ps1），起不来直接报错；等不到网关时检查内核进程还在不在，再打印 `nano-kernel status`。`nano-kernel status` 在记着任务计划、任务却不存在时提醒；`start` 这时直接拉起。`test-install-win.py`：「启动」文件夹用例本机也跑；`NANO_TEST_SERVICE=local` 在本机普通用户下真的注册任务（测试任务名，测完删，不碰 PATH），`=1` 是 CI 用的（也查 PATH）。
- **Windows run-kernel.ps1 的坑**（v0.2.0 及以前都有）：① `Start-Process -PassThru` 在没有控制台时（被后台进程拉起）拿不到可等待的句柄，`WaitForExit` 立刻返回，配合 `SilentlyContinue` 看不出错，runner 会再拉一个内核；② 有句柄时也要先取 `.Handle`，否则 `ExitCode` 为空；③ 不带 BOM 的脚本里有中文，PowerShell 5.1 按 GBK 读，注释吃掉换行、下一行代码失效。现在 runner 用 `System.Diagnostics.Process` 启动内核、内核自己 `--log` 追加写日志、脚本只有 ASCII 并带 BOM 写出。
- 用户的 Windows 机器上用 0.2.1 重装过一次，任务计划还是“拒绝访问”（上面的原因），内核没有运行；**v0.2.2 已发布并修好这一条**，重新运行一行安装（Windows 用 `install.ps1`）即可，配置和配对保留。

## 更新和卸载（2026-10-23）

- 下载和校验：`internal/release`（`Latest` 读 `latest/download/VERSION`，`Fetch` 按 `SHA256SUMS` 校验、缓存在 `$NANO_HOME/cache/releases/<版本>/`，`Unpack` 读 tar.gz / zip 去掉顶层目录）。下载地址：配置 `[update] url` → `NANO_DOWNLOAD_URL` → GitHub。
- 网关自更新（`cmd/nano-gateway/update.go`）：只换 `$NANO_HOME/bin/nano-gateway` 装的（源码目录里的拒绝）。下载网关包 → 用新程序跑 `version` 和 `check-config`（读不懂现在的配置就不更新）→ 备份到 `backup/<旧版本>-<时间>/`（程序、lib、gateway.toml、data/*.json，只留 3 份）→ 改名覆盖 bin/lib（正在运行的旧程序不受影响）→ 自带人格和技能按 `.shipped.sha256` 只覆盖没改过的，改过的写 `.new` → 写 `update.json` → 重启。新进程一启动就跑 `updateGuard`（在读配置之前）：启动次数超过 3 次就把备份放回去、re-exec 旧版本；正常运行 20 秒后删 `update.json` 并在发起的会话里回话；退回的也回话。终端 `nano-gateway update` 重启后等结果（看 update.json 和控制 socket 报的版本）。
- 入口：`nano-gateway update [--check] [--version x.y.z] [--yes]`、聊天 `/update`（管理员）、GatewayAdmin `update`（审批）、网页设置页（`/api/update`、`/api/update/check`）。每天查一次新版本只提示（`[update] check`）。`main.testFailStart` 只在测试构建里用 -ldflags 设置，模拟新版本启动失败。
- 内核自动同版本（`app/update.go` 的 `maybeUpdateKernel`）：内核 hello 的 `version` 和网关不同、网关是发布版本、不是网关自己启动的本机内核、`update_failed` 不是这个版本、这次运行没推过，就从 Release 取那个系统和架构的内核包，推 `update`。`[update] kernels = false` 关掉。
- 卸载：`nano-gateway uninstall [--keep-data] [--yes]`：停 systemd 或后台进程（连看护进程）、删服务文件、WSL 保活脚本、`.profile` 自启行；同一个 NANO_HOME 里有 `kernel/` 时只删网关的部分、PATH 行留着。`install-gateway.sh --uninstall` 交给它（旧版本程序没有这个命令时用脚本里的逻辑，也保留内核）。内核：安装时把安装脚本留在内核目录里，`nano-kernel version|update|uninstall`；Windows 同样留一份 `install-kernel.ps1`。
- 测试：`scripts/test-update.py`（本地编出 0.0.1/0.0.2/0.0.3，0.0.3 启动必失败：更新、退回、内核自动升级、两种卸载）、`app/update_test.go`、`release/release_test.go`；`test-install-win.py` 加了 Windows 内核自更新（exe 改名、退出码 75、run-kernel.ps1 拉起）和用留下的脚本卸载。

## 分支（2026-10-23 起）

- `main` 是稳定分支：只放发布过的版本和小修。大版本在 `dev/<版本>` 分支上开发（v0.3 → `dev/v0.3`），做完、用户同意后 `--no-ff` 合并回 main，再在 main 上打标签发布。main 上的小修合并进开发分支，不 rebase 已推送的开发分支。
- CI 在 `main` 和 `dev/**` 上都跑。`release.yml` 先有一个 `guard` 任务：标签指向的提交不在 main 上就拒绝发布，防止开发版变成“最新版本”被所有机器自动更新上去。
- 详见 [PLAN-roadmap.md](PLAN-roadmap.md) 的「分支」。

## 发布、一行安装、内核自我更新、搬家（2026-10-22）

- 仓库公开在 https://github.com/tofu-zyc/nano-agent （Apache-2.0）。提交身份是 `tofu-zyc <286995380+tofu-zyc@users.noreply.github.com>`（仓库本地 git config 已设，历史改写过；改写前的历史留在本地 ref `refs/backup/before-email-rewrite`，不推送）。**第一次推送之后不再改写历史、不强推。**
- 推送前：`scripts/fetch-gitleaks.sh` 后 `build/tools/gitleaks git --no-banner .`（`.gitleaks.toml` 放过脱敏测试里的假密钥）；用户本地的真实密钥（data/ 里加密保存的）要解密后在内存里比对历史，不能打印。
- CI（`.github/workflows/ci.yml`）：Linux 跑 `make test`；Windows runner 原生跑 `test_exec_win.py` 和 `test-install-win.py`（`NANO_TEST_SERVICE=1` 时测任务计划程序）；macOS runner 跑内核测试、Go 测试和 e2e。发布（`release.yml`）：推 `v1.2.3` 标签 → 先跑 CI → `make release VERSION=1.2.3` → `gh release create`，附件是各平台包、`install.sh`、`install.ps1`、`VERSION`、`SHA256SUMS`。
- 一行安装：`scripts/install.sh`（Linux 网关 / Linux、macOS 内核；`--kernel`、`--import`、`--old-local-as`、`--version`）和 `scripts/install.ps1`（Windows 内核；字符串只用 ASCII，因为 `irm` 拿到的内容没有编码信息；不用 Get-FileHash / Expand-Archive，有的环境加载不了模块）。从 `$NANO_DOWNLOAD_URL`（默认 GitHub Release）下载 `latest/download/VERSION` 和 `download/v<版本>/` 下的包，按 `SHA256SUMS` 校验，再运行包里现有的安装脚本。`scripts/test-oneliner.py` 用本地 HTTP 服务模拟 Release 测试（含 WSL 里调 Windows 的 install.ps1）。
- 内核版本：`kernel/build/version.h`（`VERSION` 变了才重写），`nano-kernel --version`，`hello` 带 `version`。macOS 上直接 `make` 自动用 macOS 平台层。
- 内核自我更新（`kernel/src/update.c`，协议 v9 的 `update` / `update_result`，见 protocol.md）：新程序先写成 `.new`，旧的改名 `.old`，回报后重启自己（POSIX exec 安装目录里的 `nano-kernel`——不能用 /proc/self/exe，改名后它指向 `.old`；Windows 以 75 退出，`run-kernel.ps1` 现在是循环，75 立即拉起、崩溃 5 秒后拉起、0 退出）。90 秒内没被网关接受或启动超过 3 次就退回，`hello` 带 `update_failed`。**网关这边还没有推送更新的逻辑**（下一步）。
- CI 真机暴露并修好的：macOS 编译要 `_DARWIN_C_SOURCE`；macOS 默认临时目录太长（unix socket 超 104 字节，CI 里设 TMPDIR=/tmp）；没有沙箱的机器上 Shell 要审批（宿主测试自动批准）；Windows runner 的 TEMP 是 8.3 短名（执行器规范成长名，测试用 realpath）；PowerShell 冷启动慢；飞书第一次写得晚时建卡片超过 30 KB（改成正文先占位再更新）。
- 用真实 Release 实测发现：导入后 `listen = "unix:run/gateway.sock"` 在很深的 NANO_HOME 下超长、网关起不来；现在配置里的 unix 路径太长时自动换成 `$XDG_RUNTIME_DIR`（或 /tmp/nano-agent-UID）下按哈希命名的短路径（`paths.ShortSocket`）。v0.1.0 里还有这个问题，正常的 `~/.nano-agent` 路径不受影响。
- 已知偶发失败：e2e 的「HOME 会话 Shell 不能写 HOME（没等到：Permission denied）」在 CI 上偶尔超时，同一提交重跑就过，还没查原因。
- 搬家：`nano-gateway export` 打包 gateway.toml（数据/人格/技能目录和 unix socket 改成相对位置）、data/（不含日志、web.url、local-kernel.conf）、secret.key、personas/、skills/，0600；`nano-gateway import <包> [--force] [--old-local-as 名字]`，已有数据时要 `--force`（先备份到 backup/import-时间/），导入后列出不存在的“本机”工作目录和要改连地址的配对电脑。`install-gateway.sh --import` 在生成新配置之前导入。

## 已经完成的（按提交）

| 提交 | 内容 |
|---|---|
| 6a66c7a | 初版：C 内核 + Go 网关 + 协议 |
| c06f2a9 | 内置工具 Read/Write/Edit/Grep/Shell；沙箱执行器（Landlock + seccomp）；权限模式 auto/ask/readonly/none |
| c7c66df | 流式输出、思考强度（只传 OpenAI 风格的 reasoning_effort）、前缀缓存优先的上下文 |
| d90b992 | 网页管理界面：拓扑图、点节点改设置、Ctrl+K、审批、人格 |
| 9f4e5ab | 目录浏览、飞书扫码创建机器人、没有工作目录就只聊天、默认代理、只支持私聊、拖动节点 |
| eddd6dd / e1c9670 | 会话和聊天窗口分开：所有窗口能接入同一个会话（主会话 main），/session |
| b08140b | 不是管理员时说明原因；网页里一键设为管理员 |
| e636508 | 切换会话/回来时补上没看到的聊天记录（/history）；网关和终端聊天分开（make chat） |
| bd6fae2 | 多个内核（节点）：会话绑定机器，掉线时替补只聊天，/node |
| 902f8af | SSH 远程执行：服务器作为节点（经由某个内核），自动上传/现编执行器 |
| 8017fb3 | SKILL.md 按需加载、技能热更新、网页列表、SSH 附带文件上传与只读访问 |
| aad4e2a | Telegram 发送后编辑、飞书 CardKit 流式卡片，含思考/工具/用量、长回复分段和失败降级 |
| 096fbec | 超限时合并旧摘要和旧对话，完整保留近期工具链与当前轮；支持取消、失败裁剪、重启恢复和统计 |
| 512529b | 跨端自动同步、默认折叠思考与工具、完整原始历史查询，修复 WSL DrvFS Permission denied |
| 338f527 | Windows WSL2 / Linux 启动包、网页 SSH 密码 / 密钥 / config 接入与断开、自动部署、stdio 通道、接入技能 |
| 62b0dff | 各端都能停（「停」、Ctrl-C、Telegram 按钮、网页输入框旁）；撤回最近一轮（/undo、「撤回」、网页按钮放回输入框、飞书撤回事件）；电脑首次连接配对 |
| 07cdded、c71ce05 | 重写：SSH 服务器由网关自己连、只放执行器（去掉内核侧 remote 和服务器上的内核、启动包）；工作目录 = 机器 + 目录的列表；同机 / 异机两种执行模式（exec / skill_files 协议）；网页工作目录选择器、拓扑按机器画；内核安装脚本和单文件安装包；协议 v4 |
| cc51a6b、2e26cb8、5e67b7d | 网关查看和控制：`status/start/stop/restart/logs/url/doctor/version`、control socket、配置检查后 `syscall.Exec` 自重启和回话、网页重启与版本/运行时长、聊天 `/status` `/restart`、`Gateway status/logs` 和 `GatewayAdmin restart`、后台看护进程、WSL 启动文件夹保活；`scripts/e2e-control.py` 12 项 |
| 多工作目录 | 一个会话挂好几个工作目录（可跨机器），工具参数 workspace；技能移到网关（Skill 由网关处理，附带文件放到各机器的 ~/.nano-agent/skills）；网关代执行别的电脑上的工具（run）；网关启动本机内核；拓扑加内核节点、右键菜单；输入框命令提示；协议 v5 |
| 多模型 | 网页上管理多个 OpenAI 兼容接口和模型：默认模型、会话单独选（/model、会话设置）、每个模型自己的上下文窗口、历史上限、思考强度；接口只回传“有没有设置 key” |
| 图片输入 | 网页粘贴/拖拽/上传、Telegram 照片、飞书图片消息；模型能力（modalities）决定发不发给模型；图片存 data/uploads，历史里只存引用，支持图片的模型收到 OpenAI content parts |
| f9ee514 | 跨平台阶段 1：平台层（POSIX 一份、Windows 一份）+ Windows 执行器 `nano-exec.exe`；宽字符路径、符号链接/junction 解析、CRLF Edit、PowerShell UTF-8、Job Object 超时/取消、自带 rg；zig 交叉编译，WSL interop 实测 |
| 56b1e33 | 跨平台阶段 2：Windows 内核 `nano-kernel.exe`（Winsock TCP、WSAPoll、读线程 + 回环 socket 唤醒、Job Object、runner_win）；协议 v8（hello 的 os/arch、workspace 的 os）；修 127.0.0.1 TCP 被当成本机（只认 unix socket 和 AllowToken）；路径归属（ListDir 规范形式、离线机器上线后换规范路径、网关不解析远程路径）；Windows 目录选择器、机器/拓扑显示系统；`scripts/e2e-win.py` 实测 WSL 网关 + Windows 内核的远程 run、本地执行、PowerShell、CRLF、Grep、技能下发 |
| 2189325 | 跨平台阶段 3：`make release` 本地交叉编译（Linux 静态 x86_64/arm64、Windows、Linux 网关包）和 `SHA256SUMS`；`fetch-rg.sh --all`；`install-kernel.sh` 离线包模式（不编译）；`install-kernel.ps1`（离线安装、配置、配对、任务计划程序、升级、卸载）；`install-gateway.sh` 支持离线包；离线安装测试 |
| 8bf1bac | 跨平台阶段 4：macOS 平台层（`_NSGetExecutablePath`、无沙箱 `sandbox_stub`、`nexec_main` 去掉 Linux `sys/syscall.h` 依赖、`_DARWIN_C_SOURCE` 解决 `DT_DIR`）；`cross-macos` 交叉编译 aarch64/x86_64；`make release` 出 macOS 包；`install-kernel.sh` 的 launchd 用户 agent（启动、卸载）和 `nano-kernel-ctl` 的 macOS 支持。没有真机测试 |
| 262d063、aacfe97、61e2c51 | MCP（路线图阶段 11 的前两块）：`internal/mcp` 客户端（本机 stdio / 远程 streamable HTTP、懒连接、退避重连、空闲断开、密钥加密）、`data/mcp.json`；内核把不认识的工具名转给网关，网关按 `<服务>__<工具>` 分发到 MCP 客户端；真子进程端到端测试 |


## MCP（2026-10-25，阶段 11 前两块）

配置在 `~/.nano-agent/data/mcp.json`，网页里网关节点面板的「MCP 服务」能加、改、删、测试，也可以手写：

```json
[
  {
    "name": "fs",
    "command": "npx",
    "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/me"],
    "enabled": true,
    "agents": [],
    "approval": "never"
  },
  {
    "name": "remote",
    "url": "https://example.com/mcp",
    "enabled": true
  }
]
```

- `name` 只能用字母、数字、下划线、减号；它也是工具名的前缀。
- `command`/`args`/`env`/`cwd` 是网关本机起的 stdio 进程；`url` 是远程 streamable HTTP。二选一。
- `machine` 填一台在线内核的名字，这个服务就**跑到那台机器上**（协议 v10：`pipe_open` / `pipe_write` /
  `pipe_data` / `pipe_closed`，见 `kernel/src/pipe.c`）。不填（或写 `gateway`）就在网关本机跑。
  Linux / macOS（`pipe.c`）和 Windows（`pipe_win.c`）都支持；内核协议低于 10 的机器直接报“内核太旧”。
- `env` 里写明文密钥，存盘时会自动加密成 `enc:v1:`（读的时候解）。
- `tools` 是白名单，`"!名字"` 表示禁用；`agents` 空表示所有 agent 都能用。
- `approval`：`never`（直接执行，默认）、`per_turn`（一轮里第一次调用要批，批过就不再问）或
  `always`（每次都要批）。管理员在网页点批准，或者在聊天里发 `/approve 编号`。

协议版本：网关只拒绝**比 `proto.MinVersion`（现在 9，自我更新从 v9 开始才有）还老**的内核。旧一版的内核先放进来，
`maybeUpdateKernel` 才有机会把新版本推过去（以前要求版本完全相等，网关一升级老内核就永远连不上）。

工具以 `<服务>__<工具>` 交给模型，说明里带“（MCP 服务 X）”。调用时内核不认识这个名字，
会转回网关执行（`kernel/src/core.c` 的 `FORWARDED`），所以不需要给内核加动态工具表。
只有管理员能用；访客的工具集里不会有这些。

工具返回的图片存进媒体库：网页的工具卡片显示；模型能看图时也附给模型（和 Read 读到图片一样）。

**锁的规矩（改之前一定先看）**：算工具定义（`contextParts`）是在网关总锁里做的，而连 MCP 服务——尤其
`machine=` 的管道透传——要等内核回话，内核的消息又要过总锁。所以：
- 总锁里只能读缓存（`Manager.CachedTools`），**绝不能去连**；连接由后台的 `Warm` / `Wake` 发起。
- `conn.mu` 只护字段，拿着它不能连接、不能等网络；连接本身由 `conn.dialing` 串行化。
- 管道收发（`app/pipe.go` 的 `PipeWrite` / `PipeClose`、`KernelMsg` 里的 `pipe_data`）不过总锁。
- `mcpCall` 的协程只认开始时的那一轮 `run`：回来时 `s.run` 可能已经是 nil 或下一轮。

**工具快照**：签名里带「这个会话实际会拿到的 MCP 工具名」（`mcpOffered`），服务连上、加了工具、
切换只读模式都会重建；连不上时用上次的工具列表（断开不清空），并记在 `data/mcp-tools.json`，
网关重启后、服务连上之前先用它，前缀不抖。改配置会清掉退避马上重连；某台机器的内核连上来时
（`KernelUp` → `Wake`）立刻去连挂在它上面的服务，断开时（`KernelDown`）把那台机器上的连接作废。

测试：`internal/mcp`（配置、列/调工具、内存传输、真子进程、管道、`lock_test.go` 的锁和缓存）、
`app/mcp_e2e_test.go`、`app/mcp_lock_test.go`（死锁、卡顿、中途停下、审批随这一轮撤掉、
服务后来才连上、切换模式、旧内核）。

两条端到端测试：`scripts/e2e-pipe.py`（假网关 + 真内核，测管道，含网关突然断开时进程被收掉；
`NANO_TEST_KERNEL=` 可以换成 `-fsanitize=address` 编的内核查内存问题）、
`scripts/e2e-mcp-remote.py`（真网关 + 真内核 + 最小的 MCP 服务，测 `machine=` 整条链路）。
## 关键设计（改之前先理解）

1. **前缀缓存优先**：系统提示词和工具定义固定不变；历史只追加；时间、来源（`[2026-10-21 周三 15:08 · Telegram]`）、环境变化（`[系统] 在 X 上工作。工作目录：…`、替补说明）都追加在用户消息前面，**变了才追加**（Settings.EnvNote 记着上次说的）。历史太长时一次裁掉一大段（滞回），之后又只追加。任何会改动前面字节的改动都是缓存回归。
2. **网关是唯一有状态的中心，内核无状态**。会话 = 一段对话（上下文、工作目录、模式、思考强度、人格、机器）；窗口（endpoint）= 终端、某个私聊，接在会话上（data/endpoints.json）。管理员的窗口默认接主会话；别人各自一个会话。
3. **机器、工作目录和执行**（详见 ARCHITECTURE.md）：机器 = 本机（host.LocalName「本机」）、SSH 服务器（host.Manager）、内核所在的电脑（非本机内核，机器名 = 内核名；本机内核都算本机）。工作目录 `{id, host, path, default}` 存 data/workspaces.json；`path` 是那台机器给的规范形式（添加时用 ListDir 的 Path，离线时先原样保存、机器上线后自动换），网关不解析别的机器的路径。本机内核只认 unix socket 或网关自己的 AllowToken——从 127.0.0.1 TCP 连上来的可能是 WSL 转发的 Windows 内核，一律当远程。会话的 Settings.Workspaces / WorkspacesSet（挂载的 ID 列表；没设过就在第一轮落到默认挂载的那些，空列表且 WorkspacesSet 表示没有工作目录（在 HOME 下工作）；旧的 Workspace 字段启动时迁移）。每一轮 `route()`：挑内核（第一个挂载的机器上的 → 别的挂载机器上的 → preferred），每个工作目录：在内核的机器上 local=true，否则 hostFor() 能用就挂上（网关执行），用不了的进 down。input.workspaces 由 route.spec() 生成（同一台机器上挂的目录一起放进 dirs；管理员 ro 为 /，访客 ro 只有技能缓存目录）。没有工作目录的管理员拿到一个 HOME 工作目录（dirs 空、home true、ro /），Shell 在 HOME 下运行，写只到临时目录；带 elevated 的调用在 auto 下也要审批，批准后这一次调用 rw 加 /。内核按工具参数 workspace 找工作目录（全名 / 序号 / 只写机器 / 只写路径），预检的错误通过 na_call.error 直接作为结果，沙箱按调用（na_call.sandbox）。
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
16. **内核安装**（scripts/install-kernel.sh，POSIX sh；Windows 用 scripts/install-kernel.ps1）：优先用离线包里的 `nano-kernel` / `nano-exec` / `rg`（`make release` 生成），没有才用源码：源码来自脚本后面的 base64 payload（make installer 生成 dist/nano-kernel-install.sh），再没有就用脚本所在的源码目录（拷到临时目录编译，不弄脏源码树）。缺 cc/make 时按包管理器装（sudo 前确认）；装到 ~/.nano-agent/kernel/，升级只改 gateway / node 两行；从旧 `~/.local/share/nano-agent/kernel` 升级时把 kernel.conf 和 paired.token 搬过来，不用重新配对；systemd 用户服务（StandardError=append:kernel.log，UMask=0077），没有就 `nano-kernel start` 后台跑 + 可选 ~/.profile 一行（带标记，卸载时删；从旧目录迁移时把旧的启动行路径换成新路径）。Windows 装到 `%USERPROFILE%\.nano-agent\kernel`，任务计划程序登录时启动，`install-kernel.ps1 -NoService` 只装不启动，`-Uninstall` 卸载。之后读 kernel.log：确认码、网关已接受连接、网关报错（名字冲突之类，直接告诉用户）。内核的重连退避在 welcome / pairing 之后才重置，被拒时不会每秒重连。
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

0. v0.2.0 的更新和卸载已经做完（见上面「更新和卸载」）。还没做：网页上每台电脑单独的「更新内核」按钮（现在是连上时自动更新）、macOS 网关包。
1. 卡片重新设计、审批按钮都只用模拟服务测过，没往真实的飞书 / Telegram 发过，等用户实际用了再调样式。用户现有的飞书机器人是之前扫码建的：流式卡片要 cardkit:card:write、撤回同步要订阅 im.message.recalled_v1、按钮要订阅回调 card.action.trigger，需在开放平台后台补上。
2. 飞书卡片按钮要用户在开放平台订阅 card.action.trigger（扫码注册的 addons 里没有回调，没敢猜格式）。聊天里配置网关已经做了，见 [DESIGN-agent-gateway-admin.md](DESIGN-agent-gateway-admin.md)。
3. 图片的清理策略（不用了的图片一直占着 uploads）；音视频输入用户说以后再说（支持音视频的模型还少，也有别的办法）。
4. 同名冲突：远程内核和 SSH 服务器同名时 route 优先按 SSH 服务器算，没有拦；内核起名叫「本机」也没拦（安装脚本拦了）。
5. 长期记忆、群聊。跨平台按 [PLAN-cross-platform.md](PLAN-cross-platform.md) 做：阶段 1 Windows 执行器已完成；阶段 2 Windows 内核 + 网关路径/回环修复已完成；阶段 3 本地离线安装包已完成（Linux/Windows）；阶段 4 macOS 已完成交叉编译和 launchd 支持，但**没有真机测试**（本机没有 Mac）。移动端暂不做，将来要求在手机本机执行工具。

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
- macOS 交叉编译（zig）：`-target aarch64-macos` / `x86_64-macos`，必须加 `-D_DARWIN_C_SOURCE`，否则 `dirent.h` 的 `DT_DIR` 等不出现；`nexec_main.c` 的 `sys/syscall.h` 只在非 macOS 包含；macOS 用 `sandbox_stub.c`（landlock=0）。`zig cc` 的 macOS 产物只能编译检查，不能在本机运行。
- macOS 安装脚本用 launchd 用户 agent（`~/Library/LaunchAgents/com.nano-agent.kernel.plist`），（以前的）`nano-kernel-ctl` 壳脚本不能用 `readlink -f` 和 `/proc`，要用 `readlink` 循环解析符号链接、`ps -p … -o comm=` 确认进程。`install-kernel.sh` 默认在 GUI 登录会话里 `launchctl load -w`；SSH 会话里可能需要先登录桌面。
- `make test` 在没有 Mac 的机器上只做 macOS 交叉编译检查，不运行 macOS 二进制。
- 在 WSL 的 9p 上覆盖一个马上要运行的 `.exe` 偶尔会拿到旧内容；测试每次复制到新的临时目录，不要复用同一个 exe 路径。
