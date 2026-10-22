#!/usr/bin/env bash
# 安装 nano-agent 网关到 ~/.nano-agent（可以用 NANO_HOME 改）。
#
#   make install                 安装/升级（程序、配置、数据都放在 ~/.nano-agent）
#   scripts/install-gateway.sh --migrate
#                                复制源码目录的 gateway.toml 和 data/（源文件不动）
#   scripts/install-gateway.sh --uninstall
#                                停服务，删服务文件和 PATH 行，再删 ~/.nano-agent
#
# 安装后：
#   ~/.nano-agent/bin/nano-gateway
#   ~/.nano-agent/lib/            nano-kernel、nano-exec
#   ~/.nano-agent/gateway.toml
#   ~/.nano-agent/personas/ skills/
#   ~/.nano-agent/data/           会话、模型、连接、图片、日志
#   ~/.nano-agent/run/            socket
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
NANO_HOME=${NANO_HOME:-$HOME/.nano-agent}
case "$NANO_HOME" in /*) ;; *) NANO_HOME="$PWD/$NANO_HOME" ;; esac
BIN="$NANO_HOME/bin"
LIB="$NANO_HOME/lib"
DATA="$NANO_HOME/data"
CONFIG="$NANO_HOME/gateway.toml"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT=nano-gateway.service
PROFILE_MARK="# nano-agent PATH"
PROFILE_START="# nano-agent gateway"
MIGRATE=0
YES=0
UNINSTALL=0
for a in "$@"; do
  case "$a" in
  --migrate) MIGRATE=1 ;;
  --yes) YES=1 ;;
  --uninstall) UNINSTALL=1 ;;
  *) echo "不认识的选项：$a"; exit 2 ;;
  esac
done

say() { printf '%s\n' "$*"; }
step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\033[31m错误：%s\033[0m\n' "$*" >&2; exit 1; }
confirm() {
  [ "$YES" = 1 ] && return 0
  [ -r /dev/tty ] || return 1
  printf '%s [Y/n]：' "$1"
  IFS= read -r a </dev/tty || a=n
  case "$a" in "" | y | Y | yes | 是) return 0 ;; *) return 1 ;; esac
}
has_systemd() {
  [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1 &&
    systemctl --user show-environment >/dev/null 2>&1
}

# ---- 卸载 ----
if [ "$UNINSTALL" = 1 ]; then
  if [ -f "$UNIT_DIR/$UNIT" ]; then
    has_systemd && systemctl --user stop "$UNIT" >/dev/null 2>&1 || true
    has_systemd && systemctl --user disable "$UNIT" >/dev/null 2>&1 || true
    rm -f "$UNIT_DIR/$UNIT"
    has_systemd && systemctl --user daemon-reload || true
  fi
  if [ -x "$BIN/nano-gateway-ctl" ]; then "$BIN/nano-gateway-ctl" stop >/dev/null 2>&1 || true; fi
  for f in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
    if [ -f "$f" ]; then
      { grep -vF "$PROFILE_MARK" "$f" | grep -vF "$PROFILE_START" || true; } >"$f.nano-tmp"
      cat "$f.nano-tmp" >"$f" && rm -f "$f.nano-tmp"
    fi
  done
  if [ -d "$NANO_HOME" ]; then
    confirm "删除 $NANO_HOME？里面有会话记录、模型和 SSH 连接。" || { say "已取消"; exit 0; }
    rm -rf "$NANO_HOME"
  fi
  say "已卸载。"
  exit 0
fi

for f in gateway/nano-gateway kernel/nano-kernel kernel/nano-exec; do
  [ -x "$ROOT/$f" ] || { echo "缺少 $ROOT/$f，先在源码目录运行 make"; exit 1; }
done

# ---- 程序 ----
step "安装程序到 $NANO_HOME"
install -d -m755 "$BIN" "$LIB"
install -d -m700 "$NANO_HOME/run"
install -m755 "$ROOT/gateway/nano-gateway" "$BIN/nano-gateway"
install -m755 "$ROOT/kernel/nano-kernel" "$LIB/nano-kernel"
install -m755 "$ROOT/kernel/nano-exec" "$LIB/nano-exec"
for f in nano-exec-static nano-exec-src.tar.gz; do
  [ -f "$ROOT/kernel/$f" ] && install -m755 "$ROOT/kernel/$f" "$LIB/$f"
done

# ---- 配置 ----
step "安装配置到 $NANO_HOME"
install -d -m700 "$DATA" "$NANO_HOME/personas" "$NANO_HOME/skills"
if [ -d "$ROOT/personas" ] && [ -z "$(ls -A "$NANO_HOME/personas" 2>/dev/null)" ]; then
  cp -a "$ROOT/personas/." "$NANO_HOME/personas/"
fi
if [ -d "$ROOT/skills" ] && [ -z "$(ls -A "$NANO_HOME/skills" 2>/dev/null)" ]; then
  cp -a "$ROOT/skills/." "$NANO_HOME/skills/"
fi
COPIED_CONF=0
if [ "$MIGRATE" = 1 ] && [ -f "$ROOT/gateway.toml" ] && [ ! -f "$CONFIG" ]; then
  install -m600 "$ROOT/gateway.toml" "$CONFIG"
  COPIED_CONF=1
  say "已复制源码目录的 gateway.toml（监听地址原样保留）"
fi
if [ ! -f "$CONFIG" ]; then
  token=$(openssl rand -hex 16 2>/dev/null || od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
  socket="$NANO_HOME/run/gateway.sock"
  if [ ${#socket} -gt 100 ]; then
    socket_dir=${XDG_RUNTIME_DIR:-/tmp}
    socket="$socket_dir/nano-agent-${UID}.sock"
  fi
  sed -e 's|^token  = .*|token  = "'"$token"'"|' \
      -e 's|^listen = .*|listen = "unix:'"$socket"'"|' \
      "$ROOT/gateway.example.toml" >"$CONFIG"
  chmod 600 "$CONFIG"
  say "已生成新的 gateway.toml（token 已随机生成）"
elif [ "$COPIED_CONF" = 0 ]; then
  say "$CONFIG 已存在，保留不动"
fi

# ---- 迁移旧数据（只复制） ----
if [ "$MIGRATE" = 1 ] && [ -d "$ROOT/data" ] && [ -z "$(ls -A "$DATA" 2>/dev/null)" ]; then
  say "复制源码目录的 data/ 到 $DATA（源文件不动）"
  cp -a "$ROOT/data/." "$DATA/"
fi

# ---- PATH ----
if ! grep -qsF "$PROFILE_MARK" "$HOME/.profile" 2>/dev/null; then
  if confirm "把 $BIN 加进 PATH（写 ~/.profile，重新登录后生效）？"; then
    printf 'export PATH="%s:$PATH"  %s\n' "$BIN" "$PROFILE_MARK" >>"$HOME/.profile"
  fi
fi

# ---- 后台服务 ----
CHANNEL="$NANO_HOME/data/gateway.log"
: >>"$CHANNEL"
chmod 600 "$CHANNEL"
if has_systemd; then
  step "注册 systemd 用户服务（开机自启，挂了自动重启）"
  mkdir -p "$UNIT_DIR"
  cat >"$UNIT_DIR/$UNIT" <<EOF
[Unit]
Description=nano-agent 网关
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$NANO_HOME
Environment=NANO_HOME=$NANO_HOME
ExecStart=$BIN/nano-gateway -c $CONFIG
Restart=always
RestartSec=3
UMask=0077
StandardOutput=append:$CHANNEL
StandardError=append:$CHANNEL

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable "$UNIT" >/dev/null 2>&1
  systemctl --user restart "$UNIT"
  say "网关服务已启动。带密码的网页登录链接在 $NANO_HOME/data/web.url（只有你能读）。"
  if command -v loginctl >/dev/null 2>&1 && [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null)" != yes ]; then
    say "提示：现在只有登录后网关才会运行。想开机就运行：sudo loginctl enable-linger $(id -un)"
  fi
else
  step "没有 systemd，用后台进程运行"
  cat >"$BIN/nano-gateway-ctl" <<'CTL'
#!/bin/sh
umask 077
# nano-agent 网关：nano-gateway-ctl start|stop|restart|status|log
ROOT=$(cd "$(dirname "$0")/.." && pwd)
PID="$ROOT/run/gateway.pid"
running() {
  p=$(cat "$PID" 2>/dev/null) || return 1
  [ -n "$p" ] && kill -0 "$p" 2>/dev/null &&
    case "$(readlink "/proc/$p/exe" 2>/dev/null)" in "$ROOT/bin/nano-gateway"*) echo "$p" ;; *) return 1 ;; esac
}
case "${1:-status}" in
start)
  running >/dev/null && { [ "${2:-}" = --quiet ] || echo "网关已经在运行"; exit 0; }
  nohup "$ROOT/bin/nano-gateway" -c "$ROOT/gateway.toml" </dev/null >>"$ROOT/data/gateway.log" 2>&1 &
  echo $! >"$PID"
  [ "${2:-}" = --quiet ] || echo "网关已在后台启动，日志：$ROOT/data/gateway.log"
  ;;
stop)
  if p=$(running); then kill "$p"; echo "已停止"; else echo "网关没有在运行"; fi
  rm -f "$PID"
  ;;
restart) "$0" stop >/dev/null; sleep 0.3; exec "$0" start ;;
status)
  if p=$(running); then echo "运行中（pid $p）"; else echo "没有运行"; exit 3; fi
  ;;
log) exec tail -n 50 -F "$ROOT/data/gateway.log" ;;
*) echo "用法：nano-gateway-ctl start|stop|restart|status|log"; exit 2 ;;
esac
CTL
  chmod 755 "$BIN/nano-gateway-ctl"
  "$BIN/nano-gateway-ctl" start
  if ! grep -qsF "$PROFILE_START" "$HOME/.profile" 2>/dev/null; then
    if confirm "每次登录自动启动网关（写 ~/.profile）？"; then
      printf '"%s" start --quiet  %s\n' "$BIN/nano-gateway-ctl" "$PROFILE_START" >>"$HOME/.profile"
    fi
  fi
fi

say ""
say "装好了。"
say "  网关：   $BIN/nano-gateway -c $CONFIG"
say "  终端聊： $BIN/nano-gateway chat -c $CONFIG"
say "  日志：   $CHANNEL"
say "  卸载：   $ROOT/scripts/install-gateway.sh --uninstall"
