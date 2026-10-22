#!/usr/bin/env bash
# 本机一键运行：编译，前台起网关（网关自己会在本机启动一个内核）。Ctrl-C 停掉。
# 聊天另开一个终端运行 make chat，或者用网页。日志在 data/gateway.log 和 data/kernel.log。
set -euo pipefail
cd "$(dirname "$0")/.."
CFG=${NANO_HOME:+$NANO_HOME/gateway.toml}
CFG=${CFG:-gateway.toml}
LOGDIR=${NANO_HOME:+$NANO_HOME/data}
LOGDIR=${LOGDIR:-data}
[ -f "$CFG" ] || { echo "缺少 $CFG，先从 gateway.example.toml 复制一份改好"; exit 1; }
make -s all
mkdir -p "$LOGDIR"

echo "nano-agent 已启动，Ctrl-C 停止。日志在 $LOGDIR/gateway.log 和 $LOGDIR/kernel.log。"
if ! ./gateway/nano-gateway -c "$CFG" </dev/null 2>>"$LOGDIR/gateway.log"; then
  echo "网关异常退出，最近的日志（$LOGDIR/gateway.log）："
  tail -n 5 "$LOGDIR/gateway.log"
  exit 1
fi
