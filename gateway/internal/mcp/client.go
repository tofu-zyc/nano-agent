package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolInfo 是列出来的一个 MCP 工具，带暴露给模型的名字。
type ToolInfo struct {
	Server      string          // 服务名
	Name        string          // 服务里的原始工具名
	Exposed     string          // 暴露给模型的名字：<服务>__<工具>
	Description string          // 服务给的说明
	Schema      json.RawMessage // 原样的参数 schema
}

// ExposedName 是暴露给模型的工具名：<服务>__<工具>。顺序和名字都固定，保证前缀缓存不抖。
func ExposedName(server, tool string) string { return server + "__" + tool }

// SplitExposed 把 <服务>__<工具> 拆回两半；不是这种格式时 ok 为 false。
func SplitExposed(name string) (server, tool string, ok bool) {
	server, tool, ok = strings.Cut(name, "__")
	return server, tool, ok && server != "" && tool != ""
}

// DefaultTransport 按配置连一个服务：有 URL 走远程 streamable HTTP，否则在本机起 stdio 进程。
func DefaultTransport(s Server) (sdkmcp.Transport, error) {
	if s.URL != "" {
		return &sdkmcp.StreamableClientTransport{Endpoint: s.URL}, nil
	}
	cmd := exec.Command(s.Command, s.Args...)
	if s.Cwd != "" {
		cmd.Dir = s.Cwd
	}
	env, err := s.EnvFor()
	if err != nil {
		return nil, err
	}
	cmd.Env = os.Environ()
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	return &sdkmcp.CommandTransport{Command: cmd}, nil
}

// Manager 管所有 MCP 服务：用到才连，连不上按退避重试，空闲一段时间关掉。
type Manager struct {
	mu     sync.Mutex
	store  *Store
	log    *slog.Logger
	client *sdkmcp.Client
	conns  map[string]*conn
	idle   time.Duration
	now    func() time.Time
	dial   func(Server) (sdkmcp.Transport, error)
	pipes  *Pipes // 透传到别的机器上的管道（没接就是 nil）
}

// conn 是一个服务的连接状态（自己一把锁，连接时不用占着 Manager 的锁）。
type conn struct {
	mu    sync.Mutex
	cs    *sdkmcp.ClientSession
	tools []*sdkmcp.Tool
	used  time.Time
	fails int
	retry time.Time
}

// NewManager 接上配置；idle 到了就 Sweep 关掉空闲连接。
func NewManager(store *Store, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		store:  store,
		log:    log,
		client: sdkmcp.NewClient(&sdkmcp.Implementation{Name: "nano-agent", Version: "v1"}, nil),
		conns:  map[string]*conn{},
		idle:   10 * time.Minute,
		now:    time.Now,
	}
	m.dial = m.defaultTransport
	return m
}

// SetLink 接上「把 stdio 送到某台机器」的通道；nil 表示不支持透传。
func (m *Manager) SetLink(l Link) {
	m.mu.Lock()
	if l == nil {
		m.pipes = nil
	} else {
		m.pipes = NewPipes(l)
	}
	m.mu.Unlock()
}

// Pipes 是管道表（网关收到 pipe_data / pipe_closed 时交给它）。
func (m *Manager) Pipes() *Pipes {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pipes
}

// defaultTransport：指定了机器就走内核透传，否则本机 stdio / 远程 HTTP。
func (m *Manager) defaultTransport(s Server) (sdkmcp.Transport, error) {
	if s.Machine != "" && s.Machine != "gateway" {
		m.mu.Lock()
		pipes := m.pipes
		m.mu.Unlock()
		if pipes == nil {
			return nil, fmt.Errorf("MCP 透传还没接上，%s 上的服务现在用不了", s.Machine)
		}
		env, err := s.EnvFor()
		if err != nil {
			return nil, err
		}
		return &PipeTransport{Pipes: pipes, Node: s.Machine,
			Argv: append([]string{s.Command}, s.Args...), Env: env, Cwd: s.Cwd}, nil
	}
	return DefaultTransport(s)
}

func (m *Manager) conn(name string) *conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.conns[name]
	if c == nil {
		c = &conn{}
		m.conns[name] = c
	}
	return c
}

// session 拿到连上的服务（必要时现连）。连不上时记下退避时间。
func (m *Manager) session(ctx context.Context, s Server) (*sdkmcp.ClientSession, []*sdkmcp.Tool, error) {
	c := m.conn(s.Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := m.now()
	if c.cs != nil {
		c.used = now
		return c.cs, c.tools, nil
	}
	if !c.retry.IsZero() && now.Before(c.retry) {
		return nil, nil, fmt.Errorf("MCP 服务 %s 刚才连不上，%s 后再试", s.Name, c.retry.Sub(now).Round(time.Second))
	}
	tr, err := m.dial(s)
	if err != nil {
		c.backoff(now)
		return nil, nil, fmt.Errorf("MCP 服务 %s 起不来：%w", s.Name, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cs, err := m.client.Connect(cctx, tr, nil)
	if err != nil {
		c.backoff(now)
		return nil, nil, fmt.Errorf("连 MCP 服务 %s 失败：%w", s.Name, err)
	}
	tools, err := listAllTools(cctx, cs)
	if err != nil {
		_ = cs.Close()
		c.backoff(now)
		return nil, nil, fmt.Errorf("MCP 服务 %s 列工具失败：%w", s.Name, err)
	}
	c.cs, c.tools, c.used, c.fails, c.retry = cs, tools, now, 0, time.Time{}
	m.log.Info("MCP 服务已连接", "server", s.Name, "tools", len(tools))
	return cs, tools, nil
}

// backoff 记一次失败：2s、4s、8s……最多一分钟。调用方持有 c.mu。
func (c *conn) backoff(now time.Time) {
	c.fails++
	n := c.fails
	if n > 5 {
		n = 5
	}
	d := time.Duration(1<<n) * time.Second
	if d > time.Minute {
		d = time.Minute
	}
	c.retry = now.Add(d)
}

// listAllTools 把分页的工具列表取全。
func listAllTools(ctx context.Context, cs *sdkmcp.ClientSession) ([]*sdkmcp.Tool, error) {
	var out []*sdkmcp.Tool
	cursor := ""
	for {
		var params *sdkmcp.ListToolsParams
		if cursor != "" {
			params = &sdkmcp.ListToolsParams{Cursor: cursor}
		}
		res, err := cs.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// toolInfos 把 SDK 的工具转成我们能用的形状，并按配置过滤。
func toolInfos(s Server, tools []*sdkmcp.Tool) []ToolInfo {
	out := make([]ToolInfo, 0, len(tools))
	for _, t := range tools {
		if t == nil || !s.Allows(t.Name) {
			continue
		}
		schema, _ := json.Marshal(t.InputSchema)
		if string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, ToolInfo{Server: s.Name, Name: t.Name, Exposed: ExposedName(s.Name, t.Name),
			Description: t.Description, Schema: schema})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Exposed < out[j].Exposed })
	return out
}

// Tools 列出一个服务的工具（会顺手连上）。
func (m *Manager) Tools(ctx context.Context, name string) ([]ToolInfo, error) {
	s, ok := m.store.Get(name)
	if !ok {
		return nil, fmt.Errorf("没有 MCP 服务 %q", name)
	}
	_, tools, err := m.session(ctx, s)
	if err != nil {
		return nil, err
	}
	return toolInfos(s, tools), nil
}

// ToolsFor 是某个 agent 现在能用的所有 MCP 工具：按服务名排序，顺序固定。
// 连不上的服务用上次缓存下来的工具（调用时才报“服务不在线”），不从列表里拿掉。
func (m *Manager) ToolsFor(ctx context.Context, agent string) []ToolInfo {
	servers := m.store.List()
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	var out []ToolInfo
	for _, s := range servers {
		if !s.Enabled || !s.ForAgent(agent) {
			continue
		}
		_, tools, err := m.session(ctx, s)
		if err != nil {
			c := m.conn(s.Name)
			c.mu.Lock()
			tools = c.tools
			c.mu.Unlock()
			m.log.Warn("MCP 服务没连上，先用上次的工具列表", "server", s.Name, "err", err)
		}
		out = append(out, toolInfos(s, tools)...)
	}
	return out
}

// Call 调一个工具。服务连不上或调用出错时，下次会重新连。
func (m *Manager) Call(ctx context.Context, server, tool string, args any) (*sdkmcp.CallToolResult, error) {
	s, ok := m.store.Get(server)
	if !ok {
		return nil, fmt.Errorf("没有 MCP 服务 %q", server)
	}
	if !s.Allows(tool) {
		return nil, fmt.Errorf("%s 的工具 %s 没开", server, tool)
	}
	cs, _, err := m.session(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("MCP 服务 %s 现在用不了：%w", server, err)
	}
	res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		m.drop(server)
		return nil, fmt.Errorf("调 %s 的 %s 失败：%w", server, tool, err)
	}
	return res, nil
}

// drop 丢掉一个连接（下次重新连）。
func (m *Manager) drop(name string) {
	c := m.conn(name)
	c.mu.Lock()
	cs := c.cs
	c.cs, c.tools = nil, nil
	c.mu.Unlock()
	if cs != nil {
		_ = cs.Close()
	}
}

// Sweep 关掉空闲太久的连接（网关每分钟调一次）。
func (m *Manager) Sweep() {
	now := m.now()
	m.mu.Lock()
	names := make([]string, 0, len(m.conns))
	for name := range m.conns {
		names = append(names, name)
	}
	m.mu.Unlock()
	for _, name := range names {
		c := m.conn(name)
		c.mu.Lock()
		idle := c.cs != nil && now.Sub(c.used) > m.idle
		var cs *sdkmcp.ClientSession
		if idle {
			cs, c.cs, c.tools = c.cs, nil, nil
		}
		c.mu.Unlock()
		if cs != nil {
			_ = cs.Close()
			m.log.Info("MCP 服务空闲，先断开", "server", name)
		}
	}
}

// Close 全关（网关退出时）。
func (m *Manager) Close() {
	m.mu.Lock()
	names := make([]string, 0, len(m.conns))
	for name := range m.conns {
		names = append(names, name)
	}
	m.mu.Unlock()
	for _, name := range names {
		m.drop(name)
	}
}

// Server 按名字找一个服务（网关分发 <服务>__<工具> 时用）。
func (m *Manager) Server(name string) (Server, bool) {
	return m.store.Get(name)
}

// ResultText 把工具结果摊平成一段文字。图片单独说明（模型的工具结果放不进图片，
// 需要的话由网关另外塞进上下文）。
func ResultText(res *sdkmcp.CallToolResult) string {
	if res == nil {
		return "（没有输出）"
	}
	var b strings.Builder
	for _, c := range res.Content {
		switch v := c.(type) {
		case *sdkmcp.TextContent:
			b.WriteString(v.Text)
		case *sdkmcp.ImageContent:
			fmt.Fprintf(&b, "\n[图片 %s，%d 字节]", v.MIMEType, len(v.Data))
		case *sdkmcp.AudioContent:
			fmt.Fprintf(&b, "\n[音频 %s，%d 字节]", v.MIMEType, len(v.Data))
		case *sdkmcp.ResourceLink:
			fmt.Fprintf(&b, "\n[资源 %s]", v.URI)
		}
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		return s
	}
	if res.StructuredContent != nil {
		if out, err := json.Marshal(res.StructuredContent); err == nil {
			return string(out)
		}
	}
	return "（没有输出）"
}

// Status 是这个服务现在的连接状态（不主动连，没有连上就是 false）。
func (m *Manager) Status(name string) (online bool, tools int) {
	c := m.conn(name)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cs != nil, len(c.tools)
}

// Reload 在配置改了以后调用：断掉所有连接，下次用到再按新配置连。
func (m *Manager) Reload() {
	m.Close()
}

// Test 强制现连一次（忽略退避），网页上的“测试”用它 —— 用户点测试就是想看现在能不能连上。
func (m *Manager) Test(ctx context.Context, name string) ([]ToolInfo, error) {
	s, ok := m.store.Get(name)
	if !ok {
		return nil, fmt.Errorf("没有 MCP 服务 %q", name)
	}
	c := m.conn(name)
	c.mu.Lock()
	c.retry = time.Time{}
	c.fails = 0
	c.mu.Unlock()
	_, tools, err := m.session(ctx, s)
	if err != nil {
		return nil, err
	}
	return toolInfos(s, tools), nil
}
