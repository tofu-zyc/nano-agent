package session

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRenameAuxiliaryCommitRecoveryAndLifecycle(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "reset-revokes"}[reset], func(t *testing.T) {
			s, _ := New(t.TempDir())
			from, to := "tg:source", "tg:target"
			if err := s.Append(from, Msg{Role: "user", Content: "body"}); err != nil {
				t.Fatal(err)
			}
			for _, def := range []struct{ kind, ext, body string }{{"persona", ".md", "persona"}, {"interjections", ".jsonl", "{\"kind\":\"received\"}\n"}} {
				root := filepath.Join(s.Dir(), def.kind)
				path, _, err := s.AuxiliaryPath(def.kind, root, def.ext, from, true)
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, path, def.body)
			}
			// Fail exactly after target copies are written but before the source
			// tombstone. Normal access cannot publish the staged auxiliary views.
			blocker := s.deletedMarker(from) + ".tmp"
			if err := os.MkdirAll(blocker, 0700); err != nil {
				t.Fatal(err)
			}
			if err := s.Rename(from, to); err == nil {
				t.Fatal("commit failure ignored")
			}
			root := filepath.Join(s.Dir(), "persona")
			if _, _, err := s.AuxiliaryPath("persona", root, ".md", to, true); err == nil {
				t.Fatal("ordinary write published pending rename")
			}
			if err := os.Remove(blocker); err != nil {
				t.Fatal(err)
			}
			if reset {
				if err := s.Reset(to); err != nil {
					t.Fatal(err)
				}
			}
			s, _ = New(s.Dir())
			if err := s.ResumeRenames(); err != nil {
				t.Fatal(err)
			}
			path, exists, err := s.AuxiliaryPath("persona", root, ".md", to, false)
			if err != nil {
				t.Fatal(err)
			}
			if reset {
				if exists {
					t.Fatal("reset target imported old persona")
				}
				if _, exists, err := s.AuxiliaryPath("persona", root, ".md", to, true); err != nil || exists {
					t.Fatal("new write reactivated old copy", err)
				}
				if !s.Exists(from) {
					t.Fatal("superseded rename removed source")
				}
			} else {
				if !exists {
					t.Fatal("committed persona missing")
				}
				if b, _ := os.ReadFile(path); string(b) != "persona" {
					t.Fatal("persona changed")
				}
				if s.Exists(from) {
					t.Fatal("renamed source still active")
				}
				if !slices.Contains(s.List(), to) {
					t.Fatal("new ID not listed")
				}
				if path, exists, err := s.AuxiliaryPath("interjections", filepath.Join(s.Dir(), "interjections"), ".jsonl", to, false); err != nil || !exists {
					t.Fatal("journal missing", path, err)
				}
			}
		})
	}
}

func TestAuxiliaryOnlySessionInventoryRequiresPublishedBody(t *testing.T) {
	s, _ := New(t.TempDir())
	root := filepath.Join(s.Dir(), "persona")
	path, _, err := s.AuxiliaryPath("persona", root, ".md", "persona-only", true)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(s.List(), "persona-only") {
		t.Fatal("empty auxiliary record created ghost session")
	}
	writeFile(t, path, "actual persona")
	if !slices.Contains(s.List(), "persona-only") || !s.Exists("persona-only") {
		t.Fatal("auxiliary-only session undiscoverable")
	}
	if err := s.Delete("persona-only"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(s.List(), "persona-only") {
		t.Fatal("deleted auxiliary resurrected session")
	}
}
