#!/usr/bin/env bash
# 安装 nano-agent 网关到 ~/.nano-agent（可以用 NANO_HOME 改）。
#
#   make install                 安装/升级（程序、配置、数据都放在 ~/.nano-agent）
#   scripts/install-gateway.sh --migrate
#                                复制源码目录的 gateway.toml 和 data/（源文件不动）
#   scripts/install-gateway.sh --import 导出包 [--old-local-as 机器名]
#                                装好后导入另一台机器 nano-gateway export 的配置和数据
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

HERE=$(cd "$(dirname "$0")" && pwd)
if [ -x "$HERE/gateway/nano-gateway" ] || [ -x "$HERE/nano-gateway" ]; then
  ROOT="$HERE"
else
  ROOT=$(cd "$HERE/.." && pwd)
fi
NANO_HOME=${NANO_HOME:-$HOME/.nano-agent}
# 源码目录和离线包两种布局：离线包里 nano-gateway 在顶层，lib/ 下是内核和执行器
if [ -x "$ROOT/gateway/nano-gateway" ]; then
  GW="$ROOT/gateway/nano-gateway"; KERNEL="$ROOT/kernel/nano-kernel"; EXEC="$ROOT/kernel/nano-exec"; LIBDIR="$ROOT/kernel"
else
  GW="$ROOT/nano-gateway"; KERNEL="$ROOT/lib/nano-kernel"; EXEC="$ROOT/lib/nano-exec"; LIBDIR="$ROOT/lib"
fi
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
WSL_KEEPALIVE=0
IMPORT=""
OLD_LOCAL=""
while [ $# -gt 0 ]; do
  case "$1" in
  --migrate) MIGRATE=1 ;;
  --yes) YES=1 ;;
  --uninstall) UNINSTALL=1 ;;
  --wsl-keepalive) WSL_KEEPALIVE=1 ;;
  --import) IMPORT=${2:-}; shift ;;
  --import=*) IMPORT=${1#*=} ;;
  --old-local-as) OLD_LOCAL=${2:-}; shift ;;
  --old-local-as=*) OLD_LOCAL=${1#*=} ;;
  *) echo "不认识的选项：$1"; exit 2 ;;
  esac
  shift
done
if [ -n "$IMPORT" ]; then
  [ -f "$IMPORT" ] || { echo "找不到导出包：$IMPORT"; exit 1; }
  IMPORT=$(cd "$(dirname "$IMPORT")" && pwd)/$(basename "$IMPORT")
fi

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

# ---- WSL 保活：在 Windows 启动文件夹放一个隐藏的 VBS，登录后让 wsl.exe 启动网关并留一个 sleep ----
# 启动文件夹不需要管理员权限；卸载时删除，不改 .wslconfig。

wsl_startup_vbs() {
  cmd_exe=$(command -v cmd.exe 2>/dev/null || true)
  [ -n "$cmd_exe" ] || cmd_exe=/mnt/c/Windows/System32/cmd.exe
  [ -x "$cmd_exe" ] || return 1
  appdata=$(cd /mnt/c 2>/dev/null && "$cmd_exe" /c 'echo %APPDATA%' 2>/dev/null | tr -d '\r')
  [ -n "$appdata" ] || return 1
  wslpath -u "$appdata\\Microsoft\\Windows\\Start Menu\\Programs\\Startup" 2>/dev/null
}

wsl_install_keepalive() {
  is_wsl || return 0
  distro=${WSL_DISTRO_NAME:-}
  [ -n "$distro" ] || { say "认不出 WSL 发行版名字，跳过保活设置"; return 0; }
  startup=$(wsl_startup_vbs) || { say "找不到 Windows 启动文件夹，跳过保活设置"; return 0; }
  mkdir -p "$startup"
  vbs="$startup/nano-agent-wsl-keepalive.vbs"
  cmd="NANO_HOME='$NANO_HOME' '$BIN/nano-gateway' start --quiet -c '$CONFIG'; exec /bin/sleep 2147483647"
  {
    printf 'Set sh = CreateObject("WScript.Shell")\r\n'
    printf 'sh.Run "C:\\Windows\\System32\\wsl.exe -d %s --exec /bin/sh -c ""%s""", 0, False\r\n' "$distro" "$cmd"
  } >"$vbs"
  say "已设置 WSL 保活：$vbs"
}

wsl_remove_keepalive() {
  is_wsl || return 0
  startup=$(wsl_startup_vbs) || return 0
  rm -f "$startup/nano-agent-wsl-keepalive.vbs"
}

# ---- PATH：写进各个 shell 的启动文件（新开的终端生效）；~/.local/bin 已经在 PATH 里时
# 再放一个链接，当前终端马上就能用 ----
add_to_path() { # add_to_path 目录 命令…
  dir=$1; shift
  shell_name=$(basename "${SHELL:-sh}")
  for f in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
    case "$f" in
    */.bashrc) [ -f "$f" ] || [ "$shell_name" = bash ] || continue ;;
    */.zshrc) [ -f "$f" ] || [ "$shell_name" = zsh ] || continue ;;
    esac
    grep -qsF "$PROFILE_MARK" "$f" && continue
    printf '\nexport PATH="%s:$PATH"  %s\n' "$dir" "$PROFILE_MARK" >>"$f"
  done
  if [ "$shell_name" = fish ]; then
    mkdir -p "$HOME/.config/fish/conf.d"
    printf 'fish_add_path -g %s  %s\n' "$dir" "$PROFILE_MARK" >"$HOME/.config/fish/conf.d/nano-agent.fish"
  fi
  case ":$PATH:" in
  *":$dir:"*) ;;
  *":$HOME/.local/bin:"*)
    for c in "$@"; do
      if [ ! -e "$HOME/.local/bin/$c" ] || [ -L "$HOME/.local/bin/$c" ]; then ln -sf "$dir/$c" "$HOME/.local/bin/$c"; fi
    done ;;
  esac
}
remove_from_path() { # remove_from_path 目录 命令…
  dir=$1; shift
  for f in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
    if [ -f "$f" ] && grep -qF "$PROFILE_MARK" "$f"; then
      { grep -vF "$PROFILE_MARK" "$f" || true; } >"$f.nano-tmp"
      cat "$f.nano-tmp" >"$f" && rm -f "$f.nano-tmp"
    fi
  done
  rm -f "$HOME/.config/fish/conf.d/nano-agent.fish"
  for c in "$@"; do
    case "$(readlink "$HOME/.local/bin/$c" 2>/dev/null)" in "$dir"/*) rm -f "$HOME/.local/bin/$c" ;; esac
  done
}

is_wsl() {
  grep -qi microsoft /proc/sys/kernel/osrelease 2>/dev/null
}

# ---- 卸载 ----
# 新版本的网关自己会卸载（同一个目录里还有内核时只卸网关）；旧版本没有这个命令时用下面的脚本
if [ "$UNINSTALL" = 1 ] && [ -x "$BIN/nano-gateway" ] &&
  ! NANO_HOME="$NANO_HOME" "$BIN/nano-gateway" uninstall --probe 2>&1 | grep -q '不认识的子命令'; then
  if [ "$YES" = 1 ]; then exec env NANO_HOME="$NANO_HOME" "$BIN/nano-gateway" uninstall --yes; fi
  exec env NANO_HOME="$NANO_HOME" "$BIN/nano-gateway" uninstall
fi
if [ "$UNINSTALL" = 1 ]; then
  wsl_remove_keepalive
  if [ -f "$UNIT_DIR/$UNIT" ]; then
    has_systemd && systemctl --user stop "$UNIT" >/dev/null 2>&1 || true
    has_systemd && systemctl --user disable "$UNIT" >/dev/null 2>&1 || true
    rm -f "$UNIT_DIR/$UNIT"
    has_systemd && systemctl --user daemon-reload || true
  fi
  if [ -x "$BIN/nano-gateway-ctl" ]; then "$BIN/nano-gateway-ctl" stop >/dev/null 2>&1 || true; fi
  # 后台进程模式（没有 systemd）：连同看护进程一起停掉，不然删了程序它还在跑
  if [ -x "$BIN/nano-gateway" ]; then NANO_HOME="$NANO_HOME" "$BIN/nano-gateway" stop >/dev/null 2>&1 || true; fi
  for f in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
    if [ -f "$f" ]; then
      { grep -vF "$PROFILE_START" "$f" || true; } >"$f.nano-tmp"
      cat "$f.nano-tmp" >"$f" && rm -f "$f.nano-tmp"
    fi
  done
  if [ -x "$NANO_HOME/kernel/nano-kernel" ]; then
    # 内核的 nano-kernel 也在 bin/ 里，PATH 行留着
    case "$(readlink "$HOME/.local/bin/nano-gateway" 2>/dev/null)" in "$BIN"/*) rm -f "$HOME/.local/bin/nano-gateway" ;; esac
  else
    remove_from_path "$BIN" nano-gateway nano-kernel nano-kernel-ctl
  fi
  if [ -x "$NANO_HOME/kernel/nano-kernel" ]; then
    # 同一个目录里还装着内核：只删网关的部分
    confirm "删除网关的程序、配置和数据（保留 $NANO_HOME/kernel 里的内核）？" || { say "已取消"; exit 0; }
    rm -rf "$BIN/nano-gateway" "$LIB" "$NANO_HOME/run" "$NANO_HOME/backup" "$NANO_HOME/cache/releases" \
      "$CONFIG" "$DATA" "$NANO_HOME/secret.key" "$NANO_HOME/personas" "$NANO_HOME/skills" \
      "$NANO_HOME/update.json" "$NANO_HOME/.shipped.sha256"
  elif [ -d "$NANO_HOME" ]; then
    confirm "删除 $NANO_HOME？里面有会话记录、模型和 SSH 连接。" || { say "已取消"; exit 0; }
    rm -rf "$NANO_HOME"
  fi
  say "已卸载。"
  exit 0
fi

for f in "$GW" "$KERNEL" "$EXEC"; do
  [ -x "$f" ] || { echo "缺少 $f，先在源码目录运行 make，或者用 make release 的离线包"; exit 1; }
done

# ---- 程序 ----
step "安装程序到 $NANO_HOME"
install -d -m755 "$BIN" "$LIB"
install -d -m700 "$NANO_HOME/run"
install -m755 "$GW" "$BIN/nano-gateway"
install -m755 "$KERNEL" "$LIB/nano-kernel"
install -m755 "$EXEC" "$LIB/nano-exec"
if [ -f "$ROOT/VERSION" ]; then
  install -m644 "$ROOT/VERSION" "$LIB/VERSION"
elif [ -d "$ROOT/.git" ]; then
  (cd "$ROOT" && git describe --always --dirty 2>/dev/null) >"$LIB/VERSION" || echo dev >"$LIB/VERSION"
fi
rm -f "$BIN/nano-gateway-ctl"
for f in nano-exec-static nano-exec-src.tar.gz rg; do
  [ -f "$LIBDIR/$f" ] && install -m755 "$LIBDIR/$f" "$LIB/$f"
done

# ---- 导入另一台机器导出的配置和数据（在生成新配置之前） ----
if [ -n "$IMPORT" ]; then
  step "导入 $IMPORT"
  if [ -n "$OLD_LOCAL" ]; then
    NANO_HOME="$NANO_HOME" "$BIN/nano-gateway" import "$IMPORT" --old-local-as "$OLD_LOCAL" || die "导入失败"
  else
    NANO_HOME="$NANO_HOME" "$BIN/nano-gateway" import "$IMPORT" || die "导入失败"
  fi
fi

# ---- 配置 ----
step "安装配置到 $NANO_HOME"
install -d -m700 "$DATA" "$NANO_HOME/personas" "$NANO_HOME/skills"
if [ -d "$ROOT/personas" ] && [ -z "$(ls -A "$NANO_HOME/personas" 2>/dev/null)" ]; then
  cp -a "$ROOT/personas/." "$NANO_HOME/personas/"
fi
if [ -d "$ROOT/skills" ] && [ -z "$(ls -A "$NANO_HOME/skills" 2>/dev/null)" ]; then
  cp -a "$ROOT/skills/." "$NANO_HOME/skills/"
fi
# 记下自带的人格和技能装的是哪个版本：以后 nano-gateway update 只覆盖没改过的
if [ ! -f "$NANO_HOME/.shipped.sha256" ] && command -v sha256sum >/dev/null 2>&1; then
  (cd "$NANO_HOME" && find personas skills -type f ! -name '*.new' -print0 2>/dev/null | sort -z |
    xargs -0 -r sha256sum) >"$NANO_HOME/.shipped.sha256" 2>/dev/null || rm -f "$NANO_HOME/.shipped.sha256"
  chmod 600 "$NANO_HOME/.shipped.sha256" 2>/dev/null || true
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
add_to_path "$BIN" nano-gateway

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
Environment=NANO_GATEWAY_MODE=systemd
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
  "$BIN/nano-gateway" start -c "$CONFIG"
  if ! grep -qsF "$PROFILE_START" "$HOME/.profile" 2>/dev/null; then
    if confirm "每次登录自动启动网关（写 ~/.profile）？"; then
      printf 'NANO_HOME="%s" "%s" start --quiet -c "%s"  %s\n' "$NANO_HOME" "$BIN/nano-gateway" "$CONFIG" "$PROFILE_START" >>"$HOME/.profile"
    fi
  fi
fi

if is_wsl; then
  if [ "$WSL_KEEPALIVE" = 1 ]; then
    wsl_install_keepalive
  elif [ "$YES" = 0 ] && [ -r /dev/tty ] && confirm "设置 WSL 保活？会在 Windows 启动文件夹放一个隐藏脚本，关掉所有终端后网关也能被叫醒"; then
    wsl_install_keepalive
  else
    say "提示：WSL 在关掉所有终端后可能被 Windows 停掉。需要保活时运行：$0 --wsl-keepalive"
  fi
fi

say ""
say "装好了。命令 nano-gateway 已加进 PATH（新开的终端生效；当前终端要马上用就运行：export PATH=\"$BIN:\$PATH\"）"
say "  网关：   $BIN/nano-gateway -c $CONFIG"
say "  终端聊： $BIN/nano-gateway chat -c $CONFIG"
say "  日志：   $CHANNEL"
say "  卸载：   $0 --uninstall"
