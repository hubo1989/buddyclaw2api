#!/usr/bin/env bash
# goenv.sh — 定位 Go 工具链（由各脚本 source 引入）
#
# 为什么需要这个文件：
#   macOS 上用 Homebrew 装的 go 位于 /opt/homebrew/bin，但很多终端的 PATH 里
#   没有 `eval "$(brew shellenv)"`，于是 `go build` 直接 command not found。
#   各脚本又都带 `set -e`，会以一句难以理解的报错中断。
#
# 用法：
#   source "$(dirname "$0")/scripts/goenv.sh"
#   require_go || exit 1
#   "$GO_BIN" build -o ./bin ./cmd/xxx

require_go() {
    local cand
    GO_BIN=""
    if command -v go >/dev/null 2>&1; then
        GO_BIN="$(command -v go)"
    else
        for cand in \
            /opt/homebrew/bin/go \
            /opt/homebrew/opt/go/bin/go \
            /usr/local/go/bin/go \
            /usr/local/bin/go \
            /usr/lib/go/bin/go \
            "$HOME/go/bin/go" \
            "$HOME/.local/go/bin/go"; do
            if [[ -x "$cand" ]]; then
                GO_BIN="$cand"
                break
            fi
        done
    fi
    if [[ -z "$GO_BIN" ]]; then
        echo "错误：未找到 Go 工具链，无法构建。" >&2
        echo "  PATH 中没有 go，常见安装位置也都没有。请先安装：" >&2
        echo "    brew install go        # 或从 https://go.dev/dl/ 下载" >&2
        return 1
    fi
    export GO_BIN
    return 0
}
