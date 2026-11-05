package app

import (
	"os"
	"path/filepath"
	"testing"

	"nano-agent/gateway/internal/health"
)

// 工单记录打不开：保留现场，健康条目 tickets 变 error，其他照旧。
func TestCorruptTicketsFileIsKeptAndReportedInHealth(t *testing.T) {
	e := setup(t)
	path := filepath.Join(e.dir, "data", "runtime", "tickets.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a2 := newAppAt(t, e.dir, nil)
	if a2.tickets != nil || a2.ticketsErr == nil {
		t.Fatal("打不开要记下错误，不能当成空的")
	}
	var got health.State
	for _, h := range a2.health.Snapshot() {
		if h.Name == "tickets" {
			got = h.State
		}
	}
	if got != health.Error {
		t.Fatalf("健康条目 tickets：%q", got)
	}
	if b, _ := os.ReadFile(path); string(b) != "not json\n" {
		t.Fatal("文件保持原样")
	}
}
