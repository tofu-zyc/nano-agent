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

**不要用 `settings_set` 改某个 agent 自己的模型或上限**：那是全局默认值，改的是所有没覆盖的 agent。单个 agent 用 `agent_save`。
