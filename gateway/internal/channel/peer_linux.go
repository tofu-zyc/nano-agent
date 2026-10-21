package channel

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// checkPeer 确认连上来的是同一个系统用户（socket 文件的权限已经只给自己了，这里再确认一次）。
func checkPeer(conn net.Conn) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("不是 unix socket 连接")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credErr != nil {
		return credErr
	}
	if int(cred.Uid) != os.Getuid() {
		return fmt.Errorf("连接来自别的用户（uid %d）", cred.Uid)
	}
	return nil
}

// privateDir 确认 dir 是只有自己能进的目录（不是符号链接，属于自己，组和其他人没有权限），
// 免得别人在里面放一个假的 socket。
func privateDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	switch {
	case !st.IsDir():
		return fmt.Errorf("%s 不是目录", dir)
	case !ok || int(sys.Uid) != os.Getuid():
		return fmt.Errorf("%s 不属于当前用户", dir)
	case st.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("%s 的权限是 %o，别人也能进", dir, st.Mode().Perm())
	}
	return nil
}
