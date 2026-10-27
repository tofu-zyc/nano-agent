package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// S1：同名归档内容不同时必须保留双方并报冲突，不能覆盖。
func TestArchivePromotionConflictPreservesBothFiles(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:arch"
	legacyHist := filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")
	writeFile(t, legacyHist, "{\"role\":\"user\",\"content\":\"旧\"}\n")
	legacyArc := filepath.Join(dir, "archive", legacyArchiveKey(sid), "same.jsonl")
	writeFile(t, legacyArc, "{\"role\":\"user\",\"content\":\"legacy-copy\"}\n")
	newArc := filepath.Join(s.dirV1Archive(), Key(sid), "same.jsonl")
	writeFile(t, newArc, "{\"role\":\"user\",\"content\":\"valuable-new-copy\"}\n")

	if _, err := s.Migrate(); err == nil {
		t.Fatal("冲突却返回成功")
	}
	got, err := os.ReadFile(newArc)
	if err != nil || !strings.Contains(string(got), "valuable-new-copy") {
		t.Fatalf("新归档被覆盖了：%s", got)
	}
	got2, _ := os.ReadFile(legacyArc)
	if !strings.Contains(string(got2), "legacy-copy") {
		t.Fatal("旧归档被改了")
	}
	if _, err := os.Stat(s.activeMarker(sid)); err == nil {
		t.Fatal("冲突时不应该发布激活状态")
	}
}

// S2：重放迁移不能复活重置 / 删除过的会话，也不能把正常差异报成冲突。
func TestMigrationReplayHonorsResetDeleteAndActiveWrites(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:replay"
	writeFile(t, filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl"),
		"{\"role\":\"user\",\"content\":\"旧的\"}\n")
	if err := s.Append(sid, Msg{Role: "assistant", Content: "新的"}); err != nil {
		t.Fatal(err)
	}
	// 1) 重置后重放迁移：不能复活
	if err := s.Reset(sid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(); err != nil {
		t.Fatalf("重置后迁移报错：%v", err)
	}
	s2, _ := New(dir)
	if got := s2.History(sid); len(got) != 0 {
		t.Fatalf("重置后又复活：%+v", got)
	}
	// 2) 正常续写后重放迁移：不能误报冲突
	if err := s2.Append(sid, Msg{Role: "user", Content: "重置后新写"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Migrate(); err != nil {
		t.Fatalf("正常续写被当成冲突：%v", err)
	}
	s3, _ := New(dir)
	if got := s3.History(sid); len(got) != 1 || got[0].Content != "重置后新写" {
		t.Fatalf("续写后的历史：%+v", got)
	}
	// 3) 删除后重放迁移：不能复活
	if err := s3.Delete(sid); err != nil {
		t.Fatal(err)
	}
	if _, err := s3.Migrate(); err != nil {
		t.Fatalf("删除后迁移报错：%v", err)
	}
	s4, _ := New(dir)
	if got := s4.History(sid); len(got) != 0 {
		t.Fatalf("删除后又复活：%+v", got)
	}
	if s4.Exists(sid) {
		t.Fatal("删除后还存在")
	}
}

// S3：激活失败之后重试不能绕过提交门禁。
func TestPromotionRetryCannotBypassActivationFailure(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:gate"
	writeFile(t, filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl"),
		"{\"role\":\"user\",\"content\":\"旧的\"}\n")
	// 用文件占住 active 目录，让激活标记写不进去
	if err := os.RemoveAll(filepath.Join(dir, "v1", "active")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "v1", "active"), "x")

	if err := s.Append(sid, Msg{Role: "assistant", Content: "一"}); err == nil {
		t.Fatal("激活失败却受理成功")
	}
	if err := s.Append(sid, Msg{Role: "assistant", Content: "二"}); err == nil {
		t.Fatal("重试绕过了激活门禁")
	}
	// 修好之后能正常提升并写入，历史完整
	if err := os.Remove(filepath.Join(dir, "v1", "active")); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if err := s2.Append(sid, Msg{Role: "assistant", Content: "三"}); err != nil {
		t.Fatal(err)
	}
	s3, _ := New(dir)
	got := s3.History(sid)
	if len(got) != 2 || got[0].Content != "旧的" || got[1].Content != "三" {
		t.Fatalf("修好后的历史：%+v", got)
	}
}

// S4：旧会话改名后，旧名字不能在重启后复活。
func TestLegacyRenameDoesNotRecreateSourceAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:old", "tg:new"
	writeFile(t, filepath.Join(dir, "sessions", LegacyKey(from)+".jsonl"),
		"{\"role\":\"user\",\"content\":\"内容\"}\n")
	if err := s.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if got := s2.History(from); len(got) != 0 {
		t.Fatalf("旧名字复活：%+v", got)
	}
	if got := s2.History(to); len(got) != 1 {
		t.Fatalf("新名字读不到：%+v", got)
	}
	for _, sid := range s2.List() {
		if sid == from {
			t.Fatal("列表里还有旧名字")
		}
	}
}

// S4：删除的会话不能被列表当成活动会话。
func TestDeletedBackupIsNotListedAsLiveSession(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:gone"
	writeFile(t, filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl"),
		"{\"role\":\"user\",\"content\":\"内容\"}\n")
	if err := s.Append(sid, Msg{Role: "assistant", Content: "一"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(sid); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	for _, got := range s2.List() {
		if got == sid {
			t.Fatal("删掉的会话还在列表里")
		}
	}
	if s2.Exists(sid) {
		t.Fatal("删掉的会话还存在")
	}
}

// S5：分支元数据要写在新命名空间，List 也要还原成逻辑 sid。
func TestBranchMetaInV1Namespace(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "br-owned"
	if err := s.BeginBranch(BranchMeta{SID: sid, ParentSID: "main", Title: "分支"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.newPath(sid, ".branch.json")); err != nil {
		t.Fatal("分支元数据没写在新布局")
	}
	s2, _ := New(dir)
	if _, ok := s2.Branch(sid); !ok {
		t.Fatal("重启后读不到分支")
	}
	for _, got := range s2.List() {
		if got == sid {
			return
		}
	}
	t.Fatalf("列表里没有分支会话：%v", s2.List())
}
