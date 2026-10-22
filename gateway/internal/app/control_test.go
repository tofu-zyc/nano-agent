package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayLogsRedactsSecrets(t *testing.T) {
	e := setup(t)
	data := filepath.Join(e.dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	e.a.cfg.Session.DataDir = data
	e.a.cfg.Kernel.Token = "kernel-secret-token-123"
	logPath := filepath.Join(data, "gateway.log")
	if err := os.WriteFile(logPath, []byte("INFO 启动\nERROR token=kernel-secret-token-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ok bool
	var text string
	e.a.gatewayQuery(mainSID, `{"what":"logs","lines":10}`, func(o bool, s string) { ok, text = o, s })
	if !ok {
		t.Fatalf("logs 查询失败：%s", text)
	}
	if !strings.Contains(text, "ERROR") || !strings.Contains(text, "启动") {
		t.Fatalf("logs 没返回日志：%s", text)
	}
	if strings.Contains(text, "kernel-secret-token-123") {
		t.Fatalf("logs 泄露了密钥：%s", text)
	}
}

func TestGatewayAdminRestartRequestsRestart(t *testing.T) {
	e := setup(t)
	var got RestartRequest
	e.a.SetRestarter(func(r RestartRequest) error {
		got = r
		return nil
	})
	text, err := e.a.gatewayAdmin(mainSID, gatewayArgs{Action: "restart"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SID != mainSID || got.By != "gateway-admin" {
		t.Fatalf("重启请求不对：%+v", got)
	}
	if !strings.Contains(text, "重启") {
		t.Fatalf("结果不对：%s", text)
	}
}

func TestGatewayStatusIncludesRuntime(t *testing.T) {
	e := setup(t)
	e.a.SetRuntime("test-version", "background", "http://127.0.0.1:1/")
	var ok bool
	var text string
	e.a.gatewayQuery(mainSID, `{"what":"status"}`, func(o bool, s string) { ok, text = o, s })
	if !ok {
		t.Fatalf("status 查询失败：%s", text)
	}
	for _, want := range []string{`"version": "test-version"`, `"mode": "background"`, `"machines"`, `"channels"`, `"pending_restart"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("status 缺 %s：%s", want, text)
		}
	}
}
