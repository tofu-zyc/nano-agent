package app

import (
	"errors"
	"fmt"
	"strings"
)

func (a *App) sandboxModeFor(sid string) string {
	mode := a.cfg.Sandbox.ModeOrDefault()
	if override := a.agentOf(sid).SandboxMode; override != nil {
		mode = *override
	}
	return mode
}

func (a *App) freezeSandboxRoute(rt *route, mode string) {
	for i := range rt.mounts {
		mt := &rt.mounts[i]
		mt.sandboxMode = mode
		if mt.ws.SandboxMode != "" {
			mt.sandboxMode = mt.ws.SandboxMode
		}
		if n := a.nodes[a.kernelOn(mt.ws.Host)]; n != nil {
			mt.sandboxRequired = n.info.SandboxMode
		}
	}
}

func (a *App) SetWorkspaceSandbox(id, mode string) error {
	if mode != "" && mode != "auto" && mode != "on" && mode != "off" {
		return errors.New("执行隔离只能是 auto、on、off，或空字符串跟随 agent")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.findWorkspace(id)
	if w == nil {
		return errors.New("没有这个工作目录")
	}
	old := w.SandboxMode
	w.SandboxMode = mode
	if err := a.saveWorkspaces(); err != nil {
		w.SandboxMode = old
		return err
	}
	a.publishWorkspaces()
	return nil
}

func (a *App) describeSandbox(sid string) string {
	mode := a.sandboxModeFor(sid)
	rt := a.route(a.workspaceOf(sid))
	a.freezeSandboxRoute(&rt, mode)
	var b strings.Builder
	fmt.Fprintf(&b, "执行隔离请求：%s（全局 → agent → 工作目录）。\n", mode)
	for _, mt := range rt.mounts {
		required := mt.sandboxRequired
		if required == "" {
			required = "auto"
		}
		fmt.Fprintf(&b, "· %s：请求 %s，节点部署 %s，Landlock 能力 %d。\n", mt.ws.Label(), mt.sandboxMode, required, mt.landlock)
	}
	b.WriteString("能力探测不等于本次已经施加隔离；每次文件或 Shell 调用的结果记录实际状态。off 按系统用户身份执行任意 Shell，文件工具路径权限和审批规则仍分别生效。\n/sandbox global|agent auto|on|off|default；/sandbox workspace <工作目录ID> auto|on|off|default。新调用使用新设置。")
	return b.String()
}

func (a *App) cmdSandbox(c in, arg string) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		a.answer(c, a.describeSandbox(c.s.sid))
		return
	}
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能修改执行隔离")
		return
	}
	target, mode, id := "", "", ""
	if len(fields) == 2 {
		target, mode = fields[0], fields[1]
	} else if len(fields) == 3 && fields[0] == "workspace" {
		target, id, mode = fields[0], fields[1], fields[2]
	}
	if mode != "auto" && mode != "on" && mode != "off" && mode != "default" {
		a.answer(c, a.describeSandbox(c.s.sid))
		return
	}
	agentID := a.execAgentID(c.s.sid)
	go func() {
		var err error
		switch target {
		case "global":
			var value any = mode
			if mode == "default" {
				value = nil
			}
			_, err = a.setSetting("sandbox.mode", value)
		case "agent":
			var value any = mode
			if mode == "default" {
				value = nil
			}
			_, err = a.UpdateAgent(agentID, AgentPatch{Settings: map[string]any{"sandbox_mode": value}})
		case "workspace":
			if mode == "default" {
				mode = ""
			}
			err = a.SetWorkspaceSandbox(id, mode)
		default:
			err = errors.New("指定 global、agent 或 workspace ID")
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.reply(c, "执行隔离设置已保存，下一轮新任务生效。", err)
	}()
}

func (a *App) sandboxNote(mode string, rt route) string {
	var policies []string
	for _, mt := range rt.mounts {
		policies = append(policies, mt.ws.Label()+"="+mt.sandboxMode)
	}
	if len(policies) == 0 {
		policies = []string{mode}
	}
	return "\n[系统] 执行隔离请求：" + strings.Join(policies, "；") + "。on 不满足时失败；off 的 Shell 按系统用户权限执行，不受工作目录文件隔离约束；文件工具路径检查、审批及工作范围指令仍分别生效。"
}

func sandboxRouteError(mode string, rt route) error {
	valid := func(mode string) bool { return mode == "auto" || mode == "on" || mode == "off" }
	if !valid(mode) {
		return fmt.Errorf("agent 执行隔离配置无效：%q", mode)
	}
	for _, mt := range rt.mounts {
		if !valid(mt.sandboxMode) {
			return fmt.Errorf("工作目录 %s 的执行隔离配置无效：%q", mt.ws.Label(), mt.sandboxMode)
		}
	}
	return nil
}
