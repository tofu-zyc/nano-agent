package kernel

import (
	"net"
	"path/filepath"
	"testing"
)

func TestListenUnix(t *testing.T) {
	addr := "unix:" + filepath.Join(t.TempDir(), "k.sock")
	ln, err := Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(addr); err == nil {
		t.Fatal("第二个网关不应该抢走正在使用的 socket")
	}

	// 留下一个没人监听的 socket 文件，模拟上次异常退出
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	ln, err = Listen(addr)
	if err != nil {
		t.Fatalf("残留的 socket 文件应该被清理掉：%v", err)
	}
	ln.Close()
}
