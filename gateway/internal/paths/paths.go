// Package paths 是 nano-agent 自己的目录：默认 ~/.nano-agent，可以用 NANO_HOME 改。
// 程序、配置、数据、socket 都在这个目录下面，删掉它就清理干净。
package paths

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// Home 是 ~/.nano-agent（或者 $NANO_HOME）。
func Home() string {
	if h := os.Getenv("NANO_HOME"); h != "" {
		if abs, err := filepath.Abs(h); err == nil {
			return abs
		}
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".nano-agent"
	}
	return filepath.Join(home, ".nano-agent")
}

// Config 是默认的 gateway.toml。
func Config() string { return filepath.Join(Home(), "gateway.toml") }

// RunDir 是 socket 目录。
func RunDir() string { return filepath.Join(Home(), "run") }

// Socket 是运行目录下名为 name 的 socket；路径太长（unix socket 最多 107 字节）时
// 退回 $XDG_RUNTIME_DIR 或 /tmp 下的私有目录，名字用 key 的哈希保证不冲突。
func Socket(name, key string) string {
	p := filepath.Join(RunDir(), name)
	if len(p) <= 100 {
		return p
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("nano-agent-%d", os.Getuid()))
	}
	os.MkdirAll(dir, 0o700)
	sum := sha256.Sum256([]byte(p + "\x00" + key))
	return filepath.Join(dir, fmt.Sprintf("nano-%x.sock", sum[:6]))
}

// ShortRunDir 是 SSH 控制 socket 用的短目录；run/nano-ssh 太长时退回 XDG_RUNTIME_DIR 或 /tmp。
func ShortRunDir(name string) string {
	p := filepath.Join(RunDir(), name)
	if len(p)+48 <= 100 {
		os.MkdirAll(p, 0o700)
		return p
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("nano-agent-%d", os.Getuid()))
	}
	dir = filepath.Join(dir, name)
	os.MkdirAll(dir, 0o700)
	return dir
}
