package ticket

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime", "tickets.jsonl")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.Unix(1000, 0) }
	t.Cleanup(func() { s.Close() })
	return s, path
}

func mk(msg string) Ticket {
	return Ticket{Agent: "default", Title: "修登录", Goal: "登录页在手机上错位", Check: "make test", Budget: 100,
		Auth: Auth{By: "telegram:1", Scene: "g1", Workspace: "ws1", Mode: "auto", Tools: []string{"Read"},
			Source: Source{Channel: "telegram", Chat: "-100", MsgID: msg, Text: "@bot 帮忙修一下"}}}
}

func TestCreateAssignsIDsAndDedupesBySource(t *testing.T) {
	s, _ := open(t)
	a, existed, err := s.Create(mk("m1"))
	if err != nil || existed || a.ID != "tk-1" || a.Status != StatusOpen || a.Created != 1000 {
		t.Fatalf("第一张：%+v %v %v", a, existed, err)
	}
	again, existed, err := s.Create(mk("m1"))
	if err != nil || !existed || again.ID != "tk-1" {
		t.Fatalf("同一条原消息重试返回同一张：%+v %v %v", again, existed, err)
	}
	b, _, _ := s.Create(mk("m2"))
	if b.ID != "tk-2" || len(s.List()) != 2 {
		t.Fatalf("另一条消息是另一张：%+v", b)
	}
	// 没有消息 ID 的不去重
	x, _, _ := s.Create(mk(""))
	y, existed, _ := s.Create(mk(""))
	if existed || x.ID == y.ID {
		t.Fatal("没有消息 ID 的不去重")
	}
	for name, mod := range map[string]func(*Ticket){
		"没有标题":   func(t *Ticket) { t.Title = " " },
		"没有目标":   func(t *Ticket) { t.Goal = "" },
		"没有派活人":  func(t *Ticket) { t.Auth.By = "" },
		"没有工作目录": func(t *Ticket) { t.Auth.Workspace = "" },
	} {
		bad := mk("m9")
		mod(&bad)
		if _, _, err := s.Create(bad); err == nil {
			t.Errorf("%s 要拒绝", name)
		}
	}
}

func TestUpdateKeepsAuthAndReplays(t *testing.T) {
	s, path := open(t)
	a, _, _ := s.Create(mk("m1"))
	got, err := s.Update(a.ID, func(t *Ticket) {
		t.Status, t.Reason = StatusStopped, "不做了"
		t.Auth.By = "evil" // 授权记录建的时候定下，之后不变
		t.ID = "tk-99"
	})
	if err != nil || got.ID != a.ID || got.Auth.By != "telegram:1" || got.Status != StatusStopped {
		t.Fatalf("更新：%+v %v", got, err)
	}
	if _, err := s.Update("tk-77", func(*Ticket) {}); err == nil {
		t.Fatal("没有的工单要报错")
	}
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	r, ok := s2.Get(a.ID)
	if !ok || r.Status != StatusStopped || r.Reason != "不做了" || r.Auth.Source.Text != "@bot 帮忙修一下" {
		t.Fatalf("重放：%+v", r)
	}
	if n, _, _ := s2.Create(mk("m2")); n.ID != "tk-2" {
		t.Fatalf("重启后编号接着走：%s", n.ID)
	}
}

func TestNotesAreBounded(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.Create(mk("m1"))
	for i := 0; i < maxNotes+5; i++ {
		s.Update(a.ID, func(t *Ticket) { t.Notes = append(t.Notes, Note{By: "x", Text: fmt.Sprint(i)}) })
	}
	got, _ := s.Get(a.ID)
	if len(got.Notes) != maxNotes || got.Notes[len(got.Notes)-1].Text != fmt.Sprint(maxNotes+4) {
		t.Fatalf("只留最近 %d 条：%d", maxNotes, len(got.Notes))
	}
}

// 编号不重用：最大号的工单被压缩丢掉后，新工单仍拿到更大的号。
func TestIDsAreNeverReusedAfterCompaction(t *testing.T) {
	s, path := open(t)
	a, _, _ := s.Create(mk("m1"))
	b, _, _ := s.Create(mk("m2"))
	s.Now = func() time.Time { return time.Unix(1000, 0) }
	s.Update(b.ID, func(t *Ticket) { t.Status, t.Reason = StatusDone, "收下了" })
	s.Update(a.ID, func(t *Ticket) { t.Status, t.Reason = StatusStopped, "叫停" })
	s.Now = func() time.Time { return time.Unix(1000, 0).Add(KeepEnded + time.Hour) } // 都结束超过 30 天
	live, _, _ := s.Create(mk("m3"))                                                  // tk-3，没结束
	if live.ID != "tk-3" {
		t.Fatal(live.ID)
	}
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("tk-2"); ok {
		t.Fatal("结束超过 30 天的丢掉")
	}
	// 把最大号的也结束并丢掉
	s.Update(live.ID, func(t *Ticket) { t.Status, t.Reason = StatusDropped, "不用了" })
	s.Now = func() time.Time { return time.Unix(1000, 0).Add(3 * KeepEnded) }
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatalf("全丢掉了：%+v", s.List())
	}
	n, _, _ := s.Create(mk("m4"))
	if n.ID != "tk-4" {
		t.Fatalf("最大号被丢掉以后新工单仍拿更大的号：%s", n.ID)
	}
	// 重启以后也一样
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if m, _, _ := s2.Create(mk("m5")); m.ID != "tk-5" {
		t.Fatalf("重放取水位和记录里较大的：%s", m.ID)
	}
}

func TestWriteFailureReturnsErrorAndKeepsMemory(t *testing.T) {
	s, _ := open(t)
	big := mk("m1")
	big.Goal = strings.Repeat("字", RecordLimit) // 超过单条上限
	if _, _, err := s.Create(big); err == nil {
		t.Fatal("太大的工单要报错")
	}
	if len(s.List()) != 0 {
		t.Fatal("写不进去就不留在内存里")
	}
	if n, _, err := s.Create(mk("m2")); err != nil || n.ID != "tk-1" {
		t.Fatalf("失败不占编号：%+v %v", n, err)
	}
}

func TestCorruptFileIsRefusedAndKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tickets.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("损坏的文件要拒绝打开")
	}
	if b, _ := os.ReadFile(path); string(b) != "not json\n" {
		t.Fatal("文件保持原样")
	}
}

func TestSeq(t *testing.T) {
	for id, want := range map[string]int{"tk-7": 7, "tk-0": 0, "tk-": 0, "x-3": 0, "tk-a": 0, "": 0} {
		if got := Seq(id); got != want {
			t.Errorf("Seq(%q)=%d 想要 %d", id, got, want)
		}
	}
	if !IsEnded(StatusDone) || !IsEnded(StatusStopped) || IsEnded(StatusOpen) || IsEnded(StatusReady) || IsEnded(StatusPaused) {
		t.Fatal("IsEnded")
	}
}
