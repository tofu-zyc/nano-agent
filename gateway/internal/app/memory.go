package app

// 长期记忆：Knowledge 与上下文注入；见 docs/DESIGN-agent-runtime.md。
//
// 每个 agent 一个知识库 agents/<id>/knowledge（人物画像在 people/ 下），所有 agent 共用的在
// ~/.nano-agent/knowledge，自我认知是 agents/<id>/self.md。系统提示不变（缓存优先）：
// 记忆说明和“想起来的”像环境说明一样放在这一轮的用户消息前面，记忆变了才再放一次。

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/knowledge"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

const (
	memNoteBudget  = 2000 // 记忆说明最多大约这么多 token
	recallMax      = 3    // 每轮最多想起几条
	maxSelfChars   = 3000
	maxCardChars   = 1500
	sharedPrefix   = "shared/"
	selfFile       = "self.md"
	ownerCardPath  = "people/owner.md"
	memNoteOpen    = "[记忆]"
	memNoteClose   = "[/记忆]"
	recallOpen     = "[想起来的]"
	recallClose    = "[/想起来的]"
	knowledgeLabel = "Knowledge"
)

// lifeSID / dreamSID 是 agent 的内心活动、整理记忆用的会话。默认 agent 也带前缀（和房间成员会话一样），
// 不会和用户起名的会话撞在一起。
func lifeSID(agentID string) string  { return "@" + agentID + "/life" }
func dreamSID(agentID string) string { return "@" + agentID + "/dream" }

func isLifeSID(sid string) bool {
	return strings.HasPrefix(sid, "@") && strings.HasSuffix(sid, "/life")
}
func isDreamSID(sid string) bool {
	return strings.HasPrefix(sid, "@") && strings.HasSuffix(sid, "/dream")
}

// isInnerSID 是 agent 自己的会话（内心活动、整理记忆）：不当普通会话显示，不算“有人找它”。
func isInnerSID(sid string) bool { return isLifeSID(sid) || isDreamSID(sid) || isHookSID(sid) }

// memStore 是 agent 的知识库。调用方持有锁。
func (a *App) memStore(agentID string) *knowledge.Store {
	if a.mems == nil {
		a.mems = map[string]*knowledge.Store{}
	}
	st := a.mems[agentID]
	if st == nil {
		st = knowledge.Open(filepath.Join(a.agentRoot, agentID, "knowledge"))
		a.mems[agentID] = st
	}
	return st
}

// sharedMem 是所有 agent 共用的知识库（~/.nano-agent/knowledge）。调用方持有锁。
func (a *App) sharedMem() *knowledge.Store {
	if a.mems == nil {
		a.mems = map[string]*knowledge.Store{}
	}
	st := a.mems[""]
	if st == nil {
		st = knowledge.Open(filepath.Join(filepath.Dir(a.agentRoot), "knowledge"))
		a.mems[""] = st
	}
	return st
}

// memoryOn 是这个 agent 开着长期记忆（默认开；没有 agent 列表的老模式没有记忆）。调用方持有锁。
func (a *App) memoryOn(ag agents.Agent) bool {
	return a.agentRoot != "" && a.agentStore != nil && pickString(ag.Memory, "on") != "off"
}

func (a *App) selfPath(agentID string) string { return filepath.Join(a.agentRoot, agentID, selfFile) }

// selfText 是 agent 的自我认知（self.md），没有就是空。
func (a *App) selfText(agentID string) string {
	b, err := os.ReadFile(a.selfPath(agentID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// viewerOf 是会话读记忆的场合：群里只看公开的；管理员（和 agent 自己的内心活动、整理记忆、定时任务）都能看；
// 私聊里看公开的和对方的。调用方持有锁。
func (a *App) viewerOf(sid, who string) knowledge.Viewer {
	if isInnerSID(sid) || isCronSID(sid) {
		return knowledge.Viewer{Admin: true}
	}
	if a.isGroupSession(sid) || isRoomMemberSID(sid) || strings.HasPrefix(sid, "room:") {
		return knowledge.Viewer{Group: true}
	}
	if a.isAdmin(who) || who == "" {
		return knowledge.Viewer{Admin: true}
	}
	return knowledge.Viewer{Who: who}
}

// partnerCard 是这个会话里“对方”的人物画像：管理员是 people/owner.md（没有就按身份找），私聊按身份找，群里没有。
func (a *App) partnerCard(agentID string, v knowledge.Viewer, who string) (knowledge.Entry, bool) {
	if v.Group {
		return knowledge.Entry{}, false
	}
	ms := a.memStore(agentID)
	if v.Admin {
		if e, ok := ms.Get(ownerCardPath); ok && e.Invalid == "" {
			return e, true
		}
	}
	if who == "" {
		return knowledge.Entry{}, false
	}
	return ms.Person(who, "")
}

// memoryNote 是这一轮的记忆说明：自我认知、对方的画像、知识库索引。没有记忆时是空。调用方持有锁。
func (a *App) memoryNote(sid, who string) string {
	ag := a.agentOf(sid)
	if !a.memoryOn(ag) {
		return ""
	}
	v := a.viewerOf(sid, who)
	keep := func(e knowledge.Entry) bool { return e.VisibleTo(v) }
	var b strings.Builder
	if self := a.selfText(ag.ID); self != "" {
		fmt.Fprintf(&b, "## 你自己（%s）\n%s\n\n", selfFile, cutRunes(self, maxSelfChars))
	}
	cardPath := ""
	if card, ok := a.partnerCard(ag.ID, v, who); ok {
		cardPath = card.Path
		fmt.Fprintf(&b, "## 对方：%s（%s）\n%s\n\n", card.Title, card.Path, cutRunes(card.Body, maxCardChars))
	}
	ms, shared := a.memStore(ag.ID), a.sharedMem()
	index := ms.IndexText(func(e knowledge.Entry) bool { return keep(e) && e.Path != cardPath })
	sharedIndex := prefixPaths(shared.IndexText(keep))
	if session.TextTokens(b.String()+index+sharedIndex) > memNoteBudget {
		index, sharedIndex = ms.IndexTitles(func(e knowledge.Entry) bool { return keep(e) && e.Path != cardPath }), prefixPaths(shared.IndexTitles(keep))
	}
	if over := session.TextTokens(b.String()+index+sharedIndex) - memNoteBudget; over > 0 {
		index = cutLines(index, memNoteBudget-session.TextTokens(b.String())-200)
		sharedIndex = cutLines(sharedIndex, 200)
	}
	if index != "" {
		b.WriteString("## 记忆索引\n" + index + "\n")
	}
	if sharedIndex != "" {
		b.WriteString("## 共用的知识\n" + sharedIndex + "\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return memNoteOpen + "\n这是你的长期记忆（自动整理的，记的是以前的事；要细节用 Knowledge 工具 read 或 search，有值得长期记住的新东西用 write）。\n\n" +
		strings.TrimSpace(b.String()) + "\n" + memNoteClose
}

// prefixPaths 把共用知识索引里的路径前面加上 shared/
func prefixPaths(index string) string {
	if index == "" {
		return ""
	}
	lines := strings.Split(index, "\n")
	for i, l := range lines {
		if j := strings.LastIndex(l, "（"); j > 0 && strings.HasSuffix(l, "）") {
			lines[i] = l[:j] + "（" + sharedPrefix + l[j+len("（"):]
		}
	}
	return strings.Join(lines, "\n")
}

// cutLines 按行截到大约 budget 个 token
func cutLines(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	var b strings.Builder
	for _, l := range strings.Split(text, "\n") {
		if session.TextTokens(b.String()+l) > budget {
			b.WriteString("……（还有更多，用 Knowledge search 找）\n")
			break
		}
		b.WriteString(l + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "……（没放完，Knowledge read 看全文）"
}

// recallNote 是这一轮“想起来的”：提到名字的记忆，和关键词搜到的、命中够多的几条；这个会话里提过的不再提。
// 返回说明和这次提到的路径。调用方持有锁。
func (a *App) recallNote(sid, who, text string, skip []string) (string, []string) {
	ag := a.agentOf(sid)
	text = strings.TrimSpace(text)
	if !a.memoryOn(ag) || len([]rune(text)) < 2 {
		return "", nil
	}
	v := a.viewerOf(sid, who)
	if card, ok := a.partnerCard(ag.ID, v, who); ok {
		skip = append(slices.Clone(skip), card.Path) // 对方的画像已经在记忆说明里了
	}
	keep := func(e knowledge.Entry) bool { return e.VisibleTo(v) && !slices.Contains(skip, e.Path) }
	ms := a.memStore(ag.ID)
	var picks []knowledge.Hit
	has := func(p string) bool {
		return slices.ContainsFunc(picks, func(h knowledge.Hit) bool { return h.Path == p })
	}
	for _, e := range ms.Mentioned(text, keep) {
		if len(picks) >= 2 {
			break
		}
		picks = append(picks, knowledge.Hit{Entry: e, Snippet: e.Line()})
	}
	for _, h := range ms.Search(text, knowledge.SearchOptions{Keep: keep, Limit: 6}) {
		if len(picks) >= recallMax {
			break
		}
		// 一句话里大半是“我们”“聊聊”这种词：命中两个词就算有关，话很长时要三个
		need := 2
		if h.Terms > 16 {
			need = 3
		}
		if h.Matched < need || has(h.Path) {
			continue
		}
		picks = append(picks, h)
	}
	if len(picks) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString(recallOpen + "\n和这条消息可能有关的记忆（不一定相关，用不上就当没看见）：")
	var paths []string
	for _, h := range picks {
		fmt.Fprintf(&b, "\n- [%s] %s（%s）：%s", h.Type, h.Title, h.Path, cutRunes(h.Snippet, 300))
		paths = append(paths, h.Path)
	}
	b.WriteString("\n" + recallClose)
	return b.String(), paths
}

// memoryNotes 是这一轮要放在消息前面的记忆部分：记忆说明（变了才放）和想起来的。st 会被更新，dirty 表示要保存。
// 调用方持有锁。
func (a *App) memoryNotes(sid, who, text string, st *session.Settings, dirty *bool) string {
	var parts []string
	if note := a.memoryNote(sid, who); note != "" {
		if h := fmt.Sprintf("%x", sha256.Sum256([]byte(note))); h != st.MemNote {
			parts = append(parts, note)
			st.MemNote, *dirty = h, true
		}
	}
	if note := a.experienceNote(sid, who); note != "" {
		if h := fmt.Sprintf("%x", sha256.Sum256([]byte(note))); h != st.LifeNote {
			parts = append(parts, note)
			st.LifeNote, *dirty = h, true
		}
	}
	if note, paths := a.recallNote(sid, who, text, st.Recalled); note != "" {
		parts = append(parts, note)
		st.Recalled = append(st.Recalled, paths...)
		if len(st.Recalled) > 200 {
			st.Recalled = st.Recalled[len(st.Recalled)-200:]
		}
		*dirty = true
	}
	return strings.Join(parts, "\n")
}

// ---- Knowledge 工具 ----

const knowledgeSchema = `{"type":"object","properties":{
"action":{"type":"string","enum":["list","search","read","write","edit","invalidate","delete","person"],"description":"list 看索引；search 搜；read 读一条；write 新建或整条覆盖；edit 精确替换一段；invalidate 标过时（推荐代替删除）；delete 真删；person 找人物画像"},
"query":{"type":"string","description":"search：关键词，中文直接写"},
"path":{"type":"string","description":"read/edit/invalidate/delete：记忆的路径（索引里括号里的那个）。write 不填就按标题新建，填了就整条覆盖。shared/ 开头的是所有 agent 共用的知识；self.md 是你的自我认知"},
"title":{"type":"string","description":"write：标题；人物就写名字"},
"type":{"type":"string","enum":["人物","事实","偏好","事件","经验","流程","知识","看法"],"description":"write：类型"},
"summary":{"type":"string","description":"write：一句话概括，会放进索引"},
"body":{"type":"string","description":"write：正文（Markdown）。人物画像写：是谁、和你的关系、性格、偏好、近况、要紧的事"},
"tags":{"type":"array","items":{"type":"string"}},
"people":{"type":"array","items":{"type":"string"},"description":"相关的人（人物条目的标题）"},
"ids":{"type":"array","items":{"type":"string"},"description":"人物：这个人在各渠道的身份（如 tg:123），找画像用"},
"when":{"type":"string","description":"事情发生的时间，如 2026-10-20"},
"confidence":{"type":"number","description":"看法的把握，0 到 1"},
"visible":{"type":"string","description":"all：谁在都能用；admin：只在主人的会话里；某个身份：只在和他的私聊里。不填按当前会话定"},
"old":{"type":"string","description":"edit：要换掉的原文，要一字不差且唯一"},
"new":{"type":"string","description":"edit：换成什么"},
"reason":{"type":"string","description":"invalidate：为什么过时了（比如换了新工作，见哪一条）"},
"type_filter":{"type":"string","description":"search：只搜这个类型"},
"person":{"type":"string","description":"search：只要跟这个人有关的"},
"include_invalid":{"type":"boolean","description":"search：连过时的也搜"},
"who":{"type":"string","description":"person：按身份找"},
"name":{"type":"string","description":"person：按名字找"}
},"required":["action"]}`

func knowledgeTool() proto.ToolSpec {
	return proto.ToolSpec{Name: knowledgeLabel, Description: "长期记忆（知识库）：记住、查以前的事——人（画像、关系）、事件、事实、偏好、经验、流程、学到的知识、你自己的看法。" +
		"用户说“记住……”时用 write；想不起来、需要细节时先 search 或 read；同一件事已经有了就 edit 或整条覆盖，别重复建；" +
		"事情变了（搬家、换工作、改主意）用 invalidate 把旧的标成过时，不要删。私聊里知道的事别在群里随便说。",
		InputSchema: json.RawMessage(knowledgeSchema)}
}

type knowledgeArgs struct {
	Action         string   `json:"action"`
	Query          string   `json:"query"`
	Path           string   `json:"path"`
	Title          string   `json:"title"`
	Type           string   `json:"type"`
	Summary        string   `json:"summary"`
	Body           string   `json:"body"`
	Tags           []string `json:"tags"`
	People         []string `json:"people"`
	IDs            []string `json:"ids"`
	When           string   `json:"when"`
	Confidence     *float64 `json:"confidence"`
	Visible        string   `json:"visible"`
	Old            string   `json:"old"`
	New            string   `json:"new"`
	Reason         string   `json:"reason"`
	TypeFilter     string   `json:"type_filter"`
	Person         string   `json:"person"`
	IncludeInvalid bool     `json:"include_invalid"`
	Who            string   `json:"who"`
	Name           string   `json:"name"`
}

// knowledgeCall 执行 Knowledge。调用方持有锁。
func (a *App) knowledgeCall(sid, who string, raw []byte) (string, error) {
	var x knowledgeArgs
	if err := json.Unmarshal(raw, &x); err != nil {
		return "", errors.New("参数格式不对")
	}
	ag := a.agentOf(sid)
	if !a.memoryOn(ag) {
		return "", errors.New("这个 agent 关掉了长期记忆")
	}
	v := a.viewerOf(sid, who)
	keep := func(e knowledge.Entry) bool { return e.VisibleTo(v) }
	ms, shared := a.memStore(ag.ID), a.sharedMem()
	today := a.now().In(a.loc).Format("2006-01-02")
	readOnly := v.Group
	inner := isInnerSID(sid)
	resolve := func(p string) (*knowledge.Store, string, bool) {
		p = strings.TrimSpace(p)
		if rest, ok := strings.CutPrefix(p, sharedPrefix); ok {
			return shared, rest, true
		}
		return ms, p, false
	}
	needWrite := func(isShared bool) error {
		if readOnly {
			return errors.New("群里只能查记忆，不能改（私聊或者整理记忆的时候再记）")
		}
		if isShared && !v.Admin {
			return errors.New("共用的知识只有主人的会话和整理记忆时能改")
		}
		return nil
	}
	switch x.Action {
	case "list":
		out := ms.IndexText(keep)
		if s := prefixPaths(shared.IndexText(keep)); s != "" {
			out += "\n# 共用的知识\n" + s
		}
		if a.selfText(ag.ID) != "" {
			out = "（自我认知在 self.md）\n" + out
		}
		if strings.TrimSpace(out) == "" {
			return "知识库还是空的（说“记住……”会马上记下；平时空闲或睡着时会把最近的聊天整理进来）。", nil
		}
		return out, nil
	case "search":
		o := knowledge.SearchOptions{Type: x.TypeFilter, Person: x.Person, IncludeInvalid: x.IncludeInvalid, Keep: keep, Limit: 8}
		hits := ms.Search(x.Query, o)
		for _, h := range shared.Search(x.Query, o) {
			h.Path = sharedPrefix + h.Path
			hits = append(hits, h)
		}
		slices.SortStableFunc(hits, func(p, q knowledge.Hit) int {
			switch {
			case p.Score > q.Score:
				return -1
			case p.Score < q.Score:
				return 1
			}
			return 0
		})
		if len(hits) > 8 {
			hits = hits[:8]
		}
		if len(hits) == 0 {
			return "没搜到。换个说法，或者 list 看看索引。", nil
		}
		var b strings.Builder
		for _, h := range hits {
			mark := ""
			if h.Invalid != "" {
				mark = "（已过时：" + h.Invalid + "）"
			}
			fmt.Fprintf(&b, "- [%s] %s（%s）%s：%s\n", h.Type, h.Title, h.Path, mark, h.Snippet)
		}
		return strings.TrimSpace(b.String()), nil
	case "read":
		if strings.TrimSpace(x.Path) == selfFile {
			if t := a.selfText(ag.ID); t != "" {
				return t, nil
			}
			return "self.md 还是空的。", nil
		}
		st, rel, _ := resolve(x.Path)
		e, ok := st.Get(rel)
		if !ok || !keep(e) {
			return "", fmt.Errorf("没有这条记忆：%s", x.Path)
		}
		return st.Raw(rel)
	case "person":
		e, ok := ms.Person(x.Who, x.Name)
		if !ok || !keep(e) {
			return "没有这个人的画像。", nil
		}
		return knowledge.Format(e), nil
	case "write":
		if strings.TrimSpace(x.Path) == selfFile {
			if readOnly || !v.Admin {
				return "", errors.New("自我认知只在主人的会话、内心活动和整理记忆时能改")
			}
			return "已经写好 self.md。", a.writeSelf(ag.ID, x.Body)
		}
		st, rel, isShared := resolve(x.Path)
		if err := needWrite(isShared); err != nil {
			return "", err
		}
		e := knowledge.Entry{Path: rel, Title: x.Title, Type: x.Type, Summary: x.Summary, Body: x.Body, Tags: x.Tags,
			People: x.People, IDs: x.IDs, When: x.When, Confidence: x.Confidence, Visible: strings.TrimSpace(x.Visible),
			Source: a.memSource(sid, today), Updated: today}
		if rel != "" {
			if old, ok := st.Get(rel); ok && !keep(old) {
				return "", fmt.Errorf("没有这条记忆：%s", x.Path)
			}
		}
		e.Visible = a.defaultVisible(v, e.Type, e.Visible)
		if e.Body == "" && e.Summary == "" {
			return "", errors.New("要写正文（body）或者一句话概括（summary）")
		}
		saved, err := st.Write(e)
		if err != nil {
			return "", err
		}
		a.publishMemory(ag.ID)
		if isShared {
			saved.Path = sharedPrefix + saved.Path
		}
		return fmt.Sprintf("记下了：[%s] %s（%s）", saved.Type, saved.Title, saved.Path), nil
	case "edit":
		if strings.TrimSpace(x.Path) == selfFile {
			if readOnly || !v.Admin {
				return "", errors.New("自我认知只在主人的会话、内心活动和整理记忆时能改")
			}
			t := a.selfText(ag.ID)
			if x.Old == "" || strings.Count(t, x.Old) != 1 {
				return "", errors.New("old 要和 self.md 里的一段一字不差、而且只出现一次")
			}
			return "改好了。", a.writeSelf(ag.ID, strings.Replace(t, x.Old, x.New, 1))
		}
		st, rel, isShared := resolve(x.Path)
		if err := needWrite(isShared); err != nil {
			return "", err
		}
		if old, ok := st.Get(rel); !ok || !keep(old) {
			return "", fmt.Errorf("没有这条记忆：%s", x.Path)
		}
		if _, err := st.Edit(rel, x.Old, x.New, today); err != nil {
			return "", err
		}
		a.publishMemory(ag.ID)
		return "改好了。", nil
	case "invalidate":
		st, rel, isShared := resolve(x.Path)
		if err := needWrite(isShared); err != nil {
			return "", err
		}
		if old, ok := st.Get(rel); !ok || !keep(old) {
			return "", fmt.Errorf("没有这条记忆：%s", x.Path)
		}
		if _, err := st.Invalidate(rel, x.Reason, today); err != nil {
			return "", err
		}
		a.publishMemory(ag.ID)
		return "标成过时了（还留着，搜索时加 include_invalid 能看到）。", nil
	case "delete":
		st, rel, isShared := resolve(x.Path)
		if err := needWrite(isShared); err != nil {
			return "", err
		}
		if !v.Admin && !inner {
			return "", errors.New("只有主人的会话和整理记忆时能真删；一般用 invalidate 标过时")
		}
		if err := st.Delete(rel); err != nil {
			return "", err
		}
		a.publishMemory(ag.ID)
		return "删了。", nil
	}
	return "", fmt.Errorf("不认识的动作 %q", x.Action)
}

// defaultVisible 是没写 visible 时的可见范围：群里公开；知识、流程、经验默认公开；其他跟着会话
// （主人的会话 admin，私聊是对方）。访客的私聊里只能写成对方自己可见的。
func (a *App) defaultVisible(v knowledge.Viewer, typ, vis string) string {
	switch {
	case v.Group:
		return "all"
	case !v.Admin:
		return v.Who
	case vis != "":
		return vis
	case typ == "知识" || typ == "流程" || typ == "经验":
		return "all"
	}
	return "admin"
}

// memSource 是记忆的出处：哪个会话、哪天。
func (a *App) memSource(sid, today string) string {
	switch {
	case isDreamSID(sid):
		return "dream " + today
	case isLifeSID(sid):
		return "内心活动 " + today
	}
	return sid + " " + today
}

func (a *App) writeSelf(agentID, text string) error {
	p := a.selfPath(agentID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimSpace(text)+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	a.publishMemory(agentID)
	return nil
}

// publishMemory 告诉网页某个 agent 的记忆变了。
func (a *App) publishMemory(agentID string) {
	a.publish(events.Event{T: "memory", Name: agentID})
}

// recentLines 是会话最近 n 条别人说的话（群里由触发开的一轮，用它来想起相关的记忆）。调用方持有锁。
func (a *App) recentLines(sid string, n int) string {
	h := a.store.History(sid)
	var lines []string
	for i := len(h) - 1; i >= 0 && len(lines) < n; i-- {
		if h[i].Role == "user" && !strings.HasPrefix(h[i].Content, memNoteOpen) && !strings.HasPrefix(h[i].Content, recallOpen) && !strings.HasPrefix(h[i].Content, lifeRecentOp) {
			lines = append(lines, h[i].Content)
		}
	}
	slices.Reverse(lines)
	return strings.Join(lines, "\n")
}

// cmdMemory 是 /memory：没有关键词看索引，有就搜（按这个窗口能看的范围）。
func (a *App) cmdMemory(c in, arg string) {
	if !a.memoryOn(a.agentOf(c.s.sid)) {
		a.answer(c, "这个 agent 关掉了长期记忆（在网页的 agent 节点里能打开）。")
		return
	}
	args := map[string]any{"action": "list"}
	if q := strings.TrimSpace(arg); q != "" {
		args = map[string]any{"action": "search", "query": q}
	}
	raw, _ := json.Marshal(args)
	out, err := a.knowledgeCall(c.s.sid, c.who, raw)
	if err != nil {
		out = err.Error()
	}
	a.answer(c, out)
}

// cmdForget 是 /forget：按标题或路径找到一条，标成过时。
func (a *App) cmdForget(c in, arg string) {
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能让 agent 忘掉东西")
		return
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		a.answer(c, "用法：/forget <标题或路径>（先 /memory 看看有哪些）")
		return
	}
	ag := a.agentOf(c.s.sid)
	if !a.memoryOn(ag) {
		a.answer(c, "这个 agent 关掉了长期记忆。")
		return
	}
	ms := a.memStore(ag.ID)
	path := ""
	if e, ok := ms.Get(arg); ok {
		path = e.Path
	} else {
		for _, e := range ms.List() {
			if e.Invalid == "" && e.Title == arg {
				path = e.Path
				break
			}
		}
	}
	if path == "" {
		a.answer(c, "没找到「"+arg+"」。先 /memory "+arg+" 搜一下，再用搜到的路径。")
		return
	}
	if _, err := ms.Invalidate(path, "主人让忘掉", a.now().In(a.loc).Format("2006-01-02")); err != nil {
		a.answer(c, "没改成："+err.Error())
		return
	}
	a.publishMemory(ag.ID)
	a.answer(c, "好，「"+arg+"」标成过时了，以后不会再想起（文件还在 "+path+"，网页里能恢复）。")
}
