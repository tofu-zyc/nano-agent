package channel

import (
	"html"
	"regexp"
	"strings"
)

// Telegram 不认 Markdown 原文（会显示一堆 ** 和 `），把模型写的 Markdown 转成它支持的 HTML 子集：
// b、i、s、code、pre、a、blockquote。标题变粗体，列表换成 •，表格放进等宽的 pre。
// 回复是边生成边转的，没写完的代码块、粗体也要能转成合法的 HTML。

var (
	reFence   = regexp.MustCompile("^\\s*(```|~~~)\\s*([\\w+#.-]*)\\s*$")
	reHeading = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	reBullet  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	reTask    = regexp.MustCompile(`^\[( |x|X)\]\s+`)
	reRule    = regexp.MustCompile(`^\s{0,3}([-*_])(\s*[-*_]){2,}\s*$`)
	reTable   = regexp.MustCompile(`^\s*\|.*\|\s*$`)
)

// tgHTML 把 Markdown 转成 Telegram 的 HTML。
func tgHTML(md string) string {
	var b strings.Builder
	lines := strings.Split(md, "\n")
	inFence, fenceMark, fenceLines := false, "", 0
	var quote, table []string
	flushQuote := func() {
		if len(quote) > 0 {
			b.WriteString("<blockquote>" + strings.Join(quote, "\n") + "</blockquote>\n")
			quote = nil
		}
	}
	flushTable := func() {
		if len(table) > 0 {
			b.WriteString("<pre>" + html.EscapeString(strings.Join(table, "\n")) + "</pre>\n")
			table = nil
		}
	}
	for i, line := range lines {
		last := i == len(lines)-1
		if inFence {
			if m := reFence.FindStringSubmatch(line); m != nil && m[1] == fenceMark && m[2] == "" {
				b.WriteString("</code></pre>")
				if !last {
					b.WriteString("\n")
				}
				inFence = false
				continue
			}
			if fenceLines > 0 {
				b.WriteString("\n") // 代码块里的换行写在行前面，结尾不多出一个空行
			}
			b.WriteString(html.EscapeString(line))
			fenceLines++
			continue
		}
		if m := reFence.FindStringSubmatch(line); m != nil {
			flushQuote()
			flushTable()
			inFence, fenceMark, fenceLines = true, m[1], 0
			if m[2] != "" {
				b.WriteString(`<pre><code class="language-` + html.EscapeString(m[2]) + `">`)
			} else {
				b.WriteString("<pre><code>")
			}
			continue
		}
		if reTable.MatchString(line) {
			flushQuote()
			table = append(table, strings.TrimSpace(line))
			continue
		}
		flushTable()
		if rest, ok := strings.CutPrefix(strings.TrimLeft(line, " "), ">"); ok {
			quote = append(quote, tgInline(strings.TrimPrefix(rest, " ")))
			continue
		}
		flushQuote()
		var out string
		switch {
		case reRule.MatchString(line):
			out = "——————"
		case reHeading.MatchString(line):
			out = "<b>" + tgInline(reHeading.FindStringSubmatch(line)[1]) + "</b>"
		case reBullet.MatchString(line):
			m := reBullet.FindStringSubmatch(line)
			item := m[2]
			mark := "•"
			if t := reTask.FindStringSubmatch(item); t != nil {
				mark = "☐"
				if t[1] != " " {
					mark = "☑"
				}
				item = item[len(t[0]):]
			}
			out = strings.Repeat(" ", len(m[1])) + mark + " " + tgInline(item)
		default:
			out = tgInline(line)
		}
		b.WriteString(out)
		if !last {
			b.WriteString("\n")
		}
	}
	flushQuote()
	flushTable()
	if inFence {
		b.WriteString("</code></pre>") // 还没写完的代码块
	}
	return strings.TrimRight(b.String(), "\n")
}

// tgInline 处理一行里的行内格式：`代码`、**粗体**、*斜体*、~~删除线~~、[链接](url)。
// 没闭合的标记原样保留（转义后），保证输出的标签总是成对的。
func tgInline(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		switch {
		case s[0] == '`':
			if j := strings.IndexByte(s[1:], '`'); j >= 0 {
				b.WriteString("<code>" + html.EscapeString(s[1:1+j]) + "</code>")
				s = s[j+2:]
				continue
			}
		case strings.HasPrefix(s, "**") || strings.HasPrefix(s, "__"):
			if j := strings.Index(s[2:], s[:2]); j > 0 {
				b.WriteString("<b>" + tgInline(s[2:2+j]) + "</b>")
				s = s[j+4:]
				continue
			}
		case strings.HasPrefix(s, "~~"):
			if j := strings.Index(s[2:], "~~"); j > 0 {
				b.WriteString("<s>" + tgInline(s[2:2+j]) + "</s>")
				s = s[j+4:]
				continue
			}
		case s[0] == '*' || (s[0] == '_' && (b.Len() == 0 || !wordByte(lastByte(&b)))):
			// 斜体：*x* 或 _x_（下划线要在词的边界上，不然 snake_case 会被当成斜体）
			mark := s[:1]
			if j := strings.Index(s[1:], mark); j > 0 && s[1] != ' ' && s[j] != ' ' {
				end := 1 + j + 1
				if mark == "*" || end >= len(s) || !wordByte(s[end]) {
					b.WriteString("<i>" + tgInline(s[1:1+j]) + "</i>")
					s = s[end:]
					continue
				}
			}
		case s[0] == '[':
			if m := reLink.FindStringSubmatch(s); m != nil {
				b.WriteString(`<a href="` + html.EscapeString(m[2]) + `">` + tgInline(m[1]) + "</a>")
				s = s[len(m[0]):]
				continue
			}
		}
		// 普通字符：一直到下一个可能的标记
		n := strings.IndexAny(s[1:], "`*_~[")
		if n < 0 {
			n = len(s)
		} else {
			n++
		}
		b.WriteString(html.EscapeString(s[:n]))
		s = s[n:]
	}
	return b.String()
}

var reLink = regexp.MustCompile(`^\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)

// wordByte：ASCII 的字母、数字、下划线（中文和标点算边界，“，_斜体_”能识别）
func wordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func lastByte(b *strings.Builder) byte {
	s := b.String()
	return s[len(s)-1]
}

// tgMarkdownChunks 把 Markdown 按长度切成几段（尽量在换行处），每段单独转换；
// 切在代码块中间时，下一段补上代码块的开头，上一段由转换自动闭合。
func tgMarkdownChunks(md string, limit int) []string {
	parts := telegramChunks(md, limit)
	if len(parts) < 2 {
		return parts
	}
	// 在换行处重新切，并记住每段结束时是不是在代码块里
	var out []string
	rest := md
	open := ""
	for rest != "" {
		chunk := rest
		if c := telegramChunks(rest, limit); len(c) > 1 {
			chunk = c[0]
			if i := strings.LastIndex(chunk, "\n"); i > len(chunk)*3/5 {
				chunk = chunk[:i+1]
			}
		}
		rest = rest[len(chunk):]
		out = append(out, open+chunk)
		for _, line := range strings.Split(chunk, "\n") {
			if m := reFence.FindStringSubmatch(line); m != nil {
				if open == "" {
					open = m[1] + m[2] + "\n"
				} else if m[2] == "" {
					open = ""
				}
			}
		}
	}
	return out
}
