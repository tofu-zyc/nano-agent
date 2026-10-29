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
 "action":{"type":"string","enum":["list","add","move","cancel","progress","done"]},
 "id":{"type":"string","description":"议程项的 ID（move、cancel、done 必填；先 list 查）"},
 "title":{"type":"string","description":"add：要做什么"},
 "kind":{"type":"string","enum":["commitment","checkup","plan","exploration"],"description":"add：commitment 和用户明确约定的；checkup 约好过会儿再看；plan 自己定的；exploration 自选探索"},
 "when":{"type":"string","description":"add/move：15:30、明天 09:00、2026-10-27 15:00、+30m"},
 "minutes":{"type":"integer","description":"预计要做多久（分钟）"},
 "note":{"type":"string","description":"到时要想起的事；done 时写真实结果（做了什么、看到了什么）"},
 "criteria":{"type":"string","description":"add：做到什么、如何检查才算完成"},
 "checkpoint":{"type":"string","description":"progress：已经做到哪、下一步做什么、哪些操作结果未知而不能重放"},
 "evidence":{"type":"array","items":{"type":"string"},"description":"done 必填：从工具结果开头 [本轮验收凭据] 复制 id 字符串（也可用 list 查），不是工具名、描述或结果文字；查询日程/想法/心智不能作证据"},
 "movable":{"type":"boolean","description":"探索可以挪动，约定默认不挪"},
 "intention":{"type":"string","description":"推进哪个目标（意图 ID，可省略）"}
},"required":["action"]}`

func agendaTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "Agenda", Description: "你自己的日程：查看、安排、挪动、取消，或者报告一件事真的做完了。这是把事情交给以后的自己的显式入口；聊过的话不会自动变成待办。" +
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

const mindSchema = `{"type":"object","properties":{
 "action":{"type":"string","enum":["view","interest_add","interest_remove","belief","reflect"]},
 "text":{"type":"string","description":"interest_add：兴趣内容；interest_remove：要删掉的兴趣原文；reflect：这次反思"},
 "source":{"type":"string","description":"interest_add / belief：来源，写清从哪次经历、哪段聊天或哪次反思来的（必填）"},
 "subject":{"type":"string","description":"belief：这条认识是关于什么的（比如某个人、某件事）"},
 "value":{"type":"string","description":"belief：你认为它是怎样的"},
 "confidence":{"type":"number","description":"belief：置信度 0–1，默认 0.6"},
 "change":{"type":"string","description":"reflect：这次反思之后打算怎么调整"}
},"required":["action"]}`

func mindTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "Mind", Description: "管理你跨轮次的心智状态：长期兴趣、对人和事的认识、反思。" +
		"兴趣和认识必须写来源；它们会一直留着，下次醒来还能看到。先 view 看现在有什么。" +
		"不要用它改人格或权限。", InputSchema: json.RawMessage(mindSchema)}
}

// lifeMind 是这个会话所属的拟人型 agent 的心智状态。调用方持有锁。
func (a *App) lifeMind(sid string) (*autonomy.State, agents.Agent, error) {
	ag := a.agentOf(sid)
	a.loadMinds()
	if a.mindsErr != nil {
		return nil, ag, a.mindsErr
	}
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
		Action     string   `json:"action"`
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		Kind       string   `json:"kind"`
		When       string   `json:"when"`
		Minutes    int      `json:"minutes"`
		Note       string   `json:"note"`
		Criteria   string   `json:"criteria"`
		Checkpoint string   `json:"checkpoint"`
		Evidence   []string `json:"evidence"`
		Movable    *bool    `json:"movable"`
		Intention  string   `json:"intention"`
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
				if it.Criteria != "" {
					fmt.Fprintf(&b, "  完成条件：%s\n", it.Criteria)
				}
				if it.Checkpoint != "" {
					fmt.Fprintf(&b, "  检查点：%s\n", it.Checkpoint)
				}
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
		if s := a.sessions[sid]; s != nil {
			if receipts := availableTaskEvidence(s.run); receipts != "" {
				fmt.Fprintf(&b, "本轮可用验收凭据（evidence 复制 id；仍需按完成条件核对结果）：\n%s\n", receipts)
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
			Movable: movable, Source: "chat:" + sid, Note: strings.TrimSpace(x.Note), IntentionID: x.Intention,
			Criteria: strings.TrimSpace(x.Criteria), ScopeSet: true, Workspaces: a.workspaceOf(sid).ids(), Mode: a.workspaceOf(sid).mode}, now)
		if err != nil {
			return "", err
		}
		if kind == autonomy.KindCommitment {
			st.AddEvent(autonomy.Event{Kind: autonomy.EventCommitment, Source: "agenda:" + item.ID, SID: sid, AgendaID: item.ID,
				Dedupe: "agenda:" + item.ID, Text: item.Title}, now, 0)
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
	case "progress":
		if err := st.SetCheckpoint(x.ID, x.Checkpoint, now); err != nil {
			return "", err
		}
		a.afterMindChange(st, ag, now)
		return "检查点已保存，下次从这里继续。", nil
	case "done":
		if err := a.stageAgendaCompletion(sid, x.ID, x.Note, x.Evidence); err != nil {
			return "", err
		}
		return "验收结果已记录，本轮正常结束后提交；中断或失败不会记成完成。", nil
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
		// 人工锁定的数值覆盖模型的提议（设计 §7.3）：锁定期间想法按用户给的值排序
		note := ""
		if v, ok := st.Profile().Pin("interest"); ok && int(v) != x.Interest {
			x.Interest, note = int(v), note+"兴趣按锁定值 "+fmt.Sprint(int(v))+"。"
		}
		if v, ok := st.Profile().Pin("novelty"); ok && int(v) != x.Novelty {
			x.Novelty, note = int(v), note+"新颖按锁定值 "+fmt.Sprint(int(v))+"。"
		}
		it, err := st.AddIdea(autonomy.Idea{Text: x.Text, Source: x.Source, Interest: x.Interest, Novelty: x.Novelty,
			CostMinutes: x.CostMinutes}, now, 7*24*time.Hour)
		if err != nil {
			return "", err
		}
		a.afterMindChange(st, ag, now)
		return "记下了想法 " + it.ID + "：" + it.Text + note, nil
	case "schedule":
		at, err := autonomy.ParseWhen(x.When, now, a.loc)
		if err != nil {
			return "", err
		}
		in, item, err := st.ScheduleIdea(x.ID, at.Unix(), x.CostMinutes, now)
		if err != nil {
			return "", err
		}
		item.ScopeSet, item.Workspaces, item.Mode = true, a.workspaceOf(sid).ids(), a.workspaceOf(sid).mode
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

// mindCall 执行 Mind：查看或更新长期兴趣、认识、反思。调用方持有锁。
func (a *App) mindCall(sid string, raw []byte) (string, error) {
	var x struct {
		Action     string  `json:"action"`
		Text       string  `json:"text"`
		Source     string  `json:"source"`
		Subject    string  `json:"subject"`
		Value      string  `json:"value"`
		Confidence float64 `json:"confidence"`
		Change     string  `json:"change"`
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
	case "view", "":
		return a.mindSummary(st, now), nil
	case "interest_add":
		it, err := st.AddInterest(x.Text, x.Source, now)
		if err != nil {
			return "", err
		}
		a.afterMindChange(st, ag, now)
		return "记下了长期兴趣：" + it.Text + "（来源：" + it.Source + "）", nil
	case "interest_remove":
		if !st.RemoveInterest(x.Text, now) {
			return "", errors.New("没有这条兴趣")
		}
		a.afterMindChange(st, ag, now)
		return "不再追这个兴趣了。", nil
	case "belief":
		if strings.TrimSpace(x.Subject) == "" || strings.TrimSpace(x.Value) == "" {
			return "", errors.New("belief 要写 subject 和 value")
		}
		if strings.TrimSpace(x.Source) == "" {
			return "", errors.New("认识要写来源（从哪次观察、聊天或反思来的）")
		}
		conf := x.Confidence
		if conf <= 0 {
			conf = 0.6
		}
		if conf > 1 {
			conf = 1
		}
		st.SetBelief(x.Subject, x.Value, x.Source, conf, now)
		a.afterMindChange(st, ag, now)
		return "记下了这条认识（置信度 " + fmt.Sprintf("%.0f%%", conf*100) + "）。", nil
	case "reflect":
		if strings.TrimSpace(x.Text) == "" {
			return "", errors.New("reflect 要写这次反思")
		}
		st.AddReflection(x.Text, x.Change, now)
		a.afterMindChange(st, ag, now)
		return "记下了这次反思。", nil
	}
	return "", fmt.Errorf("不认识的 Mind action %q", x.Action)
}

// mindSummary 是 Mind view 看到的：兴趣、认识、反思、目标、日程。
func (a *App) mindSummary(st *autonomy.State, now time.Time) string {
	var b strings.Builder
	if len(st.Self.Interests) == 0 {
		b.WriteString("长期兴趣：还没有。\n")
	} else {
		b.WriteString("长期兴趣：\n")
		for _, x := range st.Self.Interests {
			fmt.Fprintf(&b, "- %s（来源：%s，%s）\n", x.Text, x.Source, autonomy.FormatMinute(x.At, a.loc))
		}
	}
	if len(st.Beliefs) == 0 {
		b.WriteString("认识：还没有。\n")
	} else {
		b.WriteString("认识：\n")
		start := max(0, len(st.Beliefs)-8)
		for _, x := range st.Beliefs[start:] {
			fmt.Fprintf(&b, "- %s：%s（来源：%s，置信度 %.0f%%，%s）\n", x.Subject, x.Value, x.Source, x.Confidence*100, autonomy.FormatMinute(x.At, a.loc))
		}
	}
	if len(st.Reflections) > 0 {
		b.WriteString("最近反思：\n")
		start := max(0, len(st.Reflections)-3)
		for _, x := range st.Reflections[start:] {
			fmt.Fprintf(&b, "- %s %s", autonomy.FormatMinute(x.At, a.loc), x.Text)
			if x.Change != "" {
				b.WriteString("（调整：" + x.Change + "）")
			}
			b.WriteString("\n")
		}
	}
	if intents := st.ActiveIntentions(); len(intents) > 0 {
		b.WriteString("在推进的目标：\n")
		for _, in := range intents {
			fmt.Fprintf(&b, "- %s（%s，进度 %d，%s）\n", in.Title, autonomy.StatusLabel(in.State), in.Progress, in.ID)
		}
	}
	if open := st.OpenAgenda(); len(open) > 0 {
		b.WriteString("日程：\n")
		for _, it := range open {
			fmt.Fprintf(&b, "- %s %s（%s，%s）\n", autonomy.FormatMinute(it.At, a.loc), it.Title, autonomy.KindLabel(it.Kind), it.ID)
		}
	}
	return strings.TrimSpace(b.String())
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
