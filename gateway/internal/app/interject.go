package app

import (
	"fmt"
	"strings"
	"time"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/media"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// ---- 用户插话（设计文档 §3.4） ----
//
// 当前一轮还没结束时，发起人在同一会话里又发来的普通消息是“插话”：它不取消正在
// 运行的命令、不打断正在生成的回复，而是在下一个安全边界注入：
//
//	一批工具调用全部闭合、请求下一次模型之前；
//	本次模型生成完整结束、把结果交给内核之前。
//
// 收件箱有界（条数 + 字节数），满了就明确回执“未受理”，不静默截断用户的要求。
// 后台 Job 不参与这里的等待条件：插话只等前台当前边界。
const (
	maxInterjections     = 16      // 每个 run 最多收几条待插入的补充
	maxInterjectionBytes = 64 << 10 // 每个 run 待插入的正文合计上限
	maxInterjectionIDs   = 64      // 记住最近多少个平台消息 ID，用来去重
)

// interjection 是一条已经受理、还没写进历史的用户补充。
type interjection struct {
	seq  int
	msg  channel.Message
	ep   endpoint
	who  string
	text string
	at   time.Time
}

// canInterject 判断这条消息能不能进入当前 run 的收件箱：只有本轮发起人的普通
// 消息才算；命令、审批、撤回、群聊、agent 自己的一轮都走原来的入口。
func (r *run) canInterject(who string) bool {
	if r == nil || r.inner || r.scheduled != nil {
		return false
	}
	return r.from == who
}

// acceptInterjection 把一条消息放进当前 run 的收件箱。返回 true 表示已经受理，
// 调用方不要再把它塞进会话队列。
func (a *App) acceptInterjection(s *sess, r *run, ep endpoint, m channel.Message, who string) bool {
	if m.MsgID != "" {
		if r.seenIDs == nil {
			r.seenIDs = map[string]bool{}
		}
		if r.seenIDs[m.MsgID] {
			return true // 渠道重发的同一条消息：已经受理过，别插两次
		}
	}
	text := strings.TrimSpace(m.Text)
	if text == "" && len(m.Images) == 0 {
		return false
	}
	// 条数和字节数都设上限：超出时明确回执未受理，提示稍后发送
	if len(r.inbox) >= maxInterjections || r.inboxBytes+len(text) > maxInterjectionBytes {
		a.deliver(ep, fmt.Sprintf("这条补充没有受理：当前任务已经收到 %d 条待处理的补充（上限 %d 条、%d KiB）。"+
			"请等这一轮处理完再发，或者先发 /stop 停下当前任务", len(r.inbox), maxInterjections, maxInterjectionBytes>>10), "")
		return true
	}
	r.interjectSeq++
	r.inbox = append(r.inbox, interjection{seq: r.interjectSeq, msg: m, ep: ep, who: who, text: text, at: a.now()})
	r.inboxBytes += len(text)
	if len(r.seenIDs) >= maxInterjectionIDs {
		r.seenIDs = map[string]bool{} // 只用来挡住紧跟着的重发，满了就清掉
	}
	if m.MsgID != "" {
		r.seenIDs[m.MsgID] = true
	}
	// 工具还在跑就等这一批结束；正在生成就等这次生成结束
	state := "本轮工具调用结束"
	if r.cancelLLM != nil {
		state = "这次生成结束"
	}
	if !r.notified {
		r.notified = true
		a.deliver(ep, "已收到插话，会等"+state+"后处理。", "")
	}
	a.publish(events.Event{T: "interjection", SID: s.sid, ID: m.MsgID, Kind: "accepted",
		Text: text, Data: map[string]any{"waiting_for": state, "pending": len(r.inbox)}})
	return true
}

// drainInterjections 把收件箱里的一批补充写进正式历史（合成一条 user 消息，带明确
// 标记）。返回是否真的写进去了；写失败时把消息放回收件箱，下一次边界再试。
func (a *App) drainInterjections(s *sess, r *run) bool {
	if len(r.inbox) == 0 {
		return false
	}
	batch := r.inbox
	r.inbox, r.inboxBytes = nil, 0

	var images []media.Attachment
	ids := make([]string, 0, len(batch))
	for _, it := range batch {
		images = append(images, it.msg.Images...)
		ids = append(ids, it.msg.MsgID)
	}
	msg := session.Msg{
		Role:      "user",
		Content:   wrapInterjections(batch),
		Images:    images,
		Kind:      "user_interjection",
		ParentRun: r.runID,
		SourceID:  strings.Join(ids, ","),
		Sender:    batch[0].who,
		TS:        a.now().Unix(),   // 和其他历史消息一样用秒；毫秒是接收时间，另记在事件里
	}
	if err := a.store.Append(s.sid, msg); err != nil {
		// 写历史失败：消息留着，下一个边界再试（仍受 run 总时限约束）
		a.log.Warn("写入用户插话失败，等下一个边界重试", "sid", s.sid, "err", err)
		r.inbox = append(batch, r.inbox...)
		for _, it := range batch {
			r.inboxBytes += len(it.text)
		}
		return false
	}
	r.injected += len(batch)
	r.notified = false
	last := batch[len(batch)-1].ep
	a.deliver(last, "已插入，接下来会结合补充继续。", "")
	a.publish(events.Event{T: "interjection", SID: s.sid, Kind: "injected", Text: msg.Content,
		Data: map[string]any{"count": len(batch), "parent_run_id": r.runID}})
	a.log.Info("插入用户插话", "sid", s.sid, "run", r.runID, "count", len(batch))
	return true
}

// wrapInterjections 是给模型看的正文：宿主机生成标记和元信息，用户原文原样保留，
// 仍然是 user 角色，不改写成 system。
func wrapInterjections(batch []interjection) string {
	var b strings.Builder
	b.WriteString("[用户插话]\n")
	b.WriteString("这是执行当前任务期间收到的用户补充，已等待上一轮工具调用或本次模型生成结束。\n")
	b.WriteString("请结合这条补充调整后续步骤，并以真实工具结果判断已经完成的操作。\n")
	for i, it := range batch {
		if i > 0 {
			b.WriteString("---\n")
		}
		fmt.Fprintf(&b, "发送者：%s\n", it.who)
		fmt.Fprintf(&b, "收到时间：%s\n", it.at.Format("2006-01-02 15:04:05"))
		b.WriteString("用户原文：\n")
		if it.text == "" {
			b.WriteString(fmt.Sprintf("（发来 %d 张图片）", len(it.msg.Images)))
		} else {
			b.WriteString(it.text)
		}
		b.WriteString("\n")
	}
	b.WriteString("[/用户插话]")
	return b.String()
}

// requeueInterjections 把这一轮没来得及插入的补充放回会话队列，交给后续 run 处理，
// 并明确告诉用户，不能悄悄丢掉。
func (a *App) requeueInterjections(s *sess, r *run) []queued {
	if len(r.inbox) == 0 {
		return nil
	}
	batch := r.inbox
	r.inbox, r.inboxBytes = nil, 0
	pending := make([]queued, 0, len(batch))
	for _, it := range batch {
		pending = append(pending, queued{m: it.msg, ep: it.ep, who: it.who})
	}
	a.deliver(batch[len(batch)-1].ep,
		fmt.Sprintf("这一轮没有处理你后来的 %d 条补充，已经放在下一轮接着处理。", len(batch)), "")
	a.publish(events.Event{T: "interjection", SID: s.sid, Kind: "requeued",
		Data: map[string]any{"count": len(batch)}})
	return pending
}

// pendingInterjections 是给界面和日志看的当前状态。
func (r *run) pendingInterjections() int { return len(r.inbox) }

// yieldToInterjection 是“生成完整结束、把结果交给内核之前”的检查点（设计文档 §3.4）。
// 这次生成已经跑完、用量也结算过了，如果这期间收到了插话：
//   - 正文照常保存并展示，run 不结束；
//   - 这次提出的 tool_calls 还没交给内核，不派发、也不写进正式历史（不留孤立调用）；
//   - 写进插话后重新请求模型，由下一步结合补充重新规划。
//
// 返回 true 表示这次生成的结果已经让位给插话，调用方不要再发 turn。
func (a *App) yieldToInterjection(s *sess, r *run, res *llm.Result, u llm.Usage) bool {
	if len(r.inbox) == 0 || s.run != r {
		return false
	}
	switch res.Reason {
	case llm.ReasonToolUse, llm.ReasonFinal:
	default:
		return false // 拒绝等结局按原样收尾
	}
	// 旧节点不认识 model_yield：不能在这里自己重开一次模型请求（那样绕过了内核的步数预算）。
	// 退回原来的行为：补充放回会话队列，这一轮按正常结果收尾，下一轮再处理。
	if !a.interjectionReady(r.node) {
		if left := a.requeueInterjections(s, r); len(left) > 0 {
			s.queue = append(left, s.queue...)
		}
		return false
	}
	// 已经生成出来的正文照常保存：它确实说过了，保留它，不伪装成“已经考虑了后来的补充”。
	// 注意不带 ToolCalls —— 这批调用没有派发，写进去就没有配对结果了。
	if strings.TrimSpace(res.Content) != "" || res.Reasoning != "" {
		msg := session.Msg{Role: "assistant", Content: res.Content, Reasoning: res.Reasoning,
			Usage: &u, Agent: a.execAgentID(s.sid), TS: a.now().UnixMilli()}
		if err := a.store.Append(s.sid, msg); err != nil {
			a.log.Warn("保存生成结果失败", "sid", s.sid, "err", err)
		}
		r.streamed = r.streamed || res.Content != ""
	}
	if res.Reason == llm.ReasonToolUse && len(res.Calls) > 0 {
		r.dropped += len(res.Calls)
		a.log.Info("生成后收到插话：这一批工具没有派发，交给模型重新规划", "sid", s.sid, "calls", len(res.Calls))
		a.publish(events.Event{T: "interjection", SID: s.sid, Kind: "dropped_calls",
			Data: map[string]any{"count": len(res.Calls)}})
	}
	r.final = ""
	a.drainInterjections(s, r)
	// 重新请求模型这件事交给内核决定：它要按自己的步数预算算这一次，插话不能绕过上限。
	// 内核不认识 model_yield（旧节点）才退回网关自己请求。
	if a.interjectionReady(r.node) {
		a.hub.Send(r.node, proto.Msg{T: proto.TModelYield, SID: s.sid, RunID: r.runID, TurnID: r.turnID})
	} else {
		a.callModel(s)
	}
	return true
}

// interjectionReady 报告这个内核认不认识 model_yield（协议 v11 的能力集合）。
func (a *App) interjectionReady(node string) bool {
	n := a.nodes[node]
	return n != nil && n.info.Features["user_interjection"] == 1
}
