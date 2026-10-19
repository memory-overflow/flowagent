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
# GOPROXY / GOTOOLCHAIN / GOSUMDB 的统一兜底见 scripts/go-build-env.sh
#（国内直连 proxy.golang.org 会卡死在 go: downloading；工具链模块又强制走 sumdb 校验）。
# shellcheck source=scripts/go-build-env.sh
source "${ROOT_DIR}/scripts/go-build-env.sh"
prepare_go_build_env

# Windows（Git Bash / MSYS / Cygwin）下产物必须带 .exe 后缀：
# supervisor 的 binPath、start.bat、install.bat 都按 agent-server.exe 查找，
# 无后缀会导致 spawn 失败 —— 症状是「编译明明成功，重启后却还在跑旧代码」。
EXE=""
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) EXE=".exe" ;;
esac

echo "  -> bin/agent-server${EXE}"
go_build "${BIN_DIR}/agent-server${EXE}" ./cmd/server

echo "  -> bin/agent-scheduler${EXE}"
go_build "${BIN_DIR}/agent-scheduler${EXE}" ./cmd/scheduler

echo "  -> bin/agent-supervisor${EXE}"
go_build "${BIN_DIR}/agent-supervisor${EXE}" ./cmd/supervisor

if ${BUILD_FRONTEND}; then
  echo "==> 编译前端 ..."
  (cd "${FRONTEND_DIR}" && npm run build)
fi

echo ""
echo "==> 编译完成"
ls -lh "${BIN_DIR}"
