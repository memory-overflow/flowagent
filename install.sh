#!/usr/bin/env bash
# ============================================================================
# flowagent 依赖安装脚本（Linux / macOS）
#
# 用途：一次性装好运行 flowagent 所需的全部依赖——
#   1) Go（检测，缺失给指引）          5) 编译后端到 bin/
#   2) Node.js（检测，缺失给指引）     6) 前端 npm install
#   3) Python（检测，缺失给指引）      7) 项目级 venv + 基础库
#   4) 生成默认配置 config/config.yaml  8) skills 运行时依赖（pdfkit-py 独立）
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

# ── 4. 生成默认配置 ──
step "4. 生成默认配置（config/config.yaml）"
if [ -f config/config.yaml ]; then
  ok "config/config.yaml 已存在，跳过"
else
  cp config/config.example.yaml config/config.yaml
  ok "已从 config.example.yaml 生成 config/config.yaml（默认 sqlite）"
fi

# ── 5. 编译后端 ──
step "5. 编译后端（bin/agent-server + bin/agent-scheduler）"
mkdir -p bin
go build -trimpath -o bin/agent-server ./cmd/server && ok "bin/agent-server"
go build -trimpath -o bin/agent-scheduler ./cmd/scheduler && ok "bin/agent-scheduler"
go build -trimpath -o bin/agent-supervisor ./cmd/supervisor && ok "bin/agent-supervisor"

# ── 6. 前端依赖 ──
step "6. 安装前端依赖（frontend/node_modules）"
(cd frontend && npm install --no-audit --no-fund)

# ── 7. 项目级 Python 虚拟环境 ──
step "7. 项目级 Python 虚拟环境（.venv + 基础库）"
if has python3; then
  if [ ! -x .venv/bin/python3 ]; then
    python3 -m venv .venv && ok ".venv 已创建"
  else
    ok ".venv 已存在"
  fi
  [ -x .venv/bin/python ] || ln -s python3 .venv/bin/python
  echo "安装基础库（实时显示进度）..."
  .venv/bin/pip install -r requirements-base.txt
  echo ""
  ok "基础库安装完成，已安装的库："
  .venv/bin/pip list --format=freeze 2>/dev/null | grep -iE '^(requests|httpx|beautifulsoup4|lxml|PyYAML|pandas|openpyxl|python-dotenv|python-dateutil)==' | sed 's/^/    /'
else
  warn "未找到 python3，跳过项目级 venv（pdfkit-py 仍会自装独立 Python）"
fi

# ── 8. skills 运行时依赖 ──
step "8. 安装 skills 运行时依赖（pdfkit-py / minimax-xlsx / browser-use）"
if [ -f skills/setup.sh ]; then
  # 子安装器（skills/setup.sh）有自己的 0-N 步骤编号，整体缩进输出，
  # 视觉上明确归属本步骤，避免与外层 1-8 编号混淆。pipefail 保退出码。
  bash skills/setup.sh 2>&1 | while IFS= read -r line; do printf '    %s\n' "$line"; done
  rc=$?
  if [ "$rc" -ne 0 ]; then
    warn "skills 依赖安装失败（退出码 $rc，详见上方输出）"
  fi
else
  warn "缺少 skills/setup.sh，跳过（skills 依赖未安装）"
fi

echo ""
echo -e "${GREEN}=== 安装完成 ===${NC}"
echo "一键启动：bash start.sh"
