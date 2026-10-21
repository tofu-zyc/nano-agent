package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
)

// 节点
//
// 每个内核是一个节点（Windows 电脑、手机、服务器……），按名字区分，可以同时连好几个。内核没有状态：
// 上下文、会话、审批都在网关，哪个内核在线都能接手。但文件在具体的机器上，所以每个会话有自己
// 工作的机器：会话设置里的 Node，没设就用配置里的默认机器 workspace.node，再没有就用和网关在
// 同一台机器上的内核。工作目录是那台机器上的路径。
//
// 每一轮开始时挑一个内核来跑：会话的机器在线就用它；不在线就找一个在线的内核顶上（替补），
// 这一轮只聊天、不给工具，并在用户消息前说明原因（和环境说明一样，变了才追加，不破坏前缀缓存）。

// KernelInfo 是一个在线的内核。
type KernelInfo struct {
	Node     string            `json:"node"`
	Landlock int               `json:"landlock"` // 0 表示没有沙箱
	Tools    []string          `json:"tools"`
	Skills   []proto.SkillSpec `json:"skills"`
	Since    int64             `json:"since"`
	Local    bool              `json:"local"`   // 和网关在同一台机器上
	Remotes  []proto.Remote    `json:"remotes"` // 它通过 SSH 能用的服务器
}

type node struct {
	info  KernelInfo
	tools []llm.Tool
	seq   int // 第几个连上来的，挑替补时优先最近连上的
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
	n.info.Remotes = hello.Remotes
	if n.info.Remotes == nil {
		n.info.Remotes = []proto.Remote{}
	}
	a.nodeSeq++
	n.seq = a.nodeSeq
	a.nodes[name] = n
	// 默认机器是和网关在同一台机器上的内核；已经有一个在线时，后来的不顶替它
	if local && (a.localNode == "" || a.nodes[a.localNode] == nil) {
		a.localNode = name
	}
	a.publish(events.Event{T: "kernel", Name: name, Data: n.info})
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

// remotesChanged：内核报告它能用的服务器变了（准备好了、连不上了）
func (a *App) remotesChanged(name string, list []proto.Remote) {
	n := a.nodes[name]
	if n == nil {
		return
	}
	if list == nil {
		list = []proto.Remote{}
	}
	n.info.Remotes = list
	for _, r := range list {
		if r.OK {
			a.log.Info("服务器可用", "server", r.Name, "via", name)
		} else {
			a.log.Warn("服务器不可用", "server", r.Name, "via", name, "detail", r.Detail)
		}
	}
	a.publish(events.Event{T: "kernel", Name: name, Data: n.info})
}

// via 找一个能在机器 name 上执行工具的在线内核：name 本身就是内核，或者某个内核能 SSH 到它。
// 多个内核都能到时，和网关同一台机器的优先，其次是最近连上来的。
func (a *App) via(name string) string {
	if name == "" {
		return ""
	}
	if a.nodes[name] != nil {
		return name
	}
	best := ""
	for kn, n := range a.nodes {
		for _, r := range n.info.Remotes {
			if r.Name != name || !r.OK {
				continue
			}
			if b := a.nodes[best]; best == "" || (n.info.Local && !b.info.Local) || (n.info.Local == b.info.Local && n.seq > b.seq) {
				best = kn
			}
		}
	}
	return best
}

// remoteOf 是某个内核报告的服务器 name（不管能不能用），用来说明为什么用不了
func (a *App) remoteOf(name string) *proto.Remote {
	for _, n := range a.nodes {
		for i := range n.info.Remotes {
			if n.info.Remotes[i].Name == name {
				return &n.info.Remotes[i]
			}
		}
	}
	return nil
}

// defaultNode 是没有单独设置机器的会话工作的地方
func (a *App) defaultNode() string {
	if n := a.cfg.Workspace.Node; n != "" {
		return n
	}
	return a.localNode
}

// pick 挑一个内核来跑会话的这一轮。tools 为 false 表示会话的机器不在线、由替补顶上，这一轮不能用工具；
// note 是这时要告诉模型的说明。没有任何内核在线时 node 为空。调用方持有锁。
func (a *App) pick(w workspace) (node string, tools bool, note string) {
	if k := a.via(w.node); k != "" {
		return k, true, ""
	}
	sub := a.substitute()
	if sub == "" {
		return "", false, ""
	}
	if w.node == "" || len(w.dirs) == 0 {
		return sub, true, "" // 不挑机器（或者本来就只聊天）：谁在线谁来
	}
	if r := a.remoteOf(w.node); r != nil {
		return sub, false, fmt.Sprintf("[系统] 工作目录所在的服务器 %s 现在用不了（%s），这一轮由 %s 接手：不能读写文件、执行命令，只能聊天。", w.node, r.Detail, sub)
	}
	return sub, false, fmt.Sprintf("[系统] 工作目录所在的机器 %s 现在不在线，这一轮由 %s 接手：不能读写文件、执行命令，只能聊天。", w.node, sub)
}

// substitute 挑一个在线的内核当替补：和网关在同一台机器上的优先，其次是最近连上来的
func (a *App) substitute() string {
	best := ""
	for name, n := range a.nodes {
		if best == "" {
			best = name
			continue
		}
		b := a.nodes[best]
		if n.info.Local != b.info.Local {
			if n.info.Local {
				best = name
			}
			continue
		}
		if n.seq > b.seq {
			best = name
		}
	}
	return best
}

func (a *App) kernelList() []KernelInfo {
	out := []KernelInfo{}
	for _, n := range a.nodes {
		out = append(out, n.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

func validNodeName(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == ':' || r == '/' {
			return false
		}
	}
	return true
}

// setNode 改会话工作的机器（空表示用默认的）。换了机器，原来单独设置的工作目录是旧机器上的路径，
// 一并清掉（回到默认机器时又会用回默认工作目录）。返回给用户的说明。调用方持有锁。
func (a *App) setNode(sid, name string) (string, error) {
	if name != "" && !validNodeName(name) {
		return "", errors.New("机器的名字不对：不能有空白和冒号，最长 64 个字")
	}
	old := a.workspaceOf(sid).node
	st := a.store.Settings(sid)
	st.Node = name
	cur := name
	if cur == "" {
		cur = a.defaultNode()
	}
	cleared := false
	if cur != old && st.DirsSet && len(st.Dirs)+len(st.RO) > 0 {
		st.Dirs, st.RO, st.DirsSet, cleared = nil, nil, false, true
	}
	if err := a.store.SaveSettings(sid, st); err != nil {
		return "", err
	}
	a.publishSession(sid)
	where := cur
	if where == "" {
		where = "默认的机器"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "这个会话改在 %s 上工作了", where)
	if cur != "" && a.via(cur) == "" {
		b.WriteString("（它现在不在线，上线之前只能聊天）")
	}
	b.WriteString("。")
	if cleared {
		fmt.Fprintf(&b, "原来的工作目录是 %s 上的路径，已经清掉了，用 /dir add 添加这台机器上的目录。", old)
	}
	return b.String(), nil
}

const nodeUsage = "用 /node <名字> 切换这个会话工作的机器，/node default 用默认的机器"

func (a *App) cmdNode(c in, arg string) {
	if arg == "" {
		a.answer(c, a.describeNodes(c.s.sid))
		return
	}
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能切换机器")
		return
	}
	name := arg
	if arg == "default" {
		name = ""
	}
	msg, err := a.setNode(c.s.sid, name)
	if err != nil {
		a.answer(c, err.Error())
		return
	}
	a.answer(c, msg)
}

func (a *App) describeNodes(sid string) string {
	w := a.workspaceOf(sid)
	var b strings.Builder
	switch {
	case w.node == "":
		b.WriteString("这个会话没有指定机器，哪个内核在线就用哪个。")
	case a.nodes[w.node] != nil:
		fmt.Fprintf(&b, "这个会话在 %s 上工作（在线）。", w.node)
	case a.via(w.node) != "":
		fmt.Fprintf(&b, "这个会话在服务器 %s 上工作（经由 %s 的内核，通过 SSH）。", w.node, a.via(w.node))
	default:
		fmt.Fprintf(&b, "这个会话在 %s 上工作，但它现在不在线，由别的内核顶上时只能聊天。", w.node)
	}
	var online []string
	for _, k := range a.kernelList() {
		s := k.Node
		var tags []string
		if k.Local {
			tags = append(tags, "和网关同一台机器")
		}
		if k.Landlock == 0 {
			tags = append(tags, "没有沙箱")
		}
		if len(tags) > 0 {
			s += "（" + strings.Join(tags, "，") + "）"
		}
		online = append(online, s)
	}
	if len(online) == 0 {
		b.WriteString("\n现在没有在线的内核。")
	} else {
		b.WriteString("\n在线的机器：" + strings.Join(online, "、"))
	}
	var servers []string
	seen := map[string]bool{}
	for _, k := range a.kernelList() {
		for _, r := range k.Remotes {
			if seen[r.Name] {
				continue
			}
			seen[r.Name] = true
			if v := a.via(r.Name); v != "" {
				servers = append(servers, fmt.Sprintf("%s（经由 %s）", r.Name, v))
			} else {
				servers = append(servers, fmt.Sprintf("%s（用不了：%s）", r.Name, r.Detail))
			}
		}
	}
	if len(servers) > 0 {
		b.WriteString("\n能用 SSH 执行的服务器：" + strings.Join(servers, "、"))
	}
	b.WriteString("\n" + nodeUsage)
	return b.String()
}
