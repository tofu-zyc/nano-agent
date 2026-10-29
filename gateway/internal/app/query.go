package app

import (
	"context"
	"errors"
	"fmt"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/dispatch"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// 网页界面用的查询和操作。都在锁里完成，返回的是副本。

// Totals 是网关启动以来的累计。
type Totals struct {
	Runs             int `json:"runs"`
	ModelCalls       int `json:"model_calls"`
	PromptTokens     int `json:"prompt_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}

func (t *Totals) add(u llm.Usage) {
	t.PromptTokens += u.PromptTokens
	t.CachedTokens += u.CachedTokens
	t.CompletionTokens += u.CompletionTokens
	t.ReasoningTokens += u.ReasoningTokens
}

type SessionInfo struct {
	SID            string         `json:"sid"`
	Agent          string         `json:"agent"`       // 当前执行身份：人格、模型、工具、限制、记忆按它算（可以切换）
	RouteAgent     string         `json:"route_agent"` // 会话 ID 的命名空间（机器人接线、主会话按它算，不跟着切换变）
	AgentFixed     bool           `json:"agent_fixed"` // 身份固定：内心活动、整理记忆、定时任务、房间成员
	CanSwitchAgent bool           `json:"can_switch_agent"`
	SwitchBlocked  string         `json:"switch_blocked,omitempty"` // 现在不能切换的原因（空表示可以）
	Group          bool           `json:"group"`                    // 这是某个群聊里的会话
	Channel        string         `json:"channel"`                  // 主要的渠道（第一个接着的窗口所在的），网页配色用
	Endpoints      []EndpointInfo `json:"endpoints"`                // 接着这个会话的聊天窗口（网页不算，网页能接入任何会话）
	Title          string         `json:"title"`
	Workspaces     []MountInfo    `json:"workspaces"` // 挂载的工作目录，空表示只读
	Kernel         string         `json:"kernel"`     // 跑这一轮的内核（没在跑时是下一轮会用的）
	RunNode        string         `json:"run_node"`   // 正在跑这一轮的内核
	Running        bool           `json:"running"`
	Pending        int            `json:"pending"` // 待审批的工具调用
	LastAt         int64          `json:"last_at"` // 最后一条消息的时间（秒）
	Messages       int            `json:"messages"`
	SandboxMode    string         `json:"sandbox_mode"`
	Mode           string         `json:"mode"`
	Thinking       string         `json:"thinking"`
	ThinkingLevels []string       `json:"thinking_levels"` // 这个模型的思考强度可选值
	Model          string         `json:"model"`           // 这个会话选的模型 ID，空表示用默认模型
	ModelUsed      string         `json:"model_used"`      // 实际用的模型 ID
	ModelLabel     string         `json:"model_label"`     // 实际用的模型的名字

	Persona     string              `json:"persona"`      // 用的人格文件名
	PersonaFile string              `json:"persona_file"` // 这个会话专属人格的文件名（不一定存在）
	Usage       llm.Usage           `json:"usage"`        // 历史里记下的累计用量
	Context     session.ContextInfo `json:"context"`
	Compacted   int                 `json:"compacted"`
	// Interrupted 是"上次中断、还没处理完"的用户补充条数（持久状态，重启后仍然算得到）
	Interrupted int `json:"interrupted,omitempty"`
	// Branch 是这个会话本身是分支时的元信息（网页按父子折叠、显示安静运行状态用）
	Branch *BranchInfo `json:"branch,omitempty"`
}

// MountInfo 是会话挂载的一个工作目录。How：local（跑这一轮的内核在同一台机器上，自己执行）、
// gateway（交给网关在那台机器上执行）、down（那台机器现在用不了）。
type MountInfo struct {
	SandboxMode     string `json:"sandbox_mode,omitempty"`
	SandboxRequired string `json:"sandbox_required,omitempty"`
	ID              string `json:"id"`
	Host            string `json:"host"`
	Path            string `json:"path"`
	How             string `json:"how"`
}

type ApprovalInfo struct {
	N      int    `json:"n"`
	SID    string `json:"sid"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
	From   string `json:"from"`
	At     int64  `json:"at"`
}

type ExecutionInfo struct {
	Limits config.Runtime   `json:"limits"`
	Leases []dispatch.Lease `json:"leases"`
	Error  string           `json:"error,omitempty"`
}
type Snapshot struct {
	Execution     ExecutionInfo          `json:"execution"`
	StorageIssues []session.StorageIssue `json:"storage_issues"`
	Started       int64                  `json:"started"`
	Kernels       []KernelInfo           `json:"kernels"`
	Machines      []MachineInfo          `json:"machines"`
	Workspaces    []WorkspaceInfo        `json:"workspaces"`
	Totals        Totals                 `json:"totals"`
	Sessions      []SessionInfo          `json:"sessions"`
	Approvals     []ApprovalInfo         `json:"approvals"`
}

// Snapshot 返回网关当前的全貌：内核、所有会话（包括只在磁盘上的）、待审批、累计用量。
func (a *App) Snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	snap := Snapshot{Started: a.started.UnixMilli(), Totals: a.totals, Kernels: a.kernelList(), Machines: a.machineList(), Workspaces: a.workspaceInfos()}
	for _, sid := range a.sessionIDs() {
		if isRoomMemberSID(sid) || isDreamSID(sid) {
			continue // agent 在房间里旁听的会话，用户看群那个会话就够了
		}
		// 按 ID 前缀属于某个已删除 agent 的会话不再列出来（历史还在磁盘上，重新建一个同 ID 的 agent
		// 才会再出现）；用户明确交给别的 agent 的会话不受影响，仍然可见。
		if !a.hasExplicitAgent(sid) {
			if ag := a.routeAgentID(sid); ag != agents.DefaultID {
				if _, ok := a.agentByID(ag); !ok {
					continue
				}
			}
		}
		snap.Sessions = append(snap.Sessions, a.sessionInfo(sid))
	}
	sort.Slice(snap.Sessions, func(i, j int) bool { return snap.Sessions[i].LastAt > snap.Sessions[j].LastAt })
	snap.Approvals = a.approvalList()
	snap.StorageIssues = a.store.StorageIssues()
	snap.Execution.Limits = a.runtimeLimits
	if a.dispatcher != nil {
		cp, problem := a.dispatcher.Snapshot()
		snap.Execution.Leases, snap.Execution.Error = cp.Leases, problem
	} else if a.dispatchErr != nil {
		snap.Execution.Error = a.dispatchErr.Error()
	}
	return snap
}

func (a *App) approvalList() []ApprovalInfo {
	out := []ApprovalInfo{}
	for n, ap := range a.approvals {
		out = append(out, ApprovalInfo{N: n, SID: ap.sid, ID: ap.id, Name: ap.name, Detail: ap.detail, From: ap.from, At: ap.at.UnixMilli()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out
}

func (a *App) sessionInfo(sid string) SessionInfo {
	st := a.store.Settings(sid)
	w := a.workspaceOf(sid)
	branch := a.isBranchSID(sid)
	active := a.sessions[sid] != nil && a.sessions[sid].run != nil
	var context session.ContextInfo
	if (!branch && !isHookSID(sid)) || active {
		context = a.contextInfo(sid)
	} else {
		context = session.ContextInfo{Lazy: true, Last: st.Meter.Usage, Compacted: st.Base, CompactedAt: st.CompactedAt, Total: st.Meter.Usage.PromptTokens}
	}
	info := SessionInfo{SID: sid, Agent: a.execAgentID(sid), RouteAgent: a.routeAgentID(sid), Group: st.Group, Channel: "web", Endpoints: a.endpointsOf(sid), Title: st.Title,
		SandboxMode: a.sandboxModeFor(sid), Mode: w.mode, Thinking: a.thinkingOf(sid), ThinkingLevels: a.levelsOf(sid),
		Model: st.Model, Compacted: st.Base, Context: context}
	info.Interrupted = a.interruptedCount(sid)
	if meta, ok := a.store.Branch(sid); ok {
		info.Branch = &BranchInfo{}
		*info.Branch = a.branchInfo(meta)
	}
	info.AgentFixed = fixedAgentSID(sid) || branch
	info.SwitchBlocked = a.agentSwitchBlocked(sid)
	info.CanSwitchAgent = info.SwitchBlocked == "" && a.agentStore != nil
	if m, _, ok := a.modelOf(sid); ok {
		info.ModelUsed, info.ModelLabel = m.ID, modelLabel(m)
	}
	rt := a.route(w)
	a.freezeSandboxRoute(&rt, info.SandboxMode)
	if s := a.sessions[sid]; s != nil && s.run != nil {
		rt = s.run.rt
	}
	info.Kernel = rt.kernel
	info.Workspaces = []MountInfo{}
	for _, x := range w.wss {
		how := "down"
		if m := rt.find(x.Label()); m != nil && m.local {
			how = "local"
		} else if m != nil {
			how = "gateway"
		}
		requested, required := info.SandboxMode, ""
		if mt := rt.find(x.Label()); mt != nil {
			requested, required = mt.sandboxMode, mt.sandboxRequired
		} else if x.SandboxMode != "" {
			requested = x.SandboxMode
		}
		info.Workspaces = append(info.Workspaces, MountInfo{ID: x.ID, Host: x.Host, Path: x.Path, How: how, SandboxMode: requested, SandboxRequired: required})
	}
	if len(info.Endpoints) > 0 {
		info.Channel = info.Endpoints[0].Channel
	}
	if isLifeSID(sid) {
		info.Channel = "life" // 拟人型的内心活动：网页上单独的图标，只能看
	}
	if info.Title == "" {
		switch {
		case sid == mainSID:
			info.Title = "主会话"
		case isLifeSID(sid):
			ag := a.agentOf(sid)
			info.Title = ag.Name + "的内心"
		case a.eps[sid] != nil && a.eps[sid].Name != "":
			info.Title = a.eps[sid].Name // 某个人自己的会话：用聊天的名字
		default:
			info.Title = sid
		}
	}
	if s := a.sessions[sid]; s != nil && s.run != nil {
		info.Running, info.RunNode = true, s.run.node
	}
	for _, ap := range a.approvals {
		if ap.sid == sid {
			info.Pending++
		}
	}
	if branch && !active {
		info.Messages, info.LastAt, info.Usage = a.store.RawStats(sid)
		info.Context.Messages = max(0, info.Messages-st.Base)
		info.PersonaFile = persona.FileFor(sid)
		return info
	}
	h := a.store.History(sid)
	info.Messages = len(h)
	for _, m := range h {
		if m.TS > info.LastAt {
			info.LastAt = m.TS
		}
		if m.Usage != nil {
			info.Usage.PromptTokens += m.Usage.PromptTokens
			info.Usage.CachedTokens += m.Usage.CachedTokens
			info.Usage.CompletionTokens += m.Usage.CompletionTokens
			info.Usage.ReasoningTokens += m.Usage.ReasoningTokens
		}
	}
	if isLifeSID(sid) {
		info.Messages, info.LastAt, info.Usage = a.store.RawStats(sid)
	}
	if _, path := a.personaFor(sid); path != "" {
		info.Persona = filepath.Base(filepath.Dir(path)) + "/" + filepath.Base(path)
	}
	info.PersonaFile = persona.FileFor(sid)
	return info
}

// Session 返回一个会话的信息，没有这个会话时 ok 为 false。
func (a *App) Session(sid string) (info SessionInfo, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.hasSession(sid) {
		return SessionInfo{}, false
	}
	info = a.sessionInfo(sid)
	if a.isBranchSID(sid) || isHookSID(sid) {
		info.Context = a.contextInfo(sid)
		if a.sessions[sid] == nil || a.sessions[sid].run == nil {
			a.store.UnloadHistory(sid)
		}
	}
	return info, true
}

// History 返回会话历史（从上次裁剪的位置开始）和代替被裁掉部分的说明。
func (a *App) History(sid string) ([]session.Msg, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	h := slices.Clone(a.store.History(sid))
	annotateLegacyReports(h)
	summary := a.store.Settings(sid).Summary
	if (a.isBranchSID(sid) || isHookSID(sid)) && (a.sessions[sid] == nil || a.sessions[sid].run == nil) {
		a.store.UnloadHistory(sid)
	}
	return h, summary
}

// SessionPatch 是网页里对会话设置的修改，nil 表示不改。
type SessionPatch struct {
	Title      *string   `json:"title"`
	Workspaces *[]string `json:"workspaces"` // 挂载的工作目录（ID），空列表表示只读
	Mode       *string   `json:"mode"`
	Thinking   *string   `json:"thinking"` // 空字符串表示用默认值
	Model      *string   `json:"model"`    // 空字符串表示用默认模型
	// Agent 是当前执行的 agent：人格、模型、工具、限制、记忆都换成它的。空字符串表示恢复按会话 ID
	// 前缀推断（一般只有“本身就是 @<agent>/…”的会话需要）。会话 ID、历史、窗口接线、会话级设置都不变。
	Agent *string `json:"agent"`
}

// UpdateSession 改会话设置。整份 patch 先全部验证，再一次性写入：任何一项不合法都不会留下半套设置。
func (a *App) UpdateSession(sid string, p SessionPatch) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.hasSession(sid) {
		return errors.New("没有这个会话")
	}
	st := a.store.Settings(sid)
	oldAgent := a.execAgent(sid)
	newAgentID := oldAgent.ID
	if p.Agent != nil {
		want := strings.TrimSpace(*p.Agent)
		if want == "" {
			want = a.routeAgentID(sid)
		}
		if !a.agentExists(want) {
			return fmt.Errorf("没有这个 agent：%s", want)
		}
		if fixedAgentSID(sid) && want != newAgentID {
			return errors.New(a.agentSwitchBlocked(sid))
		}
		if want != newAgentID {
			if why := a.agentSwitchBlocked(sid); why != "" {
				return errors.New(why)
			}
		}
		newAgentID = want
	}
	switching := newAgentID != oldAgent.ID
	var errs []error
	// 工作目录：先只验证，不写盘（以前的实现会在这里先保存，后面的模型校验失败就留下半套设置）
	cleanWorkspaces, workspacesChanged := st.Workspaces, false
	if p.Workspaces != nil {
		ids, err := a.cleanWorkspaces(*p.Workspaces)
		if err != nil {
			errs = append(errs, err)
		} else {
			cleanWorkspaces = ids
			workspacesChanged = !slices.Equal(ids, st.Workspaces) || !st.WorkspacesSet
		}
	}
	title := st.Title
	if p.Title != nil {
		title = strings.TrimSpace(*p.Title)
		if len([]rune(title)) > 40 {
			errs = append(errs, errors.New("名字最长 40 个字"))
		}
	}
	mode := st.Mode
	if p.Mode != nil {
		if _, ok := modeNames[*p.Mode]; !ok {
			errs = append(errs, errors.New("模式只能是 full、auto、ask 或 readonly"))
		} else {
			mode = *p.Mode
		}
	}
	model := st.Model
	if p.Model != nil {
		if *p.Model != "" && !a.modelExists(*p.Model) {
			errs = append(errs, fmt.Errorf("没有模型 %s", *p.Model))
		} else {
			model = *p.Model
		}
	}
	// 思考强度按换过 agent、换过模型以后实际生效的模型检查（agent 的默认模型也算）
	levels := a.levelsForAgentModel(newAgentID, model)
	thinking := st.Thinking
	if p.Thinking != nil {
		if *p.Thinking != "" && !slices.Contains(levels, *p.Thinking) {
			errs = append(errs, fmt.Errorf("思考强度只能是 %s", strings.Join(levels, "、")))
		}
		thinking = *p.Thinking
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	// 换了模型或者 agent：原来的思考强度可能不在新的可选值里，恢复成默认
	if thinking != "" && !slices.Contains(levels, thinking) {
		thinking = ""
	}
	st.Title, st.Mode, st.Model, st.Thinking = title, mode, model, thinking
	if p.Workspaces != nil {
		st.Workspaces, st.WorkspacesSet = cleanWorkspaces, true
	}
	if switching {
		newAgent, _ := a.agentByID(newAgentID)
		// 历史位置用绝对位置：被整理掉的部分也算，消息来源才定得准
		a.recordAgentSwitch(sid, &st, oldAgent, newAgent, st.Base+len(a.store.History(sid)))
	}
	if err := a.store.SaveSettings(sid, st); err != nil {
		return err
	}
	if switching {
		// 旧 agent 的 FollowUp、主动消息标记、群里排着的触发都作废
		a.clearAgentBoundState(sid, oldAgent.ID)
		a.endDesktopSession(sid) // 旧身份占着的 Driver session 释放掉
		a.publishAgents()        // 两个 agent 的会话计数都变了
		a.log.Info("会话换了 agent", "sid", sid, "from", oldAgent.ID, "to", newAgentID)
	}
	a.publishSession(sid)
	if workspacesChanged {
		a.publishWorkspaces()
	}
	return nil
}

// ResetSession 清空会话的上下文；有任务在跑就先停掉，结束后再清。
func (a *App) ResetSession(sid string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[sid]; s != nil && s.run != nil {
		s.queue = nil
		s.resetAfter = true
		a.cancelRun(s)
		return nil
	}
	if err := a.resetHistory(a.sessOf(sid)); err != nil {
		return err
	}
	a.endDesktopSession(sid)
	a.publish(events.Event{T: "session", SID: sid, Data: a.sessionInfo(sid)})
	return nil
}

// StopSession 停止会话里正在跑的任务。
func (a *App) StopSession(sid string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[sid]
	if s == nil || s.run == nil {
		return errors.New("这个会话没有进行中的任务")
	}
	s.queue = nil
	a.endDesktopSession(sid)
	a.cancelRun(s)
	return nil
}

func (a *App) cancelRun(s *sess) {
	if s.compactCancel != nil {
		s.compactCancel()
		s.compactCancel = nil
		s.compacting = false
		a.publish(events.Event{T: "context", SID: s.sid, Kind: "end"})
	}
	if s.run.cancelLLM != nil {
		s.run.cancelLLM()
		s.run.cancelLLM = nil
	}
	for _, cancel := range s.run.execs {
		cancel() // 交给网关执行的工具也马上停下（内核也会发 exec_cancel）
	}
	a.hub.Send(s.run.node, proto.Msg{T: proto.TCancel, SID: s.sid}) // 内核会回 done(cancelled)
}

// DirListing 是内核列出的一个目录（只有子目录）。
type DirListing struct {
	Path      string           `json:"path"`
	Home      string           `json:"home"`
	OS        string           `json:"os,omitempty"` // 这台机器的系统：linux / macos / windows
	Blocked   bool             `json:"blocked"`      // 包含 nano-agent 自己，不能当工作目录
	Entries   []proto.DirEntry `json:"entries"`
	Truncated bool             `json:"truncated"`
}

// ListDir 列出机器 hostName 上的一个目录（只有子目录），网页上选工作目录用。path 可以是 ~ 开头。
// 本机和 SSH 服务器由网关自己列；装了内核的电脑请它的内核列。
func (a *App) ListDir(ctx context.Context, hostName, path string) (*DirListing, error) {
	if hostName == "" {
		hostName = host.LocalName
	}
	a.mu.Lock()
	h, _, why := a.hostFor(hostName)
	known := a.knownHost(hostName)
	osName := a.machineOS(hostName)
	a.mu.Unlock()
	if h == nil {
		if !known {
			return nil, fmt.Errorf("没有叫 %s 的机器", hostName)
		}
		return nil, fmt.Errorf("%s 现在用不了（%s），没法浏览它上面的目录", hostName, why)
	}
	l, err := h.Ls(ctx, path)
	if err != nil {
		return nil, err
	}
	out := &DirListing{Path: l.Path, Home: l.Home, OS: osName, Blocked: l.Blocked, Entries: l.Entries, Truncated: l.Truncated}
	if out.Entries == nil {
		out.Entries = []proto.DirEntry{}
	}
	return out, nil
}

// Approvals 返回待审批的工具调用。
func (a *App) Approvals() []ApprovalInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.approvalList()
}

// Approve 以网页管理员的身份批准或拒绝。
func (a *App) Approve(n int, ok bool) error {
	a.mu.Lock()
	ap := a.approvals[n]
	// MCP 这类工具在网关这边执行：批不批交回调用方，不用告诉内核
	if ap != nil && ap.resolve != nil {
		res := ap.resolve
		delete(a.approvals, n)
		a.resolveApproval(ap, a.decided(ok, "web:admin"))
		a.publish(events.Event{T: "approval_done", SID: ap.sid, N: n})
		a.mu.Unlock()
		res(ok)
		return nil
	}
	defer a.mu.Unlock()
	var s *sess
	if ap != nil {
		s = a.sessions[ap.sid]
	}
	if s == nil || s.run == nil {
		return errors.New("没有这个编号的待审批请求，可能已经处理过了")
	}
	if !a.hub.Send(s.run.node, proto.Msg{T: proto.TApprove, SID: ap.sid, ID: ap.id, OK: proto.Bool(ok), From: "web:admin"}) {
		return errors.New("内核没有连接")
	}
	a.resolveApproval(ap, a.decided(ok, "web:admin"))
	return nil
}

func (a *App) FullHistory(sid string) []session.Msg {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.store.Archive(sid))
}

// interruptedCount 是"上次中断、还没处理完"的补充条数：从会话状态或日志里算，给界面看。
func (a *App) interruptedCount(sid string) int {
	if s := a.sessions[sid]; s != nil {
		return len(s.interrupted)
	}
	// 会话还没被访问过：直接折叠日志（有界，只读）
	recs, _, err := readJournal(a.journalPath(sid))
	if err != nil {
		return 0
	}
	gen := a.store.Settings(sid).SessionGeneration
	n := 0
	for _, st := range foldJournal(recs, gen) {
		if st.Outcome == outcomeInterrupted && !st.settled && !st.withdrawn {
			n++
		}
	}
	return n
}

// annotateLegacyReports 给旧版（只有正文、没有结构化字段）的分支报告补上卡片信息。
// 只认完整的老模板（含结尾那句宿主说明），用户只写相同前缀不会被当成报告。
func annotateLegacyReports(history []session.Msg) {
	for i := range history {
		m := &history[i]
		if m.Role != "user" || m.Kind != "" || m.Report != nil {
			continue
		}
		if card := parseLegacyReport(m.Content); card != nil {
			m.Kind, m.Report = "branch_report", card
		}
	}
}

const legacyReportTail = "\n这是主人主动带回来的分支结论，分支里更早的对话没有合并进来；需要过程就打开分支看。"

func parseLegacyReport(text string) *session.Report {
	if !strings.HasPrefix(text, "[分支报告 ") || !strings.HasSuffix(text, legacyReportTail) {
		return nil
	}
	body := strings.TrimSuffix(text, legacyReportTail)
	head, rest, ok := strings.Cut(strings.TrimPrefix(body, "[分支报告 "), "]\n")
	if !ok {
		return nil
	}
	// head 形如 "<sid>「<title>」"
	sid, title, ok := strings.Cut(head, "「")
	if !ok {
		return nil
	}
	title = strings.TrimSuffix(title, "」")
	lines := strings.Split(rest, "\n")
	card := &session.Report{Kind: "branch_report", BranchSID: sid, Title: title}
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "来源会话："):
			fields := strings.Split(strings.TrimPrefix(line, "来源会话："), "；")
			for _, field := range fields {
				if v, ok := strings.CutPrefix(field, "分叉点：第 "); ok {
					v = strings.TrimSuffix(strings.TrimSuffix(v, " 条"), "条")
					if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
						card.Checkpoint = n
					}
				} else if v, ok := strings.CutPrefix(field, "分支状态："); ok {
					card.Status = strings.TrimSuffix(strings.TrimSpace(v), "。")
				} else {
					card.ParentSID = strings.TrimSpace(field)
				}
			}
		case strings.HasPrefix(line, "结论："):
			card.Conclusion = strings.TrimPrefix(line, "结论：")
		case strings.HasPrefix(line, "依据："):
			for _, e := range strings.Split(strings.TrimPrefix(line, "依据："), "；") {
				if e = strings.TrimSpace(e); e != "" {
					card.Evidence = append(card.Evidence, e)
				}
			}
		}
	}
	if card.BranchSID == "" || card.Conclusion == "" {
		return nil
	}
	return card
}
