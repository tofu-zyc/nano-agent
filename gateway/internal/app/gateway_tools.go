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
    "what":{"type":"string","enum":["status","models","ssh","workspaces","pairing","ssh_config","logs"],
      "description":"要查什么：网关状态、模型接口、SSH 连接、工作目录、电脑配对、~/.ssh/config 里的别名，或网关日志"},
    "lines":{"type":"integer","description":"logs 要最近几行，默认 50，最多 500"}
  }
}`

const gatewayAdminSchema = `{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":[
      "model_endpoint_save","model_endpoint_delete","model_fetch","model_save","model_delete","model_default","model_test","model_use",
      "ssh_save","ssh_delete","ssh_connect","ssh_disconnect","ssh_setup",
      "workspace_add","workspace_delete","workspace_mount","workspace_default",
      "pair_allow","pair_deny","pair_remove","restart"
    ],"description":"要做什么"},
    "id":{"type":"string","description":"接口 / 模型 / SSH / 工作目录的 ID，或配对请求 ID"},
    "node":{"type":"string","description":"要移除的已配对电脑名字"},
    "name":{"type":"string","description":"接口或 SSH 连接的名字"},
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
    "default":{"type":"boolean","description":"设为新会话默认"},
    "mount":{"type":"boolean","description":"workspace_mount：true 挂上，false 取下"}
  },
  "required":["action"]
}`

func (a *App) gatewayTools() []proto.ToolSpec {
	return []proto.ToolSpec{
		{Name: "Gateway", Description: "查询 nano-agent 网关的真实状态：内核、机器、模型和接口（不含 key）、SSH 连接（不含密码）、工作目录、电脑配对、~/.ssh/config 里的别名。配置东西之前先用它查清楚。", InputSchema: json.RawMessage(gatewaySchema)},
		{Name: "GatewayAdmin", Description: "修改 nano-agent 网关：模型接口/模型、SSH 连接、工作目录、电脑配对。每次修改都要管理员审批。能查到的先用 Gateway 查，失败不要反复重试；密钥会加密保存，不要在结果里回显。", InputSchema: json.RawMessage(gatewayAdminSchema)},
	}
}

// gatewayQuery 执行 Gateway 查询。在独立 goroutine 里跑，不持有 App 锁。
func (a *App) gatewayQuery(sid, raw string, reply func(bool, string)) {
	var args struct {
		What  string `json:"what"`
		Lines int    `json:"lines"`
	}
	json.Unmarshal([]byte(raw), &args)
	if args.What == "" {
		args.What = "status"
	}
	var out any
	var err error
	switch args.What {
	case "status":
		st := a.Status()
		out = map[string]any{
			"running": st.Running, "pid": st.PID, "version": st.Version,
			"started": st.Started, "uptime_ms": st.UptimeMS, "mode": st.Mode,
			"web_url": st.WebURL, "kernel_listen": st.KernelListen,
			"kernels": st.Kernels, "machines": st.Machines, "channels": st.Channels,
			"workspaces": a.Workspaces(), "default_model": st.DefaultModel,
			"pending_restart": st.PendingRestart, "recent_errors": st.RecentErrors,
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

type gatewayArgs struct {
	Action, ID, Node, Name, Host, User, Password, IdentityFile           string
	Port, ContextWindow, MaxTokens, MaxHistory                           int
	AutoConnect, Default                                                 bool
	Mount                                                                *bool
	BaseURL, APIKey, Proxy, Kind, Endpoint, Model, Label, Thinking, Path string
	ThinkingLevels, Modalities                                           []string
	What                                                                 string
	Lines                                                                int
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
		w, err := a.AddWorkspace(context.Background(), args.Host, args.Path, args.Default)
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
	case "restart":
		if err := a.RequestRestart(RestartRequest{By: "gateway-admin", SID: sid}); err != nil {
			return "", err
		}
		return "网关正在重启…", nil
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
