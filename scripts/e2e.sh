#!/usr/bin/env bash
# 端到端测试：真实的内核和网关进程，模型用 mock，聊天走命令行通道。
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d)
cleanup() {
  exec 3>&- 2>/dev/null || true
  kill $(jobs -p) 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT
make -s -C "$ROOT" all >/dev/null

cat > "$TMP/gateway.toml" <<CONF
[kernel]
listen = "unix:$TMP/k.sock"
token = "e2e"
[llm]
provider = "mock"
[persona]
dir = "$ROOT/personas"
[session]
data_dir = "$TMP/data"
[cli]
enabled = true
CONF
cat > "$TMP/kernel.conf" <<CONF
gateway = unix:$TMP/k.sock
token = e2e
tools_dir = $ROOT/tools
approval_timeout = 5
admin = cli:local
CONF

mkfifo "$TMP/in"
"$ROOT/gateway/nano-gateway" -c "$TMP/gateway.toml" <"$TMP/in" >"$TMP/out" 2>"$TMP/gateway.log" &
exec 3>"$TMP/in"   # 一直开着写端，网关的 stdin 就不会 EOF
"$ROOT/kernel/nano-kernel" -c "$TMP/kernel.conf" 2>"$TMP/kernel.log" &
for _ in $(seq 50); do grep -q '内核已连接' "$TMP/gateway.log" 2>/dev/null && break; sleep 0.1; done

pass=0 fail=0
send() { echo "$1" >&3; }
expect() {
  local desc=$1 want=$2
  for _ in $(seq 50); do
    if grep -qF -- "$want" "$TMP/out"; then echo "  ok   $desc"; pass=$((pass + 1)); return; fi
    sleep 0.1
  done
  echo "  FAIL $desc（没等到：$want）"; fail=$((fail + 1))
}

send "你好";                                      expect "普通对话" "（mock）收到：你好"
send '!call get_time {}';                         expect "调用本地工具" "（mock）工具结果：20"
send '!call run_shell {"cmd":"echo e2e-$((6*7))"}'; expect "高危工具先要审批" "需要审批 #1"
send "/approve 1";                                expect "审批通过后执行" "e2e-42"
send '!call nope {}';                             expect "未知工具变成错误结果" "未知工具：nope"
send '!call get_time {oops';                      expect "非法参数变成错误结果" "不是合法的 JSON 对象"
send "/whoami";                                   expect "查身份" "你的身份：cli:local"
send "/reset";                                    expect "清空上下文" "上下文已清空"

echo "e2e: $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  echo "--- 输出"; cat "$TMP/out"
  echo "--- gateway.log"; tail -20 "$TMP/gateway.log"
  echo "--- kernel.log"; tail -20 "$TMP/kernel.log"
  exit 1
fi
