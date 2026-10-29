package autonomy

import (
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
	Day      string   `json:"day"`
	TZ       string   `json:"tz,omitempty"` // 结算用的时区名（时区改了仍然按当天记）
	Source   string   `json:"source"`
	Starts   int      `json:"starts"`
	Tokens   int      `json:"tokens"`
	Reserved int      `json:"reserved,omitempty"` // 已经预留、还没结算的启动
	Runs     []string `json:"runs,omitempty"`
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

// LedgerStarts 是这一天某个来源已经开始的次数（含已预留）。
func (s *State) LedgerStarts(source string, now time.Time, loc *time.Location) int {
	u := s.ledgerEntry(LedgerDay(now, loc), "", source, false)
	if u == nil {
		return 0
	}
	return u.Starts + u.Reserved
}

// ReserveLedger 在真的开始之前占一个位置；两个入口不会同时通过"还剩一次"。
func (s *State) ReserveLedger(source string, now time.Time, loc *time.Location) {
	if source == "" {
		source = LedgerCommitment
	}
	day := LedgerDay(now, loc)
	if u := s.ledgerEntry(day, tzName(loc), source, true); u != nil {
		u.Reserved++
	}
	s.Touch()
}

// ReleaseLedger 是没有真的开始时退回预留（启动失败、模型没被调用）。
func (s *State) ReleaseLedger(source string, now time.Time, loc *time.Location) {
	u := s.ledgerEntry(LedgerDay(now, loc), "", source, false)
	if u == nil || u.Reserved <= 0 {
		return
	}
	u.Reserved--
	s.Touch()
}

// SettleLedger 是一次活动结束：预留转成一次开始，token 记到对应来源。
func (s *State) SettleLedger(source, runID string, tokens int, now time.Time, loc *time.Location) {
	if source == "" {
		source = LedgerCommitment
	}
	u := s.ledgerEntry(LedgerDay(now, loc), tzName(loc), source, true)
	if u == nil {
		return
	}
	if u.Reserved > 0 {
		u.Reserved--
	}
	u.Starts++
	if tokens > 0 {
		u.Tokens += tokens
	}
	if runID != "" {
		u.Runs = append(u.Runs, runID)
		if len(u.Runs) > maxLedgerRuns {
			u.Runs = u.Runs[len(u.Runs)-maxLedgerRuns:]
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
