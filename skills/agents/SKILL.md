---
name: agents
description: 查看和管理多个 agent 和它们绑定的机器人：谁是谁、各自用什么模型和人格、每个人绑了哪个机器人、给某个 agent 改设置或新建一个。用户问“有几个 agent”“让小鲍用另一个模型”“给这个 agent 绑个 Telegram”时使用。
---

# agent 和机器人

nano-agent 里可以有多个 **agent**：每个是一直在的个体，有自己的名字、头像、人格（`agents/<id>/persona.md`）、模型和会话。会话 ID 默认 agent 是 `main`、`tg:123`，别的 agent 前面加前缀，比如 `@bob/main`。

**机器人**（Telegram、飞书）绑在 agent 上：一个机器人只绑一个 agent，它上面收到的消息都归那个 agent。同一个 agent 可以绑好几个机器人。内置的两个机器人 ID 是 `tg` 和 `feishu`，它们的凭据在网关配置里（网页的 Telegram / 飞书节点）；另外加的机器人凭据存在 `data/bots.json`（加密）。

## 先查

```json
{"what":"agents"}
```

返回 `agents` 和 `bots` 两份：

- 每个 agent：`id`、`name`、`emoji`、`default`（是不是默认的）、`model`、`model_label`、`thinking`、`sessions`（几个会话）、`bots`（绑了哪些机器人）、`persona_file`、`settings`（能覆盖的全局设置：每项有 `global` 全局值、`value` 它自己的值（`null` 表示跟随全局）、`effective` 实际生效的值）。
- 每个机器人：`id`、`kind`（telegram / feishu）、`name`、`agent`（绑在谁身上）、`channel`（渠道名，比如 `tg`、`tg.bob`）、`enabled`、`legacy`（内置的）。

## 再改（每次审批）

```json
{"action":"agent_save","name":"小鲍","emoji":"🦦"}
```

- `name` 是新的 agent 的名字，ID 会自动起（小写字母、数字、-）。
- 改已有的：带上 `"agent":"bob"`，再加要改的字段（`name`、`emoji`、`model`、`thinking`）。
- 改它覆盖的设置：`"settings":{"max_steps":50,"mode":"ask"}`；写 `null` 恢复跟随全局（可覆盖的键：`max_steps`、`run_timeout`、`approval_timeout`、`max_output_kb`、`mode`、`compact_at`、`compact_to`、`keep_turns`、`idle_compact`、`idle_compact_min`）。
- 删掉：`{"action":"agent_delete","id":"bob"}`。默认 agent 不能删；还绑着机器人的 agent 要先换绑。
- 绑机器人：`{"action":"bot_bind","id":"tg","agent":"bob"}`。换绑以后新消息进新 agent 的会话，以前的会话留在原来的 agent 下。

## 网页上

拓扑图的 Agent 列里，每个 agent 是一个节点：点开能改它的人格、模型、思考强度、覆盖的设置、工具白名单，能看它绑了哪些机器人、能加一个机器人。下面「Agent 默认值」是所有 agent 没覆盖的项（每条消息的上限、管理员、访客、显示档位、时区），「添加 agent」新建一个。机器人都是「入口」列上的节点。

## 群聊

机器人在群里（Telegram、飞书）不是每句都回：被 @、回复它、叫到名字才回；群里安静够了、额度够的时候可以自己插一句，不想说就只回 `[不回复]`。能改的：

- `group_eagerness`：`off` 只回被叫到的；`low`（默认）每小时最多主动说 2 次；`normal` 6 次；`high` 15 次。
- `group_quiet`：群里安静多久才可能插话，默认 `20s`；`group_min_gap`：两次发言最短间隔，默认 `10s`。
- `group_max_len`：一条回复最多多少字，默认 300；`group_parts`：最多拆成几条发，默认 3。
- `group_tools`：群里能用的工具，默认只有搜索和技能；群里永远看不到文件、命令这些。

群聊的系统提示是「人格 + 群聊守则」，守则在 `agents/<id>/group.md`，没有就用内置的。用户想在群里让它更活泼或者更安静，改 `group_eagerness`；想改说话方式，让他去网页那个 agent 节点右边写 `group.md`（目前网页上只能改人格，`group.md` 要他直接写文件）。

## 群和私聊

网关里可以建群（房间）：把几个 agent 放进同一个群，它们在群里像同事一样说话，人在网页的会话页里也能发言。

- 查有哪些群、谁在里面：`Gateway {"what":"agents"}` 现在也带着 agents 和 bots；群的信息用网页的房间节点看。
- 给群或某个 agent 发消息：用 `Message` 工具 —— `{"to":"room:产品组","text":"……"}`、`{"to":"agent:小鲍","text":"……","wait":true}`（等对方回最多 5 分钟）。
- 群里的说话规矩和外部群聊一样（短、口语、不发思考），点名模式只有被叫到才回。

**不要用 `settings_set` 改某个 agent 自己的模型或上限**：那是全局默认值，改的是所有没覆盖的 agent。单个 agent 用 `agent_save`。
