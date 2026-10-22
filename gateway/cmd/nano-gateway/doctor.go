package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"nano-agent/gateway/internal/app"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/control"
	"nano-agent/gateway/internal/host"
	"nano-agent/gateway/internal/paths"
	"nano-agent/gateway/internal/version"
)

type doctorItem struct {
	ok   bool
	name string
	note string
	fix  string
}

func addItem(items *[]doctorItem, ok bool, name, note, fix string) {
	*items = append(*items, doctorItem{ok: ok, name: name, note: note, fix: fix})
}

func runDoctor(args []string) int {
	cfgPath, rest, err := pathFlag("doctor", args)
	if err != nil || len(rest) > 0 {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway doctor [-c 配置文件]")
		return 2
	}
	items := []doctorItem{}

	var c *config.Config
	if err := initSecret(); err != nil {
		addItem(&items, false, "secret.key", "读不了：", "检查 ~/.nano-agent/secret.key 的权限；不行就重新安装网关")
	} else {
		key := filepath.Join(paths.Home(), "secret.key")
		if st, err := os.Stat(key); err != nil {
			addItem(&items, false, "secret.key", "不存在：", "重新启动网关会生成；如果没有权限，检查 NANO_HOME")
		} else if st.Mode().Perm()&0o077 != 0 {
			addItem(&items, false, "secret.key", fmt.Sprintf("权限是 %04o，别人能读", st.Mode().Perm()), "chmod 600 "+key)
		} else {
			addItem(&items, true, "secret.key", "存在，权限 0600", "")
		}
	}

	if err := config.Check(cfgPath); err != nil {
		addItem(&items, false, "配置", "加载不了："+err.Error(), "修好配置后再重启；网关不会带着坏配置重启")
	} else {
		addItem(&items, true, "配置", "能加载（gateway.toml + data/config.json）", "")
		if err := initSecret(); err == nil {
			if s, _, err := config.Open(cfgPath); err == nil {
				c = s.Current()
			}
		}
	}
	if c == nil {
		if b, err := os.ReadFile(cfgPath); err == nil {
			_ = b
		}
		printDoctor(items)
		return 1
	}

	if st, err := os.Stat(c.Session.DataDir); err != nil {
		addItem(&items, false, "数据目录", "不存在："+c.Session.DataDir, "启动一次网关会创建，或检查 session.data_dir")
	} else if st.Mode().Perm()&0o077 != 0 {
		addItem(&items, false, "数据目录", fmt.Sprintf("权限是 %04o，别人能读", st.Mode().Perm()), "chmod 700 "+c.Session.DataDir)
	} else {
		addItem(&items, true, "数据目录", "存在，权限 0700", "")
	}

	running := false
	var st app.Status
	if raw, err := control.Query(c.Session.DataDir); err == nil {
		if json.Unmarshal(raw, &st) == nil {
			running = true
		}
	}
	if running {
		addItem(&items, true, "运行", fmt.Sprintf("pid %d，版本 %s，运行方式 %s", st.PID, st.Version, st.Mode), "")
	} else if useSystemd() {
		enabled, _ := exec.Command("systemctl", "--user", "is-enabled", systemdUnit).Output()
		if strings.TrimSpace(string(enabled)) != "enabled" {
			addItem(&items, false, "systemd 服务", "没有启用", "systemctl --user enable --now "+systemdUnit)
		} else {
			addItem(&items, true, "systemd 服务", "已启用", "")
		}
		if out, err := exec.Command("loginctl", "show-user", currentUser(), "-p", "Linger", "--value").Output(); err == nil && strings.TrimSpace(string(out)) != "yes" {
			addItem(&items, false, "systemd linger", "只在登录后运行", "sudo loginctl enable-linger "+currentUser())
		}
	} else {
		if pid, ok := readAlivePID(pidFile("gateway.supervisor.pid")); ok {
			addItem(&items, true, "后台进程", fmt.Sprintf("看护进程在运行（pid %d）", pid), "")
		} else {
			addItem(&items, false, "后台进程", "没有运行", "nano-gateway start")
		}
	}

	// 端口：网关正在运行时这些端口由它自己占用，不能拿“被占用”当问题
	for _, p := range []struct{ name, addr string }{
		{"网页端口", c.Web.Listen},
		{"内核端口", c.Kernel.Listen},
		{"电脑接入端口", c.Kernel.DeviceListen},
	} {
		if p.addr == "" || p.addr == "off" {
			continue
		}
		if strings.HasPrefix(p.addr, "unix:") {
			path := strings.TrimPrefix(p.addr, "unix:")
			if running {
				addItem(&items, true, p.name, "由正在运行的网关使用："+path, "")
			} else if _, err := os.Stat(path); err == nil {
				addItem(&items, false, p.name, "残留的 socket："+path, "rm "+path)
			} else {
				addItem(&items, true, p.name, "路径可用："+path, "")
			}
			continue
		}
		if running {
			addItem(&items, true, p.name, "由正在运行的网关使用："+p.addr, "")
			continue
		}
		addr := strings.TrimPrefix(p.addr, "tcp:")
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			addItem(&items, false, p.name, fmt.Sprintf("%s 被占用：%v", addr, err), "换个端口，或者停掉占用它的程序")
		} else {
			ln.Close()
			addItem(&items, true, p.name, addr+" 可用", "")
		}
	}

	if c.Kernel.Listen != "" && strings.HasPrefix(c.Kernel.Listen, "unix:") &&
		c.Kernel.DeviceListen == "" {
		addItem(&items, false, "内核监听方式", "内核监听地址是 unix，别的电脑和 Windows 内核连不上",
			"把 [kernel] listen 改成 tcp:0.0.0.0:7700（或需要的地址），再重启")
	}

	exe, err := host.FindExecutor(c.Dir)
	if err != nil {
		addItem(&items, false, "本机执行器", "找不到 nano-exec："+err.Error(), "重新运行 make 或安装网关")
	} else {
		kernel := filepath.Join(filepath.Dir(exe.Path), "nano-kernel")
		if st, err := os.Stat(kernel); err != nil || st.IsDir() {
			addItem(&items, false, "本机内核", "找不到 "+kernel, "重新安装内核/网关")
		} else {
			note := "存在：" + kernel
			if v, err := os.ReadFile(filepath.Join(filepath.Dir(exe.Path), "VERSION")); err == nil {
				kv := strings.TrimSpace(string(v))
				if version.Version != "dev" && kv != version.Version {
					addItem(&items, false, "版本", fmt.Sprintf("网关 %s，本机内核 %s", version.Version, kv), "重新安装同一版本的内核")
				} else {
					addItem(&items, true, "版本", fmt.Sprintf("网关 %s，本机内核 %s", version.Version, kv), "")
				}
			} else {
				addItem(&items, true, "本机内核", note+"（没有 VERSION 文件，版本未知）", "")
			}
		}
	}

	if isWSL() {
		addItem(&items, false, "WSL 保活", "关掉所有终端后 Windows 可能停掉 WSL，网关跟着停",
			wslKeepaliveHint())
	} else {
		addItem(&items, true, "WSL 保活", "不在 WSL 里，不需要", "")
	}

	printDoctor(items)
	for _, it := range items {
		if !it.ok {
			return 1
		}
	}
	return 0
}

func printDoctor(items []doctorItem) {
	fmt.Println("网关自检：")
	for _, it := range items {
		if it.ok {
			fmt.Printf("  [正常] %s：%s\n", it.name, it.note)
			continue
		}
		fmt.Printf("  [问题] %s：%s\n", it.name, it.note)
		if it.fix != "" {
			fmt.Printf("         修：%s\n", it.fix)
		}
	}
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return fmt.Sprintf("%d", os.Getuid())
}

func isWSL() bool {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

func wslKeepaliveHint() string {
	distro := os.Getenv("WSL_DISTRO_NAME")
	if distro == "" {
		distro = "<发行版名字>"
	}
	return `在 Windows 的启动文件夹里放一个隐藏的 nano-agent-wsl-keepalive.vbs：
%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\nano-agent-wsl-keepalive.vbs
内容让 wsl.exe 启动这个发行版里的网关并一直留一个进程：
  Set sh = CreateObject("WScript.Shell")
  sh.Run "C:\Windows\System32\wsl.exe -d ` + distro + ` --exec /bin/sh -c ""nano-gateway start --quiet; exec /bin/sleep 2147483647""", 0, False
（启动文件夹不需要管理员权限，也不要改 .wslconfig；卸载网关时安装脚本会删掉这个文件）`
}
