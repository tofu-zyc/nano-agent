package app

import (
	"nano-agent/gateway/internal/channel"
	"strings"
)

func (a *App) showOf(ep endpoint) string {
	if ep.ch == "web" {
		return "详细"
	}
	if rec := a.eps[ep.key()]; rec != nil && rec.Show != "" {
		return rec.Show
	}
	if ep.ch == "tg" && a.cfg.Telegram.Show != "" {
		return a.cfg.Telegram.Show
	}
	if ep.ch == "feishu" && a.cfg.Feishu.Show != "" {
		return a.cfg.Feishu.Show
	}
	if a.cfg.Channels.Show != "" {
		return a.cfg.Channels.Show
	}
	return "过程"
}
func (a *App) filterView(ep endpoint, v channel.RunView) channel.RunView {
	return channel.FilterView(v, a.showOf(ep))
}
func (a *App) cmdShow(c in, arg string) {
	if arg == "" {
		a.answer(c, "当前显示："+a.showOf(c.ep)+"。/show 详细|过程|简洁，/show default 跟随默认")
		return
	}
	if c.ep.ch == "web" {
		a.answer(c, "网页始终显示全部过程")
		return
	}
	if arg == "default" {
		arg = ""
	} else if !strings.Contains("|详细|过程|简洁|", "|"+arg+"|") {
		a.answer(c, "显示档位只能是详细、过程或简洁")
		return
	}
	rec := a.eps[c.ep.key()]
	old := rec.Show
	rec.Show = arg
	if err := a.saveEndpoints(); err != nil {
		rec.Show = old
		a.answer(c, "保存显示设置失败："+err.Error())
		return
	}
	a.answer(c, "显示档位已设为"+a.showOf(c.ep)+"，下一轮生效")
}
