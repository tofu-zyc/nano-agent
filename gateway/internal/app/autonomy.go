package app

// 拟人型 agent 的自主活动（v0.7.3，docs/PLAN-agent-autonomy.md）。
//
// 心智状态（目标、想法、日程、执行记录）在 internal/autonomy；这里把它接进现有的网关：
//   - 事件来源：聊天、FollowUp、dream、日程到点、执行结果、主人叫醒；
//   - 每次只选一件到期的事，在 @<agent>/life 里跑一轮（复用模型循环、工具、记忆）；
//   - 真实执行结果写回状态，再决定下次什么时候看；
//   - 内心决策、资源占用、主动联系分开判断：不再要求“最近 20 分钟没人找”。
//
// 旧版 life.json 继续保留（近况、主动找了谁、FollowUp），只把“什么时候醒”交给心智状态。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nano-agent/gateway/internal/autonomy"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/experience"
	"nano-agent/gateway/internal/knowledge"
	"nano-agent/gateway/internal/proto"
)

// mindRun 是一次自主活动的一轮：结束时把结果写回哪个议程 / 意图。
type mindRun struct {
	AgentID string
	Trigger autonomy.Trigger
	Started time.Time
	Manual  bool
	Source  string // 账本来源：exploration / commitment / hook / manual
	Ledger  string // 账本预留身份：跨日结算和重复回调靠它对齐
}

type autonomyDoc struct {
	Version int                        `json:"version"`
	Agents  map[string]*autonomy.State `json:"agents"`
}

func (a *App) autonomyFile() string { return filepath.Join(a.store.Dir(), "autonomy.json") }

// loadMinds 从 data/autonomy.json 读入所有 agent 的心智状态（只读一次）。调用方持有锁。
func (a *App) loadMinds() {
	if a.mindsLoaded {
		return
	}
	a.mindsLoaded = true
	if a.minds == nil {
		a.minds = map[string]*autonomy.State{}
	}
	b, err := os.ReadFile(a.autonomyFile())
	if err != nil {
		return
	}
	var doc autonomyDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		// 读不动就当成"不知道现在是什么"，绝不拿空状态覆盖原文件
		a.mindsErr = fmt.Errorf("自主状态解析失败（%v），先不覆盖：%s", err, a.autonomyFile())
		a.log.Error(a.mindsErr.Error())
		return
	}
	needsMigration := doc.Version < autonomy.SchemaVersion
	if doc.Version > autonomy.SchemaVersion {
		a.mindsErr = fmt.Errorf("自主状态版本过新，拒绝降级覆盖")
		a.log.Error(a.mindsErr.Error())
		return
	}
	for _, st := range doc.Agents {
		if st != nil {
			if st.SchemaVersion > autonomy.SchemaVersion || doc.Version > autonomy.SchemaVersion {
				a.mindsErr = fmt.Errorf("自主状态版本过新，拒绝降级覆盖")
				a.log.Error(a.mindsErr.Error())
				return
			}
			needsMigration = needsMigration || st.SchemaVersion < autonomy.SchemaVersion
		}
	}
	if needsMigration {
		backup := a.autonomyFile() + ".schema1.bak"
		if saved, readErr := os.ReadFile(backup); readErr == nil {
			if !json.Valid(saved) {
				a.mindsErr = fmt.Errorf("旧自主状态备份损坏，保留现场")
				return
			}
		} else if errors.Is(readErr, os.ErrNotExist) {
			tmp := backup + ".tmp"
			f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
			if err == nil {
				_, err = f.Write(b)
				if err == nil {
					err = f.Sync()
				}
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
			}
			if err == nil {
				err = os.Rename(tmp, backup)
			}
			if err != nil {
				a.mindsErr = fmt.Errorf("自主状态迁移备份失败：%w", err)
				a.log.Error(a.mindsErr.Error())
				return
			}
		} else {
			a.mindsErr = readErr
			return
		}

	}
	for id, st := range doc.Agents {
		if st != nil {
			st.Normalize()
			a.minds[id] = st
		}
	}
	if needsMigration {
		a.saveMinds()
	}
}

// saveMinds 把所有 agent 的心智状态写盘（原子替换）。调用方持有锁。
func (a *App) saveMinds() {
	if a.minds == nil || a.mindsErr != nil {
		return
	}
	doc := autonomyDoc{Version: autonomy.SchemaVersion, Agents: a.minds}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		a.log.Warn("自主活动状态序列化失败", "err", err)
		return
	}
	p := a.autonomyFile()
	tmp := p + ".tmp"
	if err = os.WriteFile(tmp, b, 0o600); err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		a.log.Warn("保存自主活动状态失败", "err", err)
	}
}

// mind 是 agent 的心智状态；第一次用时从磁盘读、导入旧近况、处理重启。调用方持有锁。
func (a *App) mind(agentID string) *autonomy.State {
	a.loadMinds()
	if a.minds == nil {
		a.minds = map[string]*autonomy.State{}
	}
	st := a.minds[agentID]
	if st == nil {
		st = autonomy.New()
		a.minds[agentID] = st
	}
	st.Normalize()
	now := a.now().In(a.loc)
	// Recent 已改为经历索引；不再从旧近况制造心智事件。
	if st.Migrated&1 == 0 {
		st.Migrated |= 1
		st.Touch()
	}
	// 重启恢复：上次没结束的执行结果未知，不重放，改成稍后重新确认（每个进程启动做一次）
	if !a.mindsRecovered[agentID] {
		if a.mindsRecovered == nil {
			a.mindsRecovered = map[string]bool{}
		}
		a.mindsRecovered[agentID] = true
		if st.Recover(now, 5*time.Minute) {
			a.saveMinds()
		}
	}
	st.RollDay(now, a.loc)
	return st
}

// mindOf 是给网页和查询用的只读入口（会顺带补齐设置里的预算上限）。
func (a *App) mindOf(agentID string) *autonomy.State {
	st := a.mind(agentID)
	if ag, ok := a.agentByID(agentID); ok {
		st.SetBudgetLimit(pickInt(ag.LifeBudget, 0))
	}
	return st
}

// publishAutonomy 通知网页某个 agent 的自主活动状态变了。
func (a *App) publishAutonomy(agentID string) {
	a.publish(events.Event{T: "autonomy", Name: agentID})
}

// autonomyView 是 agent 节点上的自主活动概况；工具型 agent 没有。
func (a *App) autonomyView(ag agents.Agent) *autonomy.View {
	if !ag.Life() || a.agentRoot == "" {
		return nil
	}
	st := a.mindOf(ag.ID)
	v := st.View(a.now().In(a.loc), a.loc, 6)
	return &v
}

// mindHeard 保留旧调用兼容；聊天只在真实轮次结束后写经历，不产生待办或唤醒。
func (a *App) mindHeard(sid, who, name, text string) {}

// FollowUp 由原聊天自己触发，不再复制成内心的认识或事件。
func (a *App) mindFollowUp(sid, agentID, note string, at time.Time) {}

// mindDream 是整理完记忆之后的反思：经验写进心智状态，下一次规划能看到。
func (a *App) mindDream(agentID, note string) {
	if a.agentRoot == "" {
		return
	}
	st := a.mind(agentID)
	now := a.now().In(a.loc)
	st.AddReflection(note, "整理记忆后更新认识", now)
	st.AddEvent(autonomy.Event{Kind: autonomy.EventReflection, Source: "dream:" + agentID, Dedupe: "dream:" + now.Format("2006-01-02"), Text: cutRunes(note, 200)}, now, 0)
	a.saveMinds()
	a.publishAutonomy(agentID)
}

// mindDropSession 是会话交给别的 agent 时，旧 agent 关于这个聊天的约定 / 复查作废。调用方持有锁。
func (a *App) mindDropSession(sid, agentID string) {
	if a.agentRoot == "" || agentID == "" {
		return
	}
	st := a.mind(agentID)
	now := a.now()
	changed := false
	for _, ag := range st.OpenAgenda() {
		if ag.Kind != autonomy.KindCommitment && ag.Kind != autonomy.KindCheckup {
			continue
		}
		if strings.Contains(ag.Source, sid) || strings.Contains(ag.Note, sid) {
			if err := st.CancelAgenda(ag.ID, "这个会话已经交给别的 agent 了", now); err == nil {
				changed = true
			}
		}
	}
	if changed {
		a.saveMinds()
		a.publishAutonomy(agentID)
	}
}

// lifeBegin 仍是“刚变成拟人型/第一次启动，安排第一次自己活动”：5–15 分钟后。
func (a *App) lifeBegin(ag agents.Agent) {
	if !ag.Life() || a.agentRoot == "" {
		return
	}
	ls := a.lifeOf(ag.ID)
	st := a.mind(ag.ID)
	if ls.Next == 0 {
		ls.Next = a.now().Add(5*time.Minute + time.Duration(a.random()*float64(10*time.Minute))).Unix()
	}
	// 心智状态的下次检查和旧的 Next 保持同一个时间
	if st.NextCheck == 0 {
		st.SetNextCheck(time.Unix(ls.Next, 0))
	}
	a.saveLife()
	a.saveMinds()
	a.publish(events.Event{T: "life", Name: ag.ID, Kind: "next"})
}

// lifeBlocked 是旧版（固定间隔醒来）的说明，保留给状态显示；20 分钟空闲门槛已经取消。
// 新的判断在 autonomy.Trigger.CanStart 里，按约定/探索、作息、次数、预算分别判断。
func (a *App) lifeBlocked(ag agents.Agent, ls *lifeState, now time.Time) string {
	if !a.awakeOf(ag).contains(now) {
		return "睡着"
	}
	if b := pickInt(ag.LifeBudget, 0); b > 0 && ls.Tokens >= b {
		return "今天的预算用完了"
	}
	if s := a.sessions[lifeSID(ag.ID)]; s != nil && (s.run != nil || len(s.queue) > 0 || s.compacting) {
		return "正在想事情"
	}
	if p := effectiveLifePolicy(ag); !p.WakesUnlimited() && ls.Wakes >= p.MaxWakes {
		return "今天醒够了"
	}
	return ""
}

// lifeTick 改成事件驱动的自主活动调度：到期的约定/计划/复查/想法，或自己安排的检查。
// 调用方不持有锁。
func (a *App) lifeTick() {
	a.autonomyTick()
}

// autonomyTick 看一遍所有拟人型 agent：有没有该处理的约定、计划、想法。
// 普通时钟扫描只检查到期的事，不要求每次扫描都调用模型。
func (a *App) autonomyTick() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentStore == nil || a.agentRoot == "" {
		return
	}
	now := a.now().In(a.loc)
	if a.bootAt.IsZero() {
		a.bootAt = now // 算“多久没人找它”的起点
	}
	for _, ag := range a.agentStore.List() {
		if !ag.Life() {
			continue
		}
		a.autonomyAgent(ag, now)
	}
}

// autonomyAgent 处理一个 agent 的到期事项。调用方持有锁。
func (a *App) autonomyAgent(ag agents.Agent, now time.Time) {
	a.loadMinds()
	if a.mindsErr != nil {
		return
	}
	ls := a.lifeOf(ag.ID)
	// 旧版约好的回访（FollowUp）仍然是自己的执行者，先处理
	a.followUpTick(ag, ls, now)
	st := a.mind(ag.ID)
	limit := pickInt(ag.LifeBudget, 0)
	st.SetBudgetLimit(limit)
	if st.RollDay(now, a.loc) {
		ls.Tokens = 0
		a.saveLife()
	}
	if st.Paused {
		a.mirrorNextCheck(st, ls)
		return
	}
	if ls.Next == 0 && st.NextCheck == 0 {
		a.lifeBegin(ag) // 刚变成拟人型 / 第一次启动：安排第一次自己活动
	}
	a.syncDimensions(ag)
	st.SetRuntime(a.dimensionRuntime(ag))
	trig := st.Next(now)
	if trig == nil {
		a.mirrorNextCheck(st, ls)
		return
	}
	// 资源占用：内心会话正在跑 / 整理上下文，等一会儿再看（不丢意图）
	s := a.sessOf(lifeSID(ag.ID))
	if s.run != nil || len(s.queue) > 0 || s.compacting {
		st.SetNextCheck(now.Add(2 * time.Minute))
		a.saveMinds()
		return
	}
	if a.models != nil && len(a.models.Models()) == 0 {
		st.Retry(*trig, "还没有配置模型", now, 30*time.Minute)
		a.saveMinds()
		return
	}
	if a.route(a.workspaceOf(s.sid)).kernel == "" {
		st.Retry(*trig, "没有在线的内核", now, 5*time.Minute)
		a.saveMinds()
		return
	}
	opts := autonomy.StartOptions{Paused: st.Paused, Awake: a.awakeOf(ag).contains(now), BudgetLeft: st.BudgetLeft(limit),
		Wakes: ls.Wakes, MaxWakes: effectiveLifePolicy(ag).MaxWakes}
	if ok, why := trig.CanStart(opts); !ok {
		next := now.Add(15 * time.Minute)
		switch {
		case strings.Contains(why, "睡着"):
			if w := a.awakeOf(ag); !w.contains(now) {
				next = w.nextStart(now)
			}
		case strings.Contains(why, "醒够了"):
			next = now.Add(30 * time.Minute)
		case strings.Contains(why, "预算"):
			next = now.Add(time.Hour)
		}
		st.SetNextCheck(next)
		a.log.Info("自主活动先不做", "agent", ag.ID, "kind", trig.Kind, "why", why)
		a.saveMinds()
		a.publishAutonomy(ag.ID)
		return
	}
	if err := a.startMindLocked(ag, st, *trig, false); err != nil {
		a.log.Warn("自主活动没能开始", "agent", ag.ID, "err", err)
	}
}

// mirrorNextCheck 把心智状态的下次检查同步到旧版 life.json（网页、状态都用它显示）。
func (a *App) mirrorNextCheck(st *autonomy.State, ls *lifeState) {
	next, ok := st.NextDueTime()
	if st.Paused || !ok {
		return
	}
	if ls.Next != next.Unix() {
		ls.Next = next.Unix()
		a.saveLife()
	}
}

// startLife 是 /wake、网页按钮和旧调用入口：让 agent 现在自主活动一次。
// manual 表示主人叫醒的，不算每天次数、不看作息；仍然受模型/内核可用性的限制。
func (a *App) startLife(agentID string, manual bool) error {
	a.loadMinds()
	if a.mindsErr != nil {
		return a.mindsErr
	}
	ag, ok := a.agentByID(agentID)
	if !ok {
		return errors.New("没有这个 agent")
	}
	if !ag.Life() {
		return errors.New("只有拟人型 agent 有内心活动（在 agent 节点里把类型改成拟人型）")
	}
	s := a.sessOf(lifeSID(agentID))
	if s.run != nil || len(s.queue) > 0 || s.compacting {
		return errors.New("它正在想事情")
	}
	if a.models != nil && len(a.models.Models()) == 0 {
		return errors.New("还没有配置模型")
	}
	st := a.mind(agentID)
	now := a.now().In(a.loc)
	trig := autonomy.Trigger{Kind: autonomy.EventManualWake, Reason: "主人把你叫醒了", Due: now.Unix()}
	if !manual {
		if t := st.Next(now); t != nil {
			trig = *t
		} else {
			trig = autonomy.Trigger{Kind: autonomy.EventSelfCheck, Reason: "自己醒一会儿", Due: now.Unix()}
		}
	}
	if a.route(a.workspaceOf(s.sid)).kernel == "" {
		return errors.New("没有在线的内核")
	}
	opts := autonomy.StartOptions{Paused: st.Paused, Awake: a.awakeOf(ag).contains(now),
		BudgetLeft: st.BudgetLeft(pickInt(ag.LifeBudget, 0)), Wakes: a.lifeOf(agentID).Wakes,
		MaxWakes: effectiveLifePolicy(ag).MaxWakes}
	if manual {
		opts.Paused, opts.Awake, opts.BudgetLeft = false, true, -1
	}
	if ok, why := trig.CanStart(opts); !ok {
		return errors.New(why)
	}
	return a.startMindLocked(ag, st, trig, manual)
}

// dimensionLabel 是维度的中文名（只用于显示，字段名仍然是 curiosity 这些）。
func dimensionLabel(name string) string {
	switch name {
	case "curiosity":
		return "好奇"
	case "social_drive":
		return "社交"
	case "focus":
		return "专注"
	case "energy":
		return "精力"
	}
	return name
}

// profileLine 是心声简报和 /autonomy 共用的一行心智档案：权重 + 已启用的维度。
func profileLine(p autonomy.MindProfile) string {
	var dims []string
	for _, name := range autonomy.DimensionNames {
		if d, ok := p.Dimensions[name]; ok && d.Enabled {
			label := fmt.Sprintf("%s %.0f", dimensionLabel(name), d.Value)
			if d.Threshold > 0 {
				label += fmt.Sprintf("（%.0f 会提醒）", d.Threshold)
			}
			dims = append(dims, label)
		}
	}
	line := fmt.Sprintf("你现在的倾向：%s", strings.Join(dims, " · "))
	if len(dims) == 0 {
		line = "可选的心智维度都没打开"
	}
	return line + "（0–100 的自评，不是真实生理状态）"
}

// effectiveDimensions 把 agent 配置里的维度默认值合并进当前档案：
// 配置管默认值、范围和漂移，状态只管当前值、计算时刻和迟滞。调用方持有锁。
func (a *App) effectiveDimensions(ag agents.Agent) map[string]autonomy.Dimension {
	st := a.mind(ag.ID)
	profile := st.Profile()
	merged := map[string]autonomy.Dimension{}
	for name, d := range profile.Dimensions {
		merged[name] = d
	}
	for name, cfg := range ag.MindDimensions {
		d, ok := merged[name]
		if !ok {
			d = autonomy.Dimension{Value: cfg.Initial, Baseline: cfg.Initial, Min: 0, Max: 100}
		}
		d.Enabled = cfg.Enabled
		if cfg.Initial != 0 && !ok {
			d.Value = cfg.Initial
		}
		if cfg.Baseline != 0 {
			d.Baseline = cfg.Baseline
		}
		if cfg.Max > cfg.Min {
			d.Min, d.Max = cfg.Min, cfg.Max
		}
		d.Drift = cfg.Drift
		d.Threshold = cfg.Threshold
		d.Rearm = cfg.Rearm
		d.AllowDelta = cfg.AllowDelta
		d.MaxDelta = cfg.MaxDelta
		if d.MaxDelta <= 0 {
			d.MaxDelta = 5
		}
		merged[name] = d
	}
	return merged
}

// syncDimensions 把 agent 配置里的维度写进持久档案（第一次启用时用 initial），
// 之后漂移、阈值和显示都只看状态里的值，配置改动也会在这里对齐（评审 §25 S3）。
// 调用方持有锁。
func (a *App) syncDimensions(ag agents.Agent) {
	if len(ag.MindDimensions) == 0 {
		return
	}
	st := a.mind(ag.ID)
	profile := st.Profile()
	now := a.now()
	for name, cfg := range ag.MindDimensions {
		d, ok := profile.Dimensions[name]
		changed := false
		if !ok {
			baseline := cfg.Baseline
			if baseline == 0 {
				baseline = 50
			}
			d = autonomy.Dimension{Value: baseline, Baseline: baseline, Min: 0, Max: 100}
			changed = true
		}
		if cfg.Initial != 0 && !ok {
			d.Value, changed = cfg.Initial, true
		}
		if d.Enabled != cfg.Enabled {
			d.Enabled, changed = cfg.Enabled, true
		}
		if cfg.Baseline != 0 && d.Baseline != cfg.Baseline {
			d.Baseline, changed = cfg.Baseline, true
		}
		if cfg.Max > cfg.Min && (d.Min != cfg.Min || d.Max != cfg.Max) {
			d.Min, d.Max, changed = cfg.Min, cfg.Max, true
		}
		if d.Drift != cfg.Drift {
			d.Drift, changed = cfg.Drift, true
		}
		if d.Threshold != cfg.Threshold {
			d.Threshold, changed = cfg.Threshold, true
		}
		if d.Rearm != cfg.Rearm {
			d.Rearm, changed = cfg.Rearm, true
		}
		if d.AllowDelta != cfg.AllowDelta {
			d.AllowDelta, changed = cfg.AllowDelta, true
		}
		if cfg.MaxDelta > 0 && d.MaxDelta != cfg.MaxDelta {
			d.MaxDelta, changed = cfg.MaxDelta, true
		}
		if !changed {
			continue
		}
		if err := st.SetDimension(name, d, now.Unix()); err != nil {
			a.log.Warn("维度配置没有生效", "agent", ag.ID, "dimension", name, "err", err)
		}
	}
}

// enabledDimension 返回某个维度的当前值，以及它是不是真的开着。调用方持有锁。
func (a *App) enabledDimension(ag agents.Agent, name string) (float64, bool) {
	d, ok := a.effectiveDimensions(ag)[name]
	if !ok || !d.Enabled {
		return 0, false
	}
	return d.Value, true
}

// dimensionRuntime 把已启用的维度整理成这次规划用的影响值；没启用的就是 0（旧行为）。
func (a *App) dimensionRuntime(ag agents.Agent) autonomy.Runtime {
	var rt autonomy.Runtime
	for name, d := range a.effectiveDimensions(ag) {
		if !d.Enabled {
			continue
		}
		switch name {
		case "curiosity":
			rt.Curiosity = d.Value
		case "social_drive":
			rt.SocialDrive = d.Value
		case "focus":
			rt.Focus = d.Value
		case "energy":
			rt.Energy = d.Value
		}
	}
	return rt
}

// autoActivityRoom 是"agent 自己还能不能再开一轮"的统一准入：暂停、当天总预算。
// 内心活动、自主分支的继续、异步 Fork 完成后启动和排队后放行都走这里（评审 §24 R2）。
// 调用方持有锁。
func (a *App) autoActivityRoom(agentID string) error {
	ag, ok := a.agentByID(agentID)
	if !ok {
		return errors.New("没有这个 agent")
	}
	st := a.mind(agentID)
	if st.Paused {
		return errors.New("自主活动暂停了，这次不开始")
	}
	limit := pickInt(ag.LifeBudget, 0)
	st.SetBudgetLimit(limit)
	if limit > 0 && st.BudgetLeft(limit) == 0 {
		return fmt.Errorf("今天的自主活动预算用完了（%d token），这次不开始", limit)
	}
	return nil
}

// ledgerSource 是这次自主活动记到账本的哪个来源：主人叫醒 > Hook > 约定/排好的事 > 自选探索。
func ledgerSource(trig autonomy.Trigger, manual, hook bool) string {
	switch {
	case manual:
		return autonomy.LedgerManual
	case hook:
		return autonomy.LedgerHook
	case trig.IsCommitment() || !trig.IsExploration():
		return autonomy.LedgerCommitment
	default:
		return autonomy.LedgerExploration
	}
}

// migrateLifeLedger 把旧 life.json 的当日次数和 token 导一次进账本：
// 升级当天不该显示成 0，只导一次（账本这一天已经有记录就不再动）。
func (a *App) migrateLifeLedger(st *autonomy.State, ls *lifeState, now time.Time) {
	if ls == nil || (ls.Wakes == 0 && ls.Tokens == 0) {
		return
	}
	st.SeedLedger(autonomy.LedgerExploration, ls.Wakes, ls.Tokens, now, a.loc)
}

// startMindLocked 开一轮自主活动：内心会话、交接用的说明、状态先标成进行中。调用方持有锁。
func (a *App) startMindLocked(ag agents.Agent, st *autonomy.State, trig autonomy.Trigger, manual bool) error {
	return a.startMindWithHookLocked(ag, st, trig, manual, nil)
}
func (a *App) startMindWithHookLocked(ag agents.Agent, st *autonomy.State, trig autonomy.Trigger, manual bool, hook *hookInvocation) error {
	if item, ok := st.AgendaByID(trig.AgendaID); ok && item.ScopeSet {
		for _, id := range item.Workspaces {
			if a.findWorkspace(id) == nil {
				st.Retry(trig, "创建日程时的工作目录已删除："+id+"；请重新安排目录，不能自动改到 HOME 执行", a.now(), 5*time.Minute)
				a.saveMinds()
				return fmt.Errorf("日程的工作目录 %s 已删除", id)
			}
		}
	}
	s := a.sessOf(lifeSID(ag.ID))
	if s.run != nil || len(s.queue) > 0 || s.compacting {
		return errors.New("它正在想事情")
	}
	now := a.now().In(a.loc)
	ls := a.lifeOf(ag.ID)
	// 旧 life.json 的当日次数先进账本，再记这一次（顺序反了会把这一次数两遍）
	a.migrateLifeLedger(st, ls, now)
	a.syncDimensions(ag)
	// 先记次数，这样这次醒来的说明里能看到“第几次”；开始失败时下面会退回去
	if !manual && trig.IsExploration() {
		ls.Wakes++
	}
	if err := a.store.BeginEpisode(s.sid); err != nil {
		if !manual && trig.IsExploration() {
			ls.Wakes--
		}
		return fmt.Errorf("开启内心片段失败：%w", err)
	}
	source := ledgerSource(trig, manual, hook != nil)
	resID := fmt.Sprintf("mind-%s-%d", ag.ID, now.UnixNano())
	st.ReserveLedger(source, resID, now, a.loc)
	note := a.mindNote(ag, st, trig, now, manual)
	pending := st.UnprocessedEvents(20)
	ids := make([]string, 0, len(pending))
	for _, e := range pending {
		ids = append(ids, e.ID)
	}
	st.MarkStarted(trig, now)
	ls.Next = now.Add(a.lifeInterval(ag, now)).Unix()
	a.saveMinds()
	a.saveLife()
	s.mind = &mindRun{AgentID: ag.ID, Trigger: trig, Started: now, Manual: manual, Source: source, Ledger: resID}
	who := "life:" + ag.ID
	if hook != nil {
		who = hook.work.Delivery.Creator
	}
	s.queue = append(s.queue, queued{hook: hook, ep: endpoint{ch: "life", chat: ag.ID}, who: who, inner: true,
		m: channel.Message{Text: note}})
	a.startRun(s)
	if s.run == nil {
		s.queue = nil
		s.mind = nil
		st.ReleaseLedger(resID)
		if !manual && trig.IsExploration() {
			ls.Wakes--
		}
		st.Retry(trig, "没能开始（没有可用的模型或在线的内核）", now, 5*time.Minute)
		a.saveMinds()
		a.saveLife()
		return errors.New("没能开始：没有可用的模型或在线的内核")
	}
	st.MarkProcessed(ids)
	a.saveMinds()
	a.log.Info("自主活动开始", "agent", ag.ID, "kind", trig.Kind, "agenda", trig.AgendaID, "intention", trig.IntentionID, "manual", manual)
	a.publish(events.Event{T: "life", Name: ag.ID, Kind: "start"})
	a.publishAutonomy(ag.ID)
	return nil
}

// mindNote 是这一次醒来看到的：为什么醒、现在几点、心智状态、能找谁、能做什么。
func (a *App) mindNote(ag agents.Agent, st *autonomy.State, trig autonomy.Trigger, now time.Time, manual bool) string {
	var b strings.Builder
	switch {
	case manual:
		b.WriteString("[系统] 主人把你叫醒了。")
	case trig.AgendaID != "":
		if item, ok := st.AgendaByID(trig.AgendaID); ok {
			fmt.Fprintf(&b, "[系统] 你安排的时间到了：%s（%s）。", item.Title, autonomy.KindLabel(item.Kind))
			fmt.Fprintf(&b, "当前日程 ID：%s。\n", item.ID)
			if item.Note != "" {
				fmt.Fprintf(&b, "任务说明：%s\n", item.Note)
			}
			if item.Criteria != "" {
				fmt.Fprintf(&b, "完成条件：%s\n", item.Criteria)
			}
			if item.Checkpoint != "" {
				fmt.Fprintf(&b, "上次的检查点（先观察，再接着做）：%s\n", item.Checkpoint)
			}
		} else {
			b.WriteString("[系统] 你安排的时间到了。")
		}
	case trig.Kind == autonomy.EventSelfCheck:
		b.WriteString("[系统] 到了你自己安排的下次检查时间。")
	case trig.IntentionID != "":
		b.WriteString("[系统] 接着推进你之前决定的事。")
	default:
		b.WriteString("[系统] 有件事需要你处理。")
	}
	fmt.Fprintf(&b, "现在是 %s %s。", now.Format("01-02"), now.Format("15:04"))
	if !a.awakeOf(ag).contains(now) {
		b.WriteString("这是你平常睡觉的时间，只有明确的约定才现在做。")
	}
	ls := a.lifeOf(ag.ID)
	p := effectiveLifePolicy(ag)
	fmt.Fprintf(&b, "%s；%s。\n", p.wakeText(ls.Wakes), p.reachText(ls.Reaches))
	fmt.Fprintf(&b, "【要处理的】%s\n", trig.Reason)
	b.WriteString(st.BriefForModel(now, a.loc))
	for sid, f := range a.lifeOf(ag.ID).FollowUps {
		fmt.Fprintf(&b, "\n【聊天回访】%s %s：%s；到点会在那个聊天里醒来，由那边处理，不是本片段的任务。\n", sid, time.Unix(f.At, 0).In(a.loc).Format("01-02 15:04"), f.Note)
	}
	b.WriteString("\n【最近的经历】以下都已经在当时的场合处理结束，只供了解，不是给你的任务；其中的上下文或会话指当时那个聊天。\n")
	b.WriteString(experience.Render(a.experienceOf(ag.ID).Recent(knowledge.Viewer{Admin: true}, "", 12, now), a.loc, 4500))

	if targets := a.lifeTargets(ag); len(targets) > 0 {
		b.WriteString("\n可以找的聊天（Message 的 to 写括号里的）：\n")
		for _, t := range targets {
			fmt.Fprintf(&b, "- %s（%s）", t.Name, t.To)
			if t.Last != "" {
				b.WriteString("：" + t.Last)
			}
			if at, waiting := ls.Awaiting[strings.TrimPrefix(t.To, "chat:")]; waiting {
				fmt.Fprintf(&b, "。你 %s 主动找过他，他还没回，先别再找他", agoText(now, at))
			}
			b.WriteString("\n")
		}
	}
	if len(a.searchTools()) == 0 {
		b.WriteString("\n（网络搜索没开，这次上不了网。）")
	}
	if !a.memoryOn(ag) {
		b.WriteString("\n（长期记忆关着，这次记不了东西。）")
	}
	if parts := profileLine(st.Profile()); parts != "" {
		b.WriteString("\n" + parts)
	}
	// 社交维度打开时只调"想不想找人"的倾向：联系范围和次数上限不变（设计 §7.3）
	if drive, on := a.enabledDimension(ag, "social_drive"); on {
		if drive >= 60 {
			b.WriteString("\n今天你更想找人说话：有正经事再联系，别为了说话而说话。")
		} else if drive <= 40 {
			b.WriteString("\n今天你更想自己待着：除非有明确的约定，别主动找人。")
		}
	}
	if sid, ok := func() (string, bool) {
		if topic := topicOfTrigger(trig); topic != "" {
			return st.BranchOfTopic(topic)
		}
		return "", false
	}(); ok {
		fmt.Fprintf(&b, "\n这个话题已经有一个持久分支 %s：要接着做就在那里 continue（Session 工具），不要把这件事重新 fork 一份。", sid)
	}
	b.WriteString("\n做完用一两句话写下真的做了什么、结果怎样；什么都没做就只回 [不做]。")
	return b.String()
}

// autonomyDone 是一轮自主活动结束：把真实结果写回心智状态，安排下次检查。调用方持有锁。
func (a *App) autonomyDone(s *sess, r *run, status, body string) {
	mr := s.mind
	if mr == nil {
		return
	}
	s.mind = nil
	now := a.now().In(a.loc)
	st := a.mind(mr.AgentID)
	ag, ok := a.agentByID(mr.AgentID)
	if !ok {
		return
	}
	tokens := r.usage.PromptTokens + r.usage.CompletionTokens
	text := strings.TrimSpace(body)
	noop := isNoopMind(text)
	okRun := status == proto.StatusOK && !noop
	summary := text
	if text == "" {
		if status == proto.StatusOK {
			summary = "这一轮没有要做的事"
		} else {
			summary = "这一轮没有正常结束（" + status + "）"
		}
	}
	rec := autonomy.Execution{ID: st.NewID("ex"), Trigger: mr.Trigger.Kind, AgendaID: mr.Trigger.AgendaID,
		IntentionID: mr.Trigger.IntentionID, RunSID: s.sid, Started: mr.Started.Unix(), Ended: now.Unix(),
		OK: okRun, UsedTools: r.usedTools, Summary: summary, Tokens: tokens}
	item, itemExists := st.AgendaByID(mr.Trigger.AgendaID)
	if done, exists := r.agendaDone[mr.Trigger.AgendaID]; exists && okRun && itemExists && item.Status == autonomy.StatusInProgress {
		rec.Verified, rec.Summary = true, done.Note
		for _, id := range done.Evidence {
			rec.Evidence = append(rec.Evidence, r.toolEvidence[id])
		}
	}
	if mr.Trigger.AgendaID == "" && mr.Trigger.IntentionID == "" && okRun {
		for _, evidence := range r.toolEvidence {
			if evidence.OK && evidence.Verified && taskEvidence(evidence.Name) {
				rec.Verified = true
				rec.Evidence = append(rec.Evidence, evidence)
			}
		}
	}
	if noop && status == proto.StatusOK && !mr.Trigger.IsCommitment() {
		// 明确决定这次不做：探索类的日程标成跳过，不反复重试；约定仍然要补上
		st.RecordSkipped(rec, now)
	} else {
		st.Finish(rec, now, a.mindRetry(mr.Trigger, st))
	}
	st.Spend(tokens)
	st.SetBudgetLimit(pickInt(ag.LifeBudget, 0))
	st.SettleLedger(mr.Source, mr.Ledger, r.runID, tokens, now, a.loc)
	// 维度按自己的漂移速度回落向基线；越过阈值的发一条可靠事件，Hook 可以订阅
	for _, change := range st.DriftDimensions(now.Unix()) {
		if !change.Crossed() {
			continue
		}
		a.emitAgentRuntimeEvent("mind.threshold_crossed", mr.AgentID, map[string]any{
			"dimension": change.Name, "from": change.From, "to": change.To, "threshold": change.Threshold, "agent": mr.AgentID})
	}
	a.scheduleMindNext(st, ag, now)
	ls := a.lifeOf(ag.ID)
	if next, ok := st.NextDueTime(); ok {
		ls.Next = next.Unix()
	}
	a.saveMinds()
	a.saveLife()
	a.log.Info("自主活动结束", "agent", ag.ID, "kind", mr.Trigger.Kind, "ok", okRun, "used_tools", r.usedTools,
		"tokens", tokens, "status", status)
	a.publishAutonomy(ag.ID)
}

// mindAborted 是一轮自主活动没有正常结束（内核断线、进程在跑时停掉）：记成没做成，稍后重试。
func (a *App) mindAborted(s *sess, r *run, why string) {
	mr := s.mind
	if mr == nil {
		return
	}
	a.recordRunExperience(s, r, "interrupted", why)
	s.mind = nil
	now := a.now().In(a.loc)
	st := a.mind(mr.AgentID)
	ag, ok := a.agentByID(mr.AgentID)
	if !ok {
		return
	}
	st.Finish(autonomy.Execution{ID: st.NewID("ex"), Trigger: mr.Trigger.Kind, AgendaID: mr.Trigger.AgendaID,
		IntentionID: mr.Trigger.IntentionID, RunSID: s.sid, Started: mr.Started.Unix(), Ended: now.Unix(),
		OK: false, UsedTools: r.usedTools, Summary: why, Tokens: r.usage.PromptTokens + r.usage.CompletionTokens},
		now, a.mindRetry(mr.Trigger, st))
	st.Spend(r.usage.PromptTokens + r.usage.CompletionTokens)
	a.scheduleMindNext(st, ag, now)
	a.saveMinds()
	a.publishAutonomy(ag.ID)
}

// mindRetry 是失败 / 被打断以后多久再试：约定尽快，探索慢一点，越失败越久。
func (a *App) mindRetry(t autonomy.Trigger, st *autonomy.State) time.Duration {
	attempts := 0
	if in, ok := st.IntentionByID(t.IntentionID); ok {
		attempts = in.Attempts
	}
	base := 10 * time.Minute
	if t.IsExploration() || t.Kind == autonomy.EventSelfCheck {
		base = 30 * time.Minute
	}
	d := base * time.Duration(max(1, attempts+1))
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// scheduleMindNext 决定下一次什么时候再看：最近的一件计划/等待中的意图；都没有就按活跃度隔一阵。
func (a *App) scheduleMindNext(st *autonomy.State, ag agents.Agent, now time.Time) {
	if st.Paused {
		return
	}
	if next, ok := st.NextDueTime(); ok {
		st.SetNextCheck(next)
		return
	}
	d := a.lifeInterval(ag, now)
	if d < 30*time.Minute {
		d = 30 * time.Minute
	}
	st.SetNextCheck(now.Add(d))
}

// isNoopMind 是模型明确说这次不做（允许“什么都不做”）。
func isNoopMind(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	return (strings.Contains(t, "[不做]") || strings.Contains(t, "[不回复]")) && len([]rune(t)) <= 24
}

// ---- 定时任务 / 外部事件 ----

// mindExternal 把外部变化记进心智状态（定时任务结果、机器上下线等）。调用方持有锁。
func (a *App) mindExternal(agentID, source, text string) {
	if a.agentStore == nil || a.agentRoot == "" {
		return
	}
	ag, ok := a.agentByID(agentID)
	if !ok || !ag.Life() {
		return
	}
	a.appendExperience(agentID, experience.Entry{Kind: "external", Where: source, Visible: "admin", Said: text, Status: "ok", Reason: source})
	st := a.mind(agentID)
	now := a.now()
	st.AddEvent(autonomy.Event{Kind: autonomy.EventExternal, Source: source, Dedupe: "external:" + source,
		Text: cutRunes(text, 200)}, now, 10*time.Minute)
	if next := now.Add(time.Minute).Unix(); st.NextCheck == 0 || st.NextCheck > next {
		st.SetNextCheck(time.Unix(next, 0))
	}
	a.saveMinds()
	a.publishAutonomy(agentID)
}

// ---- 网页和聊天里的管理入口 ----

// AutonomyActionReq 是网页 / 聊天里对自主活动的操作。
type AutonomyActionReq struct {
	Action string  `json:"action"` // pause | resume | cancel_agenda | move_agenda | complete_agenda | drop_idea | defer_idea | set_number
	ID     string  `json:"id"`
	When   string  `json:"when"`
	Note   string  `json:"note"`
	Field  string  `json:"field"` // set_number / set_weight / set_dimension 要改的字段
	Value  float64 `json:"value"`
}

// AutonomyAgent 是某个 agent 的自主活动概况。
func (a *App) AutonomyAgent(id string) (autonomy.View, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadMinds()
	if a.mindsErr != nil {
		return autonomy.View{}, a.mindsErr
	}
	ag, ok := a.agentByID(id)
	if !ok {
		return autonomy.View{}, errors.New("没有这个 agent")
	}
	if !ag.Life() {
		return autonomy.View{}, errors.New("工具型 agent 没有自主活动；在 agent 节点里把类型改成拟人型")
	}
	v := a.autonomyView(ag)
	if v == nil {
		return autonomy.View{}, errors.New("自主活动还没有准备好")
	}
	return *v, nil
}

// AutonomyAction 暂停/恢复自主活动，或者取消/挪动一个日程、放弃/推迟一个想法。
func (a *App) AutonomyAction(id string, x AutonomyActionReq) (autonomy.View, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadMinds()
	if a.mindsErr != nil {
		return autonomy.View{}, a.mindsErr
	}
	ag, ok := a.agentByID(id)
	if !ok {
		return autonomy.View{}, errors.New("没有这个 agent")
	}
	if !ag.Life() {
		return autonomy.View{}, errors.New("工具型 agent 没有自主活动")
	}
	st := a.mind(id)
	now := a.now().In(a.loc)
	switch x.Action {
	case "pause":
		reason := strings.TrimSpace(x.Note)
		if reason == "" {
			reason = "在网页里暂停了自主活动"
		}
		st.SetPaused(true, reason, now)
		// 正在进行的自主活动马上停下（保留下一步，恢复后接着做）
		if s := a.sessions[lifeSID(id)]; s != nil && s.run != nil {
			s.queue = nil
			a.cancelRun(s)
		}
	case "resume":
		st.SetPaused(false, "", now)
		a.lifeOf(id).Next = now.Unix()
	case "cancel_agenda":
		if err := st.CancelAgenda(x.ID, x.Note, now); err != nil {
			return autonomy.View{}, err
		}
	case "move_agenda":
		at, err := autonomy.ParseWhen(x.When, now, a.loc)
		if err != nil {
			return autonomy.View{}, err
		}
		if err := st.MoveAgenda(x.ID, at.Unix(), now); err != nil {
			return autonomy.View{}, err
		}
	case "complete_agenda":
		if err := st.CompleteAgenda(x.ID, x.Note, now); err != nil {
			return autonomy.View{}, err
		}
	case "drop_idea":
		if err := st.DropIdea(x.ID, x.Note, now); err != nil {
			return autonomy.View{}, err
		}
	case "defer_idea":
		at, err := autonomy.ParseWhen(x.When, now, a.loc)
		if err != nil {
			return autonomy.View{}, err
		}
		if err := st.DeferIdea(x.ID, at.Unix(), now); err != nil {
			return autonomy.View{}, err
		}
	case "set_number":
		if err := st.SetNumber(x.ID, x.Field, x.Value, now); err != nil {
			return autonomy.View{}, err
		}
	case "set_preset":
		if err := st.SetPreset(strings.ToLower(strings.TrimSpace(x.ID)), now.Unix()); err != nil {
			return autonomy.View{}, err
		}
	case "set_weight":
		if err := st.SetWeight(strings.ToLower(strings.TrimSpace(x.Field)), x.Value, now.Unix()); err != nil {
			return autonomy.View{}, err
		}
	case "set_dimension":
		name := strings.ToLower(strings.TrimSpace(x.ID))
		profile := st.Profile()
		d, exists := profile.Dimensions[name]
		if !exists {
			d = autonomy.Dimension{Value: 50, Baseline: 50, Min: 0, Max: 100}
		}
		switch strings.ToLower(strings.TrimSpace(x.Field)) {
		case "enabled":
			d.Enabled = x.Value != 0
		case "value":
			d.Value = x.Value
		case "baseline":
			d.Baseline = x.Value
		case "min":
			d.Min = x.Value
		case "max":
			d.Max = x.Value
		case "drift":
			d.Drift = x.Value
		case "threshold":
			d.Threshold = x.Value
		default:
			return autonomy.View{}, errors.New("可以改维度的 enabled、value、baseline、min、max、drift、threshold")
		}
		if err := st.SetDimension(name, d, now.Unix()); err != nil {
			return autonomy.View{}, err
		}
	default:
		return autonomy.View{}, fmt.Errorf("不认识的操作 %q", x.Action)
	}
	a.afterMindChange(st, ag, now)
	v := a.autonomyView(ag)
	if v == nil {
		return autonomy.View{}, errors.New("自主活动还没有准备好")
	}
	return *v, nil
}

// cmdAutonomy 是 /autonomy：查看、暂停、恢复自主活动。
func (a *App) cmdAutonomy(c in, arg string) {
	if c.s == nil || c.s.sid == "" {
		a.answer(c, "还没有接到会话：先发一条消息再说 /autonomy")
		return
	}
	ag := a.agentOf(c.s.sid)
	if !ag.Life() {
		a.answer(c, "「"+ag.Name+"」是工具型 agent，没有自主活动。")
		return
	}
	st := a.mind(ag.ID)
	now := a.now().In(a.loc)
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "":
		v := st.View(now, a.loc, 3)
		var b strings.Builder
		if v.Paused {
			b.WriteString("自主活动现在是暂停的：" + v.PauseReason + "\n")
		} else {
			b.WriteString("自主活动开着。")
		}
		if v.NextCheck > 0 {
			fmt.Fprintf(&b, "下次自己看：%s\n", autonomy.FormatMinute(v.NextCheck/1000, a.loc))
		}
		if len(v.Agenda) == 0 {
			b.WriteString("今天没有排着的事。\n")
		} else {
			b.WriteString("安排：\n")
			for _, it := range v.Agenda {
				fmt.Fprintf(&b, "- %s %s（%s，%s）\n", autonomy.FormatMinute(it.At, a.loc), it.Title, autonomy.KindLabel(it.Kind), it.ID)
			}
		}
		if len(v.Intentions) > 0 {
			b.WriteString("在推进：\n")
			for _, in := range v.Intentions {
				fmt.Fprintf(&b, "- %s（%s，进度 %d，优先 %d，%s）\n", in.Title, autonomy.StatusLabel(in.State), in.Progress, in.Priority, in.ID)
			}
		}
		if len(v.Ideas) > 0 {
			b.WriteString("想到的事：\n")
			for _, x := range v.Ideas {
				fmt.Fprintf(&b, "- %s（%s，兴趣 %d，新颖 %d，约 %d 分钟）\n", x.Text, x.ID, x.Interest, x.Novelty, x.CostMinutes)
			}
		}
		{
			var exploration, commitment, hook, manual int
			for _, row := range v.Ledger {
				switch row.Source {
				case autonomy.LedgerExploration:
					exploration = row.Starts
				case autonomy.LedgerCommitment:
					commitment = row.Starts
				case autonomy.LedgerHook:
					hook = row.Starts
				case autonomy.LedgerManual:
					manual = row.Starts
				}
			}
			wakes, limit := a.lifeOf(ag.ID).Wakes, effectiveLifePolicy(ag).MaxWakes
			if exploration > wakes {
				wakes = exploration
			}
			if limit > 0 {
				fmt.Fprintf(&b, "今天：探索 %d / %d 次", wakes, limit)
			} else {
				fmt.Fprintf(&b, "今天：探索 %d 次（不限）", wakes)
			}
			fmt.Fprintf(&b, " · 约定 %d · Hook %d · 手动 %d（都不占探索次数）\n", commitment, hook, manual)
		}
		{
			p := st.Profile()
			fmt.Fprintf(&b, "心智档案：%s；权重 兴趣 %.1f / 新颖 %.1f / 成本 %.1f（参照 %d 分钟）\n", p.Preset, p.Weights.Interest, p.Weights.Novelty, p.Weights.Cost, p.Weights.CostScale)
			b.WriteString(profileLine(p) + "\n")
			if len(p.Pinned) > 0 {
				var pins []string
				for _, field := range autonomy.PinFields {
					if v, ok := p.Pin(field); ok {
						pins = append(pins, fmt.Sprintf("%s=%g（锁定）", field, v))
					}
				}
				if len(pins) > 0 {
					b.WriteString("人工锁定：" + strings.Join(pins, " · ") + "\n")
				}
			}
		}
		fmt.Fprintf(&b, "今天用了 %d token", v.Budget.Tokens)
		if v.Budget.Limit > 0 {
			fmt.Fprintf(&b, "（上限 %d）", v.Budget.Limit)
		}
		b.WriteString("\n最近的经历（已经结束，不是待办）：\n" + experience.Render(a.experienceOf(ag.ID).Recent(a.viewerOf(c.s.sid, c.who), c.s.sid, 3, now), a.loc, 1200))
		b.WriteString("\n/autonomy pause 暂停，/autonomy resume 恢复，/autonomy wake 现在叫醒，/autonomy set <编号> <字段>=<数值> 改数值。")
		a.answer(c, b.String())
	case "pause", "resume":
		if !a.isAdmin(c.who) {
			a.answer(c, "只有管理员能暂停或恢复自主活动")
			return
		}
		pause := strings.ToLower(strings.TrimSpace(arg)) == "pause"
		st.SetPaused(pause, "", now)
		if pause {
			if s := a.sessions[lifeSID(ag.ID)]; s != nil && s.run != nil {
				a.cancelRun(s)
			}
		} else {
			a.lifeOf(ag.ID).Next = now.Unix()
		}
		a.afterMindChange(st, ag, now)
		if pause {
			a.answer(c, "自主活动暂停了：它会继续聊天，但不会自己安排或做事。发 /autonomy resume 恢复。")
		} else {
			a.answer(c, "自主活动恢复了，过一会儿（或到点）它会接着做之前的事。")
		}
	case "wake":
		if !a.isAdmin(c.who) {
			a.answer(c, "只有管理员能叫醒它")
			return
		}
		if err := a.startLife(ag.ID, true); err != nil {
			a.answer(c, "没叫醒："+err.Error())
			return
		}
		a.answer(c, ag.Name+" 醒了（网页的 agent 节点里能看到它在做什么）。")
	case "profile":
		p := st.Profile()
		b := fmt.Sprintf("心智档案：%s\n权重：兴趣 %.1f · 新颖 %.1f · 成本 %.1f（参照 %d 分钟）\n", p.Preset, p.Weights.Interest, p.Weights.Novelty, p.Weights.Cost, p.Weights.CostScale)
		for _, name := range autonomy.DimensionNames {
			d, ok := p.Dimensions[name]
			if !ok {
				b += fmt.Sprintf("- %s（%s）：没设置\n", name, dimensionLabel(name))
				continue
			}
			state := "关"
			if d.Enabled {
				state = "开"
			}
			b += fmt.Sprintf("- %s（%s）：%s，值 %.0f，基线 %.0f，范围 %.0f–%.0f，漂移 %.0f，阈值 %.0f\n",
				name, dimensionLabel(name), state, d.Value, d.Baseline, d.Min, d.Max, d.Drift, d.Threshold)
		}
		b += "\n/autonomy preset balanced|curious|focused · /autonomy weight novelty=2 · /autonomy dimension focus value=80 threshold=90 enabled=true"
		a.answer(c, b)
	default:
		if rest, ok := strings.CutPrefix(strings.TrimSpace(arg), "lock "); ok {
			if !a.isAdmin(c.who) {
				a.answer(c, "只有管理员能锁定心智数值")
				return
			}
			field, raw, ok := strings.Cut(strings.TrimSpace(rest), "=")
			value, convErr := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if !ok || convErr != nil {
				a.answer(c, "用法：/autonomy lock novelty=3（interest / novelty 1–5，priority 0–10）")
				return
			}
			if err := st.SetPin(strings.ToLower(strings.TrimSpace(field)), value, now.Unix()); err != nil {
				a.answer(c, "没锁上："+err.Error())
				return
			}
			a.afterMindChange(st, ag, now)
			a.answer(c, fmt.Sprintf("%s 锁定为 %g：模型之后自己提的同类数值会被替换成它，直到 /autonomy unlock %s。", field, value, field))
			return
		}
		if rest, ok := strings.CutPrefix(strings.TrimSpace(arg), "unlock "); ok {
			if !a.isAdmin(c.who) {
				a.answer(c, "只有管理员能解锁心智数值")
				return
			}
			field := strings.ToLower(strings.TrimSpace(rest))
			if !st.ClearPin(field, now.Unix()) {
				a.answer(c, field+" 本来就没有锁定")
				return
			}
			a.afterMindChange(st, ag, now)
			a.answer(c, field+" 解锁了，模型自己提的数值重新生效。")
			return
		}
		if rest, ok := strings.CutPrefix(strings.TrimSpace(arg), "preset "); ok {
			if !a.isAdmin(c.who) {
				a.answer(c, "只有管理员能改心智档案")
				return
			}
			name := strings.ToLower(strings.TrimSpace(rest))
			if err := st.SetPreset(name, now.Unix()); err != nil {
				a.answer(c, "没改成："+err.Error())
				return
			}
			a.afterMindChange(st, ag, now)
			a.answer(c, "心智档案换成「"+st.Profile().Preset+"」了，下一次挑选和简报就用新值。")
			return
		}
		if rest, ok := strings.CutPrefix(strings.TrimSpace(arg), "weight "); ok {
			if !a.isAdmin(c.who) {
				a.answer(c, "只有管理员能改心智档案")
				return
			}
			field, raw, ok := strings.Cut(strings.TrimSpace(rest), "=")
			value, convErr := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if !ok || convErr != nil {
				a.answer(c, "用法：/autonomy weight novelty=2（interest / novelty / cost 为 0–5，cost_scale 为 1–480 分钟）")
				return
			}
			if err := st.SetWeight(strings.ToLower(strings.TrimSpace(field)), value, now.Unix()); err != nil {
				a.answer(c, "没改成："+err.Error())
				return
			}
			a.afterMindChange(st, ag, now)
			a.answer(c, fmt.Sprintf("权重 %s 改成 %g 了；候选排序下一次就用新值。", field, value))
			return
		}
		if rest, ok := strings.CutPrefix(strings.TrimSpace(arg), "dimension "); ok {
			if !a.isAdmin(c.who) {
				a.answer(c, "只有管理员能改心智档案")
				return
			}
			fields := strings.Fields(rest)
			if len(fields) < 2 {
				a.answer(c, "用法：/autonomy dimension focus value=80 threshold=90 enabled=true")
				return
			}
			name := strings.ToLower(fields[0])
			p := st.Profile()
			d, exists := p.Dimensions[name]
			if !exists {
				d = autonomy.Dimension{Value: 50, Baseline: 50, Min: 0, Max: 100}
			}
			for _, pair := range fields[1:] {
				key, raw, ok := strings.Cut(pair, "=")
				if !ok {
					a.answer(c, "参数要写成 字段=数值")
					return
				}
				key = strings.ToLower(strings.TrimSpace(key))
				raw = strings.TrimSpace(raw)
				if key == "enabled" {
					d.Enabled = raw == "true" || raw == "1"
					continue
				}
				value, convErr := strconv.ParseFloat(raw, 64)
				if convErr != nil {
					a.answer(c, "参数 "+key+" 要是数值")
					return
				}
				switch key {
				case "value":
					d.Value = value
				case "baseline", "base":
					d.Baseline = value
				case "min":
					d.Min = value
				case "max":
					d.Max = value
				case "drift":
					d.Drift = value
				case "threshold":
					d.Threshold = value
				default:
					a.answer(c, "可以改：value、baseline、min、max、drift、threshold、enabled")
					return
				}
			}
			if err := st.SetDimension(name, d, now.Unix()); err != nil {
				a.answer(c, "没改成："+err.Error())
				return
			}
			a.afterMindChange(st, ag, now)
			a.answer(c, fmt.Sprintf("维度 %s 改好了：%s，值 %.0f，基线 %.0f，漂移 %.0f，阈值 %.0f。", name,
				map[bool]string{true: "开", false: "关"}[d.Enabled], d.Value, d.Baseline, d.Drift, d.Threshold))
			return
		}
		if rest, ok := strings.CutPrefix(strings.TrimSpace(arg), "set "); ok {
			if !a.isAdmin(c.who) {
				a.answer(c, "只有管理员能改这些数值")
				return
			}
			id, rhs, ok := strings.Cut(strings.TrimSpace(rest), " ")
			field, raw, ok2 := strings.Cut(rhs, "=")
			value, convErr := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if !ok || !ok2 || convErr != nil {
				a.answer(c, "用法：/autonomy set <编号或认识主题> <字段>=<数值>，字段可以是 interest、novelty、cost_minutes、priority、progress、confidence")
				return
			}
			if err := st.SetNumber(strings.TrimSpace(id), strings.ToLower(strings.TrimSpace(field)), value, now); err != nil {
				a.answer(c, "没改成："+err.Error())
				return
			}
			a.afterMindChange(st, ag, now)
			a.answer(c, fmt.Sprintf("%s 的 %s 改成 %g 了。想法按兴趣+新颖排序，目标按优先级排序，下一次挑选就会用新值。", id, field, value))
			return
		}
		a.answer(c, "用法：/autonomy [pause | resume | wake | profile | set <编号> <字段>=<数值> | preset <名字> | weight <字段>=<值> | dimension <名字> <字段>=<值> | lock <字段>=<值> | unlock <字段>]")
	}
}
