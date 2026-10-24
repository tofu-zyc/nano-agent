package app

import (
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// 访客不能拿 Message 给群和别的 agent 发消息。
func TestMessageGuestRejected(t *testing.T) {
	e := setup(t)
	e.useDir(t, "work")
	e.a.cfg.Access.Guest = "readonly"
	e.sid = "fake:c1"
	e.say("你好", from("u2", "李四"))
	e.k.expect(proto.TInput)
	e.a.mu.Lock()
	s := e.a.sessOf(e.sid)
	e.a.execTool("test", s, &proto.Msg{T: proto.TExec, SID: e.sid, ID: "c1", Name: "Message",
		Args: []byte("{\"to\":\"agent:default\",\"text\":\"你好\"}")})
	e.a.mu.Unlock()
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || *res.OK || !strings.Contains(res.Content, "只有管理员") {
		t.Fatalf("访客不该能用 Message：%+v", res)
	}
}

// “要不要说话”的判断本身也花模型调用：额度跟积极度走，两次之间至少隔一分钟。
func TestGroupThoughtBudget(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.Local)
	e.a.mu.Lock()
	ag, ok := e.a.agentStore.Get(agents.DefaultID)
	if !ok {
		e.a.mu.Unlock()
		t.Fatal("没有默认 agent")
	}
	for eag, want := range map[string]int{"low": 6, "normal": 18, "high": 45} {
		v := eag
		ag.GroupEagerness = &v
		if err := e.a.agentStore.Update(ag); err != nil {
			e.a.mu.Unlock()
			t.Fatal(err)
		}
		rule := e.a.groupRuleOf("main")
		if rule.Decisions != want || rule.ThinkGap != time.Minute {
			e.a.mu.Unlock()
			t.Fatalf("%s：判断额度 %d / %v，期望 %d / 1m", eag, rule.Decisions, rule.ThinkGap, want)
		}
	}
	rule := e.a.groupRuleOf("main")
	s := e.a.sessOf("main")
	s.groupThoughtAt = now
	if e.a.groupMayThink(s, rule, now.Add(30*time.Second)) {
		e.a.mu.Unlock()
		t.Fatal("两次判断之间不该只隔 30 秒")
	}
	if !e.a.groupMayThink(s, rule, now.Add(61*time.Second)) {
		e.a.mu.Unlock()
		t.Fatal("隔够一分钟应该能判断")
	}
	// 一小时内判断够额度以后就不再判断
	s.groupThoughtAt = time.Time{}
	s.groupThoughts = nil
	for i := 0; i < rule.Decisions; i++ {
		s.groupThoughts = append(s.groupThoughts, now.Add(-time.Duration(i)*time.Minute))
	}
	if e.a.groupMayThink(s, rule, now) {
		e.a.mu.Unlock()
		t.Fatal("判断次数到了额度还在判断")
	}
	e.a.mu.Unlock()
}

// 中文名字取不出拉丁字母：用短随机 ID。
func TestAgentIDForChineseName(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	id := agentIDSuggestion(store, "小鲍")
	if id == "agent" || !strings.HasPrefix(id, "a-") || len(id) != 8 {
		t.Fatalf("中文名字的 ID 不对：%q", id)
	}
	if again := agentIDSuggestion(store, "另一个人"); again == id {
		t.Fatalf("两次随机 ID 撞了：%q", again)
	}
	if got := agentIDSuggestion(store, "Alice Li"); got != "alice-li" {
		t.Fatalf("英文名字还应该用名字：%q", got)
	}
}

// 时长按 2h / 1h30m / 30m / 20s 写，不是 30m0s。
func TestDurText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                               "不限",
		30 * time.Minute:                "30m",
		2 * time.Hour:                   "2h",
		90 * time.Minute:                "1h30m",
		20 * time.Second:                "20s",
		45*time.Minute + 30*time.Second: "45m30s",
	} {
		if got := durText(d); got != want {
			t.Fatalf("%v 应该写成 %q，实际 %q", d, want, got)
		}
	}
}

// 停用的机器人也在 Channels 里（图上能看到它，也能改回来）。
func TestChannelsIncludeDisabledBots(t *testing.T) {
	e := setup(t)
	_, bots := withAgents(t, e)
	if err := bots.Create(agents.Bot{ID: "work", Kind: agents.KindTelegram, Name: "工作号",
		Token: "123:abc", Agent: agents.DefaultID, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	e.a.reconcileBots()
	var found bool
	for _, c := range e.a.Channels() {
		if c.Name == "tg.work" {
			found = true
			if c.Status.State != "disabled" {
				t.Fatalf("停用的机器人状态：%+v", c.Status)
			}
		}
	}
	if !found {
		t.Fatal("停用的机器人不在 Channels 里")
	}
	// 群里 agent 旁听的会话不当普通会话显示
	e.a.mu.Lock()
	sid := e.a.roomMemberSID("default", "x1")
	_ = e.a.store.Append(sid, session.Msg{Role: "user", Content: "用户: hi"})
	e.a.mu.Unlock()
	for _, s := range e.a.Snapshot().Sessions {
		if s.SID == sid {
			t.Fatal("旁听会话不该出现在会话列表里")
		}
	}
}
