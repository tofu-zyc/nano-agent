package app

// 拟人型的内心活动（docs/DESIGN-v0.6-living-agent.md「拟人型：内心活动」）。

import (
	"time"

	"nano-agent/gateway/internal/session"
)

// innerLimits 是 agent 自己的一轮的上限：整理记忆要读很多日记、写很多条，内心活动只是醒一会儿。
func innerLimits(sid string) (steps int, limit time.Duration) {
	if isDreamSID(sid) {
		return 150, 40 * time.Minute
	}
	return 12, 10 * time.Minute
}

// innerDone 是 agent 自己的一轮结束了。调用方持有锁。
func (a *App) innerDone(s *sess, r *run, status, body string) {
	if isDreamSID(s.sid) {
		a.dreamDone(s, status, body)
	}
}

// lifeHeard 把一件事记进 agent 的内心会话（比如昨晚整理了记忆），它下次醒来时会看到。调用方持有锁。
func (a *App) lifeHeard(agentID, text string) {
	if err := a.store.Append(lifeSID(agentID), session.Msg{Role: "user", Content: "[系统] " + text}); err != nil {
		a.log.Warn("记进内心会话失败", "agent", agentID, "err", err)
	}
}

// agentLoop 每 15 秒看一遍：哪些 agent 该整理记忆了（以后还有内心活动）。
func (a *App) agentLoop() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			a.dreamTick()
		}
	}
}
