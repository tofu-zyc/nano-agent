package channel

import (
	"strings"
	"testing"
)

func TestTelegramMarkdownToHTML(t *testing.T) {
	cases := []struct{ md, want string }{
		{"**粗体** 和 *斜体*、~~删掉~~", "<b>粗体</b> 和 <i>斜体</i>、<s>删掉</s>"},
		{"用 `a<b>` 比较", "用 <code>a&lt;b&gt;</code> 比较"},
		{"snake_case_name 不是斜体，_这个是_", "snake_case_name 不是斜体，<i>这个是</i>"},
		{"## 标题\n正文", "<b>标题</b>\n正文"},
		{"- 一\n- [x] 完成\n  * 二", "• 一\n☑ 完成\n  • 二"},
		{"> 引用 **重点**\n> 第二行\n之后", "<blockquote>引用 <b>重点</b>\n第二行</blockquote>\n之后"},
		{"看 [文档](https://example.com/a?b=1&c=2)", `看 <a href="https://example.com/a?b=1&amp;c=2">文档</a>`},
		{"```go\nif a < b {\n}\n```\n完", "<pre><code class=\"language-go\">if a &lt; b {\n}</code></pre>\n完"},
		{"```\n还没写完", "<pre><code>还没写完</code></pre>"},
		{"**没闭合的粗体", "**没闭合的粗体"},
		{"| a | b |\n|---|---|\n| 1 | 2 |", "<pre>| a | b |\n|---|---|\n| 1 | 2 |</pre>"},
		{"a & b < c", "a &amp; b &lt; c"},
		{"---", "——————"},
		{"2 * 3 * 4", "2 * 3 * 4"},
	}
	for _, c := range cases {
		if got := tgHTML(c.md); got != c.want {
			t.Errorf("%q\n got: %q\nwant: %q", c.md, got, c.want)
		}
	}
}

func TestTelegramMarkdownChunksKeepCodeBlocks(t *testing.T) {
	md := "开头\n```py\n" + strings.Repeat("print('hello')\n", 400) + "```\n结尾"
	parts := tgMarkdownChunks(md, 3000)
	if len(parts) < 2 {
		t.Fatalf("应该切成几段：%d", len(parts))
	}
	for i, p := range parts {
		h := tgHTML(p)
		if strings.Count(h, "<pre>") != strings.Count(h, "</pre>") {
			t.Fatalf("第 %d 段标签不成对：%q", i, h[:80])
		}
		if i > 0 && !strings.HasPrefix(h, `<pre><code class="language-py">`) && i < len(parts)-1 {
			t.Fatalf("第 %d 段应当接着代码块：%q", i, h[:60])
		}
	}
	if !strings.HasSuffix(tgHTML(parts[len(parts)-1]), "结尾") {
		t.Fatal("最后一段不对")
	}
}
