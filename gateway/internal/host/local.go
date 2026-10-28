package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/paths"
)

// LocalName 是网关所在的机器在工作目录里的名字。
const LocalName = "本机"

// Local 是网关所在的机器：网关直接运行执行器。
type Local struct {
	exec  Executor
	guard []string // 不能用作工作目录的路径：网关的配置、数据和执行器（解析过符号链接）

	mu           sync.Mutex
	probed       bool
	landlock     int
	sandboxModes []string
	osName       string
	detail       string
}

func NewLocal(e Executor, guard []string) *Local {
	l := &Local{exec: e}
	for _, g := range append(guard, filepath.Dir(e.Path)) {
		if g == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(g); err == nil {
			g = real
		}
		l.guard = append(l.guard, g)
	}
	return l
}

// Probe 运行一次执行器的自检，返回沙箱版本、系统和架构（0 表示没有沙箱）。失败时 detail 是原因。
func (l *Local) Probe() (landlock int, osName string, ok bool, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.probed {
		l.probed = true
		l.osName = runtime.GOOS
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, l.exec.Path, "--probe").Output()
		var p struct {
			Landlock *int   `json:"landlock"`
			OS       string `json:"os"`
		}
		if err != nil || json.Unmarshal(out, &p) != nil || p.Landlock == nil {
			l.detail = "执行器 " + l.exec.Path + " 运行不了"
		} else {
			l.landlock = *p.Landlock
			l.sandboxModes = probeSandboxModes(string(out))
			if p.OS != "" {
				l.osName = p.OS
			}
		}
	}
	return l.landlock, l.osName, l.detail == "", l.detail
}

func (l *Local) Exec(ctx context.Context, req []byte) Result {
	if _, _, ok, detail := l.Probe(); !ok {
		return Result{Content: detail}
	}
	if err := checkSandboxRequest(req, l.sandboxModes); err != nil {
		return Result{Content: err.Error()}
	}
	out, errOut, code, err := run(ctx, exec.Command(l.exec.Path), req, true)
	return parseResult(out, errOut, code, err)
}

// Blocked 判断 path（已解析符号链接的绝对路径）是不是包含 nano-agent 自己
func (l *Local) Blocked(path string) bool {
	for _, g := range l.guard {
		if g == path || strings.HasPrefix(g, strings.TrimSuffix(path, "/")+"/") {
			return true
		}
	}
	return false
}

func (l *Local) Ls(ctx context.Context, path string) (*Listing, error) {
	out, err := exec.CommandContext(ctx, l.exec.Path, "--ls", path).Output()
	if err != nil && len(out) == 0 {
		return nil, errors.New("执行器运行不了：" + err.Error())
	}
	return l.mark(out)
}

func (l *Local) mark(out []byte) (*Listing, error) {
	var ls Listing
	if err := json.Unmarshal(out, &ls); err != nil {
		return nil, errors.New("执行器的输出看不懂")
	}
	if !ls.OK {
		return nil, errors.New(ls.Detail)
	}
	ls.Blocked = ls.Blocked || l.Blocked(ls.Path)
	for i, e := range ls.Entries {
		if l.Blocked(filepath.Join(ls.Path, e.Name)) {
			ls.Entries[i].Blocked = true
		}
	}
	return &ls, nil
}

func (l *Local) PutFiles(_ context.Context, dir string, files []File) (string, error) {
	rel, err := relDir(dir)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, rel)
	if suffix, ok := strings.CutPrefix(filepath.ToSlash(rel), ".nano-agent/"); ok {
		root = filepath.Join(paths.Home(), filepath.FromSlash(suffix))
	}
	if err := os.RemoveAll(root); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	for _, f := range files {
		p, err := cleanRel(f.Path)
		if err != nil {
			return "", err
		}
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return "", err
		}
		mode := os.FileMode(0o600)
		if f.Exec {
			mode = 0o700
		}
		if err := os.WriteFile(full, f.Data, mode); err != nil {
			return "", err
		}
	}
	return root, nil
}
