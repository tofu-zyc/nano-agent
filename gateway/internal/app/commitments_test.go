package app

import (
	"fmt"
	"testing"
	"time"

	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/schedule"
	"nano-agent/gateway/internal/session"
)

// 汇总：汇报和“知道了”叠在宿主自己的事实上；只列还存在的、结束没超过 30 天的。

func findCommit(list []CommitView, kind, id string) *CommitView {
	for i := range list {
		if list[i].Kind == kind && list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func TestCommitmentsOverlayReportsAndSeen(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	now := e.a.now().In(e.a.loc)
	e.a.mu.Lock()
	st := e.a.mind("default")
	item, err := st.AddAgenda(autonomy.AgendaItem{Title: "整理文档", Kind: autonomy.KindCommitment, At: now.Add(time.Hour).Unix()}, now)
	e.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	get := func() *CommitView {
		t.Helper()
		v := findCommit(e.a.Commitments("default", ""), commit.KindAgenda, item.ID)
		if v == nil {
			t.Fatal("议程项没列出来")
		}
		return v
	}
	if v := get(); v.Phase != commit.Waiting || v.Report != nil {
		t.Fatalf("还没汇报：%+v", v)
	}
	add := func(r commit.CommitRec) commit.CommitRec {
		t.Helper()
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		out, err := e.a.commits.Append(r)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	rep := func(status, text, question string) commit.CommitRec {
		return add(commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindAgenda, ID: item.ID, Agent: "default", Status: status, Text: text, Question: question})
	}
	// progress、waiting 只记一笔
	rep(commit.StatusProgress, "开始整理了", "")
	if v := get(); v.Phase != commit.Waiting || v.Report == nil || v.Report.Text != "开始整理了" {
		t.Fatalf("进展不改阶段：%+v", v)
	}
	// blocked：带问题的是“等你回答”
	r := rep(commit.StatusBlocked, "有两份重复的", "保留哪一份？")
	v := get()
	if v.Phase != commit.Blocked || v.Why != "等你回答：保留哪一份？" || v.Mark != fmt.Sprintf("r:%d", r.Seq) {
		t.Fatalf("blocked 带问题：%+v", v)
	}
	// 主人回答了：不再算
	add(commit.CommitRec{Type: commit.TypeAnswer, Kind: commit.KindAgenda, ID: item.ID, Mark: v.Mark, By: "web:me", Answer: "留新的"})
	if v := get(); v.Phase != commit.Waiting {
		t.Fatalf("回答过的不再算：%+v", v)
	}
	// blocked 不带问题是受阻；failed 同
	r = rep(commit.StatusFailed, "文档打不开", "")
	if v := get(); v.Phase != commit.Blocked || v.Why != "文档打不开" {
		t.Fatalf("failed：%+v", v)
	}
	add(commit.CommitRec{Type: commit.TypeSeen, Kind: commit.KindAgenda, ID: item.ID, Mark: fmt.Sprintf("r:%d", r.Seq), By: "web:me"})
	if v := get(); v.Phase != commit.Waiting {
		t.Fatalf("点了知道了：%+v", v)
	}
	// ready、not_needed 进就绪
	r = rep(commit.StatusReady, "整理好了，放在 docs/", "")
	if v := get(); v.Phase != commit.Ready || v.Why != "整理好了，放在 docs/" {
		t.Fatalf("ready：%+v", v)
	}
	rep(commit.StatusNotNeeded, "已经有人整理过了", "")
	if v := get(); v.Phase != commit.Ready || v.Why != "不用做了：已经有人整理过了" {
		t.Fatalf("not_needed：%+v", v)
	}
	// 筛选
	if got := e.a.Commitments("default", commit.Ready); findCommit(got, commit.KindAgenda, item.ID) == nil {
		t.Fatal("按阶段筛选")
	}
	if got := e.a.Commitments("default", commit.Blocked); findCommit(got, commit.KindAgenda, item.ID) != nil {
		t.Fatal("别的阶段不该出现")
	}
	if got := e.a.Commitments("bob", ""); len(got) != 0 {
		t.Fatalf("别的 agent 没有：%+v", got)
	}
}

func TestCommitmentsOnlyListLiveAndRecent(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	makeLife(t, e, store, nil)
	now := e.a.now().In(e.a.loc)
	e.a.mu.Lock()
	st := e.a.mind("default")
	old, _ := st.AddAgenda(autonomy.AgendaItem{Title: "很久以前的事", Kind: autonomy.KindPlan, At: now.Unix()}, now)
	recent, _ := st.AddAgenda(autonomy.AgendaItem{Title: "刚做完的事", Kind: autonomy.KindPlan, At: now.Unix()}, now)
	for i := range st.Agenda {
		switch st.Agenda[i].ID {
		case old.ID:
			st.Agenda[i].Status, st.Agenda[i].Result, st.Agenda[i].Updated = autonomy.StatusDone, "做完了", now.AddDate(0, 0, -31).Unix()
		case recent.ID:
			st.Agenda[i].Status, st.Agenda[i].Result, st.Agenda[i].Updated = autonomy.StatusDone, "做完了", now.AddDate(0, 0, -29).Unix()
		}
	}
	e.a.mu.Unlock()
	list := e.a.Commitments("", "")
	if findCommit(list, commit.KindAgenda, old.ID) != nil {
		t.Fatal("结束超过 30 天的不列")
	}
	if v := findCommit(list, commit.KindAgenda, recent.ID); v == nil || v.Phase != commit.Done {
		t.Fatalf("30 天内结束的要列：%+v", v)
	}
	// 压缩时：已经不在的承诺的记录丢掉，还在的留着
	e.a.mu.Lock()
	for _, id := range []string{old.ID, recent.ID, "ag-gone"} {
		e.a.commits.Append(commit.CommitRec{Type: commit.TypeReport, Kind: commit.KindAgenda, ID: id, Agent: "default", Status: commit.StatusProgress, Text: "x"})
	}
	if err := e.a.commits.Compact(); err != nil {
		t.Fatal(err)
	}
	_, goneOK := e.a.commits.Latest(commit.KindAgenda, "ag-gone")
	_, oldOK := e.a.commits.Latest(commit.KindAgenda, old.ID)
	_, recentOK := e.a.commits.Latest(commit.KindAgenda, recent.ID)
	e.a.mu.Unlock()
	if goneOK || oldOK || !recentOK {
		t.Fatalf("压缩只留还在的：gone=%v old=%v recent=%v", goneOK, oldOK, recentOK)
	}
}

// 汇报叠加的先后：问题先于受阻，受阻先于就绪。
func TestOverlayPrecedence(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	add := func(kind, id, status, text, question string) commit.CommitRec {
		t.Helper()
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		r, err := e.a.commits.Append(commit.CommitRec{Type: commit.TypeReport, Kind: kind, ID: id, Status: status, Text: text, Question: question})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	overlay := func(c commitItem) commitItem {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		e.a.overlayReport(&c)
		return c
	}
	// 定时任务上一轮汇报 ready 没人看，这一轮 Last.OK 为假：宿主的受阻压过汇报的就绪，Mark 是 last:
	task := schedule.Task{ID: "t1", Name: "每天看新闻", When: "every 24h", Enabled: true, Next: time.Unix(nowU+3600, 0),
		Last: &schedule.Result{At: time.Unix(nowU-100, 0), OK: false, Summary: "接口超时"}}
	add(commit.KindSchedule, "t1", commit.StatusReady, "上一轮的结果", "")
	c := overlay(scheduleCommit(task, nowU))
	wantPhase(t, "宿主受阻压过汇报就绪", c, commit.Blocked, "上一次失败：接口超时")
	if c.Facts.Mark != "last:999900" {
		t.Fatalf("Mark 跟着决定阶段的那条事实：%q", c.Facts.Mark)
	}
	// 主人点了“知道了”（针对上一次失败）：下一条才显出来——汇报的就绪
	e.a.mu.Lock()
	e.a.commits.Append(commit.CommitRec{Type: commit.TypeSeen, Kind: commit.KindSchedule, ID: "t1", Mark: "last:999900", By: "web:me"})
	e.a.mu.Unlock()
	c = overlay(scheduleCommit(task, nowU))
	wantPhase(t, "知道了之后显出就绪", c, commit.Ready, "上一轮的结果")
	if c.Facts.Mark != "r:1" {
		t.Fatalf("Mark：%q", c.Facts.Mark)
	}
	// 汇报的受阻、问题压过宿主的就绪
	m := session.BranchMeta{SID: "main/b1", ParentSID: "main", OwnerAgent: "default", CreatedBy: "web:me", Intent: "查竞品",
		Status: session.BranchCompleted, ProgressAt: nowU - 10, Progress: "完成：这一轮做了「查竞品」"}
	wantPhase(t, "宿主就绪", overlay(branchCommit(m, false, false, nowU)), commit.Ready, "")
	r := add(commit.KindBranch, "main/b1", commit.StatusBlocked, "有个问题", "")
	b := overlay(branchCommit(m, false, false, nowU))
	wantPhase(t, "汇报受阻压过宿主就绪", b, commit.Blocked, "有个问题")
	if b.Facts.Mark != "r:"+itoaU(r.Seq) {
		t.Fatalf("Mark：%q", b.Facts.Mark)
	}
	r = add(commit.KindBranch, "main/b1", commit.StatusBlocked, "要决定", "用哪个方案？")
	wantPhase(t, "问题压过受阻和就绪", overlay(branchCommit(m, false, false, nowU)), commit.Blocked, "等你回答：用哪个方案？")
	// 之后的 progress 不顶掉还没回答的问题
	add(commit.KindBranch, "main/b1", commit.StatusProgress, "又看了一眼", "")
	add(commit.KindBranch, "main/b1", commit.StatusWaiting, "在等回答", "")
	got := overlay(branchCommit(m, false, false, nowU))
	wantPhase(t, "progress 顶不掉问题", got, commit.Blocked, "等你回答：用哪个方案？")
	if got.Facts.Mark != "r:"+itoaU(r.Seq) {
		t.Fatalf("Mark：%q", got.Facts.Mark)
	}
	// 回答之后：宿主的就绪重新显出来
	e.a.mu.Lock()
	e.a.commits.Append(commit.CommitRec{Type: commit.TypeAnswer, Kind: commit.KindBranch, ID: "main/b1", Mark: got.Facts.Mark, By: "web:me", Answer: "第一个"})
	e.a.mu.Unlock()
	wantPhase(t, "回答之后", overlay(branchCommit(m, false, false, nowU)), commit.Ready, "")
}

func itoaU(n uint64) string { return fmt.Sprintf("%d", n) }
