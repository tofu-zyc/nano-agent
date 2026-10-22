package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

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
	SID         string         `json:"sid"`
	Channel     string         `json:"channel"`   // 主要的渠道（第一个接着的窗口所在的），网页配色用
	Endpoints   []EndpointInfo `json:"endpoints"` // 接着这个会话的聊天窗口（网页不算，网页能接入任何会话）
	Title       string         `json:"title"`
	Workspaces  []MountInfo    `json:"workspaces"` // 挂载的工作目录，空表示只聊天
	Kernel      string         `json:"kernel"`     // 跑这一轮的内核（没在跑时是下一轮会用的）
	RunNode     string         `json:"run_node"`   // 正在跑这一轮的内核
	Running     bool           `json:"running"`
	Pending     int            `json:"pending"` // 待审批的工具调用
	LastAt      int64          `json:"last_at"` // 最后一条消息的时间（秒）
	Messages    int            `json:"messages"`
	Mode        string         `json:"mode"`
	Thinking    string         `json:"thinking"`
	Persona     string         `json:"persona"`      // 用的人格文件名
	PersonaFile string         `json:"persona_file"` // 这个会话专属人格的文件名（不一定存在）
	Usage       llm.Usage      `json:"usage"`        // 历史里记下的累计用量
	Compacted   int            `json:"compacted"`
}

// MountInfo 是会话挂载的一个工作目录。How：local（跑这一轮的内核在同一台机器上，自己执行）、
// gateway（交给网关在那台机器上执行）、down（那台机器现在用不了）。
type MountInfo struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Path string `json:"path"`
	How  string `json:"how"`
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

type Snapshot struct {
	Started    int64           `json:"started"`
	Kernels    []KernelInfo    `json:"kernels"`
	Machines   []MachineInfo   `json:"machines"`
	Workspaces []WorkspaceInfo `json:"workspaces"`
	Totals     Totals          `json:"totals"`
	Sessions   []SessionInfo   `json:"sessions"`
	Approvals  []ApprovalInfo  `json:"approvals"`
}

// Snapshot 返回网关当前的全貌：内核、所有会话（包括只在磁盘上的）、待审批、累计用量。
func (a *App) Snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	snap := Snapshot{Started: a.started.UnixMilli(), Totals: a.totals, Kernels: a.kernelList(), Machines: a.machineList(), Workspaces: a.workspaceInfos()}
	for _, sid := range a.sessionIDs() {
		snap.Sessions = append(snap.Sessions, a.sessionInfo(sid))
	}
	sort.Slice(snap.Sessions, func(i, j int) bool { return snap.Sessions[i].LastAt > snap.Sessions[j].LastAt })
	snap.Approvals = a.approvalList()
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
	info := SessionInfo{SID: sid, Channel: "web", Endpoints: a.endpointsOf(sid), Title: st.Title,
		Mode: w.mode, Thinking: a.thinkingOf(sid), Compacted: st.Base}
	rt := a.route(w)
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
		info.Workspaces = append(info.Workspaces, MountInfo{ID: x.ID, Host: x.Host, Path: x.Path, How: how})
	}
	if len(info.Endpoints) > 0 {
		info.Channel = info.Endpoints[0].Channel
	}
	if info.Title == "" {
		switch {
		case sid == mainSID:
			info.Title = "主会话"
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
	if _, path := a.persona.For(sid); path != "" {
		info.Persona = filepath.Base(path)
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
	return a.sessionInfo(sid), true
}

// History 返回会话历史（从上次裁剪的位置开始）和代替被裁掉部分的说明。
func (a *App) History(sid string) ([]session.Msg, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.store.History(sid)), a.store.Settings(sid).Summary
}

// SessionPatch 是网页里对会话设置的修改，nil 表示不改。
type SessionPatch struct {
	Title      *string   `json:"title"`
	Workspaces *[]string `json:"workspaces"` // 挂载的工作目录（ID），空列表表示只聊天
	Mode       *string   `json:"mode"`
	Thinking   *string   `json:"thinking"` // 空字符串表示用默认值
}

func (a *App) UpdateSession(sid string, p SessionPatch) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.hasSession(sid) {
		return errors.New("没有这个会话")
	}
	if p.Workspaces != nil {
		if err := a.setSessionWorkspaces(sid, *p.Workspaces); err != nil {
			return err
		}
	}
	st := a.store.Settings(sid)
	var errs []error
	if p.Title != nil {
		st.Title = strings.TrimSpace(*p.Title)
		if len([]rune(st.Title)) > 40 {
			errs = append(errs, errors.New("名字最长 40 个字"))
		}
	}
	if p.Mode != nil {
		if _, ok := modeNames[*p.Mode]; !ok {
			errs = append(errs, errors.New("模式只能是 auto、ask 或 readonly"))
		}
		st.Mode = *p.Mode
	}
	if p.Thinking != nil {
		if *p.Thinking != "" && !slices.Contains(a.cfg.LLM.ThinkingLevels, *p.Thinking) {
			errs = append(errs, fmt.Errorf("思考强度只能是 %s", strings.Join(a.cfg.LLM.ThinkingLevels, "、")))
		}
		st.Thinking = *p.Thinking
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := a.store.SaveSettings(sid, st); err != nil {
		return err
	}
	a.publishSession(sid)
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
	if err := a.store.Reset(sid); err != nil {
		return err
	}
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
	a.cancelRun(s)
	return nil
}

func (a *App) cancelRun(s *sess) {
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
	Blocked   bool             `json:"blocked"` // 包含 nano-agent 自己，不能当工作目录
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
	out := &DirListing{Path: l.Path, Home: l.Home, Blocked: l.Blocked, Entries: l.Entries, Truncated: l.Truncated}
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
	defer a.mu.Unlock()
	ap := a.approvals[n]
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
