package app

import (
	"encoding/json"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
	"strings"
	"testing"
	"time"
)

func TestSessionForkCommandDoesNotDeadlock(t *testing.T) {
	e := setup(t)
	e.say("hello")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	done := make(chan struct{})
	go func() { e.say("/session fork 命令分支"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fork command acquired App lock twice")
	}
	e.ch.wait(t, "已创建分支")
}

func TestSessionToolCannotReadOrControlOtherUsersContexts(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "admin secret"}); err != nil {
		t.Fatal(err)
	}
	adminBranch, err := e.a.sessionForkLocked(SessionForkRequest{SourceSID: mainSID, By: "fake:u1", RequestID: "same-id"})
	if err != nil {
		t.Fatal(err)
	}
	s := e.a.sessOf("guest-private")
	s.run = &run{from: "fake:guest"}
	call := func(args map[string]any) (string, bool) {
		b, _ := json.Marshal(args)
		return e.a.handleSessionTool(s, &proto.Msg{Args: b})
	}
	for _, action := range []string{"fork", "list_branches"} {
		if out, ok := call(map[string]any{"action": action, "source_sid": mainSID, "request_id": "same-id"}); ok {
			t.Fatalf("%s disclosed admin source: %s", action, out)
		}
	}
	for _, action := range []string{"inspect", "continue", "pause", "archive"} {
		if out, ok := call(map[string]any{"action": action, "branch_sid": adminBranch.BranchSID, "prompt": "continue"}); ok {
			t.Fatalf("%s allowed cross-user control: %s", action, out)
		}
	}
	if err := e.a.store.Append(s.sid, session.Msg{Role: "user", Content: "guest context"}); err != nil {
		t.Fatal(err)
	}
	out, ok := call(map[string]any{"action": "fork", "request_id": "same-id", "delivery": "report"})
	if !ok || strings.Contains(out, adminBranch.BranchSID) {
		t.Fatalf("own-source access/idempotency wrong: %q %v", out, ok)
	}
	for _, br := range e.a.store.Children(s.sid) {
		if br.CreatedBy != "fake:guest" || br.Delivery != session.BranchDeliverySilent {
			t.Fatalf("forged privilege/delivery: %+v", br)
		}
	}
	s.run = nil
	e.a.mu.Unlock()
}
