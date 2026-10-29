# v0.9.0 二轮验收与下一步执行清单

日期：2026-10-29（本次整版验收）；首次审查基线：`05e995e`，此前修复提交：`32c97fc`；执行链复验：`8093f20`；持久收件箱首次验收：`33673fd`（含 `7f48078`）；前次复验：`5d8bc9a`、`cbdadac`、`2bb1953`；B 通过提交：`50ee283`；F1 复验：`c3c0489`（含 `049c848`）；Windows W1 复验：`cc66cad`、`c4cccb6`、`e25c1f8`、`c89d832`、`6a63be6`、`f009124`、`a13353c`、`b379366`；第 21 节通过提交：`d3e4787`；逐项提升清单复验：`bccb4d1`；整版验收基线：`5e57bf0`；集中修复复验：`eb874e2`；后续修复：`f0c598d`；三项收尾核验：`2afecac / 246f77d / df0afa5`；收尾修复复验：`3eb0759`；最终收尾验收：`c66dfa8 / c55229b`。

**最新结论：c55229b 通过当前约定的 Linux＋Windows 收尾验收，第 27 节两处问题关闭，第 26 节三项收尾通过。** 独立反例确认：旧退订只取消旧对象、新对象保留；报告轮实际模型请求读到交接说明；多卡片与归档状态继续正确。Windows 测试已由我从本机 PowerShell 在新的 Windows 本地目录实际运行，12 checks / 0 failures，退出码 0，不再使用“Windows 无环境/无法实测”的结论。Go build/vet/全套测试、相关 race、前端 check/7 项测试/build、Linux C/ASan 全套均通过。macOS 原生验证由用户明确暂缓，不作为本轮阻塞；保留已实现/编译记录，未声称原生通过。详细证据见第 28 节。原各节失败证据保留为历史，不再将已关闭条目计为当前未完成。本轮仅更新评审文档，未改产品源码。

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


## 10. `2bb1953` 复验记录

本节三个场景已在 `50ee283` 复验通过；最新验收结论和下一步见第 11 节。

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


## 11. `50ee283` 复验：B 通过，进入 Fork 存储与显式入口

### 11.1 独立验证结果

重新运行 `go vet ./...`、`go test ./... -count=1`，全部通过，app 包约 24.24 秒；重新运行 C core，205 checks / 0 failures。此次没有 C 源码改动，未重复运行执行器 / 宿主 ASan 套件。

使用临时 Go overlay 重跑第 10 节的真实反例：

| 项目 | 独立复验结果 |
| --- | --- |
| 停止失效写入连续失败 | 从普通 `/stop` 入口重试五次，每次确认 cancel 发出；inbox 始终 1 条，稳定 ID 和字节数不变；修复日志后停止成功，重启不恢复 |
| 活动重置历史清理失败 | 文件占住实际归档目录，收到“清空上下文失败”，重启后历史仍在，说明与实际状态一致 |
| 空闲重置历史清理失败 | 同样制造归档错误，收到“清空上下文时出错”，历史保留 |
| 流式历史回查 | 目标位于 Base 之前，当前有效窗口仅 1 条；ID 与 SourceIDs 均可查到，存在多条匹配时返回最后一条 |
| 回查期间存活堆 | 在扫描中的匹配回调处 GC 后采样，4 MiB 前缀约增加 0.068 MiB，16 MiB 前缀约增加 0.063 MiB；不再随整份前缀大小线性增长 |

本轮没有发现阻塞上述 B 交付范围的新问题。结合已有完整回归及历次反例，持久收件箱 B 通过验收，下一阶段按既定设计推进。

验证范围是 Linux / WSL 下的代码、真实存储、进程重建和已注入的文件错误；没有进行断电、真实 Sync 失败、真实飞书 / Telegram 或 Windows 实机验证，不把本次通过扩展为这些场景的耐久性或平台认证。

### 11.2 非阻塞：加强永久内存回归的夹具与采样

当前 `TestArchiveLookupDoesNotLoadWholeHistory` 把目标消息放在长历史末尾；Compact 后目标可能仍在有效 History，查找会在内存直接命中。该测试也只在回查返回后 GC 再比较 HeapAlloc，无法测出“扫描时整份加载、返回后释放”的临时峰值。

本次已用独立补验把目标放到 Base 之前，并在扫描回调执行期间采样，确认产品实现符合流式与低内存要求，因此这项测试改进不阻塞 B，也不阻塞开始 Fork。

下一次测试整理时，将永久用例改为相同有效路径：目标明确位于压缩前缀；在查找期间采样或使用能检出旧实现的峰值测量；对比 4 / 16 MiB 前缀；保留最后匹配与 SourceIDs 的正确性断言。此次未修改产品测试文件。

### 11.3 下一步只做 F1：Fork 存储与显式入口

对应设计 `docs/DESIGN-agent-runtime.md` 第 8 节及本文第 4 节 C 第一小步。agent 工具调用循环继续由 C 内核负责；F1 建立持久上下文分支，不另写 Go agent 循环。

交付范围：

1. 持久 Fork 操作：用户显式入口与 Session.fork 工具；记录 request_id / operation_id、parent_sid / root_sid、owner_agent、稳定分叉边界和 branch_sid。重试、断线与重启返回同一个操作和分支。
2. 不可变上下文快照：分叉点的 Summary、Base、Omitted、有效消息及来源、附件引用和适用设置完整保存。共享只读快照、独立分支尾部；源会话继续追加、压缩、重置或删除，不改变已就绪分支。
3. 边界闭合：当前模型生成或前台整批工具未完成时等待既定安全边界。同批 Session.fork 先持久预留并返回 pending_boundary，使该调用本身闭合；整批闭合后再发布 ready。源 run 取消或异常结束时持久标识取消 / 待复核，不发布残缺上下文。
4. 轻量展示：父子关系与操作状态可查询，原聊天窗口保持原绑定。分支记录默认 silent 策略；拟人自主调度和所有投递路径的完整贯通放到 F2 单独验收。
5. 低内存与回收：流式生成固定边界快照，复制期间不占 App 总锁；小型只读缓存有上限，闲置分支卸载历史；快照与附件引用的释放可对账。

F1 至少通过以下场景再提交验收：

- 空闲 Fork、生成中 Fork、同批工具内 Fork；模型完整结束 / 工具整批闭合前不发布 ready，同批工具内无等待自身结果的死锁。
- 同一操作重复请求及不同提交点重启，不创建多个分支、不重复发布 ready；未完成操作状态可以检查和恢复。
- 父会话继续追加、压缩、重置、删除后，分支保留分叉时上下文；附件仍可读取，分支尾部不写回父会话。
- 多分支复用同一快照，删除一个分支不破坏其他引用；源会话破坏性操作遵守快照读租约。
- 创建并列出 100 个闲置分支，列表读取轻量元数据，不触发 100 份完整 History / contextInfo 常驻；缓慢磁盘复制期间前台仍可处理其他会话。

F1 验收后再推进 F2：同一拟人 agent 自主创建、继续话题分支；共享心智和额度；以新 sid / run_id 进入原 C 循环；统一 silent 规则覆盖普通回复、工具进度、审批、失败、恢复提示和显式消息工具。最终用记录所有发送行为的假飞书 / Telegram 验证主聊天零推送，用户主动进入分支可查看并继续。

本轮只更新验收文档，没有修改产品源码，没有开始 Fork、后台 Job 或完整 Hook。


## 12. F1 `c3c0489` 复验：六组根因，暂不通过

### 12.1 验证范围和已有通过项

本轮基线是 `c3c0489`，存储前置提交 `049c848`。进入本轮时验收文档已有第 11 节的未提交修改；本轮保留该修改并继续追加，未覆盖产品源码。

- `go vet ./...`、`go test ./... -count=1` 全部通过，app 包约 24.32 秒。
- 重新编译受本次工具注册改动影响的 C 宿主 ASan 版本；C core 205 checks / 0 failures。
- 执行器、宿主 ASan 测试串行执行，均为 0 failures。
- 正常空闲 Fork、未压缩主会话的单层上下文、完成操作重试、简单快照共用等已有测试通过。
- 另用临时 Go overlay 在真实 App / Store 上运行 12 个反例，全部复现下列问题。overlay 只新增测试，没有替换产品实现；运行后已清理。

### F1-R1 [P1]：用户命令重复加锁，慢快照也会阻塞整个 App

位置：`app.go:OnMessage`；`session_fork.go:115 / 224 / 503`。

两个独立复现：

1. 从真实 OnMessage 入口发送 `/session fork 名称`。OnMessage 已持有 App.mu，cmdSessionFork 再调用会获取同一把锁的 SessionFork，250 ms 内不返回，App 锁也不释放。现有测试全部直接调用 SessionFork，没有覆盖聊天命令。
2. 在临时目录用受控 FIFO 模拟慢源文件读取，Fork 进入 SnapshotIDFor 后保持读取未完成；另一 goroutine 调用轻量 Branches 也被阻塞，直到释放源文件 I/O。SessionFork 和 endRun 中的 materializeForkLocked 都在 App 总锁内完成哈希和磁盘复制。

修复：已持锁的命令调用内部持锁入口；导出入口才负责加锁。快照按第 8.4 节设计固定字节边界与设置版本，建立读租约，在锁外流式复制；重新入锁核对操作、世代与版本后发布。源文件追加可继续，截断、重置、删除等待对应读租约，不能用 App 总锁替代租约。

永久回归：`TestSessionForkCommandDoesNotDeadlock`、`TestSlowForkSnapshotKeepsOtherSessionsResponsive`，后者同时检查其他会话消息处理及列表响应。

### F1-R2 [P1]：Base 被扣两次，Fork 分支时只复制尾部

位置：`session/branch.go:WriteSnapshot / ReadyBranch / loadBranch`；`session/store.go:load / Archive`。

两个独立复现：

1. 父会话 8 条消息，真实 CommitCompact 将 Base 推到 2，有效窗口为 6 条。SnapshotMessages 已去掉 Base 前缀，ReadyBranch 却仍设置 branch.Base=2，loadBranch 再 skip 2，分支只剩 **4 条**。
2. 主会话 3 条 → Fork → 分支追加 1 条尾部 → 从该分支再 Fork。源分支有效历史 4 条，新子分支只有 **1 条**。WriteSnapshot / SnapshotIDFor 只读 `s.file(srcSID)`，它对分支只是尾部文件，继承的快照完全没有进入新快照。

修复：统一虚拟历史的坐标约定，明确物理快照条数、快照有效窗口、分支 Base 与尾部偏移；Omitted、Summary、Checkpoint、Undo、Reset、Compact、Archive 都使用该约定。提供可流式读取“继承快照 + 独立尾部”的统一接口，普通与分支来源均用它计算内容版本和生成快照。检查原始历史的接口也要覆盖继承内容。

永久回归：`TestForkPreservesCompactedEffectiveContext`、`TestForkOfBranchPreservesSnapshotAndTail`；逐条比较消息内容、来源、Summary / Omitted，跨重启再比较，并覆盖分支自己的压缩、撤回和重置。

### F1-R3 [P1]：快照 ID 没有包含完整上下文版本，复用旧设置

位置：`session/branch.go:289`。

SnapshotIDFor 当前只纳入 source_sid、Base、generation、Summary 和历史字节，没有纳入 Omitted 与 SnapshotSettings。

独立复现：父会话 Model=A，创建快照；不追加消息，改为 Model=B 并增加 Omitted；再次 Fork。得到相同 snapshot_id，第二个分支实际仍是 **Model=A、Omitted 为空**。这不是两个相同上下文复用，而是不同上下文被合并成同一版本。

修复：对完整、规范序列化的上下文版本做指纹，包含真实消息边界、Summary、Base、Omitted、来源与适用设置及附件引用。哈希与复制针对同一个租约版本；ReadyBranch 从已冻结快照构建适用设置，不能夹带当时又变化的父会话设置。

永久回归：`TestForkSnapshotVersionIncludesSettingsAndOmissions`，分别改变模型、思考、工作目录、模式、Omitted 等而不追加消息，校验分支继承和复用规则。

### F1-R4 [P1]：操作中间提交、恢复和 ready 发布尚未形成闭环

位置：`session_fork.go:183 / 194 / 275`；`app.go:1138`；`session/branch.go:127 / 431`。

四个独立实测：

| 场景 | 实际结果 |
| --- | --- |
| ForkOp 已落盘，但把 branch_sid 写回操作文件失败；修复后重启重试 | 同一个 request_id 留下 **2 个分支记录** |
| pending_boundary 已落盘，重建 App 后重试同一请求 | 返回原 pending，但 pendingForks 为空，没有恢复所有者或后续驱动，永远等待 |
| 源 run 在所需边界前被 `/stop` 取消 | endRun 无条件 materialize，分支仍发布 **ready** |
| 分支 settings 写入失败 | ReadyBranch 已先写 ready 元数据；查询与同请求重试仍返回 **ready** |

修复要求：

- 一次操作先持久预留不可变 branch_sid，与 operation_id / request_id / 主体 / 来源范围绑定；重启按操作和分支记录对账，补齐同一个操作，不能再次随机分配。
- 持久记录来源 run、generation、边界与上下文 revision，恢复未完成状态。旧 run 已不存在时进入明确的取消 / 待复核状态，不能永久 pending，也不能随意用新会话的最新上下文补做。
- 按真正的模型结束和整批工具结果闭合推进；正常、取消、异常、断线与空闲 compaction 完成分别处理。仅在 endRun 调用一次不足以覆盖所有安全边界。取消 / 异常不能直接 ready。
- 快照、分支设置、引用和操作结果都成功提交后才发布 ready。失败保留可恢复状态，拒绝 continue / start 操作未就绪分支。持久写入错误必须传播；快照读错误不能都当 EOF，状态文件不能仅凭 Write + Rename 就扩大持久性承诺。

永久回归至少覆盖上述四个中断点，并补真实 C 内核 + Go 网关的 Session / 并行兄弟工具往返，检查快照中 assistant.tool_calls 与 tool results 完整配对。现有同批测试直接伪造 exec / result / done，不能替代这条真实执行链。

### F1-R5 [P1]：Session 工具没有验证来源会话和目标分支的访问范围

位置：`nodes.go:129`；`session_fork.go:126 / 343 / 419`。

独立复现：配置只读访客，从非管理员会话取得模型工具表，确认其中有 Session；调用 Session.fork 并指定 source_sid=main。操作成功，新分支复制了管理员主会话上下文。代码只检查源会话存在，没有验证调用者能否读取它；inspect / continue / pause / archive 也没有对应的目标访问校验。

修复：创建、列举、查看和改变分支状态统一进行调用主体与上下文范围校验。默认允许当前可访问会话及授权分支；跨来源必须有明确授权。相同 agent 身份不自动等于任意用户会话共享。幂等键也绑定主体与来源范围，重试不能用别人的 request_id 取出另一范围的分支；模型参数中的 created_by / delivery 不能成为授权依据。

永久回归：只读访客只能操作授权范围；不能 Fork / 查看 / 暂停管理员或其他用户的分支；另一个主体或来源复用 request_id 时不能拿到旧操作。拟人 life 的合法来源授权另设正向用例，不把“默认放开所有 source_sid”当自助 Fork。

### F1-R6 [P1 / P2]：真实界面路径仍加载 100 份历史，缓存预算漏算内容

位置：`app/query.go:104 / 137`；`session/branch.go:332 / 350 / 365 / 393`；`session/store.go:History`。

两个独立复现：

1. 建立 100 个闲置分支并卸载其历史。新 Branches 接口的确只读元数据，但调用现有网页使用的 App.Snapshot 后，store.hist 中驻留了 **100 份分支历史**。SessionInfo 仍调用 contextInfo / History；现有“列表轻量”测试只检查新接口返回条数，没有覆盖实际页面路径。
2. 设置快照缓存预算为 1024 字节，加载两个只有工具调用参数的快照。缓存仍持有 **65558 字节参数**，账面两个条目都是 0；预算只计算 Content / Reasoning。默认 New 也没有初始化实际使用的 snapBudget，名义 8 MiB 的 fallback 函数没有参与准入。

修复：网页列表、拓扑、SessionInfo 使用持久轻量统计，不读完整历史；把 UnloadHistory 接到空闲生命周期，限制普通 hist 与快照缓存共同持有的引用。缓存预算覆盖 Msg 切片、ToolCalls / Arguments、附件引用与字符串等真实保留对象，并正确初始化默认预算。活动模型窗口的峰值单独计量。

永久回归覆盖真实 App.Snapshot / SessionInfo、曾经打开后又闲置的分支、工具参数和附件占主要体积的快照；不仅测从未加载过的分支列表。

### 12.2 F1 的下一步范围

先处理访问范围与命令锁，随后统一虚拟历史 / 快照版本，再完成可恢复操作状态机、锁外复制和内存生命周期。每组以独立小提交带永久回归；保持 C 内核驱动模型 / 工具循环。F2 完整静默投递尚未交付，不能将现有 start / continue 入口当作已经通过“安静自主运行”验收；在 F2 贯通前应清楚限制可用范围。

结合下一节 Windows 问题，当前不推进 F2、后台 Job 或完整 Hook；Windows 落盘修复为下一轮最高优先级。

## 13. Windows issue #2：并入下一轮修复方案

### 13.1 核对依据与优先级

已读取 [issue #2](https://github.com/tofu-zyc/nano-agent/issues/2) 及其 [Windows 0.8.0 附件](https://github.com/user-attachments/files/34007129/nano-agent.Windows.md)，对照当前 `c3c0489` 源码，并核对 Microsoft / Go 官方资料。用户提供的已校验 0.8.0 包实测作为 Windows 复现基线；本轮没有重新运行整套官方包复现。

尝试在 Windows 原生临时目录追加验证 ADS 时，执行被工具自动审查拦截，仅返回 blocked by policy，未执行；本轮改用只读源码和官方资料核验，没有修改 Windows 会话或迁移用户数据。

| 顺序 | 项目 | 判断及本轮目标 |
| --- | --- | --- |
| W1，P0 | 会话落盘和旧 ADS 数据 | 当前代码仍有同类文件名规则；先止住新增错误写入，再保留、恢复旧数据，覆盖所有会话关联文件 |
| W2，P1 | 中文 NANO_HOME / 路径 | C 两处 getenv 与 UTF-8 路径接口不一致；统一平台层宽字符读取，不以改控制台代码页代替修复 |
| W3，P1 | 显式 IANA 时区 | 当前入口未嵌入 tzdata，发布构建也没有 timetzdata；让独立 Windows 包离线可加载时区 |
| W4，P2 | 更新检查的安装识别与能力说明 | 补 .exe 与正确路径比较，检查新版本与自动应用能力分开；保留当前手动更新策略 |
| W5，待定位 | Low Integrity 归因 | 现有源码没有主动降低完整性级别的实现，用户复测未重现；先取启动链证据，不安排盲目权限修改 |

W1 优先于新功能；W2 → W3 随后。W4 可作为独立小提交穿插。F1-R1 / R5 属于新功能发布前必须消除的阻塞，与 Windows 修复分别提交和验收。

### W1：统一磁盘键，恢复 ADS，并兼容原 Linux 数据

当前受影响入口：

- `session/store.go:file`：历史；`session/settings.go:settingsFile / List`：设置和枚举。
- `session/history.go:archiveKey`：归档目录与相关读取 / 清理。
- `app/interject_journal.go:journalPath`：已回执插话的持久日志。
- `persona/persona.go:FileFor` 与 `web/api.go` 的人格文件反向解析。
- `session/branch.go:branchFile / loadBranches`：分支元数据。当前自动 br-* 通常不含冒号，但同一错误编码仍应统一；操作索引、快照引用和恢复不能另用一套磁盘命名解释。

**逻辑 sid 保持原值，仅改变文件系统键。** url.PathEscape 是 URL 编码，不能充当跨平台文件名接口。Windows 还受保留字符、设备名、末尾点 / 空格及大小写规则影响；不能只替换冒号或仅在 Windows 改格式。[Microsoft 文件命名规则](https://learn.microsoft.com/en-us/windows/win32/fileio/naming-a-file)

实现方案：新增统一、带版本的存储键组件，所有上述读 / 写 / List / 反向解析共用；新格式使用带固定前缀的规范 ASCII 编码，确保原始 ID 大小写、字面百分号、中文及不同 UTF-8 字节不会碰撞。可逆编码超出文件名长度时，使用稳定哈希与持久原始 SID 映射，禁止截断 ID。新旧格式由版本或迁移记录明确区分，不能将旧名称恰好长得像新前缀的文件误解码。正常 Linux 旧文件与导出包也要能迁移和读取。

迁移顺序：

1. 在升级启动阶段、恢复模型任务和接收新受理回执之前，盘点正常文件、旧格式键及命名数据流；按原逻辑 sid 建立迁移清单。先提供只读检查结果，保留无法确定归属的项目。
2. 将每个待处理流和相关普通文件复制为可见备份，记录源位置 / stream 名、逻辑 sid、类型、长度与校验和。NTFS 流的命名规则是 filename:stream:type，普通目录枚举不足以发现历史；使用 Windows 的流枚举 API，不只依赖 ReadDir。[Microsoft File Streams](https://learn.microsoft.com/en-us/windows/win32/fileio/file-streams)
3. 流式复制到新键临时文件，验证 JSON / JSONL、消息 ID、世代、插话序号、Base / Summary / Omitted 和附件引用，持久提交后再激活索引。历史、设置、人格、插话与分支元数据按同一会话一起对账。
4. 新旧位置都存在时先核验是否相同；不同内容保留双方并报告冲突。旧 settings.tmp / .new 可以是恢复候选，但不能凭文件名或 base 文件时间直接认定它已提交。**不丢弃旧 ADS，也不直接拼接历史 / 日志。**
5. 迁移清单持久记录阶段、源校验和及目标版本；中断后续做同一项，不重复追加、不复活 withdrawn / discard，不改变逻辑 sid、父子关系或 endpoint 绑定。只在已确认提交后启用新位置；初版保留旧流和备份，不自动清理。

退出条件：在原生 Windows NTFS 上对带冒号的真实渠道 sid 验证写入、重启、枚举、设置、人格、插话回执 / 撤回、归档和 Fork；确认没有新增业务 ADS。覆盖大小写不同 ID、百分号、保留设备名、中文、分隔符、末尾点 / 空格、超长 ID。使用包含旧 ADS 的受控迁移夹具，验证中断恢复、冲突保留、重复执行及 Linux ↔ Windows 导入；全程逐条核对原始数据数量与校验和。

### W2：C 平台层统一环境变量的 Unicode 边界

当前直接入口：`kernel/src/nexec_main.c:219` 的 tmp / cache / skill-cache 根目录；`kernel/src/main.c:1019` 的技能落盘根目录。两者调用 getenv("NANO_HOME")，随后交给按 CP_UTF8 转换的 plat_win 路径函数。plat_home 已用 GetEnvironmentVariableW，证明当前平台层两条来源的编码约定不一致。

新增 `plat_getenv_utf8` 一类明确所有权的接口：Windows 用 GetEnvironmentVariableW 动态取 UTF-16，再转 UTF-8；POSIX 复制 getenv 结果。返回值统一由调用方释放，区分不存在、空值与读取失败；处理缓冲长度变化及转换错误，不静默截断路径。同步审计 C 中其余承载路径的环境变量，替换调用处而非仅修改安装脚本。[GetEnvironmentVariableW](https://learn.microsoft.com/en-us/windows/win32/api/processenv/nf-processenv-getenvironmentvariablew)、[getenv / _wgetenv](https://learn.microsoft.com/en-us/cpp/c-runtime-library/reference/getenv-wgetenv)

退出条件：使用当前源码产物在中文 Windows、传统 ANSI 代码页与 UTF-8 环境下测试中文、空格和非 BMP 字符的 NANO_HOME；安装、启动、Read / Write / Shell、技能传输、tmp / cache 创建均落在预期目录，没有乱码同级目录。使用普通用户运行，重装升级保留 gateway.toml / data；Linux 同一平台接口回归通过。保持 C 内核与执行器的实现，不增加常驻辅助进程。

### W3：Windows 网关独立携带时区数据

当前 `config.Validate` 会执行 time.LoadLocation，而 Windows 入口及发布 / CI 构建没有嵌入 tzdata。优先在 Windows 网关 main 的构建文件中引入 `_ "time/tzdata"`，让命令行、配置检查、手动启动和安装后的进程都使用同一后备数据库；不依赖用户装 Go 或启动脚本恰好设置 ZONEINFO。Go 官方说明该包提供嵌入后备数据，二进制体积约增加 450 KB；这不是为 C 内核增加常驻运行时。[Go time/tzdata](https://pkg.go.dev/time/tzdata)

退出条件：在无 Go 工具链 / 无系统 IANA 数据、无 ZONEINFO 的 Windows 测试环境中运行最终发行 exe，Asia/Shanghai 与一个含夏令时的时区可加载；默认本地时区行为正确，非法时区仍明确报错。核对定时任务和每日唤醒计数的日期边界。Linux 测试或交叉编译成功不能替代这一包级测试。

### W4：修安装识别，保持更新能力与实现一致

`gateway/cmd/nano-gateway/update.go:installedExe` 的期望路径缺少 .exe；同文件 installGatewayVersion 已明确拒绝 Windows 自动更新，`service_windows.go` 也拒绝自重启。因此修后缀并不构成自动更新实现。

使用正确可执行文件后缀；比较规范化的实际文件身份，覆盖大小写、中文路径、链接 / junction 等安装路径别名。将“检查最新版本是否成功”“是否发现新版”“是否支持自动应用”“是否为已安装布局”分开。Windows 正常安装应准确识别，同时 Updatable=false 并明确说明手动升级方式，避免修完后缀反而显示可自动更新、点击后才失败。

退出条件：安装包布局、开发目录及路径别名都有对应测试；网络检查成功、已有新版和网络失败分别显示正确状态；重跑手动安装保留配置、数据、密钥及迁移后的索引。Windows 的完整自更新 / 自重启作为独立未来工作，不混入此小修。

### W5：Low Integrity 先收证据，不改变现有权限模型

当前 sandbox_win.c 是空实现，plat_win 的进程创建路径也未发现 SetTokenInformation / CreateRestrictedToken 等主动降权逻辑；这与用户提供的官方包 Medium 复测相符，但不能据此解释报告者机器上的 Low 来源。

下一轮为执行器准备目录失败补充可诊断信息：实际 UTF-8 路径、原生 Windows 错误码、失败阶段，以及可取得的执行器 / 父进程完整性级别和启动方式。核对版本和可执行文件 SHA256、父进程 / 快捷方式 / 计划任务 / 服务入口，构造普通用户的最小复现；不记录整份环境或密钥。

在真实复现定位之前，保持现有沙箱与审批语义，不对整棵 NANO_HOME 或工作目录统一设置 Low 标签，也不把“执行器主动降权”写成已确认根因。诊断项不阻塞已确认的 W1–W4 修复。

### 13.2 下一轮交付与验收顺序

1. **先 W1 的统一键与兼容读取，再交付可恢复的 ADS 迁移**；Windows 数据恢复与新写入一起验收。同步跑 B 的插话回归，稳定消息 ID / 世代语义保持不变。
2. **W2 → W3**，分别独立提交；用原生 Windows 最终产物跑安装 / 启动 / 工具 / 时区的端到端回归。W4 可独立穿插，W5 保留诊断结论与后续证据。
3. 修第 12 节 F1 六组根因，先锁与访问范围，再上下文 / 操作状态机，最后锁外复制和内存生命周期。使用统一存储键，避免 F1 修完再留出 Windows 文件名分叉。
4. 上述通过后再进入 F2 的安静自主分支；后台 Job、完整 Hook 仍在其后。agent 模型 / 工具循环保持 C，迁移和快照采用有界流式 I/O。

每个交付提交说明修改点、是否迁移旧数据、回滚 / 恢复位置、原生 Windows 实测与 Linux 回归结果。明确区分“源码定位”“测试夹具复现”“官方包实测”，不以编译或同进程缓存命中代替落盘、重启与目录枚举验证。本轮仅完成检查和方案整理，未实施产品修复、Windows 数据迁移或权限调整。


## 14. W1 第一阶段 `cc66cad` 复验记录

本节记录上轮反例；`c4cccb6` 已通过的范围和当前剩余问题见第 15 节。

### 14.1 本轮范围与验证

此次只验收已经声明交付的普通文件键、兼容读与 Store.MigrateKeys。ADS 枚举 / 恢复、插话写入口和人格文件切换，仍按用户明确列出的未完成部分跟踪；不要求本阶段用未经原生验证的 ADS 实现补齐平台范围。

- 重新运行 `go vet ./...`、`go test ./... -count=1`，全部通过，app 包约 24.30 秒。
- C core 205 checks / 0 failures。本提交没有 C 源码改动，未重复运行执行器 / 宿主 ASan 套件。
- 规范 ASCII 键的基本性质及手工调用映射辅助函数的原有回归通过；本轮问题主要在实际写入口、格式归属和切换状态。
- 临时 Go overlay 在真实 Store、真实旧格式文件及重建 Store 上补充九个反例，全部复现下列问题；不需要 Windows 或 ADS 环境。overlay 只新增测试，没有替换产品实现，运行后已清理。
- 保留进入本轮前已有的验收文档修改；产品源码及用户数据未改动。

### W1-R1 [P0]：兼容读后直接创建新尾部，重启遮蔽旧历史；删除新文件又会回退复活

位置：`session/store.go:89 / 167 / 320 / 373 / 402`；`session/history.go:ArchiveReset`。

四条已复现的路径：

| 路径 | 实际结果 |
| --- | --- |
| 旧键历史 1 条 → 普通 Append 一条 → 重建 Store | 追加成功，内存为 2 条；重启只剩新文件中的 1 条 assistant，旧 user 被遮蔽 |
| MigrateKeys 完成 → Reset 成功 → 重建 Store | 新文件删除后回退旧备份，旧历史重新出现 |
| 旧键历史 → 真正调用 Undo | offset 从旧文件算出，但 Truncate 对新文件执行，返回不存在错误 |
| 仅有旧键历史 → Exists | History / List 能读到，Exists 却是 false，调用方对会话存在性的判断不一致 |

旧原始文件在这些反例中仍保留，没有物理销毁；但正常读取已经丢失前缀、回滚或复活已清空内容，不能作为可部署的兼容方案。

修复：在首次新写入前，对原来完整的已提交会话做可靠提升或完成启动迁移；不能让新位置只有增量却被当作完整历史。引入明确的布局激活 / 会话存在与失效状态：一旦新布局激活，重置、删除后新文件不存在也不能回退旧备份。旧备份只能作为恢复来源，不再是自动活动数据。Append、SaveSettings、Undo、Reset、ArchiveReset、Rename、Exists、按 ID 对账均经过同一个权威定位 / 切换接口，不各自猜测文件存在性。

永久回归至少包括旧会话续写后重启、迁移前后重置 / 删除后重启、真实 Undo、Rename 和 Exists。逐条检查历史内容与来源，不能只测试 History 第一次能读出旧文件。

### W1-R2 [P1]：新前缀与合法旧 sid 歧义，已出现串会话读取

位置：`session/storekey.go:48 / 119`；`session/store.go:89`。

独立复现：旧逻辑 sid 是字面字符串 `v1-61`，其旧文件名就是 `v1-61.jsonl`。List 优先 ParseKey 后把它枚举成 sid=`a`；随后 History("a") 直接把同一物理路径当作 a 的新文件，读到 `v1-61` 的历史。

仅给新编码增加前缀无法区分任意旧名称；本例不是新 Key 算法内部 hash 碰撞。旧字面 `v1h-*` 也可能被误判成缺映射的长键而直接消失。ParseKey 还应验证规范编码形式 / Key(sid) 对应关系。

修复：新布局采用明确隔离的目录命名空间（推荐），或提供能验证文件归属、格式与冲突改址的可靠索引，不能凭前缀猜版本。盘点既有 `cc66cad` 试运行产物和旧文件时，无证据确定格式 / 所属 sid 的内容保留并报告，不自动重命名为另一个会话。物理位置冲突时不能覆写保留的旧文件。

兼容规则要按原数据类型还原：旧历史 / settings 对 `.`、`..` 的规则与旧归档 archiveKey 的特殊处理并不相同；当前一个 LegacyKey 不能冒充所有旧格式。

永久回归：旧字面 v1-* / v1h-* 与新目标同时存在、不同大小写 / 百分号 ID、旧点目录特殊名，以及迁移前后跨会话读取隔离。反解必须有明确格式归属。

### W1-R3 [P1]：长 ID 映射没有接到实际写入口

位置：`session/storekey.go:85`；`session/store.go:Append`；`session/settings.go:SaveSettings`。

rememberLongKey 目前仅由 MigrateKeys 和测试显式调用。正常 Append / SaveSettings 都不调用它。

独立复现：普通 Append 和 SaveSettings 成功保存一个超过 96 字节的新 sid；重建 Store 后 List 返回空，sessions/keys 没有对应映射。按已知 sid 打开可能仍可算出 hash 文件，但会话枚举和恢复失去入口。

修复：真实新写入与创建入口共用长键登记逻辑，映射可靠提交并核验归属后才允许受理成功。映射已有不同或损坏内容时保留现场、返回冲突，不能静默覆盖。后续 journal / persona 接入也共用这一注册与解析接口。

永久回归通过实际 Append、SaveSettings、BeginBranch 等入口创建长 ID，不先手工调用 rememberLongKey；跨重启验证 List、读取、修改、删除及映射写失败不假报受理成功。

### W1-R4 [P1]：归档目录存在不代表迁移完成，归档兼容与逐文件恢复缺失

位置：`session/storekey.go:219 / 231 / 266`；`session/history.go:archiveDir / Archives / ReadArchive`。

两个独立实测：

- 仅有旧归档目录时，Archives 只查新目录，旧归档在兼容读取阶段不可见。
- 旧目录有两份归档，新目录已经创建但为空（等价于在创建目录后中断）。连续运行两次 MigrateKeys，仍一份都不复制；HasLegacyKeys 还返回 false，因为它不检查归档目录。

修复：按每个归档文件校验和推进状态；目录已存在时继续逐项核对，缺失项补齐、相同项跳过、不同项报告冲突，不能跳过整目录。校验 / 回读 / 持久提交与普通历史一样成立。全部相关文件就绪后才能激活新归档位置。归档读取、删除、计数、清理和 HasLegacyKeys 的范围需与真实布局状态一致，不能遗漏 branch-only / archive-only 等会话。

永久回归覆盖创建目标目录后中断、只复制第一份后中断、目标含冲突、归档损坏、只有旧归档 / 分支元数据时的发现与恢复。保留旧源，不把“目录创建成功”记为整项迁移成功。

### W1-R5 [P1]：清单提交失败被吞掉，存在新文件也不能证明可恢复迁移完成

位置：`session/storekey.go:259 / 285`。

独立复现：让 sessions/keys/migration.json 的提交真实失败，MigrateKeys 仍返回 nil error 和 Moved=[sid]。当前只在末尾重写一份汇总，忽略写入错误；它没有逐项可恢复阶段，也不能依据新文件是否存在判断冲突或切换是否完成。

修复：逐项记录源身份 / 类型 / 校验和、目标、复制 / 核验 / 激活阶段，持久保存真实提交结果；最终概况可以另存，但不能替代恢复记录。清单失败要向调用方传播，含冲突 / 未知状态时明确为部分完成。重复执行重新核验，不因目标存在就跳过。源读取权限错误、目标读取失败等情况不能一律当“目标不存在”。

迁移当前还用 os.ReadFile 整份加载历史及回读比较，jsonlCheck 也对整份内容 Split；后续修复时一并采用有界流式校验、复制和 hash 比较，保留低内存要求。本轮没有对迁移内存单独计量，不把源码检查写成已测峰值。

永久回归覆盖清单写失败、每个阶段中断、冲突保留与重复执行。只有可验证的阶段完成才能报告成功，不能只在干净目录完整跑一次。

### W1-R6 [P1]：迁移已复制插话日志，但原写入口继续写旧位置

位置：`session/storekey.go:162`；`app/interject_journal.go:journalPath`。

用户已明确说明 journal 写入口尚未切换，但 MigrateKeys 已把 interjections 纳入复制目标。

独立复现：旧键日志已有 received(first) → MigrateKeys 创建新键副本 → 按当前 journalPath 的行为再向旧日志追加 received(second)。新日志没有 second，而 HasLegacyKeys 返回 false。此时两个位置已经分叉；将来直接切新入口会遗漏已经回执的 second。

修复：每类文件的写入口与激活步骤必须同步。尚未接入新键的类型只能做备份 / 待迁移候选，不可宣告已切换；切换时先冻结旧写者、复制核验到固定提交边界、可靠激活新位置，再恢复接收。共享解析 / 长键映射与布局状态，避免 journal、persona 和 session 各自择路。迁移完成前保留各类组件的明确状态，不能仅凭一份镜像让 HasLegacyKeys 返回整体完成。

永久回归使用真实 App 受理两条插话，在迁移 / 切换前后重启，检查回执、稳定 ID、世代、撤回和结局；完整覆盖迁移期间继续收到消息的策略，不止检查磁盘上有副本。

### 14.2 下一步收敛顺序

1. 先修本阶段：独立布局归属 + 权威激活 / tombstone 状态，再统一读写 / 删除路径；优先消除续写遮蔽和重置复活。短键的纯编码逻辑可继续复用，不需要因此重写 agent 循环。
2. 接入真实长键登记，补归档逐项恢复与可靠迁移清单；将上面九个反例落为永久回归。当前产品中 MigrateKeys / HasLegacyKeys 还没有生产调用方，接入启动流程前需明确迁移门禁与受理顺序。
3. 再把插话与人格适配器并入同一个定位 / 切换协议，避免创建无人维护的新键镜像；阶段回归通过后再做原生 Windows ADS 枚举与迁移。当前明确的 Windows 验收缺口继续保留。
4. 然后沿第 13 节推进 W2 → W3 → W4，W5 只做诊断；第 12 节 F1 与后续 F2 的既有边界不变。

本轮只更新验收文档，保留原有未提交内容。没有修改产品源码，没有执行真实旧数据迁移，也没有尝试未验证的 Windows ADS 专用实现。


## 15. W1 `c4cccb6` 复验记录

本节保留该提交的历史证据。S1–S5 的八组直接反例已在 `e25c1f8` 通过；新的删除重建与重命名失败场景见第 16 节，S6 仍按明确未完成项跟踪。

### 15.1 本轮范围与通过项

按用户声明，仅复验独立命名空间、locate / promote、激活 / 墓碑、长键真实写入及普通归档复制；逐项恢复清单、有界流式迁移、插话 / 人格适配、ADS 以及 W2–W5 不当作已完成。

- `go vet ./...`、`go test ./... -count=1` 全部通过，app 包约 24.40 秒。
- C core 205 checks / 0 failures；本提交未改 C，未重复运行执行器 / 宿主 ASan 套件。
- 独立重跑七组上轮直接反例，均通过：旧会话追加后重启历史完整；提升后仅重启的 Reset / Delete 不回读备份；旧会话真实 Undo；旧字面 v1-61 不串到 a；长 ID 经普通 Append 后可枚举；目标归档目录预先存在时逐份复制；迁移汇总写失败会返回错误。
- 另补充十个真实文件场景（含迁移重跑的三个分支），复现下列问题。使用临时 Go overlay、真实 Store / 文件和重建 Store，未替换产品实现；测试后已清理，未修改产品源码。

### W1-S1 [P0]：同名归档内容冲突时覆盖目标，导致已有新内容丢失

位置：`session/storekey.go:272`。

当前归档复制只在两边内容相等时跳过；不相等时直接 writeFileAtomic(dst, data)，没有返回冲突。

独立复现：旧归档 `same.jsonl` 为 legacy-copy，新布局同名归档为 valuable-new-copy → Migrate。返回 nil error，新归档已经变成 legacy-copy，原新内容被覆盖。这个观察与“冲突在 promote 里报错”的提交说明不符。

修复：内容不同应保留双方、返回明确冲突并保持未激活 / 待处理状态，不替换任何一方。目标读取失败需区分不存在与未知错误。promote 也会被 locate 的读取路径调用，因此冲突保护必须在复制原语本身实现，不能只在显式 Migrate 外层补判断。

永久回归：`TestArchivePromotionConflictPreservesBothFiles`，比对新旧两份原始字节，验证重复执行和重启后均不覆盖，且没有发布成功激活状态。

### W1-S2 [P0]：Migrate 忽略 active / deleted，把旧备份重新变成活动数据

位置：`session/storekey.go:Migrate / promote / HasLegacyKeys`。

三个独立实测：

| 前置操作 | 再次 Migrate 的实际结果 |
| --- | --- |
| 提升完成 → Reset → 重建 Store | 从旧备份重新复制历史，清空内容复活 |
| 提升完成 → Delete → 重建 Store | 墓碑没有挡住 promote，旧历史重新出现 |
| 提升完成 → 正常 Append 新消息 → 重建 Store | 把活动新历史与只读旧备份的正常差异报成冲突 |

Migrate 从旧文件枚举所有 sid 后无条件 promote；HasLegacyKeys 只认 active，没有排除 deleted。locate 又先认新文件，导致被重新复制出来的文件优先于墓碑。

修复：所有迁移、发现、读取与写入共用同一份权威状态；active 会话的旧文件是备份，不再重做首次导入，deleted 不能由扫描 / 重放自动恢复。显式创建新代会话与恢复旧备份必须是不同操作，必要时使用 generation / revision。active 与 deleted 同时存在或状态损坏时不能按文件存在性静默选边。清单恢复也要校验状态，不能成为另一条绕过墓碑的通路。

永久回归：`TestMigrationReplayHonorsResetDeleteAndActiveWrites`，分别执行上述三条流程，并检查内容、列表、存在性、状态及重启；删除备份仍保留但不参与自动激活。

### W1-S3 [P1]：未激活半成品可在下一次访问时被当成权威文件

位置：`session/storekey.go:177`；`session/store.go:readFileFor / Delete`。

两个独立实测：

1. 阻断 active 标记写入，第一次 Append 在完整复制后返回提升失败。保持故障不变再次 Append，却成功回执，新消息写入了尚未激活的副本。原因是 locate 一看到目标文件就直接返回，完全绕过最后提交点。
2. 旧历史合法、旧 settings 损坏；Delete 调用 locate 时提升失败，却把错误当作没有文件而继续。随后 Delete 返回成功，但提升已复制的历史仍在；重启后新文件优先于 deleted 标记，内容依旧可读。

修复：将待提交副本与已发布数据明确分开，未完成 promote 只能恢复 / 失败，不能被普通访问当作已激活。权威状态应能区分新建、提升中、活动、已删除及失败待处理；解析已提交视图与执行迁移共用状态约定，但读取不能绕过提交门禁。Delete / Rename 不得吞掉定位错误；破坏性操作以可靠终态或完整成功为依据，再清理文件，失败如实报告。

永久回归：`TestPromotionRetryCannotBypassActivationFailure`、`TestDeletePropagatesLocatorFailureAndHidesNoPartialData`；加入每个提升步骤及终态提交前后中断点，检查成功回执与真实权威状态一致。

### W1-S4 [P1]：Rename 和 List 没有统一消费失效状态

位置：`session/store.go:434`；`session/settings.go:134`。

两个独立实测：

- 旧会话 Rename 成功后，源 active 标记被移除，没有留下不可再次提升的来源终态。重启后读取旧 sid 又从备份 promote，List 同时出现新旧名称。
- 提升后的会话 Delete 成功，Exists=false，但 List 仍从旧备份把它枚举出来。当前 add 函数没有检查 tombstone。

修复：Rename 的来源终态、目标登记和文件移动形成一次可恢复操作；旧备份不能再成为第二个活动会话。列表只展示权威状态允许的会话，不能把备份存在当作会话存在。新代的显式重建应更新状态，不与删除墓碑长期矛盾。目标长 ID 的映射、归档与相关元数据也要在 Rename 路径核对；状态写入错误不能忽略。

永久回归：`TestLegacyRenameDoesNotRecreateSourceAfterRestart`、`TestDeletedBackupIsNotListedAsLiveSession`，覆盖短 / 长目标 ID、失败重试及显式重建。

### W1-S5 [P1]：分支元数据实际上仍写在旧根目录

位置：`session/branch.go:109 / 593`。

branchFile 仍是 `<data>/sessions/<Key(sid)>.branch.json`，loadBranches 也只读旧 sessions 根；两者尚未使用新命名空间。

独立复现：BeginBranch(SID=br-owned) 后，`v1/sessions/<key>.branch.json` 不存在，实际写在旧根的 `62722d6f776e6564.branch.json`。List 按旧格式把这个十六进制名字当作逻辑 sid，返回 `62722d6f776e6564`，没有返回 br-owned。

修复：branchFile、save / loadBranches、SetBranch、DeleteBranch 和分支枚举一并接入权威定位及目录格式归属，校验元数据中的 SID 与文件身份。已有旧根十六进制文件、cc66cad 的 v1-* 文件属于需要盘点的中间版本，不能直接当作任意新逻辑会话去迁移；不确定归属保留并报告。

永久回归：`TestBranchMetadataUsesV1NamespaceAndListsRealSID`；通过真实 Fork 创建、重启、列出和删除，断言没有十六进制 / 编码后的幽灵会话，保留其他分支的快照引用。

### W1-S6 [P2，承接明确未完成的 R4]：旧归档能列出，但内容读取仍只查新目录

位置：`session/store.go:ReadArchive`；`session/history.go:Archives / archiveReadDir / ArchiveCount / DeleteArchive`。

独立复现：只有旧归档时，Archives 返回一份条目，但 ReadArchive(sid, id) 返回 0 条，因为后者直接读新 archiveDir。另从源码看，ArchiveCount 仍只遍历旧根，DeleteArchive 只操作新根；归档操作尚未共享同一权威视图。

这部分与用户已列出的 R4 继续工作合并处理，不把它宣称为全部归档已经通过。逐份迁移与清单完成时，同时覆盖列举、读内容、删除、计数和部分复制视图；使用对应类型的旧名称反解器，尤其核对旧点名称的特殊规则。

永久回归：`TestLegacyArchiveListingAndReadUseSameView`，再覆盖未激活副本、新旧同名冲突、重启、计数和删除。

### 15.2 当前最小修复顺序

1. **先修 S1 / S2 两个 P0**：同名不同内容绝不覆盖；迁移重放服从活动 / 删除 / 新代状态。
2. 修 S3 的发布门禁与错误传播，再统一 S4 的 Rename / List 和 S5 的分支元数据。保持本轮七组已通过场景持续通过。
3. 继续用户已排定的 R4 / R5：逐项可恢复清单、有界流式复制 / 校验，顺带完成 S6 的整个归档读写视图；再接插话和人格适配。每类新入口切换均要覆盖读取 → 更新 → 清空 / 删除 → 重启 → 再迁移，不只跑“复制后立即重启”。
4. ADS 仍在受控 Windows runner / 实机验证后交付；不需要等待 Windows 才能修本节已复现的普通文件错误。W2 → W3 → W4、W5 诊断及 F1 / F2 的原顺序保留。

本轮只更新验收文档，保留原有未提交内容；没有修改产品源码或执行用户数据迁移。


## 16. W1 `e25c1f8` 复验记录

本节保留 `e25c1f8` 的历史证据。`c89d832` 修复了无清理故障的删除重建、原生分支删除，以及目标激活 / 长键映射失败的基本重试；清理失败、旧分支备份与改名各提交阶段的剩余问题见第 17 节。

### 16.1 本轮通过范围

- `go vet ./...`、`go test ./... -count=1` 全部通过，app 包约 24.42 秒。
- C core 205 checks / 0 failures。本轮未改 C，未重新执行执行器 / 宿主 ASan，不能将提交者报告当成本轮独立复测结果。
- 独立重跑第 15 节八组直接反例，均通过：归档冲突保留双方；Reset / Delete / 活动追加后三类迁移重放；激活失败连续重试与修复后提升；普通 Rename 后源名称不复活；删除不出现在列表；分支元数据使用新命名空间并还原逻辑 sid。
- 新增六个失败场景，使用临时 Go overlay、真实 Store / 文件和同目录重建 Store，不替换产品实现。场景归入以下两组根因，尚不能验收为安全升级版本。

### W1-T1 [P0]：删除清理被墓碑挡住，同 sid 重建复活旧内容；分支继承视图还能绕过删除

位置：`session/store.go:Delete / Append / ensureActive / load`、`session/storekey.go:locate`、`session/branch.go:Branch / loadBranches / loadBranch`、`session/settings.go:List`。

Delete 先写 deleted 并移除 active，然后用普通 locate 查找待删文件。locate 看到墓碑就返回不存在，因此这轮删除实际上跳过了历史、设置和分支元数据的物理清理。

随后同 sid 的 Append 虽然拿不到旧上下文，却用 O_APPEND 打开残留旧文件；ensureActive 又新增 active，不处理已有 deleted。当前内存只含新消息，重启后 active 优先，旧正文重新加载；List 则仍过滤 deleted，形成历史可读但列表不可见的矛盾。

独立复现：

| 场景 | 实测结果 |
| --- | --- |
| 原生新布局：写旧消息 → Delete → 重建 Store → 同 sid 写新消息 → 再重建 | 写入后的内存只有 1 条；重启后恢复为旧、新共 2 条；列表没有该 sid |
| 已从旧布局提升：旧备份 + 追加 → Delete → 重建 → 同 sid 写新消息 → 再重建 | 内存只有新消息；重启后 3 条，旧备份内容和删除前追加都出现；列表没有该 sid |
| preparing 分支 → Store.Delete → 重建 | Branch 仍从残留 branch.json 加载已删除元数据 |
| ready 分支，继承父快照 → Store.Delete → 重建 | Exists=false、列表无该分支，但 History 仍返回继承的私有上下文 |

最后一条不需要重新创建：loadBranches 没有消费墓碑，loadBranch 直接读共享快照；普通定位门禁不足以保护分支的虚拟历史视图。

修复要求：

1. 分开“已提交可见视图的解析”和“本次删除所拥有的物理资源清理”。二者使用可信 sid / 键身份，但墓碑不能令清理计划变成空集。清理失败应保留可重试记录并如实返回，不能返回已清理成功。
2. 显式同 sid 重建必须创建干净的新代，先隔离旧代残留，再提交新激活状态；不能把普通 Append 到旧文件当作重建。处理 active / deleted 的一致状态与中断恢复，旧备份仍保持失效，不被新代重新引入。
3. 墓碑和世代必须约束 History、Branch、Branches、Exists、List 以及内存缓存；分支的快照继承视图也受约束。可以保留其他有效分支引用的共享快照，但不能因此继续授权已删除分支读取。

永久回归：`TestDeleteThenRecreateStartsCleanAfterRestart`（原生 / 提升两路）、`TestDeletedBranchMetadataIsNotReloaded`、`TestDeletedReadyBranchCannotReadInheritedSnapshot`。至少检查“删除 → 重启 → 同 sid 新消息 → 重启”的正文、设置、列表与 Exists 一致性，加入文件清理失败、墓碑提交与新代激活之间的中断点。

### W1-T2 [P1]：Rename 先移动数据，再验证目标提交条件，失败后无法可靠重试

位置：`session/store.go:Rename`、`session/storekey.go:rememberLongKey`。

Rename 在移动源文件以后才写目标 active，再登记长键；没有操作记录来区分自己的半成品与既有目标。

| 故障注入 | 实测结果 |
| --- | --- |
| 阻断目标 active 的临时文件写入 | 首次 Rename 报错，但源文件已被移走；修复故障、重建 Store 后重试，返回“目标已存在”，源历史为空 |
| 目标为长 sid，预先放入损坏的映射文件 | Rename 报映射错误，但目标已经被激活、源历史为空；重启后目标因缺少合法映射未能正常列出，列表仍可显示空的源名称 |

修复要求：先校验目标身份、映射和冲突条件；对文件移动及来源失效 / 目标激活建立可恢复的 from/to 操作记录。失败后应保留可用源，或存在明确可重试 / 回滚的操作；重试应识别自己创建的半成品，不能仅以 Exists(to) 拒绝。目标归档和分支内部 SID / Parent 等引用也需纳入重命名契约，不以“几个文件已改名”代替完整提交。

永久回归：`TestRenameActivationFailureCanResume`、`TestRenameLongKeyFailurePreservesSourceOrRecoveryOp`。覆盖短 / 长目标、每个提交点失败 → 同进程重试 / 重启重试，检查源 / 目标可见性、正文、映射、来源终态与操作状态。

### 16.2 下一步最小顺序

1. 先修 T1：删除、同 sid 新代重建和分支继承视图必须遵守同一失效边界。这是继续铺开新存储入口之前的数据隔离阻塞。
2. 将 T2 纳入接下来的可恢复操作设计；与计划中的逐项清单一起落实“源身份 / 校验和 / 阶段 / 最终提交”，但 Rename 自身也必须可重试，不能只有迁移复制可恢复。
3. 继续已排定的逐项迁移清单与有界流式复制，完成第 15 节 S6 的归档列举 / 读取 / 删除 / 计数一致视图，再接插话日志与人格文件。切换每个入口都覆盖删除 / 重建 / 撤回 / 重置 / 重启，不能只测首次读写。
4. ADS 原生 Windows 验证、W2–W5 和 F1 六组根因仍未完成；F2、后台 Job 与完整 Hook 不提前展开。

本轮只更新验收文档，保留此前未提交内容。未修改产品源码，未执行用户旧数据迁移；临时 overlay 探针已清理。


## 17. W1 `c89d832` 复验记录

本节保留 `c89d832` 的历史证据。六个原反例已在 `6a63be6` 独立复验通过，含真实 committed 清理恢复；剩余的写入入口、世代 / 版本绑定与未知阶段问题见第 18 节。

### 17.1 已验证范围

- `go vet ./...`、`go test ./... -count=1` 全部通过，app 包约 24.37 秒；包括提交新增的六个永久用例。
- C core 205 checks / 0 failures。本提交未改 C，未重新运行执行器 / 宿主 ASan；本轮没有扩大到原生 Windows、ADS 或真实断电持久性验收。
- 使用临时 Go overlay 补充六个独立恢复场景，全部复现失败；均走真实 Store、真实文件与同一数据目录重建。权限故障使用普通 Linux 用户对临时目录撤去写权限，测试后恢复；没有替换产品实现或修改用户数据。
- 本轮问题仍在第 16 节要求的“清理失败、提交点中断、旧备份失效、重启后可重试”范围内，不能仅凭正常删除和一次激活失败可重试认定 T1 / T2 完成。

### W1-U1 [P0]：新代没有隔离未清理残留，分支备份也没有消费失效状态

位置：`session/store.go:102 / 199 / 553`、`session/branch.go:491 / 503 / 598`。

| 独立场景 | 实测结果与原因 |
| --- | --- |
| 原有历史 → 撤去 sessions 目录写权限 → Delete 返回清理失败 → 恢复权限 → 重建 Store → 同 sid Append → 再重建 | Append 成功，但历史包含旧私有正文和新消息共 2 条。Delete 如实报错本身已修好，然而 Append 仍打开旧残留 O_APPEND；ensureActive 只清墓碑，没有清理完成门禁或代际隔离 |
| 正常 Delete → 撤去 deleted 目录写权限 → 同 sid Append → 恢复权限并重建 | Append 返回 nil，但墓碑没能删除，冷启动 History 为空、Exists=false。ensureActive 对 Remove(deleted) 使用 `_ =`，吞掉真实提交错误 |
| ready 分支有保留的旧布局 branch.json 备份 → Delete → 重建 → Branch / Branches → 同 sid Append → 再重建 | Branch / Branches 又枚举出旧 ready 元数据；重建后的历史为旧继承快照 + 新正文。loadBranches 仍无条件读旧根，Branch / Branches 也没有墓碑 / 世代过滤 |

第三条在删除后、重建前读取 History 确实返回空，说明 load 中的 isDead 起作用；但元数据枚举绕过它，墓碑一旦被普通 Append 清掉，旧继承关系立即重新授权。因此不能通过“加载历史前补一个 isDead”完成新代边界。

提交报告中的“Branch 与 loadBranches 已统一受墓碑约束”尚不符合实际代码：本提交只改 store.go / storekey.go，Branch 与 loadBranches 没有相应门禁，清掉内存 map 项不等于控制冷启动读取旧备份。

修复要求：

1. 将 deleting / cleanup_pending、deleted、creating、active 及世代放入一份可恢复的权威生命周期记录，或提供有相同保证的状态设计。删除未清理完时可以拒绝重建，或先可靠隔离旧代；绝不能先追加残留文件再取消墓碑。物理存储即使复用路径，也必须先建立空的新代并处理创建提交前后中断。
2. 新代提交的每个错误都要返回并可恢复；active 与 deleted 更新到一半时不能回成功。读取、普通设置保存和后台收尾不能自行把失效旧代变成活动身份；只有明确的新代创建路径有此权限。
3. Branch、Branches、Children、loadBranches、缓存和共享快照继承统一消费权威身份 / 世代。只读保留的旧根元数据在旧代失效后永久失去自动授权；同 sid 新代激活也不能恢复旧分支关系。不要通过删除共享快照规避此问题，其他有效分支的引用仍须保留。

永久回归：`TestDeleteCleanupFailureCannotReviveOldData`、`TestRecreateCannotAckWhenTombstoneRemovalFails`、`TestDeletedBranchLegacyBackupMustStayDead`。覆盖同进程 / 重建 Store、正文 / 设置 / 元信息 / 列表 / 存在性，并让迁移重放仍无法复活旧代。

### W1-U2 [P0 / P1]：Rename 有阶段记录但不读取阶段，重试从头覆盖目标，提交后清理又无法恢复

位置：`session/store.go:469–548`、`session/storekey.go:186–213`；真实启动入口见 `app/endpoints.go:migrate`。

当前只以 op 文件存在判断 resuming，没有反序列化 / 校验 From、To、Stage，也没有按阶段选择后续步骤。恢复时仍从头 os.WriteFile(dst, data)，已经发布的目标和未发布副本共用同一路径。

| 独立场景 | 实测结果 |
| --- | --- |
| 阻断目标 active 临时文件 → Rename 复制完返回激活失败 → 重建 Store 查目标 | Exists(target)=true，History 返回源正文，未提交副本已被普通定位当成新会话发布；locate 的“无旧文件 + 有新文件”分支绕过操作提交状态 |
| 阻断源墓碑临时文件 → Rename 已激活目标但返回失败 → 目标 Append 新消息成功 → 修好故障 → 重试 Rename → 重建 | 重试返回 nil，目标已成功受理的新消息消失，只剩源原文。resuming 绕过目标存在检查，随后无条件截断重写，构成 P0 数据丢失 |
| 先制造激活失败形成真实复制副本 → 修复激活并撤去 sessions 目录写权限 → 重试提交成功、源清理失败 → 恢复权限并重建 → 再重试 | 操作记录已是 committed，但入口先判断 isDead(from)，直接报“源已删除”，清理不能继续，违背错误提示里的“可以重试” |

第一个问题也影响真实启动迁移：App.migrate 先看 Exists(main)，半成品已被视为目标存在时，不会进入 Rename 续办。仅在测试里直接再次调用 Rename，不能证明启动路径可以恢复。

修复要求：

1. 真正读取、校验并驱动操作状态机，持久记录源 / 目标身份和世代、文件集合、长度 / 校验和、阶段。先识别本操作的 committed / cleanup_pending 再判断来源是否失效；提交后的重试只能继续清理，不得重新复制。
2. 复制写入操作私有 staging，完整验证后才发布目标；所有普通定位 / 枚举入口不得看见未提交副本。恢复入口要连接到真实启动 / locate 路径，不能要求用户知道 op 文件后手工续办。
3. 目标一旦可接受新消息，恢复不得再截断或覆盖目标。若提交尚未完成，需阻止目标写入，或对世代 / revision 做冲突检查并保留双方；不可假定“记录存在”就仍拥有任意当前目标。回读字节一致不能证明目标没有后来被其他操作更新。
4. source tombstone、目标发布与内存视图按阶段对账；失败后状态 / 回执一致。Copy、promote、Rename 共用有界复制和冲突保护，但操作级提交不能仅靠逐文件成功替代。清单真正承诺可靠落盘时再落实文件与目录持久化；本轮不把写文件后未 Sync 的阶段记录算作断电保证。

永久回归：`TestPendingRenameMustNotPublishTarget`、`TestRenameRetryPreservesAcceptedTargetMessages`、`TestRenameCommittedCleanupCanResume`。对每个阶段做失败 → 同进程 / 重启恢复，检查已受理正文不丢失、未提交目标不可见、提交后的清理可重试，再经真实 App.migrate 验证启动入口。

### 17.2 下一轮执行顺序与验收门槛

1. 先完成 U1 的删除 / 创建代际边界和分支备份门禁，保持现有六个正常路径用例通过。
2. 再完成 U2 的 Rename 状态机，尤其是“目标接受新消息后绝不被恢复覆盖”。逐项清单可以与此共用状态与复制原语，但不能先铺开更多存储入口而保留这一覆盖路径。
3. 随后继续逐项迁移清单、有界流式复制及第 15 节 S6 的归档一致视图，再接插话日志和人格文件。清单至少包含版本、操作身份、源 / 目标归属与世代、每项长度 / 校验和 / 阶段及最终提交；恢复读取清单并核对现状，不能只在末尾输出汇总。
4. ADS 原生 Windows 验证、W2–W5、F1 六组根因继续保持未完成；F2、后台 Job 和完整 Hook 不提前展开。

本轮只更新验收文档，保留此前未提交内容。没有修改产品源码或执行真实数据迁移；临时 overlay 探针已清理。


## 18. W1 `6a63be6` 复验记录

本节保留 `6a63be6` 的历史证据。`f009124` 已通过 V2 两个原反例、V3 和 V4；V1 的两个原反例仍失败，新的文件集合 / 暂存校验与世代格式兼容问题见第 19 节。

### 18.1 本轮证据和通过范围

- `go vet ./...`、`go test ./... -count=1` 全部通过，app 包约 24.46 秒；C core 205 checks / 0 failures。本轮 C 未改，未重新运行执行器 / 宿主 ASan，没有扩大为原生 Windows / ADS / 断电验收。
- 独立重跑第 17 节六个原反例，均通过：删除清理失败后的 Append 隔离、墓碑移除失败不成功回执、旧分支备份门禁、激活失败不发布副本、目标已受理新消息保留、committed 阶段失败后冷启动续办清理。
- 为保证最后一例真的进入 committed：先阻断源墓碑使目标文件实际发布；修复墓碑，再把 sessions 目录改为只读，使恢复跳过已有目标并提交后在源清理失败；读取 op 确认 stage=committed，再重建 Store 调 ResumeRenames。这个路径通过。
- 提交中的 `TestRenameCommittedCleanupCanResume` 在新目标文件尚不存在时就将 sessions 目录设为只读，实际可能在发布目标文件时失败，停留 copied；其最终重试通过并不能单独证明 committed 恢复。建议永久用例断言注入点的真实阶段，避免测试名称替代覆盖证据。
- 另加六个独立失败场景，使用临时 Go overlay、真实 Store / 文件和同目录重建，权限故障按普通 Linux 用户真实注入并恢复。没有替换产品实现；临时探针已清理。

### W1-V1 [P0]：只有 Append 经过新代隔离，SaveSettings 仍可取消墓碑并复活旧代

位置：`session/settings.go:108–133`、`session/store.go:startNewGeneration / ensureActive`。

两个独立失败场景：

| 操作 | 实测结果 |
| --- | --- |
| 写旧正文 → sessions 目录只读 → Delete 报清理失败 → 恢复权限、重建 Store → SaveSettings → Append 新消息 → 再重建 | 保存设置绕过 startNewGeneration，ensureActive 清掉墓碑；后续 Append 已看不到删除状态，因此接在残留正文后，冷启动历史包含旧、新 2 条 |
| ready 分支有旧根元数据备份 → 正常 Delete → 重建 → SaveSettings → 再重建 | 没有生成 gen 标记；旧 branch.json 又被加载，History 重新返回父快照中的私有上下文；不需要 Append 就已复活 |

`v1/gen/<key>` 当前只写固定的 Key(sid)，作用是“旧根备份已失效”的布尔标记。它不是可用于识别具体某一代的 generation ID；更不能让没有经过 startNewGeneration 的写入自动安全。

修复：先定义统一写入前置契约，区分现有代更新、明确新建一代与旧代迟到写入；Append、SaveSettings、分支发布及恢复操作共用它。设置更新不能凭 sid 隐式清墓碑。若允许设置入口明确重建，须先完成隔离和旧根永久失效，再写新设置；普通旧代收尾应带 expected generation 并被拒绝。读取和缓存也使用同一身份，不能靠“每个入口都补一次 Remove 墓碑”拼出生命周期。

永久回归：`TestSaveSettingsCannotReactivateUnisolatedHistory`、`TestSaveSettingsCannotRestoreLegacyBranch`，再经真实 App 设置更新入口验证；与删除 / 重启 / 再迁移组合，确认设置操作不旁路隔离。

### W1-V2 [P0]：Rename 恢复只按阶段和文件存在判断，源 / 目标变化后仍会删除唯一正文

位置：`session/store.go:575–637`。

| 操作 | 实测结果 |
| --- | --- |
| 复制到 staging 后阻断目标激活，Rename 报错 → 源 Append 一条新消息成功 → 修复故障、重建并 ResumeRenames | 直接发布旧 staging，随后将源标记删除并清理；新受理的源消息在两个 sid 下均不存在，Resume 返回 nil |
| sessions 目录只读，Rename 在 ensureActive(target) 成功后发布文件失败 → 恢复权限、重建 → 目标 Append 第一条新消息成功 → ResumeRenames | 目标文件已存在，被无条件跳过；源原上下文没有并入目标，随后源被删除。最终只剩目标的新消息，源唯一正文丢失，Resume 返回 nil |

第二条说明“从覆盖目标改成跳过目标”仍不能成立：文件存在可能表示本操作已完成发布，也可能是另一次写入创建的文件；两者不能按 Stat 成功合并处理。ensureActive 先于文件发布，使失败后的目标成为可写空会话，刚好暴露这个窗口。

修复：

1. 操作绑定源 generation + revision（或可核对的文件身份 / 长度 / 校验和）与目标分配的 generation。源在复制后变化时必须重新取一致快照，或返回冲突并保留源；不能继续删除变化后的源。
2. 目标提交前整体保持不可读 / 不可写，或使用明确预留状态将后续输入可靠排队 / 拒绝成功回执。若允许并发写，恢复必须有 revision 冲突检测和保留双方的策略，不能把“存在”当成“这就是已发布的那份”。
3. 私有 staging 的文件集合先完整验证，再发布一致目标视图。发现已有但不属于本操作的目标文件时保留源和目标并报告冲突；仅在证明目标完整承接相应源版本以后失效 / 清理源。

永久回归：`TestRenameRecoveryCannotDropPostCopySourceMessage`、`TestRenameTargetWriteBeforePublishCannotLoseSource`；分别覆盖同进程重试与启动 ResumeRenames，检查两侧所有已受理消息都仍有可靠归属。

### W1-V3 [P0]：committed 清理没有绑定旧代，会删掉同 sid 新代已受理的消息

位置：`session/store.go:639–644`、`session/store.go:startNewGeneration`。

独立复现：先制造上文已验证的真实 committed / 清理失败状态 → 恢复权限并重建 → 源 sid Append 成功创建干净新代 → ResumeRenames → 再重建。恢复返回 nil，却把新代 jsonl 删掉；源 History 为空，目标只有旧源正文，新代已受理消息从两侧都消失。

原因是清理计划仍是 `newPath(from, suffix)`，只拥有逻辑 sid，没有记录所清理的那一代资源；“提交后只做清理”不等于“可以清理现在这个路径上的任何文件”。

修复：给每代持久分配唯一 / 单调身份，并把操作可删除的资源绑定到旧 generation。可将旧代清理资源先移到操作私有目录，或在删除前核对 generation / 文件身份；新代可复用逻辑 sid，但不能继承旧操作的删除授权。若身份已变化，旧操作只清自己的 staging / 旧代隔离资源并结算，不碰当前新代。也可以在旧操作结束前拒绝新代创建，但不能先回执成功再被启动恢复删除。

永久回归：`TestOldCommittedCleanupCannotDeleteNewSourceGeneration`。必须先断言 op.stage=committed，再在源 sid 真实 Append 新代，最后调用启动恢复并检查新代正文、设置和列表仍在。

### W1-V4 [P1，错误阶段会触发破坏性清理]：stage 没有白名单，未知值落入 committed 清理

位置：`session/store.go:545–550 / 575 / 601 / 639`。

独立注入一份 JSON 正确、from/to 匹配、stage=`future-v2` 的操作记录 → ResumeRenames。返回 nil，源原生历史被删，目标没有正文；没有检查 stage 的分支直接落入末尾清理。提交说明中的“校验 stage”尚未实现。

修复：用显式 switch 白名单驱动状态，unknown / 缺失 / 不支持版本均在任何文件变更前报错并保留现场；清理只能从已验证的 committed 分支进入。ResumeRenames 同时校验记录版本、文件名与内容身份、路径归属，读取错误不能默默当作无需恢复；故障操作应隔离并明确报告，不赋予默认删除权限。

永久回归：`TestUnknownRenameStageMustNotDeleteSource`，检查未知 / 缺失 / 过新版本、记录身份不匹配均不修改源、目标及现有操作记录。

### 18.2 下一轮应完成同一份状态契约，而不是逐入口取消墓碑

1. 先落实可持久区分具体一代的会话身份，以及统一的“创建 / 更新 / 失效”写入契约；补齐 V1，保留原六例通过。gen 布尔标记可以保留作兼容信息，但不能代替这一身份。
2. Rename 记录补齐源 / 目标世代、源版本和每项复制身份，并明确“预留 / 复制 / 验证 / 发布 / 来源失效 / 清理”各阶段的可见性与写入规则。解决 V2 / V3，任何恢复都不能让已成功受理的正文从所有可靠位置消失。
3. 立即补 V4 的阶段白名单；测试断言真实故障阶段，尤其不能把 copied 的恢复当作 committed 已覆盖。启动恢复成功必须代表身份 / 版本核对完成，遇冲突保留数据并报告，不静默跳过当作完成。
4. 这套身份 / 版本 / 阶段规则与计划中的逐项迁移清单共用，之后继续有界流式复制、S6 归档一致视图、插话日志和人格适配。不要先把尚未收敛的 ensureActive / 墓碑约定复制到更多入口。
5. ADS 原生 Windows 验证、W2–W5、F1 六组根因仍未完成；F2、后台 Job 与完整 Hook 保持后置。以上问题均可在当前 Linux 普通文件环境修复和验证，不依赖 ADS 环境。

本轮只更新验收文档，保留此前未提交内容；未修改产品源码、未执行真实旧数据迁移。临时 overlay 探针已清理。


## 19. W1 `f009124` 复验记录

本节五个精确失败场景已在 `a13353c` 独立复验通过。X4 的旧格式兼容已改善，但读取失败不能降为 0 的要求仍未完成；当前失败边界见第 20 节。

### 19.1 本轮验证范围

- `go vet ./...`、`go test ./... -count=1` 全部通过，app 包约 24.42 秒；C core 205 checks / 0 failures。本提交未改 C，本轮未重跑执行器 / 宿主 ASan，未扩大为原生 Windows / ADS / 断电验收。
- 独立重跑第 18 节六个原反例：四个通过（已有源历史的追加变化保留 / 冲突、发布窗口中目标新消息与源正文同时保留、真实 committed 清理不碰新代、未知阶段拒绝清理）；两个 SaveSettings 场景仍失败。
- 额外三个真实 Store / 文件场景均复现失败：复制后原先不存在的源历史文件被创建、staging 中的必需文件缺失、6a63be6 的合法世代标记兼容。
- 共九个独立叶子场景，四个通过、五个失败。使用临时 Go overlay、真实权限故障及同目录重建 Store；未替换产品实现、未修改用户数据，临时探针已清理。

### W1-X1 [P0，V1 尚未落地]：SaveSettings 实际仍未调用 startNewGeneration

位置：`session/settings.go:108–133`；唯一实际调用仍在 `session/store.go:Append`。

当前实现仍是 locate → 写设置 → ensureActive，没有 startNewGeneration；本提交也没有修改 settings.go。交付报告中的“SaveSettings 与 Append 走同一入口”不符合当前源码。

两个原反例实测结果未变：

1. 删除清理失败、旧正文残留 → 恢复权限、重建 Store → SaveSettings → Append 新消息 → 再重建：历史是旧私有正文 + 新正文共 2 条。
2. ready 分支保留旧根元数据备份 → 正常 Delete → 重建 → SaveSettings → 再重建：旧分支关系和私有继承快照重新可读。

新增的 `TestSaveSettingsCannotReviveAfterDelete` 只覆盖正常 Delete 已经清理所有文件、没有分支备份的情况，在漏接隔离入口时也会通过，未覆盖本轮阻塞条件。

修复与验收：真正接入统一写入前置契约，在任何设置文件写入或墓碑取消之前，完成新代隔离 / 世代登记，或拒绝不具备明确创建授权的旧代更新。保留第 18 节的两个精确条件，不要用普通“删除后设置保存”替代。永久回归沿用 `TestSaveSettingsCannotReactivateUnisolatedHistory`、`TestSaveSettingsCannotRestoreLegacyBranch`，检查冷启动正文、分支、世代、设置与存在性。

### W1-X2 [P0]：内容版本清单只记录当时存在的文件，新增文件不参加校验却参与清理

位置：`session/store.go:Rename` 中 started / copied 的 op.Files 处理与 committed 清理。

独立复现：源会话只有 settings、没有 jsonl → Rename 复制 settings 后因目标激活失败返回 → 重建 Store，源收到第一条消息并 Append 成功，创建 jsonl → 修复故障、ResumeRenames。恢复返回 nil，源和目标 History 都为空，新受理消息从两侧消失。

原因：op.Files 没有 jsonl 项，copied 阶段 `!tracked` 就跳过，未识别“原本不存在、现在新增”的源变化；末尾却按固定 suffixes 删除 jsonl，删除授权超过实际验证的文件集合。

修复：清单应描述完整受管文件集合的状态，显式记录 present / absent、长度、摘要和归属世代；恢复核对新增、消失及修改三类变化。新出现的文件应使操作冲突 / 重新取一致快照，不能悄悄跳过。清理只针对本操作已验证并完整承接的资源；源 generation 也应在发布前核对，不能只在清理时检查。

永久回归：`TestRenameSourceFileCreatedAfterCopyIsNotLost`，至少覆盖 settings-only 源随后首次收到消息，以及复制后新增设置 / 分支元数据；检查错误回执与双方数据归属，不能只断言现有文件长度未变。

### W1-X3 [P0]：记录了摘要，但发布不校验 staging；必需文件缺失被当成可选文件跳过

位置：`session/store.go:Rename` 的 copied 发布循环，`ReadFile(staged)` 的 ErrNotExist 分支。

独立复现：源有唯一原始正文 → 复制到 staging 后阻断目标激活 → 删除 staging/jsonl（故障现场中的必需副本丢失，源仍完整）→ 修复激活并重建 → ResumeRenames。返回 nil，源被失效并物理清理，目标无正文，唯一原文丢失。

源码还显示实际发布字节未与 op.Files 的长度 / sha256 对账：源版本被验证，不代表暂存字节仍完整。当前目标对比使用的是 staged 的现值，无法证明其仍是清单里的那份副本。

修复：按完整清单验证 staging 文件存在性、长度、摘要及文件集合，必需文件丢失 / 内容不符一律在来源失效前报错保留源。只有清单明确 absent 的文件才能跳过。共享复制原语必须将实际复制、暂存回读与最终目标核验绑定同一份身份和摘要；不能仅向操作记录添加字段而恢复时不消费。

永久回归：`TestMissingStagedFileMustNotDeleteOriginal`，再覆盖暂存截断 / 内容替换 / 目标未知读取错误，验证错误返回、源仍可靠保存、操作现场未被成功清理。

X2 / X3 可以直接纳入用户计划中的逐项清单与公共流式复制原语，一并完成，不需要再维护两套不完整的校验协议。

### W1-X4 [P0]：旧版合法世代标记被读成 0，升级后旧分支备份重新获得授权

位置：`session/storekey.go:readGen / legacyDead / bumpGen`。

6a63be6 在同一个 `v1/gen/<key>` 路径写入的是 `Key(sid)`。新实现按十进制 ParseInt，解析失败返回 0；legacyDead 因而为 false。

独立兼容夹具：保留 ready 分支的旧根元数据备份 → 删除并以同 sid 创建干净新代 → 将 gen 文件置为 6a63be6 实际写入的 Key(sid) 格式 → 重建当前 Store。旧 branch.json 又被加载，History 变成旧私有继承快照 + 新代正文。这是合法旧格式的兼容测试，不是把任意损坏文件当正常输入。

readGen 对读取失败、无效内容同样都返回 0，等于把“身份未知”解释为“从未删除重建”，也可能错误授予旧代清理权限。

修复：区分真正不存在、已识别旧格式、当前版本合法计数、未知 / 损坏 / 读取失败；旧格式迁移后保留非零失效语义。未知状态应拒绝破坏性恢复并保留现场，不能默认第 0 代；世代格式应携带版本与 sid 身份，校验合法范围并处理计数错误。旧操作缺少世代 / 清单字段也不能默认拥有当前第 0 代资源，需明确兼容或报错流程。

永久回归：`TestLegacyGenerationMarkerRemainsInvalidating`，再覆盖读取失败、损坏、负值 / 越界、旧操作缺字段，检查分支继承和清理两条授权链都不会降为 0。

### 19.2 下一轮最小闭环

1. 先补 X1 的真实入口漏接与 X4 的世代旧格式兼容，要求上面两个旧反例和旧格式夹具通过。
2. 将 X2 / X3 合入逐项迁移清单与有界流式复制：完整文件集合（含 absent）、源 / 目标世代、每项长度 / 摘要、暂存核验、阶段和最终承接证据。恢复真正消费这些字段，发现变化保留源 / 目标与现场并报告。
3. 保持本轮四个已通过原反例，以及第 17 节的六个反例持续通过；committed 注入须断言真实阶段。不能用静态伪造 committed 记录替代全部提交后恢复场景。
4. 之后完成 S6 归档一致视图，再接插话日志与人格适配。ADS 原生 Windows 验证、W2–W5、F1 六组根因仍未完成；F2、后台 Job 与完整 Hook 后置。

本轮只更新验收文档，保留此前未提交内容；未修改产品源码或执行真实旧数据迁移。


## 20. W1 `a13353c` 复验记录

本节两个直接反例（权限读取错误、真实阶段写失败）已在 `b379366` 独立复验通过；严格格式校验和操作失效归属仍未完成，当前证据见第 21 节。

### 20.1 本轮通过范围

- `go vet ./...`、`go test ./... -count=1` 全部通过，app 包约 24.40 秒；C core 205 checks / 0 failures。本提交未改 C，本轮未重跑执行器 / 宿主 ASan，没有扩展到原生 Windows、ADS 或断电持久性验证。
- 独立通过第 19 节五个精确反例：设置保存前存在删除失败残留；设置保存前存在旧根 ready 分支备份；复制后新增源历史文件；发布前必需 staging 文件缺失；旧格式世代标记下保留旧分支备份。读取 / 更新 / 重建 Store 后均满足相应保留或隔离要求。
- 额外两个故障边界使用真实 Store / 文件和同目录冷启动，均复现失败。权限故障采用普通 Linux 用户的文件读权限；提交记录写失败用同名临时目录阻断。没有替换产品实现，临时 Go overlay 已清理。
- 本轮认可上述五条修复落地；不将其扩大为世代错误处理、所有阶段恢复或整个 W1 已通过。

### W1-Y1 [P0，承接 X4 的未完成错误处理]：世代文件不可读仍返回 0，旧操作删除新代

位置：`session/storekey.go:110–141`、`session/store.go:708–713`。

独立复现：

1. 真正完成改名的来源墓碑与 committed 记录，但用只读 sessions 目录令来源清理失败；读取操作记录，确认 stage=committed、from_gen=0。
2. 修复权限、重建 Store，源 sid Append 成功创建第 1 代，记录当前世代确为 1。
3. 撤去 gen 文件读权限，重建 Store 后 ResumeRenames；恢复返回 nil。
4. 恢复 gen 读权限并冷启动，源新代 History 为空，目标只有旧源正文，新代已受理消息从两侧消失。

readGen 把 EACCES 等所有读取错误解释为 0；committed 清理便误判当前路径仍属于 op.FromGen=0，并删除新代文件。这一要求已在第 19 节 X4 明确提出，旧键格式解析修好没有解决它。

当前对空内容返回 0、任意非数字返回 1，也没有区分已识别的旧 Key(sid) 与损坏 / 未知格式。不能通过统一选择一个“保守数字”解决：数字一旦恰好匹配旧操作，就会重新授予删除权限。

修复要求：

- 世代读取返回可区分的结果和 error；只有真正不存在才允许按约定视为初始 0。读取失败、空文件、未知格式、非法范围均不得获得默认世代。
- 合法旧格式按身份识别并显式兼容，不能把任意非数字都当成第 1 代；计数更新必须在成功读取并验证原值后进行，溢出不得回绕。
- Rename 的发布 / 清理、bumpGen、legacyDead 与分支旧根授权都消费该错误语义。身份未知时保留受影响文件和操作现场、报告错误，不清理也不重新授权旧备份；不能只给一个调用点补判断。

永久回归：`TestUnreadableGenerationCannotAuthorizeCleanup`，使用真实 committed 故障状态与已成功 Append 的新代；另测空 / 损坏 / 负值 / 越界及合法旧键，检查出错时所有新代文件与操作记录仍在，恢复读取权限后可安全续办。

### W1-Y2 [P1]：来源墓碑成功、阶段记录失败，修好故障后仍无法续办

位置：`session/store.go:627 / 637–638 / 697–703`。

独立复现：先因目标激活失败留下 stage=copied → 修复激活，阻断 op.json.tmp 写入 → 再次 Rename：目标发布和来源墓碑成功，写 committed 阶段失败 → 移除故障并重建 Store → ResumeRenames。恢复报“改名的源文件不见了（.jsonl）”，记录一直停在 copied。

源文件物理上仍在，但普通 locate 按墓碑隐藏来源；恢复重新执行 copied 的源可见性检查，无法识别这是本操作已经完成的来源失效步骤。此例目标正文保留，未观察到正文丢失，因此按 P1 记录为恢复无法完成，区别于 Y1 的数据丢失。

修复要求：把提交意图、目标完整承接证据、来源失效的归属与阶段转换形成可恢复协议。例如先可靠落盘 committing 意图，来源失效记录带 operation_id / generation，再依据这些证据补记 committed；或由一份权威事务记录决定两侧可见性。恢复应核对属于本操作的资源，不将普通读视图的“不可见”当作物理缺失。

不能简单看到 source dead / target exists 就宣布成功：也可能是用户另行删除来源或目标已换代。只有验证本操作的身份、世代、复制版本及失效归属后才能续办；缺少证据的旧记录应保留双方并提供明确处理状态。

永久回归：`TestSourceTombstoneBeforeCommitRecordCanResume`，在原始记录中确认 copied、来源墓碑真实存在、目标正文完整；修复故障后经启动 ResumeRenames 完成，并验证来源不复活、目标正文不变、操作清理结束。再覆盖前后每个阶段写失败及明确用户删除不能被恢复绕过。

### 20.2 下一轮最小顺序

1. 先收敛 Y1 的严格世代读取及调用方处理，保持本轮五个原反例持续通过。
2. Y2 可以直接合入用户计划的逐项可恢复清单：版本、operation_id、源 / 目标世代、完整文件集合、每项长度 / 摘要、提交意图与失效归属，不再只用 copied / committed 两个独立文件的先后顺序推测结果。
3. promote / Rename 共用有界流式复制与校验；保持新增 / 消失 / 改写、暂存缺失 / 损坏及目标冲突均保留可靠来源。低内存验证使用真实大文件，不依赖整份 ReadFile；不将哈希字段存在当成恢复核验已完成。
4. 之后继续 S6 归档一致视图、插话日志 / 人格接入。ADS 原生 Windows 验证、W2–W5 和 F1 六组根因仍未完成，F2、后台 Job、完整 Hook 后置。

本轮只更新验收文档，保留此前未提交内容；没有修改产品源码或迁移真实数据。


## 21. W1 `b379366` 复验记录

本节两组 P0、四个精确反例已在 `d3e4787` 独立复验通过；本节保留历史证据。通过范围和后续明确未完成项见第 22 节。

日期：2026-10-28。

### 21.1 已验证通过的范围

- `go vet ./...`、`go test ./... -count=1` 全部通过，app 包约 24.36 秒；C core 205 checks / 0 failures。本提交未改 C，本轮未重跑执行器 / 宿主 ASan，没有扩大为原生 Windows / ADS / 断电验证。
- 独立通过第 20 节两个原反例：真实 committed 清理现场中，gen 文件 chmod 000 后恢复报错并保留新代；恢复权限后可续办。真实 Rename 中，来源墓碑已写、op 临时文件写失败后，修复故障、重建 Store 并 ResumeRenames 可以收尾，目标正文不丢。
- 两条测试均通过实际操作 / 故障产生状态，读取记录确认真实阶段；没有仅靠手工写一份 committed / copied JSON 代替全过程。
- 额外四个失败场景归为下面两组。使用真实 Store / 临时文件、同目录冷启动及临时 Go overlay，未替换产品实现；探针已清理。

### W1-Z1 [P0，承接 Y1]：格式错误仍映射为可授权的世代，旧操作能删新代

位置：`session/storekey.go:118–127 / bumpGen`、`session/store.go:Rename` 的 committed 清理。

权限错误已修，但空内容仍返回 `(0,nil)`，任意非数字仍返回 `(1,nil)`，并非已完成严格世代解析。

两个独立实测均先产生真实 committed / 清理失败记录，再同 sid 成功 Append 新代：

| 故障现场 | 恢复结果 |
| --- | --- |
| 旧操作 from_gen=0，新代真实 gen=1；将 gen 文件截为空文件 | ResumeRenames 返回 nil，将其当作第 0 代；新代已受理消息被删除，两侧均无该消息 |
| 旧操作 from_gen=1，新代真实 gen=2；将 gen 文件置为未知文本 broken-generation | ResumeRenames 返回 nil，将其当作第 1 代；新代已受理消息被删除 |

这两项是持久元数据损坏时的故障注入，不宣称程序正常写入会产生这些字节；第 19 / 20 节已经明确要求未知格式不得获得默认身份。修权限错误不覆盖可读但无效的文件。

修复要求：

1. 只有文件真正不存在才可按约定视为初始 0；存在但为空、非法 / 过新格式、负值、越界均返回错误。计数增长也要验证上界，不能整数回绕。
2. 兼容旧键时校验它确实等于当前 sid 的已知旧格式，不能将任意非数字当作旧标记。格式存在歧义时保留现场并明确迁移，不用猜测的数字授予删除权。
3. 世代解析出错时，Rename 各阶段、bumpGen、legacyDead 和其他旧备份授权均传播或采取不授权的处理；保留源、新代、操作记录。统一读函数已具备 error 返回值，应把格式校验也落实进去。

永久回归：`TestMalformedGenerationCannotAuthorizeCleanup`，包含 empty / unknown 两条真实旧操作与新代分支；同时保留合法旧键和读取权限错误回归。断言错误、新代正文和操作现场均保留，不能只检查 legacyDead 为 true。

### W1-Z2 [P0，承接 Y2 的操作归属要求]：目标删除 / 重置没有使旧改名失效，启动恢复复活旧上下文

位置：`session/store.go:renameOp / Rename / targetHasOurContent / ensureActive`。

两个独立场景都先真实执行 Rename，阻断来源墓碑写入，使目标已发布、记录停在 copied；随后进行明确的新用户操作：

| 后续操作 | 修复故障、重建并 ResumeRenames 的结果 |
| --- | --- |
| Delete(target) 返回成功，目标有持久墓碑且内容已清理 | 恢复返回 nil；ensureActive 清掉墓碑，旧源私有上下文重新写入目标，Exists=true |
| Reset(target) 返回成功，目标 History 已为空 | 恢复返回 nil；目标清空的历史又被 staging 原文填回 |

重置场景也与正常上下文清空入口相关，不能只把问题归为手工编辑操作文件。恢复的是早于删除 / 重置的操作，不能凭旧授权覆盖后来的失效决定。

当前 renameOp 仍只有 From / To / Stage / FromGen / Files，没有目标世代 / 上下文 revision 或来源墓碑的 operation_id 归属。新增 `source dead + target content matches` 分支只证明某些字节存在，不能证明本操作已提交；相同字节也可出现在新代、其他复制操作或用户主动保留的文件中。第 20 节已明确要求不得据此单独认定提交。

修复要求：

1. 为操作分配持久 operation_id，并绑定源 / 目标具体世代、上下文 revision 和失效状态。Delete / Reset / 目标换代要可靠地使更早、涉及该目标的未完成操作失效，或推进一个恢复必须核对的 revision。
2. copied / committing 的恢复在任何目标激活、文件写入或来源失效之前校验这些状态。普通 ensureActive 不具备取消后续删除墓碑的权限；被失效的旧操作不能创建新代或重放 staging。
3. 来源墓碑记录应能证明是谁、针对哪一代执行的失效，或由权威事务记录原子决定提交视图。恢复只有在归属、双方世代与目标完整承接证据均成立时才能补 committed；普通用户删除与本操作提交要能区分。
4. 遇后续失效时保留合法当前状态及必要证据，将旧操作结算为 cancelled / superseded / conflict，不静默发布或清理另一代。不要以一个 source isDead 判断替代提交意图和归属。

永久回归：`TestPendingRenameCannotReviveDeletedTarget`、`TestTargetResetCannotBeUndoneByOldRename`，再覆盖源被用户另行删除、目标换代和内容相同的新代。经真实 Delete / Reset → 冷启动 ResumeRenames 检查目标不复活、来源不被越权清理、操作终态明确；保留真实提交写失败可以安全续办的旧用例。

### 21.2 下一轮收敛门槛

1. 完成 Z1 的严格格式校验；这是现有错误返回接口上的补齐，不能继续默认 0 / 1。
2. 将 Z2 与逐项清单同时落实：版本 / operation_id / 双方世代与 revision / 完整文件集合 / 长度与摘要 / 提交意图与失效归属 / 终态。恢复时先核对授权仍有效，再消费复制内容；删除与重置是必须纳入的后续事件。
3. promote / Rename 的公共流式复制可以继续并入这一轮，但流式和哈希核验不能替代操作授权。先让本轮四个反例、旧两例及各类冲突保留场景持续通过，再扩展 S6、插话和人格入口。
4. ADS 原生 Windows、W2–W5、F1 六组根因仍未完成；F2、后台 Job 与完整 Hook 后置。当前验证范围仍是 Linux 普通文件与进程重启故障恢复。

本轮只更新验收文档，保留此前未提交内容；未修改产品源码或迁移真实数据。


## 22. W1 `d3e4787`：第 21 节修复与公共流式复制验收通过

日期：2026-10-28。

### 22.1 结论与独立验证

本批通过。在第 21 节明确要求的严格世代解析、改名操作归属、后续删除 / 重置失效，以及本次新增的流式复制范围内，未发现新的阻塞。此结论覆盖当前新建的 Rename v2 操作；不把它扩大为所有旧格式事务、断电持久性、Windows / ADS 或整个 W1 已交付。

基础检查：

- `go vet ./...`、`go test ./... -count=1` 全部通过，app 包约 24.35 秒。
- `go test ./internal/session -race` 通过。
- C core 205 checks / 0 failures；本提交未改 C，本轮未重新运行执行器 / 宿主 ASan。

独立十个叶子场景全部通过，使用临时 Go overlay、真实 Store / 文件 / 权限故障和同目录冷启动，没有替换产品实现：

| 场景 | 独立观察结果 |
| --- | --- |
| 第 0 代旧操作、新第 1 代，世代文件为空 | 恢复明确报错，新代已受理消息和活动操作记录均保留 |
| 第 1 代旧操作、新第 2 代，世代文件是未知文本 | 恢复明确报错，不再将未知文本默认授予第 1 代清理权 |
| 目标发布后用户 Delete，随后启动恢复 | 目标墓碑保留、上下文不复活；来源保留；操作结算 superseded 并保留 operation_id |
| 目标发布后用户 Reset，随后启动恢复 | 目标上下文保持为空；来源不被旧操作清理；superseded 终态可读 |
| 世代文件 chmod 000，随后恢复权限再续办 | 首次报错且新代保留；修复权限后恢复可以完成，新代仍保留 |
| 来源墓碑已落盘，写 committed 记录时真实失败 | 记录停在 committing，墓碑 operation_id 与操作记录一致；冷启动正确核验目标并补提交、清理完成 |
| 用户另行 Delete 来源 | 用户墓碑不会冒充本操作提交，旧操作结算 superseded |
| 目标 Delete 后同 sid 创建新代并成功接收消息 | 旧改名不能覆盖目标新代，也不能清理来源；superseded 终态正确 |
| 约 64 MiB 历史改名 | 文件长度及 SHA-256 一致；累计分配符合有界流式路径，详见下文 |
| 旧归档与新归档同名、内容不同 | Migrate 报冲突，新旧原始字节均保留，没有发布 active 标记 |

涉及真实 committed 的测试均经过“来源墓碑写失败 → 修复 → 来源目录删除权限不足”产生现场，并读取记录确认阶段；提交窗口测试也由实际阶段写失败产生，没有仅手工写 JSON 代替真实边界。

### 22.2 公共流式复制的通过范围

源码确认 `filecopy.go` 提供固定 64 KiB 缓冲的复制、文件摘要、前缀摘要、内容相等及版本核验；promote / Rename 已使用这些原语，目标未知读取错误不会被当作不存在后覆盖。

独立改名文件大小 67,303,500 字节；在改名调用前后读取 Go `TotalAlloc`，累计新增分配 675,096 字节，约 0.64 MiB。源 / 目标长度与 SHA-256 一致。这个数字是本次 Rename 调用的 Go 累计分配，不是进程 RSS 或全部常驻内存；它支持“不按完整历史文件分配”的结论。

`writeFileAtomic` 和公共复制均先 Sync 写入文件，再执行原子改名。本轮验证针对普通文件故障与进程重建，不声称已验证断电后的目录持久性。

### 22.3 需补回未完成清单：promote 的逐项可恢复记录

后续更新：`bccb4d1` 已实现本节所列清单与启动入口，下面保留 `d3e4787` 时的历史证据；当前验收及剩余恢复问题以第 23 节为准，不再把清单描述为尚未编写。

本轮已完成公共流式复制和 Rename v2 的操作记录，旧布局提升仍缺此前排定的逐项迁移清单。用户本轮“未完成项”列表漏列了这一项，需要继续保留。

当前源码证据：`storekey.go:357` 的 promote 仍直接逐项复制 / 校验，最后写 active；`storekey.go:465` 的 MigrationItem 只有 SID / Kind / Source / Target / Stage / Bytes；`Migrate` 在 promote 之后收集结果，最后一次写 `migration.json`（约第 538 行）。没有恢复时读取这份清单并按每项身份 / 摘要 / 阶段核对续办的流程。

因此下一轮先完成该项，再进 S6：

1. 对旧布局提升建立有版本与 operation_id 的操作记录，绑定源 / 目标身份、世代 / revision、完整文件集合（包括归档）、长度 / 摘要与提交阶段。复制和状态提交都共用当前安全原语。
2. 在处理每项之前可靠登记，复制 / 验证 / 发布后更新可恢复状态；启动或 locate 触发提升时读取记录并核对现状。已存在目标必须依据内容和操作归属复用或报冲突，不能仅按存在性宣告完成。
3. 新增中断回归：复制完成但状态未记、状态已 verified 后源 / 目标变化、active 提交失败、清单提交失败，以及后续 Delete / Reset / 换代。恢复应保持授权失效、冲突保留双方，不能重新激活旧备份。
4. 归档的每个文件也要纳入集合与阶段，避免清单只覆盖历史 / 设置 / 分支。随后由 S6 统一列举 / 读取 / 删除 / 计数的权威视图。
5. 继续验证真实大文件提升的峰值活跃内存，并区分复制缓冲、逐条 JSON 校验的内存与总分配量；保持本轮 Rename 内存与完整性回归通过。

### 22.4 后续顺序

逐项提升迁移清单 → S6 归档一致视图 → 插话日志 / 人格入口接入 → 原生 Windows / ADS 与 W2–W5 → F1 六组根因。F2、后台 Job 与完整 Hook 仍按既定顺序后置。

`50ee283` 的持久插话历史验收范围不自动扩展到之后的存储入口切换；插话与人格接入时须覆盖当前世代 / revision / 失效约定。

本轮只更新验收文档，保留此前未提交内容；未修改产品源码或迁移真实数据，临时探针已清理。


## 23. W1 `bccb4d1`：逐项清单已落地，恢复授权仍有两条绕过路径

日期：2026-10-28。

### 23.1 本轮验证范围与通过项

源码确认新增 `promotion.go`，提升计划包含版本、operation_id、SID、双方世代 / revision、历史 / 设置 / 分支 / 每份归档的长度与 SHA-256，以及逐项和整体阶段；复制前登记、复制后核验，最终 active 提交。`ResumePromotions` 已接入网关启动，locate 触发时也会读取活动操作记录。清单不是只在结束时写一次的运行报告，第 22.3 节要求的主体已经实现。

基础检查：

- `go vet ./...`、`go test ./... -count=1` 全部通过，app 包约 24.39 秒。
- `go test ./internal/session -race` 通过。
- C core 205 checks / 0 failures。本提交未改 C，本轮未重跑执行器 / 宿主 ASan。

使用临时 Go overlay、真实 Store 和同目录重建独立验证 13 个叶子场景：10 个通过，3 个失败，对应下文两条 P1 和一条 P2。未替换产品实现，也未在仓库增加探针文件。

| 通过场景 | 实际观察 |
| --- | --- |
| 历史已复制，设置复制被真实文件故障阻断，另有分支和归档待处理 | 四类文件均在同一清单；修复后冷启动按原 operation_id 续办，目标逐字节一致，旧源保留 |
| 复制完成、verified 状态尚未记下的恢复窗口 | 在真实中断清单上回退该项状态后重启，按摘要复用目标，不重建操作 |
| 新增源、移除唯一源、改写源、改写目标四种变化 | ResumePromotions 报错，保留冲突清单与目标字节，不写 active；但只看这一步不足，后续访问反例见 AB2 |
| active 已提交，settled 目录不可写，随后成功追加消息 | 修复权限后启动续办保留已受理的追加，并补同一个 operation_id 的 committed 终态 |
| 真实 Delete、成功完成的 Reset、Delete 后同 sid 新代 | 冷启动和后续 History 均不带回旧正文，新代消息保留；本轮并非这三条已通过路径回归 |
| 约 64 MiB 历史提升 | 67,110,720 字节源 / 目标长度和 SHA-256 一致，Go TotalAlloc 增量 4,557,760 字节，约 4.35 MiB |

上述内存数字是提升调用的 Go 累计分配，不是峰值活跃堆或进程 RSS。逐条 JSONL 校验仍有单行扫描缓冲，本次大文件由重复的约 1 MiB 消息构成；结论是该路径不按整份历史文件分配，不能据此声称所有内存指标已经测完。

### W1-AB1 [P1]：superseded 只撤销一个 operation_id，后续访问可新建计划导入相同旧源

位置：`session/promotion.go:379–384 / 400–416`；`session/storekey.go:321`。

独立复现沿用现有 `TestPromotionAfterRevisionChangeDoesNotReactivate` 的 revision 推进方式，但增加“失效收尾后的第二次冷启动与真实访问”：

1. 通过阻断 active 临时文件创建产生真实 verified 清单，源 / 目标历史均已复制，尚未提交。
2. 推进 revision，从第 0 版变为第 1 版。
3. 新建 Store 后 ResumePromotions 正确报旧操作已失效，原操作被移到 settled/superseded。
4. 再新建 Store、再次恢复、调用 History。此时活动清单已移除，gen 仍为 0、没有 active / 墓碑；promoteSession 的无活动清单分支只阻止 gen > 0，于是按当前 revision 创建全新 operation_id，再把同一旧正文提升为 committed。

独立读取两份终态确认：原 operation_id / revision=0 / superseded 与新 operation_id / revision=1 / committed 同时存在；History 返回旧的 original 正文。不是同一个操作被错误重试，而是换操作 ID 绕过持久失效决定。

边界说明：本反例针对“revision 已推进、尚未发布 active”的中断窗口，使用与产品现有回归相同的 bumpRev 注入；不宣称成功返回的完整 Reset 场景失败，该场景独立通过。当前源码和设计以 revision 提前记录失效，不能在活动记录收尾后丢掉这层授权约束。

修复要求：

- 将旧源在该 SID / 世代下是否仍有自动导入资格持久化到权威状态；superseded 不是仅移除活动 JSON。首次导入、新建空会话和恢复失效操作应能区分。
- 所有后续默认读取、枚举、Migrate 和 locate 都消费同一资格，不能靠换 operation_id 重获旧源授权；也不能简单禁止全部非零 revision，破坏对尚未提升旧会话的真实 Reset。
- 用户在失效后发来新消息，如果允许创建干净上下文，应走明确的新上下文提交路径并隔离旧副本；普通消息不等于授权重新导入旧备份。

永久回归：`TestPromotionSupersededRevisionCannotReplanOnLaterAccess`。断言第一次恢复、第二次冷启动、History / Settings / Migrate 与后续新消息均不重新继承失效正文；若允许新建，正文只能含新上下文。不能只验证第一次 Resume 的错误和 superseded 文件存在。

### W1-AB2 [P1]：旧源全部消失时 locate 跳过活动清单，普通 Append 激活未提交副本

位置：`session/storekey.go:321–335`；`session/store.go:539 / 580`；`session/promotion.go:386–390`。

独立复现：

1. 旧会话同时有历史和设置。通过设置目标的临时文件故障让历史项 verified、设置项 copying，设置副本尚不存在，active 尚未提交。
2. 移除旧历史和旧设置，冷启动 ResumePromotions。它正确发现完整源集合变化，写入 conflict 并报错。
3. 接着调用真实 History 和 Append。因为 hasLegacy 已为 false，locate 不再读取冲突清单，直接将现有新位置历史当作新会话文件。History 暴露未提交正文；Append 返回 nil 并通过 ensureActive 写入 active，缺失的设置仍未交付。
4. 再冷启动 ResumePromotions。active 分支没有核对原清单是否具备提交资格，直接把该 conflict 操作结算为 committed；终态里设置项仍是 copying。

实际输出：History 包含 uncommitted copy；AppendError=nil；active 存在；下次恢复 Resume=nil，整体 committed，计划中的 settings 项仍为 copying。此路径把未完成迁移伪装成完整提交，丢掉清单对完整会话集合的保护。

修复要求：

- 权威定位必须先判断“这个新位置由谁创建、是否存在未提交 / 冲突提升”，不能只用旧文件现在还在不在来决定是否需要恢复。没有旧源不等于没有未完成操作。
- 活动 / 冲突清单未解决前，普通 Append / SaveSettings 不得通过 ensureActive 无提示发布半成品；读取也不得将它冒充完整权威视图。需要允许用户另建时，应显式隔离旧操作与副本。
- active 不能单独证明任意活动清单已提交。补 committed 应核对提交归属和清单资格，同时保留“确实提交后正常追加新消息”的通过场景；不能要求已提交历史必须与快照字节完全相同而误拒绝合法追加。

永久回归：`TestPromotionMissingSourcesCannotExposePartialTarget`、`TestPromotionConflictCannotBecomeCommittedThroughAppend`。必须走“真实部分复制 → 旧源全部消失 → 冷启动 conflict → 真实读写 → 再冷启动”，断言未提交副本不被普通入口激活、缺项清单不被升级成 committed，并覆盖 SaveSettings 的同一定位路径。

### W1-AB3 [P2]：终态写入失败被恢复入口静默吞掉

位置：`session/promotion.go:375–388 / 519–521`；`ResumePromotions`。

真实 pending 提升后执行 Delete，再让 promotions/settled 目录 chmod 0500。冷启动 ResumePromotions 返回 nil，但活动操作记录仍在，superseded 终态没有写入。根因是调用 settlePromoteOp / finishPromoteCommitted 时使用了 `_ =`，网关启动路径因此无法输出已经准备好的恢复错误日志。

该场景中删除墓碑仍保护数据，本项未观察到正文复活；问题是磁盘结算失败对调用方不可见，不能宣称收尾成功。修复应保留活动记录，向启动恢复传播可重试错误。对于 active 确已提交的数据操作，可以区分“提交已成功、收尾失败”并记录后者，不能因后置终态失败反向撤销已受理消息；本轮 post-commit 追加测试必须继续通过。

永久回归：`TestResumePromotionReportsSettlementFailure`。覆盖 superseded 和已提交收尾，验证首次失败可见、操作现场保留、修复权限后按原 operation_id 完成，不重放旧数据。

### 23.2 下一轮最小范围

1. 先补 AB1 / AB2 的统一授权和发布门禁，修复 AB3 的错误可见性。范围限于本轮逐项清单与既有存储入口，不增加新的产品功能。
2. 回归同时检查“恢复函数返回什么”和“用户随后读写会发生什么”，至少跨两次冷启动；保留已通过的逐项续办、内容变化冲突、提交后追加、Delete / 成功 Reset / 新代隔离与大文件内存用例。
3. 以上通过后再进入第 15 节 S6：归档列举 / 读取 / 删除 / 计数统一权威视图，覆盖新旧同名冲突、未提交副本、删除后重启及再迁移。逐份归档已进入本次清单，不再把该部分记成尚未实现。
4. 之后依次为插话日志 / 人格入口、原生 Windows / ADS、W2–W5、F1 六组根因。F2、后台 Job 与完整 Hook 继续后置。`50ee283` 的 B 历史验收不自动覆盖这些布局变更。

本轮仅更新评审文档，保留此前未提交内容；未修改产品源码，未迁移真实数据，临时探针已清理。


## 24. v0.9.0 整版验收：主体已实现，仍有两组实际缺陷和范围遗漏

日期：2026-10-29。基线：`5e57bf0`（最新实现收口记录 `85bb07a`）。开始验收时工作区干净。

本轮对应用户授权的全部 v0.9.0 范围，不再要求每个小阶段先单独验收。优先判断功能是否真正接通，并以已复现、影响设计要求的差异作为收尾依据，不新增无关功能或扩大故障模型。

### 24.1 代码核对与通过项

| 范围 | 本轮判断 |
| --- | --- |
| P0/P1/P1b：身份、并行、排队/资源锁、插话 | 已实现，现有完整测试通过；C core 已增至 262 checks |
| P2/P3：C Job、流式输出、后台移交、用户控制与平台展示 | 已有实际实现；真 C 内核 + Go 网关的聊天→后台 Shell→Job→完成事件→Hook 测试通过，节点 Job 集成测试通过 |
| P4：可选沙箱 | 完整测试通过，包括 off 实际关闭、workspace/节点策略和结果往返 |
| P5/P7：可靠事件、Hook、前置屏障、观察器 | 可靠事件/Hook 与 Linux 观察器已实现；Windows/macOS 观察器没有实现，不能统称仅“未验证” |
| P6b：共享经历与独立片段 | 核心改造已实现，事故回归与片段隔离测试通过；没有把它误列为尚未开始 |
| F1/F2：Fork 与安静自主分支 | 快照、话题复用、分支界面等已有实现；直接 Message 静默通过，但派生 Schedule 与自主准入仍有下述实测缺陷 |
| P6：心智数值/档案/账本 | 已有可用入口、权重、维度、锁定和账本；行为映射、时间语义、账本边界及配置/锁定范围未完整实现 |
| W1–W5 与归档一致视图 | 已有后续实现和相应回归，本轮 Linux 测试通过；本轮未重跑原生 Windows/ADS，实施记录中的既往原生实测单独保留 |

实际运行结果：

- `go vet ./...`、`go test ./... -count=1` 全通过，app 包约 40.15 秒。
- session/autonomy/experience/hooks/eventlog/dispatch 六个包 race 全通过。
- app 的 P6b 事故、真实 C Job 用户路径、直接 Message 静默、分支数量/深度用例以 race 重跑通过。
- 网页 `npm run check`：0 errors / 0 warnings；`npm test`：6/6；生产构建通过。
- C/ASan：core 262/0、watch 10/0、runner 4 children/0 failures、exec-stream 273/0、job-io 1573/0、job-manager 87/0；执行器/宿主均 0 failures；node-jobs 的移交、竞争、取消、恢复和唯一结果等集成测试通过。
- Windows 执行器专项被当前测试环境跳过；没有将 Linux 通过扩大为本轮原生 Windows 或真实平台联调通过。

下述独立探针使用临时 Go overlay，仅新增测试，不替换产品实现，运行后清理。共五个叶子反例，均复现预期缺陷：Schedule 泄漏、暂停后启动、预算耗尽后启动、零时间漂移、跨日/重复账本结算。

### V09-R1 [P1]：安静分支可用 Schedule 绕过静默投递

位置：`app/schedules.go:184–212 / 507–512`；`schedule/schedule.go:Task`；`app/exec.go:127`。

独立复现：创建 owner_agent=default、By=life:default 的真实 silent 分支；在该来源调用 Schedule 创建每 30 分钟运行的任务，deliver 指向已存在的主人窗口 fake:c1。接口接受请求，随后拨模拟时钟到到期时间并走真实 ScheduleTick、模型轮次和结束路由。

观察到：

- 新任务的 input.sid 为 cron:<id>，Silent=false。
- 模型最终输出实际出现在 fake:c1：`[定时任务 quiet child]` 加任务正文。
- 原分支的直接 Message 仍会被拒绝，但改用 Schedule 就能发出去。

原因：schedule.Task 没有持久来源分支/投递策略；create/update 直接接受 deliver；launch/completeSchedule 无法继承原分支 silent。当前实施记录称“到点后的那一轮自己也会带安静策略”，与真实路径不符。即便用户允许该分支执行 Schedule 工具，也不等于授权模型自行改变分支交付策略。

集中修复：派生任务持久保存来源及有效投递策略，并在创建/修改、启动、审批和结果通知处消费同一策略。自动来源不能用 deliver 取消 silent；显式用户交付授权单独记录。覆盖任务创建后重启、失败结果和网页投递，保留普通用户有意创建的可投递任务。

永久回归：`TestSilentBranchDerivedScheduleNeverPushes`。测试必须包含到期执行与所有对外发送记录，不能仅检查 Schedule 创建参数。顺带核对 FollowUp/Agenda 的来源继承，但本轮没有将未复现的路径另列为新缺陷。

### V09-R2 [P1]：自主分支只检查并发，绕过暂停与预算准入

位置：`app/session_fork.go:463 / 611–617 / 652–657`；`app/life.go:506`。

两个独立场景均使用真实 ready 分支，调用主体为 life:default：

1. 设置该 agent 心智 Paused=true，然后 SessionBranchAction(continue)。返回成功，实际向内核发送新的分支 input。
2. agent LifeBudget=10，状态已经 Spend(10)、BudgetLimit=10，然后同样 continue。仍返回成功，发送新 input。

来源访问检查和分支并发检查都正常经过；问题在于自动来源只调用 checkAutoBranchRoom，之后直接 startBranchRunLocked。branchDone 事后 Spend 只能记账，不能替代启动前的暂停/总预算约束。BranchMeta 也未带设计中的稳定 activity_id，无法证明跨 sid/run 的同一次活动总步数和预算受到同一上限控制。

集中修复：将自主分支的启动、排队后放行和异步 Fork 完成后启动统一接到自动活动准入；冻结并检查 owner、暂停、预算和 activity 归属，派生分支不能重获新的一份活动额度。保留用户明确手动继续的既有语义，不把用户交互统一挡掉。

永久回归：`TestAutonomousBranchRespectsPauseAndBudget`，分别检查暂停、已用尽额度和排队后再暂停；使用 life 来源并断言没有新 input，另保留主人手动操作正向用例。

### V09-R3 [P2，未完成设计]：心智维度尚未按时间更新，也未接入行为映射

位置：`autonomy/profile.go:Dimension / ScoreIdea / DriftDimensions`；`app/autonomy.go:profileLine`；设计 §7.3。

确定性反例：energy=80、baseline=50、drift=10，在完全相同的时间戳 1000 连续调用两次 DriftDimensions，值依次变成 77、74.3。当前公式是“每次活动向基线靠一个百分比”，now 仅用于记录 Updated；设计规定的是按实际经过小时、固定点数向基线移动，达到基线停止。因此不同唤醒频率会造成不同漂移，即使时间没过也发生变化。

源码核对的其他未完成项：

- 四个维度仅出现在 profile、简报和阈值事件中；ScoreIdea 只使用独立权重，调度间隔和联系候选没有消费对应维度。调整 curiosity/social_drive/focus/energy 尚未实现设计规定的固定、可解释行为映射。
- mind_profile 当前放在自治状态，agent 配置中的默认值/维度配置与当前值分离尚未落实；缺少设计中的受限 agent delta 等接口。
- priority 可以被 Pin，但 Agenda 工具未消费该锁定；实施记录也明确承认。不能将当前全部锁定功能标为完成。

集中补齐这些已经写在方案里的功能，不另加情绪系统。测试应验证同一经过时间的结果不依赖中间活动次数，并验证维度开启/关闭对相应行为的实际影响及硬预算不被突破。

### V09-R4 [P2]：活动账本跨日留下预留，结算也未按 run 去重

位置：`autonomy/ledger.go:ReserveLedger / ReleaseLedger / SettleLedger`；`app/autonomy.go:autonomyDone`。

独立状态层反例：

- 10-28 23:59 预留 exploration；10-29 00:01 结算 same-run。
- 旧日仍 Reserved=1，新日 Starts=1/Tokens=10。
- 再以同一 same-run 结算，新日变 Starts=2/Tokens=20，Runs 两次保存同一 ID。

当前预留没有活动/日期身份，结算按结束时的当前日期寻找预留；最近 run_id 列表只展示、不参与去重。本反例是在实际账本接口上注入跨日和重复结算，不宣称已独立复现进程崩溃后的自动重复回调；但设计要求的跨日结算与幂等凭据尚未实现。当前每日上限又仍使用 life.json 标量，不能把新账本宣称为所有入口共享的完整额度权威。

集中修复：预留绑定稳定活动/run 身份、启动日期和时区版本；跨日结束可靠释放原预留，token 在结束时结算，按明确的日期策略归档并保留 started_at。重复结算不重复记次数/token；补重启/时区变更回归，并将自主分支和 Hook 用量纳入同一预算语义。

### 24.2 明确尚未实现与外部未实测应分开

1. **尚未实现**：Windows/macOS 文件观察器。watch.c 只有 Linux inotify，非 Linux 的 watch_attach 直接返回 -1、supported=false。它们是代码缺口，不是只有测试账号/平台缺失。
2. **尚未完整实现**：P6 中上述行为映射、时间漂移、配置/锁定与活动账本；分支报告当前仍为普通标记消息，列表缺最近进展，属于界面收尾项，不升格为核心运行阻塞。
3. **已有实现但外部未实测**：真实飞书/Telegram 租户联调、真实 SSH/多机长时运行、真实模型和浏览器点击回归。现有假渠道、确定性模型、真 C 内核测试有实际价值，不因缺外部账号否定这些已完成部分；也不把未联调写成实测通过。

### 24.3 集中收尾，不再拆成逐项等待审批

建议一次性完成 R1/R2 两组实际缺陷、R3/R4 设计遗漏及跨平台观察器，并同步修正“全完成”清单。实现、测试和记录可以按模块提交，但不要每修一条就停止等待用户继续。

保持已经通过的真实 C Job→Hook、P6b 事故/片段、直接 Message 静默、Fork 与持久化回归。针对新增修复跑对应永久用例，再跑受影响模块及最终整套检查；没有新变化/失败时不重复扩大测试范围。

本轮产品源码未改、用户数据未迁移；只更新本评审记录，临时探针已清理。


## 25. `eb874e2` 集中修复复验：旧反例通过，仍需补升级与真实入口

日期：2026-10-29。保留进入本轮前已有的第 24 节未提交修改。核对本提交改动和真实执行路径，没有重新扩大整版范围。

### 25.1 已通过的部分

- `go build ./...`、`go vet ./...`、`go test ./... -count=1` 全部通过，app 包约 40.70 秒。
- autonomy/schedule race 通过；app 新增 Schedule 静默、自主准入、agent 维度配置与账本相关定向 race 通过。
- C core 262 checks / 0 failures；watch 10 checks / 0 failures。本提交没有改 C 或前端，本轮未重复执行器/宿主 ASan 或网页套件。
- 用上一轮独立探针重跑原有五个叶子场景，全部通过（仅调整账本函数的新签名）：

| 原反例 | 本轮结果 |
| --- | --- |
| 安静分支新建 Schedule、到点执行并向主人投递 | input.silent=true，假渠道零推送 |
| Paused=true 后自动主体继续分支 | 被拒绝，没有新 input |
| 总预算耗尽后自动主体继续分支 | 被拒绝，没有新 input |
| 同一时间戳重复计算漂移 | 不再变化 |
| 跨日预留/结算与同一 run 重复结算 | 原日 reserved=0/starts=1，次日 tokens=10；重复结算不加账 |

R2 原有直接缺陷可以关闭。R1 的新建路径和 R4 的新格式结算路径通过，不因为后文其他入口的问题撤销这些通过结果。curiosity/focus 等已有映射也有实现与回归，不再统称“维度仅供展示”。

以下六个补验失败项归为四组：一条 Schedule 更新、旧账本加载、配置维度实际漂移、按计算频率漂移、schema 暴露、单 run 两次 delta。使用临时 Go overlay、真实 App/Store/模拟时钟；探针仅新增测试，不替换产品实现，运行后已清理。

### V09-S1 [P1，R4 引入的升级回归]：旧账本 runs 格式导致整份心智状态被空状态覆盖

位置：`autonomy/ledger.go:37`；`app/autonomy.go:63–66 / saveMinds`。

上一提交 LedgerUsage.Runs 的磁盘格式是字符串数组，如 `"runs":["old-run"]`；本提交直接改为 []LedgerRun 对象数组，schema 仍为 2，没有兼容解码。

独立测试写入上一格式的有效 autonomy.json（version=2，包含 paused=true、一条 sentinel 认识与一条含字符串 runs 的账本），从实际 loadMinds/mind 路径重新加载，再保存：

- JSON 解析发生类型错误，loadMinds 仅警告并返回，没有设置阻止覆盖的 mindsErr。
- mind 随后创建全新空状态；paused=false、beliefs=0。
- 下一次正常 saveMinds 后原 sentinel 不再存在，旧的认识和暂停状态实际被覆盖。

这是前一提交正常产生的格式，不是人为损坏文件。R4 的新格式跨日逻辑正确不能抵消升级时丢整份状态的问题。

修复：兼容旧字符串和新对象两种 runs，或在可靠备份后显式迁移；反序列化失败不得当作可写空状态继续覆盖。读取失败与不存在须区分。保留原暂停、认识、日程、预算与其他字段。

永久回归：`TestLegacyLedgerRunsUpgradePreservesMindState`，真实旧格式 → 重建 App → 查看 → 新活动/保存 → 再重启，验证全部原状态保留；另覆盖无法解析时拒绝覆盖且保留原文件。

### V09-S2 [P1，R1 的更新入口遗漏]：安静分支修改已有普通任务仍可推送

位置：`app/schedules.go:scheduleAction`（create 与 update 两分支）。

新建任务会继承 silent，但 update 直接复用已有 task，未消费当前调用来源的静默限制。

独立复现：

1. 主人在 main 创建正常任务，已有投递窗口 fake:c1。
2. 创建一个真实 silent 自主分支。
3. 该分支以 life:default 调 Schedule.update，修改原任务 prompt，使它输出分支内容；请求被接受。
4. 到期后真实 ScheduleTick/模型/收尾路径向主人发送结果。修改后的 task 仍 source=main、silent=false。

因此不能把“创建时保存 silent”当成所有 Schedule 操作已服从统一投递策略。原用户授权普通任务的既定内容/用途，也不等于授权安静分支拿它绕过交付限制。

修复：对静默来源的 create/update/run_now 等动作都核对有效策略；可以拒绝这类会改变普通外发任务的自动请求，或建立明确独立的静默派生任务。不要为了堵漏洞无提示取消正常用户任务的原投递配置。继续保留普通用户修改任务的正向回归。

永久回归：`TestSilentBranchCannotPushThroughUpdatingExistingSchedule`，走实际到期执行并记录全部发送；不能只断言 source/silent 字段。

### V09-S3 [P2，R3 的时间状态接线]：配置维度不漂移，频繁计算仍丢失小数增量

位置：`app/autonomy.go:effectiveDimensions / autonomyDone`；`autonomy/profile.go:DriftDimensions`。

两条确定性补验：

1. 只在 agents.json 配置 energy：initial=80、baseline=50、drift_per_hour=10。真实 Wake→MODEL→DONE 一次，拨钟两小时，再完成一个真实片段。生效值仍为 80，持久 Profile.Dimensions 为空。effectiveDimensions 只返回临时合并结果，实际 DriftDimensions 只处理状态里的 Profile，配置未进入被更新的状态。
2. 两份相同状态 energy=80、baseline=50、drift=1，比较一小时后算一次与每分钟算一次：分别得到 79 和 80。每次将结果舍入到一位小数，即使舍入后没变化也推进 computed_at，分钟级的小数增量被永久丢弃。“同一时间戳不变”已修，但“同一经过时间不依赖活动次数”仍不成立。

修复：统一配置、生效值和持久当前值的更新入口；配置初始化后进入同一惰性计算/阈值路径。保留固定精度余量或稳定计算锚点，不能每轮丢掉小数又推进基准时刻。继续保持无 ticker、有界状态和达到基线停止的设计。

永久回归：`TestConfiguredDimensionDriftsAcrossRealEpisodes`、`TestDimensionDriftIndependentOfActivityFrequency`；分别验证配置→真实片段，以及一小时内不同计算分段得到同样结果。

### V09-S4 [P2，R3 的工具入口和限幅]：模型看不到新 action，delta 实际按调用次数叠加

位置：`app/autonomy_tools.go:57–70 / 334–366`。

模型可见 mindSchema.action.enum 仍为 view/interest_add/interest_remove/belief/reflect，没有 dimension；subject/confidence 的说明也仍只描述 belief。因此 switch 中的新分支没有完整暴露给模型，直接调用 mindCall 的单测不能替代工具 schema 接通。

另在一个真实处于活动状态的 life run 中，连续调用两次 dimension（focus 初值 50，max_delta_per_run=5，每次 delta=5），实际值变成 60。当前只给每次请求限幅，没有按 run 汇总，允许通过多次调用绕过每轮上限。

修复：将 dimension 和可正负 delta 的参数语义暴露到工具 schema/说明，同步前缀和工具签名；在实际 run 上追踪已消费的维度增量并按设计限制累计量，不能每调用一次重新得到全部额度。拒绝无执行归属或不允许修改的维度时给出具体错误。

永久回归：`TestMindDimensionExposedInModelToolSchema`、`TestDimensionDeltaBudgetSharedByCallsInOneRun`；后者覆盖同轮多次调用及下一轮重新计算额度，避免把所有未来活动一起永久锁死。

### 25.2 集中收尾范围

先集中补齐 S1–S4，保留本轮已通过的旧反例和正常任务/手动分支行为，不按每条修复停止等待用户验收。S1 涉及真实升级数据保留，应优先处理。

Windows/macOS 观察器仍是明确代码缺口，R5 本轮未交付；网页分支报告卡片和最近进展仍为界面收尾项。缺实机限制的是验证结论，不自动等于必须搁置实现。若要宣告完整跨平台 v0.9.0，仍需实现并按能力清楚验证；若另行确定 Linux 首发，则应在发布范围中明确写出平台差异，不能写成全平台全部完成。

本轮只更新本评审文档，保留此前第 24 节等未提交内容；未修改产品源码，未迁移用户数据，临时探针已清理。


## 26. `2afecac / 246f77d / df0afa5`：主体已落地，仍需补订阅身份与两个界面边界

日期：2026-10-29。进入时工作区仅本文已有未提交修改，本轮保留。核验范围是观察器、报告卡片和最近进展，不重新扩大整版设计。

### 26.1 已实现与实际验证

| 范围 | 已确认内容 |
| --- | --- |
| 观察器 | Linux inotify、macOS kqueue/线程/管道、Windows ReadDirectoryChangesW/IOCP/UDP 唤醒均已有代码；初始化成功才宣告能力；公开句柄改为 intptr_t |
| 报告卡片 | session.Msg.Report、队列到历史、API 类型、buildBlocks、ReportCard 和旧模板兼容已接通；单份报告已有回归通过 |
| 最近进展 | BranchMeta.Progress/ProgressAt 持久化，列表/面板读取轻量元数据；正常开始、完成和重启已有回归通过 |

实际运行：

- `go build ./...`、`go vet ./...`、`go test ./... -count=1` 全通过，app 包约 41.01 秒。
- 报告结构、旧格式兼容与进展持久化三个 app 用例定向 race 通过。
- 网页 check：0 errors / 0 warnings；语义测试 7/7；生产构建通过。本轮没有浏览器点击实测。
- `make -C kernel test NANO_TEST_NO_WINDOWS=1` 全通过：core 262/0、watch 10/0、runner 4 children/0、exec-stream 273/0、job-io 1573/0、job-manager 87/0；执行器/宿主 ASan 与节点 Job 集成通过。
- Windows/macOS 交叉编译通过沿用实现方记录，本轮未独立重编。尝试以限时、重定向输出、临时目录的原生进程运行 test_watch.exe，启动被工具自动审查拒绝，仅返回 blocked by policy，未执行；没有改用其他入口绕过。本轮不能断言 Windows 原生成功、失败或实际死锁。
- CI 已有 Windows 运行 test_watch.exe 和 macOS make kernel test 路径；本轮没有取得 runner 的实际结果，CI 配置不等于 CI 通过。

两个新增 Go overlay 反例使用真实 App/Store/队列和内核事件入口，均复现下文 G2/G3；探针不替换产品实现，运行后清理。G1 是确定的源码关联分析，未冒充原生实测。

### V09-G1 [P1，观察器]：删除登记会移动数组，后台事件仍指向旧槽位

位置：`kernel/src/watch.c:115–118 / 297–300 / 411–412 / 678–687 / 723–724 / 816–825`。

Windows win_watch.slot 与 macOS kevent.udata 在挂接时保存 items 下标，后台事件队列仍用该下标。公共 na_watch_sync 删除登记时却把剩余 items 向前压缩，没有同步后台对象、udata 或已排队事件。

确定性路径：

1. 登记 A、B，分别在 items[0]、items[1]；B 后台身份固定为 slot=1/gen=B。
2. 新完整集合只保留 B，sync 摘除 A，执行 items[0]=items[1] 并令 n=1。
3. B 正常产生事件仍携带 slot=1；watch_by_slot 检查 slot>=n，返回 NULL，be_ready 丢弃事件。
4. B 的 wd 仍显示已挂接，普通 tick 不会修复映射，因此保留订阅会持续丢变化通知。

异步命令队列还保存可被搬移/覆写的 watch_entry 指针；Windows 退订命令按当前槽位查 ww。同 key 换路径时，旧取消命令也可能作用于该槽位的新对象。generation 能过滤旧事件，但不能修复当前条目和后台对象已不一致的关联。

修复：使用稳定槽位/不可变订阅身份及代际，或实现一致的后台重挂/映射更新；退订绑定具体旧对象/代际，不能从可变指针猜当前对象。保持单工作线程和有界队列。

永久回归：`TestWatchRetainsSecondSubscriptionAfterRemovingFirst`、`TestWatchPathReplacementDoesNotCancelNewGeneration`。原生 Windows/macOS 覆盖两个订阅删前留后、同 key 换路径和旧事件/取消迟到；可移植状态层先覆盖身份转移。平台测试 wait_ready 也应保留 intptr_t，不重新窄化为 int。

这是代码身份关系问题，与缺少真机验证分别处理。

### V09-G2 [P2，报告卡片]：队列合并只保留 head.report，第二份报告被卡片隐藏

位置：`app/app.go:1114–1127 / 1199–1203`；`web/src/lib/conv.ts:buildBlocks`。

独立复现：两个真实分支各有不同结论；父会话正在运行时连续引入两份报告，均受理成功。父 run 结束后，同 endpoint/who 的队列项被合并为一条 user Msg，正文含两份报告，但结构字段只取第一项 head.report。

实测：正式历史只有一份 Report，第二份结论只留在合并正文。前端见到 Report 后只画该卡片、不再显示整段正文，因此第二份报告从卡片界面消失。单份报告与刷新测试通过，尚未覆盖这个普通队列批处理场景。

修复：保留每份宿主报告各自的结构与可见内容；可以在一次模型请求中处理，不能合并正文后只保留第一份 metadata，也不能把普通用户文字藏到卡片背后。

永久回归：`TestQueuedBranchReportsKeepEveryStructuredCard`。覆盖两份报告、报告前后混排普通消息及刷新后的 buildBlocks，断言每份内容都可见。

### V09-G3 [P2，进展状态]：归档运行中分支后，取消回执把 archived 改回 paused

位置：`app/session_fork.go:sessionBranchActionLocked`；`app/life.go:branchDone:515–528`。

独立复现：真实分支 continue 启动 → 主人 archive 返回成功并发送 CANCEL → 内核 DONE(cancelled) → 正常 finish/innerDone/branchDone。

最终实测 status=paused，progress=“已取消：这一轮被打断”，而非已经提交的 archived。新增进展写入无条件重写 BranchMeta.Status，撤销了用户的归档决定，也让“不允许继续已归档分支”的限制失效。

修复：进展记录和会话生命周期决策分开；完成回调不能撤销已提交的 archive/pause 意图。可以记录归档时终止了当前任务，但 archived 必须跨收尾和重启保留。

永久回归：`TestArchivingRunningBranchRemainsArchivedAfterCancel`。走真实归档入口和 cancelled 收尾，重启后仍 archived，继续操作仍按规则拒绝；保留正常完成的进展回归。

### 26.2 集中收尾

三项不再记为“未实现”：主体已有代码。修复范围收敛到 G1 稳定订阅身份、G2 报告批处理、G3 归档状态三处。集中实现和回归，不逐条等待用户验收，不增加产品需求。

保持本轮已通过的 Linux C/ASan、完整 Go、前端和单份报告/进展用例。修复后补上述普通操作回归，取得或明确标注 Windows/macOS 原生执行结果；不以编译或 CI 配置宣告原生通过，也不因此否定已完成的 Linux 和界面主体。

本轮仅更新本评审文档，保留既有未提交修改；未改产品源码，未迁移真实数据。Windows 原生执行被自动审查拦截，未产生本轮原生运行结果。


## 27. 3eb0759：两个旧反例通过，剩余旧代取消与报告上下文

日期：2026-10-29。保留本文已有未提交修改。只核验第 26 节修复及其直接影响，不扩大整版范围。

### 27.1 已通过

- 独立重跑原有两个 App 反例均通过：父会话忙时引入两份报告，正式历史保留两份 Report；运行中归档经 cancelled 收尾仍 archived。G3 关闭，G2 原来的丢卡片问题关闭。
- G1 不再搬移活动条目，删前留后时剩余订阅下标不变；Linux watch 回归通过。但固定槽位不等于固定后台对象，见 H1。
- Go build/vet/完整测试通过，app 约 40.98 秒；报告、旧格式兼容、进展、多报告、归档五个 app 用例定向 race 全通过。
- 网页 check 0 errors/0 warnings，测试 7/7，生产构建通过。
- Linux C/ASan 全套通过：core 262/0、watch 13/0、runner 4 children/0、exec-stream 273/0、job-io 1573/0、job-manager 87/0、执行器/宿主 0 failures，节点 Job 集成通过。
- 本轮没有重新尝试上一轮被自动审查拦截的 Windows 原生执行，也没有 Windows/macOS runner 实际结果。

### V09-H1 [P1，G1 未完成部分]：旧退订命令取消同槽位的新观察对象

位置：kernel/src/watch.c 的 win_drain_commands（518–535）、be_attach（688）、be_detach（700–709）。

命令队列仍只保存 watch_entry 指针，消费时才按 slot 取 w->ww[slot]。可确定地安排如下时序：

1. slot=0 上旧对象 O、gen=1 正在观察旧路径。
2. 主线程 be_detach 把退订命令入队，工作线程尚未消费。
3. 同 key 换路径，be_attach 创建 N、gen=2，并令 w->ww[0]=N。
4. 工作线程消费旧命令，查当前 slot=0 得到 N，设置 N.dead 并 CancelIoEx(N)，O 反而没被取消。

本轮从实际 watch.c 原样提取 be_detach、win_drain_commands 两个函数体，在 Linux C 单元夹具中模拟 OS 调用，控制上述时序。实际结果：

    cancelled_handle=22 old_dead=0 new_dead=1

预期取消旧句柄 11，实际取消新句柄 22。只模拟了外部锁/取消等 OS 调用，没有替换命令的对象选择逻辑；这是确定性单元验证，不是 Windows 原生实测。

新增 Windows 同 key 换路径用例只断言旧路径不报，没有在该段验证新路径仍能上报；新对象被取消也能满足前者。Linux 同类用例没有后台线程，无法检出该时序。

修复：退订绑定不可变旧对象/旧代际，且在其完成回收前保持对象存活；可让工作线程统一拥有挂接/退订/回收，或实现明确的旧对象退休协议。不能仅加 generation 比较后跳过取消，让旧观察泄漏。

永久回归：TestWatchDelayedDetachTargetsOldGeneration。让旧退订消费晚于新挂接，断言取消 O、N 仍观察且新路径确有事件、O 最终回收；保留删前留后的通过用例。

### V09-H2 [P2，G2 引入的回归]：拆分报告绕过准备好的上下文，却标记已经消费

位置：app/app.go:1166–1189 / 1209–1215。

startRun 先将环境、记忆/经历和交接说明放进 content，并更新 EnvNote/MemNote/Handoff 等持久标记。reports 分支随后完全不用 content，直接把各项原始 q.m.Text 写入历史。卡片保住了，正常上下文预处理却被绕过。

独立 App 验证：父会话运行时安排一份真实报告，同时为下一轮设置非空 Handoff；父轮结束后检查报告轮的实际模型请求与持久状态，结果：

    handoff present in model=false
    persisted pending handoff=""

模型没有读到交接说明，设置却已将其清空。该实测聚焦 Handoff；同一分支也丢弃已经放入 content 的环境/记忆/经历，不能只检查 Report 数量。

修复：报告保留逐条结构，同时将准备好的宿主上下文真正注入本轮模型历史，例如独立上下文消息或首项内容；每轮只注入一次。消费标记必须对应真实写入，复用既有上下文预处理与限制。

永久回归：TestQueuedReportsPreservePreparedHostContext。断言实际请求收到交接/变更的环境或记忆、没有重复注入，多份卡片与普通消息仍保留，整批仍只启动一次模型请求。

### 27.2 下一轮只集中收尾两处

修复 H1 的旧对象取消归属和 H2 的报告轮上下文，不增加新产品功能，不重开已通过的归档、多卡片和稳定槽位部分。补相应永久回归与受影响检查，再如实附上平台运行范围。

临时 Go overlay 和 Linux C 单元夹具已清理。本轮未执行 Windows 原生程序、未改产品源码、未迁移用户数据，仅更新评审文档。


## 28. c55229b 最终收尾验收通过（Linux＋Windows，macOS 原生暂缓）

日期：2026-10-29。验收基线 c55229b，包含 c66dfa8 的旧代退订及报告上下文修复。用户要求直接完成验收，并明确 macOS 原生验证暂缓。本轮未修改产品源码，保留本文既有未提交内容。

### 28.1 阻塞关闭证据

| 条目 | 本轮独立结果 | 结论 |
| --- | --- | --- |
| H1：旧退订误取消新对象 | 重新从当前源码提取实际 Windows be_detach/win_drain_commands 函数体，模拟 OS 调用与旧命令晚于新挂接的时序；输出 cancelled_handle=11 old_dead=1 new_dead=0 | 关闭；旧对象取消，新对象保持 |
| H2：报告上下文被丢弃 | 真实 App/Store/队列及模型替身，报告轮请求含交接原文，持久 Handoff 只在该轮消费；输出 handoff present in model=true | 关闭 |
| G2：连续报告丢卡片 | 独立旧反例重跑，父会话忙时连续引入两份报告，正式历史保留两份结构和第二份结论 | 关闭 |
| G3：取消回执撤销归档 | 独立旧反例重跑，运行中 archive 后实际 cancelled 收尾仍 status=archived | 关闭 |
| G1/Windows 观察器实际运行 | 当前构建的 Windows test_watch.exe 在 Windows 本地目录执行，12 checks / 0 failures，退出码 0 | 本轮原生验证通过；不再只依据交叉编译 |

H1 的函数体夹具是在 Linux 上运行的确定性单元测试，不冒充 Windows 原生；它与下文真实 Windows 运行是两条分别记录的证据。临时夹具和 Go overlay 已清理。

### 28.2 本机 Windows 原生执行

本机实际运行系统为 Windows NT 10.0.26300，命令由原生 PowerShell 执行。将当前工作区产物复制到全新的 Windows 本地临时目录，输出写入 Windows 本地日志，等待进程正常退出：

    C:\Users\Lenovo\AppData\Local\Temp\nano-accept-watch-91601854a0ce43deb0e323e7fa79ccf7\test_watch.exe

复制后二进制 SHA-256：

    FB724D6407F99EC944699213D598DB3BA5B1FD2DDDE97414E10A2EB9051FDA3E

实际标准输出及调用者取得的退出码：

    watch-win: 12 checks, 0 failures
    Native test exit code: 0

本次没有使用 WSL 启动后猜测退出状态，也没有沿用此前从共享路径启动、输出与源码不一致的残留进程。当前新进程已经正常退出；未终止其他进程。日志保存在上述本地目录的 stdout.log/stderr.log。

这纠正此前“Windows 只能交叉编译/没有原生环境”的不准确说法。本轮的本地复制、直接运行和日志读取均实际执行成功。macOS 没有设备的情况单独处理。

### 28.3 完整检查

- Go build、vet、完整测试全部通过，app 包约 40.86 秒。
- 新旧报告批处理、交接上下文、归档/进展、旧账本升级、损坏状态保护、静默 Schedule 更新、维度漂移/schema/delta 相关 app 定向 race 通过；autonomy/schedule race 通过。
- 前端 check：0 errors / 0 warnings；语义测试 7/7；生产构建通过。
- Linux C/ASan 全套通过：core 262/0、watch 13/0、runner 4 children/0、exec-stream 273/0、job-io 1573/0、job-manager 87/0；执行器/宿主 0 failures；节点 Job 集成通过。
- Windows 原生观察器测试：12/0，退出码 0。

现有完整测试包含此前各轮新增的永久回归。没有因本轮已通过而继续扩大测试模型，也没有把已修复历史条目重新计为阻塞。

### 28.4 验收范围与收口

当前约定的 Linux＋Windows 收尾范围通过，第 27 节两处阻塞关闭；第 26 节的观察器、报告卡片和最近进展三项可以收尾。产品实现与原生运行/平台发布范围分别记录。

macOS 观察器保留代码及实现方的编译记录，原生运行未验证；按用户明确要求暂缓，不作为本轮阻塞，也不宣称 macOS 原生通过。真实飞书/Telegram 租户、SSH 多机和浏览器点击等既有外部联调范围不自动扩大为本轮实测通过。

本轮仅更新评审记录，保留此前未提交内容；没有修改产品源码或迁移真实用户数据。
