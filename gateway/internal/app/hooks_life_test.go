package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"nano-agent/gateway/internal/hooks"
	"nano-agent/gateway/internal/proto"
)

// 内心活动（life 会话）要能给自己登记 Hook：job.completed 叫我看图这类用法走的就是这条路。
// 以前 canAccessForkContext 只认主人会话和公开群聊，life 会话登记一律
// "Hook 来源会话与 agent 不可用"，于是 agent 永远只能用 Schedule 轮询。
func TestHookCreateFromLifeSession(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	sid := lifeSID("default")
	spec := hooks.Spec{Name: "任务好了叫我", On: []string{"device.online"},
		Handler: hooks.Handler{Type: "agent", Prompt: "看看这次事件"}}
	out, err := e.a.HookAction(context.Background(), sid, "life:default", HookRequest{Action: "create", OperationID: "life-one", Spec: &spec})
	if err != nil || out.Rule == nil {
		t.Fatalf("life 会话建不了 Hook：%v / %v", out, err)
	}
	if out.Rule.Spec.Scope.SID != sid || out.Rule.AgentID != "default" {
		t.Fatalf("来源会话没记对：%+v", out.Rule.Spec.Scope)
	}
	// 登记之后每次派发都要重新过一遍权限：life 自己就是这条规则的合法来源。
	e.a.mu.Lock()
	authority := e.a.hookAuthority(out.Rule.RuleRecord, out.Rule.Spec)
	e.a.mu.Unlock()
	if authority != nil {
		t.Fatalf("执行时的权限复检不认这条规则：%v", authority)
	}
	e.a.mu.Lock()
	rules := e.a.jobEvents.Rules("default")
	e.a.mu.Unlock()
	if len(rules) != 1 {
		t.Fatalf("规则没落到 agent 上：%+v", rules)
	}
}

// 登记成功只是第一步：事件到了要真的叫醒内心活动，而且这次投递要记成成功。
// 以前结果投影会去找聊天窗口，life 没有窗口，于是每次执行都记成 uncertain。
func TestLifeHookWakesTheInnerSession(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	sid := lifeSID("default")
	spec := hooks.Spec{Name: "任务好了叫我", On: []string{"device.online"},
		Handler: hooks.Handler{Type: "agent", Conversation: "life", Prompt: "看看这次事件"}}
	out, err := e.a.HookAction(context.Background(), sid, "life:default", HookRequest{Action: "create", OperationID: "fire-one", Spec: &spec})
	if err != nil || out.Rule == nil {
		t.Fatalf("建不了：%v / %v", out, err)
	}
	event := appHookEvent(t, e, 1, sid)
	in := e.k.expect(proto.TInput)
	if in.SID != sid || in.From != "life:default" {
		t.Fatalf("内心片段开在 %s（from=%s），不是 %s", in.SID, in.From, sid)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	hookDelivery(t, e, *out.Rule, event, "succeeded")
}

// 没有聊天窗口的会话登记 notify 只会永远重试；登记的时候就要说清楚。
func TestHookNotifyFromLifeSessionExplainsTheMissingWindow(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	spec := hooks.Spec{Name: "到点提醒", On: []string{"deadline.at"},
		Handler: hooks.Handler{Type: "notify", Text: "到点了"}, DeadlineAt: 4102444800000}
	_, err := e.a.HookAction(context.Background(), lifeSID("default"), "life:default", HookRequest{Action: "create", OperationID: "life-two", Spec: &spec})
	if err == nil || !strings.Contains(err.Error(), "投递窗口") || !strings.Contains(err.Error(), "conversation=life") {
		t.Fatalf("notify 没有窗口时应当说清楚怎么改：%v", err)
	}
	// deliver=none 的（安静）规则不需要窗口，照样能建
	spec.Deliver = "none"
	if _, err := e.a.HookAction(context.Background(), lifeSID("default"), "life:default", HookRequest{Action: "create", OperationID: "life-three", Spec: &spec}); err != nil {
		t.Fatalf("deliver=none 不需要投递窗口：%v", err)
	}
}

// MCP 多的会话工具表上百条。以前登记规则时把整张工具表塞进 scope.tools，
// 一定会撞 128 这个上限，于是「换遍事件名、名称、scope 都没用」。
func TestHookCreateOnSessionWithManyTools(t *testing.T) {
	e := setup(t)
	tools := []proto.ToolSpec{}
	for i := 0; i < 140; i++ {
		tools = append(tools, proto.ToolSpec{Name: fmt.Sprintf("mcp_srv__tool_%03d", i), Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`)})
	}
	e.k.send(proto.Msg{T: proto.TTools, Tools: tools})
	waitFor(t, func() bool { return len(e.a.Snapshot().Kernels[0].Tools) == 140 })
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	e.a.mu.Unlock()
	// notify 不调用工具：不冻结工具表，登记照常通过
	notify := hooks.Spec{Name: "看图", On: []string{"job.completed"}, Handler: hooks.Handler{Type: "notify", Text: "任务好了"}}
	out, err := e.a.HookAction(context.Background(), mainSID, "fake:u1", HookRequest{Action: "create", OperationID: "many-notify", Spec: &notify})
	if err != nil || out.Rule == nil {
		t.Fatalf("工具多的会话建不了 notify：%v / %v", out, err)
	}
	if out.Rule.Spec.Scope.Tools != nil {
		t.Fatalf("notify 不该冻结工具表：%d 条", len(out.Rule.Spec.Scope.Tools))
	}
	// agent 处理器要用工具，但工具表超过上限时也不能把登记挡死：留空表示不额外收窄
	agent := hooks.Spec{Name: "看图", On: []string{"job.completed"}, Handler: hooks.Handler{Type: "agent", Prompt: "看看这张图"}}
	out2, err := e.a.HookAction(context.Background(), mainSID, "fake:u1", HookRequest{Action: "create", OperationID: "many-agent", Spec: &agent})
	if err != nil || out2.Rule == nil {
		t.Fatalf("工具多的会话建不了 agent 规则：%v / %v", out2, err)
	}
}

// 超出上限的报错要说清是哪一项、收了多少，不能合成一句“超限”。
func TestHookValidationSaysWhichLimit(t *testing.T) {
	on := make([]string, 17)
	for i := range on {
		on[i] = "device.online"
	}
	_, err := hooks.Normalize(hooks.Spec{Name: "x", On: on, Handler: hooks.Handler{Type: "notify", Text: "x"}, Deliver: "none"})
	if err == nil || !strings.Contains(err.Error(), "16") || !strings.Contains(err.Error(), "17") {
		t.Fatalf("事件数量超限没有报出实际数字：%v", err)
	}
	_, err = hooks.Normalize(hooks.Spec{Name: "x", On: []string{"device.online"}, Handler: hooks.Handler{Type: "notify", Text: "x"},
		Deliver: "none", Scope: hooks.Scope{Mode: "none"}, DebounceMS: 90000})
	if err == nil || !strings.Contains(err.Error(), "debounce_ms") || !strings.Contains(err.Error(), "90000") {
		t.Fatalf("debounce 超限没有报出字段名和数值：%v", err)
	}
	_, err = hooks.Normalize(hooks.Spec{Name: "x", On: []string{"device.online"}, Handler: hooks.Handler{Type: "nope"}, Scope: hooks.Scope{Mode: "none"}})
	if err == nil || !strings.Contains(err.Error(), "handler.type") {
		t.Fatalf("处理器类型写错没有说是哪一项：%v", err)
	}
}
