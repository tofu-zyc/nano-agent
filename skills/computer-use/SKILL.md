---
name: computer-use
description: 操作有图形桌面的电脑（Windows、macOS、Linux）：先观察窗口和辅助功能树，再点击、输入、滚动，最后重新观察验证。通过 MCP 连 Cua Driver（本机或远端电脑上的图形会话），网关只负责模型循环和调度。
---

# 用桌面

你现在可以操作一台真实的桌面（Windows / macOS / Linux）。Driver 是一个 MCP 服务，工具名带服务前缀（例如 `desktop__get_window_state`）。先看它 initialize 时给的操作说明（也会随系统提示带进来），需要更细的玩法用它的 `get_guide`。

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