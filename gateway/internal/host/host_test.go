package host

import (
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
	"testing"
	"time"

	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/secret"
)

// 测试用仓库里编译好的执行器（make 之后才有）
func testExecutor(t *testing.T) Executor {
	t.Helper()
	root, _ := filepath.Abs("../../..")
	e, err := NewExecutor(filepath.Join(root, "kernel", "nano-exec"))
	if err != nil {
		t.Skip("先在仓库根目录 make：", err)
	}
	return e
}

func request(tool string, args any, dir string) []byte {
	b, _ := json.Marshal(map[string]any{"tool": tool, "args": args, "dirs": []string{dir}, "write": true, "tmp_key": "t", "max_output": 4096, "watch_stdin": true})
	return append(b, '\n')
}

func TestLocalExecLsAndFiles(t *testing.T) {
	e := testExecutor(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := filepath.Join(home, "work")
	data := filepath.Join(home, "gateway-data")
	os.MkdirAll(work, 0o755)
	os.MkdirAll(data, 0o755)
	l := NewLocal(e, []string{data})
	if _, _, ok, detail := l.Probe(); !ok {
		t.Fatal(detail)
	}
	ctx := context.Background()
	if r := l.Exec(ctx, request("Write", map[string]string{"path": "a.txt", "content": "本机执行\n"}, work)); !r.OK {
		t.Fatalf("write: %+v", r)
	}
	if r := l.Exec(ctx, request("Read", map[string]string{"path": "a.txt"}, work)); !r.OK || !strings.Contains(r.Content, "本机执行") {
		t.Fatalf("read: %+v", r)
	}
	if r := l.Exec(ctx, request("Read", map[string]string{"path": "/etc/hostname"}, work)); r.OK {
		t.Fatalf("工作目录外的文件不该读得到：%+v", r)
	}
	// 停止：关掉 stdin，执行器停下命令
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	start := time.Now()
	l.Exec(cctx, request("Shell", map[string]string{"command": "sleep 30"}, work))
	cancel()
	if time.Since(start) > 5*time.Second {
		t.Fatal("停止后执行器没有及时退出")
	}

	ls, err := l.Ls(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	blocked := map[string]bool{}
	for _, en := range ls.Entries {
		blocked[en.Name] = en.Blocked
	}
	if !blocked["gateway-data"] || blocked["work"] {
		t.Fatalf("网关的数据目录不能当工作目录：%+v", ls.Entries)
	}
	if !ls.Blocked {
		t.Fatal("包含网关数据目录的上级目录也不能当工作目录")
	}

	files := []File{{Path: "SKILL.md", Data: []byte("说明")}, {Path: "scripts/run.sh", Data: []byte("echo hi"), Exec: true}}
	if err := l.PutFiles(ctx, "~/.nano-agent/skills/pc/demo", files); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(home, ".nano-agent/skills/pc/demo/scripts/run.sh"))
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("附带的脚本：%v %v", st, err)
	}
	if err := l.PutFiles(ctx, "~/../etc", files); err == nil {
		t.Fatal("目录不能跑到 HOME 外面")
	}
	if err := l.PutFiles(ctx, "~/x", []File{{Path: "../escape"}}); err == nil {
		t.Fatal("文件路径不能跑到目录外面")
	}
}

// 假的 ssh：-M 建立主连接（用控制路径上的文件表示，一直挂着直到被杀）；-O check 看主连接在不在；
// 其他命令在 $REMOTE_ROOT/<目标> 这个“服务器主目录”里用 sh 执行。目标 down 连不上。
const fakeSSH = `#!/bin/sh
master=0; op=""; ctl=""
while [ $# -gt 0 ]; do
  case "$1" in
    -M) master=1; shift ;;
    -N|-T) shift ;;
    -O) op=$2; shift 2 ;;
    -o) case "$2" in ControlPath=*) ctl=${2#ControlPath=} ;; esac; shift 2 ;;
    -p|-l|-i) shift 2 ;;
    --) shift; break ;;
    *) break ;;
  esac
done
target=$1; shift
if [ "$target" = down ]; then echo "ssh: connect to host down port 22: Connection refused" >&2; exit 255; fi
if [ -n "$op" ]; then [ -e "$ctl" ]; exit $?; fi
if [ "$master" = 1 ]; then touch "$ctl"; while :; do sleep 1; done; fi
[ -e "$ctl" ] || { echo "ssh: control socket missing" >&2; exit 255; }
export HOME="$REMOTE_ROOT/$target"; mkdir -p "$HOME"; cd "$HOME" && exec sh -c "$*"
`

func setupManager(t *testing.T) (*Manager, string) {
	t.Helper()
	e := testExecutor(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "remote")
	os.MkdirAll(root, 0o755)
	ssh := filepath.Join(dir, "fake-ssh")
	os.WriteFile(ssh, []byte(fakeSSH), 0o755)
	t.Setenv("REMOTE_ROOT", root)
	t.Setenv("NANO_SSH_PROGRAM", ssh)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "run"))
	os.MkdirAll(filepath.Join(dir, "run"), 0o700)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := New(ctx, filepath.Join(dir, "data"), e, nil, kernel.NewHub("tok", nil, log), log)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "data"), 0o700)
	return m, root
}

func waitState(t *testing.T, m *Manager, id, state string, d time.Duration) Info {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		for _, i := range m.List() {
			if i.ID == id && i.State == state {
				return i
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到 %s：%+v", state, m.List())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSSHPreparesExecutorAndRunsTools(t *testing.T) {
	m, root := setupManager(t)
	id, err := m.Save(Connection{Name: "box", Host: "box"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := m.Host("nope"); ok {
		t.Fatal("没有的机器")
	}
	if h, info, ok := m.Host("box"); !ok || h != nil || info.State != "offline" {
		t.Fatalf("还没连：%v %+v", h, info)
	}
	m.Connect(id)
	info := waitState(t, m, id, "online", 30*time.Second)
	if !info.Ready || info.Landlock == 0 {
		t.Fatalf("info: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(root, "box/.nano-agent/nano-exec-"+m.exec.Ver)); err != nil {
		t.Fatal("执行器没有传到服务器上", err)
	}
	h, _, _ := m.Host("box")
	work := filepath.Join(root, "box", "proj")
	os.MkdirAll(work, 0o755)
	ctx := context.Background()
	if r := h.Exec(ctx, request("Write", map[string]string{"path": "r.txt", "content": "remote\n"}, "~/proj")); !r.OK {
		t.Fatalf("write: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "r.txt")); string(b) != "remote\n" {
		t.Fatalf("文件没写在服务器上：%q", b)
	}
	ls, err := h.Ls(ctx, "~")
	if err != nil || ls.Path != filepath.Join(root, "box") {
		t.Fatalf("ls: %+v %v", ls, err)
	}
	if err := h.PutFiles(ctx, "~/.nano-agent/skills/pc/demo", []File{{Path: "data.txt", Data: []byte("技能")}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "box/.nano-agent/skills/pc/demo/data.txt")); string(b) != "技能" {
		t.Fatalf("技能文件：%q", b)
	}

	// 断开：主连接关掉，机器用不了
	m.Disconnect(id)
	waitState(t, m, id, "offline", 5*time.Second)
	if h, _, _ := m.Host("box"); h != nil {
		t.Fatal("断开后还能用")
	}
}

func TestSSHUnreachableAndBuildFromSource(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("需要 C 编译器")
	}
	m, root := setupManager(t)
	if m.exec.SrcTar == "" {
		t.Skip("没有执行器的源码包")
	}
	down, _ := m.Save(Connection{Name: "down", Host: "down", Password: "secret-pw"})
	m.Connect(down)
	if info := waitState(t, m, down, "reconnecting", 10*time.Second); !strings.Contains(info.Detail, "Connection refused") || strings.Contains(info.Detail, "secret-pw") {
		t.Fatalf("连不上的原因：%+v", info)
	}
	if b, _ := os.ReadFile(filepath.Join(m.dir, "connections.json")); !strings.Contains(string(b), "secret-pw") {
		t.Fatal("密码要存在网关的数据目录里")
	}
	for _, i := range m.List() {
		if b, _ := json.Marshal(i); strings.Contains(string(b), "secret-pw") {
			t.Fatal("列表里不能有密码")
		}
	}
	m.Disconnect(down)

	// 服务器的架构和本机不一样：把源码传过去现编
	bin := filepath.Join(root, "fakebin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "uname"), []byte("#!/bin/sh\necho riscv64\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	id, _ := m.Save(Connection{Name: "rv", Host: "rv"})
	m.Connect(id)
	waitState(t, m, id, "online", 2*time.Minute)
	if _, err := os.Stat(filepath.Join(root, "rv/.nano-agent/src-"+m.exec.Ver+"/build.log")); err != nil {
		t.Fatal("应该在服务器上现编", err)
	}
	h, _, _ := m.Host("rv")
	os.MkdirAll(filepath.Join(root, "rv", "proj"), 0o755)
	if r := h.Exec(context.Background(), request("Shell", map[string]string{"command": "echo built-$((6*7))"}, "~/proj")); !r.OK || !strings.Contains(r.Content, "built-42") {
		t.Fatalf("exec: %+v", r)
	}
}

func TestSSHHostsConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh/hosts"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh/config"), []byte("Include hosts/*\nHost primary *.example !blocked\n  HostName example.test\n"), 0o600)
	os.WriteFile(filepath.Join(home, ".ssh/hosts/work"), []byte("Host secondary\n  Port 2222\n"), 0o600)
	if got := strings.Join(SSHHosts(), ","); got != "primary,secondary" {
		t.Fatal(got)
	}
}

func TestSaveValidation(t *testing.T) {
	m, _ := setupManager(t)
	for _, c := range []Connection{{Name: "本机", Host: "x"}, {Name: "a b", Host: "x"}, {Name: "ok", Host: "-oProxyCommand=x"}, {Name: "ok", Host: "x", User: "-x"}} {
		if _, err := m.Save(c); err == nil {
			t.Fatalf("应该拒绝：%+v", c)
		}
	}
	id, _ := m.Save(Connection{Name: "a", Host: "x", Password: "pw"})
	if _, err := m.Save(Connection{Name: "a", Host: "y"}); err == nil {
		t.Fatal("同名")
	}
	m.Save(Connection{ID: id, Name: "a", Host: "y"})
	if !m.List()[0].HasPassword {
		t.Fatal("留空要保留原来的密码")
	}
	m.Save(Connection{ID: id, Name: "a", Host: "y", ClearPassword: true})
	if m.List()[0].HasPassword {
		t.Fatal("清除密码没生效")
	}
}

// 真实的 OpenSSH：临时起一个 sshd，用密钥登录
func TestRealOpenSSH(t *testing.T) {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		if sshd = "/usr/sbin/sshd"; !fileExists(sshd) {
			t.Skip("没有 sshd")
		}
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("没有 ssh")
	}
	m, _ := setupManager(t)
	dir := t.TempDir()
	for _, name := range []string{"host-key", "client-key"} {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(dir, name)).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, out)
		}
	}
	remote := filepath.Join(dir, "remote")
	os.MkdirAll(remote, 0o700)
	force := filepath.Join(dir, "remote-command")
	os.WriteFile(force, []byte("#!/bin/sh\nexport HOME="+shellQuote(remote)+"\ncd \"$HOME\"\nexec sh -c \"$SSH_ORIGINAL_COMMAND\"\n"), 0o700)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	usr, _ := user.Current()
	conf := fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nHostKey %s\nPidFile %s\nAuthorizedKeysFile %s\nStrictModes no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nAllowUsers %s\nForceCommand %s\nLogLevel ERROR\n",
		port, filepath.Join(dir, "host-key"), filepath.Join(dir, "sshd.pid"), filepath.Join(dir, "client-key.pub"), usr.Username, force)
	os.WriteFile(filepath.Join(dir, "sshd.conf"), []byte(conf), 0o600)
	logf, _ := os.Create(filepath.Join(dir, "sshd.log"))
	server := exec.Command(sshd, "-D", "-e", "-f", filepath.Join(dir, "sshd.conf"))
	server.Stdout, server.Stderr = logf, logf
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Process.Kill(); server.Wait(); logf.Close() })
	for i := 0; i < 100; i++ {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 20*time.Millisecond); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 独立的 known_hosts 和配置，不动用户自己的 SSH 设置
	wrapper := filepath.Join(dir, "ssh-client")
	os.WriteFile(wrapper, []byte("#!/bin/sh\nexec ssh -F /dev/null -o UserKnownHostsFile="+shellQuote(filepath.Join(dir, "known_hosts"))+" \"$@\"\n"), 0o700)
	m.sshProgram = wrapper
	id, err := m.Save(Connection{Name: "real", Host: "127.0.0.1", Port: port, User: usr.Username, IdentityFile: filepath.Join(dir, "client-key")})
	if err != nil {
		t.Fatal(err)
	}
	m.Connect(id)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if h, _, _ := m.Host("real"); h != nil {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(dir, "sshd.log"))
			t.Fatalf("真实 SSH 没有就绪：%+v\n%s", m.List(), b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	h, _, _ := m.Host("real")
	os.MkdirAll(filepath.Join(remote, "proj"), 0o755)
	if r := h.Exec(context.Background(), request("Shell", map[string]string{"command": "echo real-$((6*7))"}, "~/proj")); !r.OK || !strings.Contains(r.Content, "real-42") {
		t.Fatalf("exec: %+v", r)
	}
	if ls, err := h.Ls(context.Background(), "~"); err != nil || ls.Path != remote {
		t.Fatalf("ls: %+v %v", ls, err)
	}
	m.Disconnect(id)
	waitState(t, m, id, "offline", 10*time.Second)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestConnectionPasswordEncrypted(t *testing.T) {
	home := t.TempDir()
	if err := secret.Init(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(secret.Reset)
	dir := t.TempDir()
	m, err := New(context.Background(), dir, Executor{}, nil, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.Save(Connection{Name: "box", Host: "127.0.0.1", Password: "hunter2-secret"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "connections.json"))
	if strings.Contains(string(b), "hunter2-secret") || !strings.Contains(string(b), "enc:v1:") {
		t.Fatalf("connections.json 没加密：%s", b)
	}
	m2, err := New(context.Background(), dir, Executor{}, nil, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.records[id].Password; got != "hunter2-secret" {
		t.Fatalf("密码读回来不对：%q", got)
	}
}
