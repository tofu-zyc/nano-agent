package session

import (
	"os"
	"path/filepath"
	"testing"
)

// U3：保存设置也必须走新代隔离，不能清掉墓碑带回残留正文。
func TestSaveSettingsCannotReviveAfterDelete(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:setg"
	if err := s.Append(sid, Msg{Role: "user", Content: "旧正文"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(sid); err != nil {
		t.Fatal(err)
	}
	st := s.Settings(sid)
	st.Title = "重建后的标题"
	if err := s.SaveSettings(sid, st); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if got := s2.History(sid); len(got) != 0 {
		t.Fatalf("保存设置把旧正文带回来了：%+v", got)
	}
	if got := s2.Settings(sid).Title; got != "重建后的标题" {
		t.Fatalf("设置没保存：%q", got)
	}
}

// U3：复制之后源又被写了新消息，恢复必须报冲突而不是丢掉源的内容。
func TestRenameSourceAppendDuringCopyIsNotLost(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:sc1", "tg:sc2"
	if err := s.Append(from, Msg{Role: "user", Content: "第一版"}); err != nil {
		t.Fatal(err)
	}
	deleted := filepath.Join(dir, "v1", "deleted")
	chmod(t, deleted, 0o500)
	err := s.Rename(from, to)
	chmod(t, deleted, 0o700)
	if err == nil {
		t.Fatal("墓碑写入失败却报成功")
	}
	// 源在这之后又收到一条新消息
	if err := s.Append(from, Msg{Role: "user", Content: "第二版"}); err != nil {
		t.Fatal(err)
	}
	err = s.Rename(from, to)
	if err == nil {
		t.Fatal("源变了却仍然改名成功（会丢消息）")
	}
	// 双方都要还在
	if got := s.History(from); len(got) != 2 {
		t.Fatalf("源被破坏：%+v", got)
	}
}

// U3：提交之后的清理不能删掉同 sid 新代的数据。
func TestRenameCommittedCleanupDoesNotTouchNewGeneration(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:ng1", "tg:ng2"
	if err := s.Append(from, Msg{Role: "user", Content: "源正文"}); err != nil {
		t.Fatal(err)
	}
	// 模拟“改名已提交、但清理源文件失败”留下的操作记录
	op := s.renameOpFile(from, to)
	if err := os.MkdirAll(filepath.Dir(op), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(op, []byte(`{"from":"`+from+`","to":"`+to+`","stage":"committed","from_gen":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 同 sid 开新代（这会用到同一批路径）
	if err := s.Delete(from); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(from, Msg{Role: "user", Content: "新代正文"}); err != nil {
		t.Fatal(err)
	}
	// 启动恢复：不能删掉新代
	if err := s.ResumeRenames(); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	s2, _ := New(dir)
	got := s2.History(from)
	if len(got) != 1 || got[0].Content != "新代正文" {
		t.Fatalf("新代被旧操作删了：%+v", got)
	}
}

// U3：不认识的阶段必须拒绝，不能落进清理代码。
func TestRenameUnknownStageRefusesCleanup(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:us1", "tg:us2"
	if err := s.Append(from, Msg{Role: "user", Content: "内容"}); err != nil {
		t.Fatal(err)
	}
	op := s.renameOpFile(from, to)
	if err := os.MkdirAll(filepath.Dir(op), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(op, []byte(`{"from":"`+from+`","to":"`+to+`","stage":"weird"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(from, to); err == nil {
		t.Fatal("不认识的阶段却被接受")
	}
	if got := s.History(from); len(got) != 1 {
		t.Fatalf("未知阶段却动了源文件：%+v", got)
	}
}
