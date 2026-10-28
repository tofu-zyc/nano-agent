package dispatch

import (
	"errors"
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
		if saved.ID == "" || saved.Owner == "" || saved.Machine == "" || saved.Session == "" || next[saved.ID] != nil {
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

// SyncNode accepts a complete inventory from an authenticated node. A changed
// node boot retires the old execution owner; no old request is replayed.
func (s *Scheduler) SyncNode(node, owner string, inventory []Lease, now time.Time) ([]Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if node == "" || !strings.HasPrefix(owner, node+"/") || len(inventory) > 256 {
		return nil, errors.New("节点租约对账身份或数量无效")
	}
	next := map[string]Lease{}
	for _, item := range inventory {
		if item.ID == "" || item.Owner != owner || item.Machine == "" || item.Session == "" || next[item.ID].ID != "" {
			return nil, errors.New("节点租约清单身份不匹配")
		}
		switch item.State {
		case "queued", "granted", "running", "cancelling", "unknown":
		default:
			return nil, errors.New("节点租约清单状态无效")
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
		next[item.ID] = item
	}
	for _, old := range s.leases {
		if !strings.HasPrefix(old.Owner, node+"/") {
			continue
		}
		if _, found := next[old.ID]; !found {
			s.close(old)
		}
	}
	for id, item := range next {
		if old := s.leases[id]; old != nil {
			if old.Owner != owner {
				return nil, errors.New("节点试图认领另一执行者的租约")
			}
			// The gateway's durable identity and scopes remain authoritative.
			old.State = item.State
			old.WaitFor, old.Holder = "", ""
		} else {
			if _, closed := s.closed[id]; closed {
				return nil, errors.New("节点清单试图恢复已结算执行")
			}
			if len(s.leases) >= s.cfg.Total {
				return nil, errors.New("节点清单超出租约总上限")
			}
			s.serial++
			item.serial = s.serial
			s.leases[id] = &item
		}
		if _, ok := s.served[sessionKey(item.Request)]; !ok {
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
