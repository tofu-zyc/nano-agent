# Linux 图形会话与已验证路径

Linux 网关可以没有桌面；Driver 必须在目标图形会话中。先确定用户要操作的是 X11、Sway、GNOME 还是其他桌面，不为完成任务悄悄切换桌面。

## 后台网关/SSH 启动的 Driver

MCP 子进程继承的是启动它的网关或内核环境。SSH 里的 DISPLAY 为空，不代表桌面不存在；也不能任意猜 `:0`。

在目标账号查询图形会话实际变量，只取这些键，不回显整份环境（其中可能有密钥）：

```sh
systemctl --user show-environment | rg '^(DISPLAY|XAUTHORITY|WAYLAND_DISPLAY|XDG_RUNTIME_DIR|DBUS_SESSION_BUS_ADDRESS|SWAYSOCK|XDG_CURRENT_DESKTOP)='
```

可从已知图形会话或其启动记录补齐，不要读无关进程的完整环境。将**实际值**通过 GatewayAdmin mcp_save 的 `env` 保存到已有服务，再 mcp_test：

- X11：DISPLAY、需要时的 XAUTHORITY，以及同用户的 DBUS_SESSION_BUS_ADDRESS、XDG_RUNTIME_DIR。辅助功能和截图必须指向同一会话。
- 原生 Wayland：WAYLAND_DISPLAY、XDG_RUNTIME_DIR、会话总线，另按当前 Driver 要求设 `CUA_DRIVER_RS_ENABLE_WAYLAND:"1"`。Sway 还需要本会话的 SWAYSOCK。不能把 GNOME 的总线和另一个 Sway 的 socket 混在一起。
- 已有正确桌面 daemon：可让 MCP 连接其实际 socket，避免重复启动；不要为了 MCP 握手成功而停掉其他任务使用的 daemon。

## 平台差异

| 桌面 | 已验证与处理方式 |
| --- | --- |
| X11 | Driver 0.7.0 / 0.37.0 在独立 Xvfb + Openbox + xcompmgr 通过发现、非空截图、set_value 完整中文、点击和应用输出核验；0.37.0 真网关自主任务也通过 |
| Sway / wlroots | Driver 0.37.0、Sway 1.11 / wlroots 0.19 的独立无 XWayland 桌面通过同一闭环；窗口 `shell:xdg_shell`、`window:null`，不是 XWayland 退路 |
| GNOME / Mutter | 当前 linux-pc 的正式会话没有显示输出，XWayland 根屏幕为 0×0，尚未通过操作验收；可能需要官方 WinRects Shell helper，按该版本官方说明处理，别自动重启用户桌面 |
| KDE / Hyprland 等 | 不能沿用 Sway 的通过结论；查当前 Driver 的平台说明，先做只读观察并遵守拒绝结果 |

`doctor`、窗口树、PNG 文件头都不能证明画面可用，实际截图必须看见目标界面。只读到辅助功能树而截图为空时，不要盲点。X11 测试里缺合成器曾出现纯黑图；先修截图来源，不反复输入。

## 故障与恢复

- 文本不完整：重新读控件值；可设置值的控件用 set_value 写入完整预期内容，再观察。不要在残缺内容后反复追加 type_text。
- Wayland 拒绝原始后台输入：优先该控件实际支持的辅助功能动作；若任务确实允许前台操作，再按工具 schema 用前台模式。不能把“知道窗口位置”当成已获得向任意窗口注入输入的能力。
- 断线或读取失败：重新 get_window_state 完整观察，拿新 token；未知的点击不自动重放。
- 没有显示输出：明确缺少实际显示/授权条件，保留任务检查点。不能另起一个测试桌面、在其中操作测试窗口后声称用户任务已完成。

开发者复测用仓库 `scripts/e2e-desktop.py --isolated-x11` 或 `--isolated-wayland --wayland-compositor /path/to/sway`；需要预先具备依赖。普通使用无需创建这些临时桌面。

平台边界参考：[Cua 官方平台说明](https://cua.ai/docs/cua-driver/concepts/platform-support)。
