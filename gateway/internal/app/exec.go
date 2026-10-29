package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/dispatch"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/skills"
)

// 内核交给网关的工具调用（exec）：
//   - 工作目录不在跑这一轮的内核的机器上：网关在那台机器上执行（本机、SSH 服务器网关自己执行，
//     装了内核的电脑请它的内核代为执行），结果交回内核（exec_result）。执行的约束（工作目录、只读、
//     能不能写）由网关按这一轮开始时的设置生成，不用内核给的；
//   - Skill：技能在网关上，网关读说明，把附带文件放到挂载的每台机器上。

// execTool 处理内核的 exec。调用方持有锁。
func (a *App) execTool(node string, s *sess, m *proto.Msg) {
	if (m.Name == "Agenda" || m.Name == "Idea" || m.Name == "Mind") && !a.isAdmin(s.run.from) {
		a.hub.Send(node, proto.Msg{T: proto.TExecResult, SID: s.sid, ID: m.ID, RunID: m.RunID, TurnID: m.TurnID, OK: proto.Bool(false), Content: "只有管理员和 agent 自己的自主活动能修改日程、想法和心智状态"})
		return
	}
	// Gateway state/delivery operations join the same per-run arbiter as native
	// processes. Without this, four state calls plus four node calls could run
	// concurrently after the node acknowledged its queued calls.
	switch m.Name {
	case "Session", "Schedule", "Message", "SendFile", "FollowUp", "Agenda", "Idea", "Mind", "Diary", knowledgeLabel, "Skill", "Gateway", "GatewayAdmin":
		r := s.run
		if r.execs == nil {
			r.execs = map[string]context.CancelFunc{}
		}
		lease := a.gatewayLeaseRequest(s, m, "gateway:tools", nil, []string{"state:" + a.execAgentID(s.sid)})
		ctx, cancel := context.WithCancel(a.ctx)
		r.execs[m.ID] = cancel
		a.execQueued(node, m)
		copied := *m
		copied.Args = append(json.RawMessage(nil), m.Args...)
		go func() {
			release, err := a.acquireExecution(ctx, lease)
			if err == nil {
				a.executionLeaseStarted(lease)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if err != nil {
				delete(r.execs, m.ID)
				a.hub.Send(node, proto.Msg{T: proto.TExecResult, SID: m.SID, ID: m.ID, RunID: m.RunID, TurnID: m.TurnID, OK: proto.Bool(false), Content: err.Error()})
				cancel()
				return
			}
			if s.run != r || ctx.Err() != nil {
				go release(false)
				cancel()
				return
			}
			var once sync.Once
			a.execToolNow(node, s, &copied, func() {
				once.Do(func() {
					cancel()
					go func() {
						a.mu.Lock()
						delete(r.execs, m.ID)
						a.mu.Unlock()
						release(false)
					}()
				})
			})
		}()
		return
	}
	a.execToolNow(node, s, m, nil)
}

func (a *App) execToolNow(node string, s *sess, m *proto.Msg, completed func()) {
	r := s.run
	// Synchronous Gateway operations start here. Remote execution, network and
	// MCP report started only after their own queue/approval and real admission.
	switch m.Name {
	case "Read", "Write", "Edit", "Grep", "Fetch", "Shell", "WebSearch", "WebFetch":
		a.execQueued(node, m)
	default:
		if _, _, external := mcp.SplitExposed(m.Name); external {
			a.execQueued(node, m)
		} else {
			a.execStarted(node, m)
		}
	}
	reply := func(ok bool, content string) {
		if completed != nil {
			completed()
		}
		a.hub.Send(node, proto.Msg{T: proto.TExecResult, SID: s.sid, ID: m.ID, RunID: m.RunID, TurnID: m.TurnID,
			OK: proto.Bool(ok), Content: content})
	}
	if m.Name == "WebSearch" || m.Name == "WebFetch" {
		a.webTool(s, m, reply)
		return
	}
	if m.Name == "Job" {
		a.jobToolCall(s, m, reply)
		return
	}
	if m.Name == "Session" {
		// 会话分叉在网关执行：不碰文件、不需要工作目录
		text, ok := a.handleSessionTool(s, m)
		reply(ok, text)
		return
	}
	if m.Name == "Schedule" {
		var args ScheduleRequest
		if err := json.Unmarshal(m.Args, &args); err != nil {
			reply(false, "定时任务参数格式不正确")
			return
		}
		result, err := a.scheduleAction(s.sid, r.from, args)
		if err != nil {
			reply(false, err.Error())
		} else {
			b, _ := json.Marshal(result)
			reply(true, string(b))
		}
		return
	}
	if m.Name == "Message" {
		// 管理员让它发，或者拟人型在内心活动里自己想找人（内心活动里有自己的规矩，见 messageChat）
		if !a.isAdmin(r.from) && !isLifeSID(s.sid) {
			reply(false, "只有管理员能用 Message 给群和别的 agent 发消息")
			return
		}
		raw := string(m.Args)
		if raw == "" {
			raw = "{}"
		}
		go a.messageTool(s.sid, raw, reply)
		return
	}
	if m.Name == "SendFile" {
		a.sendFileCall(s, m, reply)
		return
	}
	if m.Name == "FollowUp" {
		out, err := a.followUpCall(s.sid, m.Args)
		if err != nil {
			reply(false, err.Error())
		} else {
			reply(true, out)
		}
		return
	}
	if m.Name == "Agenda" {
		out, err := a.agendaCall(s.sid, m.Args)
		if err != nil {
			reply(false, err.Error())
		} else {
			reply(true, out)
		}
		return
	}
	if m.Name == "Idea" {
		out, err := a.ideaCall(s.sid, m.Args)
		if err != nil {
			reply(false, err.Error())
		} else {
			reply(true, out)
		}
		return
	}
	if m.Name == "Mind" {
		out, err := a.mindCall(s.sid, m.Args)
		if err != nil {
			reply(false, err.Error())
		} else {
			reply(true, out)
		}
		return
	}
	if m.Name == "Diary" {
		out, err := a.diaryCall(s.sid, m.Args)
		if err != nil {
			reply(false, err.Error())
		} else {
			reply(true, out)
		}
		return
	}
	if m.Name == knowledgeLabel {
		out, err := a.knowledgeCall(s.sid, r.from, m.Args)
		if err != nil {
			reply(false, err.Error())
		} else {
			reply(true, out)
		}
		return
	}
	if m.Name == "Skill" {
		a.skillTool(r, s.sid, m, reply)
		return
	}
	if m.Name == "Gateway" || m.Name == "GatewayAdmin" {
		if !a.isAdmin(r.from) {
			reply(false, "只有管理员能用网关管理工具")
			return
		}
		raw := string(m.Args)
		if raw == "" {
			raw = "{}"
		}
		if m.Name == "Gateway" {
			go a.gatewayQuery(s.sid, raw, reply)
		} else {
			go a.gatewayAdminCall(s.sid, r.from, raw, reply)
		}
		return
	}
	// MCP 工具：内核不认识 <服务>__<工具> 这种名字，会转回网关执行。
	// 服务没配置/没连上时给明确结果，别掉到“工作目录”分支去。
	if server, _, ok := mcp.SplitExposed(m.Name); ok {
		if a.mcp == nil {
			reply(false, "没有这个 MCP 服务："+server)
			return
		}
		if _, exists := a.mcp.Server(server); !exists {
			reply(false, "没有这个 MCP 服务："+server)
			return
		}
		if !a.isAdmin(r.from) {
			reply(false, "只有管理员能用 MCP 工具")
			return
		}
		a.mcpCall(s, m, reply)
		return
	}
	mt := r.rt.find(m.Workspace)
	if mt == nil {
		reply(false, "这一轮没有挂载工作目录 "+m.Workspace)
		return
	}
	if mt.local {
		reply(false, "工作目录 "+m.Workspace+" 在跑这一轮的内核的机器上，应该由内核自己执行")
		return
	}
	h, _, why := a.hostFor(mt.ws.Host)
	if h == nil {
		reply(false, fmt.Sprintf("工作目录 %s 所在的 %s 现在用不了：%s", mt.ws.Label(), mt.ws.Host, why))
		return
	}
	spec := r.rt.spec(*mt, a.isAdmin(r.from))
	write := r.write
	rw := spec.RW
	if m.Elevated {
		if !a.isAdmin(r.from) {
			reply(false, "当前身份不能提权写工作目录外")
			return
		}
		write = true
		rw = append(append([]string{}, rw...), "/")
	}
	args := m.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	// Host execution takes the frozen host policy, never a model argument or
	// a coordinator-supplied override.
	sandbox := mt.sandboxMode
	if sandbox == "" {
		sandbox = r.sandboxMode
	}
	if sandbox == "" {
		sandbox = "auto"
	}
	priority := 0
	if r.inner || a.isBranchSID(s.sid) {
		priority = 10
	}
	origin := proto.LeaseSpec{CreatedBy: r.from, Silent: a.branchSilent(s.sid), SID: s.sid, RunID: r.runID, TurnID: r.turnID, CallID: m.ID, Agent: a.execAgentID(s.sid), Name: m.Name, Priority: priority}
	req, _ := json.Marshal(map[string]any{
		"origin": origin, "tool": m.Name, "args": args, "dirs": spec.Dirs, "ro": spec.RO, "rw": rw, "write": write, "elevated": m.Elevated,
		"tmp_key": s.sid, "max_output": min(max(m.MaxOutput, 4096), 1<<20), "watch_stdin": true,
		"sandbox": sandbox, // 在别的机器上执行时，那台机器按这个模式准备执行器
	})
	timeout := time.Duration(m.TimeoutMs) * time.Millisecond
	if timeout <= 0 || timeout > time.Hour {
		timeout = 10 * time.Minute
	}
	// 内核自己也计时，超时会发 exec_cancel；这里多留一点余量兜底
	var lease dispatch.Request
	ctx, cancel := context.WithCancel(a.ctx)
	ctx = host.WithStarted(ctx, func() {
		if ctx.Err() != nil {
			return
		}
		timer := time.AfterFunc(timeout, cancel)
		context.AfterFunc(ctx, func() { timer.Stop() })
		if lease.ID != "" {
			a.executionLeaseStarted(lease)
		}
		a.mu.Lock()
		if s.run == r {
			a.execStarted(node, m)
		}
		a.mu.Unlock()
	})
	if _, managedByNode := h.(kernelHost); !managedByNode {
		// A direct/SSH executor cannot attest canonical paths before launching.
		// Protect the machine file domain, shared by every native C file call,
		// rather than trusting unverified path aliases supplied over this route.
		lease = a.gatewayLeaseRequest(s, m, mt.ws.Host, nil, []string{"machine:files"})
		lease.Windows = mt.os == "windows"
	}
	r.execs[m.ID] = cancel
	a.log.Info("在 "+mt.ws.Host+" 上执行", "sid", s.sid, "tool", m.Name, "id", m.ID, "kernel", node)
	go func() {
		var res host.Result
		if lease.ID != "" {
			release, err := a.acquireExecution(ctx, lease)
			if err != nil {
				res = host.Result{Content: err.Error()}
			} else {
				res = h.Exec(ctx, append(req, '\n'))
				release(res.Lost)
			}
		} else {
			res = h.Exec(ctx, append(req, '\n'))
		}
		cancel()
		a.mu.Lock()
		delete(r.execs, m.ID)
		a.mu.Unlock()
		if res.Lost {
			a.log.Warn("执行时连不上机器", "host", mt.ws.Host, "detail", res.Content)
		}
		state := res.Sandbox
		if state == nil {
			state = &proto.Sandbox{Requested: sandbox, Mode: sandbox, Effective: "unknown", Reason: "执行器未回报实际隔离状态"}
		} else if state.Mode == "" {
			state.Mode = sandbox
		}
		if state.Requested == "" {
			state.Requested = sandbox
		}
		a.hub.Send(node, proto.Msg{T: proto.TExecResult, SID: s.sid, ID: m.ID, RunID: m.RunID, TurnID: m.TurnID, OK: proto.Bool(res.OK), Content: res.Content, Sandbox: state})
	}()
}

// skillTool 加载技能，将配套文件放到各执行机器的独立、按内容分版本的只读缓存。
// 调用方持有锁。
func (a *App) skillTool(r *run, sid string, m *proto.Msg, reply func(bool, string)) {
	var args struct {
		Name string `json:"name"`
	}
	json.Unmarshal(m.Args, &args)
	text, files, hash, ok := skills.LoadAny(args.Name, a.ownSkills(a.agentOf(sid).ID), a.skills)
	if !ok {
		reply(false, text)
		return
	}
	targets := map[string]host.Host{}
	for _, mt := range r.rt.mounts {
		if _, done := targets[mt.ws.Host]; done {
			continue
		}
		var h host.Host
		if mt.local && mt.ws.Host != host.LocalName {
			h = kernelHost{a: a, node: r.node} // 内核所在的电脑
		} else {
			h, _, _ = a.hostFor(mt.ws.Host)
		}
		if h != nil {
			targets[mt.ws.Host] = h
		}
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	r.execs[m.ID] = cancel
	a.log.Info("加载技能", "sid", sid, "skill", args.Name, "machines", len(targets))
	go func() {
		var mu sync.Mutex
		var wg sync.WaitGroup
		var failed, placed []string
		for name, h := range targets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				path, err := h.PutFiles(ctx, skills.CacheDir+"/"+args.Name+"-"+hash, files)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failed = append(failed, name+"："+err.Error())
				} else if path != "" {
					placed = append(placed, "- "+name+"："+path)
				} else {
					failed = append(failed, name+"：没有返回实际缓存目录")
				}
			}()
		}
		wg.Wait()
		cancel()
		a.mu.Lock()
		delete(r.execs, m.ID)
		a.mu.Unlock()
		sort.Strings(placed)
		if len(placed) > 0 {
			text = strings.Replace(text, "\n", "\n技能目录（只读；按执行机器选择完整路径）：\n"+strings.Join(placed, "\n")+"\n", 1)
		} else if len(files) > 1 && len(failed) == 0 {
			failed = append(failed, "当前没有可用的执行机器，配套文件尚未部署")
		}
		if len(failed) > 0 {
			text += "\n\n（附带文件没能放到这些机器上：" + strings.Join(failed, "；") + "）"
		}
		reply(true, text)
	}()
}
