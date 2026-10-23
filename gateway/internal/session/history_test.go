package session

import (
	"bytes"
	"nano-agent/gateway/internal/llm"
	"os"
	"strings"
	"testing"
	"time"
)

func TestElideArchiveDelete(t *testing.T) {
	s, _ := New(t.TempDir())
	sid := "main"
	for i := 0; i < 6; i++ {
		for _, m := range []Msg{{Role: "user", Content: "question"}, {Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "r", Function: llm.FunctionCall{Name: "Read", Arguments: `{"path":"a.go"}`}}}}, {Role: "tool", ToolCallID: "r", Content: strings.Repeat("x", 12000)}, {Role: "assistant", Content: "answer"}} {
			if err := s.Append(sid, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	raw, _ := os.ReadFile(s.file(sid))
	if err := s.Elide(sid, 4); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(s.file(sid))
	if !bytes.Equal(raw, after) || !strings.Contains(s.EffectiveHistory(sid)[2].Content, "已省略") || len(s.EffectiveHistory(sid)[10].Content) != 12000 {
		t.Fatal("placeholder damaged journal or protected turns")
	}
	st := s.Settings(sid)
	st.Base = 8
	st.Meter = Meter{Position: 20}
	_ = s.SaveSettings(sid, st)
	delete(s.hist, sid)
	if err := s.DeleteTurns(sid, 1, 2); err != nil {
		t.Fatal(err)
	}
	st = s.Settings(sid)
	if st.Base != 4 || st.Meter.Position != 0 || len(s.Archive(sid)) != 16 || len(s.History(sid)) != 12 {
		t.Fatal(st, len(s.History(sid)))
	}
	if err := s.ArchiveReset(sid, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(s.Archives(sid)) != 1 || len(s.History(sid)) != 0 {
		t.Fatal("reset did not archive")
	}
	if err := s.DeleteArchive(sid, "../bad.jsonl"); err == nil {
		t.Fatal("unsafe path")
	}
	arc := s.Archives(sid)[0]
	old := time.Now().AddDate(0, 0, -91)
	_ = os.Chtimes(s.archiveDir(sid)+"/"+arc.ID, old, old)
	if err := s.PruneArchives(time.Now(), 90); err != nil || len(s.Archives(sid)) != 0 {
		t.Fatal(err)
	}
	_ = s.Append(sid, Msg{Role: "user", Content: "delete"})
	if err := s.DeleteAllHistory(sid); err != nil {
		t.Fatal(err)
	}
	if len(s.Archive(sid)) != 0 {
		t.Fatal("deleted history remains")
	}
}
