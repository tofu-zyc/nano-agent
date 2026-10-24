package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAndFormat(t *testing.T) {
	text := "---\ntitle: 张三\ntype: 人物\nsummary: 产品经理，说话直\ntags: [同事, \"产品, 组\"]\nids:\n  - tg:123\n  - feishu:ou_x\nconfidence: 0.7\nvisible: all\nmood: 开心\n---\n\n# 张三\n\n养了两只猫。\n"
	e := Parse("people/zhang.md", text)
	if e.Title != "张三" || e.Type != "人物" || len(e.Tags) != 2 || e.Tags[1] != "产品, 组" || len(e.IDs) != 2 || e.IDs[1] != "feishu:ou_x" {
		t.Fatalf("解析不对：%+v", e)
	}
	if e.Confidence == nil || *e.Confidence != 0.7 {
		t.Fatalf("confidence 没读出来：%v", e.Confidence)
	}
	again := Parse(e.Path, Format(e))
	if again.Title != e.Title || strings.Join(again.Tags, "|") != strings.Join(e.Tags, "|") || again.Body != e.Body || !strings.Contains(Format(again), "mood: 开心") {
		t.Fatalf("写出来再读不一样：%+v\n%s", again, Format(again))
	}
	// 没有 frontmatter：标题取第一个标题行，类型按目录猜
	plain := Parse("people/li.md", "# 李四\n\n喜欢爬山")
	if plain.Title != "李四" || plain.Type != "人物" || plain.Line() != "李四" && plain.Line() != "喜欢爬山" {
		t.Fatalf("没有 frontmatter 的文件：%+v", plain)
	}
}

func TestTokens(t *testing.T) {
	got := strings.Join(Tokens("产品经理 likes Go1.22，猫"), "|")
	if got != "产品|品经|经理|likes|go1|22|猫" {
		t.Fatalf("切词不对：%s", got)
	}
}

func TestStoreWriteSearchIndex(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	zhang, err := s.Write(Entry{Title: "张三", Type: "人物", Summary: "产品经理，养了两只猫", IDs: []string{"tg:123"}, Body: "说话直，周末喜欢爬山。"})
	if err != nil || zhang.Path != "people/张三.md" {
		t.Fatalf("写人物：%+v %v", zhang, err)
	}
	if _, err := s.Write(Entry{Title: "张三", Type: "人物", Body: "另一个张三"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("people/张三-2.md"); !ok {
		t.Fatal("重名应该加 -2")
	}
	trip, _ := s.Write(Entry{Title: "国庆去成都", Type: "事件", People: []string{"张三"}, When: "2026-10-01", Visible: "admin", Body: "张三国庆去成都吃火锅。"})
	if _, err := s.Write(Entry{Title: "x", Type: "随便"}); err == nil {
		t.Fatal("不认识的类型应该报错")
	}
	// 搜索：中文关键词
	hits := s.Search("成都火锅", SearchOptions{})
	if len(hits) == 0 || hits[0].Path != trip.Path || !strings.Contains(hits[0].Snippet, "成都") {
		t.Fatalf("搜不到：%+v", hits)
	}
	// 可见范围：群里看不到 admin 的
	group := func(e Entry) bool { return e.VisibleTo(Viewer{Group: true}) }
	if hits := s.Search("成都火锅", SearchOptions{Keep: group}); len(hits) != 0 {
		t.Fatalf("群里不该看到 admin 的记忆：%+v", hits)
	}
	if !trip.VisibleTo(Viewer{Admin: true}) || trip.VisibleTo(Viewer{Who: "tg:9"}) {
		t.Fatal("admin 的记忆只有管理员能看")
	}
	// 按人找
	if p, ok := s.Person("tg:123", ""); !ok || p.Title != "张三" {
		t.Fatalf("按身份找人：%+v", p)
	}
	// 按人列：这个人的画像和跟他有关的记忆
	if hits := s.Search("", SearchOptions{Person: "张三", Type: "事件"}); len(hits) != 1 || hits[0].Path != trip.Path {
		t.Fatalf("按人列：%+v", hits)
	}
	if hits := s.Search("", SearchOptions{Person: "张三"}); len(hits) != 3 {
		t.Fatalf("按人列（含画像）：%+v", hits)
	}
	// 索引文件
	b, err := os.ReadFile(filepath.Join(dir, IndexFile))
	if err != nil || !strings.Contains(string(b), "## 人物") || !strings.Contains(string(b), "- 张三 — 产品经理，养了两只猫（people/张三.md）") {
		t.Fatalf("索引：%s %v", b, err)
	}
	// 标失效：索引里没了，搜索默认不出，带上 include 能搜到
	if _, err := s.Invalidate(trip.Path, "其实没去", "2026-10-02"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.IndexText(nil), "国庆去成都") {
		t.Fatal("过时的不该进索引")
	}
	if len(s.Search("成都", SearchOptions{})) != 0 || len(s.Search("成都", SearchOptions{IncludeInvalid: true})) != 1 {
		t.Fatal("过时的默认不该搜出来")
	}
	// 精确替换
	if _, err := s.Edit(zhang.Path, "爬山", "钓鱼", "2026-10-03"); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get(zhang.Path); !strings.Contains(e.Body, "钓鱼") || e.Updated != "2026-10-03" || len(e.IDs) != 1 {
		t.Fatalf("替换后：%+v", e)
	}
	if _, err := s.Edit(zhang.Path, "没有这段", "x", ""); err == nil {
		t.Fatal("找不到 old 应该报错")
	}
	// 听到名字想起来
	if m := s.Mentioned("今天张三说他要搬家", nil); len(m) == 0 || m[0].Title != "张三" {
		t.Fatalf("提到名字：%+v", m)
	}
}

func TestStoreSeesOutsideEdits(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	if len(s.List()) != 0 {
		t.Fatal("空目录")
	}
	if _, err := os.Stat(filepath.Join(dir, IndexFile)); err == nil {
		t.Fatal("空知识库不该为了索引建文件")
	}
	os.MkdirAll(filepath.Join(dir, "people"), 0o700)
	os.WriteFile(filepath.Join(dir, "people", "wang.md"), []byte("---\ntitle: 王五\ntype: 人物\n---\n同学"), 0o600)
	s.mu.Lock()
	s.checked = time.Time{}
	s.mu.Unlock()
	if e, ok := s.Get("people/wang.md"); !ok || e.Title != "王五" {
		t.Fatalf("用户自己加的文件没读到：%+v", e)
	}
	os.Remove(filepath.Join(dir, "people", "wang.md"))
	s.mu.Lock()
	s.checked = time.Time{}
	s.mu.Unlock()
	if len(s.List()) != 0 {
		t.Fatal("删掉的文件还在")
	}
}

func TestCleanPath(t *testing.T) {
	for _, bad := range []string{"../x.md", "/etc/x.md", "a/../../x.md", "x.txt", "INDEX.md", ".hidden/x.md", "a/b/c/d.md", ""} {
		if _, err := clean(bad); err == nil {
			t.Fatalf("%q 应该被拒", bad)
		}
	}
	for _, ok := range []string{"x.md", "people/张三.md", "a/b/c.md"} {
		if _, err := clean(ok); err != nil {
			t.Fatalf("%q 不该被拒：%v", ok, err)
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Alice Li": "alice-li", "张三": "张三", "  !!! ": "note", "国庆/成都": "国庆-成都"} {
		if got := Slug(in); got != want {
			t.Fatalf("Slug(%q)=%q，期望 %q", in, got, want)
		}
	}
}
