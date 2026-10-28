//go:build windows

package app

import (
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/session"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsWorkspaceUnicodeAndContainment(t *testing.T) {
	root := t.TempDir()
	store, err := session.New(filepath.Join(root, "自己 数据🚀"))
	if err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: &config.Config{Dir: filepath.Join(root, "配置")}, store: store}
	if err := a.checkWorkspace(host.LocalName, filepath.Join(root, "用户 工作🚀")); err != nil {
		t.Fatal("Windows absolute Unicode path rejected", err)
	}
	for _, path := range []string{root, strings.ToUpper(store.Dir())} {
		if got := a.checkDir(path); got == "" {
			t.Fatalf("containing path accepted in auto: %s", path)
		}
	}
	if got := a.checkDir(store.Dir() + "-sibling"); got != "" {
		t.Fatal("sibling confused with containing path", got)
	}
}
