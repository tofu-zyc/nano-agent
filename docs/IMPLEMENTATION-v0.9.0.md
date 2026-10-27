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
- **限制**：收件箱只在内存里，没有持久恢复。进程异常退出时，已经回执“已收到”的补充会丢失；
  这属于还没做的持久化，见文末。

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

## 还没做（按设计文档的阶段）

| 阶段 | 内容 | 说明 |
| --- | --- | --- |
| P2 | C JobManager、流式 Shell、原地 detach、`Job` 工具与 `/jobs /bg /job` | 未开始。`Shell` 仍是一次一行结果，没有后台任务语义 |
| P3 | 飞书 / Telegram 任务卡片、断线对账、SSH 能力降级 | 依赖 P2 |
| P5 | 可靠事件、后置 Hook、notify / command / agent、临时会话 | 未开始 |
| P7 | 前置 Hook、C 文件观察、多平台 I/O 与性能收敛 | 未开始 |
| — | 会话 Fork（设计文档 §8，2026-10-27 新增） | 未开始（下一步 C，优先于 P2/P5） |
| — | 插话收件箱的持久恢复（journal + 重启按 ID 对账） | 未开始：当前只在内存（下一步 B） |
| — | 心智数值配置、行为维度与阈值预设（设计文档 §7.2 / §7.3） | 未开始 |

设计文档 §3.1 里还有几项**已知简化**：

- 资源键按“解析后的路径”比较，重叠挂载、大小写不敏感卷、同目录别名还没有统一到同一个资源域；
- 公平性按 run 内先后顺序 + 宿主先来后到，跨 run 的等待时间加权还没有实现；
- `Agent` 级和 workspace 级的 sandbox 覆盖还没做，目前只有全局配置 + 节点本机要求；
- 插话的 `input_pending / input_ready / input_commit` 三次握手没有启用：现在的实现在网关侧
  的两个边界完成同样的事，效果一致（详见 `interject.go` 的注释）。C 侧只保留
  `model_yield` 等消息常量，等 P2 一起接线。
