package app

// 桌面操作（computer use）接入：Cua Driver 是一个 MCP 服务，运行在目标机器的图形会话里，
// 网关通过现有的 MCP 客户端（本机 stdio、HTTP，或者远端内核 pipe 透传）连它。
//
// 这里处理四件事：
//   - 给每个 nano-agent 会话一个稳定的 Driver session（同一个会话的多轮操作共用，互不串台）；
//   - 同一台桌面上的工具调用串行执行（多个人/任务同时点一个桌面会互相抢输入）；
//   - 把 Driver 在 initialize 里给的 instructions 接进上下文，并提供 computer-use 技能；
//   - 会话停止、重置、删除时结束 Driver session，尽量清掉 agent 光标和录制。
//
// 注意：这里不接管 Cua 的 Agent 循环，也不把驱动编进 C 内核。结果以 Driver 的 structuredContent
// 为准：工具调用“正常返回”不等于操作成功，模型要再观察界面确认。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/session"
)

type desktopLease struct {
	sid   string
	runID uint64
	done  chan struct{}
}

func (a *App) desktopMachine(server string) string {
	if store := a.mcpStoreRef.Load(); store != nil {
		if s, ok := store.Get(server); ok && s.Machine != "" && s.Machine != "gateway" {
			a.mu.Lock()
			n := a.nodes[s.Machine]
			local := n != nil && n.info.Local
			a.mu.Unlock()
			if local {
				return "gateway"
			}
			return s.Machine
		}
	}
	return "gateway"
}

// reserveDesktop 在整轮观察、操作、核验期间保留桌面，等待时不占任何锁。
// 获取顺序为 App.mu → deskMu；停止/结束一轮时释放并唤醒等待者。
func (a *App) reserveDesktop(ctx context.Context, machine, sid string, runID uint64) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.mu.Lock()
		s := a.sessions[sid]
		if s == nil || s.run == nil || s.run.id != runID {
			a.mu.Unlock()
			return fmt.Errorf("这轮桌面任务已经结束，重新观察后再操作")
		}
		a.deskMu.Lock()
		if a.desktopLeases == nil {
			a.desktopLeases = map[string]*desktopLease{}
		}
		owner := a.desktopLeases[machine]
		if owner == nil {
			a.desktopLeases[machine] = &desktopLease{sid: sid, runID: runID, done: make(chan struct{})}
		} else if owner.sid != sid || owner.runID != runID {
			for _, lease := range a.desktopLeases {
				if lease.sid == sid && lease.runID == runID {
					a.deskMu.Unlock()
					a.mu.Unlock()
					return fmt.Errorf("这台桌面正在被其他任务使用，本轮已占用另一台桌面；请先结束当前工作并安排稍后重试，避免互相等待")
				}
			}
		}
		a.deskMu.Unlock()
		a.mu.Unlock()
		if owner == nil || (owner.sid == sid && owner.runID == runID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-owner.done:
		}
	}
}

func (a *App) releaseDesktopLeases(sid string, runID uint64) {
	a.deskMu.Lock()
	defer a.deskMu.Unlock()
	for machine, lease := range a.desktopLeases {
		if lease.sid == sid && lease.runID == runID {
			delete(a.desktopLeases, machine)
			close(lease.done)
		}
	}
}

// desktopSessionID 是一个 nano-agent 会话在 Driver 里的稳定 session：只跟会话 ID 有关，
// 换 agent、刷新页面、网关重启都不变（Driver 闲着会自动回收，重新声明是幂等的）。
func desktopSessionID(sid string) string {
	sum := sha256.Sum256([]byte("nano-agent/session/" + sid))
	return "na-" + hex.EncodeToString(sum[:6])
}

// isDesktopTools 判断这组工具是不是 Cua Driver 这类桌面工具（会话 + 界面树 + 输入）。
// 名字可能随版本变，所以看能力组合，不看服务名。
func isDesktopTools(tools []mcp.ToolInfo) bool {
	has := map[string]bool{}
	for _, t := range tools {
		has[t.Name] = true
	}
	return has["start_session"] && has["get_window_state"] && (has["click"] || has["get_desktop_state"] || has["get_accessibility_tree"])
}

// isDesktopServer 是某个 MCP 服务现在是不是桌面驱动（读缓存，不连接）。调用方持有锁。
func (a *App) isDesktopServer(name string) bool {
	if a.mcp == nil {
		return false
	}
	return isDesktopTools(a.mcp.ServerTools(name))
}

// desktopServersFor 是这个 agent 能用的桌面驱动服务。调用方持有锁。
func (a *App) desktopServersFor(agentID string) []mcp.Server {
	if a.mcp == nil || a.mcpStore == nil {
		return nil
	}
	var out []mcp.Server
	for _, s := range a.mcpStore.List() {
		if !s.Enabled || !s.ForAgent(agentID) {
			continue
		}
		if isDesktopTools(a.mcp.ServerTools(s.Name)) {
			out = append(out, s)
		}
	}
	return out
}

// desktopGuide 是接进系统提示词的桌面操作说明：Driver 自己给的 instructions（版本相关）
// 加上这个项目的几条安全规则。没有桌面服务时返回空。调用方持有锁。
func (a *App) desktopGuide(sid string) string {
	ag := a.agentOf(sid)
	servers := a.desktopServersFor(ag.ID)
	if len(servers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# 桌面操作（computer use）\n")
	b.WriteString("这一轮可以用 MCP 工具操作远程或本机的图形桌面。先观察再动手：先 get_window_state / list_windows / get_desktop_state，看清窗口和元素；工具返回 success 不代表操作已经生效，动作后要重新观察界面确认。\n")
	b.WriteString("操作结果未知（超时、断线、取消）时不要重放点击或输入：重新截图/读界面，确认当前状态再决定下一步。网关会在本轮观察、操作、核验期间保留同一桌面，其他任务会排队；用户仍可能操作桌面，动作前核对状态。\n")
	b.WriteString("需要更细的 Driver 用法时用 Skill 工具加载 computer-use，或调用它的 get_guide。\n")
	for _, s := range servers {
		ver := a.mcp.Version(s.Name)
		if ver == "" {
			ver = "版本未知"
		}
		fmt.Fprintf(&b, "\n## MCP 服务 %s（Cua Driver %s）\n", s.Name, ver)
		if in := strings.TrimSpace(a.mcp.Instructions(s.Name)); in != "" {
			b.WriteString(cutRunes(in, 4000))
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "这个会话的 Driver session 是 %s（网关自动带上，不要自己改成别的值）。\n", desktopSessionID(sid))
	}
	return strings.TrimSpace(b.String())
}

// injectDesktopSession 给桌面工具补上 session 参数：工具 schema 里有 session 才加，
// 模型自己写了的尊重它（但通常由网关统一分配）。args 可以是 map 或 nil。
func injectDesktopSession(info mcp.ToolInfo, args any, sid string) any {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(info.Schema, &schema) != nil {
		return args
	}
	if _, ok := schema.Properties["session"]; !ok {
		return args
	}
	m, ok := args.(map[string]any)
	if !ok {
		if args == nil {
			m = map[string]any{}
		} else {
			return args
		}
	}
	if cur, ok := m["session"].(string); ok && strings.TrimSpace(cur) != "" {
		return m
	}
	m["session"] = desktopSessionID(sid)
	return m
}

// desktopGate 是同一台桌面上的串行闸门。同一台机器上的桌面工具一次只跑一个，
// 免得两个任务同时点/拖。调用方持有 deskMu。
func (a *App) desktopGate(machine string) chan struct{} {
	if a.desktopGates == nil {
		a.desktopGates = map[string]chan struct{}{}
	}
	if machine == "" {
		machine = "gateway"
	}
	g := a.desktopGates[machine]
	if g == nil {
		g = make(chan struct{}, 1)
		a.desktopGates[machine] = g
	}
	return g
}

// desktopGateFor 按服务所在机器拿闸门（不依赖总锁，结束 session 时也能用）。
func (a *App) desktopGateFor(server string) chan struct{} {
	machine := a.desktopMachine(server)
	a.deskMu.Lock()
	defer a.deskMu.Unlock()
	return a.desktopGate(machine)
}

// recordDesktopUse 记下这个会话用过哪个桌面服务（结束时清理用）。
func (a *App) recordDesktopUse(sid, server string) {
	a.deskMu.Lock()
	defer a.deskMu.Unlock()
	if a.desktopUsed == nil {
		a.desktopUsed = map[string]map[string]bool{}
	}
	if a.desktopUsed[sid] == nil {
		a.desktopUsed[sid] = map[string]bool{}
	}
	a.desktopUsed[sid][server] = true
}

// endDesktopSession 结束这个会话在 Driver 里的 session：agent 光标、按会话的记录、临时配置。
// Driver 那边是幂等的；失败只记日志，不影响会话本身。不能拿着 App 锁等它。
func (a *App) endDesktopSession(sid string) {
	a.deskMu.Lock()
	servers := map[string]bool{}
	for s := range a.desktopUsed[sid] {
		servers[s] = true
	}
	delete(a.desktopUsed, sid)
	a.deskMu.Unlock()
	mg := a.mcpRef.Load()
	if mg == nil || len(servers) == 0 {
		return
	}
	go func() {
		for server := range servers {
			if _, ok := mg.ToolInfo(server, "end_session"); !ok {
				continue
			}
			gate := a.desktopGateFor(server)
			select {
			case gate <- struct{}{}:
			case <-time.After(10 * time.Second):
				a.log.Warn("桌面一直在忙，没能结束 Driver session（空闲后它会自己回收）", "sid", sid, "server", server)
				continue
			}
			ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
			_, err := mg.Call(ctx, server, "end_session", map[string]any{"session": desktopSessionID(sid)})
			cancel()
			<-gate
			if err != nil {
				a.log.Warn("结束桌面 session 失败（Driver 空闲后也会自己回收）", "sid", sid, "server", server, "err", err)
				continue
			}
			a.log.Info("结束桌面 session", "sid", sid, "server", server)
		}
	}()
}

// desktopCall 是一次桌面工具调用的包装：同一台桌面串行、自动补 session、记下用过哪个服务。
// 返回结果和错误；调用方负责回复模型。ctx 取消（用户停止、超时）会放弃排队。
func (a *App) desktopCall(ctx context.Context, sid, server string, info mcp.ToolInfo, args any, runIDs ...uint64) (*sdkmcp.CallToolResult, error) {
	if len(runIDs) > 0 {
		if err := a.reserveDesktop(ctx, a.desktopMachine(server), sid, runIDs[0]); err != nil {
			return nil, err
		}
	}
	gate := a.desktopGateFor(server)
	mg := a.mcpRef.Load()
	if mg == nil {
		return nil, fmt.Errorf("没有接 MCP 服务")
	}
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	res, err := mg.Call(ctx, server, info.Name, args)
	if err == nil {
		a.recordDesktopUse(sid, server)
	}
	return res, err
}

// desktopToolInfo 判断这次 MCP 调用是不是桌面工具，并把参数处理好。调用方持有锁。
func (a *App) desktopToolInfo(sid, server, tool string, args any) (mcp.ToolInfo, any, bool) {
	if a.mcp == nil || !a.isDesktopServer(server) {
		return mcp.ToolInfo{}, args, false
	}
	info, ok := a.mcp.ToolInfo(server, tool)
	if !ok {
		return mcp.ToolInfo{}, args, false
	}
	return info, injectDesktopSession(info, args, sid), true
}

// trimToolImages 是模型请求里的截图预算：历史原样保留，只把最近 keep 个带图片的工具结果
// 留给模型看，更早的图片换成一句说明（同一轮的任务还可以重新截图）。
func trimToolImages(h []session.Msg, keep int) []session.Msg {
	if keep < 0 {
		keep = 0
	}
	out := append([]session.Msg(nil), h...)
	seen := 0
	const note = "[以前的截图已省略：需要时重新观察或截图]"
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != "tool" || len(out[i].Images) == 0 {
			continue
		}
		seen++
		if seen <= keep {
			continue
		}
		out[i].Images = nil
		if !strings.Contains(out[i].Content, note) {
			out[i].Content = strings.TrimSpace(out[i].Content + "\n" + note)
		}
	}
	return out
}
