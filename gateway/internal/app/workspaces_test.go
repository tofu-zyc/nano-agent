package app

import (
	"testing"

	"nano-agent/gateway/internal/host"
)

func TestParseWorkspaceKeepsForeignPaths(t *testing.T) {
	a := &App{}
	a.wss = []Workspace{{Host: "laptop", Path: `C:\proj`}}
	tests := []struct{ in, host, path string }{
		{`C:\Users\me\proj`, host.LocalName, `C:\Users\me\proj`},
		{`C:/Users/me/proj`, host.LocalName, `C:/Users/me/proj`},
		{`\\server\share\proj`, host.LocalName, `\\server\share\proj`},
		{`laptop:C:\proj`, "laptop", `C:\proj`},
		{`laptop:C:/proj`, "laptop", `C:/proj`},
		{`nowhere:/tmp`, "nowhere", "/tmp"},
		{`nowhere:~/x`, "nowhere", "~/x"},
		{`/tmp/x`, host.LocalName, "/tmp/x"},
		{`~/x`, host.LocalName, "~/x"},
	}
	for _, tt := range tests {
		h, p := a.parseWorkspace(tt.in, host.LocalName)
		if h != tt.host || p != tt.path {
			t.Errorf("parseWorkspace(%q) = %q, %q；要 %q, %q", tt.in, h, p, tt.host, tt.path)
		}
	}
}

func TestCheckWorkspaceLeavesRemotePathsAlone(t *testing.T) {
	a := &App{}
	a.wss = []Workspace{{Host: "laptop", Path: `C:\proj`}}
	if err := a.checkWorkspace("laptop", `C:\Users\me\proj`); err != nil {
		t.Fatalf("远程 Windows 路径不应该被网关的 POSIX 规则拒绝：%v", err)
	}
	if err := a.checkWorkspace("nowhere", "/tmp"); err == nil {
		t.Fatal("不存在的机器要报错")
	}
	if err := a.checkWorkspace(host.LocalName, "relative"); err == nil {
		t.Fatal("本机相对路径要报错")
	}
}
