package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/schedule"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type ScheduleRequest struct {
	Action     string   `json:"action"`
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	When       string   `json:"when"`
	Agent      string   `json:"agent"`
	Prompt     string   `json:"prompt"`
	Deliver    []string `json:"deliver"`
	Workspaces []string `json:"workspaces"`
	Mode       string   `json:"mode"`
	Enabled    *bool    `json:"enabled"`
	CatchUp    *bool    `json:"catch_up"`
	Timeout    string   `json:"timeout"`
}
type ScheduleView struct {
	Tasks    []schedule.Task `json:"tasks"`
	Timezone string          `json:"timezone"`
	Error    string          `json:"error,omitempty"`
}

const scheduleSchema = `{"type":"object","properties":{
 "action":{"type":"string","enum":["create","list","update","delete","run_now"]},
 "id":{"type":"string","description":"任务编号（更新、删除、立即运行时必填）"},
 "name":{"type":"string"},"when":{"type":"string","description":"cron 五段式、every 30m、at YYYY-MM-DD HH:mm；网关时区"},
 "agent":{"type":"string","description":"任务归哪个 agent；省略就是当前会话的执行 agent。已有任务不会被聊天里的切换改动"},"prompt":{"type":"string"},
 "deliver":{"type":"array","items":{"type":"string"},"description":"把结果投到哪些窗口（会话的窗口 key，网页用 web:会话ID）；省略就只放在任务页，不往聊天里推"},
 "workspaces":{"type":"array","items":{"type":"string"},"description":"工作目录 ID；省略时继承当前会话"},
 "mode":{"type":"string","enum":["readonly","ask","auto"],"description":"不得高于创建者会话的模式"},
 "enabled":{"type":"boolean"},"catch_up":{"type":"boolean","description":"停机错过一小时内补跑一次，默认 true"},
 "timeout":{"type":"string","description":"运行超时，默认 30m"}
},"required":["action"]}`

func scheduleTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "Schedule", Description: "管理定时任务（仅管理员）：create/list/update/delete/run_now。创建、修改、删除遵循当前权限模式。任务独立会话运行；结果默认只放在任务页，想让谁看到就把 deliver 写成那个窗口。先列出再按 id 修改，回答中说明运行时间和投递位置。", InputSchema: json.RawMessage(scheduleSchema)}
}

// ScheduleSummary 是网页「定时任务」节点要显示的概况
type ScheduleSummary struct {
	Total   int    `json:"total"`
	Enabled int    `json:"enabled"`
	Running int    `json:"running"`
	Next    int64  `json:"next,omitempty"` // 最近一次要运行的时间（毫秒）
	Error   string `json:"error,omitempty"`
}

func (a *App) ScheduleSummary() ScheduleSummary {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scheduleSummary()
}
func (a *App) scheduleSummary() ScheduleSummary {
	var sum ScheduleSummary
	if a.schedulesErr != nil {
		sum.Error = a.schedulesErr.Error()
		return sum
	}
	if a.schedules == nil {
		return sum
	}
	for _, t := range a.schedules.List() {
		sum.Total++
		if t.Running {
			sum.Running++
		}
		if t.Enabled {
			sum.Enabled++
			if !t.Next.IsZero() && (sum.Next == 0 || t.Next.UnixMilli() < sum.Next) {
				sum.Next = t.Next.UnixMilli()
			}
		}
	}
	return sum
}

// publishSchedules 告诉网页任务有变化（带上概况，拓扑图的节点直接用）
func (a *App) publishSchedules() {
	a.publish(events.Event{T: "schedules", Data: a.scheduleSummary()})
}

func (a *App) loadSchedules() {
	a.schedules, a.schedulesErr = schedule.Open(filepath.Join(a.store.Dir(), "schedules.json"), a.now())
	if a.schedulesErr != nil {
		a.log.Warn("定时任务不可用", "err", a.schedulesErr)
	}
}
func (a *App) Schedules() ScheduleView {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := ScheduleView{Tasks: []schedule.Task{}, Timezone: a.loc.String()}
	if a.schedulesErr != nil {
		v.Error = a.schedulesErr.Error()
	} else {
		v.Tasks = a.schedules.List()
	}
	return v
}
func modeRank(mode string) int {
	switch mode {
	case proto.ModeReadonly:
		return 0
	case proto.ModeAsk:
		return 1
	case proto.ModeAuto:
		return 2
	case proto.ModeFull:
		return 3
	}
	return -1
}
func (a *App) scheduleCallerMode(sid, who string) string {
	if s := a.sessions[sid]; s != nil && s.run != nil && s.run.from == who {
		return a.runMode(s.run.w, who)
	}
	return a.runMode(a.workspaceOf(sid), who)
}
func (a *App) ScheduleAction(sid, who string, p ScheduleRequest) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scheduleAction(sid, who, p)
}
func (a *App) scheduleAction(sid, who string, p ScheduleRequest) (any, error) {
	if !a.isAdmin(who) {
		return nil, errors.New("只有管理员能管理定时任务")
	}
	if a.schedulesErr != nil {
		return nil, a.schedulesErr
	}
	tasks := a.schedules.List()
	if p.Action == "list" {
		return tasks, nil
	}
	i := slices.IndexFunc(tasks, func(t schedule.Task) bool { return t.ID == p.ID })
	mode := a.scheduleCallerMode(sid, who)
	// 安静来源（安静分支或它派生的任务）只能动自己派生的安静任务：
	// 改、立即运行、删除普通任务都可能把结果推给主人（评审 §25 S2）
	if a.branchSilent(sid) && p.Action != "list" && p.Action != "create" {
		if i < 0 || !tasks[i].Silent {
			return nil, errors.New("这是从安静分支派生的请求：不能修改、立即运行或删除会往聊天里发结果的普通任务；需要的话请主人自己操作")
		}
	}
	if p.Action == "run_now" {
		if i < 0 {
			return nil, errors.New("没有这个任务")
		}
		if modeRank(tasks[i].Mode) > modeRank(mode) {
			return nil, errors.New("任务模式高于当前会话，不能立即运行")
		}
		if err := a.launchSchedule(tasks[i], false); err != nil {
			return nil, err
		}
		return "已开始运行 " + tasks[i].Name, nil
	}
	if modeRank(mode) <= 0 {
		return nil, errors.New("只读模式不能修改定时任务")
	}
	if p.Action == "delete" {
		if i < 0 {
			return nil, errors.New("没有这个任务")
		}
		if tasks[i].Running {
			return nil, errors.New("任务正在运行，先在任务会话中停止")
		}
		if err := a.schedules.Save(slices.Delete(tasks, i, i+1)); err != nil {
			return nil, err
		}
		a.publishSchedules()
		return "已删除任务", nil
	}
	if p.Action != "create" && p.Action != "update" {
		return nil, errors.New("未知的 Schedule action")
	}
	var task schedule.Task
	if p.Action == "create" {
		// 新任务归当前会话的执行 agent（会话可以切换过）；已有任务不跟着聊天走
		task = schedule.Task{ID: schedule.NewID(), Agent: a.execAgentID(sid), Mode: mode, CreatorMode: mode, CreatedBy: who, Enabled: true, CatchUp: true,
			Workspaces: a.workspaceOf(sid).ids(), Source: sid, Silent: a.branchSilent(sid)}
		// deliver 不写就是「只放任务页」：结果留在任务自己的会话和任务列表里，不往聊天窗口推。
		// 以前这里默认投到创建任务时所在的那个窗口，模型在私聊里建的周期任务就会每轮往聊天里发结果。
	} else {
		if i < 0 {
			return nil, errors.New("没有这个任务")
		}
		task = tasks[i]
		if task.Running {
			return nil, errors.New("任务正在运行，稍后再修改")
		}
	}
	oldWhen, oldEnabled := task.When, task.Enabled
	if p.Name != "" {
		task.Name = strings.TrimSpace(p.Name)
	}
	if p.When != "" {
		task.When = strings.TrimSpace(p.When)
	}
	if p.Agent != "" {
		task.Agent = p.Agent
	}
	if p.Prompt != "" {
		task.Prompt = p.Prompt
	}
	if p.Deliver != nil {
		task.Deliver = slices.Clone(p.Deliver)
	}
	if p.Workspaces != nil {
		task.Workspaces = slices.Clone(p.Workspaces)
	}
	if p.Mode != "" {
		task.Mode = p.Mode
	}
	if p.Enabled != nil {
		task.Enabled = *p.Enabled
	}
	if p.CatchUp != nil {
		task.CatchUp = *p.CatchUp
	}
	if p.Timeout != "" {
		task.Timeout = p.Timeout
	}
	if task.Name == "" || task.Prompt == "" || task.When == "" {
		return nil, errors.New("name、when、prompt 不能为空")
	}
	if _, ok := a.agentByID(task.Agent); !ok {
		return nil, fmt.Errorf("没有这个 agent：%s", task.Agent)
	}
	if modeRank(task.Mode) < 0 || modeRank(task.Mode) > modeRank(mode) || modeRank(task.Mode) > modeRank(task.CreatorMode) {
		return nil, errors.New("任务模式不能高于创建者或当前会话的模式")
	}
	if _, err := schedule.Timeout(task); err != nil {
		return nil, err
	}
	for _, key := range task.Deliver {
		ep := parseEndpoint(key)
		if ep.chat == "" || (ep.ch == "web" && !a.hasSession(ep.chat)) || (ep.ch != "web" && a.eps[key] == nil) {
			return nil, fmt.Errorf("没有这个投递窗口：%s", key)
		}
	}
	for _, id := range task.Workspaces {
		if a.findWorkspace(id) == nil {
			return nil, fmt.Errorf("没有这个工作目录：%s", id)
		}
	}
	next, err := schedule.Next(task.When, a.now(), a.loc)
	if err != nil {
		return nil, err
	}
	if p.Action == "create" || oldWhen != task.When || (!oldEnabled && task.Enabled) {
		if next.IsZero() {
			return nil, errors.New("运行时间已经过去")
		}
		task.Next = next
	}
	if p.Action == "create" {
		tasks = append(tasks, task)
	} else {
		tasks[i] = task
	}
	if err = a.schedules.Save(tasks); err != nil {
		return nil, err
	}
	a.publishSchedules()
	return task, nil
}
func (a *App) scheduleLoop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-tick.C:
			a.ScheduleTick()
		}
	}
}

// ScheduleTick 每秒检查一次到点的任务和超时的运行；和上下文维护用同一个时钟（a.now），测试里可以拨
func (a *App) ScheduleTick() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.jobHintTick()
	if a.schedulesErr != nil || a.schedules == nil || a.ctx.Err() != nil {
		return
	}
	for _, s := range a.sessions {
		if r := s.run; r != nil && r.scheduled != nil {
			limit, _ := schedule.Timeout(*r.scheduled)
			if a.now().Sub(r.start) >= limit {
				a.cancelRun(s)
				a.finish(s, proto.StatusTimeout, "")
			}
		}
	}
	// 等内核和渠道连上之后，再处理停机期间错过的任务
	if !a.schedulerStarted && (a.hub == nil || len(a.nodes) == 0) {
		return
	}
	startup := !a.schedulerStarted
	a.schedulerStarted = true
	for _, task := range a.schedules.List() {
		if !task.Enabled || task.Next.IsZero() || task.Next.After(a.now()) {
			continue
		}
		late := a.now().Sub(schedule.LatestDue(task, a.now(), a.loc))
		if task.Running || late > time.Hour || (startup && late > time.Second && !task.CatchUp) {
			why := "错过运行时间，未补跑"
			if task.Running {
				why = "上一轮仍在运行，跳过本次"
			}
			next, err := schedule.Advance(task, a.now(), a.loc)
			if err != nil {
				a.log.Warn("定时任务时间无效", "id", task.ID, "err", err)
				continue
			}
			task.Next = next
			if next.IsZero() {
				task.Enabled = false
			}
			schedule.Record(&task, schedule.Result{At: a.now(), Summary: why})
			a.saveTask(task)
			continue
		}
		if err := a.launchSchedule(task, true); err != nil {
			a.log.Warn("启动定时任务失败", "id", task.ID, "err", err)
		}
	}
}

// cronSID 是定时任务的会话 ID：默认 agent 是 cron:<id>，别的 agent 是 @<agent>/cron:<id>。
func cronSID(t schedule.Task) string {
	agent := t.Agent
	if agent == "" {
		agent = agents.DefaultID
	}
	return agents.SessionFor(agent, "cron:"+t.ID)
}

// isCronSID 判断会话是不是定时任务的会话。
func isCronSID(sid string) bool { return strings.Contains(sid, "cron:") }

func (a *App) saveTask(task schedule.Task) error {
	tasks := a.schedules.List()
	i := slices.IndexFunc(tasks, func(t schedule.Task) bool { return t.ID == task.ID })
	if i < 0 {
		return errors.New("任务已删除")
	}
	tasks[i] = task
	if err := a.schedules.Save(tasks); err != nil {
		a.log.Error("保存定时任务失败", "err", err)
		return err
	}
	a.publishSchedules()
	return nil
}
func (a *App) launchSchedule(task schedule.Task, advance bool) error {
	if task.Running {
		return errors.New("任务已在运行")
	}
	s := a.sessOf(cronSID(task))
	if s.run != nil || s.compacting || len(s.queue) > 0 {
		return errors.New("任务会话正在使用")
	}
	if advance {
		next, err := schedule.Advance(task, a.now(), a.loc)
		if err != nil {
			return err
		}
		task.Next = next
		if next.IsZero() {
			task.Enabled = false
		}
	}
	task.Running = true
	if err := a.saveTask(task); err != nil {
		return err
	}
	fail := func(err error) error { a.completeSchedule(&task, false, err.Error()); return err }
	if strings.TrimSpace(task.Prompt) == "" {
		return fail(errors.New("任务的 prompt 是空的"))
	}
	if _, ok := a.agentByID(task.Agent); !ok {
		return fail(errors.New("任务指定的 agent 已经删掉了"))
	}
	if !a.isAdmin(task.CreatedBy) {
		return fail(errors.New("创建者已不再是管理员，任务未执行"))
	}
	if modeRank(task.Mode) < 0 || modeRank(task.Mode) > modeRank(task.CreatorMode) {
		return fail(errors.New("任务权限设置无效"))
	}
	if _, err := schedule.Timeout(task); err != nil {
		return fail(err)
	}
	for _, id := range task.Workspaces {
		if a.findWorkspace(id) == nil {
			return fail(fmt.Errorf("工作目录 %s 已不存在", id))
		}
	}
	if err := a.resetHistory(s); err != nil {
		return fail(fmt.Errorf("归档任务上下文失败：%w", err))
	}
	st := a.store.Settings(s.sid)
	st.Title = "定时任务 · " + task.Name
	st.Workspaces = slices.Clone(task.Workspaces)
	st.WorkspacesSet = true
	st.Mode = task.Mode
	if err := a.store.SaveSettings(s.sid, st); err != nil {
		return fail(err)
	}
	if a.models != nil && len(a.models.Models()) == 0 {
		return fail(errors.New("还没有配置模型"))
	}
	if a.route(a.workspaceOf(s.sid)).kernel == "" {
		return fail(errors.New("没有在线的内核"))
	}
	s.scheduled = &task
	// 没有投递窗口（只放任务页）时，这一轮挂在任务自己的会话上；结果由 completeSchedule 记进任务。
	// 从安静分支派生的任务（task.Silent）一律不投递：deliver 不能用来把派生任务变吵。
	ep := endpoint{ch: "web", chat: s.sid}
	if len(task.Deliver) > 0 && !task.Silent {
		ep = parseEndpoint(task.Deliver[0])
	}
	s.queue = []queued{{ep: ep, who: task.CreatedBy, m: channel.Message{Text: a.scheduleNote(task) + task.Prompt, ChatID: ep.chat}}}
	a.startRun(s)
	if s.run == nil {
		if s.scheduled == nil {
			return errors.New("任务启动后连接已中断")
		}
		s.scheduled = nil
		s.queue = nil
		return fail(errors.New("无法启动：没有可用的模型或在线内核"))
	}
	return nil
}

// scheduleNote 告诉模型这一轮的结果会去哪：有投递窗口的，没什么要说就只回 [不回复]，不推送。
// 不说的话，工具型 agent 不知道还能这样，每一轮都会往聊天里发一条“没有新消息”。调用方持有锁。
func (a *App) scheduleNote(task schedule.Task) string {
	if task.Silent {
		return "[系统] 这是从安静分支派生出来的定时任务「" + task.Name + "」。结果只记在任务页，不会发到任何聊天；\n" +
			"要联系主人，由主人自己在来源会话里「引入结论」，不要用 Message 往外发。\n\n"
	}
	if len(task.Deliver) == 0 {
		return "[系统] 这是定时任务「" + task.Name + "」。结果只记在任务页，不会发到聊天里。\n\n"
	}
	var to []string
	for _, key := range task.Deliver {
		ep := parseEndpoint(key)
		name := sourceOf(ep.ch)
		if r := a.eps[key]; r != nil && r.Name != "" {
			name += "「" + r.Name + "」"
		}
		to = append(to, name)
	}
	return "[系统] 这是定时任务「" + task.Name + "」。你最后的回复会发到" + strings.Join(to, "、") +
		"；没有值得告诉对方的，就只回 [不回复]（结果只记在任务页，不推送）。\n\n"
}

// scheduleSilent 是模型决定这次不推送：回复以 [不回复] 开头或结尾（只回了它，或者“没有新私信。[不回复]”）。
// 正文中间提到这几个字（比如复述规则）不算。
func scheduleSilent(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "[不回复]") || strings.HasSuffix(t, "[不回复]")
}

// completeSchedule 记下这一轮的结果，再按需推送到任务的投递窗口：
//   - 模型回了 [不回复]：只记在任务页，不推送（每 3 分钟查一次私信的任务，没新消息时不该每次都来一条）；
//   - 和上一次一模一样的失败：只推第一次（内核掉线一小时，3 分钟一次的任务不该推 20 条同样的通知）。
//
// 调用方持有锁。
func (a *App) completeSchedule(task *schedule.Task, ok bool, text string) {
	silent := ok && scheduleSilent(text)
	summary := text
	if silent {
		summary = strings.TrimSpace(strings.ReplaceAll(text, "[不回复]", ""))
		if summary == "" {
			summary = "（这次没有要说的，没有推送）"
		} else {
			summary += "\n（没有推送）"
		}
	}
	push := !silent && !task.Silent
	tasks := a.schedules.List()
	i := slices.IndexFunc(tasks, func(t schedule.Task) bool { return t.ID == task.ID })
	if i >= 0 {
		current := tasks[i]
		if prev := current.Last; !ok && prev != nil && !prev.OK && prev.Summary == clip(summary, 2000) {
			push = false
		}
		current.Running = false
		schedule.Record(&current, schedule.Result{At: a.now(), OK: ok, Summary: clip(summary, 2000)})
		if err := a.saveTask(current); err != nil {
			a.log.Error("记录任务结果失败", "err", err)
		}
	}
	body := "[定时任务 " + task.Name + "]\n" + summary
	if push && !task.Silent {
		a.mindExternal(task.Agent, "schedule:"+task.ID, body)
	}
	a.publish(events.Event{T: "say", SID: cronSID(*task), Text: body})
	if !push {
		return
	}
	for _, key := range task.Deliver {
		ep := parseEndpoint(key)
		if ep.ch == "web" {
			a.publish(events.Event{T: "say", SID: ep.chat, Text: body})
		} else {
			a.deliver(ep, body, "")
		}
	}
}
func (a *App) cmdCron(c in, arg string) {
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能管理定时任务")
		return
	}
	if a.schedulesErr != nil {
		a.answer(c, a.schedulesErr.Error())
		return
	}
	tasks := a.schedules.List()
	words := strings.Fields(arg)
	if len(words) == 0 {
		var b strings.Builder
		b.WriteString("定时任务（" + a.loc.String() + "）：")
		for i, t := range tasks {
			state := "关闭"
			if t.Enabled {
				state = "开启"
			}
			if t.Running {
				state = "运行中"
			}
			next := "无"
			if !t.Next.IsZero() {
				next = t.Next.In(a.loc).Format("01-02 15:04:05")
			}
			fmt.Fprintf(&b, "\n%d. %s · %s · %s · 下次 %s（%s）", i+1, t.Name, t.When, state, next, t.ID)
		}
		if len(tasks) == 0 {
			b.WriteString("\n还没有任务，可以请我创建，或在网页拓扑图的「定时任务」节点里添加。")
		}
		a.answer(c, b.String())
		return
	}
	if len(words) != 2 {
		a.answer(c, "用法：/cron [run|on|off|rm <编号>]")
		return
	}
	id := words[1]
	if n, err := strconv.Atoi(id); err == nil && n >= 1 && n <= len(tasks) {
		id = tasks[n-1].ID
	}
	p := ScheduleRequest{ID: id}
	switch words[0] {
	case "run":
		p.Action = "run_now"
	case "on", "off":
		p.Action = "update"
		on := words[0] == "on"
		p.Enabled = &on
	case "rm":
		p.Action = "delete"
	default:
		a.answer(c, "用法：/cron [run|on|off|rm <编号>]")
		return
	}
	_, err := a.scheduleAction(c.s.sid, c.who, p)
	if err != nil {
		a.answer(c, "没有完成："+err.Error())
	} else {
		a.answer(c, "已完成 /cron "+arg)
	}
}
