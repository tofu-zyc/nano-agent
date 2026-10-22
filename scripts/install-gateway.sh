#!/usr/bin/env bash
# 安装网关到用户目录，把程序、配置、数据分开放，不再塞在源码目录里。
#
#   make install               安装到 ~/.local
#   scripts/install-gateway.sh --migrate
#                              顺便把源码目录里的 gateway.toml 和 data/ 复制到新位置
#
# 安装后：
#   ~/.local/bin/nano-gateway                 网关程序
#   ~/.local/lib/nano-agent/                  执行器、本机内核
#   ~/.config/nano-agent/gateway.toml         配置
#   ~/.config/nano-agent/personas、skills/    人格和技能
#   ~/.local/share/nano-agent/                会话、模型、连接、图片
#
# 源码目录只保留代码；工作目录可以随便指到项目目录，不会碰到网关自己的文件。
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PREFIX=${PREFIX:-$HOME/.local}
CONFIG_DIR=${XDG_CONFIG_HOME:-$HOME/.config}/nano-agent
DATA_DIR=${XDG_DATA_HOME:-$HOME/.local/share}/nano-agent
LIB_DIR=$PREFIX/lib/nano-agent
MIGRATE=0
[ "${1:-}" = "--migrate" ] && MIGRATE=1

for f in gateway/nano-gateway kernel/nano-kernel kernel/nano-exec; do
  [ -x "$ROOT/$f" ] || { echo "缺少 $ROOT/$f，先在源码目录运行 make"; exit 1; }
done

echo "安装程序到 $PREFIX"
install -d -m755 "$PREFIX/bin" "$LIB_DIR"
install -m755 "$ROOT/gateway/nano-gateway" "$PREFIX/bin/nano-gateway"
install -m755 "$ROOT/kernel/nano-kernel" "$LIB_DIR/nano-kernel"
install -m755 "$ROOT/kernel/nano-exec" "$LIB_DIR/nano-exec"
for f in nano-exec-static nano-exec-src.tar.gz; do
  [ -f "$ROOT/kernel/$f" ] && install -m755 "$ROOT/kernel/$f" "$LIB_DIR/$f"
done

echo "安装配置到 $CONFIG_DIR"
install -d -m700 "$CONFIG_DIR" "$DATA_DIR"
[ -d "$CONFIG_DIR/personas" ] || cp -a "$ROOT/personas" "$CONFIG_DIR/personas"
[ -d "$CONFIG_DIR/skills" ] || cp -a "$ROOT/skills" "$CONFIG_DIR/skills"
chmod 700 "$CONFIG_DIR/personas" "$CONFIG_DIR/skills" 2>/dev/null || true

if [ ! -f "$CONFIG_DIR/gateway.toml" ]; then
  if [ "$MIGRATE" = 1 ] && [ -f "$ROOT/gateway.toml" ]; then
    install -m600 "$ROOT/gateway.toml" "$CONFIG_DIR/gateway.toml"
    echo "  已复制源码目录的 gateway.toml（去掉旧路径）"
  else
    token=$(openssl rand -hex 16 2>/dev/null || od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
    socket_dir=${XDG_RUNTIME_DIR:-/tmp}
    socket="$socket_dir/nano-agent-${UID}.sock"
    sed -e 's|^token  = .*|token  = "'"$token"'"|' \
        -e 's|^listen = .*|listen = "unix:'"$socket"'"|' \
        "$ROOT/gateway.example.toml" > "$CONFIG_DIR/gateway.toml"
    chmod 600 "$CONFIG_DIR/gateway.toml"
    echo "  已生成新的 gateway.toml（token 已随机生成）"
  fi
  # 数据目录固定指向安装位置，配置和源码目录分开
  tmp="$CONFIG_DIR/gateway.toml.tmp"
  sed 's|^data_dir[[:space:]]*=.*|data_dir = "'"$DATA_DIR"'"|' "$CONFIG_DIR/gateway.toml" > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$CONFIG_DIR/gateway.toml"
else
  echo "  $CONFIG_DIR/gateway.toml 已存在，保留不动"
fi

if [ "$MIGRATE" = 1 ] && [ -d "$ROOT/data" ] && [ -z "$(ls -A "$DATA_DIR" 2>/dev/null)" ]; then
  echo "复制源码目录的 data/ 到 $DATA_DIR"
  cp -a "$ROOT/data/." "$DATA_DIR/"
fi

echo
echo "装好了。启动："
echo "  $PREFIX/bin/nano-gateway -c $CONFIG_DIR/gateway.toml"
echo
echo "源码目录 $ROOT 现在只是代码；工作目录可以指到任何项目目录。"
echo "如果刚才的网关还在运行，先停掉它，再用上面的命令启动。"
