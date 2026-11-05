// Package extagent 是外部 agent 的登记（设计 §16.4）：Claude Code、Codex 这样的命令行程序，登记成命令模板，
// 存在 data/external-agents.json。登记只管“是什么、怎么跑”；在哪个工作目录能用由 Workspace.External 开启。
package extagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

// PromptSlot 是命令模板里要换成这次要求的那一项。
const PromptSlot = "{prompt}"

const (
	DefaultTimeout = 1800 // 秒
	MaxTimeout     = 86400
	maxArgv        = 32
	maxDirs        = 8
	maxEnv         = 16 // 和执行器的 env 上限一样
	maxEnvBytes    = 8 << 10
	maxItem        = 1024
)

// Agent 是一个登记的外部 agent。
type Agent struct {
	Name    string            `json:"name"`              // claude、codex
	Argv    []string          `json:"argv"`              // 命令模板，{prompt} 换成这次的要求
	Dirs    []string          `json:"dirs"`              // 它自己的配置和登录目录，可读写（~/ 开头，执行器按那台机器真实的 HOME 展开）
	Env     map[string]string `json:"env,omitempty"`     // 比如 CLAUDE_CONFIG_DIR、CODEX_HOME：放路径和开关，不放密钥
	Timeout int               `json:"timeout,omitempty"` // 秒，默认 1800
}

// EffectiveTimeout 是时限（秒）。
func (a Agent) EffectiveTimeout() int {
	if a.Timeout <= 0 {
		return DefaultTimeout
	}
	return a.Timeout
}

var (
	nameRE    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	envNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// ValidEnvName 是 16.3 的 env 名字规则和黑名单（和执行器一样）：HOME、PATH 这些改不了。
func ValidEnvName(k string) error {
	if !envNameRE.MatchString(k) || len(k) > 128 {
		return fmt.Errorf("env 的名字 %q 不对：只能是大写字母、数字和下划线，不以数字开头", k)
	}
	switch k {
	case "PATH", "HOME", "TMPDIR", "SHELL", "IFS", "BASH_ENV", "ENV":
		return fmt.Errorf("env 不能改 %s", k)
	}
	for _, p := range []string{"LD_", "DYLD_", "NANO_"} {
		if strings.HasPrefix(k, p) {
			return fmt.Errorf("env 不能改 %s*", p)
		}
	}
	return nil
}

// Validate 是登记时的校验。
func (a Agent) Validate() error {
	if !nameRE.MatchString(a.Name) {
		return errors.New("名字只能是小写字母开头的 [a-z0-9_-]，最长 32")
	}
	if len(a.Argv) == 0 || strings.TrimSpace(a.Argv[0]) == "" {
		return errors.New("命令的第一项不能空")
	}
	if len(a.Argv) > maxArgv {
		return fmt.Errorf("命令最多 %d 项", maxArgv)
	}
	slots := 0
	for i, s := range a.Argv {
		if len(s) > maxItem || strings.ContainsRune(s, 0) {
			return fmt.Errorf("命令的第 %d 项太长或有 NUL", i+1)
		}
		switch {
		case s == PromptSlot:
			if i == 0 {
				return errors.New("{prompt} 不能是第一项")
			}
			slots++
		case strings.Contains(s, PromptSlot):
			return errors.New("{prompt} 必须单独占一项，不能嵌在别的字里")
		}
	}
	if slots != 1 {
		return errors.New("{prompt} 必须恰好出现一次")
	}
	if len(a.Dirs) > maxDirs {
		return fmt.Errorf("目录最多 %d 个", maxDirs)
	}
	for _, d := range a.Dirs {
		if err := validDir(d); err != nil {
			return err
		}
	}
	if len(a.Env) > maxEnv {
		return fmt.Errorf("env 最多 %d 项", maxEnv)
	}
	size := 0
	for k, v := range a.Env {
		if err := ValidEnvName(k); err != nil {
			return err
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("env %s 的值里有 NUL", k)
		}
		size += len(k) + len(v) + 2
	}
	if size > maxEnvBytes {
		return errors.New("env 总共超过 8KB")
	}
	if a.Timeout < 0 || a.Timeout > MaxTimeout {
		return fmt.Errorf("时限是 0（默认 %d 秒）到 %d 秒", DefaultTimeout, MaxTimeout)
	}
	return nil
}

// validDir：以 ~/ 开头，不是 ~/ 本身，不含 ..（展开和别的校验在执行器上，按那台机器真实的 HOME）。
func validDir(d string) error {
	rest, ok := strings.CutPrefix(d, "~/")
	if !ok {
		return fmt.Errorf("目录 %q 要以 ~/ 开头", d)
	}
	rest = strings.Trim(rest, "/")
	if rest == "" || rest == "." {
		return fmt.Errorf("目录 %q 不能是家目录本身", d)
	}
	if len(d) > maxItem || strings.ContainsRune(d, 0) || strings.Contains(d, "\\") {
		return fmt.Errorf("目录 %q 不对", d)
	}
	for _, part := range strings.Split(rest, "/") {
		if part == ".." {
			return fmt.Errorf("目录 %q 不能含 ..", d)
		}
	}
	return nil
}

// Store 是 data/external-agents.json。写盘失败不改内存。
//
// 读入时每一项再校验一遍（文件可能被手改过）：不通过的不能用（List、Get 里没有），原样留在文件里，
// 用同一个名字重新登记或者删掉才去掉。
type Store struct {
	mu     sync.Mutex
	path   string
	agents []Agent
	bad    []badEntry
}

type badEntry struct {
	name string
	raw  json.RawMessage
	err  error
}

// Problem 是读入时没通过校验的一项。
type Problem struct {
	Name string
	Err  error
}

// Open 读入登记（文件不存在就是空的）。整个文件读不出来是错误；单项不通过的见 Problems。
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		return nil, err
	}
	for _, raw := range raws {
		var a Agent
		err := json.Unmarshal(raw, &a)
		if err == nil {
			err = a.Validate()
		}
		if err == nil && slices.ContainsFunc(s.agents, func(o Agent) bool { return o.Name == a.Name }) {
			err = errors.New("名字重复了")
		}
		if err != nil {
			s.bad = append(s.bad, badEntry{name: a.Name, raw: raw, err: err})
			continue
		}
		s.agents = append(s.agents, a)
	}
	return s, nil
}

// Problems 是读入时没通过校验、现在不能用的项。
func (s *Store) Problems() []Problem {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Problem
	for _, b := range s.bad {
		out = append(out, Problem{Name: b.name, Err: b.err})
	}
	return out
}

// List 按名字排序。
func (s *Store) List() []Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Agent, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, clone(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get 按名字找。
func (s *Store) Get(name string) (Agent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agents {
		if a.Name == name {
			return clone(a), true
		}
	}
	return Agent{}, false
}

// Has 是这个名字在不在文件里（包括读入时没通过校验的）。
func (s *Store) Has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.agents, func(a Agent) bool { return a.Name == name }) ||
		slices.ContainsFunc(s.bad, func(b badEntry) bool { return b.name == name })
}

// SameGrant 是两次登记给出去的东西一样不一样：命令、目录、环境。时限不算。
func SameGrant(a, b Agent) bool {
	return slices.Equal(a.Argv, b.Argv) && slices.Equal(a.Dirs, b.Dirs) && maps.Equal(a.Env, b.Env)
}

// Put 新增或替换（按名字）。同名的、读入时没通过校验的那项一起换掉。
func (s *Store) Put(a Agent) error {
	if err := a.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]Agent, 0, len(s.agents)+1)
	replaced := false
	for _, o := range s.agents {
		if o.Name == a.Name {
			o, replaced = a, true
		}
		next = append(next, o)
	}
	if !replaced {
		next = append(next, a)
	}
	bad := slices.DeleteFunc(slices.Clone(s.bad), func(b badEntry) bool { return b.name == a.Name })
	if err := s.save(next, bad); err != nil {
		return err
	}
	s.agents, s.bad = next, bad
	return nil
}

// Delete 删掉一个登记（读入时没通过校验的也能删）。
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := slices.DeleteFunc(slices.Clone(s.agents), func(a Agent) bool { return a.Name == name })
	bad := slices.DeleteFunc(slices.Clone(s.bad), func(b badEntry) bool { return b.name == name })
	if len(next) == len(s.agents) && len(bad) == len(s.bad) {
		return errors.New("没有这个外部 agent")
	}
	if err := s.save(next, bad); err != nil {
		return err
	}
	s.agents, s.bad = next, bad
	return nil
}

func (s *Store) save(agents []Agent, bad []badEntry) error {
	out := make([]any, 0, len(agents)+len(bad))
	for _, a := range agents {
		out = append(out, a)
	}
	for _, b := range bad {
		out = append(out, b.raw)
	}
	b, err := json.MarshalIndent(out, "", "  ")
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

func clone(a Agent) Agent {
	a.Argv = append([]string(nil), a.Argv...)
	a.Dirs = append([]string(nil), a.Dirs...)
	if a.Env != nil {
		env := make(map[string]string, len(a.Env))
		for k, v := range a.Env {
			env[k] = v
		}
		a.Env = env
	}
	return a
}
