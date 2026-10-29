package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/hooks"
	"nano-agent/gateway/internal/proto"
)

type HookRequest struct {
	Action      string          `json:"action"`
	ID          string          `json:"id,omitempty"`
	Revision    uint64          `json:"revision,omitempty"`
	OperationID string          `json:"operation_id,omitempty"`
	Agent       string          `json:"agent,omitempty"`
	Spec        *hooks.Spec     `json:"spec,omitempty"`
	Once        *bool           `json:"once,omitempty"`
	Enabled     *bool           `json:"enabled,omitempty"`
	Rearm       bool            `json:"rearm,omitempty"`
	Event       *eventlog.Event `json:"event,omitempty"`
	After       string          `json:"after,omitempty"`
}
type HookView struct {
	eventlog.RuleRecord
	Spec         hooks.Spec `json:"spec"`
	PausedReason string     `json:"paused_reason,omitempty"`
}
type HookResponse struct {
	Rules      []HookView          `json:"rules,omitempty"`
	Rule       *HookView           `json:"rule,omitempty"`
	Deliveries []eventlog.Delivery `json:"deliveries,omitempty"`
	Next       string              `json:"next,omitempty"`
	Matched    bool                `json:"matched,omitempty"`
}

func hookTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "Hook", Description: "管理可靠事件 Hook：list/get/create/update/enable/disable/delete/test/runs。notify 固定模板不调用模型，command 是有界短程序，agent 继续由 C 运行。模型任务可选 temporary/origin/life/branch；branch 默认安静。一次性 deadline 与周期 Schedule 分开。create 带稳定 operation_id；更新带 revision。不要建立 Schedule 反复轮询 Job。修改总是需要管理员审批。", InputSchema: json.RawMessage(`{
 "type":"object","properties":{
 "action":{"type":"string","enum":["list","get","create","update","enable","disable","delete","test","runs"]},
 "id":{"type":"string","maxLength":128},"revision":{"type":"integer","minimum":1},
 "operation_id":{"type":"string","maxLength":96},"agent":{"type":"string"},
 "spec":{"type":"object","description":"name,on,match,handler,deliver,debounce_ms,cooldown_ms,retries,priority,on_error,deadline_at,watch。handler.type=notify/command/agent；conversation=temporary/origin/life/branch。scope 仅可缩小当前工作目录和模式；投递窗口由网关固定。","properties":{
 "name":{"type":"string"},"on":{"type":"array","items":{"type":"string"}},"match":{"type":"object"},
 "handler":{"type":"object","properties":{"type":{"enum":["notify","command","agent"]},"text":{"type":"string"},"command":{"type":"string"},"script":{"type":"string"},"prompt":{"type":"string"},"conversation":{"enum":["temporary","origin","life","branch"]},"branch_sid":{"type":"string"},"timeout_ms":{"type":"integer"},"token_budget":{"type":"integer"},"retry_safe":{"type":"boolean"}}},
 "deliver":{"enum":["creator","origin","none"]},"scope":{"type":"object"},
 "deadline_at":{"type":"integer","description":"一次性 Unix 毫秒；on 包含 deadline.at"},
 "debounce_ms":{"type":"integer"},"cooldown_ms":{"type":"integer"},"retries":{"type":"integer"},"priority":{"type":"integer"},"on_error":{"enum":["allow","deny"]},"watch":{"type":"object"}}},
 "once":{"type":"boolean"},"enabled":{"type":"boolean"},"rearm":{"type":"boolean"},
 "event":{"type":"object","description":"test 的普通测试数据，不执行副作用"},"after":{"type":"string"}
 },"required":["action"]}`)}
}
func (a *App) hookToolCall(s *sess, m *proto.Msg, reply func(bool, string)) {
	var req HookRequest
	if err := json.Unmarshal(m.Args, &req); err != nil {
		reply(false, "Hook 参数不正确")
		return
	}
	sid, who := s.sid, s.run.from
	go func() {
		out, err := a.HookAction(a.ctx, sid, who, req)
		if err != nil {
			reply(false, err.Error())
			return
		}
		raw, _ := json.Marshal(out)
		reply(true, string(raw))
	}()
}
func (a *App) hookFingerprint(ch string) string {
	var identity any = ch
	switch ch {
	case "tg":
		identity = []string{a.cfg.Telegram.Token, a.cfg.Telegram.APIBase}
	case "feishu":
		identity = []string{a.cfg.Feishu.AppID, a.cfg.Feishu.AppSecret, a.cfg.Feishu.Domain}
	default:
		if _, id, ok := strings.Cut(ch, "."); ok && a.botStore != nil {
			if b, exists := a.botStore.Get(id); exists {
				identity = []string{b.ID, b.Kind, b.Agent, b.Token, b.AppID, b.AppSecret, b.APIBase, b.BotID}
			}
		}
	}
	raw, _ := json.Marshal(identity)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (a *App) hookScope(sid, who, agent string, s hooks.Spec) (hooks.Spec, error) {
	if sid == "" || !a.agentExists(agent) || a.execAgentID(sid) != agent || !a.canAccessForkContext(who, "", sid) {
		return s, errors.New("Hook 来源会话与 agent 不可用")
	}
	w := a.workspaceOf(sid)
	_, offered, _, _ := a.contextParts(sid)
	names := []string{}
	for _, t := range offered {
		names = append(names, t.Function.Name)
	}
	if s.Scope.Tools == nil {
		s.Scope.Tools = names
	} else {
		for _, tool := range s.Scope.Tools {
			if !slices.Contains(names, tool) {
				return s, fmt.Errorf("来源没有工具 %s", tool)
			}
		}
	}
	if s.Scope.SID != "" && s.Scope.SID != sid {
		return s, errors.New("不能替换 Hook 来源会话")
	}
	s.Scope.SID = sid
	ceiling := a.runMode(w, who)
	if s.Scope.Mode == "" {
		s.Scope.Mode = ceiling
	}
	if modeRank(s.Scope.Mode) > modeRank(ceiling) {
		return s, errors.New("Hook 模式超过登记会话的权限")
	}
	if s.Scope.Workspaces == nil {
		s.Scope.Workspaces = w.ids()
	}
	for _, id := range s.Scope.Workspaces {
		if !w.has(id) {
			return s, fmt.Errorf("来源未挂载工作目录 %s", id)
		}
	}
	s.Scope.Endpoints = nil
	if strings.HasPrefix(who, "web:") {
		s.Scope.Endpoints = append(s.Scope.Endpoints, hooks.Endpoint{Channel: "web", Chat: sid, Fingerprint: a.hookFingerprint("web")})
	}
	for key, ep := range a.eps {
		if ep.Session != sid || ep.Who != who || !a.allowed(key) || strings.HasPrefix(who, "web:") && strings.HasPrefix(key, "web:") {
			continue
		}
		target := parseEndpoint(key)
		s.Scope.Endpoints = append(s.Scope.Endpoints, hooks.Endpoint{Channel: target.ch, Chat: target.chat, Fingerprint: a.hookFingerprint(target.ch)})
	}
	slices.SortFunc(s.Scope.Endpoints, func(x, y hooks.Endpoint) int { return strings.Compare(x.Channel+":"+x.Chat, y.Channel+":"+y.Chat) })
	if s.Handler.Conversation == "life" {
		ag, _ := a.agentByID(agent)
		if !ag.Life() {
			return s, errors.New("life 目标需要拟人 agent")
		}
	}
	if s.Handler.Conversation == "branch" && s.Handler.BranchSID != "" {
		br, ok := a.store.Branch(s.Handler.BranchSID)
		if !ok || br.OwnerAgent != agent || !a.canAccessForkContext(who, sid, s.Handler.BranchSID) {
			return s, errors.New("没有指定分支的访问权限")
		}
	}
	return hooks.Normalize(s)
}
func (a *App) hookView(r eventlog.RuleRecord) HookView {
	spec, _ := hooks.Parse(r)
	v := HookView{RuleRecord: r, Spec: spec}
	v.Body = nil
	if err := a.hookAuthority(r, spec); err != nil {
		v.PausedReason = err.Error()
	}
	return v
}

// App authorization is frozen before the single-writer journal commit. No
// fsync or script file read occupies App.mu; execution rechecks current scope.
func (a *App) HookAction(ctx context.Context, sid, who string, req HookRequest) (HookResponse, error) {
	a.mu.Lock()
	if !a.isAdmin(who) {
		a.mu.Unlock()
		return HookResponse{}, errors.New("只有管理员能管理 Hook")
	}
	store := a.jobEvents
	if store == nil {
		a.mu.Unlock()
		return HookResponse{}, errors.New("可靠事件日志不可用")
	}
	agent := req.Agent
	if agent == "" {
		agent = a.execAgentID(sid)
	}
	out := HookResponse{}
	switch req.Action {
	case "list":
		out.Rules = []HookView{}
		for _, r := range store.Rules(agent) {
			out.Rules = append(out.Rules, a.hookView(r))
		}
		a.mu.Unlock()
		return out, nil
	case "runs":
		rows, next := store.Deliveries(req.After, 128)
		out.Deliveries = []eventlog.Delivery{}
		for _, d := range rows {
			if d.Registered && d.AgentID == agent && (req.ID == "" || d.HookID == req.ID) {
				d.Rule = nil
				out.Deliveries = append(out.Deliveries, d)
			}
		}
		out.Next = next
		a.mu.Unlock()
		return out, nil
	}
	r, exists := store.Rule(req.ID)
	if req.Action == "create" {
		if req.Spec == nil || req.OperationID == "" || len(req.OperationID) > 96 {
			a.mu.Unlock()
			return out, errors.New("创建需要 spec 与稳定 operation_id")
		}
		sum := sha256.Sum256([]byte(agent + "\x00" + sid + "\x00" + who + "\x00" + req.OperationID))
		operation := "hc_" + hex.EncodeToString(sum[:])
		if old, ok := store.RuleOperation(operation); ok {
			if old.Deleted {
				a.mu.Unlock()
				return out, errors.New("该创建操作已删除，不能换 ID 重新导入")
			}
			v := a.hookView(old)
			out.Rule = &v
			a.mu.Unlock()
			return out, nil
		}
		if len(store.Rules(agent)) >= 64 {
			a.mu.Unlock()
			return out, errors.New("每个 agent 最多 64 条 Hook")
		}
		id := req.ID
		if id == "" {
			id = "hook_" + randHex(12)
		}
		if exists {
			a.mu.Unlock()
			return out, errors.New("规则 ID 已存在")
		}
		r = eventlog.RuleRecord{ID: id, AgentID: agent, Creator: who, OperationID: operation, Revision: 1, Enabled: true, Silent: a.branchSilent(sid)}
	} else if !exists || r.Deleted || r.AgentID != agent {
		a.mu.Unlock()
		return out, errors.New("没有这个 Hook")
	}
	spec, err := hooks.Parse(r)
	if req.Action == "create" {
		spec = *req.Spec
		err = nil
	}
	if req.Action == "get" {
		v := a.hookView(r)
		out.Rule = &v
		a.mu.Unlock()
		return out, nil
	}
	if req.Action == "test" {
		if err == nil && req.Event != nil {
			out.Matched = hooks.Matches(spec, *req.Event)
		}
		a.mu.Unlock()
		return out, err
	}
	expected := r.Revision
	if req.Action != "create" {
		if req.Revision != r.Revision {
			a.mu.Unlock()
			return out, errors.New("规则修订已变化，请重新读取")
		}
		r.Revision++
	}
	switch req.Action {
	case "create", "update":
		if req.Spec != nil {
			spec = *req.Spec
		}
		source := sid
		if req.Action == "update" {
			old, _ := hooks.Parse(r)
			source = old.Scope.SID
		}
		spec, err = a.hookScope(source, r.Creator, r.AgentID, spec)
		if req.Once != nil {
			r.Once = *req.Once
		}
		if req.Enabled != nil {
			r.Enabled = *req.Enabled
		}
	case "enable":
		r.Enabled = true
	case "disable":
		r.Enabled = false
	case "delete":
		r.Enabled = false
		r.Deleted = true
	default:
		err = errors.New("不支持的 Hook 操作")
	}
	if err == nil && req.Action == "enable" {
		err = a.hookAuthority(r, spec)
	}
	if err == nil {
		r.Body, err = json.Marshal(spec)
	}
	engine := a.hookEngine
	a.mu.Unlock()
	if err != nil {
		return out, err
	}
	if (req.Action == "create" || req.Action == "update") && spec.Handler.Script != "" {
		a.mu.Lock()
		root := a.agentRoot
		a.mu.Unlock()
		content, readErr := hookScript(root, r.AgentID, spec.Handler.Script)
		if readErr != nil {
			return out, readErr
		}
		spec.Handler.Digest = scriptDigest(content)
		spec, err = hooks.Normalize(spec)
		if err != nil {
			return out, err
		}
		r.Body, _ = json.Marshal(spec)
	}
	if req.Action == "create" {
		expected = 0
	}
	if err = store.Commit(ctx, eventlog.Transaction{Rule: &eventlog.RuleChange{Record: r, Expected: expected, Rearm: req.Rearm}}); err != nil {
		return out, err
	}
	if engine != nil {
		engine.Wake()
	}
	a.mu.Lock()
	v := a.hookView(r)
	out.Rule = &v
	a.publish(events.Event{T: "hooks", Name: agent})
	a.mu.Unlock()
	return out, nil
}
func isHookSID(sid string) bool {
	return strings.HasPrefix(sid, "hook:") || strings.Contains(sid, "/hook:")
}
func hookSID(agent, id string) string { return agents.SessionFor(agent, "hook:"+id) }

func (a *App) cmdHook(c in, command, arg, messageID string) {
	req := HookRequest{Action: "list"}
	if command == "hook" {
		fields := strings.Fields(arg)
		if len(fields) > 0 && fields[0] == "create" {
			var spec hooks.Spec
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(arg, "create"))), &spec); err != nil {
				a.answer(c, "创建规则需为 /hook create <规则 JSON>")
				return
			}
			op := messageID
			if op == "" {
				op = randHex(12)
			}
			req = HookRequest{Action: "create", OperationID: c.ep.key() + ":" + op, Spec: &spec}
		} else if len(fields) > 0 {
			req.ID = fields[0]
			req.Action = "get"
			if len(fields) > 1 {
				req.Action = fields[1]
			}
			if a.jobEvents != nil {
				if record, ok := a.jobEvents.Rule(req.ID); ok {
					req.Revision = record.Revision
				}
			}
		} else {
			a.answer(c, "/hooks 列出规则；/hook <编号> [get|runs|enable|disable|delete]；/hook create <规则 JSON>")
			return
		}
	}
	sid, who, ep, ch, agent := c.s.sid, c.who, c.ep, a.chans[c.ep.ch], a.execAgentID(c.s.sid)
	go func() {
		out, err := a.HookAction(a.ctx, sid, who, req)
		text := ""
		if err != nil {
			text = err.Error()
		} else if req.Action == "list" {
			var lines []string
			for _, r := range out.Rules {
				status := "停用"
				if r.Enabled {
					status = "启用"
				}
				lines = append(lines, fmt.Sprintf("%s · %s · %s", r.ID, r.Spec.Name, status))
			}
			text = strings.Join(lines, "\n")
			if text == "" {
				text = "还没有事件规则"
			}
		} else {
			raw, _ := json.MarshalIndent(out, "", "  ")
			text = string(raw)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.jobReply(ch, ep, agent, sid, text)
	}()
}
