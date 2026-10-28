package session

import (
	"fmt"
	"nano-agent/gateway/internal/llm"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadyBranchSettingsFailureDoesNotPublish(t *testing.T) {
	s, _ := New(t.TempDir())
	_ = s.Append("main", Msg{Role: "user", Content: "source"})
	st := s.Settings("main")
	snap, err := s.WriteSnapshot("main", s.SnapshotIDFor("main", st), st)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BeginBranch(BranchMeta{SID: "branch", ParentSID: "main"}); err != nil {
		t.Fatal(err)
	}
	blocked := s.settingsFile("branch") + ".tmp"
	if err := os.MkdirAll(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadyBranch("branch", snap, BranchReady); err == nil {
		t.Fatal("settings failure ignored")
	}
	if br, _ := s.Branch("branch"); br.Status == BranchReady || br.SnapshotID != "" {
		t.Fatalf("ready published early: %+v", br)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadyBranch("branch", snap, BranchReady); err != nil {
		t.Fatal(err)
	}
}

func forkStore(t *testing.T, s *Store, parent, sid string) SnapshotInfo {
	t.Helper()
	st := s.Settings(parent)
	snap, err := s.WriteSnapshot(parent, s.SnapshotIDFor(parent, st), st)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BeginBranch(BranchMeta{SID: sid, ParentSID: parent, OwnerAgent: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadyBranch(sid, snap, BranchReady); err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestForkPreservesCompactedEffectiveContext(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	for i := 0; i < 8; i++ {
		role := "user"
		if i%2 != 0 {
			role = "assistant"
		}
		if err := s.Append("main", Msg{Role: role, Content: fmt.Sprint(i), Agent: "default", ID: fmt.Sprint("m", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CommitCompact("main", 2, "summary at cutoff", time.Now()); err != nil {
		t.Fatal(err)
	}
	st := s.Settings("main")
	st.Omitted = map[int]string{3: "omitted 3"}
	if err := s.SaveSettings("main", st); err != nil {
		t.Fatal(err)
	}
	forkStore(t, s, "main", "branch")
	want := s.EffectiveHistory("main")
	s, _ = New(dir)
	if got := s.EffectiveHistory("branch"); !reflect.DeepEqual(got, want) {
		t.Fatalf("compacted context mismatch: %+v / %+v", got, want)
	}
	if s.Settings("branch").Summary != st.Summary || len(s.Archive("branch")) != 8 {
		t.Fatal("lost summary/raw provenance")
	}
}

func TestForkOfBranchPreservesSnapshotAndTail(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	for i := 0; i < 3; i++ {
		if err := s.Append("main", Msg{Role: "user", Content: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	forkStore(t, s, "main", "first")
	if err := s.Append("first", Msg{Role: "user", Content: "tail"}); err != nil {
		t.Fatal(err)
	}
	want := s.History("first")
	forkStore(t, s, "first", "nested")
	s, _ = New(dir)
	if got := s.History("nested"); !reflect.DeepEqual(got, want) {
		t.Fatalf("nested source lost: %+v / %+v", got, want)
	}
	if _, err := s.Undo("first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Undo("first"); err != nil {
		t.Fatal("inherited undo:", err)
	}
	if err := s.Append("first", Msg{Role: "user", Content: "replacement"}); err != nil {
		t.Fatal(err)
	}
	forkStore(t, s, "first", "after-undo")
	s, _ = New(dir)
	if h := s.History("after-undo"); len(h) != 3 || h[2].Content != "replacement" {
		t.Fatalf("undo resurrected inherited prefix: %+v", h)
	}
	if len(s.History("nested")) != 4 {
		t.Fatal("source undo mutated existing child")
	}
	if err := s.Reset("nested"); err != nil {
		t.Fatal(err)
	}
	s, _ = New(dir)
	if len(s.History("nested")) != 0 {
		t.Fatal("reset inherited content returned")
	}
}

func TestForkSnapshotVersionIncludesSettingsAndOmissions(t *testing.T) {
	s, _ := New(t.TempDir())
	_ = s.Append("main", Msg{Role: "user", Content: "fixed bytes"})
	st := s.Settings("main")
	first := s.SnapshotIDFor("main", st)
	for _, edit := range []func(*Settings){func(s *Settings) { s.Model = "B" }, func(s *Settings) { s.Thinking = "high" }, func(s *Settings) { s.Workspaces = []string{"different"}; s.WorkspacesSet = true }, func(s *Settings) { s.Mode = "readonly" }, func(s *Settings) { s.Omitted = map[int]string{0: "hidden"} }, func(s *Settings) { s.SnapshotPersona = "different persona" }} {
		next := st
		edit(&next)
		if id := s.SnapshotIDFor("main", next); id == first || id == "" {
			t.Fatalf("incomplete context fingerprint: %s", id)
		}
	}
}

func TestFrozenSnapshotSurvivesSourceUndoAndReset(t *testing.T) {
	s, _ := New(t.TempDir())
	_ = s.Append("main", Msg{Role: "user", Content: "original"})
	_ = s.Append("main", Msg{Role: "assistant", Content: "finished"})
	lease, err := s.FreezeSnapshot("main", s.Settings("main"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err = s.Undo("main"); err != nil {
		t.Fatal(err)
	}
	if err = s.Reset("main"); err != nil {
		t.Fatal(err)
	}
	snap, err := lease.Write()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Messages != 2 {
		t.Fatal("destructive source edit changed frozen bytes")
	}
}

func TestSnapshotCacheAccountsForToolArguments(t *testing.T) {
	s, _ := New(t.TempDir())
	s.SetSnapshotBudget(1024)
	_ = s.Append("main", Msg{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c", Function: llm.FunctionCall{Name: "Write", Arguments: strings.Repeat("x", 64<<10)}}}})
	snap := forkStore(t, s, "main", "branch")
	_ = s.SnapshotMessages(snap.SnapshotID)
	if len(s.snapCache) != 0 {
		t.Fatal("tool arguments escaped snapshot cache budget")
	}
}
