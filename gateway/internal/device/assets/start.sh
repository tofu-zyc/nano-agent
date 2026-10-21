#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
conf="$root/kernel.conf"
umask 077
if [ "${1:-}" != "--saved" ]; then
  current=$(sed -n 's/^gateway = //p' "$conf")
  printf '网关地址 [%s]：' "$current"; read -r address
  address=${address:-$current}
  printf '连接口令（留空使用包内口令）：'; read -r token
  if [ -z "$token" ]; then token=$(sed -n 's/^token = //p' "$conf"); fi
  printf '这台电脑的名字 [%s]：' "$(hostname)"; read -r name
  name=${name:-$(hostname)}
  case "$address$token$name" in *'
'*) echo '输入中不能包含换行' >&2; exit 1;; esac
  printf 'gateway = %s\ntoken = %s\nnode = %s\nexec = bin/nano-exec\n' "$address" "$token" "$name" > "$conf.tmp"
  mv "$conf.tmp" "$conf"
fi
sh "$root/ensure-engine.sh" "$root"
echo '正在连接网关；Ctrl-C 停止。以后可用 ./start.sh --saved 沿用配置。'
cd "$root"
exec "$root/bin/nano-kernel" -c "$conf"
