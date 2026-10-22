package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/proto"
)

func TestDeviceAPIHidesCredentials(t *testing.T) {
	srv, s, conf := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := host.New(ctx, conf.Current().Session.DataDir, host.Executor{}, nil, kernel.NewHub("tok", s.app, log), log)
	if err != nil {
		t.Fatal(err)
	}
	s.SetDevices(m)
	c := newClient(t, srv)
	if code, _ := c.do("GET", "/api/devices", nil, false); code != 401 {
		t.Fatal("未登录看到了服务器列表", code)
	}
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	code, out := c.do("POST", "/api/devices", map[string]any{"name": "test-server", "host": "127.0.0.1", "password": "private-device-test-secret"}, true)
	if code != 200 {
		t.Fatal(code, out)
	}
	id := out["id"].(string)
	code, out = c.do("GET", "/api/devices", nil, false)
	b, _ := json.Marshal(out)
	if code != 200 || strings.Contains(string(b), "private-device-test-secret") || !strings.Contains(string(b), "test-server") {
		t.Fatal("设备列表泄露凭据", code, string(b))
	}
	if code, out = c.do("PUT", "/api/devices/listener", map[string]string{"address": "127.0.0.1:0"}, true); code != 200 || out["listener"] == "" {
		t.Fatal("不能热启用接入地址", code, out)
	}
	if conf.Current().Kernel.DeviceListen != "127.0.0.1:0" {
		t.Fatal("监听地址没有保存")
	}
	if code, _ = c.do("PUT", "/api/devices/listener", map[string]string{"address": ""}, true); code != 200 {
		t.Fatal("不能关闭监听")
	}
	if code, _ := c.do("DELETE", "/api/devices/"+id, nil, true); code != 200 {
		t.Fatal("删除连接失败")
	}
}

// 网页上允许一台电脑的配对请求，之后能看到它、移除它
func TestPairingAPI(t *testing.T) {
	srv, s, conf := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := kernel.NewHub("tok", s.app, log)
	dir := t.TempDir()
	if err := hub.EnablePairing(filepath.Join(dir, "paired.json")); err != nil {
		t.Fatal(err)
	}
	m, err := host.New(ctx, conf.Current().Session.DataDir, host.Executor{}, nil, hub, log)
	if err != nil {
		t.Fatal(err)
	}
	s.SetDevices(m)
	sock := filepath.Join(dir, "k.sock")
	ln, err := kernel.Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	go hub.Serve(ctx, ln)

	k, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	b, _ := json.Marshal(proto.Msg{T: proto.THello, V: proto.Version, Node: "laptop", Pair: true})
	k.Write(append(b, '\n'))
	sc := bufio.NewScanner(k)
	k.SetReadDeadline(time.Now().Add(3 * time.Second))
	sc.Scan() // pairing

	c := newClient(t, srv)
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	var id string
	for i := 0; i < 100 && id == ""; i++ {
		_, out := c.do("GET", "/api/devices", nil, false)
		if reqs, _ := out["pairing"].([]any); len(reqs) == 1 {
			id = reqs[0].(map[string]any)["id"].(string)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("设备列表里没有配对请求")
	}
	if code, _ := c.do("POST", "/api/pairing/"+id, map[string]bool{"ok": true}, false); code == 200 {
		t.Fatal("没有 X-Nano 头也能允许配对")
	}
	if code, out := c.do("POST", "/api/pairing/"+id, map[string]bool{"ok": true}, true); code != 200 {
		t.Fatal(code, out)
	}
	if !sc.Scan() || !strings.Contains(sc.Text(), proto.TPaired) {
		t.Fatalf("内核没有拿到口令：%s", sc.Text())
	}
	_, out := c.do("GET", "/api/devices", nil, false)
	if paired, _ := out["paired"].([]any); len(paired) != 1 || paired[0].(map[string]any)["node"] != "laptop" {
		t.Fatalf("配对的电脑：%v", out["paired"])
	}
	if code, _ := c.do("DELETE", "/api/paired/laptop", nil, true); code != 200 {
		t.Fatal("移除失败")
	}
	if len(hub.PairedDevices()) != 0 {
		t.Fatal("没有移除")
	}
}

func TestWorkspacesAPI(t *testing.T) {
	srv, _, _ := setup(t)
	c := newClient(t, srv)
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	code, out := c.do("POST", "/api/workspaces", map[string]any{"host": "本机", "path": "/srv/proj", "default": true}, true)
	if code != 200 || out["id"] == "" || out["default"] != true {
		t.Fatal(code, out)
	}
	id := out["id"].(string)
	if code, out := c.do("POST", "/api/workspaces", map[string]any{"host": "nowhere", "path": "/x"}, true); code != 400 {
		t.Fatal("没有的机器", code, out)
	}
	if code, _ := c.do("PATCH", "/api/sessions/main", map[string]any{"workspaces": []string{id}}, true); code != 200 {
		t.Fatal("会话挂工作目录", code)
	}
	_, out = c.do("GET", "/api/sessions/main", nil, false)
	ws := out["info"].(map[string]any)["workspaces"].([]any)
	if len(ws) != 1 || ws[0].(map[string]any)["host"] != "本机" || ws[0].(map[string]any)["path"] != "/srv/proj" {
		t.Fatalf("info: %v", ws)
	}
	if code, _ := c.do("PUT", "/api/workspaces/default", map[string]any{"id": id, "default": false}, true); code != 200 {
		t.Fatal("取消默认", code)
	}
	if code, _ := c.do("DELETE", "/api/workspaces/"+id, nil, true); code != 200 {
		t.Fatal("删除", code)
	}
	_, out = c.do("GET", "/api/sessions/main", nil, false)
	if ws := out["info"].(map[string]any)["workspaces"].([]any); len(ws) != 0 {
		t.Fatalf("删掉后会话只聊天：%v", ws)
	}
}
