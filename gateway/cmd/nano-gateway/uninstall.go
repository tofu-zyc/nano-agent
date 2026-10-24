package main

// nano-gateway uninstall：停掉网关服务，删掉程序、服务文件、PATH 行和 WSL 保活脚本。
// 同一个 ~/.nano-agent 里还装着内核时只卸网关、保留内核；--keep-data 保留配置和数据（以后重装接着用）。

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"nano-agent/gateway/internal/paths"
)

const (
	pathMark      = "# nano-agent PATH"    // 安装脚本写进 shell 启动文件的 PATH 行
	autostartMark = "# nano-agent gateway" // 没有 systemd 时 .profile 里的自启行
)

func runUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	keepData := fs.Bool("keep-data", false, "保留配置、会话记录、模型和密钥，只删程序")
	yes := fs.Bool("yes", false, "不提问")
	fs.BoolVar(yes, "y", false, "不提问")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "用法：nano-gateway uninstall [--keep-data] [--yes]")
		return 2
	}
	home := paths.Home()
	kernel := hasKernel(home)
	what := "删除 " + home + "（配置、会话记录、模型、SSH 连接、密钥都会删掉）"
	switch {
	case kernel && *keepData:
		what = "删除网关程序，保留配置和数据，也保留这台机器上的内核（" + filepath.Join(home, "kernel") + "）"
	case kernel:
		what = "删除网关的程序、配置和数据（会话记录、模型、SSH 连接、密钥），保留这台机器上的内核（" + filepath.Join(home, "kernel") + "）"
	case *keepData:
		what = "删除网关程序，配置和数据留在 " + home + "（以后重装接着用）"
	}
	fmt.Println("将要：停止网关服务，" + what + "。")
	if !*yes && !confirmTTY("确定卸载？") {
		fmt.Println("已取消。")
		return 0
	}
	if err := uninstallGateway(home, *keepData, kernel); err != nil {
		fmt.Fprintln(os.Stderr, "卸载没有完成：", err)
		return 1
	}
	fmt.Println("网关已卸载。")
	switch {
	case kernel:
		fmt.Println("这台机器上的内核还在：nano-kernel status 查看，nano-kernel uninstall 卸载。")
	case *keepData:
		fmt.Println("配置和数据在 " + home + "，重新运行安装命令就能接着用。")
	}
	return 0
}

// hasKernel：同一个 NANO_HOME 里有没有用安装脚本装的内核（它和网关共用 bin/ 和 PATH 行）。
func hasKernel(home string) bool {
	for _, n := range []string{"nano-kernel", "nano-kernel.exe"} {
		if _, err := os.Stat(filepath.Join(home, "kernel", n)); err == nil {
			return true
		}
	}
	return false
}

func uninstallGateway(home string, keepData, kernel bool) error {
	// 1. 停服务
	if _, err := os.Stat(unitPath()); err == nil {
		if hasSystemd() {
			exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).Run()
		}
		os.Remove(unitPath())
		if hasSystemd() {
			exec.Command("systemctl", "--user", "daemon-reload").Run()
		}
	}
	stopBackground(true)
	removeWSLKeepalive()

	// 2. shell 启动文件：自启行总是去掉；PATH 行只在没有内核时去掉（内核的 nano-kernel 也在 bin/ 里）
	marks := []string{autostartMark}
	if !kernel {
		marks = append(marks, pathMark)
		os.Remove(filepath.Join(userHome(), ".config", "fish", "conf.d", "nano-agent.fish"))
	}
	for _, rc := range []string{".profile", ".bashrc", ".zshrc"} {
		removeLines(filepath.Join(userHome(), rc), marks)
	}
	removeLocalBinLink("nano-gateway", home)
	if !kernel {
		removeLocalBinLink("nano-kernel", home)
		removeLocalBinLink("nano-kernel-ctl", home) // 旧版本装的
	}

	// 3. 文件
	if !kernel && !keepData {
		return os.RemoveAll(home)
	}
	remove := []string{"bin/nano-gateway", "lib", "run", "backup", "cache/releases", "update.json", shippedFile}
	if !keepData {
		remove = append(remove, "gateway.toml", "data", "secret.key", "agents", "personas", "skills")
	}
	var firstErr error
	for _, rel := range remove {
		if err := os.RemoveAll(filepath.Join(home, filepath.FromSlash(rel))); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, dir := range []string{"bin", "cache"} {
		os.Remove(filepath.Join(home, dir)) // 空了才删得掉
	}
	return firstErr
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// removeLines 去掉文件里带某个标记的行。
func removeLines(path string, marks []string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var out strings.Builder
	changed := false
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		hit := false
		for _, m := range marks {
			if strings.Contains(line, m) {
				hit = true
			}
		}
		if hit {
			changed = true
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if changed {
		st, _ := os.Stat(path)
		mode := os.FileMode(0o644)
		if st != nil {
			mode = st.Mode().Perm()
		}
		os.WriteFile(path, []byte(out.String()), mode)
	}
}

// removeLocalBinLink 删掉安装脚本放在 ~/.local/bin 里、指向 NANO_HOME 的链接。
func removeLocalBinLink(name, home string) {
	p := filepath.Join(userHome(), ".local", "bin", name)
	if target, err := os.Readlink(p); err == nil && strings.HasPrefix(target, home+string(filepath.Separator)) {
		os.Remove(p)
	}
}

// removeWSLKeepalive 删掉 Windows 启动文件夹里的 WSL 保活脚本（不在 WSL 里就什么也不做）。
func removeWSLKeepalive() {
	if !isWSL() {
		return
	}
	dir, err := wslStartupDir()
	if err != nil {
		return
	}
	os.Remove(filepath.Join(dir, "nano-agent-wsl-keepalive.vbs"))
}

// wslStartupDir 是 Windows 当前用户的启动文件夹（WSL 里的路径）。
func wslStartupDir() (string, error) {
	cmd := exec.Command("/mnt/c/Windows/System32/cmd.exe", "/c", "echo %APPDATA%")
	cmd.Dir = "/mnt/c"
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(out), "\r", "")), "\n")
	appdata := strings.TrimSpace(lines[len(lines)-1])
	if appdata == "" || strings.Contains(appdata, "%") {
		return "", errors.New("拿不到 APPDATA")
	}
	p, err := exec.Command("wslpath", "-u", appdata+`\Microsoft\Windows\Start Menu\Programs\Startup`).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(p)), nil
}
