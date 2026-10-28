package session

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuxiliaryCanonicalKeyLifecycleAndPreservedBackup(t *testing.T) {
	for _, kind := range []string{"interjections", "persona"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := New(t.TempDir())
			root := filepath.Join(s.Dir(), kind)
			ext, body := ".jsonl", "{\"v\":1,\"kind\":\"received\",\"id\":\"one\"}\n"
			if kind == "persona" {
				ext, body = ".md", "私有人格"
			}
			sid := "tg:中文/" + strings.Repeat("long-id", 20)
			if err := s.Append(sid, Msg{Role: "user", Content: "context"}); err != nil {
				t.Fatal(err)
			}
			legacy := filepath.Join(root, url.PathEscape(sid)+ext)
			writeFile(t, legacy, body)
			path, exists, err := s.AuxiliaryPath(kind, root, ext, sid, true)
			if err != nil || !exists {
				t.Fatalf("migration: %v %v", exists, err)
			}
			if strings.ContainsAny(filepath.Base(path), ":/\\") {
				t.Fatal("unsafe canonical key")
			}
			if b, err := os.ReadFile(path); err != nil || string(b) != body {
				t.Fatal("wrong migrated body")
			}
			if b, err := os.ReadFile(legacy); err != nil || string(b) != body {
				t.Fatal("legacy source removed")
			}
			files, _ := filepath.Glob(filepath.Join(s.dirV1(), "legacy-backups", kind, "*", "content"+ext))
			if len(files) != 1 {
				t.Fatal("missing visible backup")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			s, _ = New(s.Dir())
			if _, exists, err := s.AuxiliaryPath(kind, root, ext, sid, false); err != nil || exists {
				t.Fatal("deleted canonical view fell back to backup")
			}
			if err := s.Delete(sid); err != nil {
				t.Fatal(err)
			}
			if _, exists, err := s.AuxiliaryPath(kind, root, ext, sid, false); err != nil || exists {
				t.Fatal("deleted session aux readable")
			}
			if err := s.Append(sid, Msg{Role: "user", Content: "new generation"}); err != nil {
				t.Fatal(err)
			}
			if _, exists, err := s.AuxiliaryPath(kind, root, ext, sid, false); err != nil || exists {
				t.Fatal("new generation inherited old auxiliary")
			}
			if path, exists, err := s.AuxiliaryPath(kind, root, ext, sid, true); err != nil || exists {
				t.Fatalf("new empty generation: %q %v %v", path, exists, err)
			}
		})
	}
}

func TestAuxiliaryInterruptedCopyCannotBeActivatedByOrdinaryWrite(t *testing.T) {
	s, _ := New(t.TempDir())
	root := filepath.Join(s.Dir(), "persona")
	sid := "tg:partial"
	writeFile(t, filepath.Join(root, url.PathEscape(sid)+".md"), "original")
	manifest := filepath.Join(s.dirV1(), "assets", "persona", Key(sid)+".json")
	// Block the first manifest commit; no body may be copied or accepted.
	if err := os.MkdirAll(manifest+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuxiliaryPath("persona", root, ".md", sid, true); err == nil {
		t.Fatal("manifest failure ignored")
	}
	path := AuxiliaryFile(root, sid, ".md")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("copied before ownership was durable")
	}
	if err := os.Remove(manifest + ".tmp"); err != nil {
		t.Fatal(err)
	}
	// Unknown canonical bytes cannot become active simply because a legacy
	// source is absent or an ordinary writer asks for the path.
	writeFile(t, path, "unowned copy")
	if err := os.Remove(filepath.Join(root, url.PathEscape(sid)+".md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuxiliaryPath("persona", root, ".md", sid, true); err == nil {
		t.Fatal("unowned target published")
	}
	if b, _ := os.ReadFile(path); string(b) != "unowned copy" {
		t.Fatal("conflict overwritten")
	}
}

func TestAuxiliaryWholeInstallationMoveKeepsPublishedIdentity(t *testing.T) {
	parent := t.TempDir()
	old := filepath.Join(parent, "old")
	s, _ := New(filepath.Join(old, "data"))
	root := filepath.Join(old, "personas")
	sid := "tg:移动🚀"
	writeFile(t, filepath.Join(root, url.PathEscape(sid)+".md"), "legacy")
	path, _, err := s.AuxiliaryPath("persona", root, ".md", sid, true)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "edited after import")
	next := filepath.Join(parent, "new")
	if err := os.Rename(old, next); err != nil {
		t.Fatal(err)
	}
	s, _ = New(filepath.Join(next, "data"))
	path, exists, err := s.AuxiliaryPath("persona", filepath.Join(next, "personas"), ".md", sid, false)
	if err != nil || !exists {
		t.Fatal("portable published root lost", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "edited after import" {
		t.Fatal("reimported stale legacy content")
	}
}
