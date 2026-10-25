package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/secret"
)

// 参数表里的每个参数，gatewayArgs 里都要有同名的 json 字段。
// 以前 base_url、api_key、identity_file、context_window 这些带下划线的参数没写标签，Go 对不上，
// AI 在聊天里配模型接口时地址和 key 都悄悄丢了。
func TestGatewayAdminSchemaFields(t *testing.T) {
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(gatewayAdminSchema), &schema); err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.Anonymous {
				walk(f.Type)
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			have[name] = true
		}
	}
	walk(reflect.TypeOf(gatewayArgs{}))
	for p := range schema.Properties {
		if !have[p] {
			t.Errorf("参数表里有 %q，gatewayArgs 里没有对应的 json 字段（AI 传了也收不到）", p)
		}
	}
}

// AI 配模型接口：地址和 key 要真的存进去。
func TestGatewayAdminModelEndpointKeepsURLAndKey(t *testing.T) {
	e := setup(t)
	ms, err := models.Open(e.dir, e.a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetModels(ms, func(config.LLM) (llm.Client, error) { return &recLLM{}, nil })
	ok, text := adminCall(e, `{"action":"model_endpoint_save","name":"deepseek","kind":"openai","base_url":"https://api.deepseek.com/v1","api_key":"sk-endpoint-secret-123456"}`)
	if !ok {
		t.Fatal(text)
	}
	var got *models.Endpoint
	for _, ep := range ms.Endpoints() {
		if ep.Name == "deepseek" {
			got = &ep
		}
	}
	if got == nil || got.BaseURL != "https://api.deepseek.com/v1" || got.APIKey != "sk-endpoint-secret-123456" || got.Kind != "openai" {
		t.Fatalf("地址或 key 没存上：%+v", got)
	}
}

// AI 在聊天里配 MCP：加、查、测、改（没写的字段不动、密钥沿用）、改名、删；密钥不回显。
func TestGatewayAdminMCP(t *testing.T) {
	if testing.Short() {
		t.Skip("要起子进程")
	}
	e := setup(t)
	home := t.TempDir()
	if err := secret.Init(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(secret.Reset)
	path := filepath.Join(t.TempDir(), "mcp.json")
	st, err := mcp.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mgr := mcp.NewManager(st, nil)
	t.Cleanup(mgr.Close)
	e.a.SetMCP(st, mgr)
	exe, _ := os.Executable()
	key := strings.Repeat("x", 16) // 假的密钥（低熵，免得被密钥扫描当真）
	save, _ := json.Marshal(map[string]any{"action": "mcp_save", "name": "helper", "command": exe,
		"args": []string{"-test.run=TestNoSuchTest"}, "env": map[string]string{mcpHelperEnv: "1", "API_KEY": key}, "approval": "per_turn"})
	ok, text := adminCall(e, string(save))
	if !ok || !strings.Contains(text, "加好了") {
		t.Fatalf("加服务：%s", text)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "API_KEY") || strings.Contains(string(raw), key) {
		t.Fatal("密钥明文存盘了")
	}
	if ok, text = adminCall(e, `{"action":"mcp_test","name":"helper"}`); !ok || !strings.Contains(text, "helper__echo") {
		t.Fatalf("测试：%s", text)
	}
	ok, text = queryCall(e, `{"what":"mcp"}`)
	if !ok || !strings.Contains(text, `"helper__echo"`) || !strings.Contains(text, `"API_KEY"`) || !strings.Contains(text, `"machines"`) {
		t.Fatalf("查 MCP：%s", text)
	}
	if strings.Contains(text, key) {
		t.Fatal("查 MCP 时把密钥带出来了")
	}
	// 只改审批：命令、参数、密钥都不动
	if ok, text = adminCall(e, `{"action":"mcp_save","name":"helper","approval":"always"}`); !ok || !strings.Contains(text, "改好了") {
		t.Fatalf("改审批：%s", text)
	}
	s, _ := st.Get("helper")
	env, _ := s.EnvFor()
	if s.Approval != mcp.ApprovalAlways || s.Command != exe || len(s.Args) != 1 || env["API_KEY"] != key {
		t.Fatalf("没写的字段被改掉了：%+v env=%v", s, env)
	}
	// 改名：密钥跟着走
	if ok, text = adminCall(e, `{"action":"mcp_save","id":"helper","name":"helper2"}`); !ok {
		t.Fatalf("改名：%s", text)
	}
	if _, old := st.Get("helper"); old {
		t.Fatal("改名以后旧的还在")
	}
	s, _ = st.Get("helper2")
	if env, _ = s.EnvFor(); env["API_KEY"] != key {
		t.Fatalf("改名以后密钥丢了：%v", env)
	}
	// 审批卡上密钥打星
	if _, body := callCard("GatewayAdmin", save); strings.Contains(body, key) || !strings.Contains(body, "API_KEY") {
		t.Fatalf("审批卡：%s", body)
	}
	if ok, text = adminCall(e, `{"action":"mcp_delete","name":"helper2"}`); !ok || len(st.List()) != 0 {
		t.Fatalf("删除：%s %v", text, st.List())
	}
}
