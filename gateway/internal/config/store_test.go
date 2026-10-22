package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConf(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gateway.toml")
	os.WriteFile(p, []byte(`
[kernel]
token = "k"
[llm]
provider = "openai"
base_url = "http://x/v1"
api_key = "sk-file"
model = "m1"
[cli]
enabled = true
`), 0o600)
	return p
}

func find(st []FieldState, key string) FieldState {
	for _, f := range st {
		if f.Key == key {
			return f
		}
	}
	return FieldState{}
}

func TestStoreOverlay(t *testing.T) {
	path := writeConf(t)
	s, warns, err := Open(path)
	if err != nil || len(warns) != 0 {
		t.Fatalf("open: %v %v", err, warns)
	}
	var changes int
	s.OnChange(func(old, cur *Config) { changes++ })

	if err := s.Set(map[string]any{"llm.timeout": float64(5000), "workspace.mode": "auto", "access.admins": []any{" tg:1 ", ""},
		"llm.send_reasoning": true}); err != nil {
		t.Fatal(err)
	}
	c := s.Current()
	if c.LLM.Timeout != 5000 || c.Workspace.Mode != "auto" || len(c.Access.Admins) != 1 || c.Access.Admins[0] != "tg:1" ||
		!c.LLM.SendReasoning || changes != 1 {
		t.Fatalf("after set: %+v changes=%d", c, changes)
	}

	d := s.Describe()
	if f := find(d, "llm.timeout"); f.Value != float64(5000) || f.Source != "web" {
		t.Fatalf("timeout: %+v", f)
	}
	// 重新打开：网页的修改还在
	s2, _, _ := Open(path)
	if s2.Current().LLM.Timeout != 5000 {
		t.Fatalf("overlay not persisted")
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "data", "config.json"))
	if strings.Contains(string(b), "sk-file") {
		t.Fatalf("overlay 里不该有没改过的密钥：%s", b)
	}

	// 恢复成配置文件的值
	s.Set(map[string]any{"llm.timeout": nil})
	if s.Current().LLM.Timeout != 600 || find(s.Describe(), "llm.timeout").Source != "default" {
		t.Fatalf("revert failed: %+v", find(s.Describe(), "llm.timeout"))
	}

	// 非法的值整批拒绝
	err = s.Set(map[string]any{"workspace.mode": "yolo", "llm.timeout": float64(1000)})
	if err == nil || s.Current().LLM.Timeout != 600 {
		t.Fatalf("invalid enum: %v", err)
	}
	// 模型相关的旧设置项已经不在网页设置里
	if err := s.Set(map[string]any{"llm.model": "m3"}); err == nil {
		t.Fatalf("llm.model 不该还能从网页改")
	}
	if err := s.Set(map[string]any{"llm.modalities": []any{"image"}}); err == nil {
		t.Fatalf("llm.modalities 不该还能从网页改")
	}
	if err := s.Set(map[string]any{"session.data_dir": "/tmp"}); err == nil {
		t.Fatalf("readonly accepted")
	}
	if err := s.Set(map[string]any{"llm.timeout": 1.5}); err == nil {
		t.Fatalf("float for int accepted")
	}
	if err := s.Set(map[string]any{"nope.key": "x"}); err == nil {
		t.Fatalf("unknown key accepted")
	}

	// 要重启才生效的配置项
	s.Set(map[string]any{"kernel.listen": "tcp:127.0.0.1:7701"})
	if f := find(s.Describe(), "kernel.listen"); !f.Pending {
		t.Fatalf("pending: %+v", f)
	}
}
