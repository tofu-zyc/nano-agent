package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"nano-agent/gateway/internal/autonomy"
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
	Progress    string `json:"progress,omitempty"`
	ProgressAt  int64  `json:"progress_at,omitempty"`
	Note        string `json:"note,omitempty"`
	Running     bool   `json:"running,omitempty"`
}

// maxBranchesPerSession 是一个会话最多能分出来几个分支：不让自动分支无限展开。
const maxBranchesPerSession = 128

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
	res, err := a.sessionForkLocked(req)
	done := a.forkWork[res.BranchSID]
	a.mu.Unlock()
	if err != nil {
		return res, err
	}
	if done != nil && res.Status == session.BranchPreparing {
		select {
		case <-done:
		case <-a.ctx.Done():
			return res, a.ctx.Err()
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if br, ok := a.store.Branch(res.BranchSID); ok {
			res = a.forkResultOf(br, res.OperationID)
			res.Started = br.Status == session.BranchRunning
			if br.Status == session.BranchNeedsAttention {
				return res, errors.New(br.Note)
			}
		}
	}
	return res, nil
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
	if req.Start && strings.TrimSpace(req.Prompt) == "" {
		return SessionForkResult{}, errors.New("启动分支必须提供 prompt")
	}
	if autonomousForker(req.By) {
		if a.branchDepth(src)+1 > maxAutoBranchDepth {
			return SessionForkResult{}, fmt.Errorf("自动分支最多 %d 层；想比较别的做法请在主人的会话里手动 Fork", maxAutoBranchDepth)
		}
		if req.Start {
			if err := a.checkAutoBranchRoom(a.execAgentID(src)); err != nil {
				return SessionForkResult{}, err
			}
		}
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
			if br.Status == session.BranchNeedsAttention && op.Stage == "snapshot_ready" {
				a.recoverForksLocked()
				br, _ = a.store.Branch(op.BranchSID)
			}
			return a.forkResultOf(br, op.OperationID), nil
		}
	}
	if len(a.store.Children(src)) >= maxBranchesPerSession {
		return SessionForkResult{}, fmt.Errorf("这个会话已达到保留上限 %d；请显式删除不用的分支后再创建", maxBranchesPerSession)
	}
	op, err := a.store.BeginOp(session.ForkOp{Version: 2, RequestID: requestID, ParentSID: src, BranchSID: a.newBranchSID(), Stage: "reserved"})
	if err != nil {
		return SessionForkResult{}, err
	}
	if op.BranchSID != "" { // 上一个进程受理过但没做完
		if br, ok := a.store.Branch(op.BranchSID); ok {
			return a.forkResultOf(br, op.OperationID), nil
		}
	}
	if op.BranchSID == "" {
		for _, br := range a.store.Branches() {
			if br.OperationID == op.OperationID {
				op.BranchSID = br.SID
				break
			}
		}
		if op.BranchSID == "" {
			op.BranchSID = a.newBranchSID()
		}
		if err := a.store.UpdateOp(op); err != nil {
			return SessionForkResult{}, err
		}
		if br, ok := a.store.Branch(op.BranchSID); ok {
			return a.forkResultOf(br, op.OperationID), nil
		}
	}
	agentID := a.execAgentID(src)
	meta := session.BranchMeta{
		SID:            op.BranchSID,
		ParentSID:      src,
		OwnerAgent:     agentID,
		CreatedBy:      by,
		CreatedAt:      a.now().Unix(),
		Title:          title,
		Intent:         req.Prompt,
		Status:         session.BranchPreparing,
		Delivery:       delivery,
		OperationID:    op.OperationID,
		RequestID:      requestID,
		Generation:     a.store.Settings(src).SessionGeneration,
		StartRequested: req.Start,
	}
	if source := a.sessions[src]; source != nil && source.run != nil {
		meta.OriginRunID = source.run.runID
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
		if err := a.store.SetBranch(meta.SID, func(m *session.BranchMeta) {
			m.Status = session.BranchPendingBoundary
			m.Note = "等源会话这一轮结束"
		}); err != nil {
			return SessionForkResult{}, err
		}
		a.pendingForks[src] = append(a.pendingForks[src], forkPending{BranchSID: meta.SID, Prompt: req.Prompt, Start: req.Start, By: by})
		got, _ := a.store.Branch(meta.SID)
		return a.forkResultOf(got, op.OperationID), nil
	}
	res, err := a.materializeForkLocked(meta.SID)
	if err != nil {
		return SessionForkResult{}, err
	}
	return res, nil
}

// Recovery never substitutes today's context for an interrupted boundary.
// A durable completed snapshot can finish publication; an unknown source run
// stays visible for review and is not automatically replayed.
func (a *App) recoverForksLocked() {
	for _, br := range a.store.Branches() {
		if a.forkWork[br.SID] != nil {
			continue
		}
		if current := a.sessions[br.SID]; current != nil && current.run != nil {
			continue
		}
		if source := a.sessions[br.ParentSID]; source != nil && source.run != nil && br.OriginRunID != "" && br.OriginRunID == source.run.runID && br.Status == session.BranchPendingBoundary {
			continue
		}
		if br.Status == session.BranchRunning {
			if err := a.store.SetBranch(br.SID, func(m *session.BranchMeta) {
				m.Status = session.BranchNeedsAttention
				m.Note = "上次分支执行中断，先确认真实状态，再显式继续；不重放旧工具"
			}); err != nil {
				a.log.Error("记录分支中断失败", "err", err)
			}
			continue
		}
		if br.Status != session.BranchPreparing && br.Status != session.BranchPendingBoundary && br.Status != session.BranchNeedsAttention {
			continue
		}
		op, ok := a.store.OpByRequest(br.RequestID)
		if br.Status == session.BranchNeedsAttention && (!ok || op.Stage != "snapshot_ready") {
			continue
		}
		var recoveryErr error
		if ok && op.BranchSID == br.SID && op.OperationID == br.OperationID && op.Stage == "snapshot_ready" && op.SnapshotID != "" && a.checkForkPublication(op) == nil {
			if snap, exists := a.store.Snapshot(op.SnapshotID); exists {
				recoveryErr = a.store.ReadyBranch(br.SID, snap, session.BranchReady)
				if recoveryErr == nil {
					continue
				}
			} else {
				recoveryErr = errors.New("已登记快照缺失")
			}
		} else {
			recoveryErr = errors.New("来源执行已中断，无法确认所需边界；请检查后显式继续或重新分叉")
		}
		if err := a.store.SetBranch(br.SID, func(m *session.BranchMeta) { m.Status = session.BranchNeedsAttention; m.Note = recoveryErr.Error() }); err != nil {
			a.log.Error("恢复分支状态失败", "branch", br.SID, "err", err)
		}
	}
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
	if err := a.ctx.Err(); err != nil {
		return SessionForkResult{}, err
	}
	meta, ok := a.store.Branch(branchSID)
	if !ok {
		return SessionForkResult{}, errors.New("没有这个分支")
	}
	if a.forkWork == nil {
		a.forkWork = map[string]chan struct{}{}
		a.forkSlots = make(chan struct{}, 2)
	}
	if a.forkWork[branchSID] != nil {
		return a.forkResultOf(meta, meta.OperationID), nil
	}
	if len(a.forkWork) >= 32 {
		return SessionForkResult{}, errors.New("快照队列已满，请稍后重试")
	}
	src := a.store.Settings(meta.ParentSID)
	src.Agent = meta.OwnerAgent
	if m, _, ok := a.modelOf(meta.ParentSID); ok {
		src.Model = m.ID
	}
	src.Thinking = a.thinkingOf(meta.ParentSID)
	w := a.workspaceOf(meta.ParentSID)
	src.Workspaces, src.WorkspacesSet = w.ids(), true
	src.Mode = w.mode
	if !a.isAdmin(meta.CreatedBy) {
		src.Mode = a.runMode(w, meta.CreatedBy)
	}
	src.SnapshotPersona, _ = a.personaFor(meta.ParentSID)
	lease, err := a.store.FreezeSnapshot(meta.ParentSID, src)
	if err != nil {
		return SessionForkResult{}, err
	}
	if err = a.store.SetBranch(branchSID, func(m *session.BranchMeta) {
		m.Status = session.BranchPreparing
		m.Note = "正在保存固定边界快照"
	}); err != nil {
		lease.Close()
		return SessionForkResult{}, err
	}
	done := make(chan struct{})
	a.forkWork[branchSID] = done
	a.forkWG.Add(1)
	go func() {
		defer a.forkWG.Done()
		select {
		case a.forkSlots <- struct{}{}:
		case <-a.ctx.Done():
			lease.Close()
			a.mu.Lock()
			delete(a.forkWork, branchSID)
			close(done)
			a.mu.Unlock()
			return
		}
		snap, copyErr := lease.Write()
		lease.Close()
		<-a.forkSlots
		a.mu.Lock()
		defer a.mu.Unlock()
		defer close(done)
		defer delete(a.forkWork, branchSID)
		current, exists := a.store.Branch(branchSID)
		if a.ctx.Err() != nil {
			return
		}
		if !exists || current.OperationID != meta.OperationID || current.Status != session.BranchPreparing {
			return
		}
		err := copyErr
		if err == nil {
			op, ok := a.store.OpByRequest(meta.RequestID)
			if !ok || op.BranchSID != branchSID {
				err = errors.New("分叉操作记录缺失或归属不匹配")
			} else if checkErr := a.checkForkPublication(op); checkErr != nil {
				err = checkErr
			} else {
				op.SnapshotID = snap.SnapshotID
				op.Stage = "snapshot_ready"
				err = a.store.UpdateOp(op)
				if err == nil {
					err = a.store.ReadyBranch(branchSID, snap, session.BranchReady)
				}
			}
		}
		if err != nil {
			if saveErr := a.store.SetBranch(branchSID, func(m *session.BranchMeta) {
				m.Status = session.BranchNeedsAttention
				m.Note = "快照未提交：" + err.Error()
			}); saveErr != nil {
				a.log.Error("保存分支恢复错误失败", "branch", branchSID, "err", saveErr)
			}
			a.log.Error("保存分支快照失败", "branch", branchSID, "err", err)
			return
		}
		a.lifecycleExperienceFor(meta.OwnerAgent, meta.ParentSID, "在稳定边界创建了分支 "+branchSID)
		a.publish(events.Event{T: "branch", SID: branchSID, Name: meta.ParentSID, Kind: "ready", Data: map[string]any{"status": session.BranchReady, "snapshot_id": snap.SnapshotID, "title": meta.Title}})
		a.store.UnloadHistory(branchSID)
		if current.StartRequested && strings.TrimSpace(current.Intent) != "" {
			if err := a.startBranchRunLocked(branchSID, current.Intent, current.CreatedBy); err != nil {
				// 暂停或预算用完：分支保留就绪状态，等恢复后再由 agent 自己继续
				_ = a.store.SetBranch(branchSID, func(m *session.BranchMeta) {
					if m.Status == session.BranchRunning {
						m.Status = session.BranchPaused
					}
					m.Note = "这次没有开始：" + err.Error()
				})
				a.log.Info("自主分支没有开始", "branch", branchSID, "err", err)
			}
		}
	}()
	meta.Status = session.BranchPreparing
	return a.forkResultOf(meta, meta.OperationID), nil
}

func (a *App) checkForkPublication(op session.ForkOp) error {
	if op.Version > 2 {
		return errors.New("分叉操作版本过新")
	}
	gen, rev, err := a.store.LifecycleVersion(op.BranchSID)
	if err != nil {
		return err
	}
	if gen != op.TargetGeneration || rev != op.TargetRevision {
		return errors.New("目标已被重置或换代，旧分叉发布授权失效")
	}
	return nil
}

// startBranchRunLocked 在分支里排一条自己的任务并开跑。分支没有聊天窗口，输出只写进分支。
// 自动来源（agent 自己继续分支）在这里统一过一次准入：并发、暂停、当天预算和归属，
// 用户手动继续不受这些自动限制影响（评审 §24 R2）。调用方持有锁。
func (a *App) startBranchRunLocked(sid, prompt, by string) error {
	m, _ := a.store.Branch(sid)
	if autonomousForker(by) {
		if owner := autonomousOwner(by); owner != "" && m.OwnerAgent != "" && m.OwnerAgent != owner {
			return errors.New("这个分支不属于你，不能自己继续")
		}
		if err := a.checkAutoBranchRoom(m.OwnerAgent); err != nil {
			return err
		}
		if err := a.autoActivityRoom(m.OwnerAgent); err != nil {
			return err
		}
	}
	s := a.sessOf(sid)
	s.queue = append(s.queue, queued{
		m:     channel.Message{ChatID: sid, SenderID: by, Text: prompt, MsgID: "fork-" + sid},
		who:   by,
		inner: true, // 不对任何窗口回复：它是分支自己的任务
	})
	_ = a.store.SetBranch(sid, func(m *session.BranchMeta) {
		m.Status = session.BranchRunning
		m.LastActive = a.now().Unix()
		// 最近进展：从真实执行状态生成，不调模型、不读历史
		m.Progress = clip("运行中："+strings.TrimSpace(prompt), 200)
		m.ProgressAt = a.now().Unix()
	})
	a.startRun(s)
	return nil
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
		if source := a.sessions[sid]; source != nil && source.run != nil {
			r := source.run
			if (r.outcome != "" && r.outcome != proto.StatusOK) || (got.OriginRunID != "" && got.OriginRunID != r.runID) {
				if err := a.store.SetBranch(p.BranchSID, func(m *session.BranchMeta) {
					m.Status = session.BranchCancelled
					m.Note = "来源在完整边界前中止，未发布快照"
				}); err != nil {
					a.log.Error("保存分叉取消失败", "err", err)
				}
				continue
			}
		}
		if _, err := a.materializeForkLocked(p.BranchSID); err != nil {
			a.log.Warn("边界到了但分支没做成", "branch", p.BranchSID, "err", err)
			continue
		}
		if p.Start && strings.TrimSpace(p.Prompt) != "" {
			// The copy worker starts only after durable ready publication.
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
		Checkpoint: m.Checkpoint, CreatedAt: m.CreatedAt, ReadyAt: m.ReadyAt, LastActive: m.LastActive, Note: m.Note,
		Progress: m.Progress, ProgressAt: m.ProgressAt}
	if s := a.sessions[m.SID]; s != nil && (s.run != nil || len(s.queue) > 0) {
		info.Running = true
	}
	return info
}

// 自主分支的默认限额（设计 §8.6）：用户自己的前台 Fork / 继续不受这些数字限制。
const (
	maxAutoBranchRunning = 1
	maxAutoBranchQueued  = 2
	maxAutoBranchDepth   = 3
)

// autonomousForker 判断这次分叉/继续是不是 agent 自己发起的（而不是主人手动操作）。
func autonomousForker(by string) bool {
	for _, prefix := range []string{"life:", "hook:", "dream:"} {
		if strings.HasPrefix(by, prefix) {
			return true
		}
	}
	return false
}

// branchDepth 是一个分支往上数几层：直接挂在普通会话上的分支是 1。
func (a *App) branchDepth(sid string) int {
	depth := 0
	for i := 0; i < 8; i++ {
		m, ok := a.store.Branch(sid)
		if !ok || m.ParentSID == "" {
			return depth
		}
		depth++
		sid = m.ParentSID
	}
	return depth
}

// autonomousBranchLoad 数一个 agent 正在跑和在排队的自主分支。
func (a *App) autonomousBranchLoad(agent string) (running, queued int) {
	if agent == "" {
		return 0, 0
	}
	for sid, s := range a.sessions {
		m, ok := a.store.Branch(sid)
		if !ok || m.OwnerAgent != agent {
			continue
		}
		if s.run != nil {
			running++
		}
		if len(s.queue) > 0 {
			queued++
		}
	}
	return running, queued
}

// checkAutoBranchRoom 在 agent 自己又要开一个分支 run 之前检查并发和排队上限。
func (a *App) checkAutoBranchRoom(owner string) error {
	running, queued := a.autonomousBranchLoad(owner)
	if running >= maxAutoBranchRunning || queued >= maxAutoBranchQueued {
		return fmt.Errorf("这个 agent 已经有 %d 个自主分支在跑、%d 个在排队（上限 %d / %d），这次不开始",
			running, queued, maxAutoBranchRunning, maxAutoBranchQueued)
	}
	return nil
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
		if err := a.startBranchRunLocked(sid, prompt, by); err != nil {
			return BranchInfo{}, err
		}
	case "pause", "archive":
		if s := a.sessions[sid]; s != nil && s.run != nil {
			s.run.dropInbox = true
			a.cancelRun(s)
		}
		status := session.BranchPaused
		if action == "archive" {
			status = session.BranchArchived
		}
		if err := a.store.SetBranch(sid, func(m *session.BranchMeta) {
			m.Status = status
			m.Progress = clip(map[string]string{"pause": "已暂停（主人操作）", "archive": "已归档（主人操作）"}[action], 200)
			m.ProgressAt = a.now().Unix()
		}); err != nil {
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

// BranchesForSession 是某个会话的分支（网页父子折叠和分支卡片用）。
func (a *App) BranchesForSession(sid string) []BranchInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sid == "" {
		return []BranchInfo{}
	}
	if out := a.branchInfos(sid); out != nil {
		return out
	}
	return []BranchInfo{}
}

// BranchReport 以网页管理员的身份把分支结论带回父会话，回复走网页通道。
func (a *App) BranchReport(branchSID, by string) (BranchReport, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ep := endpoint{}
	if meta, ok := a.store.Branch(branchSID); ok && meta.ParentSID != "" {
		ep = endpoint{ch: "web", chat: meta.ParentSID}
	}
	return a.introduceBranchReportLocked(branchSID, by, ep)
}

// BranchReport 是主人主动从分支带回来的结论（设计 §8.7）：只有来源、分叉点和证据引用，
// 不合并分支历史，也不切换当前窗口。
type BranchReport struct {
	BranchSID  string   `json:"branch_sid"`
	ParentSID  string   `json:"parent_sid"`
	Title      string   `json:"title"`
	Checkpoint int      `json:"checkpoint"`
	SnapshotID string   `json:"snapshot_id,omitempty"`
	Conclusion string   `json:"conclusion"`
	Evidence   []string `json:"evidence,omitempty"`
	Injected   bool     `json:"injected"`
}

// IntroduceBranchReport 是主人主动"引入这个分支的结论"：把带来源和证据引用的报告放进
// 父会话的下一个安全边界，不合并历史、不自动切换窗口。调用方持有锁。
func (a *App) introduceBranchReportLocked(branchSID, by string, ep endpoint) (BranchReport, error) {
	sid := strings.TrimSpace(branchSID)
	m, ok := a.store.Branch(sid)
	if !ok {
		return BranchReport{}, errors.New("没有这个分支")
	}
	if !a.canAccessForkContext(by, "", sid) {
		return BranchReport{}, errors.New("没有这个分支的访问权限")
	}
	parent := m.ParentSID
	if parent == "" || !a.hasSession(parent) {
		return BranchReport{}, errors.New("分支的父会话已经不在了")
	}
	report := BranchReport{BranchSID: sid, ParentSID: parent, Title: m.Title, Checkpoint: m.Checkpoint, SnapshotID: m.SnapshotID}
	history := a.store.History(sid)
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" && strings.TrimSpace(history[i].Content) != "" {
			report.Conclusion = clip(history[i].Content, 1200)
			break
		}
	}
	if report.Conclusion == "" {
		return BranchReport{}, errors.New("分支里还没有可以带回来的结论")
	}
	if m.SnapshotID != "" {
		report.Evidence = append(report.Evidence, "分叉点快照 "+m.SnapshotID+"（第 "+strconv.Itoa(m.Checkpoint)+" 条）")
	}
	for _, ex := range a.mind(a.execAgentID(parent)).Executions {
		if ex.RunSID == sid && len(report.Evidence) < 4 {
			report.Evidence = append(report.Evidence, "分支里的执行："+clip(ex.Summary, 200))
		}
	}
	text := "[分支报告 " + sid + "「" + m.Title + "」]\n来源会话：" + parent + "；分叉点：第 " + strconv.Itoa(m.Checkpoint) + " 条；分支状态：" + m.Status + "。\n结论：" + report.Conclusion
	if len(report.Evidence) > 0 {
		text += "\n依据：" + strings.Join(report.Evidence, "；")
	}
	text += "\n这是主人主动带回来的分支结论，分支里更早的对话没有合并进来；需要过程就打开分支看。"
	// 结构化报告：网页按它画卡片，不靠正文前缀（用户自己写同样的话不会带这个字段）
	card := &session.Report{Kind: "branch_report", BranchSID: sid, ParentSID: parent, Title: m.Title,
		Conclusion: report.Conclusion, Checkpoint: m.Checkpoint, Status: m.Status, Evidence: append([]string(nil), report.Evidence...),
		SnapshotID: m.SnapshotID, At: a.now().Unix()}
	// 同一个结论只带回一次：分支之后又做了别的事、结论变了才允许再带
	for _, old := range a.store.History(parent) {
		if old.Role == "user" && strings.Contains(old.Content, "[分支报告 "+sid+"「") && strings.Contains(old.Content, "结论："+report.Conclusion) {
			return BranchReport{}, errors.New("这个分支的结论已经带回来过了")
		}
	}
	msg := channel.Message{ChatID: parent, SenderID: by, Text: text, MsgID: "report-" + sid + "-" + strconv.Itoa(m.Checkpoint)}
	s := a.sessOf(parent)
	if !a.queueFits(s, msg) {
		return BranchReport{}, errors.New("父会话的等待队列已满，稍后再试")
	}
	s.queue = append(s.queue, queued{m: msg, ep: ep, who: by, report: card})
	_ = a.store.SetBranch(sid, func(m *session.BranchMeta) {
		m.Progress = clip("已把结论带回来源会话："+report.Conclusion, 200)
		m.ProgressAt = a.now().Unix()
	})
	report.Injected = true
	a.publish(events.Event{T: "branch", SID: sid, Kind: "report"})
	if s.run == nil && !s.compacting {
		a.startRun(s)
	}
	return report, nil
}

// cmdBranch 是 /branch：列出这个会话的分支，或者把某个分支的结论带回它的父会话。
func (a *App) cmdBranch(c in, arg string) {
	if c.s == nil || c.s.sid == "" {
		a.answer(c, "还没有接到会话：先发一条消息再说 /branch")
		return
	}
	fields := strings.Fields(arg)
	if len(fields) > 0 && strings.EqualFold(fields[0], "report") {
		if len(fields) < 2 {
			a.answer(c, "用法：/branch report <分支编号>；编号用 /branch 查看。")
			return
		}
		rep, err := a.introduceBranchReportLocked(fields[1], c.who, c.ep)
		if err != nil {
			a.answer(c, "没带回来："+err.Error())
			return
		}
		a.answer(c, fmt.Sprintf("已经把分支 %s 的结论放到「%s」的下一个安全边界（分叉点第 %d 条）；分支历史和当前窗口都没有被改动。",
			rep.BranchSID, rep.ParentSID, rep.Checkpoint))
		return
	}
	list := a.branchInfos(c.s.sid)
	if len(list) == 0 {
		a.answer(c, "这个会话还没有分支。分支都是安静运行的：过程留在分支里，需要时用 /branch report <编号> 带结论回来。")
		return
	}
	var b strings.Builder
	b.WriteString("这个会话的分支：")
	for _, br := range list {
		fmt.Fprintf(&b, "\n- %s「%s」%s", br.SID, br.Title, br.Status)
		if br.Running {
			b.WriteString("（正在跑）")
		}
		if br.Checkpoint > 0 {
			fmt.Fprintf(&b, " 分叉点第 %d 条", br.Checkpoint)
		}
	}
	b.WriteString("\n/branch report <分支编号> 把那个分支的结论带回这个会话。")
	a.answer(c, b.String())
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
	if inv := a.sessionHook(sid); inv != nil {
		return inv.work.Delivery.Silent || inv.work.Spec.Handler.Conversation == "branch"
	}
	if a.branchDelivery(sid) == session.BranchDeliverySilent {
		return true
	}
	// 从安静分支派生出来的定时任务同样安静：任务自己那一轮不对外发声（评审 §24 R1）
	return a.silentTaskSID(sid)
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

// topicOfTrigger 是这次自主活动的话题：一个意图或一个日程。同一个话题复用同一个持久分支。
func topicOfTrigger(t autonomy.Trigger) string {
	if t.IntentionID != "" {
		return "in:" + t.IntentionID
	}
	if t.AgendaID != "" {
		return "ag:" + t.AgendaID
	}
	return ""
}

// topicBranch 是这次自主活动的话题已经在用的分支（还能继续时才返回）。调用方持有锁。
func (a *App) topicBranch(s *sess) (string, bool) {
	if s == nil || s.run == nil || s.mind == nil {
		return "", false
	}
	sid, ok := a.mind(s.mind.AgentID).BranchOfTopic(topicOfTrigger(s.mind.Trigger))
	if !ok {
		return "", false
	}
	meta, found := a.store.Branch(sid)
	if !found || meta.Status == session.BranchArchived || meta.Status == session.BranchCancelled {
		return "", false
	}
	return sid, true
}

// linkForkTopic 把这次分叉记到当前自主活动的话题上，下一次接着用同一个分支。调用方持有锁。
func (a *App) linkForkTopic(s *sess, branchSID string) {
	if s == nil || s.run == nil || s.mind == nil || branchSID == "" {
		return
	}
	if topic := topicOfTrigger(s.mind.Trigger); topic != "" {
		a.mind(s.mind.AgentID).LinkTopicBranch(topic, branchSID, a.now())
		a.saveMinds()
	}
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
		// 同一个话题不每次醒来都 Fork：已经有可用分支就复用它，上下文和进度都还在那里
		if reuse, ok := a.topicBranch(s); ok {
			return fmt.Sprintf("这个话题已经有分支 %s；要用就在它里面 continue（Session 工具，branch_sid=%s），这次没有新建分支。", reuse, reuse), true
		}
		src := args.SourceSID
		if src == "" {
			src = s.sid
		}
		res, err := a.sessionForkLocked(SessionForkRequest{SourceSID: src, CallerSID: s.sid, Title: args.Title, Prompt: args.Prompt,
			Start: args.Start != nil && *args.Start, Delivery: session.BranchDeliverySilent, RequestID: args.RequestID, By: from})
		if err != nil {
			return err.Error(), false
		}
		a.linkForkTopic(s, res.BranchSID)
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
		if action == "continue" {
			a.linkForkTopic(s, args.BranchSID)
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
	} else if res.Status == session.BranchPreparing {
		text = "已创建分支记录 " + res.BranchSID + "，正在保存固定边界上下文（preparing），尚未就绪；原聊天窗口继续使用。"
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
		// 内心活动是 agent 自己的会话：它可以处理自己的内心上下文（登记 Hook、约回访都要这一条），
		// 除此以外仍然只能碰主人的会话和公开群聊。
		ownLife := isLifeSID(target) && a.routeAgentID(target) == id
		return a.execAgentID(target) == id && (ownLife || a.isOwnerSID(id, target) || a.isGroupSession(target))
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
    "delivery": {"type": "string", "enum": ["silent"], "description": "结果留在分支里；只有用户显式操作可以开启报告"},
    "request_id": {"type": "string", "description": "幂等键：同一个 request_id 重试返回同一个分支"}
  },
  "required": ["action"]
}`
	return proto.ToolSpec{Name: "Session", Description: "在会话之间分叉（Fork）：把一个会话在某个安全边界上的上下文保存成不可变快照，" +
		"分支带着这份上下文独立继续，源会话照常用；分支默认安静运行，结果不发回原来的聊天。action 取 fork / list_branches / inspect / continue / pause / archive。" +
		"在工具批次里调用 fork 会先返回 pending_boundary，等这批调用结束才真正建好；用同一个 request_id 重试返回同一个分支。",
		InputSchema: json.RawMessage(schema)}
}

// autonomousOwner 是自动来源对应的 agent：life:default / hook:<agent> / dream:<agent>。
func autonomousOwner(by string) string {
	for _, prefix := range []string{"life:", "hook:", "dream:"} {
		if id, ok := strings.CutPrefix(by, prefix); ok {
			return id
		}
	}
	return ""
}
