package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
)

// 工作目录和权限模式：配置里是默认值，每个会话可以用 /dir、/mode 单独改。
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
	ws           *Workspace // 工作目录（workspaces.go），nil 表示只聊天
	dirs, ro, rw []string   // 交给执行器的：工作目录、额外的只读和读写路径
	mode         string
}

func (a *App) workspaceOf(sid string) workspace {
	st := a.store.Settings(sid)
	w := workspace{mode: a.cfg.Workspace.Mode}
	if st.Mode != "" {
		w.mode = st.Mode
	}
	if st.WorkspaceSet {
		w.ws = a.findWorkspace(st.Workspace)
	} else {
		w.ws = a.defaultWorkspace()
	}
	if w.ws != nil {
		w.dirs = []string{w.ws.Path}
		w.ro = slices.Clone(a.cfg.Workspace.ExtraRO)
		w.rw = slices.Clone(a.cfg.Workspace.ExtraRW)
	}
	return w
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

// noToolsNote 告诉模型这个会话没有工具。不说的话，模型（尤其是人格里写了“需要时使用工具”）
// 会在回复里编造工具调用的文本。
const noToolsNote = "[系统] 这个会话没有工作目录，你没有任何可用的工具，不能读写文件、执行命令，只能聊天。" +
	"用户需要这些的话，请他让管理员选一个工作目录（/dir，或者在网页里设置）。"

// envNote 描述当前环境，追加在用户消息前面（变了才追加，不改系统提示词）。
func envNote(w workspace) string {
	if w.ws == nil {
		return noToolsNote
	}
	where := w.ws.Host
	if where == host.LocalName {
		where = "本机（网关所在的机器）"
	}
	return fmt.Sprintf("[系统] 在 %s 上工作。工作目录：%s（相对路径和命令都从这里开始）。权限模式：%s，%s。",
		where, w.ws.Path, modeNames[w.mode], modeDesc[w.mode])
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

// ---- 审批提示 ----

// describeCall 把工具调用写成人能一眼看懂的样子
func describeCall(name string, raw json.RawMessage) string {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return fmt.Sprintf("工具：%s\n参数：%s", name, prettyArgs(raw))
	}
	str := func(k string) string { v, _ := args[k].(string); return v }
	switch name {
	case "Shell":
		return "执行命令：\n" + clip(str("command"), 1500)
	case "Write":
		return fmt.Sprintf("写入文件：%s（%d 字节）\n%s", str("path"), len(str("content")), clip(str("content"), 300))
	case "Edit":
		return fmt.Sprintf("修改文件：%s\n原文：\n%s\n改为：\n%s", str("path"), clip(str("old_string"), 400), clip(str("new_string"), 400))
	}
	return fmt.Sprintf("工具：%s\n参数：%s", name, prettyArgs(raw))
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
