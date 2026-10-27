package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 键必须跨平台安全、可逆、不碰撞。
func TestStoreKeyRoundTrip(t *testing.T) {
	sids := []string{
		"main", "tg:12345", "@bot/main", "@bot/life", "web:abc", "cli:local",
		"中文会话", "with space", "with%percent", "UPPER", "upper",
		"trailing.", "trailing ", "CON", "nul", "a/b", "a\\b", "..", ".", "emoji🙂",
	}
	seen := map[string]string{}
	for _, sid := range sids {
		k := Key(sid)
		if strings.ContainsAny(k, ":\\/?*\"<>|") || strings.HasSuffix(k, ".") || strings.HasSuffix(k, " ") {
			t.Fatalf("键里有 Windows 不允许的字符：%q → %q", sid, k)
		}
		if len(k) > 120 {
			t.Fatalf("键太长：%q → %d", sid, len(k))
		}
		got, ok := ParseKey(k)
		if !ok || got != sid {
			t.Fatalf("反解：%q → %q (%v)", k, got, ok)
		}
		if other, dup := seen[k]; dup && other != sid {
			t.Fatalf("碰撞：%q 和 %q", other, sid)
		}
		seen[k] = sid
	}
	// 大小写不同的 ID 不能撞
	if Key("UPPER") == Key("upper") {
		t.Fatal("大小写不同却同一个键")
	}
}

// 超长 ID 用稳定哈希 + 持久映射，绝不截断。
func TestStoreKeyLongID(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("很长的会话标识", 20)
	k := Key(long)
	if !strings.HasPrefix(k, keyLongPrefix) {
		t.Fatalf("超长 ID 应该用哈希键：%q", k)
	}
	if _, ok := ParseKey(k); ok {
		t.Fatal("哈希键不该被当成可逆键")
	}
	if _, ok := s.resolveKey(k); ok {
		t.Fatal("还没记映射就解出来了")
	}
	if err := s.rememberLongKey(long); err != nil {
		t.Fatal(err)
	}
	got, ok := s.resolveKey(k)
	if !ok || got != long {
		t.Fatalf("映射反解：%v %q", ok, got)
	}
	if Key(long) != k {
		t.Fatal("同一个 ID 的键不稳定")
	}
}

// 旧键（url.PathEscape）的文件仍然能读。
func TestLegacyFilesStillReadable(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sid := "tg:777"
	legacy := filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("{\"role\":\"user\",\"content\":\"旧数据\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacySet := filepath.Join(dir, "sessions", LegacyKey(sid)+".settings.json")
	if err := os.WriteFile(legacySet, []byte("{\"title\":\"旧标题\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.History(sid); len(got) != 1 || got[0].Content != "旧数据" {
		t.Fatalf("旧键历史读不出来：%+v", got)
	}
	if got := s.Settings(sid).Title; got != "旧标题" {
		t.Fatalf("旧键设置读不出来：%q", got)
	}
	if list := s.List(); len(list) != 1 || list[0] != sid {
		t.Fatalf("枚举：%v", list)
	}
}

// 迁移：复制到新键、保留旧文件、可重复执行。
func TestMigrateKeysCopiesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sid := "tg:888"
	legacy := filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	data := "{\"role\":\"user\",\"content\":\"一\"}\n{\"role\":\"assistant\",\"content\":\"二\"}\n"
	if err := os.WriteFile(legacy, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := s.MigrateKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Moved) != 1 || rec.Moved[0] != sid {
		t.Fatalf("迁移记录：%+v", rec)
	}
	newPath := filepath.Join(dir, "sessions", Key(sid)+".jsonl")
	got, err := os.ReadFile(newPath)
	if err != nil || string(got) != data {
		t.Fatalf("新键内容：%v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("旧文件被删了（应该保留）")
	}
	// 再跑一次：不再动
	rec2, err := s.MigrateKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec2.Moved) != 0 {
		t.Fatalf("重复迁移：%+v", rec2)
	}
	if s.HasLegacyKeys() {
		t.Fatal("迁移完还有旧键")
	}
}

// 内容看不懂的旧文件保留现场，不删不改。
func TestMigrateKeysKeepsUnreadable(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "sessions", LegacyKey("tg:999")+".jsonl")
	os.MkdirAll(filepath.Dir(bad), 0o700)
	os.WriteFile(bad, []byte("不是 JSON\n"), 0o600)
	rec, err := s.MigrateKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Kept) != 1 {
		t.Fatalf("应该保留现场：%+v", rec)
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatal("坏文件被动了")
	}
}
