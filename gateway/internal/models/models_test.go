package models

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/secret"
)

func testConfig() *config.Config {
	return &config.Config{
		LLM: config.LLM{
			Provider: "openai", BaseURL: "https://example.com/v1", APIKey: "cfg-key", Model: "cfg-model",
			ContextWindow: 100000, MaxTokens: 4096, MaxHistoryTokens: 50000, Thinking: "high",
			ThinkingLevels: []string{"none", "low", "high"},
		},
	}
}

// 旧配置里的 [llm] 模型只导入一次，变成普通接口和普通模型。
func TestLegacyImport(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	eps := st.Endpoints()
	if len(eps) != 1 || eps[0].ID != ConfigEndpoint || eps[0].Name != "example.com" {
		t.Fatalf("端点：%+v", eps)
	}
	ms := st.Models()
	if len(ms) != 1 || ms[0].ID != "config/cfg-model" || ms[0].Name != "cfg-model" || ms[0].ContextWindow != 100000 {
		t.Fatalf("模型：%+v", ms)
	}
	if st.Default() != "config/cfg-model" {
		t.Fatalf("默认模型：%q", st.Default())
	}
	cfg := st.LLMConfig(ms[0], eps[0])
	if cfg.Model != "cfg-model" || cfg.BaseURL != "https://example.com/v1" || cfg.APIKey != "cfg-key" {
		t.Fatalf("配置：%+v", cfg)
	}
	if cfg.ThinkingLevels == nil || cfg.Thinking != "high" {
		t.Fatalf("思考强度没沿用配置：%+v", cfg)
	}

	// 删掉导入的模型和接口，重启不会复活
	if err := st.DeleteModel("config/cfg-model"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteEndpoint(ConfigEndpoint); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.Models()) != 0 || len(st2.Endpoints()) != 0 || st2.Default() != "" {
		t.Fatalf("删掉后重启又出现了：%+v %+v %q", st2.Endpoints(), st2.Models(), st2.Default())
	}
}

// 没有旧模型时就是空的，不自动造一个。
func TestEmptyStore(t *testing.T) {
	cfg := &config.Config{LLM: config.LLM{Provider: "openai"}}
	st, err := Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Models()) != 0 || len(st.Endpoints()) != 0 || st.Default() != "" {
		t.Fatalf("空模型列表：%+v %+v %q", st.Endpoints(), st.Models(), st.Default())
	}
}

// 端点、模型的增删改和默认模型，都要存到磁盘。
func TestSaveEndpointModelAndDefault(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	e, err := st.SaveEndpoint(Endpoint{Name: "本地", BaseURL: "http://127.0.0.1:8000/v1", APIKey: "k1", Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID == "" || e.Kind != "openai" {
		t.Fatalf("新端点：%+v", e)
	}
	m, err := st.SaveModel(Model{Endpoint: e.ID, Name: "qwen", Label: "Qwen", ContextWindow: 200000,
		MaxTokens: 8192, MaxHistory: 100000, Thinking: "low", ThinkingLevels: []string{"low", "high"}})
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != e.ID+"/qwen" {
		t.Fatalf("模型 ID：%q", m.ID)
	}
	if err := st.SetDefault(m.ID); err != nil {
		t.Fatal(err)
	}
	if st.Default() != m.ID {
		t.Fatalf("默认模型：%q", st.Default())
	}
	// 同一个端点、同一个名字是修改，不是新增
	if _, err := st.SaveModel(Model{Endpoint: e.ID, Name: "qwen", Label: "Qwen 2"}); err != nil {
		t.Fatal(err)
	}
	found, ep, ok := st.Find(m.ID)
	if !ok || found.Label != "Qwen 2" || ep.ID != e.ID {
		t.Fatalf("改过的模型：%+v %+v %v", found, ep, ok)
	}

	// 重新打开还在
	st2, err := Open(dir, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if st2.Default() != m.ID || len(st2.Models()) != 2 || len(st2.Endpoints()) != 2 {
		t.Fatalf("重新打开：default=%q models=%d endpoints=%d", st2.Default(), len(st2.Models()), len(st2.Endpoints()))
	}
	// 改端点时 API key 留空表示不改原来的
	ne, err := st2.SaveEndpoint(Endpoint{ID: e.ID, Name: "本地 2", BaseURL: "http://127.0.0.1:8000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if ne.APIKey != "k1" || ne.Name != "本地 2" {
		t.Fatalf("改端点：%+v", ne)
	}
	// 删端点时它上面的模型一起删掉，默认模型回退到第一个
	if err := st2.DeleteEndpoint(e.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := st2.Find(m.ID); ok {
		t.Fatal("删端点以后模型还在")
	}
	if st2.Default() != "config/cfg-model" {
		t.Fatalf("默认模型没回退：%q", st2.Default())
	}
}

func TestValidation(t *testing.T) {
	st, err := Open(t.TempDir(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		run  func() error
	}{
		{"地址不合法", func() error {
			_, err := st.SaveEndpoint(Endpoint{Name: "x", BaseURL: "ftp://x"})
			return err
		}},
		{"接口类型不合法", func() error {
			_, err := st.SaveEndpoint(Endpoint{Name: "x", Kind: "nope", BaseURL: "https://x"})
			return err
		}},
		{"端点不存在", func() error {
			_, err := st.SaveModel(Model{Endpoint: "missing", Name: "m"})
			return err
		}},
		{"模型名不能为空", func() error {
			e, _ := st.SaveEndpoint(Endpoint{Name: "x", Kind: "mock"})
			_, err := st.SaveModel(Model{Endpoint: e.ID})
			return err
		}},
	}
	for _, c := range cases {
		if err := c.run(); err == nil {
			t.Errorf("%s：应该报错", c.name)
		}
	}
}

// 从端点的 /models 拉模型列表，认得出几种常见的上下文字段。
func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("请求了 %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer k1" {
			t.Errorf("没有带 API key：%q", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "m2", "max_model_len": 131072, "owned_by": "me"},
			{"id": "m1", "top_provider": map[string]any{"context_length": 32768}},
		}})
	}))
	defer srv.Close()
	st, err := Open(t.TempDir(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	e, err := st.SaveEndpoint(Endpoint{Name: "本地", BaseURL: srv.URL + "/v1", APIKey: "k1", Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := st.Fetch(context.Background(), e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "m1" || list[1].ID != "m2" {
		t.Fatalf("列表：%+v", list)
	}
	if list[0].ContextWindow != 32768 || list[1].ContextWindow != 131072 {
		t.Fatalf("上下文长度：%+v", list)
	}
	if list[0].Added || list[1].Added {
		t.Fatal("还没加过的模型不该标成已添加")
	}
	if _, err := st.SaveModel(Model{Endpoint: e.ID, Name: "m1"}); err != nil {
		t.Fatal(err)
	}
	list, err = st.Fetch(context.Background(), e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !list[0].Added || list[1].Added {
		t.Fatalf("已添加的标记：%+v", list)
	}
}

func TestAPIKeyEncryptedAtRest(t *testing.T) {
	home := t.TempDir()
	if err := secret.Init(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(secret.Reset)
	dir := t.TempDir()
	st, err := Open(dir, &config.Config{LLM: config.LLM{Provider: "openai"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveEndpoint(Endpoint{Name: "x", Kind: "openai", BaseURL: "https://x/v1", APIKey: "sk-super-secret"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "models.json"))
	if strings.Contains(string(b), "sk-super-secret") || !strings.Contains(string(b), "enc:v1:") {
		t.Fatalf("models.json 没加密：%s", b)
	}
	st2, err := Open(dir, &config.Config{LLM: config.LLM{Provider: "openai"}})
	if err != nil {
		t.Fatal(err)
	}
	if eps := st2.Endpoints(); len(eps) != 1 || eps[0].APIKey != "sk-super-secret" {
		t.Fatalf("重新读出来的 key 不对：%+v", eps)
	}
}
