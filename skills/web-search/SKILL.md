---
name: web-search
description: 用 WebSearch / WebFetch 上网查资料，以及在聊天里开关和配置网络搜索（DeepSeek key、默认条数、费用）；用户要查最新的信息、读某个网页，或者问搜索为什么用不了时使用。
---

# 网络搜索

搜索由网关调用 DeepSeek 官方接口，不经过任何机器，所以没挂工作目录、只读模式、访客会话里也能用。

## 怎么搜

- `WebSearch {"query":"…","count":6}`：返回带编号的标题、链接、日期、站点和一段摘要。`count` 1–10，不写用设置里的默认值（一般是 6）。
  - 搜索词 1–128 个字。写成搜索引擎能懂的关键词，不要整句复述用户的话；要限定网站可以加 `site:github.com` 这类运算符。
  - 问的是最近的事，在词里带上年份或月份。
- `WebFetch {"url":"…"}`：把网页读成 Markdown，每次最多 20000 字；结果末尾写着“还有 N 字，用 offset=… 继续读”时，用同一个 `url` 加 `offset` 接着读。10 分钟内重复读同一个网页不再收费。
- 先搜，挑一两条最相关的再 `WebFetch` 读原文；摘要够回答就不用读。
- **回答时附上来源链接**（写在相关句子后面或者结尾列出来）。搜不到就说搜不到，不要编。

## 每一次都花钱

按次计费：每千次成功请求 5 元（一次 0.005 元），空结果和失败不收费。不要为了“保险”连搜很多次；同一个问题换两三次关键词还没有，就告诉用户。

## 用不了的时候

先查：`Gateway {"what":"search"}`，看 `enabled`（开没开）、`key`（`settings` 是设置里填的，`endpoint:<名字>` 是借用了那个模型接口的 key，空表示没有可用的 key）、`today`（今天搜了几次、读了几次网页）。

- 没开：`GatewayAdmin {"action":"settings_set","key":"search.enabled","value":true}`。
- 没有 key：让用户给一个 DeepSeek API key，再 `GatewayAdmin {"action":"settings_set","key":"search.api_key","value":"sk-…"}`（加密保存，审批卡片上打星）。不填 key 时，网关会借用模型接口里地址是 `api.deepseek.com` 的那个 key。
- 改默认条数：`settings_set` 改 `search.count`（1–10）。
- 报错 401：key 不对；402：DeepSeek 账户余额不足，要去充值；429：太快了，等一会儿再试。
- 刚开好或刚填 key 时，**已经在聊的会话里还没有这两个工具**（工具定义在新会话、`/reset` 或整理上下文之后才刷新）。告诉用户发 `/reset` 或开个新会话再试，不要以为没配好。

每次 `GatewayAdmin` 都要管理员审批。不要把 key 原样写进给用户看的回复。
