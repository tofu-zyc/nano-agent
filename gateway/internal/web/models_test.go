package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// 网页上的模型管理：列出、添加端点（不回传 key）、添加模型、设默认、会话选模型、测试连接、删除后回退。
func TestModelsAPI(t *testing.T) {
	srv, _, _ := setup(t)
	c := newClient(t, srv)
	if code, _ := c.do("POST", "/api/login", map[string]string{"password": "pw"}, true); code != 200 {
		t.Fatalf("登录：%d", code)
	}

	code, body := c.do("GET", "/api/models", nil, false)
	if code != 200 {
		t.Fatalf("列表：%d", code)
	}
	if def, _ := body["default"].(string); def != "config/mock" {
		t.Fatalf("默认模型：%q %v", def, body)
	}

	// 添加端点：接口只告诉有没有设置 key
	code, body = c.do("POST", "/api/models/endpoints", map[string]any{
		"name": "本地接口", "kind": "openai", "base_url": "http://127.0.0.1:9/v1", "api_key": "sk-secret"}, true)
	if code != 200 {
		t.Fatalf("添加端点：%d %v", code, body)
	}
	epID, _ := body["id"].(string)
	if epID == "" {
		t.Fatalf("端点的 ID：%v", body)
	}
	code, full := c.do("GET", "/api/models", nil, false)
	if code != 200 {
		t.Fatalf("列表：%d", code)
	}
	if b, _ := json.Marshal(full); strings.Contains(string(b), "sk-secret") {
		t.Fatalf("接口泄露了 API key：%s", b)
	}
	eps, _ := full["endpoints"].([]any)
	found := false
	for _, it := range eps {
		e, _ := it.(map[string]any)
		if e["id"] == epID {
			found = true
			if e["has_key"] != true || e["config"] != false {
				t.Fatalf("端点：%v", e)
			}
		}
	}
	if !found {
		t.Fatalf("列表里没有新端点：%v", eps)
	}

	// 添加模型、设成默认
	code, body = c.do("POST", "/api/models/items", map[string]any{
		"endpoint": epID, "name": "m1", "label": "模型一", "context_window": 200000,
		"modalities": []string{"text", "image"}, "thinking_levels": []string{"low", "high"}}, true)
	if code != 200 {
		t.Fatalf("添加模型：%d %v", code, body)
	}
	mID, _ := body["id"].(string)
	if mID != epID+"/m1" {
		t.Fatalf("模型 ID：%q", mID)
	}
	if code, _ := c.do("POST", "/api/models/default", map[string]string{"id": "config/mock"}, true); code != 200 {
		t.Fatalf("设默认：%d", code)
	}

	// 会话可以单独选模型
	code, sess := c.do("PATCH", "/api/sessions/main", map[string]any{"model": mID}, true)
	if code != 200 {
		t.Fatalf("会话选模型：%d %v", code, sess)
	}
	if sess["model"] != mID || sess["model_used"] != mID || sess["model_label"] != "模型一" {
		t.Fatalf("会话信息：%v", sess)
	}
	levels, _ := sess["thinking_levels"].([]any)
	if len(levels) != 2 {
		t.Fatalf("思考强度可选值：%v", sess["thinking_levels"])
	}
	// 模型能接收图片也要存下来、读出来
	_, list := c.do("GET", "/api/models", nil, false)
	mods, _ := list["models"].([]any)
	foundMods := false
	for _, it := range mods {
		m, _ := it.(map[string]any)
		if m["id"] == mID {
			mm, _ := m["modalities"].([]any)
			foundMods = len(mm) == 2
		}
	}
	if !foundMods {
		t.Fatalf("模型的 modalities 没存下来：%v", list["models"])
	}

	// 测试连接用的就是这个模型（测试里工厂返回 mock，不会真的发请求）
	code, tr := c.do("POST", "/api/test/model", map[string]string{"id": mID}, true)
	if code != 200 || tr["ok"] != true || tr["label"] != "模型一" {
		t.Fatalf("测试连接：%d %v", code, tr)
	}

	// 删掉模型：会话回到默认，列表里也没有了
	if code, _ := c.do("DELETE", "/api/models/items?id="+url.QueryEscape(mID), nil, true); code != 200 {
		t.Fatalf("删模型：%d", code)
	}
	code, one := c.do("GET", "/api/sessions/main", nil, false)
	info, _ := one["info"].(map[string]any)
	if code != 200 || info["model"] != "" || info["model_used"] != "config/mock" {
		t.Fatalf("删模型以后的会话：%d %v", code, one)
	}
	if b, _ := json.Marshal(one); strings.Contains(string(b), mID) {
		t.Fatalf("会话还指着删掉的模型：%s", b)
	}

	// 删端点
	if code, _ := c.do("DELETE", "/api/models/endpoints/"+epID, nil, true); code != 200 {
		t.Fatalf("删端点：%d", code)
	}
}

func TestModelValidationAPI(t *testing.T) {
	srv, _, _ := setup(t)
	c := newClient(t, srv)
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	for _, bad := range []map[string]any{
		{"name": "坏地址", "base_url": "ftp://x"},
		{"name": "坏类型", "kind": "nope", "base_url": "https://x"},
	} {
		if code, _ := c.do("POST", "/api/models/endpoints", bad, true); code != http.StatusBadRequest {
			t.Errorf("%v 应该被拒绝：%d", bad, code)
		}
	}
	if code, _ := c.do("POST", "/api/models/items", map[string]any{"endpoint": "missing", "name": "x"}, true); code != http.StatusBadRequest {
		t.Errorf("不存在的端点应该被拒绝：%d", code)
	}
	// 不存在的模型不能设成默认
	if code, _ := c.do("POST", "/api/models/default", map[string]string{"id": "nope"}, true); code != http.StatusBadRequest {
		t.Errorf("不存在的模型设默认：%d", code)
	}
}
