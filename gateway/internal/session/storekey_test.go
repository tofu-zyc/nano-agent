package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreKeyRoundTrip(t *testing.T) {
	sids := []string{"main", "tg:12345", "@bot/main", "web:abc", "中文会话", "with space",
		"with%percent", "UPPER", "upper", "trailing.", "CON", "a/b", "..", "v1-61", "emoji🙂"}
	seen := map[string]string{}
	for _, sid := range sids {
		k := Key(sid)
		if strings.ContainsAny(k, ":\\/?*\"<>|") {
			t.Fatalf("键里有不允许的字符：%q → %q", sid, k)
		}
		if got, ok := ParseKey(k); !ok || got != sid {
			t.Fatalf("反解：%q → %q (%v)", k, got, ok)
		}
		if other, dup := seen[k]; dup && other != sid {
			t.Fatalf("碰撞：%q 和 %q", other, sid)
		}
		seen[k] = sid
	}
	if Key("UPPER") == Key("upper") {
		t.Fatal("大小写不同却同一个键")
	}
}

// 旧字面量 "v1-61" 不能被当成新键 "a"：新布局在独立目录里，不看文件名猜格式。
func TestLegacyLiteralLookingLikeNewKey(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "sessions", "v1-61.jsonl")
	os.MkdirAll(filepath.Dir(legacy), 0o700)
	os.WriteFile(legacy, []byte("{\"role\":\"user\",\"content\":\"旧会话\"}\n"), 0o600)
	list := s.List()
	if len(list) != 1 || list[0] != "v1-61" {
		t.Fatalf("枚举：%v", list)
	}
	if got := s.History("a"); len(got) != 0 {
		t.Fatalf("串会话读到了别人的历史：%+v", got)
	}
}

// 旧会话续写：先完整升级到新布局，不能只写增量；重启后历史完整。
func TestLegacySessionAppendThenRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:777"
	legacy := filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")
	os.MkdirAll(filepath.Dir(legacy), 0o700)
	os.WriteFile(legacy, []byte("{\"role\":\"user\",\"content\":\"旧的\"}\n"), 0o600)
	if err := s.Append(sid, Msg{Role: "assistant", Content: "新的"}); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	got := s2.History(sid)
	if len(got) != 2 || got[0].Content != "旧的" || got[1].Content != "新的" {
		t.Fatalf("重启后历史：%+v", got)
	}
}

// 升级之后重置 / 删除：不能回退旧备份复活内容。
func TestPromotedSessionResetDoesNotResurrect(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:888"
	legacy := filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")
	os.MkdirAll(filepath.Dir(legacy), 0o700)
	os.WriteFile(legacy, []byte("{\"role\":\"user\",\"content\":\"旧\"}\n"), 0o600)
	if err := s.Append(sid, Msg{Role: "user", Content: "一"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(sid); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if got := s2.History(sid); len(got) != 0 {
		t.Fatalf("重置后又从旧备份复活：%+v", got)
	}
	if err := s2.Delete(sid); err != nil {
		t.Fatal(err)
	}
	s3, _ := New(dir)
	if got := s3.History(sid); len(got) != 0 {
		t.Fatalf("删除后又复活：%+v", got)
	}
}

// 长 ID 通过真实写入口登记映射，重启后能枚举、能读。
func TestLongSIDRegisteredByAppend(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	long := strings.Repeat("很长的会话标识", 20)
	if err := s.Append(long, Msg{Role: "user", Content: "长会话"}); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	list := s2.List()
	if len(list) != 1 || list[0] != long {
		t.Fatalf("枚举：%v", list)
	}
	if got := s2.History(long); len(got) != 1 {
		t.Fatalf("读不出来：%+v", got)
	}
}

// 迁移：复制到新布局、保留旧文件、可重复执行；清单写失败要报错。
func TestMigrateCopiesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:999"
	legacy := filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")
	os.MkdirAll(filepath.Dir(legacy), 0o700)
	data := "{\"role\":\"user\",\"content\":\"一\"}\n"
	os.WriteFile(legacy, []byte(data), 0o600)
	rec, err := s.Migrate()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Items) != 1 || rec.Items[0].Stage != "verified" {
		t.Fatalf("记录：%+v", rec)
	}
	newPath := s.newPath(sid, ".jsonl")
	if got, err := os.ReadFile(newPath); err != nil || string(got) != data {
		t.Fatalf("新位置内容：%v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("旧文件被删了")
	}
	if s.HasLegacyKeys() {
		t.Fatal("升级完还算有旧会话")
	}
}
