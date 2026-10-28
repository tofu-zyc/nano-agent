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

// prefixVer 是网关给模型的工具这一版的编号：加了新工具（v0.6 的 Knowledge，v0.7 的 SendFile）、
// 改了工具的参数（v0.7.2 的 Gateway / GatewayAdmin 能查、能配 MCP；v0.7.4 的 Agenda / Idea / Mind）就加一，
// 旧会话的工具快照作废一次（缓存失效一次），之后照常只在固定时机刷新。
const prefixVer = 9

func (a *App) contextParts(sid string) (string, []llm.Tool, string, int) {
	system, _ := a.personaFor(sid)
	// 群聊：人格后面加上群聊守则（agents/<id>/group.md 或内置的），内容对这个群保持不变
	if a.isGroupSession(sid) {
		if g := a.groupGuide(a.agentOf(sid).ID); g != "" {
			system = strings.TrimSpace(system + "\n\n" + g)
		}
	}
	var tools []llm.Tool
	rt, admin, noTools := a.permOf(sid)
	ag := a.agentOf(sid)
	switch {
	case isDreamSID(sid):
		// 整理记忆：人格后面加上整理方法（dream 技能），工具只有日记和记忆
		system = strings.TrimSpace(system + "\n\n" + a.skillText(ag.ID, "dream", defaultDreamSkill))
	case ag.Life():
		// 拟人型：每个会话都带拟人守则；内心活动再加上“一个人的时候做什么”（life 技能）
		system = strings.TrimSpace(system + "\n\n" + a.lifeGuide(ag.ID))
		if isLifeSID(sid) {
			system += "\n\n" + a.skillText(ag.ID, "life", defaultLifeSkill)
		}
	}
	// 桌面操作：把 Driver 的版本相关说明接进系统提示（只在能用 MCP 的管理员会话里）
	if admin && !noTools && !isDreamSID(sid) && !a.isGroupSession(sid) {
		if g := a.desktopGuide(sid); g != "" {
			system = strings.TrimSpace(system + "\n\n" + g)
		}
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
		if ag.Life() {
			all = append(all, toolOf(followUpTool()))
		}
		all = append(all, toolOf(sendFileTool()))
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
		} else {
			// MCP 服务带的工具（<服务>__<工具>）：访客、只读和不用工具的模式都没有
			tools = append(tools, a.mcpOffered(sid, admin)...)
		}
		if a.memoryOn(a.agentOf(sid)) {
			tools = append(tools, toolOf(knowledgeTool()))
		}
		if ag.Life() {
			if !isLifeSID(sid) {
				tools = append(tools, toolOf(followUpTool()))
			}
			if admin {
				tools = append(tools, toolOf(agendaTool()), toolOf(ideaTool()), toolOf(mindTool()))
			}
		}
		tools = append(tools, toolOf(sendFileTool()))
		sort.SliceStable(tools, func(i, j int) bool { return tools[i].Function.Name < tools[j].Function.Name })
		// agent 可以限定这个会话能用哪些工具（空表示全部）
		tools = a.filterAgentTools(sid, tools)
	}
	if n := a.nodes[rt.kernel]; len(tools) > 0 && n != nil && n.info.Features["bounded_parallel"] >= 1 {
		system = strings.TrimSpace(system + "\n\n[工具调度] 相互独立的工具调用可以在同一次响应中一起提出；运行时会按执行槽和资源锁有界调度。结果按调用身份关联，全部结果返回后才继续模型请求。有数据依赖的步骤分到下一次响应，不能假定同批调用按顺序完成。")
	}
	// 工具定义用快照保持请求前缀稳定；技能目录/MCP 工具或权限变化会刷新，不必清空聊天。
	// 人格改了（/persona、网页保存、直接改文件）是用户有意换的，也算刷新时机：系统提示一变缓存本来就全部失效，工具定义顺便一起刷新
	st := a.store.Settings(sid)
	// 群聊和房间的工具由白名单单独算，不套用旧快照（旧快照可能是普通会话的整套工具）
	if st.Prefix != nil && st.Prefix.System == system && st.Prefix.Ver == prefixVer &&
		st.Prefix.Perm == a.permSignature(sid) && !a.isGroupSession(sid) {
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

// permOf 是这一轮和权限有关的三件事：用哪台内核、是不是管理员、有没有工具。
// contextParts 用它决定给什么工具，permSignature 用它判断要不要换工具快照。调用方持有锁。
func (a *App) permOf(sid string) (route, bool, bool) {
	rt := a.route(a.workspaceOf(sid))
	admin := sid == mainSID || (isLifeSID(sid) && a.isAdmin("life:"+a.routeAgentID(sid)))
	if s := a.sessions[sid]; s != nil && s.run != nil {
		return s.run.rt, a.isAdmin(s.run.from), s.run.noTools
	}
	for _, ep := range a.endpointsOf(sid) {
		if ep.Channel != "web" && ep.Admin {
			admin = true
		}
	}
	return rt, admin, false
}

// permSignature 是「这轮该不该换工具快照」的判据：窗口刚被设为管理员、内核重新连上这类
// 权限变化要换（不然访客时期冻下的那套工具会一直用下去，用户看到的是“设置都好了就是没有
// Shell”）；只是配置变了（比如搜索 key）不换，会话中途前缀保持稳定。
func (a *App) permSignature(sid string) string {
	// 只看管理员/访客和内核：noTools 是“这一轮”算出来的，运行结束就变回去，放进签名会抖
	rt, admin, _ := a.permOf(sid)
	// MCP 按「这个会话实际会拿到的 MCP 工具」算：加了服务、后台连上了、换了模式都会变，
	// 只看配置的话，服务暂时连不上时建的快照，之后连上了也不会重建
	var names strings.Builder
	for _, t := range a.mcpOffered(sid, admin) {
		names.WriteString(t.Function.Name)
		names.WriteByte(',')
	}
	mcpSig := ""
	if names.Len() > 0 {
		mcpSig = fmt.Sprintf("%x", sha256.Sum256([]byte(names.String())))[:16]
	}
	skillSig := ""
	if spec, ok := a.skillSpec(sid); ok {
		catalog, _ := json.Marshal(spec)
		skillSig = fmt.Sprintf("%x", sha256.Sum256(catalog))[:16]
	}
	return fmt.Sprintf("%t|%s|%t|%s|%s", admin, rt.kernel, a.nodes[rt.kernel] != nil, mcpSig, skillSig)
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
