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
CHECKIN_LABEL="${LABEL}.opencodex-checkin"
CHECKIN_PLIST="$HOME/Library/LaunchAgents/${CHECKIN_LABEL}.plist"
APP_DIR="$HOME/Library/Application Support/workbuddy2api"
BIN="$APP_DIR/wb2api"
CHECKIN_BIN="$APP_DIR/opencodex-checkin"
LISTEN="127.0.0.1:7863"
PORT="${LISTEN##*:}"
LOG_DIR="$HOME/Library/Logs/workbuddy2api"
LOG_OUT="$LOG_DIR/wb2api.out.log"
LOG_ERR="$LOG_DIR/wb2api.err.log"
CHECKIN_LOG_OUT="$LOG_DIR/opencodex-checkin.out.log"
CHECKIN_LOG_ERR="$LOG_DIR/opencodex-checkin.err.log"
DOMAIN="gui/$(id -u)"
SERVICE="${DOMAIN}/${LABEL}"
CHECKIN_SERVICE="${DOMAIN}/${CHECKIN_LABEL}"

say() { printf '%s\n' "$*"; }

# go 未必在 PATH 里（macOS 上 brew 装的 go 常缺 shellenv），由 goenv.sh 兜底定位。
source "$ROOT/scripts/goenv.sh"

build_bin() {
    # force=1 时即使二进制存在也重建，避免代码更新后 launchd 继续跑旧版本。
    # APP_DIR 必须先建：首次安装时 go build 的 -o 目标位于该目录。
    local force="${1:-0}"
    mkdir -p "$APP_DIR" "$LOG_DIR"
    chmod 700 "$APP_DIR" "$LOG_DIR"
    if [[ "$force" == "1" || ! -x "$BIN" ]]; then
        say "构建二进制 $BIN ..."
        require_go || exit 1
        # 安装到内置卷：macOS launchd/xpcproxy 映射外置卷二进制可能停在 dyld open。
        ( cd "$ROOT" && "$GO_BIN" build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/server )
        ( cd "$ROOT" && "$GO_BIN" build -trimpath -ldflags="-s -w" -o "$CHECKIN_BIN" ./cmd/opencodex-checkin )
    fi
}


# 安装运行时快照到内置卷：macOS launchd 进程访问外置卷文件可能在 open() 卡住。
# 每次安装/重启同步 CLI 新账号，同时不回滚运行时刷新出的较新 token。
install_runtime() {
    mkdir -p "$APP_DIR/auths" "$APP_DIR/data" "$APP_DIR/scripts" "$APP_DIR/config-assets"
    chmod 700 "$APP_DIR" "$APP_DIR/auths" "$APP_DIR/data" "$APP_DIR/config-assets"

    python3 - "$ROOT" "$APP_DIR" <<'PY'
import hashlib
import json
import os
import shutil
import sys
from pathlib import Path

root = Path(sys.argv[1]).resolve()
app = Path(sys.argv[2]).resolve()
config = {}
source_config = root / "config.json"
if source_config.exists():
    config = json.loads(source_config.read_text())
assets = app / "config-assets"

def copy_file(value):
    source = (root / value).resolve()
    if not source.is_file():
        return value
    try:
        relative = str(source.relative_to(root))
    except ValueError:
        relative = str(source)
    name = hashlib.sha256(relative.encode()).hexdigest()[:16] + (source.suffix or ".bin")
    target = assets / name
    shutil.copy2(source, target)
    os.chmod(target, 0o600)
    return str(target)

def normalize(node):
    if isinstance(node, dict):
        out = {}
        for key, value in node.items():
            if isinstance(value, str) and value and not os.path.isabs(value):
                if key == "auth_dir":
                    source = (root / value).resolve()
                    target = assets / ("auth-dir-" + hashlib.sha256(value.encode()).hexdigest()[:16])
                    if source.is_dir():
                        shutil.copytree(source, target, dirs_exist_ok=True)
                        value = str(target)
                elif key in {"file", "device_token_file"}:
                    value = copy_file(value)
            out[key] = normalize(value)
        return out
    if isinstance(node, list):
        return [normalize(value) for value in node]
    return node

config = normalize(config)
config.setdefault("auth_dir", str(app / "auths"))
config.setdefault("autoclaw", {}).setdefault("auth_dir", str(app / "auths"))
config.setdefault("qoder", {}).setdefault("auth_dir", str(app / "auths"))
config.setdefault("state_file", str(app / "data" / "state.json"))
target_config = app / "config.json"
target_config.write_text(json.dumps(config, ensure_ascii=False, indent=2) + "\n")
os.chmod(target_config, 0o600)

def expiry(data):
    nested = data.get("auth", {}) if isinstance(data, dict) else {}
    return int(nested.get("expiresAt", data.get("expiresAt", 0)) or 0)

runtime = app / "auths"
for source in (root / "auths").glob("*.json"):
    if source.name.split("-", 1)[0] not in {"workbuddy", "autoclaw", "qoder"}:
        continue
    target = runtime / source.name
    if not target.exists():
        shutil.copy2(source, target)
        os.chmod(target, 0o600)
        continue
    try:
        before, after = json.loads(source.read_text()), json.loads(target.read_text())
    except Exception:
        continue
    # CLI 新账号/重登文件只在 expiry 不旧于运行时快照时同步；运行时刷新的新 token 不被旧文件覆盖。
    if source.stat().st_mtime > target.stat().st_mtime and expiry(before) >= expiry(after):
        shutil.copy2(source, target)
        os.chmod(target, 0o600)

state = app / "data" / "state.json"
source_state = root / "data" / "state.json"
if not state.exists() and source_state.exists():
    shutil.copy2(source_state, state)
PY

    cp -R "$ROOT/scripts/." "$APP_DIR/scripts/"
}

write_plist() {
    mkdir -p "$HOME/Library/LaunchAgents" "$APP_DIR" "$LOG_DIR" "$ROOT/auths" "$ROOT/data"
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
        <string>${APP_DIR}/config.json</string>
    </array>

    <!-- macOS 27 launchd 访问外置卷 cwd/文件可能卡住；运行时快照和 cwd 都使用内置卷。 -->
    <key>WorkingDirectory</key>
    <string>${APP_DIR}</string>

    <key>EnvironmentVariables</key>
    <dict>
        <!-- 只绑回环：仅本机可达，启动闸门只告警不拦，故无需 api_key -->
        <key>WB2A_LISTEN</key>
        <string>${LISTEN}</string>
        <key>WB2A_AUTH_DIR</key>
        <string>${APP_DIR}/auths</string>
        <key>WB2A_STATE_FILE</key>
        <string>${APP_DIR}/data/state.json</string>
        <key>WB2A_ROOT</key>
        <string>${APP_DIR}</string>
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
    <string>${LOG_OUT}</string>
    <key>StandardErrorPath</key>
    <string>${LOG_ERR}</string>
</dict>
</plist>
PLIST_EOF

    cat > "$CHECKIN_PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${CHECKIN_LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${CHECKIN_BIN}</string>
        <string>-auth</string>
        <string>${HOME}/.opencodex/auth.json</string>
        <string>-state</string>
        <string>${APP_DIR}/opencodex-checkin-state.json</string>
        <string>-opencodex-home</string>
        <string>${HOME}/.opencodex</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>TZ</key>
        <string>Asia/Shanghai</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>StartInterval</key>
    <integer>300</integer>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>${CHECKIN_LOG_OUT}</string>
    <key>StandardErrorPath</key>
    <string>${CHECKIN_LOG_ERR}</string>
</dict>
</plist>
PLIST_EOF
}

loaded() { launchctl print "$1" >/dev/null 2>&1; }

install_one() {
    local plist="$1" service="$2"
    plutil -lint "$plist" >/dev/null || { say "❌ plist 语法错误: $plist"; exit 1; }
    if loaded "$service"; then
        launchctl bootout "$service" 2>/dev/null || launchctl unload -w "$plist" 2>/dev/null || true
        sleep 1
    fi
    launchctl bootstrap "$DOMAIN" "$plist" 2>/dev/null || true
    if ! loaded "$service"; then
        launchctl load -w "$plist" 2>/dev/null || true
    fi
}

do_install() {
    build_bin 1
    install_runtime
    write_plist
    install_one "$PLIST" "$SERVICE"
    install_one "$CHECKIN_PLIST" "$CHECKIN_SERVICE"

    sleep 2
    if loaded "$SERVICE"; then
        say "✅ 已注册并启动：${LABEL}"
        say "   监听：http://${LISTEN}"
        say "   开机自启：已开启（RunAtLoad + KeepAlive）"
        say "   日志：${LOG_OUT} / ${LOG_ERR}"
    else
        say "❌ 网关加载失败：请手动执行 launchctl bootstrap ${DOMAIN} \"${PLIST}\""
        exit 1
    fi
    if loaded "$CHECKIN_SERVICE"; then
        say "✅ 已注册 OpenCodeX 每日签到器：${CHECKIN_LABEL}"
        say "   触发：启动后与每 300 秒；进程内检测 OpenCodeX 存活并按自然日去重"
        say "   日志：${CHECKIN_LOG_OUT} / ${CHECKIN_LOG_ERR}"
    else
        say "❌ 签到器加载失败：请手动执行 launchctl bootstrap ${DOMAIN} \"${CHECKIN_PLIST}\""
        exit 1
    fi
}

do_uninstall() {
    if loaded "$SERVICE"; then
        launchctl bootout "$SERVICE" 2>/dev/null || launchctl unload -w "$PLIST" 2>/dev/null || true
        say "✅ 已停止网关"
    else
        say "网关未加载"
    fi
    if loaded "$CHECKIN_SERVICE"; then
        launchctl bootout "$CHECKIN_SERVICE" 2>/dev/null || launchctl unload -w "$CHECKIN_PLIST" 2>/dev/null || true
        say "✅ 已停止 OpenCodeX 每日签到器"
    else
        say "签到器未加载"
    fi
    for plist in "$PLIST" "$CHECKIN_PLIST"; do
        if [[ -f "$plist" ]]; then
            rm -f "$plist"
            say "✅ 已删除 ${plist}（不再开机自启）"
        fi
    done
}

do_restart() {
    if ! loaded "$SERVICE"; then
        say "❌ 服务未加载，先执行：./service.sh install"
        exit 1
    fi
    install_runtime
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
    if loaded "$SERVICE"; then
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
    echo "进程:    $(pgrep -nfl "$BIN" 2>/dev/null || echo '无')"
    if loaded "$CHECKIN_SERVICE"; then
        echo "签到器:  已加载 ${CHECKIN_SERVICE}"
    else
        echo "签到器:  未加载"
    fi
    # 探测本地端口必须绕过 http_proxy：否则代理会把"连接被拒"伪装成 502
    code="$(curl -s -o /dev/null -m 3 --noproxy '*' -w '%{http_code}' "http://${LISTEN}/healthz" 2>/dev/null || true)"
    echo "healthz: ${code:-连不上}"
    accounts="$(curl -s -m 3 --noproxy '*' "http://${LISTEN}/status" 2>/dev/null \
        | python3 -c "import json,sys; d=json.load(sys.stdin); print(d['total'])" 2>/dev/null || echo '?')"
    echo "账号数:  ${accounts}"
    echo "============================================================"
}

do_logs() {
    mkdir -p "$LOG_DIR"
    touch "$LOG_OUT" "$LOG_ERR" "$CHECKIN_LOG_OUT" "$CHECKIN_LOG_ERR"
    say "跟踪日志（Ctrl-C 退出）..."
    tail -f "$LOG_OUT" "$LOG_ERR" "$CHECKIN_LOG_OUT" "$CHECKIN_LOG_ERR"
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
