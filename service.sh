#!/usr/bin/env bash
# service.sh — 把 workbuddy2api 注册为 macOS 开机自启服务（launchd LaunchAgent）
#
# 用法:
#   ./service.sh install     生成 plist → 加载 → 开机自启（幂等，可反复执行）
#   ./service.sh uninstall   停止并移除开机自启
#   ./service.sh restart     重启（登录添加新账号后加载凭证用）
#   ./service.sh status      查看服务状态 + 端口探测
#   ./service.sh logs        跟踪日志（Ctrl-C 退出）
#
# 本机自用：只绑回环 127.0.0.1:7863，仅本机可达，因此不需要 api_key。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
LABEL="com.hubo.workbuddy2api"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"
BIN="$ROOT/wb2api"
LISTEN="127.0.0.1:7863"
PORT="${LISTEN##*:}"
DOMAIN="gui/$(id -u)"
SERVICE="${DOMAIN}/${LABEL}"

say() { printf '%s\n' "$*"; }

# go 未必在 PATH 里（macOS 上 brew 装的 go 常缺 shellenv），由 goenv.sh 兜底定位。
source "$ROOT/scripts/goenv.sh"

build_bin() {
    if [[ ! -x "$BIN" ]]; then
        say "构建二进制 $BIN ..."
        require_go || exit 1
        ( cd "$ROOT" && "$GO_BIN" build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server )
    fi
}

write_plist() {
    mkdir -p "$HOME/Library/LaunchAgents" "$ROOT/logs" "$ROOT/auths" "$ROOT/data"
    cat > "$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LABEL}</string>

    <key>ProgramArguments</key>
    <array>
        <string>${BIN}</string>
        <string>-config</string>
        <string>${ROOT}/config.json</string>
    </array>

    <!-- 相对路径（auths/、data/）依赖 cwd，必须显式指定 -->
    <key>WorkingDirectory</key>
    <string>${ROOT}</string>

    <key>EnvironmentVariables</key>
    <dict>
        <!-- 只绑回环：仅本机可达，启动闸门只告警不拦，故无需 api_key -->
        <key>WB2A_LISTEN</key>
        <string>${LISTEN}</string>
        <key>WB2A_AUTH_DIR</key>
        <string>${ROOT}/auths</string>
        <key>WB2A_STATE_FILE</key>
        <string>${ROOT}/data/state.json</string>
        <key>TZ</key>
        <string>Asia/Shanghai</string>
    </dict>

    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <!-- 崩溃重启的最小间隔，避免配置错误时打爆日志 -->
    <key>ThrottleInterval</key>
    <integer>10</integer>

    <key>ProcessType</key>
    <string>Background</string>

    <key>StandardOutPath</key>
    <string>${ROOT}/logs/wb2api.out.log</string>
    <key>StandardErrorPath</key>
    <string>${ROOT}/logs/wb2api.err.log</string>
</dict>
</plist>
PLIST_EOF
}

loaded() { launchctl print "$SERVICE" >/dev/null 2>&1; }

do_install() {
    build_bin
    write_plist
    plutil -lint "$PLIST" >/dev/null || { say "❌ plist 语法错误: $PLIST"; exit 1; }

    # 幂等：已注册先卸掉再装
    if loaded; then
        launchctl bootout "$SERVICE" 2>/dev/null || launchctl unload -w "$PLIST" 2>/dev/null || true
        sleep 1
    fi

    # 注意：launchctl load 失败时也会返回退出码 0（只往 stderr 打印 "Load failed"），
    # 因此这里绝不能靠退出码判断成败，必须事后用 loaded() 实测。
    launchctl bootstrap "$DOMAIN" "$PLIST" 2>/dev/null || true
    if ! loaded; then
        launchctl load -w "$PLIST" 2>/dev/null || true
    fi

    sleep 2
    if loaded; then
        say "✅ 已注册并启动：${LABEL}"
        say "   监听：http://${LISTEN}"
        say "   开机自启：已开启（RunAtLoad + KeepAlive）"
        say "   日志：${ROOT}/logs/wb2api.{out,err}.log"
    else
        say "❌ 加载失败：当前环境不允许写 launchd 域（plist 已就位，语法已校验通过）。"
        say "   请在你自己的终端（不是本工具的会话）里手动执行这一条："
        say "     launchctl bootstrap ${DOMAIN} \"${PLIST}\""
        say "   或重登录一次 —— ~/Library/LaunchAgents/ 下的 plist 会在下次登录时自动加载。"
        exit 1
    fi
}

do_uninstall() {
    if loaded; then
        launchctl bootout "$SERVICE" 2>/dev/null || launchctl unload -w "$PLIST" 2>/dev/null || true
        say "✅ 已停止并从 launchd 卸载"
    else
        say "服务未加载"
    fi
    if [[ -f "$PLIST" ]]; then
        rm -f "$PLIST"
        say "✅ 已删除 ${PLIST}（不再开机自启）"
    fi
}

do_restart() {
    if ! loaded; then
        say "❌ 服务未加载，先执行：./service.sh install"
        exit 1
    fi
    if launchctl kickstart -k "$SERVICE" 2>/dev/null; then
        say "✅ 已重启 ${LABEL}"
    else
        # kickstart 不可用时退回 bootout+bootstrap
        launchctl bootout "$SERVICE" 2>/dev/null || true
        sleep 1
        launchctl bootstrap "$DOMAIN" "$PLIST"
        say "✅ 已重启 ${LABEL}（bootout+bootstrap）"
    fi
    sleep 1
    do_status
}

do_status() {
    echo "============================================================"
    if loaded; then
        echo "launchd: 已加载 ${SERVICE}"
        # 注意：BSD grep（macOS）不支持 \s，必须用 [[:space:]]；且无匹配时 grep 返回 1，
        # 在 set -o pipefail + set -e 下会直接杀掉整个脚本 —— 所以用 { ...; } || true 兜住。
        {
            launchctl print "$SERVICE" 2>/dev/null \
                | grep -E "^[[:space:]]*(state|pid|last exit code|runs) " \
                | sed 's/^/  /'
        } || true
    else
        echo "launchd: 未加载（./service.sh install 可开启开机自启）"
    fi
    echo "进程:    $(pgrep -fl "$BIN" 2>/dev/null | head -1 || echo '无')"
    # 探测本地端口必须绕过 http_proxy：否则代理会把"连接被拒"伪装成 502
    code="$(curl -s -o /dev/null -m 3 --noproxy '*' -w '%{http_code}' "http://${LISTEN}/healthz" 2>/dev/null || true)"
    echo "healthz: ${code:-连不上}"
    accounts="$(curl -s -m 3 --noproxy '*' "http://${LISTEN}/status" 2>/dev/null \
        | python3 -c "import json,sys; d=json.load(sys.stdin); print(d['total'])" 2>/dev/null || echo '?')"
    echo "账号数:  ${accounts}"
    echo "============================================================"
}

do_logs() {
    mkdir -p "$ROOT/logs"
    touch "$ROOT/logs/wb2api.out.log" "$ROOT/logs/wb2api.err.log"
    say "跟踪日志（Ctrl-C 退出）..."
    tail -f "$ROOT/logs/wb2api.out.log" "$ROOT/logs/wb2api.err.log"
}

case "${1:-}" in
    install)   do_install ;;
    uninstall) do_uninstall ;;
    restart)   do_restart ;;
    status)    do_status ;;
    logs)      do_logs ;;
    *)
        sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
        exit 1
        ;;
esac
