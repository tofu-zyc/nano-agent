# v0.9.0 二轮验收与下一步执行清单

日期：2026-10-27。审查基线：`05e995e`，修复提交：`32c97fc`。

结论：上一轮指出的五个最小复现场景已经通过；扩大到实际协议和排队生命周期后，又确认了四个阻塞问题，暂不建议将本批次标记为整体验收通过。先修复这些问题，再补可靠收件箱与会话 Fork。本文记录验收和指导，没有修改产品源码。

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
