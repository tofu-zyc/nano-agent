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
// 选一个（不选就只聊天）；可以标一个为新会话的默认，新会话第一次说话时用它。
// 工作目录在哪台机器上，决定这一轮怎么跑（nodes.go 的 route）。

type Workspace struct {
	ID      string `json:"id"`
	Host    string `json:"host"` // 机器
	Path    string `json:"path"` // 那台机器上的路径，绝对路径或者 ~ 开头
	Default bool   `json:"default,omitempty"`
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
		return
	}
	a.migrateWorkspaces()
	if err := a.saveWorkspaces(); err != nil {
		a.log.Warn("保存工作目录列表失败", "err", err)
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
		if w := a.addWorkspaceLocked(hostFor(defNode), cleanPath(d)); w != nil && i == 0 {
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
		st.Workspace, st.WorkspaceSet = "", true
		if len(dirs) > 0 {
			if w := a.addWorkspaceLocked(hostFor(node), cleanPath(dirs[0])); w != nil {
				st.Workspace = w.ID
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

func (a *App) defaultWorkspace() *Workspace {
	for i := range a.wss {
		if a.wss[i].Default {
			return &a.wss[i]
		}
	}
	return nil
}

// addWorkspaceLocked 把 host:path 加进列表（已经有了就返回原来的），不保存。调用方持有锁。
func (a *App) addWorkspaceLocked(hostName, path string) *Workspace {
	for i := range a.wss {
		if a.wss[i].Host == hostName && a.wss[i].Path == path {
			return &a.wss[i]
		}
	}
	var id [4]byte
	rand.Read(id[:])
	a.wss = append(a.wss, Workspace{ID: hex.EncodeToString(id[:]), Host: hostName, Path: path})
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
func parseWorkspace(s, dflt string) (hostName, path string) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "~") {
		if h, p, ok := strings.Cut(s, ":"); ok {
			hostName, s = strings.TrimSpace(h), strings.TrimSpace(p)
		}
	}
	if hostName == "" {
		hostName = dflt
	}
	if strings.EqualFold(hostName, "local") {
		hostName = host.LocalName
	}
	return hostName, cleanPath(s)
}

// checkWorkspace 检查 host:path 能不能当工作目录。调用方持有锁。
func (a *App) checkWorkspace(hostName, path string) error {
	if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "~") {
		return errors.New("路径要写成绝对路径或 ~ 开头，比如 ~/projects/foo")
	}
	if !a.knownHost(hostName) {
		return fmt.Errorf("没有叫 %s 的机器（本机、在「设备」里添加的 SSH 服务器、或者连上来的内核）", hostName)
	}
	if hostName == host.LocalName {
		if msg := a.checkDir(path); msg != "" {
			return errors.New(msg)
		}
	}
	return nil
}

// WorkspaceInfo 是网页上显示的一个工作目录。
type WorkspaceInfo struct {
	Workspace
	Sessions int  `json:"sessions"` // 在用它的会话
	Online   bool `json:"online"`   // 它所在的机器能用
}

func (a *App) workspaceInfos() []WorkspaceInfo {
	out := []WorkspaceInfo{}
	count := map[string]int{}
	for _, sid := range a.sessionIDs() {
		if w := a.workspaceOf(sid).ws; w != nil {
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
func (a *App) AddWorkspace(ctx context.Context, hostName, path string, makeDefault bool) (Workspace, error) {
	hostName, path = parseWorkspace(path, hostName)
	a.mu.Lock()
	err := a.checkWorkspace(hostName, path)
	usable := a.hostUsable(hostName)
	a.mu.Unlock()
	if err != nil {
		return Workspace{}, err
	}
	if usable {
		l, err := a.ListDir(ctx, hostName, path)
		if err != nil {
			return Workspace{}, fmt.Errorf("%s 上的 %s 用不了：%w", hostName, path, err)
		}
		if l.Blocked {
			return Workspace{}, fmt.Errorf("%s 包含 nano-agent 自己，不能当工作目录", path)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.addWorkspaceLocked(hostName, path)
	if makeDefault {
		for i := range a.wss {
			a.wss[i].Default = false
		}
		w.Default = true
	}
	added := *w
	if err := a.saveWorkspaces(); err != nil {
		return Workspace{}, err
	}
	a.publishWorkspaces()
	return added, nil
}

// RemoveWorkspace 从列表里删掉一个工作目录，在用它的会话改成只聊天。不动机器上的文件。
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
		if w := a.workspaceOf(sid).ws; w != nil && w.ID == id {
			st := a.store.Settings(sid)
			st.Workspace, st.WorkspaceSet = "", true
			if err := a.store.SaveSettings(sid, st); err != nil {
				return err
			}
			defer a.publishSession(sid)
		}
	}
	a.wss = append(a.wss[:i], a.wss[i+1:]...)
	if err := a.saveWorkspaces(); err != nil {
		return err
	}
	a.publishWorkspaces()
	return nil
}

// SetDefaultWorkspace 设新会话的默认工作目录，id 为空表示新会话只聊天。
func (a *App) SetDefaultWorkspace(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id != "" && a.findWorkspace(id) == nil {
		return errors.New("没有这个工作目录")
	}
	for i := range a.wss {
		a.wss[i].Default = a.wss[i].ID == id
	}
	if err := a.saveWorkspaces(); err != nil {
		return err
	}
	a.publishWorkspaces()
	return nil
}

// setSessionWorkspace 让会话在工作目录 id 上工作（空表示只聊天），返回给用户的说明。调用方持有锁。
func (a *App) setSessionWorkspace(sid, id string) (string, error) {
	var w *Workspace
	if id != "" {
		if w = a.findWorkspace(id); w == nil {
			return "", errors.New("没有这个工作目录")
		}
	}
	st := a.store.Settings(sid)
	st.Workspace, st.WorkspaceSet = id, true
	if err := a.store.SaveSettings(sid, st); err != nil {
		return "", err
	}
	a.publishSession(sid)
	a.publishWorkspaces()
	if w == nil {
		return "这个会话改成只聊天了（没有工作目录，不能读写文件、执行命令）。", nil
	}
	msg := "这个会话改在 " + w.Label() + " 上工作了。"
	if !a.hostUsable(w.Host) {
		msg += "（" + w.Host + " 现在用不了，能用之前只能聊天）"
	}
	return msg, nil
}

// ---- /dir 命令 ----

const dirUsage = "/dir <编号> 切换；/dir add 机器:路径 添加并切换（只写路径就是当前的机器）；/dir none 只聊天；" +
	"/dir default <编号> 设为新会话的默认；/dir rm <编号> 从列表里删掉"

func (a *App) describeWorkspaces(sid string) string {
	w := a.workspaceOf(sid)
	var b strings.Builder
	if w.ws == nil {
		b.WriteString("这个会话没有工作目录，只聊天。\n")
	} else {
		fmt.Fprintf(&b, "这个会话在 %s 上工作，权限模式：%s。\n", w.ws.Label(), modeNames[w.mode])
	}
	if len(a.wss) == 0 {
		b.WriteString("工作目录列表是空的。\n")
	} else {
		b.WriteString("工作目录列表：\n")
		for i, x := range a.wss {
			var tags []string
			if w.ws != nil && w.ws.ID == x.ID {
				tags = append(tags, "当前")
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
		msg, err := a.setSessionWorkspace(c.s.sid, "")
		a.reply(c, msg, err)
	case "add":
		if rest == "" {
			a.answer(c, dirUsage)
			return
		}
		cur := host.LocalName
		if w := a.workspaceOf(c.s.sid).ws; w != nil {
			cur = w.Host
		}
		hostName, path := parseWorkspace(rest, cur)
		// 确认目录存在要等机器回复，不能拿着锁等
		go func() {
			ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
			defer cancel()
			w, err := a.AddWorkspace(ctx, hostName, path, false)
			a.mu.Lock()
			defer a.mu.Unlock()
			if err != nil {
				a.answer(c, err.Error())
				return
			}
			msg, err := a.setSessionWorkspace(c.s.sid, w.ID)
			a.reply(c, msg, err)
		}()
	case "default", "rm":
		w, err := a.pickWorkspace(rest)
		if err != nil {
			a.answer(c, err.Error())
			return
		}
		id, label := w.ID, w.Label()
		// 下面两个操作自己拿锁
		go func() {
			var err error
			msg := label + " 现在是新会话的默认工作目录了。"
			if sub == "rm" {
				err, msg = a.RemoveWorkspace(id), "已从列表里删掉 "+label+"（机器上的文件没有动）。"
			} else {
				err = a.SetDefaultWorkspace(id)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			a.reply(c, msg, err)
		}()
	default:
		w, err := a.pickWorkspace(sub)
		if err != nil {
			a.answer(c, err.Error()+"\n"+dirUsage)
			return
		}
		msg, err := a.setSessionWorkspace(c.s.sid, w.ID)
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
