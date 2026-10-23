package config

// Field 描述一个配置项，网页界面据此生成控件、做搜索。所有说明都集中在这里。
type Field struct {
	Key      string   `json:"key"`               // 比如 "llm.model"
	Group    string   `json:"group"`             // 归属的对象：gateway、model、kernel、workspace、cli、telegram、feishu
	Section  string   `json:"section,omitempty"` // 对象内的小标题
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

// Fields 是全部可以在网页里查看、修改的配置项。
var Fields = []Field{
	{Key: "context.compact_at", Group: "context", Section: "上下文", Label: "自动整理阈值", Type: "float", Help: "达到模型窗口的这个比例时整理，默认 0.75"},
	{Key: "context.compact_to", Group: "context", Section: "上下文", Label: "整理后目标比例", Type: "float", Help: "默认 0.4，最近保留的轮次较大时可能超过目标"},
	{Key: "context.keep_turns", Group: "context", Section: "上下文", Label: "原样保留最近几轮", Type: "int"},
	{Key: "context.idle_compact", Group: "context", Section: "上下文", Label: "空闲整理间隔", Type: "string", Help: "例如 30m，0 关闭"},
	{Key: "context.idle_compact_min", Group: "context", Section: "上下文", Label: "空闲整理最低占比", Type: "float"},
	{Key: "context.archive_days", Group: "context", Section: "上下文", Label: "归档保留天数", Type: "int", Help: "0 永久保留，默认 90"},
	// ---- 模型的全局默认（接口和模型都在网页的模型节点里配置） ----
	{Key: "llm.timeout", Group: "context", Section: "接口", Label: "请求超时（秒）", Type: "int", Advanced: true,
		Help: "所有模型接口的请求超时", Keywords: "超时 timeout"},
	{Key: "llm.send_reasoning", Group: "context", Section: "思考", Label: "回传思考内容", Type: "bool", Advanced: true,
		Help: "把当前这一轮的思考内容回传给模型，部分推理模型调用工具时需要"},
	{Key: "llm.trim_to", Group: "context", Section: "上下文", Label: "裁剪后保留比例", Type: "float",
		Help: "历史超过上限时保留这个比例，其余合并成摘要；之后只追加，让请求前缀保持稳定", Keywords: "压缩 裁剪 缓存"},
	{Key: "llm.temperature", Group: "context", Section: "采样", Label: "默认温度", Type: "float", Advanced: true,
		Help: "所有模型没单独设置温度时用这个；空表示用模型默认"},
	{Key: "llm.extra", Group: "context", Section: "采样", Label: "额外请求参数", Type: "json", Advanced: true,
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
	{Key: "update.check", Group: "gateway", Section: "更新", Label: "检查新版本", Type: "bool",
		Help: "每天查一次有没有新版本，有的话在状态里提示；更新要你自己发起（网页按钮、/update 或 nano-gateway update）", Keywords: "update 升级 版本"},
	{Key: "update.kernels", Group: "gateway", Section: "更新", Label: "内核自动更新", Type: "bool",
		Help: "接入的电脑上内核版本和网关不一样时，自动换成网关的版本（失败会自己退回）", Keywords: "update 升级 版本 内核"},
	{Key: "update.url", Group: "gateway", Section: "更新", Label: "下载地址", Type: "string", Advanced: true,
		Help: "发布包从哪里下载；空表示 GitHub Release。访问 GitHub 慢时可以换成镜像", Keywords: "update mirror 镜像"},
	{Key: "persona.dir", Group: "gateway", Section: "存储", Label: "人格目录", Type: "string", Restart: true, Advanced: true},
	{Key: "skills.dir", Group: "gateway", Section: "存储", Label: "技能目录", Type: "string", Restart: true, Advanced: true,
		Help: "每个技能一个子目录，里面是 SKILL.md 和附带的脚本、资料。改动下一轮自动生效"},
	{Key: "session.data_dir", Group: "gateway", Section: "存储", Label: "数据目录", Type: "string", ReadOnly: true, Advanced: true,
		Help: "对话历史、会话设置、网页里改的配置都存在这里，只能在配置文件里改"},
	{Key: "web.listen", Group: "gateway", Section: "网页管理", Label: "网页地址", Type: "string", Restart: true,
		Help: "默认只监听本机。要从别的机器访问，建议走 SSH 隧道或 Tailscale"},
	{Key: "web.password", Group: "gateway", Section: "网页管理", Label: "登录密码", Type: "secret",
		Help: "空表示自动生成（存在数据目录的 web.secret 里）。改了之后要重新登录"},
	{Key: "security.redact", Group: "gateway", Section: "安全", Label: "工具结果脱敏", Type: "enum", Options: []string{"off", "public", "always"},
		Help: "off：不脱敏；public（默认）：模型接口在外网、或发到 Telegram / 飞书时，把工具结果里的密钥打星；always：发给模型时总是打星。网页和终端显示原文", Keywords: "脱敏 密钥 redact secret"},

	// ---- 内核 ----
	{Key: "kernel.listen", Group: "kernel", Section: "连接", Label: "监听地址", Type: "string", Restart: true,
		Help: "内核来连的地址：unix:/path 或 tcp:host:port。跨机器请走加密隧道"},
	{Key: "kernel.token", Group: "kernel", Section: "连接", Label: "连接口令", Type: "secret", Restart: true,
		Help: "和 kernel.conf 里的 token 一致"},
	{Key: "kernel.device_listen", Group: "kernel", Section: "电脑接入", Label: "电脑接入地址", Type: "string",
		Help: "别的电脑上的内核来连的 TCP 地址，例如 0.0.0.0:7700；空表示关闭。第一次连接要在设备页面确认配对，跨网络使用私有网络或隧道"},
	{Key: "kernel.local", Group: "kernel", Section: "本机内核", Label: "启动本机内核", Type: "bool", Restart: true,
		Help: "网关自己在这台机器上启动一个内核（退出了自动重启），本机的工作目录由它执行"},
	{Key: "kernel.local_name", Group: "kernel", Section: "本机内核", Label: "本机内核的名字", Type: "string", Restart: true, Advanced: true,
		Help: "默认是 gateway。不要和别的内核同名，同名的会互相顶掉"},

	// ---- 工作目录 ----
	{Key: "workspace.mode", Group: "workspace", Section: "默认值", Label: "默认权限模式", Type: "enum", Options: []string{"auto", "ask", "readonly"},
		Help: "auto：工作目录内全部直接执行；ask：写文件、执行命令前要批准；readonly：只读", Keywords: "yolo 审批 权限"},
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
