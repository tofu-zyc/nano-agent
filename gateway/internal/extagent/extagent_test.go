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

// 读入时每一项再校验：不通过的不能用、原样留在文件里，其余照旧；同名重新登记或者删掉才去掉。
func TestOpenValidatesEachEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "external-agents.json")
	hand := `[
  {"name":"claude","argv":["claude","-p","{prompt}"],"dirs":["~/.claude"]},
  {"name":"codex","argv":["codex","exec"],"dirs":["~/.codex"]},
  {"name":"claude","argv":["claude","{prompt}"]},
  {"name":"evil","argv":["sh","{prompt}"],"dirs":["~/"]}
]`
	if err := os.WriteFile(path, []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if l := s.List(); len(l) != 1 || l[0].Name != "claude" || len(l[0].Argv) != 3 {
		t.Fatalf("只有校验过的能用：%+v", l)
	}
	if _, ok := s.Get("codex"); ok {
		t.Fatal("没有 {prompt} 的不能用")
	}
	probs := s.Problems()
	if len(probs) != 3 || !s.Has("evil") || !s.Has("codex") {
		t.Fatalf("没通过的记下来：%+v", probs)
	}
	// 改别的登记：没通过的原样留在文件里
	other := good()
	other.Name = "aider"
	if err := s.Put(other); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"codex"`) || !strings.Contains(string(b), `"evil"`) {
		t.Fatalf("没通过的留在文件里：%s", b)
	}
	// 同名重新登记换掉它；删掉也能删
	fixed := good()
	fixed.Name = "codex"
	if err := s.Put(fixed); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("evil"); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if l := s2.List(); len(l) != 3 {
		t.Fatalf("claude、aider、codex：%+v", l)
	}
	if p := s2.Problems(); len(p) != 1 || p[0].Name != "claude" {
		t.Fatalf("只剩重名的那项：%+v", p)
	}
	if b := []byte("不是 JSON"); os.WriteFile(path, b, 0o600) == nil {
		if _, err := Open(path); err == nil {
			t.Fatal("整个文件读不出来是错误")
		}
	}
}

func TestSameGrant(t *testing.T) {
	a, b := good(), good()
	b.Timeout = 99
	if !SameGrant(a, b) {
		t.Fatal("时限不算")
	}
	for what, f := range map[string]func(*Agent){
		"命令": func(x *Agent) { x.Argv = append(x.Argv, "--yolo") },
		"目录": func(x *Agent) { x.Dirs = []string{"~/.other"} },
		"环境": func(x *Agent) { x.Env = nil },
	} {
		c := good()
		f(&c)
		if SameGrant(a, c) {
			t.Errorf("改了%s要算变化", what)
		}
	}
}
