package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/channel"
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
	Machine   string   `json:"machine,omitempty"`
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
		info := MCPInfo{Name: s.Name, Command: s.Command, Args: s.Args, URL: s.URL, Cwd: s.Cwd, Machine: s.Machine,
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

// askMCPApproval 建一张审批单，返回等结果的 channel（批准往这里塞 true）。
// 调用方持有锁；resolve 只往缓冲 channel 里塞，不会再要锁。
func (a *App) askMCPApproval(s *sess, tool string, args json.RawMessage) (<-chan bool, int) {
	a.nextAppr++
	n := a.nextAppr
	ch := make(chan bool, 1)
	title, body := callCard(tool, args)
	desc := title + "：" + body
	from := ""
	if s.run != nil {
		from = s.run.from
	}
	ap := &approval{sid: s.sid, name: tool, detail: desc, from: from, at: a.now(),
		card: channel.ApprovalCard{N: n, Title: title, Detail: body, From: from},
		resolve: func(ok bool) {
			select {
			case ch <- ok:
			default:
			}
		}}
	a.approvals[n] = ap
	a.publish(events.Event{T: "approval", SID: s.sid, N: n, Name: tool, Text: desc, Data: map[string]any{"from": from}})
	text := fmt.Sprintf("⚠️ 需要审批 #%d\n%s\n发起人：%s\n管理员回复 /approve %d 批准，/deny %d 拒绝", n, desc, from, n, n)
	a.publish(events.Event{T: "say", SID: s.sid, Text: text})
	if s.run != nil {
		for _, o := range s.run.outs {
			approver, ok := a.chans[o.ep.ch].(channel.Approver)
			if !ok {
				a.deliver(o.ep, text, "")
				continue
			}
			ep, card := o.ep, ap.card
			go func() {
				ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
				defer cancel()
				ref, err := approver.SendApproval(ctx, ep.chat, card)
				a.mu.Lock()
				defer a.mu.Unlock()
				if err != nil {
					a.deliver(ep, text, "")
					return
				}
				if cur := a.approvals[n]; cur != nil {
					cur.refs = append(cur.refs, approvalRef{ep: ep, ref: ref})
				}
			}()
		}
	}
	return ch, n
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
	dial := func() {
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
		a.log.Info("MCP 工具调用完成", "sid", sid, "tool", m.Name, "from", from, "bytes", len(text))
		reply(ok, text)
	}
	// approval=always 的服务：每次调用都先问管理员（/approve、网页按钮都能批）
	if srv, ok := mg.Server(server); ok && srv.Approval == mcp.ApprovalAlways {
		wait, n := a.askMCPApproval(s, m.Name, m.Args)
		limit := a.cfg.Agent.ApprovalLimit()
		a.log.Info("MCP 工具等审批", "sid", sid, "tool", m.Name, "n", n)
		go func() {
			var timeout <-chan time.Time
			if limit > 0 {
				t := time.NewTimer(limit)
				defer t.Stop()
				timeout = t.C
			}
			select {
			case ok := <-wait:
				if !ok {
					a.mu.Lock()
					reply(false, "管理员拒绝了这次调用")
					a.mu.Unlock()
					return
				}
			case <-timeout:
				a.mu.Lock()
				a.dropApproval(n, "等审批超时")
				reply(false, "等审批超时，没有执行")
				a.mu.Unlock()
				return
			case <-a.ctx.Done():
				return
			}
			dial()
		}()
		return
	}
	go dial()
}
