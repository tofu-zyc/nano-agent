package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/proto"
)

// Report：干活的会话向主人汇报“主人需要知道什么”（设计 §15.2）。
// 哪些会话有：life 会话、分支、定时任务、Hook 的 agent 处理器；普通聊天和群聊没有（在原对话里直接回复就是汇报），戏里也没有。
// 汇报的是哪件事由宿主从这一轮推出，不由模型填。汇报只记录，不改承诺自己的状态。

const reportSchema = `{"type":"object","properties":{
"status":{"type":"string","enum":["progress","waiting","ready","blocked","failed","not_needed"],"description":"ready 有结果可用（要带 evidence）；blocked 卡住了、要人处理；failed 做不下去了；not_needed 不用做了；progress、waiting 只记一笔，不会发给任何人"},
"text":{"type":"string","description":"说明，最多 500 字；blocked、failed、not_needed 必须写"},
"question":{"type":"string","description":"要主人回答的一个具体问题，最多 200 字，只能和 blocked 一起用；定时任务和 Hook 这一轮结束后没有人接着问，不能带"},
"evidence":{"type":"array","items":{"type":"string"},"description":"ready 必填：从工具结果开头 [本轮验收凭据] 复制 id，必须是本轮真实成功的工具结果"}
},"required":["status"]}`

const (
	reportTextMax     = 500
	reportQuestionMax = 200
	reportMaxPerRun   = 4
)

func reportTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "Report", Description: "向主人汇报这件事现在的情况；汇报的是哪件事由系统从这一轮推出（自己安排的检查不对应任何承诺，不能汇报）。" +
		"只告诉主人情况，不改变这件事本身的状态：做完日程仍用 Agenda done。一轮最多汇报 4 次。",
		InputSchema: json.RawMessage(reportSchema)}
}

// reportSession 是这个会话有没有 Report：life 会话、分支、定时任务、Hook 的 agent 处理器。
func (a *App) reportSession(sid string) bool {
	if a.sceneIsolated(sid) {
		return false
	}
	return isLifeSID(sid) || isCronSID(sid) || isHookSID(sid) || a.isBranchSID(sid)
}

func (a *App) reportSpec(sid string) (proto.ToolSpec, bool) {
	if !a.reportSession(sid) {
		return proto.ToolSpec{}, false
	}
	return reportTool(), true
}

// reportSubject 从这一轮推出汇报的是哪件事（种类、ID、agent），以及这一轮之后有没有人接着问。
func (a *App) reportSubject(s *sess) (kind, id, agent string, canAsk bool, err error) {
	r := s.run
	if r == nil {
		return "", "", "", false, errors.New("现在没有在进行的一轮")
	}
	agent = a.execAgentID(s.sid)
	switch {
	case r.hook != nil: // Hook 的这一轮（跑在 life、分支或 Hook 自己的会话里都算）
		return commit.KindHook, r.hook.work.Delivery.HookID, r.hook.work.Delivery.AgentID, false, nil
	case isLifeSID(s.sid):
		if s.mind != nil {
			switch {
			case s.mind.Trigger.AgendaID != "":
				return commit.KindAgenda, s.mind.Trigger.AgendaID, agent, true, nil
			case s.mind.Trigger.IntentionID != "":
				return commit.KindIntention, s.mind.Trigger.IntentionID, agent, true, nil
			}
		}
		return "", "", "", false, errors.New("这一轮是自己安排的检查，不对应任何承诺，不能汇报；有结果想让主人知道，用 Message 发给他")
	case a.isTicketBranch(s.sid): // 工单的分支：汇报的是这张工单，不是分支
		t, _ := a.ticketOfSession(s.sid)
		return commit.KindTicket, t.ID, t.Agent, true, nil
	case a.isBranchSID(s.sid):
		return commit.KindBranch, s.sid, agent, true, nil
	case s.scheduled != nil:
		return commit.KindSchedule, s.scheduled.ID, s.scheduled.Agent, false, nil
	}
	return "", "", "", false, errors.New("这一轮没有可以汇报的承诺")
}

// reportCall 执行 Report。调用方持有锁。
func (a *App) reportCall(s *sess, raw []byte) (string, error) {
	var x struct {
		Status   string   `json:"status"`
		Text     string   `json:"text"`
		Question string   `json:"question"`
		Evidence []string `json:"evidence"`
	}
	if err := json.Unmarshal(raw, &x); err != nil {
		return "", errors.New("参数格式不对")
	}
	x.Text, x.Question = strings.TrimSpace(x.Text), strings.TrimSpace(x.Question)
	switch x.Status {
	case commit.StatusProgress, commit.StatusWaiting, commit.StatusReady, commit.StatusBlocked, commit.StatusFailed, commit.StatusNotNeeded:
	default:
		return "", errors.New("status 要是 progress、waiting、ready、blocked、failed、not_needed 之一")
	}
	if n := utf8.RuneCountInString(x.Text); n > reportTextMax {
		return "", fmt.Errorf("text 最多 %d 字（现在 %d）", reportTextMax, n)
	}
	if n := utf8.RuneCountInString(x.Question); n > reportQuestionMax {
		return "", fmt.Errorf("question 最多 %d 字（现在 %d）", reportQuestionMax, n)
	}
	if (x.Status == commit.StatusBlocked || x.Status == commit.StatusFailed || x.Status == commit.StatusNotNeeded) && x.Text == "" {
		return "", fmt.Errorf("%s 要在 text 里写清楚是怎么回事", x.Status)
	}
	if x.Question != "" && x.Status != commit.StatusBlocked {
		return "", errors.New("question 只能和 blocked 一起用")
	}
	kind, id, agent, canAsk, err := a.reportSubject(s)
	if err != nil {
		return "", err
	}
	if x.Question != "" && !canAsk {
		return "", errors.New("这一轮结束后没有人接着问，不能带 question；只汇报情况，要人处理就写在 text 里")
	}
	var evidence []string
	if x.Status == commit.StatusReady {
		if len(x.Evidence) == 0 {
			return "", errors.New("ready 要带 evidence：复制工具结果开头 [本轮验收凭据] 的 id")
		}
		for _, eid := range x.Evidence {
			e, ok := s.run.toolEvidence[eid]
			if !ok || !e.OK || !e.Verified || !taskEvidence(e.Name) {
				return "", fmt.Errorf("%s 不是本轮已成功的实际操作或观察，不能用作 ready 的凭据；请复制工具结果开头 [本轮验收凭据] 的 id", eid)
			}
			evidence = append(evidence, eid+":"+e.Name)
		}
	}
	if s.run.reports >= reportMaxPerRun {
		return "", fmt.Errorf("这一轮已经汇报 %d 次了，不能再汇报", reportMaxPerRun)
	}
	if a.commits == nil {
		return "", errors.New("承诺记录现在不可用，汇报没有记下来")
	}
	// 先记再说：写成功才算汇报了
	rec, err := a.commits.Append(commit.CommitRec{Type: commit.TypeReport, Kind: kind, ID: id, Agent: agent, Status: x.Status,
		Text: x.Text, Question: x.Question, Evidence: evidence, Run: s.run.runID})
	if err != nil {
		a.commitHealth(err)
		return "", fmt.Errorf("汇报没有记下来（%v），稍后再试", err)
	}
	a.commitHealth(nil)
	s.run.reports++
	a.publishCommitments()
	if kind == commit.KindTicket {
		a.ticketReported(id, x.Status, x.Text, x.Question)
	}
	if kind == commit.KindBranch && a.branchAttention(id) {
		switch x.Status {
		case commit.StatusReady, commit.StatusBlocked, commit.StatusFailed:
			a.attentionPush(id, x.Status, x.Text, x.Question, fmt.Sprint(rec.Seq))
		}
	}
	switch x.Status {
	case commit.StatusProgress, commit.StatusWaiting:
		return fmt.Sprintf("记下了（%s）。这一笔不会发给任何人。", x.Status), nil
	case commit.StatusBlocked:
		if x.Question != "" {
			return fmt.Sprintf("已汇报（blocked，第 %d 条）：主人会看到你的问题。", rec.Seq), nil
		}
	}
	return fmt.Sprintf("已汇报（%s，第 %d 条）。", x.Status, rec.Seq), nil
}
