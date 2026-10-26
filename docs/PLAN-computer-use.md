# Computer use 接入方案

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

本次已验证 nano-agent 的现有 MCP 透传、图片、多模态请求和取消相关测试；尚未安装或连接 Cua Driver，不能据此宣称桌面操作已经可用。

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
