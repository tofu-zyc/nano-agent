package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"nano-agent/gateway/internal/dispatch"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/proto"
)

type jobPersistence struct {
	job   eventlog.Job
	count int
	lease *proto.Msg
	epoch string
}

func jobPersistKey(node, id string) string {
	raw, _ := json.Marshal([]string{node, id})
	return string(raw)
}
func finalJob(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "timed_out", "interrupted", "unknown":
		return true
	}
	return false
}
func jobCompletedEvent(j eventlog.Job, observed int64) *eventlog.Event {
	q := j.Receipt
	if !finalJob(q.Status) {
		return nil
	}
	name := "job.failed"
	switch q.Status {
	case "succeeded":
		name = "job.completed"
	case "cancelled":
		name = "job.cancelled"
	case "timed_out":
		name = "job.timed_out"
	}
	data, _ := json.Marshal(map[string]string{"status": q.Status})
	e := &eventlog.Event{Name: name, SourceID: j.SourceID + "/" + q.JobID, SourceEpoch: q.NodeEpoch, Seq: q.LastEventSeq,
		OccurredAt: q.EndedAt, ObservedAt: observed, AgentID: q.OwnerAgent, RunID: q.OriginRunID,
		JobID: q.JobID, Silent: j.Silent, DataRef: q.JobID, Data: data}
	e.ID = eventlog.EventID(e.SourceID, e.SourceEpoch, e.Seq)
	return e
}

// Caller holds App.mu. These checks freeze Gateway authority; reporting a Job
// or claiming a creator in a node packet never grants access to a conversation.
func (a *App) authorizeNewJob(nodeName string, n *node, q proto.JobSnapshot) (eventlog.Job, error) {
	if q.Lease == nil || q.Lease.JobID != q.JobID || q.Lease.Name != "Shell" || q.NodeEpoch != n.info.NodeEpoch {
		return eventlog.Job{}, errors.New("新 Job 缺少本次启动的完整 Shell 租约身份")
	}
	message := &proto.Msg{SID: q.OriginSID, RunID: q.OriginRunID, TurnID: q.Origin.TurnID, ID: q.OriginCallID, LeaseID: q.Lease.LeaseID, Lease: q.Lease}
	req, err := a.leaseRequest(nodeName, n, message, false)
	if err != nil {
		return eventlog.Job{}, err
	}
	if req.Agent != q.OwnerAgent || req.Creator != q.CreatedBy || req.Silent != q.Origin.Silent ||
		req.Session != q.OriginSID || req.RunID != q.OriginRunID || req.CallID != q.OriginCallID ||
		req.TurnID != q.Origin.TurnID || q.Origin.Agent != req.Agent || q.Origin.CreatedBy != req.Creator ||
		q.Lease.SID != req.Session || q.Lease.RunID != req.RunID || q.Lease.TurnID != req.TurnID ||
		q.Lease.CallID != req.CallID || q.Lease.Agent != req.Agent || q.Lease.CreatedBy != req.Creator ||
		q.Lease.Silent != req.Silent {
		return eventlog.Job{}, errors.New("Job 的固定身份或 silent 与网关受理的真实调用不符")
	}
	return eventlog.Job{SourceID: nodeName, Creator: req.Creator, Silent: req.Silent, Receipt: q}, nil
}
func (a *App) jobEventIssue(node, id string, err error) {
	a.store.RecordStorageIssue("jobs:"+node, err)
	a.log.Warn("Job 状态未确认，来源保留回执", "node", node, "job", id, "err", err)
	a.publish(events.Event{T: "job_error", Name: node, ID: id, Text: err.Error()})
}

// Persistence callbacks run outside journal locks. Slow I/O never holds App.mu;
// only the durable result and any now-unblocked lease are committed here.
func (a *App) jobEventCommitted(nodeName, epoch, key string, j eventlog.Job, terminal bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pending := a.jobPersist[key]
	var lease *proto.Msg
	if pending != nil {
		pending.count--
		lease = pending.lease
		pending.lease = nil
		if pending.count == 0 {
			delete(a.jobPersist, key)
		}
	}
	n := a.nodes[nodeName]
	current := n != nil && n.info.NodeEpoch == epoch
	if err != nil {
		a.jobEventIssue(nodeName, j.Receipt.JobID, err)
		if lease != nil && current {
			a.leaseReply(nodeName, n, lease, proto.TLeaseDeny, "Job 未可靠登记，未启动命令："+err.Error())
		}
		return
	}
	// An ACK confirms journal durability, not Hook delivery or an external
	// side effect. The latter has a separate fixed delivery record.
	if finalJob(j.Receipt.Status) {
		a.signalJobCompletion(nodeName, j.Receipt.JobID)
	}
	if terminal && current {
		a.hub.Send(nodeName, proto.Msg{T: proto.TEventsAck, NodeEpoch: epoch, JobID: j.Receipt.JobID, EventSeq: j.Receipt.LastEventSeq})
	}
	a.observeJobPresentation(a.jobInfo(eventlog.JobRecord{Job: j}))
	a.publish(events.Event{T: "job", Name: nodeName, ID: j.Receipt.JobID, Data: a.jobInfo(eventlog.JobRecord{Job: j})})
	if lease != nil && current {
		a.leaseMessage(nodeName, lease)
	}
}
func (a *App) persistJob(nodeName string, n *node, q proto.JobSnapshot) {
	if a.jobEvents == nil {
		a.jobEventIssue(nodeName, q.JobID, errors.New("可靠事件日志不可用"))
		return
	}
	key := jobPersistKey(nodeName, q.JobID)
	pending := a.jobPersist[key]
	saved, known := a.jobEvents.Job(nodeName, q.JobID)
	var j eventlog.Job
	var err error
	admit := false
	switch {
	case known:
		j = saved.Job
		j.Receipt = q
	case pending != nil:
		j = pending.job
		j.Receipt = q
	default:
		if len(a.jobPersist) >= 128 {
			a.jobEventIssue(nodeName, q.JobID, eventlog.ErrBusy)
			return
		}
		j, err = a.authorizeNewJob(nodeName, n, q)
		admit = true
	}
	if err != nil {
		a.jobEventIssue(nodeName, q.JobID, err)
		return
	}
	if pending != nil && pending.count >= 8 {
		a.jobEventIssue(nodeName, q.JobID, eventlog.ErrBusy)
		return
	}
	event := jobCompletedEvent(j, a.now().Unix())
	epoch := n.info.NodeEpoch
	err = a.jobEvents.Submit(eventlog.Transaction{Admit: admit, Job: &j, Event: event}, func(err error) {
		a.jobEventCommitted(nodeName, epoch, key, j, event != nil && j.Receipt.NotificationState != "delivered", err)
	})
	if err != nil {
		a.jobEventIssue(nodeName, q.JobID, err)
		return
	}
	if pending == nil {
		pending = &jobPersistence{job: j, epoch: epoch}
		a.jobPersist[key] = pending
	}
	pending.count++
}

// The packet is already bounded by the wire parser. Each Job has its own
// durable transaction/ACK; a saturated inbox leaves unconfirmed node outbox
// entries for reconnect/retry instead of partially acknowledging an inventory.
func (a *App) jobEventMessage(nodeName string, m *proto.Msg) bool {
	if m.T != proto.TJobState && m.T != proto.TJobsSync {
		return false
	}
	n := a.nodes[nodeName]
	if n == nil || m.NodeEpoch != n.info.NodeEpoch {
		return true
	}
	if m.RequestID != "" {
		return true
	} // control responses have their own caller
	if m.T == proto.TJobsSync {
		if len(m.Jobs) > 256 {
			a.jobEventIssue(nodeName, "", errors.New("Job 清单超过 256 条"))
			return true
		}
		for _, job := range m.Jobs {
			a.persistJob(nodeName, n, job)
		}
	} else if m.Job != nil {
		if m.Job.JobID != m.JobID || (m.EventSeq != 0 && m.EventSeq != m.Job.LastEventSeq) {
			a.jobEventIssue(nodeName, m.JobID, errors.New("Job 回执与事件编号不一致"))
			return true
		}
		a.persistJob(nodeName, n, *m.Job)
	}
	return true
}
func sameJobLease(record eventlog.JobRecord, req dispatch.Request) bool {
	q := record.Receipt
	return record.SourceID != "" && q.Lease != nil && q.Lease.LeaseID == req.ID && q.JobID == req.JobID &&
		q.Lease.Name == req.Name && q.Lease.HostID == req.HostID && q.Lease.Priority == req.Priority &&
		slices.Equal(resourceKeys(q.Lease.Resources), req.Shared) && slices.Equal(resourceKeys(q.Lease.Exclusive), req.Exclusive) &&
		q.OwnerAgent == req.Agent && q.OriginSID == req.Session && q.OriginRunID == req.RunID &&
		q.OriginCallID == req.CallID && q.Origin.TurnID == req.TurnID && record.Creator == req.Creator &&
		record.Silent == req.Silent && q.Background == req.Background && !finalJob(q.Status)
}

// Lease admission waits for the Job index/outbox reservation before delegating
// to the existing durable machine scheduler. This does not start a Go run loop.
func (a *App) jobLeaseReady(nodeName string, n *node, m *proto.Msg, req dispatch.Request) bool {
	if a.jobEvents == nil || n.info.Features["job_manager"] < 1 || n.info.Features["job_stream"] < 1 {
		a.leaseReply(nodeName, n, m, proto.TLeaseDeny, "节点或网关没有可靠 Job 能力，未启动命令")
		return false
	}
	if record, ok := a.jobEvents.Job(nodeName, req.JobID); ok {
		if sameJobLease(record, req) {
			return true
		}
		a.leaseReply(nodeName, n, m, proto.TLeaseDeny, "Job 固定身份不符或已经结束，未启动命令")
		return false
	}
	if p := a.jobPersist[jobPersistKey(nodeName, req.JobID)]; p != nil && p.epoch == n.info.NodeEpoch && p.lease == nil {
		copy := *m
		if m.Lease != nil {
			lease := *m.Lease
			copy.Lease = &lease
		}
		p.lease = &copy
		return false
	}
	a.leaseReply(nodeName, n, m, proto.TLeaseDeny, fmt.Sprintf("Job %s 尚未可靠准入，未启动命令", req.JobID))
	return false
}
