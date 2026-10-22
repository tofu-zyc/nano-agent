#!/bin/sh
# 下载固定版本的 ripgrep（Grep 工具优先用执行器旁边的 rg）。
#
#   scripts/fetch-rg.sh          只要 Windows 的 rg.exe（WSL 里测 Windows 执行器用）
#   scripts/fetch-rg.sh --all    发布包要的全部平台：Linux x86_64/arm64、macOS x86_64/arm64、Windows x86_64
#
# 下载到 build/tools/，不提交。版本和 sha256 固定，下载后校验。
set -eu
VERSION=14.1.1
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DEST="$ROOT/build/tools/rg.exe"
ALL=0
[ "${1:-}" = "--all" ] && ALL=1

mkdir -p "$ROOT/build/tools" "$ROOT/build/tools/rg"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

get_tar() { # get_tar 名字 文件名 地址 校验值
	name=$1; file=$2; url=$3; sha=$4
	out="$ROOT/build/tools/rg/$name"
	if [ -x "$out" ]; then echo "$name 已经在了"; return; fi
	rm -rf "$TMP"/*
	echo "下载 ripgrep $VERSION（$name）…"
	curl -fsSL -o "$TMP/$file" "$url"
	echo "$sha  $TMP/$file" | sha256sum -c - >/dev/null
	tar xzf "$TMP/$file" -C "$TMP"
	find "$TMP" -name rg -type f -exec cp {} "$out" \;
	chmod +x "$out"
}

get_zip() { # get_zip 名字 文件名 地址 校验值 压缩包里的文件名
	name=$1; file=$2; url=$3; sha=$4; inner=$5
	out="$ROOT/build/tools/rg/$name"
	if [ -x "$out" ]; then echo "$name 已经在了"; return; fi
	rm -rf "$TMP"/*
	echo "下载 ripgrep $VERSION（$name）…"
	curl -fsSL -o "$TMP/$file" "$url"
	echo "$sha  $TMP/$file" | sha256sum -c - >/dev/null
	python3 - "$TMP/$file" "$TMP" <<'PY'
import sys, zipfile
zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])
PY
	find "$TMP" -name "$inner" -type f -exec cp {} "$out" \;
	chmod +x "$out"
}

if [ "$ALL" = 0 ]; then
	if [ -x "$DEST" ]; then echo "rg.exe 已经在了：$DEST"; exit 0; fi
	get_zip windows-x86_64.exe rg.zip \
		"https://github.com/BurntSushi/ripgrep/releases/download/$VERSION/ripgrep-$VERSION-x86_64-pc-windows-msvc.zip" \
		d0f534024c42afd6cb4d38907c25cd2b249b79bbe6cc1dbee8e3e37c2b6e25a1 rg.exe
	cp "$ROOT/build/tools/rg/windows-x86_64.exe" "$DEST"
	echo "装好了：$DEST"
	exit 0
fi

get_zip windows-x86_64.exe rg.zip \
	"https://github.com/BurntSushi/ripgrep/releases/download/$VERSION/ripgrep-$VERSION-x86_64-pc-windows-msvc.zip" \
	d0f534024c42afd6cb4d38907c25cd2b249b79bbe6cc1dbee8e3e37c2b6e25a1 rg.exe
cp "$ROOT/build/tools/rg/windows-x86_64.exe" "$DEST"

get_tar linux-x86_64 rg-linux-x86_64.tar.gz \
	"https://github.com/BurntSushi/ripgrep/releases/download/$VERSION/ripgrep-$VERSION-x86_64-unknown-linux-musl.tar.gz" \
	4cf9f2741e6c465ffdb7c26f38056a59e2a2544b51f7cc128ef28337eeae4d8e
get_tar linux-aarch64 rg-linux-aarch64.tar.gz \
	"https://github.com/BurntSushi/ripgrep/releases/download/$VERSION/ripgrep-$VERSION-aarch64-unknown-linux-gnu.tar.gz" \
	c827481c4ff4ea10c9dc7a4022c8de5db34a5737cb74484d62eb94a95841ab2f
get_tar macos-x86_64 rg-macos-x86_64.tar.gz \
	"https://github.com/BurntSushi/ripgrep/releases/download/$VERSION/ripgrep-$VERSION-x86_64-apple-darwin.tar.gz" \
	fc87e78f7cb3fea12d69072e7ef3b21509754717b746368fd40d88963630e2b3
get_tar macos-aarch64 rg-macos-aarch64.tar.gz \
	"https://github.com/BurntSushi/ripgrep/releases/download/$VERSION/ripgrep-$VERSION-aarch64-apple-darwin.tar.gz" \
	24ad76777745fbff131c8fbc466742b011f925bfa4fffa2ded6def23b5b937be

echo "装好了：$ROOT/build/tools/rg/"
