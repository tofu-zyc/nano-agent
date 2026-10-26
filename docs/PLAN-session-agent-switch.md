# 会话切换 agent 修复方案

日期：2026-10-26。状态：已在 v0.7.3 实现并验证（2026-10-26）。交接时的方案保留在下面，实施结果和与方案的差异见文末。

## 1. 已确认的问题

不仅主会话缺少入口，已有普通会话也没有切换入口。

- `gateway/web/src/components/panels/SessionPanel.svelte` 的“属于哪个 agent”只有名字和“去它的节点”按钮。
- `gateway/web/src/components/Conversation.svelte` 的聊天顶部没有选择 agent 的控件。
- `gateway/internal/app/query.go` 的 `SessionPatch` 没有 agent 字段。
- `gateway/internal/app/agents.go` 的 `agentOf` 和 `query.go` 的 `sessionInfo` 直接用 `agents.SessionAgent(sid)` 推断所属 agent。
- 默认会话 `main` 没有前缀，因此总被推断为 default；其他 agent 的会话通常是 `@<agent>/<base>`。

新建会话已有 `NewSessionIn` 和 agent 选择逻辑，但这不能更改已有会话。修复需要贯通持久化、运行时身份、接口与 UI，不能只添加一个下拉框。

## 2. 建议行为

- 主会话和普通会话均可切换；会话 ID、标题、历史、挂载目录和接入窗口保留。
- 下一轮使用新 agent 的人格、模型默认值、工具、上限、作息和记忆；现有会话级覆盖继续优先，UI 明确提示。
- 当前有运行、排队消息或上下文整理时，先禁止切换并给出原因，避免一轮中途换身份。
- 刷新、网关重启、归档和重置上下文后，选择仍然保留。
- 内部 life / dream、定时任务运行会话、房间成员会话保持固定身份。多人房间的公共记录没有单一可切换 agent，成员在房间设置修改。
- 外部平台的普通群聊会话与多人 agent 房间不同：前者仍可切换其单一回复者，群聊权限和规则保持有效。

## 3. 持久化与身份解析

在 `gateway/internal/session/settings.go` 的 `Settings` 增加可选 `Agent` 字段。旧数据为空时继续从 sid 前缀推断，避免批量重命名历史或迁移所有会话。

在 app 层增加统一的“当前执行 agent”解析函数，普通会话优先读取设置；内部会话仍由固定 ID/任务归属决定。会话 ID 的前缀继续承担原有消息路由命名空间职责。

| 调用用途 | 应使用哪种身份 |
| --- | --- |
| 人格、模型、工具、限制、记忆、正在做什么 | 当前会话选择的 agent |
| SessionInfo、会话列表、agent 的会话计数 | 当前会话选择的 agent |
| 新建定时任务的 owner | 创建时当前会话选择的 agent |
| 已有定时任务运行 | 已保存的 Task.Agent；切换聊天不自动转交独立任务 |
| life 主动消息的目标归属、FollowUp | 目标当前归属；旧归属回调不得继续执行 |
| 机器人重新绑定后的入口路由、`mainOf` 的路由家族计算 | 原 sid 命名空间；不要机械替换成当前 agent |
| life/dream/cron/房间成员内部身份 | 固定所属 agent |

特别检查 `endpoints.go:attach` 中机器人 agent 与会话前缀的比较。若直接替换成“当前执行 agent”，用户手动切换 main 后，下一条机器人消息可能被自动改接回旧会话，表现为切换不生效。

切换主会话后，新的 life agent 仍应能将这个 main 识别为主人会话；旧 agent 不能再往其中主动发送。不要仅用 `SessionFor(newAgent, "main")` 判断主人目标。

## 4. PATCH 的处理顺序

1. 校验会话存在、目标 agent 存在、会话可切换且当前没有运行/队列/压缩。
2. 验证整份 patch，包括同时提交的模型、思考强度、工作目录等；失败不能留下部分设置变更。现有 `UpdateSession` 在其他字段验证前保存 workspaces，接手时要避免沿用这一副作用。
3. 以新 agent 的实际模型检查思考强度；不兼容的旧设置恢复默认。`levelsOf` 当前主要看会话 model，需核对 agent 默认模型的覆盖。
4. 保存新 Agent，清理旧 Prefix、环境说明、记忆/近况注入缓存与 recalled 集合，确保下一轮按新身份重新构造上下文。
5. 清理旧 agent 对本会话的 Awaiting / FollowUp；取消或失效处理尚未执行的群聊触发。每个延迟回调执行时再次校验 owner，防止旧文件或恢复队列带来遗漏。
6. 发布会话和 agent 状态事件，使对话、拓扑、列表计数立即一致。

建议在 SessionInfo 暴露是否固定归属、当前禁止切换的原因；前端只据此显示状态，后端仍负责最终验证。无变化的重复提交可以幂等成功。

## 5. 历史、记忆与删除策略

保留历史意味着新 agent 能看到同一个会话的既往对话，不意味着旧 agent 的人格记忆文件也要复制过来。

切换边界应有持久来源信息或交接记录，明确既往 assistant 内容可能由其他 agent 产生。旧 life/memory 注入文本可能仍在历史中，清除注入哈希不能删除这些旧文本。新上下文应能辨认所属关系；dream 也不能把旧 agent 的自述直接当成新 agent 的亲历记忆。

接手实现时，选择并验证一种来源记录方式：消息级可选 agent 字段，或按历史位置记录归属区间。处理归档、重置和已有无字段消息的兼容关系。用户看到的历史保持完整；长期记忆按来源保留 agent 边界。这部分不要用“全部历史直接转为新 agent 的记忆”草率替代。

删除 agent 时，要特别处理显式选择它的 main 和普通会话，不能让 main 从列表中消失。建议自动交给默认 agent 并刷新上下文；存在正在执行的受影响会话时暂拒删除。已经从 `@old/...` 转给别人的会话仍应可见。全量验证后再写入，避免删除途中部分转交。

## 6. 需要逐一检查的代码

| 文件 | 检查点 |
| --- | --- |
| `internal/session/settings.go`、`store.go` | 新字段、保存/加载、Reset 与 Archive 后保留选择 |
| `internal/app/agents.go` | `agentOf`、agentInfo 计数、DeleteAgent、命令提示 |
| `internal/app/query.go` | SessionInfo、Snapshot 过滤、SessionPatch、整份 patch 验证 |
| `internal/app/models.go`、`context.go`、`memory.go` | 有效模型、思考选项、人格工具快照和记忆注入 |
| `internal/app/endpoints.go` | 区分机器人路由与执行身份，稳定保留 main |
| `internal/app/life.go`、`dream.go`、`doing.go` | 主动目标、回访、日记来源、忙闲归属和状态 |
| `internal/app/rooms.go` | Message 的发送者、目标授权及房间固定身份 |
| `internal/app/schedules.go` | 新任务使用当前 agent；旧任务归属不被改写 |
| `internal/web/api.go` | PATCH 字段和错误响应 |
| `web/src/lib/types.ts`、`state.svelte.ts` | 新字段、状态事件及当前选择 |
| `web/src/components/Conversation.svelte`、`panels/SessionPanel.svelte` | 可发现的切换入口、禁用说明及结果提示 |

可通过搜索 `agents.SessionAgent` 审查全部调用，但必须按用途分类，不能全局替换。

## 7. UI 建议

聊天顶部直接显示“对话 agent”选择器；会话设置内也提供同一控件。两处共用组件，避免校验和提示不一致。

选项显示头像和名字；当前忙碌时禁用并解释原因。保存期间防止重复提交，成功后以服务端返回值更新，失败时恢复原值；不要只修改本地 UI。

说明文案应明确“保留聊天记录，下一条消息由新 agent 回复；会话单独设置的人格和模型仍优先”。只存在一个 agent 时可仍显示当前选择，并提供已有的新建/管理入口。

## 8. 接手后的验收

1. main、普通网页会话、带其他 agent 前缀的会话、外部平台单 agent 群聊均可切换，sid 不变。
2. 通过真实测试模型请求验证下一轮的人格、模型、思考级别、工具集合和运行限制，不能只断言接口返回的 agent 字段。
3. 原机器人窗口继续连接同一会话；新消息不会把选择自动改回。
4. 保存重载、刷新、归档和重置上下文保留选择及所需的历史来源信息。
5. 无效 agent、忙碌、压缩中、内部会话及混合无效 patch 不产生部分更新。
6. 新 agent 的 life 能识别主会话；旧主动消息和 FollowUp 不得越过归属边界；切回来也不复活已取消回访。
7. 新建计划的 owner 正确，已有计划不被悄悄转交；dream 和统计按有效归属与消息来源处理。
8. 删除所选 agent 后 main 仍可用；已转交给其他 agent 的带旧前缀会话仍可见。
9. 浏览器实际操作顶部和设置内选择器，检查保存失败、刷新、窄屏与 SSE 更新。可扩展 `scripts/check-web-ui.py` 的隔离环境用例。

实施者可运行相关 Go app/session/web 测试、前端 `npm run check` 与构建，再按现有 UI 检查脚本验证。这里是后续验收清单，本次没有交付或验证一份完成的修复。

## 实施结果（v0.7.3，2026-10-26）

**已实现**

- 持久化：`session.Settings.Agent`（空表示继续按会话 ID 前缀推断，旧数据不用迁移）、`Settings.AgentSegments`（切换边界，绝对消息位置）、`Settings.Handoff`（一次性的交接说明）。`session.Msg.Agent` 记 assistant/tool 消息的来源。
- 身份解析：新增 `app/identity.go`，把「当前执行身份」（`execAgentID`/`agentOf`）和「会话 ID 路由命名空间」（`routeAgentID`）分开。人格、模型、工具、限制、记忆、会话计数用执行身份；窗口接线、`mainOf`、`@<agent>/life`、`@<agent>/cron`、房间成员继续用路由命名空间。内部会话身份固定。
- PATCH：`SessionPatch.Agent`；整份 patch 先全部验证（目标 agent、标题、模式、模型、思考强度、工作目录），再一次性写盘。运行中、排队、整理上下文时拒绝；同一 agent 的重复提交幂等。切换时清 Prefix / EnvNote / MemNote / LifeNote / Recalled，写切换区间和交接说明，清旧 agent 的 FollowUp / Awaiting / 群触发，并结束旧身份占用的桌面 session。
- 历史来源：消息级 `Agent` 字段 + 切换区间；没有字段的老消息按区间和 ID 前缀推断。dream 读到不属于当前 agent 的 assistant/tool 内容会略过并说明，不把旧 agent 的自述当成新 agent 的亲历。
- 删除 agent：显式选了它的会话（含 main）先检查忙碌，再把全部会话一次性交给默认 agent（保留 ID、历史、窗口、会话设置）；没有显式选择的 `@old/...` 会话保持旧行为（不显示，重建同 ID 后出现）。机器人、房间成员、心智状态一并处理。
- 路由：`attach` 只在会话没有显式选择时跟随机器人换绑；life 的“主人会话”识别改成看执行身份（`isOwnerSID`）；`Message` 的私聊授权也用执行身份；新建定时任务的 owner 用当前执行 agent，已有任务不被聊天切换改写。
- UI：`AgentSwitcher` 在聊天顶部和会话设置共用；`SessionInfo` 新增 `route_agent` / `agent_fixed` / `can_switch_agent` / `switch_blocked`；保存用服务端返回值，失败恢复原值并提示原因。

**与方案的差异和理由**

- 历史没有整体转成新 agent 的记忆：用消息级来源 + 边界区间。旧数据无法精确归属时按前缀/区间推断，不批量改写历史。
- 交接说明放在下一条用户消息前（`Settings.Handoff`），不改系统提示词：避免切换后立刻让整段前缀缓存失效，放一次就清掉。
- `SessionInfo.Agent` 的语义改为「当前执行身份」，新增 `RouteAgent` 明确路由命名空间，避免前后端继续混用。
- 删除 agent 采用「显式选择才转交」：前缀命名空间下本来就属于它的会话保持旧版本行为，符合已有用户的预期。
- 外部平台单 agent 群聊允许切换（沿用方案）；多人房间的成员和公共记录仍固定/无单一 agent。

**验证**

- Go：`TestSwitchAgentOnMainKeepsSessionAndNextTurnUsesNewIdentity`（真实模型请求断言人格、模型、思考强度、工具白名单、轮数上限、交接说明、消息来源）、`TestSwitchAgentBlockedWhileRunningOrCompacting`、`TestSwitchAgentPatchIsAtomic`、`TestInternalSessionsKeepFixedAgent`、`TestDeleteAgentTakesOverChosenSessions`、`TestBotEndpointKeepsExplicitAgentChoice`、`TestScheduleOwnerFollowsSessionAtCreationOnly`、`TestLifeOwnershipFollowsCurrentAgent`、`TestResetAndArchiveKeepAgentChoice`。
- 前端：`npm run check`、`npm run build`；`scripts/check-web-ui.py` 新增“聊天顶部切换、刷新后还在、会话设置里切换、切回默认”检查，0 failures。
- 未验证：真实 Telegram / 飞书 / QQ / 微信机器人换绑后的入口行为（测试里用假渠道模拟了 attach）；多人房间和长期归档历史的展示没有额外人工验收。
