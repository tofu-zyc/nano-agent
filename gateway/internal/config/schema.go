package config

// Field 描述一个配置项，网页界面据此生成控件、做搜索。所有说明都集中在这里。
type Field struct {
	Key      string   `json:"key"`               // 比如 "llm.timeout"
	Group    string   `json:"group"`             // 它在拓扑图上的哪个节点：Nodes 里的一个
	Section  string   `json:"section,omitempty"` // 节点面板里的小标题
	Label    string   `json:"label"`
	Help     string   `json:"help,omitempty"`
	Type     string   `json:"type"`               // string | int | float | bool | enum | multi | list | secret | json | proxy
	Options  []string `json:"options,omitempty"`  // enum / multi 可选的固定值
	Fixed    []string `json:"fixed,omitempty"`    // multi 里总是选上、不能去掉的值
	Restart  bool     `json:"restart,omitempty"`  // 要重启网关才生效
	ReadOnly bool     `json:"readonly,omitempty"` // 只能在配置文件里改
	Advanced bool     `json:"advanced,omitempty"`
	Keywords string   `json:"keywords,omitempty"` // 额外的搜索词
}

// Node 是拓扑图上一个挂着设置的节点。每个配置项只有一个家：Field.Group 必须是这里的一个（有测试）。
type Node struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Nodes 是挂着设置的节点，顺序就是聊天里查设置时分组的顺序。
var Nodes = []Node{
	{"agent", "Agent：每条消息的上限、权限、回复方式"},
	{"gateway", "网关：代理、更新、脱敏、内核连接、存储"},
	{"web", "网页：地址、登录密码"},
	{"cli", "终端"},
	{"telegram", "Telegram"},
	{"feishu", "飞书"},
	{"model", "模型：所有模型共用的请求参数"},
	{"memory", "上下文与记忆"},
	{"search", "网络搜索"},
	{"skills", "技能"},
	{"local", "本机：网关自己启动的内核"},
	{"machines", "接入机器：别的电脑怎么连进来"},
}

// Fields 是全部可以在网页里查看、修改的配置项，按节点排列（同一节点里按小标题第一次出现的顺序分节）。
var Fields = []Field{
	{Key: "runtime.long_call_hint_seconds", Group: "gateway", Section: "并行与队列", Label: "前台命令提示秒数", Type: "int", Restart: true, Advanced: true, Help: "命令实际开始执行后达到此秒数仍未结束，只提示一次；排队时间不计入，不受过程显示档位影响。默认 10，范围 1–3600；修改后重启生效。"},
	{Key: "runtime.max_calls_per_turn", Group: "gateway", Section: "并行与队列", Label: "每次生成的调用上限", Type: "int", Restart: true, Advanced: true, Help: "一次模型返回最多接受多少工具调用；超过上限整批拒绝，不执行其中一部分。 默认 32；修改后重启生效。"},
	{Key: "runtime.max_active_runs_per_node", Group: "gateway", Section: "并行与队列", Label: "每个内核的活动任务上限", Type: "int", Restart: true, Advanced: true, Help: "达到上限后保留在网关有界队列，按会话轮转和等待时间调度；节点自己的硬上限仍优先。 默认 16；修改后重启生效。"},
	{Key: "runtime.max_parallel_tools_per_run", Group: "gateway", Section: "并行与队列", Label: "每个任务的并行工具数", Type: "int", Restart: true, Advanced: true, Help: "只计实际运行；排队、审批不占槽，转后台后继续占机器槽。 默认 4；修改后重启生效。"},
	{Key: "runtime.max_active_execs_per_machine", Group: "gateway", Section: "并行与队列", Label: "每台机器的执行槽", Type: "int", Restart: true, Advanced: true, Help: "真实执行器总数，包含后台命令和 Hook；至少 2，预留一个 Hook 槽。 默认 8；修改后重启生效。"},
	{Key: "runtime.max_parallel_shells_per_machine", Group: "gateway", Section: "并行与队列", Label: "每台机器的并行命令数", Type: "int", Restart: true, Advanced: true, Help: "还须满足资源锁和执行槽，不根据模型自报只读放松限制。 默认 2；修改后重启生效。"},
	{Key: "runtime.max_background_jobs_per_agent", Group: "gateway", Section: "并行与队列", Label: "每个 agent 的后台任务上限", Type: "int", Restart: true, Advanced: true, Help: "后台任务仍占真实执行槽和资源锁。 默认 4；修改后重启生效。"},
	{Key: "runtime.max_live_jobs_per_machine", Group: "gateway", Section: "并行与队列", Label: "每台机器的未结束任务上限", Type: "int", Restart: true, Advanced: true, Help: "包括前台、后台、待启动调用；超出时明确拒绝新调用。 默认 16；修改后重启生效。"},
	{Key: "runtime.max_queued_calls_per_run", Group: "gateway", Section: "并行与队列", Label: "每个任务的排队调用上限", Type: "int", Restart: true, Advanced: true, Help: "限制已接受、尚未实际启动的调用。 默认 32；修改后重启生效。"},
	// ---- Agent：每条消息怎么跑、谁能用、怎么回复 ----
	{Key: "agent.max_steps", Group: "agent", Section: "每条消息", Label: "工具调用轮数上限", Type: "int",
		Help:     "一条消息里最多请求模型几次（模型每调一轮工具算一次），到了就先停下，发「继续」接着做。0 表示不限，默认 200",
		Keywords: "工具调用 调用次数 轮数 轮次 上限 步数 次数 请求模型 budget max_steps"},
	{Key: "agent.run_timeout", Group: "agent", Section: "每条消息", Label: "最长处理多久", Type: "string",
		Help:     "一条消息从开始到回复最多处理多久，例如 30m、2h；0 表示不限（默认），随时可以发 /stop 停下",
		Keywords: "超时 时限 timeout"},
	{Key: "agent.approval_timeout", Group: "agent", Section: "每条消息", Label: "等审批多久", Type: "string",
		Help:     "要你批准的操作等多久没回应就算拒绝，例如 30m（默认）；0 表示一直等",
		Keywords: "审批 批准 超时 approval"},
	{Key: "agent.max_output_kb", Group: "agent", Section: "每条消息", Label: "工具结果最多保留（KB）", Type: "int", Advanced: true,
		Help:     "一次读文件、执行命令的输出超过这么多就截断（保留开头和结尾，模型可以分段再读），默认 64",
		Keywords: "输出 截断 output"},
	{Key: "sandbox.mode", Group: "agent", Section: "权限", Label: "执行隔离（沙箱）", Type: "enum", Options: []string{"auto", "on", "off"},
		Help:     "和审批策略分开：auto 用机器上可用的隔离，没有完整隔离时保留旧行为（Shell 要审批）；on 要求完整隔离，机器给不了就明确失败；off 明确关闭执行隔离（不套 Landlock / seccomp），按当前系统用户身份执行，文件工具的路径权限和审批规则照旧。执行节点自己的 kernel.conf 里的 sandbox 更严时以节点为准",
		Keywords: "沙箱 隔离 sandbox landlock seccomp 关闭沙箱"},
	{Key: "workspace.mode", Group: "agent", Section: "权限", Label: "默认权限模式", Type: "enum", Options: []string{"full", "auto", "ask", "readonly"},
		Help: "新会话用的权限模式（每个会话可以在自己的节点里、或者发 /mode 单独改）。full：放行，所有工具直接执行、不审批；auto：工作目录内全部直接执行；ask：写文件、执行命令前要批准；readonly：只读。拟人型 agent 没单独设置时用 full", Keywords: "yolo 审批 放行 权限 模式"},
	{Key: "access.admins", Group: "agent", Section: "权限", Label: "管理员", Type: "list",
		Help:     "身份用 /whoami 查，比如 tg:123456789。管理员能用写文件、执行命令的工具，能改设置、审批。终端和网页用户始终是管理员。也可以在 Telegram、飞书节点里把聊过天的人直接设为管理员",
		Keywords: "admin 权限 身份"},
	{Key: "access.guest", Group: "agent", Section: "权限", Label: "其他人能用的工具", Type: "enum", Options: []string{"none", "readonly"},
		Help: "none：只能聊天；readonly：可以读工作目录里的文件和搜索", Keywords: "访客 权限"},
	{Key: "access.allow_chats", Group: "agent", Section: "权限", Label: "聊天窗口白名单", Type: "list",
		Help: "只响应这些聊天窗口（窗口 ID 用 /whoami 查，比如 tg:123456789），空表示不限制。终端和网页总是允许", Keywords: "白名单 群 会话"},
	{Key: "channels.show", Group: "agent", Section: "回复", Label: "显示档位", Type: "enum", Options: []string{"详细", "过程", "简洁"},
		Help:     "Telegram、飞书、终端里显示多少过程：详细 = 思考 + 工具 + 正文，过程 = 工具 + 正文，简洁 = 只有正文（处理中一行状态）。每个机器人可以在自己的节点里单独选，每个聊天窗口可以发 /show 单独设；网页始终显示全部",
		Keywords: "显示 思考 工具 过程 简洁 详细 show"},
	{Key: "session.timestamps", Group: "agent", Section: "回复", Label: "消息带时间", Type: "bool",
		Help: "在用户消息前加上发送时间，模型不用调工具就知道现在几点"},
	{Key: "session.timezone", Group: "agent", Section: "回复", Label: "时区", Type: "string",
		Help: "比如 Asia/Shanghai；空表示系统时区。消息时间和定时任务都按它", Keywords: "时间"},

	// ---- 网关：系统 ----
	{Key: "network.proxy", Group: "gateway", Section: "网络", Label: "默认代理", Type: "proxy",
		Help: "模型、聊天渠道、网络搜索的代理留空时用这里的设置；检查更新、下载新版本也走它", Keywords: "proxy 代理 网络 翻墙 更新 github"},
	{Key: "update.check", Group: "gateway", Section: "更新", Label: "检查新版本", Type: "bool",
		Help: "每天查一次有没有新版本，有的话在网关节点上提示；更新要你自己发起（这里的按钮、/update 或 nano-gateway update）", Keywords: "update 升级 版本"},
	{Key: "update.kernels", Group: "gateway", Section: "更新", Label: "内核自动更新", Type: "bool",
		Help: "接入的电脑上内核版本和网关不一样时，自动换成网关的版本（失败会自己退回）", Keywords: "update 升级 版本 内核"},
	{Key: "update.url", Group: "gateway", Section: "更新", Label: "下载地址", Type: "string", Advanced: true,
		Help: "发布包从哪里下载；空表示 GitHub Release。下载走「默认代理」；访问 GitHub 慢时也可以换成镜像", Keywords: "update mirror 镜像"},
	{Key: "security.redact", Group: "gateway", Section: "安全", Label: "工具结果脱敏", Type: "enum", Options: []string{"off", "public", "always"},
		Help: "off：不脱敏；public（默认）：模型接口在外网、或发到 Telegram / 飞书时，把工具结果里的密钥打星；always：发给模型时总是打星。网页和终端显示原文", Keywords: "脱敏 密钥 redact secret"},
	{Key: "kernel.listen", Group: "gateway", Section: "内核连接", Label: "内核监听地址", Type: "string", Restart: true, Advanced: true,
		Help: "网关自己启动的本机内核、手动启动的内核来连的地址：unix:/path 或 tcp:host:port。别的电脑用「接入机器」里的电脑接入地址", Keywords: "kernel listen socket"},
	{Key: "kernel.token", Group: "gateway", Section: "内核连接", Label: "内核连接口令", Type: "secret", Restart: true, Advanced: true,
		Help: "手动写 kernel.conf 的内核用的口令（和 kernel.conf 里的 token 一致）；用安装脚本装的电脑走配对，不用它"},
	{Key: "persona.dir", Group: "gateway", Section: "存储", Label: "人格目录", Type: "string", Restart: true, Advanced: true},
	{Key: "session.data_dir", Group: "gateway", Section: "存储", Label: "数据目录", Type: "string", ReadOnly: true, Advanced: true,
		Help: "对话历史、会话设置、网页里改的配置都存在这里，只能在配置文件里改"},

	// ---- 网页 ----
	{Key: "web.listen", Group: "web", Label: "网页地址", Type: "string", Restart: true,
		Help: "默认只监听本机。要从别的机器访问，建议走 SSH 隧道或 Tailscale", Keywords: "web 网页 端口 listen"},
	{Key: "web.password", Group: "web", Label: "登录密码", Type: "secret",
		Help: "空表示自动生成（存在数据目录的 web.secret 里）。改了之后要重新登录", Keywords: "密码 登录 password"},

	// ---- 终端、Telegram、飞书 ----
	{Key: "cli.enabled", Group: "cli", Label: "启用终端聊天", Type: "bool", Restart: true,
		Help: "另开一个终端运行 make chat 连进来聊天；网关在数据目录里开一个只有自己能连的 socket"},
	{Key: "cli.show_reasoning", Group: "cli", Label: "显示思考过程", Type: "bool"},
	{Key: "cli.show_usage", Group: "cli", Label: "显示用量", Type: "bool", Help: "每轮结束后显示用量和缓存命中率"},

	{Key: "telegram.enabled", Group: "telegram", Label: "启用", Type: "bool"},
	{Key: "telegram.token", Group: "telegram", Label: "Bot Token", Type: "secret", Help: "找 @BotFather 创建机器人拿到", Keywords: "机器人 bot"},
	{Key: "telegram.proxy", Group: "telegram", Label: "代理", Type: "proxy", Help: "国内访问 Telegram 一般需要代理"},
	{Key: "telegram.show", Group: "telegram", Label: "显示档位", Type: "enum", Options: []string{"", "详细", "过程", "简洁"},
		Help: "只管这个机器人；跟随 Agent 就用 Agent 节点里的显示档位。某个聊天窗口还可以发 /show 单独设"},
	{Key: "telegram.api_base", Group: "telegram", Label: "API 地址", Type: "string", Advanced: true},

	{Key: "feishu.enabled", Group: "feishu", Label: "启用", Type: "bool"},
	{Key: "feishu.app_id", Group: "feishu", Label: "App ID", Type: "string", Keywords: "应用 机器人"},
	{Key: "feishu.app_secret", Group: "feishu", Label: "App Secret", Type: "secret"},
	{Key: "feishu.domain", Group: "feishu", Label: "版本", Type: "enum", Options: []string{"feishu", "lark"}, Help: "feishu：飞书；lark：国际版"},
	{Key: "feishu.proxy", Group: "feishu", Label: "代理", Type: "proxy", Help: "飞书在国内一般可以直连"},
	{Key: "feishu.show", Group: "feishu", Label: "显示档位", Type: "enum", Options: []string{"", "详细", "过程", "简洁"},
		Help: "只管这个机器人；跟随 Agent 就用 Agent 节点里的显示档位。某个聊天窗口还可以发 /show 单独设"},

	// ---- 模型：所有模型共用的请求参数（接口和模型本身在模型节点的列表里） ----
	{Key: "llm.timeout", Group: "model", Section: "请求参数", Label: "请求超时（秒）", Type: "int", Advanced: true,
		Help: "所有模型接口的请求超时", Keywords: "超时 timeout"},
	{Key: "llm.send_reasoning", Group: "model", Section: "请求参数", Label: "回传思考内容", Type: "bool", Advanced: true,
		Help: "把当前这一轮的思考内容回传给模型，部分推理模型调用工具时需要"},
	{Key: "llm.temperature", Group: "model", Section: "请求参数", Label: "默认温度", Type: "float", Advanced: true,
		Help: "所有模型没单独设置温度时用这个；空表示用模型默认"},
	{Key: "llm.extra", Group: "model", Section: "请求参数", Label: "额外请求参数", Type: "json", Advanced: true,
		Help: "原样合并进请求体的 JSON 对象"},

	// ---- 上下文与记忆 ----
	{Key: "context.compact_at", Group: "memory", Section: "上下文整理", Label: "自动整理阈值", Type: "float",
		Help: "上下文到模型窗口的这个比例时自动整理（旧的工具输出换成占位，更早的对话合成摘要），默认 0.75", Keywords: "压缩 compact 记忆"},
	{Key: "context.compact_to", Group: "memory", Section: "上下文整理", Label: "整理后目标比例", Type: "float", Help: "默认 0.4，最近保留的轮次较大时可能超过目标"},
	{Key: "context.keep_turns", Group: "memory", Section: "上下文整理", Label: "原样保留最近几轮", Type: "int", Help: "整理时最近这几轮对话一字不动，默认 4"},
	{Key: "context.idle_compact", Group: "memory", Section: "上下文整理", Label: "空闲整理间隔", Type: "string",
		Help: "会话空闲这么久后，在后台把上下文整理一次，下次说话不用等；例如 30m、2h，0 关闭"},
	{Key: "context.idle_compact_min", Group: "memory", Section: "上下文整理", Label: "空闲整理最低占比", Type: "float", Help: "空闲时只整理上下文超过窗口这个比例的会话，默认 0.3"},
	{Key: "llm.trim_to", Group: "memory", Section: "上下文整理", Label: "超过历史上限时整理到", Type: "float", Advanced: true,
		Help: "只在模型设了历史上限（max_history_tokens）时用：到上限就整理到上限的这个比例，默认 0.5；没设上限时看「整理后目标比例」", Keywords: "压缩 裁剪 缓存"},
	{Key: "context.archive_days", Group: "memory", Section: "归档", Label: "归档保留天数", Type: "int",
		Help: "/reset 时旧对话挪到 data/archive/ 下，保留这么多天，0 永久保留，默认 90"},

	// ---- 网络搜索 ----
	{Key: "search.enabled", Group: "search", Label: "启用搜索", Type: "bool",
		Help: "开着并且有可用的 key 时，模型多了 WebSearch（搜网页）和 WebFetch（读网页）两个工具；访客也能用", Keywords: "搜索 联网 上网 web search fetch"},
	{Key: "search.provider", Group: "search", Label: "搜索服务", Type: "enum", Options: []string{"deepseek"}},
	{Key: "search.api_key", Group: "search", Label: "搜索 API key", Type: "secret",
		Help: "DeepSeek 的 API key。留空时借用模型接口里地址是 api.deepseek.com 的那个 key；按次计费，每千次成功请求 5 元"},
	{Key: "search.count", Group: "search", Label: "默认搜索条数", Type: "int", Help: "模型没说要几条时返回几条，1–10，默认 6"},

	// ---- 技能 ----
	{Key: "skills.dir", Group: "skills", Label: "技能目录", Type: "string", Restart: true,
		Help: "每个技能一个子目录，里面是 SKILL.md 和附带的脚本、资料。改动下一轮自动生效", Keywords: "skill 技能"},

	// ---- 本机：网关自己启动的内核 ----
	{Key: "kernel.local", Group: "local", Section: "本机内核", Label: "启动本机内核", Type: "bool", Restart: true,
		Help: "网关自己在这台机器上启动一个内核（退出了自动重启），本机的工作目录由它执行"},
	{Key: "kernel.local_name", Group: "local", Section: "本机内核", Label: "本机内核的名字", Type: "string", Restart: true, Advanced: true,
		Help: "默认是 gateway。不要和别的内核同名，同名的会互相顶掉"},

	// ---- 接入机器：别的电脑怎么连进来 ----
	{Key: "kernel.device_listen", Group: "machines", Label: "电脑接入地址", Type: "string",
		Help:     "别的电脑上的内核来连的 TCP 地址，例如 0.0.0.0:7700；空表示关闭。第一次连接要在这里核对确认码、允许接入，跨网络使用私有网络或隧道",
		Keywords: "电脑 接入 配对 端口 device"},
}

// FieldByKey 按 key 查找配置项。
func FieldByKey(key string) (Field, bool) {
	for _, f := range Fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// NodeByID 按 ID 查找挂着设置的节点。
func NodeByID(id string) (Node, bool) {
	for _, n := range Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}
