package app

import (
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/proto"
)

func doingOf(e *env, id, viewer string) Doing {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	ag, _ := e.a.agentStore.Get(id)
	return e.a.agentDoing(ag, viewer)
}

// 工具型：闲着 / 在干活
func TestDoingToolAgent(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	if d := doingOf(e, agents.DefaultID, ""); d.State != "idle" || d.Text != "闲着，等人找它" {
		t.Fatalf("闲着：%+v", d)
	}
	e.say("帮我跑一下测试")
	e.k.expect(proto.TInput)
	if d := doingOf(e, agents.DefaultID, e.sid); d.State != "working" || d.Text != "在干活（你）" {
		t.Fatalf("在干活：%+v", d)
	}
	e.say("/state")
	if msg := e.ch.wait(t, "在干活"); !strings.Contains(msg, "🛠️ 在干活（你）") {
		t.Fatalf("/state：%s", msg)
	}
}

// 拟人型：睡着、摸鱼、发呆、在冲浪、在跟人聊天
func TestDoingLifeAgent(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)

	setNow(e, day.Add(2*time.Hour)) // 02:00，默认 08:00 醒
	if d := doingOf(e, agents.DefaultID, ""); d.State != "sleeping" || d.Text != "睡着了，08:00 醒" {
		t.Fatalf("睡着：%+v", d)
	}
	setNow(e, day.Add(23*time.Hour+40*time.Minute))
	if d := doingOf(e, agents.DefaultID, ""); d.Text != "睡着了，明天 08:00 醒" {
		t.Fatalf("晚上睡着：%+v", d)
	}

	// 醒着：刚聊完算摸鱼，过一阵算发呆，很久没人找算发呆得久了
	noon := day.Add(12 * time.Hour)
	setNow(e, noon)
	e.a.mu.Lock()
	e.a.sessOf(e.sid).lastActivity = noon.Add(-10 * time.Minute)
	e.a.mu.Unlock()
	if d := doingOf(e, agents.DefaultID, ""); d.State != "slacking" || !strings.HasPrefix(d.Text, "在摸鱼") {
		t.Fatalf("摸鱼：%+v", d)
	}
	setNow(e, noon.Add(time.Hour))
	if d := doingOf(e, agents.DefaultID, ""); d.State != "idle" || !strings.HasPrefix(d.Text, "在发呆") || strings.Contains(d.Text, "没人找") {
		t.Fatalf("发呆：%+v", d)
	}
	setNow(e, noon.Add(5*time.Hour))
	if d := doingOf(e, agents.DefaultID, ""); !strings.HasPrefix(d.Text, "在发呆，5 小时没人找它了") {
		t.Fatalf("发呆久了：%+v", d)
	}

	// 内心活动：按最近用的工具说
	e.a.mu.Lock()
	ls := e.a.sessOf(lifeSID(agents.DefaultID))
	ls.run = &run{tool: "WebSearch"}
	e.a.mu.Unlock()
	if d := doingOf(e, agents.DefaultID, ""); d.State != "thinking" || d.Text != "在上网冲浪" {
		t.Fatalf("冲浪：%+v", d)
	}
	e.a.mu.Lock()
	ls.run = nil
	e.a.mu.Unlock()

	// 在跟人聊天：访客看 /state 只看到不带人名的说法
	e.say("在吗")
	e.k.expect(proto.TInput)
	if d := doingOf(e, agents.DefaultID, ""); d.State != "chatting" || d.Brief != "在跟人聊天" || !strings.HasPrefix(d.Text, "在跟") {
		t.Fatalf("聊天：%+v", d)
	}
	e.say("/state", from("u2", "李四"))
	msg := e.ch.wait(t, "在跟人聊天")
	if strings.Contains(msg, "张三") || strings.Contains(msg, "近况") {
		t.Fatalf("访客看到的：%s", msg)
	}
}
