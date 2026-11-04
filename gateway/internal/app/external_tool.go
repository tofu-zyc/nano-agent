package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"nano-agent/gateway/internal/extagent"
	"nano-agent/gateway/internal/proto"
)

// External 工具（设计 §16.4）：把一件事交给登记了的外部 agent（Claude Code、Codex 这样的命令行程序）。
// 请求由网关构造，交给工作目录所在机器上的内核，作为后台 Job 在工作目录里跑；工具结果是 Job 的回执，
// 模型用 Job 工具看输出、等结束、停下。

const externalPromptMax = 32 << 10

// externalChoice 是这个会话现在能用的一个外部 agent：哪个名字、在哪个挂载的工作目录上。
type externalChoice struct {
	agent string
	mt    *mount
}

// externalChoices 列出 rt 挂的工作目录上开启了、并且登记还在（读入时校验通过）的外部 agent。
// 开关按现在的算，不按这一轮开始时冻结的：主人中途关掉，下一次调用就用不了。调用方持有锁。
func (a *App) externalChoices(rt route) []externalChoice {
	if a.extAgents == nil {
		return nil
	}
	var out []externalChoice
	for i := range rt.mounts {
		w := a.findWorkspace(rt.mounts[i].ws.ID)
		if w == nil {
			continue
		}
		for _, name := range w.External {
			if _, ok := a.extAgents.Get(name); ok {
				out = append(out, externalChoice{agent: name, mt: &rt.mounts[i]})
			}
		}
	}
	return out
}

// externalOffered 是会话 sid 的工具表里该不该有 External：聊天里只有管理员的这一轮能用（和 MCP 一样），
// 只读和不用工具的模式没有。调用方持有锁。
func (a *App) externalOffered(sid string, admin bool) bool {
	if !admin || isDreamSID(sid) || a.isGroupSession(sid) {
		return false
	}
	switch a.workspaceOf(sid).mode {
	case proto.ModeReadonly, proto.ModeNone:
		return false
	}
	return true
}

// externalSpec 是 External 的定义：会话挂的工作目录上没有开启的外部 agent 时不提供。调用方持有锁。
func (a *App) externalSpec(sid string) (proto.ToolSpec, bool) {
	rt, _, _ := a.permOf(sid)
	choices := a.externalChoices(rt)
	if len(choices) == 0 {
		return proto.ToolSpec{}, false
	}
	var names, where []string
	for _, c := range choices {
		if !slices.Contains(names, c.agent) {
			names = append(names, c.agent)
		}
		where = append(where, c.agent+"（"+c.mt.ws.Label()+"）")
	}
	sort.Strings(names)
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":      map[string]any{"type": "string", "enum": names, "description": "外部 agent 的名字"},
			"prompt":    map[string]any{"type": "string", "description": "交给它的要求，最长 32KB。它看不到这段对话：写清楚要做什么、改哪里、做到什么程度算完"},
			"workspace": map[string]any{"type": "string", "description": "在哪个工作目录上运行，写系统消息里列出的名字（机器:路径）。只有一个工作目录开启了这个外部 agent 时可以不写"},
		},
		"required": []string{"name", "prompt"},
	})
	return proto.ToolSpec{Name: "External", Description: "把一件事交给外部 agent（Claude Code、Codex 这样的命令行程序）去做。现在能用：" + strings.Join(where, "、") + "。" +
		"它在工作目录里作为后台任务运行，能读写这个工作目录，也能联网。工具结果是后台任务的回执（job_id）：用 Job 的 wait 等它结束、output 看输出、cancel 停下。" +
		"它运行期间这个工作目录被它占着，Read、Shell 这些会排队等它结束：要等它就用 Job wait，不要反复读文件。它说做完了不算数，改完照常检查。",
		InputSchema: schema}, true
}

// externalSig 是工具快照的判据之一：开关、登记变了，External 的定义跟着变。调用方持有锁。
func (a *App) externalSig(sid string, admin bool) string {
	if !a.externalOffered(sid, admin) {
		return ""
	}
	rt, _, _ := a.permOf(sid)
	var b strings.Builder
	for _, c := range a.externalChoices(rt) {
		b.WriteString(c.agent + "@" + c.mt.ws.Label() + ",")
	}
	return b.String()
}

// externalMount 按参数挑在哪个工作目录上跑：workspace 的写法和内核工具一样（完整的名字、序号、只写机器或只写路径，唯一时）；
// 没写时，只有一个工作目录开启了这个名字才行，否则拒绝，列出可选的。调用方持有锁。
func (a *App) externalMount(rt route, name string, want json.RawMessage) (*mount, error) {
	var enabled []string
	var only *mount
	for _, c := range a.externalChoices(rt) {
		if c.agent == name {
			enabled = append(enabled, c.mt.ws.Label())
			only = c.mt
		}
	}
	if len(enabled) == 0 {
		return nil, fmt.Errorf("这个会话挂的工作目录上没有开启外部 agent %s", name)
	}
	var w string
	if len(want) > 0 && string(want) != "null" {
		var n int
		if json.Unmarshal(want, &n) == nil {
			w = strconv.Itoa(n)
		} else if json.Unmarshal(want, &w) != nil {
			return nil, errors.New("workspace 要写工作目录的名字")
		}
	}
	if w == "" {
		if len(enabled) > 1 {
			return nil, fmt.Errorf("有 %d 个工作目录开启了 %s，用 workspace 参数指明在哪个上运行：%s", len(enabled), name, strings.Join(enabled, "、"))
		}
		return only, nil
	}
	mt := pickMount(rt, w)
	if mt == nil {
		var all []string
		for _, m := range rt.mounts {
			all = append(all, m.ws.Label())
		}
		return nil, fmt.Errorf("没有工作目录 %s。这个会话挂载的工作目录：%s", w, strings.Join(all, "、"))
	}
	if !slices.Contains(enabled, mt.ws.Label()) {
		return nil, fmt.Errorf("工作目录 %s 上没有开启 %s。开启了的：%s", mt.ws.Label(), name, strings.Join(enabled, "、"))
	}
	return mt, nil
}

// pickMount 和内核的 pick_workspace 一样认 workspace：完整的“机器:路径”、序号（从 1 开始），
// 只写机器或只写路径（唯一时）。
func pickMount(rt route, w string) *mount {
	for i := range rt.mounts {
		if rt.mounts[i].ws.Label() == w {
			return &rt.mounts[i]
		}
	}
	if n, err := strconv.Atoi(w); err == nil && n >= 1 && n <= len(rt.mounts) {
		return &rt.mounts[n-1]
	}
	for _, machine := range []bool{true, false} {
		var hit *mount
		hits := 0
		for i := range rt.mounts {
			h, p, ok := strings.Cut(rt.mounts[i].ws.Label(), ":")
			if ok && (machine && h == w || !machine && p == w) {
				hit = &rt.mounts[i]
				hits++
			}
		}
		if hits == 1 {
			return hit
		}
		if hits > 1 {
			return nil
		}
	}
	return nil
}

// externalNode 是在这个工作目录上跑外部 agent 用的内核：要那台机器上有内核、协商了后台 Job；
// SSH 服务器、Windows、网关自己的执行器都不行。调用方持有锁。
func (a *App) externalNode(mt *mount) (string, string) {
	if mt.os == "windows" {
		return "", "Windows 上还不能用外部 agent"
	}
	if a.isSSH(mt.ws.Host) {
		return "", "SSH 服务器上没有内核，跑不了后台任务"
	}
	node := a.kernelOn(mt.ws.Host)
	n := a.nodes[node]
	if node == "" || n == nil {
		return "", "那台机器上没有在线的内核（外部 agent 要由那台机器的内核作为后台任务来跑）"
	}
	if n.info.OS == "windows" {
		return "", "Windows 上还不能用外部 agent"
	}
	if a.jobEvents == nil || n.info.Features["job_manager"] < 1 || n.info.Features["lease_detach"] < 1 || n.info.Features["machine_leases"] < 1 {
		return "", "那台机器的内核没有协商后台任务（要更新内核）"
	}
	return node, ""
}

// shellQuote 按 POSIX 单引号转义一项：外部 agent 的命令不经 shell 解释。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// externalCommand 把登记的命令模板拼成一条 sh 命令：每一项单引号转义，{prompt} 那一项换成这次的要求。
func externalCommand(ag extagent.Agent, prompt string) string {
	parts := make([]string, len(ag.Argv))
	for i, v := range ag.Argv {
		if v == extagent.PromptSlot {
			v = prompt
		}
		parts[i] = shellQuote(v)
	}
	return strings.Join(parts, " ")
}

// externalCall 执行 External。调用方持有锁；等内核回执在 goroutine 里。
func (a *App) externalCall(s *sess, m *proto.Msg, reply func(bool, string)) {
	r := s.run
	var x struct {
		Name      string          `json:"name"`
		Prompt    string          `json:"prompt"`
		Workspace json.RawMessage `json:"workspace"`
	}
	if err := json.Unmarshal(m.Args, &x); err != nil {
		reply(false, "参数格式不对")
		return
	}
	if !a.isAdmin(r.from) {
		reply(false, "只有管理员发起的这一轮能用外部 agent")
		return
	}
	switch {
	case strings.TrimSpace(x.Prompt) == "":
		reply(false, "prompt 不能是空的")
		return
	case len(x.Prompt) > externalPromptMax:
		reply(false, fmt.Sprintf("prompt 最长 32KB（现在 %d 字节）", len(x.Prompt)))
		return
	case strings.ContainsRune(x.Prompt, 0):
		reply(false, "prompt 里不能有 NUL")
		return
	}
	var ag extagent.Agent
	ok := false
	if a.extAgents != nil {
		ag, ok = a.extAgents.Get(x.Name)
	}
	if !ok {
		reply(false, "没有登记这个外部 agent："+x.Name)
		return
	}
	mt, err := a.externalMount(r.rt, x.Name, x.Workspace)
	if err != nil {
		reply(false, err.Error())
		return
	}
	node, why := a.externalNode(mt)
	if why != "" {
		reply(false, fmt.Sprintf("外部 agent 在 %s 上用不了：%s", mt.ws.Label(), why))
		return
	}
	timeout := ag.EffectiveTimeout()
	if limit := a.nodes[node].info.Features["job_timeout_max_seconds"]; limit > 0 && timeout > limit {
		timeout = limit
	}
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
	origin := proto.LeaseSpec{CreatedBy: r.from, Silent: a.branchSilent(s.sid), SID: s.sid, RunID: r.runID, TurnID: r.turnID, CallID: m.ID,
		Agent: a.execAgentID(s.sid), Name: "External", Priority: priority}
	ro := append([]string{}, mt.ws.ExtraRO...)
	body := map[string]any{
		"origin": origin, "tool": "Shell",
		"args": map[string]any{"command": externalCommand(ag, x.Prompt), "timeout": timeout, "execution": "background"},
		// 只给这一个工作目录（工单的是 worktree，再加只读的 .git），不带管理员读整个文件系统的 RO "/"：
		// 登记的目录不能和 dirs、ro 互相包含，执行器会查
		"dirs": []string{mt.ws.Path}, "ro": ro, "rw": []string{}, "write": true,
		// 自己的私有 HOME 和临时目录：外部 agent 往 HOME 里写的东西，同一会话里的 Shell 读不到
		"tmp_key": "ext-" + x.Name + "-" + s.sid, "max_output": min(max(m.MaxOutput, 4096), 1<<20), "watch_stdin": true,
		"sandbox": sandbox, "agent_rw": ag.Dirs,
	}
	if len(ag.Env) > 0 {
		body["env"] = ag.Env // 只在内存里交给启动进程，内核不写进 Job 的记录
	}
	req, _ := json.Marshal(body)
	ctx, cancel := context.WithCancel(a.ctx)
	r.execs[m.ID] = cancel
	a.log.Info("交给外部 agent", "sid", s.sid, "agent", x.Name, "workspace", mt.ws.Label(), "kernel", node, "id", m.ID)
	go func() {
		res := kernelHost{a: a, node: node}.Exec(ctx, append(req, '\n'))
		cancel()
		a.mu.Lock()
		delete(r.execs, m.ID)
		a.mu.Unlock()
		reply(res.OK, res.Content)
	}()
}

// 审批卡片放不下的长 prompt：改发文字（渠道会分段），审批提示里要有完整的 prompt。
const externalCardMax = 2000

func externalApprovalTooLong(name, body string) bool {
	return name == "External" && len([]rune(body)) > externalCardMax
}
