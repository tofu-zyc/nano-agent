package app

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/session"
)

// 等我处理（设计 §15.3）：阶段是受阻或就绪的承诺，最新的在前；只给管理员。
// “知道了”和回答都先写进承诺记录（seen、answer）再生效。

// maxAnswerRunes 是回答的长度上限。
const maxAnswerRunes = 2000

// PendingView 是等我处理里的一件事。Number 是聊天里用的编号：有汇报的用汇报序号，没有的用 Mark。
type PendingView struct {
	CommitView
	Number string `json:"number"`
}

func pendingNumber(v CommitView) string { return markNumber(v.Mark) }

func markNumber(mark string) string {
	if n, ok := strings.CutPrefix(mark, "r:"); ok {
		return n
	}
	return mark
}

// branchNumber 是这个分支在“等我处理”里的编号：决定阶段的那条事实的 Mark（汇报的 r:<序号>，或宿主自己的）。
// 不看这一轮是不是还在跑，通知发出时它可能还没结束。没有 Mark 返回空。调用方持有锁。
func (a *App) branchNumber(sid string) string {
	m, ok := a.store.Branch(sid)
	if !ok {
		return ""
	}
	it := branchCommit(m, false, autonomousForker(m.CreatedBy), a.now().Unix())
	a.overlayReport(&it)
	return markNumber(it.Facts.Mark)
}

// Pending 列出等我处理的承诺，最新的在前。
func (a *App) Pending() []PendingView {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pendingLocked()
}

func (a *App) pendingLocked() []PendingView {
	out := []PendingView{}
	for _, v := range a.commitViewsLocked("", "") {
		// 受阻、就绪的列进来；还没回答的问题不管这件事现在处在哪个阶段（比如这一轮还在跑）也列：
		// 主人照通知回 /answer，回答会排进下一个安全边界。结束了的不列。
		listed := v.Phase == commit.Blocked || v.Phase == commit.Ready || (v.Question != "" && !commit.IsEnded(v.Phase))
		if !listed || v.Mark == "" {
			continue
		}
		out = append(out, PendingView{CommitView: v, Number: pendingNumber(v)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

// findPending 按种类、ID 和 Mark 找一件等我处理的事。Mark 对不上说明它已经变了。
func (a *App) findPending(kind, id, mark string) (PendingView, error) {
	for _, p := range a.pendingLocked() {
		if p.Kind == kind && p.ID == id {
			if p.Mark != mark {
				return PendingView{}, errors.New("这件事已经有了新的情况，刷新再看")
			}
			return p, nil
		}
	}
	return PendingView{}, errors.New("这件事不在等我处理里（可能已经处理过了）")
}

func (a *App) recordSeen(typ string, p PendingView, mark, by, answer string) error {
	if a.commits == nil {
		return errors.New("承诺记录没有打开，现在记不了")
	}
	_, err := a.commits.Append(commit.CommitRec{Type: typ, Kind: p.Kind, ID: p.ID, Mark: mark, By: by, Answer: answer})
	a.commitHealth(err)
	return err
}

// PendingSeen 是“知道了”。汇报的就绪盖住了宿主自己的就绪时，两个 Mark 一起记，不让主人点两次。
func (a *App) PendingSeen(kind, id, mark, by string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pendingSeenLocked(kind, id, mark, by)
}

func (a *App) pendingSeenLocked(kind, id, mark, by string) error {
	p, err := a.findPending(kind, id, mark)
	if err != nil {
		return err
	}
	if p.Covered != "" { // 先记被盖住的：中途出错剩下的是汇报那条，主人再点一次就好
		if err := a.recordSeen(commit.TypeSeen, p, p.Covered, by, ""); err != nil {
			return err
		}
	}
	if err := a.recordSeen(commit.TypeSeen, p, mark, by, ""); err != nil {
		return err
	}
	a.publishCommitments()
	return nil
}

// PendingAnswer 回答一个问题：先记，再送到这件事干活的地方。
func (a *App) PendingAnswer(kind, id, mark, text, by string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pendingAnswerLocked(kind, id, mark, text, by)
}

func (a *App) pendingAnswerLocked(kind, id, mark, text, by string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("回答不能是空的")
	}
	if utf8.RuneCountInString(text) > maxAnswerRunes {
		return fmt.Errorf("回答最多 %d 字", maxAnswerRunes)
	}
	p, err := a.findPending(kind, id, mark)
	if err != nil {
		return err
	}
	if p.Question == "" {
		return errors.New("这件事没有要回答的问题")
	}
	if err := a.checkAnswerTarget(p); err != nil { // 副作用之前先判断
		return err
	}
	if err := a.recordSeen(commit.TypeAnswer, p, mark, by, text); err != nil {
		return err
	}
	a.deliverAnswer(p, text, by)
	a.publishCommitments()
	return nil
}

func (a *App) checkAnswerTarget(p PendingView) error {
	switch p.Kind {
	case commit.KindBranch:
		m, ok := a.store.Branch(p.ID)
		if !ok {
			return errors.New("没有这个分支")
		}
		if m.Status == session.BranchArchived || m.Status == session.BranchCancelled {
			return errors.New("这个分支已经归档，不能再回答")
		}
		if m.SnapshotID == "" || m.Status == session.BranchPreparing || m.Status == session.BranchPendingBoundary {
			return errors.New("分支还没有就绪")
		}
	case commit.KindAgenda, commit.KindIntention:
	default: // 定时任务、Hook 的 Report 不能带问题，没有人接着问
		return errors.New("这种承诺不接受回答")
	}
	return nil
}

func answerText(q, ans string) string {
	return "[主人的回答] 你问：" + q + "\n主人答：" + ans
}

// deliverAnswer 把已经记下的回答送到这件事干活的地方。送不到不影响已经写下的记录。
func (a *App) deliverAnswer(p PendingView, text, by string) {
	body := answerText(p.Question, text)
	switch p.Kind {
	case commit.KindBranch:
		// 和“继续”一样接着跑；分支正在跑就排在下一个安全边界
		_ = a.startBranchRunLocked(p.ID, body, by)
		a.publish(events.Event{T: "branch", SID: p.ID, Kind: "answer"})
	case commit.KindAgenda, commit.KindIntention:
		a.answerToMind(p, body)
	}
}

// answerToMind 在 agent 的心智状态里加一个带 ID 和回答的事件；议程项还没开始的，时间改成现在。
func (a *App) answerToMind(p PendingView, body string) {
	st := a.mind(p.Agent)
	now := a.now()
	ev := autonomy.Event{Kind: autonomy.EventExternal, Source: "answer:" + p.Kind + ":" + p.ID, Text: cutRunes(body, 1000)}
	if p.Kind == commit.KindAgenda {
		ev.AgendaID = p.ID
		if it, ok := st.AgendaByID(p.ID); ok && it.At > now.Unix() {
			_ = st.MoveAgenda(p.ID, now.Unix(), now)
		}
	} else {
		ev.IntentionID = p.ID
	}
	st.AddEvent(ev, now, 0)
	if next := now.Add(time.Second).Unix(); st.NextCheck == 0 || st.NextCheck > next {
		st.SetNextCheck(time.Unix(next, 0))
	}
	a.saveMinds()
	a.publishAutonomy(p.Agent)
}

// ---- 聊天命令（只在管理员窗口）----

func (a *App) cmdPending(c in) {
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能看“等我处理”。"+a.adminHint(c))
		return
	}
	list := a.pendingLocked()
	if len(list) == 0 {
		a.answer(c, "现在没有等你处理的事。")
		return
	}
	var b strings.Builder
	b.WriteString("等你处理的事：")
	for _, p := range list {
		fmt.Fprintf(&b, "\n#%s · %s「%s」· %s", p.Number, commitKindLabel(p.Kind), p.Title, pendingPhaseLabel(p.Phase))
		if p.Why != "" {
			b.WriteString("：" + clip(p.Why, 120))
		}
		if p.Question != "" {
			b.WriteString("\n    问：" + clip(p.Question, 200))
		}
	}
	b.WriteString("\n/seen <编号> 知道了；/answer <编号> <回答> 回答问题。")
	a.answer(c, b.String())
}

func commitKindLabel(k string) string {
	switch k {
	case commit.KindAgenda:
		return "日程"
	case commit.KindIntention:
		return "意图"
	case commit.KindFollowUp:
		return "回访"
	case commit.KindSchedule:
		return "定时任务"
	case commit.KindHook:
		return "Hook"
	case commit.KindBranch:
		return "分支"
	}
	return k
}

func pendingPhaseLabel(p string) string {
	switch p {
	case commit.Ready:
		return "就绪"
	case commit.Running:
		return "进行中"
	case commit.Blocked:
		return "受阻"
	}
	return p
}

// pendingByNumber 按编号找；编号重复（两件事恰好同一个 Mark）就让主人去网页上处理。
func (a *App) pendingByNumber(num string) (PendingView, error) {
	num = strings.TrimPrefix(strings.TrimSpace(num), "#")
	var hit []PendingView
	for _, p := range a.pendingLocked() {
		if p.Number == num {
			hit = append(hit, p)
		}
	}
	switch len(hit) {
	case 0:
		return PendingView{}, errors.New("没有这个编号，/pending 看现在等你处理的事")
	case 1:
		return hit[0], nil
	}
	return PendingView{}, errors.New("这个编号对应了不止一件事，请在网页的“等我处理”里处理")
}

func (a *App) cmdSeen(c in, arg string) {
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能处理。"+a.adminHint(c))
		return
	}
	if strings.TrimSpace(arg) == "" {
		a.answer(c, "用法：/seen <编号>；编号用 /pending 查看。")
		return
	}
	p, err := a.pendingByNumber(arg)
	if err == nil {
		err = a.pendingSeenLocked(p.Kind, p.ID, p.Mark, c.who)
	}
	if err != nil {
		a.answer(c, "没记下："+err.Error())
		return
	}
	a.answer(c, "好，「"+p.Title+"」记为知道了。")
}

func (a *App) cmdAnswer(c in, arg string) {
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能回答。"+a.adminHint(c))
		return
	}
	num, text, _ := strings.Cut(strings.TrimSpace(arg), " ")
	if num == "" || strings.TrimSpace(text) == "" {
		a.answer(c, "用法：/answer <编号> <回答>；编号用 /pending 查看。")
		return
	}
	p, err := a.pendingByNumber(num)
	if err == nil {
		err = a.pendingAnswerLocked(p.Kind, p.ID, p.Mark, text, c.who)
	}
	if err != nil {
		a.answer(c, "没记下："+err.Error())
		return
	}
	a.answer(c, "已经把回答交给「"+p.Title+"」。")
}

// handledByOwner：主人在别处自己动手处理了（分支引入结论、继续或归档；定时任务下一次成功），
// 这件事最近一条要主人知道的汇报算看过。upTo 非零时只处理这个序号及以前的。调用方持有锁。
func (a *App) handledByOwner(kind, id string, upTo uint64) {
	if a.commits == nil {
		return
	}
	n, ok := a.commits.Notice(kind, id)
	if !ok || (upTo != 0 && n.Seq > upTo) {
		return
	}
	mark := fmt.Sprintf("r:%d", n.Seq)
	if a.commits.Seen(kind, id, mark) {
		return
	}
	_, err := a.commits.Append(commit.CommitRec{Type: commit.TypeSeen, Kind: kind, ID: id, Mark: mark, By: "owner"})
	a.commitHealth(err)
	if err == nil {
		a.publishCommitments()
	}
}
