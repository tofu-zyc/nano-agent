// Package skills 管理网关上的技能：每个技能一个目录，里面是带 name / description 的 SKILL.md，
// 以及可选的脚本、资料。模型平时只看到名字和简介（Skill 工具的说明），调用 Skill 时才读正文；
// 附带文件由网关放到执行工具的机器上（~/.nano-agent/skills/<名字>/，只读可见）。
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
)

const (
	maxSkills  = 200
	maxMD      = 256 << 10
	maxDesc    = 1024
	maxListed  = 60
	maxBytes   = 4 << 20 // 附带文件最多传这么多
	maxFiles   = 500
	maxDepth   = 2
	CacheDir   = "~/.nano-agent/skills" // 执行工具的机器上放技能文件的地方
	toolPrompt = "使用一个技能（skill）。技能是事先写好的、做某一类事情的方法和配套脚本：调用后返回它的说明，照着做。" +
		"说明里提到的附带文件在每台机器的 " + CacheDir + "/<技能名>/ 下（只读），用 Read 读、用 Shell 运行，" +
		"在哪个工作目录上执行就用那台机器上的这个目录。遇到下面这些事情时，先调用对应的技能再动手。可用的技能："
)

var validName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Skill 是一个技能的简介。
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Dir         string `json:"dir"`
}

// Registry 是一个技能目录。并发安全。
type Registry struct {
	mu   sync.Mutex
	dir  string
	list []Skill
	sig  string
}

func New(dir string) *Registry { return &Registry{dir: dir} }

func (r *Registry) Dir() string { return r.dir }

// Scan 重新扫描技能目录，名字或简介有变化时返回 true（工具定义要跟着变）。
func (r *Registry) Scan() bool {
	var list []Skill
	ents, _ := os.ReadDir(r.dir)
	seen := map[string]bool{}
	for _, e := range ents {
		if len(list) >= maxSkills || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(r.dir, e.Name())
		md, err := readSmall(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			continue
		}
		name, desc, _ := parse(md)
		if !validName.MatchString(name) {
			name = e.Name()
		}
		if !validName.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		if desc == "" {
			desc = "（没有简介）"
		}
		list = append(list, Skill{Name: name, Description: clip(desc, maxDesc), Dir: dir})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	h := sha256.New()
	for _, s := range list {
		fmt.Fprintf(h, "%s\x00%s\x00", s.Name, s.Description)
	}
	sig := hex.EncodeToString(h.Sum(nil))
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := sig != r.sig
	r.list, r.sig = list, sig
	return changed
}

// List 返回技能列表（按名字排序）。
func (r *Registry) List() []Skill {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Skill(nil), r.list...)
}

func (r *Registry) find(name string) (Skill, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.list {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// ToolSpec 是给模型的 Skill 工具定义；没有技能时返回 false。只随技能的名字和简介变化，方便前缀缓存。
func (r *Registry) ToolSpec() (proto.ToolSpec, bool) {
	list := r.List()
	if len(list) == 0 {
		return proto.ToolSpec{}, false
	}
	var b strings.Builder
	b.WriteString(toolPrompt)
	names := make([]string, 0, len(list))
	for _, s := range list {
		fmt.Fprintf(&b, "\n- %s：%s", s.Name, s.Description)
		names = append(names, s.Name)
	}
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "技能的名字", "enum": names},
		},
		"required": []string{"name"},
	})
	return proto.ToolSpec{Name: "Skill", Description: b.String(), InputSchema: schema}, true
}

// Load 返回技能的说明（给模型看的工具结果）和附带文件。没有这个技能时 ok 为 false，text 是说明。
func (r *Registry) Load(name string) (text string, files []host.File, hash string, ok bool) {
	s, found := r.find(name)
	if !found {
		if len(r.List()) == 0 {
			return "没有这个技能：" + name + "。网关上没有安装技能", nil, "", false
		}
		return "没有这个技能：" + name + "。可用的技能见 Skill 工具的说明", nil, "", false
	}
	md, err := readSmall(filepath.Join(s.Dir, "SKILL.md"))
	if err != nil {
		return fmt.Sprintf("读不了技能 %s 的说明：%v", s.Name, err), nil, "", false
	}
	_, _, body := parse(md)
	files, err = collect(s.Dir)
	if err != nil {
		return fmt.Sprintf("技能 %s 的附带文件：%v", s.Name, err), nil, "", false
	}
	h := sha256.New()
	var listed []string
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%v\x00", f.Path, f.Exec)
		h.Write(f.Data)
		if f.Path != "SKILL.md" {
			listed = append(listed, f.Path)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "技能 %s\n技能目录：%s/%s（只读；说明里提到的文件都在这个目录下，每台机器上都是这个位置，~ 是那台机器的 HOME）",
		s.Name, CacheDir, s.Name)
	if len(listed) > 0 {
		more := ""
		if len(listed) > maxListed {
			listed, more = listed[:maxListed], "……"
		}
		b.WriteString("\n附带文件：" + strings.Join(listed, "、") + more)
	}
	b.WriteString("\n\n" + strings.TrimLeft(body, "\r\n"))
	return b.String(), files, hex.EncodeToString(h.Sum(nil))[:16], true
}

// collect 读技能目录里的文件（最多两层，不跟随符号链接，总量有限）。
func collect(dir string) ([]host.File, error) {
	var files []host.File
	total := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if strings.Count(rel, string(filepath.Separator)) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // 符号链接之类的跳过
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += int(info.Size())
		if total > maxBytes || len(files) >= maxFiles {
			return fmt.Errorf("太大了（最多 %d MB、%d 个文件）", maxBytes>>20, maxFiles)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, host.File{Path: filepath.ToSlash(rel), Data: data, Exec: info.Mode()&0o111 != 0})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

func readSmall(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, maxMD)
	n, _ := f.Read(buf)
	return strings.ToValidUTF8(string(buf[:n]), "�"), nil
}

// parse 解析 SKILL.md：frontmatter 里的 name、description（支持 > 和 | 的多行写法），以及正文。
func parse(md string) (name, desc, body string) {
	body = md
	if !strings.HasPrefix(md, "---\n") && !strings.HasPrefix(md, "---\r\n") {
		return
	}
	lines := strings.SplitAfter(md, "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r\n") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return
	}
	body = strings.Join(lines[end+1:], "")
	var blockKey string
	var folded bool
	var block []string
	flush := func() {
		if blockKey == "" {
			return
		}
		sep := "\n"
		if folded {
			sep = " "
		}
		set(&name, &desc, blockKey, strings.Join(block, sep))
		blockKey, block = "", nil
	}
	for _, raw := range lines[1:end] {
		line := strings.TrimRight(raw, "\r\n")
		if blockKey != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			block = append(block, strings.TrimSpace(line))
			continue
		}
		flush()
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "#") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "name" && k != "description" {
			continue
		}
		switch v {
		case "", ">", ">-", "|", "|-":
			blockKey, folded = k, !strings.HasPrefix(v, "|")
			continue
		}
		set(&name, &desc, k, unquote(v))
	}
	flush()
	return
}

func set(name, desc *string, k, v string) {
	if k == "name" && *name == "" {
		*name = v
	} else if k == "description" && *desc == "" {
		*desc = v
	}
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
