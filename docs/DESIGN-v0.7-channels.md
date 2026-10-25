# v0.7：QQ、微信、放行模式、飞书拟人型回复、/state

分支 `dev/v0.7`。这份文档同时是交接文档：用户要求、调研结论、做了什么、怎么测、还差什么。

## 用户的要求（2026-10-24）

1. 加 QQ 机器人和微信机器人，两边都有扫码创建的方案，先调研。发动态、朋友圈“大概率有点困难，就先以机器人的方式接入”。
2. 拟人型的飞书机器人：回复不渲染 Markdown，要改；加“状态”显示，比如正在回复。
3. 拟人型的工具审批跑到网关网页上很麻烦，“要么就直接放行，不审批”。
4. “为什么 computer-use 是强制审批的？不能直接放行吗？”——**但不能改现有的沙箱规则**：“沙箱肯定是要有的，这本来就是后续需要的，除非是我们新增一个无沙箱的模式”。用户选了：新增“放行”模式（全部放行，包括提权和管理工具），拟人型默认用它；飞书里的审批卡片本来就是正常的。
5. 加一个命令看 agent 现在的状态：睡着了、在活动、在摸鱼、在发呆等等。

## 调研结论

| | QQ | 微信 |
|---|---|---|
| 官方方案 | QQ 开放平台的官方机器人；2026-03 起给 agent 开了“扫码创建/绑定”，SDK `@tencent-connect/qqbot-connector` | 微信 ClawBot（2026-03），iLink 接口，插件 `@tencent-weixin/openclaw-weixin`，协议文档 [protocol_zh_CN.md](https://github.com/Tencent/openclaw-weixin/blob/main/docs/protocol_zh_CN.md) |
| 扫码 | `POST q.qq.com/lite/create_bind_task {key}` → 二维码 `q.qq.com/qqbot/openclaw/connect.html?task_id=…&source=&_wv=2` → `POST /lite/poll_bind_result {task_id}`：status 1 等、2 好了（`bot_appid`、`bot_encrypt_secret` 用 key 做 AES-256-GCM，IV12+密文+tag16；`user_openid`）、3 过期 | `POST ilinkai.weixin.qq.com/ilink/bot/get_bot_qrcode?bot_type=3 {local_token_list}` → `GET get_qrcode_status?qrcode=`（长轮询 35 秒）：wait / scaned / confirmed（`bot_token`、`ilink_bot_id`、`baseurl`、`ilink_user_id`）/ expired / need_verifycode（带 `&verify_code=`）/ verify_code_blocked / scaned_but_redirect（`redirect_host`）/ binded_redirect |
| 收消息 | `POST bots.qq.com/app/getAppAccessToken` → `GET api.sgroup.qq.com/gateway` → WebSocket：HELLO(10) → IDENTIFY(2，`QQBot <token>`，intents `1<<25`) / RESUME(6)，心跳(1)，DISPATCH(0)：`C2C_MESSAGE_CREATE`、`GROUP_AT_MESSAGE_CREATE`、`GROUP_MESSAGE_CREATE`；7 重连，9 会话失效；关闭码 4004 凭证、4006/4007/4009 会话、4914/4915 权限 | `POST ilink/bot/getupdates {get_updates_buf}` 长轮询，`ret/errcode = -14` 是登录失效（官方插件暂停一小时） |
| 发消息 | `POST /v2/users/{openid}/messages`、`/v2/groups/{group_openid}/messages`：`msg_type` 0 文字 / 2 Markdown / 6 正在输入（`input_notify`）；被动回复带 `msg_id`+`msg_seq`，同一条 1 小时内最多 4 次 | `POST ilink/bot/sendmessage`：`message_type 2`、`message_state 2`、`context_token`、`item_list` 文字；`getconfig` 拿 `typing_ticket`，`sendtyping` status 1/2 |
| 限制 | 一个 QQ 号最多 5 个机器人；进群要新版手机 QQ；主动消息会被限量拦截 | 只能和扫码的人私聊，不能进群；只能纯文字；鸿蒙版不支持 |
| 头 | `Authorization: QQBot <token>` | `AuthorizationType: ilink_bot_token`、`Authorization: Bearer`、`X-WECHAT-UIN`（随机 uint32 十进制再 base64）、`iLink-App-Id: bot`、`iLink-App-ClientVersion`（0x00MMNNPP），body 带 `base_info {channel_version, bot_agent}` |

发朋友圈、QQ 空间：官方接口都没有，只有模拟个人号的非官方协议，有封号风险，没做。

## 做了什么

### 放行模式（`full`）

- **内核一行没改**。`proto.ModeFull` 只在网关里：`startRun` 里 `r.full = true`，发给内核的还是 `auto`；内核照样提出审批（没沙箱的 Shell、`elevated`、管理工具），`KernelMsg` 收到 `approve_req` 时 `autoApprove` 当场批准，网页对话里记一笔“✅ 放行模式，自动批准：…”，内核的“已批准，开始执行”提示不往聊天里发。
- 拟人型没单独设置时默认放行（`modeFor`）；工具型跟随全局。`/mode full`、会话面板、agent 设置、`workspace.mode` 都能选。
- 测试：`TestFullModeAutoApproves`。

### 飞书拟人型回复、状态

- 渠道新增两个可选能力（`channel.go`）：`Markdowner`（回复正文按 Markdown 发）和 `Busy`（处理时显示状态，返回撤掉的函数）。
- `deliverReply` / `sendText`：agent 的回复正文（包括拟人型分几条发的）用 `Markdowner`；命令和系统说明仍是纯文本。
- 飞书（`feishu_md.go`）：富文本消息（post）里的 `md` 段落，还是普通聊天气泡；标题改成加粗（md 段落不支持标题）。`Busy` 在用户那条消息上贴 `Typing` 表情，回完删掉。
- `startRun`：没有流式卡片的私聊（拟人型）、渠道有 `Busy` 时用它，贴在这批消息的最后一条上。
- 测试：`TestFeishuMarkdown`、`TestLifeDMMarkdownAndBusy`。

### `/state`

- `doing.go`：`agentDoing` 从作息、正在跑的会话、这一轮最近用的工具（`run.tool`）、多久没人找推出：dreaming（在做梦 / 在整理记忆）、chatting（在跟谁聊天 / 在群里说话）、working（工具型在干活、定时任务）、thinking（内心活动：冲浪 / 找人聊天 / 翻记忆 / 想事情）、sleeping（几点醒）、slacking（醒着、忙完不到 30 分钟：摸鱼）、idle（发呆，超过 3 小时说多久没人找它；工具型是“闲着”）。
- `/state`：所有 agent，当前的排第一；拟人型还有近况、今天醒了几次、在等谁回。访客只看当前 agent 的不带人名的说法。
- 网页：`AgentInfo.doing`，拓扑卡片和 agent 面板用同一套说法；每轮开始、结束时重读 `/api/agents`（防抖 0.8 秒）。
- 测试：`TestDoingToolAgent`、`TestDoingLifeAgent`。

### QQ、微信

- `agents.KindQQ`（渠道 `qq.<id>`，AppID/AppSecret）、`agents.KindWeixin`（渠道 `wx.<id>`，Token=bot_token，APIBase=扫码给的地址，BotID，Owner）。只能另外加，没有 gateway.toml 里的配置段。
- `channel/qq.go`：见上表。聊天 ID：私聊是 `user_openid`，群是 `g.<group_openid>`。被动回复记在 `last[chat]`（每条用一次 +1，满 4 次或过 1 小时就是主动消息）。消息 ID 去重（重连可能重发）。`<@!机器人>` 去掉，@ 别人写成 `@某人`。图片下载存进媒体库，语音用 `asr_refer_text`。Markdown 被拒（4xx）就记下 `noMD`，以后发纯文本。`Typing` 每条消息只发一次 `input_notify`（序号 1000+，和回复的 1–4 错开）。
- `channel/weixin.go`：长轮询，`-14` 时状态写“登录失效了，请在网页上重新扫码”，一小时后再试。图片 AES-128-ECB 解密（`image_item.aeskey` 十六进制优先，`media.aes_key` 是 base64 的 16 字节或 32 位十六进制）。Markdown 转纯文字（`PlainText`）。`Busy` 每 5 秒续一次“正在输入”，结束发 status 2。`StateFile` 存游标和每个人最近的 `context_token`。
- 扫码：`channel.Registration` 接口（`QRURL`、`Wait(ctx, progress)`、`Verify(code)`），进展 `scanned` / `verify` / `verify_wrong` / `refreshed`（新二维码）。QQ、微信过期都自动换新二维码（最多 3 次）。
- 网页接口 `web/botreg.go`：`POST /api/bots/register {kind, id?, name, agent, proxy}` 立即返回二维码，后台等；拿到凭据 `saveScannedBot`：给了 `id`（重新扫码）或同一个 ilink_bot_id 的微信已经在了就更新，不然新建；扫码的人（`<渠道>:<owner>`）写进 `access.admins`。`POST /api/bots/register/verify {code}`、`DELETE /api/bots/register`。事件 `bot_reg`。
- 网页：`BotQR.svelte`（两种共用），「＋ 添加机器人」能选 QQ（扫码，或手动填）和微信（只能扫码），机器人面板显示主人、微信能重新扫码。`channelMeta` 按渠道名前缀找种类（`tg.x`、`feishu.x`、`qq.x`、`wx.x` 都有图标颜色），拓扑和 ⌘K 里另外加的机器人显示它自己的名字。
- 删除微信机器人时删掉它的 state 文件。
- 测试（假服务，不碰腾讯）：`TestQQChannel`、`TestQQBadSecret`、`TestQQRegistration`、`TestWeixinChannel`、`TestWeixinStaleToken`、`TestWeixinRegistration`、`TestPlainText`、`web/TestBotRegisterQQ`；`scripts/check-web-ui.py` 的 `check_bots`（扫码接口在浏览器里拦下，机器人建成停用的）。

### 修：没有流式卡片的窗口丢了调工具之前说的话

用户在飞书上用拟人型：模型先说“哎哟这还用猜嘛……等等，我扒开看一眼”，调 `Read`，再说“翻到底了，空空如也……”，飞书上只看到后一段。原因：拟人型私聊不开流式卡片，`finish` 只发 `r.final`（最后一次模型输出），调工具那一轮的正文只存进了历史。QQ、微信的工具型也一样。

- `sayMidway`：模型这一轮要调工具、又说了话，就给没有流式显示的窗口当场发出去（拟人型私聊按段拆几条、群里走 `sendGroup`、别的 `deliverReply`）；网页、网关里的群、定时任务、内心活动不发。
- `sendParts` 不再自己起协程发，改成往这个渠道的发送队列里放（`outItem.delay` 是发之前停多久），所以先说的一定先到，不会被后面一次发完的回复抢先。
- 测试：`TestLifeDMSaysBeforeTools`（模拟模型支持 `说的话 !call 工具`）。

### 看图和发文件（SendFile）

用户反馈：agent 自己下载了表情包却说看不见；希望它能在聊天里发图片，以后还要音频、视频。

- 执行器加了内部工具 `Fetch`（读整个文件，base64，默认最多 20 MB，读的规则和 Read 一样）；`Read` 读到 PNG/JPEG/GIF/WebP 时回“<路径> 是图片（PNG，N 字节）”。内核只多登记了一个网关工具 `SendFile`（PUBLIC_READ）。
- 网关 `app/fetch.go`：`fetchTarget` 按这一轮的工作目录找机器（本机 / SSH / 装了内核的电脑，HOME 模式在网关本机）；`seeImage` 在 Read 结果是图片时取过来存进 `data/uploads`，附在工具结果的 `Images` 上，取完之前 `TModel` 等着（`run.fetching` / `modelWaiting`）；`session.BuildMessages` 把这批工具结果的图片作为一条“[系统] 上面的工具读到的图片”用户消息跟在后面；旧轮次的图片随工具结果一起省略。
- `SendFile {files:[路径或网址]}`：取文件（网址下载，访客不许内网），`media.SaveFile` 存起来，放进这一轮每个聊天窗口的发送队列（`outItem.att`），渠道实现 `channel.MediaSender` 就发文件，否则发“［图片：名字］”。结果记在工具消息的 `Sent` 上，网页工具卡片下面显示图片 / 文件。
- 渠道：飞书（上传图片 / 文件，opus 发语音，其余发文件；扫码新建的应用多申请 `im:resource`，**老应用要在开放平台补开 `im:resource` 并发布**）、Telegram（photo / animation / voice / audio / video / document）、QQ（先上传 `/files` 再发 msg_type 7，只支持图片、mp4、silk 语音）、微信（AES-128-ECB 加密传 CDN，图片 / 视频 / 文件消息）。
- 拟人守则加了一句：想发表情包就用 SendFile，别用文字描述。`prefixVer` 升到 2（旧会话工具快照作废一次）。
- 测试：`TestReadImageShownToModel`、`TestSendFileSendsToChat`、`TestTelegramSendMedia`、`TestQQSendMedia`、`TestWeixinSendMedia`、`kernel/tests/test_exec.py` 的 `t_fetch`、`kernel/tests/test_exec_win.py` 的 `t_fetch`、`scripts/e2e.sh` 的 SendFile 一步（2026-10-25 跑过了，见文末「验证过」）。
- **还没做**：音频、视频格式转换（QQ 语音要 silk、飞书语音要 opus）；合并 main、发 v0.7.0（等用户确认）。

## 没做、要注意的

- **没有在真的 QQ、微信上跑过**，协议按官方插件源码和文档写的。用户第一次扫码时最可能出问题的地方：QQ 的 `source` 参数留空（扫码页显示“第三方机器人”）；微信的 `iLink-App-ClientVersion` 和 `channel_version` 跟着官方插件 2.4.10。
- QQ 没做流式回复（`/v2/users/{openid}/stream_messages`，只支持私聊和 Markdown），工具型在 QQ 上是回完一次发。发图片和文件做了（先 `POST …/files` 上传拿 `file_info`，再发 `msg_type 7`；只支持图片、mp4、silk 语音）。
- 微信没做引用消息。发图片、视频、文件做了（AES-128-ECB 加密后传 CDN）。
- QQ 群里的发送人 openid 和私聊的不一样（每个场景一个 openid），所以主人在群里不是管理员身份（群里本来也按访客算）；私聊里是管理员，和 Telegram 的管理员一样接到主会话。
- 发动态、朋友圈没做（见上）。

## 验证过（2026-10-25）

接手时（另一个会话额度用完）补做的验证和收尾。

- `bash scripts/test-isolated.sh` 通过（隔离 HOME、`NANO_TEST_NO_WINDOWS=1`）：`test_core` 153 项、执行器 / 宿主测试、`go test -race`（0 个 DATA RACE）、`scripts/e2e.sh` **74 项全过**（含新加的 SendFile 一步）、`e2e-control.py` 12 项、Linux 离线安装 6 项、一行安装、升级 / 回滚、macOS 交叉编译，没有任何 make 报错。
- **网页界面检查**单独跑过（`python3 scripts/check-web-ui.py`，0 failures）。坑：隔离脚本换了 HOME，playwright 是 `pip install --user` 装在 `~/.local` 的，所以 `make test` 里那一步会**自己跳过**、看起来像通过——要跑就单独跑，或者靠 CI（CI 装的是系统级）。
- **Windows 执行器**（`make -C kernel test-win`，zig + WSL interop）14 项全过。新增的 `t_fetch` 覆盖：Read 说是图片、Fetch 取回 base64、中文路径和中文内容、`max_bytes`、目录、工作目录外、文件不存在；`setup()` 里补了 `pic.png`。
- **Windows 内核端到端**（`python3 scripts/e2e-win.py`）17 项全过。CI 不跑这个（要 WSL interop），只在有 WSL 的机器上跑。
- `scripts/test-install-win.py` 本地没跑（它要注册计划任务；CI 的 Windows runner 上用 `NANO_TEST_SERVICE=1` 跑，不必在用户的机器上试）。
- **顺手修的 Windows 报错**：`plat_win.c` 的 `plat_stat` / `plat_open_read` / `plat_file_read` 失败时没设 `errno`，文件不存在时看到的是“读取 … 失败：No error”。现在按 `GetLastError()` 映射成 `ENOENT` / `EACCES` 等，和 Linux 上一样是“No such file or directory”。新工具 Fetch 走的正是 `plat_open_read` / `plat_file_read`，所以 v0.7 用得上；`test_exec_win.py` 加了“不存在的文件要说出原因”的断言。
- 推送 d307b8c 之后 CI 跑完是 **success**（Linux + Windows + macOS 三个 job）。