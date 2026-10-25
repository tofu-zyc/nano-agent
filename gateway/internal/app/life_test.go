package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/proto"
)

// 仓库里自带的 skills/life/SKILL.md 和内置的兜底要一样。
func TestLifeSkillMatchesRepo(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "skills", "life", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(b), "---\n", 3)
	if len(parts) != 3 || strings.TrimSpace(parts[2]) != defaultLifeSkill {
		t.Fatal("skills/life/SKILL.md 的正文和 life.go 里的 defaultLifeSkill 不一样")
	}
}

// makeLife 把默认 agent 改成拟人型，随机数固定成 0.5。
func makeLife(t *testing.T, e *env, store *agents.Store, edit func(*agents.Agent)) {
	t.Helper()
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	ag, _ := store.Get(agents.DefaultID)
	ag.Style = agents.StyleLife
	if edit != nil {
		edit(&ag)
	}
	if err := store.Update(ag); err != nil {
		t.Fatal(err)
	}
	e.a.rnd = func() float64 { return 0.5 }
}

func TestLifeWake(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	e.a.lifeTick() // 刚变成拟人型：先定下第一次醒来（5 + 0.5×10 = 10 分钟后）
	e.k.none()
	setNow(e, base.Add(9*time.Minute))
	e.a.lifeTick()
	e.k.none()
	setNow(e, base.Add(21*time.Minute)) // 也过了“刚有人找过它”的 20 分钟（启动时间算起）
	e.a.lifeTick()
	in := e.k.expect(proto.TInput)
	sid := lifeSID("default")
	if in.SID != sid || in.MaxSteps != 12 {
		t.Fatalf("内心活动的一轮：%+v", in)
	}
	e.a.mu.Lock()
	system, tools, _, _ := e.a.contextParts(sid)
	e.a.mu.Unlock()
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Function.Name)
	}
	if !strings.Contains(system, "有自己生活的人") || !strings.Contains(system, "一个人的时候") || strings.Join(names, ",") != "Knowledge,Message" {
		t.Fatalf("内心活动的提示和工具：%v", names)
	}
	e.sid = sid
	e.k.send(proto.Msg{T: proto.TModel, SID: sid})
	e.k.expect(proto.TTurn)
	wake := e.llm.lastReq().Messages
	note := wake[len(wake)-1].Content
	if !strings.Contains(note, "你醒着") || !strings.Contains(note, "主人（chat:main）") || !strings.Contains(note, "今天第 1 次醒来，今天还能主动找人 2 次") {
		t.Fatalf("醒来时看到的：%q", note)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
	e.a.mu.Lock()
	ls := e.a.lifeOf("default")
	next, wakes, recent := ls.Next, ls.Wakes, len(ls.Recent)
	info := e.a.lifeBrief(func() agents.Agent { ag, _ := store.Get("default"); return ag }())
	e.a.mu.Unlock()
	// normal：1 小时 + 0.5×1 小时
	if want := base.Add(21*time.Minute + 90*time.Minute).Unix(); next != want || wakes != 1 || recent != 1 {
		t.Fatalf("醒完：next=%v（期望 %v）wakes=%d recent=%d", time.Unix(next, 0), time.Unix(want, 0), wakes, recent)
	}
	if info.State != "awake" || len(info.Recent) != 1 || info.Wakes != 1 {
		t.Fatalf("网页上的状态：%+v", info)
	}
	// 近况带进聊天，只放一次
	e.sid = mainSID
	e.say("在干嘛")
	e.k.expect(proto.TInput)
	if got := lastUser(e.roundTrip(t)); !strings.Contains(got, lifeRecentOp) || !strings.Contains(got, "（mock）") {
		t.Fatalf("聊天里没带近况：%q", got)
	}
	e.say("哦")
	e.k.expect(proto.TInput)
	if got := lastUser(e.roundTrip(t)); strings.Contains(got, lifeRecentOp) {
		t.Fatalf("近况没变不该再放：%q", got)
	}
	// /wake 立刻叫醒（不算每天的次数）
	e.say("/wake")
	in = e.k.expect(proto.TInput)
	if in.SID != sid {
		t.Fatalf("/wake：%+v", in)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: sid, Status: proto.StatusOK})
	waitIdle(t, e, sid)
	e.a.mu.Lock()
	wakes = e.a.lifeOf("default").Wakes
	e.a.mu.Unlock()
	if wakes != 1 {
		t.Fatalf("叫醒的不算次数：%d", wakes)
	}
}

func TestLifeBlocked(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	budget := 1000
	makeLife(t, e, store, func(ag *agents.Agent) { ag.LifeBudget = &budget })
	ag, _ := store.Get(agents.DefaultID)
	e.a.mu.Lock()
	e.a.bootAt = time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	ls := e.a.lifeOf("default")
	e.a.mu.Unlock()
	check := func(at time.Time, want string) {
		t.Helper()
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		e.a.now = func() time.Time { return at }
		if got := e.a.lifeBlocked(ag, ls, at); got != want {
			t.Fatalf("%v：期望 %q，实际 %q", at, want, got)
		}
	}
	noon := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	check(noon, "")
	check(time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC), "睡着")
	e.a.mu.Lock()
	e.a.sessOf(mainSID).lastActivity = noon.Add(-5 * time.Minute)
	e.a.mu.Unlock()
	check(noon, "刚有人找过它")
	check(noon.Add(time.Hour), "")
	ls.Tokens = 1000
	check(noon.Add(time.Hour), "今天的预算用完了")
	ls.Tokens, ls.Wakes = 0, 8
	check(noon.Add(time.Hour), "今天醒够了")
	// 工具型没有内心活动
	e.a.mu.Lock()
	tool, _ := store.Get(agents.DefaultID)
	tool.Style = ""
	store.Update(tool)
	err := e.a.startLife("default", true)
	e.a.mu.Unlock()
	if err == nil {
		t.Fatal("工具型不该有内心活动")
	}
}

// 内心活动里主动找人：按找人范围、对方没回不再找、每天次数、睡觉时间不打扰。
func TestLifeReachOut(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	noon := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	setNow(e, noon)
	e.say("你好") // 管理员的窗口 fake:c1 接到主会话
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	e.sid = "fake:c2"
	e.say("我是访客", from("u2", "李四"), func(m *channel.Message) { m.ChatID = "c2" })
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	e.sid = mainSID
	life := lifeSID("default")
	send := func(target, text string) (string, error) {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		return e.a.messageChat(life, target, text)
	}
	// 只找主人（默认）：访客不行
	if _, err := send("fake:c2", "在吗"); err == nil || !strings.Contains(err.Error(), "只能主动找主人") {
		t.Fatalf("找人范围：%v", err)
	}
	out, err := send(mainSID, "我刚看到一篇讲猫的文章")
	if err != nil || !strings.Contains(out, "发出去了") {
		t.Fatalf("找主人：%q %v", out, err)
	}
	e.ch.wait(t, "讲猫的文章")
	e.a.mu.Lock()
	h := e.a.store.History(mainSID)
	e.a.mu.Unlock()
	if last := h[len(h)-1]; last.Role != "assistant" || last.Content != "我刚看到一篇讲猫的文章" {
		t.Fatalf("主动消息要记进那个聊天：%+v", last)
	}
	// 还没回：不能再找
	if _, err = send(mainSID, "人呢"); err == nil || !strings.Contains(err.Error(), "还没回") {
		t.Fatalf("没回不该再找：%v", err)
	}
	// 回了：可以再找
	e.say("什么文章")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	if _, err = send(mainSID, "就是讲橘猫的"); err != nil {
		t.Fatal(err)
	}
	e.ch.wait(t, "橘猫")
	e.say("哈哈")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	// 今天次数用完（normal 每天 2 次）
	if _, err = send(mainSID, "第三次"); err == nil || !strings.Contains(err.Error(), "次数用完") {
		t.Fatalf("每天次数：%v", err)
	}
	// 找人范围 all：能找访客；睡觉时间不行
	e.a.mu.Lock()
	ag, _ := store.Get(agents.DefaultID)
	all := "all"
	ag.LifeReach = &all
	store.Update(ag)
	e.a.lifeOf("default").Reaches = 0
	e.a.now = func() time.Time { return time.Date(2026, 10, 21, 23, 50, 0, 0, time.UTC) }
	e.a.mu.Unlock()
	if _, err = send("fake:c2", "睡了吗"); err == nil || !strings.Contains(err.Error(), "睡觉时间") {
		t.Fatalf("睡觉时间：%v", err)
	}
	setNow(e, noon.Add(2*time.Hour))
	if _, err = send("fake:c2", "下午好"); err != nil {
		t.Fatal(err)
	}
	e.ch.wait(t, "下午好")
	// 只能发自己的私聊
	if _, err = send("fake:g9", "x"); err == nil {
		t.Fatal("不存在的会话")
	}
}

// agent 设置：按组分；点“跟随全局”能恢复（v0.5 群聊几项恢复不了）；类型能改。
func TestAgentSettingsReset(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	if _, err := e.a.UpdateAgent("default", AgentPatch{Settings: map[string]any{"group_eagerness": "high", "life_awake": "09:00-22:00"}}); err != nil {
		t.Fatal(err)
	}
	info, err := e.a.UpdateAgent("default", AgentPatch{Settings: map[string]any{"group_eagerness": nil, "life_awake": nil}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range info.Settings {
		if (s.Key == "group_eagerness" || s.Key == "life_awake") && s.Value != nil {
			t.Fatalf("%s 没恢复：%+v", s.Key, s)
		}
		if s.Group == "" {
			t.Fatalf("%s 没有分组", s.Key)
		}
	}
	if _, err := e.a.UpdateAgent("default", AgentPatch{Settings: map[string]any{"life_awake": "八点到十点"}}); err == nil {
		t.Fatal("时间段写错了应该报错")
	}
	if _, err := e.a.UpdateAgent("default", AgentPatch{Settings: map[string]any{"dream_when": "sometimes"}}); err == nil {
		t.Fatal("不认识的选项应该报错")
	}
	life := "life"
	info, err = e.a.UpdateAgent("default", AgentPatch{Style: &life})
	if err != nil || info.Style != "life" || info.Life == nil {
		t.Fatalf("改成拟人型：%+v %v", info, err)
	}
	for _, s := range info.Settings {
		if s.Key == "group_eagerness" && s.Effective != "low" {
			t.Fatalf("拟人型的群聊积极度默认是 low：%+v", s)
		}
		if s.Key == "dream_when" && s.Effective != "sleep" {
			t.Fatalf("拟人型默认睡着时整理：%+v", s)
		}
	}
	bad := "robot"
	if _, err = e.a.UpdateAgent("default", AgentPatch{Style: &bad}); err == nil {
		t.Fatal("不认识的类型")
	}
}

// 新建时选拟人型：马上定下第一次醒来的时间；内心会话在列表里叫“X的内心”；醒来时说清楚上不了网。
func TestLifeCreateAndLabels(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.mu.Lock()
	e.a.rnd = func() float64 { return 0 }
	e.a.mu.Unlock()
	life := "life"
	info, err := e.a.CreateAgent(AgentPatch{Name: strp("小鲍"), Style: &life})
	if err != nil {
		t.Fatal(err)
	}
	if info.Style != "life" || info.Life == nil || info.Life.Next != e.a.now().Add(5*time.Minute).UnixMilli() {
		t.Fatalf("新建拟人型：%+v", info.Life)
	}
	e.a.mu.Lock()
	si := e.a.sessionInfo(lifeSID(info.ID))
	ag, _ := e.a.agentByID(info.ID)
	note := e.a.wakeNote(ag, e.a.lifeOf(info.ID), e.a.now(), false)
	e.a.mu.Unlock()
	if si.Title != "小鲍的内心" || si.Channel != "life" {
		t.Fatalf("内心会话的名字和图标：%q %q", si.Title, si.Channel)
	}
	if !strings.Contains(note, "网络搜索没开") || strings.Contains(note, "（agent:小纳）：") {
		t.Fatalf("醒来说明：%q", note)
	}
}

// mdCh 能按 Markdown 发、能显示“正在处理”的渠道（像飞书）
type mdCh struct {
	fakeCh
	mu   sync.Mutex
	md   []string
	busy []string // "on:<消息>" / "off:<消息>"
}

func (c *mdCh) SendMarkdown(_ context.Context, _, text, _ string) error {
	c.mu.Lock()
	c.md = append(c.md, text)
	c.mu.Unlock()
	c.sent <- text
	return nil
}

func (c *mdCh) Busy(_, msgID string) func() {
	c.mu.Lock()
	c.busy = append(c.busy, "on:"+msgID)
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.busy = append(c.busy, "off:"+msgID)
		c.mu.Unlock()
	}
}

func (c *mdCh) state() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.md), slices.Clone(c.busy)
}

// 拟人型私聊：回复正文按 Markdown 发；处理时在用户那条消息上显示“正在处理”，回完撤掉
func TestLifeDMMarkdownAndBusy(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	ch := &mdCh{fakeCh: fakeCh{name: "md", sent: make(chan string, 100)}}
	e.a.cfg.Access.Admins = append(e.a.cfg.Access.Admins, "md:u1")
	e.a.AddChannel(ch)
	e.a.OnMessage(ch, channel.Message{ChatID: "c1", SenderID: "u1", SenderName: "张三", Text: "你好", MsgID: "om_1", Mentioned: true})
	in := e.k.expect(proto.TInput)
	waitFor(t, func() bool { _, b := ch.state(); return len(b) == 1 })
	if _, b := ch.state(); b[0] != "on:om_1" {
		t.Fatalf("busy: %v", b)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	ch.wait(t, "收到：")
	waitFor(t, func() bool { _, b := ch.state(); return len(b) == 2 })
	md, b := ch.state()
	if b[1] != "off:om_1" || len(md) == 0 || !strings.Contains(md[0], "收到：") {
		t.Fatalf("md=%v busy=%v", md, b)
	}
}
