// Package repos 是 GitHub 仓库连接（设计 §16.3）：data/repos.json（token 和 webhook 密钥加密存）、
// GitHub API 客户端、事件归一化、按 GitHub 对象去重的状态日志、轮询、webhook 的签名校验和解析。
// 不依赖 app：事件交给调用方的处理函数。token 只在这里用，不进错误信息、不进日志。
package repos

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"nano-agent/gateway/internal/scene"
	"nano-agent/gateway/internal/secret"
)

// Repo 是一个仓库连接。
type Repo struct {
	ID        string     `json:"id"`
	Workspace string     `json:"workspace"` // 本地克隆所在的工作目录
	Owner     string     `json:"owner"`
	Name      string     `json:"name"`
	Base      string     `json:"base"`               // 默认分支，空表示取 origin/HEAD
	Agent     string     `json:"agent,omitempty"`    // 议题会话由哪个 agent 回答，空是默认 agent
	Token     string     `json:"token"`              // 加密存（internal/secret）
	Bot       string     `json:"bot"`                // token 对应的 GitHub 账号，保存时用 token 查出来
	Intake    string     `json:"intake"`             // poll（默认）| webhook
	Interval  int        `json:"interval,omitempty"` // 轮询间隔秒，默认 120，最少 60
	Secret    string     `json:"secret,omitempty"`   // webhook 的签名密钥，加密存
	Team      scene.Team `json:"team"`               // 议题对话的团队规则（名单用 gh:<账号>）；Workspace 以 Repo.Workspace 为准
}

// 轮询间隔。
const (
	DefaultInterval = 120
	MinInterval     = 60
)

var namePart = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// Slug 是 owner/name。
func (r Repo) Slug() string { return r.Owner + "/" + r.Name }

// EffectiveInterval 是生效的轮询间隔（秒）。
func (r Repo) EffectiveInterval() int {
	switch {
	case r.Interval <= 0:
		return DefaultInterval
	case r.Interval < MinInterval:
		return MinInterval
	}
	return r.Interval
}

// EffectiveTeam 是议题会话的团队规则：Workspace 以连接为准。
func (r Repo) EffectiveTeam() scene.Team {
	t := r.Team
	t.Workspace = r.Workspace
	return t
}

// PlainToken 是解密后的 token（只在网关里用）。
func (r Repo) PlainToken() (string, error) { return secret.Decrypt(r.Token) }

// PlainSecret 是解密后的 webhook 密钥。
func (r Repo) PlainSecret() (string, error) { return secret.Decrypt(r.Secret) }

// Validate 检查连接（不连网）。
func (r Repo) Validate() error {
	if !namePart.MatchString(r.Owner) || !namePart.MatchString(r.Name) {
		return errors.New("仓库的 owner 和 name 只能用字母、数字、点、下划线、短横线")
	}
	if strings.TrimSpace(r.Workspace) == "" {
		return errors.New("要指定本地克隆所在的工作目录")
	}
	if strings.TrimSpace(r.Token) == "" {
		return errors.New("要填 token")
	}
	switch r.Intake {
	case "", "poll", "webhook":
	default:
		return errors.New("事件怎么进来只能是 poll 或 webhook")
	}
	if r.Intake == "webhook" && strings.TrimSpace(r.Secret) == "" {
		return errors.New("选了 webhook 就要配签名密钥")
	}
	if r.Interval < 0 {
		return errors.New("轮询间隔不能是负数")
	}
	if strings.ContainsAny(r.Base, " \t\r\n") {
		return errors.New("默认分支名里不能有空白")
	}
	t := r.EffectiveTeam()
	for _, who := range t.Roster {
		if !strings.HasPrefix(who, "gh:") {
			return fmt.Errorf("议题对话的名单要写 gh:<账号>，%q 不对", who)
		}
	}
	return scene.Rules{Team: &t}.Validate(scene.External, nil, nil)
}

// Store 是 data/repos.json。线程安全。
type Store struct {
	mu    sync.Mutex
	path  string
	repos []Repo
}

// Open 打开（不存在就是空的）。文件损坏返回错误，文件保持原样。
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.repos); err != nil {
		return nil, fmt.Errorf("读取 repos.json：%w", err)
	}
	return s, nil
}

// List 是全部连接的副本（token 和密钥是加密的形式）。
func (s *Store) List() []Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Repo, len(s.repos))
	copy(out, s.repos)
	return out
}

// Get 按 ID 找。
func (s *Store) Get(id string) (Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.repos {
		if r.ID == id {
			return r, true
		}
	}
	return Repo{}, false
}

// BySlug 按 owner/name 找。
func (s *Store) BySlug(slug string) (Repo, bool) {
	for _, r := range s.List() {
		if strings.EqualFold(r.Slug(), slug) {
			return r, true
		}
	}
	return Repo{}, false
}

// ByWorkspace 找连在这个工作目录上的仓库。
func (s *Store) ByWorkspace(ws string) (Repo, bool) {
	for _, r := range s.List() {
		if r.Workspace == ws {
			return r, true
		}
	}
	return Repo{}, false
}

// Put 新增或替换一个连接（ID 空就新建）。token、密钥在这里加密；写盘失败不改内存。
func (s *Store) Put(r Repo) (Repo, error) {
	if err := r.Validate(); err != nil {
		return Repo{}, err
	}
	var err error
	if r.Token, err = secret.Encrypt(r.Token); err != nil {
		return Repo{}, errors.New("加密 token 失败")
	}
	if r.Secret, err = secret.Encrypt(r.Secret); err != nil {
		return Repo{}, errors.New("加密密钥失败")
	}
	if r.Intake == "" {
		r.Intake = "poll"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.repos {
		if o.ID != r.ID && (o.Workspace == r.Workspace || strings.EqualFold(o.Slug(), r.Slug())) {
			return Repo{}, errors.New("这个工作目录或这个仓库已经连过了")
		}
	}
	next := append([]Repo(nil), s.repos...)
	if r.ID == "" {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		r.ID = hex.EncodeToString(b)
		next = append(next, r)
	} else {
		found := false
		for i := range next {
			if next[i].ID == r.ID {
				next[i], found = r, true
			}
		}
		if !found {
			return Repo{}, errors.New("没有这个连接")
		}
	}
	if err := s.save(next); err != nil {
		return Repo{}, err
	}
	s.repos = next
	return r, nil
}

// Delete 删掉一个连接。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next []Repo
	for _, r := range s.repos {
		if r.ID != id {
			next = append(next, r)
		}
	}
	if len(next) == len(s.repos) {
		return errors.New("没有这个连接")
	}
	if err := s.save(next); err != nil {
		return err
	}
	s.repos = next
	return nil
}

func (s *Store) save(repos []Repo) error {
	b, err := json.MarshalIndent(repos, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
