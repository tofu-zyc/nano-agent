package session

import (
	"testing"
	"time"
)

// 会话里进过不可信的话（设计 §16.4）：按会话记、落盘；分出去的会话继承；只在清空重开时清，压缩历史不清。
func TestUntrustedMarkPersistsInheritsAndClearsOnlyOnReset(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	for _, c := range []string{"问", "答", "再问", "再答"} {
		if err := s.Append("main", Msg{Role: "user", Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	st := s.Settings("main")
	st.Untrusted = true
	if err := s.SaveSettings("main", st); err != nil {
		t.Fatal(err)
	}
	// 落盘：重新打开还在
	s, _ = New(dir)
	if !s.Settings("main").Untrusted {
		t.Fatal("标记要落盘")
	}
	// 压缩历史不清：摘要里还有那些话
	if err := s.CommitCompact("main", 2, "摘要", time.Now()); err != nil {
		t.Fatal(err)
	}
	if !s.Settings("main").Untrusted {
		t.Fatal("压缩历史不清标记")
	}
	// 分出去的会话继承；没标记的不会凭空多出来
	forkStore(t, s, "main", "branch")
	if !s.Settings("branch").Untrusted {
		t.Fatal("分出去的会话继承标记")
	}
	clean := Settings{}
	if err := s.SaveSettings("clean", clean); err != nil {
		t.Fatal(err)
	}
	forkStore(t, s, "clean", "clean-branch")
	if s.Settings("clean-branch").Untrusted {
		t.Fatal("没标记的分出去也没有")
	}
	// 清空重开才清；分支清空时也清（它的上下文不再继承源会话）
	if err := s.Reset("branch"); err != nil {
		t.Fatal(err)
	}
	if s.Settings("branch").Untrusted {
		t.Fatal("分支清空重开，标记清掉")
	}
	if !s.Settings("main").Untrusted {
		t.Fatal("清分支不动源会话")
	}
	if err := s.Reset("main"); err != nil {
		t.Fatal(err)
	}
	if s.Settings("main").Untrusted {
		t.Fatal("清空重开，标记清掉")
	}
	s, _ = New(dir)
	if s.Settings("main").Untrusted {
		t.Fatal("清掉的也落盘")
	}
}
