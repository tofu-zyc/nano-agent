package web

import (
	"strings"
	"testing"

	"nano-agent/gateway/internal/app"
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

// 分支的网页入口：列出来源会话的分支、继续/暂停/归档，以及"把结论带回来"的拒绝路径。
// 报告的正文与去重语义在 App 层已有真实回归（TestBranchReportComesBackAsASourcedMessageOnly）。
func TestBranchHTTPListReportAndActions(t *testing.T) {
	srv, server, _ := setup(t)
	c := newClient(t, srv)
	if code, _ := c.do("GET", "/api/sessions/main/branches", nil, false); code != 401 {
		t.Fatal("unauthenticated list", code)
	}
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	fork, err := server.app.SessionFork(app.SessionForkRequest{SourceSID: "main", Title: "日志格式", RequestID: "web-branch", By: "web:admin"})
	if err != nil || fork.BranchSID == "" {
		t.Fatal(fork, err)
	}
	code, out := c.do("GET", "/api/sessions/main/branches", nil, false)
	if code != 200 {
		t.Fatal(code, out)
	}
	list := out["branches"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["sid"] != fork.BranchSID || list[0].(map[string]any)["delivery_policy"] != "silent" {
		t.Fatal(list)
	}
	// 会话信息里带着分支元信息，网页才能按父子折叠
	info, ok := server.app.Session(fork.BranchSID)
	if !ok || info.Branch == nil || info.Branch.ParentSID != "main" || info.Branch.Status == "" {
		t.Fatal(info, ok)
	}
	// 空分支没有结论：明确拒绝，而不是塞一条空报告
	code, out = c.do("POST", "/api/branches/"+fork.BranchSID, map[string]any{"action": "report"}, true)
	if code != 400 || !strings.Contains(out["error"].(string), "结论") {
		t.Fatal(code, out)
	}
	code, out = c.do("POST", "/api/branches/"+fork.BranchSID, map[string]any{"action": "pause"}, true)
	if code != 200 || out["branch"].(map[string]any)["status"] != "paused" {
		t.Fatal(code, out)
	}
	code, out = c.do("POST", "/api/branches/"+fork.BranchSID, map[string]any{"action": "archive"}, true)
	if code != 200 || out["branch"].(map[string]any)["status"] != "archived" {
		t.Fatal(code, out)
	}
	// 归档之后不能再继续：上下文还在，但要先取消归档（现在是显式拒绝，不静默重开）
	code, out = c.do("POST", "/api/branches/"+fork.BranchSID, map[string]any{"action": "continue", "prompt": "接着做"}, true)
	if code != 400 || !strings.Contains(out["error"].(string), "归档") {
		t.Fatalf("archived branch should refuse continue: %d %v", code, out)
	}
	if code, _ := c.do("POST", "/api/branches/"+fork.BranchSID, map[string]any{"action": "nonsense"}, true); code != 400 {
		t.Fatal("unknown action", code)
	}
}
