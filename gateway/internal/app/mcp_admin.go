package app

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"nano-agent/gateway/internal/mcp"
)

// AI 在聊天里查看和配置 MCP 服务：Gateway {"what":"mcp"}、GatewayAdmin 的 mcp_save / mcp_delete / mcp_test。
// 和网页「MCP 服务」节点走同一套保存、测试（SaveMCPRenamed、TestMCP）：密钥照样加密存，查的时候只给名字。
// 这些都在协程里跑，不持有 App 锁。

// mcpView 是 Gateway mcp 的结果。
func (a *App) mcpView() (any, error) {
	a.mu.Lock()
	mgr := a.mcp
	type machine struct {
		Name  string `json:"name"`
		OS    string `json:"os,omitempty"`
		Local bool   `json:"local,omitempty"` // 和网关在同一台机器上
		Old   bool   `json:"too_old,omitempty"`
	}
	var machines []machine
	for name, n := range a.nodes {
		machines = append(machines, machine{Name: name, OS: n.info.OS, Local: n.info.Local, Old: n.info.Proto != 0 && n.info.Proto < pipeProto})
	}
	a.mu.Unlock()
	if mgr == nil {
		return nil, errors.New("没有接 MCP")
	}
	sort.Slice(machines, func(i, j int) bool { return machines[i].Name < machines[j].Name })
	type tool struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
	}
	type server struct {
		MCPInfo
		ToolNames []tool `json:"tools_listed"` // 上次连上时列到的工具（交给模型的名字）
	}
	servers := []server{}
	for _, s := range a.MCPInfo() {
		s.ToolNames = nil // 下面带说明的那份更有用，不重复给
		v := server{MCPInfo: s, ToolNames: []tool{}}
		for _, t := range mgr.ServerTools(s.Name) {
			v.ToolNames = append(v.ToolNames, tool{Name: t.Exposed, Description: cutRunes(t.Description, 100)})
		}
		servers = append(servers, v)
	}
	return map[string]any{
		"servers":  servers,
		"machines": machines,
		"note":     "machine 留空就在网关本机跑；写 machines 里的名字就在那台电脑上跑（too_old 的电脑要等内核自动更新完）。env 只给名字，值加密存着。",
	}, nil
}

// mcpSave 加一个服务，或者改已有的（没写的字段保持原样）。改名时 id 写原来的名字。
func (a *App) mcpSave(args gatewayArgs) (string, error) {
	a.mu.Lock()
	store := a.mcpStore
	a.mu.Unlock()
	if store == nil {
		return "", errors.New("没有接 MCP")
	}
	name := strings.TrimSpace(args.Name)
	orig := strings.TrimSpace(args.ID)
	if name == "" {
		name = orig
	}
	if name == "" {
		return "", errors.New("mcp_save 要写 name（服务名）")
	}
	if orig == "" {
		orig = name
	}
	list := store.List()
	i := slices.IndexFunc(list, func(s mcp.Server) bool { return s.Name == orig })
	if orig != name && i < 0 {
		return "", fmt.Errorf("没有叫 %s 的 MCP 服务，改不了名", orig)
	}
	if orig != name && slices.ContainsFunc(list, func(s mcp.Server) bool { return s.Name == name }) {
		return "", fmt.Errorf("已经有叫 %s 的 MCP 服务了", name)
	}
	srv := mcp.Server{Enabled: true}
	if i >= 0 {
		srv = list[i]
		// env 只填要改的：已存的留着（值写空表示沿用），这里先拷一份免得改到 store 里的
		env := make(map[string]string, len(srv.Env))
		for k := range srv.Env {
			env[k] = "" // SaveMCPRenamed 会把空值换回原来存的（加密的）值
		}
		srv.Env = env
	}
	srv.Name = name
	if c := strings.TrimSpace(args.Command); c != "" {
		srv.Command, srv.URL = c, ""
	}
	if u := strings.TrimSpace(args.URL); u != "" {
		srv.URL, srv.Command, srv.Args = u, "", nil
	}
	if args.Args != nil {
		srv.Args = args.Args
	}
	if args.Cwd != nil {
		srv.Cwd = strings.TrimSpace(*args.Cwd)
	}
	if args.Machine != nil {
		srv.Machine = strings.TrimSpace(*args.Machine)
		if srv.Machine == "gateway" {
			srv.Machine = ""
		}
	}
	if args.Env != nil {
		if srv.Env == nil {
			srv.Env = map[string]string{}
		}
		for k, v := range args.Env {
			srv.Env[k] = v
		}
	}
	if args.Enabled != nil {
		srv.Enabled = *args.Enabled
	}
	if args.Tools != nil {
		srv.Tools = args.Tools
	}
	if args.Agents != nil {
		srv.Agents = args.Agents
	}
	if args.Approval != "" {
		srv.Approval = args.Approval
	}
	// 那台电脑不在线不拦着（可能只是暂时没连上，连上来以后会自动去起），结果里说一声
	offline := false
	if srv.Machine != "" {
		a.mu.Lock()
		_, online := a.nodes[srv.Machine]
		a.mu.Unlock()
		offline = !online
	}
	if i >= 0 {
		list[i] = srv
	} else {
		list = append(list, srv)
	}
	renames := map[string]string{}
	if orig != name {
		renames[name] = orig
	}
	if err := a.SaveMCPRenamed(list, renames); err != nil {
		return "", err
	}
	where := "网关本机"
	if srv.Machine != "" {
		where = srv.Machine + " 上"
	}
	kind := "进程（" + srv.Command + "），在" + where + "跑"
	if srv.URL != "" {
		kind = "远程地址 " + srv.URL
	}
	verb := "加好了"
	if i >= 0 {
		verb = "改好了"
	}
	state := "已启用，正在后台连接"
	switch {
	case !srv.Enabled:
		state = "没有启用"
	case offline:
		state = "已启用；" + srv.Machine + " 现在不在线，它连上来以后会自动起"
	}
	return fmt.Sprintf("MCP 服务 %s %s：%s；%s。用 mcp_test 现连一次，看能不能连上、有哪些工具（工具名是 %s__<工具>）。", name, verb, kind, state, name), nil
}

// mcpDelete 删掉一个服务。
func (a *App) mcpDelete(args gatewayArgs) (string, error) {
	a.mu.Lock()
	store := a.mcpStore
	a.mu.Unlock()
	if store == nil {
		return "", errors.New("没有接 MCP")
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		name = strings.TrimSpace(args.ID)
	}
	list := store.List()
	i := slices.IndexFunc(list, func(s mcp.Server) bool { return s.Name == name })
	if i < 0 {
		return "", fmt.Errorf("没有叫 %q 的 MCP 服务", name)
	}
	list = slices.Delete(list, i, i+1)
	if err := a.SaveMCP(list); err != nil {
		return "", err
	}
	return "MCP 服务 " + name + " 删掉了。", nil
}

// mcpTest 现连一次，把工具列出来。
func (a *App) mcpTest(args gatewayArgs) (string, error) {
	name := strings.TrimSpace(args.Name)
	if name == "" {
		name = strings.TrimSpace(args.ID)
	}
	tools, err := a.TestMCP(name)
	if err != nil {
		return "", fmt.Errorf("连不上 %s：%w", name, err)
	}
	if len(tools) == 0 {
		return name + " 连上了，但是没有工具（或者都被 tools 过滤掉了）。", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s 连上了，%d 个工具：", name, len(tools))
	for _, t := range tools {
		fmt.Fprintf(&b, "\n- %s：%s", t.Exposed, cutRunes(strings.TrimSpace(t.Description), 80))
	}
	b.WriteString("\n已经有工具快照的会话，下一次请求时会带上这些工具。")
	return b.String(), nil
}
