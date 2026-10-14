#!/usr/bin/env bash
# ============================================================================
# flowagent 依赖安装脚本（Linux / macOS）
#
# 用途：一次性装好运行 flowagent 所需的全部依赖——
#   1) Go（缺失时用系统包管理器安装）
#   2) Node.js（缺失时用系统包管理器安装）
#   3) Python（缺失时用系统包管理器安装）
#   4) Git（缺失时用系统包管理器安装）
#   5) 生成默认配置 config/config.yaml
#   6) 编译后端到 bin/
#   7) 前端 npm install
#   8) 项目级 venv + 基础库
#   9) skills 运行时依赖（pdfkit-py 独立）
#
# 与 Windows 版 install.bat 的区别：本脚本面向 Linux / macOS，缺失依赖时优先
# 用系统自带包管理器安装（macOS=Homebrew，Debian/Ubuntu=apt，RHEL/CentOS=yum/dnf），
# 不做「下载便携运行时到 .tools/」的 download 方式——那是给没有统一包管理器的
# Windows 用的。
#
# 用法：
#   bash install.sh
#
# 幂等：可重复执行，已满足要求的会跳过。
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

OS_NAME="$(uname -s | tr '[:upper:]' '[:lower:]')"   # linux / darwin
echo "=== flowagent 依赖安装 ==="
echo "平台: $OS_NAME   目录: $ROOT"

MISSING=0

# ============================================================================
#  Helpers
# ============================================================================

# detect_pkg_manager 检测系统包管理器：brew（macOS）/ apt / yum / dnf。
# 结果写入 PKG，找不到时置空。
detect_pkg_manager() {
  PKG=""
  if [ "$OS_NAME" = "darwin" ]; then
    has brew && PKG="brew"
  elif has apt-get; then
    PKG="apt"
  elif has yum; then
    PKG="yum"
  elif has dnf; then
    PKG="dnf"
  fi
}

# pkg_install <pkg...> 用系统包管理器安装指定包。apt/yum/dnf 需要 sudo（会提示输密码）。
pkg_install() {
  case "$PKG" in
    brew) brew install "$@" ;;
    apt)  sudo apt-get install -y "$@" ;;
    yum)  sudo yum install -y "$@" ;;
    dnf)  sudo dnf install -y "$@" ;;
    *)    return 1 ;;
  esac
}

# ver_ge <actual> <required>：actual >= required 时返回 0，否则 1。
ver_ge() {
  awk -v a="$1" -v b="$2" 'BEGIN {
    na = split(a, A, "."); nb = split(b, B, ".")
    for (i = 1; i <= 3; i++) {
      av = (i <= na) ? A[i] + 0 : 0
      bv = (i <= nb) ? B[i] + 0 : 0
      if (av > bv) exit 0
      if (av < bv) exit 1
    }
    exit 0
  }'
}

# ============================================================================
#  Go
# ============================================================================
ensure_go() {
  local req=""
  if [ -f go.mod ]; then
    req="$(awk '/^go /{print $2; exit}' go.mod)"
  fi
  [ -n "$req" ] || req="1.25.0"

  if ! has go; then
    echo "  [--] Go 未找到，用 $PKG 安装 ..."
    local gopkg="golang-go"
    [ "$PKG" = "brew" ] && gopkg="go"
    [ "$PKG" = "yum" ] || [ "$PKG" = "dnf" ] && gopkg="golang"
    pkg_install "$gopkg" || { fail "Go 安装失败，请手动装 https://go.dev/dl/"; MISSING=1; return; }
  fi

  local gv="$(go version | awk '{print $3}' | sed 's/^go//')"
  if ver_ge "$gv" "$req"; then
    # ${gv} 必须用花括号：bash 3.2 下 $gv 后紧跟全角括号会被误解析成变量名
    # （报 gv0: unbound variable），与 stop.sh 之前踩过的坑相同。
    ok "Go ${gv}（满足 ${req}）"
  else
    warn "Go ${gv} 低于 go.mod 要求的 ${req}"
    echo "  编译时已启用 GOTOOLCHAIN=auto，会自动下载 go${req} 工具链（走 GOPROXY，仅需一次）"
  fi
}

# ============================================================================
#  Node.js
# ============================================================================
# install_node_via_nodesource 通过 NodeSource 官方脚本配置源并安装新版 Node.js。
# 仅 Debian/Ubuntu(apt) 与 RHEL/CentOS(yum/dnf) 支持；macOS 用 brew 已是新版，不适用。
# 失败返回 1（调用方回退到系统包管理器），不中断整个安装。
install_node_via_nodesource() {
  case "$PKG" in
    apt) local url="https://deb.nodesource.com/setup_22.x" ;;
    yum|dnf) local url="https://rpm.nodesource.com/setup_22.x" ;;
    *) return 1 ;;
  esac
  has curl || { warn "未找到 curl，无法自动配 NodeSource"; return 1; }
  echo "  配置 NodeSource 22.x 源（需 sudo，脚本来自 ${url}）..."
  curl -fsSL "$url" | sudo -E bash - && pkg_install "nodejs"
}

ensure_node() {
  # 已有 node 且版本够 → 直接通过
  if has node; then
    local nv="$(node -v | sed 's/^v//')"
    if ver_ge "$nv" "18.0.0"; then
      ok "Node.js v${nv}（满足 18+）"
      return 0
    fi
    echo "  [--] Node.js v$nv 低于 18。"
  else
    echo "  [--] Node.js 未找到。"
  fi

  # 缺失或版本老：macOS 用 brew；Linux 优先配 NodeSource 装新版（系统源里的 nodejs 常偏旧）。
  if [ "$PKG" = "brew" ]; then
    echo "  用 Homebrew 安装/升级 Node.js ..."
    if has node; then brew upgrade node || brew reinstall node; else brew install node; fi \
      || { fail "Node.js 安装失败"; MISSING=1; return; }
  else
    if ! install_node_via_nodesource; then
      warn "NodeSource 配置失败，回退到系统包管理器安装 nodejs（版本可能仍偏旧）..."
      pkg_install "nodejs" || { fail "Node.js 安装失败，请手动装 https://nodejs.org/"; MISSING=1; return; }
    fi
  fi

  # 最终校验版本
  local nv="$(node -v | sed 's/^v//')"
  if ver_ge "$nv" "18.0.0"; then
    ok "Node.js v${nv}（满足 18+）"
  else
    fail "Node.js v$nv 仍低于 18"
    echo "  macOS: brew upgrade node"
    echo "  Linux: 手动配 NodeSource 后重装：https://github.com/nodesource/distributions"
    MISSING=1
  fi
}

# ============================================================================
#  Python
# ============================================================================
ensure_python() {
  if ! has python3; then
    echo "  [--] Python 未找到，用 $PKG 安装 ..."
    local pypkg="python3"
    [ "$PKG" = "brew" ] && pypkg="python"
    pkg_install "$pypkg" || { warn "Python 安装失败，跳过（pdfkit-py 会自装独立运行时）"; return; }
  fi

  local pv="$(python3 -c 'import sys; print("%d.%d.%d" % sys.version_info[:3])' 2>/dev/null)"
  [ -n "$pv" ] || pv="$(python3 --version 2>&1 | awk '{print $2}')"
  if ver_ge "$pv" "3.10.0"; then
    ok "Python ${pv}（满足 3.10+）"
  else
    warn "Python $pv 低于 3.10，部分 skill 可能不可用"
  fi
}

# ============================================================================
#  Git
# ============================================================================
# 为什么需要：内置的 git MCP server（mcp-server-git）以及工作区里的版本控制
# 能力都直接调用系统 git 命令，缺失时这些工具会全部不可用。
#
# 缺失时用系统包管理器安装。git 在各发行版的包名都叫 git（brew / apt / yum /
# dnf 通用）。安装失败只告警、不设 MISSING —— git 不是编译前置依赖，
# 核心功能（LLM + 工作区）在没有它时仍可运行。
ensure_git() {
  if has git; then
    ok "Git $(git --version 2>/dev/null | awk '{print $3}')"
    return 0
  fi

  echo "  [--] Git 未找到，用 $PKG 安装 ..."
  if ! pkg_install "git"; then
    warn "Git 安装失败，请手动安装后重跑（git MCP 将不可用）"
    return 0
  fi

  if has git; then
    ok "Git $(git --version 2>/dev/null | awk '{print $3}')"
  else
    warn "Git 安装后仍不可用，请检查 PATH（git MCP 将不可用）"
  fi
}

# ============================================================================
#  主流程
# ============================================================================

detect_pkg_manager
if [ -z "$PKG" ]; then
  fail "未检测到系统包管理器（需要 Homebrew / apt / yum / dnf 之一）"
  echo "  请先安装包管理器，或手动安装 Go / Node.js / Python 后重跑。"
  exit 1
fi
ok "使用包管理器: $PKG"

# ── 1. Go ──
step "1. Go（缺失时用 $PKG 安装）"
ensure_go

# ── 2. Node.js ──
step "2. Node.js（缺失时用 $PKG 安装）"
ensure_node

# ── 3. Python ──
step "3. Python（缺失时用 $PKG 安装）"
ensure_python

# ── 4. Git ──
step "4. Git（缺失时用 $PKG 安装）"
ensure_git

if [ "$MISSING" = 1 ]; then
  echo ""
  fail "请先解决上述缺失的运行时，再重新运行本脚本。"
  exit 1
fi

# ── 5. 生成默认配置 ──
step "5. 生成默认配置（config/config.yaml）"
if [ -f config/config.yaml ]; then
  ok "config/config.yaml 已存在，跳过"
else
  cp config/config.example.yaml config/config.yaml
  ok "已从 config.example.yaml 生成 config/config.yaml（默认 sqlite）"
fi

# ── 6. 编译后端 ──
step "6. 编译后端（bin/agent-server + bin/agent-scheduler）"
mkdir -p bin
# 国内直连 proxy.golang.org 极慢或被墙（go: downloading 长期无进展且不报错），
# 未显式设置 GOPROXY 时默认走 goproxy.cn 优先的代理链（海外可正常回退）。
export GOPROXY="${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}"
# GOTOOLCHAIN=auto：本机 Go 低于 go.mod 要求时自动下载对应工具链（系统包管理器装的
# Go 常偏旧，靠它补齐版本）；Go 已 >= 1.25 时该变量无任何额外动作。
GOTOOLCHAIN=auto go build -trimpath -o bin/agent-server ./cmd/server && ok "bin/agent-server"
GOTOOLCHAIN=auto go build -trimpath -o bin/agent-scheduler ./cmd/scheduler && ok "bin/agent-scheduler"
GOTOOLCHAIN=auto go build -trimpath -o bin/agent-supervisor ./cmd/supervisor && ok "bin/agent-supervisor"

# ── 7. 前端依赖 ──
step "7. 安装前端依赖（frontend/node_modules）"
(cd frontend && npm install --no-audit --no-fund)

# ── 8. 项目级 Python 虚拟环境 ──
step "8. 项目级 Python 虚拟环境（.venv + 基础库）"
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

# ── 9. skills 运行时依赖 ──
step "9. 安装 skills 运行时依赖（pdfkit-py / minimax-xlsx / browser-use）"
if [ -f skills/setup.sh ]; then
  bash skills/setup.sh 2>&1 | while IFS= read -r line; do printf '    %s\n' "$line"; done
  rc=${PIPESTATUS[0]}
  if [ "$rc" -ne 0 ]; then
    warn "skills 依赖安装失败（退出码 ${rc}，详见上方输出）"
  fi
else
  warn "缺少 skills/setup.sh，跳过（skills 依赖未安装）"
fi

echo ""
echo -e "${GREEN}=== 安装完成 ===${NC}"
echo "一键启动：bash start.sh"
