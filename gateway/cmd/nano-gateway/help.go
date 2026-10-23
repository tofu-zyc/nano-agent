package main

// nano-gateway 的帮助：nano-gateway help / --help / -h 列出全部命令，
// nano-gateway help 命令 或 nano-gateway 命令 --help 看单个命令。

import (
	"fmt"
	"io"
	"strings"
)

type command struct {
	name  string
	args  string // 用法里命令后面的部分
	brief string // 列表里的一行说明
	more  string // 单个命令的帮助里再加的说明（选项等）
}

var commands = []command{
	{"status", "[-c 配置文件]", "运行状态：版本、网页地址、在线的内核和机器、消息渠道、默认模型、最近的错误", ""},
	{"start", "[-c 配置文件] [--quiet]", "在后台启动", "装了 systemd 用户服务就交给 systemd，否则作为后台进程运行（带看护，崩溃会重新拉起）。"},
	{"stop", "[--quiet]", "停止", ""},
	{"restart", "[-c 配置文件]", "重启（改了要重启才生效的设置后用）", "先检查配置，配置有误就不重启，网关保持原样。"},
	{"logs", "[-c 配置文件] [-f] [-n 行数]", "看日志", "  -f       持续输出新日志（Ctrl-C 退出）\n  -n 行数  显示最后几行，默认 100"},
	{"url", "[-c 配置文件]", "网页地址（带登录口令，复制到浏览器打开）", ""},
	{"chat", "[-c 配置文件]", "在终端里和网关聊天", "Ctrl-C 停下正在进行的任务，连按两次退出；Ctrl-D 退出。网关重启时自动重连。"},
	{"doctor", "[-c 配置文件]", "体检：配置、密钥、服务、本机内核和执行器，有问题给出修法", ""},
	{"update", "[--check] [--version x.y.z] [--yes] [-c 配置文件]", "更新到最新版本",
		"  --check          只查有没有新版本，不装\n" +
			"  --version x.y.z  装指定版本（也能用来退回旧版本）\n" +
			"  --yes            不提问\n" +
			"新程序先自检能读懂现在的配置才换上；换上后起不来会自动退回。\n" +
			"连着这个网关的内核会自动换成同一个版本。聊天里发 /update 也一样。"},
	{"uninstall", "[--keep-data] [--yes]", "卸载",
		"  --keep-data  只删程序，配置、会话记录、模型和密钥留着（重装后接着用）\n" +
			"  --yes        不提问\n" +
			"这台机器上还装着内核时只卸网关，内核留着（nano-kernel uninstall 另外卸）。"},
	{"export", "[-c 配置文件] [-o 文件]", "把配置、数据、密钥、人格和技能打成一个包，搬到另一台机器",
		"  -o 文件  导出到哪里（默认当前目录下带日期的 .tar.gz）\n" +
			"包里有密钥和各种口令，别发给别人。到新机器上用 nano-gateway import 导入，或者安装时加 --import 包。"},
	{"import", "<文件> [--force] [--old-local-as 机器名]", "导入 export 打的包",
		"  --force               已有配置和数据时先备份再覆盖\n" +
			"  --old-local-as 机器名  旧网关「本机」上的工作目录改记到这个机器名下\n" +
			"                        （旧网关那台电脑以后作为远程机器、用这个名字接进来）"},
	{"version", "", "版本号", ""},
}

func findCommand(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

func isHelpFlag(a string) bool {
	return a == "-h" || a == "--help" || a == "-help"
}

// wantsHelp：子命令的参数里有 -h / --help。
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if isHelpFlag(a) {
			return true
		}
	}
	return false
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "nano-gateway：nano-agent 的网关")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "管理（装好后在任何目录都能用）：")
	width := 0
	for _, c := range commands {
		if len(c.name) > width {
			width = len(c.name)
		}
	}
	for _, c := range commands {
		fmt.Fprintf(w, "  nano-gateway %-*s  %s\n", width, c.name, c.brief)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "运行（服务管理器用）：")
	fmt.Fprintln(w, "  nano-gateway [-c 配置文件] [-v]  在前台运行（-v 输出调试日志）")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "nano-gateway help 命令（或 nano-gateway 命令 --help）看单个命令的用法。")
	fmt.Fprintln(w, "配置文件默认是 $NANO_HOME/gateway.toml，NANO_HOME 默认 ~/.nano-agent。")
	fmt.Fprintln(w, "网关自带的本机内核由网关管理；其他电脑上单独装的内核用 nano-kernel 管理（nano-kernel help）。")
}

func commandUsage(c *command) string {
	return strings.TrimSpace("nano-gateway " + c.name + " " + c.args)
}

func printCommandHelp(w io.Writer, c *command) {
	fmt.Fprintln(w, "用法："+commandUsage(c))
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.brief)
	if c.more != "" {
		fmt.Fprintln(w, c.more)
	}
	if strings.Contains(c.args, "-c 配置文件") {
		fmt.Fprintln(w, "  -c 配置文件  默认 $NANO_HOME/gateway.toml")
	}
}

// runHelp：nano-gateway help [命令]
func runHelp(args []string, w io.Writer) int {
	if len(args) == 0 {
		printUsage(w)
		return 0
	}
	if c := findCommand(args[0]); c != nil {
		printCommandHelp(w, c)
		return 0
	}
	fmt.Fprintf(w, "没有这个命令：%s\n\n", args[0])
	printUsage(w)
	return 2
}
