package config

// Field 描述一个配置项，网页界面据此生成控件、做搜索。所有说明都集中在这里。
type Field struct {
	Key      string   `json:"key"`               // 比如 "llm.model"
	Group    string   `json:"group"`             // 归属的对象：gateway、model、kernel、workspace、cli、telegram、feishu
	Section  string   `json:"section,omitempty"` // 对象内的小标题
	Label    string   `json:"label"`
	Help     string   `json:"help,omitempty"`
	Type     string   `json:"type"` // string | int | float | bool | enum | list | secret | json | proxy
	Options  []string `json:"options,omitempty"`
	Restart  bool     `json:"restart,omitempty"`  // 要重启网关才生效
	ReadOnly bool     `json:"readonly,omitempty"` // 只能在配置文件里改
	Advanced bool     `json:"advanced,omitempty"`
	Keywords string   `json:"keywords,omitempty"` // 额外的搜索词
}

// Fields 是全部可以在网页里查看、修改的配置项。
var Fields = []Field{
	// ---- 模型 ----
	{Key: "llm.provider", Group: "model", Section: "接口", Label: "接口类型", Type: "enum", Options: []string{"openai", "mock"},
		Help: "openai：任何 OpenAI 兼容的接口（vLLM、DeepSeek、通义、Kimi……）；mock：不调用模型的假模型，调试用"},
	{Key: "llm.base_url", Group: "model", Section: "接口", Label: "接口地址", Type: "string",
		Help: "到 /v1 为止，比如 https://api.deepseek.com/v1", Keywords: "url endpoint 地址 服务器"},
	{Key: "llm.api_key", Group: "model", Section: "接口", Label: "API Key", Type: "secret", Keywords: "密钥 key token"},
	{Key: "llm.model", Group: "model", Section: "接口", Label: "模型名", Type: "string"},
	{Key: "llm.proxy", Group: "model", Section: "接口", Label: "代理", Type: "proxy",
		Help: "访问模型接口用的代理。内网地址的模型建议直连", Keywords: "proxy 网络"},
	{Key: "llm.timeout", Group: "model", Section: "接口", Label: "请求超时（秒）", Type: "int", Advanced: true},
	{Key: "llm.thinking", Group: "model", Section: "思考", Label: "默认思考强度", Type: "string",
		Help: "作为 reasoning_effort 传给模型；“模型默认”表示不传。每个会话可以单独改", Keywords: "reasoning effort 推理"},
	{Key: "llm.thinking_levels", Group: "model", Section: "思考", Label: "可选的思考强度", Type: "list", Advanced: true,
		Help: "/think 和网页里能选的值，原样作为 reasoning_effort 传给模型（none 表示关闭）。不同模型支持的值不一样，比如有的不支持 medium"},
	{Key: "llm.send_reasoning", Group: "model", Section: "思考", Label: "回传思考内容", Type: "bool", Advanced: true,
		Help: "把当前这一轮的思考内容回传给模型，部分推理模型调用工具时需要"},
	{Key: "llm.context_window", Group: "model", Section: "上下文", Label: "上下文窗口", Type: "int", Help: "模型能处理的最大 token 数"},
	{Key: "llm.max_history_tokens", Group: "model", Section: "上下文", Label: "历史上限", Type: "int",
		Help: "历史超过这么多 token（估算）时，一次裁掉最早的一段", Keywords: "裁剪 截断 记忆 长度"},
	{Key: "llm.trim_to", Group: "model", Section: "上下文", Label: "裁剪后保留比例", Type: "float",
		Help: "裁到上限的这个比例，之后只追加，前缀缓存才能一直命中", Keywords: "裁剪 缓存"},
	{Key: "llm.temperature", Group: "model", Section: "采样", Label: "温度", Type: "float", Advanced: true, Help: "空表示用模型默认"},
	{Key: "llm.max_tokens", Group: "model", Section: "采样", Label: "最大输出 token", Type: "int", Advanced: true, Help: "0 表示不限制"},
	{Key: "llm.extra", Group: "model", Section: "采样", Label: "额外请求参数", Type: "json", Advanced: true,
		Help: "原样合并进请求体的 JSON 对象"},

	// ---- 网关：访问控制、消息、网页 ----
	{Key: "access.admins", Group: "gateway", Section: "访问控制", Label: "管理员", Type: "list",
		Help:     "身份用 /whoami 查，比如 tg:123456789。管理员能用写文件、执行命令的工具，能改设置、审批。命令行和网页用户始终是管理员",
		Keywords: "admin 权限 身份"},
	{Key: "access.guest", Group: "gateway", Section: "访问控制", Label: "其他人能用的工具", Type: "enum", Options: []string{"none", "readonly"},
		Help: "none：只能聊天；readonly：可以读工作目录里的文件和搜索", Keywords: "访客 权限"},
	{Key: "access.allow_chats", Group: "gateway", Section: "访问控制", Label: "聊天窗口白名单", Type: "list",
		Help: "只响应这些聊天窗口（窗口 ID 用 /whoami 查，比如 tg:123456789），空表示不限制。终端和网页总是允许", Keywords: "白名单 群 会话"},
	{Key: "network.proxy", Group: "gateway", Section: "网络", Label: "默认代理", Type: "proxy",
		Help: "模型和各个聊天渠道的代理留空时用这里的设置", Keywords: "proxy 代理 网络 翻墙"},
	{Key: "session.timestamps", Group: "gateway", Section: "消息", Label: "消息带时间", Type: "bool",
		Help: "在用户消息前加上发送时间，模型不用调工具就知道现在几点"},
	{Key: "session.timezone", Group: "gateway", Section: "消息", Label: "时区", Type: "string",
		Help: "比如 Asia/Shanghai；空表示系统时区", Keywords: "时间"},
	{Key: "persona.dir", Group: "gateway", Section: "存储", Label: "人格目录", Type: "string", Restart: true, Advanced: true},
	{Key: "session.data_dir", Group: "gateway", Section: "存储", Label: "数据目录", Type: "string", ReadOnly: true, Advanced: true,
		Help: "对话历史、会话设置、网页里改的配置都存在这里，只能在配置文件里改"},
	{Key: "web.listen", Group: "gateway", Section: "网页管理", Label: "网页地址", Type: "string", Restart: true,
		Help: "默认只监听本机。要从别的机器访问，建议走 SSH 隧道或 Tailscale"},
	{Key: "web.password", Group: "gateway", Section: "网页管理", Label: "登录密码", Type: "secret",
		Help: "空表示自动生成（存在数据目录的 web.secret 里）。改了之后要重新登录"},

	// ---- 内核 ----
	{Key: "kernel.listen", Group: "kernel", Section: "连接", Label: "监听地址", Type: "string", Restart: true,
		Help: "内核来连的地址：unix:/path 或 tcp:host:port。跨机器请走加密隧道"},
	{Key: "kernel.token", Group: "kernel", Section: "连接", Label: "连接口令", Type: "secret", Restart: true,
		Help: "和 kernel.conf 里的 token 一致"},

	// ---- 工作目录 ----
	{Key: "workspace.dirs", Group: "workspace", Section: "默认值", Label: "默认工作目录", Type: "list",
		Help: "新会话用的工作目录，第一个是默认目录。要写绝对路径或 ~ 开头。每个会话可以单独改", Keywords: "目录 路径 dir"},
	{Key: "workspace.mode", Group: "workspace", Section: "默认值", Label: "默认权限模式", Type: "enum", Options: []string{"auto", "ask", "readonly"},
		Help: "auto：工作目录内全部直接执行；ask：写文件、执行命令前要批准；readonly：只读", Keywords: "yolo 审批 权限"},
	{Key: "workspace.extra_ro", Group: "workspace", Section: "沙箱", Label: "额外只读路径", Type: "list",
		Help: "沙箱里额外能读的路径，比如 ~/.gitconfig", Keywords: "沙箱 sandbox"},
	{Key: "workspace.extra_rw", Group: "workspace", Section: "沙箱", Label: "额外可写路径", Type: "list",
		Help: "沙箱里额外能写的路径，比如 npm、cargo 的缓存目录", Keywords: "沙箱 sandbox 缓存"},

	// ---- 渠道 ----
	{Key: "cli.enabled", Group: "cli", Label: "启用终端聊天", Type: "bool", Restart: true,
		Help: "另开一个终端运行 make chat 连进来聊天；网关在数据目录里开一个只有自己能连的 socket"},
	{Key: "cli.show_reasoning", Group: "cli", Label: "显示思考过程", Type: "bool"},
	{Key: "cli.show_usage", Group: "cli", Label: "显示用量", Type: "bool", Help: "每轮结束后显示用量和缓存命中率"},

	{Key: "telegram.enabled", Group: "telegram", Label: "启用", Type: "bool"},
	{Key: "telegram.token", Group: "telegram", Label: "Bot Token", Type: "secret", Help: "找 @BotFather 创建机器人拿到", Keywords: "机器人 bot"},
	{Key: "telegram.proxy", Group: "telegram", Label: "代理", Type: "proxy", Help: "国内访问 Telegram 一般需要代理"},
	{Key: "telegram.api_base", Group: "telegram", Label: "API 地址", Type: "string", Advanced: true},

	{Key: "feishu.enabled", Group: "feishu", Label: "启用", Type: "bool"},
	{Key: "feishu.app_id", Group: "feishu", Label: "App ID", Type: "string", Keywords: "应用 机器人"},
	{Key: "feishu.app_secret", Group: "feishu", Label: "App Secret", Type: "secret"},
	{Key: "feishu.domain", Group: "feishu", Label: "版本", Type: "enum", Options: []string{"feishu", "lark"}, Help: "feishu：飞书；lark：国际版"},
	{Key: "feishu.proxy", Group: "feishu", Label: "代理", Type: "proxy", Help: "飞书在国内一般可以直连"},
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
