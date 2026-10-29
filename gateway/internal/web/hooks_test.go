package web

import (
	"testing"
)

func TestHookHTTPManagementCASAndAuthentication(t *testing.T) {
	srv, _, _ := setup(t)
	c := newClient(t, srv)
	if code, _ := c.do("GET", "/api/hooks", nil, false); code != 401 {
		t.Fatal(code)
	}
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	body := map[string]any{"action": "create", "operation_id": "http-one", "sid": "main", "once": true, "spec": map[string]any{"name": "完成通知", "on": []string{"job.completed"}, "handler": map[string]string{"type": "notify", "text": "完成 {{job_id}}"}}}
	if code, _ := c.do("POST", "/api/hooks", body, false); code != 403 {
		t.Fatal("CSRF", code)
	}
	code, out := c.do("POST", "/api/hooks", body, true)
	if code != 200 {
		t.Fatal(code, out)
	}
	rule := out["rule"].(map[string]any)
	id := rule["id"].(string)
	code, duplicate := c.do("POST", "/api/hooks", body, true)
	if code != 200 || duplicate["rule"].(map[string]any)["id"] != id {
		t.Fatal(code, duplicate)
	}
	code, list := c.do("GET", "/api/hooks", nil, false)
	if code != 200 || len(list["rules"].([]any)) != 1 {
		t.Fatal(code, list)
	}
	spec := rule["spec"].(map[string]any)
	endpoints := spec["scope"].(map[string]any)["endpoints"].([]any)
	if len(endpoints) != 1 || endpoints[0].(map[string]any)["channel"] != "web" {
		t.Fatal(endpoints)
	}
	code, out = c.do("POST", "/api/hooks", map[string]any{"action": "disable", "id": id, "revision": 1}, true)
	if code != 200 || out["rule"].(map[string]any)["enabled"] != false {
		t.Fatal(code, out)
	}
	if code, _ := c.do("POST", "/api/hooks", map[string]any{"action": "enable", "id": id, "revision": 1}, true); code != 400 {
		t.Fatal("stale update", code)
	}
	if code, _ := c.do("GET", "/api/hooks?action=delete&id="+id, nil, false); code != 400 {
		t.Fatal("mutation on GET", code)
	}
	code, out = c.do("POST", "/api/hooks", map[string]any{"action": "delete", "id": id, "revision": 2}, true)
	if code != 200 {
		t.Fatal(code, out)
	}
	body["id"] = "different"
	if code, _ := c.do("POST", "/api/hooks", body, true); code != 400 {
		t.Fatal("revoked operation revival", code)
	}
}
