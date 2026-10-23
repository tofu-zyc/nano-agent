---
name: gateway-settings
description: 在聊天里查看和修改 nano-agent 网关的设置项：聊天里显示多少过程（思考、工具）、消息带不带时间、访客能用什么、默认代理、模型请求参数等；用户要改某个设置，又没有更专门的技能（模型、机器、搜索、上下文）时使用。
---

# 网关设置

管理员会话有 `Gateway`（查询）和 `GatewayAdmin`（修改，每次审批）。设置项和网页「设置」页是同一份，改完立即生效，存在 `data/config.json`，不用改 `gateway.toml`。

## 先查

`Gateway {"what":"settings"}` 列出全部设置项；加 `"group"` 只看一组：`memory`（上下文与记忆）、`search`（网络搜索）、`llm`（模型请求）、`gateway`（访问控制、消息、网络、更新）、`workspace`、`telegram`、`feishu`、`cli`、`kernel`。每一项有 `key`、`label`、`type`、当前 `value`、`source`（default / file / web / env）、`help`，密钥只有 `is_set`。标着 `"changeable": false` 的在聊天里改不了，`why` 写着原因（要重启才生效的、网页密码和渠道 token 这类密钥），让用户去网页设置页改。

## 再改

```json
{"action":"settings_set","key":"channels.show","value":"详细"}
```

- `value` 按 `type` 写：`bool` 写 true/false，`int`/`float` 写数字，`enum` 只能是 `options` 里的，`list` 写字符串数组。
- `value` 写 `null`：去掉网页里改过的值，恢复成配置文件里的（或默认值）。
- 一次改一项；失败看错误原因，不要反复重试。

## 常改的几项

**聊天里显示多少过程**（Telegram、飞书、终端；网页始终显示全部）：

- `channels.show`：全局默认，`详细`（思考 + 工具 + 正文）、`过程`（工具 + 正文，默认）、`简洁`（只有正文，处理中一行状态）；
- `telegram.show`、`feishu.show`：只改这个机器人，空字符串表示跟随全局；
- 只改当前这个聊天窗口：让用户发 `/show 详细`（`/show default` 恢复跟随默认）。

**其他**：`session.timestamps`（用户消息前加发送时间）、`session.timezone`（时区，比如 `Asia/Shanghai`，定时任务也按它）、`access.guest`（其他人能用的工具：`none` 只聊天，`readonly` 只读）、`network.proxy`（默认代理）、`update.check` / `update.kernels`（检查新版本、内核自动跟网关同版本）。

模型和接口用 `model-setup` 技能，机器和工作目录用 `device-connections`，网络搜索用 `web-search`，上下文整理用 `context-and-history`。
