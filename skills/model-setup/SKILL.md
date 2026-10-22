---
name: model-setup
description: 在 nano-agent 里添加或修改模型接口和模型：从 OpenAI 兼容接口拉模型列表、填上下文窗口和最大输出、设置默认模型或切换本会话模型。用户要在聊天里配置模型时使用。
---

# 配置模型

管理员会话有 `Gateway`（查询）和 `GatewayAdmin`（修改，每次审批）。

## 先查

```json
{"what":"models"}
```

返回接口和模型，接口只有 `has_key`，不会返回 key。再需要时用 `Gateway {"what":"ssh"}` 看机器（模型不需要机器）。

## 加接口

```json
{"action":"model_endpoint_save","name":"deepseek","kind":"openai","base_url":"https://api.deepseek.com/v1","api_key":"…"}
```

`api_key` 留空表示不改原来的；接口返回的 ID 在结果里。`proxy` 可以写代理地址，`none` 表示直连。

## 拉模型列表

```json
{"action":"model_fetch","id":"<接口 ID>"}
```

返回接口里的模型名、上下文窗口（如果接口报了）和是否已经添加。

## 加模型

```json
{"action":"model_save","endpoint":"<接口 ID>","model":"deepseek-chat","label":"DeepSeek Chat","context_window":128000,"max_tokens":8192,"modalities":["text"]}
```

- `context_window` 是模型能收的上下文总长度，不确定就按接口文档填；历史上限不填时按它的一半算。
- `max_tokens` 是最大输出；不填表示不限制。
- 能收图片的模型加 `"modalities":["text","image"]`；不确定就不要加。
- `thinking_levels` 是可选思考强度，比如 `["none","high"]`。

然后：

```json
{"action":"model_default","id":"<接口/模型>"}
{"action":"model_use","id":"<接口/模型>"}
```

`model_default` 改所有新会话的默认模型，`model_use` 只改本会话。`model_test` 会真的发一个小请求，`model_delete` 删除模型。

## 规则

- 每次修改都要审批；失败先看错误（地址、key、代理、接口返回），不要连续重试同一个动作。
- 不要把 key 原样复述给用户；审批卡片已经打星，落盘会加密。
- 监听地址、网页密码、渠道 token 这类要重启的配置不归这里管，别去改 `gateway.toml`。