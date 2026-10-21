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
	Node        string         `json:"node"`        // 在哪台机器上工作，空表示不挑
	NodeSet     bool           `json:"node_set"`    // 会话单独设置了机器（否则用默认的）
	NodeOnline  bool           `json:"node_online"` // 那台机器的内核在线
	RunNode     string         `json:"run_node"`    // 正在跑这一轮的内核
	Running     bool           `json:"running"`
	Pending     int            `json:"pending"` // 待审批的工具调用
	LastAt      int64          `json:"last_at"` // 最后一条消息的时间（秒）
	Messages    int            `json:"messages"`
	Dirs        []string       `json:"dirs"`
	RO          []string       `json:"ro"`
	Mode        string         `json:"mode"`
	Thinking    string         `json:"thinking"`
	Persona     string         `json:"persona"`      // 用的人格文件名
	PersonaFile string         `json:"persona_file"` // 这个会话专属人格的文件名（不一定存在）
	Usage       llm.Usage      `json:"usage"`        // 历史里记下的累计用量
	Compacted   int            `json:"compacted"`
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
	Started   int64          `json:"started"`
	Kernels   []KernelInfo   `json:"kernels"`
	Node      string         `json:"node"` // 默认的机器
	Totals    Totals         `json:"totals"`
	Sessions  []SessionInfo  `json:"sessions"`
	Approvals []ApprovalInfo `json:"approvals"`
}

// Snapshot 返回网关当前的全貌：内核、所有会话（包括只在磁盘上的）、待审批、累计用量。
func (a *App) Snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	snap := Snapshot{Started: a.started.UnixMilli(), Totals: a.totals, Kernels: a.kernelList(), Node: a.defaultNode()}
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
	info := SessionInfo{SID: sid, Channel: "web", Endpoints: a.endpointsOf(sid), Title: st.Title, Dirs: w.dirs, RO: st.RO,
		Mode: w.mode, Thinking: a.thinkingOf(sid), Compacted: st.Base}
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
	info.Node, info.NodeSet, info.NodeOnline = w.node, st.Node != "", a.nodes[w.node] != nil
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
	Title    *string   `json:"title"`
	Node     *string   `json:"node"` // 空字符串表示用默认的机器
	Dirs     *[]string `json:"dirs"`
	RO       *[]string `json:"ro"`
	Mode     *string   `json:"mode"`
	Thinking *string   `json:"thinking"` // 空字符串表示用默认值
}

func (a *App) UpdateSession(sid string, p SessionPatch) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.hasSession(sid) {
		return errors.New("没有这个会话")
	}
	if p.Node != nil { // 先换机器（会清掉旧机器上的工作目录），再设置这台机器上的目录
		if _, err := a.setNode(sid, strings.TrimSpace(*p.Node)); err != nil {
			return err
		}
	}
	st := a.store.Settings(sid)
	var errs []error
	clean := func(list []string) []string {
		out := []string{}
		for _, d := range list {
			d = cleanPath(strings.TrimSpace(d))
			if d == "" || slices.Contains(out, d) {
				continue
			}
			if msg := a.checkDir(d); msg != "" {
				errs = append(errs, errors.New(msg))
				continue
			}
			out = append(out, d)
		}
		return out
	}
	if p.Title != nil {
		st.Title = strings.TrimSpace(*p.Title)
		if len([]rune(st.Title)) > 40 {
			errs = append(errs, errors.New("名字最长 40 个字"))
		}
	}
	if p.Dirs != nil {
		st.Dirs, st.DirsSet = clean(*p.Dirs), true
	}
	if p.RO != nil {
		st.RO = clean(*p.RO)
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

// ListDir 请节点 node 上的内核列出那台机器上的一个目录，网页上选工作目录用。path 可以是 ~ 开头。
// node 为空时用默认的机器。
func (a *App) ListDir(ctx context.Context, node, path string) (*DirListing, error) {
	a.mu.Lock()
	if node == "" {
		node = a.defaultNode()
	}
	if node == "" {
		node = a.substitute()
	}
	if a.nodes[node] == nil {
		a.mu.Unlock()
		if node == "" {
			return nil, errors.New("没有在线的内核，没法浏览目录")
		}
		return nil, fmt.Errorf("%s 不在线，没法浏览它上面的目录", node)
	}
	a.lsSeq++
	id := fmt.Sprintf("ls%d", a.lsSeq)
	ch := make(chan *proto.Msg, 1)
	a.pendingLs[id] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pendingLs, id)
		a.mu.Unlock()
	}()
	if !a.hub.Send(node, proto.Msg{T: proto.TLs, ID: id, Path: path}) {
		return nil, errors.New("内核没有连接，没法浏览目录")
	}
	select {
	case m := <-ch:
		if m.OK == nil || !*m.OK {
			return nil, errors.New(m.Detail)
		}
		l := &DirListing{Path: m.Path, Home: m.Home, Blocked: m.Blocked, Entries: m.Entries, Truncated: m.Truncated}
		if l.Entries == nil {
			l.Entries = []proto.DirEntry{}
		}
		return l, nil
	case <-ctx.Done():
		return nil, errors.New("内核没有响应")
	}
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
	return nil
}
