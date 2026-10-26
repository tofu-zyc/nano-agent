package app

// 自主活动的工具（Agenda / Idea）：模型在聊天或内心活动里用它安排日程、记下想法。
// 参数都校验，写进心智状态以后立刻持久化；网页上能看、能改。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/proto"
)

const agendaSchema = `{"type":"object","properties":{
 "action":{"type":"string","enum":["list","add","move","cancel","done"]},
 "id":{"type":"string","description":"议程项的 ID（move、cancel、done 必填；先 list 查）"},
 "title":{"type":"string","description":"add：要做什么"},
 "kind":{"type":"string","enum":["commitment","checkup","plan","exploration"],"description":"add：commitment 和用户明确约定的；checkup 约好过会儿再看；plan 自己定的；exploration 自选探索"},
 "when":{"type":"string","description":"add/move：15:30、明天 09:00、2026-10-27 15:00、+30m"},
 "minutes":{"type":"integer","description":"预计要做多久（分钟）"},
 "note":{"type":"string","description":"到时要想起的事；done 时写真实结果（做了什么、看到了什么）"},
 "movable":{"type":"boolean","description":"探索可以挪动，约定默认不挪"},
 "intention":{"type":"string","description":"推进哪个目标（意图 ID，可省略）"}
},"required":["action"]}`

func agendaTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "Agenda", Description: "你自己的日程：查看、安排、挪动、取消，或者报告一件事真的做完了。" +
		"和人有明确约定（几点提醒、答应做什么）、或决定以后推进什么，就用它排进日程；到点时你会自己醒来处理（明确的约定不受作息和“最近没人找”限制）。" +
		"done 必须写真实结果，不能只说做完了。", InputSchema: json.RawMessage(agendaSchema)}
}

const ideaSchema = `{"type":"object","properties":{
 "action":{"type":"string","enum":["list","add","schedule","drop","defer"]},
 "id":{"type":"string","description":"想法 ID（schedule、drop、defer 必填）"},
 "text":{"type":"string","description":"add：想到了什么"},
 "source":{"type":"string","description":"add：来源，比如最近看到的哪件事、哪段记忆、哪次聊天（必填）"},
 "interest":{"type":"integer","description":"多感兴趣，1–5"},
 "novelty":{"type":"integer","description":"多新，1–5"},
 "cost_minutes":{"type":"integer","description":"大概花多少分钟"},
 "when":{"type":"string","description":"schedule 什么时候做；defer 推迟到什么时候"}
},"required":["action"]}`

func ideaTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "Idea", Description: "突发想法池：记下、挑一个排进日程去探索、推迟或放弃。" +
		"想法必须有来源（你最近经历的什么事、哪段记忆、哪次聊天），不能凭空冒出来；允许一个都不做。" +
		"schedule 会变成目标和一段可以挪动的探索时间。", InputSchema: json.RawMessage(ideaSchema)}
}

// lifeMind 是这个会话所属的拟人型 agent 的心智状态。调用方持有锁。
func (a *App) lifeMind(sid string) (*autonomy.State, agents.Agent, error) {
	ag := a.agentOf(sid)
	if !ag.Life() {
		return nil, ag, errors.New("只有拟人型 agent 有自己的日程和想法")
	}
	if a.agentRoot == "" {
		return nil, ag, errors.New("agent 目录还没有准备好")
	}
	return a.mind(ag.ID), ag, nil
}

// agendaCall 执行 Agenda。调用方持有锁。
func (a *App) agendaCall(sid string, raw []byte) (string, error) {
	var x struct {
		Action    string `json:"action"`
		ID        string `json:"id"`
		Title     string `json:"title"`
		Kind      string `json:"kind"`
		When      string `json:"when"`
		Minutes   int    `json:"minutes"`
		Note      string `json:"note"`
		Movable   *bool  `json:"movable"`
		Intention string `json:"intention"`
	}
	if err := json.Unmarshal(raw, &x); err != nil {
		return "", errors.New("参数格式不对")
	}
	st, ag, err := a.lifeMind(sid)
	if err != nil {
		return "", err
	}
	now := a.now().In(a.loc)
	switch x.Action {
	case "list", "":
		var b strings.Builder
		if open := st.OpenAgenda(); len(open) > 0 {
			b.WriteString("日程：\n")
			for _, it := range open {
				fmt.Fprintf(&b, "- %s %s（%s，%s，%s）\n", autonomy.FormatMinute(it.At, a.loc), it.Title,
					autonomy.KindLabel(it.Kind), autonomy.StatusLabel(it.Status), it.ID)
			}
		} else {
			b.WriteString("日程是空的。\n")
		}
		if intents := st.ActiveIntentions(); len(intents) > 0 {
			b.WriteString("在推进的目标：\n")
			for _, in := range intents {
				fmt.Fprintf(&b, "- %s（%s，进度 %d，%s）\n", in.Title, autonomy.StatusLabel(in.State), in.Progress, in.ID)
			}
		}
		return strings.TrimSpace(b.String()), nil
	case "add":
		at, err := autonomy.ParseWhen(x.When, now, a.loc)
		if err != nil {
			return "", err
		}
		if x.Minutes < 0 || x.Minutes > 240 {
			return "", errors.New("minutes 要在 0 到 240 之间")
		}
		kind := x.Kind
		if kind == "" {
			kind = autonomy.KindPlan
		}
		movable := kind == autonomy.KindExploration
		if x.Movable != nil && kind == autonomy.KindExploration {
			movable = *x.Movable
		}
		item, err := st.AddAgenda(autonomy.AgendaItem{Title: x.Title, Kind: kind, At: at.Unix(), Minutes: x.Minutes,
			Movable: movable, Source: "chat:" + sid, Note: strings.TrimSpace(x.Note), IntentionID: x.Intention}, now)
		if err != nil {
			return "", err
		}
		if kind == autonomy.KindCommitment {
			st.AddEvent(autonomy.Event{Kind: autonomy.EventCommitment, Source: "agenda:" + item.ID, SID: sid, AgendaID: item.ID,
				Dedupe: "agenda:" + item.ID, Text: item.Title}, now, 0)
			st.SetBelief("和对方的约定", item.Title, "agenda:"+item.ID, 0.9, now)
		}
		a.afterMindChange(st, ag, now)
		return "排好了：" + at.In(a.loc).Format("01-02 15:04") + " " + item.Title + "（" + item.ID + "）", nil
	case "move":
		at, err := autonomy.ParseWhen(x.When, now, a.loc)
		if err != nil {
			return "", err
		}
		if err := st.MoveAgenda(x.ID, at.Unix(), now); err != nil {
			return "", err
		}
		a.afterMindChange(st, ag, now)
		return "挪到 " + at.In(a.loc).Format("01-02 15:04") + " 了。", nil
	case "cancel":
		if err := st.CancelAgenda(x.ID, x.Note, now); err != nil {
			return "", err
		}
		a.afterMindChange(st, ag, now)
		return "取消了。", nil
	case "done":
		if err := st.CompleteAgenda(x.ID, x.Note, now); err != nil {
			return "", err
		}
		a.afterMindChange(st, ag, now)
		return "记下了结果。", nil
	}
	return "", fmt.Errorf("不认识的 Agenda action %q", x.Action)
}

// ideaCall 执行 Idea。调用方持有锁。
func (a *App) ideaCall(sid string, raw []byte) (string, error) {
	var x struct {
		Action      string `json:"action"`
		ID          string `json:"id"`
		Text        string `json:"text"`
		Source      string `json:"source"`
		Interest    int    `json:"interest"`
		Novelty     int    `json:"novelty"`
		CostMinutes int    `json:"cost_minutes"`
		When        string `json:"when"`
	}
	if err := json.Unmarshal(raw, &x); err != nil {
		return "", errors.New("参数格式不对")
	}
	st, ag, err := a.lifeMind(sid)
	if err != nil {
		return "", err
	}
	now := a.now().In(a.loc)
	switch x.Action {
	case "list", "":
		ideas := st.OpenIdeas(now)
		if len(ideas) == 0 {
			return "现在没有候选的想法。", nil
		}
		var b strings.Builder
		b.WriteString("候选的想法：\n")
		for _, it := range ideas {
			fmt.Fprintf(&b, "- %s（%s，兴趣 %d，约 %d 分钟，来源：%s）\n", it.Text, it.ID, it.Interest, it.CostMinutes, it.Source)
		}
		return strings.TrimSpace(b.String()), nil
	case "add":
		it, err := st.AddIdea(autonomy.Idea{Text: x.Text, Source: x.Source, Interest: x.Interest, Novelty: x.Novelty,
			CostMinutes: x.CostMinutes}, now, 7*24*time.Hour)
		if err != nil {
			return "", err
		}
		a.afterMindChange(st, ag, now)
		return "记下了想法 " + it.ID + "：" + it.Text, nil
	case "schedule":
		at, err := autonomy.ParseWhen(x.When, now, a.loc)
		if err != nil {
			return "", err
		}
		in, item, err := st.ScheduleIdea(x.ID, at.Unix(), x.CostMinutes, now)
		if err != nil {
			return "", err
		}
		a.afterMindChange(st, ag, now)
		return "好，" + at.In(a.loc).Format("01-02 15:04") + " 去做「" + in.Title + "」（" + item.ID + "）", nil
	case "drop":
		if err := st.DropIdea(x.ID, x.Source, now); err != nil {
			return "", err
		}
		a.afterMindChange(st, ag, now)
		return "放弃了。", nil
	case "defer":
		at, err := autonomy.ParseWhen(x.When, now, a.loc)
		if err != nil {
			return "", err
		}
		if err := st.DeferIdea(x.ID, at.Unix(), now); err != nil {
			return "", err
		}
		a.afterMindChange(st, ag, now)
		return "推迟到 " + at.In(a.loc).Format("01-02 15:04") + " 再想。", nil
	}
	return "", fmt.Errorf("不认识的 Idea action %q", x.Action)
}

// afterMindChange 保存改过的心智状态、把下次检查提前到最近一件事，并通知网页。调用方持有锁。
func (a *App) afterMindChange(st *autonomy.State, ag agents.Agent, now time.Time) {
	st.SetBudgetLimit(pickInt(ag.LifeBudget, 0))
	a.scheduleMindNext(st, ag, now)
	ls := a.lifeOf(ag.ID)
	if next, ok := st.NextDueTime(); ok {
		ls.Next = next.Unix()
	}
	a.saveMinds()
	a.saveLife()
	a.publishAutonomy(ag.ID)
}
