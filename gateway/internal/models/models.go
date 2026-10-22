// Package models 管理可以用的模型：好几个端点（OpenAI 兼容接口），每个端点上若干个模型，
// 一个默认模型，每个会话可以单独选。
//
// 配置文件里的 [llm] 是内置的端点 config 和它上面的一个模型（改它用原来的设置项）；
// 在网页里加的端点和模型存在数据目录的 models.json（只有自己能读，里面有 API key）。
package models

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/httpx"
)

// ConfigEndpoint 是配置文件里 [llm] 那个端点的 ID
const ConfigEndpoint = "config"

// Endpoint 是一个模型服务的接口。
type Endpoint struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"` // openai | mock
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
	Proxy   string `json:"proxy,omitempty"` // 空表示用网关的默认代理，none 表示直连
}

// Model 是一个可以选的模型。
type Model struct {
	ID             string   `json:"id"` // 端点ID/模型名
	Endpoint       string   `json:"endpoint"`
	Name           string   `json:"name"` // 接口里的模型名
	Label          string   `json:"label,omitempty"`
	ContextWindow  int      `json:"context_window,omitempty"`
	MaxTokens      int      `json:"max_tokens,omitempty"`
	MaxHistory     int      `json:"max_history_tokens,omitempty"` // 历史上限，0 表示按上下文窗口算
	Thinking       string   `json:"thinking,omitempty"`           // 默认的思考强度（reasoning_effort），空表示不传
	ThinkingLevels []string `json:"thinking_levels,omitempty"`
	Owner          string   `json:"owned_by,omitempty"`
}

type file struct {
	Endpoints []Endpoint `json:"endpoints"`
	Models    []Model    `json:"models"`
	Default   string     `json:"default,omitempty"`
}

// Store 是模型列表。并发安全。
type Store struct {
	mu   sync.Mutex
	path string
	f    file
	cfg  *config.Config
}

func Open(dataDir string, cfg *config.Config) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, "models.json"), cfg: cfg}
	b, err := os.ReadFile(s.path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &s.f); err != nil {
			return nil, fmt.Errorf("%s 读不了：%w", s.path, err)
		}
	}
	return s, nil
}

// SetConfig 换上新的配置（[llm] 改了的时候）
func (s *Store) SetConfig(cfg *config.Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// configEndpoint 和 configModel 是配置文件里 [llm] 的那个端点和模型
func (s *Store) configEndpoint() Endpoint {
	l := s.cfg.LLM
	return Endpoint{ID: ConfigEndpoint, Name: "配置文件", Kind: l.Provider, BaseURL: l.BaseURL, APIKey: l.APIKey, Proxy: l.Proxy}
}

func (s *Store) configModel() (Model, bool) {
	l := s.cfg.LLM
	if l.Model == "" && l.Provider != "mock" {
		return Model{}, false
	}
	name := l.Model
	if name == "" {
		name = "mock"
	}
	return Model{ID: ConfigEndpoint + "/" + name, Endpoint: ConfigEndpoint, Name: name, ContextWindow: l.ContextWindow,
		MaxTokens: l.MaxTokens, MaxHistory: l.MaxHistoryTokens, Thinking: l.Thinking, ThinkingLevels: l.ThinkingLevels}, true
}

// Endpoints 返回所有端点（配置文件的在最前面）
func (s *Store) Endpoints() []Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Endpoint{s.configEndpoint()}, s.f.Endpoints...)
}

// Models 返回所有模型（配置文件的在最前面）
func (s *Store) Models() []Model {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Model
	if m, ok := s.configModel(); ok {
		out = append(out, m)
	}
	return append(out, s.f.Models...)
}

// Default 是默认模型的 ID：设过的，否则配置文件里的，再否则第一个
func (s *Store) Default() string {
	list := s.Models()
	s.mu.Lock()
	def := s.f.Default
	s.mu.Unlock()
	for _, m := range list {
		if m.ID == def {
			return def
		}
	}
	if len(list) > 0 {
		return list[0].ID
	}
	return ""
}

// Find 按 ID 找模型和它的端点
func (s *Store) Find(id string) (Model, Endpoint, bool) {
	for _, m := range s.Models() {
		if m.ID == id {
			for _, e := range s.Endpoints() {
				if e.ID == m.Endpoint {
					return m, e, true
				}
			}
		}
	}
	return Model{}, Endpoint{}, false
}

// LLMConfig 是请求这个模型用的设置（交给 llm 包），没写的沿用配置文件里 [llm] 的
func (s *Store) LLMConfig(m Model, e Endpoint) config.LLM {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	l := cfg.LLM
	l.Provider, l.BaseURL, l.APIKey, l.Model = e.Kind, e.BaseURL, e.APIKey, m.Name
	l.Proxy = cfg.ProxyFor(e.Proxy)
	if m.MaxTokens > 0 {
		l.MaxTokens = m.MaxTokens
	}
	if e.ID != ConfigEndpoint {
		l.Extra = nil // [llm.extra] 是给配置文件里那个接口的
	}
	return l
}

func newID() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// SaveEndpoint 添加或修改一个端点（ID 为空时添加）。APIKey 为空表示不改原来的。
func (s *Store) SaveEndpoint(e Endpoint) (Endpoint, error) {
	e.Name, e.BaseURL = strings.TrimSpace(e.Name), strings.TrimRight(strings.TrimSpace(e.BaseURL), "/")
	if e.Kind == "" {
		e.Kind = "openai"
	}
	if e.Kind != "openai" && e.Kind != "mock" {
		return e, errors.New("接口类型只能是 openai（OpenAI 兼容）或 mock")
	}
	if e.Kind == "openai" && !strings.HasPrefix(e.BaseURL, "http://") && !strings.HasPrefix(e.BaseURL, "https://") {
		return e, errors.New("接口地址要以 http:// 或 https:// 开头，比如 https://api.deepseek.com/v1")
	}
	if e.Name == "" {
		e.Name = e.BaseURL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == ConfigEndpoint {
		return e, errors.New("配置文件里的端点在设置里改")
	}
	if e.ID == "" {
		e.ID = newID()
		s.f.Endpoints = append(s.f.Endpoints, e)
	} else {
		i := slices.IndexFunc(s.f.Endpoints, func(x Endpoint) bool { return x.ID == e.ID })
		if i < 0 {
			return e, errors.New("没有这个端点")
		}
		if e.APIKey == "" {
			e.APIKey = s.f.Endpoints[i].APIKey
		}
		s.f.Endpoints[i] = e
	}
	return e, s.save()
}

// DeleteEndpoint 删掉一个端点和它上面的模型
func (s *Store) DeleteEndpoint(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.f.Endpoints)
	s.f.Endpoints = slices.DeleteFunc(s.f.Endpoints, func(x Endpoint) bool { return x.ID == id })
	if len(s.f.Endpoints) == n {
		return errors.New("没有这个端点")
	}
	s.f.Models = slices.DeleteFunc(s.f.Models, func(x Model) bool { return x.Endpoint == id })
	return s.save()
}

// SaveModel 添加或修改一个模型（按 ID，ID 是 端点/模型名）
func (s *Store) SaveModel(m Model) (Model, error) {
	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" {
		return m, errors.New("模型名不能为空")
	}
	if m.Endpoint == ConfigEndpoint {
		return m, errors.New("配置文件里的模型在设置里改")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.ContainsFunc(s.f.Endpoints, func(e Endpoint) bool { return e.ID == m.Endpoint }) {
		return m, errors.New("没有这个端点")
	}
	m.ID = m.Endpoint + "/" + m.Name
	if i := slices.IndexFunc(s.f.Models, func(x Model) bool { return x.ID == m.ID }); i >= 0 {
		s.f.Models[i] = m
	} else {
		s.f.Models = append(s.f.Models, m)
	}
	return m, s.save()
}

func (s *Store) DeleteModel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.f.Models)
	s.f.Models = slices.DeleteFunc(s.f.Models, func(x Model) bool { return x.ID == id })
	if len(s.f.Models) == n {
		return errors.New("没有这个模型（配置文件里的在设置里改）")
	}
	return s.save()
}

func (s *Store) SetDefault(id string) error {
	if _, _, ok := s.Find(id); !ok {
		return errors.New("没有这个模型")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Default = id
	return s.save()
}

// Remote 是端点的 /models 列出来的一个模型
type Remote struct {
	ID            string `json:"id"`
	Owner         string `json:"owned_by,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	Added         bool   `json:"added"` // 已经在列表里了
}

// Fetch 从端点的 /models 拉取模型列表。上下文长度各家放的字段不一样，能认出来的都填上。
func (s *Store) Fetch(ctx context.Context, endpoint string) ([]Remote, error) {
	var e Endpoint
	for _, x := range s.Endpoints() {
		if x.ID == endpoint {
			e = x
		}
	}
	if e.ID == "" {
		return nil, errors.New("没有这个端点")
	}
	if e.Kind == "mock" {
		return []Remote{{ID: "mock", Owner: "nano-agent"}}, nil
	}
	s.mu.Lock()
	proxy := s.cfg.ProxyFor(e.Proxy)
	s.mu.Unlock()
	hc, err := httpx.NewClient(proxy, 20*time.Second)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上 %s：%w", e.BaseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s/models 返回 %d：%s", e.BaseURL, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)])))
	}
	var r struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Data == nil {
		return nil, fmt.Errorf("%s/models 返回的不是 OpenAI 格式的模型列表", e.BaseURL)
	}
	have := map[string]bool{}
	for _, m := range s.Models() {
		have[m.ID] = true
	}
	var out []Remote
	for _, d := range r.Data {
		id, _ := d["id"].(string)
		if id == "" {
			continue
		}
		owner, _ := d["owned_by"].(string)
		x := Remote{ID: id, Owner: owner, ContextWindow: contextOf(d), Added: have[e.ID+"/"+id]}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// contextOf 认出上下文长度：vLLM 的 max_model_len、OpenRouter 的 context_length，以及别的常见写法
func contextOf(d map[string]any) int {
	for _, k := range []string{"max_model_len", "context_length", "context_window", "max_context_length", "max_input_tokens", "input_token_limit"} {
		if v, ok := d[k].(float64); ok && v > 0 {
			return int(v)
		}
	}
	if tp, ok := d["top_provider"].(map[string]any); ok {
		if v, ok := tp["context_length"].(float64); ok {
			return int(v)
		}
	}
	return 0
}
