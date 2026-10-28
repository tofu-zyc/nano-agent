// Package host 管理能执行工具的机器：网关所在的本机，和网关通过 SSH 连接的服务器。
//
// 工具由执行器 nano-exec 执行（每次调用运行一次，套上 Landlock + seccomp 沙箱）。本机直接运行它；
// SSH 服务器上不跑内核也不常驻进程，网关保持一条复用的 SSH 连接（ControlMaster），每次调用通过它
// 在服务器上运行一次执行器。服务器上没有执行器时自动传上去，跑不了就把源码传过去现编。
// 装了内核的电脑由它自己的内核执行，不归这里管。
package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"nano-agent/gateway/internal/paths"
	"nano-agent/gateway/internal/proto"
)

// Executor 是执行器的文件。
type Executor struct {
	Path   string // 本机运行的执行器
	Upload string // 传给服务器的：有静态编译的 nano-exec-static 就用它
	SrcTar string // 服务器上现编用的源码包，没有时为空
	Ver    string // 版本指纹，服务器上的文件名带着它
	Arch   string // 本机架构（uname -m）
}

// FindExecutor 在安装布局的 $NANO_HOME/lib、网关程序旁边的 ../lib、../kernel、
// 配置目录下的 kernel/ 里找执行器。
func FindExecutor(configDir string) (Executor, error) {
	exe, _ := os.Executable()
	for _, d := range []string{
		filepath.Join(paths.Home(), "lib"),
		filepath.Join(filepath.Dir(exe), "../lib"),
		filepath.Dir(exe),
		filepath.Join(filepath.Dir(exe), "../kernel"),
		filepath.Join(configDir, "kernel"),
	} {
		for _, name := range []string{"nano-exec", "nano-exec.exe"} {
			p := filepath.Join(d, name)
			if st, err := os.Stat(p); err != nil || st.IsDir() {
				continue
			}
			return NewExecutor(p)
		}
	}
	return Executor{}, errors.New("找不到执行器 nano-exec（先在仓库根目录运行 make）")
}

// NewExecutor 用 path 这个执行器（同一目录下的 nano-exec-static、nano-exec-src.tar.gz 一起用上）。
func NewExecutor(path string) (Executor, error) {
	path, _ = filepath.Abs(path)
	e := Executor{Path: path, Upload: path}
	dir := filepath.Dir(path)
	if st, err := os.Stat(filepath.Join(dir, "nano-exec-static")); err == nil && !st.IsDir() {
		e.Upload = filepath.Join(dir, "nano-exec-static")
	}
	if st, err := os.Stat(filepath.Join(dir, "nano-exec-src.tar.gz")); err == nil && !st.IsDir() {
		e.SrcTar = filepath.Join(dir, "nano-exec-src.tar.gz")
	}
	b, err := os.ReadFile(e.Upload)
	if err != nil {
		return Executor{}, fmt.Errorf("读不了执行器：%w", err)
	}
	sum := sha256.Sum256(b)
	e.Ver = hex.EncodeToString(sum[:8])
	if out, err := exec.Command("uname", "-m").Output(); err == nil {
		e.Arch = strings.TrimSpace(string(out))
	}
	return e, nil
}

// File 是要放到执行的机器上的一个文件（技能的附带文件）。
type File struct {
	Path string // 相对路径
	Data []byte
	Exec bool
}

// Result 是一次工具调用的结果。Lost 表示连不上那台机器（不是工具本身失败）。
type Result struct {
	OK      bool
	Content string
	Lost    bool
	Sandbox *proto.Sandbox
}

// Listing 是执行器 --ls 列出的目录（只有子目录名）。
type Listing struct {
	OK        bool             `json:"ok"`
	Detail    string           `json:"detail"`
	Path      string           `json:"path"`
	Home      string           `json:"home"`
	Blocked   bool             `json:"blocked"`
	Entries   []proto.DirEntry `json:"entries"`
	Truncated bool             `json:"truncated"`
}

// Host 是一台能执行工具的机器。
type Host interface {
	// Exec 运行一次执行器，req 是一行请求 JSON。ctx 结束时停下（关掉执行器的 stdin，它会清理自己启动的命令）。
	Exec(ctx context.Context, req []byte) Result
	Ls(ctx context.Context, path string) (*Listing, error)
	// PutFiles 把文件放到 dir（~/ 开头）下，先清空 dir，返回执行机器上的实际目录。
	PutFiles(ctx context.Context, dir string, files []File) (string, error)
}

const (
	maxOut = 40 << 20 // 取文件（Fetch）时是 base64，最大 20 MB 的文件

	maxErr = 8 << 10
)

type capBuffer struct {
	b   bytes.Buffer
	max int
	cut bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
			c.cut = true
		} else {
			c.b.Write(p)
		}
	} else if len(p) > 0 {
		c.cut = true
	}
	return len(p), nil
}

type startedKey struct{}

// WithStarted observes the actual process creation, after any queue or probe.
func WithStarted(ctx context.Context, fn func()) context.Context {
	var once sync.Once
	return context.WithValue(ctx, startedKey{}, func() { once.Do(fn) })
}
func Started(ctx context.Context) {
	if fn, ok := ctx.Value(startedKey{}).(func()); ok {
		fn()
	}
}

// run 运行 cmd，把 input 写进它的 stdin。keepOpen 时 stdin 一直开着，直到进程结束或者 ctx 结束
// （执行器看到 stdin 关闭就停下，ssh 会把 EOF 传给服务器上的执行器）。ctx 结束后过一会儿还没退出就杀掉。
func run(ctx context.Context, cmd *exec.Cmd, input []byte, keepOpen bool) (out []byte, errOut string, code int, err error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, "", -1, err
	}
	so, se := &capBuffer{max: maxOut}, &capBuffer{max: maxErr}
	cmd.Stdout, cmd.Stderr = so, se
	if err := cmd.Start(); err != nil {
		return nil, "", -1, err
	}
	Started(ctx)
	done := make(chan struct{})
	go func() {
		stdin.Write(input)
		if !keepOpen {
			stdin.Close()
		}
		select {
		case <-done:
		case <-ctx.Done():
			stdin.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				cmd.Process.Kill()
			}
		}
	}()
	err = cmd.Wait()
	close(done)
	stdin.Close()
	code = -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if so.cut {
		return so.b.Bytes(), se.b.String(), code, errors.New("输出超出上限")
	}
	return so.b.Bytes(), se.b.String(), code, err
}

// parseResult 解析执行器的输出 {"ok":…,"content":…}；执行器没正常给出结果时，按退出情况说明原因。
func parseResult(out []byte, errOut string, code int, err error) Result {
	var r struct {
		OK        *bool   `json:"ok"`
		Content   *string `json:"content"`
		Effective string  `json:"sandbox_effective"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &r) == nil && r.OK != nil && r.Content != nil {
		var state *proto.Sandbox
		if r.Effective != "" {
			state = &proto.Sandbox{Effective: r.Effective}
		}
		return Result{OK: *r.OK, Content: *r.Content, Sandbox: state}
	}
	why := fmt.Sprintf("执行器异常结束（退出码 %d）", code)
	if err != nil && code < 0 {
		why = "执行器没有运行起来：" + err.Error()
	}
	if s := strings.TrimSpace(errOut); s != "" {
		why += "：" + clip(s, 2000)
	}
	return Result{Content: why}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clip(s, 300)
}

// relDir 检查 ~/ 开头的目录，返回 HOME 下的相对路径
func relDir(dir string) (string, error) {
	rel, ok := strings.CutPrefix(dir, "~/")
	clean := path.Clean(rel)
	if !ok || clean != rel || rel == "" || strings.HasPrefix(clean, "..") || path.IsAbs(clean) || strings.ContainsAny(rel, `\:`) {
		return "", fmt.Errorf("目录 %q 不对", dir)
	}
	return clean, nil
}

func cleanRel(p string) (string, error) {
	c := path.Clean(p)
	if p == "" || path.IsAbs(c) || c == "." || c == ".." || strings.HasPrefix(c, "../") || strings.ContainsAny(p, `\:`) {
		return "", fmt.Errorf("文件路径 %q 不对", p)
	}
	return c, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
