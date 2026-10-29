package dispatch

import (
	"errors"
	"slices"
	"strings"
	"time"
)

type Closed struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
}
type Checkpoint struct {
	Version int      `json:"version"`
	Leases  []Lease  `json:"leases"`
	Closed  []Closed `json:"closed"`
}

func (s *Scheduler) Checkpoint() Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Checkpoint{Version: 1}
	for _, l := range s.leases {
		out.Leases = append(out.Leases, clone(l))
	}
	// The stable insertion order is needed for queue recovery and bounded
	// tombstone retention. Snapshot's sorting is reproduced without re-locking.
	sortLeases(out.Leases)
	for _, id := range s.closedOrder {
		out.Closed = append(out.Closed, Closed{id, s.closed[id]})
	}
	return out
}

func (s *Scheduler) Restore(checkpoint Checkpoint, recovered bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if checkpoint.Version != 1 || len(checkpoint.Leases) > s.cfg.Total || len(checkpoint.Closed) > 1024 {
		return errors.New("执行租约恢复记录版本或数量无效")
	}
	next := map[string]*Lease{}
	served := map[string]uint64{}
	for i, saved := range checkpoint.Leases {
		if saved.ID == "" || saved.Owner == "" || saved.Machine == "" || saved.Session == "" || len(saved.JobID) > 96 || len(saved.DetachRequest) > 128 || next[saved.ID] != nil {
			return errors.New("执行租约恢复记录身份损坏")
		}
		switch saved.State {
		case "queued", "granted", "running", "cancelling", "unknown":
		default:
			return errors.New("执行租约恢复状态未知")
		}
		shared, err := normalizeKeys(saved.Shared, saved.Windows)
		if err != nil {
			return err
		}
		exclusive, err := normalizeKeys(saved.Exclusive, saved.Windows)
		if err != nil {
			return err
		}
		saved.Shared, saved.Exclusive, saved.serial = shared, exclusive, uint64(i+1)
		if recovered {
			saved.State = "unknown"
			saved.WaitFor = "reconcile"
		}
		saved.Holder = ""
		next[saved.ID] = &saved
		served[sessionKey(saved.Request)] = 0
	}
	closed := map[string]string{}
	order := []string{}
	for _, c := range checkpoint.Closed {
		if c.ID == "" || c.Owner == "" || next[c.ID] != nil || closed[c.ID] != "" {
			return errors.New("执行租约结算身份损坏")
		}
		closed[c.ID] = c.Owner
		order = append(order, c.ID)
	}
	s.leases, s.served, s.closed, s.closedOrder = next, served, closed, order
	s.serial = uint64(len(next))
	s.clock = 0
	return nil
}

// Job leases survive missing inventory and boot changes until a matching durable
// receipt or recovered Job proves what happened. Legacy foreground inventory
// retains its original semantics.
func finalReceipt(l Lease) bool {
	if !l.TerminalReceipt || l.JobID == "" {
		return false
	}
	switch l.State {
	case "succeeded", "failed", "cancelled", "timed_out", "interrupted":
		return true
	}
	return false
}
func sameExecution(a, b Request) bool {
	return a.Creator == b.Creator && a.Silent == b.Silent && a.ID == b.ID && a.JobID == b.JobID && a.Name == b.Name && a.Machine == b.Machine &&
		a.Session == b.Session && a.RunID == b.RunID && a.TurnID == b.TurnID && a.CallID == b.CallID &&
		a.HostID == b.HostID && a.Agent == b.Agent && a.Priority == b.Priority && a.Shell == b.Shell &&
		a.Hook == b.Hook && a.Windows == b.Windows && slices.Equal(a.Shared, b.Shared) && slices.Equal(a.Exclusive, b.Exclusive)
}
func (s *Scheduler) SyncNode(node, owner string, inventory []Lease, now time.Time) ([]Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if node == "" || !strings.HasPrefix(owner, node+"/") || len(inventory) > 512 {
		return nil, errors.New("节点租约对账身份或数量无效")
	}
	next := map[string]Lease{}
	// Validate the complete plan before touching any published ownership.
	for _, item := range inventory {
		if item.ID == "" || item.Owner != owner || item.Machine == "" || item.Session == "" || next[item.ID].ID != "" {
			return nil, errors.New("节点租约清单身份不匹配")
		}
		if len(item.DetachRequest) > 128 || item.DetachRequest != "" && item.DetachRequest != item.JobID {
			return nil, errors.New("节点后台移交的固定身份无效")
		}
		if !finalReceipt(item) {
			if item.TerminalReceipt {
				return nil, errors.New("任务终态回执缺少完整身份或状态")
			}
			switch item.State {
			case "queued", "granted", "running", "cancelling", "unknown":
			default:
				return nil, errors.New("节点租约清单状态无效")
			}
		}
		shared, err := normalizeKeys(item.Shared, item.Windows)
		if err != nil {
			return nil, err
		}
		exclusive, err := normalizeKeys(item.Exclusive, item.Windows)
		if err != nil {
			return nil, err
		}
		item.Shared, item.Exclusive = shared, exclusive
		if old := s.leases[item.ID]; old != nil {
			if !strings.HasPrefix(old.Owner, node+"/") || !sameExecution(old.Request, item.Request) {
				return nil, errors.New("节点清单的调用身份或资源域与已保存租约不符")
			}
			if old.Owner != owner && (item.JobID == "" || (!finalReceipt(item) && item.State != "unknown")) {
				return nil, errors.New("跨节点启动只能恢复同一 Job 的未知状态或终态回执")
			}
			if item.JobID != "" && !finalReceipt(item) {
				if item.DetachRequest != "" && item.DetachRequest != old.DetachRequest {
					return nil, errors.New("节点正在保存的移交没有匹配容量预留")
				}
				if old.Background && !item.Background {
					return nil, errors.New("已移交后台所有权不能降回前台")
				}
				if !old.Background && item.Background && old.DetachRequest != item.JobID {
					return nil, errors.New("后台所有权没有匹配的持久容量预留")
				}
			}
		} else if closed, ok := s.closed[item.ID]; ok {
			if !finalReceipt(item) || !strings.HasPrefix(closed, node+"/") {
				return nil, errors.New("节点清单试图恢复已结算执行或另一执行者的回执")
			}
		}
		next[item.ID] = item
	}
	count := len(s.leases)
	for _, old := range s.leases {
		if !strings.HasPrefix(old.Owner, node+"/") {
			continue
		}
		item, found := next[old.ID]
		if found && finalReceipt(item) || !found && old.JobID == "" {
			count--
		}
	}
	for id, item := range next {
		if s.leases[id] == nil && !finalReceipt(item) {
			count++
		}
	}
	if count > s.cfg.Total {
		return nil, errors.New("节点清单超出租约总上限")
	}
	for _, old := range s.leases {
		if !strings.HasPrefix(old.Owner, node+"/") {
			continue
		}
		if _, found := next[old.ID]; !found {
			if old.JobID == "" {
				s.close(old)
			} else {
				old.State = "unknown"
				old.WaitFor = "reconcile"
				old.Holder = ""
			}
		}
	}
	for id, item := range next {
		old := s.leases[id]
		if finalReceipt(item) {
			if old != nil {
				old.Owner = owner
				s.close(old)
			} else if _, closed := s.closed[id]; closed {
				s.closed[id] = owner
			} else {
				s.close(&Lease{Request: item.Request})
			}
			continue
		}
		if old != nil {
			old.Owner = owner
			old.State = item.State
			old.WaitFor = ""
			old.Holder = ""
			if item.JobID != "" {
				old.Background = item.Background
				// Published foreground cancels an unused reservation; published
				// background completes the prepared handoff after reconnection.
				old.DetachRequest = item.DetachRequest
				if item.Background {
					old.DetachRequest = ""
				}
			}
		} else {
			s.serial++
			item.serial = s.serial
			s.leases[id] = &item
		}
		if _, found := s.served[sessionKey(item.Request)]; !found {
			s.served[sessionKey(item.Request)] = s.clock
		}
	}
	return s.promote(now), nil
}

func (s *Scheduler) Pump(now time.Time) []Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promote(now)
}
func (s *Scheduler) Expire(now time.Time) ([]Lease, []Lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []Lease
	for _, l := range s.leases {
		if l.State == "queued" && !l.At.Add(s.cfg.QueueWait).After(now) {
			expired = append(expired, clone(l))
			s.close(l)
		}
	}
	return expired, s.promote(now)
}
