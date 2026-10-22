#!/bin/sh
# 下载固定版本的 zig（只用它的 cc 交叉编译），放到 build/tools/zig，不需要 sudo。
# 已经有的话直接跳过。
set -eu
VERSION=0.13.0
SHA256=d45312e61ebcc48032b77bc4cf7fd6915c11fa16e4aad116b66c9468211230ea
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DEST="$ROOT/build/tools/zig"
URL="https://ziglang.org/download/$VERSION/zig-linux-x86_64-$VERSION.tar.xz"

if [ -x "$DEST/zig" ]; then
    echo "zig $VERSION 已经在了：$DEST"
    exit 0
fi
mkdir -p "$ROOT/build/tools"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
echo "下载 zig $VERSION…"
curl -fsSL -o "$TMP/zig.tar.xz" "$URL"
echo "$SHA256  $TMP/zig.tar.xz" | sha256sum -c -
tar -xJf "$TMP/zig.tar.xz" -C "$TMP"
rm -rf "$DEST"
mv "$TMP/zig-linux-x86_64-$VERSION" "$DEST"
"$DEST/zig" version
echo "装好了：$DEST/zig"
