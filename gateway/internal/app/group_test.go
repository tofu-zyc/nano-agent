package app

import (
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
)

// 群聊：不是每句都回，先记下来；被叫到才说话；安静够了可以自己插一句。

func groupSession(t *testing.T, e *env) string {
	t.Helper()
	return "fake:c1"
}

func TestGroupListensWithoutReplying(t *testing.T) {
	e := setup(t)
	e.say("今天天气不错", group(false))
	e.k.none() // 没被叫到：不调模型
	e.a.mu.Lock()
	st := e.a.store.Settings(groupSession(t, e))
	hist := e.a.store.History(groupSession(t, e))
	e.a.mu.Unlock()
	if !st.Group {
		t.Fatal("群会话没有标记成群聊")
	}
	if len(hist) != 1 || !strings.Contains(hist[0].Content, "[") || !strings.Contains(hist[0].Content, "今天天气不错") {
		t.Fatalf("群消息没记进历史：%+v", hist)
	}
}

func TestGroupRepliesWhenMentioned(t *testing.T) {
	e := setup(t)
	sid := groupSession(t, e)
	e.say("@机器人 几点了", group(true))
	in := e.k.expect(proto.TInput) // 等 2 秒把后面几条带上
	if in.SID != sid {
		t.Fatalf("群消息没进这个群的会话：%q", in.SID)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	got := e.ch.wait(t, "几点了")
	if strings.Contains(got, "次模型调用") {
		t.Fatalf("群聊回复不该带用量：%q", got)
	}
}

func TestGroupReplySplitAndSilence(t *testing.T) {
	e := setup(t)
	e.say("随便聊聊", group(false))
	e.a.mu.Lock()
	s := e.a.sessOf(groupSession(t, e))
	ep := endpoint{ch: "fake", chat: "c1"}
	s.groupEp = ep
	// 只回 [不回复]：什么都不发
	e.a.sendGroup(s, ep, "[不回复]")
	e.a.mu.Unlock()
	select {
	case m := <-e.ch.sent:
		t.Fatalf("说了 [不回复] 还发消息：%q", m)
	case <-time.After(200 * time.Millisecond):
	}
	// 长回复：拆成几条，每条不超过上限
	e.a.mu.Lock()
	e.a.sendGroup(s, ep, strings.Repeat("这是一句话。", 120))
	e.a.mu.Unlock()
	e.ch.wait(t, "这是一句话。")
}

func TestGroupParts(t *testing.T) {
	parts := groupParts("第一段。\n\n第二段。\n\n第三段。\n\n第四段。", 3, 300)
	if len(parts) != 3 || parts[0] != "第一段。" || parts[2] != "第三段。" {
		t.Fatalf("拆条不对：%+v", parts)
	}
	long := strings.Repeat("字", 800)
	parts = groupParts(long, 3, 300)
	if len(parts) != 3 {
		t.Fatalf("最多 3 条，实际 %d", len(parts))
	}
	for _, p := range parts {
		if len([]rune(p)) > 300 {
			t.Fatalf("一条超过上限：%d 字", len([]rune(p)))
		}
	}
}

func TestGroupProactive(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	sid := groupSession(t, e)
	base := time.Date(2026, 10, 21, 15, 8, 0, 0, time.UTC)
	e.a.mu.Lock()
	e.a.now = func() time.Time { return base }
	e.a.mu.Unlock()
	e.say("大家好", group(false))

	// 安静期没到：不插话
	e.a.mu.Lock()
	e.a.now = func() time.Time { return base.Add(5 * time.Second) }
	e.a.mu.Unlock()
	e.a.groupTick()
	e.k.none()

	// 安静够了：自己凑上来一次
	e.a.mu.Lock()
	e.a.now = func() time.Time { return base.Add(25 * time.Second) }
	e.a.mu.Unlock()
	e.a.groupTick()
	in := e.k.expect(proto.TInput)
	if in.SID != sid {
		t.Fatalf("插话没进这个群的会话：%q", in.SID)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	e.ch.wait(t, "收到")

	// 刚说过话：最短间隔没到，不再说
	e.a.mu.Lock()
	e.a.now = func() time.Time { return base.Add(28 * time.Second) }
	e.a.sessOf(sid).groupFresh = true
	e.a.mu.Unlock()
	e.a.groupTick()
	e.k.none()

	// 积极度 off：永远不主动说话
	e.a.mu.Lock()
	off := "off"
	ag, _ := e.a.agentStore.Get("default")
	ag.GroupEagerness = &off
	if err := e.a.agentStore.Update(ag); err != nil {
		t.Fatal(err)
	}
	e.a.now = func() time.Time { return base.Add(10 * time.Minute) }
	e.a.sessOf(sid).groupFresh = true
	e.a.mu.Unlock()
	e.a.groupTick()
	e.k.none()
}

func TestGroupToolsAndPrompt(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.say("大家好", group(false))
	all := []llm.Tool{
		{Type: "function", Function: llm.ToolFunction{Name: "WebSearch"}},
		{Type: "function", Function: llm.ToolFunction{Name: "Read"}},
		{Type: "function", Function: llm.ToolFunction{Name: "Shell"}},
	}
	e.a.mu.Lock()
	got := e.a.groupTools(groupSession(t, e), all)
	system, _, _, _ := e.a.contextParts(groupSession(t, e))
	ws := e.a.workspaceOf(groupSession(t, e))
	ag, _ := e.a.agentStore.Get("default")
	only := []string{"Read"}
	ag.GroupTools = only
	if err := e.a.agentStore.Update(ag); err != nil {
		t.Fatal(err)
	}
	custom := e.a.groupTools(groupSession(t, e), all)
	e.a.mu.Unlock()
	if len(got) != 1 || got[0].Function.Name != "WebSearch" {
		t.Fatalf("群聊默认只该有搜索：%+v", got)
	}
	if !strings.Contains(system, "这是群聊") {
		t.Fatalf("群聊系统提示里没有群聊守则：%q", system)
	}
	if len(ws.wss) != 0 {
		t.Fatalf("群聊默认不该挂工作目录：%+v", ws.wss)
	}
	if len(custom) != 1 || custom[0].Function.Name != "Read" {
		t.Fatalf("agent 的群聊白名单没生效：%+v", custom)
	}
}
