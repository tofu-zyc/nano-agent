package extagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func good() Agent {
	return Agent{Name: "claude", Argv: []string{"claude", "-p", PromptSlot}, Dirs: []string{"~/.claude"}, Env: map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude"}}
}

func TestValidate(t *testing.T) {
	if err := good().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Agent){
		"名字大写":        func(a *Agent) { a.Name = "Claude" },
		"名字太长":        func(a *Agent) { a.Name = "a" + strings.Repeat("b", 32) },
		"第一项空":        func(a *Agent) { a.Argv[0] = " " },
		"没有 prompt":   func(a *Agent) { a.Argv = []string{"claude", "-p"} },
		"prompt 两次":   func(a *Agent) { a.Argv = []string{"claude", PromptSlot, PromptSlot} },
		"prompt 嵌在里面": func(a *Agent) { a.Argv = []string{"claude", "--prompt=" + PromptSlot} },
		"prompt 第一项":  func(a *Agent) { a.Argv = []string{PromptSlot, "x"} },
		"目录不是 ~/":     func(a *Agent) { a.Dirs = []string{"/root/.claude"} },
		"目录是家目录":      func(a *Agent) { a.Dirs = []string{"~/"} },
		"目录是家目录 2":    func(a *Agent) { a.Dirs = []string{"~//"} },
		"目录含 ..":      func(a *Agent) { a.Dirs = []string{"~/.claude/../.ssh"} },
		"env 改 HOME":  func(a *Agent) { a.Env = map[string]string{"HOME": "~/x"} },
		"env 改 PATH":  func(a *Agent) { a.Env = map[string]string{"PATH": "/bin"} },
		"env LD_":     func(a *Agent) { a.Env = map[string]string{"LD_PRELOAD": "x"} },
		"env NANO_":   func(a *Agent) { a.Env = map[string]string{"NANO_X": "x"} },
		"env 小写":      func(a *Agent) { a.Env = map[string]string{"home": "x"} },
		"env 值 NUL":   func(a *Agent) { a.Env = map[string]string{"A": "a\x00b"} },
		"时限太大":        func(a *Agent) { a.Timeout = MaxTimeout + 1 },
	}
	for what, f := range bad {
		a := good()
		f(&a)
		if err := a.Validate(); err == nil {
			t.Errorf("%s 应该被拒", what)
		}
	}
}

func TestStorePersistsAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "external-agents.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(good()); err != nil {
		t.Fatal(err)
	}
	b := good()
	b.Timeout = 60
	if err := s.Put(b); err != nil {
		t.Fatal(err)
	}
	bad := good()
	bad.Name = "codex"
	bad.Dirs = []string{"/etc"}
	if err := s.Put(bad); err == nil {
		t.Fatal("校验不过的不存")
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if l := s2.List(); len(l) != 1 || l[0].Timeout != 60 || l[0].EffectiveTimeout() != 60 {
		t.Fatalf("按名字替换、写盘：%+v", l)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("文件权限：%v", st.Mode())
	}
	if err := s2.Delete("claude"); err != nil || len(s2.List()) != 0 {
		t.Fatal(err)
	}
	if err := s2.Delete("claude"); err == nil {
		t.Fatal("没有的删不了")
	}
}
