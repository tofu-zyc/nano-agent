//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStandaloneWindowsGatewayTimezoneConfig(t *testing.T) {
	exe := os.Getenv("NANO_TEST_GATEWAY_EXE")
	if exe == "" {
		t.Skip("set NANO_TEST_GATEWAY_EXE to the final Windows gateway")
	}
	root := t.TempDir()
	cfg := filepath.Join(root, "gateway.toml")
	for _, zone := range []string{"Asia/Shanghai", "America/New_York", "Invalid/Timezone"} {
		if err := os.WriteFile(cfg, []byte("[kernel]\ntoken = 'test'\n[session]\ntimezone = '"+zone+"'\n"), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "check-config", "-c", cfg)
		for _, pair := range os.Environ() {
			if !strings.HasPrefix(pair, "NANO_HOME=") && !strings.HasPrefix(pair, "GOROOT=") && !strings.HasPrefix(pair, "ZONEINFO=") {
				cmd.Env = append(cmd.Env, pair)
			}
		}
		cmd.Env = append(cmd.Env, "NANO_HOME="+root, "GOROOT="+filepath.Join(root, "no-go"), "ZONEINFO="+filepath.Join(root, "no-zoneinfo"))
		out, err := cmd.CombinedOutput()
		if zone == "Invalid/Timezone" {
			if err == nil {
				t.Fatal("invalid timezone accepted by standalone exe")
			}
		} else if err != nil {
			t.Fatalf("standalone %s: %v %s", zone, err, out)
		}
	}
}

func TestOfflineWindowsIANATimezones(t *testing.T) {
	for _, name := range []string{"Asia/Shanghai", "America/New_York"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "America/New_York" {
			_, winter := time.Date(2026, 1, 15, 12, 0, 0, 0, loc).Zone()
			_, summer := time.Date(2026, 7, 15, 12, 0, 0, 0, loc).Zone()
			if winter == summer {
				t.Fatal("missing daylight saving transitions")
			}
		}
	}
	if _, err := time.LoadLocation("Invalid/Timezone"); err == nil {
		t.Fatal("invalid timezone accepted")
	}
}
