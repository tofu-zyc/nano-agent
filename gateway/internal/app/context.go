package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/session"
	"sort"
	"strconv"
	"strings"
	"time"
)

// prefixVer 是网关给模型的工具这一版的编号：加了新工具（v0.6 的 Knowledge）就加一，
// 旧会话的工具快照作废一次（缓存失效一次），之后照常只在固定时机刷新。
const prefixVer = 1

func (a *App) contextParts(sid string) (string, []llm.Tool, string, int) {
	system, _ := a.personaFor(sid)
	// 群聊：人格后面加上群聊守则（agents/<id>/group.md 或内置的），内容对这个群保持不变
	if a.isGroupSession(sid) {
		if g := a.groupGuide(a.agentOf(sid).ID); g != "" {
			system = strings.TrimSpace(system + "\n\n" + g)
		}
	}
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
	// 整理记忆：人格后面加上整理方法（dream 技能），工具只有日记和记忆
	if isDreamSID(sid) {
		system = strings.TrimSpace(system + "\n\n" + a.skillText(a.agentOf(sid).ID, "dream", defaultDreamSkill))
	}
	noTools := false
	if s := a.sessions[sid]; s != nil && s.run != nil {
		noTools = s.run.noTools
	}
	if isDreamSID(sid) {
		tools = []llm.Tool{toolOf(diaryTool()), toolOf(knowledgeTool())}
	} else if a.isGroupSession(sid) {
		// 群聊只有 agent 的群聊白名单（默认搜索和技能），不碰文件、命令
		var all []llm.Tool
		for _, spec := range a.searchTools() {
			all = append(all, toolOf(spec))
		}
		if spec, ok := a.skillSpec(sid); ok {
			all = append(all, toolOf(spec))
		}
		if a.memoryOn(a.agentOf(sid)) {
			all = append(all, toolOf(knowledgeTool()))
		}
		tools = a.groupTools(sid, all)
	} else {
		if n := a.nodes[rt.kernel]; n != nil && !noTools {
			tools = a.toolsFor(sid, n, false, admin)
		}
		if noTools {
			tools = nil
			for _, spec := range a.searchTools() {
				tools = append(tools, toolOf(spec))
			}
		}
		if a.memoryOn(a.agentOf(sid)) {
			tools = append(tools, toolOf(knowledgeTool()))
			sort.SliceStable(tools, func(i, j int) bool { return tools[i].Function.Name < tools[j].Function.Name })
		}
		// agent 可以限定这个会话能用哪些工具（空表示全部）
		tools = a.filterAgentTools(sid, tools)
	}
	// 工具定义用会话第一次请求时的快照，新会话、/reset、整理上下文之后才刷新，保持请求前缀稳定。
	// 人格改了（/persona、网页保存、直接改文件）是用户有意换的，也算刷新时机：系统提示一变缓存本来就全部失效，工具定义顺便一起刷新
	st := a.store.Settings(sid)
	// 群聊和房间的工具由白名单单独算，不套用旧快照（旧快照可能是普通会话的整套工具）
	if st.Prefix != nil && st.Prefix.System == system && st.Prefix.Ver == prefixVer && !a.isGroupSession(sid) {
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

// MemoryInfo 是网页「记忆」节点要显示的概况：整理过上下文的会话、归档份数
type MemoryInfo struct {
	Summarized int `json:"summarized"` // 上下文里有摘要（整理过）的会话
	Archives   int `json:"archives"`   // /reset 归档的份数
}

func (a *App) MemoryInfo() MemoryInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	var m MemoryInfo
	for _, sid := range a.sessionIDs() {
		if a.store.Settings(sid).Summary != "" {
			m.Summarized++
		}
	}
	m.Archives = a.store.ArchiveCount()
	return m
}
