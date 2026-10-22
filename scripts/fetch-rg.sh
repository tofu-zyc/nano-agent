#!/bin/sh
# 下载固定版本的 ripgrep（Grep 工具优先用执行器旁边的 rg）。放到 build/tools/rg.exe。
# Linux / macOS 的 rg 由发布包阶段再处理。
set -eu
VERSION=14.1.1
SHA256=d0f534024c42afd6cb4d38907c25cd2b249b79bbe6cc1dbee8e3e37c2b6e25a1
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DEST="$ROOT/build/tools/rg.exe"
URL="https://github.com/BurntSushi/ripgrep/releases/download/$VERSION/ripgrep-$VERSION-x86_64-pc-windows-msvc.zip"

if [ -x "$DEST" ]; then
    echo "rg.exe 已经在了：$DEST"
    exit 0
fi
mkdir -p "$ROOT/build/tools"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
echo "下载 ripgrep $VERSION（Windows x86_64）…"
curl -fsSL -o "$TMP/rg.zip" "$URL"
echo "$SHA256  $TMP/rg.zip" | sha256sum -c -
python3 - "$TMP/rg.zip" "$TMP" <<'PY'
import sys, zipfile
zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])
PY
find "$TMP" -name rg.exe -type f -exec cp {} "$DEST" \;
chmod +x "$DEST"
echo "装好了：$DEST"
