package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPromotionSupersededRevisionCannotReplanOnLaterAccess(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:revoked-import"
	setupPendingPromotion(t, s, sid, `{"role":"user","content":"old private body"}`+"\n")
	if _, err := s.bumpRev(sid); err != nil {
		t.Fatal(err)
	}
	s, _ = New(dir)
	if err := s.ResumePromotions(); err == nil {
		t.Fatal("expected invalidation")
	}
	s, _ = New(dir)
	if err := s.ResumePromotions(); err != nil {
		t.Fatal(err)
	}
	if len(s.History(sid)) != 0 {
		t.Fatal("revoked body became readable")
	}
	_ = s.Settings(sid)
	if _, err := s.Migrate(); err == nil {
		t.Fatal("migration regained old import authority")
	}
	if err := s.Append(sid, Msg{Role: "user", Content: "new context"}); err != nil {
		t.Fatal(err)
	}
	s, _ = New(dir)
	if h := s.History(sid); len(h) != 1 || h[0].Content != "new context" {
		t.Fatalf("recreated context: %+v", h)
	}
}

func TestPromotionMissingSourcesCannotExposePartialTarget(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:partial-import"
	src := s.legacyPath(sid, ".jsonl")
	writeFile(t, src, `{"role":"user","content":"uncommitted copy"}`+"\n")
	writeFile(t, s.legacyPath(sid, ".settings.json"), `{"title":"original"}`)
	if err := os.MkdirAll(s.newPath(sid, ".settings.json")+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.promote(sid); err == nil {
		t.Fatal("expected partial copy")
	}
	for _, suffix := range []string{".jsonl", ".settings.json"} {
		if err := os.Remove(s.legacyPath(sid, suffix)); err != nil {
			t.Fatal(err)
		}
	}
	s, _ = New(dir)
	if err := s.ResumePromotions(); err == nil {
		t.Fatal("expected missing-source conflict")
	}
	if len(s.History(sid)) != 0 {
		t.Fatal("exposed partial target")
	}
	if err := s.Append(sid, Msg{Role: "user", Content: "new"}); err == nil {
		t.Fatal("append activated partial target")
	}
	if err := s.SaveSettings(sid, Settings{Title: "new"}); err == nil {
		t.Fatal("settings activated partial target")
	}
	s, _ = New(dir)
	if err := s.ResumePromotions(); err == nil {
		t.Fatal("conflict became committed")
	}
	op, has, err := s.readPromoteOp(sid)
	if err != nil || !has || op.Stage != "conflict" {
		t.Fatalf("lost conflict: %+v %v", op, err)
	}
	if _, err := os.Stat(s.activeMarker(sid)); !os.IsNotExist(err) {
		t.Fatalf("published partial target: %v", err)
	}
}

func TestResumePromotionReportsSettlementFailure(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "superseded", true: "committed"}[committed], func(t *testing.T) {
			dir := t.TempDir()
			s, _ := New(dir)
			sid := "tg:settlement"
			op := setupPendingPromotion(t, s, sid, `{"role":"user","content":"original"}`+"\n")
			blocked := s.promoteSettledPath(op) + ".tmp"
			if err := os.MkdirAll(blocked, 0700); err != nil {
				t.Fatal(err)
			}
			if committed {
				if err := os.MkdirAll(filepath.Dir(s.activeMarker(sid)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := writeFileAtomic(s.activeMarker(sid), []byte(Key(sid))); err != nil {
					t.Fatal(err)
				}
				if err := s.Append(sid, Msg{Role: "user", Content: "accepted later"}); err != nil {
					t.Fatal(err)
				}
			} else if err := s.Delete(sid); err != nil {
				t.Fatal(err)
			}
			s, _ = New(dir)
			if err := s.ResumePromotions(); err == nil {
				t.Fatal("settlement error swallowed")
			}
			if _, err := os.Stat(s.promoteOpPath(sid)); err != nil {
				t.Fatal("lost recovery record")
			}
			if err := os.Remove(blocked); err != nil {
				t.Fatal(err)
			}
			s, _ = New(dir)
			if err := s.ResumePromotions(); err != nil {
				t.Fatal(err)
			}
			if got := settledPromoteOp(t, s); got.OperationID != op.OperationID {
				t.Fatal("changed operation")
			}
			if committed {
				if h := s.History(sid); len(h) != 2 || h[1].Content != "accepted later" {
					t.Fatalf("lost accepted append: %+v", h)
				}
			}
		})
	}
}
