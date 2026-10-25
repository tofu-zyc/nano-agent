package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

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
func (a *App) SaveMCP(list []mcp.Server) error { return a.SaveMCPRenamed(list, nil) }

// SaveMCPRenamed 同上，renames 是新名字 → 旧名字：改名的时候密钥和工具列表要跟着搬。
func (a *App) SaveMCPRenamed(list []mcp.Server, renames map[string]string) error {
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
		renamed := false
		if !ok {
			if from, has := renames[list[i].Name]; has {
				prev, ok = old[from]
				renamed = ok
			}
		}
		if !ok {
			continue
		}
		if renamed {
			if list[i].Env == nil {
				list[i].Env = map[string]string{}
			}
			for k, v := range prev.Env {
				if _, has := list[i].Env[k]; !has {
					list[i].Env[k] = v
				}
			}
			if len(list[i].Tools) == 0 {
				list[i].Tools = prev.Tools
			}
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
		mgr.Warm(20 * time.Second) // 先连一遍，会话那边只读缓存
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

// mcpTools 是某个 agent 现在能用的 MCP 工具定义。调用方持有锁 —— 这里只能读缓存，
// 绝不能去连服务（连接会等锁、而锁在这里，直接死锁）。
func mcpTools(m *mcp.Manager, agent string) []llm.Tool {
	if m == nil {
		return nil
	}
	infos := m.CachedTools(agent)
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

// askMCPApproval 建一张审批单，返回等结果的 channel（批准往这里塞 true）。
// 调用方持有锁；resolve 只往缓冲 channel 里塞，不会再要锁。
func (a *App) askMCPApproval(s *sess, execID, tool string, args json.RawMessage) (<-chan bool, int) {
	a.nextAppr++
	n := a.nextAppr
	ch := make(chan bool, 1)
	title, body := callCard(tool, args)
	desc := title + "：" + body
	from := ""
	if s.run != nil {
		from = s.run.from
	}
	ap := &approval{sid: s.sid, id: execID, name: tool, detail: desc, from: from, at: a.now(),
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

// mcpSignature 是 MCP 配置的指纹：加、删、启用、换机器都会让它变，
// 工具快照据此重建（不然新加的服务在已有会话里看不见）。调用方持有锁。
func (a *App) mcpSignature() string {
	if a.mcpStore == nil {
		return ""
	}
	list := a.mcpStore.List()
	names := make([]string, 0, len(list))
	for _, s := range list {
		if !s.Enabled {
			continue
		}
		names = append(names, s.Name+"@"+s.Machine)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// mimeExt 是图片的扩展名（媒体库里按它命名）。
func mimeExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ".png"
}

// cancelMCP 是内核说这次调用超时/不要了（exec_cancel）：撤掉挂着的审批，按拒绝处理。
// 不这么做的话，审批卡还挂着，管理员晚点批了工具照样执行，可模型早就被告知超时了。
// 调用方持有锁。
func (a *App) cancelMCP(id string) {
	for n, ap := range a.approvals {
		if ap.id != id || ap.resolve == nil {
			continue
		}
		res := ap.resolve
		delete(a.approvals, n)
		a.resolveApproval(ap, "已取消")
		a.publish(events.Event{T: "approval_done", SID: ap.sid, N: n})
		res(false)
		return
	}
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
		// 内核超时/用户停止时会发 exec_cancel：注册进去，它能把这个调用停下来
		if s.run != nil {
			if s.run.execs == nil {
				s.run.execs = map[string]context.CancelFunc{}
			}
			s.run.execs[m.ID] = cancel
			defer func() {
				a.mu.Lock()
				delete(s.run.execs, m.ID)
				a.mu.Unlock()
			}()
		}
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
		// 工具返回的图片：存下来附在工具结果上，网页的工具卡片会显示
		for _, c := range res.Content {
			img, ok := c.(*sdkmcp.ImageContent)
			if !ok || len(img.Data) == 0 || a.media == nil || s.run == nil {
				continue
			}
			att, err := a.media.Save(img.Data, "mcp-"+m.ID+mimeExt(img.MIMEType), "mcp")
			if err != nil {
				a.log.Warn("保存 MCP 返回的图片失败", "err", err)
				continue
			}
			s.run.sent[m.ID] = append(s.run.sent[m.ID], att)
			if s.run.withImages {
				s.run.images[m.ID] = append(s.run.images[m.ID], att)
			} else if !strings.Contains(text, "看不到") {
				text += "\n（图片附在工具结果上，但现在的模型不能看图）"
			}
		}
		ok := !res.IsError
		if !ok && text == "" {
			text = "工具返回了错误"
		}
		a.log.Info("MCP 工具调用完成", "sid", sid, "tool", m.Name, "from", from, "bytes", len(text))
		reply(ok, text)
	}
	// 只读模式不给 MCP（工具列表里也不会出现）
	if a.workspaceOf(s.sid).mode == "readonly" {
		reply(false, "当前是只读模式，不能用 MCP 工具")
		return
	}
	// approval=always 每次都要批；per_turn 一轮里批过一次就放行；放行模式（full）不问
	need := false
	if srv, ok := mg.Server(server); ok {
		switch srv.Approval {
		case mcp.ApprovalAlways:
			need = true
		case mcp.ApprovalPerTurn:
			need = s.run == nil || !s.run.mcpOK[server]
		}
	}
	if need && s.run != nil && s.run.full {
		need = false
	}
	if need {
		wait, n := a.askMCPApproval(s, m.ID, m.Name, m.Args)
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
				// 批准以后才记「这一轮批过了」：提前记的话，同一轮并发的第二次调用会绕过审批
				a.mu.Lock()
				if s.run != nil {
					if s.run.mcpOK == nil {
						s.run.mcpOK = map[string]bool{}
					}
					s.run.mcpOK[server] = true
				}
				a.mu.Unlock()
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
