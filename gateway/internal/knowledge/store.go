package knowledge

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxFiles    = 5000
	maxFileSize = 256 << 10
	maxDepth    = 3
)

// Store 是一个知识库目录。并发安全。
type Store struct {
	dir string

	mu      sync.Mutex
	stamps  map[string]stamp // 相对路径 → 读进来时的修改时间和大小
	entries map[string]Entry
	ix      *index
	checked time.Time
	index   string // 上次写进 INDEX.md 的内容
}

type stamp struct {
	mod  time.Time
	size int64
}

// Open 打开一个知识库目录（不存在也行，第一次写入时建）。
func Open(dir string) *Store {
	return &Store{dir: dir, stamps: map[string]stamp{}, entries: map[string]Entry{}}
}

// Dir 是知识库目录。
func (s *Store) Dir() string { return s.dir }

// refresh 看一眼文件有没有变（最多一秒一次），变了重新读、重建索引。调用方持有 s.mu。
func (s *Store) refresh(force bool) {
	if !force && time.Since(s.checked) < time.Second && s.ix != nil {
		return
	}
	s.checked = time.Now()
	seen := map[string]bool{}
	changed := s.ix == nil
	count := 0
	filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(s.dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || strings.Count(rel, "/") >= maxDepth) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") || rel == IndexFile || strings.HasPrefix(d.Name(), ".") || count >= maxFiles {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileSize {
			return nil
		}
		count++
		seen[rel] = true
		st := stamp{info.ModTime(), info.Size()}
		if old, ok := s.stamps[rel]; ok && old == st {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		s.stamps[rel] = st
		s.entries[rel] = Parse(rel, string(b))
		changed = true
		return nil
	})
	for rel := range s.entries {
		if !seen[rel] {
			delete(s.entries, rel)
			delete(s.stamps, rel)
			changed = true
		}
	}
	if changed {
		s.ix = buildIndex(s.sortedLocked())
		s.writeIndexLocked()
	}
}

func (s *Store) sortedLocked() []Entry {
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := typeRank(out[i].Type), typeRank(out[j].Type)
		if a != b {
			return a < b
		}
		if out[i].Title != out[j].Title {
			return out[i].Title < out[j].Title
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func typeRank(t string) int {
	if i := slices.Index(Types, t); i >= 0 {
		return i
	}
	return len(Types)
}

// List 是所有记忆（按类型、标题排序），包括过时的。
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh(false)
	return s.sortedLocked()
}

// Get 按路径取一条。
func (s *Store) Get(p string) (Entry, bool) {
	p, err := clean(p)
	if err != nil {
		return Entry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh(false)
	e, ok := s.entries[p]
	return e, ok
}

// Raw 是文件原文（网页编辑用）。
func (s *Store) Raw(p string) (string, error) {
	p, err := clean(p)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(p)))
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("没有这条记忆：%s", p)
	}
	return string(b), err
}

// IndexText 是索引（每条一行，按类型分组，过时的不列）；keep 为 nil 时全部。
func (s *Store) IndexText(keep func(Entry) bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh(false)
	return indexText(s.sortedLocked(), keep, true)
}

func indexText(list []Entry, keep func(Entry) bool, withLine bool) string {
	var b strings.Builder
	cur := ""
	for _, e := range list {
		if e.Invalid != "" || (keep != nil && !keep(e)) {
			continue
		}
		if e.Type != cur {
			cur = e.Type
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "## %s\n", cur)
		}
		if line := e.Line(); withLine && line != "" && line != e.Title {
			fmt.Fprintf(&b, "- %s — %s（%s）\n", e.Title, line, e.Path)
		} else {
			fmt.Fprintf(&b, "- %s（%s）\n", e.Title, e.Path)
		}
	}
	return b.String()
}

// IndexTitles 和 IndexText 一样，但只有标题（索引太长时用）。
func (s *Store) IndexTitles(keep func(Entry) bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh(false)
	return indexText(s.sortedLocked(), keep, false)
}

func (s *Store) writeIndexLocked() {
	text := indexText(s.sortedLocked(), nil, true)
	if len(s.entries) == 0 {
		if _, err := os.Stat(filepath.Join(s.dir, IndexFile)); err != nil {
			return // 还没有记忆：不为了一个空索引去建文件
		}
	}
	full := "# 知识库索引\n\n网关自动生成，每次改动后重写，别手改。\n\n" + text
	if full == s.index {
		return
	}
	if err := os.MkdirAll(s.dir, 0o700); err == nil {
		if writeFile(filepath.Join(s.dir, IndexFile), full) == nil {
			s.index = full
		}
	}
}

// SearchOptions 是搜索的条件。
type SearchOptions struct {
	Type           string
	Person         string // 相关的人（people 或者人物条目的标题）
	IncludeInvalid bool
	Keep           func(Entry) bool // 可见性等额外过滤
	Limit          int
}

// Search 用关键词搜，返回最相关的几条。
func (s *Store) Search(query string, o SearchOptions) []Hit {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh(false)
	if o.Limit <= 0 {
		o.Limit = 8
	}
	res, terms := s.ix.search(query + " " + o.Person)
	var out []Hit
	for _, r := range res {
		e := s.entries[r.path]
		if !s.passes(e, o) {
			continue
		}
		out = append(out, Hit{Entry: e, Score: r.score, Snippet: snippet(e, query, 160), Matched: r.matched, Terms: terms})
		if len(out) >= o.Limit {
			break
		}
	}
	// 没有关键词但给了条件（比如只按人找）：按条件列出
	if strings.TrimSpace(query) == "" && len(out) == 0 && (o.Type != "" || o.Person != "") {
		for _, e := range s.sortedLocked() {
			if s.passes(e, o) {
				out = append(out, Hit{Entry: e, Snippet: e.Line()})
				if len(out) >= o.Limit {
					break
				}
			}
		}
	}
	return out
}

func (s *Store) passes(e Entry, o SearchOptions) bool {
	if e.Invalid != "" && !o.IncludeInvalid {
		return false
	}
	if o.Type != "" && e.Type != o.Type {
		return false
	}
	if o.Person != "" && !(e.Title == o.Person && e.Type == "人物") && !slices.Contains(e.People, o.Person) {
		return false
	}
	return o.Keep == nil || o.Keep(e)
}

// Mentioned 是标题（至少两个字）出现在这段文字里的记忆，人物在前。用来“听到名字就想起这个人”。
func (s *Store) Mentioned(text string, keep func(Entry) bool) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh(false)
	lower := strings.ToLower(text)
	var out []Entry
	for _, e := range s.sortedLocked() {
		t := strings.ToLower(strings.TrimSpace(e.Title))
		if e.Invalid != "" || len([]rune(t)) < 2 || !strings.Contains(lower, t) || (keep != nil && !keep(e)) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Person 按身份找人物画像（ids 里有它），找不到再按名字（标题）找。
func (s *Store) Person(who, name string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh(false)
	list := s.sortedLocked()
	if who != "" {
		for _, e := range list {
			if e.Type == "人物" && e.Invalid == "" && slices.Contains(e.IDs, who) {
				return e, true
			}
		}
	}
	if name = strings.TrimSpace(name); name != "" {
		for _, e := range list {
			if e.Type == "人物" && e.Invalid == "" && strings.EqualFold(e.Title, name) {
				return e, true
			}
		}
	}
	return Entry{}, false
}

// Write 新建或整条覆盖一条记忆。Path 为空时按标题生成（人物放 people/），重名加 -2、-3。
func (s *Store) Write(e Entry) (Entry, error) {
	e.Title = strings.TrimSpace(e.Title)
	if e.Title == "" {
		return Entry{}, errors.New("要有标题（title）")
	}
	if e.Type == "" {
		e.Type = "知识"
	}
	if !slices.Contains(Types, e.Type) {
		return Entry{}, fmt.Errorf("类型（type）只能是：%s", strings.Join(Types, "、"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh(true)
	if e.Path == "" {
		dir := ""
		if e.Type == "人物" {
			dir = "people/"
		}
		base := dir + Slug(e.Title)
		e.Path = base + ".md"
		for i := 2; ; i++ {
			if _, taken := s.entries[e.Path]; !taken {
				break
			}
			e.Path = fmt.Sprintf("%s-%d.md", base, i)
		}
	} else {
		p, err := clean(e.Path)
		if err != nil {
			return Entry{}, err
		}
		e.Path = p
		if old, ok := s.entries[p]; ok && len(e.extra) == 0 {
			e.extra = old.extra
		}
	}
	if err := s.saveLocked(e); err != nil {
		return Entry{}, err
	}
	return s.entries[e.Path], nil
}

// WriteRaw 用原文覆盖一条（网页里直接改 Markdown）。
func (s *Store) WriteRaw(p, text string) (Entry, error) {
	p, err := clean(p)
	if err != nil {
		return Entry{}, err
	}
	e := Parse(p, text)
	if !slices.Contains(Types, e.Type) {
		return Entry{}, fmt.Errorf("类型（type）只能是：%s", strings.Join(Types, "、"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.putLocked(p, text); err != nil {
		return Entry{}, err
	}
	return s.entries[p], nil
}

// Edit 把正文或 frontmatter 里唯一的一段 old 换成 new（和 Edit 工具一样）。
func (s *Store) Edit(p, old, new, updated string) (Entry, error) {
	if old == "" {
		return Entry{}, errors.New("old 不能为空")
	}
	raw, err := s.Raw(p)
	if err != nil {
		return Entry{}, err
	}
	switch strings.Count(raw, old) {
	case 0:
		return Entry{}, errors.New("没找到 old 这段文字（要和文件里一字不差）")
	case 1:
	default:
		return Entry{}, errors.New("old 这段文字出现了不止一次，多带一点上下文让它唯一")
	}
	p, _ = clean(p)
	e := Parse(p, strings.Replace(raw, old, new, 1))
	if updated != "" {
		e.Updated = updated
	}
	if !slices.Contains(Types, e.Type) {
		return Entry{}, fmt.Errorf("类型（type）只能是：%s", strings.Join(Types, "、"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveLocked(e); err != nil {
		return Entry{}, err
	}
	return s.entries[e.Path], nil
}

// Invalidate 把一条标成过时（不删）：写上日期和原因。
func (s *Store) Invalidate(p, reason, date string) (Entry, error) {
	e, ok := s.Get(p)
	if !ok {
		return Entry{}, fmt.Errorf("没有这条记忆：%s", p)
	}
	e.Invalid = strings.TrimSpace(date + " " + strings.TrimSpace(reason))
	if e.Invalid == "" {
		e.Invalid = "过时了"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveLocked(e); err != nil {
		return Entry{}, err
	}
	return s.entries[e.Path], nil
}

// Delete 真删一条。
func (s *Store) Delete(p string) error {
	p, err := clean(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(filepath.Join(s.dir, filepath.FromSlash(p))); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("没有这条记忆：%s", p)
		}
		return err
	}
	s.refresh(true)
	return nil
}

func (s *Store) saveLocked(e Entry) error { return s.putLocked(e.Path, Format(e)) }

// putLocked 写文件，并直接更新缓存（不靠修改时间：同一秒内连着改两次、长度又一样时，修改时间可能不变）。
func (s *Store) putLocked(rel, text string) error {
	full := filepath.Join(s.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	if err := writeFile(full, text); err != nil {
		return err
	}
	s.refresh(true)
	if info, err := os.Stat(full); err == nil {
		s.stamps[rel] = stamp{info.ModTime(), info.Size()}
	}
	s.entries[rel] = Parse(rel, text)
	s.ix = buildIndex(s.sortedLocked())
	s.writeIndexLocked()
	return nil
}

// Counts 是条数：全部、人物、过时的。
func (s *Store) Counts() (total, people, invalid int) {
	for _, e := range s.List() {
		total++
		if e.Type == "人物" {
			people++
		}
		if e.Invalid != "" {
			invalid++
		}
	}
	return
}

// clean 检查相对路径：只能在知识库里面，以 .md 结尾，不能是索引。
func clean(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return "", errors.New("路径不能为空")
	}
	c := path.Clean(p)
	if path.IsAbs(c) || c == "." || c == ".." || strings.HasPrefix(c, "../") || strings.Contains(c, "/../") {
		return "", fmt.Errorf("路径 %s 不在知识库里", p)
	}
	if !strings.HasSuffix(c, ".md") {
		return "", errors.New("记忆文件要以 .md 结尾")
	}
	if c == IndexFile {
		return "", errors.New("INDEX.md 是自动生成的索引，不能直接改")
	}
	if strings.Count(c, "/") >= maxDepth {
		return "", errors.New("目录层次太深")
	}
	for _, part := range strings.Split(c, "/") {
		if strings.HasPrefix(part, ".") {
			return "", fmt.Errorf("路径 %s 不能有以 . 开头的部分", p)
		}
	}
	return c, nil
}

func writeFile(p, text string) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
