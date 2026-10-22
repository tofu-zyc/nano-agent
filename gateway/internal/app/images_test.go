package app

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/proto"
)

// 1x1 的 PNG
var pngData = func() []byte {
	b, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		panic(err)
	}
	return b
}()

func (e *env) sayImage(text string, atts ...media.Attachment) {
	e.a.OnMessage(e.ch, channel.Message{ChatID: "c1", SenderID: "u1", SenderName: "张三",
		Text: text, MsgID: "m1", Mentioned: true, Images: atts})
}

// 纯文字模型收到图片：图片留在历史里，只给模型一个文字说明，并提醒用户换模型。
func TestImagesPlaceholderWithoutVision(t *testing.T) {
	e := setup(t)
	store, err := media.Open(filepath.Join(e.dir, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetMedia(store)
	att, err := store.Save(pngData, "a.png", "web")
	if err != nil {
		t.Fatal(err)
	}

	e.sayImage("看图", att)
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.ch.wait(t, "不能接收图片")
	e.ch.wait(t, "（mock）") // 等这一轮真正结束（别让临时目录在写完之前被删掉）

	req := e.llm.lastReq()
	last := req.Messages[len(req.Messages)-1]
	if len(last.Parts) != 0 || !strings.Contains(last.Content, "[图片×1]") {
		t.Fatalf("纯文字模型收到的内容：%+v", last)
	}
	h := e.a.store.History(mainSID)
	if len(h) == 0 || len(h[0].Images) != 1 || h[0].Images[0].ID != att.ID {
		t.Fatalf("历史里的图片：%+v", h)
	}
}

// 支持图片的模型：图片做成 OpenAI 的 content 数组发给它；只有图片没有文字也能发。
func TestImagesSentToVisionModel(t *testing.T) {
	e := setup(t)
	store, err := media.Open(filepath.Join(e.dir, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetMedia(store)
	att, err := store.Save(pngData, "a.png", "web")
	if err != nil {
		t.Fatal(err)
	}
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
	ep, err := ms.SaveEndpoint(models.Endpoint{Name: "看图", Kind: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ms.SaveModel(models.Model{Endpoint: ep.ID, Name: "vision", Modalities: []string{"text", "image"}})
	if err != nil {
		t.Fatal(err)
	}
	st := e.a.store.Settings(mainSID)
	st.Model = m.ID
	e.a.store.SaveSettings(mainSID, st)

	e.sayImage("", att) // 只有图片
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）")

	mu.Lock()
	client := used
	mu.Unlock()
	if client == nil {
		t.Fatal("没有用会话选的模型")
	}
	req := client.lastReq()
	last := req.Messages[len(req.Messages)-1]
	parts := last.Parts
	if len(parts) == 0 || parts[len(parts)-1].ImageURL == nil {
		t.Fatalf("vision 模型收到的内容：%+v", last)
	}
	if !strings.HasPrefix(parts[len(parts)-1].ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("图片 URL：%q", parts[len(parts)-1].ImageURL.URL)
	}
}
