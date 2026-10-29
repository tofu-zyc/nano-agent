package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nano-agent/gateway/internal/eventlog"
)

type recorder struct {
	calls chan Execution
	hold  bool
}

func (*recorder) Visible(eventlog.RuleRecord, eventlog.Event, Spec) bool { return true }
func (r *recorder) Execute(ctx context.Context, x Execution, done func(Result)) {
	r.calls <- x
	if !r.hold {
		done(Result{Summary: Template(x.Spec.Handler.Text, x.Event)})
	}
}
func hookFixture(t *testing.T, once bool, debounce int) (*eventlog.Store, eventlog.RuleRecord) {
	t.Helper()
	store, err := eventlog.Open(context.Background(), filepath.Join(t.TempDir(), "events"), eventlog.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	spec, err := Normalize(Spec{On: []string{"device.online"}, Scope: Scope{SID: "chat", Mode: "auto"},
		Handler: Handler{Type: "notify", Text: "observed {{event_name}}"}, DebounceMS: debounce})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(spec)
	rule := eventlog.RuleRecord{ID: "device", Revision: 1, AgentID: "agent", Creator: "user", OperationID: "one-operation", Enabled: true, Once: once, Body: body}
	if err = store.Commit(context.Background(), eventlog.Transaction{Rule: &eventlog.RuleChange{Record: rule}}); err != nil {
		t.Fatal(err)
	}
	return store, rule
}
func publish(t *testing.T, store *eventlog.Store, seq uint64, origin string) eventlog.Event {
	t.Helper()
	event := eventlog.Event{Name: "device.online", AgentID: "agent", SourceID: "device", SourceEpoch: "boot", Seq: seq, OccurredAt: 100, ObservedAt: 100, OriginHook: origin}
	event.ID = eventlog.EventID(event.SourceID, event.SourceEpoch, event.Seq)
	if err := store.Commit(context.Background(), eventlog.Transaction{Event: &event}); err != nil {
		t.Fatal(err)
	}
	return event
}
func waitState(t *testing.T, store *eventlog.Store, id, status string) eventlog.Delivery {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for {
		d, ok := store.Delivery(id)
		if ok && d.Status == status {
			return d
		}
		if time.Now().After(end) {
			t.Fatal("delivery did not advance", status, d)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestReliableRuleActuallyExecutesAndConsumesOnceWithoutSchedule(t *testing.T) {
	store, rule := hookFixture(t, true, 0)
	exec := &recorder{calls: make(chan Execution, 10)}
	engine := New(context.Background(), store, exec, nil)
	defer engine.Close()
	event := publish(t, store, 1, "")
	engine.Wake()
	id := eventlog.DeliveryID(event.ID, rule.ID, rule.Revision)
	d := waitState(t, store, id, "succeeded")
	if d.Attempt != 1 || d.Result != "observed device.online" {
		t.Fatal(d)
	}
	if r, _ := store.Rule(rule.ID); r.Enabled || r.ClaimedBy != event.ID {
		t.Fatal(r)
	}
	publish(t, store, 2, "")
	engine.Wake()
	select {
	case <-exec.calls:
	default:
		t.Fatal("handler did not execute")
	}
	select {
	case x := <-exec.calls:
		t.Fatal("once executed twice", x)
	case <-time.After(300 * time.Millisecond):
	}
}
func TestDebounceCoalescesPersistedObjectAndCausationDoesNotLoop(t *testing.T) {
	store, rule := hookFixture(t, false, 200)
	var clock atomic.Int64
	clock.Store(100000)
	exec := &recorder{calls: make(chan Execution, 10)}
	engine := New(context.Background(), store, exec, func() time.Time { return time.UnixMilli(clock.Load()) })
	defer engine.Close()
	first := publish(t, store, 1, "")
	engine.Wake()
	firstID := eventlog.DeliveryID(first.ID, rule.ID, rule.Revision)
	waitState(t, store, firstID, "pending")
	second := publish(t, store, 2, "")
	engine.Wake()
	waitState(t, store, firstID, "cancelled")
	clock.Store(100500)
	engine.Wake()
	secondID := eventlog.DeliveryID(second.ID, rule.ID, rule.Revision)
	waitState(t, store, secondID, "succeeded")
	select {
	case x := <-exec.calls:
		if x.Event.ID != second.ID {
			t.Fatal(x)
		}
	case <-time.After(time.Second):
		t.Fatal("no coalesced execution")
	}
	own := publish(t, store, 3, rule.ID)
	engine.Wake()
	end := time.Now().Add(time.Second)
	for {
		current, _ := store.Event(own.ID)
		if current.Matched {
			break
		}
		if time.Now().After(end) {
			t.Fatal("causal event not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case x := <-exec.calls:
		t.Fatal("self-causal feedback executed", x)
	default:
	}
}

func TestHookDeadlineHeapPublishesOnlyItsRegisteredRuleAndSurvivesRestart(t *testing.T) {
	store, rule := hookFixture(t, true, 0)
	spec, _ := Parse(rule)
	spec.On = []string{"deadline.at"}
	spec.DeadlineAt = 100500
	body, _ := json.Marshal(spec)
	rule.Body = body
	rule.Revision++
	if err := store.Commit(context.Background(), eventlog.Transaction{Rule: &eventlog.RuleChange{Record: rule, Expected: 1}}); err != nil {
		t.Fatal(err)
	}
	spec.DeadlineAt = 200000
	body, _ = json.Marshal(spec)
	later := eventlog.RuleRecord{ID: "later", Revision: 1, AgentID: rule.AgentID, Creator: rule.Creator, OperationID: "later-op", Enabled: true, Once: true, Body: body}
	if err := store.Commit(context.Background(), eventlog.Transaction{Rule: &eventlog.RuleChange{Record: later}}); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(100000)
	exec := &recorder{calls: make(chan Execution, 8)}
	engine := New(context.Background(), store, exec, func() time.Time { return time.UnixMilli(clock.Load()) })
	engine.Wake()
	select {
	case got := <-exec.calls:
		t.Fatal("deadline early", got)
	case <-time.After(30 * time.Millisecond):
	}
	clock.Store(100500)
	engine.Wake()
	var got Execution
	select {
	case got = <-exec.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("deadline did not fire")
	}
	if got.Delivery.HookID != rule.ID || got.Event.Name != "deadline.at" {
		t.Fatal(got)
	}
	waitState(t, store, got.Delivery.ID, "succeeded")
	engine.Close()
	engine = New(context.Background(), store, exec, func() time.Time { return time.UnixMilli(clock.Load()) })
	defer engine.Close()
	engine.Wake()
	select {
	case got = <-exec.calls:
		t.Fatal("replayed once deadline or fired unrelated rule", got)
	case <-time.After(50 * time.Millisecond):
	}
	if later, _ := store.Rule("later"); later.ClaimedBy != "" {
		t.Fatal(later)
	}
}

func TestHookPreflightOrderedVerdictsAndNoBlindReplay(t *testing.T) {
	store, rule := hookFixture(t, false, 0)
	spec, _ := Parse(rule)
	spec.On = []string{"tool.before"}
	spec.Handler = Handler{Type: "command", Decision: "deny"}
	spec, err := Normalize(spec)
	if err != nil {
		t.Fatal(err)
	}
	rule.Revision++
	rule.Body, _ = json.Marshal(spec)
	if err = store.Commit(context.Background(), eventlog.Transaction{Rule: &eventlog.RuleChange{Record: rule, Expected: 1}}); err != nil {
		t.Fatal(err)
	}
	exec := &recorder{calls: make(chan Execution, 4)}
	clock := func() time.Time { return time.Unix(100, 0) }
	engine := New(context.Background(), store, exec, clock)
	defer engine.Close()
	event := eventlog.Event{Name: "tool.before", AgentID: rule.AgentID, SourceID: "preflight", SourceEpoch: "run", Seq: 1, OccurredAt: 100, ObservedAt: 100}
	event.ID = eventlog.EventID(event.SourceID, event.SourceEpoch, event.Seq)
	check := func() CheckResult {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		done := make(chan CheckResult, 1)
		if !engine.Check(ctx, event, func(r CheckResult) { done <- r }) {
			t.Fatal("check queue")
		}
		select {
		case result := <-done:
			return result
		case <-ctx.Done():
			t.Fatal(ctx.Err())
			return CheckResult{}
		}
	}
	if result := check(); result.Allow || result.RequireApproval {
		t.Fatal(result)
	}
	id := eventlog.DeliveryID(event.ID, rule.ID, rule.Revision)
	d := waitState(t, store, id, "succeeded")
	if !d.Preflight || d.Result != "deny" || d.Attempt != 1 {
		t.Fatal(d)
	}
	if result := check(); result.Allow {
		t.Fatal("duplicate changed verdict", result)
	}
	select {
	case got := <-exec.calls:
		t.Fatal("declarative check executed process", got)
	default:
	}
}
func TestHookPreflightCanRequireApprovalAndDoesNotStartModel(t *testing.T) {
	store, rule := hookFixture(t, false, 0)
	spec, _ := Parse(rule)
	spec.On = []string{"tool.before"}
	spec.Handler = Handler{Type: "command", Decision: "approval"}
	spec, _ = Normalize(spec)
	rule.Revision++
	rule.Body, _ = json.Marshal(spec)
	if err := store.Commit(context.Background(), eventlog.Transaction{Rule: &eventlog.RuleChange{Record: rule, Expected: 1}}); err != nil {
		t.Fatal(err)
	}
	engine := New(context.Background(), store, &recorder{calls: make(chan Execution, 1)}, func() time.Time { return time.Unix(100, 0) })
	defer engine.Close()
	event := eventlog.Event{Name: "tool.before", AgentID: rule.AgentID, SourceID: "check", SourceEpoch: "boot", Seq: 1, OccurredAt: 100, ObservedAt: 100}
	event.ID = eventlog.EventID(event.SourceID, event.SourceEpoch, event.Seq)
	done := make(chan CheckResult, 1)
	if !engine.Check(context.Background(), event, func(r CheckResult) { done <- r }) {
		t.Fatal("queue")
	}
	select {
	case r := <-done:
		if !r.Allow || !r.RequireApproval {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("no verdict")
	}
}

type verdictRecorder struct {
	calls     chan string
	decisions map[string]string
}

func (*verdictRecorder) Visible(eventlog.RuleRecord, eventlog.Event, Spec) bool { return true }
func (r *verdictRecorder) Execute(_ context.Context, x Execution, done func(Result)) {
	r.calls <- x.Delivery.HookID
	done(Result{Decision: r.decisions[x.Delivery.HookID]})
}

type errExec struct{}

func (*errExec) Visible(eventlog.RuleRecord, eventlog.Event, Spec) bool { return true }
func (*errExec) Execute(_ context.Context, _ Execution, done func(Result)) {
	done(Result{Err: errors.New("命令自己失败了")})
}

type blockingExec struct {
	started chan string
	release chan struct{}
}

func (*blockingExec) Visible(eventlog.RuleRecord, eventlog.Event, Spec) bool { return true }
func (b *blockingExec) Execute(ctx context.Context, x Execution, done func(Result)) {
	select {
	case b.started <- x.Delivery.HookID:
	default:
	}
	select {
	case <-b.release:
		done(Result{Decision: "allow"})
	case <-ctx.Done():
		done(Result{Err: ctx.Err()})
	}
}

func beforeStore(t *testing.T) *eventlog.Store {
	t.Helper()
	store, err := eventlog.Open(context.Background(), filepath.Join(t.TempDir(), "events"), eventlog.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}
func beforeRule(t *testing.T, store *eventlog.Store, agent, id string, priority int, handler Handler, onError string) eventlog.RuleRecord {
	t.Helper()
	spec, err := Normalize(Spec{Name: id, On: []string{"tool.before"}, Priority: priority, OnError: onError,
		Scope: Scope{SID: "chat", Mode: "auto"}, Handler: handler})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(spec)
	rule := eventlog.RuleRecord{ID: id, Revision: 1, AgentID: agent, Creator: "user", OperationID: "op-" + id, Enabled: true, Body: body}
	if err := store.Commit(context.Background(), eventlog.Transaction{Rule: &eventlog.RuleChange{Record: rule}}); err != nil {
		t.Fatal(err)
	}
	return rule
}
func beforeEvent(agent string, seq uint64) eventlog.Event {
	event := eventlog.Event{Name: "tool.before", AgentID: agent, SourceID: "preflight", SourceEpoch: "boot", Seq: seq, OccurredAt: 100, ObservedAt: 100}
	event.ID = eventlog.EventID(event.SourceID, event.SourceEpoch, event.Seq)
	return event
}
func checkNow(t *testing.T, e *Engine, event eventlog.Event) CheckResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan CheckResult, 1)
	if !e.Check(ctx, event, func(r CheckResult) { done <- r }) {
		t.Fatal("前置检查没有进入队列")
	}
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		t.Fatal("前置检查没有结论")
		return CheckResult{}
	}
}
func TestHookPreflightRunsByPriorityThenIDAndStopsAfterDeny(t *testing.T) {
	store := beforeStore(t)
	for _, rule := range []struct {
		id       string
		priority int
	}{{"z", 1}, {"a", 1}, {"b", 2}, {"c", 3}} {
		beforeRule(t, store, "agent", rule.id, rule.priority, Handler{Type: "command", Command: "true"}, "")
	}
	exec := &verdictRecorder{calls: make(chan string, 8), decisions: map[string]string{"z": "allow", "a": "allow", "b": "deny", "c": "allow"}}
	engine := New(context.Background(), store, exec, func() time.Time { return time.Unix(100, 0) })
	defer engine.Close()
	event := beforeEvent("agent", 1)
	if result := checkNow(t, engine, event); result.Allow {
		t.Fatal(result)
	}
	order := []string{}
	for len(exec.calls) > 0 {
		order = append(order, <-exec.calls)
	}
	if got := strings.Join(order, ","); got != "a,z,b" {
		t.Fatalf("前置规则执行顺序 %s", got)
	}
	if d, _ := store.Delivery(eventlog.DeliveryID(event.ID, "c", 1)); d.Status != "cancelled" || !d.Preflight {
		t.Fatal("deny 之后的规则仍然执行", d)
	}
}
func TestHookPreflightOnErrorAllowsOnlyWhenDeclared(t *testing.T) {
	for _, tc := range []struct {
		name    string
		onError string
		allow   bool
	}{{"默认拒绝", "", false}, {"显式放行", "allow", true}} {
		t.Run(tc.name, func(t *testing.T) {
			store := beforeStore(t)
			rule := beforeRule(t, store, "agent", "flaky", 0, Handler{Type: "command", Command: "exit 1"}, tc.onError)
			engine := New(context.Background(), store, &errExec{}, func() time.Time { return time.Unix(100, 0) })
			defer engine.Close()
			event := beforeEvent("agent", 1)
			if result := checkNow(t, engine, event); result.Allow != tc.allow {
				t.Fatal(result)
			}
			d := waitState(t, store, eventlog.DeliveryID(event.ID, rule.ID, rule.Revision), "failed")
			if d.Error == "" || !d.Preflight {
				t.Fatal(d)
			}
		})
	}
}
func TestHookPreflightQueueFullRefusesInsteadOfAllowing(t *testing.T) {
	store := beforeStore(t)
	beforeRule(t, store, "a", "one", 0, Handler{Type: "command", Command: "true"}, "")
	beforeRule(t, store, "b", "two", 1, Handler{Type: "command", Command: "true"}, "")
	exec := &blockingExec{started: make(chan string, 8), release: make(chan struct{})}
	engine := New(context.Background(), store, exec, func() time.Time { return time.Unix(100, 0) })
	defer engine.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan CheckResult, 8)
	for i, agent := range []string{"a", "b"} {
		if !engine.Check(ctx, beforeEvent(agent, uint64(i+1)), func(r CheckResult) { finished <- r }) {
			t.Fatal("前置检查没有进入队列")
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-exec.started:
		case <-time.After(3 * time.Second):
			t.Fatal("两个命令 worker 没有都开始")
		}
	}
	for i := 0; i < cap(engine.commands); i++ {
		if !engine.Check(ctx, beforeEvent("a", uint64(i+10)), func(CheckResult) {}) {
			t.Fatalf("队列还有空位时第 %d 个就被拒绝", i)
		}
	}
	if engine.Check(ctx, beforeEvent("a", 99), func(CheckResult) {}) {
		t.Fatal("队列已满时静默放行")
	}
	close(exec.release)
}
