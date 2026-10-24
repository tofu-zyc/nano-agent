package app

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ---- 思考强度 ----

func (a *App) thinkingOf(sid string) string {
	if t := a.store.Settings(sid).Thinking; t != "" {
		return t
	}
	if t := a.agentOf(sid).Thinking; t != "" {
		return t
	}
	if m, _, ok := a.modelOf(sid); ok {
		return m.Thinking
	}
	if a.models == nil { // 测试/嵌入式的旧单模型模式
		return a.cfg.LLM.Thinking
	}
	return ""
}

func thinkingName(level string) string {
	if level == "" {
		return "模型默认"
	}
	return level
}

func (a *App) cmdThink(c in, arg string) {
	levels := a.levelsOf(c.s.sid)
	cur := a.thinkingOf(c.s.sid)
	if arg == "" {
		a.answer(c, fmt.Sprintf("思考强度：%s\n可选：%s（none 表示关闭），用 /think <强度> 切换，/think default 恢复默认", thinkingName(cur), strings.Join(levels, "、")))
		return
	}
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能切换思考强度")
		return
	}
	if arg == "default" {
		arg = ""
	} else if !slices.Contains(levels, arg) {
		a.answer(c, "思考强度只能是 "+strings.Join(levels, "、")+"，或者 default")
		return
	}
	if arg == cur {
		a.answer(c, "思考强度已经是 "+arg)
		return
	}
	st := a.store.Settings(c.s.sid)
	st.Thinking = arg
	if err := a.store.SaveSettings(c.s.sid, st); err != nil {
		a.answer(c, "保存设置失败："+err.Error())
		return
	}
	a.answer(c, fmt.Sprintf("思考强度已切换为 %s，下一条消息开始生效", thinkingName(arg)))
}

// ---- 时间戳 ----

var weekdays = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// stamp 是放在用户消息前面的发送时间和来源，比如 [2026-10-21 周三 15:08 · Telegram]。
// 写进历史后就不再变，不影响前缀缓存；模型也不用调工具就知道现在几点、用户在哪个窗口说的话。
func (a *App) stamp(source string) string {
	t := a.now().In(a.loc)
	s := fmt.Sprintf("%s %s %s", t.Format("2006-01-02"), weekdays[t.Weekday()], t.Format("15:04"))
	if source != "" {
		s += " · " + source
	}
	return "[" + s + "]"
}

// ---- 过程显示 ----

// usageLine 总结一轮的用量，重点是缓存命中率：前缀没被破坏的话，多轮对话里应该一直很高。
func usageLine(r *run, now time.Time) string {
	u := r.usage
	in := fmt.Sprintf("输入 %d tokens", u.PromptTokens)
	if u.PromptTokens > 0 {
		in += fmt.Sprintf("（缓存命中 %d%%）", u.CachedTokens*100/u.PromptTokens)
	}
	out := fmt.Sprintf("输出 %d", u.CompletionTokens)
	if u.ReasoningTokens > 0 {
		out += fmt.Sprintf("（思考 %d）", u.ReasoningTokens)
	}
	return fmt.Sprintf("%d 次模型调用 · %s · %s · 用时 %.1fs", r.calls, in, out, now.Sub(r.start).Seconds())
}

// toolDetail 是工具调用的一行摘要，比如 Shell 的命令、Read 的路径
func toolDetail(name, args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return oneLine(args, 100)
	}
	str := func(k string) string { v, _ := m[k].(string); return v }
	switch name {
	case "Shell":
		return oneLine(str("command"), 120)
	case "Read":
		if off, ok := m["offset"].(float64); ok {
			return fmt.Sprintf("%s（从第 %d 行）", str("path"), int(off))
		}
		return str("path")
	case "Write":
		return fmt.Sprintf("%s（%d 字节）", str("path"), len(str("content")))
	case "Edit":
		return str("path")
	case "Grep":
		parts := []string{}
		for _, k := range []string{"pattern", "glob", "path"} {
			if v := str(k); v != "" {
				parts = append(parts, v)
			}
		}
		return oneLine(strings.Join(parts, "  "), 120)
	}
	b, _ := json.Marshal(m)
	return oneLine(string(b), 100)
}

// resultDetail 是工具结果的一行摘要：第一行内容，加上总行数
func resultDetail(content string) string {
	content = strings.TrimRight(content, "\n")
	lines := strings.Count(content, "\n") + 1
	first, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	s := oneLine(first, 80)
	if lines > 1 {
		s += fmt.Sprintf("（共 %d 行）", lines)
	}
	return s
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
