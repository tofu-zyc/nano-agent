package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
)

// 异机模式：工作目录不在跑这一轮的内核所在的机器上（本机没有内核，或者在 SSH 服务器上），
// 内核把工具调用交给网关（exec），网关在工作目录所在的机器上运行执行器，结果交回内核（exec_result）。
// 执行的约束（工作目录、只读、能不能写）由网关按这一轮开始时的设置生成，不用内核给的。

// skillsRoot 是执行的机器上放某个内核的技能文件的目录
func skillsRoot(node string) string { return "~/.cache/nano-agent/skills/" + node }

// execTool 处理内核的 exec。调用方持有锁。
func (a *App) execTool(node string, s *sess, m *proto.Msg) {
	r := s.run
	h, why := a.execHost(r)
	if h == nil {
		a.hub.Send(node, proto.Msg{T: proto.TExecResult, SID: s.sid, ID: m.ID, OK: proto.Bool(false), Content: why})
		return
	}
	maxOut := min(max(m.MaxOutput, 4096), 1<<20)
	args := m.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	req, _ := json.Marshal(map[string]any{
		"tool": m.Name, "args": args, "dirs": r.dirs, "ro": append(append([]string{}, r.ro...), skillsRoot(node)),
		"rw": r.rw, "write": r.write, "tmp_key": s.sid, "max_output": maxOut, "watch_stdin": true,
	})
	timeout := time.Duration(m.TimeoutMs) * time.Millisecond
	if timeout <= 0 || timeout > time.Hour {
		timeout = 10 * time.Minute
	}
	// 内核自己也计时，超时会发 exec_cancel；这里多留一点余量兜底
	ctx, cancel := context.WithTimeout(a.ctx, timeout+10*time.Second)
	r.execs[m.ID] = cancel
	a.log.Info("在 "+r.host+" 上执行", "sid", s.sid, "tool", m.Name, "id", m.ID, "kernel", node)
	go func() {
		res := h.Exec(ctx, append(req, '\n'))
		cancel()
		a.mu.Lock()
		delete(r.execs, m.ID)
		a.mu.Unlock()
		if res.Lost {
			a.log.Warn("执行时连不上机器", "host", r.host, "detail", res.Content)
		}
		a.hub.Send(node, proto.Msg{T: proto.TExecResult, SID: s.sid, ID: m.ID, OK: proto.Bool(res.OK), Content: res.Content})
	}()
}

// execHost 是这一轮执行工具的机器；用不了时返回原因。调用方持有锁。
func (a *App) execHost(r *run) (host.Host, string) {
	if !r.gateway || a.hosts == nil {
		return nil, "这一轮的工具不是交给网关执行的"
	}
	h, info, ok := a.hosts.Host(r.host)
	if !ok {
		return nil, "没有 " + r.host + " 这台机器了"
	}
	if h == nil {
		return nil, fmt.Sprintf("工作目录所在的 %s 现在用不了：%s", r.host, info.Detail)
	}
	return h, ""
}

// skillFiles 处理内核的 skill_files：把技能目录的文件放到执行的机器上，回复那台机器上的目录。调用方持有锁。
func (a *App) skillFiles(node string, s *sess, m *proto.Msg) {
	r := s.run
	fail := func(why string) {
		a.hub.Send(node, proto.Msg{T: proto.TExecResult, SID: s.sid, ID: m.ID, OK: proto.Bool(false), Content: why})
	}
	h, why := a.execHost(r)
	if h == nil {
		fail(why)
		return
	}
	if m.Skill == "" || !utf8.ValidString(m.Skill) || strings.ContainsAny(m.Skill, "/\\\x00") || strings.HasPrefix(m.Skill, ".") || len(m.Skill) > 128 {
		fail("技能的名字不对")
		return
	}
	files := make([]host.File, 0, len(m.Files))
	for _, f := range m.Files {
		data, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			fail("技能文件 " + f.Path + " 的内容不对")
			return
		}
		files = append(files, host.File{Path: f.Path, Data: data, Exec: f.Exec})
	}
	dir := skillsRoot(node) + "/" + m.Skill
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	r.execs[m.ID] = cancel
	go func() {
		err := h.PutFiles(ctx, dir, files)
		cancel()
		a.mu.Lock()
		delete(r.execs, m.ID)
		a.mu.Unlock()
		if err != nil {
			fail(err.Error())
			return
		}
		a.hub.Send(node, proto.Msg{T: proto.TExecResult, SID: s.sid, ID: m.ID, OK: proto.Bool(true), Content: dir})
	}()
}
