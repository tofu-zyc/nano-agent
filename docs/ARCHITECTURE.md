# 架构

nano-agent 由 C 内核、C 执行器和 Go 网关组成。网关保存会话与配置、连接模型和聊天平台；内核负责调用循环与执行调度。后台 Job 的进程、日志和恢复记录归执行节点管理。

本文描述当前实现。目标设计、核心概念和评审标准见 [设计原则与目标架构](DESIGN.md)。

~~~text
聊天平台 / 网页 / 命令行
           │
       Go 网关 ────────── SSH 服务器上的 nano-exec（按需启动）
           │              网关本机的执行器
      NDJSON 连接
      ┌────┴────┐
   C 内核 A   C 内核 B
   执行器      执行器
   Job/观察器  Job/观察器
~~~

## 模块职责

| 模块 | 负责 |
| --- | --- |
| kernel/src/core.c | 纯逻辑状态机：run、轮次、工具批次、审批、步数和时间预算 |
| kernel/src/main.c | 节点事件循环、执行准入、租约、代执行、协议路由和取消 |
| kernel/src/job_manager.c、job_io.c | 后台任务生命周期、持久状态和有界日志 |
| kernel/src/nexec_*.c、sandbox*.c、runner*.c | 文件与 Shell 工具、平台隔离和子进程管理 |
| kernel/src/watch.c、pipe*.c | 文件观察后端与有界事件队列；MCP 进程的 stdio 透传 |
| gateway/internal/app | 组装根：消息入口、run 调度、模型请求、工具路由、插话、分支、Hook、群聊、内部群和自主活动的协调 |
| gateway/internal/channel、web | 平台适配（命令行、网页、Telegram、飞书、QQ、微信）、网页接口和展示 |
| gateway/internal/kernel、proto、host | 内核连接与配对、协议类型、本机与 SSH 执行器 |
| gateway/internal/dispatch | 跨 run、跨执行入口的机器租约与资源仲裁 |
| gateway/internal/session | 历史、压缩视图、不可变分支快照、存储迁移与恢复 |
| gateway/internal/llm、models、media | 模型请求；模型目录与密钥；图片 |
| gateway/internal/agents、persona、skills | agent 与机器人配置、人格、技能 |
| gateway/internal/autonomy、experience、knowledge | 心智、日程与活动账本；实际经历索引；长期知识 |
| gateway/internal/schedule、rooms | 定时任务；网关内部群 |
| gateway/internal/eventlog、hooks | 可靠运行事件、规则和投递 |
| gateway/internal/events | 网页展示用的事件总线，允许丢失 |
| gateway/internal/mcp | MCP 配置、连接与工具目录 |
| gateway/internal/config、secret、redact | 配置合并、凭据加密、脱敏 |
| gateway/internal/httpx、search、release、paths、control、version | 代理与 HTTP、网络搜索、发布包、安装路径、本机控制 socket、版本号 |

模型循环始终由 C 内核控制。新增能力复用现有进程，不增加常驻 Python、Node 或 JVM 服务；桌面 Driver 等外部 MCP 服务按需接入。

## 机器、工作目录和路由

机器分三类：网关本机、连接网关的内核节点、只经 SSH 接入的服务器。工作目录是“机器 + 路径”，会话可挂载多个工作目录；没有挂载时管理员在 HOME 下按权限工作。

每轮选择一个协调内核。目标目录在协调节点上时本地执行；在其他机器上时通过网关路由到本机执行器、SSH 执行器或目标内核的代执行入口。约束由网关按真实会话生成，目标节点还会核验自己的要求。换协调内核不迁移已运行的进程；后台 Job 仍归原节点。

技能保存在网关，附带文件按需送到目标机器。MCP 由网关管理，可经节点 pipe 连接目标机器上的进程；桌面操作沿用这条链路，见 [桌面接入](COMPUTER-USE.md)。

## 会话、模型与界面

- 会话 ID 的前缀决定路由归属，其他 agent 的会话使用 `@<agent>/` 前缀。普通会话可以换执行 agent；内部会话（life、整理记忆、定时任务、内部群成员）和分支的执行身份固定。平台窗口绑定与会话数据分开保存。
- 内部群的记录是会话 `room:<id>`，每个成员 agent 在群里另有自己的会话 `@<agent>/room:<id>`。
- 接口和模型只存于 `data/models.json`；没有模型时保持空状态。旧配置只迁移一次，删除后不重新生成。
- 历史只追加，压缩保存摘要及有效窗口；系统提示和工具前缀保持稳定，时间、环境、记忆与交接说明通过消息追加。
- 图片按内容去重，历史保存引用；仅向支持图片的模型发送图像。
- 拓扑图是配置入口，节点查看与修改放在同一面板。聊天、任务、事件规则和分支视图共用后台状态。
- Gateway 工具查询管理状态，GatewayAdmin 修改配置并走审批。实现复用网页背后的管理接口，返回值不包含密钥。

## 权限与隔离

管理员、访客、工具范围、审批模式和执行隔离分别核验。full 模式由网关自动批准原本需要的审批，不改变隔离策略。auto/on/off 隔离的继承与节点要求见 [运行时](RUNTIME.md#执行隔离)。

网关保存模型、SSH 和渠道凭据，敏感值加密，查询只暴露是否已配置；工具交给模型的结果按脱敏设置处理。SSH 凭据不放进命令参数。工作目录与网关运行目录的防护、文件路径检查、提权审批继续独立生效。

## 持久化边界

网关保存配置、会话、分支、心智、经历和可靠事件；节点保存执行中的 Job 及其日志。网页事件流只用于展示，可靠 Hook 使用独立日志。重启恢复先核对身份和提交记录，结果不明的外部动作不自动重放。

发往外部聊天平台的消息经可靠出站（`data/runtime/outbox.jsonl`）：先落盘再发送，重启后继续，同一窗口按顺序，至少一次；命令行、网页、Job 卡片、审批卡片、Hook 通知和流式回复仍走内存队列，不在持久化范围内。各部件的健康状态由 `internal/health` 汇总。细节见 [运行时](RUNTIME.md#可靠出站与健康状态)。

- [调用循环、分支与自主活动](RUNTIME.md)
- [后台任务](JOBS.md) · [Hook](HOOKS.md) · [可靠事件日志](RUNTIME-events.md)
- [协议](protocol.md) · [存储与迁移](STORAGE.md)
- [开发说明](HANDOFF.md) · [发布流程](RELEASE.md)
