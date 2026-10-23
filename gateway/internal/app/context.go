package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/session"
)

func (a *App) contextParts(sid string) (string, []llm.Tool, string, int) {
	system, _ := a.persona.For(sid)
	var tools []llm.Tool
	rt := a.route(a.workspaceOf(sid))
	admin := sid == mainSID
	if s := a.sessions[sid]; s != nil && s.run != nil {
		rt = s.run.rt
		admin = a.isAdmin(s.run.from)
	} else {
		for _, ep := range a.endpointsOf(sid) {
			if ep.Channel != "web" && ep.Admin {
				admin = true
			}
		}
	}
	noTools := false
	if s := a.sessions[sid]; s != nil && s.run != nil {
		noTools = s.run.noTools
	}
	if n := a.nodes[rt.kernel]; n != nil && !noTools {
		tools = a.toolsFor(n, false, admin)
	}
	st := a.store.Settings(sid)
	if st.Prefix != nil {
		system, tools = st.Prefix.System, st.Prefix.Tools
	}
	m, _, _ := a.modelOf(sid)
	window := m.ContextWindow
	if window <= 0 {
		window = 128000
	}
	b, _ := json.Marshal([]any{m.ID, system, tools, a.store.Settings(sid).Summary})
	return system, tools, fmt.Sprintf("%x", sha256.Sum256(b)), window
}
func (a *App) contextInfo(sid string) session.ContextInfo {
	system, tools, sig, window := a.contextParts(sid)
	c := session.Measure(a.store.Settings(sid), a.store.EffectiveHistory(sid), system, tools, sig, window)
	m, _, _ := a.modelOf(sid)
	c.Model = modelLabel(m)
	c.AutoAt, _, _ = a.compactLimits(sid)
	c.IdleFor = a.cfg.Context.IdleCompact
	if c.IdleFor == "" {
		c.IdleFor = "30m"
	}
	return c
}
func (a *App) contextText(sid string) string {
	c := a.contextInfo(sid)
	text := fmt.Sprintf("上下文 %d / %d（%.0f%%）· %s\n人格 %d · 工具定义 %d · 摘要 %d（已整理 %d 条）\n对话 %d（%d 条，工具结果占 %d）\n上次请求：输入 %d（缓存命中 %.0f%%）· 输出 %d", c.Total, c.Window, float64(c.Total)*100/float64(c.Window), c.Model, c.Persona, c.Tools, c.Summary, c.Compacted, c.Dialogue, c.Messages, c.ToolResults, c.Last.PromptTokens, float64(c.Last.CachedTokens)*100/float64(max(1, c.Last.PromptTokens)), c.Last.CompletionTokens)
	high, _, _ := a.compactLimits(sid)
	idle := a.cfg.Context.IdleCompact
	if idle == "" {
		idle = "30m"
	}
	return text + fmt.Sprintf("\n到 %d 时自动整理；空闲 %s 后整理；/compact 现在整理", high, idle)
}
