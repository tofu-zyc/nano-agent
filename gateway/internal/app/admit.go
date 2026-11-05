package app

import (
	"time"
)

// admitRun 的几种不通过的原因。调用方按这些常量认出是哪一种，再用自己的说法和延后时间。
const (
	admitBusy     = "会话正在使用"
	admitNoModel  = "还没有配置模型"
	admitNoKernel = "没有在线的内核"
	admitOver     = "场景的额度用完了"
)

// admitRun 是启动一轮之前的共同检查。各种承诺按结果决定自己怎么办（延后多久、记失败还是等下次），
// 这些规则仍然各自保留。作息、每日次数、自主预算、暂停属于自主活动自己的规则，仍在 autonomy.Trigger.CanStart 里。
// 按顺序检查：
//  1. 会话在跑、在排队或在整理上下文；
//  2. 没有配置模型；
//  3. 这个会话的工作目录没有在线内核；
//  4. 这个会话所在的场景额度用完了。
//
// retry 是建议的延后时间（调用方现有的延后时间优先，这只是默认值）：会话忙 2 分钟，没模型 30 分钟，
// 没内核 5 分钟，额度用完就等到额度恢复（下一个本地自然日开始）。调用方持有锁。
func (a *App) admitRun(sid string) (ok bool, why string, retry time.Duration) {
	if s := a.sessions[sid]; s != nil && (s.run != nil || len(s.queue) > 0 || s.compacting) {
		return false, admitBusy, 2 * time.Minute
	}
	if a.models != nil && len(a.models.Models()) == 0 {
		return false, admitNoModel, 30 * time.Minute
	}
	if a.route(a.workspaceOf(sid)).kernel == "" {
		return false, admitNoKernel, 5 * time.Minute
	}
	if sc, ok := a.sceneOf(sid); ok && a.sceneOver(sc) {
		now := a.now().In(a.loc)
		next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, a.loc).AddDate(0, 0, 1)
		return false, admitOver, max(next.Sub(now), time.Minute)
	}
	return true, "", 0
}
