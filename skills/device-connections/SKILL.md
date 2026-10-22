---
name: device-connections
description: 了解和排查 nano-agent 的机器与工作目录：SSH 服务器接入、别的电脑装内核和配对、选工作目录、同机 / 异机执行；用户要接入机器或者连接、执行出问题时使用。
---

# 机器、工作目录和接入

你是管理员会话时，有 `Gateway`（查询，不审批）和 `GatewayAdmin`（修改，每次审批）两个工具。**先在聊天里把活干完，不要让用户去网页点。**

## 先查真实状态

- `Gateway {"what":"ssh"}`：SSH 连接、在线状态和错误
- `Gateway {"what":"status"}`：内核、机器、工作目录、默认模型
- `Gateway {"what":"workspaces"}`：工作目录列表和本会话挂了哪些
- `Gateway {"what":"pairing"}`：等待确认的电脑配对、已配对设备
- `Gateway {"what":"ssh_config"}`：`~/.ssh/config` 里的 Host 别名
- `Gateway {"what":"models"}`：模型接口和模型（只有 `has_key`，不返回 key）

## 再接

SSH 一步完成（保存 → 连接 → 等在线 → 可选添加并挂载工作目录）：

```json
{"action":"ssh_setup","name":"gpu","host":"192.168.1.10","user":"root","password":"…","path":"~/proj"}
```

也可以分步：`ssh_save` → `ssh_connect`，失败时用 `ssh_disconnect` / `ssh_delete`。`ssh_setup` 会返回真实状态：在线、认证失败、超时，并附原因。**失败不要反复重试同一个动作**，根据原因改参数或告诉用户。

别的电脑装内核：让用户运行安装脚本；第一次连上后用 `Gateway {"what":"pairing"}` 看确认码，再 `GatewayAdmin {"action":"pair_allow","id":"…"}`。不要让用户去网页核对。

工作目录：`workspace_add`（`host`、`path`、`default`），`workspace_mount`（`id`、`mount`），`workspace_default`。挂载后如果系统消息没更新，说明“下一条消息生效”。

## 规则

- 每次 `GatewayAdmin` 都会请管理员审批；一次只做一个明确动作，失败了不要连续重试。
- 密钥可以直接在聊天里给，但**不要把 API key / 密码原样写进给用户看的总结**；审批卡片已经自动打星。
- 不要为了管理连接去读 `gateway.toml`、`data/*.json`、浏览器 cookie；用 `Gateway` 查，用 `GatewayAdmin` 改。
- 排查顺序：认证失败 → 主机/端口/网络 → 服务器有没有 cc（架构不同要现编）→ 内核连不上网关 / 配对是否被拒 → 工作目录路径和权限。用工具返回的状态和错误定位。