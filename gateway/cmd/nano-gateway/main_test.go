package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicURLHidesPassword(t *testing.T) {
	public := publicURL("http://127.0.0.1:8080/#k=secret")
	if public != "http://127.0.0.1:8080/" || strings.Contains(public, "secret") {
		t.Fatalf("public URL: %q", public)
	}
}

func TestWritePrivateTightensMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.url")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(path, "http://127.0.0.1/#k=secret\n"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
}
