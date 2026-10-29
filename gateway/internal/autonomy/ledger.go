package autonomy

import (
	"encoding/json"
	"slices"
	"sort"
	"time"
)

// 统一活动账本（设计 §7.1）：按 agent 的本地自然日、按来源分开记启动次数和 token。
//
// 口径写清楚：每日上限只限制"自选探索"，约定、Hook 和主人叫醒各记各的，不占探索次数。
// 每次启动先预留（Reserved），开始失败就释放，模型真的跑过之后按来源结算一次 token；
// 这样两个入口争最后一次额度时不会都通过，也不会因为失败白扣一次。
const (
	LedgerExploration = "exploration" // 自选探索（自己安排的检查、探索类日程）
	LedgerCommitment  = "commitment"  // 明确的约定和已排好的目标：不占探索次数
	LedgerHook        = "hook"        // Hook 派生的模型活动
	LedgerManual      = "manual"      // 主人手动叫醒
)

// LedgerSources 是固定顺序的四个来源，网页和命令按它显示。
var LedgerSources = []string{LedgerExploration, LedgerCommitment, LedgerHook, LedgerManual}

const (
	maxLedgerRuns = 5  // 每个来源记最近几次 run_id
	maxLedgerDays = 14 // 账本保留最近多少天
)

// LedgerUsage 是某天、某个来源的用量。
type LedgerUsage struct {
	Day      string     `json:"day"`
	TZ       string     `json:"tz,omitempty"` // 结算用的时区名（时区改了仍然按当天记）
	Source   string     `json:"source"`
	Starts   int        `json:"starts"`
	Tokens   int        `json:"tokens"`
	Reserved int        `json:"reserved,omitempty"` // 已经预留、还没结算的启动
	Runs     LedgerRuns `json:"runs,omitempty"`
}

// LedgerRuns 兼容两种历史格式：旧版本写成字符串数组（只有 run id），
// 新版本写成对象数组（带 started_at 和这一次的 token）。读旧格式不会让整份心智状态反序列化失败。
type LedgerRuns []LedgerRun

func (r *LedgerRuns) UnmarshalJSON(b []byte) error {
	var objects []LedgerRun
	if err := json.Unmarshal(b, &objects); err == nil {
		*r = objects
		return nil
	}
	var ids []string
	if err := json.Unmarshal(b, &ids); err != nil {
		return err
	}
	out := make(LedgerRuns, 0, len(ids))
	for _, id := range ids {
		out = append(out, LedgerRun{ID: id})
	}
	*r = out
	return nil
}

// LedgerRun 是一次已经结算的活动：run 身份、开始时间和这一次的 token。
type LedgerRun struct {
	ID     string `json:"id"`
	Start  int64  `json:"started_at,omitempty"`
	Tokens int    `json:"tokens,omitempty"`
}

// LedgerReservation 是一次已经开始、还没结算的启动：绑定稳定的活动身份和启动日，
// 跨日结束时也能把旧那天的预留释放掉（评审 §24 R4）。
type LedgerReservation struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Day    string `json:"day"`
	TZ     string `json:"tz,omitempty"`
	At     int64  `json:"at,omitempty"`
}

// LedgerDay 是这个时刻在 loc 下的本地自然日。
func LedgerDay(now time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return now.In(loc).Format("2006-01-02")
}

func (s *State) ledgerEntry(day, tz, source string, create bool) *LedgerUsage {
	for i := range s.Ledger {
		if s.Ledger[i].Day == day && s.Ledger[i].Source == source {
			return &s.Ledger[i]
		}
	}
	if !create {
		return nil
	}
	s.Ledger = append(s.Ledger, LedgerUsage{Day: day, TZ: tz, Source: source})
	s.pruneLedger(day)
	return &s.Ledger[len(s.Ledger)-1]
}

// pruneLedger 只保留最近 maxLedgerDays 天：账本是可检查的记录，不是无限日志。
func (s *State) pruneLedger(today string) {
	days := map[string]bool{}
	for _, u := range s.Ledger {
		days[u.Day] = true
	}
	if len(days) <= maxLedgerDays {
		return
	}
	all := make([]string, 0, len(days))
	for d := range days {
		all = append(all, d)
	}
	sort.Strings(all)
	keep := map[string]bool{}
	for i := len(all) - 1; i >= 0 && len(keep) < maxLedgerDays; i-- {
		keep[all[i]] = true
	}
	out := s.Ledger[:0]
	for _, u := range s.Ledger {
		if keep[u.Day] {
			out = append(out, u)
		}
	}
	s.Ledger = out
}

// LedgerToday 是这一天的四个来源（没有记录的返回 0）。
func (s *State) LedgerToday(now time.Time, loc *time.Location) []LedgerUsage {
	day := LedgerDay(now, loc)
	out := make([]LedgerUsage, 0, len(LedgerSources))
	for _, source := range LedgerSources {
		if u := s.ledgerEntry(day, "", source, false); u != nil {
			out = append(out, *u)
			continue
		}
		out = append(out, LedgerUsage{Day: day, Source: source})
	}
	return out
}

// LedgerStarts 是这一天某个来源已经开始的次数（含还没结算的预留）。
func (s *State) LedgerStarts(source string, now time.Time, loc *time.Location) int {
	u := s.ledgerEntry(LedgerDay(now, loc), "", source, false)
	if u == nil {
		return 0
	}
	return u.Starts + u.Reserved
}

// ReserveLedger 在真的开始之前占一个位置，并记下这次活动的身份和启动日：
// 两个入口不会同时通过"还剩一次"；跨日结束时旧那天的预留能被准确释放。
func (s *State) ReserveLedger(source, id string, now time.Time, loc *time.Location) {
	if source == "" {
		source = LedgerCommitment
	}
	if id == "" {
		id = "reserve-" + LedgerDay(now, loc) + "-" + time.Now().Format("150405.000000000")
	}
	day := LedgerDay(now, loc)
	// 同一个活动身份不重复预留（重复请求、重启后重放都算同一次）
	for _, p := range s.LedgerPending {
		if p.ID == id {
			return
		}
	}
	if s.LedgerSettled != nil && slices.Contains(s.LedgerSettled, id) {
		return
	}
	s.LedgerPending = append(s.LedgerPending, LedgerReservation{ID: id, Source: source, Day: day, TZ: tzName(loc), At: now.Unix()})
	if u := s.ledgerEntry(day, tzName(loc), source, true); u != nil {
		u.Reserved++
	}
	if len(s.LedgerPending) > 128 {
		s.LedgerPending = s.LedgerPending[len(s.LedgerPending)-128:]
	}
	s.Touch()
}

// ReleaseLedger 是没有真的开始时退回预留（启动失败、模型没被调用）。
func (s *State) ReleaseLedger(id string) {
	for i, p := range s.LedgerPending {
		if p.ID != id {
			continue
		}
		if u := s.ledgerEntry(p.Day, "", p.Source, false); u != nil && u.Reserved > 0 {
			u.Reserved--
		}
		s.LedgerPending = append(s.LedgerPending[:i], s.LedgerPending[i+1:]...)
		if len(s.LedgerSettled) > 0 {
			s.LedgerSettled = append(s.LedgerSettled, id)
			if len(s.LedgerSettled) > 256 {
				s.LedgerSettled = s.LedgerSettled[len(s.LedgerSettled)-256:]
			}
		}
		s.Touch()
		return
	}
}

// SettleLedger 是一次活动结束：释放启动日的预留（可能已经跨日），开始次数记在启动日，
// token 记在结束那天，并保留开始时间。同一个 run 重复结算不再重复记次数/token。
func (s *State) SettleLedger(source, reservationID, runID string, tokens int, now time.Time, loc *time.Location) {
	if source == "" {
		source = LedgerCommitment
	}
	settleDay := LedgerDay(now, loc)
	identity := runID
	if identity == "" {
		identity = reservationID
	}
	if identity != "" && slices.Contains(s.LedgerSettled, identity) {
		return // 已经结算过：重复回调、重启后重放都只算一次
	}
	startDay, startedAt := settleDay, now.Unix()
	found := false
	for i, p := range s.LedgerPending {
		if p.ID != reservationID {
			continue
		}
		startDay, startedAt, found = p.Day, p.At, true
		if u := s.ledgerEntry(p.Day, "", p.Source, false); u != nil && u.Reserved > 0 {
			u.Reserved--
		}
		s.LedgerPending = append(s.LedgerPending[:i], s.LedgerPending[i+1:]...)
		break
	}
	_ = found
	// 开始次数记在启动日（没有预留的路径就记在结束日），token 记在结束日
	if u := s.ledgerEntry(startDay, tzName(loc), source, true); u != nil {
		u.Starts++
	}
	u := s.ledgerEntry(settleDay, tzName(loc), source, true)
	if u == nil {
		return
	}
	if startDay != settleDay {
		// 跨日的活动在结束日只记 token 和记录，不重复记一次开始
		u.Starts = max(0, u.Starts)
	}
	if tokens > 0 {
		u.Tokens += tokens
	}
	u.Runs = append(u.Runs, LedgerRun{ID: identity, Start: startedAt, Tokens: tokens})
	if len(u.Runs) > maxLedgerRuns {
		u.Runs = u.Runs[len(u.Runs)-maxLedgerRuns:]
	}
	if identity != "" {
		s.LedgerSettled = append(s.LedgerSettled, identity)
		if len(s.LedgerSettled) > 256 {
			s.LedgerSettled = s.LedgerSettled[len(s.LedgerSettled)-256:]
		}
	}
	s.Touch()
}

// SeedLedger 把旧 life.json 里的当日次数和 token 导进账本（只导一次，由调用方判断）。
func (s *State) SeedLedger(source string, starts, tokens int, now time.Time, loc *time.Location) {
	if starts <= 0 && tokens <= 0 {
		return
	}
	u := s.ledgerEntry(LedgerDay(now, loc), tzName(loc), source, true)
	if u == nil || u.Starts > 0 || u.Tokens > 0 {
		return
	}
	u.Starts, u.Tokens = starts, tokens
	s.Touch()
}

func tzName(loc *time.Location) string {
	if loc == nil {
		return "UTC"
	}
	return loc.String()
}
