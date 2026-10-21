#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
conf="$root/kernel.conf"
umask 077
if [ "${1:-}" != "--saved" ]; then
  current=$(sed -n 's/^gateway = //p' "$conf")
  printf '网关地址 [%s]：' "$current"; read -r address
  address=${address:-$current}
  node=$(sed -n 's/^node = //p' "$conf")
  node=${node:-$(hostname)}
  printf '这台电脑的名字 [%s]：' "$node"; read -r name
  name=${name:-$node}
  case "$address$name" in *'
'*) echo '输入中不能包含换行' >&2; exit 1;; esac
  token=$(sed -n 's/^token = //p' "$conf")
  {
    printf 'gateway = %s\nnode = %s\nexec = bin/nano-exec\n' "$address" "$name"
    # 一般不写口令：第一次连接时向网关请求配对，口令由网关发给这台电脑，存在 paired.token
    if [ -n "$token" ]; then printf 'token = %s\n' "$token"; fi
  } > "$conf.tmp"
  mv "$conf.tmp" "$conf"
fi
sh "$root/ensure-engine.sh" "$root"
if [ ! -s "$root/paired.token" ] && ! grep -q '^token = ' "$conf"; then
  echo '第一次连接：在网关网页的「设备」页面核对确认码，点「允许」。'
fi
echo '正在连接网关；Ctrl-C 停止。以后可用 sh start.sh --saved 沿用配置。'
cd "$root"
exec "$root/bin/nano-kernel" -c "$conf"
