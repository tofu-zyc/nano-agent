package app

import "nano-agent/gateway/internal/ticket"

// 会话里进过不可信的话（设计 §16.4）：不在名单里的人写的东西能让分支拼出一个 prompt，叫外部 agent 拿着登录凭据去联网。
// 按会话记（Settings.Untrusted），不按这一条消息：议题正文是第一次点名时带上的，后面的点名不再带，但它还在会话的历史里；
// 分支又带着派活之前的上下文。记了就不清，只在会话清空重开时清。

// outsiderSpoke 是团队群里不在名单里的人（也不是管理员）说话了：他们能和 agent 说话，只是派不了活，
// 但这些话会进上下文。不是团队群（没设团队规则）没有名单，不记。调用方持有锁。
func (a *App) outsiderSpoke(sid, who string) bool {
	team := a.teamOf(sid)
	return team != nil && !a.canDispatch(team, who)
}

// markUntrusted 记下这个会话里进过不可信的话。已经记过的不再写盘。调用方持有锁。
func (a *App) markUntrusted(sid string) {
	st := a.store.Settings(sid)
	if st.Untrusted {
		return
	}
	st.Untrusted = true
	if err := a.store.SaveSettings(sid, st); err != nil {
		// 记不下来就当作没记：下一条不可信的话还会再试；工单建的时候读的是已落盘的值
		a.log.Warn("记下会话里进过不可信的话失败", "sid", sid, "err", err)
	}
}

// ticketUntrusted 是这张工单里 External 要不要每次都问：授权记录里记了，或者干活的分支自己的会话标着
// （分支是派活之后才分出来的，那一段里不在名单里的人又说了话也算）。调用方持有锁。
func (a *App) ticketUntrusted(t ticket.Ticket) bool {
	return t.Auth.Untrusted || t.RunSID != "" && a.store.Settings(t.RunSID).Untrusted
}
