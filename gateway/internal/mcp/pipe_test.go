package mcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// loopLink 是本机版的「内核管道」：真的起子进程，把它的 stdout 一行行交回去，
// stdin 一行行收下来 —— 和内核要实现的 pipe_open/pipe_write/pipe_data/pipe_closed 一致。
type loopLink struct {
	mu       sync.Mutex
	stdin    map[string]io.WriteCloser
	procs    map[string]*exec.Cmd
	onData   func(node, pipe, line string)
	onClosed func(node, pipe string, code int, msg string)
}

func (l *loopLink) Open(node, pipe string, argv []string, env map[string]string, cwd string) error {
	if len(argv) == 0 {
		return errors.New("没有命令")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	l.mu.Lock()
	l.stdin[pipe], l.procs[pipe] = in, cmd
	l.mu.Unlock()
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), 32<<20)
		for sc.Scan() {
			l.onData(node, pipe, sc.Text())
		}
		code := 0
		if err := cmd.Wait(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		l.onClosed(node, pipe, code, "")
	}()
	return nil
}

func (l *loopLink) Write(node, pipe string, line []byte) error {
	l.mu.Lock()
	w := l.stdin[pipe]
	l.mu.Unlock()
	if w == nil {
		return errors.New("没有这条管道")
	}
	_, err := w.Write(append(append([]byte{}, line...), '\n'))
	return err
}

func (l *loopLink) Close(node, pipe string) error {
	l.mu.Lock()
	w, cmd := l.stdin[pipe], l.procs[pipe]
	delete(l.stdin, pipe)
	delete(l.procs, pipe)
	l.mu.Unlock()
	if w != nil {
		w.Close()
	}
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
	return nil
}

// machine 指向别的电脑时，走管道透传：列工具、调工具都得通。
func TestPipeTransportToOtherMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("要起子进程")
	}
	t.Setenv("NANO_HOME", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]Server{{Name: "remote", Machine: "windows-pc", Command: exe,
		Args: []string{"-test.run=TestNoSuchTest"}, Env: map[string]string{helperEnv: "1"}, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, nil)
	defer m.Close()
	link := &loopLink{stdin: map[string]io.WriteCloser{}, procs: map[string]*exec.Cmd{}}
	link.onData = func(node, pipe, line string) { m.Pipes().Data(node, pipe, line) }
	link.onClosed = func(node, pipe string, code int, msg string) { m.Pipes().Closed(node, pipe, code, msg) }
	m.SetLink(link)
	ctx := context.Background()
	tools, err := m.Tools(ctx, "remote")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Exposed != "remote__echo" {
		t.Fatalf("工具列表不对：%+v", tools)
	}
	res, err := m.Call(ctx, "remote", "echo", map[string]any{"text": "远端"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ResultText(res); got != "echo:远端" {
		t.Fatalf("管道调用结果不对：%q", got)
	}
}
