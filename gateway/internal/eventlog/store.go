package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const maxTransaction = 64 << 10

type patch struct {
	Cursors          []Cursor      `json:"cursors,omitempty"`
	Jobs             []JobRecord   `json:"jobs,omitempty"`
	Events           []EventRecord `json:"events,omitempty"`
	Deliveries       []Delivery    `json:"deliveries,omitempty"`
	ForgetEvents     []string      `json:"forget_events,omitempty"`
	ForgetDeliveries []string      `json:"forget_deliveries,omitempty"`
}
type pending struct {
	tx   Transaction
	done func(error)
}
type Store struct {
	path         string
	limits       Limits
	file         *os.File // single worker only after Open
	seq          uint64
	bytes        int64
	compactAt    int64
	mu           sync.RWMutex
	cursors      map[string]Cursor
	jobs         map[string]JobRecord
	events       map[string]EventRecord
	deliveries   map[string]Delivery
	reserved     int
	live         int64
	reserveBytes int64
	failure      error
	gate         sync.Mutex
	stopped      bool
	queue        chan pending
	done         chan struct{}
}

func Open(ctx context.Context, path string, limits Limits) (*Store, error) {
	limits = limits.defaults()
	if limits.Queue > 128 || limits.Events < limits.Jobs || limits.LiveBytes > 64<<20 ||
		limits.JournalBytes < limits.LiveBytes || limits.Queue < 1 {
		return nil, errors.New("可靠事件容量配置无效")
	}
	s := &Store{path: path, limits: limits, jobs: map[string]JobRecord{}, cursors: map[string]Cursor{},
		events: map[string]EventRecord{}, deliveries: map[string]Delivery{},
		queue: make(chan pending, limits.Queue), done: make(chan struct{})}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	s.file = file
	if err = s.replay(); err == nil {
		err = s.checkState()
	}
	if err == nil {
		err = s.file.Sync()
	}
	if err == nil {
		err = syncParent(path)
	}
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("可靠事件恢复失败，保留现场: %w", err)
	}
	s.compactAt = max(limits.CompactBytes, s.live*2)
	go s.run()
	go func() {
		select {
		case <-ctx.Done():
			s.Stop()
		case <-s.done:
		}
	}()
	return s, nil
}

// Submit never waits for disk or queue capacity. A rejected request has no
// callback. Every accepted request has exactly one callback, outside all locks.
func (s *Store) Submit(tx Transaction, done func(error)) error {
	raw, err := json.Marshal(tx)
	if err != nil || len(raw) > maxTransaction {
		return errors.New("可靠事件事务超过 64 KiB 或无效")
	}
	var frozen Transaction
	if err = json.Unmarshal(raw, &frozen); err != nil {
		return err
	}
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.stopped {
		return ErrClosed
	}
	select {
	case s.queue <- pending{frozen, done}:
		return nil
	default:
		return ErrBusy
	}
}

// Commit's context bounds the caller's wait; cancelling it does not withdraw an
// already accepted transaction. Sources must retry with the same stable ID.
func (s *Store) Commit(ctx context.Context, tx Transaction) error {
	result := make(chan error, 1)
	if err := s.Submit(tx, func(err error) { result <- err }); err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Store) Stop() {
	s.gate.Lock()
	defer s.gate.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.queue)
	}
}
func (s *Store) Close()                { s.Stop(); <-s.done }
func (s *Store) Done() <-chan struct{} { return s.done }
func (s *Store) run() {
	defer close(s.done)
	defer func() {
		if s.file != nil {
			s.file.Close()
		}
	}()
	for request := range s.queue {
		s.mu.RLock()
		err := s.failure
		s.mu.RUnlock()
		if err == nil {
			var changes patch
			changes, err = s.prepare(request.tx)
			if err == nil && !empty(changes) {
				err = s.append(changes)
				if err == nil {
					s.mu.Lock()
					s.apply(changes)
					s.mu.Unlock()
				} else {
					s.mu.Lock()
					s.failure = err
					s.mu.Unlock()
				}
			}
		}
		if request.done != nil {
			request.done(err)
		}
	}
}
func empty(p patch) bool {
	return len(p.Jobs)+len(p.Events)+len(p.Deliveries)+len(p.Cursors)+len(p.ForgetEvents)+len(p.ForgetDeliveries) == 0
}
func size(v any) int64 { raw, _ := json.Marshal(v); return int64(len(raw)) }
func (s *Store) prepare(tx Transaction) (patch, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := patch{}
	modes := 0
	if tx.Job != nil || tx.Event != nil {
		modes++
	}
	if tx.Match != nil {
		modes++
	}
	if tx.Delivery != nil {
		modes++
	}
	if len(tx.Forget) > 0 {
		modes++
	}
	if modes != 1 || (tx.Admit && tx.Job == nil) {
		return p, errors.New("可靠事件事务操作不完整")
	}
	if tx.Job != nil {
		j := *tx.Job
		if err := jobValid(j); err != nil {
			return p, err
		}
		key := jobKey(j)
		old, found := s.jobs[key]
		if !found && !tx.Admit {
			return p, errors.New("任务未经过可靠准入，不允许以观察结果激活")
		}
		if found {
			if identity(old.Job) != identity(j) {
				return p, errors.New("任务调用身份、权限来源或执行范围不能改变")
			}
			if j.Receipt.Revision < old.Receipt.Revision {
				return p, errors.New("任务状态修订倒退")
			}
			if phase(j.Receipt.Status) < phase(old.Receipt.Status) ||
				(j.Receipt.Revision == old.Receipt.Revision && (j.Receipt.Status != old.Receipt.Status || j.Receipt.Background != old.Receipt.Background)) {
				return p, errors.New("任务状态不能倒退或未经修订改变")
			}
			if old.Receipt.Background && !j.Receipt.Background {
				return p, errors.New("后台所有权不能退回前台")
			}
			if terminal(old.Receipt.Status) && !sameTerminal(old.Receipt, j.Receipt) {
				return p, errors.New("已提交终态不能被旧活动清单覆盖")
			}
		}
		next := JobRecord{Job: j, Reserved: true}
		if found {
			next.Reserved = old.Reserved
			next.TerminalEventID = old.TerminalEventID
		}
		if terminal(j.Receipt.Status) {
			if tx.Event == nil {
				return p, errors.New("任务终态必须与可靠事件一起提交")
			}
			if err := eventMatchesJob(*tx.Event, j); err != nil {
				return p, err
			}
			if found && old.TerminalEventID != "" && old.TerminalEventID != tx.Event.ID {
				return p, errors.New("任务终态事件身份不能改变")
			}
			next.Reserved = false
			next.TerminalEventID = tx.Event.ID
		} else if tx.Event != nil && tx.Event.JobID == j.Receipt.JobID {
			return p, errors.New("活动任务不能占用终态事件预留")
		}
		p.Jobs = append(p.Jobs, next)
	}
	if tx.Event != nil {
		e := *tx.Event
		if err := e.valid(); err != nil {
			return p, err
		}
		if e.JobID != "" && tx.Job == nil {
			return p, errors.New("任务事件缺少同一事务的终态")
		}
		if old, found := s.events[e.ID]; found {
			if !sameEvent(old.Event, e) {
				return p, errors.New("同一事件身份对应了不同事实")
			}
		} else {
			if e.JobID == "" {
				key := cursorKey(e.SourceID, e.SourceEpoch)
				c := s.cursors[key]
				if c.SourceID == "" {
					c = Cursor{SourceID: e.SourceID, Epoch: e.SourceEpoch}
				}
				if c.contains(e.Seq) {
					return patch{}, nil
				}
				if e.Seq-c.Through > 4096 {
					return p, errors.New("事件来源序号缺口超过有界窗口")
				}
				c.Seen = append(append([]uint64(nil), c.Seen...), e.Seq)
				sort.Slice(c.Seen, func(i, j int) bool { return c.Seen[i] < c.Seen[j] })
				for len(c.Seen) > 0 && c.Seen[0] == c.Through+1 {
					c.Through++
					c.Seen = c.Seen[1:]
				}
				if len(c.Seen) > 128 {
					return p, ErrCapacity
				}
				p.Cursors = append(p.Cursors, c)
			}
			// A retained terminal identity also deduplicates receipts after
			// completed Hook bodies have been pruned.
			consumed := false
			if tx.Job != nil {
				if old, found := s.jobs[jobKey(*tx.Job)]; found && old.TerminalEventID == e.ID {
					consumed = true
				}
			}
			if !consumed {
				p.Events = append(p.Events, EventRecord{Event: e})
			}
		}
	}
	if tx.Match != nil {
		m := tx.Match
		e, found := s.events[m.EventID]
		if !found || len(m.Deliveries) > 64 {
			return p, errors.New("待匹配事件不存在或投递过多")
		}
		ids := map[string]bool{}
		for _, d := range m.Deliveries {
			if err := d.valid(); err != nil {
				return p, err
			}
			if d.EventID != m.EventID || d.Status != "pending" || d.Attempt != 0 || ids[d.ID] ||
				d.AgentID != e.AgentID || d.HookID == e.OriginHook || e.Silent && !d.Silent {
				return p, errors.New("Hook 初次投递身份、状态或因果来源无效")
			}
			ids[d.ID] = true
			if old, exists := s.deliveries[d.ID]; exists {
				if !sameDelivery(old, d) {
					return p, errors.New("投递的固定规则版本或权限不能改变")
				}
			} else if e.Matched {
				return p, errors.New("事件已经匹配，不能追加另一套规则")
			} else {
				p.Deliveries = append(p.Deliveries, d)
			}
		}
		if e.Matched {
			if len(ids) != len(e.DeliveryIDs) {
				return p, errors.New("重试匹配必须复用同一套投递")
			}
			for _, id := range e.DeliveryIDs {
				if !ids[id] {
					return p, errors.New("重试匹配改变了投递身份")
				}
			}
		} else {
			e.Matched = true
			for id := range ids {
				e.DeliveryIDs = append(e.DeliveryIDs, id)
			}
			sort.Strings(e.DeliveryIDs)
			p.Events = append(p.Events, e)
		}
	}
	if tx.Delivery != nil {
		d := *tx.Delivery
		if err := d.valid(); err != nil {
			return p, err
		}
		old, found := s.deliveries[d.ID]
		if !found || !sameDelivery(old, d) || d.Attempt < old.Attempt || d.Attempt > old.Attempt+1 ||
			(deliveryDone(old.Status) && (d.Status != old.Status || d.Attempt != old.Attempt)) {
			return p, errors.New("Hook 投递不存在或固定身份、终态、重试序号改变")
		}
		if d.Status == "running" && d.Attempt > old.Attempt && old.Status != "pending" && old.Status != "retry" {
			return p, errors.New("不确定或结束的投递不能直接重新执行")
		}
		if (d.Status == "succeeded" || d.Status == "uncertain") && old.Status != d.Status && (old.Status != "running" || d.Attempt != old.Attempt) {
			return p, errors.New("外部动作终态缺少已持久登记的执行尝试")
		}
		if d.Status == "running" && d.Attempt == old.Attempt && old.Status != "running" {
			return p, errors.New("开始执行必须持久增加一次尝试")
		}
		p.Deliveries = append(p.Deliveries, d)
	}
	if len(tx.Forget) > 64 {
		return p, errors.New("单次回收事件超过 64 条")
	}
	seen := map[string]bool{}
	for _, id := range tx.Forget {
		if seen[id] {
			continue
		}
		seen[id] = true
		e, found := s.events[id]
		if !found {
			continue
		}
		if !e.Matched {
			return p, errors.New("尚未匹配的可靠事件不能回收")
		}
		for _, deliveryID := range e.DeliveryIDs {
			d, ok := s.deliveries[deliveryID]
			if !ok || !deliveryDone(d.Status) {
				return p, errors.New("尚未处理完的投递不能回收")
			}
			p.ForgetDeliveries = append(p.ForgetDeliveries, deliveryID)
		}
		p.ForgetEvents = append(p.ForgetEvents, id)
	}
	jobs, events, deliveries, reserved, live := len(s.jobs), len(s.events), len(s.deliveries), s.reserved, s.live
	reserveBytes := s.reserveBytes
	sources := len(s.cursors)
	for _, c := range p.Cursors {
		if old, ok := s.cursors[cursorKey(c.SourceID, c.Epoch)]; ok {
			live -= size(old)
		} else {
			sources++
		}
		live += size(c)
	}
	for _, j := range p.Jobs {
		old, ok := s.jobs[jobKey(j.Job)]
		if ok {
			live -= size(old)
			reserveBytes -= reservedBytes(old)
			if old.Reserved {
				reserved--
			}
		} else {
			jobs++
		}
		live += size(j)
		reserveBytes += reservedBytes(j)
		if j.Reserved {
			reserved++
		}
	}
	for _, e := range p.Events {
		if old, ok := s.events[e.ID]; ok {
			live -= size(old)
		} else {
			events++
		}
		live += size(e)
	}
	for _, d := range p.Deliveries {
		if old, ok := s.deliveries[d.ID]; ok {
			live -= size(old)
		} else {
			deliveries++
		}
		live += size(d)
	}
	for _, id := range p.ForgetEvents {
		live -= size(s.events[id])
		events--
	}
	for _, id := range p.ForgetDeliveries {
		live -= size(s.deliveries[id])
		deliveries--
	}
	if sources > s.limits.Sources || jobs > s.limits.Jobs || events+reserved > s.limits.Events || deliveries > s.limits.Deliveries || live+reserveBytes > s.limits.LiveBytes {
		return patch{}, ErrCapacity
	}
	return p, nil
}

// Worker owns mutation; readers hold mu and receive copies of immutable bodies.
func (s *Store) apply(p patch) {
	for _, c := range p.Cursors {
		key := cursorKey(c.SourceID, c.Epoch)
		if old, ok := s.cursors[key]; ok {
			s.live -= size(old)
		}
		s.cursors[key] = c
		s.live += size(c)
	}
	for _, j := range p.Jobs {
		key := jobKey(j.Job)
		if old, ok := s.jobs[key]; ok {
			s.live -= size(old)
			s.reserveBytes -= reservedBytes(old)
			if old.Reserved {
				s.reserved--
			}
		}
		s.jobs[key] = j
		s.live += size(j)
		s.reserveBytes += reservedBytes(j)
		if j.Reserved {
			s.reserved++
		}
	}
	for _, e := range p.Events {
		if old, ok := s.events[e.ID]; ok {
			s.live -= size(old)
		}
		s.events[e.ID] = e
		s.live += size(e)
	}
	for _, d := range p.Deliveries {
		if old, ok := s.deliveries[d.ID]; ok {
			s.live -= size(old)
		}
		s.deliveries[d.ID] = d
		s.live += size(d)
	}
	for _, id := range p.ForgetEvents {
		if old, ok := s.events[id]; ok {
			s.live -= size(old)
			delete(s.events, id)
		}
	}
	for _, id := range p.ForgetDeliveries {
		if old, ok := s.deliveries[id]; ok {
			s.live -= size(old)
			delete(s.deliveries, id)
		}
	}
}
func copyValue[T any](v T) T {
	raw, _ := json.Marshal(v)
	var out T
	json.Unmarshal(raw, &out)
	return out
}
func (s *Store) Job(source, id string) (JobRecord, bool) {
	s.mu.RLock()
	j, ok := s.jobs[cursorKey(source, id)]
	s.mu.RUnlock()
	return copyValue(j), ok
}
func (s *Store) Event(id string) (EventRecord, bool) {
	s.mu.RLock()
	e, ok := s.events[id]
	s.mu.RUnlock()
	return copyValue(e), ok
}
func (s *Store) Delivery(id string) (Delivery, bool) {
	s.mu.RLock()
	d, ok := s.deliveries[id]
	s.mu.RUnlock()
	return copyValue(d), ok
}
func (s *Store) PendingEvents(limit int) []EventRecord {
	limit = min(max(limit, 1), 128)
	s.mu.RLock()
	out := make([]EventRecord, 0, min(limit, len(s.events)))
	// Select oldest using a bounded slice; do not copy the entire index.
	for _, e := range s.events {
		if e.Matched {
			continue
		}
		at := sort.Search(len(out), func(i int) bool {
			return out[i].ObservedAt > e.ObservedAt || out[i].ObservedAt == e.ObservedAt && out[i].ID > e.ID
		})
		if at >= limit {
			continue
		}
		out = append(out, EventRecord{})
		copy(out[at+1:], out[at:])
		out[at] = e
		if len(out) > limit {
			out = out[:limit]
		}
	}
	s.mu.RUnlock()
	return copyValue(out)
}
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Stats{Jobs: len(s.jobs), Sources: len(s.cursors), Events: len(s.events), Deliveries: len(s.deliveries),
		Reserved: s.reserved, Queued: len(s.queue), LiveBytes: s.live, JournalBytes: s.bytes, ReservedBytes: s.reserveBytes, Err: s.failure}
}
