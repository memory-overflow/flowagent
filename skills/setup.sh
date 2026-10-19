#!/usr/bin/env bash
# ============================================================================
# flowagent skills 部署环境 —— 首次安装脚本（macOS / Linux）
#
# 用途：一键装好 3 个脚本型 skill 的运行时依赖，让内置 run_command 工具
#       能真正驱动它们落地执行。详见同目录 DEPENDENCIES.md。
#
# 覆盖内容：
#   1) pdfkit-py     PDF 处理（Python 3.10+，核心依赖；可选依赖 --all）
#   2) minimax-xlsx  Excel 处理（核心脚本纯标准库零依赖；可选 pandas+openpyxl --all）
#   3) browser-use   浏览器自动化（uv 装 CLI，Python 3.12）
#   4) 外部系统工具  gs / tesseract / soffice / libreoffice（仅检测+提示，不强制装）
#
# 用法：
#   bash setup.sh            仅装核心依赖（推荐首次）
#   bash setup.sh --all      连可选依赖一起装（表格提取/公式识别/pandas 等，体积大）
#   bash setup.sh --help     查看帮助
#
# 幂等：可重复执行，已装的会跳过。
# ============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_ALL=false

for arg in "$@"; do
  case "$arg" in
    --all|-a)  INSTALL_ALL=true ;;
    --help|-h)
      sed -n '2,20p' "${BASH_SOURCE[0]}"
      exit 0 ;;
  esac
done

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
ok()   { echo -e "${GREEN}✓${NC} $*"; }
warn() { echo -e "${YELLOW}△${NC} $*"; }
fail() { echo -e "${RED}✗${NC} $*"; }
step() { echo -e "\n${CYAN}── $* ──${NC}"; }

OS="$(uname -s)"
echo "=== flowagent skills 环境初始化 ==="
echo "平台: $OS  目录: $SCRIPT_DIR  模式: $([ "$INSTALL_ALL" = true ] && echo '全量(含可选)' || echo '核心')"

# 结果收集，用于末尾汇总（不因单个 skill 失败而中断整体）
RESULTS=""

# ─────────────────────────────────────────────────────────────
step "0. 环境检测"
# ─────────────────────────────────────────────────────────────
has() { command -v "$1" &>/dev/null; }

if has python3; then
  ok "python3: $(python3 --version 2>&1)"
else
  warn "python3 未找到（pdfkit 的 setup.sh 会自行下载独立 Python，可继续）"
fi

if has uv; then
  ok "uv: $(uv --version 2>&1)"
else
  warn "uv 未找到 —— browser-use 将跳过。安装 uv：brew install uv  或  curl -LsSf https://astral.sh/uv/install.sh | sh"
fi

# ─────────────────────────────────────────────────────────────
step "1. pdfkit-py（PDF 处理）"
# ─────────────────────────────────────────────────────────────
PDFKIT_SETUP="$SCRIPT_DIR/pdfkit-py/scripts/setup.sh"
if [ -f "$PDFKIT_SETUP" ]; then
  PDFKIT_ARGS=""
  [ "$INSTALL_ALL" = true ] && PDFKIT_ARGS="--all"
  if bash "$PDFKIT_SETUP" $PDFKIT_ARGS; then
    ok "pdfkit-py 就绪"; RESULTS="${RESULTS}  pdfkit-py: ok\n"
  else
    fail "pdfkit-py 安装失败（详见上方输出）"; RESULTS="${RESULTS}  pdfkit-py: FAIL\n"
  fi
else
  fail "缺少 $PDFKIT_SETUP"; RESULTS="${RESULTS}  pdfkit-py: 缺失 setup.sh\n"
fi

# ─────────────────────────────────────────────────────────────
step "2. minimax-xlsx（Excel 处理）"
# ─────────────────────────────────────────────────────────────
XLSX_DIR="$SCRIPT_DIR/minimax-xlsx/scripts"
if [ -f "$XLSX_DIR/formula_check.py" ]; then
  ok "核心脚本为纯标准库，零第三方依赖，无需安装"
  # 可选：README 分析模式需要 pandas + openpyxl
  if [ "$INSTALL_ALL" = true ]; then
    echo -n "  安装可选依赖 pandas+openpyxl ... "
    if has python3 && python3 -m pip install pandas openpyxl --quiet 2>/dev/null; then
      ok "pandas+openpyxl 已安装"
    else
      warn "pandas+openpyxl 安装失败（可手动：python3 -m pip install pandas openpyxl；仅 READ 分析模式需要）"
    fi
  fi
  # 验证：核心脚本纯标准库，做语法编译校验即可（不落盘）
  py_ok=true
  for f in "$XLSX_DIR"/*.py; do
    if ! python3 -c "import ast,sys; ast.parse(open(sys.argv[1],encoding='utf-8').read())" "$f" 2>/dev/null; then
      py_ok=false; break
    fi
  done
  if [ "$py_ok" = true ]; then
    ok "核心脚本语法校验通过（纯标准库）"; RESULTS="${RESULTS}  minimax-xlsx: ok\n"
  else
    warn "核心脚本语法校验异常"; RESULTS="${RESULTS}  minimax-xlsx: 警告\n"
  fi
else
  fail "缺少 $XLSX_DIR/formula_check.py"; RESULTS="${RESULTS}  minimax-xlsx: 缺失脚本\n"
fi

# ─────────────────────────────────────────────────────────────
step "3. browser-use（浏览器自动化）"
# ─────────────────────────────────────────────────────────────
if has browser-use; then
  ok "browser-use 已安装：$(browser-use --version 2>&1 | head -1)"
  RESULTS="${RESULTS}  browser-use: ok\n"
elif has uv; then
  # 去掉 >/dev/null 静默：browser-use 会连带下载独立 Python 3.12 + Playwright +
  # Chromium 内核（几百 MB），静默时像卡死。透传 uv 的实时下载进度。
  echo "  用 uv 安装 browser-use CLI（Python 3.12），下载较大（含 Playwright/Chromium），请耐心等待..."
  if uv tool install --python 3.12 browser-use; then
    ok "browser-use 已安装：$(browser-use --version 2>&1 | head -1)"
    RESULTS="${RESULTS}  browser-use: ok\n"
  else
    fail "browser-use 安装失败（可选，不影响核心功能），可手动：uv tool install --python 3.12 browser-use"
    RESULTS="${RESULTS}  browser-use: FAIL\n"
  fi
else
  warn "跳过 browser-use（缺少 uv）。装好 uv 后手动执行：uv tool install --python 3.12 browser-use"
  RESULTS="${RESULTS}  browser-use: 跳过(缺 uv)\n"
fi

# ─────────────────────────────────────────────────────────────
step "4. 外部系统工具（可选，按需）"
# ─────────────────────────────────────────────────────────────
check_tool() {
  local name="$1" hint="$2"
  if has "$name"; then
    ok "$name"
  else
    warn "$name 未安装 —— $hint"
  fi
}
check_tool gs        "PDF 压缩需要：brew install ghostscript / sudo apt install -y ghostscript"
check_tool tesseract "OCR 需要：brew install tesseract / sudo apt install -y tesseract-ocr（中文加 tesseract-lang）"
check_tool soffice   "格式转换需要：brew install --cask libreoffice / sudo apt install -y libreoffice"

# ─────────────────────────────────────────────────────────────
step "验证汇总"
# ─────────────────────────────────────────────────────────────
echo ""
echo -e "安装结果：\n${RESULTS}"
if has browser-use; then
  echo -e "browser-use 连接状态（本地需批准 Chrome 远程调试）："
  browser-use --doctor 2>&1 | sed 's/^/  /' | head -12 || true
fi

echo ""
echo "完成。核心依赖已就绪，可重复运行本脚本做增量补齐。"
echo "提示：browser-use 首次连接本地 Chrome 会弹出「允许远程调试」，批准后执行 \`browser-use mac-approve\`（macOS）。"
