package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T1：删除后用同一个 sid 重建，必须是干净的新代（原生布局）。
func TestDeleteThenRecreateStartsCleanAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:rebuild"
	if err := s.Append(sid, Msg{Role: "user", Content: "旧消息"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(sid); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if got := s2.History(sid); len(got) != 0 {
		t.Fatalf("删除后还有历史：%+v", got)
	}
	if err := s2.Append(sid, Msg{Role: "user", Content: "新消息"}); err != nil {
		t.Fatal(err)
	}
	s3, _ := New(dir)
	got := s3.History(sid)
	if len(got) != 1 || got[0].Content != "新消息" {
		t.Fatalf("重建后历史：%+v", got)
	}
	if !s3.Exists(sid) {
		t.Fatal("重建后应该存在")
	}
	found := false
	for _, x := range s3.List() {
		if x == sid {
			found = true
		}
	}
	if !found {
		t.Fatalf("重建后列表里没有：%v", s3.List())
	}
}

// T1：从旧布局提升过来的会话，删除后同 sid 重建也不能带回旧内容。
func TestDeletePromotedThenRecreateIsClean(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:rebuild2"
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl"),
		[]byte("{\"role\":\"user\",\"content\":\"旧备份\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(sid, Msg{Role: "assistant", Content: "提升后追加"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(sid); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if err := s2.Append(sid, Msg{Role: "user", Content: "新代"}); err != nil {
		t.Fatal(err)
	}
	s3, _ := New(dir)
	got := s3.History(sid)
	if len(got) != 1 || got[0].Content != "新代" {
		t.Fatalf("重建后带回了旧内容：%+v", got)
	}
}

// T1：删除的分支元数据不能被重新加载。
func TestDeletedBranchMetadataIsNotReloaded(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "br-deleted"
	if err := s.BeginBranch(BranchMeta{SID: sid, ParentSID: "main", Title: "分支"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(sid); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if _, ok := s2.Branch(sid); ok {
		t.Fatal("删除的分支又加载出来了")
	}
	for _, x := range s2.List() {
		if x == sid {
			t.Fatal("删除的分支出现在列表里")
		}
	}
}

// T1：删除的 ready 分支不能再读继承快照。
func TestDeletedReadyBranchCannotReadInheritedSnapshot(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	parent := "main"
	if err := s.Append(parent, Msg{Role: "user", Content: "父会话的上下文"}); err != nil {
		t.Fatal(err)
	}
	br := "br-ready"
	if err := s.BeginBranch(BranchMeta{SID: br, ParentSID: parent, Title: "分支"}); err != nil {
		t.Fatal(err)
	}
	st := s.Settings(parent)
	snapID := s.SnapshotIDFor(parent, st)
	snap, err := s.WriteSnapshot(parent, snapID, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReadyBranch(br, snap, BranchReady); err != nil {
		t.Fatal(err)
	}
	if got := s.History(br); len(got) == 0 {
		t.Fatal("就绪分支应该能读到继承的上下文")
	}
	if err := s.Delete(br); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if got := s2.History(br); len(got) != 0 {
		t.Fatalf("删除的分支还能读到继承快照：%+v", got)
	}
	if s2.Exists(br) {
		t.Fatal("删除的分支还存在")
	}
}

// T2：目标激活失败后，源还在，修好后能继续同一笔改名。
func TestRenameActivationFailureCanResume(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:a", "tg:b"
	if err := s.Append(from, Msg{Role: "user", Content: "内容"}); err != nil {
		t.Fatal(err)
	}
	// 用文件占住 active 目录，让目标激活写不进去
	if err := os.RemoveAll(filepath.Join(dir, "v1", "active")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "v1", "active"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(from, to); err == nil {
		t.Fatal("激活失败却报成功")
	}
	if got := s.History(from); len(got) != 1 {
		t.Fatalf("失败后源应该完好：%+v", got)
	}
	// 修好故障，重试同一笔改名
	if err := os.Remove(filepath.Join(dir, "v1", "active")); err != nil {
		t.Fatal(err)
	}
	s3, _ := New(dir)
	if err := s3.Rename(from, to); err != nil {
		t.Fatalf("重试失败：%v", err)
	}
	s4, _ := New(dir)
	if got := s4.History(to); len(got) != 1 || got[0].Content != "内容" {
		t.Fatalf("目标历史：%+v", got)
	}
	if got := s4.History(from); len(got) != 0 {
		t.Fatalf("源不该还在：%+v", got)
	}
}

// T2：目标长 ID 映射损坏时，源必须保持可用。
func TestRenameLongKeyFailurePreservesSource(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from := "tg:src"
	long := strings.Repeat("很长的新名字", 20)
	if err := s.Append(from, Msg{Role: "user", Content: "内容"}); err != nil {
		t.Fatal(err)
	}
	// 预先放一个损坏的映射
	badPath := s.keyMapFile(Key(long))
	if err := os.MkdirAll(filepath.Dir(badPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badPath, []byte("不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(from, long); err == nil {
		t.Fatal("映射损坏却报成功")
	}
	s2, _ := New(dir)
	if got := s2.History(from); len(got) != 1 {
		t.Fatalf("源被破坏了：%+v", got)
	}
	if got := s2.History(long); len(got) != 0 {
		t.Fatalf("目标不该有内容：%+v", got)
	}
}
