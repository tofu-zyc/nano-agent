# 设计稿：让 agent 管理网关（自举）

状态：**评审中（先定设计，不动代码）**  
最后更新：2026-10-22  
相关：[HANDOFF.md](HANDOFF.md)、[ARCHITECTURE.md](ARCHITECTURE.md)、[DESIGN-ui-interactions.md](DESIGN-ui-interactions.md)

目标场景：管理员在网页或终端里说“帮我把 debian-server 接上，用户名 debian，项目在 /home/debian/app”，agent 能完成添加 SSH 连接、连接服务器、添加并挂载工作目录，然后报告结果。密码不经过模型。

## 1. 现在为什么做不到

按实际执行链，从外到内有这些阻塞：

1. **没有工作目录时，模型没有任何工具。** 网关在 `startRun` 里看到没有挂载的工作目录，会设置 `r.noTools = true` 并把这一轮的模式改成 `none`（`app.go`），请求模型时也不带 tools（`app.go`）。所以“只聊天”的会话连查状态都做不到，更不用说管理网关。
2. **有工作目录时，也没有管理工具。** 内核只提供 Read / Write / Edit / Grep / Shell，技能 `Skill` 只是知识；`skills/device-connections/SKILL.md` 还明确写着“不给你网关的管理权限，告诉用户在网页上点哪里”。
3. **沙箱阻止了文件路线。** 工作目录不能包含网关配置目录和数据目录（`workspace.go` 的 `checkDir`、`host/local.go` 的 guard）；执行器的 HOME 是真实 HOME，但 `.ssh`、`gateway.toml`、`data/` 都不在 Landlock 允许范围内。沙箱还禁了 AF_UNIX（`kernel/src/sandbox.c`），所以也不能通过网关的 unix socket 调 API。
4. **就算能改文件也不对。** `data/connections.json` 只在网关启动时读一次（`host/manager.go` 的 `New`）；运行中的 `host.Manager` 还有内存里的连接状态、密码、ControlMaster、重连和状态推送。直接改文件需要重启网关，而且会绕过校验。
5. **在沙箱里自己跑 ssh 不是同一个东西。** 网关的 SSH 管理器负责密码 askpass、主机密钥、ControlMaster、架构探测、执行器上传/现编、重连；沙箱里没有 `~/.ssh`、没有 ssh-agent 的 unix socket、没有交互 TTY，连 known_hosts 也写不了。
6. **连接成功后还缺一步。** 设备连接和工作目录是两套操作；agent 没有“添加/挂载工作目录”的工具，只能让用户点网页或自己发 `/dir`。
7. **没有审批通道。** 现有审批只覆盖内核认识的文件/命令工具；一个“网关管理”操作没有对应的 tool policy，也没有“这是管理员操作”的确认。

结论：**沙箱没有做错。** 它保护的是 API key、聊天记录和本机密钥；把这些目录放给 agent 才是设计错误。缺的是一条**受控的能力通道**：不是更多文件权限，而是网关把少数管理动作暴露成有审批的工具。

## 2. 设计原则

1. **给能力，不给文件。** agent 通过工具表达意图（“加一台 SSH 服务器”），不读写 `gateway.toml` / `data/*.json`。
2. **白名单动作。** 不提供通用的“改配置”或 `config_set`；每个动作有参数校验、明确的副作用和错误。
3. **管理员 + 每次审批。** 只有管理员会话能看到管理工具；每个修改动作都弹审批，展示具体目标。只读查询可以不走审批，但同样只给管理员。
4. **密码/密钥不过模型。** 不把密码放进工具参数、历史、日志或工具结果。密码由审批卡片里的密码框直接交给网关，或沿用网关已有的密钥/ssh-agent。
5. **一步完成常见路径。** 用 `ssh_setup` 把“添加 + 连接 + 加工作目录 + 挂载”合成一次操作；同时保留原语用于排错。
6. **异步状态可见。** 连接是异步的，工具要等待就绪或返回明确状态，不能只说“已提交”。

## 3. 工具设计

### 3.1 两个网关工具

| 工具 | 权限 | 用途 |
| --- | --- | --- |
| `Gateway` | 只读（内核 `NA_TOOL_READ`） | 查看网关、连接、工作目录、可用 SSH 别名；不含密码/密钥 |
| `GatewayAdmin` | 新增 `NA_TOOL_ADMIN`，任何模式都审批 | 增删/连接 SSH 服务器，添加/挂载工作目录 |

两个工具都由网关提供描述和 schema（像 `Skill` 一样，内核只有策略，不把描述写死），只在**管理员会话 + 内核声明支持**时放进模型工具列表。

`Gateway` 的动作：

- `status`：网关运行时间、在线内核、默认模型（只有名字）、机器列表摘要。
- `ssh_list`：连接名、主机、用户、端口、状态、是否保存密码；**永远不返回密码**。
- `ssh_hosts`：网关 `~/.ssh/config` 里的别名（只有名字，供填写 host）。
- `workspace_list`：工作目录列表和当前会话挂载状态。

`GatewayAdmin` 的动作：

| 动作 | 参数 | 行为 |
| --- | --- | --- |
| `ssh_setup` | `name`、`host`、`port?`、`user?`、`identity_file?`、`path?`、`mount?` | 保存连接 → 连接 → 等就绪 → 可选添加并挂载 `path` |
| `ssh_connect` | `name`、`wait_seconds?` | 连接已有服务器并等待状态 |
| `ssh_disconnect` | `name` | 断开，但保留配置 |
| `ssh_delete` | `name` | 删除配置和网关侧的认证文件；连接中先要求断开 |
| `workspace_add` | `host`、`path`、`mount?`、`default?` | 添加工作目录，可选挂到当前会话/设为默认 |
| `workspace_mount` | `host`、`path` 或工作目录名 | 把已有工作目录挂到当前会话/取下 |

第一版不做：修改 `gateway.toml` 任意字段、改 web 密码、改 access 名单、装/删软件、执行任意命令。模型/渠道配置以后按同样的白名单方式加。

### 3.2 内核改动

- `na_tool_kind` 增加 `NA_TOOL_ADMIN`。
- `decide()`：
  - `none`：拒绝。
  - `readonly`：`Gateway` 可读，`GatewayAdmin` 拒绝。
  - `ask` / `auto`：`GatewayAdmin` 永远 `ASK`，不受 auto 影响；`Gateway` 直接执行。
- `tools_specs()` 不内嵌这两个工具的描述；网关像补 `Skill` 一样补 schema。
- 内核 hello 增加能力声明（例如 `caps: ["gateway_admin"]`）。旧内核没有这个能力，网关就不提供工具，避免调用后被判“未知工具”。协议版本是否升到 v6 在实现时定。
- 工具调用预检跳过这两个工具（它们没有工作目录）；`exec_tool` 像 `Skill` 一样直接交给网关，不只在本机执行。
- 内核仍是权限判断点：`GatewayAdmin` 的审批请求由内核发，审批结果再驱动执行。

### 3.3 网关改动

- `startModel` 不再因为“没有工作目录”就把工具清空：管理员会话即使没有工作目录，也提供 `Gateway` / `GatewayAdmin`（以及可选的 `Skill` 知识）；非管理员仍然没有工具。
- 没有工作目录时不要简单把模式设为 `none`，否则内核会拒绝所有工具；管理员用会话模式（至少 `ask`），`GatewayAdmin` 由新的 ADMIN 类型强制审批。文件/命令工具因为没有 schema，模型不会看到；即使硬调，预检也会因为没有工作目录失败。
- `execTool` 在查找工作目录之前处理 `Gateway` / `GatewayAdmin`；动作在 goroutine 里执行，不能拿着 App 锁等 SSH 连接。
- 动作层直接复用 `host.Manager`、`AddWorkspace`、`setSessionWorkspaces` 等现有校验，不复制一套 SSH/SFTP 逻辑。
- 连接等待：轮询 `host.Manager` 状态，直到 online / error / 超时；返回 `state`、`detail`、耗时和下一步建议。
- 新挂载的工作目录按现有规则**下一条消息生效**；工具结果要明确说明，并可以在返回前用 `ListDir` 验证路径存在。
- 所有动作写审计日志：`from`、`sid`、动作、目标、结果；不记录密码/密钥。

### 3.4 `ssh_setup` 的完整流程

1. 模型请求 `GatewayAdmin {action:"ssh_setup", name:"debian-server", host:"192.168.1.20", user:"debian", path:"/home/debian/app"}`。
2. 内核判定 ADMIN → 发审批请求。
3. 网页审批卡片显示计划：`添加 SSH 服务器 debian-server（debian@192.168.1.20:22），并挂载 /home/debian/app 到当前会话`；卡片有一个可选密码框（留空则用网关的密钥 / ssh-agent / ~/.ssh/config）。
4. 管理员批准；密码只进入网关的临时内存，随后写入 `host.Manager` 的 0600 存储。
5. 网关依次：校验名字和主机 → 保存连接 → 连接 → 等待 online（包括执行器就绪）→ 添加工作目录 → 挂到当前会话。
6. 返回结果：
   - 成功：连接状态、执行器/Landlock 信息、工作目录已挂载（下一条消息生效）。
   - 认证失败：说明是密码/密钥问题，连接已保存；让用户在网页设备页补密码或换密钥，不要重复重试。
   - 路径不存在：连接成功但工作目录失败；给出服务器上的错误，保留连接供用户修正路径。
   - 超时：返回 connecting/reconnecting 状态和当前错误，让用户稍后用 `Gateway ssh_list` 查看。

### 3.5 密码怎么进入网关（不经过模型）

- `GatewayAdmin` 的审批信息增加 `secret` 标记和 `secret_label`（例如“SSH 密码（可选）”）。
- 只有 `ssh_setup` / `ssh_connect` 的审批卡片显示密码框；批准时 `POST /api/approvals/{n}` 带上密码。密码只存在 App 的临时 map（按工具调用 ID），工具执行时取出并写入 Manager，然后立即清除。
- 网页顶部的审批 toast 遇到需要密码的操作时只提供“去对话”入口，不能直接点批准，避免绕过密码框。
- 工具参数里禁止出现 `password`；网关渲染审批内容时只显示白名单字段，避免模型幻觉出的密码进入界面或日志。
- Telegram / 飞书只能批准不能填密码：需要密码的操作在这些渠道提示“请到网页批准并填写密码”；如果留空且密钥可用，仍可正常连接。
- 超时、拒绝、这一轮结束时都清理临时密码。

## 4. 安全边界

- 不把配置目录、数据目录、`~/.ssh` 加进工作目录；沙箱规则不动。
- 不提供通用 shell/通用配置写入；动作白名单在网关代码里，模型无法扩展。
- 只有 `isAdmin(from)` 的会话能看到/执行管理工具；内核和网关双重检查。
- `GatewayAdmin` 每次都审批；审批卡片写清目标，host 可能是内网地址，管理员自己确认。
- 工具结果、事件、日志都做脱敏：不返回密码、API key、私钥内容、`connections.json` 全文。
- 旧内核没有能力声明时自动不提供工具；升级内核后自动出现。
- 这是一个“自举”能力：agent 能改网关的连接配置，但不能改自己的沙箱和权限实现。

## 5. 提示词和技能

- 没有工作目录时的系统说明改成：“没有文件/命令工具；如果你是管理员，可以用 Gateway 查看状态，用 GatewayAdmin 管理 SSH 连接和工作目录。不要尝试读取网关配置/数据目录，也不要用 Shell 跑 ssh。”
- `skills/device-connections/SKILL.md` 从“只提供知识、没有管理权限”改成：先读状态，确认真实意图后用 `GatewayAdmin`，需要密码时让用户在网页审批卡片填写；不要在聊天里索要密码。
- 排查仍先看 `Gateway ssh_list` / `status` 的真实状态，不要反复重试同一个失败动作。

## 6. 分阶段

1. **P1（本次建议先做）**：`Gateway` + `GatewayAdmin` 的 SSH/工作目录动作；`ssh_setup` 一把完成；审批卡片里的可选密码框和临时密码 map；密钥/ssh-agent 认证；审计、旧内核降级。密码是 Debian 这类服务器最常见的认证方式，放到 P1 才能真正消除手动步骤。
2. **P2**：更多白名单配置动作（模型/接口、渠道 token 等），每个动作单独定义 secret 字段和校验，不做通用 `config_set`。
3. **P3**：把“管理动作”扩展到更多对象（例如人格、渠道），再评估是否需要更细的权限分级。

## 7. 测试

- Go 单测：非管理员看不到/不能调用；ADMIN 在 auto 下仍然审批；只读查询脱敏；密码不出现在结果/日志；连接等待的 online/error/timeout；工作目录添加失败时连接保留。
- e2e（假 ssh）：`!call GatewayAdmin ssh_setup ...` → 审批 → 连接 → 挂载 → 下一条消息在服务器上执行工具；密码走审批时不出现在输出。
- 浏览器：审批卡片显示计划；密码框不显示明文；批准/拒绝/超时；`Gateway` 查询不弹审批。
- 回归：没有工作目录的普通会话仍然不能读写文件；没有能力声明的旧内核不出现管理工具。

## 8. 需要确认的选择

1. 密码框是否就放在 P1（建议是，否则用密码的服务器仍要手动去设备页）？
2. 第一版是否就只做 SSH + 工作目录，还是同时把模型/渠道配置也纳入白名单？
3. 管理动作是否每次都要审批（建议是），还是同一会话内可以短期信任？
