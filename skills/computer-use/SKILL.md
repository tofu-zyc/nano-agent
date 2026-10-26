---
name: computer-use
description: 操作有图形桌面的电脑（Windows、macOS、Linux）：先观察窗口和辅助功能树，再点击、输入、滚动，最后重新观察验证；也可以安装、配置、诊断 Cua Driver（通过 MCP，在本机或远端电脑的图形会话里），网关只负责模型循环和调度。
---

# 用桌面

你现在可以操作一台真实的桌面（Windows / macOS / Linux）。Driver 是一个 MCP 服务，工具名带服务前缀（例如 `desktop__get_window_state`）。先看它 initialize 时给的操作说明（也会随系统提示带进来），需要更细的玩法用它的 `get_guide`。

## 安装和配置（AI 自己来，不要一上来让用户操作）

用户说“帮我装 Cua Driver / 让你能操作这台电脑”时，按下面的顺序做。目标是把 **驱动装在要操作的那台机器上**，再把它的 `mcp` 子进程注册成 nano-agent 的 MCP 服务。先读 [Cua 官方安装页](https://cua.ai/docs/how-to-guides/driver/install) 和 [接入 agent](https://cua.ai/docs/cua-driver/guides/connect-your-agent)；命令随版本可能变，以目标机器上的 `cua-driver --help` 和官方页为准。

### 0. 先确认在哪台机器、有没有图形会话

- 用 `Gateway {"what":"mcp"}` 看 `machines`，确认目标机器在线；用 `Gateway {"what":"workspaces"}`（或设备技能）确认那台机器上有可执行的工作目录。`Shell` 是在工作目录所在机器上执行的，不是网关本机。
- 在那台机器上跑：`uname -s`、`cat /etc/os-release`（Linux）、`sw_vers`（macOS）、`ver`（Windows）；再看 `echo "$DISPLAY / $WAYLAND_DISPLAY / $DBUS_SESSION_BUS_ADDRESS"`。
- **安装第三方软件前先明确告诉用户要装什么、从哪个官方地址装，并取得同意。** 不要静默 `curl | sh`，不要擅自 `sudo`。

### 1. 安装驱动

Linux（不需要管理员；最小 Debian/Ubuntu 先补依赖）：

```bash
curl -fsSL https://cua.ai/install.sh | sh -s -- --select cua-driver
# 或只装驱动（以官方安装页当前写法为准）：
# /bin/bash -c "$(curl -fsSL https://cua.ai/driver/install.sh)"
```

缺依赖时先问用户再执行，例如 Debian/Ubuntu：`sudo apt install libxi6 at-spi2-core`；GNOME 还要装它自带的 WinRects helper，装完注销再登录一次。

macOS：

```bash
/bin/bash -c "$(curl -fsSL https://cua.ai/driver/install.sh)"
# 或 curl -fsSL https://cua.ai/install.sh | sh -s -- --select cua-driver
```

Windows（PowerShell，交互式桌面会话）：

```powershell
irm https://cua.ai/driver/install.ps1 | iex
```

装完确认：`command -v cua-driver`、`cua-driver --version`。nano-agent 的 MCP 配置里**必须写绝对路径**。

我们在 nano-agent 上验证的是 **0.7.0**。新版通常兼容；要严格复现就按官方安装页的固定版本参数装 0.7.0（不同平台的变量/参数不同，别猜）。第一次接新版先只做只读观察。

### 2. 让驱动能看见桌面（最容易卡住的一步）

Driver 必须能连到**已登录的图形会话**，不是只装在后台服务里。

- Linux：在桌面会话的终端里启动 `cua-driver serve`（或者让内核/网关本身从这个桌面会话启动，子进程就会继承 `DISPLAY`/`WAYLAND_DISPLAY`/`DBUS_SESSION_BUS_ADDRESS`）。`cua-driver status` 会打印 socket，例如 `~/.cache/cua-driver/cua-driver.sock`。
- Windows：`cua-driver` 要跑在用户登录的交互式会话；可以 `cua-driver autostart enable` 注册登录自启（当前版本 Windows 才有 autostart），然后重新登录或 `cua-driver autostart kick`。
- macOS：必须通过 CuaDriver.app/代理进程，后台随便起的进程不继承“辅助功能/屏幕录制”授权。

如果你（AI）的 `Shell` 在后台服务里、没有 `DISPLAY`/会话总线，**不要假装成功**：一边在能访问桌面的终端里起 `cua-driver serve`，一边把 MCP 命令改成连这个 daemon（当前版本用 `cua-driver mcp --socket <status 显示的路径>`；先 `cua-driver mcp --help` 确认这个版本支持）。如果做不到，就明确告诉用户“只差把这条命令在桌面终端里跑一下”，而不是让用户从头配置。

验证：

```bash
cua-driver doctor
cua-driver status
cua-driver call list_apps --json
cua-driver call list_windows --json
```

`doctor` 退出码 0 不代表桌面一定能操作，要读完整报告和警告。`list_windows` 能看到真实窗口才算桌面通了。

### 3. 注册成 nano-agent 的 MCP 服务

先 `Gateway {"what":"mcp"}` 看有没有重名服务和目标机器名，然后：

```json
{"action":"mcp_save","name":"desktop","machine":"要操作的机器名（网关本机写空字符串）","command":"/绝对路径/cua-driver","args":["mcp"],"enabled":true,"approval":"per_turn"}
```

如果驱动在桌面会话里跑成了 daemon、需要连 socket：

```json
{"action":"mcp_save","name":"desktop","command":"/绝对路径/cua-driver","args":["mcp","--socket","/home/用户/.cache/cua-driver/cua-driver.sock"],"machine":"机器名","enabled":true,"approval":"per_turn"}
```

`machine` 必须是 `Gateway {"what":"mcp"}` 里在线的内核名；网关本机写 `""`。不要写 `"gateway"`（会被当成空，但建议直接写空）。桌面操作建议 `approval:"per_turn"` 或 `"always"`。

然后用 `{"action":"mcp_test","name":"desktop"}` 现连一次。连上后新工具从下一条消息起可用；先调用 `desktop__list_windows`，再 `desktop__get_window_state`。失败时看拓扑图 MCP 节点的“目标机器 / 桌面 / 最近错误”，不要反复重试同一个错误。

### 4. 权限和安全边界

- **Linux**：通常不需要 root；装系统依赖才用 `sudo`。Wayland 下截图/输入可能被合成器拒绝，先按 [平台差异] 降低预期。
- **Windows**：安装/首次运行可能弹 UAC；必须由用户在交互式桌面确认。服务账号/会话 0 看不到用户桌面。
- **macOS**：装完要授予“辅助功能”和“屏幕录制”。可以让用户运行 `cua-driver permissions grant` 并点这两个系统弹窗；只有用户明确授权时，才可以由 AI 通过远程桌面去点 **CuaDriver 自己的** 弹窗，绝不代点其他应用，也绝不在不属于用户的机器上做。托管 Mac 用 MDM/PPPC。
- 不要把 API key、密码、验证码放进 args 或对话；不要为了“验证”去付款、发消息、删文件。登录/授权/验证码让人来做，你把界面说清楚。

### 5. 装完怎么交付

告诉用户：装在哪台机器、驱动版本、桌面是否真的可见（`list_windows` 看到了什么）、MCP 服务名、审批档位、还有哪些平台/权限没验证。不要把“进程起来了”说成“桌面能操作”。

## 铁律

1. **先观察再动手。** 先 `list_windows` / `list_apps` 找到目标窗口和 pid，再 `get_window_state {pid, window_id}` 拿辅助功能树和截图。元素地址优先用 `element_index`（辅助功能），不行再用截图坐标（窗口内像素，不是全屏坐标）。
2. **工具返回 success 不等于操作成功。** 每次点击、输入、滚动、拖拽之后都要重新 `get_window_state`（或截图）确认界面变了；不要根据“调用没有报错”就说已经完成。
3. **结果未知不要重放。** 超时、断线、取消、进程重启以后，先重新观察当前状态，再决定下一步。不要无条件重新点一次、再发一次消息或重复提交。
4. **同一台桌面可能还有别的任务。** 网关会把同一台机器上的桌面调用排队，但不要假设自己独占鼠标键盘、剪贴板或前台窗口。操作前把窗口带到前台（`bring_to_front`），做完尽量恢复。
5. **不碰危险操作。** 删除文件、付款、发送对外消息、改系统设置这类不可逆的事，先问用户。登录、授权、验证码这类需要人做的事，把看到的界面说清楚，请用户处理。

## 一次可逆任务的循环

1. 找到窗口：`list_windows`（或 `list_apps` 看已安装/正在运行）。
2. 观察：`get_window_state`；看 `structuredContent.elements` 里的角色、名字和 bounds，同时看截图。
3. 操作：点击用 `click` / `double_click` / `right_click`（传 `element_index` 或窗口内 x,y），输入用 `type_text`，按键用 `press_key` / `hotkey`，滚动用 `scroll`。
4. 验证：再 `get_window_state`，确认文字、选中状态或窗口标题发生变化。没变就换一种方法（辅助功能动作 / 坐标 / 键盘）。
5. 收尾：`end_session` 会用网关给的 session 释放光标和临时状态；用户停止、重置会话时网关也会调用它。

## 平台差异

- **Windows**：Driver 必须跑在已登录的交互式桌面会话里（服务/后台会话看不到用户桌面）。截图是真实屏幕像素；高 DPI 下坐标是窗口内截图像素。WSL 里的 Linux 桌面和 Windows 宿主桌面是两个桌面，要分别接。
- **macOS**：Driver 要以 CuaDriver.app / 代理进程的身份获得“辅助功能”和“屏幕录制”权限；后台随便起的进程不一定继承授权。坐标是屏幕点（逻辑像素）。
- **Linux**：X11 和 Wayland 的路径不同；GNOME、KDE、Sway、Hyprland 的截图、全局输入限制不一样。XWayland 窗口通常可以，原生 Wayland 窗口可能拒绝模拟输入；必要时让用户切到 X11 会话或授予远程桌面权限。

## 出问题怎么查

- 网页拓扑图的「MCP 服务」节点会显示：目标电脑在不在线、Driver 版本、initialize 说明和最近一次错误。
- 设备在线只说明网络通，不代表桌面可操作：图形会话没登录、授权没给、驱动版本不对，工具都会失败。
- Driver 自己的诊断：`cua-driver doctor`、`cua-driver diagnose`（在目标机器上跑），或者 MCP 的 `health_report` / `get_config`。
- 断线重连后 Driver 的 session 状态可能丢了：重新 `list_windows` / `get_window_state`，不要沿用旧的窗口坐标和元素编号。

## 结果怎么写

告诉用户：做了什么、在哪个窗口、看到了什么变化、有没有没验证的地方。不要把“我点了”写成“已经完成”。