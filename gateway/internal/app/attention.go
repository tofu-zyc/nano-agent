package app

import (
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/session"
)

// attention 投递档（设计 §15.3）：分支的第三种投递档。和 silent 一样安静，
// 唯一的例外是这个分支的 Report 是 ready、blocked 或 failed，或者宿主记下这一轮出错：
// 发一条通知到父会话最近的窗口。通知不写进父会话的历史。

const (
	attentionPerHour = 3 // 每个分支每小时最多推几条
	attentionNote    = "\n\n[分支] 这个分支的结果主人只会在你用 Report 汇报时看到：有结果时用 Report ready 汇报，卡住时用 Report blocked 问一个具体问题；其他时候主人看不到你。"
)

// branchAttention 是这个会话是不是 attention 分支。
func (a *App) branchAttention(sid string) bool {
	return a.branchDelivery(sid) == session.BranchDeliveryAttention
}

// attentionPush 推一条通知。state 是 ready、blocked、failed（宿主自己记下的出错也用 blocked），
// number 是聊天里 /answer、/seen 用的编号。先写记录，再入队；入队前崩溃的那一条只留在“等我处理”里，不补发。
// 调用方持有锁。
func (a *App) attentionPush(sid, state, text, question, number string) {
	m, ok := a.store.Branch(sid)
	if !ok || a.commits == nil || m.ParentSID == "" || a.branchSilent(m.ParentSID) {
		return
	}
	notes := a.commits.Notes(commit.KindBranch, sid)
	if n := len(notes); n > 0 && notes[n-1].Status == state && notes[n-1].Text == text {
		return // 和上一条推送一样
	}
	recent, cutoff := 0, a.now().Add(-time.Hour).UnixMilli()
	for _, n := range notes {
		if n.TS >= cutoff {
			recent++
		}
	}
	if recent >= attentionPerHour {
		return // 多出来的只留在“等我处理”里
	}
	if _, err := a.commits.Append(commit.CommitRec{Type: commit.TypeNotify, Kind: commit.KindBranch, ID: sid, Agent: m.OwnerAgent, Status: state, Text: text}); err != nil {
		a.commitHealth(err)
		return
	}
	if n := a.branchNumber(sid); n != "" { // 通知里的编号要和“等我处理”里的一致
		number = n
	}
	body := attentionBody(m.Title, state, text, question, number)
	a.publish(events.Event{T: "say", SID: m.ParentSID, Text: body})
	if ep, ok := a.lastEndpoint(m.ParentSID); ok {
		a.deliver(ep, body, "")
	}
}

func attentionBody(title, state, text, question, number string) string {
	label := map[string]string{"ready": "就绪", "blocked": "受阻", "failed": "失败"}[state]
	if question != "" {
		label = "需要你回答"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[分支「%s」· %s] %s", title, label, text)
	if question != "" {
		fmt.Fprintf(&b, "\n问：%s\n回复 /answer %s <你的回答>，或在网页的“等我处理”里回答。", question, number)
	} else if state != "ready" {
		fmt.Fprintf(&b, "\n在网页的“等我处理”里处理，或回复 /seen %s 记为知道了。", number)
	}
	return b.String()
}
