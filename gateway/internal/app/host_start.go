package app

import "nano-agent/gateway/internal/host"

// Both lease_started and run_started may arrive first. Resolve a managed
// timeout from the same durable invocation before consuming its one start
// notification; an earlier lease callback must not install a legacy timer.
func (a *App) notifyHostStarted(node, id, jobID string, timeoutMS int64, assertTimeout bool) bool {
	wait, ok := a.hostWaits[id]
	if !ok || wait.node != node || wait.started == nil || wait.jobID != "" && jobID != wait.jobID {
		return false
	}
	info := host.StartInfo{}
	if jobID != "" {
		if a.jobEvents == nil {
			return false
		}
		saved, exists := a.jobEvents.Job(node, jobID)
		target := a.nodes[node]
		q := saved.Receipt
		if !exists || target == nil || q.NodeEpoch != target.info.NodeEpoch || q.Lease == nil || q.Lease.HostID != id ||
			wait.origin == nil || q.OriginSID != wait.origin.SID || q.OriginRunID != wait.origin.RunID ||
			q.OriginCallID != wait.origin.CallID || q.Origin.TurnID != wait.origin.TurnID ||
			q.OwnerAgent != wait.origin.Agent || q.CreatedBy != wait.origin.CreatedBy || q.Origin.Silent != wait.origin.Silent ||
			q.TimeoutSeconds < 0 || q.TimeoutSeconds > 31536000 {
			return false
		}
		duration := int64(q.TimeoutSeconds) * 1000
		if assertTimeout && duration != timeoutMS {
			return false
		}
		info = host.StartInfo{JobID: jobID, TimeoutMS: duration}
		if info.TimeoutMS == 0 {
			info.TimeoutMS = -1
		}
	}
	notify := wait.started
	wait.started = nil
	a.hostWaits[id] = wait
	go notify(info)
	return true
}
