# nano-agent

一个轻量的 bot 型 AI agent，拆成两个小程序：

- **内核**（C，除内嵌的 cJSON 外没有依赖）：只跑 agent 循环，负责预算、权限模式和审批。工具交给执行器 `nano-exec`，在沙箱里执行。
- **网关**（Go）：保存对话上下文，对接模型（任何 OpenAI 兼容接口）和聊天平台（Telegram、飞书、命令行）。

两者通过一条一行一条 JSON 的连接通信（[协议说明](docs/protocol.md)），可以跑在同一台机器上，也可以分开：比如网关在 VPS 上，内核在家里的机器上执行工具。

```
聊天平台（Telegram / 飞书 / 命令行）
   |
网关 Go     平台适配 · 会话上下文 · 人格 · 模型接入 · 工作目录设置
   |        NDJSON over Unix socket / TCP，由内核主动连网关
内核 C      循环状态机 · 步数和时间预算 · 权限模式与审批
   |        每次工具调用启动一次执行器：请求走 stdin，结果走 stdout
nano-exec   Read / Write / Edit / Grep / Shell，关在 Landlock + seccomp 沙箱里
```

## 快速开始（本机，命令行聊天）

需要 gcc 或 clang、make、Go 1.22 以上。沙箱需要 Linux 5.13 以上（WSL2 也可以）。

```sh
cp gateway.example.toml gateway.toml   # 填模型的 base_url / api_key / model，
cp kernel.example.conf kernel.conf     # 两边的 token 改成同一串随机字符
make run
```

`make run` 会编译三个程序，后台启动内核，前台启动网关，直接在终端里聊天，Ctrl-C 或 Ctrl-D 退出。回复边生成边显示，思考过程和工具调用是灰色的，每轮结束后会显示用量和前缀缓存的命中率：

```
写个脚本打印 1 到 5 的平方并运行
思考> 用户要……
  ⚙ Write  squares.py（38 字节）
    ✓ 已创建 /home/me/foo/squares.py（2 行，38 字节）
  ⚙ Shell  python3 squares.py
    ✓ 1（共 5 行）
bot> 结果是 1、4、9、16、25。
  3 次模型调用 · 输入 9120 tokens（缓存命中 96%） · 输出 210（思考 120） · 用时 4.2s
```

日志在 `data/kernel.log` 和 `data/gateway.log`，另开一个终端 `tail -f data/*.log` 能看到每次模型调用和工具执行。

想让它读写文件、执行命令，先给它一个工作目录：

```
/dir add ~/projects/foo
/mode auto
```

也可以分开启动：

```sh
make
./gateway/nano-gateway -c gateway.toml
./kernel/nano-kernel -c kernel.conf      # 另一个终端；-t 只检查配置和执行器，-v 打印协议消息
```

## 改人格（系统提示词）

人格文件在 `personas/` 目录下：

- `personas/default.md`：默认人格，所有会话都用它。
- `personas/<会话ID>.md`：给某个会话单独设定，比如 `personas/tg:-1001234567890.md`。会话 ID 在聊天里发 `/whoami` 查。

**改完保存，下一条消息就生效，不用重启。** 历史里还留着旧人格说过的话，人格改动大的话可以发一次 `/reset` 清空上下文。

群聊里网关会在每条用户消息前加上 `[昵称] `，让模型分得清是谁在说话，默认人格里已经写了这条说明。

## 工具

工具是固定的五个，内置在执行器里：

| 工具 | 作用 | 类别 |
|---|---|---|
| `Read` | 读文件（带行号，可分段），读目录时列出内容 | 只读 |
| `Grep` | 搜索文件内容，或按文件名列文件（有 ripgrep 就用它，没有退回 grep） | 只读 |
| `Write` | 新建或覆盖文件 | 写 |
| `Edit` | 把文件里的一段原文精确替换成新文本 | 写 |
| `Shell` | 用 `/bin/sh` 执行命令，默认超时 120 秒，最长 600 秒 | 命令 |

工具集固定不变，工具定义对模型的前缀缓存最友好。要扩展能力，后面会通过 skill 来做。

## 工作目录和权限

**工作目录**是工具能碰到的范围。所有工具都在沙箱里执行：

- 工作目录：可读写（只读模式下只读）；
- 系统目录（`/usr`、`/etc` 等）、`PATH` 里的命令：只读；
- 每个会话自己的临时目录（`$TMPDIR`）和共用的缓存目录：可读写；
- 其他地方，包括 HOME 里的其他东西、`/tmp`：一律访问不了；
- 不能连 unix socket（防止借 D-Bus、systemd、docker 在沙箱外执行命令），不能给沙箱外的进程发信号；
- 环境变量只保留 `PATH`、`HOME`、`LANG` 等少数几个，shell 配置里 export 的密钥带不进去。

沙箱用的是 Linux 自带的 Landlock 和 seccomp，普通用户就能用，不需要 root 和容器。机器不支持 Landlock 时（比如很老的内核），文件工具仍然只能访问工作目录，但 Shell 没有沙箱，自动模式下也会改为每次审批。

**权限模式**决定工作目录内的操作要不要先问：

| 模式 | 读文件、搜索 | 写文件 | 执行命令 |
|---|---|---|---|
| `auto` 自动 | 直接执行 | 直接执行 | 直接执行 |
| `ask` 审批 | 直接执行 | 管理员批准后执行 | 管理员批准后执行 |
| `readonly` 只读 | 直接执行 | 不允许 | 不允许 |

工作目录之外的操作在任何模式下都不允许，需要的话把目录加进来。

**设置方法**：`gateway.toml` 的 `[workspace]` 是默认值；每个会话可以用 `/dir`、`/mode` 单独改，改动保存在 `data/sessions/` 下，下一条消息开始生效。工作目录或模式变化时，网关会在下一条用户消息前加一句 `[系统] 工作目录：…` 告诉模型，而不是去改系统提示词，这样前缀缓存不会失效。

几条限制：

- 工作目录不能包含 nano-agent 自己：配置文件里有密钥，执行器要是能被改写，沙箱就形同虚设。内核和网关都会拒绝这样的目录，所以别把 nano-agent 装在要让 bot 工作的目录里。
- 严格限制之后，有些工具需要额外放行。默认已经只读放行了 `~/.gitconfig`（git 提交需要用户名和邮箱）；npm、cargo 之类写缓存的，在 `[workspace]` 的 `extra_rw` 里加对应目录。
- 命令结束时，它启动的后台进程会被清理。

## 管理员

- 管理员身份写在 `gateway.toml` 的 `[access] admins` 里（比如 `"tg:123456789"`），自己的身份在聊天里发 `/whoami` 查看。命令行用户始终是管理员。
- 只有管理员能用写文件、执行命令这类工具，能改工作目录和模式，能审批。
- 其他人默认只能聊天（`guest = "none"`）；设成 `readonly` 的话可以读工作目录里的文件和搜索。
- 需要审批时，bot 会在聊天里发 `⚠️ 需要审批 #3`，写清楚要执行的命令或要改的文件，管理员回复 `/approve 3` 或 `/deny 3`。超时（默认 300 秒）按拒绝处理。

## 聊天命令

| 命令 | 作用 |
|---|---|
| `/help` | 列出命令 |
| `/whoami` | 显示你的身份和当前会话 ID（配置管理员、单独人格时要用） |
| `/dir` | 查看工作目录；`/dir add <路径> [ro]` 添加（`ro` 表示只读），`/dir rm <路径>` 移除，`/dir default <路径>` 设为默认 |
| `/mode` | 查看权限模式；`/mode auto\|ask\|readonly` 切换 |
| `/think` | 查看思考强度；`/think off\|low\|high…` 切换（可选值见 `llm.thinking_levels`） |
| `/stop` | 停止当前任务（正在执行的命令会被杀掉） |
| `/reset` | 清空本会话的上下文（工作目录和模式保留） |
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
- 内核断线后会自动重连。断线时进行中的任务会作废，正在执行的命令会被杀掉，聊天里会收到提示。
- 工作目录是内核所在机器上的路径。

## 模型接入

任何 OpenAI 兼容的 `/chat/completions` 接口都能接，比如 vLLM、DeepSeek、通义、Kimi。请求走流式接口。几个注意点：

- `proxy`：Go 不认 `NO_PROXY` 里 `10.*` 这种通配写法，**内网的模型地址要显式写 `proxy = "none"`**，否则请求会被发到代理上。
- `[llm.extra]` 里的参数会原样合并进请求体。
- **思考强度**：`thinking_style` 决定怎么传给模型——`vllm` 写进 `chat_template_kwargs`（`thinking` 开关和 `reasoning_effort`），`openai` 用顶层的 `reasoning_effort`。默认值是 `thinking`，每个会话可以用 `/think` 单独改。多数模型的对话模板把思考强度写在提示词最前面，**切换后这个会话的前缀缓存会失效一次**（之后又能命中），所以它是一个手动切换的会话设置，不会自动变。

## 上下文和前缀缓存

模型服务一般都有前缀缓存：这次请求的开头和上次一样，那部分就不用重新计算，又快又便宜。网关的上下文管理围绕这一点设计——**每次请求都是上一次请求原样加上新消息**：

- 系统提示词（人格）和工具定义固定不变，里面不放时间、目录这类会变的东西。
- 时间写在每条用户消息前面（`[2026-10-21 周三 15:08]`）；工作目录、权限模式变了，就在下一条用户消息前加一句 `[系统] …`。都是追加，不改前面的内容。
- 工具输出在写进历史时就截断定型，思考内容不回传，历史只追加。
- 历史超过 `max_history_tokens`（按字节数粗估）时，**一次**裁掉最早的一大段，裁到 `trim_to` 的比例（默认一半），之后又只追加，直到下次超出；不会每次都裁一点（那样每次请求的开头都不一样，缓存永远不命中）。裁剪只在用户消息处切，工具调用和它的结果不会被拆开；被裁掉的部分用一句说明代替，工作目录信息也会保留。以后的上下文压缩（把裁掉的部分换成摘要）也放在这个位置。
- 每次模型调用的缓存命中数记在日志里（`cached_tokens`），命令行每轮结束显示命中率。

对话历史存在 `data/sessions/<会话ID>.jsonl`，一行一条消息，只追加，重启后会恢复上下文；会话设置（工作目录、模式、思考强度、裁剪位置）在同目录的 `.settings.json` 里。

## 测试

```sh
make test
```

- `kernel/tests/test_core.c`：状态机单元测试（ASan + UBSan）。
- `kernel/tests/test_exec.py`：直接驱动执行器，测五个工具，以及各种绕过沙箱的尝试（`..`、符号链接、`~`、unix socket、发信号、环境变量里的密钥）。
- `kernel/tests/test_host.py`：用假网关驱动真实的内核和执行器，测权限模式、审批、超时、取消、断线重连、进程清理（ASan 版本）。
- `gateway/...`：Go 单元测试，`internal/app` 里用假内核走真实 socket 测调度逻辑（`-race`）。
- `scripts/e2e.sh`：真实的内核、执行器和网关一起跑，模型用 mock，不花钱。

把 `llm.provider` 设成 `mock` 就能不接模型调试整条链路：消息里写 `!call 工具名 {参数}` 会让假模型去调这个工具，比如 `!call Shell {"command":"ls"}`。

## 目录

```
kernel/            C 内核和执行器
  src/core.c         状态机（纯逻辑，不做 I/O）
  src/main.c         内核的 poll 主循环
  src/runner.c       执行器子进程的管理
  src/tools.c        内置工具的描述和策略
  src/link.c         网关连接
  src/conf.c         配置解析
  src/nexec_*.c      执行器：五个工具的实现
  src/sandbox.c      Landlock + seccomp 沙箱
gateway/           Go 网关
  internal/app       调度：排队、run、审批、命令、工作目录
  internal/kernel    内核连接
  internal/llm       模型接入（OpenAI 兼容 / mock）
  internal/session   对话历史、会话设置、裁剪
  internal/persona   人格文件热加载
  internal/channel   Telegram / 飞书 / 命令行
personas/          人格文件
docs/protocol.md   内核 ⇄ 网关协议
```
