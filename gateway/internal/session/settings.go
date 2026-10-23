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
	// Workspaces 是挂载的工作目录（工作目录列表里的 ID，app/workspaces.go），可以有好几个、在不同的机器上。
	// WorkspacesSet 为 false 时用新会话的默认工作目录，第一次说话时定下来；为 true 而列表为空表示只聊天。
	Workspaces    []string `json:"workspaces,omitempty"`
	WorkspacesSet bool     `json:"workspaces_set,omitempty"`
	// 旧版本：只能挂一个工作目录（启动时迁移到 Workspaces）
	Workspace    string `json:"workspace,omitempty"`
	WorkspaceSet bool   `json:"workspace_set,omitempty"`
	Mode         string `json:"mode,omitempty"` // 空表示用配置里的默认模式

	// 旧版本的设置（机器和目录分开设），启动时迁移到工作目录列表后清掉
	Node    string   `json:"node,omitempty"`
	DirsSet bool     `json:"dirs_set,omitempty"`
	Dirs    []string `json:"dirs,omitempty"`
	RO      []string `json:"ro,omitempty"`

	Thinking string `json:"thinking,omitempty"` // 思考强度，空表示用模型的默认值
	Model    string `json:"model,omitempty"`    // 用哪个模型（models 包里的 ID），空表示用默认模型
	Title    string `json:"title,omitempty"`    // 会话的名字，网页里显示
	Web      bool   `json:"web,omitempty"`      // 网页接着这个会话：在网页里新建的，或者在网页里说过话

	// EnvNote 是上一次告诉模型的环境说明。环境变了才在下一条用户消息前追加新的说明，
	// 不去改系统提示词，前缀缓存就不会失效。
	EnvNote string `json:"env_note,omitempty"`

	// Base 是历史文件里已经被 Compact 丢掉的消息数；Summary 代替被丢掉的部分，放在历史最前面。
	Meter       Meter  `json:"meter,omitempty"`
	CompactedAt int64  `json:"compacted_at,omitempty"`
	Base        int    `json:"base,omitempty"`
	Summary     string `json:"summary,omitempty"`
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
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.settingsFile(sid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.settingsFile(sid)); err != nil {
		return err
	}
	s.settings[sid] = st
	return nil
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
