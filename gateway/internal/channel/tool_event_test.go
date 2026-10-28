package channel

import (
	"fmt"
	"strings"
	"testing"
)

func TestToolEventsAssociateOutOfOrderAndReusedCallIDs(t *testing.T) {
	r := &streamRun{wake: make(chan struct{}, 1)}
	a := ToolEvent{RunID: "run", TurnID: 1, CallID: "same", Name: "Read", State: "accepted", Detail: "a.txt"}
	b := ToolEvent{RunID: "run", TurnID: 1, CallID: "second", Name: "Shell", State: "accepted", Detail: "command"}
	EmitToolEvent(FilterView(r, "过程"), a)
	r.ToolEvent(b)
	b.State, b.OK, b.Detail = "done", false, "shell failure"
	r.ToolEvent(b)
	snap := r.snapshot()
	if snap.Tools[0].Done || !snap.Tools[1].Done || snap.Tools[1].Result != "shell failure" {
		t.Fatal("out of order result paired by position", snap.Tools)
	}
	if !strings.Contains(snap.status(), "Read") {
		t.Fatal("finished last entry hid running first entry", snap.status())
	}
	a.State, a.OK, a.Detail = "done", true, "read result"
	r.ToolEvent(a)
	// The model may reuse a call ID in another turn, and a fresh run may
	// reuse both. All three are separate entries.
	next := a
	next.TurnID = 2
	next.State = "accepted"
	next.Detail = "next.txt"
	r.ToolEvent(next)
	newer := next
	newer.RunID = "next-run"
	r.ToolEvent(newer)
	next.State, next.Detail = "done", "turn two"
	r.ToolEvent(next)
	a.Detail = "late duplicate"
	r.ToolEvent(a)
	stale := a
	stale.RunID = "unknown"
	r.ToolEvent(stale)
	snap = r.snapshot()
	if len(snap.Tools) != 4 || snap.Tools[0].Result != "read result" || snap.Tools[2].Result != "turn two" || snap.Tools[3].Done {
		t.Fatal("identity/terminal event overwritten", snap.Tools)
	}
	for _, text := range []string{telegramProcess(snap), feishuTools(snap)} {
		if !strings.Contains(text, "shell failure") || !strings.Contains(text, "turn two") || strings.Contains(text, "late duplicate") {
			t.Fatal("platform did not render exact calls", text)
		}
	}
}

func TestToolEventBuffersStayBoundedAndKeepActiveCall(t *testing.T) {
	r := &streamRun{wake: make(chan struct{}, 1)}
	active := ToolEvent{RunID: "run", TurnID: 1, CallID: "active", Name: "Shell", State: "accepted"}
	r.ToolEvent(active)
	for i := range 4000 {
		event := ToolEvent{RunID: "run", TurnID: i + 2, CallID: fmt.Sprint(i), Name: "Read", State: "accepted"}
		r.ToolEvent(event)
		event.State, event.OK = "done", true
		r.ToolEvent(event)
		r.Note(strings.Repeat("note", 1000))
		r.Content(strings.Repeat("字", 1000))
		r.Reasoning(strings.Repeat("思", 1000))
	}
	snap := r.snapshot()
	if len(snap.Tools) > 128 || snap.Tools[0].CallID != "active" || len(r.notes) > 32 || len(snap.Content) > streamTextLimit || len(snap.Reasoning) > streamTextLimit {
		t.Fatal("unbounded platform state or active entry dropped")
	}
	active.State, active.OK, active.Detail = "done", true, "still correlated"
	r.ToolEvent(active)
	if r.snapshot().Tools[0].Result != "still correlated" {
		t.Fatal("active call lost identity")
	}
}
