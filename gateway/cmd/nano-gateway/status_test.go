package main

import (
	"testing"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/health"
)

func TestStatusExitCode(t *testing.T) {
	st := app.Status{Health: []health.Entry{{Name: "a", State: health.OK}, {Name: "b", State: health.Degraded}}}
	if got := statusExit(st); got != 0 {
		t.Fatalf("degraded 不影响退出码: %d", got)
	}
	st.Health = append(st.Health, health.Entry{Name: "c", State: health.Error})
	if got := statusExit(st); got != 3 {
		t.Fatalf("有 error 条目退出码为 3: %d", got)
	}
	if got := statusExit(app.Status{}); got != 0 {
		t.Fatalf("没有条目: %d", got)
	}
}
