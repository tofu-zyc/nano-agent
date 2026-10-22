#!/bin/sh
# nano-agent 一行安装：从 GitHub Release 下载对应系统和架构的包，校验 SHA256，再运行包里的安装脚本。
#
#   curl -fsSL https://github.com/tofu-zyc/nano-agent/releases/latest/download/install.sh | sh
#       装网关（Linux）。再运行一次就是升级，配置和数据保留。
#   curl -fsSL …/install.sh | sh -s -- --kernel --gateway tcp:192.168.1.2:7700
#       只装内核（Linux / macOS），接到已有的网关上
#   curl -fsSL …/install.sh | sh -s -- --import nano-agent-export-xxx.tar.gz [--old-local-as 旧机器名]
#       装网关，并导入另一台机器 nano-gateway export 的配置和数据
#
# 选项：
#   --kernel             只装内核（macOS 上默认就是）
#   --gateway 地址       内核要连的网关地址
#   --name 名字          这台电脑的名字（内核）
#   --dir 目录           内核装到哪里（默认 ~/.nano-agent/kernel）
#   --no-service         不注册开机自启，也不启动
#   --import 文件        装网关后导入导出包
#   --old-local-as 名字  导入时，旧网关“本机”上的工作目录改记到这个机器名下
#   --version x.y.z      装指定版本（默认最新）
#   --yes                不提问
#
# 环境变量 NANO_DOWNLOAD_URL 可以换下载地址（镜像），默认 https://github.com/tofu-zyc/nano-agent/releases
set -eu

BASE=${NANO_DOWNLOAD_URL:-https://github.com/tofu-zyc/nano-agent/releases}
BASE=${BASE%/}
MODE=""
VERSION=""
GW_ARGS=""
K_ARGS=""
GW_ONLY=""   # 用了只有装网关才有的选项
K_ONLY=""    # 用了只有装内核才有的选项

say() { printf '%s\n' "$*"; }
die() { printf '\033[31m错误：%s\033[0m\n' "$*" >&2; exit 1; }
# 把一个参数安全地加进参数串（之后用 eval set -- 展开）
quote() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }
add_gw() { GW_ARGS="$GW_ARGS $(quote "$1")"; }
add_k() { K_ARGS="$K_ARGS $(quote "$1")"; }
need_value() { [ $# -ge 2 ] || die "$1 后面要跟一个值"; }

while [ $# -gt 0 ]; do
	case "$1" in
	--kernel) MODE=kernel ;;
	--gateway) need_value "$@"; add_k --gateway; add_k "$2"; K_ONLY=$1; shift ;;
	--name) need_value "$@"; add_k --name; add_k "$2"; K_ONLY=$1; shift ;;
	--dir) need_value "$@"; add_k --dir; add_k "$2"; K_ONLY=$1; shift ;;
	--no-service) add_k --no-service; K_ONLY=$1 ;;
	--import)
		need_value "$@"
		[ -f "$2" ] || die "找不到导出包：$2"
		add_gw --import; add_gw "$(cd "$(dirname "$2")" && pwd)/$(basename "$2")"; GW_ONLY=$1; shift ;;
	--old-local-as) need_value "$@"; add_gw --old-local-as; add_gw "$2"; GW_ONLY=$1; shift ;;
	--version) need_value "$@"; VERSION=${2#v}; shift ;;
	--yes | -y) add_gw --yes; add_k --yes ;;
	-h | --help) sed -n '2,26p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "不认识的选项：$1（--help 看用法）" ;;
	esac
	shift
done

case "$(uname -s)" in
Linux) OS=linux ;;
Darwin) OS=macos ;;
*) die "不支持这个系统：$(uname -s)（Windows 用 install.ps1）" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) ARCH=x86_64 ;;
aarch64 | arm64) ARCH=aarch64 ;;
*) die "不支持这个 CPU 架构：$(uname -m)" ;;
esac
if [ -z "$MODE" ]; then
	if [ "$OS" = macos ]; then
		say "macOS 上目前只提供内核（网关请装在 Linux 上），装内核"
		MODE=kernel
	else
		MODE=gateway
	fi
fi
[ "$MODE" = kernel ] || [ "$OS" = linux ] || die "网关目前只有 Linux 版"
[ "$MODE" = gateway ] || [ -z "$GW_ONLY" ] || die "$GW_ONLY 是装网关用的，不能和 --kernel 一起用"
[ "$MODE" = kernel ] || [ -z "$K_ONLY" ] || die "$K_ONLY 是装内核用的，要加 --kernel"

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	die "需要 curl 或 wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "需要 sha256sum 或 shasum 来校验下载的包"
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

if [ -z "$VERSION" ]; then
	fetch "$BASE/latest/download/VERSION" "$TMP/VERSION" || die "查不到最新版本（$BASE）；网络不通的话可以设 NANO_DOWNLOAD_URL 换镜像"
	VERSION=$(tr -d ' \r\n' <"$TMP/VERSION")
	VERSION=${VERSION#v}
fi
case "$VERSION" in
[0-9]*.[0-9]*.[0-9]*) ;;
*) die "版本号看不懂：$VERSION" ;;
esac

if [ "$MODE" = kernel ]; then
	PKG="nano-kernel-$VERSION-$OS-$ARCH"
else
	PKG="nano-gateway-$VERSION-$OS-$ARCH"
fi
URL="$BASE/download/v$VERSION"
say "下载 $PKG.tar.gz（$URL）…"
fetch "$URL/$PKG.tar.gz" "$TMP/$PKG.tar.gz" || die "下载失败：$URL/$PKG.tar.gz"
fetch "$URL/SHA256SUMS" "$TMP/SHA256SUMS" || die "下载失败：$URL/SHA256SUMS"
want=$(awk -v f="$PKG.tar.gz" '$2 == f || $2 == "*" f { print $1 }' "$TMP/SHA256SUMS")
[ -n "$want" ] || die "SHA256SUMS 里没有 $PKG.tar.gz"
got=$(sha256 "$TMP/$PKG.tar.gz")
[ "$want" = "$got" ] || die "校验失败：$PKG.tar.gz 的 SHA256 不对，可能下载坏了或者被篡改"
say "校验通过"
tar -xzf "$TMP/$PKG.tar.gz" -C "$TMP"
[ -d "$TMP/$PKG" ] || die "包里没有 $PKG/"

if [ "$MODE" = kernel ]; then
	eval "set -- $K_ARGS"
	sh "$TMP/$PKG/install-kernel.sh" "$@"
else
	eval "set -- $GW_ARGS"
	bash "$TMP/$PKG/install-gateway.sh" "$@"
fi
