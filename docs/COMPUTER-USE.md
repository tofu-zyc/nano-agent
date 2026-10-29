# 桌面操作接入与验证

nano-agent 通过已有 MCP 通道接入 Cua Driver。Driver 运行在目标机器的图形登录会话中；Go 网关处理模型与授权，C 内核继续控制工具循环。

~~~text
模型 ↔ Gateway ↔ 目标 nano-kernel ↔ cua-driver mcp ↔ 桌面应用
                     pipe 透传          stdio MCP
~~~

## 接入和诊断

优先按自带 [computer-use 技能](../skills/computer-use/SKILL.md) 复用已有服务；安装、平台权限和 Linux 环境细节见该技能的 references。Driver 是外部依赖，不编入 C 内核。

拓扑图的 MCP 服务节点配置目标机器、Driver 绝对路径和参数 mcp，审批按需要选择 per_turn 或 always。例如：

~~~json
{
  "name": "desktop",
  "machine": "desktop-linux",
  "command": "/absolute/path/to/cua-driver",
  "args": ["mcp"],
  "enabled": true,
  "approval": "per_turn"
}
~~~

Windows 使用实际 exe 路径；macOS 需辅助功能和录屏授权；Linux 需真实可用的 X11/Wayland 图形会话。设备在线不等于桌面可操作。先用 doctor、health_report、list_windows 和 get_window_state 检查，不能只凭握手或窗口树宣告截图与输入可用。

## 执行约定

- 每个会话映射稳定的 Driver session，结束、停止、重置、删除或换 agent 时尝试清理。
- 同桌面按整轮观察、操作和核验保留资源；取消、结束和断线释放。其他任务等待，跨桌面竞争不会互相死等。
- 保留文字和 structuredContent，动作效果以实际回读为准。结果未知时重新观察，不重放点击或输入。
- 依实际 schema 选择 element_token 等定位字段；完整中英文输入优先使用经回读验证的 set_value。
- 模型请求最多保留最近两个带图工具结果，每个结果最多两张图；原始历史和附件保留。初始化说明与 Driver 版本供模型参考。
- 桌面 MCP 走网关/pipe，不经过 nano-exec 的 Shell 隔离。不要用 Shell 的沙箱错误推断 Driver 不可用。

## 已验证范围

记录日期：2026-10-26。以下是具体环境结果，不推广为任意桌面任务的可靠性保证。

| 环境 | 结果 |
| --- | --- |
| Linux 独立 X11，Driver 0.7.0 / 0.37.0 | 非纯色截图、中英文 set_value、点击确认和真实应用回读通过 |
| Sway 1.11 / wlroots 0.19，Driver 0.37.0，无 XWayland | 原生 Wayland 夹具及网关/内核/日程/审批/验收链路通过 |
| Linux 网关 → Windows C 内核 → Driver 0.37.0 | 同上完整链路通过；另以真实模型验证自主排期、选工具和回读验收 |
| 无显示输出的 GNOME / WSLg 会话 | 读树不能证明截图或输入可用，该环境不算完整通过 |
| macOS | 无实机结论，用户暂缓 |

X11 专项发现 type_text 可能只写部分中英文，set_value 回读通过。多窗口、缩放、复杂滚动和长期稳定性仍需实际任务评估；断线后重新观察，不自动恢复焦点和滚动位置。

## 复跑

Linux 夹具使用 scripts/e2e-desktop.py，可选择 --isolated-x11 或 --isolated-wayland；依赖图形环境和脚本指定的 GTK/Pillow 等组件。可传 --gateway、--kernel、--executor 验证真网关与 C 链路，模型默认为本地替身。用脚本 --help 查看完整参数。

Windows 跨机夹具先构建 Windows 内核/执行器，然后在 WSL 运行：

~~~sh
make -C kernel windows-kernel windows-exec
python3 scripts/e2e-desktop-win.py --ssh linux-pc --gateway /absolute/path/nano-gateway \
  --driver 'C:/Users/me/AppData/Local/Programs/Cua/cua-driver/bin/cua-driver.exe'
~~~

要求已登录的 Windows 桌面、可用 Driver、SSH 目标和测试依赖。脚本只操作自建窗口、使用临时配置，不替换正式内核或改系统授权。--real-model-home 可选择已有真实模型，会消费 API 用量；按明确授权使用，失败或跳过不得记为通过。

Driver 本身的可选握手测试：

~~~sh
cd gateway
NANO_CUA_REAL=1 NANO_CUA_BIN=/path/to/cua-driver go test ./internal/mcp -run TestRealCuaDriver -v
~~~
