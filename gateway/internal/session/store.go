// Package session 保存每个会话的对话历史：内存一份，磁盘上每个会话一个 JSONL 文件（只追加）。
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

// Msg 是历史里的一条消息。Reasoning 只保存备查，默认不回传给模型。
type Msg struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Reasoning  string         `json:"reasoning,omitempty"`
	TS         int64          `json:"ts,omitempty"`
}

type Store struct {
	dir        string
	keepTokens int // 内存里最多保留多少（估算的）token，超出时从最老的一轮开始丢
	hist       map[string][]Msg
	settings   map[string]Settings
}

func New(dir string, keepTokens int) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, keepTokens: keepTokens, hist: map[string][]Msg{}, settings: map[string]Settings{}}, nil
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
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Role != "" {
			h = append(h, m)
		}
	}
	return Trim(h, s.keepTokens)
}

// Append 追加一条消息，同时写入磁盘。
func (s *Store) Append(sid string, m Msg) error {
	if m.TS == 0 {
		m.TS = time.Now().Unix()
	}
	h := append(s.History(sid), m)
	if Estimate(h) > 2*s.keepTokens {
		h = Trim(h, s.keepTokens)
	}
	s.hist[sid] = h

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

// Reset 清空会话历史（磁盘文件一并删除）。工作目录等设置保留，但环境说明要在新的历史里重新给一次。
func (s *Store) Reset(sid string) error {
	s.hist[sid] = nil
	if st := s.Settings(sid); st.EnvNote != "" {
		st.EnvNote = ""
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
