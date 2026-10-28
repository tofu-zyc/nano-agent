package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/redact"
)

// 网关管理工具：查询和修改都只给管理员。描述和 schema 由网关加进模型请求，
// 内核只认名字和权限类型（Skill 一样，调用时用 exec 交给网关）。

const gatewaySchema = `{
  "type":"object",
  "properties":{
    "what":{"type":"string","enum":["status","session","turns","models","ssh","workspaces","pairing","ssh_config","logs","search","settings","agents","mcp"],
      "description":"要查什么：网关状态、这个会话（含上下文用量）、这个会话的对话轮次（删除历史前先查编号）、模型接口、SSH 连接、工作目录、电脑配对、~/.ssh/config 里的别名、网关日志、网络搜索状态、设置项（当前值和说明）、agents（每个 agent 的名字、模型、人格、绑定的机器人、自己覆盖的设置），或者 mcp（MCP 服务：配置、在不在线、有哪些工具，环境变量只给名字；还有能挂 MCP 的在线电脑）"},
    "lines":{"type":"integer","description":"logs 要最近几行，默认 50，最多 500；turns 要最近几轮，默认 20"},
    "group":{"type":"string","enum":["agent","gateway","web","cli","telegram","feishu","model","memory","search","skills","local","machines"],
      "description":"settings 只看拓扑图上这个节点的设置：agent（工具调用轮数上限、每条消息的时限、默认权限模式、管理员、访客、显示档位、消息时间、时区）、gateway（代理、更新、脱敏、内核连接）、web（网页地址、密码）、cli/telegram/feishu（各渠道）、model（模型请求参数）、memory（上下文整理、归档；每个 agent 的长期记忆设置在它自己的 settings 里，用 agent_save 改）、search（网络搜索）、skills、local（本机内核）、machines（电脑接入地址）；不填就是全部"}
  }
}`

const messageSchema = `{
  "type":"object",
  "properties":{
    "to":{"type":"string","description":"发给谁：room:<群名或 ID> 发到群里，agent:<名字或 ID> 私聊某个 agent，chat:<会话 ID> 发到你自己的某个私聊（比如 chat:main 是主人）；也可以只写名字"},
    "text":{"type":"string","description":"要说的内容"},
    "wait":{"type":"boolean","description":"等对方回话再返回（最多 5 分钟），默认 false"}
  },
  "required":["to","text"]
}`

const gatewayAdminSchema = `{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":[
      "model_endpoint_save","model_endpoint_delete","model_fetch","model_save","model_delete","model_default","model_test","model_use",
      "ssh_save","ssh_delete","ssh_connect","ssh_disconnect","ssh_setup",
      "workspace_add","workspace_delete","workspace_mount","workspace_default","workspace_sandbox",
      "pair_allow","pair_deny","pair_remove","restart","update","history_delete","settings_set","agent_save","agent_delete","bot_bind",
      "mcp_save","mcp_delete","mcp_test"
    ],"description":"要做什么"},
    "key":{"type":"string","description":"settings_set：设置项的 key，比如 search.enabled、context.compact_at、channels.show（先用 Gateway settings 查）"},
    "value":{"description":"settings_set：新值（数字、true/false、文本或列表，类型按 Gateway settings 里的 type）；null 表示去掉网页里改的值、恢复成配置文件里的"},
    "id":{"type":"string","description":"接口 / 模型 / SSH / 工作目录的 ID、要改的 agent ID、要换绑的机器人 ID，或配对请求 ID"},
    "node":{"type":"string","description":"要移除的已配对电脑名字"},
    "name":{"type":"string","description":"接口或 SSH 连接的名字；agent_save 时是 agent 的名字；mcp_save / mcp_delete / mcp_test 时是 MCP 服务名（字母、数字、下划线、减号，也是工具名的前缀）"},
    "emoji":{"type":"string","description":"agent_save：头像，一个 emoji"},
    "style":{"type":"string","enum":["tool","life"],"description":"agent_save：类型。tool 工具型（有人找才干活，群里只回被叫到的）；life 拟人型（有作息，没事时自己上网、想事情、找人聊天，群里会插话）"},
    "settings":{"type":"object","description":"agent_save：这个 agent 自己的设置（键见 Gateway agents 里每个 agent 的 settings，比如 max_steps、mode、compact_at、group_eagerness、life_awake、life_level、life_reach、memory、dream_when）；值为 null 表示恢复跟随全局或默认"},
    "host":{"type":"string","description":"SSH 主机地址或 SSH 别名；添加工作目录时是机器名"},
    "port":{"type":"integer","description":"SSH 端口"},
    "user":{"type":"string","description":"SSH 用户"},
    "password":{"type":"string","description":"SSH 密码（可留空表示不改）"},
    "identity_file":{"type":"string","description":"SSH 私钥路径"},
    "auto_connect":{"type":"boolean","description":"网关启动时自动连接"},
    "base_url":{"type":"string","description":"模型接口地址，到 /v1 为止"},
    "api_key":{"type":"string","description":"模型 API key（可留空表示不改）"},
    "proxy":{"type":"string","description":"代理；none 表示直连"},
    "kind":{"type":"string","enum":["openai","mock"],"description":"接口类型"},
    "endpoint":{"type":"string","description":"模型所属接口的 ID"},
    "model":{"type":"string","description":"接口里的模型名"},
    "label":{"type":"string","description":"模型显示名"},
    "context_window":{"type":"integer","description":"上下文窗口"},
    "max_tokens":{"type":"integer","description":"最大输出"},
    "max_history_tokens":{"type":"integer","description":"历史上限"},
    "thinking":{"type":"string","description":"默认思考强度"},
    "thinking_levels":{"type":"array","items":{"type":"string"},"description":"可选思考强度"},
    "modalities":{"type":"array","items":{"type":"string"},"description":"能接收的输入类型：text、image"},
    "path":{"type":"string","description":"工作目录路径，或者 ssh_setup 时要添加并挂载的目录"},
    "sandbox_mode":{"type":"string","enum":["auto","on","off",""],"description":"workspace_sandbox：工作目录隔离覆盖，空字符串跟随 agent。agent_save 用 settings.sandbox_mode，null 跟随全局"},
    "default":{"type":"boolean","description":"设为新会话默认"},
    "all":{"type":"boolean","description":"history_delete：删除全部历史和归档"},
    "from":{"type":"integer","description":"history_delete：起始轮次（从 0 开始）"},
    "to":{"type":"integer","description":"history_delete：结束轮次（含）"},
    "mount":{"type":"boolean","description":"workspace_mount：true 挂上，false 取下"},
    "agent":{"type":"string","description":"agent_save / bot_bind：agent 的 ID（先用 Gateway agents 查）；agent_save 时留空表示新建"},
    "command":{"type":"string","description":"mcp_save：在本机或某台电脑上起的 stdio 进程，比如 npx、uvx、python（和 url 二选一）"},
    "args":{"type":"array","items":{"type":"string"},"description":"mcp_save：命令行参数，一项一个，比如 [\"-y\",\"@modelcontextprotocol/server-filesystem\",\"/home/me\"]"},
    "url":{"type":"string","description":"mcp_save：远程 MCP 服务的 streamable HTTP 地址（和 command 二选一）"},
    "cwd":{"type":"string","description":"mcp_save：进程的工作目录，留空用默认"},
    "machine":{"type":"string","description":"mcp_save：进程在哪台电脑上跑，写在线内核的名字（Gateway mcp 里的 machines）；空字符串或 gateway 表示网关本机"},
    "env":{"type":"object","additionalProperties":{"type":"string"},"description":"mcp_save：环境变量，密钥放这里（加密保存，查的时候只给名字）；值写空字符串表示沿用已存的"},
    "enabled":{"type":"boolean","description":"mcp_save：是否启用"},
    "tools":{"type":"array","items":{"type":"string"},"description":"mcp_save：只开放哪些工具（服务里的原名）；写成 \"!名字\" 表示禁用这个；空数组表示全部"},
    "agents":{"type":"array","items":{"type":"string"},"description":"mcp_save：给哪些 agent 用（agent ID）；空数组表示所有 agent"},
    "approval":{"type":"string","enum":["never","per_turn","always"],"description":"mcp_save：调用前要不要管理员批准：never 不用；per_turn 一轮里第一次要批；always 每次都要批（放行模式下都不问）"}
  },
  "required":["action"]
}`

func (a *App) gatewayTools() []proto.ToolSpec {
	return []proto.ToolSpec{
		{Name: "Gateway", Description: "查询 nano-agent 网关的真实状态：内核、机器、模型和接口（不含 key）、SSH 连接（不含密码）、工作目录、电脑配对、~/.ssh/config 里的别名、这个会话的上下文和对话轮次、网络搜索、设置项、MCP 服务。配置东西之前先用它查清楚。", InputSchema: json.RawMessage(gatewaySchema)},
		{Name: "Message", Description: "给网关里的一个群（房间）、某个 agent，或者你自己的某个私聊（chat:）发一条消息；发给 agent 时把 wait 设成 true 可以等对方回话（最多 5 分钟）。群里的成员都看得到，对方是那个一直在的 agent，不是临时开出来的。发到私聊的消息对方会在聊天软件里收到，他回了就在那个聊天里接着聊。给 chat:<自己的 life 会话> 留言不会发给任何人，只是放进它的内心。", InputSchema: json.RawMessage(messageSchema)},
		{Name: "GatewayAdmin", Description: "修改 nano-agent 网关：模型接口/模型、SSH 连接、工作目录、电脑配对、设置项（settings_set：上下文整理、网络搜索、显示档位等）、agent、MCP 服务（mcp_save 加或改、mcp_delete、mcp_test 现连一次看有哪些工具），删除会话历史，重启（restart）和更新到最新版本（update）。每次修改都要管理员审批。能查到的先用 Gateway 查，失败不要反复重试；密钥会加密保存，不要在结果里回显。", InputSchema: json.RawMessage(gatewayAdminSchema)},
	}
}

// gatewayQuery 执行 Gateway 查询。在独立 goroutine 里跑，不持有 App 锁。
func (a *App) gatewayQuery(sid, raw string, reply func(bool, string)) {
	var args struct {
		What  string `json:"what"`
		Lines int    `json:"lines"`
		Group string `json:"group"`
	}
	json.Unmarshal([]byte(raw), &args)
	if args.What == "" {
		args.What = "status"
	}
	var out any
	var err error
	switch args.What {
	case "session":
		out, _ = a.Session(sid)
	case "status":
		st := a.Status()
		out = map[string]any{
			"running": st.Running, "pid": st.PID, "version": st.Version,
			"started": st.Started, "uptime_ms": st.UptimeMS, "mode": st.Mode,
			"web_url": st.WebURL, "kernel_listen": st.KernelListen,
			"kernels": st.Kernels, "machines": st.Machines, "channels": st.Channels,
			"workspaces": a.Workspaces(), "default_model": st.DefaultModel,
			"pending_restart": st.PendingRestart, "recent_errors": st.RecentErrors, "update": st.Update,
		}
	case "logs":
		n := args.Lines
		if n <= 0 {
			n = 50
		}
		if n > 500 {
			n = 500
		}
		out = map[string]any{"lines": logTail(a.cfg.Session.DataDir, n, a.knownSecrets())}
	case "models":
		st := a.ModelStore()
		if st == nil {
			err = errors.New("模型列表没有启用")
			break
		}
		eps := []map[string]any{}
		for _, e := range st.Endpoints() {
			eps = append(eps, map[string]any{"id": e.ID, "name": e.Name, "kind": e.Kind, "base_url": e.BaseURL, "proxy": e.Proxy, "has_key": e.APIKey != ""})
		}
		ms := []map[string]any{}
		for _, m := range st.Models() {
			ms = append(ms, map[string]any{"id": m.ID, "endpoint": m.Endpoint, "name": m.Name, "label": m.Label,
				"context_window": m.ContextWindow, "max_tokens": m.MaxTokens, "max_history_tokens": m.MaxHistory,
				"thinking": m.Thinking, "thinking_levels": m.ThinkingLevels, "modalities": m.Modalities,
				"default": m.ID == st.Default()})
		}
		out = map[string]any{"endpoints": eps, "models": ms, "default": st.Default()}
	case "ssh":
		if a.hosts == nil {
			out = []any{}
			break
		}
		out = a.hosts.Infos()
	case "workspaces":
		info, _ := a.Session(sid)
		out = map[string]any{"all": a.Workspaces(), "mounted": info.Workspaces, "mode": info.Mode}
	case "pairing":
		if a.hub == nil {
			out = map[string]any{"pending": []any{}, "paired": []any{}}
			break
		}
		out = map[string]any{"pending": a.hub.PairRequests(), "paired": a.hub.PairedDevices()}
	case "ssh_config":
		out = map[string]any{"hosts": sshAliases()}
	case "turns":
		out = a.Turns(sid, args.Lines)
	case "search":
		out = a.SearchInfo()
	case "settings":
		out, err = a.settingsView(args.Group)
	case "agents":
		out = map[string]any{"agents": a.AgentList(), "bots": a.Bots()}
	case "mcp":
		out, err = a.mcpView()
	default:
		err = fmt.Errorf("不认识的查询 %q", args.What)
	}
	if err != nil {
		reply(false, a.safeToolText(err.Error()))
		return
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	reply(true, a.safeToolText(string(b)))
}

func sshAliases() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.EqualFold(f[0], "host") {
			continue
		}
		for _, name := range f[1:] {
			if name == "" || strings.HasPrefix(name, "!") || strings.ContainsAny(name, "*?[") || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

type hostAdmin interface {
	Infos() []host.Info
	Save(host.Connection) (string, error)
	Delete(string) error
	Connect(string) error
	Disconnect(string) error
}

// gatewayArgs 是 GatewayAdmin 的参数。每个字段都写明 json 名字：参数表里是 base_url、api_key 这种
// 带下划线的写法，不写标签的话 Go 对不上（以前 AI 配模型接口时地址和 key 都丢了）。
// TestGatewayAdminSchemaFields 保证参数表里的每个参数这里都有。
type gatewayArgs struct {
	HistoryDelete

	Key   string `json:"key"`
	Value any    `json:"value"`

	Action         string         `json:"action"`
	ID             string         `json:"id"`
	Node           string         `json:"node"`
	Name           string         `json:"name"`
	Host           string         `json:"host"`
	User           string         `json:"user"`
	Password       string         `json:"password"`
	IdentityFile   string         `json:"identity_file"`
	Port           int            `json:"port"`
	ContextWindow  int            `json:"context_window"`
	MaxTokens      int            `json:"max_tokens"`
	MaxHistory     int            `json:"max_history_tokens"`
	AutoConnect    bool           `json:"auto_connect"`
	Default        bool           `json:"default"`
	SandboxMode    string         `json:"sandbox_mode"`
	Mount          *bool          `json:"mount"`
	BaseURL        string         `json:"base_url"`
	APIKey         string         `json:"api_key"`
	Proxy          string         `json:"proxy"`
	Kind           string         `json:"kind"`
	Endpoint       string         `json:"endpoint"`
	Model          string         `json:"model"`
	Label          string         `json:"label"`
	Thinking       string         `json:"thinking"`
	Path           string         `json:"path"`
	ThinkingLevels []string       `json:"thinking_levels"`
	Modalities     []string       `json:"modalities"`
	What           string         `json:"what"`
	Lines          int            `json:"lines"`
	Emoji          string         `json:"emoji"`
	Style          string         `json:"style"`
	Agent          string         `json:"agent"`
	Settings       map[string]any `json:"settings"`

	// MCP 服务（mcp_save）：没写的字段保持原样
	Command  string            `json:"command"`
	Args     []string          `json:"args"`
	URL      string            `json:"url"`
	Cwd      *string           `json:"cwd"`
	Machine  *string           `json:"machine"`
	Env      map[string]string `json:"env"`
	Enabled  *bool             `json:"enabled"`
	Tools    []string          `json:"tools"`
	Agents   []string          `json:"agents"`
	Approval string            `json:"approval"`
}

func (a *App) gatewayAdminCall(sid, who, raw string, reply func(bool, string)) {
	var args gatewayArgs
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		reply(false, a.safeToolText("参数不是合法的 JSON 对象："+err.Error()))
		return
	}
	if args.Action == "" {
		reply(false, "缺少 action")
		return
	}
	if !a.isAdmin(who) {
		reply(false, "只有管理员能修改网关")
		return
	}
	res, err := a.gatewayAdmin(sid, args)
	target := args.Action
	if args.ID != "" {
		target += ":" + args.ID
	} else if args.Name != "" {
		target += ":" + args.Name
	} else if args.Host != "" {
		target += ":" + args.Host
	}
	detail := ""
	if err != nil {
		detail = a.safeToolText(err.Error())
	}
	a.log.Info("GatewayAdmin", "who", who, "sid", sid, "action", args.Action, "target", target, "ok", err == nil, "error", detail)
	if err != nil {
		reply(false, a.safeToolText(err.Error()))
		return
	}
	reply(true, res)
}

func (a *App) gatewayAdmin(sid string, args gatewayArgs) (string, error) {
	switch args.Action {
	case "mcp_save":
		return a.mcpSave(args)
	case "mcp_delete":
		return a.mcpDelete(args)
	case "mcp_test":
		return a.mcpTest(args)
	case "model_endpoint_save":
		st := a.ModelStore()
		if st == nil {
			return "", errors.New("模型列表没有启用")
		}
		e, err := st.SaveEndpoint(models.Endpoint{ID: args.ID, Name: args.Name, Kind: args.Kind, BaseURL: args.BaseURL, APIKey: args.APIKey, Proxy: args.Proxy})
		if err != nil {
			return "", err
		}
		a.ModelsChanged()
		return "接口已保存：" + e.ID, nil
	case "model_endpoint_delete":
		st := a.ModelStore()
		if st == nil {
			return "", errors.New("模型列表没有启用")
		}
		if err := st.DeleteEndpoint(args.ID); err != nil {
			return "", err
		}
		a.ModelsChanged()
		return "接口已删除：" + args.ID, nil
	case "model_fetch":
		st := a.ModelStore()
		if st == nil {
			return "", errors.New("模型列表没有启用")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		list, err := st.Fetch(ctx, args.ID)
		if err != nil {
			return "", err
		}
		b, _ := json.MarshalIndent(list, "", "  ")
		return string(b), nil
	case "model_save":
		st := a.ModelStore()
		if st == nil {
			return "", errors.New("模型列表没有启用")
		}
		m, err := st.SaveModel(models.Model{Endpoint: args.Endpoint, Name: args.Model, Label: args.Label,
			ContextWindow: args.ContextWindow, MaxTokens: args.MaxTokens, MaxHistory: args.MaxHistory,
			Thinking: args.Thinking, ThinkingLevels: args.ThinkingLevels, Modalities: args.Modalities})
		if err != nil {
			return "", err
		}
		a.ModelsChanged()
		return "模型已保存：" + m.ID, nil
	case "model_delete":
		st := a.ModelStore()
		if st == nil {
			return "", errors.New("模型列表没有启用")
		}
		if err := st.DeleteModel(args.ID); err != nil {
			return "", err
		}
		a.ModelsChanged()
		return "模型已删除：" + args.ID, nil
	case "model_default":
		st := a.ModelStore()
		if st == nil {
			return "", errors.New("模型列表没有启用")
		}
		if err := st.SetDefault(args.ID); err != nil {
			return "", err
		}
		a.ModelsChanged()
		return "默认模型已设为 " + args.ID, nil
	case "model_use":
		id := args.ID
		if err := a.UpdateSession(sid, SessionPatch{Model: &id}); err != nil {
			return "", err
		}
		return "这个会话的模型已切换为 " + args.ID, nil
	case "model_test":
		return a.testModel(args.ID)
	case "ssh_save":
		ha, err := a.hostAdmin()
		if err != nil {
			return "", err
		}
		id, err := ha.Save(host.Connection{ID: args.ID, Name: args.Name, Host: args.Host, Port: args.Port, User: args.User,
			IdentityFile: args.IdentityFile, Password: args.Password, AutoConnect: args.AutoConnect})
		if err != nil {
			return "", err
		}
		return "SSH 已保存：" + id, nil
	case "ssh_delete":
		ha, err := a.hostAdmin()
		if err != nil {
			return "", err
		}
		if err := ha.Delete(args.ID); err != nil {
			return "", err
		}
		return "SSH 已删除：" + args.ID, nil
	case "ssh_connect":
		ha, err := a.hostAdmin()
		if err != nil {
			return "", err
		}
		if err := ha.Connect(args.ID); err != nil {
			return "", err
		}
		return "正在连接 " + args.ID, nil
	case "ssh_disconnect":
		ha, err := a.hostAdmin()
		if err != nil {
			return "", err
		}
		if err := ha.Disconnect(args.ID); err != nil {
			return "", err
		}
		return "已断开 " + args.ID, nil
	case "ssh_setup":
		return a.sshSetup(sid, args)
	case "workspace_add":
		w, err := a.AddWorkspace(context.Background(), args.Host, args.Path, args.Default, args.SandboxMode)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(w)
		return "工作目录已添加：" + string(b), nil
	case "workspace_delete":
		if err := a.RemoveWorkspace(args.ID); err != nil {
			return "", err
		}
		return "工作目录已删除：" + args.ID, nil
	case "workspace_mount":
		info, ok := a.Session(sid)
		if !ok {
			return "", errors.New("没有这个会话")
		}
		var ids []string
		for _, w := range info.Workspaces {
			ids = append(ids, w.ID)
		}
		on := true
		if args.Mount != nil {
			on = *args.Mount
		}
		if on && !strings.Contains(strings.Join(ids, "\x00"), args.ID) {
			ids = append(ids, args.ID)
		}
		if !on {
			ids = removeString(ids, args.ID)
		}
		idsCopy := ids
		if err := a.UpdateSession(sid, SessionPatch{Workspaces: &idsCopy}); err != nil {
			return "", err
		}
		if on {
			return "工作目录已挂到本会话：" + args.ID, nil
		}
		return "工作目录已取下：" + args.ID, nil
	case "workspace_sandbox":
		if err := a.SetWorkspaceSandbox(args.ID, args.SandboxMode); err != nil {
			return "", err
		}
		return "工作目录执行隔离已保存", nil
	case "workspace_default":
		if err := a.SetDefaultWorkspace(args.ID, args.Default); err != nil {
			return "", err
		}
		if args.Default {
			return "新会话会默认挂上 " + args.ID, nil
		}
		return "新会话不再默认挂 " + args.ID, nil
	case "pair_allow", "pair_deny":
		if a.hub == nil {
			return "", errors.New("配对没有启用")
		}
		if err := a.hub.DecidePair(args.ID, a.isAdminChoice(args.Action)); err != nil {
			return "", err
		}
		return "配对已处理：" + args.ID, nil
	case "pair_remove":
		if a.hub == nil {
			return "", errors.New("配对没有启用")
		}
		if err := a.hub.Unpair(args.Node); err != nil {
			return "", err
		}
		return "已移除配对：" + args.Node, nil
	case "history_delete":
		a.mu.Lock()
		err := a.deleteHistory(sid, args.HistoryDelete, true)
		a.mu.Unlock()
		return "指定历史已删除；摘要可能保留概要，可在网页清空摘要", err
	case "settings_set":
		return a.setSetting(args.Key, args.Value)
	case "agent_save":
		p := AgentPatch{}
		if args.Name != "" {
			p.Name = &args.Name
		}
		if args.Emoji != "" {
			p.Emoji = &args.Emoji
		}
		if args.Model != "" {
			p.Model = &args.Model
		}
		if args.Thinking != "" {
			p.Thinking = &args.Thinking
		}
		if args.Style != "" {
			p.Style = &args.Style
		}
		ip := args.ID
		if ip == "" && args.Agent != "" {
			ip = args.Agent
		}
		p.Settings = args.Settings
		if ip == "" {
			if strings.TrimSpace(args.Name) == "" {
				return "", errors.New("新建 agent 要给 name")
			}
			info, err := a.CreateAgent(p)
			if err != nil {
				return "", err
			}
			return "已经建好 agent " + info.ID + "（" + info.Name + "）", nil
		}
		if _, ok := a.Agent(ip); !ok {
			return "", errors.New("没有这个 agent")
		}
		info, err := a.UpdateAgent(ip, p)
		if err != nil {
			return "", err
		}
		return "agent 已更新：" + info.ID + "（" + info.Name + "）", nil
	case "agent_delete":
		if err := a.DeleteAgent(args.ID); err != nil {
			return "", err
		}
		return "agent 已删除：" + args.ID, nil
	case "bot_bind":
		if args.ID == "" || args.Agent == "" {
			return "", errors.New("bot_bind 要同时给 id（机器人）和 agent（agent ID）")
		}
		if _, ok := a.Agent(args.Agent); !ok {
			return "", errors.New("没有这个 agent")
		}
		if _, err := a.UpdateBot(args.ID, BotPatch{Agent: &args.Agent}); err != nil {
			return "", err
		}
		return "机器人 " + args.ID + " 已经绑到 " + args.Agent, nil
	case "restart":
		if err := a.RequestRestart(RestartRequest{By: "gateway-admin", SID: sid}); err != nil {
			return "", err
		}
		return "网关正在重启…", nil
	case "update":
		ctx, cancel := context.WithTimeout(a.ctx, 15*time.Minute)
		defer cancel()
		return a.RunUpdate(ctx, RestartRequest{By: "gateway-admin", SID: sid})
	}
	return "", fmt.Errorf("不认识的 action %q", args.Action)
}

func (a *App) isAdminChoice(action string) bool { return action == "pair_allow" }

// safeToolText 把工具结果/错误里网关自己知道的密钥打星（不管 redact 配置）。
func (a *App) safeToolText(s string) string { return redact.ToolResult(s, a.knownSecrets()) }

func removeString(xs []string, s string) []string {
	out := xs[:0]
	for _, x := range xs {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func (a *App) hostAdmin() (hostAdmin, error) {
	if a.hosts == nil {
		return nil, errors.New("没有连接 SSH 管理器")
	}
	h, ok := a.hosts.(hostAdmin)
	if !ok {
		return nil, errors.New("当前没有可管理的 SSH 连接")
	}
	return h, nil
}

var sshSetupTimeout = 25 * time.Second

func (a *App) sshSetup(sid string, args gatewayArgs) (string, error) {
	ha, err := a.hostAdmin()
	if err != nil {
		return "", err
	}
	id, err := ha.Save(host.Connection{ID: args.ID, Name: args.Name, Host: args.Host, Port: args.Port, User: args.User,
		IdentityFile: args.IdentityFile, Password: args.Password, AutoConnect: true})
	if err != nil {
		return "", err
	}
	if err := ha.Connect(id); err != nil {
		return "", err
	}
	deadline := time.Now().Add(sshSetupTimeout)
	var last host.Info
	for time.Now().Before(deadline) {
		for _, info := range ha.Infos() {
			if info.ID == id {
				last = info
				break
			}
		}
		if last.Ready {
			break
		}
		if last.State == "error" || last.State == "offline" {
			return "", fmt.Errorf("连接失败：%s", last.Detail)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !last.Ready {
		return "", fmt.Errorf("连接超时：%s", last.Detail)
	}
	msg := "SSH 已在线：" + last.Name
	if args.Path != "" {
		w, err := a.AddWorkspace(context.Background(), last.Name, args.Path, false)
		if err != nil {
			return msg + "；添加工作目录失败：" + err.Error(), nil
		}
		info, _ := a.Session(sid)
		var ids []string
		for _, x := range info.Workspaces {
			ids = append(ids, x.ID)
		}
		if !strings.Contains(strings.Join(ids, "\x00"), w.ID) {
			ids = append(ids, w.ID)
		}
		idsCopy := ids
		if err := a.UpdateSession(sid, SessionPatch{Workspaces: &idsCopy}); err != nil {
			return msg + "；工作目录已添加 " + w.ID + "，但挂到本会话失败：" + err.Error(), nil
		}
		msg += "；已添加并挂载工作目录 " + w.ID
	}
	return msg, nil
}

func (a *App) testModel(id string) (string, error) {
	a.mu.Lock()
	st, nc := a.models, a.newClient
	a.mu.Unlock()
	if st == nil || nc == nil {
		return "", errors.New("模型列表没有启用")
	}
	m, e, ok := st.Find(id)
	if !ok {
		return "", errors.New("没有这个模型")
	}
	client, err := nc(st.LLMConfig(m, e))
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	res, err := client.Chat(ctx, llm.Request{Messages: []llm.Message{{Role: "user", Content: "回复 ok 两个字"}}, MaxTokens: 32}, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("模型 %s 正常（%d ms）：%s", id, time.Since(start).Milliseconds(), res.Content), nil
}

// settingChangeable：聊天里能改的设置项。要重启才生效的（监听地址、网页地址等）、只能在配置文件里改的、
// 渠道 token 和网页密码这类密钥不开放；搜索 key 可以改（加密保存）。
func settingChangeable(f config.Field) (bool, string) {
	switch {
	case f.ReadOnly:
		return false, "只能在配置文件里改"
	case f.Restart:
		return false, "要重启网关才生效，请在网页拓扑图的「" + nodeLabel(f.Group) + "」节点里改"
	case f.Type == "secret" && f.Key != "search.api_key":
		return false, "这类密钥请在网页拓扑图的「" + nodeLabel(f.Group) + "」节点里填"
	}
	return true, ""
}

// settingsView 是 Gateway {"what":"settings"} 的结果：当前值、来源、说明，密钥只说有没有设置
func (a *App) settingsView(group string) (any, error) {
	if a.confStore == nil {
		return nil, errors.New("设置存储没有接上")
	}
	out := []map[string]any{}
	for _, f := range a.confStore.Describe() {
		if group != "" && f.Group != group {
			continue
		}
		item := map[string]any{"key": f.Key, "group": f.Group, "node": nodeLabel(f.Group), "label": f.Label, "type": f.Type, "value": f.Value, "source": f.Source}
		if f.Section != "" {
			item["section"] = f.Section
		}
		if f.Help != "" {
			item["help"] = f.Help
		}
		if len(f.Options) > 0 {
			item["options"] = f.Options
		}
		if f.Type == "secret" {
			item["is_set"] = f.IsSet
			delete(item, "value")
		}
		if ok, why := settingChangeable(f.Field); !ok {
			item["changeable"] = false
			item["why"] = why
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有 %q 这一组设置", group)
	}
	return out, nil
}

// nodeLabel 是设置项所在节点的名字（冒号前面那段），告诉用户去哪儿改
func nodeLabel(group string) string {
	n, ok := config.NodeByID(group)
	if !ok {
		return group
	}
	name, _, _ := strings.Cut(n.Label, "：")
	return name
}

// setSetting 是 GatewayAdmin settings_set：和网页节点面板里改一项一样，写进 data/config.json，立即生效
func (a *App) setSetting(key string, value any) (string, error) {
	if a.confStore == nil {
		return "", errors.New("设置存储没有接上")
	}
	f, ok := config.FieldByKey(key)
	if !ok {
		return "", fmt.Errorf("没有这个设置项：%s（先用 Gateway settings 查）", key)
	}
	if ok, why := settingChangeable(f); !ok {
		return "", fmt.Errorf("「%s」%s", f.Label, why)
	}
	if err := a.confStore.Set(map[string]any{key: value}); err != nil {
		return "", err
	}
	a.publish(events.Event{T: "config"})
	switch {
	case value == nil:
		return fmt.Sprintf("已恢复「%s」为配置文件里的值", f.Label), nil
	case f.Type == "secret":
		return fmt.Sprintf("已保存「%s」（加密存储）", f.Label), nil
	}
	b, _ := json.Marshal(value)
	return fmt.Sprintf("已把「%s」改成 %s", f.Label, b), nil
}
