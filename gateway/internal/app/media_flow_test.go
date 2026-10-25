package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/proto"
)

// useVision 给主会话换上能看图的模型，返回“最近一次用的客户端”。
func useVision(t *testing.T, e *env) func() *recLLM {
	t.Helper()
	ms, err := models.Open(e.dir, e.a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var used *recLLM
	e.a.SetModels(ms, func(config.LLM) (llm.Client, error) {
		mu.Lock()
		defer mu.Unlock()
		used = &recLLM{}
		return used, nil
	})
	ep, _ := ms.SaveEndpoint(models.Endpoint{Name: "看图", Kind: "mock"})
	m, err := ms.SaveModel(models.Model{Endpoint: ep.ID, Name: "vision", Modalities: []string{"text", "image"}})
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	st := e.a.store.Settings(mainSID)
	st.Model = m.ID
	e.a.store.SaveSettings(mainSID, st)
	e.a.mu.Unlock()
	return func() *recLLM {
		mu.Lock()
		defer mu.Unlock()
		return used
	}
}

func withMedia(t *testing.T, e *env) *media.Store {
	t.Helper()
	store, err := media.Open(filepath.Join(e.dir, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetMedia(store)
	return store
}

// Read 读到图片：网关从那台机器上取过来（执行器的 Fetch），图片取完才请求模型，模型在工具结果后面看到图片
func TestReadImageShownToModel(t *testing.T) {
	e := setup(t)
	withMedia(t, e)
	client := useVision(t, e)
	local := e.hosts.set(host.LocalName, "local", true, 7, "")
	local.mu.Lock()
	local.result = host.Result{OK: true, Content: base64.StdEncoding.EncodeToString(pngData)}
	gate := make(chan struct{})
	local.gate = gate // 先卡住：看网关是不是等图取完才请求模型
	local.mu.Unlock()

	e.say(`!call Read {"path":"memes/a.png"}`)
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	turn := e.k.expect(proto.TTurn)
	if len(turn.Calls) != 1 || turn.Calls[0].Name != "Read" {
		t.Fatalf("turn: %+v", turn)
	}
	e.k.send(proto.Msg{T: proto.TResult, SID: mainSID, ID: turn.Calls[0].ID, OK: proto.Bool(true), Content: "/home/x/memes/a.png 是图片（PNG，67 字节）"})
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	req := local.lastReq(t)
	if req["tool"] != "Fetch" || req["args"].(map[string]any)["path"] != "/home/x/memes/a.png" {
		t.Fatalf("取图片的请求：%v", req)
	}
	e.k.none() // 图片还没取完：不请求模型
	close(gate)
	e.k.expect(proto.TTurn)
	r := client().lastReq()
	last := r.Messages[len(r.Messages)-1]
	tool := r.Messages[len(r.Messages)-2]
	if last.Role != "user" || len(last.Parts) != 2 || last.Parts[1].ImageURL == nil ||
		!strings.HasPrefix(last.Parts[1].ImageURL.URL, "data:image/png;base64,") || !strings.Contains(last.Parts[0].Text, "工具读到的图片") {
		t.Fatalf("模型没看到图片：%+v", last)
	}
	if tool.Role != "tool" || !strings.Contains(tool.Content, "图片附在下面") {
		t.Fatalf("工具结果：%+v", tool)
	}
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）")
}

// mediaCh 能发文件的渠道（像飞书、Telegram）
type mediaCh struct {
	fakeCh
	mu   sync.Mutex
	got  []media.Attachment
	data [][]byte
}

func (c *mediaCh) SendMedia(_ context.Context, _ string, att media.Attachment, data []byte) error {
	c.mu.Lock()
	c.got = append(c.got, att)
	c.data = append(c.data, data)
	c.mu.Unlock()
	c.sent <- "［收到文件］" + att.Name
	return nil
}

// SendFile：取文件、存起来、发到这一轮的聊天窗口；能发文件的渠道发文件，不能的发一行说明；记在工具结果上
func TestSendFileSendsToChat(t *testing.T) {
	e := setup(t)
	withMedia(t, e)
	local := e.hosts.set(host.LocalName, "local", true, 7, "")
	local.mu.Lock()
	local.result = host.Result{OK: true, Content: base64.StdEncoding.EncodeToString(pngData)}
	local.mu.Unlock()
	mc := &mediaCh{fakeCh: fakeCh{name: "mc", sent: make(chan string, 100)}}
	e.a.cfg.Access.Admins = append(e.a.cfg.Access.Admins, "mc:u1")
	e.a.AddChannel(mc)
	e.sayOn(mc, "发个表情包")
	in := e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TExec, SID: in.SID, ID: "f1", Name: "SendFile", Args: json.RawMessage(`{"files":["memes/doge.png"]}`)})
	res := e.k.expect(proto.TExecResult)
	if res.OK == nil || !*res.OK || !strings.Contains(res.Content, "已发送图片：doge.png") {
		t.Fatalf("SendFile 的结果：%+v", res)
	}
	mc.wait(t, "［收到文件］doge.png")
	mc.mu.Lock()
	if len(mc.data) != 1 || string(mc.data[0]) != string(pngData) || mc.got[0].MIME != "image/png" {
		t.Fatalf("发出去的文件：%+v", mc.got)
	}
	mc.mu.Unlock()
	if req := local.lastReq(t); req["tool"] != "Fetch" || req["args"].(map[string]any)["path"] != "memes/doge.png" {
		t.Fatalf("取文件的请求：%v", req)
	}
	// 工具结果记进历史时带上发出去的文件（网页显示）
	e.k.send(proto.Msg{T: proto.TResult, SID: in.SID, ID: "f1", OK: proto.Bool(true), Content: res.Content})
	waitFor(t, func() bool {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		h := e.a.store.History(in.SID)
		return len(h) > 0 && len(h[len(h)-1].Sent) == 1
	})
	e.k.send(proto.Msg{T: proto.TDone, SID: in.SID, Status: proto.StatusCancelled})

	// 不能发文件的渠道：发一行说明
	e.say("再发一个")
	in = e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TExec, SID: in.SID, ID: "f2", Name: "SendFile", Args: json.RawMessage(`{"files":["memes/doge.png"]}`)})
	e.k.expect(proto.TExecResult)
	e.ch.wait(t, "［图片：doge.png］")
}
