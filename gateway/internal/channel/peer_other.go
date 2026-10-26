//go:build !linux

package channel

import (
	"fmt"
	"net"
	"os"
	"runtime"
)

// checkPeer 在其他系统上只靠 socket 文件的权限（只有自己能连）。
func checkPeer(net.Conn) error { return nil }

// privateDir 确认 dir 是只有自己能进的目录。
func privateDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s 不是只有自己能进的目录", dir)
	}
	// Windows 没有 Unix 权限位，目录访问由用户 ACL 控制。
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s 不是只有自己能进的目录", dir)
	}
	return nil
}
