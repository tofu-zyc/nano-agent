package app

import (
	"errors"
	"fmt"

	"nano-agent/gateway/internal/mcp"
	"nano-agent/gateway/internal/proto"
)

// MCP 透传（协议 v10）：网关不自己起进程，而是让某台机器的内核起（pipe_open），
// stdio 通过 pipe_write / pipe_data 来回传，进程退出是 pipe_closed。

// MCPLink 把 App 的管道方法交给 MCP 管理器（App 自己有个 Close()，名字撞了，包一层）。
func MCPLink(a *App) mcp.Link { return pipeLink{a} }

type pipeLink struct{ a *App }

func (l pipeLink) Open(node, pipe string, argv []string, env map[string]string, cwd string) error {
	return l.a.PipeOpen(node, pipe, argv, env, cwd)
}

func (l pipeLink) Write(node, pipe string, line []byte) error { return l.a.PipeWrite(node, pipe, line) }

func (l pipeLink) Close(node, pipe string) error { return l.a.PipeClose(node, pipe) }

// PipeOpen 让某台机器起一个子进程（MCP 服务）。
func (a *App) PipeOpen(node, pipe string, argv []string, env map[string]string, cwd string) error {
	if len(argv) == 0 {
		return errors.New("没有要跑的命令")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hub == nil || a.nodes[node] == nil {
		return fmt.Errorf("机器 %s 现在不在线", node)
	}
	if !a.hub.Send(node, proto.Msg{T: proto.TPipeOpen, Pipe: pipe, Argv: argv, Env: env, Cwd: cwd}) {
		return fmt.Errorf("机器 %s 现在不在线", node)
	}
	a.log.Info("让内核起 MCP 进程", "node", node, "pipe", pipe, "cmd", argv[0])
	return nil
}

// PipeWrite 往那个进程的 stdin 写一行。
func (a *App) PipeWrite(node, pipe string, line []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hub == nil || !a.hub.Send(node, proto.Msg{T: proto.TPipeWrite, Pipe: pipe, Data: string(line)}) {
		return fmt.Errorf("机器 %s 现在不在线", node)
	}
	return nil
}

// PipeClose 关掉那条管道。
func (a *App) PipeClose(node, pipe string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hub == nil {
		return nil
	}
	a.hub.Send(node, proto.Msg{T: proto.TPipeClose, Pipe: pipe})
	return nil
}
