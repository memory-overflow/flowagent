#!/usr/bin/env bash
# ============================================================================
#  Go 编译环境准备（被 install.sh / start.sh / build.sh source 后调用）
#
#  抽成公共文件的原因：三处都要编译后端，而这组坑必须每一处都兜住，零散复制容易漏改。
#    1) GOPROXY：国内直连 proxy.golang.org 极慢或被墙，表现为 "go: downloading ..."
#       长时间无进展且不报错，重试逻辑永远不会触发。默认改走 goproxy.cn 优先的链。
#
#    2) GOTOOLCHAIN=auto：go.mod 要求 go 1.25.5（mcp-go v0.57.0 的硬性要求），
#       本机 Go 更旧时会自动下载 golang.org/toolchain 模块补齐版本。
#
#    3) GOSUMDB：工具链模块**强制**走校验和数据库校验，GOPRIVATE / GONOSUMDB 都
#       豁免不了。GOSUMDB=off 时必然失败：
#         go: golang.org/toolchain@v0.0.1-go1.25.5.darwin-arm64: verifying module:
#             checksum database disabled by GOSUMDB=off
#       因此只在「确实需要下载工具链」时临时恢复默认 sumdb —— 仅当前进程生效，
#       不写用户的 go env 配置。普通依赖都在 go.sum 里，不需要查 sumdb，无副作用。
#
#  用法：
#    source scripts/go-build-env.sh
#    prepare_go_build_env
#    go_build bin/agent-server ./cmd/server
# ============================================================================

# _ver_ge <actual> <required>：actual >= required 返回 0，否则 1。
_ver_ge() {
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

# _go_local_version：本机 Go 版本（如 1.24.3），没装 Go 时输出空。
# 必须带 GOTOOLCHAIN=local：在仓库目录里直接 `go version` 会先按 go.mod 切到已下载的
# 工具链再输出（例如本机是 1.24.3 却打印 1.25.5），那样就判断不出「要不要下载工具链」了。
_go_local_version() {
  command -v go &>/dev/null || return 0
  GOTOOLCHAIN=local go version 2>/dev/null | awk '{print $3}' | sed -e 's/^go//' -e 's/[^0-9.].*$//'
}

# _go_required_version：go.mod 声明的版本（如 1.25.5），读不到时输出空。
_go_required_version() {
  [ -f go.mod ] || return 0
  awk '/^go[ \t]+[0-9]/{print $2; exit}' go.mod
}

# prepare_go_build_env：export 编译所需的 Go 环境变量。
prepare_go_build_env() {
  export GOPROXY="${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}"
  export GOTOOLCHAIN=auto

  local req loc sumdb
  req="$(_go_required_version)"
  loc="$(_go_local_version)"

  # 本机 Go 已满足 go.mod 要求 → 不会下载工具链，没必要动 GOSUMDB。
  if [ -n "$req" ] && [ -n "$loc" ] && _ver_ge "$loc" "$req"; then
    return 0
  fi

  sumdb="$(GOTOOLCHAIN=local go env GOSUMDB 2>/dev/null || true)"
  case "$sumdb" in
    off|"")
      export GOSUMDB=sum.golang.org
      echo "  [提示] GOSUMDB=${sumdb:-off} 会拒绝工具链模块校验；本次编译临时使用 GOSUMDB=sum.golang.org（不改动你的 go env 配置）"
      ;;
  esac
}

# go_build <输出路径> <包路径>：编译，失败时给出针对工具链 / 校验和问题的排查提示。
go_build() {
  local out="$1" pkg="$2"
  if GOTOOLCHAIN=auto go build -trimpath -o "$out" "$pkg"; then
    return 0
  fi

  local req loc
  req="$(_go_required_version)"
  loc="$(_go_local_version)"
  echo "" >&2
  echo "编译 ${pkg} 失败。" >&2
  if [ -n "$req" ] && [ -n "$loc" ] && ! _ver_ge "$loc" "$req"; then
    echo "本机 Go ${loc} 低于 go.mod 要求的 ${req}，编译需要先自动下载 go${req} 工具链。" >&2
    echo "若报错含 \"checksum database disabled by GOSUMDB=off\"，任选其一：" >&2
    echo "  1) 安装 Go >= ${req}（推荐，之后不再下载工具链）：" >&2
    echo "       macOS: brew upgrade go      其他: https://go.dev/dl/" >&2
    echo "  2) 恢复校验和数据库（国内把 GOPROXY 设为 goproxy.cn 即可代理 sumdb）：" >&2
    echo "       go env -w GOSUMDB=sum.golang.org" >&2
  fi
  return 1
}
