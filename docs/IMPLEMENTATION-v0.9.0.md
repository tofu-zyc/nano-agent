# v0.9.0 实施记录：执行能力与拟人配置

日期：2026-10-27。分支：`dev/v0.9.0`。对应设计：`docs/DESIGN-agent-runtime.md`。

本文记录**已经落到代码里**的部分（含测试）和还没做的部分。设计文档描述目标，本文只写当前实现；
两者不一致时以本文和代码为准。

## 已实现

### P0 完整调用身份与能力协商

- 协议升到 **v11**（`gateway/internal/proto/proto.go`、`kernel/src/main.c`）：
  - `input` 带 `run_id`，`model / turn` 带 `turn_id`，`exec / approve / result / done` 原样带回；
  - `hello` 增加 `features`（`call_identity`、`bounded_parallel`、`sandbox_policy` 和本机限额）；
  - `welcome` 的版本号被内核记住，旧网关少发的消息不会让新内核空等。
- 内核状态机（`kernel/src/core.h`、`core.c`）里的完整身份是 `(sid, run_id, turn_id, call_id)`：
  跨 run、跨轮次的迟到 `turn / approve / tool_done` 一律忽略；`run_id` 为空时按旧网关兼容处理。
- 一轮调用的结构在**执行前**校验：数量上限、空 ID、重复 ID。任何一条不满足，整轮按模型协议
  错误处理（`DONE(error)`），不会执行其中一部分再报错。
- 宿主对线上消息做防御性截断（一条 `turn` 最多认 256 个调用）。

### P1 有界并行、排队与资源锁

- 调用状态：`校验 → 审批 → 排队 → 已派发 → 执行中 → 完成`（`core.c` 的 `call_state`）。
  审批和排队都**不占执行槽**；执行时限从宿主确认 `started` 才开始走，派发到 started 之间
  另有受限的等待（默认 30 秒）。
- 额度：`max_calls_per_turn` 32、`max_parallel_tools_per_run` 4、`max_queued_calls_per_run` 32、
  `queued_timeout_ms`（默认不单独计时）、`max_active_runs` 16。
- 资源冲突由**宿主**按真实工具和参数算出资源键（`main.c:call_resource`）：
  | 工具 | 资源键 | 锁 |
  | --- | --- | --- |
  | Read / Grep / Fetch | 目标文件（或工作目录） | 共享 |
  | Write / Edit | 目标文件 | 独占 |
  | Shell | 工作目录；提权时退到 `machine:files` | 独占 |
  | MCP `服务__工具` | `mcp:服务` | 独占 |
  两个调用共有一个键且至少一方独占就不能同时跑；符号链接能解析就解析成同一份文件的键。
- 本机真实执行槽（`main.c`）：`max_execs` 8、`max_parallel_shells` 2、`max_live_jobs` 16。
  没槽位先排队，排到再启动（先来后到，不插队），排队期间不开始执行计时。
- 活跃 run 到上限时，`input` 在内核里有界排队（32 条），前面的 run 结束再放行。
- 网关侧：`tool_queued` / `exec_started` 事件进 UI 事件总线；`exec_started` 在真正开始执行
  （本机执行器、MCP 调用）时才发，旧内核（协议 <11）不发也不影响。

### P1b 用户插话（设计文档 §3.4）

- 发起人在任务进行中继续发来的普通消息进入**当前 run 的有序收件箱**
  （`gateway/internal/app/interject.go`），不取消正在跑的命令、不打断正在生成的回复。
  命令、审批、撤回、群聊、其他人的消息仍走原来的入口。
- 两个注入边界：
  1. **请求模型前**（`generateModel` 开头）：先把补充写进历史，所以这次请求的快照能看到它；
  2. **生成结束、交给内核前**（`yieldToInterjection`）：这次生成完整结束、用量已经结算；
     若这期间有补充，就不把这批尚未接受的 `tool_calls` 交给内核（也不写进历史、不留孤立调用），
     写进补充后重新规划。已经生成出来的正文照常保存和展示。
- 历史里的角色仍是 `user`，正文带 `[用户插话] … [/用户插话]` 标记和发送者/时间；
  消息带 `kind=user_interjection`、`parent_run_id`、`source_id`、`sender`。
- 有界：每个 run 最多 16 条、正文合计 64 KiB；超出明确回执“没有受理”，不静默丢弃。
  平台消息 ID 去重；连续几条只回执一次。
- 未插入的补充在 run 异常收尾时放回会话队列（下一轮处理）；`/stop`、`/reset` 明确停止时
  不再自动开新一轮。撤回未插入的补充直接从收件箱拿掉；已经注入的按“更正”处理，不截断
  仍有待返回工具结果的历史。
- 后台 Job（尚未实现）不参与这里的等待条件。
- 收件箱现在是**持久**的（见下面「插话的持久接收与恢复」）。

### P4 沙箱 auto / on / off

- `kernel.conf` 新增 `sandbox = auto | on | off`（示例见 `kernel.example.conf`）；
  网关配置新增 `[sandbox] mode`（示例见 `gateway.example.toml`、配置 schema）。
- 执行器（`kernel/src/nexec_main.c`）按请求里的 `sandbox` 决定：
  - `off`：**真的跳过** Landlock / seccomp / no_new_privs，按当前 OS 用户身份执行；
    应用层的路径权限照旧。这时不再因为“可写目录包含执行器自己”挡掉 HOME 之类的工作目录。
  - `on`：这台机器没有可用的 Landlock 就**明确失败**，不静默降级。
  - `auto`：保持原行为。
  响应里多一个 `sandbox_effective`（full / partial / none）。
- 内核：`sandbox = on` 而本机没有 Landlock 时**启动即失败**；`hello` 上报请求模式和实际生效程度；
  每轮 `input` 和每次执行都带最终模式。明确 `off` 与“意外没有沙箱”分开：前者不再退回每次
  审批，后者保留旧行为。
- 测试用“此前会被 provider 拒绝的合法操作”验证 off 真的关掉了（`kernel/tests/test_exec.py`
  的 `t_sandbox_modes`）。

### P6 拟人 agent：每日唤醒与联系次数

- `agents.json` 新增 `life_max_wakes`、`life_max_reaches`、`life_wake_min`、`life_wake_max`。
- `effectiveLifePolicy`（`gateway/internal/app/life.go`）是唯一解析入口：不填跟随档位
  （low 4 / normal 8 / high 16），**0 表示不限**，正整数是上限；间隔独立于次数，
  写反了按小的在前处理，写法不合法回落到档位。
- 保存时校验：负数、非法时长、最短大于最长都拒绝（`validateLifePolicy`）。
- 状态、聊天提示和网页统一显示“今天探索 N 次 / 8 次”或“/ 不限”，不再出现 N/0；
  主动联系同理，彻底关闭仍用 `LifeReach=off`。
- 不限次数不等于零间隔：仍受唤醒间隔、token 预算、作息和并发上限约束。
- **范围**：这次只做了唤醒次数、主动联系次数和唤醒间隔。设计文档 §7.2 / §7.3 的
  **心智数值配置**（confidence / interest / novelty / priority 等的编辑、行为维度选择和
  阈值预设）还没实现。

## 测试

| 范围 | 命令 | 现状 |
| --- | --- | --- |
| C 状态机 | `make -C kernel build/test_core && ./kernel/build/test_core` | 195 checks, 0 failures |
| C 执行器 | `python3 kernel/tests/test_exec.py kernel/build/nano-exec-asan` | 0 failures |
| C 宿主（真实内核+执行器+假网关） | `python3 kernel/tests/test_host.py …` | 0 failures |
| 网关 | `cd gateway && go test ./...` | 全部通过 |

新增的关键用例：

- `kernel/tests/test_core.c`：并行上限与排队、资源锁（同文件写串行 / 相同读共享）、排队超时、
  派发超时、完整身份（跨 run / 跨轮次）、一轮批量校验、排队上限、审批不占槽位、
  `sandbox = off` 与“意外没有沙箱”的区别。
- `kernel/tests/test_host.py`：真实进程下的排队（`tool_queued`）、同目录 Shell 串行、
  完整身份往返、网关不同步 `exec_started`、活跃 run 上限排队。
- `gateway/internal/app/interject_test.go`：插话在工具边界注入、生成中收到插话后重新规划且
  不派发未接受的工具、最终回答后继续、异常收尾回队列、只对发起人、收件箱上限、已注入撤回。
- `gateway/internal/app/life_policy_test.go`：档位默认值、0 表示不限、间隔覆盖、写反纠正、
  保存校验、`lifeBlocked`。

## 2026-10-27 验收修复

第一轮交付后按代码核对发现的五个问题已经修掉（提交「修复验收问题」）：

1. **完整调用身份贯穿执行链**：内核匹配网关调用改成 `(sid, run_id, turn_id, call_id)`；
   网关的 `KernelMsg` 拒绝别的 run 的 `done` 和上一轮的迟到结果。
2. **沙箱模式请求真正生效**：最终模式 = 节点 `on/off` 优先，否则用这一轮的请求；
   远程执行也把模式带给目标机器。
3. **资源锁保护整个目录**：资源键分共享 / 独占两组，文件操作在目录上取共享锁、
   Shell 在目录上取独占锁；宿主再做一次跨 run 仲裁。
4. **代执行不再绕过机器限额**：`run` 请求走同一个 `exec_admit`，同样的槽位、资源锁和排队。
5. **插话后的重新请求回到内核预算**：新增 `model_yield`，由内核按自己的步数预算再请求，
   到顶就 `DONE(budget)`。

## 2026-10-27 二轮验收修复（R1–R4）

对应 `docs/REVIEW-v0.9.0-runtime.md` 的四项阻塞问题，全部修掉并补了回归：

| 编号 | 问题 | 修法 | 回归 |
| --- | --- | --- | --- |
| R1 | C 把 `sandbox` 发成字符串，Go 按对象解析，连接被断开 | exec 顶层改用 `sandbox_mode`；`sandbox` 只保留 hello 的能力对象和 run / 执行器请求里的字符串 | `TestCExecWireCompatibleWithProtoMsg`、`test_host` 断言真实 exec 帧字段名、真实 C 内核 + 真实 Go 网关的 `TestRealKernelRoundTripWithGatewayTool` |
| R2 | 宿主已排队的调用被 core 重复派发（一次 Shell 执行了四次） | `dispatch` 严格跳过 `sent=true` 的队列项；宿主再按完整调用身份做一次防重复准入 | `test_host`「宿主排队的调用只执行一次」（追加计数必须正好 1 个 X） |
| R3 | 取消排队中的代执行后它仍会跑 | `run_cancel` 先 `execq_drop`，没有再 `runner_kill` | `test_host`「取消排队中的代执行后不会再启动」（释放槽位后继续观察） |
| R4 | 目标节点 `sandbox=on` 被代执行请求的 `off` 绕过 | `spawn_exec` 统一走 `effective_sandbox`，代执行同样受本机部署要求约束 | `test_host`「目标节点 sandbox=on 时，代执行请求 off 不生效」 |

同一批还对齐了 §3 里能确认的几处：

- 提权 Shell 的 `machine:files` 现在和所有文件操作（读写在 `machine:files` 上取共享锁）冲突；
- 旧节点缺 `user_interjection` 时不再在网关里自己重开模型请求，改为把补充放回会话队列、
  这一轮按正常结果收尾；
- 插话的三个状态做了对账：已受理（收件箱）→ 已注入（写进历史）→ 已被模型请求读到，
  结束时明确告诉用户哪些补充这一轮没被处理；
- 插话写入历史的 `TS` 改回秒，和其他消息一致。

## 插话的持久接收与恢复（二轮验收 B）

每个会话一份只追加的 `data/interjections/<编码后的 sid>.jsonl`，单写者（App 锁下），
每条记录 `fsync` 之后才回执“已收到”。

**状态不是三态**：

| 记录 | 能证明什么 | 恢复时 |
| --- | --- | --- |
| `received` | 已可靠受理，还没进正式历史 | 有界地放回待处理（16 条 / 64 KiB），先按历史 ID 查重 |
| `injected` | 已写进正式历史（带稳定 `history_message_id` 与 `source_ids`） | 不再追加正文；没请求过的标 `interrupted` |
| `included` | 已被某个模型请求快照包含（`request_id` + revision） | **不代表请求成功**：没有结局就标 `interrupted`，保留继续处理的入口 |
| `outcome` | 有明确结局（ok / budget / timeout / cancelled / error / requeued） | 按实际结局收尾，不再重复处理 |
| `withdrawn` / `discard` | 用户撤回、停止、重置、删除导致失效 | 不恢复，重启后不会复活 |

配套：

- 去重键 `sid|渠道|聊天窗口|消息 ID`，渠道重发和重启后重收都只受理一次；
- 会话有 `session_generation`，重置时加一，旧世代的记录一律作废；
- 历史写入改成**先落盘并 fsync、成功才进内存**（原来的顺序会让写失败看起来成功、重试写两遍，
  而且写盘后调 `History` 会把刚写的再读回来一次）；
- 恢复只读日志尾部 + 折叠，日志超过 512 条重写成小检查点；末尾半条视为未提交的尾部忽略，
  中间损坏报告并保留现场；`/reset`、删除、`/stop`、撤回都写持久终态；
- 恢复出来的补充在下一轮开始时装进收件箱并告诉用户，仍然只在“工具整批结束 / 生成完整结束”
  的边界插入。

回归（`internal/app/interject_journal_test.go`）：受理回执要求落盘成功、重启后恢复且不重复、
历史写了但 injected 没写时不重复追加正文、已进入请求但没结局标 interrupted、撤回后不复活、
重置世代作废、渠道重发只受理一次；`internal/session/store_test.go` 覆盖写盘失败时内存不被污染。

### 三轮验收修复（B1–B6）

| 编号 | 问题 | 修法 |
| --- | --- | --- |
| B1 | 恢复丢失稳定 ID，处理完还会再出现 | 恢复时带上 `id`（贯穿受理/注入/请求/结局），平台消息 ID 单独存 `msg_id`；恢复同时恢复字节数；重启后接收序号取日志最大值继续递增 |
| B2 | 尾部残片没截、损坏日志仍受理 | `readJournal` 返回最后完整提交的偏移，打开时截掉未提交尾部；中间损坏 / 版本过新 / 读不出来时把日志标成 `broken`，追加一律失败，受理方明确回"没有保存下来"并保留现场 |
| B3 | requeued 只进内存却标成完成 | 重新排队回到**可恢复的待处理**（日志保持未完成），并立刻排一轮在下一个边界插入；崩溃只会恢复、不会重复 |
| B4 | 检查点丢身份和去重 | 检查点按完整状态往返（稳定 ID、去重键、身份、世代、请求与 revision），每次追加都检查错误，替换失败保留旧句柄继续可用 |
| B5 | 普通 / 网页撤回没持久化 | `undoLatest` 先可靠写 `withdrawn` 再移除，覆盖收件箱和恢复的待处理；写失败时不回执成功 |
| B6 | interrupted 被当完成回收、没有继续入口 | `interrupted` 不算结束（`settled=false`），跨检查点与重启保留；恢复后在会话里可见（`SessionInfo.interrupted`）、开始下一轮时提示，用户发「继续」就按既有历史接着做，正常结束后才结算 |

回归对应第 7 节给的场景名：`TestRecoveredInterjectionProcessedThenRestartDoesNotReturn`、
`TestTruncatedJournalThenAppendThenRestart`、`TestCorruptJournalRefusesNewReceipt`、
`TestRequeuedInterjectionSurvivesCrashBeforeNextRun`、`TestCheckpointPreservesAgentAndDedupAcrossRestart`、
`TestUndoInterjectionDoesNotReturnAfterRestart`、`TestInterruptedInterjectionRemainsVisibleAfterCheckpointAndRestart`。

### 四轮验收修复（B7–B10）

| 编号 | 问题 | 修法 |
| --- | --- | --- |
| B7 | 检查点破坏去重与序号分配 | 检查点额外保存**接收序号高水位**（`head.seq`）和**有界的去重窗口**（最近 256 个 `dedup` 记录）；替换完成后内存索引与高水位与新文件一致。重启后接收序号接着高水位往上走，无平台 ID 的消息不会分到旧 ID |
| B8 | 部分写入失败后同进程重试拼坏日志 | 记录每条成功记录之后的文件偏移；Write / Sync 失败时先把文件截回上一个完整提交位置并确认成功，再返回错误（调用方不回执）；截不回去就冻结该日志并保留现场 |
| B9 | 历史对账只看最近 400 条；补记 injected 后首次恢复不可见 | `FindMessage` / `FindMessageBySource` 支持 `limit<=0` 查全部（历史本来就在内存里），恢复与 drain 查重都改成查全部；补记 injected 的同一次恢复里就判成 interrupted 并计入可见状态，不再要求重启第二次 |
| B10 | 停止 / 重置的失效转换 | 新增 `discardPending`：停止、重置统一处理待处理、中断未完成与字节计数；**没有活跃 run 的 /stop 也清**；终态写失败时保留可对账状态并明确报告（先停进程，再报告丢弃结果）；重置当场清内存里的 interrupted / pendingBytes；平台撤回同样检查写错误 |

回归：`TestAutomaticCheckpointPreservesDedupWithoutRestart`、`TestCheckpointPreservesReceiveHighWaterAfterAllSettled`、
`TestPartialJournalWriteThenRetryWithoutRestart`、`TestHistoryReconciliationBeyondRecentWindow`、
`TestRecoveredHistoryWithoutInjectedIsImmediatelyContinuable`、`TestStopRestoredPendingWithoutRun`、
`TestStopReportsJournalFailureAndDoesNotClaimDurableDiscard`、`TestResetClearsInterruptedAndPendingBytesImmediately`。

### 五轮验收修复（第 9 节）

| 路径 | 问题 | 修法 |
| --- | --- | --- |
| 运行中的 /stop | 清空 inbox 却没写失效记录 | `discardPending` 接收收件箱快照，连同待处理/中断一起作废；先停进程，再报告结果，写失败时把条目放回并如实说明 |
| 运行中的 /reset | 没有切世代 | 活动/空闲两条路径汇入同一个 `commitReset`：换世代、作废收件箱与待处理、清内存状态，历史清理由 `afterRun` 或当次完成；世代只换一次 |
| 压缩后的历史对账 | `limit=0` 只查 Base 之后的有效窗口 | `FindMessage` / `FindMessageBySource` 在 `Base>0` 时追加查正式文件（含已压缩前缀），模型上下文仍遵守 Base / Summary |
| 平台撤回 | 先删内存再检查写盘 | 改成与撤回命令同一顺序：先落盘失效，成功后移除内存项与字节计数，失败保留可重试 |

回归：`TestActiveStopDiscardsInterjectionAcrossRestart`、`TestActiveResetAdvancesGenerationAndDoesNotRestoreInterjections`、
`TestCompactedHistoryInterjectionIsNotReplayedAfterRestart`、`TestPlatformRecallWriteFailurePreservesInboxAndCanRetry`。
停止 / 重置现在各覆盖有 run、无 run 两条分支；历史对账覆盖未压缩长历史与 Base 已推进两种情况。

### 六轮验收修复（第 10 节）

| 问题 | 修法 |
| --- | --- |
| 停止写失败后重试把收件箱翻倍 | 失败分支不再回填（收件箱本来就没被清），只如实报告；重试幂等 |
| 重置归档失败仍报"上下文已清空" | `afterRun` 与空闲分支都把 `resetHistory` 的真实错误报告出来，不再吞掉；失败时上下文确实还在，说法与状态一致 |
| 历史回查整份加载 | `findInArchive` 改流式扫描（`bufio.Scanner`，只保留最后一条匹配），峰值内存不再随文件大小增长 |

回归：`TestStopFailureDoesNotDuplicateInbox`（连续三次失败条数不变）、
`TestResetFailureReportsError`（归档失败必须报错且历史仍在）、
`TestArchiveLookupDoesNotLoadWholeHistory`（16 MiB 前缀下回查的活跃堆增量 < 4 MiB）。

## 还没做（按设计文档的阶段）

| 阶段 | 内容 | 说明 |
| --- | --- | --- |
| P2 | C JobManager、流式 Shell、原地 detach、`Job` 工具与 `/jobs /bg /job` | C 节点持久执行/原地移交已实现并有真实进程验证；Gateway Job 索引、用户命令、渠道及完整对账继续实施，尚未协商启用 |
| P3 | 飞书 / Telegram 任务卡片、断线对账、SSH 能力降级 | 依赖 P2 |
| P5 | 可靠事件、后置 Hook、notify / command / agent、临时会话 | 未开始 |
| P7 | 前置 Hook、C 文件观察、多平台 I/O 与性能收敛 | 未开始 |
| — | 会话 Fork（设计文档 §8，2026-10-27 新增） | 未开始（下一步 C，优先于 P2/P5） |
| — | 插话收件箱的持久恢复 | 已完成（见上，含三轮验收修复 B1–B6） |
| — | 心智数值配置、行为维度与阈值预设（设计文档 §7.2 / §7.3） | 未开始 |
| P6b | 内心活动与外界会话共用一份经历：经历记录、按片段运行的内心活动、聊天不再叫醒内心、心智状态清理（设计文档 §14，2026-10-28 新增） | 未开始；F2 以它为前提 |

设计文档 §3.1 里还有几项**已知简化**：

- 资源键按“解析后的路径”比较，重叠挂载、大小写不敏感卷、同目录别名还没有统一到同一个资源域；
- 公平性按 run 内先后顺序 + 宿主先来后到，跨 run 的等待时间加权还没有实现；
- `Agent` 级和 workspace 级的 sandbox 覆盖还没做，目前只有全局配置 + 节点本机要求；
- 插话的 `input_pending / input_ready / input_commit` 三次握手没有启用：现在的实现在网关侧
  的两个边界完成同样的事，效果一致（详见 `interject.go` 的注释）。C 侧只保留
  `model_yield` 等消息常量，等 P2 一起接线。

## 2026-10-28 全范围续作记录（进行中）

本次授权覆盖整个 v0.9.0，不再沿用旧评审的阶段停工限制。已重新通读设计（含第 14 节与 12.6）、实施记录及评审，核对真实代码；现有文档修改保留。用户要求不使用子代理，后续由单个主代理实施。

依赖与工作清单：

- [x] W1 第 23 节 AB1–AB3：旧来源持久撤权、未提交副本的统一定位门禁、提交归属与完整清单核对、恢复结算错误传播。
- [x] S6：归档列举、读取、计数、删除、清理使用同一已发布视图；旧归档特殊名称使用独立反解规则，冲突保留双方，备份不作活动数据。
- [x] P6b 核心：经历派生索引、真实结果写入、聊天不触发心智事件/自动认识/十分钟唤醒、schema 2 与版本备份、独立片段、按权限和变化注入 `[经历]`、主动消息来源、经历接口、网页分段和经历卡、技能及 prefixVer。
- [ ] P6b 补充整合：Fork/自主分支经历（已做：分支轮次记 `kind=branch` 的 admin 经历）、全部平台显示及网页浏览器回归、完整生命周期与故障验证。
- [x] W1 辅助入口、ADS 枚举/保留备份/可恢复迁移及 W2–W5 实现；原生 Windows 专项通过，W5 原问题未复现，只交付诊断。
- [x] F1 六组：权限/锁、虚拟历史、完整快照版本、恢复与发布、真实界面缓存及 100 闲置分支内存验证。
- [ ] P0/P1/P1b 补全：公平性、资源域、调用身份展示与能力协商、插话协议边界。
- [ ] P2/P3：C JobManager、流式执行、原地后台化、日志/重连/取消/槽锁、命令/网页/平台按钮。
- [x] P4：agent/workspace 沙箱覆盖与所有目标执行链，含实际 requested/mode/effective 回执及原生 Windows 验证。
- [ ] P5/P7：可靠事件、完整 Hook、四种目标、前置屏障、deadline、文件观察（已做：Linux inotify 的明确文件与非递归目录；macOS/Windows 观察器未做）及资源回收。
- [ ] P6：统一活动账本、数值配置（已做：兴趣/新颖/成本/优先级/进度/置信度的编辑与生效）、预设/权重/维度/漂移/delta/锁定/阈值（未做）。
- [x] F2：按话题复用持久自主分支、共享预算、统一 silent、报告入口（`/branch report`）及主聊天零推送；网页的分支列表/界面仍未做。
- [ ] 最终：Go vet/全套测试/race、C/ASan、前端、真实协议、并行输出/Job/Hook/100分支内存实测、协议/API/示例/使用文档与提交清单。

已完成提交：`3c0c792`（AB1–AB3）、`26423c0`（S6）、`6da5b93`（旧归档名称）。P6b 的实现由后续独立提交记录。

本次已运行：`go test ./... -count=1` 与 `go vet ./...` 通过；session/autonomy/experience race 通过，P6b/Life/Autonomy 专项 race 通过；网页 `npm run check` 和 `npm run build` 通过。事故回归使用主动重复未闭合请求的确定性模型，实际经 Knowledge 写两条记忆、reset、推进时钟、手动唤醒、第二次片段；验证没有十分钟聊天唤醒、不重复写记忆、新请求仅系统前缀和当前片段、原始片段均保留。

恢复说明：提升失效资格保存在 `v1/promotions/<key>.revoked`，未提交/冲突计划阻止普通读写发布副本，显式新上下文先隔离旧副本。经历保存在 `experience/<agent-key>.jsonl`，尾部半条截回完整边界，中间损坏冻结追加并显示错误；最多 500 条/30 天，检查点保留序号高水位。autonomy schema 1 首次升级前保留 `.schema1.bak`，未来版本拒绝覆盖。经历是派生索引，写入失败不撤销真实聊天结果，不宣称与历史跨文件事务。

尚未实测真实飞书/Telegram、远程 SSH、Windows/ADS、断电持久性及整版性能；上述专项通过不扩大为这些范围或 v0.9.0 全部交付。

### 本次 Windows 后续实测

P6b 核心提交为 `4884dea`。随后完成 W2 的 `plat_getenv_utf8`（不存在/空值/读取失败区分，Windows 宽字符动态读取）、W3 的 Windows `time/tzdata`、W4 的 `.exe` 与真实文件身份识别及安装/自动应用分离、W5 的目录失败诊断。W5 返回阶段、实际 UTF-8 路径、Win32 错误码、当前/父进程完整性 RID、父进程与执行器路径和执行器 SHA-256；未设置完整性标签、未修改权限模型。

本轮确实在原生 Windows 跑过执行器整套（0 failures），包含中文/空格/非 BMP NANO_HOME 的 Read/Write/Shell 与 tmp/cache，以及目录故障的非零原生错误、可取得的完整性 RID 和 SHA-256 与实际二进制比对。独立 Windows 网关最终 exe 在不存在的 GOROOT/ZONEINFO 下通过 Asia/Shanghai、America/New_York 配置检查，非法时区被拒绝；原生测试检查夏令时偏移差异与安装布局/文件别名。真实 Linux 网关 + Windows C 节点跨机 E2E 17 项通过，技能缓存实际写在中文/空格/非 BMP 根目录。夹具先前把工具进度当作整轮结束而偶发插话，已改成等终态用量行后再发下一条顺序请求，审批控制仍立即发送。

最新 C core 205 checks / 0 failures；受改动影响的执行器与宿主 ASan 串行测试均 0 failures；Go vet 与更新专项通过，前端检查/构建通过。CI 增加 Windows 最终 exe 离线时区和安装识别回归。这里的 Windows 实测不包含旧 ADS 迁移、完整安装升级包测试或此前 Low Integrity 报告的原机器归因。

### Fork 底座续作

`bbe35fe` 修正命令重复加锁、来源/目标授权和主体+来源绑定的幂等范围；模型工具只能创建 silent 分支。`a61c6f3` 改为固定文件描述符与字节边界的虚拟历史读租约（继承快照+独立尾部），最多 32 个待复制、2 个实际复制者，哈希/复制/校验不占 App 总锁。历史撤回改为原子替换，旧描述符继续指向原边界；嵌套、压缩、撤回、重置共享虚拟坐标。指纹含完整适用设置、persona、Summary/Base/Omitted、来源与真实消息字节；Ready 从冻结值构造设置，先提交设置/操作后发布元数据，目标世代/revision 变化会撤销发布授权。

ForkOp 先可靠预留 branch_sid，旧版缺失关联按 operation_id 对账；pending 恢复若无可确认边界则显式 needs_attention，不拿最新上下文替代，不重放旧工具；已保存 snapshot_ready 可续办发布。模型整批工具闭合、插话落盘后即能固定边界；取消/异常不会 ready。分支固定 owner_agent，查看/列表/操作统一访问范围。真实 C 内核 + Go 网关测试同时执行 Session.fork 和兄弟 Skill，快照内两条调用与两个结果严格配对。

网页概览只读轻量快照统计/尾部，不加载闲置分支 History/context；用户打开单个分支按需计算上下文再卸载，维护/完成时释放闲置历史和设置缓存；共享快照缓存核算工具参数、附件和结构开销。100 个闲置分支（同一约 4.2 MB 来源、刷新真实 App.Snapshot、逐个打开 History）无 race 实测：累计 Go 分配 75,419,624 字节、GC 后活跃堆增量 195,152 字节、共享缓存账面 4,550,308 字节；该测试进程 RSS 51,280 kB、PSS 49,965 kB。进程指标包含 Go 测试宿主，不等于新增常驻内存；race 进程的较高 RSS 单独记录，不与无 race 数据混算。

上述源代码和永久回归已提交；Fork/快照/恢复专项 race、真实 C 并行边界、完整 Go 测试与 vet 通过。F2 统一 silent/自主预算/话题复用/完整界面仍继续实施；后台 Job、Hook、心智完整配置、W1 辅助入口与 ADS 尚未交付，不把本节当作 v0.9.0 全部完成。

- W1 辅助存储与 ADS：人格/持久插话改用规范键及共同世代/版本授权；辅助文件参与可恢复重命名，pending 副本不能由普通写入发布，reset 可撤权。启动盘点恢复、可见迁移备份、ADS 枚举、未确认候选备份、Windows 大小写归属和既有绑定识别已实现。新增网页恢复错误、执行前存储检查和迁移说明 docs/STORAGE-v0.9.0.md。相对根引用通过整个安装目录搬迁回归。全量 Go 测试/vet、定向 race、网页 check/build 通过；原生 Windows 运行 ADS/辅助文件/重命名恢复/reset 撤销/身份归属测试通过。真实用户数据目录未迁移，不能据此声称用户迁移已完成。

- P4 隔离覆盖链补齐：全局→agent→workspace；运行开始冻结目标策略，模型参数和协调内核帧不能覆盖；目标节点 on 强制施加，节点 off 与请求 on 冲突明确失败。旧目标执行器缺少 sandbox_modes 时拒绝显式 on/off，旧协调内核缺少所需能力时在模型启动前失败。sandbox_policy 升至 2。逐次结果回传 requested/mode/effective/reason，写历史、网页调用卡片，和能力探测分开。配置/schema、agent 设置、工作目录添加/修改、GatewayAdmin、/sandbox、会话/节点面板与说明已同步；prefixVer=8。off 允许包含自身的工作目录，应用层路径权限照旧；补修 Windows 绝对 Unicode 路径和含自身判断。
  - 验证：全量 Go test/vet；针对隔离/Host/配置/100 分支查询的 race；C core 205/0、Linux 执行器与宿主 ASan 全套 0 failures（含新 workspace 覆盖、状态往返、节点 on/off 部署规则）；Windows C 交叉构建通过，真实 Windows auto/off=none、on 失败无副作用通过。Windows 原执行器全套的已有用例全部通过，新用例首次暴露测试 helper 缺参数，修复 helper 后单独重跑该新用例通过。原生 Windows Go Unicode 工作目录与大小写/分隔符包含判断通过。网页 check/build 通过。没有将 Windows 编译等同于真实运行验证。

- P0 平台关联与提案边界：新增可选 ToolEvent，CLI/飞书/Telegram/过滤层及网页按 run+turn+call 身份更新，乱序完成和跨轮复用 ID 正确；实时排队、开始、审批状态接通。工具历史、附件、SSE 保留身份，迟到模型正文不会写进后续网页 run。平台缓冲限额：正文/思考各 128 KiB、notes 32、tools 128，保留未完成项。修复异步取图在下一轮 model 已抵达时误用新 turn_id。未接受工具提案改为独立 proposed_calls 查看，正式模型历史没有孤立 tool_calls；空/重复/过长 ID、超过 32 调用整批未接受、无部分执行，C 超硬边界不再截取。prefixVer=9，仅对声明 bounded_parallel 的内核注入并行说明。
  - 新增 Go 模型替身验证提案整批拒绝、用量结算和历史重建；渠道验证乱序、重用 ID、重复终态、4000 次调用/输出下有界状态及渲染；网页新增无额外依赖的 Node 内建语义测试并接 CI；C 宿主 ASan 验证超大协议提案不执行任何部分。定向 race、网页 test/check/build 通过。P0 的机器级 Gateway 租约、公平优先级和完整资源域仍需继续；未将本批声明为整个 P0 已完成。


### 统一执行租约与 run 接纳续作

新增有界 dispatch 调度器和单个持久化工作者；按会话轮转、用户优先与等待加权，统一每 run、
同机多节点、Shell、资源域和后台容量。文件域处理重叠目录、点段、符号链接和 Windows 路径比较；
off/提权/作用域不明的 Shell 取机器文件域独占。Hook 保留分区已在调度器验证，Hook 处理器仍待交付。

授权执行前先同步保存租约；磁盘工作不占 App 总锁。C 节点启动身份由系统随机数生成，握手协商、
完整清单核对之后才放行新执行；断线/恢复保留 unknown，不自动重放。校验原 run/turn/call、固定 agent
与 host RPC 归属；代执行编号包含网关启动身份，防止重启后复用。网关直接执行、SSH、MCP、网络及
网关状态工具也进入同一 per-run 仲裁；不能核实路径别名的直接/SSH路径采用机器域保守串行。
MCP 默认服务串行，结束 RPC 不宣称外部副作用回滚。SSH 执行连接丢失保留未知占用。

排队不占 C run 运行槽，实际进程启动后才计执行时限。修复已有的宿主排队后启动失败不能被 core
接受、导致整批卡住的问题；排队失败直接闭合，既不伪造 started，也不跳过同批其他调用。
网关的活动 run 按节点硬上限接纳，普通消息每会话 32 条、总计 256 条/8 MiB；等待消息保留，
完成后按会话轮转。runtime 配置、示例、网页设置/API/类型与机器资源占用卡片接通，改变限额需重启。
补齐概览对 storage_issues 的传递；新增 App.Wait，取消后等待持久工作者结束再退出/重新执行。

验证：完整 Go test/vet 通过；调度器持久恢复/授权失败/公平性/锁与取消、网关接纳/排队/执行时限、
MCP 及真实 C 往返的 race 通过；C core 215/0，Linux 宿主 ASan 全套 0 failures；
网页语义测试/check/build 通过；Windows 内核和执行器构建后，真实 Linux 网关 + 原生 Windows
节点 E2E 17 项通过（包含文件、Shell、CRLF、Unicode 自定义根和技能缓存）。

这批是共享调度/持久租约层，不代表后台 Job、Hook、完整心智配置或 F2 已完成。仍需继续实现
C JobManager、流式输出/原地移交与所有渠道操作，再接可靠 Hook、统一活动账本和 silent 分支。


### P2 流式执行底座续作（JobManager 与渠道仍在实施）

执行器 v2 增加 job_stream=1，显式 stream=1 时真实启动后给 started，stdout/stderr 分别增量输出，
8 KiB 原始块/base64、16 KiB 帧上限，保留唯一 exit 与取消/超时/退出码/实际隔离回执。
终态只含最多 2 KiB 原始预览；完整二进制输出经过流式帧。默认 30 分钟，0 仅在可信请求显式允许
不限时时有效；旧模式继续一问一答及 120/600 秒规则。

新增 C 增量解码器，验证序号、完整 job_id、base64、状态次序和唯一终态；中断或半条不会伪装成功。
runner 改为每个调用独立冻结输出上限。原生测试还发现 Windows runner 原来只传 argv[0]，已复用平台的
完整 Windows 参数引用；读取线程在释放 Job/参数前停止并等待退出，避免清理时引用已释放内存。

已接入现有 Linux/Windows 测试和 CI：真实执行器在命令仍阻塞于门控文件时交付输出；验证任意
0–255 字节共 256 KiB 无损、命令伪造 JSON 仍是数据、终态有界、超时/取消与允许的不限时；
真实 wire 帧再交给 C 解码器核对。C decoder 273 checks/0；实际三个进程分别保留自己的输出上限，
Linux ASan 与原生 Windows runner 均 3 children/0。执行器的 Linux ASan/原生 Windows 整套通过；
宿主 ASan 整套通过。最终更新后的完整 C 检查（含 Windows 执行器、Linux 执行器/宿主 ASan、core、runner、decoder）全部通过。

尚未宣称后台化或 Job 工具完成；接下来把流解码接入节点 JobManager 的有界日志、可靠所有权变更和
资源租约，再接命令、网页和平台操作。缺少 JobManager 的节点继续使用原来的前台 Shell。


### P2 C 宿主与持久生命周期续作（仍未完成整条用户路径）

C runner 已接入逐帧解码；Linux poll 和 Windows stdout 读线程都不再累计完整流式输出，
只缓存终态帧。真实 C 宿主测试执行 1 MiB 输出命令后，按唯一 run_result 取得有界预览；
实际四个子进程的独立限额/增量结果在 Linux ASan 与原生 Windows 都通过。

新增单个有界 C Job I/O 工作者：队列 128、日志只用前 112 个位置、状态操作保留容量；
每个任务最多四个 512 KiB 日志段，二进制游标读取最多 64 KiB/64 块，写入缺口显式标记。
元数据保存先刷日志再持久发布；Linux 同步文件及目录，Windows 使用已 flush 的原子替换。
重启后 queued/preparing 变 interrupted，其余非终态变 unknown，不重放保存的命令；
未来版本或损坏身份冻结恢复并保留原文件。恢复及记录数量最多 256，I/O 在节点 poll 外。

C JobManager 的创建、启动授权、真实启动观察、移交保存、终态保存和读取已接持久工作者。
保存失败不能发布后台所有权；移交保存与结束竞争由同一生命周期队列闭合。C core 增加
独立 detach 事件，只闭合原等待关系，不取消真实进程；保留批次屏障和原模型步数预算。
重置不再给已完成调用发 KILL；宿主排队超时则会取消仍受理的请求，防止失败后又启动。

已验证：Job I/O 在 Linux ASan 与原生 Windows 均 1573 checks/0（含 4 MiB 无损轮转段、
饱和队列与缺口、游标读取、写入失败、重建与未来版本保留）；Job 生命周期 49 checks/0，
覆盖先完成/先移交和持久失败；C core 最新 232 checks/0。
仍需把生命周期接到真实节点的创建/取消/移交、网关租约后台化与对账、Job 工具/命令和各渠道，
并完成完成事件与 Hook。现有 Gateway 尚未协商开启后台 Job，不把这些底座测试当作用户路径通过。


- 后台容量移交补为两步：prepare 持久预留容量但不释放前台预算，节点保存所有权后才 commit；
  失败 abort 保持原前台关系。lease_detach 能力、完整执行者/请求身份、成功/失败响应已接 Gateway；
  调度器保存/恢复和真实 Hub 往返 race 通过。尚待 C 节点发送此流程及 Job 工具/界面整体接通。
- 最新底座验证：完整 C 测试及全量 Go test/vet 通过；修正旧 MCP 停止夹具的同步，工具取消回执后等待实际 run 结束再开始下一次独立调用，race 重复 15 次通过。覆盖 Windows 原生执行器、Linux ASan、core 232、stream decoder 273、runner 4 个真实子进程、Job I/O 1573、Job 生命周期 49。

### P2 C 节点实际执行链（Gateway 用户入口仍在实施）

真实节点现在先可靠保存每个 Shell，再申请租约和本机槽；starting 保存后才创建流式执行器，
started 从真实子进程观察产生并冻结实际执行期限。background 可在未取得槽时可靠移交 queued，
auto 使用独立 yield 时钟；手动 detach、取消、输出读取和状态查询通过 job_control 接通。
保存后台所有权→提交租约后台化→原调用唯一结果；完成和移交竞争不启动第二条命令，也不返回两条结果。
断开 Gateway 不停止已移交任务；已移交排队项也保留原顺序。自动更新遇到活动 Job 明确延后。
终态及其 ACK 均持久化，未确认回执至少一次补发。重启恢复不重放命令，unknown 保留槽和锁。

实际进程测试覆盖 Linux ASan 和原生 Windows：13 个 host 请求，加 C 模型循环内 Shell 后台化与
Read 的双调用批次；包含排队/自动/手动移交、容量拒绝、退出与提交 ACK 竞争、运行/排队取消、
断线重连同 PID、原 run 结束后 Job 继续、节点重建 unknown/interrupted、ACK 和输出尾部保存。
Linux 再用错误协议夹具验证重复、截断或缺失终态都不能发布成功。测试发现并修复队列在节点退出时
未释放的内存、未取得真实资源就过早保存 starting，以及恢复时用空尾部覆盖已保存日志摘要。
新增永久 CI 路径，Windows 用实际原生内核和执行器，不以交叉编译代替运行。

全量 Go test/vet、相关协议/完成证据 race 通过；完整 C 测试通过，随后改动的协议终态校验又经
Linux ASan 与原生 Windows 节点专项验证。后台/queued/unknown 等回执不能作为任务完成凭据。
这仍不是 P2/P3 全部完成：Gateway 尚未协商 job_manager；需继续任务索引/权限、可靠重连终态证明
与租约归属对账、旧回执回收、Job 工具、命令及全部平台入口，然后完成可靠 Hook、统一活动预算和 F2。

- Job 对账续作：租约保存固定 job_id；跨节点启动只认领完整身份/资源一致的 unknown 或持久终态证明。
  leases 与 lease_receipts 独立有界，unknown/遗漏的 Job 保留槽和锁，旧活动清单不能恢复已结算调用；
  整份清单先验证再发布。节点仍在保存的 detach_request 保留后台容量预留，恢复已发布后台状态可
  完成原移交。覆盖排队/保存中/已发布/取消/终态/重复回执/拒绝部分改写的 race 和真实 Hub 往返。
  最新全量 Go test/vet、Linux ASan 节点以及原生 Windows 节点路径均通过。Gateway Job 索引、
  可靠事件处理和用户入口仍继续实施，尚未对 App 开启 job_manager。

- C Job 记录回收已接实际节点：240 条触发，按最旧且已确认的确定终态回收到 224 条；I/O 工作者
  删除成功才释放身份与日志槽，unknown 和待确认回执不淘汰。阻止回收时新增日志读取、重复删除或
  直接释放未提交记录，删除失败限频退避；避免每次选择复制 256 份输出。Linux ASan/原生 Windows
  生命周期最新各 62 checks/0；两个真实平台均从磁盘重建 240 条任务，确认最旧已 ACK 记录回收、
  最新记录保留、pending 和已 ACK 的 unknown 保留。更新后的节点 13 host 请求和原生双工具批次通过。

- C/Go 实际协议契约：Go 增加完整 Job/结果/二进制游标类型，job_data 与旧管道字符串 data 分离；
  真实 Hub 解码、后台回执、原调用取消后任务继续、状态/日志、唯一完成事件和持久 ACK 的 race 通过。
  协商必须同时具备 job_stream/machine_leases/lease_detach 和处理器能力，生产 App 尚未开启。
- 复现并修复长命令叠加控制字符输出导致终态元数据超限而无法闭合：初始元数据限 28 KiB，
  提前预留终态空间，预览限 2 KiB（完整日志不变）；超限在执行前明确拒绝。Linux ASan 与原生
  Windows 生命周期各 73 checks/0。协议改动后的节点专项、全量 Go test/vet 与相关 race 均通过。

- 可靠事件单写者 journal 已实现并经 Linux race 与原生 Windows 验证：Job+终态原子落盘，
  准入时预留终态位置和字节，规则匹配+固定 revision 投递原子保存；完整校验、半条恢复、
  同文件快照替换、磁盘失败冻结确认、队列排空、silent 继承和因果限制。事件正文回收后保留
  Job 终态身份或来源确认游标，乱序有界，旧正文不能重投激活；Windows CI 加入同套测试。
  详见 docs/RUNTIME-events.md。这是可靠日志模块，尚需继续 App 准入/确认与用户入口接线，
  不以此宣称整个 Job 或 Hook 功能交付。

- Gateway 已接可靠 Job 索引：首次核对真实 C 调用/host wait、固定 creator/silent 和完整资源；
  索引及终态名额保存成功前，lease_acquire 有界等待，失败不取得执行授权；终态与事件一起
  落盘后才 ACK。来源失效或换 ID 的副本不凭节点自报激活，恢复问题进入可见存储诊断。
  creator/silent 进入 C policy、Job origin、持久租约和重连身份核验。
- 新增实际 C 模型循环→安静持久分支→后台任务→原 run 结束→原命令完成→可靠事件的专项：
  native Job 保留 creator/silent，主聊天零推送；Gateway 自动 ACK 与真实磁盘重建通过。
  全量 Go test/vet、App/dispatch/eventlog 相关 race 通过；Linux ASan 与原生 Windows 节点
  13 host 请求、原生双工具批次和 240 条恢复回收继续通过，并验证 native policy 的 silent 来源。
  生产 App 尚未开启 job_manager，继续任务工具、命令和平台入口；完整 Hook、统一活动账本与 F2 仍未全部交付。

- Job 统一用户控制入口已实现：权限范围、只读限制、离线状态、日志游标、最多 10 秒事件驱动等待、
  取消与原地 detach；控制/等待各 128 个、取消后清理，不取消真实执行来结束查询。
  Job 工具按 job_control_tool 协商，C 路由到 Gateway；/jobs、/bg、/job、/stop <job_id>
  直接处理，不进入模型消息队列。列表只复制有界元信息，分页游标不暴露不可见任务。
  prefixVer=10；仅 Job status/wait 的 succeeded 可作完成证据，其余查询及受理回执不算。
- 真实 C 模型循环已跑通 Shell 后台化→Job 查询→原会话结束→Gateway 取消/等待；命令、权限、
  跨 agent 隔离、离线、错误 epoch、取消清理、分页与完成凭据的 race 通过。全量 Go test/vet
  及相关 race 通过；节点专项继续覆盖 Linux ASan 和原生 Windows。
  仍继续网页/API/平台卡片与长调用入口，尚未打开生产 job_manager；完整 Hook、心智配置和 F2 继续实施。

- 网页执行任务面板已接统一 Job 控制入口：顶栏/搜索/任务链接、分页元信息、实际日志游标、
  事件驱动短等、取消和转后台；保留当前聊天，silent 状态变化不发聊天通知、不自动跳转。
  仅保留 200 条任务、200 条近期状态及 128K 日志字符，关闭面板撤销订阅和定时合并。
  SSE 改用 JobInfo 投影，不传命令/资源/结果正文；API 严格限制参数和正文。
- 真实 HTTP→Hub→节点 NDJSON 验证输出/移交/取消、可靠终态 ACK 和 SSE，并从磁盘重建
  初始索引；登录/请求保护、离线、分页与参数回归通过。网页 UTF-8 跨页、终态防倒退测试、
  类型检查和生产构建通过；web 包 race/vet 通过。继续长调用提示、飞书/Telegram 控件、
  SSH 能力降级与其余 Hook/心智/F2；生产 App 尚未打开后台协商。

- 长调用提示与 JobPresenter 已接实际持久运行状态：默认实际运行 10 秒，只提示一次、不计排队，
  不受过程显示档位影响；runtime.long_call_hint_seconds 已进入配置示例/schema。
  飞书卡片/Telegram inline keyboard 与无按钮命令共用同一任务身份；26 字节令牌绑定原卡片、
  渠道对象/agent/Job/来源会话，点击当前授权，重复操作合并、终态撤销无效按钮、重启令牌失效。
  有界发送队列执行慢 I/O，合并卡片更新、有限重试，来源失效和过期回收。
- 真实 C/Go 前台 Shell→模拟时钟提示→实际按钮 detach→原 run 结束→原 PID 完成的 race 通过，
  单次启动标记只有一份；同时覆盖慢卡片不占 App 总锁、原身份/消息/操作者、重复点击、
  terminal/过期/换渠道与 silent 零推送。两平台模拟 API 与回调测试通过；
  全量 Go test/vet、相关 race 通过，真实账号凭据与卡片权限未实测。
- 网页补入浏览器回归：实际日志/转后台/查询控件、跨页中文与表情、HTTP/SSE 终态竞态、
  silent 主聊天/通知零变化、保留聊天与直接链接均通过，截图 build/ui-check/jobs.png。
  仍继续生产 Shell 能力和 SSH 降级、可靠 Hook、完整拟人配置/统一账本及 F2，未宣告全版本交付。

- /stop all 已接停止当前前台及本人在同一来源会话发起的活动 Job；管理员也不扩大本人范围。
  最多 16 个取消组、每组 4 个工作者，重复请求合并，报告受理/未确认而非假称已退出；
  /job logs 别名接原日志游标。异步命令与按钮共用原渠道/agent 投递校验，换渠道不转发旧结果。
  真实控制往返的 scope/重复/缓存回收 race 通过，全量 Go test/vet 通过。
- 复现并修复 Job base64 日志分片绕过现有模型工具脱敏：有界合并页内分片后脱敏再编码，
  保留原始游标跨度，标记模型视图 redacted；原始历史/日志 API 不改写。
  实际模型请求替身验证 always/off 及原历史，覆盖跨分片密钥、长/短私钥与无敏感内容字节保持；
  App/redact 相关 race、全量 Go test/vet 通过。后续完整范围继续，未打开生产后台协商。

- 复现并修复跨机器前台 Job 的计时传递缺口：目标节点给出的实际时限原先在 Gateway/
  协调 C 内核之间丢失，且 lease_started 先到会装上旧 610 秒计时器。
  从同一可靠 Job 和固定 host wait 验证策略，合并两种真实开始信号，一次通知；
  明确 Job 身份后不再用旧 Gateway 工具看门狗，64 位毫秒进入协商后的 C started 事件。
  内核超时说明也使用实际时限；run 总预算、前台取消和旧无标签执行保持原有语义。
- 新宿主回归在旧 C 二进制复现失败，更新后按实际 1 秒时限闭合，完整宿主 ASan 与
  C core（232 checks/0）通过。Go 覆盖不匹配来源/时限、lease/run 信号竞争、
  1800 秒、0 不限及一年时限、旧通知，以及真实 C/Go Job/提示相关 race。
  全量 Go test/vet 通过；原生 Windows 新节点的 13 host、native 双调用、Job 工具、
  重连/重建与 240 条记录回收继续通过。Windows 未单独实测异机 1 秒超时分支。
  Shell 目标能力、配置迁移、SSH 后台及完整 Hook/心智/F2 等剩余范围继续推进。

- 执行节点硬时限已接实际链路：kernel.conf.max_job_timeout 默认 86400 秒，
  0 才允许显式不限时；受管理/流式请求不能通过顶层参数提高部署上限。
  新能力进入 hello 与网页节点显示，旧非流式 120/600 秒规则保留。
  yield_after_ms 非整数/越界/错误类型均在 Job 登记前拒绝，避免悄悄使用另一种等待时间。
- 实际流式命令试图提高上限仍于部署的 3 秒超时，未经允许的不限时在启动前失败；
  完整 Linux 宿主 ASan 通过。JobManager Linux ASan/原生 Windows 均 87 checks/0，
  两平台节点的 13 host、原生双调用、Job 工具、重连/恢复/240 条记录回收通过。
  网页类型检查和生产构建通过。网关默认值/配置迁移/SSH 链路与完整 Hook/心智/F2 继续。

### Hook 可靠规则与派发基础（2026-10-29）

已接入同日志规则修订、固定创建操作身份、删除墓碑、一次性认领与成功停用；新增有界派发、按对象去抖、有限重试和不安全动作恢复为 uncertain。格式 2 首次写规则前流式保留 v1 备份。go test -race ./internal/hooks ./internal/eventlog -count=1 通过。此项是运行基础，实际 App 处理器与管理入口仍在接通，不能视为完整 Hook 交付。

### Hook 实际处理入口与一次性到期（2026-10-29）

已连接 App 的 Hook 工具、HTTP、网页规则面板、斜杠命令；notify/短命令/C agent 四种目标接通。权限上限、工具集、工作目录和窗口凭据指纹固定，当前权限撤销会暂停，临时缓存完成及异常退出均卸载。新增 C 子进程有界事件 stdin、实际执行器能力检查、脚本摘要与只读版本缓存、一次性 deadline 最小堆、持久冷却与速率、完成事件有界保留、运行对话入口，prefixVer=11。相关五个 Go 包全量 race 与 go vet 通过；Linux 执行器 ASan、Windows 原生执行器各 0 failures，包含事件 stdin；实际 Go→C 短命令、临时 C 协议、安静事件无源聊天推送、规则权限与 CAS 测试通过；网页实际 CRUD 与到期派发浏览器回归 0 failures，类型检查 0 errors/0 warnings，构建通过。前置 C 屏障、原生文件观察、message.received 对账、统一拟人活动账本和 F2 仍待后续模块完成，未把本提交视为完整版本交付。

### 前置 Hook 屏障（P7 第一批）

C 内核在调用通过身份与声明校验后进入 `CALL_WAIT_HOOK`，发 `hook_check`，收到 `hook_result` 再决定审批或排队；上限 2 秒，超时或失败都收尾为未执行，判断只能追加审批。网关 `hooks.Engine.Check` 复用既有命令 worker 和有界队列（满则明确拒绝），按 priority、id 执行 `tool.before` 规则，投递标 `preflight` 且不自动重放；内核缺 `hook_check` 能力时该会话直接失败。网页事件面板在事件只有 `tool.before` 时固定 command 并给出 decision 下拉，投递列表显示「前置检查」标签。

验证：`go build/vet/test ./...` 通过；`go test -race ./internal/hooks ./internal/app` 通过；`kernel/build/test_core` 262 checks / 0 failures；Linux ASan 宿主 `test_host.py` 0 failures；网页 `npm run check`/`test`/`build` 通过。

未做：文件观察器、`message.received` 生产者、macOS/Windows 观察器。模型可见的工具说明未变，`prefixVer` 仍为 11。

### P7 文件观察（B1）

新增 `kernel/src/watch.c`（Linux inotify，明确文件或非递归目录，最多 64 条登记、500 ms 去抖）；网关把规则的 `watch` 解析成稳定 key 下发 `watch_sync`，内核按 key 回 `file_event`，网关写成可靠 `file.changed` 事件（复用现有日志与 Hook 匹配）。首次登记不发事件；断线、内核重启和 inotify 队列溢出各给一条 `resync_required`，重启不重放旧变化。Hook 动作执行窗口内的文件变化记成该 Hook 派生的事件，同一个 Hook 不消费自己写出的变化。

验证：`kernel/build/test_watch` 10 checks / 0 failures（去抖、集合替换、重连缺口、17000 次变化触发队列溢出后的 resync、超过上限的登记）；真实进程回归 `TestFileWatchRealKernelDebounceAndResyncAfterRestart`（首次登记不报缺口、窗口内两次写入只一条、杀掉内核重启后一条 resync 且不重放）；`go test ./...`、`go vet`、hooks/app race、test_core 262/0、Linux ASan 宿主全套 0 failures 通过。

未做：macOS（kqueue/FSEvents）和 Windows（ReadDirectoryChangesW）观察器，递归目录，以及网关直接观察远端 SSH 机器。未观察到的路径只按 `resync_required` 告知，不声称完整审计。

### P7 message.received 生产者（B2）

渠道入站消息通过白名单、命令、空消息和分支就绪检查之后，先写一条可靠的 `message.received` 事件（`SourceID=message`，正文最多 512 字），再由 Hook 匹配；Hook 只看得到已提交的事件。去重沿用插话日志：同一个 `sid|渠道|聊天窗口|消息ID` 在同一份日志里占独立的一格（`jrecDedup`），平台重发不再产生第二条事件，进程重启后仍有效。

验证：`TestMessageReceivedEventIsRecordedOncePerPlatformMessage`（第一条记一次、同 ID 重发不重复、新 ID 照常记）；`go test ./...`、`go vet`、app race 通过；插话日志既有回归全部通过。

未做：群聊记录会话里的发言在更早的入口处理，不落这条事件；去重窗口与插话共用有界裁剪，很久以后才重发的消息仍可能再记一条。

### P6b 补充整合：分支也留经历（B3）

`recordRunExperience` 识别分支会话（`isBranchSID`）：Fork 与拟人自主分支的轮次记成 `kind=branch` 的经历，`where` 为「安静分支」，可见范围固定 `admin`，不因为分支里说过话就放宽给访客，也不把分支结论写进源聊天。什么都没说、也没有工具或明确动作的分支轮次不记，避免每次空跑都留一条。

验证：`TestBranchRunLeavesAnAdminOnlyExperience`（分支轮次留一条 admin 经历、非主人视图看不到、空轮次不记）；`go test ./internal/app ./internal/experience ./internal/session`、`go vet` 通过。

未做：网页分支卡片上的经历入口和真实浏览器回归；分支经历的目录/清理仍沿用现有 `experience/<agent>.jsonl` 上限。

### P6 心智数值编辑（B4）

`autonomy.State.SetNumber` 是唯一的范围校验点：想法 `interest`/`novelty` 1–5、`cost_minutes` 1–240，目标 `priority` 1–5、`progress` 0–100，认识 `confidence` 0–1；非整数、越界和找不到的对象都明确报错。网页 agent 节点给想法、目标、认识都加了数字输入，走既有 `POST /api/agents/<id>/autonomy` 的 `set_number`；聊天入口是 `/autonomy set <编号或认识主题> <字段>=<数值>`，仅管理员可用，`/autonomy` 列表同时显示新颖、优先级和置信度。

生效：想法仍按兴趣+新颖排序、目标按优先级排序（都是现有挑选逻辑），改完下一次挑选立即使用新值；内心简报里的认识带上置信度，低于 0.5 标「还没确认」。验证：`TestSetNumberValidatesAndChangesThePick`（越界/小数/不存在被拒绝，想法与目标的挑选顺序真的翻转）与 `TestAutonomySetNumberFromWebAndCommand`（网页、命令、访客被拒）；`go test ./internal/autonomy ./internal/app`、`go vet`、网页 check/test/build 通过。

未做：可选行为维度（curiosity/social_drive/focus/energy）与阈值预设（设计 §7.3）整块未做；数值目前存在 agent 的心智状态文件里，没有 `agents.json` 级别的默认值/权重配置；`confidence=0` 仍与「没标过」共用 0 值。

### F2 按话题复用持久自主分支

心智状态新增 `topic_branches`（话题 `in:<意图>` / `ag:<日程>` → 分支 sid）。自主活动里用 Session 工具分叉或 continue 时，把分支记到当前话题上；同一个话题下一次再想分叉时直接复用已有分支（不新建、不另起上下文），内心说明里也会写明「这个话题已经有一个持久分支 X，要接着做就在那里 continue」。分支仍只接受 silent 投递（F1 的统一策略），自主分支这一轮的 token 现在经 `branchDone` 计入 agent 的自主活动预算，换个 sid 不能绕开每日预算。

验证：`TestTopicBranchIsReusedForTheSameTopic`（同一话题第二次分叉返回已有分支、分支数仍是 1、下一次内心说明提到它）与 `TestBranchRunTokensCountIntoTheActivityBudget`（分支 token 计入 `Budget.Tokens`）；`go test ./...`、`go vet`、app/autonomy race 通过。

未做：自主活动仍是「醒来先决定、再进分支」，没有把整段内心活动搬进分支里跑；每 agent 的自主分支并发/深度上限、分支报告入口和「一次活动只计一次唤醒」的账本字段仍未接；分支经历（B3）已能看到结论，但不含分支内的过程。

### F2 补充：自主分支限额与结论带回

自主分支默认每 agent 同时 1 个在跑、2 个排队，自动分叉最多 3 层；只约束 agent 自己发起的 Fork/继续（`life:`/`hook:`/`dream:` 主体），主人手动分叉和继续不受这些数字限制，超限时明确报错而不是静默排队。`/branch` 列出父会话的分支，`/branch report <编号>` 把分支结论带回父会话：报告带来源会话、分叉点、快照和执行依据，作为一条带 `[分支报告]` 标记的消息进入父会话的下一安全边界；分支历史不合并、窗口不切换，同一个结论只带回一次（分支之后又有新结论才允许再带）。

验证：`TestAutonomousBranchLimitsAndDepth`（三层后 agent 再分叉被拒、已有自主分支在跑时 agent 再开被拒、主人手动继续照常）与 `TestBranchReportComesBackAsASourcedMessageOnly`（报告带来源/分叉点/结论、父会话只多一条、继承历史没被合并、同结论不重复）；全量 `go test`/`go vet` 与 app race 通过。

未做：网页没有分支界面，所以报告入口只有 `/branch`；报告不会自动生成或自动推送，也没有 HTTP 接口。

### 观察事件序号与机器测试偶发失败（修正）

`file.changed` 的事件序号改成按内核启动身份（`SourceEpoch`）各自记：同一来源 `watch` 下多台机器交错上报时，原本共用一个全局计数器会让每台机器的序号被别的机器挤出空档，超过可靠日志 4096 的缺口窗口就会拒收这些观察事件。现在每台机器（每次内核启动）的序号连续，节点下线时释放对应计数。

全量 app 测试偶发的 `TestMountsOnSeveralMachines` / `TestKernelMachineWorkspace` 失败也定位到了：假内核夹具不回答网关在后台规范化工作目录时发的 `ls`，那条请求留在缓冲里被后面的断言当成别的消息读走。夹具改成和真实内核一样随手回 `ls_result`（断言不变）。连续 6 次全量 app 测试在修正后全部通过。

验证：新增 `TestFileWatchKeepsGaplessSequencesPerNodeEpoch`（两个节点交错上报时各自序号为 1、2 和 1）；全量 `go test ./...`、`go vet`、app/hooks race 与连续全量 app 运行通过。

### P2 生产协商：后台 Job 用户路径打通

`App` 现在实现 `JobProtocol()`，内核 hello 里 `job_manager`/`job_stream`/`lease_detach` 都在时真正协商开启节点上的后台 Job；此前只有测试包装器实现了它，生产网关从不宣告，所以后台化、Job 工具、`/jobs /bg /job`、网页任务页和平台卡片都停在“底盘已实现但不启用”。网关仍在任务身份与终态名额可靠落盘之后才给执行授权（既有 `jobs.go`/`job_events.go` 路径）。

验证：`TestRealCJobUserPathFromChatToHook`（真 C 内核、不带包装：模型在聊天里起后台 Shell → 网关登记 → `Job` 工具 status 到达网关 → `/jobs` 列出该任务 → 完成后 `job.completed` 经可靠日志投递给 Hook，终态持久且有 `TerminalEventID`）；既有真实 C Job 专项（移交/取消/按钮原地转后台/安静分支保留身份）与全量 `go test ./internal/app` 通过。

未做：真实飞书/Telegram 卡片联调（需要真实渠道账号），SSH 目标节点能力降级的现场验证。

### §8.8 网页分支界面（F1/F2 收尾）

网页现在真的能看到、操作安静分支：会话列表按父子折叠（来源会话下面一枚「展开 N 个分支」，展开后缩进显示，分支行标「安静分支 · 状态」），会话信息（`SessionInfo.branch`）带上分支元信息；会话面板新增分支卡片——本身是分支时显示分叉点、安静运行和「把结论带回来源会话」，普通会话则列出自己的分支，可继续（可填这一轮要做什么）、暂停、引入结论。新增两个只读/操作接口：`GET /api/sessions/{sid}/branches`、`POST /api/branches/{sid}`（action=continue/pause/archive/inspect/report，身份 `web:admin`，报告回复走网页通道）。

验证：`TestBranchHTTPListReportAndActions`（未登录 401、列表字段 `delivery_policy=silent`、`SessionInfo.branch.parent_sid`、空分支报告 400、暂停、归档后继续被拒绝、未知 action 400）；网页 `npm run check` 0 errors / 0 warnings、`npm test` 6/6、`npm run build` 通过。这一轮没有在真实浏览器里点一遍（当前环境没有浏览器），只做了类型检查和构建。

未做：设计 §8.8 里"分支报告在父会话按 kind=branch_report 显示成卡片"仍是一条带 `[分支报告]` 标记的普通消息；会话列表还没有分支的活动摘要（最近进展）。
