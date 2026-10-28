package persona

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPresets(t *testing.T) {
	ps := Presets()
	if len(ps) < 2 || ps[0].ID != "xiaona" || ps[1].ID != "dafeiyu" {
		t.Fatalf("预设顺序：%+v", ps)
	}
	for _, p := range ps {
		if p.Name == "" || p.Desc == "" || strings.HasPrefix(p.Text, "---") || strings.Contains(p.Text, "\ndesc:") {
			t.Errorf("预设 %s 没解析好：%+v", p.ID, p)
		}
		// 消息格式和工具的说明每个预设都要有，不然时间戳、群聊昵称、[系统] 行会被当成用户说的话
		for _, must := range []string{"## 消息格式", "「[系统]」", "## 工具", "不要编造工具结果"} {
			if !strings.Contains(p.Text, must) {
				t.Errorf("预设 %s 缺少：%s", p.ID, must)
			}
		}
	}
	if p, ok := FindPreset("大肥鱼"); !ok || p.ID != "dafeiyu" {
		t.Fatal("按名字找不到大肥鱼")
	}
	if _, ok := FindPreset("DAFEIYU"); !ok {
		t.Fatal("按 ID 找（不分大小写）")
	}
	if _, ok := FindPreset("没有"); ok {
		t.Fatal("找到了不存在的预设")
	}
}

// 自带的 agents/default/persona.md 就是小纳预设，两份要一样（v0.5 起人格在 agent 目录里）。
func TestShippedDefaultIsXiaona(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "agents", "default", "persona.md"))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := MatchPreset(string(b))
	if !ok || p.ID != "xiaona" {
		t.Fatal("agents/default/persona.md 和 presets/xiaona.md 的正文不一样了，改一份要同步另一份")
	}
}

func TestWriteRemove(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := FindPreset("dafeiyu")
	name := FileFor("tg:-100123")
	if err := l.Write(name, p.Text); err != nil {
		t.Fatal(err)
	}
	if text, path := l.For("tg:-100123"); path != filepath.Join(dir, name) || !strings.Contains(text, "大肥鱼") {
		t.Fatalf("For: %q %q", path, text)
	}
	if err := l.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := l.Remove(name); err != nil {
		t.Fatal("删不存在的文件不算错：", err)
	}
	if l.Remove("default.md") == nil || l.Write("../x.md", "x") == nil {
		t.Fatal("不该能删 default.md 或写到目录外")
	}
}
