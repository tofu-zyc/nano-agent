package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"nano-agent/gateway/internal/app"
)

func TestReadAlivePID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NANO_HOME", dir)
	path := pidFile("x.pid")
	if err := writePID(path, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if pid, ok := readAlivePID(path); !ok || pid != os.Getpid() {
		t.Fatalf("读不到自己的 pid：%d %v", pid, ok)
	}
	if err := os.WriteFile(path, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, ok := readAlivePID(path); ok || pid != 0 {
		t.Fatalf("坏 pid 文件应该读不到：%d %v", pid, ok)
	}
	if err := os.WriteFile(path, []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readAlivePID(path); ok {
		t.Fatal("不存在的进程应该读不到")
	}
}

func TestIsGatewayProcess(t *testing.T) {
	if !isGatewayProcess(os.Getpid()) {
		t.Fatalf("测试进程自己的 exe 应该认成 nano-gateway：%s", strconv.Itoa(os.Getpid()))
	}
}

func TestUseSystemdWithEmptyConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if useSystemd() {
		t.Fatal("临时 XDG_CONFIG_HOME 里没有 unit，不应该走 systemd")
	}
}

func TestRequestRestartRejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NANO_HOME", dir)
	cfgPath := filepath.Join(dir, "gateway.toml")
	if err := os.WriteFile(cfgPath, []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	ch := make(chan app.RestartRequest, 1)
	if err := requestRestart(cfgPath, data, app.RestartRequest{By: "test"}, ch); err == nil {
		t.Fatal("坏配置应该拒绝重启")
	}
	select {
	case <-ch:
		t.Fatal("坏配置不应该发出重启请求")
	default:
	}
	if _, err := os.Stat(markerPath(data)); !os.IsNotExist(err) {
		t.Fatal("坏配置不应该留下重启标记")
	}
}
