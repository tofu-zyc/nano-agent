package app

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/persona"
)

// ---- /persona ----

// personaLabel 说明会话现在用的人格：默认人格还是会话专属的，原样是哪个预设。
func (a *App) personaLabel(sid string) string {
	text, path := a.persona.For(sid)
	if path == "" {
		return "没有人格文件"
	}
	label := "默认人格（default.md）"
	if filepath.Base(path) != "default.md" {
		label = "这个会话专属的人格"
	}
	if p, ok := persona.MatchPreset(text); ok {
		label += "：" + p.Name
	} else {
		label += "（改过的）"
	}
	return label
}

func (a *App) cmdPersona(c in, arg string) {
	presets := persona.Presets()
	if arg == "" {
		var b strings.Builder
		b.WriteString("这个会话用的人格：" + a.personaLabel(c.s.sid) + "\n自带的人格：\n")
		for i, p := range presets {
			fmt.Fprintf(&b, "%d. %s：%s\n", i+1, p.Name, p.Desc)
		}
		b.WriteString("/persona <编号或名字> 给这个会话换上，/persona default 改回默认人格。默认人格和人格内容在网页「人格」页里改。")
		a.answer(c, b.String())
		return
	}
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能换人格")
		return
	}
	name := persona.FileFor(c.s.sid)
	if arg == "default" || arg == "默认" {
		if err := a.persona.Remove(name); err != nil {
			a.answer(c, "没改成："+err.Error())
			return
		}
		a.publish(events.Event{T: "persona", Name: name})
		a.answer(c, "这个会话改回默认人格（"+a.personaLabel(c.s.sid)+"），下一条消息开始生效")
		return
	}
	p, ok := persona.FindPreset(arg)
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(presets) {
		p, ok = presets[n-1], true
	}
	if !ok {
		a.answer(c, "没有这个人格，用 /persona 看看有哪些")
		return
	}
	if err := a.persona.Write(name, p.Text); err != nil {
		a.answer(c, "没换成："+err.Error())
		return
	}
	a.publish(events.Event{T: "persona", Name: name})
	a.answer(c, "这个会话换上了「"+p.Name+"」，下一条消息开始生效。前面聊的内容还在上下文里，想让它彻底换个样子可以 /reset 清空。")
}
