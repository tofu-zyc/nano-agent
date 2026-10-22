package channel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestSplit(t *testing.T) {
	if got := Split("短消息", 10); len(got) != 1 || got[0] != "短消息" {
		t.Fatalf("%q", got)
	}
	long := strings.Repeat("一二三四五六七八九十", 30) // 300 字
	parts := Split(long, 100)
	if len(parts) != 3 || strings.Join(parts, "") != long {
		t.Fatalf("parts=%d", len(parts))
	}
	// 优先在换行处切
	text := strings.Repeat("甲", 80) + "\n" + strings.Repeat("乙", 80)
	parts = Split(text, 100)
	if len(parts) != 2 || parts[0] != strings.Repeat("甲", 80) || parts[1] != strings.Repeat("乙", 80) {
		t.Fatalf("%q", parts)
	}
	for _, p := range Split(strings.Repeat("😀", 50), 7) {
		if !utf8.ValidString(p) || utf8.RuneCountInString(p) > 7 {
			t.Fatalf("bad part %q", p)
		}
	}
}

func TestStripMention(t *testing.T) {
	cases := []struct {
		in, out string
		ok      bool
	}{
		{"@NanoBot 你好", " 你好", true},
		{"你好 @nanobot", "你好 ", true},
		{"/reset@nanobot", "/reset", true},
		{"@nanobot_x 不是我", "@nanobot_x 不是我", false},
		{"email a@nanobot.com", "email a.com", true}, // 用户名后面跟 . 仍算提及，这里接受
		{"没有提及", "没有提及", false},
	}
	for _, c := range cases {
		out, ok := stripMention(c.in, "NanoBot")
		if out != c.out || ok != c.ok {
			t.Errorf("%q → %q,%v；期望 %q,%v", c.in, out, ok, c.out, c.ok)
		}
	}
}

func TestTelegramConvert(t *testing.T) {
	tg := &Telegram{botID: 99, botUser: "nanobot", log: slog.Default()}
	user := &tgUser{ID: 7, FirstName: "张", LastName: "三"}

	m, ok := tg.convert(&tgMessage{MessageID: 1, From: user, Chat: tgChat{ID: 7, Type: "private"}, Text: "hi"})
	if !ok || m.Group || !m.Mentioned || m.ChatID != "7" || m.SenderName != "张 三" {
		t.Fatalf("private: %+v", m)
	}
	m, _ = tg.convert(&tgMessage{MessageID: 2, From: user, Chat: tgChat{ID: -100, Type: "supergroup"}, Text: "大家好"})
	if !m.Group || m.Mentioned {
		t.Fatalf("group no mention: %+v", m)
	}
	m, _ = tg.convert(&tgMessage{MessageID: 3, From: user, Chat: tgChat{ID: -100, Type: "group"}, Text: "@nanobot 几点了"})
	if !m.Mentioned || m.Text != "几点了" || m.MsgID != "3" {
		t.Fatalf("group mention: %+v", m)
	}
	reply := &tgMessage{From: &tgUser{ID: 99, IsBot: true}}
	m, _ = tg.convert(&tgMessage{MessageID: 4, From: user, Chat: tgChat{ID: -100, Type: "group"}, Text: "继续", ReplyTo: reply})
	if !m.Mentioned {
		t.Fatalf("reply to bot: %+v", m)
	}
	if _, ok := tg.convert(&tgMessage{From: &tgUser{ID: 5, IsBot: true}, Chat: tgChat{Type: "private"}, Text: "x"}); ok {
		t.Fatal("bot messages should be ignored")
	}
	if _, ok := tg.convert(&tgMessage{From: user, Chat: tgChat{Type: "channel"}, Text: "x"}); ok {
		t.Fatal("channel posts should be ignored")
	}
}

func s(v string) *string { return &v }

func TestFeishuConvert(t *testing.T) {
	f := &Feishu{log: slog.Default(), botOpenID: "ou_bot", names: map[string]string{"ou_u1": "李四"}, seen: map[string]time.Time{}}
	ev := func(id, chatType, content string, mentions ...*larkim.MentionEvent) *larkim.P2MessageReceiveV1 {
		return &larkim.P2MessageReceiveV1{
			EventV2Base: &larkevent.EventV2Base{},
			Event: &larkim.P2MessageReceiveV1Data{
				Sender: &larkim.EventSender{SenderType: s("user"), SenderId: &larkim.UserId{OpenId: s("ou_u1")}},
				Message: &larkim.EventMessage{
					MessageId: s(id), ChatId: s("oc_1"), ChatType: s(chatType),
					MessageType: s("text"), Content: s(content), Mentions: mentions,
				},
			},
		}
	}
	botMention := &larkim.MentionEvent{Key: s("@_user_1"), Name: s("nano"), Id: &larkim.UserId{OpenId: s("ou_bot")}}
	other := &larkim.MentionEvent{Key: s("@_user_2"), Name: s("王五"), Id: &larkim.UserId{OpenId: s("ou_w5")}}

	m, ok := f.convert(context.Background(), ev("m1", "group", `{"text":"@_user_1 问一下 @_user_2 的事"}`, botMention, other))
	if !ok || !m.Group || !m.Mentioned || m.Text != "问一下 @王五 的事" || m.SenderName != "李四" || m.ChatID != "oc_1" {
		t.Fatalf("group mention: %+v", m)
	}
	if _, ok := f.convert(context.Background(), ev("m1", "group", `{"text":"重复投递"}`)); ok {
		t.Fatal("duplicate event should be dropped")
	}
	m, _ = f.convert(context.Background(), ev("m2", "group", `{"text":"@_user_2 不是找机器人"}`, other))
	if m.Mentioned {
		t.Fatalf("mention of someone else: %+v", m)
	}
	m, _ = f.convert(context.Background(), ev("m3", "p2p", `{"text":"私聊"}`))
	if m.Group || !m.Mentioned || m.Text != "私聊" {
		t.Fatalf("p2p: %+v", m)
	}
}

func TestFeishuExtractPost(t *testing.T) {
	post := `{"title":"标题","content":[[{"tag":"text","text":"第一行"},{"tag":"a","text":"链接","href":"https://x"}],[{"tag":"img","image_key":"k"}],[{"tag":"text","text":"第三行"}]]}`
	if got := extractText("post", post); got != "标题\n第一行链接\n\n第三行" {
		t.Fatalf("%q", got)
	}
	if got := extractText("image", `{"image_key":"k"}`); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestCLIRunView(t *testing.T) {
	var out strings.Builder
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCLI("", true, true, log)
	c.out = &out
	v := c.OpenRun("local", "")
	v.Reasoning("先看看")
	v.Reasoning("目录\n结构")
	v.EndMessage()
	v.Tool("Shell", "ls")
	v.ToolResult(true, "a.txt（共 2 行）")
	v.Content("有两个文件：\n")
	v.Content("\na.txt 和 b.txt\n")
	v.EndMessage()
	c.Send(context.Background(), "local", "（已停止）", "")
	v.Note("1 次模型调用")
	v.Close()
	v.Content("关闭之后不再显示")
	want := "思考> 先看看目录\n      结构\n" +
		"  ⚙ Shell  ls\n" +
		"    ✓ a.txt（共 2 行）\n" +
		"bot> 有两个文件：\n\n     a.txt 和 b.txt\n" +
		"bot> （已停止）\n" +
		"  1 次模型调用\n"
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}

	// 关掉思考和用量显示
	out.Reset()
	c = NewCLI("", false, false, log)
	c.out = &out
	v = c.OpenRun("local", "")
	v.Reasoning("不显示")
	v.Content("你好")
	v.Note("不显示")
	v.Close()
	if out.String() != "bot> 你好\n" {
		t.Fatalf("got %q", out.String())
	}
}

// 终端通过 unix socket 连上来：打招呼、收发消息、新终端顶掉旧终端
func TestCLISocket(t *testing.T) {
	dir, _ := os.MkdirTemp("", "nc")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "cli.sock")
	c := NewCLI(sock, false, false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	connected := make(chan struct{}, 4)
	c.OnConnect(func() { connected <- struct{}{} })
	got := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx, func(_ Channel, m Message) { got <- m.Text })

	var conn net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if conn, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(sock); st.Mode().Perm() != 0o600 {
		t.Fatalf("socket 权限：%v", st.Mode())
	}
	fmt.Fprintln(conn, ChatHello)
	<-connected
	if st := c.Status(); st.State != StateOnline {
		t.Fatalf("status: %+v", st)
	}
	fmt.Fprintln(conn, "你好")
	if m := <-got; m != "你好" {
		t.Fatalf("got %q", m)
	}
	c.Send(context.Background(), "local", "回复", "")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(conn)
	if line, _ := br.ReadString('\n'); line != "bot> 回复\n" {
		t.Fatalf("read %q", line)
	}

	// 第二个终端连上来，第一个被断开
	conn2, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	fmt.Fprintln(conn2, ChatHello+" color")
	<-connected
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "另一个终端接进来了") {
		t.Fatalf("旧终端：%q", rest)
	}
	// 不按规矩打招呼的连接直接断开
	bad, _ := net.Dial("unix", sock)
	fmt.Fprintln(bad, "hello")
	bad.SetReadDeadline(time.Now().Add(2 * time.Second))
	if msg, _ := io.ReadAll(bad); !strings.Contains(string(msg), "nano-gateway chat") {
		t.Fatalf("bad: %q", msg)
	}
	conn2.Close()
	for i := 0; i < 50 && c.Status().State != StateIdle; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if st := c.Status(); st.State != StateIdle {
		t.Fatalf("断开后：%+v", st)
	}
}

func TestCLISocketPath(t *testing.T) {
	home := t.TempDir()
	os.Chmod(home, 0o700)
	t.Setenv("NANO_HOME", home)
	if p, err := CLISocket("/srv/nano/data"); err != nil || p != filepath.Join(home, "run", "cli.sock") {
		t.Fatalf("%q %v", p, err)
	}
	run := t.TempDir()
	os.Chmod(run, 0o700)
	t.Setenv("XDG_RUNTIME_DIR", run)
	longHome := "/" + strings.Repeat("很长的目录/", 12) + "nano"
	t.Setenv("NANO_HOME", longHome)
	long := "/" + strings.Repeat("很长的目录/", 12) + "data"
	p, err := CLISocket(long)
	if err != nil || filepath.Dir(p) != run || len(p) > 100 {
		t.Fatalf("%q %v", p, err)
	}
	if p2, _ := CLISocket(long); p2 != p {
		t.Fatalf("同一个数据目录要算出同一个路径：%q %q", p, p2)
	}
	os.Chmod(run, 0o755)
	if _, err := CLISocket(long); err == nil {
		t.Fatal("别人也能进的目录不能用")
	}
}
