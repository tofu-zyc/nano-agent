package experience

import (
	"fmt"
	"nano-agent/gateway/internal/knowledge"
	"os"
	"strings"
	"testing"
	"time"
)

func TestExperiencePermissionsResetAndReach(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s, _ := Open(t.TempDir(), "default")
	for _, e := range []Entry{
		{Kind: "chat", SID: "main", Visible: "admin", Said: "private original", Status: "ok"},
		{Kind: "lifecycle", SID: "main", Visible: "admin", Said: "reset", Status: "ok"},
		{Kind: "life", Visible: "all", Said: "public browsing", Status: "ok"},
		{Kind: "reach", SID: "main", Visible: "admin", Said: "proactive", Episode: "r-episode", Status: "ok"},
		{Kind: "chat", SID: "guest", Visible: "fake:guest", Said: "guest original", Status: "ok"},
	} {
		if err := s.Append(e, now); err != nil {
			t.Fatal(err)
		}
	}
	rows := s.Recent(knowledge.Viewer{Admin: true}, "main", 12, now)
	if len(rows) != 3 {
		t.Fatalf("current chat leaked/reset history reintroduced: %+v", rows)
	}
	if got := Render(rows, time.UTC, 3000); strings.Contains(got, "private original") || !strings.Contains(got, "r-episode") {
		t.Fatal(got)
	}
	if rows := s.Recent(knowledge.Viewer{Group: true}, "group", 12, now); len(rows) != 1 || rows[0].Visible != "all" {
		t.Fatalf("group leak: %+v", rows)
	}
	if rows := s.Recent(knowledge.Viewer{Who: "fake:guest"}, "other", 12, now); len(rows) != 2 {
		t.Fatalf("guest view: %+v", rows)
	}
}

func TestExperienceBoundedMergeCheckpointAndRecovery(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s, _ := Open(dir, "default")
	for i := 0; i < 10; i++ {
		if err := s.Append(Entry{Kind: "chat", SID: "group", Visible: "all", Did: []string{"WebFetch"}, Status: "ok", Sources: []string{fmt.Sprint("run", i)}}, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.entries) != 1 || len(s.entries[0].Sources) != 10 {
		t.Fatal("chat merge failed")
	}
	for i := 0; i < 1100; i++ {
		if err := s.Append(Entry{Kind: "life", Visible: "admin", Said: strings.Repeat("x", 1000), Status: "ok"}, now); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.entries) > MaxEntries || s.records > MaxEntries*2 {
		t.Fatal("unbounded log")
	}
	seq := s.seq
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{partial")
	_ = f.Close()
	s, err = Open(dir, "default")
	if err != nil {
		t.Fatal(err)
	}
	if s.seq != seq {
		t.Fatal("lost highwater")
	}
	if err = s.Append(Entry{Kind: "life", Visible: "admin", Said: "after recovery", Status: "ok"}, now); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, "default")
	if err != nil || s.seq != seq+1 {
		t.Fatalf("tail/reappend: %d %v", s.seq, err)
	}
	if s.entries[len(s.entries)-1].ID != fmt.Sprintf("x-%d", seq+1) {
		t.Fatal("reused entry id")
	}
}

func TestExperienceCorruptionFreezesAppend(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	s, _ := Open(dir, "default")
	if err := s.Append(Entry{Kind: "life", Visible: "admin", Status: "ok"}, now); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString("broken\n")
	_ = f.Close()
	s, err := Open(dir, "default")
	if err == nil {
		t.Fatal("corruption hidden")
	}
	if err := s.Append(Entry{Kind: "chat", Status: "ok"}, now); err == nil {
		t.Fatal("appended past corruption")
	}
}
