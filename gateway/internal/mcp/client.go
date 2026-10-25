package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
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
	pipes  *Pipes     // 透传到别的机器上的管道（没接就是 nil）
	saveMu sync.Mutex // 写 mcp-tools.json
	change func()     // 连上、断开时通知（网页刷新在线状态）；不拿任何锁调用
}

// OnChange 设置连接状态变化时的通知（连上、断开、连不上）。
func (m *Manager) OnChange(f func()) {
	m.mu.Lock()
	m.change = f
	m.mu.Unlock()
}

func (m *Manager) changed() {
	m.mu.Lock()
	f := m.change
	m.mu.Unlock()
	if f != nil {
		f()
	}
}

// conn 是一个服务的连接状态。mu 只护着下面这几个字段，拿着它的时候**绝不能去连接、等网络或等进程**：
// 算工具定义的地方是在网关总锁里读 tools 的，而连接（管道透传）要等内核回话、内核的消息又要过网关总锁，
// 拿着 mu 去连就是锁顺序反了，会死锁。连接本身由 dialing 串行化（同一个服务同时只连一次）。
type conn struct {
	dialing sync.Mutex
	mu      sync.Mutex
	cs      *sdkmcp.ClientSession
	tools   []*sdkmcp.Tool // 最近一次列到的工具；断开时留着，连不上的时候工具定义照样用它
	used    time.Time
	fails   int
	retry   time.Time
	gen     int // 每断开一次（改了配置、调用出错）加一：连到一半的旧连接回来时作废
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
	m.loadCache()
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
	if cs, tools, done, err := c.ready(m.now(), s.Name); done {
		return cs, tools, err
	}
	c.dialing.Lock()
	defer c.dialing.Unlock()
	// 排队的时候别人可能已经连上了（或者刚失败、进了退避）
	if cs, tools, done, err := c.ready(m.now(), s.Name); done {
		return cs, tools, err
	}
	c.mu.Lock()
	gen := c.gen
	c.mu.Unlock()
	cs, tools, err := m.connect(ctx, s)
	c.mu.Lock()
	if err != nil {
		c.backoff(m.now())
		c.mu.Unlock()
		m.changed()
		return nil, nil, err
	}
	if c.gen != gen {
		// 连的时候配置改了（或者被断开了）：这条是按旧配置连上的，不要
		c.mu.Unlock()
		go cs.Close()
		return nil, nil, fmt.Errorf("MCP 服务 %s 的配置刚改过，请再试一次", s.Name)
	}
	c.cs, c.tools, c.used, c.fails, c.retry = cs, tools, m.now(), 0, time.Time{}
	c.mu.Unlock()
	m.log.Info("MCP 服务已连接", "server", s.Name, "tools", len(tools))
	m.saveCache() // 工具列表记到磁盘：重启以后连上之前先用它
	m.changed()
	return cs, tools, nil
}

// ready：已经连着就给出连接；还在退避期就给出错误；done 为 false 表示该去连了。
func (c *conn) ready(now time.Time, name string) (cs *sdkmcp.ClientSession, tools []*sdkmcp.Tool, done bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cs != nil {
		c.used = now
		return c.cs, c.tools, true, nil
	}
	if !c.retry.IsZero() && now.Before(c.retry) {
		return nil, nil, true, fmt.Errorf("MCP 服务 %s 刚才连不上，%s 后再试", name, c.retry.Sub(now).Round(time.Second))
	}
	return nil, nil, false, nil
}

// connect 真正去连（起进程 / 发 HTTP / 让内核起进程），不拿任何锁。
func (m *Manager) connect(ctx context.Context, s Server) (*sdkmcp.ClientSession, []*sdkmcp.Tool, error) {
	tr, err := m.dial(s)
	if err != nil {
		return nil, nil, fmt.Errorf("MCP 服务 %s 起不来：%w", s.Name, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cs, err := m.client.Connect(cctx, tr, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("连 MCP 服务 %s 失败：%w", s.Name, err)
	}
	tools, err := listAllTools(cctx, cs)
	if err != nil {
		_ = cs.Close()
		return nil, nil, fmt.Errorf("MCP 服务 %s 列工具失败：%w", s.Name, err)
	}
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
		// 只有连接本身坏了才断开重连：用户停下（取消）、服务回了一个 JSON-RPC 错误，连接都还是好的，
		// 断开会把 computer use 这类服务的状态（开着的浏览器之类）一起丢掉
		var werr *jsonrpc.Error
		if !errors.Is(err, context.Canceled) && !errors.As(err, &werr) {
			m.drop(server)
		}
		return nil, fmt.Errorf("调 %s 的 %s 失败：%w", server, tool, err)
	}
	return res, nil
}

// drop 丢掉一个连接（下次重新连）。
func (m *Manager) drop(name string) {
	if cs := m.detach(name, false); cs != nil {
		_ = cs.Close()
		m.changed()
	}
}

// detach 把连接从表里摘下来交给调用方去关（关可能要等进程退出，不在锁里做）。
// 工具列表留着：连不上的这段时间工具定义还用它，重连上会刷新。fresh 表示配置换了，退避也清掉。
func (m *Manager) detach(name string, fresh bool) *sdkmcp.ClientSession {
	c := m.conn(name)
	c.mu.Lock()
	defer c.mu.Unlock()
	cs := c.cs
	c.cs = nil
	c.gen++
	if fresh {
		c.retry, c.fails = time.Time{}, 0
	}
	return cs
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
			// 只断连接，工具列表留着：下次算工具定义时还能用（连上以后会刷新）
			cs, c.cs = c.cs, nil
		}
		c.mu.Unlock()
		if cs != nil {
			_ = cs.Close()
			m.log.Info("MCP 服务空闲，先断开", "server", name)
			m.changed()
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
	m.mu.Lock()
	names := make([]string, 0, len(m.conns))
	for name := range m.conns {
		names = append(names, name)
	}
	m.mu.Unlock()
	for _, name := range names {
		// 配置换了：旧配置连不上留下的退避不算数，马上按新的连
		if cs := m.detach(name, true); cs != nil {
			_ = cs.Close()
		}
	}
	m.changed()
}

// DropMachine 是某台机器的内核断开了：它上面跑的服务连接都作废（进程已经跟着没了）。
// 网关总锁里调用：只摘下来，关在协程里做。
func (m *Manager) DropMachine(machine string) {
	for _, s := range m.store.List() {
		if s.Machine != machine {
			continue
		}
		if cs := m.detach(s.Name, true); cs != nil {
			go cs.Close()
		}
	}
	m.changed()
}

// Wake 是某台机器的内核刚连上：挂在它上面的服务马上去连（不等退避、不等下一次维护）。
func (m *Manager) Wake(machine string, timeout time.Duration) {
	for _, s := range m.store.List() {
		if !s.Enabled || s.Machine != machine {
			continue
		}
		c := m.conn(s.Name)
		c.mu.Lock()
		c.retry, c.fails = time.Time{}, 0
		c.mu.Unlock()
		go m.warmOne(s, timeout)
	}
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

// CachedTools 是这个 agent 现在能用的 MCP 工具，**只读缓存、不连接**。
// 工具定义要在网关的总锁里算，绝不能在这里发起网络/进程连接（会死锁、也会卡住整个网关）；
// 连接由 Warm 在后台按需建立。
func (m *Manager) CachedTools(agent string) []ToolInfo {
	servers := m.store.List()
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	var out []ToolInfo
	for _, s := range servers {
		if !s.Enabled || !s.ForAgent(agent) {
			continue
		}
		c := m.conn(s.Name)
		c.mu.Lock()
		tools := c.tools
		c.mu.Unlock()
		out = append(out, toolInfos(s, tools)...)
	}
	return out
}

// Warm 把还没连上的服务后台连一遍（启动时、改了配置以后、每轮维护时调用）。
// 连不上就按退避下一轮再试；这里不返回错误，日志里能看到原因。
func (m *Manager) Warm(timeout time.Duration) {
	for _, s := range m.store.List() {
		if !s.Enabled {
			continue
		}
		go m.warmOne(s, timeout)
	}
}

func (m *Manager) warmOne(s Server, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, _, err := m.session(ctx, s); err != nil {
		m.log.Warn("MCP 服务还没连上", "server", s.Name, "err", err)
	}
}

// ServerTools 是一个服务上次列到的工具（按它的白名单过滤），只读缓存、不连接。
func (m *Manager) ServerTools(name string) []ToolInfo {
	s, ok := m.store.Get(name)
	if !ok {
		return nil
	}
	c := m.conn(name)
	c.mu.Lock()
	tools := c.tools
	c.mu.Unlock()
	return toolInfos(s, tools)
}
