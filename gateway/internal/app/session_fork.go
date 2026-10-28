package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// ---- 会话 Fork：显式入口与持久操作（设计文档 §8 / F1） ----
//
// 一次 Fork 是一个持久操作：先落 ForkOp（request_id / operation_id），再建分支元信息，
// 到了安全边界才做快照并发布 ready。重试、断线、重启都返回同一个操作和分支；
// 同一个 request_id 永远不会建出第二个分支。
//
// 边界：源会话正在生成、有工具批次没闭合、正在整理上下文时，先回 pending_boundary。
// 模型在同一批工具里调用 Session.fork 时也是这样——工具自己先拿到结果，批次闭合后才做快照，
// 不会出现"Fork 等批次、批次等 Fork"的死锁。

// SessionForkRequest 是创建分支的请求（Session 工具或 /session fork）。
type SessionForkRequest struct {
	CallerSID string `json:"-"` // 宿主确认的调用场合，不能来自模型参数
	SourceSID string `json:"source_sid"`
	Title     string `json:"title"`
	Prompt    string `json:"prompt"`
	Start     bool   `json:"start"`
	Delivery  string `json:"delivery"`
	RequestID string `json:"request_id"`
	By        string `json:"created_by"`
}

// SessionForkResult 是创建分支的结果。Status=pending_boundary 时表示还在等源会话的边界，
// 不能当成已经就绪，也不能当成任务完成。
type SessionForkResult struct {
	BranchSID   string `json:"branch_sid"`
	ParentSID   string `json:"parent_sid"`
	SnapshotID  string `json:"snapshot_id,omitempty"`
	OperationID string `json:"operation_id"`
	Status      string `json:"status"`
	Title       string `json:"title"`
	Started     bool   `json:"started,omitempty"`
}

// BranchInfo 是给界面和工具看的分支概况（只读元信息，不读历史）。
type BranchInfo struct {
	SID         string `json:"sid"`
	ParentSID   string `json:"parent_sid"`
	RootSID     string `json:"root_sid,omitempty"`
	SnapshotID  string `json:"snapshot_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	Title       string `json:"title"`
	Intent      string `json:"intent,omitempty"`
	Status      string `json:"status"`
	Delivery    string `json:"delivery_policy"`
	Checkpoint  int    `json:"fork_checkpoint"`
	CreatedAt   int64  `json:"created_at"`
	ReadyAt     int64  `json:"ready_at,omitempty"`
	LastActive  int64  `json:"last_active,omitempty"`
	Note        string `json:"note,omitempty"`
	Running     bool   `json:"running,omitempty"`
}

// maxBranchesPerSession 是一个会话最多能分出来几个分支：不让自动分支无限展开。
const maxBranchesPerSession = 16

// forkPending 是一个已经落盘、还在等源会话安全边界的 Fork。
type forkPending struct {
	BranchSID string
	Prompt    string
	Start     bool
	By        string
}

// isBranchSID 报告这个会话是不是分支。
func (a *App) isBranchSID(sid string) bool {
	if a.store == nil {
		return false
	}
	_, ok := a.store.Branch(sid)
	return ok
}

// branchMeta 取分支元信息。
func (a *App) branchMeta(sid string) (session.BranchMeta, bool) {
	if a.store == nil {
		return session.BranchMeta{}, false
	}
	return a.store.Branch(sid)
}

// newBranchSID 是分支的会话 ID。
func (a *App) newBranchSID() string {
	for i := 0; i < 100; i++ {
		sid := "br-" + randHex(6)
		if a.store == nil {
			return sid
		}
		if _, ok := a.store.Branch(sid); ok {
			continue
		}
		if a.store.Exists(sid) {
			continue
		}
		return sid
	}
	return fmt.Sprintf("br-%d", time.Now().UnixNano())
}

// SessionFork 创建一个会话分支。用户命令和 agent 的 Session 工具都走这里。
func (a *App) SessionFork(req SessionForkRequest) (SessionForkResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionForkLocked(req)
}

func (a *App) sessionForkLocked(req SessionForkRequest) (SessionForkResult, error) {
	src := strings.TrimSpace(req.SourceSID)
	if src == "" {
		src = mainSID
	}
	if !a.canAccessForkContext(req.By, req.CallerSID, src) {
		return SessionForkResult{}, errors.New("没有这个来源会话的访问权限")
	}
	if !a.hasSession(src) && !a.isBranchSID(src) {
		return SessionForkResult{}, errors.New("没有这个会话")
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "分支 " + a.now().Format("01-02 15:04")
	}
	delivery := session.BranchDeliverySilent
	if req.Delivery == session.BranchDeliveryReport {
		delivery = session.BranchDeliveryReport // 只有用户明确要求时才带回结果
	}
	by := req.By
	if by == "" {
		by = "system"
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		// 没给 request_id 的（用户手动点的）：按来源、标题和当前时刻生成，仍然可审计
		requestID = fmt.Sprintf("fork:%s:%d", src, a.now().UnixNano())
	}
	// 幂等责任绑定真实主体和来源范围；模型不能借别人的 request_id
	// 取出另一个人的私有上下文。
	scope, _ := json.Marshal([]string{by, req.CallerSID, src, requestID})
	requestID = string(scope)
	// 幂等：同一个 request_id 返回同一个操作和分支
	if op, ok := a.store.OpByRequest(requestID); ok {
		if br, ok := a.store.Branch(op.BranchSID); ok {
			return a.forkResultOf(br, op.OperationID), nil
		}
	}
	if len(a.store.Children(src)) >= maxBranchesPerSession {
		return SessionForkResult{}, fmt.Errorf("这个会话已经有 %d 个分支了，先归档一些再建新的", maxBranchesPerSession)
	}
	op, err := a.store.BeginOp(session.ForkOp{RequestID: requestID, ParentSID: src})
	if err != nil {
		return SessionForkResult{}, err
	}
	if op.BranchSID != "" { // 上一个进程受理过但没做完
		if br, ok := a.store.Branch(op.BranchSID); ok {
			return a.forkResultOf(br, op.OperationID), nil
		}
	}
	agentID := a.execAgentID(src)
	meta := session.BranchMeta{
		SID:         a.newBranchSID(),
		ParentSID:   src,
		OwnerAgent:  agentID,
		CreatedBy:   by,
		CreatedAt:   a.now().Unix(),
		Title:       title,
		Intent:      req.Prompt,
		Status:      session.BranchPreparing,
		Delivery:    delivery,
		OperationID: op.OperationID,
		RequestID:   requestID,
		Generation:  a.store.Settings(src).SessionGeneration,
	}
	meta.Checkpoint = a.forkCheckpoint(src)
	if err := a.store.BeginBranch(meta); err != nil {
		return SessionForkResult{}, err
	}
	op.BranchSID = meta.SID
	if err := a.store.UpdateOp(op); err != nil {
		return SessionForkResult{}, err
	}
	a.log.Info("受理会话分支", "branch", meta.SID, "parent", src, "op", op.OperationID, "by", by)
	// 同批工具里调用、或者源会话正在跑：先回 pending_boundary，等安全边界再做快照
	if a.forkWaitBoundary(src) {
		_ = a.store.SetBranch(meta.SID, func(m *session.BranchMeta) {
			m.Status = session.BranchPendingBoundary
			m.Note = "等源会话这一轮结束"
		})
		a.pendingForks[src] = append(a.pendingForks[src], forkPending{BranchSID: meta.SID, Prompt: req.Prompt, Start: req.Start, By: by})
		got, _ := a.store.Branch(meta.SID)
		return a.forkResultOf(got, op.OperationID), nil
	}
	res, err := a.materializeForkLocked(meta.SID)
	if err != nil {
		return SessionForkResult{}, err
	}
	if req.Start && strings.TrimSpace(req.Prompt) != "" {
		a.startBranchRunLocked(meta.SID, req.Prompt, by)
		res.Started = true
	}
	return res, nil
}

// forkCheckpoint 是源会话现在的分叉点（绝对位置）。
func (a *App) forkCheckpoint(sid string) int {
	return a.store.Settings(sid).Base + len(a.store.History(sid))
}

// forkWaitBoundary 报告源会话现在是不是不能取快照。
func (a *App) forkWaitBoundary(sid string) bool {
	s := a.sessions[sid]
	if s == nil {
		return false
	}
	return s.run != nil || s.compacting
}

// materializeForkLocked 做快照、把分支连上去、发布 ready。调用方持有锁。
func (a *App) materializeForkLocked(branchSID string) (SessionForkResult, error) {
	meta, ok := a.store.Branch(branchSID)
	if !ok {
		return SessionForkResult{}, errors.New("没有这个分支")
	}
	src := a.store.Settings(meta.ParentSID)
	snapID := a.store.SnapshotIDFor(meta.ParentSID, src)
	snap, err := a.store.WriteSnapshot(meta.ParentSID, snapID, src)
	if err != nil {
		a.log.Warn("做分叉快照失败", "branch", branchSID, "err", err)
		_ = a.store.SetBranch(branchSID, func(m *session.BranchMeta) {
			m.Status = session.BranchNeedsAttention
			m.Note = "快照失败：" + err.Error()
		})
		return SessionForkResult{}, err
	}
	if err := a.store.ReadyBranch(branchSID, snap, session.BranchReady); err != nil {
		return SessionForkResult{}, err
	}
	got, _ := a.store.Branch(branchSID)
	a.publish(events.Event{T: "branch", SID: branchSID, Name: got.ParentSID, Kind: "ready",
		Data: map[string]any{"status": got.Status, "snapshot_id": snap.SnapshotID, "title": got.Title}})
	a.log.Info("分支就绪", "branch", branchSID, "snapshot", snap.SnapshotID)
	return a.forkResultOf(got, got.OperationID), nil
}

// startBranchRunLocked 在分支里排一条自己的任务并开跑。分支没有聊天窗口，输出只写进分支。
func (a *App) startBranchRunLocked(sid, prompt, by string) {
	s := a.sessOf(sid)
	s.queue = append(s.queue, queued{
		m:     channel.Message{ChatID: sid, SenderID: by, Text: prompt, MsgID: "fork-" + sid},
		who:   by,
		inner: true, // 不对任何窗口回复：它是分支自己的任务
	})
	_ = a.store.SetBranch(sid, func(m *session.BranchMeta) {
		m.Status = session.BranchRunning
		m.LastActive = a.now().Unix()
	})
	a.startRun(s)
}

// forkResultOf 把分支元信息转成给调用方的结果。
func (a *App) forkResultOf(meta session.BranchMeta, opID string) SessionForkResult {
	if opID == "" {
		opID = meta.OperationID
	}
	return SessionForkResult{BranchSID: meta.SID, ParentSID: meta.ParentSID, SnapshotID: meta.SnapshotID,
		OperationID: opID, Status: meta.Status, Title: meta.Title}
}

// finishPendingForks 在源会话到达安全边界之后，把等着的分支做出来。调用方持有锁。
func (a *App) finishPendingForks(sid string) {
	pending := a.pendingForks[sid]
	if len(pending) == 0 {
		return
	}
	delete(a.pendingForks, sid)
	for _, p := range pending {
		got, ok := a.store.Branch(p.BranchSID)
		if !ok {
			continue
		}
		if got.Status != session.BranchPendingBoundary && got.Status != session.BranchPreparing {
			continue
		}
		if _, err := a.materializeForkLocked(p.BranchSID); err != nil {
			a.log.Warn("边界到了但分支没做成", "branch", p.BranchSID, "err", err)
			continue
		}
		if p.Start && strings.TrimSpace(p.Prompt) != "" {
			a.startBranchRunLocked(p.BranchSID, p.Prompt, p.By)
		}
	}
}

// Branches 是分支列表（网页和 Session 工具用）。
func (a *App) Branches() []BranchInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.branchInfos("")
}

// branchInfos 列出分支；parent 非空时只看这个会话的。调用方持有锁。
// 只读元信息，不碰历史（100 个空闲分支不会因此各加载一份 History）。
func (a *App) branchInfos(parent string) []BranchInfo {
	if a.store == nil {
		return nil
	}
	var metas []session.BranchMeta
	if parent == "" {
		metas = a.store.Branches()
	} else {
		metas = a.store.Children(parent)
	}
	out := make([]BranchInfo, 0, len(metas))
	for _, m := range metas {
		out = append(out, a.branchInfo(m))
	}
	return out
}

func (a *App) branchInfo(m session.BranchMeta) BranchInfo {
	info := BranchInfo{SID: m.SID, ParentSID: m.ParentSID, RootSID: m.RootSID, SnapshotID: m.SnapshotID,
		OperationID: m.OperationID, Title: m.Title, Intent: m.Intent, Status: m.Status, Delivery: m.Delivery,
		Checkpoint: m.Checkpoint, CreatedAt: m.CreatedAt, ReadyAt: m.ReadyAt, LastActive: m.LastActive, Note: m.Note}
	if s := a.sessions[m.SID]; s != nil && (s.run != nil || len(s.queue) > 0) {
		info.Running = true
	}
	return info
}

// SessionBranchAction 是分支的后续操作（continue / pause / archive / inspect）。
func (a *App) SessionBranchAction(action, sid, prompt, by string) (BranchInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionBranchActionLocked(action, sid, prompt, by)
}

// sessionBranchActionLocked 是 SessionBranchAction 的持锁版本（exec 路径里已经拿着锁）。
func (a *App) sessionBranchActionLocked(action, sid, prompt, by string) (BranchInfo, error) {
	if !a.canAccessForkContext(by, "", sid) {
		return BranchInfo{}, errors.New("没有这个分支的访问权限")
	}
	m, ok := a.store.Branch(sid)
	if !ok {
		return BranchInfo{}, errors.New("没有这个分支")
	}
	switch action {
	case "inspect":
		return a.branchInfo(m), nil
	case "continue":
		if m.SnapshotID == "" || m.Status == session.BranchPreparing || m.Status == session.BranchPendingBoundary || m.Status == session.BranchCancelled {
			return BranchInfo{}, errors.New("分支还没有就绪")
		}
		if m.Status == session.BranchArchived {
			return BranchInfo{}, errors.New("这个分支已经归档，先取消归档再继续")
		}
		if strings.TrimSpace(prompt) == "" {
			return BranchInfo{}, errors.New("继续分支要给出这一轮做什么")
		}
		if a.forkWaitBoundary(sid) {
			return BranchInfo{}, errors.New("这个分支正在跑，等它结束")
		}
		a.startBranchRunLocked(sid, prompt, by)
	case "pause", "archive":
		if s := a.sessions[sid]; s != nil && s.run != nil {
			s.run.dropInbox = true
			a.cancelRun(s)
		}
		status := session.BranchPaused
		if action == "archive" {
			status = session.BranchArchived
		}
		if err := a.store.SetBranch(sid, func(m *session.BranchMeta) { m.Status = status }); err != nil {
			return BranchInfo{}, err
		}
		if s := a.sessions[sid]; s != nil {
			s.queue = nil
		}
	default:
		return BranchInfo{}, fmt.Errorf("不认识的分支操作：%s", action)
	}
	got, _ := a.store.Branch(sid)
	a.publish(events.Event{T: "branch", SID: sid, Kind: action})
	return a.branchInfo(got), nil
}

// branchDelivery 是分支的有效投递策略（不是分支返回空）。
func (a *App) branchDelivery(sid string) string {
	m, ok := a.branchMeta(sid)
	if !ok {
		return ""
	}
	if m.Delivery == "" {
		return session.BranchDeliverySilent
	}
	return m.Delivery
}

// branchSilent 报告这个会话是不是"安静运行"。
func (a *App) branchSilent(sid string) bool {
	return a.branchDelivery(sid) == session.BranchDeliverySilent
}

// branchNote 是给模型看的分支说明：继承的历史是分叉前的经历，当前任务以这次的要求为准。
func (a *App) branchNote(sid string) string {
	m, ok := a.branchMeta(sid)
	if !ok {
		return ""
	}
	parent := m.ParentSID
	if p, ok := a.store.Branch(m.ParentSID); ok && p.Title != "" {
		parent = p.Title
	} else if info := a.sessionInfo(m.ParentSID); info.Title != "" {
		parent = info.Title
	}
	return "[分支] 这是一个从「" + parent + "」分出来的独立分支，上面的对话是分叉前的经历。" +
		"当前任务以这次的要求为准，不要自动重放已经做过的操作；结果默认留在分支里，不会发回原来的聊天。"
}

// handleSessionTool 处理 Session 工具（网关执行）。
func (a *App) handleSessionTool(s *sess, m *proto.Msg) (string, bool) {
	var args struct {
		Action    string `json:"action"`
		SourceSID string `json:"source_sid"`
		BranchSID string `json:"branch_sid"`
		Title     string `json:"title"`
		Prompt    string `json:"prompt"`
		Start     *bool  `json:"start"`
		Delivery  string `json:"delivery"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(m.Args, &args); err != nil {
		return "Session 工具的参数不是合法的 JSON：" + err.Error(), false
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "fork"
	}
	from := a.runBy(s)
	switch action {
	case "fork":
		src := args.SourceSID
		if src == "" {
			src = s.sid
		}
		res, err := a.sessionForkLocked(SessionForkRequest{SourceSID: src, CallerSID: s.sid, Title: args.Title, Prompt: args.Prompt,
			Start: args.Start != nil && *args.Start, Delivery: session.BranchDeliverySilent, RequestID: args.RequestID, By: from})
		if err != nil {
			return err.Error(), false
		}
		text := fmt.Sprintf("已创建分支 %s（%s），来源会话 %s，操作 %s。", res.BranchSID, res.Title, res.ParentSID, res.OperationID)
		switch res.Status {
		case session.BranchPendingBoundary:
			text += "（status=pending_boundary）源会话还在跑，等这一轮的工具批次和生成都结束之后才复制上下文、" +
				"发布 ready；这不算任务完成证据，不要假定分支已经在工作。用同一个 request_id 重试会返回同一个分支。"
		case session.BranchReady:
			text += "分叉点上下文已经保存成不可变快照，分支现在就可以用 Session 工具的 continue 继续。"
		default:
			text += "当前状态：" + res.Status + "。"
		}
		if res.Started {
			text += "分支任务已经排队，会在分支自己的上下文里执行，结果留在分支里。"
		}
		return text, true
	case "list_branches":
		src := args.SourceSID
		if src == "" {
			src = s.sid
		}
		if !a.canAccessForkContext(from, s.sid, src) {
			return "没有这个来源会话的访问权限", false
		}
		list := a.branchInfos(src)
		if len(list) == 0 {
			return "这个会话还没有分支。", true
		}
		var b strings.Builder
		b.WriteString("这个会话的分支：")
		for _, br := range list {
			fmt.Fprintf(&b, "\n- %s「%s」%s", br.SID, br.Title, br.Status)
			if br.Intent != "" {
				b.WriteString("：" + clip(br.Intent, 60))
			}
		}
		return b.String(), true
	case "inspect", "continue", "pause", "archive":
		if args.BranchSID == "" {
			return "要指定 branch_sid", false
		}
		if !a.canAccessForkContext(from, s.sid, args.BranchSID) {
			return "没有这个分支的访问权限", false
		}
		info, err := a.sessionBranchActionLocked(action, args.BranchSID, args.Prompt, from)
		if err != nil {
			return err.Error(), false
		}
		return fmt.Sprintf("分支 %s 现在是 %s（%s）。", info.SID, info.Status, info.Title), true
	}
	return "不认识的 Session 操作：" + action, false
}

// runBy 是这一轮的发起人（没有在跑时是空）。
func (a *App) runBy(s *sess) string {
	if s == nil || s.run == nil {
		return ""
	}
	return s.run.from
}

// cmdSessionFork 是用户入口：/session fork [标题]
func (a *App) cmdSessionFork(c in, title string) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "分支 " + a.now().Format("01-02 15:04")
	}
	res, err := a.sessionForkLocked(SessionForkRequest{SourceSID: c.s.sid, CallerSID: c.s.sid, Title: title, By: c.who})
	if err != nil {
		a.answer(c, "创建分支失败："+err.Error())
		return
	}
	text := "已创建分支 " + res.BranchSID + "（" + res.Title + "）。它带着分叉点的上下文独立继续，" +
		"结果留在分支里，不会发到这个聊天。"
	if res.Status == session.BranchPendingBoundary {
		text = "已登记分支 " + res.BranchSID + "，等当前这一轮结束（工具批次和生成都完成）后才会复制上下文并发布 ready。"
	}
	a.answer(c, text)
}

// 同一 agent 并不代表不同用户的聊天可以互读。群内管理员也只能
// 按群聊范围操作；life 的正向授权来自同 agent 的主人会话或公开群聊。
func (a *App) canAccessForkContext(by, caller, target string) bool {
	if by == "" {
		return false
	}
	if caller != "" && caller == target {
		return true
	}
	if caller != "" && a.isGroupSession(caller) {
		br, ok := a.store.Branch(target)
		return ok && br.RootSID == caller
	}
	if br, ok := a.store.Branch(target); ok {
		if br.CreatedBy == by && (caller == "" || a.execAgentID(caller) == br.OwnerAgent) {
			return true
		}
		if caller != "" && a.isGroupSession(caller) {
			return br.RootSID == caller
		}
	}
	if id, life := strings.CutPrefix(by, "life:"); life {
		return a.execAgentID(target) == id && (a.isOwnerSID(id, target) || a.isGroupSession(target))
	}
	if caller != "" && a.isGroupSession(caller) {
		return false
	}
	if a.isAdmin(by) {
		return true
	}
	for _, ep := range a.eps {
		if ep.Session == target && ep.Who == by {
			return true
		}
	}
	return false
}

// sessionTool 是 Session 工具的定义：Fork、列分支、查看、继续、暂停、归档。
func sessionTool() proto.ToolSpec {
	const schema = `{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["fork", "list_branches", "inspect", "continue", "pause", "archive"], "description": "要做的事，默认 fork"},
    "source_sid": {"type": "string", "description": "从哪个会话分叉，默认当前会话"},
    "branch_sid": {"type": "string", "description": "inspect / continue / pause / archive 要操作的分支"},
    "title": {"type": "string", "description": "分支标题"},
    "prompt": {"type": "string", "description": "start=true 或 continue 时，这一轮要在分支里做什么"},
    "start": {"type": "boolean", "description": "建完就在分支里开始跑"},
    "delivery": {"type": "string", "enum": ["silent", "report"], "description": "默认 silent：结果留在分支里，不发回原来的聊天"},
    "request_id": {"type": "string", "description": "幂等键：同一个 request_id 重试返回同一个分支"}
  },
  "required": ["action"]
}`
	return proto.ToolSpec{Name: "Session", Description: "在会话之间分叉（Fork）：把一个会话在某个安全边界上的上下文保存成不可变快照，" +
		"分支带着这份上下文独立继续，源会话照常用；分支默认安静运行，结果不发回原来的聊天。action 取 fork / list_branches / inspect / continue / pause / archive。" +
		"在工具批次里调用 fork 会先返回 pending_boundary，等这批调用结束才真正建好；用同一个 request_id 重试返回同一个分支。",
		InputSchema: json.RawMessage(schema)}
}
