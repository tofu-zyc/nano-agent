package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/experience"
	"nano-agent/gateway/internal/knowledge"
)

func (a *App) experienceOf(agent string) *experience.Store {
	if a.experiences == nil {
		a.experiences = map[string]*experience.Store{}
	}
	if s := a.experiences[agent]; s != nil {
		return s
	}
	s, err := experience.Open(a.store.Dir(), agent)
	if err != nil {
		a.log.Warn("经历记录不可追加，保留现场", "agent", agent, "err", err)
	}
	a.experiences[agent] = s
	// 旧近况保留读兼容，稳定来源键保证重启/部分导入不重复。
	ls := a.lifeOf(agent)
	if ls.ExperienceImported {
		return s
	}
	for i, n := range ls.Recent {
		e := experience.Entry{Kind: "life", Where: "旧版内心活动", Visible: "admin", Said: n.Text, Status: "ok", Sources: []string{fmt.Sprintf("legacy-recent:%d:%d", n.At, i)}}
		if err := s.Append(e, time.Unix(n.At, 0)); err != nil {
			a.log.Warn("旧近况导入失败", "agent", agent, "err", err)
			return s
		}
	}
	ls.ExperienceImported = true
	a.saveLife()
	return s
}

func (a *App) appendExperience(agent string, e experience.Entry) {
	if a.agentRoot == "" {
		return
	}
	ag, ok := a.agentByID(agent)
	if !ok || !ag.Life() {
		return
	}
	if err := a.experienceOf(agent).Append(e, a.now()); err != nil {
		a.log.Warn("经历写入失败（聊天结果保留）", "agent", agent, "err", err)
	}
}

func (a *App) experienceVisibility(sid, who string) string {
	v := a.viewerOf(sid, who)
	if v.Group {
		return "all"
	}
	if v.Admin {
		return "admin"
	}
	if v.Who != "" {
		return v.Who
	}
	return "admin"
}

func (a *App) experienceNote(sid, who string) string {
	ag := a.agentOf(sid)
	if !ag.Life() || isInnerSID(sid) || a.agentRoot == "" {
		return ""
	}
	rows := a.experienceOf(ag.ID).Recent(a.viewerOf(sid, who), sid, 6, a.now())
	if len(rows) == 0 {
		return ""
	}
	return "[经历]\n以下都已在当时的场合处理结束，是经历，不是待办。\n" + experience.Render(rows, a.loc, 1600) + "[/经历]"
}

func (a *App) recordRunExperience(s *sess, r *run, status, body string) {
	ag := a.agentOf(s.sid)
	if !ag.Life() || a.agentRoot == "" {
		return
	}
	kind, where, visible := "chat", s.sid, a.experienceVisibility(s.sid, r.from)
	if r.inner {
		if !isLifeSID(s.sid) {
			return
		}
		kind, where, visible = "life", "自己的时间", "admin"
	} else if isInnerSID(s.sid) || isCronSID(s.sid) {
		return
	} else if a.isBranchSID(s.sid) {
		// 分支（Fork 和拟人自主分支）也是主人看得见的经历，但保持安静：不因为
		// 分支里说过话就给访客看到，也不把分支的结论当成源聊天的回答。
		kind, where, visible = "branch", "安静分支", "admin"
	} else if a.isOwnerSID(ag.ID, s.sid) {
		where = "和主人的私聊"
	}
	if kind == "chat" && (isSilence(body) || strings.TrimSpace(r.asked) == "") {
		return
	}
	if kind == "branch" && isSilence(body) && len(r.toolEvidence) == 0 && len(r.experienceActions) == 0 {
		return
	}
	e := experience.Entry{Kind: kind, SID: s.sid, Where: where, Visible: visible, RunID: r.runID, Asked: r.asked, Said: body, Status: status, Generation: a.store.Settings(s.sid).SessionGeneration, Sources: []string{"run:" + r.runID}}
	if kind == "life" {
		e.Asked = ""
		e.Episode = r.runID
		if s.mind != nil {
			e.Reason = s.mind.Trigger.Reason
			e.AgendaID = s.mind.Trigger.AgendaID
		}
	}
	publicWeb := kind == "life" && len(r.toolEvidence) > 0
	ids := make([]string, 0, len(r.toolEvidence))
	for id := range r.toolEvidence {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		proof := r.toolEvidence[id]
		if proof.Name != "WebSearch" && proof.Name != "WebFetch" {
			publicWeb = false
		}
		label := proof.Name
		if call, ok := r.callArgs[id]; ok {
			var args map[string]any
			if json.Unmarshal(call.Args, &args) == nil {
				for _, field := range []string{"action", "title", "id", "to"} {
					if val, ok := args[field].(string); ok && val != "" {
						label += " " + field + "=" + cutRunes(val, 80)
					}
				}
			}
		}
		if !proof.OK {
			label += "（失败）"
		} else {
			label += "（已执行）"
		}
		e.Did = append(e.Did, label)
	}
	if publicWeb {
		e.Visible = "all"
	}
	if len(r.experienceActions) > 0 {
		e.Did = append([]string(nil), r.experienceActions...)
		if kind == "life" && !r.experiencePrivate {
			e.Visible = "all"
		} else if kind == "life" {
			e.Visible = "admin"
		}
	}
	for _, item := range a.mind(ag.ID).OpenAgenda() {
		if item.Created >= r.start.Unix() {
			e.Open = append(e.Open, item.ID)
		}
	}
	a.appendExperience(ag.ID, e)
}

func (a *App) lifecycleExperience(sid, action string) {
	a.lifecycleExperienceFor(a.execAgentID(sid), sid, action)
}
func (a *App) lifecycleExperienceFor(agent, sid, action string) {
	ag, ok := a.agentByID(agent)
	if !ok || !ag.Life() || a.agentRoot == "" {
		return
	}
	e := experience.Entry{Kind: "lifecycle", SID: sid, Where: sid, Visible: a.experienceVisibility(sid, ""), Said: action, Status: "ok", Generation: a.store.Settings(sid).SessionGeneration}
	if strings.Contains(action, "清空") {
		for _, row := range a.experienceOf(agent).Recent(knowledge.Viewer{Admin: true}, "", 500, a.now()) {
			if row.Kind == "chat" && row.SID == sid {
				e.Did = append([]string(nil), row.Did...)
				break
			}
		}
	}
	a.appendExperience(agent, e)
}

// AgentExperience 是管理页的主人视图；聊天注入另走 viewerOf 的权限过滤。
func (a *App) AgentExperience(agent string, limit int) ([]experience.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ag, ok := a.agentByID(agent)
	if !ok || !ag.Life() {
		return nil, fmt.Errorf("没有这个拟人 agent")
	}
	if limit <= 0 {
		limit = 12
	}
	return a.experienceOf(agent).Recent(knowledge.Viewer{Admin: true}, "", limit, a.now()), nil
}
