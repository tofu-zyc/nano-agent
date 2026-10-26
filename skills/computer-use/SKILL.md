---
name: computer-use
description: 用已接入的 Cua Driver 操作 Windows、Linux 或 macOS 桌面，完成观察、点击、输入和结果核验；也用于首次接入或修复桌面 MCP。用户要操作某台电脑的窗口、软件、浏览器界面时使用。
---

# 操作电脑桌面

通过当前可用的 MCP 工具操作指定电脑。工具名前缀取自已有服务名，例如 `desktop__get_window_state`；不要假定一定叫 desktop。驱动不负责调用模型，nano-agent 负责计划、审批和任务验收。

## 先复用已有接入

1. 看当前工具和系统提示里的 Driver 版本、说明。已有对应服务时直接观察目标窗口，**不要每次重装、换版本、另起 daemon 或重复添加 MCP**。
2. 不知道服务在哪台机器上，用 `Gateway {"what":"mcp"}` 查服务和在线内核；目标是有桌面的电脑，可能与网关和当前 Shell 所在机器不同。
3. 缺少接入才读 [references/setup.md](references/setup.md)。只在 Linux 环境或截图/输入故障时读 [references/linux.md](references/linux.md)；普通任务不用重新诊断整台电脑。

配套文件由 Skill 放在返回的“技能目录”下，用 Read 读该目录里的 `references/...`。有多个工作目录时，选对应机器的 workspace；不能把相对参考路径当成当前项目里的文件。

## 观察 → 操作 → 再观察

1. 用 `list_windows` / `list_apps` 找到用户指定的窗口，核对 `pid`、`window_id`、标题；排除隐藏的 1×1 辅助窗口。多个候选时继续定位，不猜窗口。
2. 对这个窗口调用 `get_window_state`，读取辅助功能树和截图。0.37.0 可用 `tree_format:"both"`；其他版本按实际工具 schema。第一次或恢复后做完整观察，省略 `since`；增量观察只能引用实际返回的 `snapshot_id`，不要填 `"latest"`。
3. 从**本次观察**选 `element_token`，只做下一步操作。只有旧版 schema 仍支持时才用 `element_index`。改变界面后必须刷新，再取新 token。
4. 可编辑控件优先 `set_value` 设置完整文本；需要真实按键时才用 `type_text` / `press_key`。点击优先元素；坐标按本次截图及 Driver schema 的坐标系，不能把辅助功能树的屏幕坐标直接当窗口坐标。
5. 动作后重新 `get_window_state`。核对实际文字、选中状态、状态栏或产物；`success:true`、返回字符数、发出点击都不是任务完成证明。

一段已观察的控件输入示例（服务前缀、窗口和 token 都换成实际返回值）：

```json
{"pid":1234,"window_id":56,"element_token":"s00000001:0","value":"需要输入的完整文字"}
```

这是 `服务名__set_value` 的参数。之后再次观察，确认内容完整；中文混输实测中 `type_text` 可能只写入部分内容，不能盲目追加重试。

## 中断与结果不明

- 超时、断线、取消、重启或 `effect:unverifiable` / `verified:false` 后，先重新观察；不要直接重放点击、输入、发送或提交。动作可能已经生效。
- 增量树读不出来时改为完整 `get_window_state`；元素变了就取新 token，不反复使用旧索引。
- 网关在一轮观察、操作、验证期间保留同一桌面；其他任务会等待。用户仍能操作桌面。跨两台已占用桌面互等时，保存检查点并结束本轮，稍后再继续，不轮询抢占。
- 不要结束其他任务的 Driver session。网关会带稳定 `session` 并在停止、重置、换 agent 时清理；通常不用手写 `start_session` / `end_session`，也不要改网关给的 session。
- 一次修正后若同样的错误仍在，报告具体缺少的条件。没有显示输出、系统授权待用户处理等情况，不靠重复安装或换服务名解决。

## 任务验收

优先用用户关心的结果核验，例如保存后的文件、应用自己的状态或重新读取的内容。不要为了演示而操作用户没有要求的其他应用。

若这是 Agenda 日程，按完成条件核验后再 `Agenda done`，从本轮工具结果开头 `[本轮验收凭据]` 复制 `id` 放进 `evidence`；也可 `Agenda list` 查。结果未知的动作写进 `Agenda progress` 的 `checkpoint`。需要安排或续做多轮任务时，可加载 `autonomous-tasks` 技能。

遵守用户已给的任务范围和网关审批；不为方便而关审批、提权或增开浏览器登录态授权。已授权的普通步骤直接推进；涉及额外范围或需要用户处理的登录、验证码、系统授权时，说明具体界面和缺少的操作。

完成后简短说明操作了哪个窗口、观察到什么结果、还有什么未验证。测试用的独立桌面只能验证驱动，不能代替用户指定桌面上的任务。
