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
mkdir -p "$TMP/skills/demo"
cat > "$TMP/skills/demo/SKILL.md" <<'SKILL'
---
name: demo
description: 端到端测试技能
---
读取技能目录下的 data.txt。
SKILL
echo "skill-e2e-data" > "$TMP/skills/demo/data.txt"
echo "hello e2e" > "$TMP/work/hello.txt"
echo "secret-profile" > "$TMP/home/.profile"
export HOME="$TMP/home"   # 执行器的临时目录和缓存放在 HOME 下，别弄脏真实的 HOME

cat > "$TMP/gateway.toml" <<CONF
[kernel]
listen = "unix:$TMP/k.sock"
token = "e2e"
[llm]
provider = "mock"
max_history_tokens = 4000
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
node = pc
exec = $ROOT/kernel/nano-exec
approval_timeout = 5
CONF
# phone 的内核还能通过 SSH 在服务器 box 上干活：用假的 ssh，把命令放到 $TMP/remote/box 这个“服务器主目录”里执行
cat > "$TMP/fakessh" <<'SSH'
#!/bin/sh
while [ $# -gt 0 ]; do case "$1" in -o) shift 2 ;; -T) shift ;; --) shift; break ;; *) break ;; esac; done
target=$1; shift
export HOME="$REMOTE_ROOT/$target"
mkdir -p "$HOME" && cd "$HOME" && exec sh -c "$*"
SSH
chmod +x "$TMP/fakessh"
mkdir -p "$TMP/remote/box/proj"
{ sed 's/^node = pc$/node = phone/' "$TMP/kernel.conf"; echo "ssh = $TMP/fakessh"; echo "remote = box box"; } > "$TMP/kernel2.conf"

"$ROOT/gateway/nano-gateway" -c "$TMP/gateway.toml" </dev/null >"$TMP/gateway.out" 2>"$TMP/gateway.log" &
"$ROOT/kernel/nano-kernel" -c "$TMP/kernel.conf" 2>"$TMP/kernel.log" &
KPID=$!
for _ in $(seq 50); do grep -q '内核已连接' "$TMP/gateway.log" 2>/dev/null && break; sleep 0.1; done
# 终端聊天：nano-gateway chat 连到网关，消息从 fifo 进
mkfifo "$TMP/in"
"$ROOT/gateway/nano-gateway" chat -c "$TMP/gateway.toml" <"$TMP/in" >"$TMP/out" 2>&1 &
exec 3>"$TMP/in"   # 一直开着写端，chat 的 stdin 就不会 EOF

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

expect "终端连上网关" "已连上网关。这个终端接在会话「主会话」上"
send "你好";                                          expect "普通对话" "（mock）收到：你好"
send "/history";                                      expect "查看最近的对话" "你：你好"
send '!call Read {"path":"hello.txt"}';               expect "读工作目录里的文件" "1	hello e2e"
send '!call Shell {"command":"echo e2e-$((6*7))"}';   expect "审批模式下命令先要审批" "需要审批 #1"
send "/approve 1";                                    expect "审批通过后执行" "e2e-42"
send "/mode auto";                                    expect "切换到自动模式" "已切换为自动"
send '!call Shell {"command":"echo auto-$((6*7))"}';  expect "自动模式直接执行" "auto-42"
send '!call Skill {"name":"demo"}';                   expect "加载技能说明" "技能 demo"
send "!call Read {\"path\":\"$TMP/skills/demo/data.txt\"}"; expect "读取技能附带文件" "skill-e2e-data"
send '!call Read {"path":"/etc/hostname"}';           expect "工作目录外的文件读不了" "不在工作目录内"
send '!call Shell {"command":"cat ~/.profile"}';        expect "沙箱里读不到 HOME" "Permission denied"
send '!call nope {}';                                 expect "未知工具变成错误结果" "未知工具：nope"
send '!call Read {oops';                              expect "非法参数变成错误结果" "不是合法的 JSON 对象"
send "/whoami";                                       expect "查身份" "你的身份：cli:local"
send "/dir";                                          expect "查看工作目录" "1. $TMP/work"
send "/reset";                                        expect "清空上下文" "上下文已清空"
send "/session new 第二个";                            expect "新建会话并切换" "已切换到会话「第二个」"
send "/session 1";                                    expect "切回主会话" "已切换到会话「主会话」"
send "回到主会话";                                    expect "在主会话里说话" "（mock）收到：回到主会话"

# 第二个内核 phone 连上来；pc 掉线后由 phone 顶上（只聊天），pc 回来后照常用工具
REMOTE_ROOT="$TMP/remote" XDG_RUNTIME_DIR="$TMP/run" "$ROOT/kernel/nano-kernel" -c "$TMP/kernel2.conf" 2>"$TMP/kernel2.log" &
for _ in $(seq 50); do grep -q 'node=phone' "$TMP/gateway.log" 2>/dev/null && break; sleep 0.1; done
send "/node";                                         expect "两个内核同时在线" "在线的机器：pc（和网关同一台机器）、phone（和网关同一台机器）"
kill $KPID; wait $KPID 2>/dev/null || true
for _ in $(seq 50); do grep -q '内核连接断开.*node=pc' "$TMP/gateway.log" 2>/dev/null && break; sleep 0.1; done
send "pc 关机了";                                     expect "pc 掉线后 phone 顶上" "（mock）收到：pc 关机了"
if grep -q '开始处理 sid=main node=phone' "$TMP/gateway.log"; then echo "  ok   这一轮由 phone 跑"; pass=$((pass + 1)); else echo "  FAIL 这一轮由 phone 跑"; fail=$((fail + 1)); fi
send '!call Read {"path":"hello.txt"}';               expect "替补不能用工具" "没有使用工具的权限"
echo "pc is back" > "$TMP/work/back.txt"
"$ROOT/kernel/nano-kernel" -c "$TMP/kernel.conf" 2>>"$TMP/kernel.log" &
for _ in $(seq 50); do [ "$(grep -c '内核已连接 node=pc' "$TMP/gateway.log")" -ge 2 ] && break; sleep 0.1; done
send '!call Read {"path":"back.txt"}';                expect "pc 回来后照常用工具" "1	pc is back"

# 会话切到服务器 box：工具在服务器上执行（循环在 phone 的内核上）
for _ in $(seq 100); do grep -q '服务器可用.*server=box' "$TMP/gateway.log" 2>/dev/null && break; sleep 0.1; done
send "/node box";                                     expect "切到服务器" "这个会话改在 box 上工作了"
send "/dir add ~/proj";                               expect "添加服务器上的目录" "1. ~/proj"
send '!call Write {"path":"r.txt","content":"remote-e2e"}'; expect "在服务器上写文件" "已创建"
if [ "$(cat "$TMP/remote/box/proj/r.txt" 2>/dev/null)" = "remote-e2e" ]; then echo "  ok   文件写在服务器上"; pass=$((pass + 1)); else echo "  FAIL 文件写在服务器上"; fail=$((fail + 1)); fi
send '!call Skill {"name":"demo"}';                   expect "服务器上加载技能" "技能目录：~/.cache/nano-agent/skills/"
if find "$TMP/remote/box/.cache/nano-agent/skills" -name data.txt -exec cat {} \; | grep -qF "skill-e2e-data"; then
  echo "  ok   技能附带文件传到服务器"; pass=$((pass + 1))
else
  echo "  FAIL 技能附带文件传到服务器"; fail=$((fail + 1))
fi
send "/node default";                                 expect "切回默认机器" "这个会话改在 pc 上工作了"

# 超出历史上限后生成摘要，再沿用近期上下文继续调用工具。
send "/session new 压缩测试";                          expect "新建压缩测试会话" "已切换到会话「压缩测试」"
long=$(printf '旧%.0s' {1..1800})
send "摘要第一轮-$long";                              expect "累积待整理的历史" "（mock）收到：摘要第一轮-"
send "摘要第二轮-$long";                              expect "历史超限后生成摘要" "正在整理较早的对话"
echo "compaction-e2e-ok" > "$TMP/work/summary.txt"
send '!call Read {"path":"summary.txt"}';             expect "压缩之后照常调用工具" "compaction-e2e-ok"
if python3 - "$TMP/data/sessions" <<'PY'
import json, pathlib, sys
settings = [json.loads(p.read_text()) for p in pathlib.Path(sys.argv[1]).glob('*.settings.json')]
assert any(s.get('title') == '压缩测试' and s.get('base', 0) > 0 and '较早对话已整理' in s.get('summary', '') for s in settings)
PY
then
  echo "  ok   摘要和裁剪位置已保存"; pass=$((pass + 1))
else
  echo "  FAIL 摘要和裁剪位置已保存"; fail=$((fail + 1))
fi

# 再开一个终端：顶掉原来的那个，打招呼时补上最近的对话
"$ROOT/gateway/nano-gateway" chat -c "$TMP/gateway.toml" </dev/null >"$TMP/out2" 2>&1 &
expect "旧终端被顶掉" "另一个终端接进来了"
for _ in $(seq 50); do grep -qF "compaction-e2e-ok" "$TMP/out2" && break; sleep 0.1; done
if grep -qF "最近 3 轮对话" "$TMP/out2" && grep -qF "compaction-e2e-ok" "$TMP/out2"; then echo "  ok   新终端看到完整历史里的最近对话"; pass=$((pass + 1)); else echo "  FAIL 新终端看到完整历史里的最近对话"; fail=$((fail + 1)); fi

echo "e2e: $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  echo "--- 输出"; cat "$TMP/out"
  echo "--- out2"; cat "$TMP/out2" 2>/dev/null
  echo "--- gateway.log"; tail -20 "$TMP/gateway.log"
  echo "--- kernel.log"; tail -20 "$TMP/kernel.log"
  exit 1
fi
