# Hook 事件与投递

Hook 使用 runtime/events.jsonl 的可靠事件，不依赖网页事件流。规则、修订、一次性认领和固定投递集合由同一个单写者日志提交。执行前记录 running 和尝试次数，完成后记录结果。事件、规则正文、队列、并发和重试均有上限。

每个 event_id、hook_id、hook_revision 对应一个稳定 delivery_id。重启后 notify 可有限重试，外部平台可能重复收到消息；不安全的 command 和 agent 动作进入 uncertain，不能换一次身份盲目重放。一次性规则在认领事件时防止并发重复，成功后停用；停用或删除后，尚未开始的投递不再具有执行权限。

规则写入使用 journal 格式 2；普通事件仍可用格式 1。首次向已有日志写入规则前，以 32 KiB 缓冲保留 events.jsonl.v1.bak，已有备份不会覆盖。旧程序不能读取格式 2；降级必须先停止新版并保留整个 runtime 目录，不能把备份与新版日志混合恢复。

日志不把外部副作用描述为恰好一次。notify 的成功表示平台已受理请求；进程在外部成功后、本地回执前退出时仍有不确定窗口。

内部执行引擎使用一个调度循环、一个通知 worker、两个短命令 worker 和一个模型启动 worker；每个 Hook 同时最多一个处理器，每个 agent 同时最多一个 Hook 模型 run。模型与工具循环继续由 C 内核执行。相同对象的待执行事件可合并，正在运行的动作不会被去抖取消。

## 管理入口

网页顶栏「事件规则」可创建、编辑、启停和删除规则，查看投递记录、错误及独立运行对话。聊天可用 /hooks、/hook <编号> [get|runs|enable|disable|delete] 和 /hook create <规则 JSON>。Hook 工具的修改沿用 GatewayAdmin 审批；网页与斜杠命令是管理员的显式操作。

GET /api/hooks?agent=default 返回规则；action=runs&id=<编号> 查看有界投递页。POST /api/hooks 接收 action、id、revision、spec、once、enabled。创建必须带稳定 operation_id；同一操作重复提交返回同一规则，删除后不能换 ID 重新导入。更新携带读取到的 revision，冲突需要重新读取。启用已消费的一次性规则需显式 rearm=true。

```json
{"action":"create","operation_id":"test-completion-1","once":true,"spec":{
 "name":"测试完成后分析","on":["job.completed","job.failed"],
 "match":{"job_id":"j_真实任务编号"},
 "handler":{"type":"agent","conversation":"temporary","prompt":"读取此 Job 的真实终态和必要日志，说明结果。","token_budget":10000},
 "deliver":"creator","debounce_ms":500,"cooldown_ms":1000
}}
```

规则登记时固定来源会话、agent、创建者、权限上限、工作目录、工具集合和原窗口凭据指纹；运行时取其与当前权限的交集。删掉或卸载工作目录会暂停，不会回退 HOME。临时会话为固定的 hook:<delivery_id>（其他 agent 带 @agent/ 前缀），不复制来源整段历史，完成或异常退出后释放 App 和 Store 缓存。life 写一条确定性外部经历，再开启独立片段；branch 复用同一 delivery_id 的持久 Fork，结果不投回来源。

notify 只替换固定模板变量。command 从标准输入读取最多 4096 字节的事件 JSON，标准输出必须是单个 JSON 对象，例如 {"trigger":true,"summary":"构建失败"}。默认 5 秒，最长 30 秒，最多 64 KiB 输出。script 使用 agent/hooks 下的相对路径，登记固定内容摘要；执行前重新校验并放入目标节点只读版本缓存，文件改变必须更新规则。普通命令结果不隐式启动模型。

deadline.at 使用 deadline_at 的 Unix 毫秒值，统一最小堆调度；每个规则修订最多产生一个固定到期事件。停用、一次性消费和重启都不会重新触发同一修订。相同对象的冷却和每分钟最多 16 次触发从持久尝试记录恢复。已完成的事件正文保留每 agent 最近 100 条、最多 7 天；正在处理的投递不被清理，来源游标保留去重能力。来源游标总量最多 4096，达到上限明确暂停接收。

当前模块已连接 Job 终态、run.started/finished、tool.after/failed 与 approval.waiting。前置 C 屏障、文件观察和 message.received 的平台 ID 对账另在后续运行链模块实现，不能仅凭这份配置声明已支持。
