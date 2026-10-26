package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
)

// MCP 的 RPC 成功和动作效果已确认是两件事；明确的否定不能充当完成证据。
func mcpOutcomeVerified(value any) bool {
	if value == nil {
		return true
	}
	b, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var outcome struct {
		Verified *bool  `json:"verified"`
		Success  *bool  `json:"success"`
		Effect   string `json:"effect"`
	}
	if json.Unmarshal(b, &outcome) != nil {
		return true // 普通数组/标量结果没有声明失败
	}
	if outcome.Verified != nil && !*outcome.Verified {
		return false
	}
	if outcome.Success != nil && !*outcome.Success {
		return false
	}
	switch outcome.Effect {
	case "unverifiable", "unverified", "failed", "denied", "rejected", "not_applied":
		return false
	}
	return true
}

// mindMachineChanged 只通知使用该设备的 agent；状态不变时不重复触发模型。
func (a *App) mindMachineChanged(machine string, ready bool) {
	if a.ctx.Err() != nil || a.agentStore == nil || a.agentRoot == "" {
		return
	}
	if a.mindMachines == nil {
		a.mindMachines = map[string]bool{}
	}
	if old, exists := a.mindMachines[machine]; exists && old == ready {
		return
	}
	a.mindMachines[machine] = ready
	for _, ag := range a.agentStore.List() {
		if !ag.Life() {
			continue
		}
		uses := false
		w := a.workspaceOf(lifeSID(ag.ID))
		if len(w.wss) == 0 && machine == host.LocalName {
			uses = true
		}
		for _, ws := range w.wss {
			uses = uses || ws.Host == machine
		}
		st := a.mind(ag.ID)
		for _, item := range st.OpenAgenda() {
			for _, id := range item.Workspaces {
				if ws := a.findWorkspace(id); ws != nil && ws.Host == machine {
					uses = true
					if ready && strings.HasPrefix(item.Result, "没完成：") {
						_ = st.MoveAgenda(item.ID, a.now().Unix(), a.now())
					}
				}
			}
		}
		if a.mcpStore != nil {
			for _, server := range a.mcpStore.List() {
				target := server.Machine
				if target == "" || target == "gateway" {
					target = host.LocalName
				}
				uses = uses || (server.Enabled && server.ForAgent(ag.ID) && target == machine)
			}
		}
		if uses {
			state := "离线，结果未知的动作不要重放"
			if ready {
				state = "上线，可以重新观察后继续等待中的任务"
			}
			a.mindExternal(ag.ID, "machine:"+machine, machine+" "+state)
		}
	}
}

type agendaCompletion struct {
	Note     string
	Evidence []string
}

// recordToolEvidence 只接收当前 run 已发出的调用对应的真实结果。
func (a *App) recordToolEvidence(r *run, m *proto.Msg) {
	call, ok := r.callArgs[m.ID]
	if !ok {
		return
	}
	if r.toolEvidence == nil {
		r.toolEvidence = map[string]autonomy.ToolEvidence{}
	}
	if _, exists := r.toolEvidence[m.ID]; exists {
		return
	}
	r.toolEvidence[m.ID] = autonomy.ToolEvidence{ID: m.ID, Name: call.Name,
		OK: m.OK == nil || *m.OK, Verified: (m.OK == nil || *m.OK) && !r.unverifiedTools[m.ID], Result: cutRunes(m.Content, 1000)}
}

func taskEvidence(name string) bool {
	switch name {
	case "Agenda", "Idea", "Mind", "Skill", "Gateway", "FollowUp", "Schedule":
		return false
	}
	return name != ""
}

// stageAgendaCompletion 把模型的验收声明绑定到本轮真实且成功的工具结果。
// 中断、超时、预算耗尽均不提交；手动网页验收仍由管理员显式确认。
func (a *App) stageAgendaCompletion(sid, id, note string, ids []string) error {
	st := a.mind(a.execAgentID(sid))
	item, ok := st.AgendaByID(id)
	if !ok || item.Status == autonomy.StatusCancelled || item.Status == autonomy.StatusDone || item.Status == autonomy.StatusSkipped {
		return errors.New("没有待验收的这个日程项")
	}
	if strings.TrimSpace(note) == "" {
		return errors.New("要写明真实结果，以及怎样满足日程的验收条件")
	}
	s := a.sessions[sid]
	if s == nil || s.run == nil || len(ids) == 0 {
		return errors.New("验收需要 evidence：引用本轮真实成功的操作或观察的工具调用 ID")
	}
	for _, id := range ids {
		e, ok := s.run.toolEvidence[id]
		if !ok || !e.OK || !e.Verified || !taskEvidence(e.Name) {
			return fmt.Errorf("%s 不是本轮已成功的实际操作或观察，不能用作验收证据", id)
		}
	}
	if s.run.agendaDone == nil {
		s.run.agendaDone = map[string]agendaCompletion{}
	}
	s.run.agendaDone[id] = agendaCompletion{Note: strings.TrimSpace(note), Evidence: append([]string(nil), ids...)}
	return nil
}

// commitAgendaResults 在 run 正常结束时提交。当前自主日程由 State.Finish 连同执行记录一起完成。
func (a *App) commitAgendaResults(s *sess, r *run, status string) {
	if status != proto.StatusOK || len(r.agendaDone) == 0 {
		return
	}
	st := a.mind(a.execAgentID(s.sid))
	for id, done := range r.agendaDone {
		if s.mind != nil && s.mind.Trigger.AgendaID == id {
			continue
		}
		if item, ok := st.AgendaByID(id); !ok || item.Status == autonomy.StatusCancelled || item.Status == autonomy.StatusSkipped {
			continue
		}
		if err := st.CompleteAgenda(id, done.Note, a.now()); err != nil {
			a.log.Warn("提交日程验收失败", "agenda", id, "err", err)
		}
	}
	a.saveMinds()
	a.publishAutonomy(a.execAgentID(s.sid))
}
