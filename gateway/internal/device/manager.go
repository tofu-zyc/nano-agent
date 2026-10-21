package device

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/kernel"
)

type Connection struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port,omitempty"`
	User          string `json:"user,omitempty"`
	IdentityFile  string `json:"identity_file,omitempty"`
	Password      string `json:"password,omitempty"`
	AutoConnect   bool   `json:"auto_connect"`
	ClearPassword bool   `json:"clear_password,omitempty"`
}

// Info 不包含密码。密码只存放在网关的数据目录内，权限为 0600。
type Info struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	User         string `json:"user"`
	IdentityFile string `json:"identity_file"`
	HasPassword  bool   `json:"has_password"`
	AutoConnect  bool   `json:"auto_connect"`
	State        string `json:"state"`
	Detail       string `json:"detail,omitempty"`
}
type status struct{ state, detail string }
type active struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type Manager struct {
	mu         sync.Mutex
	ctx        context.Context
	dir, token string
	bundle     Bundle
	hub        *kernel.Hub
	log        *slog.Logger
	records    map[string]Connection
	running    map[string]*active
	states     map[string]status
	sshProgram string
	listener   net.Listener
	listenAddr string
}

func New(ctx context.Context, dir, token string, bundle Bundle, hub *kernel.Hub, log *slog.Logger) (*Manager, error) {
	m := &Manager{ctx: ctx, dir: dir, token: token, bundle: bundle, hub: hub, log: log, records: map[string]Connection{}, running: map[string]*active{}, states: map[string]status{}, sshProgram: "ssh"}
	b, err := os.ReadFile(filepath.Join(dir, "connections.json"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &m.records); err != nil {
			return nil, fmt.Errorf("读取服务器连接：%w", err)
		}
	}
	for id, c := range m.records {
		if c.AutoConnect {
			if err := m.Connect(id); err != nil {
				m.setStatus(id, "error", err.Error())
			}
		}
	}
	return m, nil
}

func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Info
	for id, c := range m.records {
		st := m.states[id]
		if st.state == "" {
			st.state = "offline"
		}
		if m.running[id] != nil && st.state != "disconnecting" && m.hub.Connected(c.Name) {
			st = status{"online", ""}
		}
		out = append(out, Info{ID: id, Name: c.Name, Host: c.Host, Port: c.Port, User: c.User, IdentityFile: c.IdentityFile, HasPassword: c.Password != "", AutoConnect: c.AutoConnect, State: st.state, Detail: st.detail})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if out == nil {
		out = []Info{}
	}
	return out
}

func (m *Manager) persist(records map[string]Connection) error {
	b, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(m.dir, "connections.json")
	if err := os.WriteFile(path+".tmp", b, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (m *Manager) Save(c Connection) (string, error) {
	c.Name, c.Host, c.User = strings.TrimSpace(c.Name), strings.TrimSpace(c.Host), strings.TrimSpace(c.User)
	if !kernel.ValidNode(c.Name) {
		return "", errors.New("电脑名字不能为空，不能有空白、冒号或斜杠，最多 64 个字")
	}
	if c.Host == "" || strings.ContainsAny(c.Host, "\r\n\t ") || strings.HasPrefix(c.Host, "-") {
		return "", errors.New("请填写主机地址或 SSH 别名")
	}
	if c.Port < 0 || c.Port > 65535 {
		return "", errors.New("端口应为 1–65535，留空沿用 SSH 配置")
	}
	if strings.ContainsAny(c.User+c.IdentityFile, "\r\n") {
		return "", errors.New("用户和密钥路径不能包含换行")
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
	if m.running[c.ID] != nil {
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
	return c.ID, nil
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running[id] != nil {
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
	// 清除本连接的临时认证文件；不会删除服务器上的文件。
	return os.RemoveAll(filepath.Join(m.dir, "ssh", id))
}

func (m *Manager) Connect(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.records[id]
	if !ok {
		return errors.New("没有这个服务器连接")
	}
	if m.running[id] != nil {
		return nil
	}
	if m.hub.Connected(c.Name) {
		return errors.New("同名内核已经在线，请换一个电脑名字")
	}
	ctx, cancel := context.WithCancel(m.ctx)
	r := &active{cancel: cancel, done: make(chan struct{})}
	m.running[id] = r
	m.states[id] = status{"connecting", "正在建立 SSH 连接"}
	go m.run(ctx, c, r)
	return nil
}

func (m *Manager) Disconnect(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.records[id]; !ok {
		return errors.New("没有这个服务器连接")
	}
	if r := m.running[id]; r != nil {
		m.states[id] = status{"disconnecting", "正在断开"}
		r.cancel()
	} else {
		m.states[id] = status{"offline", ""}
	}
	return nil
}

func (m *Manager) setStatus(id, state, detail string) {
	m.mu.Lock()
	m.states[id] = status{state, detail}
	m.mu.Unlock()
}

func (m *Manager) run(ctx context.Context, c Connection, r *active) {
	defer func() {
		os.Remove(filepath.Join(m.dir, "ssh", c.ID, "password"))
		m.mu.Lock()
		delete(m.running, c.ID)
		if ctx.Err() != nil {
			m.states[c.ID] = status{"offline", ""}
		}
		m.mu.Unlock()
		close(r.done)
	}()
	for {
		err := m.connect(ctx, c)
		if ctx.Err() != nil {
			return
		}
		detail := "SSH 连接已关闭"
		if err != nil {
			detail = err.Error()
		}
		if c.Password != "" {
			detail = strings.ReplaceAll(detail, c.Password, "<密码>")
		}
		m.log.Warn("服务器连接关闭", "node", c.Name, "detail", detail)
		if !c.AutoConnect {
			m.setStatus(c.ID, "error", detail)
			return
		}
		m.setStatus(c.ID, "reconnecting", detail)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		m.setStatus(c.ID, "connecting", "正在重新连接")
	}
}

func (m *Manager) ssh(ctx context.Context, c Connection, script string) (*exec.Cmd, error) {
	args := []string{"-T", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "StrictHostKeyChecking=accept-new"}
	if c.Port != 0 {
		args = append(args, "-p", fmt.Sprint(c.Port))
	}
	if c.User != "" {
		args = append(args, "-l", c.User)
	}
	if c.IdentityFile != "" {
		args = append(args, "-i", expandHome(c.IdentityFile))
	}
	if c.Password == "" {
		args = append(args, "-o", "BatchMode=yes")
	} else {
		args = append(args, "-o", "BatchMode=no", "-o", "NumberOfPasswordPrompts=1")
	}
	args = append(args, "--", c.Host, "sh -c "+shellQuote(script))
	cmd := exec.CommandContext(ctx, m.sshProgram, args...)
	if c.Password != "" {
		dir := filepath.Join(m.dir, "ssh", c.ID)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "password"), []byte(c.Password), 0600); err != nil {
			return nil, err
		}
		ask := filepath.Join(dir, "askpass.sh")
		if err := os.WriteFile(ask, []byte("#!/bin/sh\nexec cat \"$NANO_SSH_PASSWORD_FILE\"\n"), 0700); err != nil {
			return nil, err
		}
		cmd.Env = append(os.Environ(), "SSH_ASKPASS="+ask, "SSH_ASKPASS_REQUIRE=force", "DISPLAY=nano-agent", "NANO_SSH_PASSWORD_FILE="+filepath.Join(dir, "password"))
	}
	return cmd, nil
}

type tailBuffer struct{ data []byte }

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.data = append(b.data, p...)
	if len(b.data) > 8000 {
		b.data = b.data[len(b.data)-8000:]
	}
	return n, nil
}

func (m *Manager) connect(ctx context.Context, c Connection) error {
	archive, err := m.bundle.remoteArchive(c.Name, m.token)
	if err != nil {
		return err
	}
	prepCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	cmd, err := m.ssh(prepCtx, c, bootstrap(c.ID))
	if err != nil {
		cancel()
		return err
	}
	var output tailBuffer
	cmd.Stdin = bytes.NewReader(archive)
	cmd.Stdout = &output
	cmd.Stderr = &output
	err = cmd.Run()
	cancel()
	if err != nil {
		return fmt.Errorf("部署内核失败：%w\n%s", err, strings.TrimSpace(string(output.data)))
	}
	m.setStatus(c.ID, "connecting", "内核已就绪，正在接入网关")
	cmd, err = m.ssh(ctx, c, remoteCommand(c.ID))
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var logs tailBuffer
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		return err
	}
	hubSide, sshSide := net.Pipe()
	m.hub.Attach(ctx, hubSide, false)
	outDone, inDone := make(chan struct{}), make(chan struct{})
	go func() { io.Copy(stdin, sshSide); stdin.Close(); close(inDone) }()
	go func() { io.Copy(sshSide, stdout); sshSide.Close(); close(outDone) }()
	<-outDone
	err = cmd.Wait()
	<-inDone
	if err != nil {
		return fmt.Errorf("SSH 内核退出：%w\n%s", err, strings.TrimSpace(string(logs.data)))
	}
	return nil
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
		ln, err = net.Listen("tcp", address)
		if err != nil {
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
func (m *Manager) Package(platform, gateway string) ([]byte, string, error) {
	return m.bundle.Package(platform, gateway, m.token)
}

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
