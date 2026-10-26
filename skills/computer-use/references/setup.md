# 首次接入或修复 Cua Driver

已有可用 MCP 服务就回主流程，不再安装。先用 Gateway mcp 查已有服务、命令、目标内核、版本和最近错误；修改同一个服务，保留没有必要改动的配置。

## 定位目标

- Shell 跑在选中的工作目录所在机器上。用 Gateway workspaces / session 确认目标，不把 Linux 网关的环境当成 Windows 桌面的环境。
- Linux/macOS 用 `command -v cua-driver` 查已有路径；Windows 用 PowerShell `Get-Command cua-driver -ErrorAction SilentlyContinue`，必要时查当前用户的 `%LOCALAPPDATA%/Programs/Cua/cua-driver/bin/cua-driver.exe`。查到后用**该绝对路径**运行 `--version`、`mcp --help`、`doctor --json`。
- 只在驱动确实未安装时安装。用户已要求安装/接入时，正常安装步骤不用再次索取相同授权；未授权的提权、系统会话重启、系统权限或额外软件另行说明。不要静默 sudo，也不要改其他 agent 客户端的配置。

官方安装入口：Linux/macOS 为 `https://cua.ai/driver/install.sh`，Windows 为 `https://cua.ai/driver/install.ps1`。实际安装前核对 [官方快速入门](https://cua.ai/docs/cua-driver) 和本机平台；已经可用的版本不为一次任务强行升级。

## nano-agent 的 MCP 配置

调用 GatewayAdmin，字段按目标机器实际值填写：

```json
{"action":"mcp_save","name":"desktop","machine":"my-desktop","command":"/absolute/path/cua-driver","args":["mcp"],"enabled":true,"approval":"per_turn"}
```

- `machine` 是 Gateway mcp 返回的在线内核名；网关本机用 `""`。SSH 别名、显示名称和内核名不一定相同，不能混写。
- Windows `command` 示例：`C:/Users/me/AppData/Local/Programs/Cua/cua-driver/bin/cua-driver.exe`。也可以用 JSON 正确转义的反斜杠。`args` 一项一个，不在路径外另加引号。
- Linux 图形会话变量按 [linux.md](linux.md) 处理，写入服务的 `env`，不要换全局环境或修改 Shell 沙箱。
- 当前 Windows/Linux 的裸 `mcp` 自己拥有运行时，退出时清理；图形环境正确时不必另起 `serve`。只有需要连接已有桌面 daemon 时才用 `args:["mcp","--socket","实际端点"]`，端点从那个 daemon 的 status 读取，不能猜路径。
- macOS 按官方 CuaDriver.app/代理进程路线接入，确认其辅助功能和屏幕录制权限；没有实机时不要宣称验收通过。
- 保留已有的审批档位、agent/工具范围和授权。`--grant existing-profile` 属于额外的现有浏览器配置访问边界，不是普通桌面任务的默认必需参数；已有授权才沿用，不为修普通输入问题擅自增加它。

然后 GatewayAdmin `{"action":"mcp_test","name":"desktop"}`。成功只说明握手完成；继续用该服务的 list_windows、get_window_state 对**用户指定窗口**做一次只读观察，确认画面可见。新工具会在后续模型请求中出现，不用清空原聊天。

## 常见故障按证据处理

| 现象 | 下一步 |
| --- | --- |
| 命令不存在 / 进程退出 | 核对目标机器上的绝对路径、`mcp --help`、一项一个的 args；不用网关的路径代替 |
| 内核不在线 | 恢复已有设备连接，再测同一服务；不要新增同名或备用桌面服务 |
| Windows 看不到用户窗口 | 核对内核/Driver 是否在已登录用户的交互式 Session，服务 Session 0 不能代替它；不杀其他内核 |
| Linux 有树无图 / 黑图 | 读 linux.md，查实际桌面输出、会话变量和合成器；不以 doctor 退出码代替截图验收 |
| macOS 系统授权不足 | 说明需要用户处理的 CuaDriver 授权界面，不用后台任意进程替代获授权 app |
| 服务连接成功但模型没工具 | 查服务 enabled、agents/tools 限制、当前 agent 白名单和管理员身份；下一轮重新查询，不先删聊天或重装 |

只修有证据指向的问题。交付时记录服务名、目标内核、驱动绝对路径/版本、审批方式、已通过的窗口观察，便于后续直接复用。

参考：[官方连接机制](https://cua.ai/docs/cua-driver/guides/connect-your-agent)、[CLI 与运行时](https://cua.ai/docs/cua-driver/reference/cli/serve-and-configure)。工具参数以当前服务 schema 为准。
