package session

import (
	"os"
	"testing"
)

// Append 必须“先落盘、成功才进内存”：写失败时内存里不能看起来已经写进去了。
func TestAppendFailureKeepsMemoryClean(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append("s1", Msg{Role: "user", Content: "第一条"}); err != nil {
		t.Fatal(err)
	}
	// 把历史文件换成一个目录：追加必然失败
	path := s.file("s1")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("s1", Msg{Role: "user", Content: "第二条"}); err == nil {
		t.Fatal("写盘失败却返回成功")
	}
	if got := len(s.History("s1")); got != 1 {
		t.Fatalf("内存历史被污染：%d 条", got)
	}
}
