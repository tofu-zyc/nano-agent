// Package knowledge 是长期记忆的知识库：一条记忆一个 Markdown 文件，开头一段 frontmatter
// （标题、类型、相关的人、可见范围、出处、是否过时……），人物画像放在 people/ 下，
// INDEX.md 由网关在每次改动后重新生成。用户可以直接打开文件看、改：网关每次用之前看一眼
// 文件的修改时间和大小，变了就重新读。
//
// 过时的记忆不删，而是写上 invalid（什么时候、为什么），索引里不再列，搜索默认不出。
package knowledge

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// IndexFile 是自动生成的索引，不当成一条记忆。
const IndexFile = "INDEX.md"

// Types 是记忆的类型，也是索引里分组的顺序。
var Types = []string{"人物", "事实", "偏好", "事件", "经验", "流程", "知识", "看法"}

// Entry 是一条记忆。
type Entry struct {
	Path       string   `json:"path"` // 相对知识库目录，用 / 分隔
	Title      string   `json:"title"`
	Type       string   `json:"type"`
	Summary    string   `json:"summary,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	People     []string `json:"people,omitempty"`
	IDs        []string `json:"ids,omitempty"` // 人物：这个人在各个渠道的身份（消息里的 who）
	When       string   `json:"when,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"` // 看法的把握，0–1
	Visible    string   `json:"visible,omitempty"`    // all（空也是）/ admin / 某个身份
	Source     string   `json:"source,omitempty"`
	Updated    string   `json:"updated,omitempty"`
	Invalid    string   `json:"invalid,omitempty"` // 过时了：日期和原因
	Body       string   `json:"body"`

	extra [][2]string // 不认识的 frontmatter，原样留着
}

// Line 是索引里的那一句话：summary，没有就取正文第一行。
func (e Entry) Line() string {
	s := strings.TrimSpace(e.Summary)
	if s == "" {
		for _, l := range strings.Split(e.Body, "\n") {
			l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#->*"))
			if l != "" {
				s = l
				break
			}
		}
	}
	return clipRunes(s, 80)
}

// VisibleTo 是这条记忆在某个场合能不能用：群里只有 all；管理员都能；私聊里 all 和对方的身份。
func (e Entry) VisibleTo(v Viewer) bool {
	vis := strings.TrimSpace(e.Visible)
	if vis == "" || vis == "all" {
		return true
	}
	if v.Group {
		return false
	}
	if v.Admin {
		return true
	}
	return v.Who != "" && vis == v.Who
}

// Viewer 是读记忆的场合。
type Viewer struct {
	Admin bool   // 管理员的会话（以及内心活动、做梦）
	Group bool   // 群聊、网关里的群
	Who   string // 私聊里对方的身份
}

// Parse 读一个记忆文件。没有 frontmatter 时整篇都是正文，标题取第一个标题行或文件名。
func Parse(path, text string) Entry {
	e := Entry{Path: path}
	text = strings.TrimPrefix(text, "\ufeff")
	body := text
	if strings.HasPrefix(text, "---\n") || strings.HasPrefix(text, "---\r\n") {
		lines := strings.SplitAfter(text, "\n")
		end := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimRight(lines[i], "\r\n") == "---" {
				end = i
				break
			}
		}
		if end > 0 {
			e.parseFront(lines[1:end])
			body = strings.Join(lines[end+1:], "")
		}
	}
	e.Body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if e.Title == "" {
		for _, l := range strings.Split(e.Body, "\n") {
			if t := strings.TrimSpace(strings.TrimLeft(l, "#")); strings.HasPrefix(l, "#") && t != "" {
				e.Title = t
				break
			}
		}
	}
	if e.Title == "" {
		name := path[strings.LastIndex(path, "/")+1:]
		e.Title = strings.TrimSuffix(name, ".md")
	}
	if e.Type == "" {
		e.Type = "知识"
		if strings.HasPrefix(path, "people/") {
			e.Type = "人物"
		}
	}
	return e
}

func (e *Entry) parseFront(lines []string) {
	var listKey string
	var list []string
	flush := func() {
		if listKey != "" {
			e.set(listKey, "", list)
		}
		listKey, list = "", nil
	}
	for _, raw := range lines {
		l := strings.TrimRight(raw, "\r\n")
		if strings.TrimSpace(l) == "" {
			continue
		}
		if t := strings.TrimSpace(l); listKey != "" && strings.HasPrefix(t, "- ") {
			list = append(list, unquote(strings.TrimSpace(t[2:])))
			continue
		}
		flush()
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if v == "" {
			listKey = k // 下面跟着 - 列表
			continue
		}
		if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
			e.set(k, "", splitList(v[1:len(v)-1]))
			continue
		}
		e.set(k, unquote(v), nil)
	}
	flush()
}

func (e *Entry) set(k, v string, list []string) {
	one := v
	if list != nil && one == "" {
		one = strings.Join(list, ", ")
	}
	switch k {
	case "title":
		e.Title = one
	case "type":
		e.Type = one
	case "summary":
		e.Summary = one
	case "tags":
		e.Tags = orList(list, v)
	case "people":
		e.People = orList(list, v)
	case "ids":
		e.IDs = orList(list, v)
	case "when":
		e.When = one
	case "confidence":
		if f, err := strconv.ParseFloat(one, 64); err == nil {
			e.Confidence = &f
		}
	case "visible":
		e.Visible = one
	case "source":
		e.Source = one
	case "updated":
		e.Updated = one
	case "invalid":
		e.Invalid = one
	default:
		if list != nil {
			b, _ := json.Marshal(list)
			one = string(b)
		}
		e.extra = append(e.extra, [2]string{k, one})
	}
}

func orList(list []string, v string) []string {
	if list != nil {
		return list
	}
	if v == "" {
		return nil
	}
	return splitList(v)
}

// splitList 拆 a, b, "c, d"
func splitList(s string) []string {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ',' || r == '，' || r == '、':
			if t := strings.TrimSpace(cur.String()); t != "" {
				out = append(out, t)
			}
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if t := strings.TrimSpace(cur.String()); t != "" {
		out = append(out, t)
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		if s[0] == '"' {
			if u, err := strconv.Unquote(s); err == nil {
				return u
			}
		}
		return s[1 : len(s)-1]
	}
	return s
}

// Format 把一条记忆写成文件内容（键的顺序固定）。
func Format(e Entry) string {
	var b strings.Builder
	b.WriteString("---\n")
	kv := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, scalar(v))
		}
	}
	lst := func(k string, v []string) {
		if len(v) == 0 {
			return
		}
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = listItem(x)
		}
		fmt.Fprintf(&b, "%s: [%s]\n", k, strings.Join(parts, ", "))
	}
	kv("title", e.Title)
	kv("type", e.Type)
	kv("summary", e.Summary)
	lst("tags", e.Tags)
	lst("people", e.People)
	lst("ids", e.IDs)
	kv("when", e.When)
	if e.Confidence != nil {
		fmt.Fprintf(&b, "confidence: %s\n", strconv.FormatFloat(*e.Confidence, 'f', -1, 64))
	}
	kv("visible", e.Visible)
	kv("source", e.Source)
	kv("updated", e.Updated)
	kv("invalid", e.Invalid)
	for _, x := range e.extra {
		kv(x[0], x[1])
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(e.Body))
	b.WriteString("\n")
	return b.String()
}

// scalar 在值以特殊字符开头或者跨行时加引号
func scalar(v string) string {
	v = strings.TrimSpace(v)
	if strings.ContainsAny(v, "\n\r") || strings.HasPrefix(v, "[") || strings.HasPrefix(v, "\"") || strings.HasPrefix(v, "'") || strings.HasPrefix(v, "- ") {
		return strconv.Quote(v)
	}
	return v
}

func listItem(v string) string {
	v = strings.TrimSpace(v)
	if strings.ContainsAny(v, ",，、[]\"'\n") {
		return strconv.Quote(v)
	}
	return v
}

// Slug 由标题生成文件名（不含 .md）：拉丁字母转小写、中文保留，其他字符换成 -。
func Slug(title string) string {
	var b strings.Builder
	dash := false
	n := 0
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if n >= 40 {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
			n++
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "note"
	}
	return s
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
