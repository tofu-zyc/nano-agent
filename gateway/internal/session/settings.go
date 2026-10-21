package session

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Settings 是单个会话覆盖默认值的设置，存在 data/sessions/<会话ID>.settings.json。
type Settings struct {
	DirsSet bool     `json:"dirs_set,omitempty"` // 为 true 时用下面的 Dirs，否则用配置里的默认工作目录
	Dirs    []string `json:"dirs,omitempty"`     // 工作目录，第一个是默认目录
	RO      []string `json:"ro,omitempty"`       // 只读的工作目录
	Mode    string   `json:"mode,omitempty"`     // 空表示用配置里的默认模式

	Thinking string `json:"thinking,omitempty"` // 思考强度，空表示用配置里的默认值
	Title    string `json:"title,omitempty"`    // 会话的名字，网页里显示
	Web      bool   `json:"web,omitempty"`      // 网页接着这个会话：在网页里新建的，或者在网页里说过话

	// EnvNote 是上一次告诉模型的环境说明。环境变了才在下一条用户消息前追加新的说明，
	// 不去改系统提示词，前缀缓存就不会失效。
	EnvNote string `json:"env_note,omitempty"`

	// Base 是历史文件里已经被 Compact 丢掉的消息数；Summary 代替被丢掉的部分，放在历史最前面。
	Base    int    `json:"base,omitempty"`
	Summary string `json:"summary,omitempty"`
}

func (s *Store) settingsFile(sid string) string {
	return filepath.Join(s.dir, "sessions", url.PathEscape(sid)+".settings.json")
}

// Settings 返回会话设置（第一次访问时从磁盘加载）。
func (s *Store) Settings(sid string) Settings {
	if st, ok := s.settings[sid]; ok {
		return st
	}
	var st Settings
	if b, err := os.ReadFile(s.settingsFile(sid)); err == nil {
		json.Unmarshal(b, &st)
	}
	s.settings[sid] = st
	return st
}

// SaveSettings 保存会话设置。
func (s *Store) SaveSettings(sid string, st Settings) error {
	s.settings[sid] = st
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.settingsFile(sid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.settingsFile(sid))
}

// List 返回磁盘上所有会话的 ID（有历史或者有设置的）。
func (s *Store) List() []string {
	entries, _ := os.ReadDir(filepath.Join(s.dir, "sessions"))
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		name := e.Name()
		var esc string
		switch {
		case strings.HasSuffix(name, ".settings.json"):
			esc = strings.TrimSuffix(name, ".settings.json")
		case strings.HasSuffix(name, ".jsonl"):
			esc = strings.TrimSuffix(name, ".jsonl")
		default:
			continue
		}
		sid, err := url.PathUnescape(esc)
		if err != nil || seen[sid] {
			continue
		}
		seen[sid] = true
		out = append(out, sid)
	}
	return out
}
