# 交接文档

给接手这个项目的人（或者下一个 AI 会话）看的：项目现在是什么样、为什么这样设计、做到哪儿了、接下来做什么、有哪些坑。
用户说明见 [README](../README.md)，内核和网关之间的协议见 [protocol.md](protocol.md)。

最后更新：2026-10-21，上下文摘要压缩阶段完成。

## 一句话

nano-agent 是作者自己的轻量 bot 智能体：**C 内核**（agent 循环、权限、沙箱里执行工具，没有状态）+ **Go 网关**（上下文、模型、聊天平台、网页界面，唯一有状态的中心）。网关可以同时管理好几个内核（节点），内核还能通过 SSH 在服务器上执行工具（服务器上不跑内核）。

## 目录

```
kernel/                C 内核和执行器
  src/main.c           内核：poll 主循环，连网关、派发工具、列目录
  src/core.c/h         循环状态机（sans-I/O）：预算、权限模式、审批
  src/runner.c/h       子进程任务（执行器、ssh、上传）
  src/remote.c/h       SSH 远程执行：准备服务器（检查/上传/现编）、拼 ssh 命令
  src/skills.c/h       技能目录扫描、SKILL.md 解析、工具定义和按需加载
  src/conf.c/h         kernel.conf 解析（含 remote = …）
  src/nexec_*.c        执行器 nano-exec：每次工具调用启动一次，套上 Landlock + seccomp 再执行
  src/sandbox.c        Landlock / seccomp
  tests/               test_core.c（状态机）、test_exec.py（执行器）、test_host.py（假网关驱动真内核，含假 ssh）
gateway/               Go 网关
  cmd/nano-gateway     入口；chat.go 是 `nano-gateway chat` 终端客户端
  internal/app         调度：会话、窗口（endpoints）、节点（nodes）、补聊天记录（recap）、命令
    compact.go         超限后异步生成摘要，取消、失败裁剪和用量统计
  internal/channel     聊天渠道：cli（unix socket）、web、telegram、feishu（含扫码创建）
    stream.go          平台流式显示：后台合并、限频、结束补齐、失败时发送完整文本
  internal/kernel      内核连接（hub，多节点）
  internal/session     历史（JSONL 只追加）、会话设置、裁剪
  internal/llm         OpenAI 兼容接口（流式）、mock
  internal/config      配置：gateway.toml + data/config.json（网页改的），schema 驱动设置页
  internal/web         网页接口 + 嵌入的前端（dist/）
  web/                 前端源码：Svelte 5 + Vite（make web 构建到 internal/web/dist）
scripts/               run-local.sh（make run）、e2e.sh
docs/                  protocol.md、本文件
```

## 怎么跑、怎么测

```sh
make run          # 编译，后台起内核、前台起网关（Ctrl-C 停）；打印网页登录链接
make chat         # 另一个终端：终端聊天（nano-gateway chat），随时进出
make test         # C 测试（ASan）+ Go 测试 + e2e（mock 模型，真实内核/执行器/网关）
make web          # 改了前端后重新构建（npm run check && build），产物要提交
cd gateway && go test -race ./...
```

- e2e（scripts/e2e.sh）现在 39 项：终端聊天、工具、审批、模式、沙箱、会话切换、两个内核替补、SSH 服务器（假 ssh）、终端重连、本机与远程技能，以及超限摘要、保存和继续用工具。
- 网页的界面检查用 headless Chromium + CDP 脚本截图（在之前会话的 scratchpad 里，不在仓库里）。演示环境用 mock 模型，别连真模型、别动用户的 data/。
- 本机是 Fedora 43（WSL），glibc 2.42，**不能静态链接**（没装 glibc-static），所以 `nano-exec-static` 不会生成。

## 已经完成的（按提交）

| 提交 | 内容 |
|---|---|
| b924520 | 初版：C 内核 + Go 网关 + 协议 |
| b97b56c | 内置工具 Read/Write/Edit/Grep/Shell；沙箱执行器（Landlock + seccomp）；权限模式 auto/ask/readonly/none |
| 2bab4a0 | 流式输出、思考强度（只传 OpenAI 风格的 reasoning_effort）、前缀缓存优先的上下文 |
| 1741bed | 网页管理界面：拓扑图、点节点改设置、Ctrl+K、审批、人格 |
| 60d2c8a | 目录浏览、飞书扫码创建机器人、没有工作目录就只聊天、默认代理、只支持私聊、拖动节点 |
| 2c8dfa0 / 8883a05 | 会话和聊天窗口分开：所有窗口能接入同一个会话（主会话 main），/session |
| a8bf7b6 | 不是管理员时说明原因；网页里一键设为管理员 |
| f08e92d | 切换会话/回来时补上没看到的聊天记录（/history）；网关和终端聊天分开（make chat） |
| 131e2e2 | 多个内核（节点）：会话绑定机器，掉线时替补只聊天，/node |
| b3c1929 | SSH 远程执行：服务器作为节点（经由某个内核），自动上传/现编执行器 |
| fb07909 | SKILL.md 按需加载、技能热更新、网页列表、SSH 附带文件上传与只读访问 |
| 8c2e48f | Telegram 发送后编辑、飞书 CardKit 流式卡片，含思考/工具/用量、长回复分段和失败降级 |
| 摘要压缩阶段 | 超限时合并旧摘要和旧对话，完整保留近期工具链与当前轮；支持取消、失败裁剪、重启恢复和统计 |

## 关键设计（改之前先理解）

1. **前缀缓存优先**：系统提示词和工具定义固定不变；历史只追加；时间、来源（`[2026-10-21 周三 15:08 · Telegram]`）、环境变化（`[系统] 在 X 上工作。工作目录：…`、替补说明）都追加在用户消息前面，**变了才追加**（Settings.EnvNote 记着上次说的）。历史太长时一次裁掉一大段（滞回），之后又只追加。任何会改动前面字节的改动都是缓存回归。
2. **网关是唯一有状态的中心，内核无状态**。会话 = 一段对话（上下文、工作目录、模式、思考强度、人格、机器）；窗口（endpoint）= 终端、某个私聊，接在会话上（data/endpoints.json）。管理员的窗口默认接主会话；别人各自一个会话。
3. **节点**：每个内核是一个节点（按 hello 的 node 区分）；内核能 SSH 到的服务器也是节点（经由那个内核）。每个会话在一台机器上工作（Settings.Node → workspace.node → 和网关同一台机器的内核）。每一轮 `pick()`：会话的机器能到就用；到不了就替补（本机优先，其次最近连上的），**只聊天**并说明原因。默认工作目录只属于默认机器。
4. **沙箱**：执行器每次调用启动一次，Landlock（工作目录读写、系统目录只读、HOME 其他地方不可见）+ seccomp（禁 AF_UNIX、io_uring 等）+ 清理环境变量。工作目录不能包含 nano-agent 自己（内核、网关、执行器各自把关）。没有沙箱的机器，自动模式下 Shell 也要审批（按每一轮实际执行的机器算）。
5. **SSH 远程**：服务器上不跑内核。内核后台准备执行器（检查 → 上传 → 现编），工具调用是 `ssh … 目标 sh -c 'exec 执行器'`，ControlMaster 复用，关 stdin 取消。执行器放在服务器 `~/.cache/nano-agent/nano-exec-<指纹>`。
6. **安全**：网页只听本机、密码登录、HttpOnly SameSite=Strict、写操作要 X-Nano 头、本机时查 Host；终端聊天走数据目录里只有自己能连的 unix socket（0600 + SO_PEERCRED）；内核连接用 token（**目前所有内核共用一个 token，连接不加密**，跨网络要走 Tailscale/WireGuard/SSH 隧道）。
7. **技能**：由内核管理，默认目录是 kernel.conf 旁的 skills/（skills 配置可改）。名字和简介放在 Skill 工具定义里，正文调用时读。下一轮开始和空闲每 30 秒扫描；元数据变化用 tools 消息同步网关和网页。正文、附带文件更新不改变工具定义。SSH 调用 Skill 时把所选技能打包上传到服务器缓存，后续 Read/Shell 在沙箱里只读访问。沿用 runner 的异步执行和取消，不另起技能服务。
8. **聊天平台流式显示**：RunView 只追加到内存，streamRun 的后台协程合并更新，Close 不阻塞调度锁，但会补齐尾部并关闭卡片。Telegram 每秒编辑消息，飞书每半秒用 CardKit Content 更新卡片，每张卡片的 sequence 严格递增。长回复分段，错误只记录一次，结束时发送完整文本。无正文增量的模型最终结果也会交给 RunView。飞书需要 cardkit:card:write，扫码注册已加入；平台接口测试用 httptest 模拟，无真实凭据的渠道仍需使用时验证。
9. **上下文压缩**：历史超限且有完整旧轮次可切时，额外用当前模型生成摘要。已有 Summary 和被切掉的历史作为 JSON 数据传入，不传工具、人格和推理记录；输出 2048 token，支持 none 时关闭思考，60 秒超时。最近消息和当前轮留在原位置；摘要连同 EnvNote 存在 Settings.Summary，只在压缩时替换，JSONL 不改写。生成失败沿用裁剪，已有摘要不丢；取消或迟到结果不提交，保存失败不推进 Base。额外调用和 token 都计入本轮及全局统计。

## 用户的偏好和约定（重要）

- 中文交流；界面、日志、文档都是中文。用户喜欢“有创意、好看、好用”的网页，不要填表式的设置；拓扑图节点能拖动。
- 先把项目主体功能搭好，每完成一个阶段提交；实现保持直接，做合适的错误处理，不堆防御性代码，体验优化留到实际使用中推进。
- 思考强度只用 OpenAI 风格的 `reasoning_effort`（用户的模型接受 none/low/high/xhigh/max，不接受 medium/minimal/off）。切换思考强度导致缓存失效是 vLLM 的问题，**不要提示**。
- 目前只支持私聊，群聊以后做。
- 提交：分支 main，作者 `tofu-zyc <286995380+tofu-zyc@users.noreply.github.com>`，每完成一个阶段提交一次，Co-Authored-By 记录实际参与者。用户说过“提交，然后开始下一步即可”。
- **密钥**：用户的 API key 只在本地 gateway.toml（gitignore，权限 600）。提交前用 `git grep --cached` 查一遍。不要删用户的 data/。工作目录不能包含 nano-agent 自己。
- 用户的模型：自部署的 OpenAI 兼容接口（deepseek-v4.1-flash，1M 上下文），地址在 gateway.toml。
- 杀进程前确认 /proc/PID/cwd 是演示目录，用户自己可能开着网关（cwd 是项目目录）。

## 接下来要做的（用户要求先把主体功能做齐）

1. **长期记忆**：在会话摘要之外，补充跨会话保留的用户偏好和项目事实；保持前缀稳定，不把每次检索结果塞进系统提示词。
2. 群聊、每台机器单独的 token、审批按钮、Telegram / 飞书真机验证（本轮平台接口用模拟服务测试）。

## 坑

- Svelte 5：`(this.live[sid] ??= []).push(x)` 推进的是原始数组，界面看不到，要用 `liveOf()`。
- 拓扑图拖节点时不能捕获指针，否则节点点不开。
- 飞书注册接口返回的是 `expires_in`，SDK 里写的是 `expire_in`，两个都认。
- unix socket 路径最长 107 字节：终端聊天的 socket 路径太长时改放 `$XDG_RUNTIME_DIR`（CLISocket）。
- 测试内核都通过 unix socket 连上来，都算“和网关同一台机器”，替补会选最近连上的。
- 内核日志用 `%.*s` + `utf8_cut` 截断，别用 `%.200s`（会切坏中文）。
- `make` 每次都会尝试静态链接执行器，失败只提示一次（build/.static-tried）。
