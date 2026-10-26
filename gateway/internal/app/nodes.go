package app

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/skills"
)

// 内核和机器
//
// 内核是 agent 本身，装在哪台设备上都行，可以同时连好几个，按名字区分；它们没有状态，上下文、
// 会话、审批都在网关，哪个在线都能接手。工作目录在具体的机器上（workspaces.go）：网关所在的本机、
// SSH 服务器、装了内核的电脑。一个会话可以同时挂好几个。每一轮开始时决定怎么跑（route）：
//
//   - 先挑跑这一轮的内核：第一个挂载的工作目录所在的机器上有内核就用它，其次是别的挂载的机器上的，
//     再其次本机的、最近连上来的；
//   - 每个工作目录：在这个内核的机器上（同机），内核自己执行；否则（异机）工具交给网关，
//     网关在那台机器上执行（exec.go）：本机和 SSH 服务器网关自己执行，别的电脑请那台电脑的内核代为执行；
//   - 机器用不了（电脑不在线、服务器连不上）的工作目录这一轮不挂，在用户消息前说明
//     （和环境说明一样，变了才追加，不破坏前缀缓存）。

// Hosts 是网关能直接执行工具的机器：本机和 SSH 服务器（host 包）。
type Hosts interface {
	Host(name string) (host.Host, host.Info, bool)
	Infos() []host.Info
}

func (a *App) SetHosts(h Hosts) {
	a.mu.Lock()
	a.hosts = h
	a.mu.Unlock()
}

// HostsChanged：SSH 服务器连上、断开了（host 包通知），网页刷新机器和工作目录
func (a *App) HostsChanged() {
	a.mu.Lock()
	if a.hosts != nil {
		for _, h := range a.hosts.Infos() {
			a.mindMachineChanged(h.Name, h.Ready)
		}
	}
	a.publish(events.Event{T: "machines", Data: a.machineList()})
	a.publishWorkspaces()
	a.mu.Unlock()
	go a.normalizeWorkspaces("")
}

// SetManagedKernel 记下网关自己启动的本机内核的名字（网页上标出来）。
func (a *App) SetManagedKernel(name string) {
	a.mu.Lock()
	a.managedKernel = name
	a.mu.Unlock()
}

// KernelInfo 是一个在线的内核。
type KernelInfo struct {
	Node     string   `json:"node"`
	OS       string   `json:"os,omitempty"`
	Arch     string   `json:"arch,omitempty"`
	Version  string   `json:"version,omitempty"`
	Proto    int      `json:"proto,omitempty"` // 内核说的协议版本（旧内核先放进来、随后自动更新）
	Landlock int      `json:"landlock"`        // 0 表示没有沙箱
	Tools    []string `json:"tools"`
	Since    int64    `json:"since"`
	Local    bool     `json:"local"`             // 和网关在同一台机器上
	Managed  bool     `json:"managed,omitempty"` // 网关自己启动的本机内核
}

type node struct {
	info  KernelInfo
	tools []llm.Tool
	seq   int // 第几个连上来的，挑内核时优先最近连上的
}

func toolOf(t proto.ToolSpec) llm.Tool {
	params := t.InputSchema
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: t.Name, Description: t.Description, Parameters: params}}
}

func (n *node) setTools(tools []proto.ToolSpec) {
	n.tools = nil
	n.info.Tools = []string{}
	for _, t := range tools {
		if t.Name == "Skill" {
			continue // 技能在网关上，Skill 由网关提供
		}
		n.tools = append(n.tools, toolOf(t))
		n.info.Tools = append(n.info.Tools, t.Name)
	}
}

// toolsFor 是给模型的工具：内核的基础工具，加上网关上的技能（Skill）；管理员再加 Gateway /
// GatewayAdmin。按名字排序（顺序固定，方便前缀缓存）。访客没有工具（调用方跳过）。
// 调用方持有锁。
func (a *App) toolsFor(sid string, n *node, readOnly, admin bool) []llm.Tool {
	tools := slices.Clone(n.tools)
	for _, spec := range a.searchTools() {
		tools = append(tools, toolOf(spec))
	}
	if readOnly {
		tools = slices.DeleteFunc(tools, func(t llm.Tool) bool {
			return t.Function.Name != "Read" && t.Function.Name != "Grep" && t.Function.Name != "WebSearch" && t.Function.Name != "WebFetch"
		})
	}
	if spec, ok := a.skillSpec(sid); ok {
		tools = append(tools, toolOf(spec))
	}
	if admin {
		tools = append(tools, toolOf(scheduleTool()))
		for _, spec := range a.gatewayTools() {
			tools = append(tools, toolOf(spec))
		}
	}
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Function.Name < tools[j].Function.Name })
	return tools
}

// ownSkills 是 agent 自己的技能目录 agents/<id>/skills（和全局的合并，同名用自己的）。调用方持有锁。
func (a *App) ownSkills(agentID string) *skills.Registry {
	if a.agentRoot == "" {
		return nil
	}
	if a.agentSkills == nil {
		a.agentSkills = map[string]*skills.Registry{}
	}
	r := a.agentSkills[agentID]
	if r == nil {
		r = skills.New(filepath.Join(a.agentRoot, agentID, "skills"))
		a.agentSkills[agentID] = r
	}
	r.Scan()
	return r
}

// skillSpec 是会话能用的 Skill 工具：全局技能加上所属 agent 自己的。调用方持有锁。
func (a *App) skillSpec(sid string) (proto.ToolSpec, bool) {
	return skills.ToolSpecFor(skills.Merged(a.skills, a.ownSkills(a.agentOf(sid).ID)))
}

// skillText 是保留技能（dream、life）的正文：agent 自己的 > 全局的 > 内置的 fallback。调用方持有锁。
func (a *App) skillText(agentID, name, fallback string) string {
	if t, ok := skills.Body(name, a.ownSkills(agentID), a.skills); ok {
		return t
	}
	return fallback
}

// refreshSkills 重新扫描技能目录，有变化时通知网页。调用方持有锁。
func (a *App) refreshSkills() {
	if a.skills.Scan() {
		a.publish(events.Event{T: "skills", Data: a.skills.List()})
		a.log.Info("技能有变化", "count", len(a.skills.List()), "dir", a.skills.Dir())
	}
}

// Skills 是网关上的技能（网页显示用）。
func (a *App) Skills() []skills.Skill {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshSkills()
	return a.skills.List()
}

func (a *App) KernelUp(name string, hello *proto.Msg, local bool) {
	a.mu.Lock()
	n := &node{info: KernelInfo{Node: name, OS: hello.OS, Arch: hello.Arch, Version: hello.Version,
		Proto: hello.V, Since: time.Now().UnixMilli(), Local: local}}
	n.setTools(hello.Tools)
	n.info.Managed = a.managedKernel != "" && name == a.managedKernel
	if hello.Sandbox != nil {
		n.info.Landlock = hello.Sandbox.Landlock
	}
	a.nodeSeq++
	n.seq = a.nodeSeq
	a.nodes[name] = n
	if local && (a.localNode == "" || a.nodes[a.localNode] == nil) {
		a.localNode = name
	}
	// 和网关同一台机器上的内核，它的目录就是本机的目录（旧数据里可能按内核的名字记的）
	if local {
		changed := false
		for i := range a.wss {
			if a.wss[i].Host == name {
				a.wss[i].Host, changed = host.LocalName, true
			}
		}
		if changed {
			if err := a.saveWorkspaces(); err != nil {
				a.log.Warn("保存工作目录列表失败", "err", err)
			}
		}
	}
	a.publish(events.Event{T: "kernel", Name: name, Data: n.info})
	a.publish(events.Event{T: "machines", Data: a.machineList()})
	a.log.Info("内核报告的工具", "node", name, "local", local, "tools", strings.Join(n.info.Tools, ","), "landlock", n.info.Landlock)
	if n.info.Landlock == 0 {
		a.log.Warn("这个内核所在的机器没有沙箱（Landlock），自动模式下的 Shell 也需要审批", "node", name)
	}
	machine := a.machineOf(name)
	a.mindMachineChanged(machine, true)
	mg := a.mcp
	a.mu.Unlock()
	// 挂在这台机器上的 MCP 服务马上去连（不等下一次维护）
	if mg != nil {
		mg.Wake(name, 30*time.Second)
	}
	// 机器上线后把离线时先按原样保存的工作目录换成那台机器给的规范形式
	go a.normalizeWorkspaces(machine)
	a.maybeUpdateKernel(name, hello)
}

func (a *App) toolsChanged(name string, m *proto.Msg) {
	n := a.nodes[name]
	if n == nil {
		return
	}
	n.setTools(m.Tools)
	a.publish(events.Event{T: "kernel", Name: name, Data: n.info})
}

func (a *App) KernelDown(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	machine := a.machineOf(name)
	delete(a.nodes, name)
	if machine != host.LocalName || a.localKernel() == "" {
		a.mindMachineChanged(machine, false)
	}
	if a.mcp != nil {
		if p := a.mcp.Pipes(); p != nil {
			p.DropNode(name) // 它上面起的 MCP 进程跟着没了
		}
		a.mcp.DropMachine(name) // 连接也作废：等它连回来再重新起（不然重连后第一次调用会失败）
	}
	a.hostGone(name)
	a.publish(events.Event{T: "kernel", Name: name})
	a.publish(events.Event{T: "machines", Data: a.machineList()})
	for _, s := range a.sessions {
		if s.run == nil || s.run.node != name {
			continue
		}
		note := fmt.Sprintf("（%s 断开了，这一轮已中止）", name)
		if text, ok := a.afterRun(s); ok {
			note += "\n" + text
		}
		a.endRun(s, note)
		a.startRun(s) // 排着队的消息交给别的内核
	}
}

// localKernel 是和网关在同一台机器上的在线内核
func (a *App) localKernel() string {
	if a.nodes[a.localNode] != nil {
		return a.localNode
	}
	best := ""
	for name, n := range a.nodes {
		if n.info.Local && (best == "" || n.seq > a.nodes[best].seq) {
			best = name
		}
	}
	return best
}

// preferred 挑一个在线的内核来跑不挑机器的一轮：和网关在同一台机器上的优先，其次是最近连上来的
func (a *App) preferred() string {
	if k := a.localKernel(); k != "" {
		return k
	}
	best := ""
	for name, n := range a.nodes {
		if best == "" || n.seq > a.nodes[best].seq {
			best = name
		}
	}
	return best
}

// machineOf 是内核所在的机器：和网关在同一台机器上的内核都算本机
func (a *App) machineOf(kernel string) string {
	if n := a.nodes[kernel]; n != nil && n.info.Local {
		return host.LocalName
	}
	return kernel
}

// kernelOn 是机器 name 上在线的内核
func (a *App) kernelOn(name string) string {
	if name == host.LocalName {
		return a.localKernel()
	}
	if a.nodes[name] != nil {
		return name
	}
	return ""
}

func (a *App) isSSH(name string) bool {
	if name == host.LocalName || a.hosts == nil {
		return false
	}
	_, info, ok := a.hosts.Host(name)
	return ok && info.Kind == "ssh"
}

// hostFor 是网关在机器 name 上执行工具用的 Host：本机和 SSH 服务器网关自己执行，装了内核的电脑请它的内核
// 代为执行。用不了时返回 nil 和原因。调用方持有锁。
func (a *App) hostFor(name string) (host.Host, int, string) {
	if a.hosts != nil {
		if h, info, ok := a.hosts.Host(name); ok {
			if h != nil {
				return h, info.Landlock, ""
			}
			if k := a.kernelOn(name); k == "" {
				d := info.Detail
				if d == "" {
					d = "连不上"
				}
				return nil, 0, d
			}
		}
	}
	if k := a.kernelOn(name); k != "" {
		return kernelHost{a: a, node: k}, a.nodes[k].info.Landlock, ""
	}
	return nil, 0, "不在线"
}

// mount 是这一轮挂上的一个工作目录
type mount struct {
	ws       *Workspace
	local    bool // 在跑这一轮的内核的机器上，内核自己执行
	landlock int  // 异机时执行的那台机器的沙箱版本
	os       string
}

// route 是一轮怎么跑
type route struct {
	kernel string   // 跑这一轮的内核，空表示没有在线的内核
	mounts []mount  // 能用的工作目录
	down   []string // 用不了的工作目录和原因
}

func (a *App) route(w workspace) route {
	rt := route{}
	for _, x := range w.wss {
		if k := a.kernelOn(x.Host); k != "" {
			rt.kernel = k
			break
		}
	}
	if rt.kernel == "" {
		rt.kernel = a.preferred()
	}
	if rt.kernel == "" {
		return rt
	}
	here := a.machineOf(rt.kernel)
	for _, x := range w.wss {
		if x.Host == here {
			rt.mounts = append(rt.mounts, mount{ws: x, local: true, landlock: a.nodes[rt.kernel].info.Landlock, os: a.machineOS(x.Host)})
			continue
		}
		if _, landlock, why := a.hostFor(x.Host); why != "" {
			rt.down = append(rt.down, fmt.Sprintf("%s：%s %s", x.Label(), x.Host, why))
		} else {
			rt.mounts = append(rt.mounts, mount{ws: x, landlock: landlock, os: a.machineOS(x.Host)})
		}
	}
	return rt
}

// find 按给模型看的名字（机器:路径）找这一轮挂上的工作目录
func (rt route) find(name string) *mount {
	for i := range rt.mounts {
		if rt.mounts[i].ws.Label() == name {
			return &rt.mounts[i]
		}
	}
	return nil
}

// spec 是交给内核的工作目录：同一台机器上挂的别的工作目录也能读写（方便在它们之间拷文件）。
// 管理员可以读整个文件系统（RO "/"），但只能写工作目录；访客仍然只能读工作目录和技能缓存。
func (rt route) spec(m mount, readAll bool) proto.WorkspaceSpec {
	dirs := []string{m.ws.Path}
	for _, o := range rt.mounts {
		if o.ws.Host == m.ws.Host && o.ws.ID != m.ws.ID {
			dirs = append(dirs, o.ws.Path)
		}
	}
	ro := []string{skills.CacheDir}
	if readAll {
		ro = []string{"/"}
	}
	return proto.WorkspaceSpec{Name: m.ws.Label(), Dirs: dirs, RO: ro, Local: m.local, Landlock: m.landlock,
		OS: m.os}
}

// hostUsable：机器 name 上的工作目录现在能用（有内核，或者网关能在上面执行）
func (a *App) hostUsable(name string) bool {
	h, _, _ := a.hostFor(name)
	return h != nil
}

// MachineInfo 是网页和 /node 上显示的一台机器。
type MachineInfo struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"` // local（网关所在的机器）、kernel（装了内核的电脑）、ssh（SSH 服务器）
	OS       string   `json:"os,omitempty"`
	Online   bool     `json:"online"` // 上面的工作目录能用
	Landlock int      `json:"landlock"`
	State    string   `json:"state,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Kernels  []string `json:"kernels"` // 这台机器上在线的内核
}

func (a *App) machineList() []MachineInfo {
	local := MachineInfo{Name: host.LocalName, Kind: "local", Kernels: []string{}}
	var others []MachineInfo
	if a.hosts != nil {
		for _, h := range a.hosts.Infos() {
			if h.Kind == "local" {
				local.Online, local.Landlock, local.State, local.Detail = h.Ready, h.Landlock, h.State, h.Detail
				local.OS = h.OS
				continue
			}
			others = append(others, MachineInfo{Name: h.Name, Kind: "ssh", OS: h.OS, Online: h.Ready, Landlock: h.Landlock, State: h.State, Detail: h.Detail, Kernels: []string{}})
		}
	}
	for _, k := range a.kernelList() {
		if k.Local {
			local.Kernels = append(local.Kernels, k.Node)
			if !local.Online {
				local.Online, local.Landlock, local.Detail = true, k.Landlock, ""
			}
			if local.OS == "" {
				local.OS = k.OS
			}
			continue
		}
		others = append(others, MachineInfo{Name: k.Node, Kind: "kernel", OS: k.OS, Online: true, Landlock: k.Landlock, Kernels: []string{k.Node}})
	}
	seen := map[string]bool{}
	for _, m := range others {
		seen[m.Name] = true
	}
	// 工作目录引用了、但现在不在线的电脑
	for _, w := range a.wss {
		if w.Host != host.LocalName && !seen[w.Host] {
			seen[w.Host] = true
			others = append(others, MachineInfo{Name: w.Host, Kind: "kernel", Kernels: []string{}, Detail: "内核不在线"})
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].Name < others[j].Name })
	return append([]MachineInfo{local}, others...)
}

func (a *App) kernelList() []KernelInfo {
	out := []KernelInfo{}
	for _, n := range a.nodes {
		out = append(out, n.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

func (a *App) cmdNode(c in, _ string) {
	a.answer(c, a.describeMachines(c.s.sid))
}

func (a *App) describeMachines(sid string) string {
	var b strings.Builder
	w := a.workspaceOf(sid)
	rt := a.route(w)
	switch {
	case rt.kernel == "":
		b.WriteString("现在没有在线的内核，没法处理消息。\n")
	case len(w.wss) == 0:
		fmt.Fprintf(&b, "这个会话没有挂载工作目录，只读，由内核 %s 跑。\n", rt.kernel)
	default:
		fmt.Fprintf(&b, "这个会话由内核 %s 跑，挂载的工作目录：\n", rt.kernel)
		for _, m := range rt.mounts {
			how := "工具交给网关在 " + m.ws.Host + " 上执行"
			if m.local {
				how = "同一台机器，内核自己执行"
			}
			fmt.Fprintf(&b, "· %s（%s）\n", m.ws.Label(), how)
		}
		for _, d := range rt.down {
			fmt.Fprintf(&b, "· %s（用不了）\n", d)
		}
	}
	b.WriteString("机器：\n")
	for _, m := range a.machineList() {
		var tags []string
		switch m.Kind {
		case "local":
			tags = append(tags, "网关所在的机器")
		case "ssh":
			tags = append(tags, "SSH 服务器")
		default:
			tags = append(tags, "装了内核的电脑")
		}
		if !m.Online {
			d := m.Detail
			if d == "" {
				d = "用不了"
			}
			tags = append(tags, d)
		} else if m.Landlock == 0 {
			tags = append(tags, "没有沙箱")
		}
		if len(m.Kernels) > 0 {
			tags = append(tags, "内核 "+strings.Join(m.Kernels, "、"))
		}
		fmt.Fprintf(&b, "· %s（%s）\n", m.Name, strings.Join(tags, "，"))
	}
	b.WriteString("用 /dir 挂载或取下工作目录")
	return b.String()
}
