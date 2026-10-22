# 内核 ⇄ 网关协议（v8）

- **传输**：TCP 或 Unix socket，由内核主动连网关。SSH 服务器不跑内核，网关自己通过 SSH 在上面运行执行器（见[执行器](#执行器)），不走这个协议。
- **版本**：v8 起 `hello` 带 `os` / `arch`，`input.workspaces` 每项带 `os`（linux / macos / windows），网关在机器列表里显示系统；路径属于它所在的机器，网关只保存、显示、转发，不解析。v7 起提供网关管理工具 `Gateway` / `GatewayAdmin`（只给管理员，修改每次审批）；v6 起所有 nano-agent 文件统一放在 `~/.nano-agent`（技能附带文件在 `~/.nano-agent/skills/<名字>/`，SSH 执行器在 `~/.nano-agent/nano-exec-<版本>`，内核在 `~/.nano-agent/kernel/`）。v5 起一个会话可以同时挂好几个工作目录（`input.workspaces`），技能放在网关上（`Skill` 交给网关），网关可以请内核代为执行（`run`）。版本不同的内核和网关不能互连，网关回 `error` 说明哪边太旧，旧内核用安装脚本重装即可。
- **编码**：一行一条 JSON（UTF-8，`\n` 结尾），字段 `t` 是消息类型，没用到的字段省略。
- **握手**：内核发 `hello` → 网关校验口令和版本 → 回 `welcome`；不通过就回 `error` 并断开。口令有两种：
  - 网关的共用口令（`kernel.token`），任何名字都能用，本机和自己配置的内核用它；
  - 配对得到的口令，只能用配对时的名字。
- **配对**：安装脚本装的内核没有口令。内核第一次连上来时 `hello` 带 `"pair": true`、不带 `token`，网关登记一个配对请求并回 `pairing`（带 6 位确认码，内核打印出来）；管理员在网页“设备”里核对确认码后允许，网关回 `paired`（带这台电脑专用的口令），内核把口令存到配置目录的 `paired.token`（0600），断开后马上用它重新连接。拒绝时回 `error`（`reason: denied`），内核退出；10 分钟没人处理也回 `error`，内核过一会儿重新申请。网关只保存口令的 SHA-256（`data/paired.json`），在网页里移除一台电脑，它的口令就作废；内核再连时收到 `error`（`reason: auth`），删掉 `paired.token` 重新申请。
- **心跳**：内核每 30 秒发 `ping`，网关回 `pong`。内核 90 秒、网关 120 秒没收到对方任何消息就断开。
- **多个内核**：网关可以同时连着好几个内核，按 `hello` 里的 `node` 区分（不能为空，不能有空白、冒号和斜杠，最长 64 个字）。同名的新连接会顶替旧连接，旧连接上进行中的任务全部作废。每个会话的一轮只在一个内核上跑，这一轮的 `approve`、`cancel`、`exec_result` 都发给那个内核；`ls`、`run`、`skill_put` 发给要用的那台电脑上的内核（本机和 SSH 服务器由网关自己处理）。
- **工作目录和执行**：一个会话可以挂好几个工作目录，可以在不同的机器上，`input.workspaces` 列出这一轮能用的。每个工具调用用参数 `workspace` 指明在哪个上执行（只挂了一个时可以不写），内核按它找到工作目录：`local` 的（在这个内核的机器上）内核自己执行；否则把调用用 `exec` 交给网关，网关在那台机器上执行（本机、SSH 服务器网关自己执行；别的电脑请它的内核用 `run` 代为执行），结果用 `exec_result` 回给内核。权限模式、审批由跑这一轮的内核管；沙箱的范围由网关生成，网关执行时不用内核给的路径。路径的规范形式（解析 `~`、符号链接、Windows 盘符大小写）由那台机器在执行器里给出，添加工作目录时网关用 `ls_result.path` 保存；网关不按自己的系统去解析别的机器的路径。本机只认 unix socket，或者用网关自己生成的额外口令（`AllowToken`）连上来的本机内核；从 127.0.0.1 用 TCP 连上来的别的机器（WSL 网关加 Windows 内核）不算本机。
- **加密**：TCP 本身不加密，跨机器请走 WireGuard、Tailscale 或 SSH 隧道。网关到 SSH 服务器的执行走 SSH。

## 消息

### 内核 → 网关

| t | 字段 | 含义 |
|---|---|---|
| `hello` | `v` `token` `pair?` `node` `os` `arch` `tools[]` `sandbox` | 握手。没有口令时 `token` 为空、`pair` 为 true，请求配对，报告工具、沙箱能力和系统。`tools` 每项是 `{name, description, input_schema}`（五个基础工具，`Skill` 由网关提供）；`sandbox` 是 `{landlock: N}`，0 表示没有沙箱；`os` 是 linux / macos / windows，`arch` 是 x86_64 / arm64 |
| `tools` | `tools[]` | 工具定义变了（旧内核用来报技能，现在网关忽略其中的 `Skill`） |
| `model` | `sid` | 请模型基于这个会话的上下文生成下一轮 |
| `result` | `sid` `id` `ok` `content` | 一次工具调用的结果，包括内核自己生成的失败结果（权限不足、超时、被拒……） |
| `approve_req` | `sid` `id` `name` `args` `from` | 这次调用需要管理员审批 |
| `notice` | `sid` `text` | 发给用户看的提示 |
| `done` | `sid` `status` `detail?` | 这一轮处理结束，`status` 见下表 |
| `ping` | | 心跳 |
| `ls_result` | `id` `ok` `path` `home` `blocked` `entries[]` `truncated?` `detail?` | `ls` 的结果：`path` 是解析过符号链接的绝对路径，`entries` 每项是 `{name, blocked?}`（只有子目录）；`blocked` 表示这个目录包含内核自己，不能当工作目录 |
| `exec` | `sid` `id` `name` `args` `workspace?` `elevated?` `timeout_ms` `max_output` | 请网关执行一次已经放行（审批过）的工具调用：工作目录 `workspace` 不在这个内核的机器上，或者 `Skill`（没有 `workspace`）。`args` 里已经去掉了 `workspace`；`elevated` 表示这次是提权写工作目录外 |
| `exec_cancel` | `sid` `id` | 这次调用超时了，或者这一轮被停止，请网关停下（之后到的 `exec_result` 内核直接丢弃） |
| `run_result` | `id` `ok` `content` | `run` 的结果（工具输出），或者 `skill_put` 的结果（`ok` 时 `content` 是放好的目录） |

### 网关 → 内核

| t | 字段 | 含义 |
|---|---|---|
| `welcome` | `v` | 握手成功 |
| `error` | `detail` `reason?` | 握手失败的原因，随后断开。`reason` 为 `auth` 表示口令不对（配对得到的口令失效了就重新配对），`denied` 表示管理员拒绝了配对（内核退出） |
| `pairing` | `code` | 收到配对请求，等管理员在网页上确认；`code` 是确认码 |
| `paired` | `token` | 管理员同意了配对：`token` 是这台电脑专用的口令，内核保存后断开重连 |
| `input` | `sid` `from` `mode` `elevate?` `workspaces[]` | 会话收到新消息，开始一轮处理。`mode` 是这一轮的权限模式（见下表）；`elevate` 表示这个身份能不能提权（访客为 false）。`workspaces` 每项是 `{name, dirs, ro, rw, home?, local, landlock, os}`：`name` 是给模型看的名字（机器:路径），工具参数 `workspace` 用它（也可以写序号、只写机器或只写路径，唯一时）；`dirs` 第一个是这个工作目录，后面是同一台机器上挂的别的工作目录（都能读写）；`ro` / `rw` 是额外的只读 / 读写路径（包括技能缓存目录；管理员读全盘用 `"/"`，Windows 执行器把它当成所有盘）；`home` 是没有工作目录时的 HOME 模式（dirs 为空，相对路径和 Shell 用 HOME）；`local` 表示在这个内核的机器上；`landlock` 是那台机器的沙箱版本（0 表示没有，自动模式下的 Shell 也要审批）；`os` 是那台机器的系统，内核用它决定路径怎么比较（Windows 不区分大小写）。没有工作目录的管理员拿到一个 HOME 工作目录；访客没有工具时 `workspaces` 为空、`mode` 为 `none` |
| `turn` | `sid` `reason` `calls[]?` `detail?` | 模型完成了一轮，`reason` 见下表；`calls` 每项是 `{id, name, args}` |
| `approve` | `sid` `id` `ok` `from` | 审批结果。`from` 是审批人身份；网关只会转发管理员的审批 |
| `cancel` | `sid` | 用户要求停止 |
| `pong` | | 心跳 |
| `ls` | `id` `path` | 列出内核所在机器上的一个目录（网页上选工作目录用），`path` 可以是 `~` 开头。只返回子目录名，不读文件内容 |
| `exec_result` | `sid` `id` `ok` `content` | `exec` 的结果：工具输出，或者 `Skill` 的说明 |
| `run` | `id` `req` | 请这个内核在它的机器上执行一次工具（工作目录在这台机器上，但这一轮由别的内核跑）。`req` 是交给执行器的完整请求（见[执行器](#执行器)），内核照样检查路径不包含自己 |
| `run_cancel` | `id` | 停下 `run`（不再回结果） |
| `skill_put` | `id` `skill` `files[]` | 把技能文件放到这台机器的 `~/.nano-agent/skills/<skill>/`（先清空）。`files` 每项是 `{path, data, exec?}`，`path` 是相对路径（不能有 `..`），`data` 是 base64。内核只肯写这个目录 |

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

谁用什么模式由网关决定（管理员用会话设置的模式，其他人用 `access.guest`）；内核负责按模式放行，执行器负责把工具关在工作目录里。`Write` / `Edit` / `Shell` 带 `elevated: true` 时，不管 mode 是什么都要审批（readonly / none 和访客直接拒绝）；批准后这一次调用的 `rw` 加 `/`。

## 网关管理工具

`Gateway`（查询）和 `GatewayAdmin`（修改，任何模式都审批）不在 `hello.tools` 里报描述：内核只在 `BUILTINS` 里登记名字和类型（`Gateway` 是只读，`GatewayAdmin` 是 `NA_TOOL_ADMIN`），模型看到的描述和 schema 由网关加。调用时内核和 `Skill` 一样，用 `exec` 交给网关（没有 `workspace`）；网关再查一次管理员身份后执行。修改直接走网页接口背后的函数，不重启。详见 [DESIGN-agent-gateway-admin.md](DESIGN-agent-gateway-admin.md)。

## 一次完整的处理

```
K→G {"t":"hello","v":8,"token":"…","node":"home-pc","os":"linux","arch":"x86_64","tools":[{"name":"Read","description":"…","input_schema":{…}},…],"sandbox":{"landlock":7}}
G→K {"t":"welcome","v":5}
G→K {"t":"input","sid":"tg:-1001234","from":"tg:10001","mode":"auto","workspaces":[{"name":"本机:~/projects/foo","dirs":["~/projects/foo"],"ro":["~/.gitconfig","~/.nano-agent/skills"],"rw":[],"local":true,"os":"linux"}]}
K→G {"t":"model","sid":"tg:-1001234"}
G→K {"t":"turn","sid":"tg:-1001234","reason":"tool_use","calls":[{"id":"call_1","name":"Shell","args":{"command":"date"}}]}
K→G {"t":"result","sid":"tg:-1001234","id":"call_1","ok":true,"content":"Tue Oct 20 21:15:03 CST 2026\n"}
K→G {"t":"model","sid":"tg:-1001234"}
G→K {"t":"turn","sid":"tg:-1001234","reason":"final"}
K→G {"t":"done","sid":"tg:-1001234","status":"ok"}
```

回复正文从头到尾不经过内核：网关收到 `done(ok)` 后，自己把最后一次 `final` 的内容发到聊天平台。

同时挂了本机的 `~/web` 和 SSH 服务器 `gpu` 的 `~/train`，本机的内核来跑：

```
G→K {"t":"input","sid":"main","from":"cli:local","mode":"auto","workspaces":[
       {"name":"本机:~/web","dirs":["~/web"],"ro":[…],"rw":[],"local":true},
       {"name":"gpu:~/train","dirs":["~/train"],"ro":[…],"rw":[],"landlock":7}]}
K→G {"t":"model","sid":"main"}
G→K {"t":"turn","sid":"main","reason":"tool_use","calls":[
       {"id":"c1","name":"Read","args":{"path":"README.md","workspace":"本机:~/web"}},
       {"id":"c2","name":"Shell","args":{"command":"nvidia-smi -L","workspace":"gpu"}}]}
                                       （c1 内核自己执行；c2 交给网关）
K→G {"t":"exec","sid":"main","id":"c2","name":"Shell","args":{"command":"nvidia-smi -L"},"workspace":"gpu:~/train","timeout_ms":610000,"max_output":65536}
G→K {"t":"exec_result","sid":"main","id":"c2","ok":true,"content":"GPU 0: …"}
K→G {"t":"result","sid":"main","id":"c1","ok":true,"content":"1\t# web …"}
K→G {"t":"result","sid":"main","id":"c2","ok":true,"content":"GPU 0: …"}
…
```

挂了好几个工作目录却没写 `workspace`、或者写的不存在时，内核直接给模型一个错误结果，列出挂载的工作目录。

## 约定

- **身份和会话 ID**：身份是 `平台:用户ID` 的形式，平台是 `tg`、`feishu`、`cli`、`web`，用来判断是不是管理员（网关配置里的 `access.admins`）。会话 ID 用来区分上下文，由网关决定，内核只把它当作不透明的字符串：主会话是 `main`，别人私聊机器人的会话是 `平台:聊天ID`，新建的会话是 `s-xxxxxx`。一个会话可以有好几个聊天窗口接着，同一时间只有一个 run。
- **一个会话同时只有一轮处理**：网关负责排队，上一轮没结束时来的新消息等它结束再处理；同一个人连发的几条会合并成一轮。
- **内核的保证**：一轮里模型请求的每个调用都恰好得到一个 `result`；全部有了结果才会发 `model`；每一轮都以 `done` 结束（断线时除外，断线后双方都丢弃进行中的任务）。
- **参数**：`args` 是 JSON 对象。模型给的参数不是合法 JSON 对象时，网关把原始文本作为 JSON 字符串放进 `args`，内核据此回一个"参数不合法"的错误结果，让模型自己纠正。
- **对不上的消息直接忽略**：比如超时之后才到的工具结果、已经取消的任务的 `turn`。

## 执行器

执行基础工具时每次启动一次 `nano-exec`，一问一答。工作目录在跑这一轮的内核的机器上时由内核启动；否则由网关安排：本机直接运行，SSH 服务器上复用网关的 SSH 主连接运行（执行器放在服务器的 `~/.nano-agent/nano-exec-<版本>`），别的电脑请它的内核运行（`run`）：

```
stdin  {"tool":"Shell","args":{"command":"ls"},"dirs":["/home/u/foo"],"write":true,"ro":[],"rw":[],
        "tmp_key":"tg:-1001234","max_output":65536,"watch_stdin":true}
stdout {"ok":true,"content":"a.txt\nb.txt\n"}
```

- 执行器先套上沙箱（Landlock + seccomp），再执行工具；沙箱启动失败就不执行。
- 请求写完后 stdin 保持打开。stdin 被关闭或收到 SIGTERM 表示取消：执行器杀掉它启动的命令（整个进程组），不输出结果直接退出。
- 工作目录不能包含内核的配置目录和执行器本身，内核会在启动执行器之前检查；执行器自己也拒绝包含它自己的目录。
- `nano-exec --probe` 输出本机的沙箱能力：`{"v":1,"landlock":7,"os":"Linux","arch":"x86_64","rg":true}`。

`Skill` 由网关处理：技能放在网关的技能目录里（`skills.dir`，默认是网关配置文件旁边的 `skills/`），网关读 `SKILL.md` 返回说明，并把附带文件放到这一轮挂载的每台机器的 `~/.nano-agent/skills/<技能名>/`（本机、SSH 服务器网关自己放，别的电脑用 `skill_put`）。每台机器上都是同一个路径，每次执行的 `ro` 都包含 `~/.nano-agent/skills`。
