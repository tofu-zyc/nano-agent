package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/host"
)

// 工作目录
//
// 工作目录是“机器 + 路径”：网关所在的本机、SSH 服务器（host 包）或者装了内核的电脑（内核的名字）
// 上的一个目录。网关维护一张列表（data/workspaces.json），拓扑里挂在各自的机器下。每个会话从列表里
// 挂载任意几个（可以在不同的机器上，工具调用时用 workspace 参数指定；一个都不挂就是只读）。
// 标了默认的工作目录，新会话第一次说话时都挂上。工作目录在哪台机器上，决定工具怎么执行（nodes.go 的 route）。

type Workspace struct {
	SandboxMode string `json:"sandbox_mode,omitempty"` // 空跟随 agent，全局 -> agent -> workspace
	ID          string `json:"id"`
	Host        string `json:"host"` // 机器
	Path        string `json:"path"` // 那台机器上的路径，规范形式由那台机器给
	OS          string `json:"os,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

func (w Workspace) Label() string { return w.Host + ":" + w.Path }

func (a *App) workspacesFile() string { return filepath.Join(a.store.Dir(), "workspaces.json") }

// loadWorkspaces 读入工作目录列表；还没有列表时，从旧版本的设置迁移（机器和目录是分开设的）。
func (a *App) loadWorkspaces() {
	b, err := os.ReadFile(a.workspacesFile())
	if err == nil {
		if err := json.Unmarshal(b, &a.wss); err != nil {
			a.log.Error("工作目录列表读不了，先用空的", "file", a.workspacesFile(), "err", err)
		}
	} else {
		a.migrateWorkspaces()
		if err := a.saveWorkspaces(); err != nil {
			a.log.Warn("保存工作目录列表失败", "err", err)
		}
	}
	// 上一个版本每个会话只能挂一个工作目录
	for _, sid := range a.store.List() {
		st := a.store.Settings(sid)
		if !st.WorkspaceSet || st.WorkspacesSet {
			continue
		}
		st.Workspaces, st.WorkspacesSet = nil, true
		if st.Workspace != "" {
			st.Workspaces = []string{st.Workspace}
		}
		st.Workspace, st.WorkspaceSet = "", false
		if err := a.store.SaveSettings(sid, st); err != nil {
			a.log.Warn("迁移会话的工作目录失败", "sid", sid, "err", err)
		}
	}
}

func (a *App) migrateWorkspaces() {
	// 旧版本里本机内核的名字（仓库里 kernel.conf 的 node）：它上面的目录就是本机的目录
	oldLocal := ""
	if f, err := os.Open(filepath.Join(a.cfg.Dir, "kernel.conf")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, v, ok := strings.Cut(sc.Text(), "="); ok && strings.TrimSpace(k) == "node" {
				oldLocal = strings.TrimSpace(v)
			}
		}
		f.Close()
	}
	hostFor := func(node string) string {
		if node == "" || node == oldLocal {
			return host.LocalName
		}
		return node
	}
	defNode := a.cfg.Workspace.Node
	for i, d := range a.cfg.Workspace.Dirs {
		if w := a.addWorkspaceLocked(hostFor(defNode), cleanPath(d), ""); w != nil && i == 0 {
			w.Default = true
		}
	}
	for _, sid := range a.store.List() {
		st := a.store.Settings(sid)
		if !st.DirsSet && st.Node == "" && len(st.RO) == 0 {
			continue // 用默认的：迁移后就是默认工作目录
		}
		node := st.Node
		if node == "" {
			node = defNode
		}
		dirs := st.Dirs
		if !st.DirsSet && node == defNode {
			dirs = a.cfg.Workspace.Dirs
		} else if !st.DirsSet {
			dirs = nil // 旧版本里默认工作目录只属于默认机器
		}
		st.Workspaces, st.WorkspacesSet = nil, true
		for _, d := range dirs {
			if w := a.addWorkspaceLocked(hostFor(node), cleanPath(d), ""); w != nil {
				st.Workspaces = append(st.Workspaces, w.ID)
			}
		}
		st.Node, st.DirsSet, st.Dirs, st.RO = "", false, nil, nil
		if err := a.store.SaveSettings(sid, st); err != nil {
			a.log.Warn("迁移会话的工作目录失败", "sid", sid, "err", err)
		}
	}
	if len(a.wss) > 0 {
		a.log.Info("已把旧的工作目录设置迁移到工作目录列表", "count", len(a.wss))
	}
}

func (a *App) saveWorkspaces() error {
	b, err := json.MarshalIndent(a.wss, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.workspacesFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.workspacesFile())
}

func (a *App) findWorkspace(id string) *Workspace {
	for i := range a.wss {
		if a.wss[i].ID == id {
			return &a.wss[i]
		}
	}
	return nil
}

// defaultWorkspaces 是新会话默认挂载的工作目录
func (a *App) defaultWorkspaces() []*Workspace {
	var out []*Workspace
	for i := range a.wss {
		if a.wss[i].Default {
			out = append(out, &a.wss[i])
		}
	}
	return out
}

// addWorkspaceLocked 把 host:path 加进列表（已经有了就返回原来的），不保存。调用方持有锁。
func (a *App) addWorkspaceLocked(hostName, path, osName string) *Workspace {
	for i := range a.wss {
		if a.wss[i].Host == hostName && a.wss[i].Path == path {
			if osName != "" {
				a.wss[i].OS = osName
			}
			return &a.wss[i]
		}
	}
	var id [4]byte
	rand.Read(id[:])
	a.wss = append(a.wss, Workspace{ID: hex.EncodeToString(id[:]), Host: hostName, Path: path, OS: osName})
	return &a.wss[len(a.wss)-1]
}

// knownHost：机器 name 存在——本机、添加过的 SSH 服务器，或者连上来过的内核
func (a *App) knownHost(name string) bool {
	if name == host.LocalName || a.nodes[name] != nil {
		return true
	}
	if a.hosts != nil {
		if _, _, ok := a.hosts.Host(name); ok {
			return true
		}
	}
	for _, w := range a.wss {
		if w.Host == name {
			return true
		}
	}
	return false
}

// parseWorkspace 把“机器:路径”拆开；只写路径时用 dflt 那台机器。“local”也表示本机。
// 冒号前面的名字必须是一台已知的机器，否则整串都当路径（Windows 的 C:\x 就是路径）。
func (a *App) parseWorkspace(s, dflt string) (hostName, path string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return dflt, ""
	}
	if !strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "~") && !isWindowsAbs(s) {
		if h, p, ok := strings.Cut(s, ":"); ok {
			h, p = strings.TrimSpace(h), strings.TrimSpace(p)
			drive := len(h) == 1 && ((h[0] >= 'A' && h[0] <= 'Z') || (h[0] >= 'a' && h[0] <= 'z'))
			if a.knownHost(h) || strings.EqualFold(h, "local") {
				hostName, s = h, p
			} else if !drive && (strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || isWindowsAbs(p)) {
				// 看起来是“机器:路径”，但机器不存在：留给 checkWorkspace 报错
				hostName, s = h, p
			}
		}
	}
	if hostName == "" {
		hostName = dflt
	}
	if strings.EqualFold(hostName, "local") {
		hostName = host.LocalName
	}
	return hostName, s
}

func isWindowsAbs(p string) bool {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return (p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')
	}
	return strings.HasPrefix(p, `\\`)
}

// checkWorkspace 检查 host:path 能不能当工作目录。调用方持有锁。
// 路径属于那台机器：远程机器的路径网关不解析，只检查机器存在，交给那台机器的执行器。
func (a *App) checkWorkspace(hostName, path string) error {
	if !a.knownHost(hostName) {
		return fmt.Errorf("没有叫 %s 的机器（本机、在拓扑图「接入机器」里添加的 SSH 服务器、或者连上来的内核）", hostName)
	}
	if hostName == host.LocalName {
		if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "~") && !isWindowsAbs(path) {
			return errors.New("路径要写成绝对路径或 ~ 开头，比如 ~/projects/foo")
		}
		if msg := a.checkDir(path); msg != "" {
			return errors.New(msg)
		}
	}
	return nil
}

// WorkspaceInfo 是网页上显示的一个工作目录。
type WorkspaceInfo struct {
	Workspace
	Sessions int  `json:"sessions"` // 挂着它的会话
	Online   bool `json:"online"`   // 它所在的机器能用
}

func (a *App) workspaceInfos() []WorkspaceInfo {
	out := []WorkspaceInfo{}
	count := map[string]int{}
	for _, sid := range a.sessionIDs() {
		for _, w := range a.workspaceOf(sid).wss {
			count[w.ID]++
		}
	}
	for _, w := range a.wss {
		out = append(out, WorkspaceInfo{Workspace: w, Sessions: count[w.ID], Online: a.hostUsable(w.Host)})
	}
	return out
}

func (a *App) Workspaces() []WorkspaceInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workspaceInfos()
}

func (a *App) publishWorkspaces() {
	a.publish(events.Event{T: "workspaces", Data: a.workspaceInfos()})
}

// AddWorkspace 添加一个工作目录（已经有了就返回原来的）。机器能用时先确认目录存在。
func (a *App) AddWorkspace(ctx context.Context, hostName, path string, makeDefault bool, sandboxOverride ...string) (Workspace, error) {
	a.mu.Lock()
	hostName, path = a.parseWorkspace(path, hostName) // 要查已知机器，持锁
	mode := a.cfg.Sandbox.ModeOrDefault()
	override := ""
	if len(sandboxOverride) > 0 {
		override = sandboxOverride[0]
		if override != "" {
			mode = override
		}
	}
	if mode != "auto" && mode != "on" && mode != "off" {
		a.mu.Unlock()
		return Workspace{}, errors.New("执行隔离只能是 auto、on、off")
	}
	err := a.checkWorkspace(hostName, path)
	if mode == "off" && hostName == host.LocalName && (strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~") || isWindowsAbs(path)) {
		err = nil
	}
	usable := a.hostUsable(hostName)
	a.mu.Unlock()
	if err != nil {
		return Workspace{}, err
	}
	osName := ""
	if usable {
		l, err := a.ListDir(ctx, hostName, path)
		if err != nil {
			return Workspace{}, fmt.Errorf("%s 上的 %s 用不了：%w", hostName, path, err)
		}
		// 规范形式由那台机器给：~、相对路径、符号链接、Windows 的盘符大小写都在 ListDir 里定好
		if l.Blocked && mode != "off" {
			return Workspace{}, fmt.Errorf("%s 包含 nano-agent 自己，隔离模式为 %s 时不能用作工作目录", path, mode)
		}
		if l.Path != "" {
			path = l.Path
		}
		osName = l.OS
	}
	if osName == "" {
		a.mu.Lock()
		osName = a.machineOS(hostName)
		a.mu.Unlock()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.addWorkspaceLocked(hostName, path, osName)
	if override != "" {
		w.SandboxMode = override
	}
	if makeDefault {
		w.Default = true
	}
	added := *w
	if err := a.saveWorkspaces(); err != nil {
		return Workspace{}, err
	}
	a.publishWorkspaces()
	return added, nil
}

// normalizeWorkspaces 把机器上的工作目录换成那台机器给的规范形式。
// 机器不在线时只能先按原样保存；它上线后第一次 list 时再换，换完通知网页。
func (a *App) normalizeWorkspaces(hostName string) {
	type item struct{ id, host, path string }
	a.mu.Lock()
	var items []item
	for _, w := range a.wss {
		if hostName != "" && w.Host != hostName {
			continue
		}
		if !a.hostUsable(w.Host) {
			continue
		}
		items = append(items, item{w.ID, w.Host, w.Path})
	}
	a.mu.Unlock()
	if len(items) == 0 {
		return
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	changed := false
	for _, it := range items {
		l, err := a.ListDir(ctx, it.host, it.path)
		if err != nil || l.Path == "" {
			continue
		}
		a.mu.Lock()
		if w := a.findWorkspace(it.id); w != nil && w.Path == it.path && (w.Path != l.Path || (l.OS != "" && w.OS != l.OS)) {
			w.Path = l.Path
			if l.OS != "" {
				w.OS = l.OS
			}
			changed = true
		}
		a.mu.Unlock()
	}
	if !changed {
		return
	}
	a.mu.Lock()
	if err := a.saveWorkspaces(); err != nil && a.log != nil {
		a.log.Warn("保存工作目录列表失败", "err", err)
	}
	a.publishWorkspaces()
	a.mu.Unlock()
}

// RemoveWorkspace 从列表里删掉一个工作目录，挂着它的会话取下它。不动机器上的文件。
func (a *App) RemoveWorkspace(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := -1
	for j := range a.wss {
		if a.wss[j].ID == id {
			i = j
		}
	}
	if i < 0 {
		return errors.New("没有这个工作目录")
	}
	for _, sid := range a.sessionIDs() {
		st := a.store.Settings(sid)
		if !st.WorkspacesSet || !slices.Contains(st.Workspaces, id) {
			continue
		}
		st.Workspaces = slices.DeleteFunc(st.Workspaces, func(x string) bool { return x == id })
		if err := a.store.SaveSettings(sid, st); err != nil {
			return err
		}
		defer a.publishSession(sid)
	}
	a.wss = append(a.wss[:i], a.wss[i+1:]...)
	if err := a.saveWorkspaces(); err != nil {
		return err
	}
	a.publishWorkspaces()
	return nil
}

// SetDefaultWorkspace 设置一个工作目录是不是新会话默认挂载的（可以有好几个）。
func (a *App) SetDefaultWorkspace(id string, on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.findWorkspace(id)
	if w == nil {
		return errors.New("没有这个工作目录")
	}
	w.Default = on
	if err := a.saveWorkspaces(); err != nil {
		return err
	}
	a.publishWorkspaces()
	return nil
}

// setSessionWorkspaces 设置会话挂载的工作目录（按顺序，空表示只读）。调用方持有锁。
// cleanWorkspaces 去重并检查工作目录都在列表里，只验证、不写盘（整份 patch 验证用）。调用方持有锁。
func (a *App) cleanWorkspaces(ids []string) ([]string, error) {
	var clean []string
	for _, id := range ids {
		if a.findWorkspace(id) == nil {
			return nil, errors.New("没有这个工作目录：" + id)
		}
		if !slices.Contains(clean, id) {
			clean = append(clean, id)
		}
	}
	return clean, nil
}

func (a *App) setSessionWorkspaces(sid string, ids []string) error {
	clean, err := a.cleanWorkspaces(ids)
	if err != nil {
		return err
	}
	st := a.store.Settings(sid)
	st.Workspaces, st.WorkspacesSet = clean, true
	if err := a.store.SaveSettings(sid, st); err != nil {
		return err
	}
	a.publishSession(sid)
	a.publishWorkspaces()
	return nil
}

// describeMounts 是会话挂着哪些工作目录（给用户看）
func (a *App) describeMounts(sid string) string {
	w := a.workspaceOf(sid)
	if len(w.wss) == 0 {
		return "这个会话没有挂载工作目录，工具在 HOME 下工作（可以读文件；写工作目录外要提权）。"
	}
	var labels []string
	for _, x := range w.wss {
		l := x.Label()
		if !a.hostUsable(x.Host) {
			l += "（" + x.Host + " 现在用不了）"
		}
		labels = append(labels, l)
	}
	return "这个会话挂载的工作目录：" + strings.Join(labels, "、") + "。"
}

// mountWorkspace 让会话挂上（on）或取下工作目录，返回给用户的说明。调用方持有锁。
func (a *App) mountWorkspace(sid, id string, on bool) (string, error) {
	ids := a.workspaceOf(sid).ids()
	w := a.findWorkspace(id)
	if w == nil {
		return "", errors.New("没有这个工作目录")
	}
	has := slices.Contains(ids, id)
	switch {
	case on && has:
		return w.Label() + " 已经挂着了。" + a.describeMounts(sid), nil
	case !on && !has:
		return w.Label() + " 本来就没有挂。" + a.describeMounts(sid), nil
	case on:
		ids = append(ids, id)
	default:
		ids = slices.DeleteFunc(ids, func(x string) bool { return x == id })
	}
	if err := a.setSessionWorkspaces(sid, ids); err != nil {
		return "", err
	}
	verb := "挂上了 "
	if !on {
		verb = "取下了 "
	}
	return verb + w.Label() + "。" + a.describeMounts(sid), nil
}

// ---- /dir 命令 ----

const dirUsage = "/dir <编号> 挂上；/dir -<编号> 取下；/dir add 机器:路径 添加并挂上（只写路径就是本机）；/dir none 全部取下；" +
	"/dir default <编号> 设为/取消新会话默认挂载；/dir rm <编号> 从列表里删掉"

func (a *App) describeWorkspaces(sid string) string {
	w := a.workspaceOf(sid)
	var b strings.Builder
	b.WriteString(a.describeMounts(sid))
	if len(w.wss) > 0 {
		fmt.Fprintf(&b, "权限模式：%s。", modeNames[w.mode])
	}
	b.WriteString("\n")
	if len(a.wss) == 0 {
		b.WriteString("工作目录列表是空的。\n")
	} else {
		b.WriteString("工作目录列表：\n")
		for i, x := range a.wss {
			var tags []string
			if w.has(x.ID) {
				tags = append(tags, "已挂载")
			}
			if x.Default {
				tags = append(tags, "新会话默认")
			}
			if !a.hostUsable(x.Host) {
				tags = append(tags, x.Host+" 现在用不了")
			}
			fmt.Fprintf(&b, "%d. %s", i+1, x.Label())
			if len(tags) > 0 {
				b.WriteString("（" + strings.Join(tags, "，") + "）")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString(dirUsage)
	return b.String()
}

// pickWorkspace 按列表里的编号找工作目录
func (a *App) pickWorkspace(arg string) (*Workspace, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
	if err != nil || n < 1 || n > len(a.wss) {
		return nil, fmt.Errorf("没有第 %s 个工作目录，用 /dir 查看列表", arg)
	}
	return &a.wss[n-1], nil
}

func (a *App) cmdDir(c in, arg string) {
	sub, rest, _ := strings.Cut(strings.TrimSpace(arg), " ")
	rest = strings.TrimSpace(rest)
	if sub == "" {
		a.answer(c, a.describeWorkspaces(c.s.sid))
		return
	}
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能修改工作目录")
		return
	}
	switch sub {
	case "none":
		err := a.setSessionWorkspaces(c.s.sid, nil)
		a.reply(c, "工作目录全部取下了，这个会话在 HOME 下工作（可以读文件；写工作目录外要提权）。", err)
	case "add":
		if rest == "" {
			a.answer(c, dirUsage)
			return
		}
		hostName, path := a.parseWorkspace(rest, host.LocalName)
		sandbox := a.sandboxModeFor(c.s.sid)
		// 确认目录存在要等机器回复，不能拿着锁等
		go func() {
			ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
			defer cancel()
			w, err := a.AddWorkspace(ctx, hostName, path, false, sandbox)
			a.mu.Lock()
			defer a.mu.Unlock()
			if err != nil {
				a.answer(c, err.Error())
				return
			}
			msg, err := a.mountWorkspace(c.s.sid, w.ID, true)
			a.reply(c, msg, err)
		}()
	case "default", "rm":
		w, err := a.pickWorkspace(rest)
		if err != nil {
			a.answer(c, err.Error())
			return
		}
		id, label, on := w.ID, w.Label(), !w.Default
		// 下面两个操作自己拿锁
		go func() {
			var err error
			var msg string
			switch {
			case sub == "rm":
				err, msg = a.RemoveWorkspace(id), "已从列表里删掉 "+label+"（机器上的文件没有动）。"
			case on:
				err, msg = a.SetDefaultWorkspace(id, true), "新会话会默认挂上 "+label+"。"
			default:
				err, msg = a.SetDefaultWorkspace(id, false), "新会话不再默认挂 "+label+"。"
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			a.reply(c, msg, err)
		}()
	default:
		on := !strings.HasPrefix(sub, "-")
		w, err := a.pickWorkspace(strings.TrimPrefix(strings.TrimPrefix(sub, "-"), "+"))
		if err != nil {
			a.answer(c, err.Error()+"\n"+dirUsage)
			return
		}
		msg, err := a.mountWorkspace(c.s.sid, w.ID, on)
		a.reply(c, msg, err)
	}
}

func (a *App) reply(c in, msg string, err error) {
	if err != nil {
		a.answer(c, err.Error())
		return
	}
	a.answer(c, msg)
}
