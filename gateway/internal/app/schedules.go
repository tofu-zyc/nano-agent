package app

import (
	"encoding/json"
	"errors"
	"fmt"
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
 "agent":{"type":"string","enum":["default"]},"prompt":{"type":"string"},
 "deliver":{"type":"array","items":{"type":"string"},"description":"窗口 key 列表，省略时发到当前窗口；网页用 web:会话ID"},
 "workspaces":{"type":"array","items":{"type":"string"},"description":"工作目录 ID；省略时继承当前会话"},
 "mode":{"type":"string","enum":["readonly","ask","auto"],"description":"不得高于创建者会话的模式"},
 "enabled":{"type":"boolean"},"catch_up":{"type":"boolean","description":"停机错过一小时内补跑一次，默认 true"},
 "timeout":{"type":"string","description":"运行超时，默认 30m"}
},"required":["action"]}`

func scheduleTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "Schedule", Description: "管理定时任务（仅管理员）：create/list/update/delete/run_now。创建、修改、删除遵循当前权限模式。任务独立会话运行，结果和审批送到 deliver，默认当前窗口。先列出再按 id 修改，回答中说明运行时间和投递位置。", InputSchema: json.RawMessage(scheduleSchema)}
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
		a.publish(events.Event{T: "schedules"})
		return "已删除任务", nil
	}
	if p.Action != "create" && p.Action != "update" {
		return nil, errors.New("未知的 Schedule action")
	}
	var task schedule.Task
	if p.Action == "create" {
		task = schedule.Task{ID: schedule.NewID(), Agent: "default", Mode: mode, CreatorMode: mode, CreatedBy: who, Enabled: true, CatchUp: true, Workspaces: a.workspaceOf(sid).ids()}
		if s := a.sessions[sid]; s != nil && s.run != nil && len(s.run.outs) > 0 && s.run.outs[0].ep.ch != "web" {
			task.Deliver = []string{s.run.outs[0].ep.key()}
		} else {
			task.Deliver = []string{"web:" + sid}
		}
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
	if task.Agent != "default" {
		return nil, errors.New("当前只支持 default agent")
	}
	if modeRank(task.Mode) < 0 || modeRank(task.Mode) > modeRank(mode) || modeRank(task.Mode) > modeRank(task.CreatorMode) {
		return nil, errors.New("任务模式不能高于创建者或当前会话的模式")
	}
	if _, err := schedule.Timeout(task); err != nil {
		return nil, err
	}
	if len(task.Deliver) == 0 {
		return nil, errors.New("至少选一个投递窗口")
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
	a.publish(events.Event{T: "schedules"})
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

// ScheduleTick uses the same clock as context maintenance, allowing deterministic tests.
func (a *App) ScheduleTick() {
	a.mu.Lock()
	defer a.mu.Unlock()
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
	// Wait for kernel/channel startup before processing downtime catch-up.
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
	a.publish(events.Event{T: "schedules"})
	return nil
}
func (a *App) launchSchedule(task schedule.Task, advance bool) error {
	if task.Running {
		return errors.New("任务已在运行")
	}
	s := a.sessOf("cron:" + task.ID)
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
	if task.Agent != "default" || strings.TrimSpace(task.Prompt) == "" {
		return fail(errors.New("任务 agent 或 prompt 无效"))
	}
	if len(task.Deliver) == 0 {
		return fail(errors.New("没有投递窗口"))
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
	ep := parseEndpoint(task.Deliver[0])
	s.queue = []queued{{ep: ep, who: task.CreatedBy, m: channel.Message{Text: task.Prompt, ChatID: ep.chat}}}
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
func (a *App) completeSchedule(task *schedule.Task, ok bool, text string) {
	tasks := a.schedules.List()
	i := slices.IndexFunc(tasks, func(t schedule.Task) bool { return t.ID == task.ID })
	if i >= 0 {
		current := tasks[i]
		current.Running = false
		schedule.Record(&current, schedule.Result{At: a.now(), OK: ok, Summary: clip(text, 2000)})
		if err := a.saveTask(current); err != nil {
			a.log.Error("记录任务结果失败", "err", err)
		}
	}
	body := "[定时任务 " + task.Name + "]\n" + text
	a.publish(events.Event{T: "say", SID: "cron:" + task.ID, Text: body})
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
			b.WriteString("\n还没有任务，可以请我创建，或在网页的定时任务页添加。")
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
