package channel

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"nano-agent/gateway/internal/config"
)

const testJobToken = "j:00112233445566778899aabb"

func TestTelegramJobCardsOpaqueButtonsAndAckBeforeDispatch(t *testing.T) {
	var mu sync.Mutex
	var sent []map[string]any
	acked := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		mu.Lock()
		if method == "answerCallbackQuery" {
			acked = true
			if strings.Contains(body["text"].(string), "成功") {
				t.Error("premature success")
			}
		} else {
			sent = append(sent, body)
		}
		mu.Unlock()
		io.WriteString(w, `{"ok":true,"result":{"message_id":77}}`)
	}))
	defer srv.Close()
	tg, _ := NewTelegram(config.Telegram{Token: "test", APIBase: srv.URL, Proxy: "none"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	card := JobCard{JobID: "j_original-long-identity", Node: "node", Status: "运行中", Summary: "make test 中文",
		Buttons: []JobButton{{Label: "转后台", Token: testJobToken}}}
	ref, err := tg.SendJob(context.Background(), "123", card)
	if err != nil || ref != "77" {
		t.Fatal(ref, err)
	}
	card.Finished = true
	card.Status = "已完成"
	card.Buttons = nil
	if err := tg.UpdateJob(context.Background(), "123", ref, card); err != nil {
		t.Fatal(err)
	}
	got := make(chan Message, 1)
	tg.onButton(context.Background(), &tgCallbackQuery{ID: "cb", From: tgUser{ID: 7},
		Message: &tgMessage{MessageID: 77, Chat: tgChat{ID: 123, Type: "group"}}, Data: testJobToken}, func(_ Channel, m Message) {
		mu.Lock()
		defer mu.Unlock()
		if !acked {
			t.Error("operation dispatched before callback acknowledged")
		}
		got <- m
	})
	m := <-got
	if m.Action != ActionJob || m.Text != testJobToken || m.MsgID != "77" || m.ChatID != "123" || m.SenderID != "7" || !m.Group {
		t.Fatal(m)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 {
		t.Fatal(sent)
	}
	keyboard := sent[0]["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	data := keyboard[0].([]any)[0].(map[string]any)["callback_data"].(string)
	if len(data) > 64 || data != testJobToken || strings.Contains(data, "original") {
		t.Fatal(data)
	}
	if len(sent[1]["reply_markup"].(map[string]any)["inline_keyboard"].([]any)) != 0 {
		t.Fatal("completed card retained buttons")
	}
	if strings.Contains(sent[1]["text"].(string), "转后台：") {
		t.Fatal("completed card offers detach")
	}
}

func TestFeishuJobCardProjectionAndOriginalCallbackIdentity(t *testing.T) {
	f, m := mockFeishu(t, false)
	card := JobCard{JobID: "j-original", Node: "node", Status: "运行中", Summary: "command ` formatted",
		Buttons: []JobButton{{Label: "转后台", Token: testJobToken}}}
	ref, err := f.SendJob(context.Background(), "chat", card)
	if err != nil || ref != "msg-test" {
		t.Fatal(ref, err)
	}
	card.Finished = true
	card.Status = "已完成"
	card.Buttons = nil
	if err := f.UpdateJob(context.Background(), "chat", ref, card); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	if len(m.sent) != 1 || !strings.Contains(m.sent[0], testJobToken) || len(m.patched) != 1 || strings.Contains(m.patched[0], testJobToken) {
		t.Fatal(m.sent, m.patched)
	}
	m.mu.Unlock()
	ev := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Operator: &callback.Operator{OpenID: "operator"},
		Context:  &callback.Context{OpenChatID: "chat", OpenMessageID: ref},
		Action:   &callback.CallBackAction{Value: map[string]any{fsAction: ActionJob, "token": testJobToken}},
	}}
	message, toast := cardAction(ev)
	if message.Action != ActionJob || message.Text != testJobToken || message.MsgID != ref || message.SenderID != "operator" ||
		strings.Contains(toast, "成功") || !strings.Contains(toast, "收到") {
		t.Fatal(message, toast)
	}
	ev.Event.Action.Value["token"] = strings.Repeat("a", 1000)
	if m, _ := cardAction(ev); m.Action != "" {
		t.Fatal("accepted malformed token", m)
	}
}
