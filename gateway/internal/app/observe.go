package app

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"nano-agent/gateway/internal/health"
	"nano-agent/gateway/internal/journal"
	"nano-agent/gateway/internal/llm"
)

// observe 档：场景主动插话的那一轮照常请求模型，但回复不写进任何历史、不发出，
// 模型提出的工具调用不执行。本来会说的话和触发原因记在 data/runtime/observe.jsonl。
// 只作用于主动插话（groupTick 触发的）：被点名的回复、两个 agent 的私聊、Message、FollowUp 不受影响。

var observeKeep = 2000 // 最多保留多少条；超了重写，只留最新的一半（测试里改小）

const (
	observeRecMax = 64 << 10
	observeFile   = 16 << 20
)

// observeWhy 是触发这一轮的原因：groupTick 触发时记下。
type observeWhy struct {
	QuietSec int     `json:"quiet_sec"` // 群里安静了多久
	Pending  int     `json:"pending"`   // 有几条新消息
	Gate     float64 `json:"gate"`      // 门槛概率（已乘上积极度）
}

// ObserveTool 是模型本来会调用的工具。
type ObserveTool struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"` // 参数摘要
}

// ObserveRecord 是 observe.jsonl 里的一条。
type ObserveRecord struct {
	At               int64         `json:"at"` // 秒
	Scene            string        `json:"scene"`
	SID              string        `json:"sid"`
	Agent            string        `json:"agent"`
	Say              string        `json:"say,omitempty"` // 本来会说的话
	Silent           bool          `json:"silent"`        // 本来也不会说话（[不回复]）
	Tools            []ObserveTool `json:"tools,omitempty"`
	Why              observeWhy    `json:"why"`
	PromptTokens     int           `json:"prompt_tokens"`
	CompletionTokens int           `json:"completion_tokens"`
}

type observeLog struct {
	j    *journal.Journal
	recs []ObserveRecord
	h    *health.Handle // 写入失败时才认领
}

// observeOpen 第一次用到时打开文件（没用 observe 的网关不会创建它）。调用方持有锁。
func (a *App) observeOpen() *observeLog {
	if a.observe != nil || a.observeDir == "" {
		return a.observe
	}
	ol := &observeLog{}
	j, err := journal.Open(filepath.Join(a.observeDir, "runtime", "observe.jsonl"), 1, journal.Limits{Record: observeRecMax, File: observeFile},
		func(_ uint64, data json.RawMessage) error {
			var r ObserveRecord
			if err := json.Unmarshal(data, &r); err != nil {
				return err
			}
			ol.recs = append(ol.recs, r)
			return nil
		})
	if err != nil {
		a.log.Warn("observe 记录打不开，这一轮的结果丢掉", "err", err)
		a.observeFail(ol, err)
		a.observe = ol // 没有 j：以后每次写都会再试一次打开太贵，保持降级
		return ol
	}
	ol.j = j
	if len(ol.recs) > observeKeep {
		ol.trim()
	}
	a.observe = ol
	return ol
}

func (a *App) observeFail(ol *observeLog, err error) {
	if ol.h == nil {
		ol.h, _ = a.health.Claim("observe")
	}
	ol.h.Set(health.Degraded, "observe 记录写不进去："+err.Error())
}

// trim 只留最新的一半，重写文件。
func (ol *observeLog) trim() error {
	keep := ol.recs[len(ol.recs)-observeKeep/2:]
	all := make([]any, len(keep))
	for i, r := range keep {
		all[i] = r
	}
	if err := ol.j.Rewrite(all); err != nil {
		return err
	}
	ol.recs = append([]ObserveRecord(nil), keep...)
	return nil
}

// observeAppend 记一条。写入失败时这一轮的结果丢掉（反正不发），健康状态记为 degraded。调用方持有锁。
func (a *App) observeAppend(r ObserveRecord) {
	ol := a.observeOpen()
	if ol == nil || ol.j == nil {
		return
	}
	if err := ol.j.Append(r); err != nil {
		a.observeFail(ol, err)
		return
	}
	ol.recs = append(ol.recs, r)
	if len(ol.recs) > observeKeep {
		if err := ol.trim(); err != nil {
			a.observeFail(ol, err)
			return
		}
	}
	if ol.h != nil {
		ol.h.Set(health.OK, "")
	}
}

// Observe 是某个场景最近的 observe 记录，新的在前。all 为假时不含“本来也不会说话”的判断。
func (a *App) Observe(sceneKey string, n int, all bool) []ObserveRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []ObserveRecord{}
	ol := a.observeOpen()
	if ol == nil {
		return out
	}
	if n <= 0 || n > 500 {
		n = 50
	}
	for i := len(ol.recs) - 1; i >= 0 && len(out) < n; i-- {
		r := ol.recs[i]
		if sceneKey != "" && r.Scene != sceneKey || !all && r.Silent {
			continue
		}
		out = append(out, r)
	}
	return out
}

// observeTurn 处理 observe 的那一轮模型回复：记下本来会做的事，不写历史、不执行工具、不发出。调用方持有锁。
func (a *App) observeTurn(s *sess, r *run, res *llm.Result) {
	rec := ObserveRecord{At: a.now().Unix(), SID: s.sid, Agent: a.execAgentID(s.sid), Why: *r.observe,
		Say: strings.TrimSpace(res.Content), PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens}
	if sc, ok := a.sceneOf(s.sid); ok {
		rec.Scene = sc.Key
	}
	for _, c := range res.Calls {
		rec.Tools = append(rec.Tools, ObserveTool{Name: c.Function.Name, Args: toolDetail(c.Function.Name, c.Function.Arguments)})
	}
	rec.Silent = isSilence(rec.Say) && len(rec.Tools) == 0
	if rec.Silent {
		rec.Say = ""
	}
	// “不想说就越等越久”按判断了一次计
	if rec.Silent {
		s.groupSilent++
	} else {
		s.groupSilent = 0
	}
	a.observeAppend(rec)
	a.log.Info("observe：本来会说的话已记录，没有发出", "sid", s.sid, "silent", rec.Silent, "tools", len(rec.Tools))
}

func (w observeWhy) String() string {
	return fmt.Sprintf("安静 %d 秒，%d 条新消息，门槛 %.2f", w.QuietSec, w.Pending, w.Gate)
}
