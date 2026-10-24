package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
)

// 工作目录和权限模式：权限模式的默认值在配置里，每个会话可以用 /mode 单独改；
// 一个会话可以同时挂好几个工作目录（workspaces.go），可以在不同的机器上，工具调用时用 workspace 参数指定。
// 改动在下一条消息开始的那一轮生效。

var modeNames = map[string]string{
	proto.ModeAuto:     "自动",
	proto.ModeAsk:      "审批",
	proto.ModeReadonly: "只读",
}

var modeDesc = map[string]string{
	proto.ModeAuto:     "工作目录内的读写和命令直接执行，目录外的一律不允许",
	proto.ModeAsk:      "读文件和搜索直接执行，写文件和执行命令要管理员批准",
	proto.ModeReadonly: "只能读文件和搜索",
}

// workspace 是一个会话实际生效的设置。
type workspace struct {
	wss  []*Workspace // 挂载的工作目录（workspaces.go），空表示只读
	mode string
}

func (a *App) workspaceOf(sid string) workspace {
	st := a.store.Settings(sid)
	// 权限模式：会话自己的 > agent 的 > 全局默认
	w := workspace{mode: a.agentModeDefault(sid)}
	if st.Mode != "" {
		w.mode = st.Mode
	}
	if st.WorkspacesSet {
		for _, id := range st.Workspaces {
			if x := a.findWorkspace(id); x != nil {
				w.wss = append(w.wss, x)
			}
		}
	} else if !st.Group {
		// 群聊默认不挂工作目录（管理员可以在会话节点里单独给它挂）
		w.wss = a.defaultWorkspaces()
	}
	return w
}

func (w workspace) ids() []string {
	ids := make([]string, len(w.wss))
	for i, x := range w.wss {
		ids[i] = x.ID
	}
	return ids
}

func (w workspace) has(id string) bool {
	for _, x := range w.wss {
		if x.ID == id {
			return true
		}
	}
	return false
}

// isAdmin：命令行用户（能在这台机器上开终端的人本来就有全部权限）和登录了网页的用户始终是管理员。
func (a *App) isAdmin(who string) bool {
	return strings.HasPrefix(who, "cli:") || strings.HasPrefix(who, "web:") || slices.Contains(a.cfg.Access.Admins, who)
}

// runMode 是这一轮实际使用的模式：管理员用会话的设置，其他人用 access.guest。
func (a *App) runMode(w workspace, from string) string {
	if a.isAdmin(from) {
		return w.mode
	}
	if a.cfg.Access.Guest == proto.ModeReadonly {
		return proto.ModeReadonly
	}
	return proto.ModeNone
}

// noToolsNote 告诉访客模型这个会话没有工具。不说的话，模型（尤其是人格里写了“需要时使用工具”）
// 会在回复里编造工具调用的文本。
const noToolsNote = "[系统] 这个会话没有挂载工作目录，你没有任何可用的工具，不能读写文件、执行命令，只能聊天。" +
	"用户需要这些的话，请他让管理员挂一个工作目录（/dir，或者在网页里设置）。"

// homeNote 是没有工作目录的管理员：在 HOME 下工作，读不限，只能写临时目录；写外面要提权。
const homeNote = "[系统] 这个会话没有挂载工作目录，工作目录是 HOME：可以读这台机器上当前用户能读的文件，Shell 在这里运行，" +
	"但只能写临时目录和缓存。要写工作目录外的文件或让命令写外面，给 Write / Edit / Shell 加 elevated: true，会请用户批准。"

// machineDesc 是给模型看的机器说明（加上那台机器的系统和 shell）
func (a *App) machineDesc(name string) string {
	base := "电脑"
	switch {
	case name == host.LocalName:
		base = "网关所在的机器"
	case a.isSSH(name):
		base = "SSH 服务器"
	}
	osName := a.machineOS(name)
	if osName == "" {
		return base
	}
	return base + "，" + osLabel(osName) + "，命令用 " + shellName(osName)
}

// machineOS 返回机器所在的系统：本机 / SSH 服务器 / 装了内核的电脑；不知道时返回空。
func (a *App) machineOS(name string) string {
	if name == host.LocalName && a.hosts != nil {
		for _, h := range a.hosts.Infos() {
			if h.Kind == "local" && h.OS != "" {
				return h.OS
			}
		}
	}
	if n := a.nodes[name]; n != nil && n.info.OS != "" {
		return n.info.OS
	}
	if name == host.LocalName {
		for _, n := range a.nodes {
			if n.info.Local && n.info.OS != "" {
				return n.info.OS
			}
		}
	}
	if a.hosts != nil {
		if _, info, ok := a.hosts.Host(name); ok && info.OS != "" {
			return info.OS
		}
	}
	for _, w := range a.wss {
		if w.Host == name && w.OS != "" {
			return w.OS
		}
	}
	return ""
}

func osLabel(osName string) string {
	switch osName {
	case "windows":
		return "Windows"
	case "macos":
		return "macOS"
	case "linux":
		return "Linux"
	}
	return osName
}

func shellName(osName string) string {
	if osName == "windows" {
		return "PowerShell"
	}
	return "/bin/sh"
}

// envNote 描述这一轮的环境，追加在用户消息前面（变了才追加，不改系统提示词）。
func (a *App) envNote(w workspace, rt route, admin bool) string {
	if len(w.wss) == 0 {
		if admin {
			return homeNote
		}
		return noToolsNote
	}
	if len(rt.mounts) == 0 {
		if admin && a.localKernel() != "" {
			return fmt.Sprintf("[系统] 挂载的工作目录所在的机器现在都用不了（%s），这一轮由 %s 接手，在 HOME 下工作：可以读文件、运行命令，只能写临时目录；写工作目录外要 elevated。",
				strings.Join(rt.down, "；"), rt.kernel)
		}
		if admin {
			return fmt.Sprintf("[系统] 挂载的工作目录所在的机器现在都用不了（%s），这一轮由 %s 接手：没有可用的工作目录，只能聊天。",
				strings.Join(rt.down, "；"), rt.kernel)
		}
		return fmt.Sprintf("[系统] 挂载的工作目录所在的机器现在都用不了（%s），这一轮由 %s 接手：不能读写文件、执行命令，只能聊天。",
			strings.Join(rt.down, "；"), rt.kernel)
	}
	var b strings.Builder
	if len(rt.mounts) == 1 && len(rt.down) == 0 {
		x := rt.mounts[0].ws
		fmt.Fprintf(&b, "[系统] 在 %s 上工作（%s）。相对路径和命令都从这个目录开始。", x.Label(), a.machineDesc(x.Host))
	} else {
		b.WriteString("[系统] 这个会话挂载了下面的工作目录，可以在不同的机器上。工具调用时用 workspace 参数写明在哪个上执行（写这里列出的名字），" +
			"相对路径和命令从那个目录开始：")
		for _, m := range rt.mounts {
			fmt.Fprintf(&b, "\n- %s（%s）", m.ws.Label(), a.machineDesc(m.ws.Host))
		}
		for _, d := range rt.down {
			fmt.Fprintf(&b, "\n- %s，这一轮用不了", d)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "权限模式：%s，%s。", modeNames[w.mode], modeDesc[w.mode])
	return b.String()
}

// ---- 命令 ----

func (a *App) cmdMode(c in, arg string) {
	if arg == "" {
		w := a.workspaceOf(c.s.sid)
		a.answer(c, fmt.Sprintf("权限模式：%s（%s）\n可选：auto 自动、ask 审批、readonly 只读，用 /mode <模式> 切换",
			modeNames[w.mode], modeDesc[w.mode]))
		return
	}
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能切换权限模式")
		return
	}
	if _, ok := modeNames[arg]; !ok {
		a.answer(c, "模式只能是 auto、ask 或 readonly")
		return
	}
	st := a.store.Settings(c.s.sid)
	st.Mode = arg
	if err := a.store.SaveSettings(c.s.sid, st); err != nil {
		a.answer(c, "保存设置失败："+err.Error())
		return
	}
	a.answer(c, fmt.Sprintf("权限模式已切换为%s：%s。下一条消息开始生效", modeNames[arg], modeDesc[arg]))
}

// checkDir 拒绝会暴露网关自己的目录：配置里有 API key，数据目录里有所有会话的聊天记录。
func (a *App) checkDir(path string) string {
	if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "~") {
		return "工作目录要写成绝对路径或 ~ 开头，比如 /dir add ~/projects/foo"
	}
	full := expandHome(path)
	if real, err := filepath.EvalSymlinks(full); err == nil {
		full = real
	}
	for _, mine := range []string{a.cfg.Dir, a.store.Dir()} {
		if mine == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(mine); err == nil {
			mine = real
		}
		if mine == full || strings.HasPrefix(mine, strings.TrimSuffix(full, "/")+"/") {
			return "不能把 " + path + " 设为工作目录：它包含 nano-agent 网关自己的配置或数据（里面有 API key 和聊天记录）"
		}
	}
	return ""
}

func cleanPath(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		rest := filepath.Clean(strings.TrimPrefix(p, "~"))
		if rest == "/" || rest == "." {
			return "~"
		}
		return "~" + rest
	}
	return filepath.Clean(p)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + strings.TrimPrefix(p, "~")
		}
	}
	return p
}

// homeSpec 是没有工作目录的管理员用的 HOME 模式：dirs 为空，相对路径和 Shell 用 HOME；
// ro 是整台机器，只有提权时才会加 rw "/"（Windows 执行器把 "/" 当成所有盘）。
func homeSpec() proto.WorkspaceSpec {
	return proto.WorkspaceSpec{Name: "HOME", RO: []string{"/"}, Home: true, Local: true}
}

// ---- 审批提示 ----

// describeCall 把工具调用写成人能一眼看懂的样子
func describeCall(name string, raw json.RawMessage) string {
	title, body := callCard(name, raw)
	return title + "：\n" + body
}

// callCard 是审批时显示的标题（做什么、在哪个工作目录上）和内容（命令、文件）
func callCard(name string, raw json.RawMessage) (title, body string) {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return "工具 " + name, prettyArgs(raw)
	}
	str := func(k string) string { v, _ := args[k].(string); return v }
	elevated, _ := args["elevated"].(bool)
	where := ""
	if w := str("workspace"); w != "" {
		where = "（在 " + w + " 上）"
	}
	prefix := "提权 · "
	if !elevated && name != "GatewayAdmin" {
		prefix = ""
	}
	if name == "GatewayAdmin" {
		return prefix + "管理网关：" + str("action"), maskedGatewayArgs(raw)
	}
	switch name {
	case "Shell":
		return prefix + "执行命令" + where, clip(str("command"), 1500)
	case "Write":
		return fmt.Sprintf("%s写入文件%s：%s（%d 字节）", prefix, where, str("path"), len(str("content"))), clip(str("content"), 300)
	case "Edit":
		return prefix + "修改文件" + where + "：" + str("path"), fmt.Sprintf("原文：\n%s\n改为：\n%s", clip(str("old_string"), 400), clip(str("new_string"), 400))
	}
	return prefix + "工具 " + name + where, prettyArgs(raw)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// maskedGatewayArgs 给审批卡片看：密钥打星，其他字段照常显示。
func maskedGatewayArgs(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return prettyArgs(raw)
	}
	if key, _ := m["key"].(string); m["action"] == "settings_set" && m["value"] != nil {
		if f, ok := config.FieldByKey(key); ok && f.Type == "secret" {
			m["value"] = "***"
		}
	}
	for _, k := range []string{"api_key", "password"} {
		if _, ok := m[k]; ok {
			m[k] = "***"
		}
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return strings.TrimSpace(string(b))
}
