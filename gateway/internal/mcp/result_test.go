package mcp

import (
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 文字和 structuredContent 都要保留：Cua Driver 的动作效果 / 拒绝原因 / 建议在结构化结果里，
// 只拿文字会把“调用正常返回”误当成“操作成功”。
func TestResultTextPreservesStructuredContent(t *testing.T) {
	res := &sdkmcp.CallToolResult{
		Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: "clicked"}},
		StructuredContent: map[string]any{"verified": false, "effect": "unverifiable", "path": "ax"},
	}
	text := ResultText(res)
	for _, want := range []string{"clicked", "[结构化结果]", `"verified":false`, `"effect":"unverifiable`} {
		if !strings.Contains(text, want) {
			t.Fatalf("结构化结果没有保留（缺 %q）：%q", want, text)
		}
	}
	// 只有 structuredContent
	only := ResultText(&sdkmcp.CallToolResult{StructuredContent: map[string]any{"ok": true}})
	if !strings.Contains(only, `"ok":true`) {
		t.Fatalf("只有结构化结果时丢了：%q", only)
	}
	// 只有文字
	plain := ResultText(&sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "hello"}}})
	if plain != "hello" {
		t.Fatalf("纯文字不该被改动：%q", plain)
	}
	// 空结构不写噪声
	empty := ResultText(&sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "x"}}, StructuredContent: map[string]any{}})
	if strings.Contains(empty, "结构化结果") {
		t.Fatalf("空结构不该写进去：%q", empty)
	}
	if ResultText(nil) == "" {
		t.Fatal("nil 也要有说明")
	}
}
