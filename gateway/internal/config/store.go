package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// Store 管理运行中的配置。gateway.toml 是手写的底稿；网页里的修改存在数据目录的 config.json，
// 叠加在底稿上（优先级：配置文件 < 环境变量 < 网页）。改动后立即通知订阅者；
// 标了 Restart 的配置项要重启网关才生效。
type Store struct {
	path        string
	overlayPath string

	mu      sync.Mutex
	cur     *Config
	boot    *Config // 启动时的配置，用来判断哪些改动还在等重启
	md      toml.MetaData
	overlay map[string]any
	watch   []func(old, cur *Config)
}

var envKeys = map[string]string{
	"kernel.token":      "NANO_KERNEL_TOKEN",
	"llm.api_key":       "NANO_LLM_API_KEY",
	"telegram.token":    "NANO_TELEGRAM_TOKEN",
	"feishu.app_secret": "NANO_FEISHU_APP_SECRET",
}

// Open 读取配置文件和网页里保存的修改。网页的修改有问题时忽略它（不删除），只给出警告。
func Open(path string) (*Store, []string, error) {
	base, md, warnings, err := parse(path, nil)
	if err != nil {
		return nil, warnings, err
	}
	s := &Store{path: path, overlayPath: filepath.Join(base.Session.DataDir, "config.json"), overlay: map[string]any{}}
	if b, err := os.ReadFile(s.overlayPath); err == nil {
		if err := json.Unmarshal(b, &s.overlay); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s 不是合法的 JSON，已忽略：%v", s.overlayPath, err))
			s.overlay = map[string]any{}
		}
	}
	cur, md2, _, err := s.build(s.overlay)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("网页里保存的配置有误，已忽略：%v", err))
		cur, s.overlay = base, map[string]any{}
	} else {
		md = md2
	}
	s.cur, s.boot, s.md = cur, cur, md
	return s, warnings, nil
}

func (s *Store) build(ov map[string]any) (*Config, toml.MetaData, []string, error) {
	var b []byte
	if len(ov) > 0 {
		b, _ = json.Marshal(ov)
	}
	return parse(s.path, b)
}

// Current 返回当前配置。返回值不会再被修改，可以放心在别的协程里读。
func (s *Store) Current() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// OnChange 登记配置变化时的回调（在调用 Set 的协程里依次执行）。
func (s *Store) OnChange(f func(old, cur *Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watch = append(s.watch, f)
}

// Set 修改一批配置项（key 是 "llm.model" 这样的路径）。值为 nil 表示去掉网页里的修改，
// 恢复成配置文件里的值；密钥类配置项传空字符串表示不改。全部合法才会生效。
func (s *Store) Set(values map[string]any) error {
	s.mu.Lock()
	ov := cloneMap(s.overlay)
	var errs []error
	for key, v := range values {
		f, ok := FieldByKey(key)
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("没有这个配置项：%s", key))
		case f.ReadOnly:
			errs = append(errs, fmt.Errorf("%s 只能在配置文件里改", f.Label))
		case v == nil:
			deletePath(ov, key)
		case f.Type == "secret" && v == "":
		default:
			nv, err := normalize(f, v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s：%w", f.Label, err))
				continue
			}
			setPath(ov, key, nv)
		}
	}
	if len(errs) > 0 {
		s.mu.Unlock()
		return errors.Join(errs...)
	}
	cur, md, _, err := s.build(ov)
	if err == nil {
		err = s.saveOverlay(ov)
	}
	if err != nil {
		s.mu.Unlock()
		return err
	}
	old := s.cur
	s.cur, s.md, s.overlay = cur, md, ov
	watch := slices.Clone(s.watch)
	s.mu.Unlock()
	for _, f := range watch {
		f(old, cur)
	}
	return nil
}

// Reload 重新读取配置文件（手动改过 gateway.toml 之后）。
func (s *Store) Reload() error {
	s.mu.Lock()
	cur, md, _, err := s.build(s.overlay)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	old := s.cur
	s.cur, s.md = cur, md
	watch := slices.Clone(s.watch)
	s.mu.Unlock()
	for _, f := range watch {
		f(old, cur)
	}
	return nil
}

func (s *Store) saveOverlay(ov map[string]any) error {
	b, err := json.MarshalIndent(ov, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.overlayPath), 0o700); err != nil {
		return err
	}
	tmp := s.overlayPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.overlayPath)
}

// FieldState 是一个配置项的当前状态，给网页界面用。
type FieldState struct {
	Field
	Value   any    `json:"value"`
	Source  string `json:"source"`            // file | web | env | default
	IsSet   bool   `json:"is_set,omitempty"`  // 密钥类：是否已经设置（值本身不返回）
	Pending bool   `json:"pending,omitempty"` // 改过了，但要重启网关才生效
}

// Describe 返回所有配置项的当前值和来源。密钥只告诉有没有设置。
func (s *Store) Describe() []FieldState {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, boot := asMap(s.cur), asMap(s.boot)
	out := make([]FieldState, 0, len(Fields))
	for _, f := range Fields {
		v, _ := getPath(cur, f.Key)
		st := FieldState{Field: f, Value: v, Source: "default"}
		parts := strings.Split(f.Key, ".")
		_, inOverlay := getPath(s.overlay, f.Key)
		switch {
		case inOverlay:
			st.Source = "web"
		case envKeys[f.Key] != "" && os.Getenv(envKeys[f.Key]) != "":
			st.Source = "env"
		case s.md.IsDefined(parts...):
			st.Source = "file"
		}
		if f.Type == "secret" {
			st.IsSet = v != nil && v != ""
			st.Value = ""
		}
		if f.Restart {
			bv, _ := getPath(boot, f.Key)
			st.Pending = !reflect.DeepEqual(v, bv)
		}
		out = append(out, st)
	}
	return out
}

// normalize 检查网页传来的值是否符合配置项的类型，并整理成写进 JSON 的样子
func normalize(f Field, v any) (any, error) {
	switch f.Type {
	case "string", "secret", "proxy":
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("应该是文本")
		}
		return strings.TrimSpace(s), nil
	case "enum":
		s, ok := v.(string)
		if !ok || !slices.Contains(f.Options, s) {
			return nil, fmt.Errorf("只能是 %s", strings.Join(f.Options, "、"))
		}
		return s, nil
	case "int":
		n, ok := v.(float64)
		if !ok || n != math.Trunc(n) {
			return nil, errors.New("应该是整数")
		}
		return n, nil
	case "float":
		n, ok := v.(float64)
		if !ok {
			return nil, errors.New("应该是数字")
		}
		return n, nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("应该是开或关")
		}
		return b, nil
	case "multi":
		arr, ok := v.([]any)
		if !ok {
			return nil, errors.New("应该是一组选项")
		}
		out := []string{}
		for _, s := range f.Fixed { // 固定值总是选中，而且排在最前面
			out = append(out, s)
		}
		for _, it := range arr {
			s, ok := it.(string)
			if !ok || !slices.Contains(f.Options, s) {
				return nil, fmt.Errorf("只能是 %s", strings.Join(f.Options, "、"))
			}
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
		return out, nil
	case "list":
		arr, ok := v.([]any)
		if !ok {
			return nil, errors.New("应该是列表")
		}
		out := []any{}
		for _, it := range arr {
			s, ok := it.(string)
			if !ok {
				return nil, errors.New("列表里应该都是文本")
			}
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	case "json":
		if _, ok := v.(map[string]any); !ok {
			return nil, errors.New("应该是 JSON 对象")
		}
		return v, nil
	}
	return nil, fmt.Errorf("不支持的类型 %s", f.Type)
}

func asMap(c *Config) map[string]any {
	b, _ := json.Marshal(c)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func getPath(m map[string]any, key string) (any, bool) {
	parts := strings.Split(key, ".")
	var cur any = m
	for _, p := range parts {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func setPath(m map[string]any, key string, v any) {
	parts := strings.Split(key, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}

func deletePath(m map[string]any, key string) {
	parts := strings.Split(key, ".")
	if len(parts) == 1 {
		delete(m, key)
		return
	}
	next, ok := m[parts[0]].(map[string]any)
	if !ok {
		return
	}
	deletePath(next, strings.Join(parts[1:], "."))
	if len(next) == 0 {
		delete(m, parts[0])
	}
}

func cloneMap(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	out := map[string]any{}
	json.Unmarshal(b, &out)
	return out
}
