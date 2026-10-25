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
export NANO_HOME="$TMP/home/.nano-agent"
export HOME="$TMP/home"   # 执行器的临时目录和缓存放在 HOME 下，别弄脏真实的 HOME

cat > "$TMP/gateway.toml" <<CONF
[kernel]
listen = "unix:$TMP/k.sock"
token = "e2e"
local = false   # 内核由测试自己启动（pc、phone）
[skills]
dir = "$TMP/skills"
[llm]
provider = "mock"
max_history_tokens = 4000
[context]
keep_turns = 1
[persona]
dir = "$TMP/personas"
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
# 网关自己通过 SSH 连服务器 box：用假的 ssh，-M 建主连接（控制路径上的文件表示），
# 其他命令放到 $TMP/remote/box 这个“服务器主目录”里执行
cat > "$TMP/fakessh" <<'SSH'
#!/bin/sh
master=0; op=""; ctl=""
while [ $# -gt 0 ]; do
  case "$1" in
    -M) master=1; shift ;;
    -N|-T) shift ;;
    -O) op=$2; shift 2 ;;
    -o) case "$2" in ControlPath=*) ctl=${2#ControlPath=} ;; esac; shift 2 ;;
    -p|-l|-i) shift 2 ;;
    --) shift; break ;;
    *) break ;;
  esac
done
target=$1; shift
if [ -n "$op" ]; then [ -e "$ctl" ]; exit $?; fi
if [ "$master" = 1 ]; then touch "$ctl"; while :; do sleep 1; done; fi
[ -e "$ctl" ] || { echo "ssh: control socket missing" >&2; exit 255; }
export HOME="$REMOTE_ROOT/$target"; mkdir -p "$HOME"; cd "$HOME" && exec sh -c "$*"
SSH
chmod +x "$TMP/fakessh"
mkdir -p "$TMP/remote/box/proj" "$TMP/data" "$TMP/run"
chmod 700 "$TMP/run"
cat > "$TMP/data/connections.json" <<'JSON'
{"e2ebox": {"id": "e2ebox", "name": "box", "host": "box", "auto_connect": true}}
JSON
sed 's/^node = pc$/node = phone/' "$TMP/kernel.conf" > "$TMP/kernel2.conf"

REMOTE_ROOT="$TMP/remote" NANO_SSH_PROGRAM="$TMP/fakessh" XDG_RUNTIME_DIR="$TMP/run" \
  "$ROOT/gateway/nano-gateway" -c "$TMP/gateway.toml" </dev/null >"$TMP/gateway.out" 2>"$TMP/gateway.log" &
"$ROOT/kernel/nano-kernel" -c "$TMP/kernel.conf" 2>"$TMP/kernel.log" &
KPID=$!
for _ in $(seq 50); do grep -q '内核已连接' "$TMP/gateway.log" 2>/dev/null && break; sleep 0.1; done
# 终端聊天：nano-gateway chat 连到网关，消息从 fifo 进
mkfifo "$TMP/in"
"$ROOT/gateway/nano-gateway" chat -c "$TMP/gateway.toml" <"$TMP/in" >"$TMP/out" 2>&1 &
CHATPID=$!
exec 3>"$TMP/in"   # 一直开着写端，chat 的 stdin 就不会 EOF

pass=0 fail=0
output_start=0
send() { output_start=$(wc -c < "$TMP/out"); echo "$1" >&3; }
expect() {
  local desc=$1 want=$2
  for _ in $(seq 50); do
    if tail -c +$((output_start + 1)) "$TMP/out" | grep -qF -- "$want"; then echo "  ok   $desc"; pass=$((pass + 1)); return; fi
    sleep 0.1
  done
  echo "  FAIL $desc（没等到：$want）"; fail=$((fail + 1))
}
# expect_n：输出里 want 出现了至少 n 次
expect_n() {
  local desc=$1 want=$2 n=$3
  for _ in $(seq 50); do
    if [ "$(grep -cF -- "$want" "$TMP/out")" -ge "$n" ]; then echo "  ok   $desc"; pass=$((pass + 1)); return; fi
    sleep 0.1
  done
  echo "  FAIL $desc（没等到第 $n 个：$want）"; fail=$((fail + 1))
}

expect "终端连上网关" "已连上网关。这个终端接在会话「主会话」上"
send "你好";                                          expect "普通对话" "（mock）收到：你好"
# 回复正文先流式显示，这一轮写进历史要等到结束（用量那一行之后）；先等它，/history 才看得到这一轮
expect "这一轮结束" "次模型调用"
send "/history";                                      expect "查看最近的对话" "你：你好"
send '!call Read {"path":"hello.txt"}';               expect "读工作目录里的文件" "1	hello e2e"
send '!call Shell {"command":"echo e2e-$((6*7))"}';   expect "审批模式下命令先要审批" "需要审批 #1"
send "/approve 1";                                    expect "审批通过后执行" "e2e-42"
send '!call Shell {"command":"echo never"}';          expect "又一个等审批的任务" "需要审批 #2"
send "停";                                            expect "发「停」停下任务" "（已停止）"
send '!call Shell {"command":"echo never"}';          expect "再一个等审批的任务" "需要审批 #3"
kill -INT $CHATPID;                                   expect_n "终端里 Ctrl-C 停下任务" "（已停止）" 2
expect "Ctrl-C 提示再按一次退出" "再按一次 Ctrl-C 退出"
send "/mode auto";                                    expect "切换到自动模式" "已切换为自动"
send '!call Shell {"command":"echo auto-$((6*7))"}';  expect "自动模式直接执行" "auto-42"
send '!call Skill {"name":"demo"}';                   expect "加载技能说明" "技能 demo"
send '!call Read {"path":"~/.nano-agent/skills/demo/data.txt"}'; expect "技能附带文件放到执行的机器上" "skill-e2e-data"
send '!call Read {"path":"~/.profile"}';                expect "工作目录外也能读 HOME" "secret-profile"
send '!call Shell {"command":"cat ~/.profile"}';        expect "Shell 也能读 HOME" "secret-profile"
send '!call Write {"path":"/tmp/nano-e2e-outside.txt","content":"x"}'; expect "工作目录外不能写" "elevated"
send '!call nope {}';                                 expect "未知工具变成错误结果" "未知工具：nope"
send '!call Read {oops';                              expect "非法参数变成错误结果" "不是合法的 JSON 对象"
send "/whoami";                                       expect "查身份" "你的身份：cli:local"
send "/dir";                                          expect "查看工作目录" "1. 本机:$TMP/work（已挂载，新会话默认）"
send "/reset";                                        expect "清空上下文" "上下文已清空"
send "/session new 第二个";                            expect "新建会话并切换" "已切换到会话「第二个」"
send "/session 1";                                    expect "切回主会话" "已切换到会话「主会话」"
send "回到主会话";                                    expect "在主会话里说话" "（mock）收到：回到主会话"
# 没有工作目录的管理员：HOME 模式，能读、能执行，写工作目录外要提权
send "/session new HOME测试";                          expect "新建 HOME 会话" "已切换到会话「HOME测试」"
send "/dir none";                                     expect "HOME 会话没有工作目录" "在 HOME 下工作"
send "/mode auto";                                    expect "HOME 会话自动模式" "已切换为自动"
send '!call Read {"path":"~/.profile"}';               expect "HOME 会话能读 HOME" "secret-profile"
send '!call Shell {"command":"cat ~/.profile"}';       expect "HOME 会话 Shell 能读" "secret-profile"
send '!call Write {"path":"~/.nano-e2e-readonly","content":"x"}'; expect "HOME 会话写外面要提权" "elevated"
send '!call Shell {"command":"echo x > ~/.nano-e2e-readonly"}'; expect "HOME 会话 Shell 不能写 HOME" "Permission denied"
send '!call Write {"path":"~/.nano-e2e-elevated.txt","content":"elevated-ok","elevated":true}'; expect "提权要审批" "需要审批 #4"
send "/approve 4";                                    expect "批准提权后写入" "（mock）工具结果：已创建"
send '!call Read {"path":"~/.nano-e2e-elevated.txt"}'; expect "提权写到了工作目录外" "elevated-ok"
# 网关管理工具：查询不审批，修改要审批；结果和日志里没有密钥
send '!call Gateway {"what":"status"}';                 expect "Gateway 查询状态" "\"kernels\""
mkdir -p "$TMP/gw-ws"
send "!call GatewayAdmin {\"action\":\"workspace_add\",\"host\":\"本机\",\"path\":\"$TMP/gw-ws\"}"
expect "GatewayAdmin 要审批" "提权"
expect "GatewayAdmin 审批编号" "需要审批 #5"
send "/approve 5";                                    expect "GatewayAdmin 添加工作目录" "工作目录已添加"
send "/dir";                                          expect "网关工具添加的目录出现在列表里" "本机:$TMP/gw-ws"
send "/session 1";                                    expect "回到主会话" "已切换到会话「主会话」"
send "撤回";                                          expect "撤回最近一轮" "已撤回：「回到主会话」"
if ! grep -qF "回到主会话" "$TMP/data/sessions/main.jsonl"; then echo "  ok   撤回的一轮从历史文件里删掉"; pass=$((pass + 1)); else echo "  FAIL 撤回的一轮从历史文件里删掉"; fail=$((fail + 1)); fi
send "撤回之后";                                      expect "撤回后照常对话" "（mock）收到：撤回之后"
# 长期记忆（v0.6）：Knowledge 由内核转给网关执行（任何权限模式都转）；/memory 能搜到；/sleep 整理记忆写梦境日志
send '!call Knowledge {"action":"write","title":"端到端测试的人","type":"人物","body":"喜欢喝茶"}'; expect "Knowledge 由内核转给网关执行" "记下了：[人物] 端到端测试的人"
send "/memory 喝茶";                                   expect "/memory 搜得到刚记下的" "端到端测试的人"
send "/sleep";                                        expect "/sleep 开始整理记忆" "去整理记忆了"
dreamlog() { find "$TMP" -path '*/agents/default/dreams/*.md' 2>/dev/null | head -1; }
for _ in $(seq 80); do [ -n "$(dreamlog)" ] && break; sleep 0.1; done
if [ -n "$(dreamlog)" ] && grep -q "（mock）" "$(dreamlog)"; then echo "  ok   整理记忆跑完写了梦境日志"; pass=$((pass + 1)); else echo "  FAIL 整理记忆跑完写了梦境日志"; grep -E "整理记忆|dream" "$TMP/gateway.log" | tail -5; fail=$((fail + 1)); fi

# 图片和文件（v0.7）：SendFile 由内核转给网关，网关让本机的执行器取文件（Fetch）；命令行发不了文件，发一行说明
printf '\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR' > "$TMP/work/e2e-pic.png"
send '!call SendFile {"files":["e2e-pic.png"]}';      expect "SendFile 取文件发到聊天里" "［图片：e2e-pic.png］"
expect "SendFile 的结果" "已发送图片：e2e-pic.png"

# 第二个内核 phone 连上来；pc 掉线后由 phone 顶上（只聊天），pc 回来后照常用工具
"$ROOT/kernel/nano-kernel" -c "$TMP/kernel2.conf" 2>"$TMP/kernel2.log" &
for _ in $(seq 50); do grep -q 'node=phone' "$TMP/gateway.log" 2>/dev/null && break; sleep 0.1; done
send "/node";                                         expect "两个内核同时在线" "· 本机（网关所在的机器，内核 pc、phone）"
kill $KPID; wait $KPID 2>/dev/null || true
for _ in $(seq 50); do grep -q '内核连接断开.*node=pc' "$TMP/gateway.log" 2>/dev/null && break; sleep 0.1; done
send "pc 关机了";                                     expect "pc 掉线后 phone 顶上" "（mock）收到：pc 关机了"
if grep -q '开始处理 sid=main node=phone' "$TMP/gateway.log"; then echo "  ok   这一轮由 phone 跑"; pass=$((pass + 1)); else echo "  FAIL 这一轮由 phone 跑"; fail=$((fail + 1)); fi
send '!call Read {"path":"hello.txt"}';               expect_n "替补照样能用工具" "1	hello e2e" 2
echo "pc is back" > "$TMP/work/back.txt"
"$ROOT/kernel/nano-kernel" -c "$TMP/kernel.conf" 2>>"$TMP/kernel.log" &
for _ in $(seq 50); do [ "$(grep -c '内核已连接 node=pc' "$TMP/gateway.log")" -ge 2 ] && break; sleep 0.1; done
send '!call Read {"path":"back.txt"}';                expect "pc 回来后照常用工具" "1	pc is back"

# 再挂上服务器 box 上的目录：网关自己连 SSH，工具交给网关在服务器上执行（服务器上不跑内核）
for _ in $(seq 100); do grep -q '服务器已就绪.*server=box' "$TMP/gateway.log" 2>/dev/null && break; sleep 0.1; done
# 工作目录的规范形式由那台机器给：~/proj 会被换成服务器上的真实路径
BOXWS="box:$TMP/remote/box/proj"
send "/dir add box:~/proj";                           expect "挂上服务器上的目录" "挂上了 $BOXWS"
send '!call Write {"path":"r.txt","content":"x"}';    expect "挂了两个要写明在哪个上执行" "用 workspace 参数指明"
send '!call Write {"path":"r.txt","content":"remote-e2e","workspace":"'"$BOXWS"'"}'; expect "在服务器上写文件" "已创建"
for _ in $(seq 50); do [ "$(cat "$TMP/remote/box/proj/r.txt" 2>/dev/null)" = "remote-e2e" ] && break; sleep 0.1; done
got=$(cat "$TMP/remote/box/proj/r.txt" 2>&1 || true)
if [ "$got" = "remote-e2e" ]; then echo "  ok   文件写在服务器上"; pass=$((pass + 1)); else echo "  FAIL 文件写在服务器上（$got）"; fail=$((fail + 1)); fi
if ls "$TMP"/remote/box/.nano-agent/nano-exec-* >/dev/null 2>&1 && ! ls "$TMP"/remote/box/.nano-agent/ | grep -q kernel; then
  echo "  ok   服务器上只有执行器，没有内核"; pass=$((pass + 1))
else
  echo "  FAIL 服务器上只有执行器，没有内核"; fail=$((fail + 1))
fi
send '!call Skill {"name":"demo"}';                   expect_n "加载技能（两台机器上都放好附带文件）" "技能目录：~/.nano-agent/skills/demo" 2
if find "$TMP/remote/box/.nano-agent/skills" -name data.txt -exec cat {} \; | grep -qF "skill-e2e-data"; then
  echo "  ok   技能附带文件传到服务器"; pass=$((pass + 1))
else
  echo "  FAIL 技能附带文件传到服务器"; fail=$((fail + 1))
fi
send '!call Read {"path":"~/.nano-agent/skills/demo/data.txt","workspace":"box"}'; expect_n "服务器上读技能文件" "skill-e2e-data" 2
send '!call Read {"path":"back.txt","workspace":"本机:'"$TMP"'/work"}'; expect_n "同一轮也能用本机的目录" "1	pc is back" 2
send "/dir -3";                                       expect "取下服务器上的目录" "取下了 $BOXWS"

# 超出历史上限后生成摘要，再沿用近期上下文继续调用工具。
send "/session new 压缩测试";                          expect "新建压缩测试会话" "已切换到会话「压缩测试」"
long=$(printf '旧%.0s' {1..1800})
send "摘要第一轮-$long";                              expect "累积待整理的历史" "（mock）收到：摘要第一轮-"
send "摘要第二轮-$long";                              expect "历史超限后生成摘要" "正在整理上下文"
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
