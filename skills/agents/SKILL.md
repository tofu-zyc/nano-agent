---
name: agents
description: 查看和管理多个 agent 和它们绑定的机器人：谁是谁、各自用什么模型和人格、每个人绑了哪个机器人、给某个 agent 改设置或新建一个。用户问“有几个 agent”“让小鲍用另一个模型”“给这个 agent 绑个 Telegram”时使用。
---

# agent 和机器人

nano-agent 里可以有多个 **agent**：每个是一直在的个体，有自己的名字、头像、人格（`agents/<id>/persona.md`）、模型和会话。会话 ID 默认 agent 是 `main`、`tg:123`，别的 agent 前面加前缀，比如 `@bob/main`。

**机器人**（Telegram、飞书、QQ、微信）绑在 agent 上：一个机器人只绑一个 agent，它上面收到的消息都归那个 agent。同一个 agent 可以绑好几个机器人。内置的两个机器人 ID 是 `tg` 和 `feishu`，它们的凭据在网关配置里（网页的 Telegram / 飞书节点）；另外加的机器人凭据存在 `data/bots.json`（加密）。QQ 和微信机器人要在网页上扫码加（「＋ 添加机器人」→ 选 QQ 或微信 → 生成二维码），扫码的人自动成为管理员；聊天里加不了，用户要加时告诉他去网页上扫。微信只能和扫码的人私聊、不能进群；QQ 私聊群聊都行，但隔太久的主动消息可能被 QQ 拦下。

## 先查

```json
{"what":"agents"}
```

返回 `agents` 和 `bots` 两份：

- 每个 agent：`id`、`name`、`emoji`、`default`（是不是默认的）、`style`（`tool` 工具型 / `life` 拟人型）、`model`、`model_label`、`thinking`、`sessions`（几个会话）、`bots`（绑了哪些机器人）、`persona_file`、`memory`（长期记忆：开没开、多少条、多少人、上次整理）、`life`（拟人型的状态：睡着 / 醒着 / 在想事情、下次醒来、今天醒了几次、近况）、`doing`（现在在干嘛，和聊天里 `/state` 是同一套说法：`state` 是 dreaming 整理记忆 / chatting 聊天 / working 干活 / thinking 内心活动 / sleeping 睡着 / slacking 摸鱼 / idle 发呆或闲着，`text` 是给人看的一句话）、`settings`（它自己的设置：每项有 `group` 分组（work 干活 / group 群聊 / life 拟人 / memory 记忆）、`global` 全局值或默认值、`value` 它自己的值（`null` 表示跟随全局或默认）、`effective` 实际生效的值）。
- 每个机器人：`id`、`kind`（telegram / feishu / qq / weixin）、`name`、`agent`（绑在谁身上）、`channel`（渠道名，比如 `tg`、`tg.bob`、`qq.x`、`wx.x`）、`enabled`、`legacy`（内置的）、`owner`（扫码的人，已经是管理员）。

## 再改（每次审批；放行模式下自动批准）

```json
{"action":"agent_save","name":"小鲍","emoji":"🦦"}
```

- `name` 是新的 agent 的名字，ID 会自动起（小写字母、数字、-）。
- 改已有的：带上 `"agent":"bob"`，再加要改的字段（`name`、`emoji`、`model`、`thinking`）。
- 改它覆盖的设置：`"settings":{"max_steps":50,"mode":"ask"}`；写 `null` 恢复跟随全局（可覆盖的键：`max_steps`、`run_timeout`、`approval_timeout`、`max_output_kb`、`mode`、`compact_at`、`compact_to`、`keep_turns`、`idle_compact`、`idle_compact_min`）。`mode` 是新会话的权限模式：`full` 放行（什么都不审批）、`auto`、`ask`、`readonly`；拟人型没设时是 `full`，工具型跟随全局。
- 改类型：`"style":"life"`（拟人型）或 `"tool"`（工具型）。拟人型自己的设置：`life_awake`（醒着的时间，如 `08:00-23:30`）、`life_level`（`low` / `normal` / `high`，多久自己醒一次）、`life_reach`（`owner` 只主动找主人 / `all` 私聊过的人都可能找 / `off` 不主动找人）、`life_budget`（内心活动每天最多用多少 token，0 不限）。
- 长期记忆的设置：`memory`（`on` / `off`）、`dream_when`（什么时候整理记忆：`idle` 空闲时、`nightly` 每天定点、`sleep` 睡着时、`off`）、`dream_at`（定点几点，如 `03:00`）、`dream_idle`（空闲多久，如 `2h`）、`dream_max`（每天最多自动整理几次）、`dream_model`（整理用的模型 ID，空就用自己的）。
- 删掉：`{"action":"agent_delete","id":"bob"}`。默认 agent 不能删；还绑着机器人的 agent 要先换绑。
- 绑机器人：`{"action":"bot_bind","id":"tg","agent":"bob"}`。换绑以后新消息进新 agent 的会话，以前的会话留在原来的 agent 下。

## 网页上

拓扑图的 Agent 列里，每个 agent 是一个节点：点开能改它的人格、模型、思考强度、覆盖的设置、工具白名单，能看它绑了哪些机器人、能加一个机器人。下面「Agent 默认值」是所有 agent 没覆盖的项（每条消息的上限、管理员、访客、显示档位、时区），「添加 agent」新建一个。机器人都是「入口」列上的节点。

## 群聊

机器人在群里（Telegram、飞书、QQ）不是每句都回：被 @、回复它、叫到名字才回。**拟人型**在群里安静够了、额度够的时候可以自己插一句，不想说就只回 `[不回复]`；**工具型**只回被叫到的。能改的：

- `group_eagerness`（只对拟人型有用）：`off` 只回被叫到的；`low`（默认）每小时最多主动说 2 次；`normal` 6 次；`high` 15 次。
- `group_quiet`：群里安静多久才可能插话，默认 `20s`；`group_min_gap`：两次发言最短间隔，默认 `10s`。
- `group_max_len`：一条回复最多多少字，默认 300；`group_parts`：最多拆成几条发，默认 3。
- `group_tools`：群里能用的工具，默认只有搜索和技能；群里永远看不到文件、命令这些。

群聊的系统提示是「人格 + 群聊守则」，守则在 `agents/<id>/group.md`，没有就用内置的。用户想在群里让它更活泼或者更安静，改 `group_eagerness`；想改说话方式，让他去网页那个 agent 节点右边写 `group.md`（目前网页上只能改人格，`group.md` 要他直接写文件）。

## 群和私聊

网关里可以建群（房间）：把几个 agent 放进同一个群，它们在群里像同事一样说话，人在网页的会话页里也能发言。

- 查有哪些群、谁在里面：`Gateway {"what":"agents"}` 现在也带着 agents 和 bots；群的信息用网页的房间节点看。
- 给群或某个 agent 发消息：用 `Message` 工具 —— `{"to":"room:产品组","text":"……"}`、`{"to":"agent:小鲍","text":"……","wait":true}`（等对方回最多 5 分钟）；发到你自己的某个私聊：`{"to":"chat:<会话ID>","text":"……"}`（主人是 `chat:main`，别的 agent 是 `chat:@<id>/main`），对方在聊天软件里收到。
- 群里的说话规矩和外部群聊一样（短、口语、不发思考），点名模式只有被叫到才回。

**不要用 `settings_set` 改某个 agent 自己的模型或上限**：那是全局默认值，改的是所有没覆盖的 agent。单个 agent 用 `agent_save`。

## 长期记忆（v0.6 起）

- 你有自己的知识库（人物画像、事件、事实、偏好、经验、流程、知识、看法），用 `Knowledge` 工具：`search` 搜、`read` 读、`write` 记、`edit` 改、`invalidate` 标过时、`person` 找某个人的画像；`self.md` 是你的自我认知。每次聊天网关会把记忆索引和对方的画像放在消息前面。
- 用户问“你记得……吗”“你都记了我什么”：先 `search` 或 `person`，按记下的回答；记错了用 `edit` 改，用户说“忘掉”就 `invalidate`。
- 主人可以在网页「上下文与记忆」节点里看、改所有记忆，聊天里 `/memory`、`/forget`、`/sleep`（马上整理记忆）。
