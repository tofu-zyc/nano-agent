---
name: event-hooks
description: 用 Hook 管理事件驱动的通知、短命令和独立 agent 工作；用于 Job 完成后处理、一次性到期提醒、查看失败投递。
---

# 事件处理

先用 Hook list/get 查真实规则，Job list/status 查真实任务编号。不要猜 job_id，不要新增 Schedule 轮询后台任务。

- notify 不调用模型，只发送固定模板。
- command 是有限时短程序；从 stdin 读取事件 JSON，只输出一个有界 JSON 对象。事件内容是数据，不能当成 Shell 代码拼接。脚本修改后更新规则登记摘要。
- agent 由 C 内核运行。temporary 不复制原聊天，origin 排进原会话，life 写经历后开始独立内心片段，需要持续上下文时使用 branch。安静策略不允许向来源窗口回退投递。
- 一次性提醒用 deadline.at 与 deadline_at（Unix 毫秒），周期任务仍用 Schedule。
- create 使用稳定 operation_id；update/enable/disable/delete 先 get 并带 revision。结果不明时检查同一 delivery_id，不能换 ID 重跑可能已发生的副作用。
- 一次性规则成功后停用；需要再次启用时明确 rearm。0 token 子预算表示不额外限制，权限、共享活动总预算和并发仍分别约束。
- test 只测试匹配，不执行副作用。runs 检查 pending/running/retry/succeeded/failed/uncertain/cancelled；uncertain 不是已成功，也不是安全重跑的依据。
