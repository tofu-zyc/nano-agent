package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeBase(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "gateway.toml")
	body := `[kernel]
listen = "unix:` + filepath.Join(dir, "k.sock") + `"
token = "test-token"
local = false
[llm]
provider = "mock"
[session]
data_dir = "` + filepath.Join(dir, "data") + `"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckAcceptsValidConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Check(writeBase(t, dir)); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRejectsBadTOML(t *testing.T) {
	dir := t.TempDir()
	path := writeBase(t, dir)
	if err := os.WriteFile(path, []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Check(path); err == nil {
		t.Fatal("坏 TOML 应该报错")
	}
}

func TestCheckRejectsBadOverlay(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	path := writeBase(t, dir)
	if err := os.WriteFile(filepath.Join(data, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Check(path); err == nil {
		t.Fatal("坏 config.json 应该报错")
	}
}
