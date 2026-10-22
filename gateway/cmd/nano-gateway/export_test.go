package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/secret"
)

func TestExportImportMovesEverything(t *testing.T) {
	src := t.TempDir()
	// 源码目录里跑的网关：配置里是别处的绝对路径，密钥文件在 NANO_HOME
	data := filepath.Join(src, "elsewhere", "data")
	personas := filepath.Join(src, "elsewhere", "personas")
	for _, d := range []string{data, personas} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(src, "gateway.toml")
	toml := "[kernel]\nlisten = \"unix:" + filepath.Join(src, "k.sock") + "\"\ntoken = \"abc\"\n\n" +
		"[persona]\ndir = \"" + personas + "\"\n\n[session]\ndata_dir = \"" + data + "\"  # 历史\n\n[llm]\nprovider = \"mock\"\n"
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(cfg, toml)
	write(filepath.Join(personas, "default.md"), "你是助手")
	write(filepath.Join(data, "gateway.log"), "不要带走")
	write(filepath.Join(data, "web.url"), "http://x/#k=pw")
	write(filepath.Join(data, "sessions", "s1.jsonl"), "{}\n")
	write(filepath.Join(data, "workspaces.json"), `[{"id":"a","host":"本机","path":"/nowhere/proj"},{"id":"b","host":"box","path":"~/x"}]`)
	write(filepath.Join(data, "paired.json"), `[{"node":"winpc","addr":"1.2.3.4"}]`)

	t.Setenv("NANO_HOME", src)
	secret.Reset()
	defer secret.Reset()
	if err := secret.Init(src); err != nil {
		t.Fatal(err)
	}
	enc, err := secret.Encrypt("sk-real-key")
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(data, "models.json"), `{"endpoints":[{"api_key":"`+enc+`"}]}`)

	out := filepath.Join(t.TempDir(), "export.tar.gz")
	if _, err := exportTo(cfg, out); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Fatalf("导出包权限 %o", st.Mode().Perm())
	}

	dst := filepath.Join(t.TempDir(), "home")
	report, err := importFrom(out, dst, false, "wsl")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "gateway.toml"))
	for _, want := range []string{`data_dir = "data"`, `dir = "personas"`, `listen = "unix:run/gateway.sock"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("gateway.toml 里没有 %s：\n%s", want, got)
		}
	}
	// 配置里的明文 token 在导出前已经被挪进 data/config.json 加密保存
	for _, p := range []string{"data/sessions/s1.jsonl", "data/config.json", "personas/default.md", "secret.key"} {
		st, err := os.Stat(filepath.Join(dst, p))
		if err != nil {
			t.Errorf("缺 %s", p)
		} else if st.Mode().Perm() != 0o600 {
			t.Errorf("%s 权限 %o", p, st.Mode().Perm())
		}
	}
	for _, p := range []string{"data/gateway.log", "data/web.url"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err == nil {
			t.Errorf("%s 不该带走", p)
		}
	}
	var wss []map[string]string
	b, _ := os.ReadFile(filepath.Join(dst, "data", "workspaces.json"))
	if err := json.Unmarshal(b, &wss); err != nil || wss[0]["host"] != "wsl" || wss[1]["host"] != "box" {
		t.Fatalf("工作目录：%s", b)
	}
	if !strings.Contains(report, "winpc") || !strings.Contains(report, "1 个工作目录改记到 wsl") {
		t.Errorf("报告：%s", report)
	}

	// 导入的 secret.key 能解开导出前加密的密钥
	secret.Reset()
	if err := secret.Init(dst); err != nil {
		t.Fatal(err)
	}
	if plain, err := secret.Decrypt(enc); err != nil || plain != "sk-real-key" {
		t.Fatalf("解密：%q %v", plain, err)
	}

	// 已经有数据时不覆盖，--force 先备份
	if _, err := importFrom(out, dst, false, ""); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("应该拒绝覆盖：%v", err)
	}
	if _, err := importFrom(out, dst, true, ""); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(dst, "backup", "import-*", "secret.key"))
	if len(backups) != 1 {
		t.Fatalf("没有备份：%v", backups)
	}
}
