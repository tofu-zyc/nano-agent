package host

import (
	"strings"
	"testing"
)

func TestExplicitSandboxRequiresTargetProbeCapability(t *testing.T) {
	for _, mode := range []string{"on", "off"} {
		req := []byte(`{"sandbox":"` + mode + `"}`)
		if err := checkSandboxRequest(req, nil); err == nil || !strings.Contains(err.Error(), "没有执行") {
			t.Fatalf("legacy capability accepted %s", mode)
		}
		if err := checkSandboxRequest(req, []string{"auto", "on", "off"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkSandboxRequest([]byte(`{}`), nil); err != nil {
		t.Fatal("legacy auto compatibility broken")
	}
	r := parseResult([]byte(`{"ok":false,"content":"tool failed inside isolation","sandbox_effective":"full"}`), "", 0, nil)
	if r.OK || r.Sandbox == nil || r.Sandbox.Effective != "full" {
		t.Fatal("tool outcome confused with applied isolation", r)
	}
}

func TestHookInputRequiresActualExecutorSupport(t *testing.T) {
	request := []byte(`{"hook_input":"{\"event\":\"done\"}"}`)
	if err := checkHookInput(request, false); err == nil {
		t.Fatal("old executor accepted Hook stdin")
	}
	if err := checkHookInput(request, true); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"hook_input":null}`, `{"hook_input":12}`, `{"hook_input":"` + strings.Repeat("x", 4097) + `"}`} {
		if err := checkHookInput([]byte(raw), true); err == nil {
			t.Fatal("invalid input", raw[:min(100, len(raw))])
		}
	}
}
