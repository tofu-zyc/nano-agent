// Package models 管理可以用的模型：好几个接口（OpenAI 兼容），每个接口上若干个模型，
// 一个默认模型，每个会话可以单独选。所有接口和模型都只存在数据目录的 models.json。
//
// 旧版本只有一个模型，写在 gateway.toml 的 [llm] 里；升级后第一次启动会把它一次性
// 导入成普通接口和普通模型（见 importLegacy），之后 [llm] 不再参与模型列表。
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
	"net/url"
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

// ConfigEndpoint 是旧配置模型迁移后的接口 ID。只为了兼容已经引用 config/模型名 的会话设置，
// 没有任何特殊行为，界面上也不显示它。
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
	Modalities     []string `json:"modalities,omitempty"` // 能接收什么：text、image；空表示只有 text
	Owner          string   `json:"owned_by,omitempty"`
}

type file struct {
	Endpoints []Endpoint `json:"endpoints"`
	Models    []Model    `json:"models"`
	Default   string     `json:"default,omitempty"`
	Seeded    bool       `json:"seeded,omitempty"` // 已经处理过旧配置（或确认没有旧模型），不再导入
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
	// 旧版本没有 modalities：按纯文本处理；写坏的也按纯文本，不挡住启动
	for i := range s.f.Models {
		if mods, err := normalizeModalities(s.f.Models[i].Modalities); err == nil {
			s.f.Models[i].Modalities = mods
		} else {
			s.f.Models[i].Modalities = []string{"text"}
		}
	}
	// 旧配置里的那一个模型只导入一次。导入后它和别的模型完全一样，可以改、可以删；
	// seeded 防止删掉以后重启又冒出来。
	if !s.f.Seeded {
		s.importLegacy()
		s.f.Seeded = true
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// importLegacy 把旧版 [llm] 里的单个模型导入成普通接口 + 普通模型。只在第一次启动时调用。
func (s *Store) importLegacy() {
	l := s.cfg.LLM
	name := strings.TrimSpace(l.Model)
	if name == "" && l.Provider == "mock" {
		name = "mock"
	}
	if name == "" {
		return
	}
	if !slices.ContainsFunc(s.f.Endpoints, func(e Endpoint) bool { return e.ID == ConfigEndpoint }) {
		s.f.Endpoints = append(s.f.Endpoints, Endpoint{
			ID: ConfigEndpoint, Name: legacyEndpointName(l), Kind: l.Provider,
			BaseURL: l.BaseURL, APIKey: l.APIKey, Proxy: l.Proxy,
		})
	}
	id := ConfigEndpoint + "/" + name
	if !slices.ContainsFunc(s.f.Models, func(m Model) bool { return m.ID == id }) {
		mods, err := normalizeModalities(l.Modalities)
		if err != nil {
			mods = []string{"text"}
		}
		s.f.Models = append(s.f.Models, Model{
			ID: id, Endpoint: ConfigEndpoint, Name: name, ContextWindow: l.ContextWindow, MaxTokens: l.MaxTokens,
			MaxHistory: l.MaxHistoryTokens, Thinking: l.Thinking, ThinkingLevels: l.ThinkingLevels, Modalities: mods,
		})
	}
	if s.f.Default == "" || !slices.ContainsFunc(s.f.Models, func(m Model) bool { return m.ID == s.f.Default }) {
		s.f.Default = id
	}
}

// legacyEndpointName 给导入的接口起一个像样的名字（地址里的主机名），不叫“配置文件”。
func legacyEndpointName(l config.LLM) string {
	if l.Provider == "mock" {
		return "假模型"
	}
	if u, err := url.Parse(l.BaseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "模型接口"
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

// Endpoints 返回所有接口
func (s *Store) Endpoints() []Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Endpoint{}, s.f.Endpoints...)
}

// Models 返回所有模型
func (s *Store) Models() []Model {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Model{}, s.f.Models...)
}

// Default 是默认模型的 ID：设过的，否则第一个；没有模型时为空
func (s *Store) Default() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.f.Models {
		if m.ID == s.f.Default {
			return s.f.Default
		}
	}
	if len(s.f.Models) > 0 {
		return s.f.Models[0].ID
	}
	return ""
}

// Find 按 ID 找模型和它的接口
func (s *Store) Find(id string) (Model, Endpoint, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.f.Models {
		if m.ID != id {
			continue
		}
		for _, e := range s.f.Endpoints {
			if e.ID == m.Endpoint {
				return m, e, true
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
	l.MaxTokens = m.MaxTokens // 最大输出属于模型；0 表示不限制
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
	mods, err := normalizeModalities(m.Modalities)
	if err != nil {
		return m, err
	}
	m.Modalities = mods
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
	ID            string   `json:"id"`
	Owner         string   `json:"owned_by,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
	Modalities    []string `json:"modalities,omitempty"` // 接口报的输入类型（OpenRouter 之类才有）
	Added         bool     `json:"added"`                // 已经在列表里了
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
		x := Remote{ID: id, Owner: owner, ContextWindow: contextOf(d), Modalities: modalitiesOf(d), Added: have[e.ID+"/"+id]}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// normalizeModalities 检查并整理输入类型：去重、小写，文字总是有（text），空表示只有 text。
func normalizeModalities(in []string) ([]string, error) {
	out := []string{}
	for _, x := range in {
		switch x = strings.ToLower(strings.TrimSpace(x)); x {
		case "text", "image", "audio", "video":
			if !slices.Contains(out, x) {
				out = append(out, x)
			}
		case "":
		default:
			return nil, fmt.Errorf("不认识的输入类型 %q（只有 text、image、audio、video）", x)
		}
	}
	if !slices.Contains(out, "text") {
		out = append([]string{"text"}, out...)
	}
	return out, nil
}

// modalitiesOf 读接口报的输入类型：OpenRouter 的 architecture.input_modalities，或者 input_modalities
func modalitiesOf(d map[string]any) []string {
	var raw []any
	if arch, ok := d["architecture"].(map[string]any); ok {
		raw, _ = arch["input_modalities"].([]any)
	}
	if raw == nil {
		raw, _ = d["input_modalities"].([]any)
	}
	var out []string
	for _, v := range raw {
		s, _ := v.(string)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
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
