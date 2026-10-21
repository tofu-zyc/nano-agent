# 内核 ⇄ 网关协议（v3）

- **传输**：TCP 或 Unix socket，由内核主动连网关；网页管理的 SSH 连接使用 `gateway = stdio`，网关主动建立 SSH，再将其 stdin/stdout 接入同一 Hub，协议消息保持不变。
- **编码**：一行一条 JSON（UTF-8，`\n` 结尾），字段 `t` 是消息类型，没用到的字段省略。
- **握手**：内核发 `hello` → 网关校验口令和版本 → 回 `welcome`；不通过就回 `error` 并断开。口令有三种：
  - 网关的共用口令（`kernel.token`），任何名字都能用，本机和自己配置的内核用它；
  - 配对得到的口令，只能用配对时的名字；
  - 网关自己通过 SSH 启动的内核（stdio）不查口令，服务器上不留网关的口令。
- **配对**：电脑启动包里没有口令。内核第一次连上来时 `hello` 带 `"pair": true`、不带 `token`，网关登记一个配对请求并回 `pairing`（带 6 位确认码，内核打印出来）；管理员在网页“设备”里核对确认码后允许，网关回 `paired`（带这台电脑专用的口令），内核把口令存到配置目录的 `paired.token`（0600），断开后马上用它重新连接。拒绝时回 `error`（`reason: denied`），内核退出；10 分钟没人处理也回 `error`，内核过一会儿重新申请。网关只保存口令的 SHA-256（`data/paired.json`），在网页里移除一台电脑，它的口令就作废；内核再连时收到 `error`（`reason: auth`），删掉 `paired.token` 重新申请。
- **心跳**：内核每 30 秒发 `ping`，网关回 `pong`。内核 90 秒、网关 120 秒没收到对方任何消息就断开。
- **多个内核**：网关可以同时连着好几个内核，按 `hello` 里的 `node` 区分（不能为空，不能有空白、冒号和斜杠，最长 64 个字）。同名的新连接会顶替旧连接，旧连接上进行中的任务全部作废。每个会话的一轮只在一个内核上跑，这一轮的 `approve`、`cancel` 都发给那个内核；`ls` 发给要浏览的那台机器上的内核。
- **加密**：TCP 本身不加密，跨机器请走 WireGuard、Tailscale 或 SSH 隧道；管理的 SSH stdio 连接由 SSH 加密。
- **SSH stdio 生命周期**：stdout 只承载 NDJSON，日志写 stderr；两个管道分别非阻塞读写，仍由同一个 poll 循环处理。stdin EOF 时重置状态、终止工具并退出，不尝试在已断开的管道上重连。Hub.Attach 将此类连接标记为远程，避免被选为本机默认节点。

## 消息

### 内核 → 网关

| t | 字段 | 含义 |
|---|---|---|
| `hello` | `v` `token` `pair?` `node` `tools[]` `skills[]` `sandbox` `remotes[]` | 握手。没有口令时 `token` 为空、`pair` 为 true，请求配对，报告工具和沙箱能力。`tools` 每项是 `{name, description, input_schema}`；`skills` 每项是 `{name, description}`；`sandbox` 是 `{landlock: N}`，0 表示没有沙箱；`remotes` 是这个内核能通过 SSH 执行工具的服务器，每项是 `{name, target, ok, landlock, detail?}`，`ok` 为 false 时 `detail` 是原因（连不上、还在准备执行器……） |
| `tools` | `tools[]` `skills[]` | 技能列表变化后重新报告完整工具定义和技能简介，网关替换该内核的工具列表，后续模型请求立即使用；删除全部技能时 `skills` 是空数组，`tools` 只含五个基础工具 |
| `remotes` | `remotes[]` | 服务器能不能用变了（准备好了、连不上了），格式同 `hello` |
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
| `error` | `detail` `reason?` | 握手失败的原因，随后断开。`reason` 为 `auth` 表示口令不对（配对得到的口令失效了就重新配对），`denied` 表示管理员拒绝了配对（内核退出） |
| `pairing` | `code` | 收到配对请求，等管理员在网页上确认；`code` 是确认码 |
| `paired` | `token` | 管理员同意了配对：`token` 是这台电脑专用的口令，内核保存后断开重连 |
| `input` | `sid` `from` `mode` `dirs[]` `ro[]` `rw[]` `node?` | 会话收到新消息，开始一轮处理。`mode` 是这一轮的权限模式（见下表），`dirs` 是工作目录（第一个是默认目录），`ro` / `rw` 是额外的只读 / 读写路径；`node` 是这个内核报告的某台服务器的名字时，这一轮的工具通过 SSH 在那台服务器上执行，路径都是服务器上的，有没有沙箱也按服务器算 |
| `turn` | `sid` `reason` `calls[]?` `detail?` | 模型完成了一轮，`reason` 见下表；`calls` 每项是 `{id, name, args}` |
| `approve` | `sid` `id` `ok` `from` | 审批结果。`from` 是审批人身份；网关只会转发管理员的审批 |
| `cancel` | `sid` | 用户要求停止 |
| `pong` | | 心跳 |
| `ls` | `id` `path` `node?` | 列出内核所在机器（或者 `node` 指定的服务器）上的一个目录（网页上选工作目录用），`path` 可以是 `~` 开头。只返回子目录名，不读文件内容 |

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
K→G {"t":"hello","v":3,"token":"…","node":"home-pc","tools":[{"name":"Read","description":"…","input_schema":{…}},…],"sandbox":{"landlock":7}}
G→K {"t":"welcome","v":3}
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

- **身份和会话 ID**：身份是 `平台:用户ID` 的形式，平台是 `tg`、`feishu`、`cli`、`web`，用来判断是不是管理员（网关配置里的 `access.admins`）。会话 ID 用来区分上下文，由网关决定，内核只把它当作不透明的字符串：主会话是 `main`，别人私聊机器人的会话是 `平台:聊天ID`，新建的会话是 `s-xxxxxx`。一个会话可以有好几个聊天窗口接着，同一时间只有一个 run。
- **一个会话同时只有一轮处理**：网关负责排队，上一轮没结束时来的新消息等它结束再处理；同一个人连发的几条会合并成一轮。
- **内核的保证**：一轮里模型请求的每个调用都恰好得到一个 `result`；全部有了结果才会发 `model`；每一轮都以 `done` 结束（断线时除外，断线后双方都丢弃进行中的任务）。
- **参数**：`args` 是 JSON 对象。模型给的参数不是合法 JSON 对象时，网关把原始文本作为 JSON 字符串放进 `args`，内核据此回一个"参数不合法"的错误结果，让模型自己纠正。
- **对不上的消息直接忽略**：比如超时之后才到的工具结果、已经取消的任务的 `turn`。

## 执行器

内核执行基础工具时每次启动一次 `nano-exec`（远程主机上通过 SSH 启动），一问一答：

```
stdin  {"tool":"Shell","args":{"command":"ls"},"dirs":["/home/u/foo"],"write":true,"ro":[],"rw":[],
        "tmp_key":"tg:-1001234","max_output":65536,"watch_stdin":true}
stdout {"ok":true,"content":"a.txt\nb.txt\n"}
```

- 执行器先套上沙箱（Landlock + seccomp），再执行工具；沙箱启动失败就不执行。
- 请求写完后 stdin 保持打开。stdin 被关闭或收到 SIGTERM 表示取消：执行器杀掉它启动的命令（整个进程组），不输出结果直接退出。
- 工作目录不能包含内核的配置目录和执行器本身，内核会在启动执行器之前检查。
- `nano-exec --probe` 输出本机的沙箱能力：`{"v":1,"landlock":7,"os":"Linux","arch":"x86_64","rg":true}`。

`Skill` 由内核读取技能目录，返回说明正文和附带文件的位置，不经过执行器。本机工具的 `ro` 自动包含技能目录；远程时先通过 SSH 上传所选技能，再返回服务器上的 `~/.cache/nano-agent/skills/…` 路径，并把对应缓存目录加入后续工具请求的 `ro`。
