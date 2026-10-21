package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
	dirs, ro, rw []string
	mode         string
}

func (a *App) workspaceOf(sid string) workspace {
	st := a.store.Settings(sid)
	w := workspace{dirs: a.cfg.Workspace.Dirs, mode: a.cfg.Workspace.Mode}
	if st.DirsSet {
		w.dirs = st.Dirs
	}
	if st.Mode != "" {
		w.mode = st.Mode
	}
	w.ro = append(slices.Clone(a.cfg.Workspace.ExtraRO), st.RO...)
	w.rw = slices.Clone(a.cfg.Workspace.ExtraRW)
	return w
}

// isAdmin：命令行用户始终是管理员（能在这台机器上开终端的人本来就有全部权限）。
func (a *App) isAdmin(who string) bool {
	return strings.HasPrefix(who, "cli:") || slices.Contains(a.cfg.Access.Admins, who)
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

// envNote 描述当前环境，追加在用户消息前面。没设置过工作目录的会话（纯聊天）不打扰。
func envNote(w workspace, sessionRO []string, prev string) string {
	if len(w.dirs) == 0 {
		if prev == "" {
			return ""
		}
		return "[系统] 现在没有工作目录了，文件和命令类工具暂时用不了。"
	}
	var b strings.Builder
	b.WriteString("[系统] 工作目录：")
	b.WriteString(w.dirs[0])
	b.WriteString("（默认，相对路径和命令都从这里开始）")
	for _, d := range w.dirs[1:] {
		b.WriteString("、")
		b.WriteString(d)
	}
	if len(sessionRO) > 0 {
		b.WriteString("；只读目录：")
		b.WriteString(strings.Join(sessionRO, "、"))
	}
	fmt.Fprintf(&b, "。权限模式：%s，%s。", modeNames[w.mode], modeDesc[w.mode])
	return b.String()
}

// ---- 命令 ----

const dirUsage = "用法：/dir 查看；/dir add <路径> [ro] 添加（ro 表示只读）；/dir rm <路径> 移除；/dir default <路径> 设为默认"

func (a *App) cmdDir(s *sess, who, arg, reply string) {
	sub, rest, _ := strings.Cut(arg, " ")
	rest = strings.TrimSpace(rest)
	if sub == "" {
		a.say(s, a.describeWorkspace(s.sid), reply)
		return
	}
	if !a.isAdmin(who) {
		a.say(s, "只有管理员能修改工作目录", reply)
		return
	}
	st := a.store.Settings(s.sid)
	if !st.DirsSet {
		st.Dirs, st.DirsSet = slices.Clone(a.cfg.Workspace.Dirs), true
	}
	path, flag, _ := strings.Cut(rest, " ")
	path = cleanPath(path)
	var note string
	switch sub {
	case "add":
		if path == "" || (flag != "" && flag != "ro") {
			a.say(s, dirUsage, reply)
			return
		}
		if msg := a.checkDir(path); msg != "" {
			a.say(s, msg, reply)
			return
		}
		if slices.Contains(st.Dirs, path) || slices.Contains(st.RO, path) {
			a.say(s, path+" 已经在列表里了", reply)
			return
		}
		if flag == "ro" {
			st.RO = append(st.RO, path)
		} else {
			st.Dirs = append(st.Dirs, path)
		}
		if _, err := os.Stat(expandHome(path)); err != nil {
			note = "\n（注意：在网关所在的机器上找不到这个目录。内核在别的机器上的话可以忽略）"
		}
	case "rm":
		n := len(st.Dirs) + len(st.RO)
		st.Dirs = slices.DeleteFunc(st.Dirs, func(d string) bool { return d == path })
		st.RO = slices.DeleteFunc(st.RO, func(d string) bool { return d == path })
		if len(st.Dirs)+len(st.RO) == n {
			a.say(s, path+" 不在工作目录列表里", reply)
			return
		}
	case "default":
		if path == "" {
			a.say(s, dirUsage, reply)
			return
		}
		if msg := a.checkDir(path); msg != "" {
			a.say(s, msg, reply)
			return
		}
		st.Dirs = append([]string{path}, slices.DeleteFunc(st.Dirs, func(d string) bool { return d == path })...)
		st.RO = slices.DeleteFunc(st.RO, func(d string) bool { return d == path })
	default:
		a.say(s, dirUsage, reply)
		return
	}
	if err := a.store.SaveSettings(s.sid, st); err != nil {
		a.say(s, "保存设置失败："+err.Error(), reply)
		return
	}
	a.say(s, a.describeWorkspace(s.sid)+note, reply)
}

func (a *App) cmdMode(s *sess, who, arg, reply string) {
	if arg == "" {
		w := a.workspaceOf(s.sid)
		a.say(s, fmt.Sprintf("权限模式：%s（%s）\n可选：auto 自动、ask 审批、readonly 只读，用 /mode <模式> 切换",
			modeNames[w.mode], modeDesc[w.mode]), reply)
		return
	}
	if !a.isAdmin(who) {
		a.say(s, "只有管理员能切换权限模式", reply)
		return
	}
	if _, ok := modeNames[arg]; !ok {
		a.say(s, "模式只能是 auto、ask 或 readonly", reply)
		return
	}
	st := a.store.Settings(s.sid)
	st.Mode = arg
	if err := a.store.SaveSettings(s.sid, st); err != nil {
		a.say(s, "保存设置失败："+err.Error(), reply)
		return
	}
	a.say(s, fmt.Sprintf("权限模式已切换为%s：%s。下一条消息开始生效", modeNames[arg], modeDesc[arg]), reply)
}

func (a *App) describeWorkspace(sid string) string {
	w := a.workspaceOf(sid)
	st := a.store.Settings(sid)
	var b strings.Builder
	if len(w.dirs) == 0 && len(st.RO) == 0 {
		b.WriteString("还没有设置工作目录，文件和命令类工具暂时用不了。\n")
	} else {
		b.WriteString("工作目录（第一个是默认目录）：\n")
		for i, d := range w.dirs {
			fmt.Fprintf(&b, "%d. %s\n", i+1, d)
		}
		for i, d := range st.RO {
			fmt.Fprintf(&b, "%d. %s（只读）\n", len(w.dirs)+i+1, d)
		}
	}
	fmt.Fprintf(&b, "权限模式：%s（%s）\n", modeNames[w.mode], modeDesc[w.mode])
	b.WriteString(dirUsage)
	return b.String()
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
