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

mkdir -p "$TMP/work" "$TMP/home"
echo "hello e2e" > "$TMP/work/hello.txt"
echo "secret-profile" > "$TMP/home/.profile"
export HOME="$TMP/home"   # 执行器的临时目录和缓存放在 HOME 下，别弄脏真实的 HOME

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
[workspace]
dirs = ["$TMP/work"]
mode = "ask"
[cli]
enabled = true
[web]
listen = "off"
CONF
cat > "$TMP/kernel.conf" <<CONF
gateway = unix:$TMP/k.sock
token = e2e
exec = $ROOT/kernel/nano-exec
approval_timeout = 5
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

send "你好";                                          expect "普通对话" "（mock）收到：你好"
send '!call Read {"path":"hello.txt"}';               expect "读工作目录里的文件" "1	hello e2e"
send '!call Shell {"command":"echo e2e-$((6*7))"}';   expect "审批模式下命令先要审批" "需要审批 #1"
send "/approve 1";                                    expect "审批通过后执行" "e2e-42"
send "/mode auto";                                    expect "切换到自动模式" "已切换为自动"
send '!call Shell {"command":"echo auto-$((6*7))"}';  expect "自动模式直接执行" "auto-42"
send '!call Read {"path":"/etc/hostname"}';           expect "工作目录外的文件读不了" "不在工作目录内"
send '!call Shell {"command":"cat ~/.profile"}';        expect "沙箱里读不到 HOME" "Permission denied"
send '!call nope {}';                                 expect "未知工具变成错误结果" "未知工具：nope"
send '!call Read {oops';                              expect "非法参数变成错误结果" "不是合法的 JSON 对象"
send "/whoami";                                       expect "查身份" "你的身份：cli:local"
send "/dir";                                          expect "查看工作目录" "1. $TMP/work"
send "/reset";                                        expect "清空上下文" "上下文已清空"
send "/session new 第二个";                            expect "新建会话并切换" "已切换到会话「第二个」"
send "/session 1";                                    expect "切回主会话" "已切换到会话「主会话」"

echo "e2e: $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  echo "--- 输出"; cat "$TMP/out"
  echo "--- gateway.log"; tail -20 "$TMP/gateway.log"
  echo "--- kernel.log"; tail -20 "$TMP/kernel.log"
  exit 1
fi
