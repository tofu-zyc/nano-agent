package app

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 网关发起的执行带着 env 交给装了内核的机器代跑：env 的值不以明文落到任何文件里。
func TestZZGatewayEnvIsNotWrittenToDisk(t *testing.T) {
	e, _ := setupRealKernel(t) // 正式的 App 协商 Job 能力：内核把这次执行收成 Job
	e.a.mu.Lock()
	nodeName := ""
	for name := range e.a.nodes {
		nodeName = name
	}
	e.a.mu.Unlock()
	work := filepath.Join(e.dir, "env-work")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	const secret = "ghs_PROBEtokenVALUE0123456789"
	req, _ := json.Marshal(map[string]any{
		"tool": "Shell", "args": map[string]any{"command": "test -n \"$PROBE_TOKEN\" && echo has-env"},
		"origin": map[string]any{"agent": "default", "sid": "env-probe", "run_id": "env-probe-1", "turn_id": 1, "call_id": "shell", "created_by": "fake:u1"},
		"dirs":   []string{work}, "write": true, "sandbox": "off",
		"env": map[string]string{"PROBE_TOKEN": secret},
	})
	ctx, cancel := context.WithTimeout(e.a.ctx, 20*time.Second)
	defer cancel()
	result := (kernelHost{a: e.a, node: nodeName}).Exec(ctx, req)
	if !result.OK || !strings.Contains(result.Content, "has-env") {
		t.Fatalf("执行：%+v", result)
	}
	time.Sleep(500 * time.Millisecond)
	var hits []string
	filepath.WalkDir(e.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), secret) {
			hits = append(hits, strings.TrimPrefix(p, e.dir))
		}
		return nil
	})
	if len(hits) > 0 {
		t.Fatalf("env 的值以明文写进了：%v", hits)
	}
}
