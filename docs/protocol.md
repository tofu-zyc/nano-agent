# 内核 ⇄ 网关协议（v3）

- **传输**：TCP 或 Unix socket，**内核主动连网关**（内核所在的机器往往在 NAT 后面）。
- **编码**：一行一条 JSON（UTF-8，`\n` 结尾），字段 `t` 是消息类型，没用到的字段省略。
- **握手**：内核发 `hello` → 网关校验 token 和版本 → 回 `welcome`；不通过就回 `error` 并断开。
- **心跳**：内核每 30 秒发 `ping`，网关回 `pong`。内核 90 秒、网关 120 秒没收到对方任何消息就断开。
- **单内核**：网关同一时间只认一个内核，新连接会顶替旧连接，旧连接上进行中的任务全部作废。
- **不加密**：跨机器部署请走 WireGuard、Tailscale 或 SSH 隧道。

## 消息

### 内核 → 网关

| t | 字段 | 含义 |
|---|---|---|
| `hello` | `v` `token` `node` `tools[]` `sandbox` | 握手，报告内置工具和沙箱能力。`tools` 每项是 `{name, description, input_schema}`；`sandbox` 是 `{landlock: N}`，0 表示没有沙箱 |
| `model` | `sid` | 请模型基于这个会话的上下文生成下一轮 |
| `result` | `sid` `id` `ok` `content` | 一次工具调用的结果，包括内核自己生成的失败结果（权限不足、超时、被拒……） |
| `approve_req` | `sid` `id` `name` `args` `from` | 这次调用需要管理员审批 |
| `notice` | `sid` `text` | 发给用户看的提示 |
| `done` | `sid` `status` `detail?` | 这一轮处理结束，`status` 见下表 |
| `ping` | | 心跳 |
| `ls_result` | `id` `ok` `path` `home` `blocked` `entries[]` `truncated?` `detail?` | `ls` 的结果：`path` 是解析过符号链接的绝对路径，`entries` 每项是 `{name, blocked?}`（只有子目录）；`blocked` 表示这个目录包含内核自己，不能当工作目录 |

### 网关 → 内核

| t | 字段 | 含义 |
|---|---|---|
| `welcome` | `v` | 握手成功 |
| `error` | `detail` | 握手失败的原因，随后断开 |
| `input` | `sid` `from` `mode` `dirs[]` `ro[]` `rw[]` | 会话收到新消息，开始一轮处理。`mode` 是这一轮的权限模式（见下表），`dirs` 是工作目录（第一个是默认目录），`ro` / `rw` 是额外的只读 / 读写路径 |
| `turn` | `sid` `reason` `calls[]?` `detail?` | 模型完成了一轮，`reason` 见下表；`calls` 每项是 `{id, name, args}` |
| `approve` | `sid` `id` `ok` `from` | 审批结果。`from` 是审批人身份；网关只会转发管理员的审批 |
| `cancel` | `sid` | 用户要求停止 |
| `pong` | | 心跳 |
| `ls` | `id` `path` | 列出内核所在机器上的一个目录（网页上选工作目录用），`path` 可以是 `~` 开头。只返回子目录名，不读文件内容 |

`turn.reason`：

| reason | 含义 | 内核的反应 |
|---|---|---|
| `final` | 模型给出了最终回复 | `done(ok)` |
| `tool_use` | 模型要调用工具 | 逐个检查、派发，全部有结果后再发 `model` |
| `refused` | 模型拒绝回答 | `done(refused)` |
| `error` | 模型请求失败（网关已重试过） | `done(error)` |

各家模型 API 的差异（`finish_reason`、输出截断、内容过滤……）都由网关归一化成这四种，内核不感知具体是哪家模型。

`done.status`：`ok`、`budget`（模型调用次数达到上限）、`timeout`（整轮超时）、`cancelled`、`refused`、`error`。

`input.mode`：

| mode | 只读工具（Read、Grep） | 写文件（Write、Edit） | 命令（Shell） |
|---|---|---|---|
| `none` | 拒绝 | 拒绝 | 拒绝 |
| `readonly` | 执行 | 拒绝 | 拒绝 |
| `ask` | 执行 | 先审批 | 先审批 |
| `auto` | 执行 | 执行 | 执行（没有沙箱时先审批） |

谁用什么模式由网关决定（管理员用会话设置的模式，其他人用 `access.guest`）；内核负责按模式放行，执行器负责把工具关在工作目录里。

## 一次完整的处理

```
K→G {"t":"hello","v":2,"token":"…","node":"home-pc","tools":[{"name":"Read","description":"…","input_schema":{…}},…],"sandbox":{"landlock":7}}
G→K {"t":"welcome","v":2}
G→K {"t":"input","sid":"tg:-1001234","from":"tg:10001","mode":"auto","dirs":["~/projects/foo"],"ro":["~/.gitconfig"]}
K→G {"t":"model","sid":"tg:-1001234"}
G→K {"t":"turn","sid":"tg:-1001234","reason":"tool_use","calls":[{"id":"call_1","name":"Shell","args":{"command":"date"}}]}
K→G {"t":"result","sid":"tg:-1001234","id":"call_1","ok":true,"content":"Tue Oct 20 21:15:03 CST 2026\n"}
K→G {"t":"model","sid":"tg:-1001234"}
G→K {"t":"turn","sid":"tg:-1001234","reason":"final"}
K→G {"t":"done","sid":"tg:-1001234","status":"ok"}
```

回复正文从头到尾不经过内核：网关收到 `done(ok)` 后，自己把最后一次 `final` 的内容发到聊天平台。

## 约定

- **身份和会话 ID**：都是 `平台:ID` 的形式，平台是 `tg`、`feishu`、`cli`。身份用来判断是不是管理员（网关配置里的 `access.admins`），会话 ID 用来区分上下文。
- **一个会话同时只有一轮处理**：网关负责排队，上一轮没结束时来的新消息等它结束再处理；同一个人连发的几条会合并成一轮。
- **内核的保证**：一轮里模型请求的每个调用都恰好得到一个 `result`；全部有了结果才会发 `model`；每一轮都以 `done` 结束（断线时除外，断线后双方都丢弃进行中的任务）。
- **参数**：`args` 是 JSON 对象。模型给的参数不是合法 JSON 对象时，网关把原始文本作为 JSON 字符串放进 `args`，内核据此回一个"参数不合法"的错误结果，让模型自己纠正。
- **对不上的消息直接忽略**：比如超时之后才到的工具结果、已经取消的任务的 `turn`。

## 执行器

内核每次执行工具都启动一次 `nano-exec`（以后远程主机上是 `ssh 主机 nano-exec`），一问一答：

```
stdin  {"tool":"Shell","args":{"command":"ls"},"dirs":["/home/u/foo"],"write":true,"ro":[],"rw":[],
        "tmp_key":"tg:-1001234","max_output":65536,"watch_stdin":true}
stdout {"ok":true,"content":"a.txt\nb.txt\n"}
```

- 执行器先套上沙箱（Landlock + seccomp），再执行工具；沙箱启动失败就不执行。
- 请求写完后 stdin 保持打开。stdin 被关闭或收到 SIGTERM 表示取消：执行器杀掉它启动的命令（整个进程组），不输出结果直接退出。
- 工作目录不能包含内核的配置目录和执行器本身，内核会在启动执行器之前检查。
- `nano-exec --probe` 输出本机的沙箱能力：`{"v":1,"landlock":7,"os":"Linux","arch":"x86_64","rg":true}`。
