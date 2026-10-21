package device

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/proto"
)

type testHandler struct {
	up       chan bool
	down     chan string
	messages chan *proto.Msg
}

func (h *testHandler) KernelUp(_ string, _ *proto.Msg, local bool) { h.up <- local }
func (h *testHandler) KernelDown(name string)                      { h.down <- name }
func (h *testHandler) KernelMsg(_ string, m *proto.Msg)            { h.messages <- m }

func setupManager(t *testing.T) (*Manager, *testHandler, string) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &testHandler{up: make(chan bool, 10), down: make(chan string, 10), messages: make(chan *proto.Msg, 100)}
	hub := kernel.NewHub("test-token", h, log)
	if err := hub.EnablePairing(filepath.Join(dir, "paired.json")); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.Abs("../../..")
	m, err := New(ctx, dir, FindBundle(root), hub, log)
	if err != nil {
		t.Fatal(err)
	}
	return m, h, dir
}
func (h *testHandler) expect(t *testing.T, typ string) *proto.Msg {
	t.Helper()
	select {
	case m := <-h.messages:
		if m.T != typ {
			t.Fatalf("want %s got %+v", typ, m)
		}
		return m
	case <-time.After(8 * time.Second):
		t.Fatalf("没有收到 %s", typ)
		return nil
	}
}

func TestConnectionsPersistWithoutReturningPassword(t *testing.T) {
	m, _, dir := setupManager(t)
	id, err := m.Save(Connection{Name: "server", Host: "my-alias", Password: "private-test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	info := m.List()
	b, _ := json.Marshal(info)
	if len(info) != 1 || !info[0].HasPassword || strings.Contains(string(b), "private-test-secret") {
		t.Fatalf("密码未隔离: %s", b)
	}
	st, _ := os.Stat(filepath.Join(dir, "connections.json"))
	if st.Mode().Perm() != 0600 {
		t.Fatalf("credential file mode %v", st.Mode())
	}
	_, err = m.Save(Connection{ID: id, Name: "server", Host: "new-alias"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.List()[0].HasPassword {
		t.Fatal("留空丢失已保存密码")
	}
	_, err = m.Save(Connection{ID: id, Name: "server", Host: "new-alias", ClearPassword: true})
	if err != nil {
		t.Fatal(err)
	}
	if m.List()[0].HasPassword {
		t.Fatal("清除密码没有生效")
	}
	m2, err := New(m.ctx, dir, m.bundle, m.hub, m.log)
	if err != nil || m2.List()[0].Host != "new-alias" {
		t.Fatalf("reload: %v", err)
	}
	if err := m.Delete(id); err != nil || len(m.List()) != 0 {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.Save(Connection{ID: "../bad", Name: "server", Host: "host"}); err == nil {
		t.Fatal("不应允许外部指定记录 ID")
	}
}

func TestSSHDeploysRealKernelAndRunsToolsOverStdio(t *testing.T) {
	m, h, dir := setupManager(t)
	if m.bundle.Dir == "" {
		t.Skip("先构建 kernel 以运行真实进程集成测试")
	}
	remote := filepath.Join(dir, "server-home")
	work := filepath.Join(remote, "project")
	os.MkdirAll(work, 0700)
	os.WriteFile(filepath.Join(work, "input.txt"), []byte("ssh-managed-tool-ok"), 0600)
	t.Setenv("NANO_TEST_REMOTE_HOME", remote)
	script := `#!/bin/sh
if [ -n "${SSH_ASKPASS:-}" ]; then
  [ "$("$SSH_ASKPASS")" = "private-test-secret" ] || exit 99
fi
while [ $# -gt 0 ]; do case "$1" in -T) shift;; -o|-p|-l|-i) shift 2;; --) shift;break;; *) break;; esac;done
shift
export HOME="$NANO_TEST_REMOTE_HOME"
cd "$HOME"
exec sh -c "$*"
`
	m.sshProgram = filepath.Join(dir, "ssh-test")
	os.WriteFile(m.sshProgram, []byte(script), 0700)
	id, err := m.Save(Connection{Name: "test-server", Host: "my-alias", Port: 2222, User: "test-user", Password: "private-test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Connect(id); err != nil {
		t.Fatal(err)
	}
	select {
	case local := <-h.up:
		if local {
			t.Fatal("SSH 服务器被误认为本机")
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("服务器没有接入: %+v", m.List())
	}
	if m.List()[0].State != "online" {
		t.Fatal(m.List())
	}
	if _, err := m.Save(Connection{ID: id, Name: "test-server", Host: "other"}); err == nil {
		t.Fatal("运行中应先断开再修改")
	}
	m.hub.Send("test-server", proto.Msg{T: proto.TInput, SID: "ssh-session", From: "cli:local", Mode: proto.ModeAuto, Dirs: []string{work}})
	h.expect(t, proto.TModel)
	m.hub.Send("test-server", proto.Msg{T: proto.TTurn, SID: "ssh-session", Reason: proto.ReasonToolUse, Calls: []proto.Call{{ID: "read", Name: "Read", Args: json.RawMessage(`{"path":"input.txt"}`)}}})
	result := h.expect(t, proto.TResult)
	if result.OK == nil || !*result.OK || !strings.Contains(result.Content, "ssh-managed-tool-ok") {
		t.Fatalf("工具没有在服务器运行: %+v", result)
	}
	h.expect(t, proto.TModel)
	m.hub.Send("test-server", proto.Msg{T: proto.TTurn, SID: "ssh-session", Reason: proto.ReasonFinal})
	h.expect(t, proto.TDone)
	m.mu.Lock()
	done := m.running[id].done
	m.mu.Unlock()
	if err := m.Disconnect(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SSH 内核断开后没有退出")
	}
	select {
	case <-h.down:
	case <-time.After(3 * time.Second):
		t.Fatal("网关还认为 SSH 内核在线")
	}
	if m.List()[0].State != "offline" {
		t.Fatal(m.List())
	}
}

func TestCancelDuringDeployment(t *testing.T) {
	m, _, dir := setupManager(t)
	m.sshProgram = filepath.Join(dir, "slow-ssh")
	os.WriteFile(m.sshProgram, []byte("#!/bin/sh\nexec sleep 60\n"), 0700)
	id, err := m.Save(Connection{Name: "slow", Host: "host"})
	if err != nil {
		t.Fatal(err)
	}
	m.Connect(id)
	m.mu.Lock()
	done := m.running[id].done
	m.mu.Unlock()
	m.Disconnect(id)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("部署中无法取消")
	}
	if m.List()[0].State != "offline" {
		t.Fatal(m.List())
	}
}

func TestAutoConnectAfterRestartCanBeStoppedWhileWaiting(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("需要 OpenSSH")
	}
	m, _, dir := setupManager(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	id, err := m.Save(Connection{Name: "retry", Host: "127.0.0.1", Port: port, AutoConnect: true})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(m.ctx, dir, m.bundle, m.hub, m.log)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for restarted.List()[0].State != "reconnecting" {
		if time.Now().After(deadline) {
			t.Fatal("没有自动连接并进入等待重连", restarted.List())
		}
		time.Sleep(10 * time.Millisecond)
	}
	restarted.mu.Lock()
	done := restarted.running[id].done
	restarted.mu.Unlock()
	restarted.Disconnect(id)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("重连等待中无法断开")
	}
	if restarted.List()[0].State != "offline" {
		t.Fatal(restarted.List())
	}
}

func TestSSHConfigAliasesAndIncludes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh/hosts"), 0700)
	os.WriteFile(filepath.Join(home, ".ssh/config"), []byte("Include hosts/*\nHost primary *.example !blocked\n  HostName example.test\n"), 0600)
	os.WriteFile(filepath.Join(home, ".ssh/hosts/work"), []byte("Host secondary\n  Port 2222\n"), 0600)
	if got := strings.Join(SSHHosts(), ","); got != "primary,secondary" {
		t.Fatal(got)
	}
}

func TestPackagesIncludeLaunchersConfigAndPortableSources(t *testing.T) {
	m, _, _ := setupManager(t)
	for _, platform := range []string{"linux", "windows"} {
		data, _, err := m.Package(platform, "tcp:example.test:7700")
		if err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{}
		if platform == "linux" {
			gz, _ := gzip.NewReader(bytes.NewReader(data))
			tr := tar.NewReader(gz)
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(tr)
				files[h.Name] = b
			}
			gz.Close()
		} else {
			zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range zr.File {
				r, _ := f.Open()
				b, _ := io.ReadAll(r)
				r.Close()
				files[f.Name] = b
			}
		}
		if !strings.Contains(string(files["kernel.conf"]), "gateway = tcp:example.test:7700") || len(files["kernel-src.tar.gz"]) == 0 || len(files["start.ps1"]) == 0 || len(files["start.sh"]) == 0 {
			t.Fatal("启动包缺少配置、源码或启动脚本")
		}
		if strings.Contains(string(files["kernel.conf"]), "token") || strings.Contains(string(files["kernel.conf"]), "test-token") {
			t.Fatal("启动包里不该有网关的口令（电脑第一次连接时配对）")
		}
		if platform == "windows" && !bytes.HasPrefix(files["start.ps1"], []byte{0xef, 0xbb, 0xbf}) {
			t.Fatal("PowerShell 启动器应带 UTF-8 BOM")
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestLinuxPackageBuildsFromSourceAndConnects(t *testing.T) {
	for _, tool := range []string{"cc", "make"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("需要 C 编译器和 make")
		}
	}
	m, h, _ := setupManager(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go m.hub.Serve(m.ctx, ln)
	data, _, err := (Bundle{}).Package("linux", "tcp:"+ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	gz, _ := gzip.NewReader(bytes.NewReader(data))
	tr := tar.NewReader(gz)
	for {
		head, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, head.Name)
		os.MkdirAll(filepath.Dir(path), 0700)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, os.FileMode(head.Mode))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(f, tr)
		f.Close()
	}
	gz.Close()
	ctx, cancel := context.WithTimeout(m.ctx, 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(dir, "start.sh"), "--saved")
	cmd.Dir = dir
	var output syncBuffer // 内核还在跑的时候就要读它的输出
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// 包里没有口令：内核先请求配对，网页上允许之后拿到专用口令，重新连上来
	var req kernel.PairRequest
	for req.ID == "" {
		if reqs := m.hub.PairRequests(); len(reqs) > 0 {
			req = reqs[0]
		} else if ctx.Err() != nil {
			cmd.Wait()
			t.Fatalf("源码启动包没有请求配对: %s", output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(req.Code) != 6 || !strings.Contains(output.String(), req.Code) {
		t.Fatalf("内核没有打印确认码 %s: %s", req.Code, output.String())
	}
	if err := m.hub.DecidePair(req.ID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.up:
	case <-ctx.Done():
		cmd.Wait()
		t.Fatalf("源码启动包没有接入网关: %s", output.String())
	}
	if b, err := os.ReadFile(filepath.Join(dir, "paired.token")); err != nil || len(strings.TrimSpace(string(b))) != 64 {
		t.Fatalf("配对口令没有保存: %v %q", err, b)
	}
	// 启动脚本最后 exec 内核，停止后不会留下编译器或子进程。
	cancel()
	cmd.Wait()
	if _, err := os.Stat(filepath.Join(dir, "bin/nano-kernel")); err != nil {
		t.Fatal("没有生成内核", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin/nano-exec")); err != nil {
		t.Fatal("没有生成执行器", err)
	}
}

func TestRealOpenSSHConnection(t *testing.T) {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Skip("没有 sshd")
	}
	m, h, dir := setupManager(t)
	if m.bundle.Dir == "" {
		t.Skip("先构建内核")
	}
	for _, name := range []string{"host-key", "client-key"} {
		cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(dir, name))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, out)
		}
	}
	remote := filepath.Join(dir, "remote")
	os.MkdirAll(remote, 0700)
	force := filepath.Join(dir, "remote-command")
	os.WriteFile(force, []byte("#!/bin/sh\nexport HOME="+shellQuote(remote)+"\ncd \"$HOME\"\nexec sh -c \"$SSH_ORIGINAL_COMMAND\"\n"), 0700)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	usr, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	conf := fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nHostKey %s\nPidFile %s\nAuthorizedKeysFile %s\nStrictModes no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nAllowUsers %s\nForceCommand %s\nLogLevel ERROR\n", port, filepath.Join(dir, "host-key"), filepath.Join(dir, "sshd.pid"), filepath.Join(dir, "client-key.pub"), usr.Username, force)
	confPath := filepath.Join(dir, "sshd.conf")
	os.WriteFile(confPath, []byte(conf), 0600)
	logPath := filepath.Join(dir, "sshd.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command(sshd, "-D", "-e", "-f", confPath)
	server.Dir = dir
	server.Stdout = logFile
	server.Stderr = logFile
	if err := server.Start(); err != nil {
		logFile.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Process.Kill(); server.Wait(); logFile.Close() })
	address := fmt.Sprintf("127.0.0.1:%d", port)
	ready := false
	for i := 0; i < 100; i++ {
		c, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err == nil {
			c.Close()
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("sshd 没有启动: %s", b)
	}
	// 独立 known_hosts 和配置，整个验证不改动用户的 SSH 配置。
	wrapper := filepath.Join(dir, "ssh-client")
	os.WriteFile(wrapper, []byte("#!/bin/sh\nexec ssh -F /dev/null -o UserKnownHostsFile="+shellQuote(filepath.Join(dir, "known_hosts"))+" \"$@\"\n"), 0700)
	m.sshProgram = wrapper
	id, err := m.Save(Connection{Name: "real-ssh", Host: "127.0.0.1", Port: port, User: usr.Username, IdentityFile: filepath.Join(dir, "client-key")})
	if err != nil {
		t.Fatal(err)
	}
	m.Connect(id)
	select {
	case local := <-h.up:
		if local {
			t.Fatal("远程标记错误")
		}
	case <-time.After(15 * time.Second):
		b, _ := os.ReadFile(logPath)
		t.Fatalf("真实 SSH 没有接入: %+v\n%s", m.List(), b)
	}
	// 服务器上的配置里没有网关的口令（stdio 通道由网关自己建立）
	matches, _ := filepath.Glob(filepath.Join(remote, ".local/share/nano-agent/managed/*/kernel.conf"))
	if len(matches) != 1 {
		t.Fatalf("服务器上没有部署内核配置：%v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); strings.Contains(string(b), "token") {
		t.Fatalf("服务器上留下了口令：%s", b)
	}
	m.mu.Lock()
	done := m.running[id].done
	m.mu.Unlock()
	m.Disconnect(id)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("真实 SSH 断开后进程没有退出")
	}
}
