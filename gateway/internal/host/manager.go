package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/paths"
	"nano-agent/gateway/internal/secret"
)

// Connection 是一个 SSH 服务器的设置，存在 data/connections.json（0600）。
type Connection struct {
	ID            string `json:"id"`
	Name          string `json:"name"` // 工作目录里用的名字
	Host          string `json:"host"` // 地址，或者 ~/.ssh/config 里的别名
	Port          int    `json:"port,omitempty"`
	User          string `json:"user,omitempty"`
	IdentityFile  string `json:"identity_file,omitempty"`
	Password      string `json:"password,omitempty"`
	AutoConnect   bool   `json:"auto_connect"`
	ClearPassword bool   `json:"clear_password,omitempty"`
}

// Info 是一台能执行工具的机器的状态（不含密码）。本机只有 Name、Kind、Ready、Landlock、State、Detail。
type Info struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`  // local（网关所在的机器）或 ssh
	Ready    bool   `json:"ready"` // 能执行工具
	Landlock int    `json:"landlock"`
	State    string `json:"state"` // offline、connecting、preparing、online、reconnecting、disconnecting、error
	Detail   string `json:"detail,omitempty"`

	ID           string `json:"id,omitempty"`
	Host         string `json:"host,omitempty"`
	Port         int    `json:"port,omitempty"`
	User         string `json:"user,omitempty"`
	IdentityFile string `json:"identity_file,omitempty"`
	HasPassword  bool   `json:"has_password,omitempty"`
	AutoConnect  bool   `json:"auto_connect,omitempty"`
}

type status struct{ state, detail string }

// Manager 管理本机和 SSH 服务器，也负责电脑接入用的 TCP 监听和配对（转给 Hub）。
type Manager struct {
	ctx        context.Context
	dir        string
	exec       Executor
	local      *Local
	hub        *kernel.Hub
	log        *slog.Logger
	sshProgram string
	ctlDir     string

	mu         sync.Mutex
	records    map[string]Connection
	conns      map[string]*sshConn // 连着或者正在连的（按 ID）
	states     map[string]status
	onChange   func()
	listener   net.Listener
	listenAddr string
}

// New 读入 SSH 服务器的设置，设了自动连接的马上开始连。guard 是本机上不能用作工作目录的路径。
func New(ctx context.Context, dir string, e Executor, guard []string, hub *kernel.Hub, log *slog.Logger) (*Manager, error) {
	m := &Manager{ctx: ctx, dir: dir, exec: e, local: NewLocal(e, guard), hub: hub, log: log, sshProgram: "ssh",
		records: map[string]Connection{}, conns: map[string]*sshConn{}, states: map[string]status{}}
	if p := os.Getenv("NANO_SSH_PROGRAM"); p != "" {
		m.sshProgram = p // 测试用
	}
	var err error
	if m.ctlDir, err = controlDir(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "connections.json"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &m.records); err != nil {
			return nil, fmt.Errorf("读取服务器连接：%w", err)
		}
		old := false
		for id, c := range m.records {
			pw, err := secret.Decrypt(c.Password)
			if err != nil {
				return nil, fmt.Errorf("connections.json 里的密码：%w", err)
			}
			if pw != c.Password {
				c.Password = pw
				m.records[id] = c
			} else if pw != "" && secret.Ready() && !secret.IsEncrypted(pw) {
				old = true
			}
		}
		if old {
			if err := m.persist(m.records); err != nil {
				return nil, err
			}
		}
	}
	for id, c := range m.records {
		if c.AutoConnect {
			m.Connect(id)
		}
	}
	return m, nil
}

// controlDir 是 ssh 控制 socket 所在的目录：$NANO_HOME/run/nano-ssh；
// 路径太长时退回 $XDG_RUNTIME_DIR 或 /tmp（unix socket 最长 107 字节）。
func controlDir() (string, error) {
	dir := paths.ShortRunDir("nano-ssh")
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("没法使用 SSH 连接复用的目录 %s（要求是只有自己能进的目录）", dir)
	}
	return dir, nil
}

// OnChange 在机器的状态变了时调用（网页刷新）。
func (m *Manager) OnChange(f func()) {
	m.mu.Lock()
	m.onChange = f
	m.mu.Unlock()
}

func (m *Manager) changed() {
	m.mu.Lock()
	f := m.onChange
	m.mu.Unlock()
	if f != nil {
		f()
	}
}

func (m *Manager) setStatus(id, state, detail string) {
	m.mu.Lock()
	old := m.states[id]
	m.states[id] = status{state, detail}
	m.mu.Unlock()
	if old != (status{state, detail}) {
		m.changed()
	}
}

// Local 是网关所在的机器。
func (m *Manager) Local() *Local { return m.local }

// Host 返回名字是 name 的机器（本机或者 SSH 服务器）。ok 为 false 表示没有这台机器；
// 有但现在用不了时 h 为 nil，info 里有原因。
func (m *Manager) Host(name string) (h Host, info Info, ok bool) {
	if name == LocalName {
		info = m.localInfo()
		if info.Ready {
			return m.local, info, true
		}
		return nil, info, true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, c := range m.records {
		if c.Name != name {
			continue
		}
		info = m.infoOf(id, c)
		if conn := m.conns[id]; conn != nil && info.Ready {
			return sshHost{conn}, info, true
		}
		return nil, info, true
	}
	return nil, Info{}, false
}

func (m *Manager) localInfo() Info {
	landlock, ok, detail := m.local.Probe()
	info := Info{Name: LocalName, Kind: "local", Ready: ok, Landlock: landlock, State: "online", Detail: detail}
	if !ok {
		info.State = "error"
	}
	return info
}

// infoOf 调用方持有 m.mu
func (m *Manager) infoOf(id string, c Connection) Info {
	st := m.states[id]
	if st.state == "" {
		st.state = "offline"
	}
	info := Info{Name: c.Name, Kind: "ssh", State: st.state, Detail: st.detail, ID: id, Host: c.Host, Port: c.Port,
		User: c.User, IdentityFile: c.IdentityFile, HasPassword: c.Password != "", AutoConnect: c.AutoConnect}
	if conn := m.conns[id]; conn != nil {
		info.Ready, info.Landlock = conn.state()
	}
	if info.Ready && st.state != "disconnecting" {
		info.State = "online"
	}
	return info
}

// Infos 返回所有机器：本机在前，SSH 服务器按名字排。
func (m *Manager) Infos() []Info {
	out := []Info{m.localInfo()}
	return append(out, m.List()...)
}

// List 返回 SSH 服务器（不含密码）。
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Info{}
	for id, c := range m.records {
		out = append(out, m.infoOf(id, c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Secrets 是已知的 SSH 密码，给脱敏用。
func (m *Manager) Secrets() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, c := range m.records {
		if c.Password != "" {
			out = append(out, c.Password)
		}
	}
	sort.Strings(out)
	return out
}

func (m *Manager) persist(records map[string]Connection) error {
	disk := make(map[string]Connection, len(records))
	for id, c := range records {
		enc, err := secret.Encrypt(c.Password)
		if err != nil {
			return err
		}
		c.Password = enc
		disk[id] = c
	}
	b, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(m.dir, "connections.json")
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// ValidName：SSH 服务器的名字会出现在工作目录里（名字:路径），不能和本机重名
func ValidName(name string) bool {
	return kernel.ValidNode(name) && name != LocalName && !strings.EqualFold(name, "local")
}

func (m *Manager) Save(c Connection) (string, error) {
	c.Name, c.Host, c.User = strings.TrimSpace(c.Name), strings.TrimSpace(c.Host), strings.TrimSpace(c.User)
	if !ValidName(c.Name) {
		return "", errors.New("名字不能为空，不能有空白、冒号或斜杠，不能叫“本机”，最多 64 个字")
	}
	if c.Host == "" || strings.ContainsAny(c.Host, "\r\n\t ") || strings.HasPrefix(c.Host, "-") {
		return "", errors.New("请填写主机地址或 SSH 别名")
	}
	if c.Port < 0 || c.Port > 65535 {
		return "", errors.New("端口应为 1–65535，留空沿用 SSH 配置")
	}
	if strings.ContainsAny(c.User+c.IdentityFile, "\r\n") || strings.HasPrefix(c.User, "-") {
		return "", errors.New("用户和密钥路径不对")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ID == "" {
		var id [8]byte
		if _, err := rand.Read(id[:]); err != nil {
			return "", err
		}
		c.ID = hex.EncodeToString(id[:])
	} else if _, ok := m.records[c.ID]; !ok {
		return "", errors.New("没有这个服务器连接")
	}
	if m.conns[c.ID] != nil {
		return "", errors.New("请先断开连接，再修改服务器设置")
	}
	for id, other := range m.records {
		if id != c.ID && other.Name == c.Name {
			return "", errors.New("已有同名服务器")
		}
	}
	if c.ClearPassword {
		c.Password = ""
	} else if c.Password == "" {
		c.Password = m.records[c.ID].Password
	}
	c.ClearPassword = false
	next := make(map[string]Connection, len(m.records)+1)
	for id, v := range m.records {
		next[id] = v
	}
	next[c.ID] = c
	if err := m.persist(next); err != nil {
		return "", err
	}
	m.records = next
	m.states[c.ID] = status{"offline", ""}
	if c.Password == "" {
		os.Remove(filepath.Join(m.dir, "ssh", c.ID, "password"))
	}
	go m.changed()
	return c.ID, nil
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conns[id] != nil {
		return errors.New("请先断开连接")
	}
	if _, ok := m.records[id]; !ok {
		return errors.New("没有这个服务器连接")
	}
	next := make(map[string]Connection, len(m.records))
	for key, v := range m.records {
		if key != id {
			next[key] = v
		}
	}
	if err := m.persist(next); err != nil {
		return err
	}
	m.records = next
	delete(m.states, id)
	go m.changed()
	return os.RemoveAll(filepath.Join(m.dir, "ssh", id)) // 网关这边的认证文件；不动服务器上的文件
}

// Connect 开始连接（已经连着就什么也不做）。连上之后一直保持，断了自动重连，直到 Disconnect。
func (m *Manager) Connect(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.records[id]
	if !ok {
		return errors.New("没有这个服务器连接")
	}
	if m.conns[id] != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	h := &sshConn{m: m, c: c, ctl: filepath.Join(m.ctlDir, id), cancel: cancel, done: make(chan struct{})}
	m.conns[id] = h
	m.states[id] = status{"connecting", "正在建立 SSH 连接"}
	go m.run(ctx, h)
	return nil
}

func (m *Manager) Disconnect(id string) error {
	m.mu.Lock()
	if _, ok := m.records[id]; !ok {
		m.mu.Unlock()
		return errors.New("没有这个服务器连接")
	}
	h := m.conns[id]
	if h != nil {
		m.states[id] = status{"disconnecting", "正在断开"}
		h.cancel()
	} else {
		m.states[id] = status{"offline", ""}
	}
	m.mu.Unlock()
	m.changed()
	return nil
}

func (m *Manager) run(ctx context.Context, h *sshConn) {
	id := h.c.ID
	defer func() {
		os.Remove(filepath.Join(m.dir, "ssh", id, "password"))
		m.mu.Lock()
		delete(m.conns, id)
		m.states[id] = status{"offline", ""}
		m.mu.Unlock()
		close(h.done)
		m.changed()
	}()
	delay := 5 * time.Second
	for {
		start := time.Now()
		err := h.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			delay = 5 * time.Second
		}
		detail := h.redact(err.Error())
		m.log.Warn("服务器连接断开", "server", h.c.Name, "detail", detail, "retry_in", delay)
		m.setStatus(id, "reconnecting", detail)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Minute)
	}
}

func expandHome(path string) string {
	home, _ := os.UserHomeDir()
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// ---- 电脑接入：额外的 TCP 监听和配对（转给 Hub） ----

func (m *Manager) SetListener(address string) error {
	address = strings.TrimPrefix(strings.TrimSpace(address), "tcp:")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listenAddr == address {
		return nil
	}
	var ln net.Listener
	if address != "" {
		var err error
		if ln, err = net.Listen("tcp", address); err != nil {
			return err
		}
	}
	old := m.listener
	m.listener, m.listenAddr = ln, address
	if old != nil {
		old.Close()
	}
	if ln != nil {
		go m.hub.Serve(m.ctx, ln)
	}
	return nil
}

func (m *Manager) Listener() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listener == nil {
		return ""
	}
	return m.listener.Addr().String()
}

// Paired 是配对过的一台电脑和它在不在线。
type Paired struct {
	kernel.PairedDevice
	Online bool `json:"online"`
}

func (m *Manager) PairRequests() []kernel.PairRequest { return m.hub.PairRequests() }
func (m *Manager) PairedDevices() []Paired {
	out := []Paired{}
	for _, d := range m.hub.PairedDevices() {
		out = append(out, Paired{d, m.hub.Connected(d.Node)})
	}
	return out
}
func (m *Manager) DecidePair(id string, ok bool) error { return m.hub.DecidePair(id, ok) }
func (m *Manager) Unpair(node string) error            { return m.hub.Unpair(node) }

// SSHHosts 只枚举可选择的别名，实际连接仍交给 OpenSSH 解析全部配置和 Include。
func SSHHosts() []string {
	home, _ := os.UserHomeDir()
	seen, hosts := map[string]bool{}, map[string]bool{}
	var read func(string)
	read = func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(b), "\n") {
			line, _, _ = strings.Cut(line, "#")
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			switch strings.ToLower(fields[0]) {
			case "host":
				for _, h := range fields[1:] {
					if !strings.ContainsAny(h, "*?!") {
						hosts[h] = true
					}
				}
			case "include":
				for _, pattern := range fields[1:] {
					pattern = expandHome(strings.Trim(pattern, "\""))
					if !filepath.IsAbs(pattern) {
						pattern = filepath.Join(home, ".ssh", pattern)
					}
					matches, _ := filepath.Glob(pattern)
					for _, p := range matches {
						read(p)
					}
				}
			}
		}
	}
	read(filepath.Join(home, ".ssh/config"))
	out := []string{}
	for host := range hosts {
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}
