package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 有图片时 content 是 OpenAI 的多模态数组，纯文字时还是字符串。
func TestMessageContentParts(t *testing.T) {
	b, err := json.Marshal(Message{Role: "user", Content: "看这个",
		Parts: []Part{TextPart("看这个"), ImagePart("data:image/png;base64,AAAA")}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	parts, ok := m["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content：%s", b)
	}
	p1, _ := parts[1].(map[string]any)
	if p1["type"] != "image_url" {
		t.Fatalf("图片部分：%v", p1)
	}
	img, _ := p1["image_url"].(map[string]any)
	if img["url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("图片 URL：%v", img)
	}

	b, err = json.Marshal(Message{Role: "assistant", Content: "好的"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"content":"好的"`) {
		t.Fatalf("纯文字消息的 content 应该是字符串：%s", b)
	}
}

// 图片真的按 OpenAI 的 content 数组发到了接口上。
func TestMultimodalRequestOverHTTP(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		reply(w, "stop", map[string]any{"content": "看到了"})
	})
	res, err := c.Chat(context.Background(), Request{Messages: []Message{{
		Role: "user", Content: "看图",
		Parts: []Part{TextPart("看图"), ImagePart("data:image/png;base64,AAAA")},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "看到了" {
		t.Fatalf("回复：%+v", res)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages：%v", got["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	arr, ok := first["content"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("content 应该是数组：%v", first["content"])
	}
	imgPart, _ := arr[1].(map[string]any)
	img, _ := imgPart["image_url"].(map[string]any)
	if imgPart["type"] != "image_url" || img["url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("图片部分：%v", imgPart)
	}
}
