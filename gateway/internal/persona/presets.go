package persona

// 程序自带的人格预设（presets/*.md，编译进程序，跟着程序更新）。
// 套用预设就是把它的正文写进 default.md 或某个会话的专属人格文件，之后可以随便改。
// 预设文件开头是一段 --- 包起来的 name / desc，后面是正文。

import (
	"embed"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed presets/*.md
var presetFS embed.FS

type Preset struct {
	ID   string `json:"id"`   // 文件名（不带 .md）
	Name string `json:"name"` // 显示的名字
	Desc string `json:"desc"`
	Text string `json:"text"` // 人格正文
}

// presetOrder：排在前面的预设，其余按 ID 排。
var presetOrder = []string{"xiaona", "dafeiyu"}

var presets = loadPresets()

func loadPresets() []Preset {
	entries, err := presetFS.ReadDir("presets")
	if err != nil {
		panic(err)
	}
	var out []Preset
	for _, e := range entries {
		b, err := presetFS.ReadFile("presets/" + e.Name())
		if err != nil {
			panic(err)
		}
		p := parsePreset(strings.TrimSuffix(e.Name(), ".md"), string(b))
		if p.Name == "" || p.Text == "" {
			panic("人格预设缺 name 或正文：" + e.Name())
		}
		out = append(out, p)
	}
	rank := func(id string) int {
		for i, o := range presetOrder {
			if o == id {
				return i
			}
		}
		return len(presetOrder)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i].ID), rank(out[j].ID)
		if ri != rj {
			return ri < rj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func parsePreset(id, s string) Preset {
	p := Preset{ID: id}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if rest, ok := strings.CutPrefix(s, "---\n"); ok {
		if head, body, ok := strings.Cut(rest, "\n---\n"); ok {
			for _, line := range strings.Split(head, "\n") {
				k, v, _ := strings.Cut(line, ":")
				switch strings.TrimSpace(k) {
				case "name":
					p.Name = strings.TrimSpace(v)
				case "desc":
					p.Desc = strings.TrimSpace(v)
				}
			}
			s = body
		}
	}
	p.Text = strings.TrimSpace(s) + "\n"
	return p
}

// Presets 返回全部预设（小纳在最前面）。
func Presets() []Preset { return presets }

// FindPreset 按 ID 或名字找预设（不分大小写）。
func FindPreset(key string) (Preset, bool) {
	key = strings.TrimSpace(key)
	for _, p := range presets {
		if strings.EqualFold(p.ID, key) || strings.EqualFold(p.Name, key) {
			return p, true
		}
	}
	return Preset{}, false
}

// MatchPreset：这段人格正文是不是原样的某个预设。
func MatchPreset(text string) (Preset, bool) {
	text = strings.TrimSpace(text)
	for _, p := range presets {
		if strings.TrimSpace(p.Text) == text {
			return p, true
		}
	}
	return Preset{}, false
}

// Write 把人格正文写进人格目录里的文件（先写临时文件再改名）。
func (l *Loader) Write(name, text string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return errors.New("人格文件名不合法：" + name)
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(l.dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Remove 删掉人格目录里的文件（本来就没有不算错）。
func (l *Loader) Remove(name string) error {
	if name == "" || name == "default.md" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return errors.New("不能删除这个人格文件：" + name)
	}
	err := os.Remove(filepath.Join(l.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
