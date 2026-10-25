// Package mcp 接 MCP 服务（docs/PLAN-roadmap.md 阶段 11）。
//
// 配置在 data/mcp.json：每个服务要么在网关本机跑一个 stdio 进程，要么连一个远程
// streamable HTTP 地址。工具以 <服务>__<工具> 暴露给模型，参数 schema 原样透传。
// 经内核透传到别的电脑（协议 v10）是后面一块，这里先只认网关本机。
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"nano-agent/gateway/internal/secret"
)

// Server 是一个 MCP 服务的配置（data/mcp.json 里的一项）。
//
// URL 和 Command 二选一：给 URL 就连远程的 streamable HTTP；给 Command 就在网关本机起一个
// stdio 进程。Machine 留给协议 v10 的内核透传，现在只认空和 "gateway"。
type Server struct {
	Name     string            `json:"name"`               // 服务名，也是工具名的前缀
	URL      string            `json:"url,omitempty"`      // 远程 streamable HTTP 地址
	Command  string            `json:"command,omitempty"`  // 本机 stdio：可执行文件
	Args     []string          `json:"args,omitempty"`     // 命令行参数
	Env      map[string]string `json:"env,omitempty"`      // 环境变量；密钥存成 enc:v1:
	Cwd      string            `json:"cwd,omitempty"`      // 工作目录
	Machine  string            `json:"machine,omitempty"`  // 空或 gateway = 网关本机
	Enabled  bool              `json:"enabled"`            // 关掉的服务不连、不提供工具
	Tools    []string          `json:"tools,omitempty"`    // 允许的工具；"!名字" 表示禁用
	Agents   []string          `json:"agents,omitempty"`   // 给哪些 agent 用；空 = 所有
	Approval string            `json:"approval,omitempty"` // always / per_turn / never
}

// 审批方式。
const (
	ApprovalAlways  = "always"   // 每次调用都要批准
	ApprovalPerTurn = "per_turn" // 一轮里第一次要批准
	ApprovalNever   = "never"    // 不审批
)

// nameRE 是服务名允许的字符：工具名 <服务>__<工具> 要和模型的工具名规范兼容。
var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Normalize 检查并整理一个服务配置，env 里的明文密钥会被加密。
func Normalize(s Server) (Server, error) {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return s, errors.New("服务名不能为空")
	}
	if !nameRE.MatchString(s.Name) {
		return s, fmt.Errorf("服务名 %q 只能用字母、数字、下划线和减号", s.Name)
	}
	s.URL = strings.TrimSpace(s.URL)
	s.Command = strings.TrimSpace(s.Command)
	s.Machine = strings.TrimSpace(s.Machine)
	if s.Machine != "" && s.Machine != "gateway" {
		return s, fmt.Errorf("现在只能挂在网关本机（machine 留空或写 gateway），%q 要等内核透传", s.Machine)
	}
	if s.URL != "" && s.Command != "" {
		return s, errors.New("URL 和 command 只能给一个")
	}
	if s.URL == "" && s.Command == "" {
		return s, errors.New("要填 command（本机 stdio）或者 url（远程 streamable HTTP）")
	}
	switch s.Approval {
	case "":
		s.Approval = ApprovalNever
	case ApprovalNever, ApprovalAlways:
	case ApprovalPerTurn:
		// per_turn（一轮里只批一次）还没接上，先明确拒绝，别让人以为开了
		return s, errors.New("审批（per_turn）还没接上，先写 always（每次都要批）或 never（不批）")
	default:
		return s, fmt.Errorf("approval 只能是 %s、%s 或 %s", ApprovalAlways, ApprovalPerTurn, ApprovalNever)
	}
	for _, t := range s.Tools {
		if strings.TrimSpace(strings.TrimPrefix(t, "!")) == "" {
			return s, errors.New("tools 里有空的名字")
		}
	}
	for k, v := range s.Env {
		if strings.TrimSpace(k) == "" {
			return s, errors.New("env 里有空的名字")
		}
		if v == "" || secret.IsEncrypted(v) {
			continue
		}
		enc, err := secret.Encrypt(v)
		if err != nil {
			return s, fmt.Errorf("加密 env %s 失败：%w", k, err)
		}
		s.Env[k] = enc
	}
	return s, nil
}

// EnvFor 是解密以后的 env（起进程时用）。
func (s Server) EnvFor() (map[string]string, error) {
	out := make(map[string]string, len(s.Env))
	for k, v := range s.Env {
		plain, err := secret.Decrypt(v)
		if err != nil {
			return nil, fmt.Errorf("解密 env %s 失败：%w", k, err)
		}
		out[k] = plain
	}
	return out, nil
}

// Allows 是这个服务允许的工具（按配置的白名单 / 黑名单过滤）。
func (s Server) Allows(tool string) bool {
	if len(s.Tools) == 0 {
		return true
	}
	allow := map[string]bool{}
	deny := map[string]bool{}
	for _, t := range s.Tools {
		if name, ok := strings.CutPrefix(t, "!"); ok {
			deny[name] = true
		} else {
			allow[t] = true
		}
	}
	if deny[tool] {
		return false
	}
	if len(allow) == 0 {
		return true
	}
	return allow[tool]
}

// ForAgent 是这个服务对某个 agent 是否可用（Agents 空表示所有 agent 都能用）。
func (s Server) ForAgent(agent string) bool {
	return len(s.Agents) == 0 || slices.Contains(s.Agents, agent)
}

// Store 是 data/mcp.json。
type Store struct {
	path string
	mu   sync.Mutex
	list []Server
}

// Open 读 data/mcp.json；没有这个文件就是空列表。
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.list); err != nil {
		return nil, fmt.Errorf("读 data/mcp.json 失败：%w", err)
	}
	return s, nil
}

// List 是现在的服务列表（拷贝）。
func (s *Store) List() []Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.list)
}

// Get 按名字找一个服务。
func (s *Store) Get(name string) (Server, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.list {
		if x.Name == name {
			return x, true
		}
	}
	return Server{}, false
}

// Save 校验、加密后原子写回 data/mcp.json。
func (s *Store) Save(list []Server) error {
	out := make([]Server, 0, len(list))
	seen := map[string]bool{}
	for _, x := range list {
		n, err := Normalize(x)
		if err != nil {
			if x.Name != "" {
				return fmt.Errorf("%s：%w", x.Name, err)
			}
			return err
		}
		if seen[n.Name] {
			return fmt.Errorf("服务名 %q 重复了", n.Name)
		}
		seen[n.Name] = true
		out = append(out, n)
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
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.mu.Lock()
	s.list = out
	s.mu.Unlock()
	return nil
}
