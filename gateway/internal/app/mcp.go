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

// MCP 服务：由网关连接（本机 stdio 或远程 streamable
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
	ToolNames []string `json:"tool_names,omitempty"` // 上次列到的工具（服务里的原名，按白名单过滤过）

	// 诊断（v0.7.3）：设备在线不等于桌面可操作，网页上要把这些一起显示
	MachineOnline bool   `json:"machine_online"`
	Desktop       bool   `json:"desktop,omitempty"` // 看起来是 Cua Driver 这类桌面驱动（有 session/界面树/输入）
	DriverVersion string `json:"driver_version,omitempty"`
	Instructions  string `json:"instructions,omitempty"` // initialize 给的操作说明（截断）
	LastError     string `json:"last_error,omitempty"`
	LastErrorAt   int64  `json:"last_error_at,omitempty"`
}

// MCPInfo 是所有 MCP 服务和它们现在的连接状态。
func (a *App) MCPInfo() []MCPInfo {
	a.mu.Lock()
	store, mgr := a.mcpStore, a.mcp
	machines := a.machineList()
	a.mu.Unlock()
	online := map[string]bool{}
	for _, m := range machines {
		online[m.Name] = m.Online
	}
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
		if s.Machine == "" || s.Machine == "gateway" {
			info.MachineOnline = true
		} else {
			info.MachineOnline = online[s.Machine]
		}
		if mgr != nil {
			info.Online, _ = mgr.Status(s.Name)
			tools := mgr.ServerTools(s.Name)
			for _, t := range tools {
				info.ToolNames = append(info.ToolNames, t.Name)
			}
			info.ToolCount = len(info.ToolNames)
			info.Desktop = isDesktopTools(tools)
			if info.Desktop {
				info.DriverVersion = mgr.Version(s.Name)
				info.Instructions = cutRunes(mgr.Instructions(s.Name), 1200)
			}
			if msg, at := mgr.LastError(s.Name); msg != "" {
				info.LastError, info.LastErrorAt = cutRunes(msg, 300), at.UnixMilli()
			}
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
		for _, o := range a.approvalOuts(s) {
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

// mcpOffered 是这个会话的工具定义里该有的 MCP 工具：给管理员及 agent 的自主活动，
// 只读和不用工具的模式不给。contextParts 和 permSignature 共用这一处判断，
// 保证「给了什么」和「什么时候重建工具快照」是一回事。调用方持有锁。
func (a *App) mcpOffered(sid string, admin bool) []llm.Tool {
	if !admin || a.mcp == nil || isDreamSID(sid) || a.isGroupSession(sid) {
		return nil
	}
	switch a.workspaceOf(sid).mode {
	case proto.ModeReadonly, proto.ModeNone:
		return nil
	}
	return mcpTools(a.mcp, a.agentOf(sid).ID)
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

// cancelMCP 是内核说这次调用超时/不要了（exec_cancel）：撤掉挂着的审批，按没批准处理。
// 不这么做的话，审批卡还挂着，管理员晚点批了工具照样执行，可模型早就被告知超时了。
// 调用方持有锁。
func (a *App) cancelMCP(sid, id string) {
	for n, ap := range a.approvals {
		if ap.sid == sid && ap.id == id && ap.resolve != nil {
			a.dropApproval(n, "已取消")
			return
		}
	}
}

// mcpCall 执行一次 MCP 工具调用。调用方持有锁；调用本身在协程里跑。
//
// 协程里只认开始时的那一轮（r）：调用回来时这一轮可能已经结束（用户停下、超时），
// s.run 变成 nil 或者换成了下一轮，不能再往 s.run 上写东西。
func (a *App) mcpCall(s *sess, m *proto.Msg, reply func(bool, string)) {
	mg := a.mcp
	if mg == nil {
		reply(false, "没有接 MCP 服务")
		return
	}
	r := s.run
	if r == nil {
		reply(false, "这一轮已经结束了")
		return
	}
	// 只读、不用工具的模式不给 MCP（工具列表里也不会出现）
	if r.noTools || r.mode == proto.ModeReadonly || r.mode == proto.ModeNone {
		reply(false, "当前模式不能用 MCP 工具")
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
	sid, from, id := s.sid, r.from, m.ID
	// 桌面工具：自动带上这个会话的 Driver session，同一台桌面上串行执行
	info, desktopArgs, desktop := a.desktopToolInfo(sid, server, tool, args)
	if desktop {
		args = desktopArgs
	}
	// start 真正发起调用。调用方持有锁：取消函数登记在这一轮上，内核超时或用户停下时
	// 发来的 exec_cancel 能把它停下来（KernelMsg 的 TExecCancel）
	start := func() {
		lease := a.gatewayLeaseRequest(s, m, "mcp:"+server, nil, []string{"mcp:" + server})
		ctx, cancel := context.WithCancel(a.ctx)
		r.execs[id] = cancel
		go func() {
			defer cancel()
			release, acquireErr := a.acquireExecution(ctx, lease)
			if acquireErr != nil {
				a.mu.Lock()
				delete(r.execs, id)
				a.mu.Unlock()
				reply(false, acquireErr.Error())
				return
			}
			a.executionLeaseStarted(lease)
			timer := time.AfterFunc(10*time.Minute, cancel)
			defer timer.Stop()
			a.mu.Lock()
			if s.run == r {
				a.execStarted(r.node, m)
			}
			a.mu.Unlock()
			var res *sdkmcp.CallToolResult
			var err error
			if desktop {
				res, err = a.desktopCall(ctx, sid, server, info, args, r.id)
			} else {
				res, err = mg.Call(ctx, server, tool, args)
			}
			release(false) // the client RPC has ended; no claim of external side-effect rollback
			a.mu.Lock()
			defer a.mu.Unlock()
			delete(r.execs, id)
			if err != nil {
				a.log.Warn("MCP 工具调用失败", "sid", sid, "tool", m.Name, "err", err)
				reply(false, err.Error())
				return
			}
			text := cutRunes(mcp.ResultText(res), 24000)
			if !mcpOutcomeVerified(res.StructuredContent) {
				if r.unverifiedTools == nil {
					r.unverifiedTools = map[string]bool{}
				}
				r.unverifiedTools[id] = true
			}
			// 工具返回的图片：存下来附在工具结果上（网页的工具卡片显示）；模型能看图时也交给模型
			live := s.run == r
			hidden := false
			for _, c := range res.Content {
				img, ok := c.(*sdkmcp.ImageContent)
				if !ok || len(img.Data) == 0 || a.media == nil || !live {
					continue
				}
				att, err := a.media.Save(img.Data, "mcp-"+id+mimeExt(img.MIMEType), "mcp")
				if err != nil {
					a.log.Warn("保存 MCP 返回的图片失败", "err", err)
					continue
				}
				r.sent[id] = append(r.sent[id], att)
				if r.withImages {
					// 一次工具结果最多带两张图给模型：Driver 的截图很占上下文，需要再看就重新截
					if len(r.images[id]) < 2 {
						r.images[id] = append(r.images[id], att)
					}
				} else {
					hidden = true
				}
			}
			if hidden {
				text += "\n（图片附在工具结果上，但现在的模型不能看图）"
			}
			ok := !res.IsError
			if !ok && text == "" {
				text = "工具返回了错误"
			}
			a.log.Info("MCP 工具调用完成", "sid", sid, "tool", m.Name, "from", from, "bytes", len(text))
			reply(ok, text)
		}()
	}
	// approval=always 每次都要批；per_turn 一轮里批过一次就放行；放行模式（full）不问
	need := false
	if srv, ok := mg.Server(server); ok {
		switch srv.Approval {
		case mcp.ApprovalAlways:
			need = true
		case mcp.ApprovalPerTurn:
			need = !r.mcpOK[server]
		}
	}
	if r.full {
		need = false
	}
	if !need {
		start()
		return
	}
	wait, n := a.askMCPApproval(s, id, m.Name, m.Args)
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
			a.mu.Lock()
			defer a.mu.Unlock()
			if !ok {
				reply(false, "没有批准（管理员拒绝了，或者这一轮已经停了），没有执行")
				return
			}
			if s.run != r {
				reply(false, "这一轮已经结束了，没有执行")
				return
			}
			// 批准以后才记「这一轮批过了」：提前记的话，同一轮并发的第二次调用会绕过审批
			if r.mcpOK == nil {
				r.mcpOK = map[string]bool{}
			}
			r.mcpOK[server] = true
			start()
		case <-timeout:
			a.mu.Lock()
			a.dropApproval(n, "等审批超时")
			reply(false, "等审批超时，没有执行")
			a.mu.Unlock()
		case <-a.ctx.Done():
		}
	}()
}
