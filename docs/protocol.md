# 内核 ⇄ 网关协议（v11）

当前 v11：模型、工具、审批和取消使用完整 `(sid, run_id, turn_id, id)` 身份；`hello.features` 按能力协商有界并行、调用身份和执行隔离策略。旧协议演进说明如下；新字段见本文各扩展说明。

- **传输**：TCP 或 Unix socket，由内核主动连网关。SSH 服务器不跑内核，网关自己通过 SSH 在上面运行执行器（见[执行器](#执行器)），不走这个协议。
- **版本**：v9 起 `hello` 带 `version`（内核的发布版本）和 `update_failed`（上次自动更新失败、已经退回的版本），网关可以发 `update` 让内核换成新版本（见下面「内核更新」）。**从 v9 起，网关遇到协议版本比自己旧、但不低于 9 的内核时，应先推 `update` 把它升上来，而不是直接拒绝**。v8 起 `hello` 带 `os` / `arch`，`input.workspaces` 每项带 `os`（linux / macos / windows），网关在机器列表里显示系统；路径属于它所在的机器，网关只保存、显示、转发，不解析。v7 起提供网关管理工具 `Gateway` / `GatewayAdmin`（只给管理员，修改每次审批）；v6 起所有 nano-agent 文件统一放在 `~/.nano-agent`（当前技能源文件在 `skills/`，执行缓存在 `skill-cache/`，SSH 执行器在 `~/.nano-agent/nano-exec-<版本>`，内核在 `~/.nano-agent/kernel/`）。v5 起一个会话可以同时挂好几个工作目录（`input.workspaces`），技能放在网关上（`Skill` 交给网关），网关可以请内核代为执行（`run`）。版本不同的内核和网关不能互连，网关回 `error` 说明哪边太旧，旧内核用安装脚本重装即可。
- **编码**：一行一条 JSON（UTF-8，`\n` 结尾），字段 `t` 是消息类型，没用到的字段省略。
- **握手**：内核发 `hello` → 网关校验口令和版本 → 回 `welcome`；不通过就回 `error` 并断开。口令有两种：
  - 网关的共用口令（`kernel.token`），任何名字都能用，本机和自己配置的内核用它；
  - 配对得到的口令，只能用配对时的名字。
- **配对**：安装脚本装的内核没有口令。内核第一次连上来时 `hello` 带 `"pair": true`、不带 `token`，网关登记一个配对请求并回 `pairing`（带 6 位确认码，内核打印出来）；管理员在网页上（拓扑图「＋ 接入机器」节点或弹出的提示）核对确认码后允许，网关回 `paired`（带这台电脑专用的口令），内核把口令存到配置目录的 `paired.token`（0600），断开后马上用它重新连接。拒绝时回 `error`（`reason: denied`），内核退出；10 分钟没人处理也回 `error`，内核过一会儿重新申请。网关只保存口令的 SHA-256（`data/paired.json`），在网页里移除一台电脑，它的口令就作废；内核再连时收到 `error`（`reason: auth`），删掉 `paired.token` 重新申请。
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
| `model` | `sid` `run_id` `turn_id` | 请模型基于这个会话的上下文生成下一轮 |
| `result` | `sid` `run_id` `turn_id` `id` `ok` `content` `sandbox?` | 一次工具调用的结果，包括内核自己生成的失败结果（权限不足、超时、被拒……） |
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
| `skill_put` | `id` `skill` `files[]` | 把技能文件放到这台机器的 `$NANO_HOME/skill-cache/<skill>/`（默认 `~/.nano-agent/skill-cache/`，先清空；skill 可带内容版本后缀）。`files` 每项是 `{path, data, exec?}`，`path` 是相对路径（不能有 `..`），`data` 是 base64。内核只肯写这个目录 |

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
K→G {"t":"hello","v":9,"token":"…","node":"home-pc","os":"linux","arch":"x86_64","version":"0.1.0","tools":[{"name":"Read","description":"…","input_schema":{…}},…],"sandbox":{"landlock":7}}
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

- 执行器按最终 auto/on/off 策略准备执行隔离；on 无法满足时失败，off 跳过施加。应用层工具权限独立。
- 请求写完后 stdin 保持打开。关闭 stdin 表示取消：清理命令进程树；旧模式不输出结果，流式模式给出 cancelled 终态。强制杀死执行器、未收到终态时不能据退出码猜测命令成功。
- 工作目录不能包含内核的配置目录和执行器本身，内核会在启动执行器之前检查；执行器自己也拒绝包含它自己的目录。
- `nano-exec --probe` 输出本机的沙箱能力：`{"v":2,"job_stream":1,"landlock":7,"os":"linux","arch":"x86_64","rg":true,"sandbox_modes":["auto","off","on"]}`。

`Skill` 由网关处理：技能放在网关的技能目录里（`skills.dir`，默认是网关配置文件旁边的 `skills/`），网关读 `SKILL.md` 返回说明，并把附带文件放到各执行机器的独立 `skill-cache/<技能名>-<内容哈希>/`（本机、SSH 服务器网关自己放，别的电脑用 `skill_put`）。回执列出每台机器上的实际完整路径；不能假定 HOME 或 NANO_HOME 一样。缓存不覆盖全局/agent 技能源文件，每次执行只读可见。

## 内核更新（v9）

```
G→K {"t":"update","version":"0.1.1","files":[{"path":"nano-kernel","data":"<base64>"},{"path":"nano-exec","data":"<base64>"},{"path":"rg","data":"<base64>"}]}
K→G {"t":"update_result","ok":true,"version":"0.1.1"}
```

- `files[].path` 只能是 `nano-kernel`、`nano-exec`、`rg`（Windows 上带 `.exe`），必须有内核和执行器。内核和执行器要在同一个目录（安装脚本装的布局），否则回 `ok:false`。
- 内核先把所有文件写成 `.new`，再把旧的改名成 `.old`、新的改名过来，写下 `update-pending.json`，回 `update_result` 后重启自己（POSIX 上 exec 同样的参数；Windows 上以退出码 75 退出，由 `run-kernel.ps1` 重新拉起）。
- 新版本启动后 90 秒内被网关接受（收到 `welcome`）就算成功；启动超过 3 次或 90 秒内没被接受，就把 `.old` 换回来，写下 `update-failed`（之后的 `hello` 带 `update_failed`），再重启成旧版本。

### v0.3 网关工具和定时运行（协议 v9 的可选字段）

- input 可带 run_timeout_ms、approval_timeout_ms（毫秒）；缺省/0 仍用内核配置。定时任务默认都为 1800000。
- v0.4.0 起网关每一轮都带上自己的设置（网页拓扑图的 Agent 节点里改）：`max_steps`（最多请求模型几次）、`run_timeout_ms`、`approval_timeout_ms`，大于 0 是上限，小于 0 表示不限；`max_output`（工具结果最多保留的字节数，1024–64 MB），内核用在本机执行器和交给网关的 `exec` 上。kernel.conf 里的同名项只在连着旧网关、input 里没有这些字段时兜底。
- WebSearch/WebFetch 是公开只读策略：含 none/readonly 在内均可执行，内核通过 exec 交给网关，不附工作目录，不接收搜索密钥。
- Schedule 只允许管理员（input.elevate=true）。list/run_now 走只读策略，create/update/delete 走写策略：ask 审批、auto 直接、readonly 拒绝。网关再次验证身份和任务权限上限。
- GatewayAdmin 的 history_delete 在 readonly 下也要求审批，其他管理动作沿用既有规则。

### v11 执行隔离策略能力 2（v0.9.0）

hello.features.sandbox_policy=2 支持 workspace 覆盖和逐次执行状态。全局 sandbox.mode、agent.sandbox_mode、workspace.sandbox_mode 按顺序解析；会话不增加临时覆盖。input.sandbox_mode 为 agent 解析值，workspaces[].sandbox_mode 为冻结的工作目录请求；workspaces[].sandbox_required 是目标节点部署要求，供协调内核在执行前判断。工具参数不能覆盖策略。部署 on 强制施加；部署 off 与请求 on 冲突时失败。

result / exec_result / run_result 可带 sandbox 对象：requested（原请求）、mode（目标节点最终执行策略）、effective（full / partial / none / unknown）、reason（原因）。此对象来自执行器结果，和 hello 的能力探测分开。核心只在完整调用身份匹配后转发，不改变模型工具正文。Gateway 保存到原始历史并用于工具卡片显示，不将其作为模型参数。

旧执行器未声明 sandbox_modes 时，只兼容 auto；显式 on/off 在执行前失败。旧协调内核不支持所需覆盖时，在模型启动前要求更新；不假装执行了新策略。外部副作用和权限仍以实际执行系统为准，关闭隔离不授予访客工具权限或管理员身份。

### 调用展示和未接受提案（v0.9.0）

Gateway 的工具、排队、开始、结果、附件及审批事件携带 run_id / turn_id / id；原始 assistant 和 tool 历史保存相同身份。CLI、飞书卡片、Telegram 过程消息使用可选 ToolEvent 接口，乱序结果只更新对应调用；旧 RunView 通过文字适配，终态附调用标识。网页使用相同身份索引历史和实时状态，支持模型在下一轮重用 call ID。

C hello.features.max_calls_per_turn 声明批次数量上限（当前 32）。Gateway 在保存正式 tool_calls、发布执行事件前验证数量及 ID；空、重复或过长 ID 整批拒绝，不执行一部分。超出 C 协议硬边界的帧也整批拒绝，不能截取前面若干条执行。真实模型用量仍然结算。

未接受提案（生成后插话重规划、提案结构无效）可保存到原始消息的 proposed_calls，网页可展开检查；此字段不转换成模型 tool_calls，不伪造执行失败结果。执行结果仍按实际完成顺序写入。工具说明按 bounded_parallel 能力注入稳定系统前缀，prefixVer=9。

平台流式缓存有界：正文和思考各 128 KiB、32 条附注、128 个工具条目；优先回收已经完成的条目，保留在途调用身份。截断指向网页完整原始历史，不改变模型的真实工具结果。


### 机器执行租约（machine_leases=1）

C 节点的 hello 增加随机 node_epoch（每次进程启动唯一），features 报 machine_leases=1、
max_execs 和 max_parallel_shells。Gateway 的 welcome.features.machine_leases=1 才启用；
旧网关继续走既有节点硬上限。新网关的恢复存储损坏时明确拒绝执行，不降级绕过租约。

每次连接先 lease_sync：携带 node_epoch 和 leases 完整清单（最多 256 条），包括仍运行/停止中
的执行器以及尚未启动的本机请求。Gateway 可靠保存核对结果，回 lease_synced(ok)，随后才允许
新 input 和 lease_acquire。重启恢复将未结算租约视为 unknown，保留真实槽和锁；不重放命令。
非 Job 的旧前台执行保持完整清单的结算规则；持久 Job 必须按下面的固定身份与终态回执核对。来源不符或损坏的恢复文件保留并显示错误。

每个 lease_acquire 带 lease_id、node_epoch 及完整外层调用身份，lease 包含
sid/run_id/turn_id/call_id/agent/name/priority/res_keys/res_xkeys，代执行另有 host_id。
lease_id 由节点启动身份和递增序号组成；Gateway 以实际活动 run 或已登记的 host 请求核验来源，
不信任节点转抄的 agent、优先级或调用归属。资源来自执行宿主对实际路径的规范化结果；
共享/独占键仍使用有界 CSV。Gateway 映射同机节点到共同 machine，处理层级目录和 Windows 大小写。

lease_queued(detail) 显示等待资源及占用者；lease_grant 只在持久化成功后发出。节点收到授权仍须
满足自身硬槽、Shell 槽和资源检查；真实创建进程后发 lease_started，结束并回收进程/输出后发
lease_release。lease_cancel 只表示正在停止，不能释放仍在执行的资源。重复申请按不可变身份幂等；
已结算编号不能再次执行。等资源最多 5 分钟；队列和检查点都有数量及字节边界。

exec_queued 是协调内核对 Gateway 排队的确认，停止派发等待时钟并释放 run 运行槽；
exec_started 才开始执行时限，tool_started 向平台展示完整调用身份。代执行用 run_started(id)
确认真实进程启动。排队调用的启动失败/拒绝可以直接给终态失败，不伪造 started。

Gateway 直接/SSH 调用及 MCP/网络/网关工具也参与统一 per-run 仲裁。MCP 默认每个服务独占，
网络操作另有有界槽；网关状态操作按 agent 串行。不能从直接执行器取得启动前的可信路径域时，
保守占用 machine:files 独占锁，不能用未经核实的路径别名缩小资源。SSH 不明确的连接丢失结果
保留 unknown，不把无法核实的远端进程当成已经停止。MCP 客户端 RPC 已结束只结算本次请求，
不宣称外部副作用被撤回或恰好一次。

运行限额在 runtime 配置段中定义，网页设置和示例同步；修改需重启。普通消息队列每会话最多
32 条、网关总计 256 条/8 MiB（持久插话另有自己的边界）；达到内核活动 run 上限时留在网关，
按会话轮转，低优先级随等待时间提升。所有模型/工具轮次仍由 C core 决定。

### 执行器 v2 增量 Shell 协议

probe 的 job_stream=1 表示支持显式请求 stream=1（省略或 0 继续旧的一问一答）。
Shell 请求同时提供不超过 128 字节的 job_id；执行器在实际创建命令进程后输出 started，
stdout/stderr 分别读取，按不超过 8 KiB 的原始块进行 base64 编码，每个 JSON 行不超过 16 KiB。
命令打印的 JSON 也只作为输出数据，不能变成执行器控制消息。所有帧带 job_id 和从 1 单调递增的 seq。

唯一 exit 帧包括 ok/content/exit_code/signal/timed_out/cancelled/sandbox_effective；
content 是有界预览，原始保留量最多 2 KiB，完整输出通过 output 帧读取。准备失败可直接给失败 exit，
不能给 started 或成功 exit。流式超时默认 1800 秒；可信请求的 shell_timeout_max 限制最大秒数
（缺省 86400），timeout=0 只有 allow_no_timeout=true 才允许。旧模式的 120/600 秒规则保持。

C 增量解码器逐帧验证身份、序号、状态顺序、base64 和帧大小，不累计完整执行器输出。
不完整终态、重复终态、输出越界、成功状态与取消/超时/退出码矛盾都标为协议失败。
runner 的输出上限在每次启动前冻结；一个大输出调用不会扩大其他任务的上限。
这项执行器能力不单独表示节点已经提供 JobManager 或后台控制能力，节点须分别协商。


### 后台租约移交准备与提交

节点另外声明 lease_detach=1；Gateway 在 welcome 同名能力确认后，使用
lease_detach_prepare / lease_detach_commit / lease_detach_abort。每条请求带 node_epoch、
lease_id 和 request_id，回 lease_detach_result（phase=prepare/commit/abort、ok、detail）。

prepare 可靠预留 agent 后台容量，原执行仍占前台 run 预算和全部机器资源；节点保存自己的
后台所有权成功后才 commit。commit 才将等待关系转为后台并释放前台预算；保存失败用 abort
撤销预留，仍保留原命令与前台等待。整个过程不释放真实进程槽或文件锁，重复提交幂等，不能用
错误请求号或另一节点提交。未协商能力、存储不可用和队列满都给匹配的失败响应，不能让节点误认成功。

### C 节点任务所有权（job_manager=1）

节点只有成功恢复本地 Job 存储且执行器支持 job_stream=1 才报 job_manager=1 / lease_detach=1。
welcome 必须同时确认 job_manager、lease_detach 和 machine_leases，才为 Shell 使用持久任务链；
不支持的组合拒绝显式 background/auto，不把后台请求悄悄当成长前台调用执行。

每个 Shell 在申请执行租约之前保存 queued；取得授权且满足本机真实槽/锁之后保存 starting，再启动
同一个流式执行器。真实 started 才开始前台执行时限。原生工具使用完整 run/turn/call 身份，
代执行等待使用独立 wait_id；原始 origin 和 created_by 固定保存。Job 存储按配置文件旁的
`<配置文件名去扩展名>.jobs/<job_id>/meta.json` 隔离，不复用聊天历史。

节点把本地后台所有权保存后发送 lease_detach_commit；原前台只在提交 ACK 后收到一个
status=backgrounded 的结果。任务如果在提交 ACK 前退出，仍等待已保存的移交完成，再产生唯一原调用
结果；独立终态通过 job_state 发送。容量拒绝/保存失败保持原进程与前台等待。断线保留已移交的运行
和排队任务；重新连接先 lease_sync，再补 jobs_sync。节点主动重启回收进程，未启动恢复为
interrupted，已跨启动边界恢复为 unknown，不重放命令；unknown 保留执行资源占用等待对账。

job_control 带 request_id（1–128 字节）、action 和可选 job_id。节点提供
list/status/output/cancel/detach；output 的 cursor 为绝对原始字节位置，max_bytes 上限 65536，
每个 Job 同时最多一个日志读取，回 job_state(request_id,ok,job,job_data,detail)。
job_data 是按动作定义的对象或数组，和旧管道消息的字符串 data 字段分开：
list 返回 Job 数组；output 返回 chunks/next_cursor/output_cursor/truncated_before/gap，
chunk 含 cursor/next_cursor/stream/data（base64 原始字节）。Go 与 C 使用相同字段并通过真实往返校验。
普通生命周期 job_state 包含 job_id、kind、job；jobs_sync 包含有界 jobs 清单。
Gateway 只有同时具备可靠任务索引且目标支持 job_stream、machine_leases、lease_detach 时才确认 job_manager；
缺任一环节继续使用原有前台执行。
公共任务状态不包含冻结的原命令和执行参数；内部 lease 回执保留完整身份与资源域供网关核验。
这些节点控制帧由认证的 Gateway 转交；用户权限仍必须由 Gateway 按原始 agent、发起者和可见范围核验。

新任务的初始元数据最多 28 KiB，准入时为终态帧、转义输出摘要及生命周期字段预留空间；
公共 output_tail 最多 2 KiB 原始字节，完整日志仍由游标读取，不能用较长尾部把可靠终态挤出 64 KiB 元数据上限。
终态先持久保存，再按 event_seq=1 至少一次发送；events_ack(job_id,event_seq=1) 保存
notification_state=delivered，重复 ACK 幂等。未确认终态每 5 秒或重连后补发，不推动模型轮询。
节点恢复失败在 hello/lease_sync 的 job_storage_error 中明确报告，Gateway 拒绝清空已有占用。
Gateway 的 Job 索引/订阅/用户命令须分别实现后才协商开启该能力；流式解码或节点控制测试不代表
渠道用户路径已经交付。

### Job 租约的重连与跨启动核对

lease 的 job_id 固定于首次创建，和原 lease_id 的节点启动身份绑定；后来不能换 Job、调用身份、
agent 或缩小资源域。租约恢复同样保留 Job 身份。节点重启后使用当前 node_epoch 认证连接，
旧 Job 的租约 ID 继续保留原编号，只允许作为 unknown 事实或可靠终态回执认领到当前连接。

lease_sync 的 leases 是当前占用（最多 256 条）；独立 lease_receipts 是持久终态证明
（最多 256 条），包含 terminal_receipt=true、job_id、完整原调用与资源，以及
succeeded/failed/cancelled/timed_out/interrupted。unknown 不能作为释放资源的终态证明。
重复终态幂等，已结算的编号不能通过旧活动清单复活。Gateway 先验证整份清单，再一起持久发布；
任一身份或资源冲突都不会部分修改旧占用。

未出现在清单的 Job 保留 unknown 和锁，不能仅靠“节点换了启动编号”认定命令已经退出。
节点仍在保存后台移交时，在活动租约里带 detach_request=job_id；Gateway 保留匹配的容量预留。
已保存 background=true 完成先前预留；明确仍为前台且没有待保存移交才撤销未使用预留。
这样 Gateway 重启或断线不会在节点状态保存中途释放前台预算或误收回后台名额。

C 节点 Job 元数据最多 256 条；达到 240 条时，单个 I/O 工作者按创建时间回收已 ACK 的确定终态，
留至 224 条，预留 32 个名额。移除完成后才释放对应内存身份；日志读取中、未 ACK、unknown 的记录
不会回收。达到上限且没有可安全回收项时拒绝新任务，不能删除待确认事件或未知执行来腾位置。
回收选择只读取已发布的有界元数据，不为轮询扫描复制完整输出尾部；删除失败显示错误并退避重试。
Gateway 应在可靠事件落盘后 ACK，而不是等待模型轮询；Gateway 保留的历史索引可明确显示节点日志已回收。

### Gateway 可靠 Job 受理与确认

Job 初次登记必须核验实际 run/turn/call 或 Gateway host wait，包括固定 agent、created_by、
silent、完整资源域及本次启动的 lease_id/job_id。仅汇报一个节点状态不能取得聊天会话权限。
Job 索引与终态预留先写同一可靠 journal，完成回调之后才能进入机器租约调度；
此时收到的 lease_acquire 有界等待，不在 App 总锁里同步磁盘。

终态状态和完成事件作为一个事务同步后才发 events_ack。重复回执可重复确认，事件身份固定；
确认表示事件可靠入库，Hook 或外部消息发送另有投递记录，不代表外部副作用恰好一次。
恢复已有任务沿用固定来源；缺少旧索引且来源调用已经失效的副本不激活，保留节点回执并显示问题。
