package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/session"
	"strconv"
	"strings"
	"time"
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
	if noTools {
		tools = nil
		for _, spec := range a.searchTools() {
			tools = append(tools, toolOf(spec))
		}
	}
	// 工具定义用会话第一次请求时的快照，新会话、/reset、整理上下文之后才刷新，保持请求前缀稳定。
	// 人格改了（/persona、网页保存、直接改文件）是用户有意换的，也算刷新时机：系统提示一变缓存本来就全部失效，工具定义顺便一起刷新
	st := a.store.Settings(sid)
	if st.Prefix != nil && st.Prefix.System == system {
		tools = st.Prefix.Tools
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
	var b strings.Builder
	fmt.Fprintf(&b, "上下文 %s / %s（%d%%）· %s", tokenText(c.Total), tokenText(c.Window), c.Total*100/max(1, c.Window), c.Model)
	fmt.Fprintf(&b, "\n  人格 %s · 工具定义 %s · 摘要 %s", tokenText(c.Persona), tokenText(c.Tools), tokenText(c.Summary))
	if c.CompactedAt > 0 {
		at := time.Unix(c.CompactedAt, 0).In(a.loc)
		layout := "15:04"
		if at.Format("2006-01-02") != a.now().In(a.loc).Format("2006-01-02") {
			layout = "01-02 15:04"
		}
		fmt.Fprintf(&b, "（%s 整理掉 %d 条）", at.Format(layout), c.Compacted)
	}
	fmt.Fprintf(&b, "\n  对话 %s（%d 条，工具结果占 %s）", tokenText(c.Dialogue), c.Messages, tokenText(c.ToolResults))
	if c.Last.PromptTokens > 0 {
		fmt.Fprintf(&b, "\n上次请求：输入 %s（缓存命中 %d%%）· 输出 %s", tokenText(c.Last.PromptTokens), c.Last.CachedTokens*100/c.Last.PromptTokens, tokenText(c.Last.CompletionTokens))
	} else {
		b.WriteString("\n上次请求：暂无（新会话或刚整理过），上面的数字是估算")
	}
	idle := "空闲不自动整理"
	if d, err := time.ParseDuration(c.IdleFor); err == nil && d > 0 {
		idle = "空闲 " + durationText(d) + "后整理"
	}
	fmt.Fprintf(&b, "\n到 %s（%d%%）时自动整理；%s；/compact 现在整理", tokenText(c.AutoAt), c.AutoAt*100/max(1, c.Window), idle)
	return b.String()
}

// tokenText 把 token 数写成 41.2k、128k 这样
func tokenText(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1000, 'f', 1, 64), ".0") + "k"
}

// durationText 把空闲时长写成“30 分钟”“2 小时”
func durationText(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%d 小时", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%d 分钟", d/time.Minute)
	}
	return d.String() + " "
}
