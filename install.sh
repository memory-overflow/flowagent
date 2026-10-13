#!/usr/bin/env bash
# ============================================================================
# flowagent 依赖安装脚本（Linux / macOS）
#
# 用途：一次性装好运行 flowagent 所需的全部依赖——
#   1) Go（自动下载）   2) Node.js（自动下载）  3) Python（自动下载）
#   4) 生成默认配置     5) 编译后端到 bin/
#   6) 前端 npm install 7) 项目级 venv + 基础库
#   8) skills 运行时依赖（pdfkit-py 独立）
#
# Go / Node.js / Python 在缺失或版本过旧时自动下载，解压到本项目 .tools/ 下
# （可移植、免管理员、不写项目之外的任何路径），与 Windows 版 install.bat 对齐。
#
# 用法：
#   bash install.sh
#   bash install.sh --reinstall-tools   强制重新下载 Go/Node/Python
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

TOOLS_DIR="$ROOT/.tools"
FORCE_TOOLS=0
for a in "$@"; do [ "$a" = "--reinstall-tools" ] && FORCE_TOOLS=1; done

echo "=== flowagent 依赖安装 ==="
echo "平台: $(uname -s)   目录: $ROOT"

MISSING=0

# ============================================================================
#  Helpers
# ============================================================================

# detect_arch 根据 uname -m 设置 GOARCH / NODE_ARCH / PBSARCH / OS_NAME。
detect_arch() {
  OS_NAME="$(uname -s | tr '[:upper:]' '[:lower:]')"   # linux / darwin
  case "$(uname -m)" in
    x86_64|amd64)  GOARCH=amd64;  NODE_ARCH=x64;    PBSARCH=x86_64 ;;
    arm64|aarch64) GOARCH=arm64;  NODE_ARCH=arm64;  PBSARCH=aarch64 ;;
    *)             GOARCH=amd64;  NODE_ARCH=x64;    PBSARCH=x86_64 ;;
  esac
}

# use_local_tools 把 .tools 下的便携运行时提到 PATH 最前（仅本会话）。
use_local_tools() {
  [ -x "$TOOLS_DIR/go/bin/go" ]           && export PATH="$TOOLS_DIR/go/bin:$PATH"
  [ -x "$TOOLS_DIR/node/bin/node" ]       && export PATH="$TOOLS_DIR/node/bin:$PATH"
  [ -x "$TOOLS_DIR/python/bin/python3" ]  && export PATH="$TOOLS_DIR/python/bin:$PATH"
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

# download <url> <dest>：curl 下载，失败时给出明确报错。
download() {
  local url="$1" dest="$2"
  echo "  下载: $url"
  if curl -L --fail --retry 3 --connect-timeout 30 -o "$dest" "$url"; then
    [ -s "$dest" ] && return 0
  fi
  echo "  [FAIL] 下载失败，请检查网络后重试（或手动安装到 .tools/ 下）"
  return 1
}

# ============================================================================
#  Go
# ============================================================================
ensure_go() {
  # 从 go.mod 读版本要求（首行 "go 1.25.5" 之类）。
  local req=""
  if [ -f go.mod ]; then
    req="$(awk '/^go /{print $2; exit}' go.mod)"
  fi
  [ -n "$req" ] || req="1.25.0"

  if [ "$FORCE_TOOLS" = 1 ]; then
    install_go "$req" || return 1
    return 0
  fi

  if has go; then
    local gv="$(go version | awk '{print $3}' | sed 's/^go//')"
    if ver_ge "$gv" "$req"; then
      ok "Go $gv（满足 $req）"
      return 0
    fi
    echo "  [--] Go $gv 低于 go.mod 要求的 $req。"
  else
    echo "  [--] Go 未找到。"
  fi

  install_go "$req"
}

install_go() {
  local ver="$1"
  detect_arch
  echo "  安装便携 Go $ver 到 .tools/go ..."
  mkdir -p "$TOOLS_DIR"
  download "https://go.dev/dl/go${ver}.${OS_NAME}-${GOARCH}.tar.gz" "$TOOLS_DIR/go.tar.gz" || {
    echo "  [--] 重试官方中国镜像 golang.google.cn ..."
    download "https://golang.google.cn/dl/go${ver}.${OS_NAME}-${GOARCH}.tar.gz" "$TOOLS_DIR/go.tar.gz" || return 1
  }
  rm -rf "$TOOLS_DIR/go"
  tar -xf "$TOOLS_DIR/go.tar.gz" -C "$TOOLS_DIR"
  rm -f "$TOOLS_DIR/go.tar.gz"
  if [ ! -x "$TOOLS_DIR/go/bin/go" ]; then
    fail "Go 解压失败"
    return 1
  fi
  export PATH="$TOOLS_DIR/go/bin:$PATH"
  ok "Go $(go version | awk '{print $3}') 就绪"
  return 0
}

# ============================================================================
#  Node.js
# ============================================================================
ensure_node() {
  if [ "$FORCE_TOOLS" = 1 ]; then
    install_node || return 1
    return 0
  fi

  if has node; then
    local nv="$(node -v | sed 's/^v//')"
    if ver_ge "$nv" "18.0.0"; then
      ok "Node.js v$nv（满足 18+）"
      return 0
    fi
    echo "  [--] Node.js v$nv 低于 18。"
  else
    echo "  [--] Node.js 未找到。"
  fi

  install_node
}

install_node() {
  detect_arch
  echo "  安装便携 Node.js 22 LTS 到 .tools/node ..."
  mkdir -p "$TOOLS_DIR"
  # 从 SHASUMS256.txt 解析最新 22.x 的包名（linux 用 .tar.xz，darwin 用 .tar.gz）
  local nf=""
  nf="$(curl -sL "https://nodejs.org/dist/latest-v22.x/SHASUMS256.txt" \
    | grep -E "node-v22\..*-${OS_NAME}-${NODE_ARCH}\.tar\.(xz|gz)" \
    | head -n1 | awk '{print $2}')"
  if [ -z "$nf" ]; then
    fail "无法解析 Node.js 包名"
    return 1
  fi
  echo "  包: $nf"
  download "https://nodejs.org/dist/latest-v22.x/$nf" "$TOOLS_DIR/node.tar" || return 1
  rm -rf "$TOOLS_DIR/node"
  mkdir -p "$TOOLS_DIR/node"
  tar -xf "$TOOLS_DIR/node.tar" -C "$TOOLS_DIR/node" --strip-components=1
  rm -f "$TOOLS_DIR/node.tar"
  if [ ! -x "$TOOLS_DIR/node/bin/node" ]; then
    fail "Node.js 解压失败"
    return 1
  fi
  export PATH="$TOOLS_DIR/node/bin:$PATH"
  ok "Node.js $(node -v) 就绪"
  return 0
}

# ============================================================================
#  Python
# ============================================================================
ensure_python() {
  if [ "$FORCE_TOOLS" = 1 ]; then
    install_python || warn "Python 自动安装失败，跳过（pdfkit-py 会自装独立运行时）"
    return 0
  fi

  if has python3; then
    local pv="$(python3 -c 'import sys; print("%d.%d.%d" % sys.version_info[:3])' 2>/dev/null)"
    [ -n "$pv" ] || pv="$(python3 --version 2>&1 | awk '{print $2}')"
    if ver_ge "$pv" "3.10.0"; then
      ok "Python $pv（满足 3.10+）"
      return 0
    fi
    echo "  [--] Python $pv 低于 3.10。"
  else
    echo "  [--] Python 未找到。"
  fi

  install_python || warn "Python 自动安装失败，跳过（pdfkit-py 会自装独立运行时）"
}

install_python() {
  detect_arch
  local pver="3.13.13" ptag="20260414"
  echo "  安装便携 Python $pver 到 .tools/python ..."
  mkdir -p "$TOOLS_DIR"
  local url="https://github.com/astral-sh/python-build-standalone/releases/download/${ptag}/cpython-${pver}+${ptag}-${PBSARCH}-unknown-${OS_NAME}-gnu-install_only.tar.gz"
  [ "$OS_NAME" = "darwin" ] && url="https://github.com/astral-sh/python-build-standalone/releases/download/${ptag}/cpython-${pver}+${ptag}-${PBSARCH}-apple-darwin-install_only.tar.gz"
  download "$url" "$TOOLS_DIR/python.tar.gz" || return 1
  rm -rf "$TOOLS_DIR/python"
  mkdir -p "$TOOLS_DIR/python"
  tar -xzf "$TOOLS_DIR/python.tar.gz" -C "$TOOLS_DIR/python" --strip-components=1
  rm -f "$TOOLS_DIR/python.tar.gz"
  if [ ! -x "$TOOLS_DIR/python/bin/python3" ]; then
    fail "Python 解压失败"
    return 1
  fi
  export PATH="$TOOLS_DIR/python/bin:$PATH"
  ok "Python $(python3 --version 2>&1 | awk '{print $2}') 就绪"
  return 0
}

# ============================================================================
#  主流程
# ============================================================================

# 把已安装的 .tools 便携运行时提上 PATH（本会话生效）
use_local_tools

# ── 1. Go ──
step "1. Go（自动下载，缺 / 版本低时装到 .tools/go）"
ensure_go || { fail "Go 安装失败"; echo "  可手动装 https://go.dev/dl/ 后重跑"; MISSING=1; }

# ── 2. Node.js ──
step "2. Node.js（自动下载，缺 / 版本低时装到 .tools/node）"
ensure_node || { fail "Node.js 安装失败"; echo "  可手动装 https://nodejs.org/ 后重跑"; MISSING=1; }

# ── 3. Python ──
step "3. Python（自动下载，缺 / 版本低时装到 .tools/python）"
ensure_python

if [ "$MISSING" = 1 ]; then
  echo ""
  fail "请先解决上述缺失的运行时，再重新运行本脚本。"
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
# 国内直连 proxy.golang.org 极慢或被墙（go: downloading 长期无进展且不报错），
# 未显式设置 GOPROXY 时默认走 goproxy.cn 优先的代理链（海外可正常回退）。
export GOPROXY="${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}"
# GOTOOLCHAIN=auto：本机 Go 低于 go.mod 要求时自动下载对应工具链（此处用 .tools 的
# Go 已保证版本，通常无需额外下载；保留该前缀兜底 GOTOOLCHAIN=local 的环境）。
GOTOOLCHAIN=auto go build -trimpath -o bin/agent-server ./cmd/server && ok "bin/agent-server"
GOTOOLCHAIN=auto go build -trimpath -o bin/agent-scheduler ./cmd/scheduler && ok "bin/agent-scheduler"
GOTOOLCHAIN=auto go build -trimpath -o bin/agent-supervisor ./cmd/supervisor && ok "bin/agent-supervisor"

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
  bash skills/setup.sh 2>&1 | while IFS= read -r line; do printf '    %s\n' "$line"; done
  rc=${PIPESTATUS[0]}
  if [ "$rc" -ne 0 ]; then
    warn "skills 依赖安装失败（退出码 $rc，详见上方输出）"
  fi
else
  warn "缺少 skills/setup.sh，跳过（skills 依赖未安装）"
fi

echo ""
echo -e "${GREEN}=== 安装完成 ===${NC}"
echo "一键启动：bash start.sh"
