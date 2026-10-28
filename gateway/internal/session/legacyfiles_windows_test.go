//go:build windows

package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNTFSStreamsEnumeratedBackedUpAndMigrated(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sid := "tg:中文-123"
	legacy := filepath.Join(s.dirLegacySessions(), LegacyKey(sid)+".jsonl")
	body := `{"role":"user","content":"private ADS original"}` + "\n"
	if err := os.WriteFile(legacy, []byte(body), 0600); err != nil {
		t.Fatalf("NTFS ADS fixture unavailable: %v", err)
	}
	names, err := LegacyFiles(s.dirLegacySessions())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range names {
		if name == LegacyKey(sid)+".jsonl" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ReadDir-only inventory missed ADS: %v", names)
	}
	if _, err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	s, _ = New(dir)
	if h := s.History(sid); len(h) != 1 || h[0].Content != "private ADS original" {
		t.Fatalf("ADS history missing after restart: %+v", h)
	}
	if b, err := os.ReadFile(legacy); err != nil || string(b) != body {
		t.Fatal("original stream was changed")
	}
	root := filepath.Join(dir, "interjections")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, LegacyKey(sid)+".jsonl")
	jbody := "{\"v\":1,\"kind\":\"received\",\"id\":\"stable-one\"}\n"
	if err := os.WriteFile(journal, []byte(jbody), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateAuxiliary("interjections", root, ".jsonl"); err != nil {
		t.Fatal(err)
	}
	path, exists, err := s.AuxiliaryPath("interjections", root, ".jsonl", sid, false)
	if err != nil || !exists {
		t.Fatal("ADS journal not migrated", err)
	}
	if strings.Contains(filepath.Base(path), ":") {
		t.Fatal("new business ADS created")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != jbody {
		t.Fatal("journal bytes changed")
	}
	backups, _ := filepath.Glob(filepath.Join(s.dirV1(), "legacy-backups", "interjections", "*", "content.jsonl"))
	if len(backups) != 1 {
		t.Fatal("visible stream backup missing")
	}
}

func TestNTFSLegacyOwnershipSurvivesAliasAndEndpointSpelling(t *testing.T) {
	s, _ := New(t.TempDir())
	// Two named streams share the base file's case spelling. Endpoint bindings
	// preserve each logical ID even when NTFS reports the same base spelling.
	for _, sid := range []string{"tg:one", "TG:two"} {
		writeFile(t, filepath.Join(s.dirLegacySessions(), LegacyKey(sid)+".jsonl"), "{\"role\":\"user\",\"content\":\""+sid+"\"}\n")
	}
	writeFile(t, filepath.Join(s.Dir(), "endpoints.json"), `{"first":{"session":"tg:one"},"second":{"session":"TG:two"}}`)
	if _, err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"tg:one", "TG:two"} {
		h := s.History(sid)
		if len(h) != 1 || h[0].Content != sid {
			t.Fatalf("binding identity changed: %s %+v", sid, h)
		}
	}
	s, _ = New(s.Dir())
	if err := s.Append("TG:ONE", Msg{Role: "user", Content: "alias"}); err == nil {
		t.Fatal("new operation claimed another logical ID's stream")
	}
	if len(s.History("TG:ONE")) != 0 {
		t.Fatal("alias imported old body")
	}
	if len(s.StorageIssues()) == 0 {
		t.Fatal("read recovery error invisible")
	}
}
