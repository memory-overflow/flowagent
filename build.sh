#!/usr/bin/env bash
#
# 一键编译脚本：后端二进制产物输出到 bin/，可选编译前端。
#
# 用法：
#   ./build.sh              只编译后端（agent-server / agent-scheduler）到 bin/
#   ./build.sh -f           额外编译前端（frontend/dist）
#   ./build.sh --frontend   同上
#
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="${ROOT_DIR}/bin"
FRONTEND_DIR="${ROOT_DIR}/frontend"

BUILD_FRONTEND=false

for arg in "$@"; do
  case "$arg" in
    -f|--frontend|--all|all)
      BUILD_FRONTEND=true
      ;;
    -h|--help)
      echo "一键编译脚本：后端二进制输出到 bin/，可选编译前端。"
      echo ""
      echo "用法："
      echo "  ./build.sh              只编译后端到 bin/"
      echo "  ./build.sh -f           额外编译前端（frontend/dist）"
      echo "  ./build.sh --frontend   同上"
      exit 0
      ;;
    *)
      echo "未知参数: $arg（使用 -h 查看帮助）" >&2
      exit 1
      ;;
  esac
done

echo "==> 创建输出目录 ${BIN_DIR}"
mkdir -p "${BIN_DIR}"

cd "${ROOT_DIR}"

echo "==> 编译后端二进制 ..."
echo "  -> bin/agent-server"
# GOTOOLCHAIN=auto：本机 Go 低于 go.mod 要求（1.25.5）时自动下载对应工具链。
GOTOOLCHAIN=auto go build -trimpath -o "${BIN_DIR}/agent-server" ./cmd/server

echo "  -> bin/agent-scheduler"
GOTOOLCHAIN=auto go build -trimpath -o "${BIN_DIR}/agent-scheduler" ./cmd/scheduler

echo "  -> bin/agent-supervisor"
GOTOOLCHAIN=auto go build -trimpath -o "${BIN_DIR}/agent-supervisor" ./cmd/supervisor

if ${BUILD_FRONTEND}; then
  echo "==> 编译前端 ..."
  (cd "${FRONTEND_DIR}" && npm run build)
fi

echo ""
echo "==> 编译完成"
ls -lh "${BIN_DIR}"
