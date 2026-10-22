package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/skills"
)

// kernelHost 是装了内核的电脑：网关请它的内核代为执行工具（run）、列目录（ls）、放技能文件（skill_put）。
// 用在这一轮由别的内核跑、但工作目录在这台电脑上的时候。
type kernelHost struct {
	a    *App
	node string
}

// call 发一条请求给内核，等它回 run_result；ctx 结束时发 cancel（有的话）不再等。
func (k kernelHost) call(ctx context.Context, m proto.Msg, cancel string) (proto.Msg, error) {
	a := k.a
	a.mu.Lock()
	a.hostSeq++
	m.ID = fmt.Sprintf("h%d", a.hostSeq)
	ch := make(chan proto.Msg, 1)
	a.hostWaits[m.ID] = hostWait{node: k.node, ch: ch}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.hostWaits, m.ID)
		a.mu.Unlock()
	}()
	if !a.hub.Send(k.node, m) {
		return proto.Msg{}, errors.New(k.node + " 的内核没有连接")
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		if cancel != "" {
			a.hub.Send(k.node, proto.Msg{T: cancel, ID: m.ID})
		}
		return proto.Msg{}, ctx.Err()
	}
}

func (k kernelHost) Exec(ctx context.Context, req []byte) host.Result {
	r, err := k.call(ctx, proto.Msg{T: proto.TRun, Req: bytes.TrimSpace(req)}, proto.TRunCancel)
	if err != nil {
		if ctx.Err() != nil {
			return host.Result{Content: "已停止"}
		}
		return host.Result{Content: err.Error(), Lost: true}
	}
	if r.T == "" {
		return host.Result{Content: k.node + " 的内核断开了", Lost: true}
	}
	return host.Result{OK: r.OK != nil && *r.OK, Content: r.Content}
}

func (k kernelHost) Ls(ctx context.Context, path string) (*host.Listing, error) {
	r, err := k.call(ctx, proto.Msg{T: proto.TLs, Path: path}, "")
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New(k.node + " 的内核没有响应")
		}
		return nil, err
	}
	if r.OK == nil || !*r.OK {
		if r.Detail == "" {
			r.Detail = k.node + " 的内核断开了"
		}
		return nil, errors.New(r.Detail)
	}
	return &host.Listing{OK: true, Path: r.Path, Home: r.Home, Blocked: r.Blocked, Entries: r.Entries, Truncated: r.Truncated}, nil
}

// PutFiles 只用来放技能文件：内核只肯写 ~/.nano-agent/skills/<技能名>/
func (k kernelHost) PutFiles(ctx context.Context, dir string, files []host.File) error {
	name, ok := strings.CutPrefix(dir, skills.CacheDir+"/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return errors.New("内核只接收技能文件")
	}
	list := make([]proto.SkillFile, len(files))
	for i, f := range files {
		list[i] = proto.SkillFile{Path: f.Path, Data: base64.StdEncoding.EncodeToString(f.Data), Exec: f.Exec}
	}
	r, err := k.call(ctx, proto.Msg{T: proto.TSkillPut, Skill: name, Files: list}, "")
	if err != nil {
		return err
	}
	if r.OK == nil || !*r.OK {
		return errors.New(r.Content)
	}
	return nil
}

// hostWait 是一个等内核回复的请求
type hostWait struct {
	node string
	ch   chan proto.Msg
}

// hostReply 处理内核的 run_result / ls_result。调用方持有锁。
func (a *App) hostReply(node string, m *proto.Msg) {
	if w, ok := a.hostWaits[m.ID]; ok && w.node == node {
		delete(a.hostWaits, m.ID)
		w.ch <- *m
	}
}

// hostGone：内核断开了，等它回复的请求都结束（收到空消息）。调用方持有锁。
func (a *App) hostGone(node string) {
	for id, w := range a.hostWaits {
		if w.node == node {
			delete(a.hostWaits, id)
			w.ch <- proto.Msg{}
		}
	}
}
