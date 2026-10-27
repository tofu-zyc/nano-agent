# v0.9.0 二轮验收与下一步执行清单

日期：2026-10-27。首次审查基线：`05e995e`，此前修复提交：`32c97fc`；执行链复验：`8093f20`；持久收件箱首次验收：`33673fd`（含 `7f48078`）；前次复验：`5d8bc9a`、`cbdadac`；最新复验：`2bb1953`。

**最新结论：`2bb1953` 已通过第 9 节四个反例的独立复验，但停止失败重试、重置清理失败仍有问题，新增历史回查也有整份加载导致的内存增长，B 暂不通过。** 当前结果与修复要求见第 10 节。第 2 节的 R1–R4 已在 `8093f20` 复验通过，保留原始问题用于追踪。第 3 节其他衔接项继续按实际范围跟踪；B 的原始恢复约束见第 6 节。本文记录验收和指导，没有修改产品源码。

## 1. 已验证通过的部分

| 项目 | 本次验证 |
| --- | --- |
| 跨轮次迟到结果 | 两轮复用相同 call_id；旧 turn 的结果被忽略，新 turn 的真实结果可以正常闭合 |
| 网关收到旧 run 的 done | 当前 Go 回归测试通过，旧 run_id 不再结束当前 run |
| 普通同目录 Shell 与 Write | 同批次 Shell 先完成、Write 后完成，最终文件保留 Write 的内容 |
| 节点 auto、当前请求 off | 经真实内核执行此前被 seccomp 拒绝的 Unix socket 操作，off 下成功 |
| 代执行进程数量 | max_execs=1、max_parallel_shells=1 时，同时提交两个代执行 Shell，观察到的执行器子进程数量为 1 |
| 插话后的模型预算 | 真实 Go Gateway + 真实 C kernel + 受控模型替身：max_steps=1，生成时收到插话，最终只有 1 次模型请求，内核返回 budget |

基础检查：

- `go vet ./...`、`go test ./...` 通过。
- C core：205 checks，0 failures。
- ASan 执行器测试：0 failures；ASan 宿主测试：0 failures。
- 两套 Python 测试都使用全局 `pgrep -f 'sleep 4242'` 检查残留，初次并行运行相互干扰；分开执行后通过。这属于测试隔离问题，不能当成产品取消失败，也不应继续并行运行这两套测试。
- 本轮未进行真实飞书 / Telegram、真实 SSH 或 Windows 实机验收。

## 2. 必须先修的四个阻塞问题

### R1：C 发出的 exec 消息与 Go 协议字段类型冲突

位置：`kernel/src/main.c:send_exec`；`gateway/internal/proto/proto.go:Msg`；`gateway/internal/kernel/hub.go:readMsg`。

C 现在在 exec 消息顶层写入：

```json
{"t":"exec","sid":"s","name":"Skill","sandbox":"auto"}
```

但 Go 的 `Msg.Sandbox` 对应 `sandbox` 对象，当前轮请求使用 `SandboxMode`，JSON 名为 `sandbox_mode`。实际捕获 C 内核输出的 exec 帧，再交给真实 Go 解码函数，得到：

```text
json: cannot unmarshal string into Go struct field Msg.sandbox of type proto.Sandbox
```

这会使网关读消息失败、断开内核连接，影响 Skill、搜索、MCP 和跨机器执行等所有走 exec 的路径。Python 假网关使用普通字典，无法发现这个类型冲突；Go 测试通过结构体构造消息，也没有经过 C 的序列化代码。

修复要求：

- hello 顶层 `sandbox` 保持能力对象。
- input / exec 顶层请求模式统一用 `sandbox_mode`，与 `proto.Msg.SandboxMode` 一致。
- run 的 req 和 nano-exec 请求里的 `sandbox` 可以继续使用字符串，但要与外层协议明确区分。
- 加入真实 C 输出 → 真实 Go 解码 / 路由的测试，不能只测试两边各自构造的结构体。

建议回归名称：`TestCExecWireCompatibleWithProtoMsg`。验收至少包含 Skill 和一个远程文件工具，要求连接不重置、结果正常返回。

### R2：宿主已排队的工具调用会被 core 重复派发

位置：`kernel/src/core.c:on_call_queued / dispatch / call_holds_slot`；`kernel/src/main.c:exec_admit`。

宿主收到调用后暂时没有槽位，会将 core 中该调用改为 `CALL_QUEUED`，并保持 `sent=true`。但 dispatch 挑选 `CALL_QUEUED` 时没有跳过已经发给宿主的调用；后续 tick 或其他工具完成时，同一个调用再次生成 EXEC，并被宿主加入队列。

实测设置：

```text
max_execs = 1
max_parallel_shells = 1
同一模型批次：
  A：目录 one，sleep 0.2
  B：目录 two，printf X >> execution-count; sleep 0.2
```

B 只请求了一次，最终文件内容为 `XXXX`。工具结果仍可能只呈现一次，实际副作用已经重复发生，不能靠忽略重复结果解决。

修复要求：

- 将“尚未派发、在 core 等待”与“已由宿主接管、在宿主等待”明确区分，可拆状态，或在 dispatch 中严格排除 sent=true 的队列项。
- 宿主对完整调用身份增加防重复准入，作为第二道检查；重复请求不能再启动进程。
- 一个调用从派发到终态只能有一个执行所有者，取消、超时、结果和释放资源统一沿此记录处理。
- 检查排队超时分支：若调用已交给宿主，返回超时结果之前也必须移除宿主队列，不能随后又启动。

建议回归名称：`TestHostQueuedCallExecutesExactlyOnce`。检查真实追加次数为 1，同时检查结果数、后续派发和 run 结束后没有残余执行。还应覆盖队列满、tick、一个并行兄弟调用完成等触发条件。

### R3：取消排队中的代执行请求后，它仍会执行

位置：`kernel/src/main.c:handle_msg` 的 run_cancel 分支、`execq_drop`。

run 现已进入 exec_admit 排队，但 run_cancel 仍只调用 runner_kill，没有从 execq 删除尚未启动的代执行请求。

实测：max_execs=1；先启动一个短 Shell 占槽，再提交 Write；确认文件尚未生成时发送该 Write 的 run_cancel。占槽 Shell 结束后，已经取消的 Write 仍创建文件，并返回 ok=true。

修复要求：

- run_cancel 先查找并删除尚未启动的 JOB_HOST 请求，否则终止已经运行的执行器。
- 对取消与恰好启动的竞争采用单一状态转移；取消后不能再产生新的文件写入等副作用。
- 明确给等待方返回一次取消终态；重复取消幂等，不影响别的请求。
- 验证网关超时 / 用户停止 / 连接中止同样能清理对应排队项。

建议回归名称：`TestCancelHostQueuedWriteNeverStarts`。必须在释放占用槽后继续观察，检查目标文件始终不存在，不能只在发送取消后立刻断言一次。

### R4：目标节点的沙箱部署要求没有用于代执行

位置：`kernel/src/main.c:handle_run / effective_sandbox / spawn_exec`。

普通 input 使用 effective_sandbox 合成请求，但 handle_run 直接将 req.sandbox 交给 exec_admit。实测目标节点配置 `sandbox=on`，代执行请求写 `sandbox=off`，此前应被沙箱阻止的 Unix socket 操作仍然成功。

这与本次明确的规则“目标节点 on/off 优先，否则听本轮请求”不一致。本机 input 的 off 用例通过，不能代表跨节点传递已经正确。

修复要求：

- 目标节点接收 run 时自己计算 effective_sandbox；不能把协调节点传来的值视为已经满足本机部署要求。
- 将真正生效的模式写入执行记录和响应，审批判断、界面显示与实际执行保持一致。
- 加入节点配置 auto / on / off × 请求 auto / on / off 的矩阵测试；用实际受限操作确认行为，不只检查字段。

建议回归名称：`TestHostRequestRespectsTargetSandboxPolicy`。缺少 Landlock 时按 on 的既有失败语义验证或明确跳过相应平台用例。

## 3. 同一修复阶段还需检查的执行链

下面是代码检查发现的衔接缺口，应与上述四项一起覆盖；其中远程排队计时尚未做长时间场景实测，不与前四项的运行复现混为一谈。

- `app.execTool` 在入口就发送 exec_started；目标节点对 JOB_HOST 排队 / 启动没有向 kernelHost 提供可转发的对应状态。真实远程请求排队时，协调 core 仍可能已经开始计算执行期限。应将 accepted、queued、started、terminal 贯通到实际目标节点的确认，不在本地刚准备转发时宣告开始。
- 验证提权 Shell 的 `machine:files` 独占与普通目录 / 文件资源键能冲突；不能只新增一个没有其他调用共享的名字就认为机器范围已经覆盖。节点未挂工作目录的 HOME 模式也需要真实资源域。
- 旧节点缺 user_interjection 时，当前仍直接在 Gateway 重开模型请求；不能将旧路径也声称为受内核预算保护。应回退为原有消息排队，或实现有等价上限的明确兼容路径。
- 当前将插话写入历史后立即清空 inbox，但随后的 model_yield 可能只得到 DONE(budget)。持久化前先定义“已受理、已注入、已被模型请求读取”三个不同状态，结束时明确哪些补充仍未处理。

## 4. 下一步按小阶段交付

### A. 先稳定当前执行底座

先只修 R1–R4 和远程排队 / started 的传递，不同时引入后台 Job、Fork 或新心智维度。每项有独立回归和可检查提交，更新 IMPLEMENTATION 的实际范围。

必须新增一套真实 Go Gateway + 真实 C kernel / nano-exec + 确定性模型替身的集成验证。至少覆盖网关工具、本机文件、目标内核代执行和插话；跨节点路径用两个真实 C 节点。渠道可以是假渠道，不需要真实平台凭据。

现有 Python 假网关和 Go 假内核测试继续保留，但它们不能替代这个集成层。测试侧的读消息、配对和状态校验要使用产品协议；不要通过测试替身缺省接受所有字段来绕过契约问题。

退出条件：四项运行复现全部转为通过，取消 / 超时后无迟到副作用，每个实际请求最多启动一次，所有排队时长与执行时长有清楚归属，ASan 与 Go 检查通过。

### B. 补齐插话的持久接收与恢复

这一步建立可供 Fork / Hook 后续复用的小型持久操作记录，不提前实现完整 Hook 系统。

需要保存稳定消息 ID、源 endpoint / 身份、目标 sid / run_id、接收顺序和处理状态。持久受理后才回执“已收到”；历史写入与消费游标按 ID 对账；模型请求快照记录已读取的 revision。写入失败不得先清 inbox 或继续声称已处理。

恢复时区分：尚未注入、已经写历史但模型未读、已经提交模型请求。保证不丢消息、不重复加入历史，不自动重放旧工具。处理已注入但被预算终止的消息时，展示明确去向；不能通过暗中重置原 run 预算来续跑。

顺带统一会话消息 TS 的秒单位：当前 interject.go 使用 UnixMilli，而 Store 默认和多数消费位置按 Unix 秒处理。保留毫秒接收时间应使用明确的新字段，不能混在同一个 TS 中。检查现有 ParentRun 是否真正接入压缩、撤回和历史分组，不能只加字段与注释。

退出条件：在受理回执前后、历史写入前后、游标提交前后分别中断重启，都能按 ID 正确恢复；磁盘失败、容量上限、重复渠道消息和预算结束有可重复验证。

### C. 优先交付安静的持久 Fork（设计文档第 8 节）

这是用户明确强调的关键能力，可先于 P2 / P5 完成，不要继续把它压在所有后续功能之后。

第一小步只做存储与显式入口：

- Session.fork 与用户 Fork 入口；稳定分叉边界、幂等 operation_id；父子关系可查看，原窗口不改接。
- 分叉点的 Summary / Base / Omitted、有效消息、来源、附件和适用设置保存在不可变快照；不以新生成的摘要替换原上下文。
- 独立追加日志、快照引用与缓存回收；源会话重置或删除不破坏分支。
- 当前批次内调用 Fork 时先返回 pending_boundary，批次闭合后再发布 ready，避免等自身返回的死锁。

第二小步接拟人自主执行：

- agent 能自己创建、继续同一个话题分支；当前 life 轮结束后使用新 sid / run_id 走原 C 循环，不在 Go 再写一套循环。
- 统一 silent 投递策略：普通回复、过程、长工具、审批、失败、恢复说明、Message / SendFile 均不回落到主聊天；用户主动打开分支才看到结果。
- 明确约定的投递授权单独记录；默认失败也不主动升级通知，不为保持安静自动批准。
- 同一个 agent 的心智和预算共享；分支不能复制权限或剩余次数；前台聊天优先且继续可用。

退出条件：用记录所有发送行为的假飞书 / Telegram 验证自主活动对主聊天零推送；用户主会话可继续对话；分支可暂停、重启后继续；100 个闲置分支不会常驻 100 份历史。

首版不做自动历史合并；用户主动引入分支报告时只在父会话下一安全边界加入带来源的结果摘要。

### D. 再推进后台 Shell 与 Hook

Fork 稳定后推进 P2 → P3：C JobManager、流式输出、原地后台化、任务工具与平台按钮；复用已经修稳的唯一执行所有权、排队、取消和身份链。后台 Job 必须继承分支 silent 策略。

随后推进 P5：可靠事件、通知 / 短脚本 / 模型处理器，以及 temporary / branch 的执行选择；复用持久操作与上下文快照，避免再建一套相互独立的历史存储和恢复机制。

心智数值配置可作为独立小变更穿插，但在 §7.2 / §7.3 完成前，继续将 P6 标为部分完成。

## 5. 本轮边界

产品源码未改动；运行了现有测试，并通过临时测试代码 / Go overlay 做真实协议、文件副作用和跨组件验证。新增的复现场景尚未成为仓库内永久回归测试，接手实现者应按上述清单补入。测试通过只代表对应场景通过，不代表未实现的 Fork、后台任务、Hook 或心智配置已经验收。

## 6. `8093f20` 复验与 B 的实施约束

### 6.1 四项修复通过

除阅读修改和永久回归外，本次重新运行了上一轮的独立场景：

| 项目 | 复验结果 |
| --- | --- |
| R1 | 真实 C exec 帧顶层只有 sandbox_mode；真实 Go 网关 + C 内核的 Skill 往返测试通过 |
| R2 | max_execs=1、两个目录的两个 Shell；只提交一次的追加命令最终得到一个 `X`，无多次追加 |
| R3 | Write 排队时取消；释放占槽进程后再观察 1.5 秒，目标文件始终不存在 |
| R4 | 节点配置 on，代执行请求 off；实际受限操作仍然失败 |

本次重新编译内核，C core 205 checks / 0 failures；`go vet ./...`、`go test ./... -count=1` 全部通过；执行器和宿主 ASan 测试串行运行均为 0 failures。验证环境仍为 Linux / WSL、Landlock ABI 7，未扩大为真实聊天平台、真实 SSH 或 Windows 实机验收。

### 6.2 每会话追加日志方案可以采用

`data/interjections/<编码后的 sid>.jsonl` 可作为 B 的实现方向。文件名沿用现有会话 ID 编码，不能直接把包含路径字符的 sid 拼进文件系统。记录 schema_version、单调递增的接收序号和稳定 interjection_id；原平台消息去重键至少包含 bot / channel / chat / message_id，不能假定原始消息 ID 在不同窗口之间全局唯一。

仅记录 received → injected → read 仍然不够，必须区分以下含义：

| 状态 | 能证明什么 | 重启后的处理 |
| --- | --- | --- |
| received | 已可靠受理，但尚未提交到正式历史 | 恢复有界待处理项；先按稳定 ID 检查历史，避免重复注入 |
| injected | 同一条插话已进入正式历史 | 不重复写历史；仍需判断是否曾发起并完成后续模型处理 |
| included_in_request（原 read） | 已被纳入某个模型请求快照 | 关联 request_id 与 input_revision；它不证明请求完成，更不证明工具执行成功 |
| settled / request_outcome | 已有持久的有效请求结局或明确移交状态 | 按实际结局收尾；失败 / 中断 / 未知状态要保留可见的继续处理入口 |
| withdrawn / discarded | 用户撤回、明确停止或重置导致该条补充失效 | 不重新进入队列，不能重启后复活 |

可以保留原有三态命名，但请求结果与撤回终态必须有独立记录。**“不重复写进历史”与“不再处理”是两件事。**

具体反例：先写 read，随后尚未发出请求便崩溃；或请求已经发出但响应尚未提交便崩溃。恢复时若一律跳过 read，这条已经回执的补充就可能永远没有处理。正确行为是保留既有历史，按 request_id / run_outcome 标记中断或待继续，不能简单删除待处理责任，也不能自动重放旧工具。

### 6.3 跨文件提交与身份

- 回执“已收到”前，需要确认受理记录的持久提交；不要将进入内存队列或仅调用 Write 当成已完成持久受理。明确 Sync 与提交确认的边界，可合并刷盘，但不能在确认前回执。
- 历史消息必须有稳定 history_message_id，批量注入保留 source_ids 数组和 journal 序号。物理位置 / Base / parent_run_id 只能辅助定位：压缩、撤回、重置之后位置可能变化，不能作为唯一对账依据。
- 历史写入成功、injected 记录尚未提交就崩溃时，恢复应通过消息 ID 补记 injected，不能再次追加正文；反向缺少对应历史时不能仅相信 injected 标记。
- 当前 Store.Append 会先修改内存历史、再执行磁盘写入。B 应修正或提供独立的可靠写入路径：持久提交失败时，不能让内存视图看起来已经成功，再在重试时重复加入。
- 保存 session_generation / epoch，重置、删除、撤回和明确丢弃均有持久终态；恢复前重新校验原身份、agent 和会话范围，不能把旧会话的补充交给后来切换到的新身份。
- 插话何时进入上下文仍遵守原等待语义：工具整批结束、模型完整生成结束；持久化不会引入中途取消或修改在途请求的行为。

### 6.4 故障恢复与低内存边界

单个日志使用单写者或受控串行提交；追加记录有长度上限、序号与版本检查。末尾半条记录可截去未提交尾部，中间损坏应报告并保留现场，不能静默跳过后宣称无消息待处理。不要用默认大小的行扫描缓冲读取可能经过 JSON 转义后超过 64 KiB 的合法消息。

保留现有 16 条 / 64 KiB 的 pending 限额；附件只写持久引用。恢复从小型检查点和日志尾部重建有界待处理状态，不将全部历史日志永久装进内存。日志轮转、检查点替换与去重记录回收必须有提交顺序，不能为了清理日志重新放出已撤回消息。

进程恢复可能知道“模型请求曾开始”，却不知道工具副作用是否已经发生。此时标记 interrupted / unknown，根据已有历史和可观察状态继续，不能重新执行整个旧 turn、旧 Shell 或旧消息发送。

还有一处单位需顺手统一：用户插话消息的 TS 已改为秒，但 `yieldToInterjection` 保存本次 assistant 正文时仍显式使用 UnixMilli。两个历史角色都要与 Store 的秒单位一致，毫秒接收时间用独立字段。

### 6.5 B 的退出条件

先只实施 B，完成后再提交验收；不要同批启动 Fork、完整 Hook 或后台 Job。

至少在以下位置设置可控中断点，用重启后的真实存储与消息请求验证：

1. received 提交前 / 后，平台回执前 / 后；渠道重发时仍只有一条受理记录。
2. 历史消息提交后、injected 提交前；恢复后正文只出现一次。
3. included_in_request 提交后、请求发出前，以及请求发出后、响应提交前；补充不会被误判为已经处理完。
4. 响应 / run 结局已经提交后；不重复处理已经有明确结局的消息，不重复发送旧结果。
5. `/stop`、`/reset`、撤回、会话删除或身份切换后；不恢复已经失效的补充。
6. 写入 / Sync 失败、日志尾部截断、容量已满、批量插话、附件与长文本；回执和实际受理状态一致。

只承诺本地消息与历史按 ID 对账，不将其表述为任意外部副作用“恰好一次”。通过这一步后，沿第 4 节 C 继续交付安静的持久 Fork。


## 7. `33673fd` 持久收件箱验收记录

以下为当时的六项阻塞及原始复现；其七个直接反例已在 `5d8bc9a` 复验通过。当前结论和剩余交叉场景见第 8 节。

本轮重新运行 `go vet ./...`、`go test ./... -count=1`，均通过；C core 205 checks / 0 failures。本次 B 的两个提交没有修改 C 源码，未重复执行 ASan 执行器 / 宿主套件。`Store.Append` 先持久写入、成功后更新内存，以及在写入前加载旧历史的改动方向正确，原有相关回归通过。

此外，通过临时 Go overlay 向真实 app 测试包加入七个补充场景；七个场景均复现下述问题。overlay 只增加测试，未替换产品实现，运行后已清理，不是仓库已有测试失败。单次调用返回成功或第一次恢复出 pending，均不足以证明完整持久生命周期正确。

### B1 [P1]：恢复对象丢失稳定 ID，处理完成后仍会再次恢复

位置：`gateway/internal/app/interject_journal.go:492`（恢复 `interjection` 的初始化）；`gateway/internal/app/interject.go:116`（使用 `it.id` 构造历史 ID 和状态记录）。

恢复构造函数只设置 seq、msg、ep、who、text、at，没有设置 `id: st.ID`；把 `st.ID` 放到 `msg.MsgID` 并不能替代 `it.id`。下一次 drain 得到 `source_ids=[""]`，写入 injected / outcome 的 ID 也是空字符串；fold 会忽略这些状态，原 received 永远未完成。同样条数的不同恢复批次还会算出相同 history_message_id。

独立复现：正常受理一条补充 → 新建 Store / App → 将恢复 pending 按正常 drain 写入历史并记录 ok → 再建 Store / App。实际 pending 仍为 1，完成记录没有关联原 ID。

修复：稳定 interjection_id 必须贯穿恢复、注入、请求和终态；原平台 message_id 与内部 ID 应分开保存。恢复收件箱时也应恢复字节数，继续满足 16 条 / 64 KiB 的准入限制。不能只增加“恢复后条数为 1”的测试。

建议永久回归：`TestRecoveredInterjectionProcessedThenRestartDoesNotReturn`；至少再覆盖不同恢复批次不会互相误去重、恢复后的撤回及容量限制。

### B2 [P1]：尾部残片没有截去，损坏日志仍允许新受理

位置：`gateway/internal/app/interject_journal.go:110`（打开追加文件）、`:116`（忽略读取错误）、`:188`（只忽略半条尾部）。

两个独立复现：

1. 已提交 received 后追加半条 JSON，不带换行；重启可以读出旧 pending。随后 journalAppend 新 received 返回 nil，但实际把新 JSON 接到半条之后；再次读取报第二行损坏，新旧补充无法正常恢复。
2. 已有日志中放入一条带换行的坏 JSON；restore 仅记录错误，openJournal 忽略 readJournal 的错误，仍允许追加新 received 并返回成功。下次恢复仍停在坏行，新增记录不会被恢复。

修复：解析时返回最后有效提交的文件偏移；在追加前安全截去未提交尾部并确认修复持久化。对中间损坏、未知版本、不可读取文件，必须禁止继续按正常日志回执受理，保留现场并向当前发送方说明未受理。写入 / Sync 失败后的文件状态也要纳入同一恢复规则，不能假定报错就代表没有写出任何字节。

建议永久回归：`TestTruncatedJournalThenAppendThenRestart`、`TestCorruptJournalRefusesNewReceipt`。前者必须包含“恢复后又接收新消息，再重启”，不能停在首次 readJournal 成功。

### B3 [P1]：requeued 只转入内存，却已持久标成完成

位置：`gateway/internal/app/interject.go:198`（先写 outcomeRequeued）、`:202`（只构造内存 queued）；`gateway/internal/app/interject_journal.go:475`（恢复时跳过 settled）。

独立复现：正常受理 → requeueInterjections 并放入 s.queue → 在下一 run 写入正式历史之前重建 Store / App。实际 pending 为 0，历史也没有这条补充；已经回执的消息丢失。

修复：将“需要重新排队”保留为可恢复的未完成责任，或者把目标队列可靠提交后，再记录可验证的移交终态。queued 必须保留 interjection_id，后续写正式历史和结果时仍可对账；不能靠内存 s.queue 证明持久移交完成。崩溃后按已有历史判断是否已经消费，避免从丢失变成重复。

建议永久回归：`TestRequeuedInterjectionSurvivesCrashBeforeNextRun`，另测下一 run 已写历史后崩溃不会重复写入；旧节点回退也要走同一可靠路径。

### B4 [P1]：检查点丢失 agent 与去重信息，重启后身份约束失效

位置：`gateway/internal/app/interject_journal.go:195`（折叠状态没有保存 Dedup / Revision）、`:323`（重写 received 没有 Agent / Dedup）。

独立复现：写一条带 Agent=other-agent 和 Dedup 的 received → checkpointJournal → 重建 Store / App。实际 journalByDedup 查不到原消息；原本不该交给当前 agent 的 pending 变成 1 条，因为检查点中的 Agent 已经为空。正常超过 512 条记录就会进入该路径。

修复：为检查点定义完整、可往返的状态结构，保留身份、去重键、稳定 ID、世代、请求与 revision 等恢复依据；不能只复制部分展示字段。去重记录可以有明确的有界保留窗口，但压缩不能无意中清空仍需去重的记录。受理序号及无平台 ID 消息的 ID 分配也应在压缩 / 重启后保持唯一。

检查点提交还需检查每次 append 的错误，不能 `_ = nj.append(...)` 后继续替换原日志；替换失败时应保留可用旧句柄或可恢复状态，成功替换的持久边界需覆盖目录更新。这些是源码检查发现的提交缺口，本轮没有对其逐个注入磁盘故障。

建议永久回归：`TestCheckpointPreservesAgentAndDedupAcrossRestart`，使用真实阈值触发自动检查点；同时测试写检查点 / 替换失败时旧日志仍可继续安全使用。

### B5 [P1]：普通撤回和网页撤回没有持久终态

位置：`gateway/internal/app/undo.go:84`（undoLatest 收件箱分支）。

平台撤回事件已写 withdrawn，但普通“撤回”、`/undo` 和网页 UndoSession 共用的 undoLatest 仍只删除内存 inbox 并发布 withdrawn 事件，没有写日志。

独立复现：正常受理 → undoLatest 返回撤回成功 → 重建 Store / App。实际 pending 为 1，已撤回内容复活。

修复：这些入口统一先可靠提交 withdrawn，再从待处理状态移除并回执；日志失败应保留可重试状态并明确说明失败。还应覆盖恢复 pending、重新排队的插话和已有历史来源，不能只修平台原生撤回事件。

建议永久回归：`TestUndoInterjectionDoesNotReturnAfterRestart`，分别从普通命令、网页和平台事件进入，并覆盖终态写失败。

### B6 [P1]：interrupted 仍按 settled 回收，没有可见继续入口

位置：`gateway/internal/app/interject_journal.go:260`（任何 outcome 均设 settled=true）、`:463`（恢复补写 interrupted）、`:320`（检查点删除所有 settled）。

独立复现：received → injected → included，无结局 → 重建 App，补写 interrupted → checkpointJournal。实际折叠后 states=0，未完成责任被整个清除。

全文检索也没有找到该 interrupted 状态的查询接口、会话继续状态或恢复提示；现有测试只断言日志里出现 interrupted，不能证明用户可见或能够继续。历史中的正文依然存在，但它不能替代关于“上一请求中断、尚未完成”的可靠状态。

修复：分开“请求已经结束”和“这条用户补充已解决 / 已明确丢弃”。interrupted 应保留为持久可查询、可继续的状态，跨检查点 / 重启仍可见；在用户进入该会话或开始下一轮时说明，并从既有历史继续规划。继续使用新的 run / 请求身份，不自动重放旧工具或旧外部发送。

建议永久回归：`TestInterruptedInterjectionRemainsVisibleAfterCheckpointAndRestart`；验证实际用户可见的查询 / 提示及继续后的结局，不能只断言日志字符串。

### 7.1 下一步交付要求

先修日志追加 / 损坏处理与稳定 ID，再修重新排队、撤回、检查点和 interrupted 状态。每项修复都将上述独立反例变成永久回归，验证“受理 → 中断 → 恢复 → 处理或失效 → 再重启”的完整闭环。补充 Store / Journal 在 Write、Sync 和检查点替换失败时的故障注入，避免只有 OpenFile 失败测试。

本轮仍只验收 B，先不开始后台 Job、完整 Hook 或 Fork。B 通过后沿第 4 节 C 做安静的持久 Fork，保持用户最关心的“保存原上下文，自主活动不打扰主聊天”要求；agent 循环继续由 C 内核承担。


## 8. `5d8bc9a` 复验记录

本节记录上轮问题；`cbdadac` 的已通过范围与剩余项见第 9 节。

### 8.1 已通过的验证

- 重新运行 `go vet ./...`、`go test ./... -count=1`，全部通过，app 包约 23.8 秒。
- 重新运行 C core：205 checks / 0 failures。本次没有 C 源码改动，未重复运行执行器 / 宿主 ASan 套件。
- 用临时 Go overlay 独立重跑上轮七个反例：恢复后实际 drain、完成后再重启；普通撤回后重启；残尾后追加并再重启；重新排队后崩溃；检查点跨重启保留身份和去重；interrupted 跨检查点保留；中间损坏后拒绝追加。七个场景均通过。
- 上述恢复验证实际调用了 drainInterjections，再用原稳定 ID 记录结局；没有仅靠手工构造 injected / outcome 代替 drain。

以下仍是第 6 节既有的 ID、持久提交、历史对账及失效语义在组合场景下的验证。所有运行反例使用真实 App / Store 和临时数据文件；只用 overlay 增加测试，没有修改产品实现。新增测试尚未作为永久测试写入仓库。

### B7 [P1]：检查点没有保持运行中的去重索引与最高接收序号

位置：`gateway/internal/app/interject_journal.go:360`（空 nj.dedup）、`:366`（head 不保存最高序号）、`:415`（以空索引替换 j.dedup）、`:517`（只从保留的状态恢复序号）。

两个独立实测：

1. **不重启的重发。** 受理一条带平台 ID 的补充，追加记录达到真实 512 阈值触发自动检查点，原进程再收到同一平台消息。实际 inbox 从 1 条变成 2 条。原因是 nj.append 不更新 nj.dedup，检查点完成时把原索引替换成空 map；跨重启测试会重建索引，所以发现不了这个问题。
2. **所有旧补充已完成后的无 ID 新消息。** 受理无平台 ID 补充 → 实际 drain 并记 ok → 检查点回收已完成状态 → 重启 → 受理另一条无平台 ID 补充 → 实际 drain。实际新旧 interjection_id 相同，新正文没有进入历史；历史 ID 查重误把新消息当成旧消息。恢复时扫描剩余 received 的最大 Seq 无法保住已经被压缩掉的最高序号。

修复要求：

- 检查点必须保留独立的接收序号高水位，即使没有任何未完成条目；恢复时读取该高水位，不能只遍历 fold 后的未完成状态。
- 检查点成功后，内存去重索引必须与新文件一致。对已完成 / 撤回消息定义有界去重保留窗口，并一并持久化；不能在任意一次检查点时意外清空去重责任。
- 验证无 ID 消息经过多次检查点、重启和失效操作后仍分配不同 ID，且正文确实进入模型请求；仅比较两个手写 ID 的 hash 不足以验证分配器。

建议永久回归：`TestAutomaticCheckpointPreservesDedupWithoutRestart`、`TestCheckpointPreservesReceiveHighWaterAfterAllSettled`。前者必须在原进程中重发；后者必须通过真实受理与 drain 检查新正文。

### B8 [P1]：部分写入失败后，同进程重试仍会拼坏日志

位置：`gateway/internal/app/interject_journal.go:172`。错误分支只返回错误，既不截去已经写出的部分，也不将当前句柄标为 broken；尾部修复只有下一次 openJournal 才执行，但当前句柄仍缓存在 a.journals。

独立实测：在临时 Go 测试进程中使用 Linux RLIMIT_FSIZE 让一次真实 Write 只写出 20 字节，然后返回 EFBIG；立即恢复原限制，在同一 App 上再次 journalAppend。第二次追加返回 nil，但 readJournal 报第一行损坏。平台此时可收到“已受理”，重启却无法恢复该记录。测试进程已恢复原限制，没有更改宿主或生产进程的限制。

修复要求：记录追加前的有效偏移；Write / Sync 失败后，只有确认旧提交边界已安全恢复，才能继续追加。若无法确认，立即冻结该日志的受理并保留现场，向用户返回未受理；不能等一次未来重启才修。对 Sync 失败应区分“提交结果未知”与“确定没有写入”，保留按稳定 ID 对账的能力。Store.Append 的失败恢复也应遵守对应规则。

建议永久回归：`TestPartialJournalWriteThenRetryWithoutRestart`，必须注入真实部分写入而非仅让 OpenFile 失败；再覆盖 Sync 失败后的继续受理与重启对账。

### B9 [P1 / P2]：历史对账受最近 400 条限制，且首次补记 injected 后没有继续状态

位置：`gateway/internal/app/interject_journal.go:551`；`gateway/internal/session/store.go:157`、`:175`。

两个独立实测：

1. **[P1] 旧历史再次追加。** received 已提交，正式历史也已提交，但 injected 未提交；其后历史增加 401 条短消息。重启后，FindMessageBySource(..., 400) 找不到原消息，于是将其恢复为 pending。实际调用 drain 后，正式历史中同一个稳定 history_message_id 出现 **2 次**。drain 的最近 50 条查重同样没能兜底。历史达到 400 条不能改变消息是否已经提交的事实；压缩 / Base 变化也需要同等保证。
2. **[P2] 首次恢复不可见。** 不增加后续消息，直接恢复“历史成功、injected 未提交”的状态：代码找到历史并补记 injected 后直接 continue。当前进程的 pending=0、interrupted=0；只有再重启一次，才会识别刚刚补记的 injected 为 interrupted。当前恢复应当完成全部状态转换，不应要求用户额外重启。

修复要求：

- 对当前世代内尚待对账的有限 ID，使用持久索引或流式检索可靠确认；不能把最近 N 条当成正确性的边界，也不需要将全部历史永久加载进内存。检查点、历史压缩、撤回、重置应有一致的 ID 生命周期。
- 找到既有历史后，在同一次恢复中补齐 injected、未完成结局以及内存 interrupted / 可见计数；重建状态不应依赖恢复函数跑第二遍。

建议永久回归：`TestHistoryReconciliationBeyondRecentWindow`（检查真实 drain 后正文仍只有一次）、`TestRecoveredHistoryWithoutInjectedIsImmediatelyContinuable`；另测 Base / 摘要压缩后的同一边界。

### B10 [P1 / P2]：停止与重置未统一清理恢复状态，也未统一检查终态写入错误

位置：`gateway/internal/app/app.go:1172`（无 run 直接返回）、`:1179`（忽略 withdrawInterjections 错误）、`:1198`（重置不清 interrupted / pendingBytes）。平台 recall 入口也仍然忽略 withdrawInterjections 返回值。

三个独立实测：

1. **[P1] 停止恢复的待处理。** 正常受理 → 重启恢复 pending，但尚无新 run → 调用 stop → 再重启。实际 pending 仍为 1，后续普通消息会再次自动带入该补充。用户明确停止后的待处理责任没有终止。
2. **[P1] 撤回终态写失败。** 正常受理 → 注入日志句柄写入失败 → 通过真实 `/stop` 入口停止并收取 cancelled 终态 → 重启。用户已收到“已停止”，补充却再次恢复为 pending=1。当前实现没有使用 withdrawInterjections 的错误结果，仍清空内存并继续正常停止回执。当前“停止执行”和“补充已持久丢弃”的结果必须分别报告清楚。
3. **[P2] 重置留下旧中断提示。** 用真实历史和 included 记录恢复 interrupted=1 → 调用 reset。历史和世代已重置，但当前进程 interruptedCount 仍为 1，下一轮会继续提示旧上下文中还有补充没有处理完；再重启才消失。pendingBytes 也应随 pending 一起清理。

修复要求：stop、reset、普通撤回、平台撤回、删除和身份切换共用明确的失效转换，涵盖 run.inbox、恢复 pending、interrupted、队列和字节计数。没有活跃 run 不等于没有待处理责任。持久终态写失败时保留可对账状态并报告实际失败；停止进程本身仍应及时执行，不能为了写日志继续运行用户已要求停止的工具。

建议永久回归：`TestStopRestoredPendingWithoutRun`、`TestStopReportsJournalFailureAndDoesNotClaimDurableDiscard`、`TestResetClearsInterruptedAndPendingBytesImmediately`；平台撤回和恢复消息也应覆盖写失败。

### 8.2 交付顺序与复验边界

先修 B7 和 B8，再修 B9 的可靠历史对账及 B10 的失效转换；同批补齐 B9 / B10 的 P2 可见状态问题。把以上组合场景加入永久回归，覆盖原进程继续运行与再次重启两个方向。检查点替换的目录更新持久化、Sync 失败和跨文件提交仍按第 6 节的约束检查，不能只检查正常 Write 后的文件内容。

这些完成后再复验 B，通过后按第 4 节 C 开始安静的持久 Fork。此次仍只更新验收文档，没有修改产品源码，没有开始 Fork、后台 Job 或完整 Hook。


## 9. `cbdadac` 复验记录

本节四个反例已在 `2bb1953` 独立复验通过；当前剩余项见第 10 节。

### 9.1 已通过的范围

本轮重新运行 `go vet ./...`、`go test ./... -count=1`，均通过（app 包约 23.8 秒）；C core 205 checks / 0 failures。本次没有 C 源码改动，未重复执行执行器 / 宿主 ASan 套件。

临时 Go overlay 通过七组独立复验：

| 路径 | 实测结果 |
| --- | --- |
| 真实阈值触发自动检查点后，同进程平台重发 | inbox 仍只有 1 条 |
| 所有旧补充已完成，检查点后重启接收另一条无 ID 补充 | ID 不同，实际 drain 后新正文存在 |
| Linux RLIMIT_FSIZE 使真实 Write 部分写入失败，然后同进程重试 | 尾部回退后再次追加成功，日志只保留新的完整记录 |
| 历史追加 401 条后恢复，以及首次补记 injected | 不再重复注入，首次恢复即有 interrupted 入口 |
| 无活跃 run 时停止恢复 pending | 重启后 pending 为 0 |
| 无活跃 run 时停止，但终态写失败 | 有失败说明，内存 pending 保留 |
| 无活跃 run 时重置 interrupted | 中断计数和待处理字节数当场清空 |

以下四个补充反例均已运行并失败，属于 B9 / B10 原有要求的遗漏分支。overlay 仅增加测试，没有替换产品实现；测试用真实 App、Store、日志及跨实例重启。没有把这些临时测试写入产品源码。

### 9.2 [P1，B10]：运行中的 /stop 丢掉 inbox，却没有写入失效记录

位置：`gateway/internal/app/app.go:1185`；`gateway/internal/app/interject_journal.go:560`。

stop 保存 `inbox := s.run.inbox` 后立即清空 run.inbox；cancelRun 之后只调用 discardPending。该辅助函数实际上只处理 s.pending 和 s.interrupted，根本没有处理这份 inbox 快照；函数末尾的 `_ = inbox` 直接丢掉了引用。

真实流程复现：开始任务 → 受理一条补充 → 从普通聊天入口发送 `/stop` → 确认发出内核 cancel → 收取 done(cancelled) 和“已停止” → 新建 Store / App。实际 pending=1。**没有注入任何磁盘故障，正常停止就会使补充重启复活。**

修复：取消执行可以先进行，但 inbox 的稳定 ID 必须交给统一失效转换；成功持久记录后才清理相应状态。失败时保留可对账责任并报告具体失败。不能只把 inbox 清空后让辅助函数重新从已经清空的字段取值。

建议永久回归：`TestActiveStopDiscardsInterjectionAcrossRestart`，必须通过普通 `/stop` 入口和内核 cancelled 收尾，再真实重启；另覆盖取消生成和终态写失败。当前新增的停止测试只覆盖无 run 的恢复 pending，不能覆盖这一分支。

### 9.3 [P1，B10]：运行中的 /reset 没有切换世代，清空成功后补充复活

位置：`gateway/internal/app/app.go:1198`；`gateway/internal/app/undo.go:170`。

活动分支直接清空 run.inbox，再调用只处理 pending / interrupted 的 discardPending，同样遗漏 inbox；而 newGeneration 只存在于空闲分支。内核取消后 afterRun 仅调用 resetHistory，没有增加世代。注释“世代已经换了”与实际执行不符。

真实流程复现：开始任务 → 受理补充 → 从聊天入口发送 `/reset` → 内核取消 → 收到“上下文已清空” → 重建 Store / App。实际 generation 从 0 到 0，pending=1。旧补充重新进入已经清空过的会话。

修复：将活动、空闲两条路径汇入同一份明确的 reset 提交逻辑，确保成功重置一定完成持久世代转换，且只转换一次。统一失效 inbox / pending / interrupted，处理保存世代、失效记录和历史清理的错误；成功说明必须与真实提交结果一致。不能仅在活动分支补一个清空内存语句。

建议永久回归：`TestActiveResetAdvancesGenerationAndDoesNotRestoreInterjections`。验证取消前后、重置完成后再重启的状态，检查 generation、正式历史和 pending；并加入保存世代失败场景。

### 9.4 [P1，B9]：limit=0 只扫描 Base 之后的有效历史，压缩前已提交的正文仍会重复

位置：`gateway/internal/session/store.go:84`、`:162`、`:184`；`gateway/internal/app/interject_journal.go:674` 附近的恢复对账。

FindMessage 和 FindMessageBySource 改为扫描全部 s.History(sid)，解决了最近 400 条限制。但 History 在首次加载时从 Settings.Base 开始读，已压缩前缀仍在正式文件中，却不在这个切片里；因此 limit=0 并不是完整的持久提交查询。

真实流程复现：received 已提交 → 正式历史写成功，但不写 injected → 增加后续消息 → 使用真实 Store.CommitCompact 将前两条压缩，Base=2 → 重启 → 将错误恢复出的 pending 实际 drain。读取完整原始记录后，同一个稳定 history_message_id 出现 **2 次**。

修复：持久 ID / SourceIDs 对账应覆盖当前世代内完整已提交历史，包括 Base 之前的原始记录；模型上下文仍遵守原有 Base / Summary。对有限未决 ID 使用流式检索或持久索引，保持低内存；重置后不要把上一世代的归档当成本世代的提交。

建议永久回归：`TestCompactedHistoryInterjectionIsNotReplayedAfterRestart`，通过真实 CommitCompact 和实际 drain 验证原始历史 ID 只出现一次，并核对首次继续入口。不能只再增加更多未压缩消息。

### 9.5 [P2，B10]：平台撤回先删除 inbox，再检查写盘失败，错误提示与状态不符

位置：`gateway/internal/app/undo.go:197`。

平台 recall 已增加错误提示，但删除 inbox 和减少 inboxBytes 仍发生在 withdrawInterjections 之前。写失败后返回“这条补充还在队列里，稍后再试”，实际已经没有这条待处理项；同进程再次撤回也找不到它。

独立复现：正常受理 → 让日志写返回错误 → 调用真实 recall 分支 → 收到“还在队列里” → 检查 inbox。实际长度为 0，而日志里 received 尚未失效。

修复：与已经修好的 undoLatest 保持同一提交顺序：持久失效成功后再移除内存项和字节计数，失败保留状态与稳定 ID。不能仅增加错误提示。

建议永久回归：`TestPlatformRecallWriteFailurePreservesInboxAndCanRetry`；断言失败后的 ID、字节数、同进程重试，以及后续重启结果。

### 9.6 本轮交付要求

先修上述三个 P1，并在同批修复撤回顺序的 P2；保持已经通过的检查点、高水位和写入回退场景持续通过。停止 / 重置测试明确同时覆盖 **有 run / 无 run**；历史对账同时覆盖 **未压缩 / Base 已推进**。这些维度必须在同一实现上一起成立，不能用空闲分支通过替代运行中分支，也不能用未压缩长历史通过替代持久历史查询。

本轮证明了进程重启与真实部分写入回退的上述行为；未进行断电、真实 Sync 失败或检查点目录持久化的故障注入，不扩大为这些场景已验收。B 验收通过后，仍按第 4 节 C 开始安静的持久 Fork，agent 循环继续使用 C 内核。


## 10. `2bb1953` 复验：正常路径通过，失败收尾与内存回查待修

### 10.1 已通过的范围

重新运行 `go vet ./...`、`go test ./... -count=1`，全部通过（app 包约 23.85 秒）；C core 205 checks / 0 failures。没有 C 源码改动，本轮未重复执行执行器 / 宿主 ASan 套件。

通过临时 Go overlay 独立重跑第 9 节四个反例，全部通过：

- 运行中的普通 `/stop` 入口 → cancel → done(cancelled) → 重启：补充不再恢复。
- 运行中的普通 `/reset` 入口 → cancelled 收尾 → 重启：世代恰好增加一次，正式历史与 pending 均为空。
- 使用真实 CommitCompact 将前缀压缩至 Base=2，再重启并实际 drain：原始历史中稳定 ID 仍只出现一次，并有 interrupted 继续入口。
- 平台撤回第一次写失败时，ID、inbox 和字节数保留；同进程修复日志后重试并重启，撤回项不再恢复。

以下两项失败流程均已运行复现；内存问题也做了独立测量。overlay 只增加测试，没有替换产品实现；未将临时测试写入产品源码。

### 10.2 [P1，B10]：停止失效写失败后，把原本还在的 inbox 再复制一遍

位置：`gateway/internal/app/app.go:1185`、`:1189`。

新 stop 逻辑先复制 inbox，但没有从 s.run.inbox 移除；discardPending 失败也不会移除这份原数据。错误分支却再次累加 inboxBytes，并执行 append(inbox, s.run.inbox...)，于是恢复变成重复追加。

独立复现：正常受理一条 27 字节补充 → 将日志置于明确的写失败状态 → 从普通聊天入口连续发送三次 `/stop`，每次都确认 cancel 已发送并收到了失效失败说明，暂不交付异步 cancelled 终态。实际 inbox 条数为 **2 → 4 → 8**，字节数从 27 增长到 216。每次重试继续翻倍，待处理条数和字节限制也因此失效。

修复：错误时保留原状态即可；若采用快照替换，则应恢复为唯一的一份快照及准确字节数，不能追加。停止重试必须对同一 interjection_id 幂等，失败重试不能复制原消息；同时保持取消执行先于可能阻塞的日志提交。

建议永久回归：`TestActiveStopWriteFailureRetryPreservesSingleInbox`。延迟 cancelled 终态，重复停止多次，逐次核对条数、稳定 ID 和字节数，再验证日志修复后的停止以及重启结果。

### 10.3 [P1，B10]：历史清理真实失败后，活动重置仍回“上下文已清空”

位置：`gateway/internal/app/undo.go:170`；`gateway/internal/app/app.go:1209`、`:1234`。

commitReset 在活动分支只负责世代和补充失效，真正历史清理由 afterRun 执行。afterRun 对 resetHistory 错误仍只写日志，随后无条件返回“上下文已清空”。活动分支 commitReset 提前失败时，也保留 resetAfter；当前即时错误文案已经包含“已停止并清空上下文”，后续收尾还可能再次返回成功。

独立复现：正常受理补充 → 用临时文件占住该会话的归档目录，令真实 ArchiveReset 失败 → 从普通入口 `/reset` → cancel → done(cancelled)。实际收到“上下文已清空”，但当前 History 仍有 1 条，重建 Store 后仍有 1 条。世代 / 失效提交成功不能证明历史已经清理成功。

修复：保留并传播重置各阶段的实际错误，最终说明由完整重置结果决定；历史清理失败必须返回失败或明确的部分成功状态。保存世代、补充失效和历史清理不能互相替代成功依据，不能在 deferred 的 afterRun 里把此前或当前错误覆盖掉。取消执行仍须及时进行。

建议永久回归：`TestActiveResetHistoryCleanupFailureReportsActualState`；同时覆盖 newGeneration 保存失败、失效记录写失败及空闲分支，检查即时说明、最终说明、当前历史和重启状态。不要仅断言 newGeneration 返回了 error。

### 10.4 [P2，B9 / 低内存要求]：每次未命中的历史回查重新加载完整历史

位置：`gateway/internal/session/store.go:217`。

findInArchive 使用 `all := s.loadFrom(sid, 0)`，先把当前正式文件的全部 Msg 和正文构造成切片，再查 ID。对于 Base 已推进的会话，每条全新的插话在内存窗口内必然找不到新 history_message_id，因此正常 drain 就会走这条全文件加载路径。多个未决来源分别查找时还会重复加载。

独立测量：构造已压缩历史，当前有效窗口仅 1 条；在 findInArchive 第一次匹配回调时进行 GC 并读取 Go HeapAlloc，确认测的是本次扫描仍需持有的存活堆，而非累计分配量：

| 已压缩前缀正文规模 | 单次回查增加的存活堆 |
| --- | --- |
| 4 MiB | 约 4.37 MiB |
| 16 MiB | 约 17.25 MiB |

这是临时加载，并非永久缓存，但单次回查内存仍随整份历史线性增长，与既定低内存要求和第 9.4 节的流式 / 索引方向不符。

修复：流式检索当前世代的正式文件，仅保留当前记录和最近匹配项；需要保留“最后一个匹配”的语义时也可以顺序扫描并覆盖单个结果。或提供有界持久 ID 索引；恢复多个未决 ID 时可合并为一次扫描。保持 Base / Summary 的模型上下文行为。

建议永久验证：保留压缩前缀的正确性用例；增加 4 / 16 MiB 等不同前缀规模的内存测量，确认回查保留的堆不再跟整份历史成比例增加。记录行长度上限，区分累计分配与同时存活内存。

### 10.5 下一步范围

先修上述两个 P1，并在同一小步把新增回查改成流式或有界索引，持续保留此前所有通过场景。停止失败重试、重置各阶段失败和低内存回查完成后再复验 B；通过后进入第 4 节 C 的安静持久 Fork。

本轮只更新验收文档，产品源码未修改。未做断电或真实 Sync 失败注入，不将本轮通过结果扩大到这些边界。
