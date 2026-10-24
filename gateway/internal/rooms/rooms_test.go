package rooms

import (
	"os"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "rooms.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateUpdateDelete(t *testing.T) {
	s := openTemp(t)
	r, err := s.Create(Room{Name: "产品组", Members: []string{"default", "bob", "bob"}})
	if err != nil {
		t.Fatalf("建群失败：%v", err)
	}
	if len(r.Members) != 2 || r.Mode != ModeMention || r.ID == "" {
		t.Fatalf("建出来的群不对：%+v", r)
	}
	r2, err := s.Create(Room{Name: "产品组", Members: []string{"default"}})
	if err != nil || r2.Name != "产品组 2" {
		t.Fatalf("同名应该加编号：%+v %v", r2, err)
	}
	if _, err := s.Create(Room{Name: "空的"}); err == nil {
		t.Fatal("没有成员的群应该报错")
	}
	got, _ := s.Get(r.ID)
	got.Paused = true
	got.Mode = ModeFree
	got.Name = "新名字"
	if err := s.Update(got); err != nil {
		t.Fatalf("改群失败：%v", err)
	}
	again, _ := s.Get(r.ID)
	if !again.Paused || again.Mode != ModeFree || again.Name != "新名字" {
		t.Fatalf("改完不对：%+v", again)
	}
	if x, ok := s.FindByName("新名字"); !ok || x.ID != r.ID {
		t.Fatalf("按名字找不到：%+v %v", x, ok)
	}
	if err := s.Delete(r.ID); err != nil {
		t.Fatalf("删群失败：%v", err)
	}
	if _, ok := s.Get(r.ID); ok {
		t.Fatal("删掉以后还能查到")
	}
	b, err := os.ReadFile(s.path)
	if err != nil || len(b) == 0 {
		t.Fatalf("没有写进磁盘：%v", err)
	}
}
