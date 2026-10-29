package hooks

import (
	"context"
	"encoding/json"
	"path/filepath"
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
