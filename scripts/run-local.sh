#!/usr/bin/env bash
# 本机一键运行：后台起内核，前台起网关（命令行聊天）。Ctrl-C 或 Ctrl-D 退出时一起停掉。
# 日志在 data/kernel.log 和 data/gateway.log。
set -euo pipefail
cd "$(dirname "$0")/.."
[ -f gateway.toml ] || { echo "缺少 gateway.toml，先从 gateway.example.toml 复制一份改好"; exit 1; }
[ -f kernel.conf ] || { echo "缺少 kernel.conf，先从 kernel.example.conf 复制一份改好"; exit 1; }
make -s all
mkdir -p data

# 内核在后台跑，出错只会写进日志，所以先检查一遍配置和工具，有错直接显示出来
if ! out=$(./kernel/nano-kernel -c kernel.conf -t 2>&1); then
  echo "$out"
  exit 1
fi

# 内核晚一点启动，等网关先开始监听（就算没等到，内核也会自己重试）
( sleep 0.5; exec ./kernel/nano-kernel -c kernel.conf ) 2>>data/kernel.log &
KPID=$!
# Ctrl-C 时内核自己也收到信号先退出了，kill 失败不算错
trap 'kill $KPID 2>/dev/null || true; wait $KPID 2>/dev/null || true' EXIT

echo "nano-agent 已启动，直接输入消息聊天，/help 查看命令，Ctrl-C 退出。"
if ! ./gateway/nano-gateway -c gateway.toml 2>>data/gateway.log; then
  echo "网关异常退出，最近的日志（data/gateway.log）："
  tail -n 5 data/gateway.log
  exit 1
fi
