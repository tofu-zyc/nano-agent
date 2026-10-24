package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

func TestDayWindow(t *testing.T) {
	loc := time.UTC
	day := func(h, m int) time.Time { return time.Date(2026, 10, 24, h, m, 0, 0, loc) }
	w, err := parseWindow("08:00-23:30")
	if err != nil {
		t.Fatal(err)
	}
	if !w.contains(day(8, 0)) || !w.contains(day(23, 29)) || w.contains(day(23, 30)) || w.contains(day(3, 0)) {
		t.Fatal("08:00-23:30 判断不对")
	}
	if got := w.lastEnd(day(2, 0)); !got.Equal(day(23, 30).AddDate(0, 0, -1)) {
		t.Fatalf("凌晨两点，最近一次开始睡觉是昨天 23:30：%v", got)
	}
	if got := w.lastEnd(day(23, 45)); !got.Equal(day(23, 30)) {
		t.Fatalf("23:45 最近一次开始睡觉是今天 23:30：%v", got)
	}
	if got := w.nextStart(day(23, 45)); !got.Equal(day(8, 0).AddDate(0, 0, 1)) {
		t.Fatalf("下次醒来：%v", got)
	}
	night, _ := parseWindow("22:00-06:00")
	if !night.contains(day(23, 0)) || !night.contains(day(5, 0)) || night.contains(day(12, 0)) {
		t.Fatal("跨午夜的时间段判断不对")
	}
	all, _ := parseWindow("00:00-24:00")
	if !all.always() || !all.contains(day(3, 0)) {
		t.Fatal("全天")
	}
	for _, bad := range []string{"8点到23点", "25:00-01:00", "08:00"} {
		if _, err := parseWindow(bad); err == nil {
			t.Fatalf("%q 应该报错", bad)
		}
	}
}

// 仓库里自带的 skills/dream/SKILL.md 和内置的兜底要一样（改了一处别忘了另一处）。
func TestDreamSkillMatchesRepo(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "skills", "dream", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(b), "---\n", 3)
	if len(parts) != 3 || strings.TrimSpace(parts[2]) != defaultDreamSkill {
		t.Fatal("skills/dream/SKILL.md 的正文和 dream.go 里的 defaultDreamSkill 不一样")
	}
}

func fill(t *testing.T, e *env, sid string, n int, at time.Time) {
	t.Helper()
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if err := e.a.store.Append(sid, session.Msg{Role: role, Content: fmt.Sprintf("第 %d 句", i), TS: at.Add(time.Duration(i) * time.Minute).Unix()}); err != nil {
			t.Fatal(err)
		}
	}
}

func diaryDo(t *testing.T, e *env, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	return e.a.diaryCall(dreamSID("default"), raw)
}

func TestDiary(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	now := e.a.now()
	fill(t, e, "fake:old", 4, now.Add(-30*24*time.Hour)) // 一个月前：第一次整理不看
	fill(t, e, mainSID, 70, now.Add(-2*time.Hour))
	e.a.mu.Lock()
	e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "[系统] 环境说明\n[记忆]\n一堆记忆\n[/记忆]\n[2026-10-21 周三 15:08 · fake] 我搬家了", TS: now.Unix()})
	e.a.mu.Unlock()

	out, err := diaryDo(t, e, map[string]any{"action": "list"})
	if err != nil || !strings.Contains(out, "- main：") || !strings.Contains(out, "没整理的 71 条") || strings.Contains(out, "fake:old") || !strings.Contains(out, "主人") {
		t.Fatalf("list：%q %v", out, err)
	}
	out, _ = diaryDo(t, e, map[string]any{"action": "read", "source": mainSID})
	if !strings.Contains(out, "#0 [") || !strings.Contains(out, "主人：第 0 句") || !strings.Contains(out, "我：第 1 句") || !strings.Contains(out, "读到 #59") {
		t.Fatalf("read：%q", out)
	}
	if _, err = diaryDo(t, e, map[string]any{"action": "done", "source": mainSID, "upto": 59}); err != nil {
		t.Fatal(err)
	}
	out, _ = diaryDo(t, e, map[string]any{"action": "read", "source": mainSID})
	if !strings.Contains(out, "#60") || strings.Contains(out, "#59 ") || !strings.Contains(out, "主人：[2026-10-21 周三 15:08 · fake] 我搬家了") || strings.Contains(out, "一堆记忆") {
		t.Fatalf("接着读、去掉记忆说明：%q", out)
	}
	diaryDo(t, e, map[string]any{"action": "done", "source": mainSID, "upto": 70})
	if out, _ = diaryDo(t, e, map[string]any{"action": "list"}); !strings.Contains(out, "没有没整理的日记") {
		t.Fatalf("都整理完了：%q", out)
	}
	// 进度写在文件里
	b, _ := os.ReadFile(filepath.Join(e.dir, "agents", "default", "dream-state.json"))
	if !strings.Contains(string(b), `"main": 71`) {
		t.Fatalf("进度文件：%s", b)
	}
	// /reset 把历史挪进归档：会话从头算，没整理完的部分在归档里接着整理
	fill(t, e, mainSID, 3, now)
	e.a.mu.Lock()
	e.a.store.ArchiveReset(mainSID, now)
	e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "新的开始", TS: now.Unix()})
	e.a.mu.Unlock()
	out, _ = diaryDo(t, e, map[string]any{"action": "list"})
	if !strings.Contains(out, "- main：") || !strings.Contains(out, "archive:main/") || !strings.Contains(out, "没整理的 3 条") {
		t.Fatalf("清空后：%q", out)
	}
	// 不在整理记忆的会话里不能用
	raw, _ := json.Marshal(map[string]any{"action": "list"})
	e.a.mu.Lock()
	_, err = e.a.diaryCall(mainSID, raw)
	e.a.mu.Unlock()
	if err == nil {
		t.Fatal("别的会话不能读日记")
	}
}

// 整理记忆跑一轮：工具只有日记和记忆，提示里有整理方法，最终回复写进梦境日志。
func TestDreamRun(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	fill(t, e, mainSID, 6, e.a.now().Add(-time.Hour))
	if err := e.a.Dream("default"); err != nil {
		t.Fatal(err)
	}
	in := e.k.expect(proto.TInput)
	if in.SID != dreamSID("default") || in.MaxSteps != 150 {
		t.Fatalf("整理记忆的一轮：%+v", in)
	}
	e.a.mu.Lock()
	system, tools, _, _ := e.a.contextParts(in.SID)
	e.a.mu.Unlock()
	if !strings.Contains(system, "整理记忆") || len(tools) != 2 || tools[0].Function.Name != "Diary" || tools[1].Function.Name != knowledgeLabel {
		t.Fatalf("提示和工具：%d %+v", len(system), tools)
	}
	if err := e.a.Dream("default"); err == nil {
		t.Fatal("正在整理时不能再开一次")
	}
	e.sid = in.SID
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	msgs := e.llm.lastReq().Messages
	if last := msgs[len(msgs)-1].Content; !strings.Contains(last, "你要睡了") || !strings.Contains(last, "工具型") || !strings.Contains(last, "共 6 条") {
		t.Fatalf("开头的说明：%q", last)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	deadline := time.Now().Add(3 * time.Second)
	day := e.a.now().In(e.a.loc).Format("2006-01-02")
	for {
		b, _ := os.ReadFile(filepath.Join(e.dir, "agents", "default", "dreams", day+".md"))
		if strings.Contains(string(b), "（mock）") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("梦境日志没写：%q", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	days, text, err := e.a.DreamLog("default", "")
	if err != nil || len(days) != 1 || !strings.Contains(text, "（mock）") {
		t.Fatalf("网页读梦境日志：%v %q %v", days, text, err)
	}
	if info, _ := e.a.Agent("default"); info.Memory.LastDream == 0 || info.Memory.Dreaming {
		t.Fatalf("概况里的整理状态：%+v", info.Memory)
	}
	// 整理记忆的会话不出现在会话列表里
	for _, s := range e.a.Snapshot().Sessions {
		if s.SID == dreamSID("default") {
			t.Fatal("整理记忆的会话不该出现在会话列表里")
		}
	}
}

func setNow(e *env, t time.Time) {
	e.a.mu.Lock()
	e.a.now = func() time.Time { return t }
	e.a.mu.Unlock()
}

func TestDreamTriggers(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	base := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	setNow(e, base)
	e.a.dreamTick() // 记下启动时间
	fill(t, e, mainSID, 25, base.Add(-time.Hour))

	// 工具型默认空闲时整理：空闲不到 2 小时不整理
	setNow(e, base.Add(time.Hour))
	e.a.dreamTick()
	e.k.none()
	// 空闲够了、日记够多：整理
	setNow(e, base.Add(2*time.Hour+time.Minute))
	e.a.dreamTick()
	in := e.k.expect(proto.TInput)
	if in.SID != dreamSID("default") {
		t.Fatalf("应该开始整理：%+v", in)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitIdle(t, e, in.SID)
	// 每天最多一次
	setNow(e, base.Add(6*time.Hour))
	e.a.dreamTick()
	e.k.none()

	// 定时：每天 03:00
	e.a.mu.Lock()
	ag, _ := store.Get(agents.DefaultID)
	nightly, at := "nightly", "03:00"
	ag.DreamWhen, ag.DreamAt = &nightly, &at
	store.Update(ag)
	e.a.mu.Unlock()
	fill(t, e, mainSID, 2, base.Add(6*time.Hour))
	setNow(e, time.Date(2026, 10, 22, 2, 59, 0, 0, time.UTC))
	e.a.dreamTick()
	e.k.none()
	setNow(e, time.Date(2026, 10, 22, 3, 1, 0, 0, time.UTC))
	e.a.dreamTick()
	in = e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitIdle(t, e, in.SID)

	// 拟人型默认睡着时整理（醒着 08:00-23:30）
	e.a.mu.Lock()
	ag, _ = store.Get(agents.DefaultID)
	ag.Style, ag.DreamWhen, ag.DreamAt = agents.StyleLife, nil, nil
	store.Update(ag)
	e.a.mu.Unlock()
	fill(t, e, mainSID, 2, time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC))
	setNow(e, time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC))
	e.a.dreamTick()
	e.k.none()
	setNow(e, time.Date(2026, 10, 23, 23, 40, 0, 0, time.UTC))
	e.a.dreamTick()
	in = e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: in.SID})
	e.k.expect(proto.TTurn)
	if last := e.llm.lastReq().Messages; !strings.Contains(findMsg(last, "你要睡了"), "拟人型") {
		t.Fatal("拟人型的整理说明里要说形成看法、更新 self.md")
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusOK})
	waitIdle(t, e, in.SID)
	// 拟人型整理完，内心会话里记一笔
	e.a.mu.Lock()
	h := e.a.store.History(lifeSID("default"))
	e.a.mu.Unlock()
	if len(h) == 0 || !strings.Contains(h[len(h)-1].Content, "昨晚整理记忆") {
		t.Fatalf("内心会话里没记：%+v", h)
	}
	// 关掉
	e.a.mu.Lock()
	ag, _ = store.Get(agents.DefaultID)
	off := "off"
	ag.DreamWhen = &off
	store.Update(ag)
	e.a.mu.Unlock()
	setNow(e, time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC))
	e.a.dreamTick()
	e.k.none()
}

func waitIdle(t *testing.T, e *env, sid string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		e.a.mu.Lock()
		s := e.a.sessions[sid]
		busy := s != nil && s.run != nil
		e.a.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("这一轮一直没结束")
}
