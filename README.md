# nano-agent

一个轻量的 bot 型 AI agent，拆成两个小程序：

- **内核**（C，约 2000 行含注释，除内嵌的 cJSON 外没有依赖）：只跑 agent 循环，负责预算、权限、审批，以及在本机执行工具。
- **网关**（Go）：保存对话上下文，对接模型（任何 OpenAI 兼容接口）和聊天平台（Telegram、飞书、命令行）。

两者通过一条一行一条 JSON 的连接通信（[协议说明](docs/protocol.md)），可以跑在同一台机器上，也可以分开：比如网关在 VPS 上，内核在家里的机器上执行工具。

```
聊天平台（Telegram / 飞书 / 命令行）
   |
网关 Go     平台适配 · 会话上下文 · 人格 · 模型接入
   |        NDJSON over Unix socket / TCP，由内核主动连网关
内核 C      循环状态机 · 步数和时间预算 · 权限与审批 · 本地工具调度
   |        fork/exec：参数走 stdin，结果走 stdout
tools/      任意语言写的可执行文件
```

## 快速开始（本机，命令行聊天）

需要 gcc 或 clang、make、Go 1.22 以上；示例工具 `run_shell` 需要 python3。

```sh
cp gateway.example.toml gateway.toml   # 填模型的 base_url / api_key / model，
cp kernel.example.conf kernel.conf     # 两边的 token 改成同一串随机字符
make run
```

`make run` 会编译两个程序，后台启动内核，前台启动网关，直接在终端里聊天，Ctrl-C 或 Ctrl-D 退出。日志在 `data/kernel.log` 和 `data/gateway.log`，另开一个终端 `tail -f data/*.log` 能看到每次模型调用和工具执行。

也可以分开启动：

```sh
make
./gateway/nano-gateway -c gateway.toml
./kernel/nano-kernel -c kernel.conf      # 另一个终端；-t 只检查配置和工具，-v 打印协议消息
```

## 改人格（系统提示词）

人格文件在 `personas/` 目录下：

- `personas/default.md`：默认人格，所有会话都用它。
- `personas/<会话ID>.md`：给某个会话单独设定，比如 `personas/tg:-1001234567890.md`。会话 ID 在聊天里发 `/whoami` 查。

**改完保存，下一条消息就生效，不用重启。** 历史里还留着旧人格说过的话，人格改动大的话可以发一次 `/reset` 清空上下文。

群聊里网关会在每条用户消息前加上 `[昵称] `，让模型分得清是谁在说话，默认人格里已经写了这条说明。

## 加工具

每个工具是 `tools/` 下的一个目录，用什么语言写都行：

```
tools/get_weather/
  tool.json   描述和策略
  run         可执行文件：从 stdin 读参数 JSON，结果写到 stdout，退出码非 0 表示失败
```

```json
{
  "name": "get_weather",
  "description": "查询城市天气",
  "input_schema": {
    "type": "object",
    "properties": { "city": { "type": "string" } },
    "required": ["city"]
  },
  "allow": "all",
  "approval": false,
  "timeout": 10,
  "command": ["python3", "weather.py"]
}
```

| 字段 | 默认值 | 说明 |
|---|---|---|
| `name` | 必填 | 字母、数字、`_`、`-`，最长 64 位 |
| `description` / `input_schema` | 空 / 空对象 | 原样交给模型 |
| `allow` | `"admin"` | `"admin"` 只有管理员能用；`"all"` 所有人都能用。默认最严，新工具要显式放开 |
| `approval` | `false` | `true` 时每次执行前要管理员在聊天里批准 |
| `timeout` | `30` | 秒，超时会连同子进程一起杀掉 |
| `command` | `["./run"]` | 在工具目录下执行 |

工具进程还能从环境变量拿到 `NA_SID`（会话）、`NA_FROM`（触发者身份）、`NA_CALL_ID`、`NA_TOOL`。输出超过 `max_output`（默认 64 KB）会被截断。工具改动后重启内核生效。

自带两个示例：`get_time`（所有人可用）和 `run_shell`（仅管理员，并且每次都要审批）。

## 权限和审批

- 管理员身份写在 `kernel.conf` 里（`admin = tg:123456789`，可以写多行）。自己的身份在聊天里发 `/whoami` 查看。
- 权限只由内核判断：群里任何人都能和 bot 说话，也就能尝试用提示词诱导模型去调高危工具，所以这道关设在模型之外。被拒绝的调用也会给模型一个结果（"权限不足"），模型会自己跟用户解释。
- 需要审批的工具，bot 会在聊天里发 `⚠️ 需要审批 #3 …`，管理员回复 `/approve 3` 或 `/deny 3`。非管理员的审批无效，超时（默认 300 秒）按拒绝处理。
- 建议用一个专门的低权限用户运行内核，万一权限配错了还有一层保护。

## 聊天命令

| 命令 | 作用 |
|---|---|
| `/help` | 列出命令 |
| `/whoami` | 显示你的身份和当前会话 ID（配置管理员、单独人格时要用） |
| `/stop` | 停止当前任务（正在执行的工具会被杀掉） |
| `/reset` | 清空本会话的上下文 |
| `/approve <编号>` / `/deny <编号>` | 批准或拒绝一次工具执行 |

群聊里 bot 只回应 @ 它或回复它的消息（`group_reply_all = true` 可以改成每条都回）；命令不用 @ 也会响应。

## 接入 Telegram

1. 找 [@BotFather](https://t.me/BotFather) 发 `/newbot`，拿到 token，填进 `gateway.toml` 的 `[telegram]`，并设置 `enabled = true`。
2. 国内访问 Telegram 需要代理：`proxy = ""` 表示跟随环境变量 `HTTPS_PROXY`，也可以直接写 `proxy = "http://127.0.0.1:7890"`。
3. 用的是长轮询，不需要公网地址。
4. 想在群里用：Telegram 默认的隐私模式下，机器人在群里只收得到发给它的命令和回复它的消息。要让 @ 它也能触发，二选一：
   - 以管理员身份把机器人加进群；
   - 或者在 BotFather 里 `/setprivacy` → Disable，然后**把机器人移出群再重新拉进来**，改动才会生效。

## 接入飞书

用的是飞书开放平台的长连接模式，不需要公网地址，也不用配置加密。

1. 在[飞书开放平台](https://open.feishu.cn/app)创建企业自建应用，在"添加应用能力"里加上**机器人**。
2. 在"凭证与基础信息"里拿到 App ID 和 App Secret，填进 `gateway.toml` 的 `[feishu]`，并设置 `enabled = true`，然后启动网关。
3. 在"事件与回调"里把订阅方式设为**使用长连接接收事件**。如果保存时提示检测不到连接，确认网关已经在运行，再保存一次。
4. 添加事件 **接收消息 v2.0**（`im.message.receive_v1`），按提示开通配套权限。还需要的权限：
   - `im:message`、`im:message:send_as_bot`：收发消息；
   - 单聊消息、群聊中 @ 机器人消息的读取权限：添加事件时会提示；
   - 可选 `contact:user.base:readonly`：读取用户姓名，群聊里显示真实昵称（没有的话用"用户xxxx"代替）。
5. 创建版本并发布（企业里一般要管理员审核），然后把机器人拉进群或直接私聊。

具体按钮名称以飞书后台为准，后台改版后可能会变。

## 网关和内核分开部署

- 网关：`[kernel] listen = "tcp:0.0.0.0:7700"`。
- 内核：`gateway = tcp:网关地址:7700`。
- 协议本身不加密，跨公网时请走 WireGuard、Tailscale 或 SSH 隧道（比如 `ssh -L 7700:127.0.0.1:7700 vps`，内核连 `tcp:127.0.0.1:7700`）。两边的 `token` 必须一致。
- 内核断线后会自动重连。断线时进行中的任务会作废，正在执行的工具会被杀掉，聊天里会收到提示。

## 模型接入

任何 OpenAI 兼容的 `/chat/completions` 接口都能接，比如 vLLM、DeepSeek、通义、Kimi。几个注意点：

- `proxy`：Go 不认 `NO_PROXY` 里 `10.*` 这种通配写法，**内网的模型地址要显式写 `proxy = "none"`**，否则请求会被发到代理上。
- `[llm.extra]` 里的参数会原样合并进请求体，比如 vLLM 上关掉思考模式：`chat_template_kwargs = { thinking = false }`。
- `max_history_tokens` 控制每次请求最多带多少历史（按字节数粗估）。超出时从最老的一轮开始丢，工具调用和它的结果不会被拆开。
- 对话历史存在 `data/sessions/<会话ID>.jsonl`，一行一条消息，只追加，重启后会恢复上下文。

## 测试

```sh
make test
```

- `kernel/tests/test_core.c`：状态机单元测试（ASan + UBSan）。
- `kernel/tests/test_host.py`：用假网关驱动真实的内核进程，测子进程执行、超时强杀、输出截断、审批、取消、断线重连（ASan 版本）。
- `gateway/...`：Go 单元测试，`internal/app` 里用假内核走真实 socket 测调度逻辑（`-race`）。
- `scripts/e2e.sh`：真实的内核和网关进程一起跑，模型用 mock，不花钱。

把 `llm.provider` 设成 `mock` 就能不接模型调试整条链路：消息里写 `!call 工具名 {参数}` 会让假模型去调这个工具。

## 目录

```
kernel/            C 内核
  src/core.c         状态机（纯逻辑，不做 I/O）
  src/main.c         poll 主循环
  src/tools.c        工具加载和子进程执行
  src/link.c         网关连接
  src/conf.c         配置解析
gateway/           Go 网关
  internal/app       调度：排队、run、审批、命令
  internal/kernel    内核连接
  internal/llm       模型接入（OpenAI 兼容 / mock）
  internal/session   对话历史、裁剪
  internal/persona   人格文件热加载
  internal/channel   Telegram / 飞书 / 命令行
tools/             本地工具
personas/          人格文件
docs/protocol.md   内核 ⇄ 网关协议
```
