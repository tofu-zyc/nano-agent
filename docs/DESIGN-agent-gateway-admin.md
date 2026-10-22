# 让 AI 管理网关（Gateway / GatewayAdmin）

日期：2026-10-22。状态：阶段 4 实现稿。方向来自用户的四条原则和 `PLAN-next.md`。

## 要解决的问题

用户要的是：在任意设备的聊天软件里下指令，AI 把活干完——包括配置模型、接 SSH、加工作目录，不用守着网关和网页。

现在的缺口不是读权限（管理员已经能读整个文件系统），而是**写运行时状态**：

- 直接改 `gateway.toml` / `data/*.json` 不会让运行中的网关重新加载，还会绕过校验；
- 让 AI 去网页点也不符合“在聊天里把活干完”。

所以给管理员会话两个工具。工具由网关提供、网关执行；内核只负责把它们当成“需要审批的管理动作”。

## 原则

1. 安全靠人保证，不靠工具强加黑名单。这里不加新的路径/网络限制。
2. 只给管理员会话。非管理员看不到、也调不了。
3. 每个修改都要审批（`GatewayAdmin` 在任何模式都审批，auto 也一样）；查询 `Gateway` 不审批。
4. 密钥可以在聊天里给 AI（用户的选择），但审批卡片上打星，工具结果和审计日志不回显；落盘走阶段 2 的加密。
5. 直接调用网页接口背后的同一套函数，修改实时生效，不重启网关。
6. 监听地址、网页密码、渠道 token、内核口令这类要重启才生效的配置**不开放**给 AI。

## 两个工具

### `Gateway`（只读，不审批）

一个 `what` 参数，返回一段 JSON 文本：

| what | 返回 |
|---|---|
| `status` | 网关运行状态、内核、机器、默认模型、渠道 |
| `models` | 接口和模型列表（`has_key` 表示有没有 key，**不返回 key**） |
| `ssh` | SSH 连接列表和状态（`has_password`，**不返回密码**） |
| `workspaces` | 工作目录列表，以及本会话挂了哪些 |
| `pairing` | 等待确认的电脑配对、已配对设备 |
| `ssh_config` | `~/.ssh/config` 里的 Host 别名 |

默认 `status`。查询结果也走阶段 2 的脱敏（如果配置要求）；密钥本来就不返回。

### `GatewayAdmin`（修改，每次审批）

参数是一个对象，`action` 决定动作，其余字段按动作取用：

```json
{
  "action": "ssh_setup",
  "host": "192.168.1.10",
  "name": "gpu",
  "user": "root",
  "password": "…",
  "path": "~/proj",
  "mount": true
}
```

动作分四组：

**模型**
- `model_endpoint_save`：`id?`、`name`、`base_url`、`api_key?`、`proxy?`、`kind?`
- `model_endpoint_delete`：`id`
- `model_fetch`：`id`（从接口拉模型列表）
- `model_save`：`endpoint`、`model`、`label?`、`context_window`、`max_tokens?`、`max_history_tokens?`、`thinking?`、`thinking_levels?`、`modalities?`
- `model_delete`：`id`（`endpoint/model`）
- `model_default`：`id`
- `model_test`：`id`
- `model_use`：`id`（切换本会话的模型）

**SSH**
- `ssh_save`：`id?`、`name`、`host`、`port?`、`user?`、`password?`、`identity_file?`、`auto_connect?`
- `ssh_delete`：`id`
- `ssh_connect` / `ssh_disconnect`：`id`
- `ssh_setup`：保存 → 连接 → 等到在线 → 可选 `path` 添加并挂载工作目录；返回真实状态（在线 / 认证失败 / 超时 + 原因）

**工作目录**
- `workspace_add`：`host`、`path`、`default?`
- `workspace_delete`：`id`
- `workspace_mount`：`id`、`mount`（true 挂上 / false 取下）
- `workspace_default`：`id`、`default`

**电脑配对**
- `pair_allow` / `pair_deny`：`id`
- `pair_remove`：`node`

所有 `id` 都支持列表里的 ID；`host` 是本机、SSH 名字或内核名字，和网页上是同一套。

## 执行流程

1. 模型调用 `GatewayAdmin`，参数里可以带明文密钥。
2. 内核 `decide()`：`NA_TOOL_ADMIN` 在任何模式都返回 ASK；`readonly` / `none` 直接拒绝；访客（`elevate=false`）拒绝。
3. 网关发审批卡片：标题带 **“提权 · ”**，正文是动作和脱敏后的目标（例如“配置 SSH gpu（密码已隐藏）”），不带密钥。
4. 管理员批准后，内核把调用 `exec` 给网关（没有 workspace）；网关再查一次 `isAdmin`，然后在一个 goroutine 里执行，不拿着 App 锁等 SSH。
5. 执行结果回给内核、显示在聊天里；结果里不回显密钥。审计日志记：谁、哪个会话、动作、目标、结果，不记密钥。
6. 修改通过 `models.Store`、`host.Manager`、`AddWorkspace` / `setSessionWorkspaces` / `SetDefaultWorkspace`、配对 API 立即生效。新加的 SSH/模型下一条消息就能用；新挂的工作目录能通知内核就在这一轮用，不能就明确说“下一条消息生效”。

## 内核改动

- 新工具类型 `NA_TOOL_ADMIN`，`decide()` 里任何模式都 ASK，readonly/none 和访客拒绝。
- `BUILTINS` 里加 `Gateway`（READ，描述为空）和 `GatewayAdmin`（ADMIN，描述为空）；不把它们的描述报给模型，描述和 schema 由网关加。
- `exec_tool` 遇到这两个名字直接 `send_exec` 交给网关（和 `Skill` 一样，不需要工作目录）。
- 协议升到 v7；旧内核连上来会被拒绝，提示重新运行安装脚本。这样旧内核不会因为不认识管理工具而出错。

## 审批卡片

- 引擎：`GatewayAdmin` 的标题固定加“提权 · ”，并写明动作；参数里的 `api_key` / `password` 用 `***`，`base_url`、`host`、`name`、`path` 等正常显示。
- 网页、Telegram、飞书用同一份 `ApprovalCard`；按钮流程不变。
- 聊天里 `/approve <编号>` 仍然可用。

## 错误处理和审计

- 参数缺字段、找不到 ID、SSH 连不上、模型接口报错：原样返回清楚的错误，不重试同一个动作。
- `ssh_setup` 超时/认证失败返回具体原因，并保留已经保存的连接（用户可以再改）。
- 不在结果里回显密钥；`Gateway` 查询只返回 `has_key` / `has_password`。
- 每次 `GatewayAdmin` 写一条 `audit` 日志（slog）：`who`、`sid`、`action`、`target`、`ok`、`error`，不含密钥。
- 网页接口和工具共用底层函数，网页看到的状态和聊天里一致。

## 非目标

- 不开放 `listen`、`web.password`、`telegram.token`、`feishu.app_secret`、`kernel.token` 等重启项。
- 不做审批卡片里的密码输入框：密钥直接在聊天里给（用户确认过）。
- 不新增权限面板、路径白名单或网络开关。
- 不实现群聊、移动端或 Windows 原生内核。

## 测试

- 非管理员看不到、也调不了 `Gateway` / `GatewayAdmin`。
- auto 模式下 `GatewayAdmin` 也要审批；readonly / none / 访客拒绝。
- 结果和日志里没有密钥；审批卡片里密钥打星。
- 假 ssh 覆盖 `ssh_setup` 的在线、认证失败、超时三种情况。
- `Gateway` 查询返回模型/SSH/工作目录/配对状态，且不含密钥。
- e2e：`!call GatewayAdmin …` → 审批 → 生效。