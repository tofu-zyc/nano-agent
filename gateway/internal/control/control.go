// Package control 是网关的本机控制 socket：只有同一个用户能连，给 `nano-gateway status` 查询运行状态。
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"nano-agent/gateway/internal/paths"
)

// Socket 是控制 socket 的路径。和聊天 socket 一样放在 run/ 下；路径太长时退回私有临时目录。
func Socket(dataDir string) string {
	p := paths.Socket("control.sock", dataDir)
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.Chmod(filepath.Dir(p), 0o700)
	return p
}

type request struct {
	Op string `json:"op"`
}

type response struct {
	OK     bool            `json:"ok"`
	Status json.RawMessage `json:"status,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Serve 监听控制 socket，status 返回要发给客户端的对象。只处理 status；ctx 结束就关闭并删 socket。
func Serve(ctx context.Context, sock string, status func() any, log *slog.Logger) error {
	if c, err := net.DialTimeout("unix", sock, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("%s 已经有一个网关在用了", sock)
	}
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	defer os.Remove(sock)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handle(conn, status, log)
	}
}

func handle(conn net.Conn, status func() any, log *slog.Logger) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	var req request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(response{Error: "请求格式不对"})
		return
	}
	if req.Op != "status" {
		_ = json.NewEncoder(conn).Encode(response{Error: "不认识的请求：" + req.Op})
		return
	}
	b, err := json.Marshal(status())
	if err != nil {
		_ = json.NewEncoder(conn).Encode(response{Error: err.Error()})
		return
	}
	if err := json.NewEncoder(conn).Encode(response{OK: true, Status: b}); err != nil {
		log.Debug("写控制 socket 失败", "err", err)
	}
}

// Query 连上正在运行的网关，返回 status 的 JSON。网关没运行或 socket 不对时返回错误。
func Query(dataDir string) (json.RawMessage, error) {
	sock := Socket(dataDir)
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(request{Op: "status"}); err != nil {
		return nil, err
	}
	var resp response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		if resp.Error == "" {
			resp.Error = "网关没有返回状态"
		}
		return nil, errors.New(resp.Error)
	}
	return resp.Status, nil
}
