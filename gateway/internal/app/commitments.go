package app

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/commit"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/health"
	"nano-agent/gateway/internal/hooks"
)

// 承诺的汇总（设计 §15.2）：六种承诺各自推出契约和事实，阶段统一从事实推出；
// Report 的汇报和主人点过的“知道了”从 data/runtime/commitments.jsonl 叠上去。

// openCommits 打开承诺记录。打不开就报到健康条目 commitments，Report 返回错误，其他一切照旧。
func (a *App) openCommits() {
	path := filepath.Join(a.store.Dir(), "runtime", "commitments.jsonl")
	s, err := commit.Open(path)
	if err != nil {
		a.commitsErr = err
		a.log.Error("承诺记录打不开，保留现场", "err", err)
		a.hCommit.Set(health.Error, "承诺记录打不开："+err.Error())
		return
	}
	s.Now = a.now
	s.Alive = a.commitAliveFn
	a.commits = s
}

// commitAliveFn 在压缩承诺记录时调用：返回“这件承诺现在还在不在”的判断。调用方持有锁。
func (a *App) commitAliveFn() func(kind, id string) bool {
	alive := map[string]bool{}
	for _, it := range a.commitItems("", a.now()) {
		alive[it.Kind+"\x00"+it.ID] = true
	}
	return func(kind, id string) bool { return alive[kind+"\x00"+id] }
}

// commitFailed 记下承诺记录写不进去（健康条目变 degraded）；写成功时恢复。
func (a *App) commitHealth(err error) {
	if a.hCommit == nil {
		return
	}
	if err != nil {
		a.hCommit.Set(health.Degraded, "承诺记录写不进去："+err.Error())
		return
	}
	a.hCommit.Set(health.OK, "")
}

// commitItems 把现在还存在的承诺都列出来（agent 非空时只看这个 agent 的），已经结束超过 30 天的不列。
// 汇报和“知道了”已经叠上。调用方持有锁。
func (a *App) commitItems(agent string, now time.Time) []commitItem {
	var out []commitItem
	add := func(c commitItem) {
		if agent != "" && c.Agent != agent {
			return
		}
		if c.EndedAt > 0 && now.Sub(time.Unix(c.EndedAt, 0)) > commitKeep {
			return
		}
		out = append(out, c)
	}
	n := now.Unix()
	if a.agentStore != nil && a.agentRoot != "" {
		for _, ag := range a.agentStore.List() {
			if !ag.Life() {
				continue
			}
			st := a.mind(ag.ID)
			for _, it := range st.Agenda {
				add(agendaCommit(it, intentionOf(st.Intentions, it.IntentionID), st.Paused, ag.ID, n))
			}
			for _, in := range st.Intentions {
				if c, ok := intentionCommit(in, st.Agenda, st.Paused, ag.ID, n); ok {
					add(c)
				}
			}
			for sid, f := range st.FollowUps {
				add(followUpCommit(sid, f, ag.ID, n))
			}
		}
	}
	if a.schedules != nil {
		for _, t := range a.schedules.List() {
			add(scheduleCommit(t, n))
		}
	}
	if a.jobEvents != nil {
		last, running := a.hookDeliveries()
		for _, r := range a.jobEvents.Rules("") {
			spec, err := hooks.Parse(r)
			if err != nil {
				continue
			}
			add(hookCommit(r, spec, last[r.ID], running[r.ID], n, a.loc))
		}
	}
	if a.store != nil {
		for _, m := range a.store.Branches() {
			s := a.sessions[m.SID]
			busy := s != nil && (s.run != nil || len(s.queue) > 0)
			add(branchCommit(m, busy, autonomousForker(m.CreatedBy), n))
		}
	}
	if a.tickets != nil {
		for _, t := range a.tickets.List() {
			add(ticketCommit(t, n))
		}
	}
	for i := range out {
		a.overlayReport(&out[i])
	}
	return out
}

func intentionOf(list []autonomy.Intention, id string) *autonomy.Intention {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

// hookDeliveries 是每条规则最近一次投递（按更新时间）和有没有投递在跑。
func (a *App) hookDeliveries() (last map[string]*eventlog.Delivery, running map[string]bool) {
	last, running = map[string]*eventlog.Delivery{}, map[string]bool{}
	after := ""
	for {
		rows, next := a.jobEvents.Deliveries(after, 128)
		for i := range rows {
			d := rows[i]
			if !d.Registered {
				continue
			}
			if p := last[d.HookID]; p == nil || d.UpdatedMS >= p.UpdatedMS {
				last[d.HookID] = &d
			}
			if d.Status == "pending" || d.Status == "running" || d.Status == "retry" {
				running[d.HookID] = true
			}
		}
		if next == "" {
			return
		}
		after = next
	}
}

// overlayReport 把主人看过的标记和 Report 的汇报叠到事实上：
//   - 宿主自己记下的 Blocked、Ready 带着 Mark，主人点过“知道了”的不再算；
//   - 汇报只看最近一条要主人知道的（ready、blocked、failed、not_needed）、还没看过也没回答过的；
//     progress、waiting 只记一笔，顶不掉它；blocked、failed 进 Blocked（带问题的进 Question），ready、not_needed 进 Ready；
//   - 汇报和宿主自己的事实同时有，按阶段表的先后取一个：问题先于受阻，受阻先于就绪，
//     所以汇报的受阻、问题压过宿主的就绪，宿主的受阻压过汇报的就绪；Mark 跟着决定阶段的那条事实，
//     主人点过“知道了”，下一条才显出来。
func (a *App) overlayReport(c *commitItem) {
	f := &c.Facts
	if a.commits == nil {
		return
	}
	if f.Mark != "" && a.commits.Seen(c.Kind, c.ID, f.Mark) {
		f.Blocked, f.Ready, f.Question, f.Mark = "", "", "", ""
	}
	r, ok := a.commits.Notice(c.Kind, c.ID)
	if !ok {
		return
	}
	mark := fmt.Sprintf("r:%d", r.Seq)
	if a.commits.Seen(c.Kind, c.ID, mark) {
		return
	}
	hostBlocked, hostReady, hostMark := f.Blocked, f.Ready, f.Mark
	*f = withoutNotice(*f)
	c.At = r.TS / 1000
	switch {
	case (r.Status == commit.StatusBlocked || r.Status == commit.StatusFailed) && r.Question != "":
		f.Question, f.Mark = r.Question, mark
	case r.Status == commit.StatusBlocked || r.Status == commit.StatusFailed:
		f.Blocked, f.Mark = r.Text, mark
	case hostBlocked != "":
		f.Blocked, f.Mark = hostBlocked, hostMark
	case r.Status == commit.StatusNotNeeded:
		f.Ready, f.Mark = "不用做了："+r.Text, mark
		c.Covered = coveredReady(hostReady, hostMark)
	default: // ready
		f.Ready, f.Mark = r.Text, mark
		c.Covered = coveredReady(hostReady, hostMark)
	}
}

// coveredReady：汇报的就绪盖住了宿主自己的就绪时，两条说的是同一件事，点“知道了”一起记。
// 宿主的受阻不在这里：受阻和就绪是两件不同的事（受阻那一支在上面已经取了宿主的）。
func coveredReady(hostReady, hostMark string) string {
	if hostReady == "" {
		return ""
	}
	return hostMark
}

func withoutNotice(f commit.Facts) commit.Facts {
	f.Blocked, f.Ready, f.Question, f.Mark = "", "", "", ""
	return f
}

// CommitView 是 GET /api/commitments 给网页的一件承诺。
type CommitView struct {
	Kind     string            `json:"kind"`
	ID       string            `json:"id"`
	Agent    string            `json:"agent"`
	Title    string            `json:"title"`
	Contract commit.Contract   `json:"contract"`
	Phase    string            `json:"phase"`
	Why      string            `json:"why,omitempty"`
	Report   *commit.CommitRec `json:"report,omitempty"` // 最近一条汇报
	Mark     string            `json:"mark,omitempty"`
	Covered  string            `json:"covered,omitempty"` // 被汇报的就绪盖住的宿主就绪的 Mark；“知道了”一起记
	Question string            `json:"question,omitempty"`
	At       int64             `json:"at,omitempty"` // 决定阶段的那条事实发生的时间（秒）
	Link     string            `json:"link"`
}

// Commitments 列出承诺：agent、phase 非空时筛选。只给管理员（调用方检查）。
func (a *App) Commitments(agent, phase string) []CommitView {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.commitViewsLocked(agent, phase)
}

func (a *App) commitViewsLocked(agent, phase string) []CommitView {
	now := a.now()
	items := a.commitItems(agent, now)
	out := make([]CommitView, 0, len(items))
	for _, c := range items {
		p, why := commit.Phase(c.Facts)
		if p == commit.Running && why == "" {
			why = c.Note
		}
		if phase != "" && p != phase {
			continue
		}
		v := CommitView{Kind: c.Kind, ID: c.ID, Agent: c.Agent, Title: c.Title, Contract: c.Contract, Phase: p, Why: why, Mark: c.Facts.Mark, Covered: c.Covered, Question: c.Facts.Question, At: c.At, Link: c.Link}
		if a.commits != nil {
			if r, ok := a.commits.Latest(c.Kind, c.ID); ok {
				v.Report = &r
			}
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := phaseRank(out[i].Phase), phaseRank(out[j].Phase)
		if ri != rj {
			return ri < rj
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// phaseRank 要人处理的排前面。
func phaseRank(p string) int {
	for i, x := range []string{commit.Blocked, commit.Ready, commit.Running, commit.Waiting, commit.Paused, commit.Failed, commit.Done, commit.Dropped, commit.Stopped} {
		if x == p {
			return i
		}
	}
	return 99
}

func (a *App) publishCommitments() { a.publish(events.Event{T: "commitments"}) }
