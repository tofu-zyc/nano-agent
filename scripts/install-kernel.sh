#!/bin/sh
# nano-agent 内核一键安装：检查编译工具、编译、安装、写配置、注册开机自启、等网关确认接入。
#
# 两种用法：
#   在源码目录里：      ./scripts/install-kernel.sh
#   单文件安装包：      sh nano-kernel-install.sh      （在网关那台机器上 make installer 生成，拷过来就能用）
#
# 再运行一次就是升级（保留配置和配对口令）。选项：
#   --gateway 地址   网关的地址，例如 192.168.1.2:7700（不写会问）
#   --name 名字      这台电脑的名字，默认是主机名
#   --dir 目录       安装到哪里，默认 ~/.local/share/nano-agent/kernel
#   --no-service     不注册开机自启，也不启动
#   --yes            不提问，全部用默认值（缺编译工具时直接用 sudo 安装）
#   --uninstall      停止并删除
set -eu

GATEWAY=""
NAME=""
DIR="${XDG_DATA_HOME:-$HOME/.local/share}/nano-agent/kernel"
SERVICE=1
YES=0
UNINSTALL=0
UNIT=nano-kernel.service
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
PROFILE_MARK="# nano-agent 内核"

say() { printf '%s\n' "$*"; }
step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\033[31m错误：%s\033[0m\n' "$*" >&2; exit 1; }

usage() {
	sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--gateway) [ $# -ge 2 ] || usage 2; GATEWAY="$2"; shift 2 ;;
	--gateway=*) GATEWAY="${1#*=}"; shift ;;
	--name) [ $# -ge 2 ] || usage 2; NAME="$2"; shift 2 ;;
	--name=*) NAME="${1#*=}"; shift ;;
	--dir) [ $# -ge 2 ] || usage 2; DIR="$2"; shift 2 ;;
	--dir=*) DIR="${1#*=}"; shift ;;
	--no-service) SERVICE=0; shift ;;
	-y | --yes) YES=1; shift ;;
	--uninstall) UNINSTALL=1; shift ;;
	-h | --help) usage 0 ;;
	*) say "不认识的选项：$1"; usage 2 ;;
	esac
done

# 提问从终端读：用 curl … | sh 或者重定向了标准输入时也能问
TTY=""
if [ -r /dev/tty ] && (: </dev/tty) 2>/dev/null; then TTY=/dev/tty; fi

ask() { # ask 提示 默认值 → 回答写到 REPLY
	REPLY=""
	if [ "$YES" = 1 ] || [ -z "$TTY" ]; then REPLY="$2"; return; fi
	if [ -n "$2" ]; then printf '%s [%s]：' "$1" "$2"; else printf '%s：' "$1"; fi
	IFS= read -r REPLY <"$TTY" || true
	[ -n "$REPLY" ] || REPLY="$2"
}

confirm() { # confirm 提示（默认是）
	[ "$YES" = 1 ] && return 0
	[ -n "$TTY" ] || return 1
	printf '%s [Y/n]：' "$1"
	IFS= read -r a <"$TTY" || a=n
	case "$a" in "" | y | Y | yes | 是) return 0 ;; *) return 1 ;; esac
}

has_systemd() {
	[ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1 &&
		systemctl --user show-environment >/dev/null 2>&1
}

conf_get() { # conf_get 键 → 配置文件里的值
	[ -f "$DIR/kernel.conf" ] || return 0
	sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" "$DIR/kernel.conf" | tail -n 1 | sed 's/[[:space:]]*$//'
}

# ---- 后台运行（没有 systemd 时用 pid 文件管） ----

running_pid() {
	[ -f "$DIR/kernel.pid" ] || return 1
	pid=$(cat "$DIR/kernel.pid" 2>/dev/null) || return 1
	[ -n "$pid" ] && kill -0 "$pid" 2>/dev/null || return 1
	# pid 可能被别的程序复用了，确认确实是这个内核
	case "$(readlink "/proc/$pid/exe" 2>/dev/null)" in "$DIR/nano-kernel"*) echo "$pid" ;; *) return 1 ;; esac
}

stop_kernel() {
	if [ -f "$UNIT_DIR/$UNIT" ] && has_systemd; then
		systemctl --user stop "$UNIT" 2>/dev/null || true
	fi
	if pid=$(running_pid); then
		kill "$pid" 2>/dev/null || true
		i=0
		while kill -0 "$pid" 2>/dev/null && [ $i -lt 30 ]; do sleep 0.1; i=$((i + 1)); done
	fi
	rm -f "$DIR/kernel.pid"
}

# ---- 卸载 ----

if [ "$UNINSTALL" = 1 ]; then
	[ -d "$DIR" ] || die "$DIR 下面没有安装内核"
	confirm "停止内核并删除 $DIR（包括配置和配对口令）？" || { say "已取消"; exit 0; }
	stop_kernel
	if [ -f "$UNIT_DIR/$UNIT" ]; then
		has_systemd && systemctl --user disable "$UNIT" >/dev/null 2>&1 || true
		rm -f "$UNIT_DIR/$UNIT"
		has_systemd && systemctl --user daemon-reload || true
	fi
	for f in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
		if [ -f "$f" ] && grep -qF "$PROFILE_MARK" "$f"; then
			# 全部行都被去掉时 grep 返回 1，不算错
			{ grep -vF "$PROFILE_MARK" "$f" || true; } >"$f.nano-tmp"
			cat "$f.nano-tmp" >"$f" && rm -f "$f.nano-tmp"
		fi
	done
	rm -f "$HOME/.local/bin/nano-kernel-ctl"
	rm -rf "$DIR"
	say "已卸载。网关网页「设备」里配对过的这台电脑可以一并移除。"
	exit 0
fi

[ "$(uname -s)" = Linux ] || die "内核只支持 Linux（Windows 上请在 WSL2 里运行这个脚本）"

# ---- 源码：单文件安装包里带着，否则用脚本所在的源码目录 ----

WORK=""
cleanup() { [ -z "$WORK" ] || rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

if grep -q '^__NANO_PAYLOAD__$' "$0" 2>/dev/null; then
	WORK=$(mktemp -d)
	sed '1,/^__NANO_PAYLOAD__$/d' "$0" | base64 -d | tar xzf - -C "$WORK" || die "安装包损坏，重新拷一份"
	SRC="$WORK"
else
	SRC=$(cd "$(dirname "$0")/.." && pwd)
	[ -f "$SRC/kernel/Makefile" ] || die "找不到内核源码（$SRC/kernel）。在源码目录里运行，或者用 make installer 生成的单文件安装包"
fi
VERSION=$(cat "$SRC/kernel/VERSION" 2>/dev/null || (cd "$SRC" && git describe --always --dirty 2>/dev/null) || echo dev)

# ---- 编译工具 ----

CC_BIN=""
for c in "${CC:-}" cc gcc clang; do
	if [ -n "$c" ] && command -v "$c" >/dev/null 2>&1; then CC_BIN="$c"; break; fi
done
MISSING=""
[ -n "$CC_BIN" ] || MISSING="C 编译器"
command -v make >/dev/null 2>&1 || MISSING="$MISSING make"

if [ -n "$MISSING" ]; then
	step "缺少编译工具：$MISSING"
	if command -v apt-get >/dev/null 2>&1; then PKG="apt-get update && apt-get install -y build-essential"
	elif command -v dnf >/dev/null 2>&1; then PKG="dnf install -y gcc make"
	elif command -v yum >/dev/null 2>&1; then PKG="yum install -y gcc make"
	elif command -v pacman >/dev/null 2>&1; then PKG="pacman -S --needed --noconfirm gcc make"
	elif command -v zypper >/dev/null 2>&1; then PKG="zypper install -y gcc make"
	elif command -v apk >/dev/null 2>&1; then PKG="apk add build-base linux-headers"
	else die "认不出这台机器的包管理器，请先自己装好 gcc 和 make 再运行"
	fi
	SUDO=""
	[ "$(id -u)" = 0 ] || SUDO="sudo"
	[ -z "$SUDO" ] || command -v sudo >/dev/null 2>&1 || die "需要 root 权限安装：$PKG"
	confirm "用下面的命令安装？  ${SUDO:+$SUDO }sh -c '$PKG'" || die "没有编译工具，装不了。手动装好后再运行"
	$SUDO sh -c "$PKG" || die "安装编译工具失败"
	CC_BIN=cc
	command -v cc >/dev/null 2>&1 || CC_BIN=gcc
fi

# ---- 编译 ----

step "编译内核（$VERSION）"
BUILD="$SRC/kernel"
if [ -z "$WORK" ]; then
	# 源码目录里编译不弄脏源码树：拷一份到临时目录
	WORK=$(mktemp -d)
	mkdir -p "$WORK/kernel"
	(cd "$SRC/kernel" && tar cf - Makefile src third_party) | tar xf - -C "$WORK/kernel"
	BUILD="$WORK/kernel"
fi
make -s -C "$BUILD" CC="$CC_BIN" nano-kernel nano-exec >"$BUILD/build.log" 2>&1 || {
	tail -n 20 "$BUILD/build.log"
	die "编译失败（上面是最后的输出）"
}
say "完成"

# ---- 配置 ----

UPGRADE=0
[ -f "$DIR/kernel.conf" ] && UPGRADE=1

normalize_gateway() {
	case "$1" in
	unix:* | tcp:*) echo "$1" ;;
	*:*) echo "tcp:$1" ;;
	*) echo "tcp:$1:7700" ;;
	esac
}

if [ "$UPGRADE" = 1 ]; then
	step "升级 $DIR（保留配置和配对口令）"
	[ -z "$GATEWAY" ] || GATEWAY=$(normalize_gateway "$GATEWAY")
	[ -n "$GATEWAY" ] || GATEWAY=$(conf_get gateway)
	[ -n "$NAME" ] || NAME=$(conf_get node)
else
	step "安装到 $DIR"
	if [ -z "$GATEWAY" ]; then
		[ -n "$TTY" ] && [ "$YES" = 0 ] || die "没有网关地址：加上 --gateway 地址（例如 --gateway 192.168.1.2:7700）"
		say "网关的地址就是运行网关的那台机器的 IP（或域名）加电脑接入端口，默认端口 7700。"
		say "在网关网页的「设备」页面能看到，要先在那里启用电脑接入。"
		while [ -z "$GATEWAY" ]; do ask "网关地址" ""; GATEWAY="$REPLY"; done
	fi
	GATEWAY=$(normalize_gateway "$GATEWAY")
	if [ -z "$NAME" ]; then
		ask "这台电脑的名字（在网关里显示，选工作目录时用）" "$(hostname 2>/dev/null || echo my-pc)"
		NAME="$REPLY"
	fi
fi
case "$NAME" in
"" | *[[:space:]:/]*) die "名字不能为空，不能有空格、冒号和斜杠：$NAME" ;;
本机 | local | LOCAL) die "“$NAME”是网关自己那台机器的叫法，换个名字" ;;
esac

stop_kernel
mkdir -p "$DIR/skills"
install -m 755 "$BUILD/nano-kernel" "$DIR/nano-kernel.new"
install -m 755 "$BUILD/nano-exec" "$DIR/nano-exec.new"
mv -f "$DIR/nano-kernel.new" "$DIR/nano-kernel"
mv -f "$DIR/nano-exec.new" "$DIR/nano-exec"
echo "$VERSION" >"$DIR/VERSION"

if [ "$UPGRADE" = 1 ]; then
	# 只改网关地址和名字两行，别的配置原样留着
	sed -e "s|^[[:space:]]*gateway[[:space:]]*=.*|gateway = $GATEWAY|" \
		-e "s|^[[:space:]]*node[[:space:]]*=.*|node = $NAME|" "$DIR/kernel.conf" >"$DIR/kernel.conf.new"
	grep -q '^[[:space:]]*node[[:space:]]*=' "$DIR/kernel.conf.new" || echo "node = $NAME" >>"$DIR/kernel.conf.new"
	mv -f "$DIR/kernel.conf.new" "$DIR/kernel.conf"
else
	umask 077
	cat >"$DIR/kernel.conf" <<EOF
# nano-agent 内核配置（由安装脚本生成，可以改；再运行安装脚本会保留）
# 相对路径以本文件所在的目录为基准。

# 网关地址
gateway = $GATEWAY
# 这台电脑在网关里的名字
node = $NAME

# 没有写口令：第一次连上网关时请求配对，在网关网页的「设备」里核对确认码、点允许，
# 网关发给这台电脑的专用口令会存在本文件旁边的 paired.token。

# 技能目录：每个技能一个子目录，里面是 SKILL.md 和可选的脚本、资料
skills = skills

max_steps = 16
run_timeout = 600
approval_timeout = 300
max_output = 65536
EOF
	umask 022
fi

"$DIR/nano-kernel" -c "$DIR/kernel.conf" -t >/dev/null 2>"$DIR/check.log" || {
	cat "$DIR/check.log"
	die "配置检查没通过：$DIR/kernel.conf"
}
rm -f "$DIR/check.log"

# 控制脚本：启动、停止、看日志。没有 systemd 的机器（比如没开 systemd 的 WSL）靠它在后台跑
cat >"$DIR/nano-kernel-ctl" <<'EOF'
#!/bin/sh
# nano-agent 内核：nano-kernel-ctl start|stop|restart|status|log
DIR=$(cd "$(dirname "$(readlink -f "$0")")" && pwd)
UNIT=nano-kernel.service
use_systemd() {
	[ -f "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/$UNIT" ] && [ -d /run/systemd/system ] &&
		systemctl --user show-environment >/dev/null 2>&1
}
pid() {
	p=$(cat "$DIR/kernel.pid" 2>/dev/null) && [ -n "$p" ] && kill -0 "$p" 2>/dev/null &&
		case "$(readlink "/proc/$p/exe" 2>/dev/null)" in "$DIR/nano-kernel"*) echo "$p" ;; *) false ;; esac
}
case "${1:-status}" in
start)
	if use_systemd; then exec systemctl --user start "$UNIT"; fi
	pid >/dev/null && { [ "${2:-}" = --quiet ] || echo "内核已经在运行"; exit 0; }
	nohup "$DIR/nano-kernel" -c "$DIR/kernel.conf" </dev/null >/dev/null 2>>"$DIR/kernel.log" &
	echo $! >"$DIR/kernel.pid"
	[ "${2:-}" = --quiet ] || echo "内核已在后台启动，日志：$DIR/kernel.log"
	;;
stop)
	if use_systemd; then exec systemctl --user stop "$UNIT"; fi
	if p=$(pid); then kill "$p"; echo "已停止"; else echo "内核没有在运行"; fi
	rm -f "$DIR/kernel.pid"
	;;
restart) "$0" stop >/dev/null; sleep 0.3; exec "$0" start ;;
status)
	if use_systemd; then systemctl --user --no-pager status "$UNIT"; exit $?; fi
	if p=$(pid); then echo "运行中（pid $p）"; else echo "没有运行"; exit 3; fi
	;;
log) exec tail -n 50 -F "$DIR/kernel.log" ;;
*) echo "用法：nano-kernel-ctl start|stop|restart|status|log"; exit 2 ;;
esac
EOF
chmod 755 "$DIR/nano-kernel-ctl"
if [ -d "$HOME/.local/bin" ] || mkdir -p "$HOME/.local/bin" 2>/dev/null; then
	ln -sf "$DIR/nano-kernel-ctl" "$HOME/.local/bin/nano-kernel-ctl"
fi

if [ "$SERVICE" = 0 ]; then
	say ""
	say "装好了，没有启动。启动：$DIR/nano-kernel-ctl start"
	exit 0
fi

# ---- 开机自启并启动 ----

LOG="$DIR/kernel.log"
: >>"$LOG"
LOG_START=$(wc -c <"$LOG")

if has_systemd; then
	step "注册 systemd 用户服务（开机自启，掉线自动重启）"
	mkdir -p "$UNIT_DIR"
	cat >"$UNIT_DIR/$UNIT" <<EOF
[Unit]
Description=nano-agent 内核
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$DIR/nano-kernel -c $DIR/kernel.conf
Restart=always
RestartSec=5
StandardOutput=append:$LOG
StandardError=append:$LOG

[Install]
WantedBy=default.target
EOF
	systemctl --user daemon-reload
	systemctl --user enable "$UNIT" >/dev/null 2>&1
	systemctl --user restart "$UNIT"
	if command -v loginctl >/dev/null 2>&1 && [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null)" != yes ]; then
		say "提示：现在只有登录后内核才会运行。想让它开机就运行（不用登录），执行："
		say "  sudo loginctl enable-linger $(id -un)"
	fi
else
	step "这台机器没有 systemd，用后台进程运行"
	"$DIR/nano-kernel-ctl" start
	if ! grep -qsF "$PROFILE_MARK" "$HOME/.profile" &&
		confirm "每次登录（打开终端）时自动启动内核？会在 ~/.profile 里加一行"; then
		printf '"%s" start --quiet %s\n' "$DIR/nano-kernel-ctl" "$PROFILE_MARK" >>"$HOME/.profile"
	fi
	case "$(uname -r)" in *[Mm]icrosoft*)
		say "提示：WSL 关掉所有窗口一段时间后会停下来，内核也跟着停。想一直在线，可以在 /etc/wsl.conf 里开启 systemd 后重新运行本脚本。" ;;
	esac
fi

# ---- 等它连上网关 ----

step "连接网关 $GATEWAY"
new_log() { tail -c +"$((LOG_START + 1))" "$LOG" 2>/dev/null; }
code=""
i=0
while [ $i -lt 150 ]; do
	out=$(new_log)
	case "$out" in
	*网关已接受连接*) say "已接入网关。打开网关网页，在会话里选这台电脑上的工作目录就能开始工作。"; exit 0 ;;
	*没有同意这台电脑接入*) die "网关拒绝了这台电脑。要重新申请：nano-kernel-ctl restart" ;;
	*网关报错：*)
		if [ -z "$code" ]; then
			why=$(printf '%s\n' "$out" | sed -n 's/.*网关报错：//p' | tail -n 1)
			say "网关没有接受：$why"
			say "内核会在后台隔一阵重试。改名字：重新运行本脚本加 --name 新名字"
			exit 1
		fi ;;
	esac
	if [ -z "$code" ]; then
		code=$(printf '%s\n' "$out" | sed -n 's/.*核对确认码 \([0-9]*\).*/\1/p' | tail -n 1)
		if [ -n "$code" ]; then
			say ""
			say "  确认码：$(echo "$code" | cut -c1-3) $(echo "$code" | cut -c4-)"
			say ""
			say "打开网关网页的「设备」页面，核对确认码一致后点「允许接入」。"
			say "（可以 Ctrl-C 先走开，内核会在后台一直等着。）"
			i=0
		fi
	fi
	# 还没收到确认码时只等 15 秒，收到后等 5 分钟让人去点
	if [ -z "$code" ] && [ $i -ge 30 ]; then break; fi
	if [ -n "$code" ] && [ $i -ge 600 ]; then break; fi
	sleep 0.5
	i=$((i + 1))
done

if [ -n "$code" ]; then
	say "还没等到网关确认。内核在后台继续等，点了允许就会接上。"
else
	say "暂时没连上网关，内核会在后台一直重试。最近的日志："
	new_log | tail -n 5
	say "检查：网关地址对不对（$GATEWAY）、网关网页「设备」里电脑接入有没有启用、防火墙是否放行端口。"
	say "看日志：nano-kernel-ctl log"
fi
exit 0
