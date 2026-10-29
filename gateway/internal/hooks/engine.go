package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"nano-agent/gateway/internal/eventlog"
)

type Execution struct {
	Delivery eventlog.Delivery
	Event    eventlog.Event
	Spec     Spec
}
type Result struct {
	Summary   string
	Err       error
	Retry     bool
	Uncertain bool
}

// Execute may finish asynchronously (a C agent run); completion must retain the
// same delivery identity. The engine owns durable transitions and retry limits.
type Executor interface {
	Visible(eventlog.RuleRecord, eventlog.Event, Spec) bool
	Execute(context.Context, Execution, func(Result))
}
type active struct {
	context   context.Context
	work      Execution
	cancel    context.CancelFunc
	deadline  time.Time
	completed bool
}
type finished struct {
	id     string
	result Result
}
type Engine struct {
	ctx                      context.Context
	cancel                   context.CancelFunc
	store                    *eventlog.Store
	executor                 Executor
	clock                    func() time.Time
	wake                     chan struct{}
	notify, commands, agents chan *active
	results                  chan finished
	mu                       sync.Mutex
	live                     map[string]*active
	hooks                    map[string]bool
	modelAgents              map[string]bool
	last                     map[string]int64
	ruleVersion              uint64
	rules                    []eventlog.RuleRecord
	wg                       sync.WaitGroup
	cursor                   string
	err                      error
}

func New(ctx context.Context, store *eventlog.Store, executor Executor, clock func() time.Time) *Engine {
	if clock == nil {
		clock = time.Now
	}
	ctx, cancel := context.WithCancel(ctx)
	e := &Engine{ctx: ctx, cancel: cancel, store: store, executor: executor, clock: clock, wake: make(chan struct{}, 1),
		notify: make(chan *active, 32), commands: make(chan *active, 32), agents: make(chan *active, 16), results: make(chan finished, 64),
		live: map[string]*active{}, hooks: map[string]bool{}, modelAgents: map[string]bool{}, last: map[string]int64{}}
	e.wg.Add(1)
	go e.loop()
	for _, queue := range []chan *active{e.notify, e.commands, e.commands, e.agents} {
		e.wg.Add(1)
		go e.worker(queue)
	}
	return e
}
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) Close()         { e.cancel(); e.wg.Wait() }
func (e *Engine) Err() error     { e.mu.Lock(); defer e.mu.Unlock(); return e.err }
func (e *Engine) fail(err error) { e.mu.Lock(); e.err = err; e.mu.Unlock() }
func (e *Engine) loop() {
	defer e.wg.Done()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-e.ctx.Done():
			e.mu.Lock()
			for _, job := range e.live {
				job.cancel()
			}
			e.mu.Unlock()
			return
		case result := <-e.results:
			e.finish(result)
		case <-tick.C:
			e.step()
		case <-e.wake:
			e.step()
		}
	}
}
func (e *Engine) complete(id string, result Result) {
	e.mu.Lock()
	job := e.live[id]
	if job == nil || job.completed {
		e.mu.Unlock()
		return
	}
	job.completed = true
	e.mu.Unlock()
	// At most 64 live attempts; each enqueues at most one completion.
	select {
	case e.results <- finished{id, result}:
	case <-e.ctx.Done():
	}
}
func (e *Engine) worker(queue <-chan *active) {
	defer e.wg.Done()
	for {
		select {
		case <-e.ctx.Done():
			return
		case job := <-queue:
			if job.work.Delivery.ID == "" {
				continue
			}
			if job.work.Delivery.Registered {
				rule, ok := e.store.Rule(job.work.Delivery.HookID)
				if !ok || rule.Deleted || !rule.Enabled {
					e.complete(job.work.Delivery.ID, Result{Err: fmt.Errorf("Hook 已撤回")})
					continue
				}
			}
			if !e.executor.Visible(eventlog.RuleRecord{ID: job.work.Delivery.HookID, Revision: job.work.Delivery.Revision,
				AgentID: job.work.Delivery.AgentID, Creator: job.work.Delivery.Creator, Silent: job.work.Delivery.Silent,
				Body: job.work.Delivery.Rule}, job.work.Event, job.work.Spec) {
				e.complete(job.work.Delivery.ID, Result{Err: fmt.Errorf("Hook 当前权限或来源不可用")})
				continue
			}
			if job.context.Err() != nil {
				continue
			}
			e.executor.Execute(job.context, job.work, func(result Result) { e.complete(job.work.Delivery.ID, result) })
		}
	}
}
func (e *Engine) step() {
	now := e.clock()
	var expired []finished
	e.mu.Lock()
	for id, job := range e.live {
		if !job.completed && !now.Before(job.deadline) {
			job.completed = true
			job.cancel()
			expired = append(expired, finished{id, Result{Err: fmt.Errorf("Hook 执行超时"), Uncertain: job.work.Spec.Handler.Type != "command"}})
		}
	}
	e.mu.Unlock()
	for _, done := range expired {
		e.finish(done)
	}
	version := e.store.RuleVersion()
	if version != e.ruleVersion {
		e.rules = e.store.Rules("")
		e.ruleVersion = version
	}
	for _, event := range e.store.PendingEvents(16) {
		if version := e.store.RuleVersion(); version != e.ruleVersion {
			e.rules = e.store.Rules("")
			e.ruleVersion = version
		}
		deliveries := []eventlog.Delivery{}
		for _, record := range e.rules {
			if record.Deleted || !record.Enabled || record.AgentID != event.AgentID || record.ID == event.OriginHook ||
				record.Once && record.ClaimedBy != "" {
				continue
			}
			spec, err := Parse(record)
			if err != nil || spec.Handler.Type == "command" && containsBefore(spec.On) || event.Depth >= 4 || !Matches(spec, event.Event) ||
				!e.executor.Visible(record, event.Event, spec) {
				continue
			}
			deliveries = append(deliveries, eventlog.Delivery{ID: eventlog.DeliveryID(event.ID, record.ID, record.Revision),
				EventID: event.ID, HookID: record.ID, Revision: record.Revision, AgentID: record.AgentID, Creator: record.Creator,
				Silent: event.Silent || record.Silent || spec.Handler.Conversation == "branch", TargetSID: spec.Scope.SID,
				Rule: record.Body, Status: "pending", ObjectKey: ObjectKey(event.Event),
				NextAt: now.UnixMilli() + int64(spec.DebounceMS), UpdatedMS: now.UnixMilli()})
		}
		if err := e.store.Commit(e.ctx, eventlog.Transaction{Match: &eventlog.Match{EventID: event.ID, Checked: true, Coalesce: true, Deliveries: deliveries}}); err != nil {
			e.fail(err)
			break
		}
	}
	rows, next := e.store.Deliveries(e.cursor, 128)
	e.cursor = next
	for _, d := range rows {
		if !d.Registered {
			continue
		}
		e.mu.Lock()
		_, running := e.live[d.ID]
		busy := e.hooks[d.HookID]
		modelsBusy := e.modelAgents[d.AgentID]
		e.mu.Unlock()
		if running {
			continue
		}
		spec, err := Spec{}, error(nil)
		err = json.Unmarshal(d.Rule, &spec)
		if err != nil {
			continue
		}
		if d.Status == "running" {
			if (spec.Handler.Type == "notify" || spec.Handler.RetrySafe) && d.Attempt < min(8, spec.Retries+1) {
				d.Status = "retry"
				d.NextAt = now.UnixMilli() + 1000
				d.Error = "上次尝试结果不明；按至少一次重试，外部通知可能重复"
			} else {
				d.Status = "uncertain"
				d.Error = "重启前动作结果不明，需要检查同一投递"
			}
			d.UpdatedMS = now.UnixMilli()
			if err = e.store.Commit(e.ctx, eventlog.Transaction{Delivery: &d}); err != nil {
				e.fail(err)
			}
			continue
		}
		if d.Status != "pending" && d.Status != "retry" || busy || d.NextAt > now.UnixMilli() {
			continue
		}
		if spec.Handler.Type == "agent" && modelsBusy {
			continue
		}
		rule, exists := e.store.Rule(d.HookID)
		if !exists || rule.Deleted || !rule.Enabled {
			d.Status = "cancelled"
			d.Error = "Hook 已撤回"
			d.UpdatedMS = now.UnixMilli()
			if err = e.store.Commit(e.ctx, eventlog.Transaction{Delivery: &d}); err != nil {
				e.fail(err)
			}
			continue
		}
		key := d.HookID + "/" + d.ObjectKey
		if now.UnixMilli() < e.last[key]+int64(spec.CooldownMS) {
			continue
		}
		event, ok := e.store.Event(d.EventID)
		if !ok {
			continue
		}
		queue := e.notify
		if spec.Handler.Type == "command" {
			queue = e.commands
		} else if spec.Handler.Type == "agent" {
			queue = e.agents
		}
		if len(queue) == cap(queue) {
			continue
		}
		e.mu.Lock()
		if len(e.live) >= 64 {
			e.mu.Unlock()
			break
		}
		ctx, cancel := context.WithCancel(e.ctx)
		work := &active{context: ctx, work: Execution{Delivery: d, Event: event.Event, Spec: spec}, cancel: cancel,
			deadline: now.Add(time.Duration(spec.Handler.TimeoutMS) * time.Millisecond)}
		e.live[d.ID] = work
		e.hooks[d.HookID] = true
		if spec.Handler.Type == "agent" {
			e.modelAgents[d.AgentID] = true
		}
		e.mu.Unlock()
		d.Status = "running"
		d.Attempt++
		d.UpdatedMS = now.UnixMilli()
		if err = e.store.Commit(e.ctx, eventlog.Transaction{Delivery: &d}); err != nil {
			e.release(work)
			e.fail(err)
			continue
		}
		work.work.Delivery = d
		e.last[key] = now.UnixMilli()
		if len(e.last) > 1024 {
			for old := range e.last {
				delete(e.last, old)
				break
			}
		}
		select {
		case queue <- work:
		case <-e.ctx.Done():
			cancel()
			return
		}
	}
}
func containsBefore(names []string) bool {
	for _, name := range names {
		if name == "tool.before" {
			return true
		}
	}
	return false
}
func (e *Engine) release(job *active) {
	job.cancel()
	e.mu.Lock()
	delete(e.live, job.work.Delivery.ID)
	delete(e.hooks, job.work.Delivery.HookID)
	if job.work.Spec.Handler.Type == "agent" {
		delete(e.modelAgents, job.work.Delivery.AgentID)
	}
	e.mu.Unlock()
}
func (e *Engine) finish(done finished) {
	e.mu.Lock()
	job := e.live[done.id]
	e.mu.Unlock()
	if job == nil {
		return
	}
	d := job.work.Delivery
	result := done.result
	d.Result = result.Summary
	if len(d.Result) > 2048 {
		d.Result = d.Result[:2048]
	}
	d.UpdatedMS = e.clock().UnixMilli()
	switch {
	case result.Uncertain:
		d.Status = "uncertain"
	case result.Err == nil:
		d.Status = "succeeded"
	case result.Retry && d.Attempt <= job.work.Spec.Retries:
		d.Status = "retry"
		d.NextAt = d.UpdatedMS + int64(1000<<min(d.Attempt-1, 5))
	default:
		d.Status = "failed"
	}
	if result.Err != nil {
		d.Error = result.Err.Error()
		if len(d.Error) > 512 {
			d.Error = d.Error[:512]
		}
	}
	if err := e.store.Commit(e.ctx, eventlog.Transaction{Delivery: &d}); err != nil {
		e.fail(err)
	}
	e.release(job)
}
