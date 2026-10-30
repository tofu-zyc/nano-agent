package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
			a.log.Warn("Hook 操作没有通过", "action", req.Action, "sid", sid, "who", who, "err", err)
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
	if s.Scope.Tools != nil {
		for _, tool := range s.Scope.Tools {
			if !slices.Contains(names, tool) {
				return s, fmt.Errorf("来源没有工具 %s", tool)
			}
		}
	} else if s.Handler.Type == "agent" {
		// 只有会跑模型的处理器需要冻结「登记时能用的工具」；notify 和 command 一个工具都不调用，
		// 把它们塞进规则只是让 MCP 多的会话永远登记不上（工具表上百条就超过上限）。
		if len(names) <= hooks.MaxScopedTools {
			s.Scope.Tools = names
		} else {
			a.log.Warn("Hook 来源的工具太多，不冻结工具表", "sid", sid, "tools", len(names), "limit", hooks.MaxScopedTools)
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
	// notify 只会往登记时冻结的窗口发；没有窗口的会话（内心活动、安静分支）建了也永远不会响，
	// 与其让它在投递时反复重试，不如登记的时候就说清楚。
	if s.Handler.Type == "notify" && s.Deliver != "none" && len(s.Scope.Endpoints) == 0 && !a.branchSilent(sid) {
		return s, errors.New("这个会话没有投递窗口：notify 只发到登记时冻结的聊天窗口；内心活动请用 handler.type=agent 加 conversation=life，或者把 deliver 设成 none")
	}
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
	a.syncHookWatches()
	a.mu.Lock()
	v := a.hookView(r)
	out.Rule = &v
	a.publish(events.Event{T: "hooks", Name: agent})
	a.mu.Unlock()
	return out, nil
}

// ---- 文件观察（设计 §6.6）：只登记明确文件或非递归目录 ----

type hookWatchRule struct {
	Key, Path, Workspace, Machine, Agent, SID string
}

// hookWatchKey 是同一 (工作目录, 路径) 的稳定短键；内核只原样带回，不解释它。
func hookWatchKey(workspace, path string) string {
	sum := sha256.Sum256([]byte(workspace + "\x00" + path))
	return "w" + hex.EncodeToString(sum[:12])
}

// hookWatch 解析一条规则的观察登记：相对路径按工作目录展开。调用者持有 App.mu。
func (a *App) hookWatch(spec hooks.Spec) (hookWatchRule, bool) {
	if spec.Watch == nil {
		return hookWatchRule{}, false
	}
	var ws *Workspace
	for i := range a.wss {
		if a.wss[i].ID == spec.Watch.Workspace {
			ws = &a.wss[i]
			break
		}
	}
	if ws == nil {
		return hookWatchRule{}, false
	}
	path := spec.Watch.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(ws.Path, path)
	}
	return hookWatchRule{Key: hookWatchKey(ws.ID, path), Path: path, Workspace: ws.ID, Machine: ws.Host, SID: spec.Scope.SID}, true
}

// hookWatchRules 列出启用中的规则要求的观察登记（agent 在下面按规则填）。调用者持有 App.mu。
func (a *App) hookWatchRules() []hookWatchRule {
	if a.jobEvents == nil {
		return nil
	}
	out := []hookWatchRule{}
	for _, rule := range a.jobEvents.Rules("") {
		if !rule.Enabled || rule.Deleted {
			continue
		}
		spec, err := hooks.Parse(rule)
		if err != nil {
			continue
		}
		w, ok := a.hookWatch(spec)
		if !ok {
			continue
		}
		w.Agent = rule.AgentID
		out = append(out, w)
	}
	return out
}

// syncHookWatches 把当前观察集合推给在线的内核。断线或内核重启后重新下发，
// 并让内核各报一次 resync：那段时间的变化没有记录，处理器要自己重新比较。
// 只是集合变了（新加一条规则）不报缺口，原来那条一直在正常观察。
func (a *App) syncHookWatches() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hub == nil || a.jobEvents == nil {
		return
	}
	if a.watchDigest == nil {
		a.watchDigest, a.watchBroken = map[string]string{}, map[string]bool{}
	}
	want := map[string][]proto.WatchSub{}
	seen := map[string]bool{}
	for _, w := range a.hookWatchRules() {
		node := a.kernelOn(w.Machine)
		if node == "" || a.nodes[node] == nil || a.nodes[node].info.Features["file_watch"] < 1 || seen[node+"\x00"+w.Key] {
			continue
		}
		seen[node+"\x00"+w.Key] = true
		want[node] = append(want[node], proto.WatchSub{Key: w.Key, Path: w.Path})
	}
	for node, n := range a.nodes {
		if n.info.Features["file_watch"] < 1 {
			continue
		}
		list := want[node]
		if list == nil {
			list = []proto.WatchSub{}
		}
		raw, _ := json.Marshal(list)
		broken := a.watchBroken[node]
		if !broken && a.watchDigest[node] == string(raw) {
			continue
		}
		a.watchDigest[node] = string(raw)
		a.watchBroken[node] = false
		a.hub.Send(node, proto.Msg{T: proto.TWatchSync, Watches: list, Resync: broken})
	}
}

// hookCause 是一台机器上正在执行的 Hook 动作；窗口内的文件变化记在它名下。
type hookCause struct {
	Machine, Hook string
	Until         int64
}

// noteHookCause 记下一个刚开始执行的 Hook 动作（最多 64 条，过期的先扔掉）。
func (a *App) noteHookCause(machine, hook string, timeoutMS int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now().Unix()
	live := a.hookCauses[:0]
	for _, c := range a.hookCauses {
		if c.Until >= now {
			live = append(live, c)
		}
	}
	a.hookCauses = live
	if len(a.hookCauses) >= 64 {
		a.hookCauses = a.hookCauses[1:]
	}
	a.hookCauses = append(a.hookCauses, hookCause{Machine: machine, Hook: hook, Until: now + int64(timeoutMS/1000) + 5})
}

// hookCauseOnLocked 是这台机器上此刻正在执行的 Hook 动作（没有就返回空）。调用方持有锁。
func (a *App) hookCauseOnLocked(machine string) string {
	now := a.now().Unix()
	for _, c := range a.hookCauses {
		if c.Until >= now && c.Machine == machine {
			return c.Hook
		}
	}
	return ""
}

// fileChanged 把内核的观察结果写成可靠事件：每个关心这个路径的 agent/来源会话一条。
// 如果变化发生在某个 Hook 动作执行期间，就记成它派生的事件：同一个 Hook 不会消费
// 自己写出来的变化，其他 Hook 仍按因果深度处理。序号一次分配完，两台机器的观察同时
// 到达也不会撞到同一个 Seq 而被可靠日志当成重复丢掉。
func (a *App) fileChanged(node string, m *proto.Msg) {
	a.mu.Lock()
	store := a.jobEvents
	epoch := ""
	if n := a.nodes[node]; n != nil {
		epoch = n.info.NodeEpoch
	}
	rules := a.hookWatchRules()
	machine := a.machineOf(node)
	a.mu.Unlock()
	if store == nil || epoch == "" {
		return
	}
	now := a.now().Unix()
	type pendingEvent struct {
		agent, sid, workspace string
	}
	var want []pendingEvent
	seen := map[string]bool{}
	for _, w := range rules {
		if w.Key != m.WatchKey || seen[w.Agent+"\x00"+w.SID] {
			continue
		}
		seen[w.Agent+"\x00"+w.SID] = true
		want = append(want, pendingEvent{w.Agent, w.SID, w.Workspace})
	}
	if len(want) == 0 {
		return
	}
	a.mu.Lock()
	if a.watchSeq == nil {
		a.watchSeq = map[string]uint64{}
	}
	base := a.watchSeq[epoch]
	a.watchSeq[epoch] = base + uint64(len(want))
	cause := a.hookCauseOnLocked(machine)
	a.mu.Unlock()
	for i, w := range want {
		data, _ := json.Marshal(map[string]any{"workspace": w.workspace, "path": m.Path, "count": m.Count, "resync_required": m.ResyncRequired})
		event := eventlog.Event{Name: "file.changed", SourceID: "watch", SourceEpoch: epoch, Seq: base + uint64(i) + 1,
			OccurredAt: now, ObservedAt: now, AgentID: w.agent, OriginSID: w.sid, Data: data}
		if cause != "" {
			event.OriginHook, event.Depth = cause, 1
		}
		event.ID = eventlog.EventID(event.SourceID, event.SourceEpoch, event.Seq)
		if err := store.Commit(a.ctx, eventlog.Transaction{Event: &event}); err != nil {
			a.log.Error("文件观察事件没有写进可靠日志", "node", node, "err", err)
		}
	}
	if a.hookEngine != nil {
		a.hookEngine.Wake()
	}
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
