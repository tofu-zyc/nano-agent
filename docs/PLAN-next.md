# 下一步计划（用户已确认的方向）

日期：2026-10-22。来源：用户对 e5a3632（权限简化、XDG 安装布局）和 DESIGN-agent-gateway-admin.md 的反馈。
分 4 个阶段做，每个阶段 `make test` 全过后单独提交，并更新 docs/HANDOFF.md。

## 用户的原则（先读，后面的取舍都以这里为准）

1. **安全靠人保证，不靠工具强加限制。** 不加黑名单，不挡 ~/.ssh，不为了“安全”牺牲方便。权限规则只有一条：挂载的工作目录可读写；工作目录外只读；要写工作目录外 → 提权，用户审批。
2. **我们自己保存的密钥不能明文落盘**：模型 API key、SSH 密码、机器人 token、网页密码。
3. **nano-agent 的所有文件放在一处：`~/.nano-agent`**，删掉这个目录就清理干净。
4. 用户要的是：**在任何设备的聊天软件里下指令，AI 把活干完**（包括配置模型、接 SSH、加工作目录），不用守着网关和网页。

## 阶段 1：所有文件统一放到 ~/.nano-agent

替换 e5a3632 的 XDG 布局（~/.local/bin、~/.config/nano-agent、~/.local/share/nano-agent）。用户没装过 XDG 版本，不用从它迁移。

```
~/.nano-agent/
  bin/nano-gateway
  lib/              nano-kernel、nano-exec、nano-exec-src.tar.gz（有静态版也放这里）
  gateway.toml
  personas/  skills/
  data/             会话、models.json、connections.json、workspaces.json、uploads/、日志
  secret.key        阶段 2 的加密密钥
  run/              unix socket、ssh 控制 socket（路径超过 unix socket 107 字节的限制时才退回 $XDG_RUNTIME_DIR）
```

- 网关的 home：`$NANO_HOME`，没设就是 `~/.nano-agent`。`nano-gateway` 和 `nano-gateway chat` 不带 `-c` 时用 `$NANO_HOME/gateway.toml`。gateway.toml 里的相对路径仍然相对它所在的目录。
- `make install` → `scripts/install-gateway.sh`：
  - 新装：从 gateway.example.toml 生成配置（随机 token）。
  - `--migrate`：复制源码目录的 gateway.toml 和 data/（目标 data/ 为空才复制，源文件不动）。监听地址原样保留，已经配对的电脑不用改。
  - 问一次要不要把 `~/.nano-agent/bin` 加进 PATH（~/.profile 加一行带标记的，卸载时删，参考 install-kernel.sh）。
  - **网关装成后台服务**：用户要随时从聊天软件用，网关不能靠一个终端开着。装 systemd 用户服务，挂了自动重启；没有 systemd 时像内核安装脚本那样用后台进程 + ctl 脚本。本机内核仍由网关自己启动。
  - `--uninstall`：停服务，删服务文件和 ~/.profile 那一行，删 ~/.nano-agent（data/ 里有会话记录，删之前要确认）。
- 其他机器也统一到 ~/.nano-agent：
  - 内核安装脚本：装到 `~/.nano-agent/kernel/`（原来是 ~/.local/share/nano-agent/kernel/）。升级时发现旧位置，就把 kernel.conf（里面有配对口令）挪过来，删旧目录，改服务文件。用户 WSL 里已经按旧位置装了一个内核（tofu-windows），**升级后必须不用重新配对**。
  - SSH 服务器上的执行器：`~/.nano-agent/nano-exec-<版本>`（原来在 ~/.cache/nano-agent/）。部署新版本后删掉旧的 ~/.cache/nano-agent。
  - 技能附带文件：`~/.nano-agent/skills/<名字>/`（原来是 ~/.cache/nano-agent/skills/）。要一起改的有：skills.CacheDir、kernel/src/main.c 的 skill_put 根目录、app/exec.go 的说明、proto 注释、e2e。“内核只肯写这个目录”的约束保留。
  - nexec_main.c 里执行器设置的 XDG_CACHE_HOME 和临时目录也看一下，能放到 ~/.nano-agent 下的就放过去。
  - 技能路径变了，所以协议升到 v6。旧内核被拒时提示重新运行安装脚本（沿用现有文案）。
- “不能当工作目录”的检查（host/local.go 的 guard，kernel 的 holds_self）改成以 ~/.nano-agent 为准。这样 `~` 本身就不能挂成工作目录，因为挂上了 AI 就能改坏网关自己的配置和数据，正是 openclaw 那种问题，所以这条保留。~ 下面的子目录都可以挂。
- 找执行器和本机内核（host.FindExecutor、cmd/nano-gateway/localkernel.go）按新布局在 lib/ 里找。
- `make run`（开发用）可以继续用源码目录的 gateway.toml 和 data/，同时支持 `NANO_HOME=…`。日常用安装版，源码目录可以当工作目录。
- 用 `grep -rn 'cache/nano-agent\|share/nano-agent\|\.config/nano-agent\|XDG_RUNTIME'` 把散落的路径找全。
- 文档跟着改：README（安装、卸载、目录说明）、HANDOFF、ARCHITECTURE、gateway.example.toml。

## 阶段 2：密钥不明文落盘 + 可选脱敏

### 加密存储

- `~/.nano-agent/secret.key`：第一次启动时生成，32 字节随机数，权限 0600。
- 我们保存的密钥字段都存成 `enc:v1:<base64(nonce|密文)>`，用 AES-256-GCM（Go 标准库）：
  - data/models.json 的 api_key；
  - data/connections.json 的 SSH 密码；
  - data/config.json（网页改过的配置）里 schema 中 Type 为 `"secret"` 的字段：现在有 web.password、kernel.token、telegram.token、feishu.app_secret，以 schema 为准，别漏。
- 读的时候解密，内存里是明文。网页接口照旧只回传“有没有设置”。
- 旧文件里的明文：models.json、connections.json、config.json 是我们自己写的 JSON，启动时直接加密后重写。
- gateway.toml 里手写的明文密钥：启动时挪进 data/config.json（加密），再把 gateway.toml 里那一行改成 `key = ""  # 已加密保存在 data/config.json，在网页设置里改`。
  - 只改那一行，保留注释和格式。
  - 改完重新解析一遍，确认结果没变；不一致就不改，只在日志里警告。
  - 不留含明文的备份文件。
- 要跟用户说清楚这能防什么：防的是明文出现在文件里（AI 读文件、截图、备份、误传）；防不了有这个账号权限的人，因为密钥文件就在同一块盘上。这符合“安全靠人”，不要为了防这个再加限制。
- 测试：加解密往返、旧明文迁移、gateway.toml 单行改写、密钥文件丢失时报清楚的错。

### 可选脱敏（用户提的）

- 配置 `[security] redact`，取值 `"off"`、`"public"`（默认）或 `"always"`，设置页里能改。
- 只处理工具结果（Read / Grep / Shell 的输出等）。用户自己发的消息不改。
- 怎么识别：
  1. 网关自己知道的密钥原文，也就是上面加密存储的那些，精确匹配。这一条最可靠。
  2. 常见特征：PEM 私钥块（`BEGIN … PRIVATE KEY`）、`sk-…`、`ghp_` / `github_pat_`、`AKIA…`、`xox[bp]-…`、JWT，以及引号里的 `password / token / secret / api_key = 长字符串`（至少 16 个字符、没有空格）。
  
  宁可漏掉，也不要误伤普通代码。匹配到的值保留前 4 位和后 4 位，中间换成 `*`；私钥块就整块中间打星。
- 什么时候生效：
  - **发给模型时**：`always` 都脱敏。`public` 只在这一轮模型的接口不在本机或内网时脱敏，按解析出的 IP 判断：loopback、10/8、172.16/12、192.168/16、100.64/10、fc00::/7 算内网。脱敏在组请求时做（BuildMessages 那一侧），历史文件存原文。**同一段内容每次脱敏的结果必须一样**，否则前缀缓存会失效。
  - **发到 Telegram / 飞书的过程卡片时**：只要不是 off 就脱敏，因为这两个本来就是公网服务。网页在本机，显示原文。
- 测试：三种模式、内网和公网的判断、结果是否确定。

## 阶段 3：权限 = 工作目录读写，外面只读，写外面要提权

现状（e5a3632）：
- 管理员可以读整个文件系统（ro="/"），只能写工作目录和临时、缓存目录。
- 没有工作目录的管理员被强制改成 readonly 模式，只有 Read / Grep。
- 访客只能读工作目录。

要改成：

- **规则只有一条**：挂载的工作目录可读写；工作目录外只读（管理员）；写工作目录外 → 提权，每次都要用户审批。
- **提权**：Write、Edit、Shell 加一个布尔参数 `elevated`。
  - 参数描述写清楚：要写工作目录外的文件，或者命令需要写工作目录外时设为 true，会请用户批准。
  - 内核的 decide()：带 elevated 的调用在任何模式下都要审批（auto 也要）；readonly、none 模式直接拒绝。
  - 访客不能提权：网关在 input 里告诉内核（比如 `elevate: false`），内核拒绝。
  - 批准后，这一次调用的沙箱可写范围放开到整个文件系统（rw "/"，也就是当前用户能写的地方）。seccomp 和环境变量清理照旧。
  - 网关代执行的情况（exec.go 生成的 spec：SSH 服务器、别的电脑）也一样处理，协议里的 exec / run 带上 elevated。
  - 不带 elevated 写外面失败时，错误信息里要告诉模型“写工作目录外需要 elevated: true，会请用户批准”，让它能自己改正。
  - 审批卡片（网页、Telegram、飞书）上醒目地标出“提权”，以及要写的路径或要执行的命令。
- **没有工作目录时**：
  - 不再强制 readonly 模式，按会话自己的模式来。
  - Read、Grep、Shell 都给。Shell 的当前目录是 HOME，只能写临时目录；要写文件就走提权。
  - 访客没有工作目录时仍然没有工具。
  - readOnlyNote、envNote、describeMounts 和界面上“只读”的文案跟着改。
- **工具描述**：现在 Read / Grep 写的是“能读当前用户能读的文件”，对访客不成立。改成中性的说法，或者在访客的 envNote 里说明只能读工作目录。工具描述必须固定（前缀缓存），不要按会话换描述。
- 测试：
  - test_core：elevated 在 auto 下也要审批；readonly 拒绝；访客拒绝。
  - test_exec：带 elevated 能写外面，不带不能。
  - test_host。
  - e2e：审批后写工作目录外成功；拒绝后没写；没有工作目录时 Shell 能读不能写。

## 阶段 4：AI 帮用户配置网关（自举）

用户的结论：需要 AI 改的是模型、SSH 连接、工作目录这些，不是监听地址；要能在聊天软件里让 AI 做完，不用守着网页。改配置走网关提供的工具，不让 AI 去改 gateway.toml 或 data/*.json：运行中的网关不会重新读这些文件，而且会绕过校验。

- **两个工具**，只给管理员会话。描述和 schema 由网关给（像 Skill 那样：内核的 BUILTINS 里只有名字和类型，调用时用 exec 交给网关）。
  - **`Gateway`**（查询，不审批）：
    - 网关状态、内核和机器；
    - 模型和接口（不含 key）；
    - SSH 连接（不含密码）；
    - 工作目录，以及本会话挂了哪些；
    - 等待确认的电脑配对；
    - ~/.ssh/config 里的 Host 别名。
  - **`GatewayAdmin`**（修改，任何模式都要审批，等于提权）：
    - 模型：
      - 接口的增、改、删（name、base_url、api_key、proxy）；
      - 从接口拉模型列表；
      - 模型的增、改、删，字段和网页的模型面板一样（上下文窗口、最大输出、思考强度、能不能收图片等）；
      - 设默认模型、测试模型、切换本会话的模型。
    - SSH：
      - 增、改、删，连接、断开；
      - `ssh_setup` 一步完成：保存 → 连接 → 等到在线 → 可选添加并挂载一个工作目录。返回真实状态：在线，或者认证失败、超时，并附上原因。
    - 工作目录：添加、删除，挂到本会话或取下，设为默认。
    - 电脑配对：允许、拒绝、移除。现在只能在网页上点，聊天里也要能做。
- **内核**：新增一个工具类型（比如 NA_TOOL_ADMIN），decide() 里任何模式都 ASK，readonly / none 模式拒绝。网关执行时再查一次 isAdmin。旧内核不认识这个类型时，网关不提供这两个工具（靠 hello 里报能力或协议版本判断）。
- **实现**：
  - 直接复用网页接口背后的函数：models.Store、host.Manager、AddWorkspace、setSessionWorkspaces、配对。
  - 所有修改实时生效，不用重启网关，这就避开了 openclaw “改坏配置后重启失败”的问题。监听地址、网页密码、渠道 token、内核口令这类要重启才生效的配置不给 AI 改。
  - 密钥：用户会直接在聊天里把 API key、SSH 密码告诉 AI，这是用户的选择，允许作为工具参数传。但审批卡片上要脱敏显示，工具结果里不回显，存储走阶段 2 的加密。
  - 耗时的动作在 goroutine 里做，不要拿着 App 锁等 SSH。
  - 新挂载的工作目录最好这一轮就能用（网关给内核发更新）。做不到的话，结果里说清楚“下一条消息生效”。
  - 写审计日志：谁、哪个会话、什么动作、目标、结果，不记密钥。
- **技能**：改写 skills/device-connections。先用 Gateway 查真实状态，再用 GatewayAdmin 动手；不要让用户去网页点；失败了不要反复重试同一个动作。可以再加一个配置模型的技能，讲怎么从接口拉列表、上下文窗口怎么填等。
- **设计稿**：按这里重写 docs/DESIGN-agent-gateway-admin.md。原稿第 8 节三个问题，用户的回答是：
  1. 不需要审批卡片里的密码框，密钥可以在聊天里给；
  2. 第一版就包含模型、SSH、工作目录和配对；
  3. 每个修改都要审批。
- **测试**：
  - 非管理员看不到、也调不了；
  - auto 模式下 GatewayAdmin 也要审批；
  - 结果和日志里没有密钥；
  - 用假 ssh 跑通 ssh_setup 的在线、认证失败、超时三种情况；
  - e2e：`!call GatewayAdmin …` → 审批 → 生效。

## 约定（照旧）

- 用中文。实现保持直接，不堆防御代码。
- 每个阶段都要 `make test` 全过（C 带 ASan、Go -race、e2e）。改了前端要 `make web`，把 dist 一起提交。
- 提交：在 main 分支，用 `git -c user.name=tofu-zyc -c user.email=286995380+tofu-zyc@users.noreply.github.com`，Co-Authored-By 写实际参与的人。提交前用 `git grep --cached -F -q` 确认本地 gateway.toml 和 data/models.json 里的密钥没进暂存区，查的时候不要把密钥打印出来。
- 不要删、不要改用户的 data/ 和 gateway.toml（迁移只做复制）。
- 不要用用户的真实配置启动网关，那会连上飞书机器人和真实模型。演示用临时目录加 mock 模型。杀进程前确认 /proc/PID/cwd 是你自己的演示目录。
- 前缀缓存：系统提示词和工具定义固定不变，历史只追加，环境变化追加在用户消息前面。
