package app

import (
	"nano-agent/gateway/internal/channel"
	"sort"
	"time"
)

// Message queues are bounded independently of machine tool queues. The normal
// interjection inbox has its own durable count/byte limits.
func (a *App) queueFits(s *sess, m channel.Message) bool {
	if len(s.queue) >= 32 || len(m.Text) > 1<<20 {
		return false
	}
	count, bytes := 1, len(m.Text)
	for _, other := range a.sessions {
		for _, q := range other.queue {
			count++
			bytes += len(q.m.Text)
		}
	}
	return count <= 256 && bytes <= 8<<20
}
func (a *App) runPriority(s *sess, now time.Time) int {
	p := 0
	if len(s.queue) > 0 && (s.queue[0].inner || isLifeSID(s.sid) || a.isBranchSID(s.sid)) {
		p = 10
	}
	age := int(now.Sub(s.runQueuedAt) / time.Second)
	return max(0, p-age)
}
func (a *App) runCapacity(s *sess, node string) bool {
	if node == "" {
		return true
	} // existing unavailable-node path gives its concrete error
	limit := a.runtimeLimits.MaxActiveRunsPerNode
	if peer := a.nodes[node]; peer != nil && peer.info.Features["max_active_runs"] > 0 {
		limit = min(limit, peer.info.Features["max_active_runs"])
	}
	running := 0
	now := time.Now()
	for _, other := range a.sessions {
		if other.run != nil && other.run.node == node {
			running++
		}
		if other == s || other.run != nil || other.compacting || len(other.queue) == 0 ||
			other.runQueueNode != node || other.runQueueSerial == 0 {
			continue
		}
		p, op := a.runPriority(s, now), a.runPriority(other, now)
		if op < p || (op == p && other.runQueueSerial < s.runQueueSerial) {
			return false
		}
	}
	return running < limit
}
func (a *App) pumpRuns() {
	waiting := make([]*sess, 0)
	for _, s := range a.sessions {
		if s.run == nil && !s.compacting && len(s.queue) > 0 {
			waiting = append(waiting, s)
		}
	}
	now := time.Now()
	sort.Slice(waiting, func(i, j int) bool {
		l, r := waiting[i], waiting[j]
		p, q := a.runPriority(l, now), a.runPriority(r, now)
		if p != q {
			return p < q
		}
		if l.runQueueSerial != r.runQueueSerial {
			return l.runQueueSerial < r.runQueueSerial
		}
		return l.sid < r.sid
	})
	for _, s := range waiting {
		if a.branchRunBlocked(s) {
			continue
		}
		a.startRun(s)
	}
}

// branchRunBlocked 是"排队等着的自主分支任务现在不该开始"：agent 暂停了或今天的预算用完了。
// 队列留在原地，恢复之后 pumpRuns 会再放行（评审 §24 R2）。调用方持有锁。
func (a *App) branchRunBlocked(s *sess) bool {
	if len(s.queue) == 0 || !a.isBranchSID(s.sid) || !autonomousForker(s.queue[0].who) {
		return false
	}
	m, ok := a.store.Branch(s.sid)
	if !ok {
		return false
	}
	if err := a.autoActivityRoom(m.OwnerAgent); err != nil {
		a.log.Info("自主分支排队但先不开", "branch", s.sid, "err", err)
		return true
	}
	return false
}
