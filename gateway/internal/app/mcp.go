package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/proto"
)

// MCP 服务（docs/PLAN-roadmap.md 阶段 11）：服务在网关这边连（本机 stdio 或远程 streamable
// HTTP），工具以 <服务>__<工具> 的名字交给模型。模型调用时内核不认识这个名字，会转回网关
// （见 kernel/src/core.c 的 FORWARDED），由 mcpCall 交给 MCP 客户端。

// MCPInfo 是一个 MCP 服务给网页看的样子（密钥只给名字，不给值）。
type MCPInfo struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"` // stdio / http
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	URL       string   `json:"url,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	EnvKeys   []string `json:"env_keys,omitempty"`
	Enabled   bool     `json:"enabled"`
	Tools     []string `json:"tools,omitempty"`
	Agents    []string `json:"agents,omitempty"`
	Approval  string   `json:"approval"`
	Online    bool     `json:"online"`
	ToolCount int      `json:"tool_count"`
}

// MCPInfo 是所有 MCP 服务和它们现在的连接状态。
func (a *App) MCPInfo() []MCPInfo {
	a.mu.Lock()
	store, mgr := a.mcpStore, a.mcp
	a.mu.Unlock()
	out := []MCPInfo{}
	if store == nil {
		return out
	}
	for _, s := range store.List() {
		info := MCPInfo{Name: s.Name, Command: s.Command, Args: s.Args, URL: s.URL, Cwd: s.Cwd,
			Enabled: s.Enabled, Tools: s.Tools, Agents: s.Agents, Approval: s.Approval}
		if s.URL != "" {
			info.Kind = "http"
		} else {
			info.Kind = "stdio"
		}
		for k := range s.Env {
			info.EnvKeys = append(info.EnvKeys, k)
		}
		sort.Strings(info.EnvKeys)
		if mgr != nil {
			info.Online, info.ToolCount = mgr.Status(s.Name)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SaveMCP 整份保存 MCP 服务列表。env 里值留空的沿用原来存着的（网页看不到明文密钥）。
func (a *App) SaveMCP(list []mcp.Server) error {
	a.mu.Lock()
	store, mgr := a.mcpStore, a.mcp
	a.mu.Unlock()
	if store == nil {
		return fmt.Errorf("没有打开 mcp.json")
	}
	old := map[string]mcp.Server{}
	for _, s := range store.List() {
		old[s.Name] = s
	}
	for i := range list {
		prev, ok := old[list[i].Name]
		if !ok {
			continue
		}
		for k, v := range list[i].Env {
			if strings.TrimSpace(v) == "" {
				if pv, has := prev.Env[k]; has {
					list[i].Env[k] = pv
				}
			}
		}
	}
	if err := store.Save(list); err != nil {
		return err
	}
	if mgr != nil {
		mgr.Reload()
	}
	a.publish(events.Event{T: "mcp"})
	return nil
}

// TestMCP 现连一个服务，把它的工具列出来（网页上的“测试”）。
func (a *App) TestMCP(name string) ([]mcp.ToolInfo, error) {
	a.mu.Lock()
	mgr := a.mcp
	a.mu.Unlock()
	if mgr == nil {
		return nil, fmt.Errorf("没有接 MCP")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return mgr.Test(ctx, name)
}

// mcpTools 是某个 agent 现在能用的 MCP 工具定义。调用方持有锁。
func mcpTools(m *mcp.Manager, agent string) []llm.Tool {
	if m == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	infos := m.ToolsFor(ctx, agent)
	out := make([]llm.Tool, 0, len(infos))
	for _, t := range infos {
		desc := t.Description
		if desc == "" {
			desc = t.Name
		}
		out = append(out, llm.Tool{Type: "function", Function: llm.ToolFunction{
			Name:        t.Exposed,
			Description: fmt.Sprintf("%s（MCP 服务 %s）", desc, t.Server),
			Parameters:  t.Schema,
		}})
	}
	return out
}

// mcpToolNames 是这些工具的名字（给 agent 的工具白名单用）。
func mcpToolNames(m *mcp.Manager, agent string) []string {
	tools := mcpTools(m, agent)
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Function.Name)
	}
	return out
}

// mcpCall 执行一次 MCP 工具调用。调用方持有锁；调用本身在协程里跑。
func (a *App) mcpCall(s *sess, m *proto.Msg, reply func(bool, string)) {
	mg := a.mcp
	if mg == nil {
		reply(false, "没有接 MCP 服务")
		return
	}
	server, tool, _ := mcp.SplitExposed(m.Name)
	var args any
	if raw := string(m.Args); raw != "" && raw != "{}" && raw != "null" {
		if err := json.Unmarshal(m.Args, &args); err != nil {
			reply(false, "MCP 工具参数不是合法的 JSON："+err.Error())
			return
		}
	}
	sid := s.sid
	from := ""
	if s.run != nil {
		from = s.run.from
	}
	a.log.Info("调 MCP 工具", "sid", sid, "tool", m.Name, "from", from)
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Minute)
		defer cancel()
		res, err := mg.Call(ctx, server, tool, args)
		a.mu.Lock()
		defer a.mu.Unlock()
		if err != nil {
			a.log.Warn("MCP 工具调用失败", "sid", sid, "tool", m.Name, "err", err)
			reply(false, err.Error())
			return
		}
		text := mcp.ResultText(res)
		ok := !res.IsError
		if !ok && text == "" {
			text = "工具返回了错误"
		}
		a.log.Info("MCP 工具调用完成", "sid", sid, "tool", m.Name, "bytes", len(text))
		reply(ok, text)
	}()
}
