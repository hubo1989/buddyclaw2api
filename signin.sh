#!/bin/bash
# 批量签到脚本：遍历 auths/ 下所有 workbuddy-*.json 账号
# 用法: ./signin.sh [auths_dir]
set -e
cd "$(dirname "$0")"

# go 未必在 PATH 里（macOS 上 brew 装的 go 常缺 shellenv），由 goenv.sh 兜底定位。
source "./scripts/goenv.sh"
BIN=./signin_bin
if [ ! -x "$BIN" ]; then
    echo "build signin_bin ..."
    require_go || exit 1
    "$GO_BIN" build -o "$BIN" ./cmd/signin
fi

exec "$BIN" "${1:-auths}"
