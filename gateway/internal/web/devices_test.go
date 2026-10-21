package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"nano-agent/gateway/internal/device"
	"nano-agent/gateway/internal/kernel"
)

func TestDeviceAPIHidesCredentialsAndDownloadsAuthenticatedPackage(t *testing.T) {
	srv, s, conf := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := device.New(ctx, conf.Current().Session.DataDir, "kernel-package-token", device.Bundle{}, kernel.NewHub("kernel-package-token", s.app, log), log)
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
			found = strings.Contains(string(b), "kernel-package-token") && strings.Contains(string(b), "127.0.0.1:7700")
		}
	}
	if !found {
		t.Fatal("安装包没有预填网关信息")
	}
	if code, _ := c.do("GET", "/api/devices/package?platform=linux&gateway=host%0Anode=x:7700", nil, false); code != 400 {
		t.Fatal("接受了带换行的地址")
	}
	if code, _ := c.do("DELETE", "/api/devices/"+id, nil, true); code != 200 {
		t.Fatal("删除连接失败")
	}
}
