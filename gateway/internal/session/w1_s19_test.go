package session

import (
	"os"
	"path/filepath"
	"testing"
)

// V1：保存设置不能绕过新代隔离（旧正文）。
func TestSaveSettingsDoesNotReviveHistory(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:v1a"
	if err := s.Append(sid, Msg{Role: "user", Content: "旧正文"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(sid); err != nil {
		t.Fatal(err)
	}
	st := s.Settings(sid)
	st.Title = "新标题"
	if err := s.SaveSettings(sid, st); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if got := s2.History(sid); len(got) != 0 {
		t.Fatalf("保存设置复活了旧正文：%+v", got)
	}
}

// V1：保存设置也不能带回旧分支快照。
func TestSaveSettingsDoesNotReviveBranchSnapshot(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	parent := "main"
	if err := s.Append(parent, Msg{Role: "user", Content: "父上下文"}); err != nil {
		t.Fatal(err)
	}
	br := "br-v1"
	if err := s.BeginBranch(BranchMeta{SID: br, ParentSID: parent, Title: "分支"}); err != nil {
		t.Fatal(err)
	}
	stp := s.Settings(parent)
	snap, err := s.WriteSnapshot(parent, s.SnapshotIDFor(parent, stp), stp)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReadyBranch(br, snap, BranchReady); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(br); err != nil {
		t.Fatal(err)
	}
	st := s.Settings(br)
	st.Title = "重建的分支"
	if err := s.SaveSettings(br, st); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if got := s2.History(br); len(got) != 0 {
		t.Fatalf("保存设置带回了旧分支快照：%+v", got)
	}
	if _, ok := s2.Branch(br); ok {
		t.Fatal("保存设置恢复了旧分支关系")
	}
}

// V2：复制之后源新出现的历史文件也必须纳入校验，不能发布后删源。
func TestRenameDetectsNewSourceFileAfterCopy(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:v2a", "tg:v2b"
	// 源只有设置、没有历史
	st := Settings{}
	st.Title = "只有设置"
	if err := s.SaveSettings(from, st); err != nil {
		t.Fatal(err)
	}
	op := s.renameOpFile(from, to)
	// 先做一次复制，模拟“复制之后”的状态
	deleted := filepath.Join(dir, "v1", "deleted")
	chmod(t, deleted, 0o500)
	err := s.Rename(from, to)
	chmod(t, deleted, 0o700)
	if err == nil {
		t.Fatal("墓碑写入失败却报成功")
	}
	// 复制之后源又写了一条历史
	if err := s.Append(from, Msg{Role: "user", Content: "复制后新写的"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(from, to); err == nil {
		t.Fatal("新出现的源文件没有被发现，继续改名会丢消息")
	}
	if got := s.History(from); len(got) != 1 {
		t.Fatalf("源被破坏：%+v", got)
	}
	_ = op
}

// V3：暂存副本缺失时必须停下，不能发布后删源。
func TestRenameMissingStagedFileIsNotSkipped(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:v3a", "tg:v3b"
	if err := s.Append(from, Msg{Role: "user", Content: "内容"}); err != nil {
		t.Fatal(err)
	}
	deleted := filepath.Join(dir, "v1", "deleted")
	chmod(t, deleted, 0o500)
	err := s.Rename(from, to)
	chmod(t, deleted, 0o700)
	if err == nil {
		t.Fatal("墓碑写入失败却报成功")
	}
	// 删掉暂存副本
	staging := s.renameStagingDir(from, to)
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(staging, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Rename(from, to); err == nil {
		t.Fatal("暂存副本没了却继续发布")
	}
	if got := s.History(from); len(got) != 1 {
		t.Fatalf("源被删掉了：%+v", got)
	}
}

// V4：上一版写的是键本身，不是数字，必须仍被当成“已开过新一代”。
func TestLegacyGenMarkerStillInvalidatesBackup(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "br-legacygen"
	// 旧格式标记：内容是键本身
	if err := os.MkdirAll(filepath.Join(dir, "v1", "gen"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.genFile(sid), []byte(Key(sid)), 0o600); err != nil {
		t.Fatal(err)
	}
	// 旧根里还有一份分支元数据
	legacy := filepath.Join(dir, "sessions", LegacyKey(sid)+".branch.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"sid":"br-legacygen","parent_sid":"main","status":"ready"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if n, err := s2.readGen(sid); err != nil || n == 0 {
		t.Fatal("旧格式的世代标记被当成第 0 代")
	}
	if _, ok := s2.Branch(sid); ok {
		t.Fatal("旧分支备份重新获得了授权")
	}
}
