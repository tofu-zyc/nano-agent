package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 世代文件读不出来时绝不能被当成第 0 代，否则旧操作会删掉新代。
func TestUnreadableGenFileDoesNotDeleteNewGeneration(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:r20a", "tg:r20b"
	if err := s.Append(from, Msg{Role: "user", Content: "源正文"}); err != nil {
		t.Fatal(err)
	}
	// 旧操作记录：已提交、来源是第 0 代
	op := s.renameOpFile(from, to)
	if err := os.MkdirAll(filepath.Dir(op), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(op, []byte(`{"from":"`+from+`","to":"`+to+`","stage":"committed","from_gen":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 同 sid 开新代
	if err := s.Delete(from); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(from, Msg{Role: "user", Content: "新代正文"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.readGen(from); err != nil || n != 1 {
		t.Fatalf("新代世代：%d %v", n, err)
	}
	// 让世代文件不可读
	genPath := s.genFile(from)
	if err := os.Chmod(genPath, 0o000); err != nil {
		t.Fatal(err)
	}
	err := s.ResumeRenames()
	if err := os.Chmod(genPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err != nil && !containsAny(err.Error(), "世代", "generation") {
		t.Fatalf("应该是世代读取错误：%v", err)
	}
	// 不管恢复报什么，新代的消息都必须还在
	s2, _ := New(dir)
	got := s2.History(from)
	if len(got) != 1 || got[0].Content != "新代正文" {
		t.Fatalf("新代被误删了：%+v", got)
	}
}

// 源墓碑写成功、committed 记录没写成就中断：恢复应该能收尾，而不是卡在“源文件不见了”。
func TestRenameCommitWindowCanResume(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:cw1", "tg:cw2"
	if err := s.Append(from, Msg{Role: "user", Content: "源正文"}); err != nil {
		t.Fatal(err)
	}
	// 造出“提交点已过、记录还停在 copied”的现场：
	// 先把目标内容发布好、写源墓碑，然后把操作记录改回 copied。
	if err := s.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	// 手工重建现场（模拟提交窗口中断）
	if err := s.Append(from, Msg{Role: "user", Content: "源正文"}); err != nil {
		// 源已经改名走了：直接写文件造源
		_ = err
	}
	src := s.newPath(from, ".jsonl")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("{\"role\":\"user\",\"content\":\"源正文\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.deletedMarker(from)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.deletedMarker(from), []byte(Key(from)), 0o600); err != nil {
		t.Fatal(err)
	}
	op := s.renameOpFile(from, to)
	if err := os.MkdirAll(filepath.Dir(op), 0o700); err != nil {
		t.Fatal(err)
	}
	// 目标上已经发布的内容就是这一笔记录的内容版本
	published, rerr := os.ReadFile(s.newPath(to, ".jsonl"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	sum := sumOf(published)
	body := fmt.Sprintf(`{"from":%q,"to":%q,"stage":"copied","from_gen":0,"files":{".jsonl":{"len":%d,"sum":%q}}}`, from, to, len(published), sum)
	if err := os.WriteFile(op, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeRenames(); err != nil {
		t.Fatalf("提交窗口恢复失败：%v", err)
	}
	if _, err := os.Stat(op); err == nil {
		t.Fatal("操作记录没有收尾")
	}
	s2, _ := New(dir)
	if got := s2.History(to); len(got) == 0 {
		t.Fatalf("目标内容丢了：%+v", got)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
