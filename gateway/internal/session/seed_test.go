package session

import "testing"

// Seed 用一份消息建立新会话：重新打开后还在；已经存在的拒绝；没有消息什么都不建。
func TestSeedCreatesNewSessionOnly(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	msgs := []Msg{{Role: "user", Content: "甲", TS: 5}, {Role: "assistant", Content: "乙"}}
	if err := s.Seed("room:new", msgs); err != nil {
		t.Fatal(err)
	}
	if h := s.History("room:new"); len(h) != 2 || h[0].Content != "甲" || h[0].TS != 5 || h[1].TS == 0 {
		t.Fatalf("历史：%+v", h)
	}
	if err := s.Append("room:new", Msg{Role: "user", Content: "丙"}); err != nil {
		t.Fatal(err)
	}
	// 重新打开：三条都在
	s2, _ := New(dir)
	if h := s2.History("room:new"); len(h) != 3 || h[2].Content != "丙" {
		t.Fatalf("重新打开后：%+v", h)
	}
	if err := s2.Seed("room:new", msgs); err == nil {
		t.Fatal("已存在的会话要拒绝")
	}
	if h := s2.History("room:new"); len(h) != 3 {
		t.Fatalf("拒绝之后原会话不能变：%+v", h)
	}
	if err := s2.Seed("room:empty", nil); err != nil || s2.Exists("room:empty") {
		t.Fatalf("没有消息什么都不建：%v %v", err, s2.Exists("room:empty"))
	}
	// 删掉再 Seed 可以（新一代）
	if err := s2.Delete("room:new"); err != nil {
		t.Fatal(err)
	}
	if err := s2.Seed("room:new", msgs[:1]); err != nil {
		t.Fatal(err)
	}
	if h := s2.History("room:new"); len(h) != 1 {
		t.Fatalf("删掉再建：%+v", h)
	}
}
