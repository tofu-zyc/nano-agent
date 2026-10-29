// Package autonomy 是拟人 agent 的自主活动控制层：跨轮次连续的认知、目标、承诺和日程。
//
// 设计参考 BDI（信念 / 愿望 / 意图，见 Jason BDI）和 Generative Agents（记忆流、反思、日程、重规划），
// 但不引入它们的运行时：状态持久化在网关的数据目录，决策复用现有的模型循环和工具执行链，
// 长期经验仍然交给 memory / dream。这里只做“什么时候该想、该做什么、做到哪了”：
//
//	事件（聊天、约定、日程到点、执行结果、反思到点、主人叫醒）
//	  → 合并、去重、更新心智状态
//	  → 选一个到期的议程项或意图（或自己的下次检查）
//	  → 在 agent 的内心会话里跑一轮：用工具真的去做，或者用 Agenda / Idea 排期
//	  → 真实结果写回执行记录和目标进度，决定下一次处理时间
//
// 普通自主活动不再要求“最近 20 分钟没人找”：内部决策、资源占用、主动联系用户分别判断。
// 明确的约定（commitment）不受作息限制；探索按作息和每日次数；预算按天算；暂停立即生效。
package autonomy

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// SchemaVersion 是心智状态的版本，加载旧文件时按它迁移。
const SchemaVersion = 2

// 事件种类。
const (
	EventConversation = "conversation_updated" // 聊天有新内容（高频，合并）
	EventCommitment   = "commitment_created"   // 答应了别人一件事
	EventAgendaDue    = "agenda_due"           // 日程到点
	EventActionDone   = "action_completed"     // 一次行动有了真实结果
	EventActionFailed = "action_failed"        // 一次行动失败
	EventExternal     = "external_changed"     // 外部变化（机器上线、任务结果等）
	EventReflection   = "reflection_due"       // 反思：整理完记忆
	EventSelfCheck    = "self_check_due"       // 自己安排的下次检查
	EventManualWake   = "manual_wake"          // 主人叫醒
)

// 议程项的种类。
const (
	KindCommitment  = "commitment"  // 和用户明确约定的
	KindCheckup     = "checkup"     // 约好过会儿再看看
	KindPlan        = "plan"        // 自己定的计划
	KindExploration = "exploration" // 自选探索，可以挪、可以放弃
)

// 议程项状态。
const (
	StatusPlanned    = "planned"
	StatusInProgress = "in_progress"
	StatusDone       = "done"
	StatusSkipped    = "skipped"
	StatusCancelled  = "cancelled"
)

// 意图状态（BDI 的 intention 生命周期）。
const (
	IntentionAccepted  = "accepted"
	IntentionRunning   = "running"
	IntentionWaiting   = "waiting"
	IntentionPaused    = "paused"
	IntentionDone      = "done"
	IntentionFailed    = "failed"
	IntentionCancelled = "cancelled"
)

// 想法状态。
const (
	IdeaCandidate = "candidate"
	IdeaScheduled = "scheduled"
	IdeaDropped   = "dropped"
	IdeaDone      = "done"
)

// State 是一个 agent 的自主活动状态（所有聊天共享，按 agent 保存）。
type State struct {
	SchemaVersion int   `json:"schema_version"`
	Revision      int64 `json:"revision"`
	Seq           int   `json:"seq,omitempty"` // 生成稳定 ID 用（重启后接着数）

	Paused      bool   `json:"paused,omitempty"`
	PauseReason string `json:"pause_reason,omitempty"`
	NextCheck   int64  `json:"next_check,omitempty"` // 下次自己检查（秒）
	LastWake    int64  `json:"last_wake,omitempty"`

	Budget  Budget   `json:"budget,omitempty"`
	Self    Self     `json:"self,omitempty"`
	Beliefs []Belief `json:"beliefs,omitempty"`

	Ideas      []Idea       `json:"ideas,omitempty"`
	Intentions []Intention  `json:"intentions,omitempty"`
	Agenda     []AgendaItem `json:"agenda,omitempty"`

	TopicBranches []TopicBranch `json:"topic_branches,omitempty"`
	// runtime 是这次规划时启用的维度影响（不落盘，由网关按 agent 配置注入）
	runtime Runtime

	// MindProfile 是心智档案：候选权重、可选维度与阈值（设计 §7.2 / §7.3）
	MindProfile MindProfile `json:"mind_profile,omitempty"`

	// LedgerPending 是已经开始、还没结算的启动（绑定活动身份和启动日）
	LedgerPending []LedgerReservation `json:"ledger_pending,omitempty"`
	// LedgerSettled 是最近结算过的活动身份：重复回调/重启重放不再重复计数
	LedgerSettled []string `json:"ledger_settled,omitempty"`

	// Ledger 是统一的自主活动账本：按本地自然日、按来源记次数和 token（设计 §7.1）
	Ledger []LedgerUsage `json:"ledger,omitempty"`

	Events      []Event      `json:"events,omitempty"`
	Executions  []Execution  `json:"executions,omitempty"`
	Reflections []Reflection `json:"reflections,omitempty"`

	Migrated int `json:"migrated,omitempty"` // 从旧 life.json 导入过什么
}

// TopicBranch 记住某个话题（意图 in:<id> 或日程 ag:<id>）已经用自己的持久分支继续。
// 同一个话题下一次接着做时复用这个分支，不每次醒来都 Fork 一个新的。
type TopicBranch struct {
	Topic     string `json:"topic"`
	BranchSID string `json:"branch_sid"`
	Updated   int64  `json:"updated"`
}

// Interest 是长期兴趣，必须带来源（哪次经历、哪段聊天或哪次反思让它产生的）。
type Interest struct {
	Text   string `json:"text"`
	Source string `json:"source"`
	At     int64  `json:"at"`
}

// Self 是稳定的自我描述：长期兴趣、自主范围。人格文件是基线，这里只记允许变化的部分。
type Self struct {
	Interests []Interest `json:"interests,omitempty"`
	Notes     []string   `json:"notes,omitempty"`
	Updated   int64      `json:"updated,omitempty"`
}

// Belief 是对环境、约定或任务状况的认识。Source 说明从哪来，避免把传闻当事实。
type Belief struct {
	Subject    string  `json:"subject"`
	Value      string  `json:"value"`
	Source     string  `json:"source,omitempty"`
	Confidence float64 `json:"confidence,omitempty"` // 0–1
	At         int64   `json:"at"`
}

// Idea 是还没成为承诺的突发想法。
type Idea struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	Source      string `json:"source,omitempty"`   // 来源：哪个事件 / 哪段记忆 / 哪次聊天
	Interest    int    `json:"interest,omitempty"` // 1–5
	Novelty     int    `json:"novelty,omitempty"`  // 1–5
	CostMinutes int    `json:"cost_minutes,omitempty"`
	Status      string `json:"status"`
	DeferUntil  int64  `json:"defer_until,omitempty"`
	Expires     int64  `json:"expires,omitempty"`
	Created     int64  `json:"created"`
	Updated     int64  `json:"updated"`
	IntentionID string `json:"intention_id,omitempty"`
}

// Intention 是已经决定推进的目标（少量、有生命周期）。
type Intention struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Why          string `json:"why,omitempty"`
	SourceEvent  string `json:"source_event,omitempty"`
	Priority     int    `json:"priority,omitempty"` // 1–5，越大越优先
	State        string `json:"state"`
	NextAt       int64  `json:"next_at,omitempty"`
	Blocked      string `json:"blocked,omitempty"` // 等什么：预算、重启、没做完、被打断
	Progress     int    `json:"progress,omitempty"`
	Attempts     int    `json:"attempts,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	SpentTokens  int    `json:"spent_tokens,omitempty"`
	Evidence     string `json:"evidence,omitempty"` // 真实的执行结果摘要
	AgendaID     string `json:"agenda_id,omitempty"`
	Created      int64  `json:"created"`
	Updated      int64  `json:"updated"`
}

// AgendaItem 是今日计划里的一个时间块。
type AgendaItem struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Kind        string   `json:"kind"`
	At          int64    `json:"at"` // 计划开始（秒）
	Minutes     int      `json:"minutes,omitempty"`
	Movable     bool     `json:"movable,omitempty"`
	Status      string   `json:"status"`
	Source      string   `json:"source,omitempty"`
	IntentionID string   `json:"intention_id,omitempty"`
	Note        string   `json:"note,omitempty"`
	Criteria    string   `json:"criteria,omitempty"`   // 做到什么才算完成
	Checkpoint  string   `json:"checkpoint,omitempty"` // 已做到哪，下一步是什么
	ScopeSet    bool     `json:"scope_set,omitempty"`
	Workspaces  []string `json:"workspaces,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	Result      string   `json:"result,omitempty"`
	Created     int64    `json:"created"`
	Updated     int64    `json:"updated"`
}

// Event 是进入控制层的一件事。至少一次投递、按 Dedupe 去重。
type Event struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Source      string `json:"source,omitempty"`
	At          int64  `json:"at"`
	Dedupe      string `json:"dedupe,omitempty"`
	SID         string `json:"sid,omitempty"`
	AgendaID    string `json:"agenda_id,omitempty"`
	IntentionID string `json:"intention_id,omitempty"`
	Text        string `json:"text,omitempty"`
	Processed   bool   `json:"processed,omitempty"`
}

// Execution 是一次真实行动的记录：接着哪个意图 / 议程、有没有真的调工具、结果是什么。
type Execution struct {
	ID          string         `json:"id"`
	Trigger     string         `json:"trigger,omitempty"`
	IntentionID string         `json:"intention_id,omitempty"`
	AgendaID    string         `json:"agenda_id,omitempty"`
	RunSID      string         `json:"run_sid,omitempty"`
	Started     int64          `json:"started"`
	Ended       int64          `json:"ended"`
	OK          bool           `json:"ok"`
	UsedTools   bool           `json:"used_tools"`
	Verified    bool           `json:"verified"`
	Evidence    []ToolEvidence `json:"evidence,omitempty"`
	Summary     string         `json:"summary,omitempty"`
	Tokens      int            `json:"tokens,omitempty"`
}

// ToolEvidence 由网关根据内核的实际工具结果生成，模型不能自己填成功状态。
type ToolEvidence struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Verified bool   `json:"verified"`
	Result   string `json:"result"`
}

// Reflection 是反思结果：计划和实际的偏差、下一步调整。
type Reflection struct {
	At     int64  `json:"at"`
	Text   string `json:"text"`
	Change string `json:"change,omitempty"`
}

// Budget 是每天自主活动的 token 预算。
type Budget struct {
	Day    string `json:"day,omitempty"`
	Tokens int    `json:"tokens,omitempty"`
	Limit  int    `json:"limit,omitempty"` // 0 表示不限
}

// New 返回一个空状态。
func New() *State {
	return &State{SchemaVersion: SchemaVersion}
}

// Normalize 补齐旧文件缺的字段，并迁移不能识别的版本。
func (s *State) Normalize() {
	if s.SchemaVersion > SchemaVersion {
		return
	} // 未来版本不降级、不改写
	if s.SchemaVersion < 2 {
		beliefs := s.Beliefs[:0]
		for _, b := range s.Beliefs {
			autoChat := strings.HasPrefix(b.Source, "chat:") && strings.HasPrefix(b.Subject, "最近和")
			autoFollow := (strings.HasPrefix(b.Source, "followup:") || strings.HasPrefix(b.Source, "agenda:")) && b.Subject == "和对方的约定"
			if !autoChat && !autoFollow {
				beliefs = append(beliefs, b)
			}
		}
		s.Beliefs = beliefs
		for i := range s.Events {
			if s.Events[i].Kind == EventConversation || strings.HasPrefix(s.Events[i].Source, "followup:") {
				s.Events[i].Processed = true
			}
		}
		s.SchemaVersion = SchemaVersion
		s.Touch()
	}
	for i := range s.Agenda {
		if s.Agenda[i].Status == "" {
			s.Agenda[i].Status = StatusPlanned
		}
		if s.Agenda[i].Kind == "" {
			s.Agenda[i].Kind = KindPlan
		}
	}
	for i := range s.Intentions {
		if s.Intentions[i].State == "" {
			s.Intentions[i].State = IntentionAccepted
		}
	}
	for i := range s.Ideas {
		if s.Ideas[i].Status == "" {
			s.Ideas[i].Status = IdeaCandidate
		}
	}
}

// Touch 记一次修改（提交时校验 revision，过期的规划不能覆盖新状态）。
func (s *State) Touch() { s.Revision++ }

// NewID 生成一个稳定、重启后不会重复的 ID。
func (s *State) NewID(prefix string) string {
	s.Seq++
	return fmt.Sprintf("%s-%d", prefix, s.Seq)
}

// RollDay 换天时清空当天用量。
func (s *State) RollDay(now time.Time, loc *time.Location) bool {
	day := now.In(loc).Format("2006-01-02")
	if s.Budget.Day == day {
		return false
	}
	s.Budget.Day, s.Budget.Tokens = day, 0
	s.Touch()
	return true
}

// BudgetLeft 是今天还剩多少 token；没设上限时返回 -1。
func (s *State) BudgetLeft(limit int) int {
	if limit <= 0 {
		return -1
	}
	left := limit - s.Budget.Tokens
	if left < 0 {
		left = 0
	}
	return left
}

// Spend 记一次自主活动的用量。
func (s *State) Spend(tokens int) {
	if tokens > 0 {
		s.Budget.Tokens += tokens
		s.Touch()
	}
}

// AddEvent 把事件放进队列；同一个 Dedupe 在 ttl 内重复到达就合并掉。返回是否真的加进去了。
func (s *State) AddEvent(e Event, now time.Time, ttl time.Duration) bool {
	if e.Kind == "" {
		return false
	}
	if e.At == 0 {
		e.At = now.Unix()
	}
	if e.Dedupe != "" {
		for _, old := range s.Events {
			if old.Dedupe != e.Dedupe {
				continue
			}
			if ttl <= 0 || now.Sub(time.Unix(old.At, 0)) < ttl {
				return false
			}
		}
	}
	if e.ID == "" {
		e.ID = s.NewID("ev")
	}
	s.Events = append(s.Events, e)
	s.pruneEvents(now)
	s.Touch()
	return true
}

// UnprocessedEvents 是还没消费的事件（最多 n 条，新的优先）。
func (s *State) UnprocessedEvents(n int) []Event {
	var out []Event
	for i := len(s.Events) - 1; i >= 0 && len(out) < n; i-- {
		if !s.Events[i].Processed {
			out = append(out, s.Events[i])
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// MarkProcessed 把这些事件标成已消费。事件按至少一次投递设计，重复消费由这里的标记和去重挡住。
func (s *State) MarkProcessed(ids []string) {
	if len(ids) == 0 {
		return
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	for i := range s.Events {
		if set[s.Events[i].ID] {
			s.Events[i].Processed = true
		}
	}
	s.Touch()
}

// Trigger 是这次该处理的一件事。
type Trigger struct {
	Kind        string `json:"kind"`
	AgendaID    string `json:"agenda_id,omitempty"`
	IntentionID string `json:"intention_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Due         int64  `json:"due,omitempty"`
}

// IsCommitment 是明确的约定：不受作息限制。
func (t Trigger) IsCommitment() bool {
	return t.Kind == EventCommitment || t.Kind == KindCommitment
}

// Due 返回现在到期的议程项和意图：约定优先，其次复查、计划、探索；同一时间先安排的先做。
func (s *State) Due(now time.Time) []Trigger {
	nowU := now.Unix()
	var out []Trigger
	linked := map[string]bool{}
	for _, a := range s.Agenda {
		if a.Status != StatusPlanned || a.At > nowU {
			continue
		}
		out = append(out, Trigger{Kind: a.Kind, AgendaID: a.ID, IntentionID: a.IntentionID, Reason: a.Title, Due: a.At})
		if a.IntentionID != "" {
			linked[a.IntentionID] = true
		}
	}
	for _, in := range s.Intentions {
		if linked[in.ID] || in.NextAt <= 0 || in.NextAt > nowU {
			continue
		}
		if in.State != IntentionAccepted && in.State != IntentionWaiting {
			continue
		}
		out = append(out, Trigger{Kind: "intention", IntentionID: in.ID, Reason: in.Title, Due: in.NextAt})
	}
	// 专注维度打开时限制"同时推进几个目标"（软偏好）：专注越高，一次越只做手上一件。
	// 约定和复查不受影响，边界（次数/预算/暂停）也不变；没打开维度就保持旧行为。
	if s.runtime.Focus > 0 && len(out) > 1 {
		limit := 3
		switch {
		case s.runtime.Focus >= 80:
			limit = 1
		case s.runtime.Focus >= 40:
			limit = 2
		}
		intents := map[string]bool{}
		kept := out[:0]
		for _, t := range out {
			if t.IntentionID != "" && !t.IsCommitment() {
				if intents[t.IntentionID] {
					continue // 同一个目标一次只推进一件事
				}
				if len(intents) >= limit {
					continue // 同时推进的目标数量到软上限了，剩下的往后排
				}
				intents[t.IntentionID] = true
			}
			kept = append(kept, t)
		}
		out = kept
	}
	rank := func(t Trigger) int {
		switch {
		case t.IsCommitment():
			return 0
		case t.Kind == KindCheckup:
			return 1
		case t.IntentionID != "":
			return 2
		case t.Kind == KindPlan:
			return 3
		default:
			return 4
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		return out[i].Due < out[j].Due
	})
	return out
}

// Next 选出这次该处理的：到期的事优先；没有的话看是不是到了自己安排的下次检查。
// 没有返回 nil 就什么都不做、不调用模型。
func (s *State) Next(now time.Time) *Trigger {
	if due := s.Due(now); len(due) > 0 {
		return &due[0]
	}
	if s.NextCheck > 0 && s.NextCheck <= now.Unix() {
		return &Trigger{Kind: EventSelfCheck, Reason: "自己安排的下次检查", Due: s.NextCheck}
	}
	return nil
}

// IsExploration 是自选的、消耗每日次数的活动。
func (t Trigger) IsExploration() bool {
	if t.Kind == EventSelfCheck {
		return true
	}
	if t.AgendaID != "" {
		return t.Kind == KindExploration
	}
	return false
}

// StartOptions 是启动一次自主活动时的外部条件（由网关判断，不写进状态）。
type StartOptions struct {
	Paused     bool
	Awake      bool
	BudgetLeft int // -1 表示不限
	Wakes      int
	MaxWakes   int
}

// CanStart 判断现在能不能开始；why 是给人看的原因。
// 内部决策、资源占用、主动联系是分开的：这里只管内部决策；资源由调用方检查（内心会话在跑就不能开），
// 主动联系由 Message 工具自己的规则管。
func (t Trigger) CanStart(o StartOptions) (bool, string) {
	if o.Paused {
		return false, "自主活动暂停了"
	}
	if o.BudgetLeft == 0 {
		return false, "今天的自主活动预算用完了"
	}
	if !o.Awake && !t.IsCommitment() {
		return false, "现在睡着（明确的约定不受作息限制，其他的等醒了再做）"
	}
	if t.IsExploration() && o.MaxWakes > 0 && o.Wakes >= o.MaxWakes {
		return false, "今天自己醒够了"
	}
	return true, ""
}

// MarkStarted 把这次触发标成进行中。
func (s *State) MarkStarted(t Trigger, now time.Time) {
	nowU := now.Unix()
	s.LastWake = nowU
	if t.AgendaID != "" {
		if a := s.agenda(t.AgendaID); a != nil {
			a.Status, a.Updated = StatusInProgress, nowU
		}
	}
	if t.IntentionID != "" {
		if in := s.intention(t.IntentionID); in != nil {
			in.State, in.NextAt, in.Updated = IntentionRunning, 0, nowU
		}
	}
	s.NextCheck = 0
	s.Touch()
}

// Finish 把一次真实执行写回状态：只有真实工具证据支持的验收且本轮正常结束才算完成。
// 只写计划、调工具但没验收或没有真实结果，都保留检查点并延期。
func (s *State) Finish(rec Execution, now time.Time, retry time.Duration) {
	if rec.Ended == 0 {
		rec.Ended = now.Unix()
	}
	rec.Summary = clip(rec.Summary, 500)
	s.Executions = append(s.Executions, rec)
	if len(s.Executions) > 50 {
		s.Executions = append([]Execution(nil), s.Executions[len(s.Executions)-50:]...)
	}
	nowU := now.Unix()
	done := rec.OK && rec.Verified && rec.UsedTools && strings.TrimSpace(rec.Summary) != ""
	if a := s.agenda(rec.AgendaID); a != nil {
		// 本轮取消或主动挪动过的日程保持模型/用户的新决定。
		if a.Status == StatusCancelled || a.Status == StatusSkipped || a.Status == StatusDone || a.Status == StatusPlanned {
			s.Touch()
			return
		}
		a.Updated = nowU
		if done {
			a.Status, a.Result = StatusDone, rec.Summary
		} else {
			a.Status = StatusPlanned
			a.At = now.Add(retry).Unix()
			a.Result = "没完成：" + clip(rec.Summary, 200)
		}
	}
	if in := s.intention(rec.IntentionID); in != nil {
		in.Updated = nowU
		switch {
		case done:
			in.Progress++
			in.Evidence = rec.Summary
			if hasOpenAgenda(s.Agenda, in.ID) {
				in.State, in.NextAt, in.Blocked = IntentionAccepted, 0, ""
			} else {
				in.State, in.NextAt, in.Blocked = IntentionDone, 0, ""
			}
			s.setBelief("最近做的事："+in.Title, rec.Summary, "execution:"+rec.ID, 0.8, now)
		case rec.OK:
			in.State = IntentionWaiting
			in.Blocked = "这一轮没有真正执行或尚未验收；按检查点继续，不能直接重放结果未知的操作"
			in.NextAt = now.Add(retry).Unix()
			in.Attempts++
		default:
			in.State = IntentionWaiting
			in.Blocked = "上次执行失败：" + clip(rec.Summary, 120)
			in.NextAt = now.Add(retry).Unix()
			in.Attempts++
		}
	}
	s.NextCheck = 0
	s.Touch()
}

// hasOpenAgenda 是这个意图还有没有没做完的议程项。
func hasOpenAgenda(items []AgendaItem, intentionID string) bool {
	for _, a := range items {
		if a.IntentionID == intentionID && (a.Status == StatusPlanned || a.Status == StatusInProgress) {
			return true
		}
	}
	return false
}

// AddAgenda 加一个议程项。
func (s *State) AddAgenda(x AgendaItem, now time.Time) (*AgendaItem, error) {
	x.Title = strings.TrimSpace(x.Title)
	if x.Title == "" {
		return nil, errors.New("议程项要有标题")
	}
	if x.At <= 0 {
		return nil, errors.New("议程项要有时间")
	}
	if x.Kind == "" {
		x.Kind = KindPlan
	}
	if !validKind(x.Kind) {
		return nil, fmt.Errorf("不认识的议程种类 %q（只能是 commitment、checkup、plan、exploration）", x.Kind)
	}
	if x.Status == "" {
		x.Status = StatusPlanned
	}
	if x.ID == "" {
		x.ID = s.NewID("ag")
	}
	x.Created, x.Updated = now.Unix(), now.Unix()
	s.Agenda = append(s.Agenda, x)
	s.Touch()
	return &s.Agenda[len(s.Agenda)-1], nil
}

// AddIntention 加一个目标。
func (s *State) AddIntention(x Intention, now time.Time) (*Intention, error) {
	x.Title = strings.TrimSpace(x.Title)
	if x.Title == "" {
		return nil, errors.New("目标要有标题")
	}
	if x.State == "" {
		x.State = IntentionAccepted
	}
	if x.ID == "" {
		x.ID = s.NewID("in")
	}
	x.Created, x.Updated = now.Unix(), now.Unix()
	s.Intentions = append(s.Intentions, x)
	s.Touch()
	return &s.Intentions[len(s.Intentions)-1], nil
}

// MoveAgenda 改时间（自己调整计划）。
func (s *State) MoveAgenda(id string, at int64, now time.Time) error {
	a := s.agenda(id)
	if a == nil {
		return errors.New("没有这个议程项")
	}
	if a.Status == StatusDone || a.Status == StatusCancelled {
		return errors.New("这件事已经结束了")
	}
	if at <= 0 {
		return errors.New("时间不对")
	}
	a.At, a.Status, a.Updated = at, StatusPlanned, now.Unix()
	s.Touch()
	return nil
}

// CancelAgenda 取消一个议程项；它的意图没有别的议程了就一起取消。
func (s *State) CancelAgenda(id, why string, now time.Time) error {
	a := s.agenda(id)
	if a == nil {
		return errors.New("没有这个议程项")
	}
	a.Status, a.Updated = StatusCancelled, now.Unix()
	if why != "" {
		a.Result = clip(why, 200)
	}
	if in := s.intention(a.IntentionID); in != nil && !hasOpenAgenda(s.Agenda, in.ID) {
		in.State, in.Updated = IntentionCancelled, now.Unix()
	}
	s.Touch()
	return nil
}

// SetCheckpoint 保存已完成步骤和下一步，供中断或重启后重新观察并续做。
func (s *State) SetCheckpoint(id, text string, now time.Time) error {
	a := s.agenda(id)
	if a == nil || a.Status == StatusCancelled || a.Status == StatusDone || a.Status == StatusSkipped {
		return errors.New("没有正在推进的这个议程项")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("要写明已完成的步骤、下一步，以及结果未知的操作")
	}
	a.Checkpoint, a.Updated = clip(text, 2000), now.Unix()
	s.Touch()
	return nil
}

// CompleteAgenda 提交已核验的完成声明，或管理员手动确认的结果。
func (s *State) CompleteAgenda(id, evidence string, now time.Time) error {
	a := s.agenda(id)
	if a == nil {
		return errors.New("没有这个议程项")
	}
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		return errors.New("要写明真实结果（做了什么、看到了什么），不能只说做完了")
	}
	a.Status, a.Result, a.Updated = StatusDone, clip(evidence, 500), now.Unix()
	if in := s.intention(a.IntentionID); in != nil && !hasOpenAgenda(s.Agenda, in.ID) {
		in.State, in.Evidence, in.Updated = IntentionDone, clip(evidence, 500), now.Unix()
		in.Progress++
	}
	s.setBelief("最近做的事："+a.Title, clip(evidence, 300), "agenda:"+a.ID, 0.8, now)
	s.Touch()
	return nil
}

// AddIdea 记一个突发想法，必须写明来源（关联的事件、记忆或聊天）。
func (s *State) AddIdea(x Idea, now time.Time, ttl time.Duration) (*Idea, error) {
	x.Text = strings.TrimSpace(x.Text)
	if x.Text == "" {
		return nil, errors.New("想法要有内容")
	}
	if strings.TrimSpace(x.Source) == "" {
		return nil, errors.New("想法要写明来源：从哪件事、哪段记忆或哪次聊天想到的")
	}
	for _, old := range s.Ideas {
		if old.Status != IdeaDropped && strings.TrimSpace(old.Text) == x.Text {
			return nil, errors.New("这个想法已经记过了（" + old.ID + "）")
		}
	}
	if x.Status == "" {
		x.Status = IdeaCandidate
	}
	if x.Interest <= 0 {
		x.Interest = 3
	}
	if x.Novelty <= 0 {
		x.Novelty = 3
	}
	if x.CostMinutes <= 0 {
		x.CostMinutes = 15
	}
	if x.CostMinutes > 120 {
		x.CostMinutes = 120
	}
	if x.ID == "" {
		x.ID = s.NewID("id")
	}
	x.Created, x.Updated = now.Unix(), now.Unix()
	if ttl > 0 {
		x.Expires = now.Add(ttl).Unix()
	}
	s.Ideas = append(s.Ideas, x)
	s.Touch()
	return &s.Ideas[len(s.Ideas)-1], nil
}

// LinkTopicBranch 记下这个话题的持久分支；同一话题再来时复用，不重复创建。
func (s *State) LinkTopicBranch(topic, sid string, now time.Time) {
	if topic == "" || sid == "" {
		return
	}
	for i := range s.TopicBranches {
		if s.TopicBranches[i].Topic == topic {
			s.TopicBranches[i].BranchSID, s.TopicBranches[i].Updated = sid, now.Unix()
			s.Touch()
			return
		}
	}
	s.TopicBranches = append(s.TopicBranches, TopicBranch{Topic: topic, BranchSID: sid, Updated: now.Unix()})
	s.Touch()
}

// BranchOfTopic 是这个话题已经在用的分支；没有返回空。
func (s *State) BranchOfTopic(topic string) (string, bool) {
	if topic == "" {
		return "", false
	}
	for _, t := range s.TopicBranches {
		if t.Topic == topic {
			return t.BranchSID, t.BranchSID != ""
		}
	}
	return "", false
}

// SetNumber 改一个可以直接编辑的心智数值（设计 §7.2）：想法的兴趣/新颖/预计耗时、
// 目标的优先级/进度、认识的置信度。范围和单位只在这一处校验，命令、网页和工具共用；
// 改完的数值立刻参与选择（想法按兴趣+新颖排序，目标按优先级排序），不是只写给人看。
func (s *State) SetNumber(id, field string, value float64, now time.Time) error {
	if value != math.Trunc(value) && field != "confidence" {
		return errors.New(field + " 只能是整数")
	}
	n := int(value)
	switch field {
	case "interest", "novelty", "cost_minutes":
		for i := range s.Ideas {
			if s.Ideas[i].ID != id {
				continue
			}
			lo, hi := 1, 5
			if field == "cost_minutes" {
				lo, hi = 1, 240
			}
			if n < lo || n > hi {
				return fmt.Errorf("%s 只能是 %d–%d", field, lo, hi)
			}
			switch field {
			case "interest":
				s.Ideas[i].Interest = n
			case "novelty":
				s.Ideas[i].Novelty = n
			default:
				s.Ideas[i].CostMinutes = n
			}
			s.Ideas[i].Updated = now.Unix()
			s.Touch()
			return nil
		}
		return errors.New("没有这个想法")
	case "priority", "progress":
		for i := range s.Intentions {
			if s.Intentions[i].ID != id {
				continue
			}
			lo, hi := 1, 5
			if field == "progress" {
				lo, hi = 0, 100
			}
			if n < lo || n > hi {
				return fmt.Errorf("%s 只能是 %d–%d", field, lo, hi)
			}
			if field == "priority" {
				s.Intentions[i].Priority = n
			} else {
				s.Intentions[i].Progress = n
			}
			s.Intentions[i].Updated = now.Unix()
			s.Touch()
			return nil
		}
		return errors.New("没有这个目标")
	case "confidence":
		if value < 0 || value > 1 {
			return errors.New("confidence 只能是 0–1")
		}
		for i := range s.Beliefs {
			if s.Beliefs[i].Subject != id {
				continue
			}
			s.Beliefs[i].Confidence = value
			s.Touch()
			return nil
		}
		return errors.New("没有这条认识")
	}
	return errors.New("可以改的数值：interest、novelty、cost_minutes、priority、progress、confidence")
}

// OpenIdeas 是现在可以用来探索的想法：候选、没被推迟、没过期。
func (s *State) OpenIdeas(now time.Time) []Idea {
	nowU := now.Unix()
	var out []Idea
	for _, x := range s.Ideas {
		if x.Status != IdeaCandidate {
			continue
		}
		if x.DeferUntil > nowU || (x.Expires > 0 && x.Expires < nowU) {
			continue
		}
		out = append(out, x)
	}
	// 选择分用心智档案的权重（默认 兴趣 1 / 新颖 1 / 成本 0，和原来的"兴趣+新颖"一致）；
	// 好奇维度打开时只调"新颖"的权重（0.5–1.5 倍），其他边界不变；同分时新的在前。
	profile := s.Profile()
	if s.runtime.Curiosity > 0 {
		profile.Weights.Novelty *= 0.5 + s.runtime.Curiosity/100
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := profile.ScoreIdea(out[i]), profile.ScoreIdea(out[j])
		if a != b {
			return a > b
		}
		return out[i].Created > out[j].Created
	})
	return out
}

// ScheduleIdea 把一个想法变成意图 + 可以挪动的探索时间块。这是“产生想法”到“决定去做”的那一步。
func (s *State) ScheduleIdea(id string, at int64, minutes int, now time.Time) (*Intention, *AgendaItem, error) {
	var idea *Idea
	for i := range s.Ideas {
		if s.Ideas[i].ID == id {
			idea = &s.Ideas[i]
			break
		}
	}
	if idea == nil {
		return nil, nil, errors.New("没有这个想法")
	}
	if idea.Status != IdeaCandidate {
		return nil, nil, errors.New("这个想法已经处理过了（" + idea.Status + "）")
	}
	if at <= 0 {
		return nil, nil, errors.New("要写什么时候做")
	}
	if minutes <= 0 || minutes > 120 {
		minutes = idea.CostMinutes
	}
	priority := idea.Interest
	if pinned, ok := s.Profile().Pin("priority"); ok {
		priority = int(pinned) // 人工锁定的优先级：想法转成目标时按锁定值
	}
	in, err := s.AddIntention(Intention{Title: idea.Text, Why: "想法：" + idea.Source, Priority: priority, NextAt: 0}, now)
	if err != nil {
		return nil, nil, err
	}
	ag, err := s.AddAgenda(AgendaItem{Title: idea.Text, Kind: KindExploration, At: at, Minutes: minutes, Movable: true,
		Source: "idea:" + idea.ID, IntentionID: in.ID, Note: idea.Source}, now)
	if err != nil {
		return nil, nil, err
	}
	in.AgendaID = ag.ID
	idea.Status, idea.IntentionID, idea.Updated = IdeaScheduled, in.ID, now.Unix()
	s.Touch()
	return in, ag, nil
}

// DropIdea 放弃一个想法（允许一个都不选）。
func (s *State) DropIdea(id, why string, now time.Time) error {
	for i := range s.Ideas {
		if s.Ideas[i].ID != id {
			continue
		}
		if s.Ideas[i].Status == IdeaScheduled {
			return errors.New("已经排进日程了，先取消那件事")
		}
		s.Ideas[i].Status, s.Ideas[i].Updated = IdeaDropped, now.Unix()
		if why != "" {
			s.Ideas[i].Source += "；放弃：" + clip(why, 100)
		}
		s.Touch()
		return nil
	}
	return errors.New("没有这个想法")
}

// DeferIdea 把想法推到之后再考虑。
func (s *State) DeferIdea(id string, until int64, now time.Time) error {
	for i := range s.Ideas {
		if s.Ideas[i].ID != id {
			continue
		}
		if until <= now.Unix() {
			return errors.New("推迟的时间要在以后")
		}
		s.Ideas[i].DeferUntil, s.Ideas[i].Updated = until, now.Unix()
		s.Touch()
		return nil
	}
	return errors.New("没有这个想法")
}

// SetPaused 暂停或恢复自主活动；恢复时立刻检查一次。
func (s *State) SetPaused(paused bool, why string, now time.Time) {
	s.Paused = paused
	s.PauseReason = why
	if !paused {
		s.NextCheck = now.Unix()
	}
	s.Touch()
}

// Recover 处理进程重启：上一次还在执行的结果未知，不重放，改成稍后重新确认。
func (s *State) Recover(now time.Time, retry time.Duration) bool {
	changed := false
	nowU := now.Unix()
	for i := range s.Intentions {
		if s.Intentions[i].State != IntentionRunning {
			continue
		}
		s.Intentions[i].State = IntentionWaiting
		s.Intentions[i].Blocked = "网关重启前有一次执行，结果未知；先重新观察，不要直接重放"
		s.Intentions[i].NextAt = now.Add(retry).Unix()
		s.Intentions[i].Updated = nowU
		changed = true
	}
	for i := range s.Agenda {
		if s.Agenda[i].Status != StatusInProgress {
			continue
		}
		s.Agenda[i].Status = StatusPlanned
		s.Agenda[i].At = now.Add(retry).Unix()
		s.Agenda[i].Result = "网关重启，上一次的结果未知"
		s.Agenda[i].Updated = nowU
		changed = true
	}
	if changed {
		s.Touch()
	}
	return changed
}

// RecordSkipped 是模型明确决定“这次不做”：不算完成，也不算失败；探索类的日程标成跳过，不再反复重试。
func (s *State) RecordSkipped(rec Execution, now time.Time) {
	rec.OK, rec.UsedTools = true, false
	if strings.TrimSpace(rec.Summary) == "" {
		rec.Summary = "这次没做"
	}
	s.Executions = append(s.Executions, rec)
	if len(s.Executions) > 50 {
		s.Executions = append([]Execution(nil), s.Executions[len(s.Executions)-50:]...)
	}
	nowU := now.Unix()
	if a := s.agenda(rec.AgendaID); a != nil {
		if a.Status == StatusCancelled || a.Status == StatusDone {
			s.Touch()
			return
		}
		a.Status, a.Result, a.Updated = StatusSkipped, clip(rec.Summary, 200), nowU
	}
	if in := s.intention(rec.IntentionID); in != nil {
		in.Updated = nowU
		if !hasOpenAgenda(s.Agenda, in.ID) {
			in.State, in.NextAt = IntentionAccepted, 0
		}
	}
	s.NextCheck = 0
	s.Touch()
}

// Retry 是一次没开始或没做成的重新安排（网关/模型暂时不可用、被中止）。
func (s *State) Retry(t Trigger, why string, now time.Time, d time.Duration) {
	if d <= 0 {
		d = 10 * time.Minute
	}
	at := now.Add(d)
	if a := s.agenda(t.AgendaID); a != nil {
		a.Status, a.At, a.Updated = StatusPlanned, at.Unix(), now.Unix()
		if why != "" {
			a.Result = clip(why, 200)
		}
	}
	if in := s.intention(t.IntentionID); in != nil {
		in.State, in.Blocked, in.NextAt, in.Updated = IntentionWaiting, clip(why, 200), at.Unix(), now.Unix()
	}
	s.NextCheck = 0
	s.Touch()
}

// NextDueTime 是下一次需要处理的时间：最近的计划 / 等待中的意图 / 自己的检查。
func (s *State) NextDueTime() (time.Time, bool) {
	var best int64
	set := func(v int64) {
		if v > 0 && (best == 0 || v < best) {
			best = v
		}
	}
	for _, a := range s.Agenda {
		if a.Status == StatusPlanned {
			set(a.At)
		}
	}
	for _, in := range s.Intentions {
		if in.State == IntentionAccepted || in.State == IntentionWaiting {
			set(in.NextAt)
		}
	}
	set(s.NextCheck)
	if best == 0 {
		return time.Time{}, false
	}
	return time.Unix(best, 0), true
}

// SetNextCheck 记下下次自己检查的时间。
func (s *State) SetNextCheck(t time.Time) {
	v := t.Unix()
	if s.NextCheck != v {
		s.NextCheck = v
		s.Touch()
	}
}

// SetBudgetLimit 记下当天的预算上限（跟着 agent 设置走，供网页显示）。
func (s *State) SetBudgetLimit(limit int) {
	if s.Budget.Limit != limit {
		s.Budget.Limit = limit
		s.Touch()
	}
}

// setBelief 更新一条认识（同 subject 覆盖），最多留 40 条。
func (s *State) setBelief(subject, value, source string, confidence float64, now time.Time) {
	subject = clip(strings.TrimSpace(subject), 80)
	if subject == "" {
		return
	}
	b := Belief{Subject: subject, Value: clip(strings.TrimSpace(value), 300), Source: source, Confidence: confidence, At: now.Unix()}
	for i := range s.Beliefs {
		if s.Beliefs[i].Subject == subject {
			s.Beliefs[i] = b
			return
		}
	}
	s.Beliefs = append(s.Beliefs, b)
	if len(s.Beliefs) > 40 {
		s.Beliefs = append([]Belief(nil), s.Beliefs[len(s.Beliefs)-40:]...)
	}
}

// AddInterest 记一个长期兴趣；必须有来源，同一条不重复记。
func (s *State) AddInterest(text, source string, now time.Time) (*Interest, error) {
	text, source = strings.TrimSpace(text), strings.TrimSpace(source)
	if text == "" {
		return nil, errors.New("兴趣要有内容")
	}
	if source == "" {
		return nil, errors.New("兴趣要写来源：从哪次经历、哪段聊天或哪次反思来的")
	}
	for _, old := range s.Self.Interests {
		if strings.EqualFold(strings.TrimSpace(old.Text), text) {
			return nil, errors.New("这个兴趣已经记过了：" + old.Text)
		}
	}
	x := Interest{Text: clip(text, 100), Source: clip(source, 200), At: now.Unix()}
	s.Self.Interests = append(s.Self.Interests, x)
	if len(s.Self.Interests) > 20 {
		s.Self.Interests = append([]Interest(nil), s.Self.Interests[len(s.Self.Interests)-20:]...)
	}
	s.Self.Updated = now.Unix()
	s.Touch()
	return &s.Self.Interests[len(s.Self.Interests)-1], nil
}

// RemoveInterest 删掉一条长期兴趣（不想再追了）。
func (s *State) RemoveInterest(text string, now time.Time) bool {
	text = strings.TrimSpace(text)
	for i, old := range s.Self.Interests {
		if !strings.EqualFold(strings.TrimSpace(old.Text), text) {
			continue
		}
		s.Self.Interests = append(s.Self.Interests[:i], s.Self.Interests[i+1:]...)
		s.Self.Updated = now.Unix()
		s.Touch()
		return true
	}
	return false
}

// SetBelief 是外部（聊天、执行结果）更新认识时用的。
func (s *State) SetBelief(subject, value, source string, confidence float64, now time.Time) {
	s.setBelief(subject, value, source, confidence, now)
	s.Touch()
}

// AddReflection 记一条反思。
func (s *State) AddReflection(text, change string, now time.Time) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.Reflections = append(s.Reflections, Reflection{At: now.Unix(), Text: clip(text, 800), Change: clip(change, 300)})
	if len(s.Reflections) > 30 {
		s.Reflections = append([]Reflection(nil), s.Reflections[len(s.Reflections)-30:]...)
	}
	s.Touch()
}

// Prune 清掉太老的历史，避免文件无限长大。
func (s *State) Prune(now time.Time) {
	nowU := now.Unix()
	week, month := int64(7*24*3600), int64(30*24*3600)
	var agenda []AgendaItem
	for _, a := range s.Agenda {
		closed := a.Status == StatusDone || a.Status == StatusCancelled || a.Status == StatusSkipped
		if closed && a.Updated > 0 && nowU-a.Updated > week {
			continue
		}
		agenda = append(agenda, a)
	}
	s.Agenda = agenda
	var intents []Intention
	for _, in := range s.Intentions {
		closed := in.State == IntentionDone || in.State == IntentionCancelled || in.State == IntentionFailed
		if closed && in.Updated > 0 && nowU-in.Updated > month {
			continue
		}
		intents = append(intents, in)
	}
	s.Intentions = intents
	var ideas []Idea
	for _, x := range s.Ideas {
		closed := x.Status == IdeaDropped || (x.Expires > 0 && x.Expires < nowU && x.Status == IdeaCandidate)
		if closed && x.Updated > 0 && nowU-x.Updated > month {
			continue
		}
		ideas = append(ideas, x)
	}
	s.Ideas = ideas
	s.pruneEvents(now)
}

func (s *State) pruneEvents(now time.Time) {
	if len(s.Events) <= 300 {
		return
	}
	var keep []Event
	cut := now.Add(-7 * 24 * time.Hour).Unix()
	for i, e := range s.Events {
		if !e.Processed && i >= len(s.Events)-100 {
			keep = append(keep, e)
			continue
		}
		if e.At >= cut {
			keep = append(keep, e)
		}
	}
	if len(keep) > 300 {
		keep = keep[len(keep)-300:]
	}
	s.Events = keep
}

func (s *State) agenda(id string) *AgendaItem {
	if id == "" {
		return nil
	}
	for i := range s.Agenda {
		if s.Agenda[i].ID == id {
			return &s.Agenda[i]
		}
	}
	return nil
}

func (s *State) intention(id string) *Intention {
	if id == "" {
		return nil
	}
	for i := range s.Intentions {
		if s.Intentions[i].ID == id {
			return &s.Intentions[i]
		}
	}
	return nil
}

// AgendaByID / IntentionByID 是只读查找（给网关的工具和网页用）。
func (s *State) AgendaByID(id string) (AgendaItem, bool) {
	if a := s.agenda(id); a != nil {
		return *a, true
	}
	return AgendaItem{}, false
}

func (s *State) IntentionByID(id string) (Intention, bool) {
	if in := s.intention(id); in != nil {
		return *in, true
	}
	return Intention{}, false
}

// IdeaByID 查找一个想法。
func (s *State) IdeaByID(id string) (Idea, bool) {
	for _, x := range s.Ideas {
		if x.ID == id {
			return x, true
		}
	}
	return Idea{}, false
}

// OpenAgenda 是当前（未结束）的议程。
func (s *State) OpenAgenda() []AgendaItem {
	var out []AgendaItem
	for _, a := range s.Agenda {
		if a.Status == StatusPlanned || a.Status == StatusInProgress {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// ActiveIntentions 是还没结束的意图。
func (s *State) ActiveIntentions() []Intention {
	var out []Intention
	for _, in := range s.Intentions {
		switch in.State {
		case IntentionAccepted, IntentionRunning, IntentionWaiting, IntentionPaused:
			out = append(out, in)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

func validKind(k string) bool {
	return k == KindCommitment || k == KindCheckup || k == KindPlan || k == KindExploration
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Runtime 是这次规划时启用的维度影响（不落盘）：没启用的维度是 0，行为保持旧样。
// 映射是固定的、可解释的：curiosity 调自选探索排序，social_drive 调联系倾向，
// focus 调同时推进目标的软偏好，energy 由网关按间隔消费（设计 §7.3）。
type Runtime struct {
	Curiosity   float64
	SocialDrive float64
	Focus       float64
	Energy      float64
}

// SetRuntime 在规划前注入当前生效的维度值。调用方持有锁。
func (s *State) SetRuntime(r Runtime) { s.runtime = r }

// RuntimeOf 返回当前注入的维度影响（只读）。
func (s *State) RuntimeOf() Runtime { return s.runtime }
