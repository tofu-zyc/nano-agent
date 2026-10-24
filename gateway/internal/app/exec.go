package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/skills"
)

// 内核交给网关的工具调用（exec）：
//   - 工作目录不在跑这一轮的内核的机器上：网关在那台机器上执行（本机、SSH 服务器网关自己执行，
//     装了内核的电脑请它的内核代为执行），结果交回内核（exec_result）。执行的约束（工作目录、只读、
//     能不能写）由网关按这一轮开始时的设置生成，不用内核给的；
//   - Skill：技能在网关上，网关读说明，把附带文件放到挂载的每台机器上。

// execTool 处理内核的 exec。调用方持有锁。
func (a *App) execTool(node string, s *sess, m *proto.Msg) {
	r := s.run
	reply := func(ok bool, content string) {
		a.hub.Send(node, proto.Msg{T: proto.TExecResult, SID: s.sid, ID: m.ID, OK: proto.Bool(ok), Content: content})
	}
	if m.Name == "WebSearch" || m.Name == "WebFetch" {
		a.webTool(s, m, reply)
		return
	}
	if m.Name == "Schedule" {
		var args ScheduleRequest
		if err := json.Unmarshal(m.Args, &args); err != nil {
			reply(false, "定时任务参数格式不正确")
			return
		}
		result, err := a.scheduleAction(s.sid, r.from, args)
		if err != nil {
			reply(false, err.Error())
		} else {
			b, _ := json.Marshal(result)
			reply(true, string(b))
		}
		return
	}
	if m.Name == "Message" {
		if !a.isAdmin(r.from) {
			reply(false, "只有管理员能用 Message 给群和别的 agent 发消息")
			return
		}
		raw := string(m.Args)
		if raw == "" {
			raw = "{}"
		}
		go a.messageTool(s.sid, raw, reply)
		return
	}
	if m.Name == "Skill" {
		a.skillTool(r, s.sid, m, reply)
		return
	}
	if m.Name == "Gateway" || m.Name == "GatewayAdmin" {
		if !a.isAdmin(r.from) {
			reply(false, "只有管理员能用网关管理工具")
			return
		}
		raw := string(m.Args)
		if raw == "" {
			raw = "{}"
		}
		if m.Name == "Gateway" {
			go a.gatewayQuery(s.sid, raw, reply)
		} else {
			go a.gatewayAdminCall(s.sid, r.from, raw, reply)
		}
		return
	}
	mt := r.rt.find(m.Workspace)
	if mt == nil {
		reply(false, "这一轮没有挂载工作目录 "+m.Workspace)
		return
	}
	if mt.local {
		reply(false, "工作目录 "+m.Workspace+" 在跑这一轮的内核的机器上，应该由内核自己执行")
		return
	}
	h, _, why := a.hostFor(mt.ws.Host)
	if h == nil {
		reply(false, fmt.Sprintf("工作目录 %s 所在的 %s 现在用不了：%s", mt.ws.Label(), mt.ws.Host, why))
		return
	}
	spec := r.rt.spec(*mt, a.isAdmin(r.from))
	write := r.write
	rw := spec.RW
	if m.Elevated {
		if !a.isAdmin(r.from) {
			reply(false, "当前身份不能提权写工作目录外")
			return
		}
		write = true
		rw = append(append([]string{}, rw...), "/")
	}
	args := m.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	req, _ := json.Marshal(map[string]any{
		"tool": m.Name, "args": args, "dirs": spec.Dirs, "ro": spec.RO, "rw": rw, "write": write, "elevated": m.Elevated,
		"tmp_key": s.sid, "max_output": min(max(m.MaxOutput, 4096), 1<<20), "watch_stdin": true,
	})
	timeout := time.Duration(m.TimeoutMs) * time.Millisecond
	if timeout <= 0 || timeout > time.Hour {
		timeout = 10 * time.Minute
	}
	// 内核自己也计时，超时会发 exec_cancel；这里多留一点余量兜底
	ctx, cancel := context.WithTimeout(a.ctx, timeout+10*time.Second)
	r.execs[m.ID] = cancel
	a.log.Info("在 "+mt.ws.Host+" 上执行", "sid", s.sid, "tool", m.Name, "id", m.ID, "kernel", node)
	go func() {
		res := h.Exec(ctx, append(req, '\n'))
		cancel()
		a.mu.Lock()
		delete(r.execs, m.ID)
		a.mu.Unlock()
		if res.Lost {
			a.log.Warn("执行时连不上机器", "host", mt.ws.Host, "detail", res.Content)
		}
		reply(res.OK, res.Content)
	}()
}

// skillTool 加载一个技能：返回说明，把附带文件放到这一轮挂载的每台机器上（~/.nano-agent/skills/<名字>/）。
// 调用方持有锁。
func (a *App) skillTool(r *run, sid string, m *proto.Msg, reply func(bool, string)) {
	var args struct {
		Name string `json:"name"`
	}
	json.Unmarshal(m.Args, &args)
	text, files, _, ok := a.skills.Load(args.Name)
	if !ok {
		reply(false, text)
		return
	}
	targets := map[string]host.Host{}
	for _, mt := range r.rt.mounts {
		if _, done := targets[mt.ws.Host]; done {
			continue
		}
		var h host.Host
		if mt.local && mt.ws.Host != host.LocalName {
			h = kernelHost{a: a, node: r.node} // 内核所在的电脑
		} else {
			h, _, _ = a.hostFor(mt.ws.Host)
		}
		if h != nil {
			targets[mt.ws.Host] = h
		}
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	r.execs[m.ID] = cancel
	a.log.Info("加载技能", "sid", sid, "skill", args.Name, "machines", len(targets))
	go func() {
		var mu sync.Mutex
		var wg sync.WaitGroup
		var failed []string
		for name, h := range targets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := h.PutFiles(ctx, skills.CacheDir+"/"+args.Name, files); err != nil {
					mu.Lock()
					failed = append(failed, name+"："+err.Error())
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		cancel()
		a.mu.Lock()
		delete(r.execs, m.ID)
		a.mu.Unlock()
		if len(failed) > 0 {
			text += "\n\n（附带文件没能放到这些机器上：" + strings.Join(failed, "；") + "）"
		}
		reply(true, text)
	}()
}
