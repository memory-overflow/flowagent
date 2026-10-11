#!/usr/bin/env bash
# ============================================================================
# flowagent 停止脚本（Linux / macOS）
#
# 停止「当前项目目录」下由 start.sh 启动的服务进程：
#   agent-server + agent-scheduler + 前端（vite）
#
# 判断标准是进程的工作目录（cwd）：只停止 cwd 等于本脚本所在项目目录
# （后端两个进程）或其 frontend 子目录（前端）的进程，因此即使其他目录
# 也启动了同名服务，也不会被误伤。
#
# 用法：bash stop.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
ok()   { echo -e "${GREEN}✓${NC} $*"; }
warn() { echo -e "${YELLOW}△${NC} $*"; }

# proc_cwd 获取指定进程的工作目录。
# Linux 用 /proc/<pid>/cwd；macOS 用 lsof。拿不到返回空串。
proc_cwd() {
  local pid="$1"
  if [ -d /proc ] && [ -r "/proc/$pid/cwd" ]; then
    readlink "/proc/$pid/cwd" 2>/dev/null
  else
    lsof -a -d cwd -p "$pid" -Fn 2>/dev/null | awk '/^n/{print substr($0,2)}'
  fi
}

# stop_by_cwd 停止「进程名匹配且 cwd 等于指定目录」的进程，并输出结果。
stop_by_cwd() {
  local name="$1" expect_dir="$2" label="$3"
  local pid n=0
  for pid in $(pgrep -x "$name" 2>/dev/null); do
    if [ "$(proc_cwd "$pid")" = "$expect_dir" ]; then
      kill "$pid" 2>/dev/null && n=$((n + 1))
    fi
  done
  if [ "$n" -gt 0 ]; then
    ok "$label: 已停止 $n 个进程"
  else
    echo "  $label: 无本项目进程（${expect_dir}）"
  fi
}

echo "=== flowagent 停止 ==="
echo "项目目录: $ROOT"

# 后端两个进程：cwd = 项目根
stop_by_cwd agent-server "$ROOT" "agent-server"
stop_by_cwd agent-scheduler "$ROOT" "agent-scheduler"

# 前端：npm + node(vite) 进程，cwd = 项目根/frontend
FRONTEND="$ROOT/frontend"
n=0
for name in node npm; do
  for pid in $(pgrep -x "$name" 2>/dev/null); do
    if [ "$(proc_cwd "$pid")" = "$FRONTEND" ]; then
      kill "$pid" 2>/dev/null && n=$((n + 1))
    fi
  done
done
if [ "$n" -gt 0 ]; then
  ok "前端(vite): 已停止 $n 个进程"
else
  echo "  前端(vite): 无本项目进程（${FRONTEND}）"
fi

echo "完成。"
