# v0.6：活的 agent 和长期记忆

日期：2026-10-24。分支 `dev/v0.6`（从 main 的 v0.5.1 开出）。这份文档是 v0.6 的设计和交接：**接手的人先读这里**，进度在文末「进度」，每完成一步就更新。

## 用户的要求

- agent 分两种：**工具型**（纯粹干活的工具）和**拟人型**（更像人：能“模拟”出自主活动、自主思维——无聊了会自己上网冲浪、会找人聊天）。
- **私聊和群聊是同一套“活的”设计**，不是两套；先调研成熟方案再做（已调研，见下）。
- **长期记忆**一起解决：拟人型要自己梳理人物关系、人物画像、学到的知识，长期记忆至关重要；也要参考成熟方案。
- 额度不够时先写好交接文档（就是这份），方便别的 AI 接手。

还要守住路线图（[PLAN-roadmap.md](PLAN-roadmap.md)）里用户定的原则，尤其：
- **少内置机制**：怎么整理记忆、醒来干什么，这些策略写在**可以改的技能文件**里（`skills/dream/SKILL.md`、`skills/life/SKILL.md`），网关只负责触发、给工具、记进度。
- **缓存优先**：系统提示、工具定义只在固定时机变（新会话、`/reset`、压缩之后）。记忆、近况这类会变的东西，和现有的「环境说明」一样**放在用户消息前面**，变了才再放一次，不改系统提示。
- 安全靠人：不加额外的黑名单；但花钱的东西（主动说话、醒来、整理记忆）都有每天/每小时上限，能在网页上改。

## 调研结论（2026-10-24）

| 项目 | 借鉴了什么 |
|---|---|
| 麦麦 MaiBot（[仓库](https://github.com/Mai-with-u/MaiBot)、[配置](https://docs.mai-mai.org/manual/configuration/bot-config)） | 私聊、群聊同一个规划循环；新消息先过**不调模型的门槛**（`src/maisaka/turn_trigger/reply_likelihood.py`：被提到名字 +1.59、@了别人 −1.12、有问号 +0.48、只有表情图片 −1.03、最近 5 分钟自己说话占比 +2.65、最近 5 分钟消息数 −0.037/条、log(距上次自己说话秒数) +0.057、本批新消息数 +0.10/条，截距 −1.20，过 sigmoid 得回复概率）；乘发言频率；连续不说话指数退避（15 秒起翻倍，最多 300 秒，积压 6 条以上直接放行）；私聊不设门槛。人物画像自动生成 + 手动覆盖（`src/A_memorix`）。 |
| OpenClaw（[heartbeat](https://app.unpkg.com/openclaw@2026.6.6/files/docs/gateway/heartbeat.md)、[memory](https://docs.openclaw.ai/concepts/memory)） | 心跳：定时在主会话里叫醒、有作息（activeHours）、忙时跳过、没事回 `HEARTBEAT_OK` 不发；精简上下文把每次心跳从约 10 万 token 降到 2–5 千。记忆就是 Markdown 文件（精选 `MEMORY.md` + 每天日志），人能直接看能改。 |
| Inner Thoughts（[CHI 2025](https://arxiv.org/abs/2501.00383v2)） | 冷场时触发（他们用 10 秒）；冷场越久越想说；想说的理由：相关、信息差、影响、紧迫、连贯、新意、平衡、气氛——写进守则。 |
| Letta / MemGPT（[sleep-time](https://docs.letta.com/guides/agents/architectures/sleeptime)） | 常驻的核心记忆块（自我、对方）+ 用工具检索的档案；空闲时后台整理记忆。 |
| Mem0（[2026 综述](https://mem0.ai/blog/state-of-ai-agent-memory-2026)） | 写入只追加（ADD-only）、和人关联；检索多路合分。 |
| Zep / Graphiti | 事实会过时：**标失效（带时间和原因）不删除**，历史还能查。 |
| Hindsight（[论文](https://arxiv.org/abs/2512.12818)） | 世界事实、自己的经历、对人或物的总结、自己的**看法（带把握程度，新证据加强或削弱）**分开存——拟人型的“看法”“画像”就是这样分的。 |
| 斯坦福小镇 Generative Agents | 定期反思，把经历提炼成看法；作息。 |
| AstrBot、Kindroid | agent 自己约将来的事（我们的 `FollowUp`）；用户写一段“不在时它可以做什么”（我们的 `life` 技能）。 |

测试集上的分数大多是厂商自报的，只当参考。共同结论：**常驻少量核心记忆 + 可检索的记忆库 + 原始记录作证据；写入只追加；空闲时整理**。这一版不做向量检索（路线图也这么定的），关键词检索用 BM25，中文按相邻两个字切分。

## 总体结构

```
                   ┌──────────── 每个 agent ────────────┐
 聊天（私聊/外部群/网关里的群）    内心活动（拟人型）      做梦（整理记忆）
 统一聊天循环：听→触发→门槛→说/不说   @<id>/life，作息里定时醒   @<id>/dream，空闲/夜里/睡着时
        │  读：记忆说明（消息前）          │ 冲浪、想事、找人          │ 读日记（原始记录）
        │  写：Knowledge 工具              │ 写近况、Knowledge          │ 写知识库、画像、自我认知
        └──────────────── 知识库 agents/<id>/knowledge + self.md ────────────────┘
```

- **工具型**（`style: tool`，默认，老 agent 都是它）：现在的样子——有人说话就回；**群里只有被叫到才回，不主动插话**（v0.5 默认是“少插话”，这一版改了，发布说明里要写）；有长期记忆和做梦（记偏好、项目情况，不形成“看法”和“自我认知”）。
- **拟人型**（`style: life`）：加上拟人守则、统一聊天循环里的主动部分（群里插话、私聊里的“像人一样”）、内心活动、睡着时做梦、看法和自我认知。

## 数据布局

```
~/.nano-agent/
  agents/<id>/
    persona.md               人格（已有）
    group.md                 群聊守则（已有，可选）
    life.md                  拟人守则（可选，没有用内置的；拟人型的每个会话都带）
    self.md                  自我认知（拟人型；做梦、内心活动维护；人能改）
    knowledge/               这个 agent 的知识库：一条一个 .md
      people/<slug>.md       人物画像（type: 人物）
      INDEX.md               自动生成的索引，别手改
    skills/                  这个 agent 自己的技能（和全局合并，同名自己的优先；dream/、life/ 在这里覆盖）
    dreams/YYYY-MM-DD.md     梦境日志（每次整理的最终回复，同一天追加）
    dream-state.json         做梦进度：每个会话的原始记录处理到第几条、今天整理了几次
  knowledge/                 所有 agent 共用的知识（结构同上，没有 people/ 的要求）
  skills/dream/SKILL.md      自带的“怎么整理记忆”（随安装包更新；用户改了的不覆盖，和其他自带技能一样）
  skills/life/SKILL.md       自带的“醒来干什么”
  data/life.json             内心活动状态：每个 agent 的下次醒来时间、今天醒了几次、主动找了几次、近况、等谁回话
```

### 一条记忆的格式

```markdown
---
title: 张三
type: 人物            # 事实 偏好 事件 经验 流程 知识 看法 人物
summary: 产品经理，说话直，养了两只猫     # 索引里那一行；没有就取正文第一行
tags: [同事, 产品组]
people: [张三]        # 相关的人（人物条目的 title）
ids: [tg:123, feishu:ou_xxx]  # 只有人物条目有：这个人在各渠道的身份（消息里的 who）
when: 2026-10-20      # 事情发生的时间（可选）
confidence: 0.7       # 看法的把握 0–1（可选，只有看法）
visible: all          # all（谁在都能用）/ admin（只在管理员的会话里）/ 某个身份（只在和他的私聊里）
source: 私聊 tg:123 2026-10-24     # 从哪来的：会话、做梦、用户手写
updated: 2026-10-24
invalid: 2026-10-30 已经换工作了，见「张三的新工作」   # 过时了不删，标失效：索引里不再列，搜索默认不出
---
正文（Markdown，自由写）
```

- 文件名由标题生成（拉丁字母小写、中文保留），重名加 `-2`。人物放 `people/`。
- `INDEX.md`：每条一行 `- [类型] 标题 — summary（相对路径）`，按类型分组，失效的不列；每次写入后网关重建，不用模型维护。
- 管理员（`access.admins` 里的身份，可能有好几个渠道）合成一个人：画像 `people/owner.md`，`ids` 里写所有管理员身份，标题默认“主人”（agent 可以改成真名）。

## 记忆怎么进上下文（缓存优先）

系统提示不变（人格 + 群聊守则 + 拟人守则）。每轮开始时，像 `EnvNote` 那样在这轮第一条用户消息前面加说明，**内容变了才加**（会话设置里记哈希：`MemNote`、`LifeNote`）：

1. **记忆说明**（`[记忆]` 开头），最多约 2k token：
   - 自我认知 `self.md`（拟人型）；
   - 对方的人物画像（私聊：按说话人的身份找 `ids`；管理员的会话用 `owner.md`）；
   - 知识库索引（超了先保留人物和最近更新的，再不够只留标题）；
   - 群会话、房间里只放 `visible: all` 的条目；私聊里放 `all` + 对方身份的；管理员的会话全放。
2. **近况**（拟人型，`[近况]` 开头）：最近 3 次内心活动的近况，每条一行带时间。
3. **想起来的**（每轮都可能有）：用这轮用户消息做关键词检索，取 1–3 条分数够高、这个会话里还没提过的条目，附正文前 300 字和路径；不调模型。会话设置里记已提过的路径（`Recalled`）。

群里由触发（不是用户消息）开的一轮：说明作为一条单独的 `[系统]` 用户消息追加在历史末尾，同样只在变了的时候。压缩（`CommitCompact`）后把 `MemNote`/`LifeNote`/`Recalled` 清掉，下一轮重新放。

## `Knowledge` 工具（网关执行）

动作：
- `list`：索引（按会话可见性过滤）；
- `search {query, type?, person?, include_invalid?}`：BM25（标题、summary、tags 权重高），返回最相关的 8 条（路径、标题、类型、片段）；
- `read {path}`：读一条（含 frontmatter）；
- `write {path?, title, type, summary?, tags?, people?, ids?, when?, confidence?, visible?, body}`：新建或整条覆盖；
- `edit {path, old, new}`：精确替换（和 Edit 一样，`old` 要唯一）；
- `invalidate {path, reason}`：标失效（推荐代替删除）；
- `delete {path}`：真删（只有管理员的会话、做梦里能用）；
- `person {who? name?}`：按身份或名字找人物画像。

规则：
- 路径只能落在这个 agent 的 `knowledge/`、`self.md`、`skills/` 下，和共用的 `knowledge/`（写共用的按会话权限模式，`ask` 时要审批——这一版先简单：只有管理员会话和做梦能写共用的）。
- 群会话、房间里只能 `list`、`search`、`read`、`person`，只看得到 `visible: all`。
- 写入自动补 `updated`、`source`（会话 ID 和日期，做梦时是 `dream <日期>`）。
- 用户说“记住……”时模型调 `write`；工具说明里写清楚。

## 做梦（整理记忆）

- 会话 `@<id>/dream`（默认 agent 也带前缀，和房间成员会话一样），**每次都是全新上下文**（像定时任务那样先 `resetHistory` 归档）。
- 系统提示 = 人格 + dream 技能正文（`agents/<id>/skills/dream/SKILL.md` > `skills/dream/SKILL.md` > 内置兜底）+ 一段情况说明（agent 类型、今天日期、上次整理时间）。
- 工具：`Diary`、`Knowledge`（全部动作）。不挂工作目录。
- **`Diary`**：
  - `list`：这个 agent 哪些会话有没处理的日记，各多少条、时间范围（会话来源：这个 agent 的所有会话的原始记录 `store.Archive`，不含 `dream`、`life`、`cron:`；以及 `data/archive/<sid>/` 里的归档文件，按文件名记进度）；
  - `read {sid, from?, limit?}`：读一段（默认从进度往后 60 条），每条带时间、谁说的；工具输出截到 2KB；
  - `done {sid, upto}`：标记处理到哪了（写 `dream-state.json`）。
- **时机**（agent 设置 `dream_when`）：
  - `idle`（工具型默认）：这个 agent 所有会话空闲超过 `dream_idle`（默认 2h），并且没处理的日记 ≥ 20 条；
  - `nightly`：每天 `dream_at`（默认 03:00）；
  - `sleep`（拟人型默认）：作息里进入睡觉时间之后（有没处理的日记才做）；
  - `off`。
  - 每天最多 `dream_max` 次（默认 1）。管理员可以 `/sleep` 或网页上「现在整理」手动开始（不受每天上限）。
- **模型**：`dream_model` 可以单独选（便宜的模型），空就用 agent 的。
- **结果**：这一轮的最终回复追加到 `dreams/<日期>.md`；拟人型同时写一条进内心会话（“昨晚整理记忆：……”）。网页上 agent 状态显示“整理记忆中”。
- 做梦期间这个 agent 的其他会话照常（会话之间本来就并行），做梦不打断。
- dream 技能（自带版本）的要点，来自调研：
  1. `Diary list` 看有哪些新日记，按会话一段段 `read`；
  2. 抽取值得长期记住的：人（新认识的人、对人的新了解、关系）、事件（带时间）、事实、偏好、经验/流程、学到的知识；拟人型还有看法（带 confidence）；
  3. 写之前先 `search`，有就更新（`edit`/`write` 覆盖），没有就新建；**矛盾的旧条目 `invalidate`，不删**；
  4. 更新人物画像：是谁、和我的关系、性格、偏好、近况、要紧的事；一人一个文件，`ids` 写全；
  5. 拟人型：回顾当天，形成或调整看法（新证据加强就调高 confidence，反驳就调低或失效）；更新 `self.md`（我是谁、兴趣、最近在想什么、对生活的感受）；
  6. 每段处理完 `Diary done`；
  7. 最后用几句话写下今晚整理了什么（这就是梦境日志）。
  - 隐私：私聊里得知的事 `visible` 设成对方身份或 `admin`；群里公开说的才是 `all`。

## 拟人型：统一聊天循环

外部群、网关里的群、私聊用同一套；工具型只用其中“被叫到才回”的部分。

- **听**：群消息照旧只记进历史（`listenGroup`、`roomHumanSay`）。
- **触发**：① 被叫到（@、回复它、叫名字）或私聊来消息 → 一定处理；② 群里冷场了（`group_quiet`，默认 20 秒）且有新消息 → 进门槛；③ 它自己用 `FollowUp` 约的时间到了 → 在那个会话里醒来；④ 内心活动用 `Message` 找到这个会话 → 消息直接发出去，对方回了照常处理。
- **门槛**（群里的②，不调模型）：`p = sigmoid(麦麦的特征和权重)`，乘积极度系数（low 0.35 / normal 0.7 / high 1.0，off 不进）；用随机数抽，抽中才调模型。连续决定不说话（回 `[不回复]`）→ 退避：从第 2 次起 15 秒 × 2^(n−2)，最多 5 分钟；新消息积压 6 条以上不退避。原来的“每小时说几次 / 判断几次”保留当硬上限。测试里随机数可注入（`a.rand`）。
- **私聊里像人**（拟人型）：回复按空行拆成最多 3 条，中间停一会儿再发（复用 `sendGroup` 的拆条）；收尾的话（“好的”“晚安”）可以回 `[不回复]` 不接；不发思考、工具过程和用量的规则**不变**（私聊里干活时照常显示，按显示档位）。只有这一轮没有调用工具的纯聊天回复才拆条。
- **`FollowUp` 工具**（拟人型的私聊和群里）：`{minutes, note}`，到点在这个会话里叫醒它，附一条 `[系统] 你之前约好这时候再看看：<note>`；一个会话同时只留一个（新的覆盖旧的），最长 24 小时；对方先说话了就取消。状态存 `data/life.json`。
- **房间**：agent 在群里说的话 @ 了或叫到别的成员 → 叫醒那个成员（`roomShouldSpeak`），连续发言、来回轮数的暂停规则兜底；房间里的主动发言也计入每小时额度（修 v0.5 漏的 `groupSpeaks`）。

## 拟人型：内心活动

- 会话 `@<id>/life`（网页里能看，只读）。上下文预算小：超过约 40k token 就按正常流程压缩（`compactLimits` 对 `life` 会话封顶），这样保持连续又不贵。
- **作息** `life_awake`：默认 `08:00-23:30`（网关时区，可以跨午夜如 `22:00-06:00`）。睡着时不醒、不主动找人。
- **活跃度** `life_level`：
  | 档 | 醒来间隔 | 每天最多醒 | 每天最多主动找人 |
  |---|---|---|---|
  | low | 2–4 小时 | 4 | 1 |
  | normal（默认） | 1–2 小时 | 8 | 2 |
  | high | 30–60 分钟 | 16 | 4 |
  间隔在范围里随机；越久没人找它，下一次越早（“无聊”）：距上次有人找它超过 3 小时时间隔减半。
- **不醒**：睡着；它有会话正在跑或排队；20 分钟内有人找过它；今天的次数或 token 预算（`life_budget`，0 不限）到了；没有模型或内核。
- **醒来的消息**（追加进内心会话，是这条时间线的一部分）：现在几点星期几、多久没人找、今天第几次醒、还能主动找几次人；最近的聊天（每个会话一行：谁、最后一句、多久前；只列这个 agent 的）；谁还没回它的主动消息（先别再找）；可以用的 `to`。
- **能做**：`WebSearch`、`WebFetch`（冲浪）、`Knowledge`（查和记）、`Message`（找人）、什么都不做。系统提示 = 人格 + 拟人守则 + life 技能正文（`agents/<id>/skills/life/SKILL.md` > `skills/life/SKILL.md` > 内置）。
- **找人范围** `life_reach`：`owner`（默认，只找管理员）/ `all`（私聊过的人都行）/ `off`。网关里的群和别的 agent 不受 `owner` 限制（都是自己人）。
- **不追着发**：主动给某个私聊发了消息、对方还没回 → 不能再主动发给他（Message 直接拒绝并说明）。
- **近况**：这一轮的最终回复（截 200 字）记成一条近况（`data/life.json`，留最近 10 条），其他会话里按「近况」说明带上。
- 网页「现在叫醒它」、聊天 `/wake`（管理员）：立即醒一次，不受间隔限制（仍受每天上限以外的条件：没在跑）。

## `Message` 工具扩展

- `to` 新增 `chat:<会话ID>`：发到这个 agent 自己的某个私聊会话。消息记进那个会话的历史（`assistant`），发到这个会话最近说过话的那个窗口（`epRec.Last` 最大的非网页窗口；没有就只在网页显示），并给网页发事件。
- 谁能用：管理员让它发（管理员会话里），或者内心活动里发（受 `life_reach`、不追着发、每天上限约束）。访客不能用（已有）。
- 写进被发会话的设置 `Awaiting`（主动消息时间），对方在那个会话里说话时清掉。

## 设置（每个 agent，agent 面板按组显示）

`agents.Agent` 新字段（指针为 nil = 默认值；面板上的“跟随全局”对这些项显示“用默认”）：

| key | 组 | 默认 | 说明 |
|---|---|---|---|
| `style` | 基本 | `tool` | 类型：tool 工具型 / life 拟人型（面板顶部的切换，不在列表里） |
| `group_eagerness` 等已有 5 项 | 群聊 | 工具型 off（不可改）/ 拟人型 low | 已有 |
| `life_awake` | 拟人 | `08:00-23:30` | 醒着的时间 |
| `life_level` | 拟人 | `normal` | 活跃度 |
| `life_reach` | 拟人 | `owner` | 主动找谁 |
| `life_budget` | 拟人 | 0 | 内心活动每天最多用多少 token，0 不限 |
| `memory` | 记忆 | `on` | 长期记忆开关（关了就不放记忆说明、不做梦、没有 Knowledge 工具） |
| `dream_when` | 记忆 | 工具型 `idle` / 拟人型 `sleep` | 什么时候整理 |
| `dream_at` | 记忆 | `03:00` | nightly 时几点 |
| `dream_idle` | 记忆 | `2h` | idle 时空闲多久 |
| `dream_max` | 记忆 | 1 | 每天最多自动整理几次 |
| `dream_model` | 记忆 | 空 | 整理记忆用的模型，空就用 agent 的 |

`AgentSetting` 加 `Group string`（`work`/`group`/`life`/`memory`）和 `DefaultLabel`；面板分组渲染，拟人组只在拟人型显示。**顺带修**：`applyAgentSetting` 里 `v == nil`（恢复默认）时 `clear()` 没处理 `group_*` 几项，点“跟随全局”不生效（v0.5 的 bug）。

## 网页

- **agent 面板**：顶部类型切换（工具型 / 拟人型，带一句说明）；拟人型的状态卡（醒着·空闲 / 💤 睡着到几点 / 💭 在想事情 / 🌙 整理记忆；下次醒来；今天醒了几次、主动找了几次人、用了多少 token；按钮「现在叫醒它」「看它的内心」）；记忆卡（多少条、多少人、上次整理、按钮「现在整理」「看记忆」）；设置按组。
- **记忆节点**（`MemoryPanel`，原来写着“长期记忆还没有做”）：上面保留上下文整理的说明；下面选 agent → 标签页：人物、记忆、自我认知、梦境日志；搜索框；每条能看、改（Markdown 编辑框，含 frontmatter）、标失效、删；失效的灰显。
- **会话页**：`life`、`dream` 会话只读（不显示输入框，顶上写“这是它的内心活动 / 整理记忆的过程，只能看”）。
- **拓扑**：agent 卡片上显示「拟人」小标和状态图标。内心、做梦会话不画进会话列（从 agent 面板进）。

## 命令

- `/memory [关键词]`：没关键词看索引，有就搜；`/forget <标题或路径>`：标失效（管理员）；`/sleep`：现在整理记忆（管理员）；`/wake`：叫醒内心活动（管理员，拟人型）。都写进 `/help` 和命令提示表。

## 测试要点

- 知识库：解析/生成 frontmatter、文件名、索引重建、BM25 中英文、可见性过滤、路径越界被拒、失效不进索引；`Knowledge` 各动作；群会话只读。
- 记忆说明：只在变了时追加；压缩后重放；群里只有 `all`；想起来的不重复。
- 做梦：假时钟测 idle / nightly / sleep 触发、每天上限、`Diary` 分段读取和进度、技能文件优先级（agent > 全局 > 内置）、梦境日志、写入带 `source: dream <日期>`。
- 内心活动：作息（含跨午夜）、间隔、忙时不醒、每天上限、预算、找人范围、不追着发、近况带进别的会话、`/wake`。
- 统一循环：门槛的概率（注入随机数）、退避、私聊拆条和 `[不回复]`、`FollowUp` 到点/被取消、房间里 agent @ agent、房间发言计额度、工具型群里不插话。
- 网页：`check-web-ui.py` 加类型切换、拟人设置只在拟人型显示、记忆节点能搜能改、内心会话只读。
- 全部用 mock 模型和临时目录（`scripts/test-isolated.sh`）。

## 进度

分阶段做，每阶段 `make test` 过了单独提交，推到 `dev/v0.6`，更新这里。

- [x] **A 知识库**（后端完成，网页面板在 E）：
  - `gateway/internal/knowledge`：`entry.go`（frontmatter 解析/生成、`Slug`、`VisibleTo`）、`search.go`（`Tokens` 中文二元切分、BM25）、`store.go`（按修改时间刷新、`Write`/`WriteRaw`/`Edit`/`Invalidate`/`Delete`/`Search`/`Mentioned`/`Person`、自动写 `INDEX.md`；写入后直接更新缓存，不靠修改时间）。
  - `app/memory.go`：`memStore`/`sharedMem`、`viewerOf`（群里只看 all，管理员和内心活动/做梦/定时任务全看，私聊看 all+对方）、`memoryNote`（`[记忆]…[/记忆]` 块：自我认知、对方画像、索引，约 2k token 封顶）、`recallNote`（`[想起来的]…[/想起来的]`：提到标题的 + BM25 命中 ≥2 个词的，最多 3 条，会话里提过的不再提）、`memoryNotes` 在 `startRun` 里放到消息前面（群里由触发开的一轮作为单独一条 user 消息）；会话设置 `MemNote`（哈希）、`Recalled`，压缩/清空/撤回时清掉。
  - `Knowledge` 工具（`knowledgeCall`）：list/search/read/write/edit/invalidate/delete/person；`self.md`、`shared/` 前缀；群里只读；访客私聊里写的强制只给他自己看、不能真删；写入自动补 `source`、`updated`；没写 visible 时：群里 all，知识/流程/经验 all，其余主人会话 admin。工具定义加了 `prefixVer`（旧会话的工具快照作废一次，好让老会话拿到 Knowledge）。群聊白名单默认加了 Knowledge。
  - 命令 `/memory [关键词]`、`/forget <标题或路径>`。
  - 网页 API（`app/memory_web.go`、`web/agents.go`）：`GET /api/agents/{id}/memory`、`GET …/memory/search?q=`、`GET|PUT|DELETE …/memory/item?path=`（PUT 带 `{raw}`，path 为空就新建）、`POST …/memory/invalidate?path=`（`{reason, restore}`）、`PUT …/memory/self`（`{text}`）；事件 `{t:"memory", name:<agent>}`。
  - agent 自己的技能目录：`skills.Merged`/`LoadAny`/`Body`/`ToolSpecFor`；`Reserved = dream, life` 不进 Skill 工具；`app.ownSkills`/`skillSpec`/`skillText`。
  - 测试：`knowledge/knowledge_test.go`；`app/memory_test.go`（记忆说明只在变了时放、想起来的不重复、群里看不到私密、Knowledge 各动作和权限、关掉记忆、agent 技能覆盖、网页接口）。
  - **网页那边要注意**：用户消息前面现在可能有 `[记忆]…[/记忆]`、`[想起来的]…[/想起来的]` 两个多行块（在 `[系统]` 环境说明之后、时间戳之前），`web/src/lib/format.ts` 的 `parseUser` 要把它们拆出来折叠显示（E 阶段做）。
- [ ] **B 做梦**：`Diary` 工具、`dream` 会话、触发（idle/nightly/sleep/off、每天上限）、`dream-state.json`、梦境日志、自带 `skills/dream/SKILL.md`、`/sleep`、网页「现在整理」。
- [ ] **C 两种类型 + 内心活动**：`style` 字段和设置分组（含修“跟随全局”）、拟人守则、`life` 会话和心跳、`data/life.json`、`Message` 的 `chat:` 和不追着发、近况说明、自带 `skills/life/SKILL.md`、`/wake`。
- [ ] **D 统一聊天循环**：门槛 + 退避、工具型群里不插话、私聊拆条和 `[不回复]`、`FollowUp`、房间里 agent @ agent、房间发言计额度。
- [ ] **E 网页**：agent 面板、记忆节点、只读会话、拓扑状态；`check-web-ui.py`。
- [ ] **F 收尾**：README、HANDOFF、ARCHITECTURE、e2e、`make test`、CI 全绿；等用户同意合并发布 v0.6.0。

## 接手须知

- 先读本文件，再读 [HANDOFF.md](HANDOFF.md) 的「用户的偏好和约定」「坑」和 [PLAN-roadmap.md](PLAN-roadmap.md) 的原则。
- 网关调度都在 `gateway/internal/app`，持有 `a.mu` 的函数注释里写了“调用方持有锁”。新的一轮怎么开参考 `launchSchedule`（定时任务）和 `groupTick`（群里插话）：往 `s.queue` 放一条 `queued` 再 `startRun`；群聊触发用 `trigger: true`（内容已在历史里）。
- 网关执行的工具在 `exec.go` 的 `execTool` 分发，工具定义在 `gateway_tools.go`、`search.go`，给模型的工具列表在 `context.go` 的 `contextParts`（群会话走群聊白名单）。
- 不要碰用户真实的 `~/.nano-agent`、真实的网关服务和机器人；测试用 mock 模型。
- 大版本只在 `dev/v0.6` 上提交；合并回 main、打标签发布都要用户同意。
