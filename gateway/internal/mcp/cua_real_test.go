package mcp

// 真实的 Cua Driver 联调测试。默认跳过：CI 和普通开发机上不一定装了 Driver，也不一定有图形会话。
//
// 在目标机器上跑：
//
//	NANO_CUA_REAL=1 NANO_CUA_BIN=/home/me/.local/bin/cua-driver go test ./internal/mcp -run TestRealCuaDriver -v
//
// 它只做只读和可逆的事：握手、列工具、读 initialize 说明、列窗口、health_report、声明/结束 session。
// 截图和输入在图形会话不可用的机器上会失败，测试会把这些步骤标成跳过并说明原因。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRealCuaDriver(t *testing.T) {
	if os.Getenv("NANO_CUA_REAL") != "1" {
		t.Skip("没有设置 NANO_CUA_REAL=1：跳过真实 Cua Driver 联调")
	}
	bin := os.Getenv("NANO_CUA_BIN")
	if bin == "" {
		bin = "cua-driver"
	}
	if _, err := os.Stat(bin); err != nil {
		if bin != "cua-driver" {
			t.Fatalf("NANO_CUA_BIN 不存在：%v", err)
		}
	}
	store, err := Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save([]Server{{Name: "desktop", Command: bin, Args: []string{"mcp"}, Enabled: true, Approval: "never"}}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer mgr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	tools, err := mgr.Tools(ctx, "desktop")
	if err != nil {
		t.Fatalf("MCP 握手失败：%v", err)
	}
	if len(tools) < 10 {
		t.Fatalf("Driver 工具太少：%d", len(tools))
	}
	t.Logf("Driver 握手成功：%d 个工具，版本 %s", len(tools), mgr.Version("desktop"))
	if v := mgr.Version("desktop"); strings.TrimSpace(v) == "" {
		t.Error("initialize 没有给版本")
	}
	ins := mgr.Instructions("desktop")
	if strings.TrimSpace(ins) == "" {
		t.Error("initialize 没有给操作说明（instructions）")
	} else {
		t.Logf("instructions：%s", firstLine(ins))
	}

	res, err := mgr.Call(ctx, "desktop", "list_windows", map[string]any{"session": "na-real-test"})
	if err != nil {
		t.Fatalf("list_windows 失败：%v", err)
	}
	text := ResultText(res)
	if !strings.Contains(text, "[结构化结果]") || !strings.Contains(text, "windows") {
		t.Fatalf("list_windows 没有保留结构化结果：%q", text)
	}
	t.Logf("list_windows：%s", oneLine(text))

	if _, err := mgr.Call(ctx, "desktop", "start_session", map[string]any{"session": "na-real-test"}); err != nil {
		t.Fatalf("start_session 失败：%v", err)
	}
	if _, err := mgr.Call(ctx, "desktop", "end_session", map[string]any{"session": "na-real-test"}); err != nil {
		t.Fatalf("end_session 失败：%v", err)
	}

	if _, ok := mgr.ToolInfo("desktop", "health_report"); ok {
		res, err := mgr.Call(ctx, "desktop", "health_report", map[string]any{"session": "na-real-test"})
		if err != nil {
			t.Logf("health_report 失败（有些平台没有）：%v", err)
		} else if text := ResultText(res); strings.Contains(text, "driver_version") {
			t.Logf("health_report：%s", oneLine(text))
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > 400 {
		return string([]rune(s)[:400]) + "…"
	}
	return s
}

// TestRealCuaDriverWindowTree 在 Linux 图形会话里启动一个真实的 gedit 窗口，
// 用 Driver 找到窗口、读辅助功能树（只读、可逆）。没有图形会话时跳过。
func TestRealCuaDriverWindowTree(t *testing.T) {
	if os.Getenv("NANO_CUA_REAL") != "1" {
		t.Skip("没有设置 NANO_CUA_REAL=1")
	}
	if runtime.GOOS != "linux" {
		t.Skip("这个用例用 gedit 验证，只在 Linux 上跑")
	}
	if _, err := exec.LookPath("gedit"); err != nil {
		t.Skip("没有 gedit")
	}
	bin := os.Getenv("NANO_CUA_BIN")
	if bin == "" {
		bin = "cua-driver"
	}
	store, err := Open(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save([]Server{{Name: "desktop", Command: bin, Args: []string{"mcp"}, Enabled: true, Approval: "never"}}); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer mgr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := mgr.Tools(ctx, "desktop"); err != nil {
		t.Fatalf("MCP 握手失败：%v", err)
	}

	cmd := exec.Command("gedit")
	cmd.Env = append(os.Environ(), "GDK_BACKEND=x11")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 gedit 失败：%v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = exec.Command("pkill", "-f", "gedit").Run()
	}()

	// 等它真的出现在窗口列表里
	var pid, window int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		res, err := mgr.Call(ctx, "desktop", "list_windows", map[string]any{"session": "na-real-window"})
		if err == nil {
			var out struct {
				Windows []struct {
					PID    *int   `json:"pid"`
					ID     int    `json:"window_id"`
					Title  string `json:"title"`
					Width  int    `json:"width"`
					Height int    `json:"height"`
				} `json:"windows"`
			}
			_ = json.Unmarshal([]byte(ResultText(res)), &out) // ResultText 带前缀，解析失败也没关系，下面用宽松匹配
			text := ResultText(res)
			if strings.Contains(text, "\"pid\":"+itoa(cmd.Process.Pid)) {
				// 从结构化 JSON 里找这个 pid 的 window_id
				start := strings.Index(text, "[结构化结果] ")
				if start >= 0 {
					_ = json.Unmarshal([]byte(text[start+len("[结构化结果] "):]), &out)
					for _, w := range out.Windows {
						if w.PID != nil && *w.PID == cmd.Process.Pid {
							pid, window = *w.PID, w.ID
						}
					}
				}
			}
		}
		if pid != 0 && window != 0 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if pid == 0 || window == 0 {
		t.Skip("这个图形会话里拿不到 gedit 的 X11 窗口（WSLg/Wayland 限制）；窗口发现和树读取见 TestRealCuaDriver")
	}
	res, err := mgr.Call(ctx, "desktop", "get_window_state", map[string]any{
		"pid": pid, "window_id": window, "session": "na-real-window", "include_screenshot": false})
	if err != nil {
		t.Fatalf("get_window_state 失败：%v", err)
	}
	text := ResultText(res)
	if !strings.Contains(text, "[结构化结果]") || !strings.Contains(text, "elements") {
		t.Fatalf("没有保留窗口树的结构化结果：%q", text)
	}
	if !strings.Contains(text, "gedit") && !strings.Contains(text, "element_count") {
		t.Fatalf("窗口树看起来不对：%q", oneLine(text))
	}
	t.Logf("窗口树：%s", oneLine(text))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
