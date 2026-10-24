package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	return s
}

func TestEnsureDefault(t *testing.T) {
	s := openTemp(t)
	a, err := s.EnsureDefault("小纳", "🐱")
	if err != nil {
		t.Fatalf("建默认 agent 失败: %v", err)
	}
	if a.ID != DefaultID || a.Name != "小纳" {
		t.Fatalf("默认 agent 不对: %+v", a)
	}
	// 再调一次不会重复建
	if _, err := s.EnsureDefault("别的", ""); err != nil {
		t.Fatalf("再次调用失败: %v", err)
	}
	if n := len(s.List()); n != 1 {
		t.Fatalf("应该有 1 个 agent，实际 %d", n)
	}
	// 默认 agent 不能删
	if err := s.Delete(DefaultID); err == nil {
		t.Fatal("默认 agent 应该不能删除")
	}
}

func TestCreateUpdateDelete(t *testing.T) {
	s := openTemp(t)
	if _, err := s.EnsureDefault("小纳", ""); err != nil {
		t.Fatal(err)
	}
	steps := 30
	if err := s.Create(Agent{ID: "bob", Name: "小鲍", Emoji: "🦦", MaxSteps: &steps}); err != nil {
		t.Fatalf("新建失败: %v", err)
	}
	// 重复 ID
	if err := s.Create(Agent{ID: "bob", Name: "又一个"}); err == nil {
		t.Fatal("重复 ID 应该报错")
	}
	// 非法 ID
	for _, id := range []string{"", "Bob", "a b", "中文", "-ab"} {
		if err := s.Create(Agent{ID: id, Name: "x"}); err == nil {
			t.Fatalf("ID %q 应该被拒绝", id)
		}
	}
	// 没写名字
	if err := s.Create(Agent{ID: "noname"}); err == nil {
		t.Fatal("名字为空应该报错")
	}
	// 权限模式只能是三种
	bad := "yolo"
	if err := s.Create(Agent{ID: "modes", Name: "x", Mode: &bad}); err == nil {
		t.Fatal("非法权限模式应该报错")
	}
	ok := "readonly"
	if err := s.Create(Agent{ID: "modes", Name: "x", Mode: &ok}); err != nil {
		t.Fatalf("合法权限模式被拒绝: %v", err)
	}

	a, _ := s.Get("bob")
	a.Name = "小鲍鱼"
	if err := s.Update(a); err != nil {
		t.Fatalf("修改失败: %v", err)
	}
	if got, _ := s.Get("bob"); got.Name != "小鲍鱼" {
		t.Fatalf("改完的名字不对: %+v", got)
	}
	if err := s.Update(Agent{ID: "nobody", Name: "x"}); err == nil {
		t.Fatal("改不存在的 agent 应该报错")
	}
	if err := s.Delete("bob"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, ok := s.Get("bob"); ok {
		t.Fatal("删掉以后还能查到")
	}
}

func TestPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureDefault("小纳", "🐱"); err != nil {
		t.Fatal(err)
	}
	at := 0.5
	if err := s.Create(Agent{ID: "bob", Name: "小鲍", CompactAt: &at}); err != nil {
		t.Fatal(err)
	}
	// 重新打开：设置还在，没设的项仍然是 nil（跟随全局）
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	bob, ok := s2.Get("bob")
	if !ok {
		t.Fatal("重新打开后找不到 bob")
	}
	if bob.CompactAt == nil || *bob.CompactAt != 0.5 {
		t.Fatalf("compact_at 没保存: %+v", bob.CompactAt)
	}
	if bob.KeepTurns != nil || bob.Mode != nil || bob.MaxSteps != nil {
		t.Fatalf("没设的项应该保持跟随全局: %+v", bob)
	}
	def, _ := s2.Get(DefaultID)
	if def.Emoji != "🐱" {
		t.Fatalf("默认 agent 的头像没保存: %+v", def)
	}
	// 文件权限是 600
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("agents.json 权限应该是 600: %v %v", fi.Mode(), err)
	}
}

func TestBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(path, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("坏文件应该报错，不能悄悄当成空的")
	}
}

func TestSessionIDs(t *testing.T) {
	cases := []struct{ sid, agent string }{
		{"main", DefaultID},
		{"tg:123", DefaultID},
		{"@bob/main", "bob"},
		{"@bob/tg:123", "bob"},
		{"@bob/room:1", "bob"},
		{"@", DefaultID},
		{"@/main", DefaultID},
	}
	for _, c := range cases {
		if got := SessionAgent(c.sid); got != c.agent {
			t.Fatalf("SessionAgent(%q) = %q，应该是 %q", c.sid, got, c.agent)
		}
	}
	if got := SessionFor(DefaultID, "main"); got != "main" {
		t.Fatalf("默认 agent 的会话 ID 不该加前缀: %q", got)
	}
	if got := SessionFor("bob", "main"); got != "@bob/main" {
		t.Fatalf("别的 agent 的会话 ID 要加前缀: %q", got)
	}
	if got := SessionAgent(SessionFor("bob", "tg:123")); got != "bob" {
		t.Fatalf("往返后丢了 agent: %q", got)
	}
}

func TestNormalizeTrims(t *testing.T) {
	s := openTemp(t)
	if err := s.Create(Agent{ID: " Bob ", Name: "  小鲍  "}); err != nil {
		t.Fatalf("大小写和空格应该被规整: %v", err)
	}
	a, ok := s.Get("bob")
	if !ok || a.Name != "小鲍" {
		t.Fatalf("规整结果不对: %+v", a)
	}
}
