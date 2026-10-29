package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/proto"
)

type JobRequest struct {
	Action   string `json:"action"`
	Node     string `json:"node,omitempty"`
	JobID    string `json:"job_id,omitempty"`
	Cursor   uint64 `json:"cursor,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
	WaitMS   int    `json:"wait_ms,omitempty"`
	After    string `json:"after,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}
type JobInfo struct {
	Revision     uint64 `json:"revision"`
	Node         string `json:"node"`
	JobID        string `json:"job_id"`
	Agent        string `json:"agent"`
	Creator      string `json:"created_by"`
	SID          string `json:"origin_sid"`
	RunID        string `json:"origin_run_id"`
	TurnID       int    `json:"origin_turn_id"`
	CallID       string `json:"origin_call_id"`
	Status       string `json:"status"`
	Execution    string `json:"execution"`
	Background   bool   `json:"background"`
	Silent       bool   `json:"silent"`
	CreatedAt    int64  `json:"created_at"`
	StartedAt    int64  `json:"started_at,omitempty"`
	EndedAt      int64  `json:"ended_at,omitempty"`
	DeadlineAt   int64  `json:"deadline_at,omitempty"`
	Sandbox      string `json:"sandbox_effective,omitempty"`
	OutputCursor uint64 `json:"output_cursor"`
	OutputGap    bool   `json:"output_gap"`
	Online       bool   `json:"online"`
	Error        string `json:"error,omitempty"`
}
type JobResponse struct {
	Accepted    bool             `json:"accepted,omitempty"`
	Action      string           `json:"action,omitempty"`
	Job         *JobInfo         `json:"job,omitempty"`
	Jobs        []JobInfo        `json:"jobs,omitempty"`
	Output      *proto.JobOutput `json:"output,omitempty"`
	Next        string           `json:"next,omitempty"`
	WaitExpired bool             `json:"wait_expired,omitempty"`
}
type jobControlWait struct {
	node, epoch, jobID string
	ch                 chan proto.Msg
}
type jobCompletionWait struct {
	source, id string
	done       chan struct{}
}

func (a *App) jobInfo(j eventlog.JobRecord) JobInfo {
	q := j.Receipt
	n := a.nodes[j.SourceID]
	return JobInfo{Revision: q.Revision, Node: j.SourceID, JobID: q.JobID, Agent: q.OwnerAgent, Creator: j.Creator, SID: q.OriginSID,
		RunID: q.OriginRunID, TurnID: q.Origin.TurnID, CallID: q.OriginCallID, Status: q.Status, Execution: q.Execution,
		Background: q.Background, Silent: j.Silent, CreatedAt: q.CreatedAt, StartedAt: q.StartedAt, EndedAt: q.EndedAt,
		DeadlineAt: q.DeadlineAt, Sandbox: q.SandboxEffective, OutputCursor: q.OutputCursor, OutputGap: q.OutputGap,
		Online: n != nil, Error: q.Error}
}

// Permission is checked against the frozen source, never the currently bound
// main/creator conversation. A silent branch cannot operate a foreground Job.
func (a *App) canAccessJob(by, caller string, j eventlog.JobRecord) bool {
	if by == "" {
		return false
	}
	q := j.Receipt
	if caller != "" {
		if a.execAgentID(caller) != q.OwnerAgent {
			return false
		}
		if a.isBranchSID(caller) {
			return q.OriginSID == caller
		}
		if a.isGroupSession(caller) {
			if q.OriginSID == caller {
				return true
			}
			b, ok := a.store.Branch(q.OriginSID)
			return ok && b.RootSID == caller
		}
	}
	if id, life := strings.CutPrefix(by, "life:"); life {
		return id == q.OwnerAgent && (j.Creator == by || a.canAccessForkContext(by, caller, q.OriginSID))
	}
	if a.isAdmin(by) {
		return true
	}
	return j.Creator == by && a.canAccessForkContext(by, caller, q.OriginSID)
}
func (a *App) jobRecord(req JobRequest, by, caller string) (eventlog.JobRecord, error) {
	if a.jobEvents == nil {
		return eventlog.JobRecord{}, errors.New("可靠任务索引不可用")
	}
	var j eventlog.JobRecord
	var ok bool
	if req.Node != "" {
		j, ok = a.jobEvents.Job(req.Node, req.JobID)
	} else {
		j, ok = a.jobEvents.FindJob(req.JobID)
	}
	if !ok || !a.canAccessJob(by, caller, j) {
		return eventlog.JobRecord{}, errors.New("没有这个任务的访问权限，或任务编号不存在/不唯一")
	}
	return j, nil
}
func (a *App) jobList(req JobRequest, by, caller string) (JobResponse, error) {
	if a.jobEvents == nil {
		return JobResponse{}, errors.New("可靠任务索引不可用")
	}
	limit := req.Limit
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 || len(req.After) > 512 {
		return JobResponse{}, errors.New("任务列表每页 1–100 条，游标最多 512 字节")
	}
	out := JobResponse{Jobs: []JobInfo{}}
	cursor := req.After
	for {
		rows, next := a.jobEvents.Jobs(cursor, 100)
		for _, row := range rows {
			if (req.Node != "" && req.Node != row.SourceID) || !a.canAccessJob(by, caller, row) {
				continue
			}
			if len(out.Jobs) == limit {
				return out, nil
			}
			out.Jobs = append(out.Jobs, a.jobInfo(row))
			out.Next = jobPersistKey(row.SourceID, row.Receipt.JobID)
		}
		if next == "" {
			out.Next = ""
			break
		}
		cursor = next
	}
	return out, nil
}
func (a *App) jobControlReply(node string, m *proto.Msg) bool {
	if m.T != proto.TJobState || m.RequestID == "" {
		return false
	}
	w, ok := a.jobControls[m.RequestID]
	if !ok || w.node != node || w.epoch != m.NodeEpoch || w.jobID != m.JobID && !(m.JobID == "" && m.OK != nil && !*m.OK) {
		return true
	}
	if len(m.JobData) > 96<<10 {
		return true
	}
	copy := *m
	copy.JobData = append(json.RawMessage(nil), m.JobData...)
	select {
	case w.ch <- copy:
	default:
	}
	return true
}
func (a *App) signalJobCompletion(source, id string) {
	for _, wait := range a.jobCompletions {
		if wait.source == source && wait.id == id {
			select {
			case <-wait.done:
			default:
				close(wait.done)
			}
		}
	}
}

// JobAction is the shared control path for tools, slash commands and the web.
// It performs no disk or network wait while holding App.mu.
func (a *App) JobAction(ctx context.Context, by, caller string, req JobRequest) (JobResponse, error) {
	if by == "" {
		return JobResponse{}, errors.New("任务操作缺少访问者身份")
	}
	if req.Action == "" {
		req.Action = "status"
	}
	if len(req.JobID) > 96 || len(req.Node) > 128 || req.MaxBytes < 0 || req.MaxBytes > 65536 || req.WaitMS < 0 || req.WaitMS > 10000 || req.Cursor > 9007199254740991 {
		return JobResponse{}, errors.New("任务参数超出上限（日志最多 64 KiB，短等待最多 10 秒）")
	}
	if err := ctx.Err(); err != nil {
		return JobResponse{}, err
	}
	if err := a.ctx.Err(); err != nil {
		return JobResponse{}, err
	}
	a.mu.Lock()
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return JobResponse{}, err
	}
	if req.Action == "list" {
		out, err := a.jobList(req, by, caller)
		a.mu.Unlock()
		return out, err
	}
	j, err := a.jobRecord(req, by, caller)
	if err != nil {
		a.mu.Unlock()
		return JobResponse{}, err
	}
	info := a.jobInfo(j)
	if req.Action == "status" {
		a.mu.Unlock()
		return JobResponse{Job: &info}, nil
	}
	if req.Action == "wait" {
		if finalJob(j.Receipt.Status) {
			a.mu.Unlock()
			return JobResponse{Job: &info}, nil
		}
		if len(a.jobCompletions) >= 128 {
			a.mu.Unlock()
			return JobResponse{}, errors.New("短等待名额已满")
		}
		ms := req.WaitMS
		if ms == 0 {
			ms = 10000
		}
		a.hostSeq++
		id := fmt.Sprintf("jw-%s-%d", a.dispatchBoot, a.hostSeq)
		wait := jobCompletionWait{source: j.SourceID, id: j.Receipt.JobID, done: make(chan struct{})}
		a.jobCompletions[id] = wait
		a.mu.Unlock()
		timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
		expired := false
		select {
		case <-wait.done:
		case <-timer.C:
			expired = true
		case <-ctx.Done():
			err = ctx.Err()
		case <-a.ctx.Done():
			err = a.ctx.Err()
		}
		timer.Stop()
		a.mu.Lock()
		delete(a.jobCompletions, id)
		latest, readErr := a.jobRecord(req, by, caller)
		if readErr == nil {
			info = a.jobInfo(latest)
		}
		a.mu.Unlock()
		if err != nil {
			return JobResponse{}, err
		}
		if readErr != nil {
			return JobResponse{}, readErr
		}
		return JobResponse{Job: &info, WaitExpired: expired && !finalJob(info.Status)}, nil
	}
	if req.Action != "output" && req.Action != "cancel" && req.Action != "detach" {
		a.mu.Unlock()
		return JobResponse{}, errors.New("任务操作只支持 list/status/output/wait/cancel/detach")
	}
	readonly := false
	if caller != "" {
		readonly = a.workspaceOf(caller).mode == proto.ModeReadonly
		if source := a.sessions[caller]; source != nil && source.run != nil {
			readonly = source.run.mode == proto.ModeReadonly || source.run.mode == proto.ModeNone
		}
	}
	if req.Action == "detach" && (!a.agentExists(j.Receipt.OwnerAgent) || !a.isAdmin(by) || readonly) {
		a.mu.Unlock()
		return JobResponse{}, errors.New("当前权限不能把任务转后台")
	}
	n := a.nodes[j.SourceID]
	if n == nil || n.info.Features["job_manager"] < 1 {
		a.mu.Unlock()
		return JobResponse{}, errors.New("执行节点离线或不支持 Job 控制；已有状态与未知占用仍保留")
	}
	if len(a.jobControls) >= 128 {
		a.mu.Unlock()
		return JobResponse{}, errors.New("任务控制请求过多，稍后重试")
	}
	a.hostSeq++
	id := fmt.Sprintf("jc-%s-%d", a.dispatchBoot, a.hostSeq)
	ch := make(chan proto.Msg, 1)
	a.jobControls[id] = jobControlWait{node: j.SourceID, epoch: n.info.NodeEpoch, jobID: j.Receipt.JobID, ch: ch}
	bytes := req.MaxBytes
	if bytes == 0 {
		bytes = 8192
	}
	message := proto.Msg{T: proto.TJobControl, NodeEpoch: n.info.NodeEpoch, RequestID: id, Action: req.Action, JobID: j.Receipt.JobID, Cursor: req.Cursor, MaxBytes: bytes}
	sent := a.hub.Send(j.SourceID, message)
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.jobControls, id); a.mu.Unlock() }()
	if !sent {
		return JobResponse{}, errors.New("任务控制没有发到执行节点")
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	var reply proto.Msg
	select {
	case reply = <-ch:
	case <-timer.C:
		return JobResponse{}, errors.New("任务控制回执超时；取消或移交可能仍在保存，请查询同一任务")
	case <-ctx.Done():
		return JobResponse{}, ctx.Err()
	case <-a.ctx.Done():
		return JobResponse{}, a.ctx.Err()
	}
	if ctx.Err() != nil {
		return JobResponse{}, ctx.Err()
	}
	if reply.OK == nil || !*reply.OK {
		if reply.Detail == "" {
			reply.Detail = "执行节点拒绝了任务操作"
		}
		return JobResponse{}, errors.New(reply.Detail)
	}
	a.mu.Lock()
	// Recheck current access before exposing logs after a potentially slow RPC.
	latest, readErr := a.jobRecord(req, by, caller)
	err = readErr
	if err == nil {
		info = a.jobInfo(latest)
	}
	a.mu.Unlock()
	if err != nil {
		return JobResponse{}, err
	}
	out := JobResponse{Job: &info, Accepted: true, Action: req.Action}
	if req.Action == "output" {
		var output proto.JobOutput
		if err := json.Unmarshal(reply.JobData, &output); err != nil {
			return JobResponse{}, fmt.Errorf("任务日志回执无效: %w", err)
		}
		out.Output = &output
	}
	return out, nil
}
