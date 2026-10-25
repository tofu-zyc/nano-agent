---
name: mcp
description: 在 nano-agent 里查看、添加、修改、删除 MCP 服务——给模型接外部工具（文件系统、浏览器、数据库、computer use……），也可以让服务跑在某台接入的电脑上。用户说“加个 MCP”“接一下某某的 MCP 服务”“让你能操作我那台电脑的浏览器”时使用。
---

# 配置 MCP 服务

管理员会话有 `Gateway`（查询）和 `GatewayAdmin`（修改，每次要管理员批准，放行模式除外）。
网页拓扑图「能力和内核」一列的「MCP 服务」节点能做同样的事，用户也可以自己去那里改。

## 先查

```json
{"what":"mcp"}
```

返回：

- `servers`：每个服务的配置（`command` / `args` 或 `url`、`machine`、`approval`、`tools`、`agents`、`enabled`）、
  `online`（现在连着没有）、`tools_listed`（上次连上时列到的工具，名字就是交给模型的 `服务名__工具名`）、
  `env_keys`（环境变量只给名字，值加密存着，你看不到也不需要看）。
- `machines`：能挂服务的在线电脑（`name`、`os`；`local` 表示和网关同一台；`too_old` 的内核还不能跑 MCP，等它自动更新完）。

改之前先查，别重复加同一个服务。

## 加一个

`name` 只能用字母、数字、下划线、减号，它也是工具名的前缀，取短一点、有意义（`fs`、`browser`、`github`）。

在网关本机起一个进程（stdio）：

```json
{"action":"mcp_save","name":"fs","command":"npx","args":["-y","@modelcontextprotocol/server-filesystem","/home/me/docs"]}
```

连远程地址（streamable HTTP）：

```json
{"action":"mcp_save","name":"remote","url":"https://example.com/mcp"}
```

挂到别的电脑上跑（`machine` 写 `machines` 里的 `name`）——浏览器、computer use、桌面软件这类要放在有桌面的那台：

```json
{"action":"mcp_save","name":"browser","machine":"my-laptop","command":"npx","args":["-y","@playwright/mcp@latest"]}
```

- `args` 一项一个；路径里有空格也不用加引号。路径按那台机器的写法（Windows 写 `C:\\Users\\me\\docs`）。
- Windows 上直接写 `npx`、`uvx` 就行（内核会找到 `npx.cmd`）。那台电脑上得装着对应的运行环境（Node.js、uv、Python）。
- **密钥放 `env`，别放 `args`**：`env` 加密保存、查的时候只给名字；`args` 是明文，网页上看得到。
  用户把 token 发给你的话，写进 `env` 就行，回复里不要复述它。
- 不确定包名、启动参数、需要哪些环境变量时，先用 `WebSearch` 查这个服务的官方说明，别编。

## 加完一定测一次

```json
{"action":"mcp_test","name":"fs"}
```

连上了会列出工具。第一次用 `npx` / `uvx` 要下载，可能要等几十秒。连不上的常见原因：命令不存在（没装 Node.js 或 uv）、
包名或参数写错、缺环境变量、那台电脑不在线（`machines` 里没有它）。按报错改了再测，同样的错别反复重试。

测通以后用一两句话告诉用户：服务名、在哪台机器上跑、有哪些工具（挑几个说）、审批是哪一档。
新工具从下一条消息起就能用。

## 改

只写要改的字段，没写的保持原样：

```json
{"action":"mcp_save","name":"fs","approval":"always"}
```

- 改名：`{"action":"mcp_save","id":"旧名","name":"新名"}`，密钥跟着走。
- `env` 只写要加或要改的变量；值写 `""` 表示沿用已存的。
- `tools`：只开放这些工具（服务里的原名）；`["!write_file"]` 表示禁用某一个；`[]` 恢复全部。
- `agents`：只给某几个 agent 用（agent ID，用 `Gateway {"what":"agents"}` 查）；`[]` 表示所有 agent。
- `enabled`：`false` 暂停，`true` 恢复。
- `approval`：调用前要不要管理员批准。`never` 不用；`per_turn` 一轮里第一次要批；`always` 每次都批。
  会改文件、操作电脑、花钱、往外发东西的服务建议 `per_turn` 或 `always`；只读查询类可以 `never`。放行模式下都不问。

## 删

```json
{"action":"mcp_delete","name":"fs"}
```

## 要知道的

- MCP 工具只给管理员的会话；只读模式和不用工具的模式拿不到。
- 服务没连上时，工具定义里仍然有它上次列到的工具，调用时才会报连不上；网关会在后台自动重连。
- 挂在别的电脑上的服务，那台电脑断开时进程会跟着停，它连回来以后网关会自动再起。
