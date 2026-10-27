package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// U1：清理失败后同 sid 写新消息，必须先隔离旧代残留，重启不能复活。
func TestDeleteCleanupFailureCannotReviveOldData(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:cleanup"
	if err := s.Append(sid, Msg{Role: "user", Content: "旧正文"}); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(dir, "v1", "sessions")
	chmod(t, sessions, 0o500)
	err := s.Delete(sid)
	chmod(t, sessions, 0o700)
	if err == nil {
		t.Fatal("清理失败却返回成功")
	}
	// 恢复权限后同 sid 写新消息：必须是干净的新代
	if err := s.Append(sid, Msg{Role: "user", Content: "新代正文"}); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	got := s2.History(sid)
	if len(got) != 1 || got[0].Content != "新代正文" {
		t.Fatalf("旧代残留又出现了：%+v", got)
	}
}

// U1：墓碑删不掉时不能回执成功。
func TestRecreateCannotAckWhenTombstoneRemovalFails(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:tomb"
	if err := s.Append(sid, Msg{Role: "user", Content: "旧"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(sid); err != nil {
		t.Fatal(err)
	}
	deleted := filepath.Join(dir, "v1", "deleted")
	chmod(t, deleted, 0o500)
	err := s.Append(sid, Msg{Role: "user", Content: "新"})
	chmod(t, deleted, 0o700)
	if err == nil {
		t.Fatal("墓碑没删掉却回执成功")
	}
	// 修好之后能正常创建新代
	if err := s.Append(sid, Msg{Role: "user", Content: "新"}); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	got := s2.History(sid)
	if len(got) != 1 || got[0].Content != "新" {
		t.Fatalf("新消息不可见：%+v", got)
	}
}

// U1：旧根里的分支备份在删除后必须永久失效，同 sid 新代也不能恢复分支关系。
func TestDeletedBranchLegacyBackupMustStayDead(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	parent := "main"
	if err := s.Append(parent, Msg{Role: "user", Content: "父上下文"}); err != nil {
		t.Fatal(err)
	}
	// 在旧根放一份 ready 分支元数据（模拟历史备份）
	legacyBr := filepath.Join(dir, "sessions", LegacyKey("br-old")+".branch.json")
	if err := os.MkdirAll(filepath.Dir(legacyBr), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyBr, []byte(`{"sid":"br-old","parent_sid":"main","status":"ready"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("br-old"); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if _, ok := s2.Branch("br-old"); ok {
		t.Fatal("删除后旧分支元数据又被加载")
	}
	// 同 sid 重建新代：旧分支关系不能回来
	if err := s2.Append("br-old", Msg{Role: "user", Content: "新代"}); err != nil {
		t.Fatal(err)
	}
	s3, _ := New(dir)
	if _, ok := s3.Branch("br-old"); ok {
		t.Fatal("新代又带回了旧分支关系")
	}
	if got := s3.History("br-old"); len(got) != 1 || got[0].Content != "新代" {
		t.Fatalf("新代历史：%+v", got)
	}
}

// U2：没提交的改名副本不能让目标提前可见。
func TestPendingRenameMustNotPublishTarget(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:p1", "tg:p2"
	if err := s.Append(from, Msg{Role: "user", Content: "内容"}); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(dir, "v1", "active")
	chmod(t, active, 0o500)
	err := s.Rename(from, to)
	chmod(t, active, 0o700)
	if err == nil {
		t.Fatal("激活失败却报成功")
	}
	s2, _ := New(dir)
	if s2.Exists(to) {
		t.Fatal("未提交的目标提前可见")
	}
	if got := s2.History(to); len(got) != 0 {
		t.Fatalf("未提交的目标读到了内容：%+v", got)
	}
	if got := s2.History(from); len(got) != 1 {
		t.Fatalf("源应该完好：%+v", got)
	}
}

// U2：目标已经接受新消息后，改名重试不能覆盖它们。
func TestRenameRetryPreservesAcceptedTargetMessages(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:m1", "tg:m2"
	if err := s.Append(from, Msg{Role: "user", Content: "源正文"}); err != nil {
		t.Fatal(err)
	}
	deleted := filepath.Join(dir, "v1", "deleted")
	chmod(t, deleted, 0o500)
	err := s.Rename(from, to)
	chmod(t, deleted, 0o700)
	if err == nil {
		t.Fatal("墓碑写入失败却报成功")
	}
	// 目标此时已经激活：接受一条新消息
	if err := s.Append(to, Msg{Role: "user", Content: "目标新消息"}); err != nil {
		t.Fatal(err)
	}
	// 修好之后重试同一笔改名
	if err := s.Rename(from, to); err != nil {
		t.Fatalf("重试失败：%v", err)
	}
	s2, _ := New(dir)
	got := s2.History(to)
	if len(got) != 2 || got[1].Content != "目标新消息" {
		t.Fatalf("目标已受理的消息被覆盖了：%+v", got)
	}
	if len(s2.History(from)) != 0 {
		t.Fatal("源应该已经失效")
	}
}

// U2：提交之后清理失败，重试必须只做清理。
func TestRenameCommittedCleanupCanResume(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:c1", "tg:c2"
	if err := s.Append(from, Msg{Role: "user", Content: "源正文"}); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(dir, "v1", "sessions")
	// 让源文件删不掉：目录只读
	if err := os.Chmod(sessions, 0o500); err != nil {
		t.Fatal(err)
	}
	err := s.Rename(from, to)
	if err := os.Chmod(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("清理失败却报成功")
	}
	if err := s.Rename(from, to); err != nil {
		t.Fatalf("提交后的清理不能重试：%v", err)
	}
	s2, _ := New(dir)
	if got := s2.History(to); len(got) != 1 || !strings.Contains(got[0].Content, "源正文") {
		t.Fatalf("目标内容：%+v", got)
	}
	if s2.Exists(from) {
		t.Fatal("源还在")
	}
}
