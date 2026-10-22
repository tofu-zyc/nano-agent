package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

var pngData = func() []byte {
	b, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		panic(err)
	}
	return b
}()

func (c *client) upload(name string, data []byte) (int, map[string]any) {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		c.t.Fatal(err)
	}
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", c.srv.URL+"/api/uploads", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Nano", "1")
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// 网页上传图片、按 ID 取回来；不是图片拒收；发消息可以带图片。
func TestImageUploadAndChat(t *testing.T) {
	srv, _, _ := setup(t)
	c := newClient(t, srv)
	// 没登录不能上传
	if code, _ := c.upload("a.png", pngData); code != http.StatusUnauthorized {
		t.Fatalf("未登录上传：%d", code)
	}
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)

	code, att := c.upload("a.png", pngData)
	if code != 200 {
		t.Fatalf("上传：%d %v", code, att)
	}
	id, _ := att["id"].(string)
	if len(id) != 64 || att["mime"] != "image/png" {
		t.Fatalf("附件：%v", att)
	}
	// 取回来
	resp, err := c.hc.Get(srv.URL + "/api/uploads/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/png") {
		t.Fatalf("取图片：%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// 不是图片
	if code, _ := c.upload("x.txt", []byte("hello")); code != http.StatusBadRequest {
		t.Fatalf("不是图片：%d", code)
	}
	if code, _ := c.do("GET", "/api/uploads/"+strings.Repeat("a", 64), nil, false); code != http.StatusNotFound {
		t.Fatalf("不存在的图片：%d", code)
	}

	// 发消息带图片（没有内核时这一轮不会真的跑，但消息会被接收）
	code, _ = c.do("POST", "/api/chat", map[string]any{"sid": "main", "text": "看图", "images": []map[string]string{{"id": id, "name": "a.png"}}}, true)
	if code != 200 {
		t.Fatalf("带图片发消息：%d", code)
	}
	// 不存在的图片 ID
	if code, _ := c.do("POST", "/api/chat", map[string]any{"sid": "main", "text": "看图", "images": []map[string]string{{"id": strings.Repeat("b", 64)}}}, true); code != http.StatusBadRequest {
		t.Fatalf("不存在的图片：%d", code)
	}
}
