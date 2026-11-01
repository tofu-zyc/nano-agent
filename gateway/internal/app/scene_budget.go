package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"nano-agent/gateway/internal/scene"
)

// 场景的预算池：一个场景所有成员会话的用量（输入加输出 token）记在一起，包括 observe 的那几轮。
// 状态存在 data/runtime/scenes.json，键是场景的键（room:<id> 或外部群的会话 ID）。
// 用量在一轮结束时才累加，一轮可能略微超出；崩溃时可能丢掉最后一轮的累加，可以接受。

// sceneUse 是一个场景的用量。
type sceneUse struct {
	Day       string `json:"day"`                  // 当天日期
	DayTokens int    `json:"day_tokens"`           // 当天用量
	Total     int    `json:"total"`                // 这一场的累计
	Since     int64  `json:"since,omitempty"`      // 开始计数的时间（秒）
	NoticeDay string `json:"notice_day,omitempty"` // 外部群超额后最近一次回说明的日期
}

// loadSceneUses 读用量文件；不存在或损坏都从空开始（损坏时留日志，下次保存会覆盖）。
func (a *App) loadSceneUses(path string) {
	a.sceneUsePath = path
	a.sceneUses = map[string]*sceneUse{}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, &a.sceneUses); err != nil || a.sceneUses == nil {
		a.log.Warn("场景用量文件读不了，从零开始", "path", path, "err", err)
		a.sceneUses = map[string]*sceneUse{}
	}
}

// saveSceneUses 整份写临时文件再替换。调用方持有锁。
func (a *App) saveSceneUses() {
	if a.sceneUsePath == "" {
		return
	}
	b, err := json.MarshalIndent(a.sceneUses, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(a.sceneUsePath), 0o700)
	}
	if err == nil {
		tmp := a.sceneUsePath + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, a.sceneUsePath)
		}
	}
	if err != nil {
		a.log.Warn("保存场景用量失败", "err", err)
	}
}

// sceneUseOf 是场景当前的用量（跨天归零当天的部分）。调用方持有锁。
func (a *App) sceneUseOf(key string) *sceneUse {
	if a.sceneUses == nil {
		a.sceneUses = map[string]*sceneUse{}
	}
	u := a.sceneUses[key]
	if u == nil {
		u = &sceneUse{}
		a.sceneUses[key] = u
	}
	if day := a.now().In(a.loc).Format("2006-01-02"); u.Day != day {
		u.Day, u.DayTokens = day, 0
	}
	return u
}

// sceneUseView 是场景当前用量的副本，不会为没用过的场景建条目。调用方持有锁。
func (a *App) sceneUseView(key string) sceneUse {
	var u sceneUse
	if p := a.sceneUses[key]; p != nil {
		u = *p
	}
	if day := a.now().In(a.loc).Format("2006-01-02"); u.Day != day {
		u.Day, u.DayTokens = day, 0
	}
	return u
}

// sceneUsage 把一轮的用量记进场景。内部群超过任一预算就暂停，写明原因；外部群在下次用的时候查。
// 调用方持有锁。
func (a *App) sceneUsage(sid string, tokens int) {
	sc, ok := a.sceneOf(sid)
	if !ok || tokens <= 0 {
		return
	}
	u := a.sceneUseOf(sc.Key)
	if u.Since == 0 {
		u.Since = a.now().Unix()
	}
	u.DayTokens += tokens
	u.Total += tokens
	a.saveSceneUses()
	if sc.Room == nil {
		return
	}
	switch {
	case sc.Rules.Budget > 0 && u.DayTokens > sc.Rules.Budget:
		a.pauseRoom(*sc.Room, fmt.Sprintf("今天的 token 用量到了预算（%d），先暂停，明天或者你恢复", sc.Rules.Budget))
	case sc.Rules.BudgetTotal > 0 && u.Total > sc.Rules.BudgetTotal:
		a.pauseRoom(*sc.Room, fmt.Sprintf("这一场的 token 累计到了预算（%d），先暂停，清零累计或者你恢复", sc.Rules.BudgetTotal))
	}
}

// sceneOver 是场景现在超没超预算（用完了）。调用方持有锁。
func (a *App) sceneOver(sc sceneInfo) bool {
	u := a.sceneUseView(sc.Key)
	return sc.Rules.Budget > 0 && u.DayTokens >= sc.Rules.Budget ||
		sc.Rules.BudgetTotal > 0 && u.Total >= sc.Rules.BudgetTotal
}

// sceneBudgetNotice 外部群超额后被点名：每天最多回一次固定说明，不调用模型。调用方持有锁。
func (a *App) sceneBudgetNotice(s *sess, ep endpoint, sc sceneInfo) {
	u := a.sceneUseOf(sc.Key)
	if u.NoticeDay == u.Day {
		return
	}
	u.NoticeDay = u.Day
	a.saveSceneUses()
	a.deliver(ep, "今天的额度用完了", "")
}

// sceneResetTotal 把这一场累计的用量清零，重新开始计。调用方持有锁。
func (a *App) sceneResetTotal(key string) {
	u := a.sceneUseOf(key)
	u.Total, u.Since = 0, a.now().Unix()
	a.saveSceneUses()
}

// sceneForget 场景删除时删掉它的用量。调用方持有锁。
func (a *App) sceneForget(key string) {
	if _, ok := a.sceneUses[key]; ok {
		delete(a.sceneUses, key)
		a.saveSceneUses()
	}
}

// SceneUsage 是网页上看到的场景用量。
type SceneUsage struct {
	DayTokens int   `json:"day_tokens"`
	Total     int   `json:"total"`
	Since     int64 `json:"since,omitempty"`
	Over      bool  `json:"over"` // 现在超了预算（外部群当天不再调用模型）
}

// sceneUsageOf 是场景现在的用量。调用方持有锁。
func (a *App) sceneUsageOf(key string, rules scene.Rules) SceneUsage {
	u := a.sceneUseView(key)
	over := rules.Budget > 0 && u.DayTokens >= rules.Budget || rules.BudgetTotal > 0 && u.Total >= rules.BudgetTotal
	return SceneUsage{DayTokens: u.DayTokens, Total: u.Total, Since: u.Since, Over: over}
}
