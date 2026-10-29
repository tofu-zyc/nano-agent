package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/dispatch"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/proto"
)

func (a *App) LeaseProtocol() int       { return 1 }
func (a *App) LeaseDetachProtocol() int { return 1 }

func resourceKeys(csv string) []string {
	if csv == "" {
		return nil
	}
	return strings.Split(csv, ",")
}

func (a *App) leaseReply(node string, n *node, m *proto.Msg, kind, detail string) {
	a.hub.Send(node, proto.Msg{T: kind, LeaseID: m.LeaseID, NodeEpoch: n.info.NodeEpoch, Detail: detail, OK: proto.Bool(kind == proto.TLeaseSynced)})
}

func (a *App) leaseRequest(nodeName string, n *node, m *proto.Msg, observed bool) (dispatch.Request, error) {
	spec := m.Lease
	if spec == nil || spec.LeaseID != m.LeaseID || len(m.LeaseID) > 128 || !strings.HasPrefix(m.LeaseID, "l"+n.info.NodeEpoch+"-") {
		return dispatch.Request{}, errors.New("执行租约编号或节点启动身份无效")
	}
	owner := nodeName + "/" + n.info.NodeEpoch
	origin := *spec
	if !observed {
		if spec.HostID != "" {
			wait, ok := a.hostWaits[spec.HostID]
			if !ok || wait.node != nodeName || wait.tool != spec.Name {
				return dispatch.Request{}, errors.New("代执行请求已经失效或不属于此节点")
			}
			if wait.origin != nil {
				origin = *wait.origin
			} else {
				origin.SID = "host:" + nodeName
				origin.RunID = ""
				origin.CallID = spec.HostID
				origin.Priority = 0
			}
		} else {
			s := a.sessions[m.SID]
			if s == nil || s.run == nil || s.run.node != nodeName || s.run.runID != m.RunID || s.run.turnID != m.TurnID {
				return dispatch.Request{}, errors.New("工具调用所属 run 或模型轮次已经失效")
			}
			call, ok := s.run.callArgs[m.ID]
			if !ok || call.Name != spec.Name {
				return dispatch.Request{}, errors.New("工具调用身份不匹配")
			}
			origin.SID, origin.RunID, origin.TurnID, origin.CallID = s.sid, s.run.runID, s.run.turnID, m.ID
			origin.Agent = a.execAgentID(s.sid)
			origin.Priority = 0
			if s.run.inner || a.isBranchSID(s.sid) {
				origin.Priority = 10
			}
		}
	}
	if origin.SID == "" {
		origin.SID = "host:" + nodeName
	}
	req := dispatch.Request{ID: m.LeaseID, Name: spec.Name, Owner: owner, Machine: a.machineOf(nodeName), Session: origin.SID, RunID: origin.RunID, TurnID: origin.TurnID, CallID: origin.CallID, HostID: spec.HostID, Agent: origin.Agent, Priority: origin.Priority, Shell: spec.Name == "Shell", Background: spec.Background, Windows: n.info.OS == "windows", Shared: resourceKeys(spec.Resources), Exclusive: resourceKeys(spec.Exclusive), At: time.Now(), OwnerSlots: min(64, n.info.Features["max_execs"]), OwnerShells: min(64, n.info.Features["max_parallel_shells"])}
	if req.Shell && len(req.Exclusive) == 0 {
		return req, errors.New("Shell 没有声明受保护的资源域，未执行")
	}
	return req, nil
}

// Called with App.mu. All disk work is submitted to the single bounded worker.
func (a *App) leaseMessage(nodeName string, m *proto.Msg) bool {
	switch m.T {
	case proto.TLeaseAcquire, proto.TLeaseStarted, proto.TLeaseRelease, proto.TLeaseCancel, proto.TLeaseSync,
		proto.TLeaseDetachPrepare, proto.TLeaseDetachCommit, proto.TLeaseDetachAbort:
	default:
		return false
	}
	n := a.nodes[nodeName]
	if n == nil || n.info.NodeEpoch == "" || m.NodeEpoch != n.info.NodeEpoch {
		return true
	}
	detachPhase := ""
	if strings.HasPrefix(m.T, "lease_detach_") {
		detachPhase = strings.TrimPrefix(m.T, "lease_detach_")
	}
	if detachPhase != "" && n.info.Features["lease_detach"] < 1 {
		a.hub.Send(nodeName, proto.Msg{T: proto.TLeaseDetachResult, LeaseID: m.LeaseID, RequestID: m.RequestID, NodeEpoch: n.info.NodeEpoch,
			Phase: detachPhase, OK: proto.Bool(false), Detail: "节点没有协商后台租约移交能力"})
		return true
	}
	if a.dispatcher == nil {
		kind := proto.TLeaseDeny
		if m.T == proto.TLeaseSync {
			kind = proto.TLeaseSynced
		}
		detail := "执行租约存储不可用"
		if a.dispatchErr != nil {
			detail += "：" + a.dispatchErr.Error()
		}
		if detachPhase != "" {
			a.hub.Send(nodeName, proto.Msg{T: proto.TLeaseDetachResult, LeaseID: m.LeaseID, RequestID: m.RequestID, NodeEpoch: n.info.NodeEpoch,
				Phase: detachPhase, OK: proto.Bool(false), Detail: detail})
		} else {
			a.hub.Send(nodeName, proto.Msg{T: kind, LeaseID: m.LeaseID, NodeEpoch: n.info.NodeEpoch, OK: proto.Bool(false), Detail: detail})
		}
		return true
	}
	command := dispatch.Command{ID: m.LeaseID, Owner: nodeName + "/" + n.info.NodeEpoch, Node: nodeName, RequestID: m.RequestID}
	switch m.T {
	case proto.TLeaseAcquire:
		req, err := a.leaseRequest(nodeName, n, m, false)
		if err != nil {
			a.leaseReply(nodeName, n, m, proto.TLeaseDeny, err.Error())
			return true
		}
		command.Action, command.Request = "admit", req
	case proto.TLeaseDetachPrepare:
		command.Action = "detach_prepare"
	case proto.TLeaseDetachCommit:
		command.Action = "detach_commit"
	case proto.TLeaseDetachAbort:
		command.Action = "detach_abort"
	case proto.TLeaseStarted:
		command.Action = "started"
	case proto.TLeaseRelease:
		command.Action = "release"
	case proto.TLeaseCancel:
		command.Action = "cancel"
	case proto.TLeaseSync:
		if m.JobStorageError != "" {
			a.hub.Send(nodeName, proto.Msg{T: proto.TLeaseSynced, NodeEpoch: n.info.NodeEpoch, OK: proto.Bool(false), Detail: "节点 Job 恢复失败，保留已有执行占用：" + m.JobStorageError})
			return true
		}
		command.Action = "sync"
		if len(m.Leases) > 256 {
			a.hub.Send(nodeName, proto.Msg{T: proto.TLeaseSynced, NodeEpoch: n.info.NodeEpoch, OK: proto.Bool(false), Detail: "节点执行清单超过上限"})
			return true
		}
		for _, spec := range m.Leases {
			item := mCopyLease(m, spec)
			req, err := a.leaseRequest(nodeName, n, &item, true)
			if err != nil {
				a.hub.Send(nodeName, proto.Msg{T: proto.TLeaseSynced, NodeEpoch: n.info.NodeEpoch, OK: proto.Bool(false), Detail: err.Error()})
				return true
			}
			command.Inventory = append(command.Inventory, dispatch.Lease{Request: req, State: spec.State})
		}
	}
	if !a.dispatcher.Submit(command) {
		if strings.HasPrefix(command.Action, "detach_") {
			a.hub.Send(nodeName, proto.Msg{T: proto.TLeaseDetachResult, LeaseID: m.LeaseID, RequestID: m.RequestID, NodeEpoch: n.info.NodeEpoch,
				Phase: strings.TrimPrefix(command.Action, "detach_"), OK: proto.Bool(false), Detail: "执行租约队列繁忙，后台移交尚未提交"})
			return true
		}
		if m.T == proto.TLeaseAcquire {
			a.leaseReply(nodeName, n, m, proto.TLeaseDeny, "执行租约持久队列已满，本次没有受理或执行")
		} else {
			n.info.LeasesReady = false
			a.hub.Send(nodeName, proto.Msg{T: proto.TLeaseSynced, NodeEpoch: n.info.NodeEpoch, OK: proto.Bool(false), Detail: "执行租约队列繁忙，需要重新核对"})
		}
	}
	return true
}

func mCopyLease(m *proto.Msg, spec proto.LeaseSpec) proto.Msg {
	out := *m
	out.Lease = &spec
	out.LeaseID = spec.LeaseID
	return out
}

func (a *App) leaseChanged(change dispatch.Change) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if waiter := a.dispatchWait[change.ID]; waiter != nil {
		waiter(change)
	}
	nodeName, epoch, hasNode := strings.Cut(change.Owner, "/")
	n := a.nodes[nodeName]
	current := hasNode && n != nil && n.info.NodeEpoch == epoch
	if current && strings.HasPrefix(change.Action, "detach_") {
		detail := ""
		if change.Err != nil {
			detail = change.Err.Error()
		}
		a.hub.Send(nodeName, proto.Msg{T: proto.TLeaseDetachResult, LeaseID: change.ID, RequestID: change.RequestID, NodeEpoch: epoch,
			Phase: strings.TrimPrefix(change.Action, "detach_"), OK: proto.Bool(change.Err == nil), Detail: detail,
			Lease: &proto.LeaseSpec{LeaseID: change.ID, Background: change.Lease.Background}})
	}
	if change.Err != nil {
		a.log.Error("执行租约操作未完成", "action", change.Action, "lease", change.ID, "err", change.Err)
		if current && (change.Action == "admit" || change.Action == "sync") {
			kind := proto.TLeaseDeny
			if change.Action == "sync" {
				kind = proto.TLeaseSynced
			}
			a.hub.Send(nodeName, proto.Msg{T: kind, LeaseID: change.ID, NodeEpoch: epoch, OK: proto.Bool(false), Detail: change.Err.Error()})
		}
		a.publish(events.Event{T: "execution_leases", Text: change.Err.Error()})
		return
	}
	if change.Action == "sync" && current {
		n.info.LeasesReady = true
		a.hub.Send(nodeName, proto.Msg{T: proto.TLeaseSynced, NodeEpoch: epoch, OK: proto.Bool(true)})
		a.publish(events.Event{T: "kernel", Name: nodeName, Data: n.info})
		a.pumpRuns()
	}
	if change.Action == "admit" && current && change.Lease.State == "queued" {
		detail := "等待" + change.Lease.WaitFor
		if change.Lease.Holder != "" {
			detail += "；占用者 " + change.Lease.Holder
		}
		a.hub.Send(nodeName, proto.Msg{T: proto.TLeaseQueued, LeaseID: change.ID, NodeEpoch: epoch, Detail: detail})
	}
	if change.Action == "admit" && change.Lease.State == "granted" && len(change.Grants) == 0 {
		change.Grants = append(change.Grants, change.Lease)
	}
	for _, grant := range change.Grants {
		target, boot, _ := strings.Cut(grant.Owner, "/")
		if peer := a.nodes[target]; peer != nil && peer.info.NodeEpoch == boot {
			a.hub.Send(target, proto.Msg{T: proto.TLeaseGrant, LeaseID: grant.ID, NodeEpoch: boot})
		}
		if waiter := a.dispatchWait[grant.ID]; waiter != nil && grant.ID != change.ID {
			waiter(dispatch.Change{Action: "grant", ID: grant.ID, Owner: grant.Owner, Lease: grant})
		}
	}
	if change.Action == "started" {
		l := change.Lease
		if wait, ok := a.hostWaits[l.HostID]; ok && wait.node == nodeName && wait.started != nil {
			go wait.started()
		}
		if s := a.sessions[l.Session]; s != nil && s.run != nil && s.run.runID == l.RunID && s.run.turnID == l.TurnID && s.run.node != nodeName {
			a.execStarted(s.run.node, &proto.Msg{SID: l.Session, RunID: l.RunID, TurnID: l.TurnID, ID: l.CallID})
		}
	}
	for _, expired := range change.Expired {
		target, boot, _ := strings.Cut(expired.Owner, "/")
		if peer := a.nodes[target]; peer != nil && peer.info.NodeEpoch == boot {
			a.hub.Send(target, proto.Msg{T: proto.TLeaseDeny, LeaseID: expired.ID, NodeEpoch: boot, Detail: "等待执行槽或资源超过 5 分钟，任务未启动"})
		}
		if waiter := a.dispatchWait[expired.ID]; waiter != nil {
			waiter(dispatch.Change{Action: "expired", ID: expired.ID, Owner: expired.Owner, Err: fmt.Errorf("排队超时，未启动")})
		}
	}
	a.publish(events.Event{T: "execution_leases"})
}
