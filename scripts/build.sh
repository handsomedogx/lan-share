#!/bin/bash
#
# LAN Share · 交叉编译脚本（bash / macOS / Linux / Git Bash）
#
# 用法：
#   ./scripts/build.sh            # 默认版本号 0.2.0
#   ./scripts/build.sh 0.2.0      # 指定版本号
#
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-0.2.0}"
LDFLAGS="-s -w -X lanshare/internal/api.Version=${VERSION}"

echo "[1/4] 整理依赖..."
GOOS= GOARCH= CGO_ENABLED= go mod tidy

echo
echo "[2/4] 构建本机调试版（当前平台）..."
go build -trimpath -ldflags "$LDFLAGS" -o "dist/lan-share-$(go env GOOS)-$(go env GOARCH)" ./cmd/server

echo
echo "[3/4] 交叉编译 Linux ARM64..."
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "$LDFLAGS" -o dist/lan-share ./cmd/server

# 这一步不能省：GOOS/GOARCH 万一没生效（比如被上层环境变量顶掉），
# go build 会**静默**把当前平台的二进制写进 dist/lan-share，
# 而这个问题只会在路由器上报 Exec format error 时才暴露 —— 已经白跑一趟了。
# 读回头几个字节断言，别靠信任。
echo
echo "[4/4] 校验目标架构..."
if command -v python >/dev/null 2>&1; then
  python scripts/verify-arch.py
else
  echo "  [SKIP] 未找到 python，请自行确认 dist/lan-share 是 ELF 而非 PE/MZ。"
fi

echo
echo "构建完成，产物："
ls -lh dist/ 2>/dev/null || true
cat <<'EOF'

部署：
  scp dist/lan-share root@10.0.0.1:/mnt/data_mmcblk0p27/lan-share/lan-share
  ssh root@10.0.0.1 "/etc/init.d/lan-share restart"

本地调试（不要用默认 ROOT，会落到 C:\mnt\... 或 /mnt/... 的旧库）：
  LANSHARE_ROOT=./devdata LANSHARE_LISTEN=127.0.0.1:18080 ./dist/lan-share-<os>-<arch>

首次打开页面会自动引导注册，第一个注册的人即管理员。
命令行建号（可选）：
  cd /mnt/data_mmcblk0p27/lan-share
  ./lan-share -init-user=你的用户名:你的密码
EOF
