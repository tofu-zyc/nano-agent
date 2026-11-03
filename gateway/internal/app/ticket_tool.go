package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/scene"
	"nano-agent/gateway/internal/ticket"
)

// Ticket：团队群里名单上的人点名 agent，agent 用它建工单（设计 §16.1）。
// 只给设了团队规则的会话。派活的人看这一轮的触发消息，不采信模型填的。

const ticketSchema = `{"type":"object","properties":{
"action":{"type":"string","enum":["create","list","show","note","accept","cancel"],"description":"create 建工单；list、show 查看；note 转进名单里的人补充的话；accept 收下就绪的工单；cancel 叫停（要写 text 理由）"},
"title":{"type":"string","description":"create：工单标题，最多 100 字"},
"goal":{"type":"string","description":"create：要做成什么，最多 2000 字"},
"id":{"type":"string","description":"show、note、accept、cancel：工单编号 tk-<序号>"},
"text":{"type":"string","description":"note 的补充；cancel 的理由"}
},"required":["action"]}`

func ticketTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "Ticket", Description: "团队里的工单。名单里的人让你做一件要改代码的事时 create 一张工单；" +
		"建好后在回复里确认一次就行，过程不用说，结果和问题会自动发回这个群。不在名单里的人派不了活。",
		InputSchema: json.RawMessage(ticketSchema)}
}

// teamOf 是这个会话的团队设置；没设返回 nil。调用方持有锁。
func (a *App) teamOf(sid string) *scene.Team {
	if sc, ok := a.sceneOf(sid); ok && sc.Kind == scene.External {
		return sc.Rules.Team
	}
	return nil
}

func (a *App) ticketSpec(sid string) (proto.ToolSpec, bool) {
	if a.teamOf(sid) == nil {
		return proto.ToolSpec{}, false
	}
	return ticketTool(), true
}

// canDispatch：名单里的人，或管理员。
func (a *App) canDispatch(team *scene.Team, who string) bool {
	return who != "" && (a.isAdmin(who) || team.InRoster(who))
}

type ticketArgs struct {
	Action string `json:"action"`
	Title  string `json:"title"`
	Goal   string `json:"goal"`
	ID     string `json:"id"`
	Text   string `json:"text"`
}

// ticketCall 执行 Ticket 工具。调用方持有锁。
func (a *App) ticketCall(s *sess, raw []byte) (string, error) {
	var x ticketArgs
	if err := json.Unmarshal(raw, &x); err != nil {
		return "", errors.New("参数格式不对")
	}
	team := a.teamOf(s.sid)
	if team == nil || s.run == nil {
		return "", errors.New("这个会话不是团队")
	}
	if a.tickets == nil {
		return "", errors.New("工单记录现在不可用")
	}
	who := ""
	if s.run.trig != nil {
		who = s.run.trig.who
	}
	switch x.Action {
	case "create":
		return a.ticketCreate(s, team, who, x)
	case "list":
		return a.ticketList(s.sid), nil
	case "show":
		t, err := a.ticketOf(s.sid, x.ID)
		if err != nil {
			return "", err
		}
		return ticketText(t), nil
	case "note", "accept", "cancel":
		if !a.canDispatch(team, who) {
			return "", errors.New("只有名单里的人（或管理员）能这样做；请名单里的人来")
		}
		t, err := a.ticketOf(s.sid, x.ID)
		if err != nil {
			return "", err
		}
		return a.ticketAct(x, t, who)
	}
	return "", errors.New("action 要是 create、list、show、note、accept、cancel 之一")
}

func (a *App) ticketCreate(s *sess, team *scene.Team, who string, x ticketArgs) (string, error) {
	if !a.canDispatch(team, who) {
		return "", errors.New("要名单里的人来派活：现在点名的人不在团队的准入名单里。可以回话，但不能建工单")
	}
	title, goal := strings.TrimSpace(x.Title), strings.TrimSpace(x.Goal)
	if title == "" || goal == "" {
		return "", errors.New("要写 title 和 goal")
	}
	if n := utf8.RuneCountInString(title); n > ticket.MaxTitle {
		return "", fmt.Errorf("title 最多 %d 字（现在 %d）", ticket.MaxTitle, n)
	}
	if n := utf8.RuneCountInString(goal); n > ticket.MaxGoal {
		return "", fmt.Errorf("goal 最多 %d 字（现在 %d）", ticket.MaxGoal, n)
	}
	ws := a.findWorkspace(team.Workspace)
	if ws == nil || !a.hostUsable(ws.Host) {
		return "", errors.New("团队的仓库所在的机器现在不在线，建不了工单")
	}
	t := s.run.trig
	t0, existed, err := a.tickets.Create(ticket.Ticket{
		Agent: a.execAgentID(s.sid), Title: title, Goal: goal, Check: team.Check, Budget: team.EffectiveBudget(),
		Auth: ticket.Auth{By: who, Scene: s.sid, Workspace: team.Workspace, Mode: a.workspaceBase(s.sid).mode,
			Tools: a.sceneToolAllow(s.sid), At: a.now().Unix(),
			Source: ticket.Source{Channel: t.ch, Chat: t.chat, MsgID: t.msgID, Text: t.text}},
	})
	if err != nil {
		a.ticketHealth(err)
		return "", fmt.Errorf("工单没有记下来（%v），稍后再试", err)
	}
	a.ticketHealth(nil)
	if existed {
		return fmt.Sprintf("这条消息已经建过工单 %s（%s），没有重复建。", t0.ID, t0.Title), nil
	}
	a.publishCommitments()
	return fmt.Sprintf("已建工单 %s。在回复里确认一次就好；过程不用说，结果和问题会自动发回这里。", t0.ID), nil
}

// ticketOf 只认这个场景自己的工单。
func (a *App) ticketOf(sid, id string) (ticket.Ticket, error) {
	t, ok := a.tickets.Get(strings.TrimSpace(id))
	if !ok || t.Auth.Scene != sid {
		return ticket.Ticket{}, errors.New("这个群里没有这张工单")
	}
	return t, nil
}

func (a *App) ticketList(sid string) string {
	var b strings.Builder
	n := 0
	for _, t := range a.tickets.List() {
		if t.Auth.Scene != sid {
			continue
		}
		n++
		fmt.Fprintf(&b, "%s · %s · %s\n", t.ID, t.Status, t.Title)
	}
	if n == 0 {
		return "这个群还没有工单。"
	}
	return strings.TrimSpace(b.String())
}

func ticketText(t ticket.Ticket) string {
	s := fmt.Sprintf("%s · %s\n标题：%s\n目标：%s\n派活：%s\n验收命令：%s", t.ID, t.Status, t.Title, t.Goal, t.Auth.By, orNone(t.Check))
	if t.Reason != "" {
		s += "\n理由：" + t.Reason
	}
	for _, n := range t.Notes {
		s += fmt.Sprintf("\n补充（%s）：%s", n.By, n.Text)
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "（无）"
	}
	return s
}

func (a *App) ticketAct(x ticketArgs, t ticket.Ticket, who string) (string, error) {
	text := strings.TrimSpace(x.Text)
	switch x.Action {
	case "note":
		if text == "" || utf8.RuneCountInString(text) > ticket.MaxNote {
			return "", fmt.Errorf("text 要写，最多 %d 字", ticket.MaxNote)
		}
		if ticket.IsEnded(t.Status) {
			return "", errors.New("这张工单已经结束了")
		}
		if _, err := a.tickets.Update(t.ID, func(t *ticket.Ticket) {
			t.Notes = append(t.Notes, ticket.Note{By: who, Text: text, At: a.now().Unix()})
		}); err != nil {
			a.ticketHealth(err)
			return "", fmt.Errorf("没有记下来（%v）", err)
		}
		a.ticketHealth(nil)
		return "记下了，会转给干活的分支。", nil
	case "accept":
		if t.Status != ticket.StatusReady {
			return "", fmt.Errorf("工单还没有就绪（现在是 %s）", t.Status)
		}
		return "", errors.New("收下工单还没有实现")
	case "cancel":
		if text == "" {
			return "", errors.New("叫停要在 text 里写理由")
		}
		if err := a.cancelTicketLocked(t, text); err != nil {
			return "", err
		}
		return "已叫停 " + t.ID + "。", nil
	}
	return "", errors.New("不认识的操作")
}

// cancelTicketLocked 叫停一张工单：结束为 stopped，理由必须有。调用方持有锁。
func (a *App) cancelTicketLocked(t ticket.Ticket, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("叫停要写理由")
	}
	if ticket.IsEnded(t.Status) {
		return errors.New("这张工单已经结束了")
	}
	if _, err := a.tickets.Update(t.ID, func(t *ticket.Ticket) { t.Status, t.Reason = ticket.StatusStopped, reason }); err != nil {
		a.ticketHealth(err)
		return fmt.Errorf("没有记下来（%v）", err)
	}
	a.ticketHealth(nil)
	a.publishCommitments()
	return nil
}
