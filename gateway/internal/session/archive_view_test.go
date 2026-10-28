package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyArchiveListingAndReadUseSameView(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := ".."
	old := filepath.Join(s.legacyArchiveDir(sid), "old.jsonl")
	writeFile(t, old, `{"role":"user","content":"archived original"}`+"\n")
	// An existing but empty destination is not a completed migration.
	if err := os.MkdirAll(s.archiveDir(sid), 0700); err != nil {
		t.Fatal(err)
	}
	items := s.Archives(sid)
	if len(items) != 1 || items[0].Messages != 1 || s.ArchiveCount() != 1 {
		t.Fatalf("listing/count: %+v / %d", items, s.ArchiveCount())
	}
	if h := s.ReadArchive(sid, items[0].ID); len(h) != 1 || h[0].Content != "archived original" {
		t.Fatalf("read: %+v", h)
	}
	if err := s.DeleteArchive(sid, items[0].ID); err != nil {
		t.Fatal(err)
	}
	s, _ = New(dir)
	if _, err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	if len(s.Archives(sid)) != 0 || s.ArchiveCount() != 0 {
		t.Fatal("deleted archive returned from backup")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("migration backup was deleted")
	}
}

func TestArchiveConflictNeverExposesUncommittedView(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:archive-conflict"
	writeFile(t, filepath.Join(s.legacyArchiveDir(sid), "same.jsonl"), `{"role":"user","content":"old"}`+"\n")
	target := filepath.Join(s.archiveDir(sid), "same.jsonl")
	valuable := `{"role":"user","content":"valuable new"}` + "\n"
	writeFile(t, target, valuable)
	if len(s.Archives(sid)) != 0 || len(s.ReadArchive(sid, "same.jsonl")) != 0 || s.ArchiveCount() != 0 {
		t.Fatal("partial archive exposed")
	}
	if err := s.DeleteArchive(sid, "same.jsonl"); err == nil {
		t.Fatal("conflict allowed destructive archive access")
	}
	if err := s.PruneArchives(time.Now().AddDate(1, 0, 0), 1); err == nil {
		t.Fatal("pruning ignored conflict")
	}
	s, _ = New(dir)
	if b, err := os.ReadFile(target); err != nil || string(b) != valuable {
		t.Fatal("conflicting target changed")
	}
}
