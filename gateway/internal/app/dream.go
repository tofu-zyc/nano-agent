package app

// 睡觉做梦：整理记忆（docs/DESIGN-v0.6-living-agent.md「做梦」）。
//
// agent 睡着、空闲或者到点时，在 @<id>/dream 会话里跑一轮（每次都是全新的上下文）：用 Diary 读日记
// （这个 agent 的原始聊天记录，压缩不影响），把值得长期记住的用 Knowledge 写进知识库。怎么整理写在
// dream 技能里（skills/dream/SKILL.md，agent 自己的 agents/<id>/skills/dream/ 优先），网关只负责
// 触发、给工具、记进度（agents/<id>/dream-state.json）。最终回复是梦境日志（agents/<id>/dreams/<日期>.md）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

// defaultDreamSkill 是没有 dream 技能文件时用的整理方法，和仓库里的 skills/dream/SKILL.md 正文一样（有测试对比）。
const defaultDreamSkill = `# 整理记忆

你在“睡觉”：把最近的聊天记录（日记）整理进长期记忆。没有人在等你回复。整理完用几句话写下今晚整理了什么，这就是你的梦境日志。

## 工具

- ` + "`" + `Diary list` + "`" + `：哪些聊天有还没整理的日记，各有多少条，对方是谁。
- ` + "`" + `Diary read {source}` + "`" + `：读一段日记（默认从上次整理到的地方往后读 60 条）。每条前面是编号、时间、谁说的。
- ` + "`" + `Diary done {source, upto}` + "`" + `：这段整理完了，标到第几条（下次从这之后读）。
- ` + "`" + `Knowledge` + "`" + `：` + "`" + `search` + "`" + `、` + "`" + `read` + "`" + `、` + "`" + `write` + "`" + `、` + "`" + `edit` + "`" + `、` + "`" + `invalidate` + "`" + `。写之前先 ` + "`" + `search` + "`" + `，已经有的就改，别重复建。

## 步骤

1. ` + "`" + `Diary list` + "`" + `，一个聊天一个聊天来：从头往后读（一次读不完就分几次），读完一段整理一段，整理完马上 ` + "`" + `Diary done` + "`" + `。
2. 从日记里挑出**值得长期记住**的东西。闲聊客套、一次性的操作细节、工具输出不用记。
   - **人**（type 人物，路径放 ` + "`" + `people/` + "`" + ` 下）：新认识的人，对人的新了解。画像写：是谁、怎么称呼、和你的关系、性格和说话方式、喜好和忌讳、最近在忙什么、要紧的事（生日、你答应过他的事）。` + "`" + `ids` + "`" + ` 写上他在各个渠道的身份（日记开头“对方”“参与的人”后面括号里的）。主人的画像固定是 ` + "`" + `people/owner.md` + "`" + `。
   - **事件**（type 事件）：发生了什么，什么时候（` + "`" + `when` + "`" + ` 写日期），和谁有关（` + "`" + `people` + "`" + `）。
   - **事实**、**偏好**：关于人或者世界的稳定事实；谁喜欢什么、讨厌什么、习惯怎么做。
   - **经验**、**流程**：做事时踩过的坑、摸索出来的做法，以后干活用得上的。
   - **知识**：聊天里或者上网看到的、以后可能用得上的知识。
3. 写入的规矩：
   - 一条记一件事。标题短而具体（“张三换工作去了字节”，不要“关于张三”），` + "`" + `summary` + "`" + ` 一句话。
   - 已经有的就更新：` + "`" + `edit` + "`" + ` 改那一段，或者带上 ` + "`" + `path` + "`" + ` 整条覆盖；不要建重复的。
   - 和旧记忆矛盾时，旧的用 ` + "`" + `invalidate` + "`" + ` 标成过时（写清楚原因、新的是哪条），不要删。
   - **分寸**：私聊里知道的事，` + "`" + `visible` + "`" + ` 写对方的身份（只在和他的私聊里用）或者 ` + "`" + `admin` + "`" + `（只在主人的会话里用）；群里公开说的才写 ` + "`" + `all` + "`" + `。
   - 拿不准的不记，或者在正文里写明“好像”“他说”。
4. 回顾今天（**工具型 agent 跳过这一步**）：
   - 对人和事有了什么**看法**（type 看法，` + "`" + `confidence` + "`" + ` 写 0 到 1 的把握）。已有的看法，新的事支持它就调高 ` + "`" + `confidence` + "`" + `，反驳它就调低，或者标过时再写新的。
   - 更新 ` + "`" + `self.md` + "`" + `（你的自我认知）：你是谁、你的兴趣、最近在想什么、对生活的感受、和身边的人的关系。不要写成流水账，保持在一页以内。
5. 最后用三五句话写下今晚整理了什么：记住了谁、什么事，改了哪些看法。像写日记一样，用你自己的口吻。

整理不完没关系，` + "`" + `Diary done` + "`" + ` 标到哪，下次就从哪接着来。`

const (
	dreamLookback    = 14 * 24 * time.Hour // 第一次整理时只看最近两周
	dreamReadDefault = 60
	dreamReadMax     = 200
	dreamReadChars   = 12000
	dreamMinIdle     = 20 // 空闲时整理：至少有这么多条没整理的日记
	dreamPendingTTL  = 5 * time.Minute
)

// dreamState 是 agents/<id>/dream-state.json：整理到哪了、今天整理了几次。
type dreamState struct {
	Done     map[string]int `json:"done"` // 日记（会话 ID，或 archive:<会话>/<文件>）下一条要读的位置
	Day      string         `json:"day"`
	Count    int            `json:"count"` // 今天自动整理了几次
	Last     int64          `json:"last"`  // 上次开始整理的时间
	LastEnd  int64          `json:"last_end"`
	LastNote string         `json:"last_note"` // 上次梦境日志的开头

	pending   int
	pendingAt time.Time
}

func (a *App) dreamPath(agentID string) string {
	return filepath.Join(a.agentRoot, agentID, "dream-state.json")
}

// dreamOf 是 agent 的整理进度（第一次用时从文件读）。调用方持有锁。
func (a *App) dreamOf(agentID string) *dreamState {
	if a.dreams == nil {
		a.dreams = map[string]*dreamState{}
	}
	if st := a.dreams[agentID]; st != nil {
		return st
	}
	st := &dreamState{}
	if b, err := os.ReadFile(a.dreamPath(agentID)); err == nil {
		_ = json.Unmarshal(b, st)
	}
	if st.Done == nil {
		st.Done = map[string]int{}
	}
	a.dreams[agentID] = st
	return st
}

func (a *App) saveDream(agentID string) {
	st := a.dreamOf(agentID)
	b, _ := json.MarshalIndent(st, "", "  ")
	p := a.dreamPath(agentID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
		if err = os.WriteFile(p+".tmp", b, 0o600); err == nil {
			err = os.Rename(p+".tmp", p)
		}
		if err != nil {
			a.log.Warn("保存整理记忆的进度失败", "agent", agentID, "err", err)
		}
	}
}

// diarySource 是一段日记：一个会话的原始记录，或者一份归档。
type diarySource struct {
	Key     string
	SID     string
	Title   string
	Partner string
	Label   string // 别人说话时前面写什么：主人 / 对方 / 群里
	Msgs    []session.Msg
	Done    int
}

func (d diarySource) left() int { return max(0, len(d.Msgs)-d.Done) }

// firstSince 是第一条不早于 t 的消息的位置
func firstSince(msgs []session.Msg, t time.Time) int {
	for i, m := range msgs {
		if m.TS == 0 || m.TS >= t.Unix() {
			return i
		}
	}
	return len(msgs)
}

// diarySources 是 agent 所有的日记（只算有没整理的）。第一次见到的会话只从两周前开始。调用方持有锁。
func (a *App) diarySources(agentID string) []diarySource {
	st := a.dreamOf(agentID)
	now := a.now()
	dirty := false
	var out []diarySource
	for _, sid := range a.sessionIDs() {
		// 按当前执行身份算：会话中途换过 agent 的，只整理现在属于它的部分
		if a.execAgentID(sid) != agentID || isDreamSID(sid) || isCronSID(sid) || strings.HasPrefix(sid, "room:") {
			continue
		}
		title, partner, label := a.diaryPartner(agentID, sid)
		msgs := a.store.Archive(sid)
		done, seen := st.Done[sid]
		if !seen {
			done = firstSince(msgs, now.Add(-dreamLookback))
			st.Done[sid], dirty = done, true
		}
		archives := a.store.Archives(sid)
		if done > len(msgs) {
			// 历史被 /reset 挪进了归档：整理到的位置记到最新那份归档上，会话从头开始
			if len(archives) > 0 {
				if key := "archive:" + sid + "/" + archives[0].ID; st.Done[key] == 0 {
					st.Done[key] = done
				}
			}
			done, st.Done[sid], dirty = 0, 0, true
		}
		if len(msgs) > done {
			out = append(out, diarySource{Key: sid, SID: sid, Title: title, Partner: partner, Label: label, Msgs: msgs, Done: done})
		}
		for _, ar := range archives {
			key := "archive:" + sid + "/" + ar.ID
			d, ok := st.Done[key]
			if !ok && time.Unix(ar.At, 0).Before(now.Add(-dreamLookback)) {
				continue
			}
			if d >= ar.Messages {
				continue
			}
			msgs := a.store.ReadArchive(sid, ar.ID)
			if len(msgs) > d {
				out = append(out, diarySource{Key: key, SID: sid, Title: title + "（" + time.Unix(ar.At, 0).In(a.loc).Format("01-02 15:04") + " 清空前的归档）",
					Partner: partner, Label: label, Msgs: msgs, Done: d})
			}
		}
	}
	if dirty {
		a.saveDream(agentID)
	}
	return out
}

// diaryPartner 是这段日记的名字、和谁聊的、别人说话时前面写什么。调用方持有锁。
func (a *App) diaryPartner(agentID, sid string) (title, partner, label string) {
	title = a.sessionInfo(sid).Title
	switch {
	case a.isOwnerSID(agentID, sid):
		return title, "主人（管理员；终端、网页和主人的各个聊天窗口都接在这里）", "主人"
	case isLifeSID(sid):
		return "你自己的内心活动", "没有别人，这是你一个人的时候想的、做的、上网看到的", "（醒来）"
	case isRoomMemberSID(sid):
		if room, ok := a.roomOfSession(sid); ok {
			return "网关里的群「" + room.Name + "」", "群成员是网关里的几个 agent 和主人", "群里"
		}
	}
	if a.isGroupSession(sid) {
		var names []string
		for name, who := range a.store.Settings(sid).Members {
			names = append(names, name+"（"+who+"）")
		}
		p := "群聊"
		if len(names) > 0 {
			p += "，参与的人：" + strings.Join(names, "、")
		}
		return title, p, "群里"
	}
	var who []string
	for _, ep := range a.endpointsOf(sid) {
		if ep.Channel == "web" {
			continue
		}
		name := ep.Name
		if name == "" {
			name = "对方"
		}
		who = append(who, name+"（"+ep.Who+"）")
	}
	if len(who) == 0 {
		return title, "私聊", "对方"
	}
	return title, "私聊：" + strings.Join(who, "、"), "对方"
}

// diaryText 去掉网关加在消息前面的东西（环境说明、记忆说明、想起来的）
func diaryText(s string) string {
	for _, pair := range [][2]string{{memNoteOpen, memNoteClose}, {recallOpen, recallClose}, {lifeRecentOp, lifeRecentEd}, {"[经历]", "[/经历]"}} {
		for {
			i := strings.Index(s, pair[0])
			if i < 0 {
				break
			}
			j := strings.Index(s[i:], pair[1])
			if j < 0 {
				break
			}
			s = s[:i] + strings.TrimLeft(s[i+j+len(pair[1]):], "\n")
		}
	}
	for strings.HasPrefix(s, "[系统] ") {
		_, rest, ok := strings.Cut(s, "\n")
		if !ok {
			return ""
		}
		s = rest
	}
	return strings.TrimSpace(s)
}

func (a *App) diaryLine(i int, m session.Msg, label string) string {
	ts := ""
	if m.TS > 0 {
		ts = " [" + time.Unix(m.TS, 0).In(a.loc).Format("01-02 15:04") + "]"
	}
	switch m.Role {
	case "user":
		text := diaryText(m.Content)
		if text == "" {
			return ""
		}
		return fmt.Sprintf("#%d%s %s：%s", i, ts, label, cutRunes(text, 1500))
	case "assistant":
		text := strings.TrimSpace(m.Content)
		if len(m.ToolCalls) > 0 {
			var names []string
			for _, c := range m.ToolCalls {
				names = append(names, c.Function.Name)
			}
			text = strings.TrimSpace(text + "（用了工具：" + strings.Join(names, "、") + "）")
		}
		return fmt.Sprintf("#%d%s 我：%s", i, ts, cutRunes(text, 1500))
	case "tool":
		return fmt.Sprintf("#%d （工具结果，%d 字）%s", i, len([]rune(m.Content)), cutRunes(strings.ReplaceAll(m.Content, "\n", " "), 120))
	}
	return ""
}

// ---- Diary 工具（只在整理记忆时有）----

const diarySchema = `{"type":"object","properties":{
"action":{"type":"string","enum":["list","read","done"],"description":"list 看有哪些没整理的日记；read 读一段；done 标记整理到哪了"},
"source":{"type":"string","description":"read/done：哪段日记（list 里每段开头的名字）"},
"from":{"type":"integer","description":"read：从第几条开始，默认从上次整理到的地方"},
"limit":{"type":"integer","description":"read：读多少条，默认 60，最多 200"},
"upto":{"type":"integer","description":"done：整理完了第几条（含这条），下次从它后面开始读"}
},"required":["action"]}`

func diaryTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "Diary", Description: "日记：你最近的聊天记录（原始的，没被压缩过）。整理记忆时一段段读，读完一段、记完了就用 done 标记，下次从后面接着读。",
		InputSchema: json.RawMessage(diarySchema)}
}

// diaryCall 执行 Diary。调用方持有锁。
func (a *App) diaryCall(sid string, raw []byte) (string, error) {
	if !isDreamSID(sid) {
		return "", errors.New("日记只在整理记忆的时候能读")
	}
	var x struct {
		Action string `json:"action"`
		Source string `json:"source"`
		From   *int   `json:"from"`
		Limit  int    `json:"limit"`
		Upto   *int   `json:"upto"`
	}
	if err := json.Unmarshal(raw, &x); err != nil {
		return "", errors.New("参数格式不对")
	}
	agentID := agents.SessionAgent(sid)
	sources := a.diarySources(agentID)
	if x.Action == "list" {
		if len(sources) == 0 {
			return "没有没整理的日记了。", nil
		}
		var b strings.Builder
		total := 0
		for _, d := range sources {
			total += d.left()
			fmt.Fprintf(&b, "- %s：%s；没整理的 %d 条（#%d–#%d", d.Key, d.Title, d.left(), d.Done, len(d.Msgs)-1)
			if from, to := d.Msgs[d.Done].TS, d.Msgs[len(d.Msgs)-1].TS; from > 0 && to > 0 {
				fmt.Fprintf(&b, "，%s 到 %s", time.Unix(from, 0).In(a.loc).Format("01-02 15:04"), time.Unix(to, 0).In(a.loc).Format("01-02 15:04"))
			}
			fmt.Fprintf(&b, "）；%s\n", d.Partner)
		}
		fmt.Fprintf(&b, "一共 %d 段、%d 条。用 Diary read {source} 一段段读。", len(sources), total)
		return b.String(), nil
	}
	var d *diarySource
	for i := range sources {
		if sources[i].Key == strings.TrimSpace(x.Source) {
			d = &sources[i]
		}
	}
	if d == nil {
		return "", fmt.Errorf("没有这段日记：%q（先 Diary list，source 写列表里每段开头的名字；整理完的不再列出）", x.Source)
	}
	st := a.dreamOf(agentID)
	switch x.Action {
	case "read":
		from := d.Done
		if x.From != nil {
			from = min(max(0, *x.From), len(d.Msgs))
		}
		limit := x.Limit
		if limit <= 0 {
			limit = dreamReadDefault
		}
		limit = min(limit, dreamReadMax)
		var b strings.Builder
		fmt.Fprintf(&b, "【%s】%s\n", d.Title, d.Partner)
		last := from - 1
		foreignNotice := false
		for i := from; i < len(d.Msgs) && i < from+limit; i++ {
			line := a.diaryLine(i, d.Msgs[i], d.Label)
			// 会话中途换过 agent：别的 agent 的回复不能当成自己的亲历（消息自己记着来源）
			if m := d.Msgs[i]; line != "" && (m.Role == "assistant" || m.Role == "tool") && m.Agent != "" && m.Agent != agentID {
				if foreignNotice {
					line = ""
				} else {
					line = "（下面这段之前由别的 agent 负责，它说的话不算你的亲历，已略过）"
					foreignNotice = true
				}
			}
			if line != "" {
				if b.Len()+len(line) > dreamReadChars && i > from {
					break
				}
				b.WriteString(line + "\n")
			}
			last = i
		}
		if last < from {
			return "这段日记读完了。", nil
		}
		if last+1 < len(d.Msgs) {
			fmt.Fprintf(&b, "（读到 #%d，后面还有 %d 条。这一段记完后 Diary done {source:%q, upto:%d}，再接着读）", last, len(d.Msgs)-last-1, d.Key, last)
		} else {
			fmt.Fprintf(&b, "（读到 #%d，这段日记读完了。记完后 Diary done {source:%q, upto:%d}）", last, d.Key, last)
		}
		return b.String(), nil
	case "done":
		if x.Upto == nil {
			return "", errors.New("要写 upto：整理完了第几条")
		}
		next := min(max(0, *x.Upto+1), len(d.Msgs))
		st.Done[d.Key] = next
		st.pendingAt = time.Time{}
		a.saveDream(agentID)
		if next >= len(d.Msgs) {
			return "好，这段日记整理完了。", nil
		}
		return fmt.Sprintf("好，记到 #%d，这段还剩 %d 条。", *x.Upto, len(d.Msgs)-next), nil
	}
	return "", fmt.Errorf("不认识的动作 %q", x.Action)
}

// ---- 什么时候整理 ----

func (a *App) dreamWhen(ag agents.Agent) string {
	def := "idle"
	if ag.Life() {
		def = "sleep"
	}
	return pickString(ag.DreamWhen, def)
}

func (a *App) dreamMax(ag agents.Agent) int { return pickPositive(ag.DreamMax, 1) }

func (a *App) dreamIdle(ag agents.Agent) time.Duration {
	if d := parseLimitDuration(pickString(ag.DreamIdle, "2h")); d > 0 {
		return d
	}
	return 2 * time.Hour
}

func (a *App) dreamAt(ag agents.Agent) int {
	if m, err := parseClock(pickString(ag.DreamAt, "03:00")); err == nil {
		return m
	}
	return 3 * 60
}

// awakeOf 是 agent 醒着的时间（拟人型的作息；整理记忆选“睡着时”也按它）。
func (a *App) awakeOf(ag agents.Agent) dayWindow {
	if w, err := parseWindow(pickString(ag.LifeAwake, defaultAwake)); err == nil {
		return w
	}
	w, _ := parseWindow(defaultAwake)
	return w
}

const defaultAwake = "08:00-23:30"

// diaryPending 是 agent 还有多少条没整理的日记（几分钟算一次，读历史文件不便宜）。调用方持有锁。
func (a *App) diaryPending(agentID string) int {
	st := a.dreamOf(agentID)
	if !st.pendingAt.IsZero() && a.now().Sub(st.pendingAt) < dreamPendingTTL {
		return st.pending
	}
	n := 0
	for _, d := range a.diarySources(agentID) {
		n += d.left()
	}
	st.pending, st.pendingAt = n, a.now()
	return n
}

// agentLastActivity 是 agent 最近一次有人找它、它说话的时间（不算内心活动、整理记忆、定时任务）。
func (a *App) agentLastActivity(agentID string) time.Time {
	last := a.bootAt
	for sid, s := range a.sessions {
		if s == nil || a.execAgentID(sid) != agentID || isInnerSID(sid) || isCronSID(sid) {
			continue
		}
		if s.run != nil || len(s.queue) > 0 {
			return a.now()
		}
		if s.lastActivity.After(last) {
			last = s.lastActivity
		}
	}
	return last
}

// dreamTick 看看哪些 agent 该整理记忆了。调用方不持有锁。
func (a *App) dreamTick() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentStore == nil || a.agentRoot == "" {
		return
	}
	now := a.now().In(a.loc)
	if a.bootAt.IsZero() {
		a.bootAt = now
	}
	for _, ag := range a.agentStore.List() {
		when := a.dreamWhen(ag)
		if !a.memoryOn(ag) || when == "off" {
			continue
		}
		st := a.dreamOf(ag.ID)
		if day := now.Format("2006-01-02"); st.Day != day {
			st.Day, st.Count = day, 0
		}
		if st.Count >= a.dreamMax(ag) {
			continue
		}
		if s := a.sessions[dreamSID(ag.ID)]; s != nil && (s.run != nil || len(s.queue) > 0) {
			continue
		}
		last := time.Unix(st.Last, 0)
		need := 1
		switch when {
		case "idle":
			idle := a.dreamIdle(ag)
			if now.Sub(a.agentLastActivity(ag.ID)) < idle || now.Sub(last) < idle {
				continue
			}
			need = dreamMinIdle
		case "nightly":
			if t := at(now, a.dreamAt(ag)); now.Before(t) || !last.Before(t) {
				continue
			}
		case "sleep":
			w := a.awakeOf(ag)
			if w.always() || w.contains(now) || !last.Before(w.lastEnd(now)) {
				continue
			}
		default:
			continue
		}
		if a.diaryPending(ag.ID) < need {
			continue
		}
		if err := a.startDream(ag.ID, false); err != nil {
			a.log.Warn("没能开始整理记忆", "agent", ag.ID, "err", err)
		}
	}
}

// startDream 开始一次整理记忆。manual 是主人叫的（/sleep、网页按钮），不算每天的次数。调用方持有锁。
func (a *App) startDream(agentID string, manual bool) error {
	ag, ok := a.agentByID(agentID)
	if !ok {
		return errors.New("没有这个 agent")
	}
	if !a.memoryOn(ag) {
		return errors.New("这个 agent 关掉了长期记忆")
	}
	s := a.sessOf(dreamSID(agentID))
	if s.run != nil || len(s.queue) > 0 || s.compacting {
		return errors.New("正在整理记忆")
	}
	if a.models != nil && len(a.models.Models()) == 0 {
		return errors.New("还没有配置模型")
	}
	if err := a.store.Reset(s.sid); err != nil {
		return fmt.Errorf("清空上次整理的上下文：%w", err)
	}
	st := a.store.Settings(s.sid)
	st.Title = "整理记忆 · " + ag.Name
	st.Workspaces, st.WorkspacesSet = nil, true
	st.Model = pickString(ag.DreamModel, "")
	if err := a.store.SaveSettings(s.sid, st); err != nil {
		return err
	}
	if a.route(a.workspaceOf(s.sid)).kernel == "" {
		return errors.New("没有在线的内核")
	}
	ds := a.dreamOf(agentID)
	now := a.now().In(a.loc)
	prev := "这是第一次整理，只看最近两周的日记。"
	if ds.LastEnd > 0 {
		prev = "上次整理：" + time.Unix(ds.LastEnd, 0).In(a.loc).Format("01-02 15:04") + "。"
	}
	kind := "你是工具型 agent：只整理记忆（人、事、偏好、经验、知识），跳过“回顾今天”那一步，不写看法和 self.md。"
	if ag.Life() {
		kind = "你是拟人型 agent：整理完记忆还要回顾今天，形成看法、更新 self.md。"
	}
	pending, sources := 0, a.diarySources(agentID)
	for _, d := range sources {
		pending += d.left()
	}
	if pending == 0 {
		return errors.New("上次整理之后还没有新的聊天，没什么要整理的")
	}
	text := fmt.Sprintf("[系统] 现在是 %s %s %s。你要睡了：开始整理记忆。\n%s\n%s\n没整理的日记：%d 段、共 %d 条。先 Diary list。",
		now.Format("2006-01-02"), weekdayCN(now), now.Format("15:04"), kind, prev, len(sources), pending)
	if !manual {
		ds.Count++
	}
	ds.Last = now.Unix()
	ds.pendingAt = time.Time{}
	a.saveDream(agentID)
	s.queue = append(s.queue, queued{ep: endpoint{ch: "dream", chat: agentID}, who: "dream:" + agentID, inner: true,
		m: channel.Message{Text: text}})
	a.startRun(s)
	if s.run == nil {
		s.queue = nil
		return errors.New("没能开始：没有可用的模型或在线的内核")
	}
	a.log.Info("开始整理记忆", "agent", agentID, "manual", manual, "pending", pending)
	a.publish(events.Event{T: "dream", Name: agentID, Kind: "start"})
	return nil
}

// dreamDone 是一次整理结束：最终回复写进梦境日志。调用方持有锁。
func (a *App) dreamDone(s *sess, status, body string) {
	agentID := agents.SessionAgent(s.sid)
	ds := a.dreamOf(agentID)
	now := a.now().In(a.loc)
	start := time.Unix(ds.Last, 0).In(a.loc)
	if ds.Last == 0 {
		start = now
	}
	note := strings.TrimSpace(body)
	if status != proto.StatusOK {
		note = strings.TrimSpace("（这次没整理完：" + status + "）\n" + note)
	}
	if note == "" {
		note = "（没有写梦境日志）"
	}
	p := filepath.Join(a.agentRoot, agentID, "dreams", start.Format("2006-01-02")+".md")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "## %s – %s\n\n%s\n\n", start.Format("15:04"), now.Format("15:04"), note)
			f.Close()
		}
	}
	ds.LastEnd = now.Unix()
	ds.LastNote = cutRunes(note, 300)
	ds.pendingAt = time.Time{}
	a.saveDream(agentID)
	a.log.Info("整理记忆结束", "agent", agentID, "status", status)
	a.publish(events.Event{T: "dream", Name: agentID, Kind: "end", Text: ds.LastNote})
	a.publishMemory(agentID)
	if ag, ok := a.agentByID(agentID); ok && ag.Life() {
		a.lifeHeard(agentID, "昨晚整理记忆："+note)
		// 反思写进心智状态：经验要能影响之后的目标和计划，而不是只写一篇日记
		if status == proto.StatusOK {
			a.mindDream(agentID, note)
		}
	}
}

// DreamLogs 是梦境日志的日期（新的在前）和某天的内容（网页用）。
func (a *App) DreamLog(agentID, day string) ([]string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.agentByID(agentID); !ok || a.agentRoot == "" {
		return nil, "", errors.New("没有这个 agent")
	}
	dir := filepath.Join(a.agentRoot, agentID, "dreams")
	ents, _ := os.ReadDir(dir)
	days := []string{}
	for i := len(ents) - 1; i >= 0; i-- {
		if name := ents[i].Name(); strings.HasSuffix(name, ".md") {
			days = append(days, strings.TrimSuffix(name, ".md"))
		}
	}
	if day == "" && len(days) > 0 {
		day = days[0]
	}
	text := ""
	if day != "" {
		if filepath.Base(day) != day {
			return nil, "", errors.New("日期不对")
		}
		b, _ := os.ReadFile(filepath.Join(dir, day+".md"))
		text = string(b)
	}
	return days, text, nil
}

// Dream 是网页上的「现在整理」。
func (a *App) Dream(agentID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.startDream(agentID, true)
}

// cmdSleep 是 /sleep：现在整理一次记忆。
func (a *App) cmdSleep(c in) {
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能让 agent 去整理记忆")
		return
	}
	ag := a.agentOf(c.s.sid)
	if err := a.startDream(ag.ID, true); err != nil {
		a.answer(c, "没能开始整理记忆："+err.Error())
		return
	}
	a.answer(c, ag.Name+" 去整理记忆了（在网页的记忆节点里能看到梦境日志）。")
}

func weekdayCN(t time.Time) string {
	return [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[t.Weekday()]
}
