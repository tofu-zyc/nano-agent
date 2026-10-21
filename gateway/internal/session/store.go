// Package session 保存每个会话的对话历史：内存一份，磁盘上每个会话一个 JSONL 文件（只追加）。
//
// 发给模型的历史只追加不改动，这样每次请求都是上一次请求的延续，模型那边的前缀缓存才能命中。
// 历史太长时由 Compact 一次性丢掉开头的一大段（记下丢到了哪里），而不是每次都丢一点。
//
// Store 不加锁，由调用方（app）串行访问。
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"nano-agent/gateway/internal/llm"
)

// Msg 是历史里的一条消息。Reasoning 和 Usage 只保存备查，默认不回传给模型。
type Msg struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Reasoning  string         `json:"reasoning,omitempty"`
	Usage      *llm.Usage     `json:"usage,omitempty"`
	TS         int64          `json:"ts,omitempty"`
}

type Store struct {
	dir      string
	hist     map[string][]Msg // 从 Settings.Base 开始的历史
	settings map[string]Settings
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, hist: map[string][]Msg{}, settings: map[string]Settings{}}, nil
}

// Dir 是数据目录。
func (s *Store) Dir() string { return s.dir }

func (s *Store) file(sid string) string {
	return filepath.Join(s.dir, "sessions", url.PathEscape(sid)+".jsonl")
}

// History 返回会话历史（第一次访问时从磁盘加载）。调用方不要修改返回的切片。
func (s *Store) History(sid string) []Msg {
	if h, ok := s.hist[sid]; ok {
		return h
	}
	h := s.load(sid)
	s.hist[sid] = h
	return h
}

func (s *Store) load(sid string) []Msg {
	f, err := os.Open(s.file(sid))
	if err != nil {
		return nil
	}
	defer f.Close()
	var h []Msg
	skip := s.Settings(sid).Base
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Role == "" {
			continue
		}
		if skip > 0 {
			skip--
			continue
		}
		h = append(h, m)
	}
	return h
}

// Append 追加一条消息，同时写入磁盘。
func (s *Store) Append(sid string, m Msg) error {
	if m.TS == 0 {
		m.TS = time.Now().Unix()
	}
	s.hist[sid] = append(s.History(sid), m)

	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.file(sid), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("保存会话历史：%w", err)
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// Compact 在历史估算超过 high 时，一次性丢掉开头的一段，让剩下的不超过 low。
// 只在用户消息处切，当前这一轮永远保留。返回丢掉的消息数。
//
// 被丢掉的部分用 Settings.Summary 里的一段说明代替（现在只是说明加上环境信息，
// 以后做上下文压缩时换成摘要），它会放在剩下的历史最前面，直到下一次 Compact。
func (s *Store) Compact(sid string, high, low int) (int, error) {
	h := s.History(sid)
	if Estimate(h) <= high {
		return 0, nil
	}
	cut := CutPoint(h, low)
	if cut == 0 {
		return 0, nil
	}
	st := s.Settings(sid)
	st.Base += cut
	st.Summary = "[系统] 更早的对话因为太长已经省略。"
	if st.EnvNote != "" {
		st.Summary += "\n" + st.EnvNote
	}
	if err := s.SaveSettings(sid, st); err != nil {
		return 0, err
	}
	s.hist[sid] = append([]Msg(nil), h[cut:]...)
	return cut, nil
}

// Reset 清空会话历史（磁盘文件一并删除）。工作目录等设置保留，但环境说明要在新的历史里重新给一次。
func (s *Store) Reset(sid string) error {
	s.hist[sid] = nil
	if st := s.Settings(sid); st.EnvNote != "" || st.Base != 0 || st.Summary != "" {
		st.EnvNote, st.Base, st.Summary = "", 0, ""
		if err := s.SaveSettings(sid, st); err != nil {
			return err
		}
	}
	err := os.Remove(s.file(sid))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Exists 判断磁盘上有没有这个会话（有历史或者有设置）。
func (s *Store) Exists(sid string) bool {
	for _, f := range []string{s.file(sid), s.settingsFile(sid)} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// Rename 把会话的历史和设置换一个 ID（升级旧数据用）。新 ID 不能已经存在。
func (s *Store) Rename(from, to string) error {
	if s.Exists(to) {
		return fmt.Errorf("会话 %s 已经存在", to)
	}
	for _, p := range [][2]string{{s.file(from), s.file(to)}, {s.settingsFile(from), s.settingsFile(to)}} {
		if err := os.Rename(p[0], p[1]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	delete(s.hist, from)
	delete(s.settings, from)
	delete(s.hist, to)
	delete(s.settings, to)
	return nil
}

// Delete 删除会话的历史和设置。
func (s *Store) Delete(sid string) error {
	for _, f := range []string{s.file(sid), s.settingsFile(sid)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	delete(s.hist, sid)
	delete(s.settings, sid)
	return nil
}
