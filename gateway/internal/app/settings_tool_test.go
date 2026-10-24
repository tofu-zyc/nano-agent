package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/secret"
)

// 接上一个真的配置存储（和 main 里一样：改了就换上新配置）
func withConfigStore(t *testing.T, e *env) *config.Store {
	t.Helper()
	if err := secret.Init(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(secret.Reset)
	path := filepath.Join(t.TempDir(), "gateway.toml")
	os.WriteFile(path, []byte("[kernel]\ntoken = \"k\"\n[llm]\nprovider = \"mock\"\n[access]\nadmins = [\"fake:u1\"]\n"), 0o600)
	st, _, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.OnChange(func(_, cur *config.Config) { e.a.SetConfig(cur) })
	e.a.SetConfigStore(st)
	return st
}

func adminCall(e *env, raw string) (bool, string) {
	var ok bool
	var text string
	e.a.gatewayAdminCall(mainSID, "fake:u1", raw, func(o bool, s string) { ok, text = o, s })
	return ok, text
}

func queryCall(e *env, raw string) (bool, string) {
	var ok bool
	var text string
	e.a.gatewayQuery(mainSID, raw, func(o bool, s string) { ok, text = o, s })
	return ok, text
}

func TestSettingsQueryAndSet(t *testing.T) {
	e := setup(t)
	withConfigStore(t, e)

	ok, text := queryCall(e, `{"what":"settings","group":"memory"}`)
	if !ok || !strings.Contains(text, "context.compact_at") || strings.Contains(text, "search.enabled") {
		t.Fatalf("查 memory 组：%v %s", ok, text)
	}
	if ok, text = queryCall(e, `{"what":"settings","group":"web"}`); !ok || !strings.Contains(text, `"web.listen"`) || !strings.Contains(text, `"changeable": false`) || !strings.Contains(text, "网页拓扑图的「网页」节点") {
		t.Fatalf("要重启的项应当标成不能改，并说在哪个节点改：%s", text)
	}
	if ok, text = queryCall(e, `{"what":"settings","group":"agent"}`); !ok || !strings.Contains(text, `"agent.max_steps"`) || !strings.Contains(text, `"workspace.mode"`) {
		t.Fatalf("查 agent 组：%s", text)
	}
	if ok, text = adminCall(e, `{"action":"settings_set","key":"agent.max_steps","value":0}`); !ok {
		t.Fatalf("改每条消息的上限：%s", text)
	}

	if ok, text = adminCall(e, `{"action":"settings_set","key":"context.compact_at","value":0.8}`); !ok {
		t.Fatalf("改整理阈值：%s", text)
	}
	e.a.mu.Lock()
	at := e.a.cfg.Context.CompactAt
	e.a.mu.Unlock()
	if at != 0.8 {
		t.Fatalf("没生效：%v", at)
	}
	if ok, text = adminCall(e, `{"action":"settings_set","key":"channels.show","value":"详细"}`); !ok {
		t.Fatalf("改显示档位：%s", text)
	}
	if ok, _ = adminCall(e, `{"action":"settings_set","key":"channels.show","value":"乱写"}`); ok {
		t.Fatal("不合法的值应当失败")
	}
	for _, key := range []string{"web.listen", "telegram.token", "session.data_dir", "没有这项"} {
		if ok, text = adminCall(e, `{"action":"settings_set","key":"`+key+`","value":"x"}`); ok {
			t.Fatalf("%s 不该能改：%s", key, text)
		}
	}

	// 搜索 key：加密保存，结果和查询里都不出现 key 本身
	if ok, text = adminCall(e, `{"action":"settings_set","key":"search.api_key","value":"sk-search-secret-123456"}`); !ok || strings.Contains(text, "sk-search") {
		t.Fatalf("设搜索 key：%v %s", ok, text)
	}
	if si := e.a.SearchInfo(); si.Key != "settings" {
		t.Fatalf("搜索 key 来源：%+v", si)
	}
	if ok, text = queryCall(e, `{"what":"settings","group":"search"}`); !ok || strings.Contains(text, "sk-search") || !strings.Contains(text, `"is_set": true`) {
		t.Fatalf("查 search 组：%s", text)
	}
	if ok, text = queryCall(e, `{"what":"search"}`); !ok || !strings.Contains(text, `"settings"`) {
		t.Fatalf("查搜索状态：%s", text)
	}
}

// 审批卡片上，改密钥类设置的值要打星
func TestSettingsSetApprovalMasksSecret(t *testing.T) {
	_, body := callCard("GatewayAdmin", json.RawMessage(`{"action":"settings_set","key":"search.api_key","value":"sk-search-secret-123456"}`))
	if strings.Contains(body, "sk-search") || !strings.Contains(body, "***") {
		t.Fatalf("审批卡片：%s", body)
	}
	_, body = callCard("GatewayAdmin", json.RawMessage(`{"action":"settings_set","key":"context.compact_at","value":0.8}`))
	if !strings.Contains(body, "0.8") {
		t.Fatalf("普通设置照常显示：%s", body)
	}
}

// 删除历史前先查轮次：编号从 0 起，正在执行的这一轮标出来
func TestTurnsQuery(t *testing.T) {
	e := setup(t)
	e.say("第一轮问题")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	e.say("第二轮问题")
	e.k.expect(proto.TInput)
	ok, text := queryCall(e, `{"what":"turns"}`)
	if !ok || !strings.Contains(text, `"total": 2`) || !strings.Contains(text, "第一轮问题") || strings.Contains(text, "[2026") {
		t.Fatalf("轮次：%s", text)
	}
	var out struct {
		Turns []TurnInfo `json:"turns"`
	}
	json.Unmarshal([]byte(text), &out)
	if len(out.Turns) != 2 || out.Turns[0].Turn != 0 || out.Turns[0].Running || !out.Turns[1].Running || !out.Turns[0].InContext {
		t.Fatalf("轮次：%+v", out.Turns)
	}
	e.roundTrip(t)
}

// 聊天里查设置的分组就是拓扑图上的节点：工具 schema 里列的要和 config.Nodes 一致
func TestSettingsGroupsMatchNodes(t *testing.T) {
	var sc struct {
		Properties struct {
			Group struct {
				Enum []string `json:"enum"`
			} `json:"group"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(gatewaySchema), &sc); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, n := range config.Nodes {
		want = append(want, n.ID)
	}
	if strings.Join(sc.Properties.Group.Enum, ",") != strings.Join(want, ",") {
		t.Fatalf("Gateway 工具的 group：%v，节点：%v", sc.Properties.Group.Enum, want)
	}
}
