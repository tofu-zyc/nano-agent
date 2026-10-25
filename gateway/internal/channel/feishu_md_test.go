package channel

import (
	"encoding/json"
	"testing"
)

func TestFeishuMarkdown(t *testing.T) {
	in := "# 标题\n正文 **加粗**\n## 小节 ##\n```\n# 代码里的不动\n```\n- 列表"
	want := "**标题**\n正文 **加粗**\n**小节**\n```\n# 代码里的不动\n```\n- 列表"
	if got := feishuMD(in); got != want {
		t.Fatalf("feishuMD:\n%s", got)
	}
	var post map[string]struct {
		Content [][]map[string]string `json:"content"`
	}
	if err := json.Unmarshal([]byte(feishuPost("**你好**")), &post); err != nil {
		t.Fatal(err)
	}
	if p := post["zh_cn"].Content; len(p) != 1 || len(p[0]) != 1 || p[0][0]["tag"] != "md" || p[0][0]["text"] != "**你好**" {
		t.Fatalf("post: %+v", post)
	}
}
