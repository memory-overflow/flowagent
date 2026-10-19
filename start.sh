#!/usr/bin/env bash
# ============================================================================
# flowagent 一键启动（Linux / macOS）
#
# 用途：一条命令拉起后端与前端 ——
#   agent-supervisor 托管 server + scheduler（异常退出自动重启，并提供
#   127.0.0.1:9090 逃生重启端点，供前端「重启后端」按钮直连）+ 前端（vite dev）。
#   Ctrl+C 会同时停掉全部进程。
#
# 用法：
#   bash start.sh
#
# 前置：已运行过 bash install.sh；且 config/config.yaml 已配置。
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
ok()   { echo -e "${GREEN}✓${NC} $*"; }
warn() { echo -e "${YELLOW}△${NC} $*"; }
fail() { echo -e "${RED}✗${NC} $*"; }
has()  { command -v "$1" &>/dev/null; }

echo "=== flowagent 一键启动 ==="

# ── 1. 运行时检查 ──
if ! has go; then fail "未找到 Go，请先运行 bash install.sh"; exit 1; fi
if ! has node; then fail "未找到 Node.js，请先运行 bash install.sh"; exit 1; fi
# git 不是启动的硬性前置（核心功能不依赖它），缺失只提示、不阻断。
has git || warn "未找到 Git —— git MCP 与工作区版本控制工具将不可用（重跑 install.sh 可自动安装）"

# ── 2. 配置检查 ──
if [ ! -f config/config.yaml ]; then
  fail "缺少 config/config.yaml"
  echo "  请先：cp config/config.example.yaml config/config.yaml 并填写 database 与 llm"
  exit 1
fi

# ── 3. 编译后端（二进制缺失时） ──
if [ ! -x bin/agent-server ] || [ ! -x bin/agent-scheduler ] || [ ! -x bin/agent-supervisor ]; then
  echo "编译后端..."
  mkdir -p bin
  # GOPROXY / GOTOOLCHAIN / GOSUMDB 的统一兜底见 scripts/go-build-env.sh
  #（国内直连 proxy.golang.org 会卡死在 go: downloading；工具链模块又强制走 sumdb 校验，
  #  GOSUMDB=off 会报 "checksum database disabled by GOSUMDB=off"）。
  # shellcheck source=scripts/go-build-env.sh
  source "${ROOT}/scripts/go-build-env.sh"
  prepare_go_build_env
  go_build bin/agent-server ./cmd/server || { fail "编译 agent-server 失败"; exit 1; }
  go_build bin/agent-scheduler ./cmd/scheduler || { fail "编译 agent-scheduler 失败"; exit 1; }
  go_build bin/agent-supervisor ./cmd/supervisor || { fail "编译 agent-supervisor 失败"; exit 1; }
fi

# ── 4. 前端依赖 ──
if [ ! -d frontend/node_modules ]; then
  echo "安装前端依赖..."
  (cd frontend && npm install --no-audit --no-fund)
fi

# ── 5. 启动三个进程 ──
PIDS=""
cleanup() {
  echo ""
  echo "停止所有进程..."
  for pid in $PIDS; do kill "$pid" 2>/dev/null; done
  exit 0
}
trap cleanup INT TERM

./bin/agent-supervisor -config config/config.yaml &
PIDS="$PIDS $!"
ok "agent-supervisor 启动 (pid $!) —— 托管 server/scheduler，逃生端点 :9090"

(cd frontend && npm run dev) &
PIDS="$PIDS $!"
ok "前端启动 (pid $!) —— 地址见上方 vite 输出"

# 前端起来后自动在浏览器打开 http://localhost:3000（vite 冷启动需数秒，
# 后台轮询端口就绪再打开；打开失败不阻断启动，也不纳入 PIDS 由 Ctrl+C 管理）。
# 用 localhost 探测（vite 默认监听 localhost，macOS 上可能只绑 IPv6 ::1，
# 用 127.0.0.1 会探测不到）；while 而非 seq（seq 非跨平台必备），最长等 60 秒。
(
  i=0
  while [ "$i" -lt 60 ]; do
    if curl -s -o /dev/null --max-time 1 http://localhost:3000 2>/dev/null; then
      echo "  前端已就绪，正在打开浏览器 http://localhost:3000 ..."
      if has open; then open http://localhost:3000
      elif has xdg-open; then xdg-open http://localhost:3000
      fi
      break
    fi
    i=$((i + 1))
    sleep 1
  done
) &

echo ""
echo "两个进程已启动（前端就绪后会自动打开浏览器 http://localhost:3000），按 Ctrl+C 停止全部。"
wait
