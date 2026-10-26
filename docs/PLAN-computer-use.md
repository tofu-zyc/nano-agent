# Computer use 接入方案

## linux-pc 实测与主线补全（2026-10-26，未发布）

- 同桌面从“单次调用串行”改为“整轮观察、操作、核验保留桌面”；其他任务等待，取消/结束/断线释放。同一轮跨两台已占用的桌面不互相死等，返回可解释的重试原因。
- 新增 `scripts/e2e-desktop.py`：在真实 X11 服务中启动独立 GTK 输入窗口，验证实际 PNG 画面非纯色、通过驱动 `set_value` 写入完整中英文、点击确认、核对应用进程输出。可用 `--isolated-x11` 配合 Xvfb / Openbox / xcompmgr，Python 需要 GI 的 GTK3 和 Pillow；临时桌面与正式网关隔离。
- linux-pc 上 Driver **0.7.0 和 0.37.0** 均通过上述操作链。0.37.0 需按实际 schema 用 `tree_format:"both"` 取得结构化元素；优先 `element_token`，不假定旧版 `element_index` 仍存在。
- 可加 `--gateway /path/nano-gateway --kernel /path/nano-kernel --executor /path/nano-exec` 走真网关 + C 内核 + 自主日程 + MCP 操作链，应用输出作为验收证据；模型仍是确定性本地替身。该组合已在 linux-pc、Driver 0.37.0 上实际通过，包括 MCP 审批、观察、截图以多模态消息送到模型接口、中英文输入、点击、读取应用输出、`Agenda done` 验收。
- 严格验收中发现：0.7.0 / 0.37.0 的 `type_text` 在该 X11 环境写入混合中英文时可能只写部分，效果报告为未确认。`set_value` 路径完整回读通过；技能已记录选择方法，网关不允许明确未确认的结果直接用于日程验收。
- 当前登录的 GNOME 远程会话没有显示输出，XWayland 根屏幕为 **0×0**，只读到窗口树不能算截图/输入通过。Xvfb 无合成器时透明光标可能导致黑图；测试现在检查画面内容，不能把“有 PNG”当成功。

- Windows 实机补测：新增 `scripts/fixtures/desktop-win.c` 和 `scripts/e2e-desktop-win.py`。独立 Win32 窗口具有输入框、确定按钮和状态文字，只有点击确定后才从实际 Edit 控件读取内容并写出 UTF-8 文件；窗口三分钟自动退出。内核程序副本放在另一个临时目录，避免把包含内核程序的目录设为工作目录。
- linux-pc 上真实网关通过 SSH 临时转发连接独立 Windows C 内核，使用 `pipe_*` 启动已安装的 Driver **0.37.0**。到点自主任务经审批观察/截图、`set_value` 写入「中文验证：你好 nano 42」、再次观察、点击确定、用 Windows 执行器 Read 实际输出，再提交日程证据验收。6 次模型请求含截图；测试端另核对非纯色画面和文件内容，肉眼核对控件值与成功状态。正式 Linux/Windows 内核和原有 Driver 进程未替换。

- 在可重复的确定性回归外，Windows 同一链路还用 `deepseek-v4.1-flash` 实测通过：模型自己创建已到期日程，在自主活动中按最新窗口状态选择元素、输入、点击，再关联 Read 凭据验收。它接收包含辅助功能树和截图的观察；这不是纯视觉定位或任意桌面任务的可靠性评估。

原生 Wayland 已补测 Sway 1.11 / wlroots 0.19：独立 headless/pixman 会话禁用 XWayland，GTK 窗口经 Sway IPC 确认 shell=xdg_shell、window=null；Driver 0.37.0 的非纯色截图、完整中英文、点击及真实应用输出通过，真网关/C 内核/自主日程/MCP 审批/证据验收组合也通过。用 `--isolated-wayland --wayland-compositor /path/to/sway` 复跑，依赖须事先具备。只在临时目录解包测试依赖，没有安装系统软件或更改正式 GNOME。

不能将该结果推广到其他 Wayland 合成器。当前 GNOME 没有显示输出，Mac 没有可用实机，用户确认先完成 Windows/Linux。多窗口、纯视觉坐标、复杂滚动和长期稳定性可继续扩展评估。

使用方法已经放进自带 `computer-use` 技能及 `references/setup.md` / `references/linux.md`，接入 AI 日常复用已有服务，不用从验证脚本反推操作方法；自主任务由 `autonomous-tasks` 技能串起排期、检查点和验收。

### 复跑 Windows 跨机链路

先构建 Windows 内核/执行器（`make -C kernel windows-kernel windows-exec`），将待测 Linux 网关放到 SSH 目标机，在 WSL 运行：

```sh
python3 scripts/e2e-desktop-win.py --ssh linux-pc --gateway /absolute/path/nano-gateway \
  --driver 'C:/Users/me/AppData/Local/Programs/Cua/cua-driver/bin/cua-driver.exe'
```

需要已登录的 Windows 桌面、预先安装并可用的 Driver、Zig 和 Python Pillow。测试只操作自有夹具，经 `mcp --grant existing-profile` 使用现有配置，不安装驱动或修改系统授权。`--kernel` / `--executor` 可指定 Linux 目标机的程序，默认该账号 `~/.nano-agent/lib/`；不安装到正式目录。测试失败必须保留失败状态，不能将跳过算通过。

加 `--real-model-home /home/me/.nano-agent --output build/verification-windows-real` 使用 Linux 目标机已配置的默认真实模型（必须支持图片，会消费 API 用量）。测试只选取该模型所需的加密端点和密钥放进私有临时目录，正常结束或 SIGTERM 中断后删除副本；不复制生产聊天、机器人或 MCP 配置。

日期：2026-10-26。状态：v0.7.3 已接入，并在 Linux（WSL Fedora 43 + cua-driver 0.7.0）完成真机握手和窗口树验证；截图/输入在该 WSLg 会话里受限，Windows/macOS 实机未验证。

目标是让 nano-agent 操作有桌面环境的机器，覆盖 Windows、macOS 和 Linux。优先采用 Cua Driver，复用其窗口发现、截图、辅助功能树、输入和结果验证；nano-agent 负责模型循环、设备连接、会话、工具授权和上下文。

## 接入结构

```text
视觉模型 ↔ nano-agent Gateway ↔ 目标机器 nano-kernel ↔ cua-driver mcp ↔ 桌面应用
                              现有 pipe_* 透传          stdio MCP
```

网关可以运行在无桌面的服务器上。Driver 运行在目标机器的图形登录会话里。第一版通过外部程序接入，不把平台驱动编入 C 内核，也不接管 Cua 的 Agent 循环。

## 当前代码可复用的部分

- `gateway/internal/mcp/client.go`：按 `Server.Machine` 选择 `PipeTransport`，已有本机 stdio、远端内核透传和 HTTP 接口。
- `gateway/internal/app/pipe.go`、`gateway/internal/mcp/pipe.go`：协议 v10 的 `pipe_open/write/data/close/closed`。
- `kernel/src/pipe.c`、`kernel/src/pipe_win.c`：启动并管理长连接 MCP 子进程。
- `gateway/internal/app/mcp.go`：工具发现、审批、取消，以及 MCP 图片保存和关联工具结果。
- `gateway/internal/session/context.go`：工具图片转换为模型可接收的多模态消息。
- `gateway/internal/app/images.go`：通过模型配置的 `modalities` 判断是否支持图片。

单个服务的配置示例（节点名和绝对路径均须替换）：

```json
{
  "name": "desktop",
  "machine": "desktop-linux",
  "command": "/absolute/path/to/cua-driver",
  "args": ["mcp"],
  "enabled": true,
  "approval": "per_turn"
}
```

Windows 使用实际的 `.exe` 路径。macOS 遵循 CuaDriver.app 和代理进程的权限身份约定，不能假设任意后台进程直接启动就继承录屏与辅助功能权限。仅选择了支持图片的模型还不代表其桌面定位可靠，需要任务验证。

## nano-agent 需要补充的适配

1. **完整保留结果。** `mcp.ResultText()` 当前只要 `content` 产生文字就提前返回，可能忽略同时存在的 `structuredContent`。保留 Driver 的动作效果、拒绝原因、下一步建议及截图关联信息；避免把“工具调用正常返回”当作操作效果已验证。
2. **会话对应与清理。** 当前 MCP 连接按服务名共享。为 nano-agent 会话分配稳定的 Driver `session`，停止和结束时清理对应资源。标签不等于调用者身份；需要强隔离时使用独立连接。复用 Driver 已有的输入协调和坐标规则。
3. **生命周期。** 当前 Manager 默认空闲十分钟断开；Linux/Windows 的直接 MCP runtime 与连接同寿命。明确哪些状态可以恢复，哪些需要重新读取窗口、重新授权。不要重放结果不明的点击或输入。
4. **操作指引。** 当前主要读取工具列表，尚未把初始化 `instructions` 接入上下文。提供匹配所固定 Driver 版本的操作技能，或按需读取其 `get_guide`。处理本项目工具名前缀。
5. **截图预算。** 当前压缩可以省略旧轮次的工具图片，同一长任务中的截图仍会累积。保留原始历史，在模型请求视图中限制旧截图，保留必要的状态摘要及最近帧；同时控制图片尺寸和传输体积。
6. **部署与诊断。** 显示目标节点、图形会话、Driver 版本、授权情况及实际可用能力。设备在线不等于桌面可操作。

## 跨平台验证范围

以具体版本和桌面环境的实测结果声明支持，不把 Linux 当作单一后端。Cua 当前提供 X11、Wayland 的不同路径；GNOME、Sway、KDE、Hyprland 的覆盖和后台输入限制不同。WSL 的 Linux 图形会话与 Windows 宿主桌面分别验证。

第一轮验收：发现窗口 → 截图/读取界面 → 点击、滚动、中文输入 → 读取结果验证；再检查多窗口、缩放、取消、断线恢复和多会话交替操作。至少验证 Windows 与一个原生 Linux 桌面；macOS 在有真实图形会话的环境验证。

## 实施顺序

1. 固定 Cua Driver 版本，在目标桌面完成 MCP 握手和只读窗口/图片联调。
2. 跑通一个可逆任务的观察、操作、验证闭环。
3. 修正结构化结果、会话生命周期与截图预算，再加入安装和诊断入口。
4. 按真实平台扩展验收矩阵；只有 Driver 确实缺失且项目需要的能力再做补充。

## 参考

- [Cua Driver 源码](https://github.com/trycua/cua/tree/main/libs/cua-driver)
- [MCP 实现核对版本](https://github.com/trycua/cua/blob/9a3c33ca14c79dcf828033402c706ec9bb97a9f1/libs/cua-driver/rust/crates/cua-driver-core/src/mcp_wire.rs)
- [工具与会话约定](https://cua.ai/docs/cua-driver/reference/mcp-tools)
- [平台支持范围](https://cua.ai/docs/cua-driver/concepts/platform-support)

v0.7.3 已经接入 Cua Driver，并做了 Linux 真机 MCP 握手/窗口树验证；Windows/macOS 仍未实机验证，具体见下面的实施结果。

## v0.7.5：MCP 工具不能被内核的 workspace 预检挡住

MCP 工具名是 `<服务>__<工具>`，v0.7.4 及以前内核的审批预检只认写死的网关工具名单，导致多工作目录会话先报“用 workspace 参数”；工具 schema 又没有 workspace，模型无解。v0.7.5 在内核预检和执行入口都识别 `__`，直接交给网关；网关的 MCP 进程由 `pipe_open`/网关直接 `exec`，不经过 `nano-exec` 的 Landlock/seccomp。Shell 的沙箱报错与 MCP 无关，不能用 Shell 里的 Unix socket 失败推断 Driver 坏了。

## v0.7.4：安装和配置也写进技能（AI 来做）

`skills/computer-use/SKILL.md` 增加了一节“安装和配置（AI 自己来，不要一上来让用户操作）”，把 Linux/macOS/Windows 的官方安装命令、依赖、图形会话/daemon、系统权限边界、`GatewayAdmin mcp_save` 配置、`mcp_test` 和诊断串成步骤。原则：

- AI 先确认目标机器、图形会话和用户同意，再下载/执行第三方安装脚本；不静默 `sudo`。
- Linux 无图形会话时可在桌面会话起 `cua-driver serve`，MCP 用 `--socket` 连（以当前版本 `--help` 为准）；Windows/macOS 的系统权限仍需用户/MDM 确认。
- 装完必须 `doctor`/`list_windows` 真正看见窗口，才能说“桌面接好了”。
- `skills/mcp/SKILL.md` 加了指针；v0.7.4 同时补发 Windows 网关包（可直接在 Windows 上跑网关和本机内核，但 Windows 上 CUA Driver 仍未实机验证）。

## 实施结果（v0.7.3，2026-10-26）

**已实现**

- 结构化结果：`mcp.ResultText` 同时保留 `content` 文字和 `structuredContent`（动作效果、拒绝原因、下一步建议、截图关联），不再“有文字就提前返回”。
- Driver session：每个 nano-agent 会话稳定映射成 `na-<12 hex>`；schema 里有 `session` 的桌面工具自动带上；同一台桌面上的桌面调用用闸门串行；Reset / Stop / Delete 会话、切换 agent、删除 agent 时 best-effort 调用 `end_session`。连接沿用现有 MCP（本机 stdio、HTTP、远端内核 pipe），不把驱动编进 C 内核。
- 上下文与指引：握手时保存 initialize `instructions` 和 `serverInfo.version`；管理员可用的桌面 MCP 会话把版本相关说明接进系统提示；新增 `skills/computer-use/SKILL.md`（观察-操作-再观察、结果未知不重放、平台差异、诊断）。
- 截图预算：一次工具结果最多两张图交给模型；模型请求里只保留最近两个带图的工具结果，旧图换成“需要时重新截图”的文字（历史原样保留）；MCP 结果文字截到 24k 字符。
- 诊断：`MCPInfo` 增加 `machine_online` / `desktop` / `driver_version` / `instructions` / `last_error`；MCP 节点面板显示驱动版本、目标电脑在线、最近错误和 initialize 说明，并明确“设备在线不等于桌面可操作”。

**实际验证（Fedora Linux 43 WSL，cua-driver 0.7.0，X11/Weston）**

- 真机通过：MCP 握手（43 个工具）、initialize instructions、`serverInfo.version=0.7.0`；`list_windows` 结构化结果；`start_session` / `end_session`；`health_report`（X11 可达、AT-SPI 可用、屏幕捕获自检 pass）。启动真实 `gedit`（`GDK_BACKEND=x11`）后，用 `get_window_state` 读到 76 个 AT-SPI 元素（结构化 `elements` + `tree_markdown`），窗口发现和读取界面可用。 另外，通过完整网关链路（管理员会话 → 模型工具调用 → `mcp` → Driver）调用 `desktop__list_windows` 也通过，结果的文字和 `structuredContent` 都在（`TestRealCuaThroughGateway`）。
- 受限未通过：`get_desktop_state` 截图返回 X11 `GetImage BadMatch`；`get_window_state` 的截图没有在 CLI 返回图像；`click` 走 X MPX 被拒后回退 XSendEvent，`effect=unverifiable`；`type_text` 返回 `verified=true` 但重新读树看不到文本。判断为 WSLg/Weston 图形会话限制（root 只有 10x10、没有真实输出），不是网关链路问题。模型侧的多模态截图/输入闭环因此没有在本环境宣布通过。
- 未验证：Windows（本机没有 Driver 可执行文件/交互式图形会话）、macOS（没有 Mac）、原生 Linux Wayland、多窗口/缩放/断线重连的真实桌面操作。

**支持范围（按本次证据）**

| 平台 | 本次结论 |
|---|---|
| Linux X11 | 窗口发现、辅助功能树、结构化结果、Driver session 已验证；截图/输入在正常桌面预期可用，本 WSLg 会话未验证 |
| Linux Wayland | Driver 提供实验后端，本机未启用；原生 Wayland 的截图/输入限制需逐桌面验证 |
| Windows | Driver 必须跑在已登录的交互式桌面；未实机验证 |
| macOS | 需要 CuaDriver.app 的辅助功能和屏幕录制授权；未实机验证 |
| WSL | WSL 的 Linux 桌面和 Windows 宿主桌面是两个桌面，要分别接 |

**用法入口**

拓扑图「MCP 服务」节点添加 stdio 服务（命令写 Driver 绝对路径，参数 `mcp`；要挂到别的电脑就选那台在线内核），审批建议 `per_turn` 或 `always`；管理员会话里让模型先 `list_windows` / `get_window_state`。诊断在同一节点面板，或向 Driver 调 `health_report`、在目标机器上跑 `cua-driver doctor`。真实 Driver 联调测试：`NANO_CUA_REAL=1 NANO_CUA_BIN=/path/to/cua-driver go test ./internal/mcp -run TestRealCuaDriver -v`。

**剩余问题**

- Windows / macOS / 原生 Wayland 的截图与输入实机验证。
- 断线重连后的 Driver session 语义已按“重新观察、不重放”处理，但没有自动恢复窗口焦点/滚动位置。
- 多任务同桌面只做了串行，没有更细的资源预约/优先级。
- 截图预算目前按“最近两个带图工具结果”，长任务的视觉上下文质量还需要真实任务评估。
