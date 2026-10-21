package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// otherCh 是另一个渠道上的管理员窗口 other:x
func (e *env) otherCh() *fakeCh {
	e.a.cfg.Access.Admins = append(e.a.cfg.Access.Admins, "other:u9")
	ch := &fakeCh{name: "other", sent: make(chan string, 100)}
	e.a.AddChannel(ch)
	return ch
}

func sayFrom(a *App, ch channel.Channel, chat, sender, text string) {
	a.OnMessage(ch, channel.Message{ChatID: chat, SenderID: sender, SenderName: "李四", ChatName: "李四", Text: text, Mentioned: true})
}

func (c *fakeCh) none(t *testing.T) {
	t.Helper()
	select {
	case m := <-c.sent:
		t.Fatalf("不该收到：%q", m)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAdminWindowsShareMainSession(t *testing.T) {
	e := setup(t)
	other := e.otherCh()
	e.say("我在 fake")
	e.k.expect(proto.TInput)
	e.roundTrip(t)

	sayFrom(e.a, other, "x", "u9", "我在 other")
	in := e.k.expect(proto.TInput)
	if in.SID != mainSID || in.From != "other:u9" {
		t.Fatalf("input: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	// 两个窗口说的话在同一段上下文里，时间戳里写着来源
	var users []string
	for _, m := range e.llm.lastReq().Messages {
		if m.Role == "user" {
			users = append(users, m.Content)
		}
	}
	if len(users) != 2 || !strings.HasSuffix(users[0], "· fake] 我在 fake") || users[1] != "[2026-10-21 周三 15:08 · other] 我在 other" {
		t.Fatalf("users: %q", users)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	// 回复只发回说话的窗口
	other.wait(t, "（mock）收到：我在 other")
	e.ch.none(t)
}

func TestGuestHasOwnSession(t *testing.T) {
	e := setup(t)
	e.say("管理员的话")
	e.k.expect(proto.TInput)
	e.roundTrip(t)

	sayFrom(e.a, e.ch, "c2", "u2", "访客的话")
	in := e.k.expect(proto.TInput)
	if in.SID != "fake:c2" || in.Mode != proto.ModeNone {
		t.Fatalf("input: %+v", in)
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: "fake:c2"})
	e.k.expect(proto.TTurn)
	for _, m := range e.llm.lastReq().Messages {
		if strings.Contains(m.Content, "管理员的话") {
			t.Fatalf("访客看到了主会话的内容：%q", m.Content)
		}
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: "fake:c2", Status: proto.StatusOK})
	e.ch.wait(t, "收到：访客的话")

	if info, ok := e.a.Session("fake:c2"); !ok || info.Title != "李四" || len(info.Endpoints) != 1 {
		t.Fatalf("info: %+v", info)
	}
	// 主会话一直接着网页
	if info, _ := e.a.Session(mainSID); len(info.Endpoints) != 2 || info.Endpoints[0].Key != "fake:c1" || info.Endpoints[1].Key != "web" {
		t.Fatalf("main: %+v", info.Endpoints)
	}
	sayFrom(e.a, e.ch, "c2", "u2", "/session 1")
	e.ch.wait(t, "只有管理员能切换会话")
	sayFrom(e.a, e.ch, "c2", "u2", "/session")
	if msg := e.ch.wait(t, "当前会话"); strings.Contains(msg, "主会话") {
		t.Fatalf("访客不该看到别的会话：%q", msg)
	}
}

func TestSessionCommand(t *testing.T) {
	e := setup(t)
	e.say("/session")
	if msg := e.ch.wait(t, "这个窗口接在会话「主会话」上"); !strings.Contains(msg, "1. 主会话") {
		t.Fatalf("list: %q", msg)
	}
	e.say("/session new 调试")
	e.ch.wait(t, "已切换到会话「调试」")
	e.say("在调试里")
	in := e.k.expect(proto.TInput)
	if !strings.HasPrefix(in.SID, "s-") {
		t.Fatalf("input: %+v", in)
	}
	e.sid = in.SID
	e.roundTrip(t)

	e.say("/session 1")
	e.ch.wait(t, "已切换到会话「主会话」")
	e.say("/session 9")
	e.ch.wait(t, "没有这个会话")
	e.say("/session 调试") // 也可以用名字
	e.ch.wait(t, "已切换到会话「调试」")

	// 接线存在数据目录里，重启后还在
	store, _ := session.New(e.a.store.Dir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a2 := New(context.Background(), e.a.cfg, e.llm, store, persona.New(e.a.cfg.Persona.Dir, log), log)
	if r := a2.eps["fake:c1"]; r == nil || r.Session != e.sid {
		t.Fatalf("after restart: %+v", r)
	}
	if h := store.History(e.sid); len(h) != 2 {
		t.Fatalf("history: %+v", h)
	}
}

// watchCh 像命令行：能流式显示，也能同步显示同一个会话里别的窗口的消息
type watchCh struct {
	streamCh
	echoes chan string
}

func (w *watchCh) Echo(_, via, text string) { w.echoes <- via + "|" + text }

func TestWatcherMirrorsOtherWindows(t *testing.T) {
	e := setup(t)
	cli := &watchCh{streamCh: streamCh{fakeCh: fakeCh{name: "cli", sent: make(chan string, 100)}}, echoes: make(chan string, 10)}
	e.a.AddChannel(cli)
	sayFrom(e.a, cli, "local", "local", "/session") // 接上主会话
	cli.wait(t, "主会话")

	e.say("从 fake 来")
	e.k.expect(proto.TInput)
	select {
	case got := <-cli.echoes:
		if got != "fake · 张三|从 fake 来" {
			t.Fatalf("echo: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("命令行没有同步显示别的窗口的消息")
	}
	e.roundTrip(t)
	deadline := time.Now().Add(3 * time.Second)
	for {
		ev := cli.snapshot()
		if n := len(ev); n > 0 && ev[n-1].kind == "close" {
			if ev[0].kind != "content" || ev[0].text != "（mock）收到：从 fake 来" {
				t.Fatalf("events: %+v", ev)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("没等到 close：%+v", cli.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cli.none(t) // 已经流式显示过了，不会再整条发一遍
}

func TestStopFromAnotherWindow(t *testing.T) {
	e := setup(t)
	other := e.otherCh()
	e.say("长任务")
	e.k.expect(proto.TInput)
	sayFrom(e.a, other, "x", "u9", "/stop")
	e.k.expect(proto.TCancel)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusCancelled})
	e.ch.wait(t, "（已停止）")
	other.wait(t, "（已停止）")
}

func TestWebSessionsAndRewiring(t *testing.T) {
	e := setup(t)
	if err := e.a.SetEndpoint("fake:c1", mainSID); err == nil {
		t.Fatal("还没说过话的窗口不能改接")
	}
	e.say("/whoami")
	e.ch.wait(t, "还没有接到会话")
	e.say("/session")
	e.ch.wait(t, "主会话")

	info, err := e.a.NewSession("网页新建")
	if err != nil || !strings.HasPrefix(info.SID, "s-") || info.Title != "网页新建" {
		t.Fatalf("new: %+v %v", info, err)
	}
	if err := e.a.SetEndpoint("fake:c1", "s-nope"); err == nil {
		t.Fatal("不存在的会话")
	}
	if err := e.a.SetEndpoint("fake:c1", info.SID); err != nil {
		t.Fatal(err)
	}
	e.ch.wait(t, "改接到会话「网页新建」")
	e.say("你好")
	if in := e.k.expect(proto.TInput); in.SID != info.SID {
		t.Fatalf("input: %+v", in)
	}
	e.sid = info.SID
	e.roundTrip(t)
	// 网页里新建的会话也接着网页
	if got, _ := e.a.Session(info.SID); len(got.Endpoints) != 2 || got.Channel != "fake" || got.Endpoints[1].Key != "web" {
		t.Fatalf("info: %+v", got)
	}

	if err := e.a.DeleteSession(mainSID); err == nil {
		t.Fatal("主会话不能删")
	}
	if err := e.a.DeleteSession(info.SID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.a.Session(info.SID); ok {
		t.Fatal("删掉的会话还在")
	}
	// 接着它的窗口按默认规则重新接：管理员回到主会话
	e.say("回来了")
	if in := e.k.expect(proto.TInput); in.SID != mainSID {
		t.Fatalf("input: %+v", in)
	}
}

func TestWebJoinsSessionItTalksIn(t *testing.T) {
	e := setup(t)
	web := channel.NewWeb()
	e.a.AddChannel(web)
	e.say("/session new 终端开的")
	e.ch.wait(t, "已切换到会话「终端开的」")
	sid := e.a.eps["fake:c1"].Session
	if info, _ := e.a.Session(sid); len(info.Endpoints) != 1 {
		t.Fatalf("网页还没在这里说过话：%+v", info.Endpoints)
	}
	e.a.OnMessage(web, channel.Message{ChatID: "browser", SenderID: "admin", Session: sid, Text: "网页也来说一句", Mentioned: true})
	if in := e.k.expect(proto.TInput); in.SID != sid || in.From != "web:admin" {
		t.Fatalf("input: %+v", in)
	}
	if info, _ := e.a.Session(sid); len(info.Endpoints) != 2 || info.Endpoints[1].Key != "web" {
		t.Fatalf("网页说过话之后应该接着：%+v", info.Endpoints)
	}
}

func TestMigrateOldCLISession(t *testing.T) {
	dir := t.TempDir()
	old, _ := session.New(dir)
	old.SaveSettings("cli:local", session.Settings{Thinking: "high", Title: "命令行"})
	old.Append("cli:local", session.Msg{Role: "user", Content: "旧消息"})

	store, _ := session.New(dir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(context.Background(), &config.Config{}, nil, store, persona.New(filepath.Join(dir, "p"), log), log)
	if st := a.store.Settings(mainSID); st.Thinking != "high" || st.Title != "" {
		t.Fatalf("settings: %+v", st)
	}
	if h := a.store.History(mainSID); len(h) != 1 || h[0].Content != "旧消息" {
		t.Fatalf("history: %+v", h)
	}
	if store.Exists("cli:local") {
		t.Fatal("旧会话还在")
	}
}
