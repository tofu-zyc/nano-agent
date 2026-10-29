package autonomy

// 时间写法、给模型看的摘要、给网页看的视图、从旧版 life.json 迁移。

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseWhen 认几种写时间的方式（都用网关时区）：
//
//	15:04             今天这个时间；已经过了就是明天
//	明天 15:04 / 后天 15:04
//	01-02 15:04       最近的这个日期
//	2006-01-02 15:04  或 2006-01-02T15:04
//	30m / +30m / 2h   从现在起多久
func ParseWhen(s string, now time.Time, loc *time.Location) (time.Time, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return time.Time{}, errors.New("没有写时间")
	}
	now = now.In(loc)
	dayOffset := 0
	switch {
	case strings.HasPrefix(raw, "后天"):
		dayOffset, raw = 2, strings.TrimSpace(strings.TrimPrefix(raw, "后天"))
	case strings.HasPrefix(raw, "明天"):
		dayOffset, raw = 1, strings.TrimSpace(strings.TrimPrefix(raw, "明天"))
	case strings.HasPrefix(raw, "今天"):
		dayOffset, raw = 0, strings.TrimSpace(strings.TrimPrefix(raw, "今天"))
	}
	if d, err := time.ParseDuration(strings.TrimPrefix(raw, "+")); err == nil {
		if d <= 0 {
			return time.Time{}, errors.New("时间要在以后")
		}
		return now.Add(d).Truncate(time.Minute), nil
	}
	at := func(hh, mm int, day time.Time) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, loc)
	}
	if dayOffset > 0 {
		day := now.AddDate(0, 0, dayOffset)
		if t, err := time.ParseInLocation("15:04", raw, loc); err == nil {
			return at(t.Hour(), t.Minute(), day), nil
		}
		return time.Time{}, fmt.Errorf("时间写法不对：%q（比如 明天 09:00）", s)
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("15:04", raw, loc); err == nil {
		res := at(t.Hour(), t.Minute(), now)
		if !res.After(now) {
			res = res.AddDate(0, 0, 1)
		}
		return res, nil
	}
	if t, err := time.ParseInLocation("01-02 15:04", raw, loc); err == nil {
		res := time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		if res.Before(now.Add(-24 * time.Hour)) {
			res = res.AddDate(1, 0, 0)
		}
		return res, nil
	}
	return time.Time{}, fmt.Errorf("时间写法不对：%q（比如 15:30、明天 09:00、2006-01-02 15:04、+30m）", s)
}

// KindLabel 是议程种类的说法。
func KindLabel(k string) string {
	switch k {
	case KindCommitment:
		return "约定"
	case KindCheckup:
		return "复查"
	case KindPlan:
		return "计划"
	case KindExploration:
		return "探索"
	}
	return k
}

// StatusLabel 是议程/意图状态的说法。
func StatusLabel(s string) string {
	switch s {
	case StatusPlanned:
		return "等做"
	case StatusInProgress:
		return "正在做"
	case StatusDone:
		return "做完了"
	case StatusSkipped:
		return "跳过"
	case StatusCancelled:
		return "取消"
	case IntentionAccepted:
		return "推进中"
	case IntentionRunning:
		return "正在做"
	case IntentionWaiting:
		return "等下一步"
	case IntentionPaused:
		return "暂停"
	case IntentionFailed:
		return "失败"
	case IdeaCandidate:
		return "候选"
	case IdeaScheduled:
		return "已排期"
	case IdeaDropped:
		return "放弃"
	}
	return s
}

// BriefForModel 是醒来时给模型看的心智状态；只放这次决策需要的，不塞整段历史。
func (s *State) BriefForModel(now time.Time, loc *time.Location) string {
	now = now.In(loc)
	var b strings.Builder
	fmt.Fprintf(&b, "[自主活动] 现在是 %s %s。\n", now.Format("01-02"), now.Format("15:04"))
	if len(s.Self.Interests) > 0 {
		b.WriteString("你的长期兴趣：")
		for i, x := range s.Self.Interests {
			if i > 0 {
				b.WriteString("；")
			}
			b.WriteString(x.Text + "（来源：" + x.Source + "）")
		}
		b.WriteString("\n")
	}
	if s.Paused {
		b.WriteString("（自主活动现在是暂停的，这一轮是主人叫醒的。）\n")
	}
	if len(s.Beliefs) > 0 {
		b.WriteString("【最近的认识和反思】\n")
		for _, belief := range s.Beliefs[max(0, len(s.Beliefs)-5):] {
			line := fmt.Sprintf("- %s：%s（来源：%s", belief.Subject, clip(belief.Value, 200), belief.Source)
			if belief.Confidence > 0 {
				line += fmt.Sprintf("，置信度 %.0f%%", belief.Confidence*100)
			}
			if belief.Confidence > 0 && belief.Confidence < 0.5 {
				line += "，还没确认"
			}
			b.WriteString(line + "）\n")
		}
	}
	open := s.OpenAgenda()
	if len(open) > 0 {
		b.WriteString("【你的安排】：\n")
		for i, a := range open {
			if i >= 10 {
				break
			}
			when := time.Unix(a.At, 0).In(loc).Format("01-02 15:04")
			fmt.Fprintf(&b, "- [%s] %s（%s，%s，%s", when, a.Title, KindLabel(a.Kind), StatusLabel(a.Status), a.ID)
			if a.Minutes > 0 {
				fmt.Fprintf(&b, "，约 %d 分钟", a.Minutes)
			}
			if a.Note != "" {
				b.WriteString("；" + clip(a.Note, 100))
			}
			if a.Criteria != "" {
				b.WriteString("；验收：" + clip(a.Criteria, 200))
			}
			if a.Checkpoint != "" {
				b.WriteString("；接着做：" + clip(a.Checkpoint, 300))
			}
			b.WriteString("）\n")
		}
	} else {
		b.WriteString("【你的安排】：现在没有排着的事。\n")
	}
	if intents := s.ActiveIntentions(); len(intents) > 0 {
		b.WriteString("【在推进的目标】：\n")
		for i, in := range intents {
			if i >= 6 {
				break
			}
			fmt.Fprintf(&b, "- %s（%s，进度 %d，优先 %d，%s", in.Title, StatusLabel(in.State), in.Progress, in.Priority, in.ID)
			if in.NextAt > 0 {
				fmt.Fprintf(&b, "，下次 %s", time.Unix(in.NextAt, 0).In(loc).Format("01-02 15:04"))
			}
			if in.Blocked != "" {
				b.WriteString("，卡在：" + clip(in.Blocked, 100))
			}
			b.WriteString("）\n")
		}
	}
	if ideas := s.OpenIdeas(now); len(ideas) > 0 {
		b.WriteString("【想到的事】（还没决定做）：\n")
		for i, x := range ideas {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "- %s（%s，兴趣 %d，新颖 %d，约 %d 分钟，来源：%s）\n", x.Text, x.ID, x.Interest, x.Novelty, x.CostMinutes, clip(x.Source, 80))
		}
	}
	if events := s.UnprocessedEvents(6); len(events) > 0 {
		b.WriteString("最近发生：\n")
		for _, e := range events {
			if e.Kind == EventConversation || strings.HasPrefix(e.Source, "followup:") {
				continue
			}
			what := EventLabel(e.Kind)
			line := "- " + time.Unix(e.At, 0).In(loc).Format("01-02 15:04") + " " + what
			if e.Text != "" {
				line += "：" + clip(e.Text, 150)
			}
			b.WriteString(line + "\n")
		}
	}
	if n := len(s.Executions); n > 0 {
		b.WriteString("最近做过：\n")
		for i := n - 1; i >= 0 && i >= n-3; i-- {
			e := s.Executions[i]
			status := "没完成"
			if e.OK && e.Verified {
				status = "完成"
			}
			fmt.Fprintf(&b, "- %s %s：%s\n", time.Unix(e.Ended, 0).In(loc).Format("01-02 15:04"), status, clip(e.Summary, 150))
		}
	}
	if n := len(s.Reflections); n > 0 {
		r := s.Reflections[n-1]
		fmt.Fprintf(&b, "上次反思（%s）：%s\n", time.Unix(r.At, 0).In(loc).Format("01-02"), clip(r.Text, 200))
		if r.Change != "" {
			fmt.Fprintf(&b, "下次的调整：%s\n", clip(r.Change, 300))
		}
	}
	b.WriteString("\n只处理本次【要处理的】；其他记录不是任务。需要以后做的用 Agenda 显式交接。什么都不用做就只回 [不做]。\n")
	return b.String()
}

// EventLabel 是事件的说法。
func EventLabel(k string) string {
	switch k {
	case EventConversation:
		return "聊天有新内容"
	case EventCommitment:
		return "答应了别人一件事"
	case EventAgendaDue:
		return "安排的时间到了"
	case EventActionDone:
		return "一件事做完了"
	case EventActionFailed:
		return "一件事没做成"
	case EventExternal:
		return "外面有变化"
	case EventReflection:
		return "整理记忆之后的反思"
	case EventSelfCheck:
		return "自己安排的检查"
	case EventManualWake:
		return "主人把你叫醒"
	}
	return k
}

// LifeNote 是旧版 life.json 里的一条近况（迁移用，避免 autonomy 依赖 app）。
type LifeNote struct {
	At   int64
	Text string
}

// ImportLife 把旧版近况迁进来一次：当作认识，不重复导入。
func (s *State) ImportLife(recent []LifeNote, now time.Time) bool {
	if s.Migrated&1 != 0 {
		return false
	}
	for i, n := range recent {
		if i >= 5 {
			break
		}
		s.setBelief("最近自己做的事", clip(n.Text, 300), fmt.Sprintf("life:%d", n.At), 0.7, time.Unix(n.At, 0))
	}
	if len(recent) > 0 {
		s.setBelief("迁移说明", "旧版本的近况已经导入；之前的约好的回访仍由旧的 FollowUp 执行", "migration", 1, now)
	}
	s.Migrated |= 1
	s.Touch()
	return true
}

// View 是给网页看的自主活动概况。
type View struct {
	SchemaVersion int          `json:"schema_version"`
	Revision      int64        `json:"revision"`
	Paused        bool         `json:"paused"`
	PauseReason   string       `json:"pause_reason,omitempty"`
	NextCheck     int64        `json:"next_check,omitempty"` // 毫秒，方便网页直接用
	Budget        Budget       `json:"budget"`
	BudgetLeft    int          `json:"budget_left"`
	Self          Self         `json:"self"`
	TodayPlanned  int          `json:"today_planned"`
	TodayDone     int          `json:"today_done"`
	Agenda        []AgendaItem `json:"agenda"`
	Intentions    []Intention  `json:"intentions"`
	Ideas         []Idea       `json:"ideas"`
	Executions    []Execution  `json:"executions"`
	Reflections   []Reflection `json:"reflections"`
	Beliefs       []Belief     `json:"beliefs"`
}

// View 整理一份给网页的概况（个数有上限，按时间倒序）。
func (s *State) View(now time.Time, loc *time.Location, limit int) View {
	s.Normalize()
	v := View{SchemaVersion: s.SchemaVersion, Revision: s.Revision, Paused: s.Paused, PauseReason: s.PauseReason,
		Budget: s.Budget, BudgetLeft: s.BudgetLeft(s.Budget.Limit), Self: s.Self,
		Agenda: s.OpenAgenda(), Intentions: s.ActiveIntentions(), Ideas: s.OpenIdeas(now)}
	if s.NextCheck > 0 {
		v.NextCheck = s.NextCheck * 1000
	}
	start := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1)
	for _, a := range s.Agenda {
		if a.At >= start.Unix() && a.At < end.Unix() {
			v.TodayPlanned++
			if a.Status == StatusDone {
				v.TodayDone++
			}
		}
	}
	n := len(s.Executions)
	if limit > 0 && n > limit {
		n = limit
	}
	for i := len(s.Executions) - 1; i >= 0 && len(v.Executions) < n; i-- {
		v.Executions = append(v.Executions, s.Executions[i])
	}
	rn := len(s.Reflections)
	if limit > 0 && rn > limit {
		rn = limit
	}
	for i := len(s.Reflections) - 1; i >= 0 && len(v.Reflections) < rn; i-- {
		v.Reflections = append(v.Reflections, s.Reflections[i])
	}
	bn := len(s.Beliefs)
	if limit > 0 && bn > limit {
		bn = limit
	}
	for i := len(s.Beliefs) - 1; i >= 0 && len(v.Beliefs) < bn; i-- {
		v.Beliefs = append(v.Beliefs, s.Beliefs[i])
	}
	if v.Agenda == nil {
		v.Agenda = []AgendaItem{}
	}
	if v.Intentions == nil {
		v.Intentions = []Intention{}
	}
	if v.Ideas == nil {
		v.Ideas = []Idea{}
	}
	if v.Executions == nil {
		v.Executions = []Execution{}
	}
	if v.Reflections == nil {
		v.Reflections = []Reflection{}
	}
	if v.Beliefs == nil {
		v.Beliefs = []Belief{}
	}
	return v
}

// IntOr 是测试和设置里常用的小工具：把可以为 nil 的 int 换成默认值。
func IntOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// FormatMinute 是给网页/日志用的 “01-02 15:04”。
func FormatMinute(sec int64, loc *time.Location) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).In(loc).Format("01-02 15:04")
}

// ParseMinutes 是工具参数里 “30m”“20 分钟”“1h” 的写法，返回分钟数。
func ParseMinutes(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "分钟")
	if d, err := time.ParseDuration(s); err == nil {
		return int(d.Minutes()), nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	return 0, fmt.Errorf("时长写法不对：%q（比如 30m、20 分钟）", s)
}
