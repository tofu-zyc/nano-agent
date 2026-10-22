package redact

import (
	"strings"
	"testing"
)

func TestKnownAndPatterns(t *testing.T) {
	known := []string{"sk-known-1234567890"}
	text := "key=" + known[0] + "\nAKIAABCDEFGHIJKLMNOP\npassword = \"abcdefghijklmnop\"\n-----BEGIN RSA PRIVATE KEY-----\nAAABBB\n-----END RSA PRIVATE KEY-----"
	got := ToolResult(text, known)
	for _, leak := range []string{known[0], "AKIAABCDEFGHIJKLMNOP", "abcdefghijklmnop", "AAABBB"} {
		if strings.Contains(got, leak) {
			t.Fatalf("没脱敏 %q：%q", leak, got)
		}
	}
	if again := ToolResult(text, known); again != got {
		t.Fatal("结果不确定")
	}
	if !strings.Contains(got, "sk-k") || !strings.Contains(got, "mnop") {
		t.Fatalf("应该保留前后 4 位：%q", got)
	}
}

func TestPublicIP(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:8080/v1":   false,
		"http://10.1.2.3/v1":         false,
		"http://192.168.1.2/v1":      false,
		"http://172.16.0.1/v1":       false,
		"http://100.64.0.1/v1":       false,
		"http://[fc00::1]/v1":        false,
		"https://api.example.com/v1": true,
		"http://8.8.8.8/v1":          true,
	}
	for raw, want := range cases {
		if got := IsPublicBaseURL(raw); got != want {
			t.Fatalf("%s：%v，想要 %v", raw, got, want)
		}
	}
}

func TestNeededModes(t *testing.T) {
	cases := []struct {
		mode       string
		m, c, want bool
	}{
		{"off", true, true, false},
		{"public", false, false, false},
		{"public", true, false, true},
		{"public", false, true, true},
		{"always", false, false, true},
	}
	for _, x := range cases {
		if got := Needed(x.mode, x.m, x.c); got != x.want {
			t.Fatalf("%+v：%v", x, got)
		}
	}
}
