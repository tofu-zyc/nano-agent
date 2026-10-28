// Package dispatch coordinates real machine slots and resource leases. It does
// not drive model turns. Callers persist admission before calling Admit, and
// publish the returned grants to the execution owners outside this package.
package dispatch

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Slots              int
	Shells             int
	Live               int
	PerRun             int
	QueuedPerRun       int
	BackgroundPerAgent int
	HookReserve        int
	Total              int
	AgeStep            time.Duration
	QueueWait          time.Duration
}

func Defaults() Config {
	return Config{Slots: 8, Shells: 2, Live: 16, PerRun: 4, QueuedPerRun: 32, BackgroundPerAgent: 4, HookReserve: 1, Total: 1024, AgeStep: time.Second, QueueWait: 5 * time.Minute}
}

type Request struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Owner       string    `json:"owner"` // authenticated execution owner (node + boot)
	Machine     string    `json:"machine"`
	Session     string    `json:"sid"`
	RunID       string    `json:"run_id"`
	TurnID      int       `json:"turn_id"`
	CallID      string    `json:"call_id"`
	HostID      string    `json:"host_id,omitempty"`
	Agent       string    `json:"agent"`
	Priority    int       `json:"priority"` // 0 foreground, 10 autonomous; host supplied
	Shell       bool      `json:"shell"`
	Hook        bool      `json:"hook"`
	Background  bool      `json:"background"`
	Windows     bool      `json:"windows"`
	Shared      []string  `json:"shared"`
	Exclusive   []string  `json:"exclusive"`
	At          time.Time `json:"at"`
	OwnerSlots  int       `json:"owner_slots,omitempty"`
	OwnerShells int       `json:"owner_shells,omitempty"`
}

type Lease struct {
	Request
	State     string    `json:"state"` // queued | granted | running | cancelling | unknown
	GrantedAt time.Time `json:"granted_at,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	WaitFor   string    `json:"wait_for,omitempty"`
	Holder    string    `json:"holder,omitempty"`
	serial    uint64
}

type Scheduler struct {
	mu          sync.Mutex
	cfg         Config
	leases      map[string]*Lease
	served      map[string]uint64
	clock       uint64
	serial      uint64
	closed      map[string]string
	closedOrder []string
}

func New(cfg Config) (*Scheduler, error) {
	if cfg.Slots < 1 || cfg.Slots > 64 || cfg.Shells < 1 || cfg.Live < cfg.Slots || cfg.Live > 256 || cfg.PerRun < 1 || cfg.PerRun > 32 || cfg.QueuedPerRun < 1 || cfg.BackgroundPerAgent < 1 || cfg.Total < cfg.Live || cfg.Total > 4096 || cfg.HookReserve < 0 || cfg.HookReserve >= cfg.Slots || cfg.AgeStep <= 0 || cfg.QueueWait <= 0 {
		return nil, errors.New("执行调度限额无效")
	}
	return &Scheduler{cfg: cfg, leases: map[string]*Lease{}, served: map[string]uint64{}, closed: map[string]string{}}, nil
}

func normalizeKeys(keys []string, windows bool) ([]string, error) {
	if len(keys) > 128 {
		return nil, errors.New("资源声明超过 128 个键")
	}
	out := make([]string, 0, len(keys))
	total := 0
	for _, key := range keys {
		total += len(key)
		if len(key) > 8192 || total > 64<<10 {
			return nil, errors.New("资源声明超过字节上限")
		}
		kind, value, ok := strings.Cut(key, ":")
		if !ok || value == "" {
			return nil, fmt.Errorf("资源键无效：%q", key)
		}
		if kind == "dir" || kind == "file" {
			if windows {
				value = strings.ToUpper(strings.ReplaceAll(value, "\\", "/"))
			}
			value = path.Clean(value)
		}
		out = append(out, kind+":"+value)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func overlaps(a, b string) bool {
	if a == b {
		return true
	}
	if !strings.HasPrefix(a, "dir:") || !strings.HasPrefix(b, "dir:") {
		return false
	}
	a, b = strings.TrimPrefix(a, "dir:"), strings.TrimPrefix(b, "dir:")
	return strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}
func intersects(a, b []string) bool {
	for _, left := range a {
		for _, right := range b {
			if overlaps(left, right) {
				return true
			}
		}
	}
	return false
}
func conflict(a, b *Lease) bool {
	return intersects(a.Exclusive, b.Exclusive) || intersects(a.Exclusive, b.Shared) || intersects(b.Exclusive, a.Shared)
}
func holds(l *Lease) bool         { return l.State != "queued" }
func sessionKey(r Request) string { return r.Machine + "\x00" + r.Session }
func sameRun(a, b Request) bool   { return a.RunID != "" && a.Session == b.Session && a.RunID == b.RunID }
func clone(l *Lease) Lease {
	out := *l
	out.Shared = slices.Clone(l.Shared)
	out.Exclusive = slices.Clone(l.Exclusive)
	return out
}

// Admit is idempotent by immutable owner and full call identity. An ID already
// released cannot be re-admitted through a duplicate transport frame.
func (s *Scheduler) Admit(req Request, now time.Time) (Lease, []Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.ID == "" || len(req.ID) > 256 || req.Owner == "" || req.Machine == "" || req.Session == "" || req.Priority < 0 || req.Priority > 10 {
		return Lease{}, nil, errors.New("执行租约身份或优先级无效")
	}
	if old := s.leases[req.ID]; old != nil {
		if old.Owner != req.Owner || old.Machine != req.Machine || old.Session != req.Session || old.RunID != req.RunID || old.TurnID != req.TurnID || old.CallID != req.CallID {
			return Lease{}, nil, errors.New("租约 ID 已属于另一调用")
		}
		return clone(old), nil, nil
	}
	if _, closed := s.closed[req.ID]; closed {
		return Lease{}, nil, errors.New("执行租约已经结算，不能重新受理")
	}
	shared, err := normalizeKeys(req.Shared, req.Windows)
	if err != nil {
		return Lease{}, nil, err
	}
	exclusive, err := normalizeKeys(req.Exclusive, req.Windows)
	if err != nil {
		return Lease{}, nil, err
	}
	req.Shared, req.Exclusive = shared, exclusive
	if len(s.leases) >= s.cfg.Total {
		return Lease{}, nil, errors.New("执行租约总数已达上限")
	}
	live, queued, bg := 0, 0, 0
	for _, l := range s.leases {
		if l.Machine == req.Machine {
			live++
		}
		if sameRun(l.Request, req) && l.State == "queued" {
			queued++
		}
		if l.Background && l.Agent == req.Agent {
			bg++
		}
	}
	if live >= s.cfg.Live {
		return Lease{}, nil, errors.New("目标机器的运行与排队任务已达上限")
	}
	if queued >= s.cfg.QueuedPerRun {
		return Lease{}, nil, errors.New("本轮排队调用已达上限")
	}
	if req.Background && (req.Agent == "" || bg >= s.cfg.BackgroundPerAgent) {
		return Lease{}, nil, errors.New("agent 后台任务已达上限")
	}
	if req.At.IsZero() {
		req.At = now
	}
	s.serial++
	l := &Lease{Request: req, State: "queued", serial: s.serial}
	s.leases[req.ID] = l
	if _, ok := s.served[sessionKey(req)]; !ok {
		s.served[sessionKey(req)] = s.clock
	}
	grants := s.promote(now)
	return clone(l), grants, nil
}

func (s *Scheduler) canGrant(l *Lease) (bool, string, string) {
	machine, shells, hooks, run, owner, ownerShells := 0, 0, 0, 0, 0, 0
	for _, other := range s.leases {
		if other == l || !holds(other) {
			continue
		}
		if sameRun(l.Request, other.Request) && !other.Background {
			run++
		}
		if l.Owner == other.Owner {
			owner++
			if other.Shell {
				ownerShells++
			}
		}
		if other.Machine != l.Machine {
			continue
		}
		machine++
		if other.Shell && !other.Hook {
			shells++
		}
		if other.Hook {
			hooks++
		}
		if conflict(l, other) {
			return false, "resource", other.ID
		}
	}
	limit := s.cfg.Slots
	if !l.Hook {
		limit -= s.cfg.HookReserve
	}
	switch {
	case l.OwnerSlots > 0 && owner >= l.OwnerSlots:
		return false, "node_slots", ""
	case l.Shell && l.OwnerShells > 0 && ownerShells >= l.OwnerShells:
		return false, "node_shell_slots", ""
	case machine >= limit:
		return false, "machine_slots", ""
	case l.Hook && hooks >= max(1, s.cfg.HookReserve):
		return false, "hook_slots", ""
	case l.Shell && !l.Hook && shells >= s.cfg.Shells:
		return false, "shell_slots", ""
	case !l.Background && run >= s.cfg.PerRun:
		return false, "run_slots", ""
	}
	return true, "", ""
}

// Promotion uses session rotation within an effective priority. Autonomous
// work ages into foreground priority, and new sessions join the current round.
func (s *Scheduler) promote(now time.Time) []Lease {
	var grants []Lease
	for {
		var best *Lease
		bestPriority := 11
		for _, l := range s.leases {
			if l.State != "queued" {
				continue
			}
			if !l.At.Add(s.cfg.QueueWait).After(now) {
				l.WaitFor = "queue_timeout"
				continue
			}
			ok, why, holder := s.canGrant(l)
			l.WaitFor, l.Holder = why, holder
			if !ok {
				continue
			}
			waited := max(time.Duration(0), now.Sub(l.At))
			priority := max(0, l.Priority-int(waited/s.cfg.AgeStep))
			if best == nil || priority < bestPriority || (priority == bestPriority && (s.served[sessionKey(l.Request)] < s.served[sessionKey(best.Request)] || (s.served[sessionKey(l.Request)] == s.served[sessionKey(best.Request)] && l.serial < best.serial))) {
				best, bestPriority = l, priority
			}
		}
		if best == nil {
			return grants
		}
		best.State, best.GrantedAt, best.WaitFor, best.Holder = "granted", now, "", ""
		s.clock++
		s.served[sessionKey(best.Request)] = s.clock
		grants = append(grants, clone(best))
	}
}

func (s *Scheduler) Started(id, owner string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[id]
	if l == nil || l.Owner != owner {
		return errors.New("执行租约归属不匹配")
	}
	if l.State == "running" {
		return nil
	}
	if l.State != "granted" {
		return errors.New("执行租约尚未获准或已取消")
	}
	l.State, l.StartedAt = "running", now
	return nil
}

// Detach changes only the wait owner. Machine slots and resources stay held.
func (s *Scheduler) Detach(id, owner string) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[id]
	if l == nil || l.Owner != owner {
		return Lease{}, errors.New("执行租约归属不匹配")
	}
	if l.Background {
		return clone(l), nil
	}
	if l.State == "unknown" || l.State == "cancelling" {
		return Lease{}, errors.New("任务正在停止或执行状态未知，不能后台化")
	}
	n := 0
	for _, other := range s.leases {
		if other.Agent == l.Agent && other.Background {
			n++
		}
	}
	if l.Agent == "" || n >= s.cfg.BackgroundPerAgent {
		return clone(l), errors.New("agent 后台容量已满，保持原前台执行")
	}
	l.Background = true
	return clone(l), nil
}

func (s *Scheduler) close(l *Lease) {
	delete(s.leases, l.ID)
	s.closed[l.ID] = l.Owner
	s.closedOrder = append(s.closedOrder, l.ID)
	if len(s.closedOrder) > 1024 {
		delete(s.closed, s.closedOrder[0])
		s.closedOrder = s.closedOrder[1:]
	}
	for _, other := range s.leases {
		if sessionKey(other.Request) == sessionKey(l.Request) {
			return
		}
	}
	delete(s.served, sessionKey(l.Request))
}

func (s *Scheduler) Release(id, owner string, now time.Time) ([]Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[id]
	if l == nil {
		if previous, ok := s.closed[id]; ok && previous != owner {
			return nil, errors.New("执行租约归属不匹配")
		}
		return nil, nil
	}
	if l.Owner != owner {
		return nil, errors.New("执行租约归属不匹配")
	}
	s.close(l)
	return s.promote(now), nil
}

// Cancel never releases a running process's lock. Its owner must confirm exit.
func (s *Scheduler) Cancel(id, owner string, now time.Time) ([]Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[id]
	if l == nil {
		return nil, nil
	}
	if l.Owner != owner {
		return nil, errors.New("执行租约归属不匹配")
	}
	if l.State == "queued" {
		s.close(l)
		return s.promote(now), nil
	}
	if l.State != "unknown" {
		l.State = "cancelling"
	}
	return nil, nil
}

func (s *Scheduler) Disconnect(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.leases {
		if l.Owner == owner {
			if l.State == "queued" {
				s.close(l)
			} else {
				l.State = "unknown"
			}
		}
	}
}

func (s *Scheduler) Snapshot() []Lease {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Lease, 0, len(s.leases))
	for _, l := range s.leases {
		out = append(out, clone(l))
	}
	sortLeases(out)
	return out
}
func sortLeases(out []Lease) {
	slices.SortFunc(out, func(a, b Lease) int {
		if a.serial < b.serial {
			return -1
		}
		if a.serial > b.serial {
			return 1
		}
		return 0
	})
}

func (s *Scheduler) Lookup(id string) (Lease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[id]
	if l == nil {
		return Lease{}, false
	}
	return clone(l), true
}

// MarkUnknown retains the real slot and resource ownership after an ambiguous
// transport result. Only reconciliation or confirmed termination may release it.
func (s *Scheduler) MarkUnknown(id, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.leases[id]
	if l == nil || l.Owner != owner {
		return errors.New("执行租约不存在或归属不符")
	}
	l.State, l.WaitFor = "unknown", "reconcile"
	return nil
}
