package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/session"
)

// 真实 C 内核 + 真实 Go 网关的协议联调。
//
// 前面的测试用假内核（Go 结构体）和假网关（Python 字典）各测一边，都发现不了
// “C 序列化出来的字段 Go 解不开”这类契约问题。这里启动真正的 nano-kernel，
// 让它的帧经过真正的 hub 解码和路由，跑完一轮带工具调用的对话。
//
// 没有编译好的内核时跳过（不会让普通 go test 失败）：
//
//	make -C kernel nano-kernel nano-exec
func realKernelBins(t *testing.T) (kernelBin, execBin string) {
	t.Helper()
	if p := os.Getenv("NANO_KERNEL_BIN"); p != "" {
		return p, os.Getenv("NANO_EXEC_BIN")
	}
	for _, p := range []string{"../../../kernel/build/nano-kernel-asan", "../../../kernel/nano-kernel"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			kernelBin = p
			break
		}
	}
	for _, p := range []string{"../../../kernel/build/nano-exec-asan", "../../../kernel/nano-exec"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			execBin = p
			break
		}
	}
	if kernelBin == "" || execBin == "" {
		t.Skip("没有编译好的 nano-kernel / nano-exec；先 make -C kernel nano-kernel nano-exec")
	}
	abs1, _ := filepath.Abs(kernelBin)
	abs2, _ := filepath.Abs(execBin)
	return abs1, abs2
}

func setupRealKernel(t *testing.T, wrappers ...func(*App) kernel.Handler) (*env, *exec.Cmd) {
	t.Helper()
	kbin, xbin := realKernelBins(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "personas"), 0o755)
	os.WriteFile(filepath.Join(dir, "personas", "default.md"), []byte("你是测试机器人"), 0o644)
	os.MkdirAll(filepath.Join(dir, "home"), 0o755)
	sock := filepath.Join(dir, "k.sock")
	cfg := &config.Config{
		Kernel:    config.Kernel{Listen: "unix:" + sock, Token: "tok"},
		LLM:       config.LLM{Provider: "mock", MaxHistoryTokens: 10000, TrimTo: 0.5},
		Persona:   config.Persona{Dir: filepath.Join(dir, "personas")},
		Skills:    config.Skills{Dir: filepath.Join(dir, "skills")},
		Access:    config.Access{Admins: []string{"fake:u1"}, Guest: "none"},
		Workspace: config.Workspace{Mode: "ask"},
		Dir:       filepath.Join(dir, "conf"),
	}
	store, err := session.New(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &recLLM{}
	a := New(ctx, cfg, rec, store, persona.New(cfg.Persona.Dir, log), log)
	a.now = func() time.Time { return time.Date(2026, 10, 21, 15, 8, 0, 0, time.UTC) }
	a.loc = time.UTC
	a.SetHosts(newFakeHosts())
	var handler kernel.Handler = a
	if len(wrappers) > 0 {
		handler = wrappers[0](a)
	}
	hub := kernel.NewHub("tok", handler, log)
	a.SetHub(hub)
	ln, err := kernel.Listen(cfg.Kernel.Listen)
	if err != nil {
		t.Fatal(err)
	}
	go hub.Serve(ctx, ln)
	ch := &fakeCh{sent: make(chan string, 100)}
	a.AddChannel(ch)

	conf := filepath.Join(dir, "kernel.conf")
	confText := "gateway = unix:" + sock + "\ntoken = tok\nexec = " + xbin + "\n"
	if err := os.WriteFile(conf, []byte(confText), 0o600); err != nil {
		t.Fatal(err)
	}
	klog, err := os.Create(filepath.Join(dir, "kernel.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(kbin, "-c", conf)
	cmd.Env = append(os.Environ(), "HOME="+filepath.Join(dir, "home"),
		"NANO_HOME="+filepath.Join(dir, "home", ".nano-agent"))
	cmd.Stderr = klog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		klog.Close()
		cancel()
		a.Wait()
	})
	// 等真内核连上来（hello 经过真实 hub 解码）
	for i := 0; i < 5000; i++ {
		a.mu.Lock()
		n := len(a.nodes)
		a.mu.Unlock()
		if n > 0 {
			return &env{a: a, ch: ch, llm: rec, dir: dir, sid: mainSID}, cmd
		}
		if cmd.ProcessState != nil {
			t.Fatalf("内核启动失败：%s", readFileTail(filepath.Join(dir, "kernel.log")))
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("内核没有连上：%s", readFileTail(filepath.Join(dir, "kernel.log")))
	return nil, cmd
}

func readFileTail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	s := string(b)
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return s
}

// 真实 C 内核 → 真实 Go 网关：一轮 Skill 工具调用（走 exec 帧，R1 的类型冲突就在这条路上）。
func TestRealKernelRoundTripWithGatewayTool(t *testing.T) {
	e, _ := setupRealKernel(t)
	e.say(`!call Skill {"name":"demo"}`, msgID("m1"))
	// 最终回复里带着工具结果（没有这个技能 → 错误文本回到模型）
	msg := e.ch.wait(t, "技能")
	if !strings.Contains(msg, "demo") && !strings.Contains(msg, "技能") {
		t.Fatalf("回复：%q", msg)
	}
	e.a.mu.Lock()
	nodes := len(e.a.nodes)
	sid := ""
	if s := e.a.sessions[e.sid]; s != nil {
		sid = s.sid
	}
	e.a.mu.Unlock()
	if nodes != 1 {
		t.Fatalf("内核掉线了：在线 %d 个", nodes)
	}
	if sid != mainSID {
		t.Fatalf("会话：%q", sid)
	}
	// 历史里有这一轮的工具结果，说明 exec → exec_result → result 整条链闭合
	var sawTool bool
	for _, m := range e.a.store.History(e.sid) {
		if m.Role == "tool" {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatalf("历史里没有工具结果：%+v", e.a.store.History(e.sid))
	}
}
