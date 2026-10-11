#!/usr/bin/env bash
# ============================================================================
# flowagent 依赖安装脚本（Linux / macOS）
#
# 用途：一次性装好运行 flowagent 所需的全部依赖——
#   1) Go（检测，缺失给指引）          4) 编译后端到 bin/
#   2) Node.js（检测，缺失给指引）     5) 前端 npm install
#   3) Python（检测，缺失给指引）      6) skills 运行时依赖（复用 skills/setup.sh）
#
# 用法：
#   bash install.sh
#
# 幂等：可重复执行，已装的会跳过。
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
ok()   { echo -e "${GREEN}✓${NC} $*"; }
warn() { echo -e "${YELLOW}△${NC} $*"; }
fail() { echo -e "${RED}✗${NC} $*"; }
step() { echo -e "\n${CYAN}── $* ──${NC}"; }
has()  { command -v "$1" &>/dev/null; }

OS="$(uname -s)"
echo "=== flowagent 依赖安装 ==="
echo "平台: $OS   目录: $ROOT"

MISSING=0

# ── 1. Go ──
step "1. Go（需 1.25+）"
if has go; then
  ok "go $(go version | awk '{print $3}')"
else
  fail "未找到 Go"
  echo "  macOS: brew install go"
  echo "  Linux: https://go.dev/dl/ 或 sudo apt install -y golang-go"
  MISSING=1
fi

# ── 2. Node.js ──
step "2. Node.js（需 18+）"
if has node; then
  ok "node $(node -v)"
else
  fail "未找到 Node.js"
  echo "  macOS: brew install node"
  echo "  Linux: https://nodejs.org/ 或 nvm install --lts"
  MISSING=1
fi

# ── 3. Python ──
step "3. Python（需 3.10+，skills 依赖）"
if has python3; then
  ok "python3 $(python3 --version 2>&1)"
else
  warn "未找到 python3 —— pdfkit-py 会自行下载独立 Python 运行时，可继续"
fi

if [ "$MISSING" = 1 ]; then
  echo ""
  fail "请先安装上述缺失的运行时，再重新运行本脚本。"
  exit 1
fi

# ── 4. 编译后端 ──
step "4. 编译后端（bin/agent-server + bin/agent-scheduler）"
mkdir -p bin
go build -trimpath -o bin/agent-server ./cmd/server && ok "bin/agent-server"
go build -trimpath -o bin/agent-scheduler ./cmd/scheduler && ok "bin/agent-scheduler"

# ── 5. 前端依赖 ──
step "5. 安装前端依赖（frontend/node_modules）"
(cd frontend && npm install --no-audit --no-fund)

# ── 6. skills 运行时依赖 ──
step "6. 安装 skills 运行时依赖（pdfkit-py / minimax-xlsx / browser-use）"
if [ -f skills/setup.sh ]; then
  bash skills/setup.sh
else
  warn "缺少 skills/setup.sh，跳过（skills 依赖未安装）"
fi

echo ""
echo -e "${GREEN}=== 安装完成 ===${NC}"
echo "一键启动：bash start.sh"
