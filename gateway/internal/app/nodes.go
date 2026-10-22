package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
)

// 内核和机器
//
// 内核是 agent 本身，装在哪台设备上都行，可以同时连好几个，按名字区分；它们没有状态，上下文、
// 会话、审批都在网关，哪个在线都能接手。工作目录在具体的机器上（workspaces.go）：网关所在的本机、
// SSH 服务器、装了内核的电脑。每一轮开始时按会话的工作目录决定怎么跑（route）：
//
//   - 同机：工作目录所在的机器上有在线的内核（本机的目录 → 和网关同一台机器上的内核；电脑上的目录
//     → 那台电脑的内核），这一轮由它来跑，工具在它自己的机器上执行；
//   - 异机：工作目录在本机（但本机没有在线的内核）或者 SSH 服务器上，由任意在线的内核来跑
//     （本机的优先），工具交给网关执行（exec.go）；
//   - 工作目录所在的机器用不了（电脑不在线、服务器连不上）：找个在线的内核顶上，这一轮只聊天，
//     并在用户消息前说明原因（和环境说明一样，变了才追加，不破坏前缀缓存）。

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
	defer a.mu.Unlock()
	a.publish(events.Event{T: "machines", Data: a.machineList()})
	a.publishWorkspaces()
}

// KernelInfo 是一个在线的内核。
type KernelInfo struct {
	Node     string            `json:"node"`
	Landlock int               `json:"landlock"` // 0 表示没有沙箱
	Tools    []string          `json:"tools"`
	Skills   []proto.SkillSpec `json:"skills"`
	Since    int64             `json:"since"`
	Local    bool              `json:"local"` // 和网关在同一台机器上
}

type node struct {
	info  KernelInfo
	tools []llm.Tool
	seq   int // 第几个连上来的，挑内核时优先最近连上的
}

func (n *node) setTools(tools []proto.ToolSpec, skills []proto.SkillSpec) {
	n.tools = nil
	n.info.Tools = []string{}
	n.info.Skills = skills
	if n.info.Skills == nil {
		n.info.Skills = []proto.SkillSpec{}
	}
	for _, t := range tools {
		params := t.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		n.tools = append(n.tools, llm.Tool{Type: "function", Function: llm.ToolFunction{
			Name: t.Name, Description: t.Description, Parameters: params,
		}})
		n.info.Tools = append(n.info.Tools, t.Name)
	}
}

func (a *App) KernelUp(name string, hello *proto.Msg, local bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := &node{info: KernelInfo{Node: name, Since: time.Now().UnixMilli(), Local: local}}
	n.setTools(hello.Tools, hello.Skills)
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
}

func (a *App) toolsChanged(name string, m *proto.Msg) {
	n := a.nodes[name]
	if n == nil {
		return
	}
	n.setTools(m.Tools, m.Skills)
	a.publish(events.Event{T: "kernel", Name: name, Data: n.info})
	a.log.Info("内核工具已更新", "node", name, "skills", len(m.Skills))
}

func (a *App) KernelDown(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.nodes, name)
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

// route 是一轮怎么跑
type route struct {
	kernel   string // 跑这一轮的内核，空表示没有在线的内核
	gateway  bool   // 异机模式：工具交给网关执行
	landlock int    // 异机模式下执行的那台机器的沙箱版本
	note     string // 工作目录所在的机器用不了时告诉模型的说明，这一轮只聊天
}

func (a *App) route(w workspace) route {
	any := a.preferred()
	if any == "" || w.ws == nil {
		return route{kernel: any}
	}
	name := w.ws.Host
	// 同机：工作目录所在的机器上有内核
	if name == host.LocalName {
		if k := a.localKernel(); k != "" {
			return route{kernel: k}
		}
	} else if a.nodes[name] != nil {
		return route{kernel: name}
	}
	// 异机：网关自己能执行的机器（本机、SSH 服务器）
	if a.hosts != nil {
		if h, info, ok := a.hosts.Host(name); ok {
			if h != nil {
				return route{kernel: any, gateway: true, landlock: info.Landlock}
			}
			what := "服务器 " + name
			if name == host.LocalName {
				what = "本机"
			}
			return route{kernel: any, note: fmt.Sprintf("[系统] 工作目录所在的%s现在用不了（%s），这一轮由 %s 接手：不能读写文件、执行命令，只能聊天。", what, info.Detail, any)}
		}
	}
	return route{kernel: any, note: fmt.Sprintf("[系统] 工作目录所在的机器 %s 现在不在线，这一轮由 %s 接手：不能读写文件、执行命令，只能聊天。", name, any)}
}

// hostUsable：机器 name 上的工作目录现在能用
func (a *App) hostUsable(name string) bool {
	if (name == host.LocalName && a.localKernel() != "") || a.nodes[name] != nil {
		return true
	}
	if a.hosts != nil {
		if h, _, ok := a.hosts.Host(name); ok && h != nil {
			return true
		}
	}
	return false
}

// MachineInfo 是网页和 /node 上显示的一台机器。
type MachineInfo struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`   // local（网关所在的机器）、kernel（装了内核的电脑）、ssh（SSH 服务器）
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
				continue
			}
			others = append(others, MachineInfo{Name: h.Name, Kind: "ssh", Online: h.Ready, Landlock: h.Landlock, State: h.State, Detail: h.Detail, Kernels: []string{}})
		}
	}
	for _, k := range a.kernelList() {
		if k.Local {
			local.Kernels = append(local.Kernels, k.Node)
			if !local.Online {
				local.Online, local.Landlock, local.Detail = true, k.Landlock, ""
			}
			continue
		}
		others = append(others, MachineInfo{Name: k.Node, Kind: "kernel", Online: true, Landlock: k.Landlock, Kernels: []string{k.Node}})
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
	if w.ws == nil {
		b.WriteString("这个会话没有工作目录，只聊天。\n")
	} else {
		rt := a.route(w)
		switch {
		case rt.kernel == "":
			fmt.Fprintf(&b, "这个会话在 %s 上工作，现在没有在线的内核。\n", w.ws.Label())
		case rt.note != "":
			fmt.Fprintf(&b, "这个会话在 %s 上工作，但 %s 现在用不了，只能聊天。\n", w.ws.Label(), w.ws.Host)
		case rt.gateway:
			fmt.Fprintf(&b, "这个会话在 %s 上工作：由内核 %s 跑，工具交给网关在 %s 上执行。\n", w.ws.Label(), rt.kernel, w.ws.Host)
		default:
			fmt.Fprintf(&b, "这个会话在 %s 上工作：由同一台机器上的内核 %s 跑。\n", w.ws.Label(), rt.kernel)
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
	b.WriteString("用 /dir 选择在哪台机器的哪个目录上工作")
	return b.String()
}
