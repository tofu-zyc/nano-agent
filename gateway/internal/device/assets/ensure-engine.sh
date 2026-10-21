#!/bin/sh
set -eu
root=$1
mkdir -p "$root/skills"
tar xzf "$root/skills.tar.gz" -C "$root/skills"
chmod 700 "$root/bin/"* 2>/dev/null || true
if [ -x "$root/bin/nano-kernel" ] && "$root/bin/nano-kernel" -t -c "$root/kernel.conf" >/dev/null 2>&1; then
  exit 0
fi
if ! command -v make >/dev/null || ! command -v cc >/dev/null; then
  echo '当前系统不能运行包内二进制。请安装 C 编译器和 make（Ubuntu/Debian: sudo apt install build-essential；Fedora: sudo dnf install gcc make），然后重试。' >&2
  exit 1
fi
mkdir -p "$root/source" "$root/bin"
tar xzf "$root/kernel-src.tar.gz" -C "$root/source"
make -s -C "$root/source/kernel"
cp "$root/source/kernel/nano-kernel" "$root/source/kernel/nano-exec" "$root/source/kernel/nano-exec-src.tar.gz" "$root/bin/"
if [ -f "$root/source/kernel/nano-exec-static" ]; then cp "$root/source/kernel/nano-exec-static" "$root/bin/"; fi
"$root/bin/nano-kernel" -t -c "$root/kernel.conf"
