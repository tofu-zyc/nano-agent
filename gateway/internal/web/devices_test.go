package web

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/device"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/proto"
)

func TestDeviceAPIHidesCredentialsAndDownloadsAuthenticatedPackage(t *testing.T) {
	srv, s, conf := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := device.New(ctx, conf.Current().Session.DataDir, device.Bundle{}, kernel.NewHub("kernel-package-token", s.app, log), log)
	if err != nil {
		t.Fatal(err)
	}
	s.SetDevices(m)
	c := newClient(t, srv)
	if code, _ := c.do("GET", "/api/devices/package?platform=windows&gateway=127.0.0.1:7700", nil, false); code != 401 {
		t.Fatal("未登录下载了连接包", code)
	}
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	code, out := c.do("POST", "/api/devices", map[string]any{"name": "test-server", "host": "127.0.0.1", "password": "private-device-test-secret"}, true)
	if code != 200 {
		t.Fatal(code, out)
	}
	id := out["id"].(string)
	code, out = c.do("GET", "/api/devices", nil, false)
	b, _ := json.Marshal(out)
	if code != 200 || strings.Contains(string(b), "private-device-test-secret") || strings.Contains(string(b), "kernel-package-token") {
		t.Fatal("设备列表泄露凭据", code)
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
	resp, err := c.hc.Get(srv.URL + "/api/devices/package?platform=windows&gateway=127.0.0.1:7700")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("下载失败 %d", resp.StatusCode)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range zr.File {
		if f.Name == "kernel.conf" {
			r, _ := f.Open()
			b, _ := io.ReadAll(r)
			r.Close()
			found = !strings.Contains(string(b), "kernel-package-token") && strings.Contains(string(b), "127.0.0.1:7700")
		}
	}
	if !found {
		t.Fatal("安装包没有预填网关地址，或者带了口令")
	}
	if code, _ := c.do("GET", "/api/devices/package?platform=linux&gateway=host%0Anode=x:7700", nil, false); code != 400 {
		t.Fatal("接受了带换行的地址")
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
	m, err := device.New(ctx, conf.Current().Session.DataDir, device.Bundle{}, hub, log)
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
