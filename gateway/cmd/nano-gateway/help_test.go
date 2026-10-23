package main

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// main 里能用的子命令都要在帮助里（内部用的 supervise、check-config 除外）。
func TestHelpListsEveryCommand(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := runHelp(nil, &buf); code != 0 {
		t.Fatalf("help exit %d", code)
	}
	cases := regexp.MustCompile(`case "([a-z-]+)":\n\t\t\tos\.Exit`).FindAllStringSubmatch(string(src), -1)
	if len(cases) < 10 {
		t.Fatalf("main.go 里只找到 %d 个子命令，正则过时了？", len(cases))
	}
	for _, m := range cases {
		name := m[1]
		if name == "supervise" || name == "check-config" {
			continue
		}
		if findCommand(name) == nil || !strings.Contains(buf.String(), "nano-gateway "+name+" ") {
			t.Errorf("帮助里没有 %s", name)
		}
	}
}

func TestCommandHelp(t *testing.T) {
	var buf bytes.Buffer
	if code := runHelp([]string{"update"}, &buf); code != 0 || !strings.HasPrefix(buf.String(), "用法：nano-gateway update [--check]") || !strings.Contains(buf.String(), "--version x.y.z") {
		t.Fatalf("help update: %d %q", code, buf.String())
	}
	buf.Reset()
	if code := runHelp([]string{"nope"}, &buf); code != 2 || !strings.Contains(buf.String(), "没有这个命令") {
		t.Fatalf("help nope: %d %q", code, buf.String())
	}
	if !wantsHelp([]string{"-c", "x", "--help"}) || wantsHelp([]string{"--", "--help"}) || wantsHelp([]string{"-f"}) {
		t.Fatal("wantsHelp")
	}
}
