#!/bin/sh
# 下载固定版本的 gitleaks（推送前扫密钥），放到 build/tools/gitleaks，不需要 sudo。
# 已经有的话直接跳过。
set -eu
VERSION=8.21.2
SHA256=5bc41815076e6ed6ef8fbecc9d9b75bcae31f39029ceb55da08086315316e3ba
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DEST="$ROOT/build/tools/gitleaks"
URL="https://github.com/gitleaks/gitleaks/releases/download/v$VERSION/gitleaks_${VERSION}_linux_x64.tar.gz"

if [ -x "$DEST" ]; then
    echo "gitleaks $VERSION 已经在了：$DEST"
    exit 0
fi
mkdir -p "$ROOT/build/tools"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
echo "下载 gitleaks $VERSION…"
curl -fsSL -o "$TMP/gitleaks.tar.gz" "$URL"
echo "$SHA256  $TMP/gitleaks.tar.gz" | sha256sum -c -
tar -xzf "$TMP/gitleaks.tar.gz" -C "$TMP" gitleaks
mv "$TMP/gitleaks" "$DEST"
echo "装好了：$DEST"
