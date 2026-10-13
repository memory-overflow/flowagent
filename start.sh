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

# 把 install.sh 下载到 .tools 的便携运行时提上 PATH（本会话生效，对齐 install.bat）。
[ -x "$ROOT/.tools/go/bin/go" ]          && export PATH="$ROOT/.tools/go/bin:$PATH"
[ -x "$ROOT/.tools/node/bin/node" ]      && export PATH="$ROOT/.tools/node/bin:$PATH"
[ -x "$ROOT/.tools/python/bin/python3" ] && export PATH="$ROOT/.tools/python/bin:$PATH"

# ── 1. 运行时检查 ──
if ! has go; then fail "未找到 Go，请先运行 bash install.sh"; exit 1; fi
if ! has node; then fail "未找到 Node.js，请先运行 bash install.sh"; exit 1; fi

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
  # 国内直连 proxy.golang.org 极慢或被墙，未显式设置 GOPROXY 时默认 goproxy.cn 优先。
  export GOPROXY="${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}"
  # GOTOOLCHAIN=auto：本机 Go 低于 go.mod 要求（1.25.5）时自动下载对应工具链，
  # 覆盖 GOTOOLCHAIN=local/off 的环境（Go 已 >= 1.25 时该变量无任何额外动作）。
  GOTOOLCHAIN=auto go build -trimpath -o bin/agent-server ./cmd/server || { fail "编译 agent-server 失败"; exit 1; }
  GOTOOLCHAIN=auto go build -trimpath -o bin/agent-scheduler ./cmd/scheduler || { fail "编译 agent-scheduler 失败"; exit 1; }
  GOTOOLCHAIN=auto go build -trimpath -o bin/agent-supervisor ./cmd/supervisor || { fail "编译 agent-supervisor 失败"; exit 1; }
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

echo ""
echo "两个进程已启动，按 Ctrl+C 停止全部。"
wait
