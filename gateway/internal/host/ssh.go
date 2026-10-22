package host

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// sshConn 是一个连着（或者正在连）的 SSH 服务器。网关保持一条主连接（ssh -M -N），
// 执行器、列目录、传文件都通过它复用，不用每次重新登录（密码只在建立主连接时用一次）。
type sshConn struct {
	m      *Manager
	c      Connection
	ctl    string // ControlPath
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	ready    bool
	landlock int
	osName   string
	master   *exec.Cmd
}

const (
	probeTimeout = 3 * time.Minute // 检查、上传、现编执行器的时限
	recheckEvery = 5 * time.Minute
)

// baseArgs 是每条 ssh 命令都带的选项
func (h *sshConn) baseArgs() []string {
	c := h.c
	args := []string{"-T", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "ControlPath=" + h.ctl}
	if c.Port != 0 {
		args = append(args, "-p", fmt.Sprint(c.Port))
	}
	if c.User != "" {
		args = append(args, "-l", c.User)
	}
	if c.IdentityFile != "" {
		args = append(args, "-i", expandHome(c.IdentityFile))
	}
	return args
}

// masterCmd 建立主连接。有密码时经 SSH_ASKPASS 从只有自己能读的文件里给，不进命令行参数。
func (h *sshConn) masterCmd(ctx context.Context) (*exec.Cmd, error) {
	args := append(h.baseArgs(), "-M", "-N", "-o", "ControlMaster=yes", "-o", "ControlPersist=no")
	if h.c.Password == "" {
		args = append(args, "-o", "BatchMode=yes")
	} else {
		args = append(args, "-o", "BatchMode=no", "-o", "NumberOfPasswordPrompts=1")
	}
	cmd := exec.CommandContext(ctx, h.m.sshProgram, append(args, "--", h.c.Host)...)
	if h.c.Password != "" {
		dir := filepath.Join(h.m.dir, "ssh", h.c.ID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "password"), []byte(h.c.Password), 0o600); err != nil {
			return nil, err
		}
		ask := filepath.Join(dir, "askpass.sh")
		if err := os.WriteFile(ask, []byte("#!/bin/sh\nexec cat \"$NANO_SSH_PASSWORD_FILE\"\n"), 0o700); err != nil {
			return nil, err
		}
		cmd.Env = append(os.Environ(), "SSH_ASKPASS="+ask, "SSH_ASKPASS_REQUIRE=force", "DISPLAY=nano-agent",
			"NANO_SSH_PASSWORD_FILE="+filepath.Join(dir, "password"))
	}
	return cmd, nil
}

// muxCmd 在服务器上运行 script（sh -c），走主连接。主连接断了也不会去重新登录（BatchMode），直接失败。
func (h *sshConn) muxCmd(script string) *exec.Cmd {
	args := append(h.baseArgs(), "-o", "ControlMaster=no", "-o", "BatchMode=yes", "--", h.c.Host, "sh -c "+shellQuote(script))
	return exec.Command(h.m.sshProgram, args...)
}

// check 看主连接还在不在
func (h *sshConn) check(ctx context.Context) bool {
	args := append(h.baseArgs(), "-O", "check", "--", h.c.Host)
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return exec.CommandContext(cctx, h.m.sshProgram, args...).Run() == nil
}

func (h *sshConn) exe() string {
	return `"$HOME/.nano-agent/nano-exec-` + h.m.exec.Ver + `"`
}

func (h *sshConn) state() (ready bool, landlock int, osName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ready, h.landlock, h.osName
}

// lost：执行时发现连不上了。关掉主连接，由 session 重新连。
func (h *sshConn) lost() {
	h.mu.Lock()
	m := h.master
	h.ready = false
	h.mu.Unlock()
	if m != nil && m.Process != nil {
		m.Process.Kill()
	}
}

// session 建立主连接、准备好执行器，然后一直等到连接断开（或者 ctx 结束）。
func (h *sshConn) session(ctx context.Context) error {
	id := h.c.ID
	h.m.setStatus(id, "connecting", "正在建立 SSH 连接")
	os.Remove(h.ctl) // 上次没清理掉的控制 socket
	cmd, err := h.masterCmd(ctx)
	if err != nil {
		return err
	}
	var stderr capBuffer
	stderr.max = maxErr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("运行 ssh 失败：%w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		cmd.Process.Kill()
		os.Remove(h.ctl)
	}()
	h.mu.Lock()
	h.master = cmd
	h.mu.Unlock()

	why := func(prefix string) error {
		s := firstLine(h.redact(stderr.b.String()))
		if s == "" {
			s = "SSH 连接已关闭"
		}
		return errors.New(prefix + s)
	}
	for deadline := time.Now().Add(40 * time.Second); !h.check(ctx); {
		select {
		case <-exited:
			return why("连不上：")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.New("SSH 连接超时")
		}
	}
	for first := true; ; first = false {
		if first {
			h.m.setStatus(id, "preparing", "正在检查服务器上的执行器")
		}
		landlock, osName, err := h.prepare(ctx)
		if err != nil {
			return err
		}
		h.mu.Lock()
		h.ready, h.landlock, h.osName = true, landlock, osName
		h.mu.Unlock()
		h.m.setStatus(id, "online", "")
		if first {
			h.m.log.Info("服务器已就绪", "server", h.c.Name, "landlock", landlock)
		}
		select {
		case <-exited:
			h.mu.Lock()
			h.ready = false
			h.mu.Unlock()
			return why("SSH 连接断了：")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(recheckEvery): // 定期再检查一遍（服务器上的执行器可能被删了）
		}
	}
}

func (h *sshConn) redact(s string) string {
	if h.c.Password != "" {
		s = strings.ReplaceAll(s, h.c.Password, "<密码>")
	}
	return s
}

// step 在服务器上跑一段准备脚本，返回输出的最后一行
func (h *sshConn) step(ctx context.Context, script string, input []byte) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, errOut, code, err := run(cctx, h.muxCmd(script), input, false)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if code == 255 && !strings.HasPrefix(last, "OK") && !strings.HasPrefix(last, "NEED") && !strings.HasPrefix(last, "FAIL") {
		return "", fmt.Errorf("连不上：%s", firstLine(errOut))
	}
	if last == "" && err != nil {
		return "", fmt.Errorf("准备执行器失败：%s", firstLine(errOut+" "+err.Error()))
	}
	return last, nil
}

func parseProbe(line string) (int, string, error) {
	var p struct {
		Landlock *int   `json:"landlock"`
		OS       string `json:"os"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(line, "OK ")), &p) != nil || p.Landlock == nil {
		return 0, "", fmt.Errorf("执行器的自检结果看不懂：%s", clip(line, 200))
	}
	osName := strings.ToLower(p.OS)
	if osName == "" {
		osName = "linux"
	}
	if osName == "darwin" {
		osName = "macos"
	}
	return *p.Landlock, osName, nil
}

// dropOldCache 删掉旧版本放在 ~/.cache/nano-agent 的执行器和技能文件；失败不影响使用。
func (h *sshConn) dropOldCache(ctx context.Context) {
	_, _ = h.step(ctx, `rm -rf "$HOME/.cache/nano-agent"`, nil)
}

// prepare 确保服务器上有能用的执行器：检查 → 架构相同就上传 → 跑不了或者架构不同就传源码现编。
func (h *sshConn) prepare(ctx context.Context) (int, string, error) {
	e := h.m.exec
	line, err := h.step(ctx, `f=`+h.exe()+`; if [ -x "$f" ] && o=$("$f" --probe 2>/dev/null); then echo "OK $o"; `+
		`else echo "NEED $(uname -m) $(uname -s) $(command -v cc || command -v gcc || true)"; fi`, nil)
	if err != nil {
		return 0, "", err
	}
	if strings.HasPrefix(line, "OK ") {
		h.dropOldCache(ctx)
		return parseProbe(line)
	}
	f := strings.Fields(strings.TrimPrefix(line, "NEED"))
	if !strings.HasPrefix(line, "NEED") || len(f) == 0 {
		return 0, "", fmt.Errorf("检查执行器失败：%s", clip(line, 200))
	}
	arch, osName, cc := f[0], "linux", ""
	if len(f) > 1 {
		osName = strings.ToLower(f[1])
		if osName == "darwin" {
			osName = "macos"
		}
	}
	if len(f) > 2 {
		cc = f[2]
	}
	if arch == e.Arch {
		h.m.setStatus(h.c.ID, "preparing", "正在上传执行器")
		bin, err := os.ReadFile(e.Upload)
		if err != nil {
			return 0, "", err
		}
		line, err = h.step(ctx, `d="$HOME/.nano-agent"; f=`+h.exe()+`; `+
			`mkdir -p "$d" && chmod 700 "$d" && cat > "$f.tmp" && chmod 700 "$f.tmp" && mv -f "$f.tmp" "$f" || exit 1; `+
			`if o=$("$f" --probe 2>&1); then echo "OK $o"; else rm -f "$f"; echo "FAIL $o"; fi`, bin)
		if err != nil {
			return 0, "", err
		}
		if strings.HasPrefix(line, "OK ") {
			return parseProbe(line)
		}
		if cc == "" || e.SrcTar == "" {
			return 0, "", fmt.Errorf("上传的执行器在服务器上跑不起来（%s）。服务器上装个 C 编译器（cc）就能现编", clip(strings.TrimPrefix(line, "FAIL "), 200))
		}
		h.m.log.Info("服务器跑不了上传的执行器，改为现编", "server", h.c.Name, "why", clip(line, 200))
	} else if cc == "" || e.SrcTar == "" {
		return 0, "", fmt.Errorf("服务器是 %s，网关这边的执行器是 %s，服务器上又没有 C 编译器（cc），没法现编。装上 gcc 后重新连接", arch, e.Arch)
	}
	h.m.setStatus(h.c.ID, "preparing", "正在服务器上编译执行器")
	src, err := os.ReadFile(e.SrcTar)
	if err != nil {
		return 0, "", err
	}
	line, err = h.step(ctx, `d="$HOME/.nano-agent"; s="$d/src-`+e.Ver+`"; f=`+h.exe()+`; `+
		`mkdir -p "$s" && chmod 700 "$d" && cd "$s" && tar xzf - || exit 1; `+
		`if `+shellQuote(cc)+` -std=c11 -O2 -D_DEFAULT_SOURCE -D_POSIX_C_SOURCE=200809L -Isrc -Ithird_party/cjson -o "$f.tmp" `+
		`src/nexec_main.c src/nexec_fs.c src/nexec_proc.c src/sandbox.c src/util.c src/plat_posix.c third_party/cjson/cJSON.c -lm > build.log 2>&1 `+
		`&& mv -f "$f.tmp" "$f"; then if o=$("$f" --probe 2>&1); then echo "OK $o"; else echo "FAIL $o"; fi; `+
		`else echo "FAIL $(tail -n 3 build.log)"; fi`, src)
	if err != nil {
		return 0, "", err
	}
	if strings.HasPrefix(line, "OK ") {
		h.dropOldCache(ctx)
		return parseProbe(line)
	}
	return 0, osName, fmt.Errorf("在服务器上编译执行器失败：%s", clip(strings.TrimPrefix(line, "FAIL "), 300))
}

// sshHost 是一个就绪的 SSH 服务器（实现 Host）
type sshHost struct{ h *sshConn }

func (s sshHost) Exec(ctx context.Context, req []byte) Result {
	out, errOut, code, err := run(ctx, s.h.muxCmd("exec "+s.h.exe()), req, true)
	r := parseResult(out, errOut, code, err)
	if code == 255 && !bytes.Contains(out, []byte(`"ok"`)) && ctx.Err() == nil {
		s.h.lost()
		return Result{Content: fmt.Sprintf("连不上服务器 %s：%s", s.h.c.Name, firstLine(errOut)), Lost: true}
	}
	return r
}

func (s sshHost) Ls(ctx context.Context, path string) (*Listing, error) {
	if path == "" {
		path = "~"
	}
	out, errOut, code, _ := run(ctx, s.h.muxCmd("exec "+s.h.exe()+" --ls "+shellQuote(path)), nil, false)
	if code == 255 && len(out) == 0 {
		s.h.lost()
		return nil, fmt.Errorf("连不上服务器 %s：%s", s.h.c.Name, firstLine(errOut))
	}
	var ls Listing
	if err := json.Unmarshal(out, &ls); err != nil {
		return nil, fmt.Errorf("列目录失败：%s", firstLine(errOut))
	}
	if !ls.OK {
		return nil, errors.New(ls.Detail)
	}
	return &ls, nil
}

func (s sshHost) PutFiles(ctx context.Context, dir string, files []File) error {
	rel, err := relDir(dir)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		p, err := cleanRel(f.Path)
		if err != nil {
			return err
		}
		mode := int64(0o600)
		if f.Exec {
			mode = 0o700
		}
		if err := tw.WriteHeader(&tar.Header{Name: p, Mode: mode, Size: int64(len(f.Data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		tw.Write(f.Data)
	}
	tw.Close()
	q := shellQuote(rel)
	_, errOut, code, err := run(ctx, s.h.muxCmd(`d="$HOME"/`+q+`; rm -rf "$d" && mkdir -p "$d" && tar xf - -C "$d"`), buf.Bytes(), false)
	if code != 0 {
		if code == 255 {
			s.h.lost()
		}
		return fmt.Errorf("传文件到服务器 %s 失败：%s", s.h.c.Name, firstLine(errOut+" "+fmt.Sprint(err)))
	}
	return nil
}
