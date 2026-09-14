#!/usr/bin/env bash
# login.sh — WorkBuddy CN OAuth 登录 → 落盘 auth 文件
#
# 用法:
#   ./login.sh
#   API_KEY=<你的 api_key> ./login.sh   # 可选：登录后顺带查询账号数
#
# 环境变量:
#   API_KEY  可选。服务端鉴权 key，仅用于登录后查询 /status 的账号数。
#            未设置时会自动尝试读 config.json 的 api_key；两者都没有则跳过该步。
#            不提供内置默认值——密钥不得进入版本库。
#
# 流程:
#   1. POST /v2/plugin/auth/state 拿授权 URL（无 PKCE，state 由服务端签发）
#   2. 你在浏览器打开 URL 完成登录
#   3. 脚本自动轮询拿 token+uid+nickname（无需按键）→ 立即落盘 auths/workbuddy-<uid>.json
#      （凭证先落盘再签到：后续任何步骤失败都不会丢 token，state 是一次性的）
#   4. 上游每日签到（幂等，失败不阻塞）
#   5. 重启服务加载新账号（docker 容器 或 launchd 服务；账号目录不做运行期热加载）
#
# ⚠️ 改本脚本时注意：变量引用紧贴中文标点必须写成 ${VAR}。
#    macOS 自带 bash 3.2 在 C.UTF-8 locale 下会把多字节字符吞进变量名，
#    变量名后面直接跟全角逗号/括号时会被解析成 "USER_ID<全角标点>" → unbound variable 直接中止脚本。
#    反例(✗)：echo "uid=$USER_ID<中文标点>"      正例(✓)：echo "uid=${USER_ID}<中文标点>"
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"
CONTAINER="workbuddy2api"

mkdir -p "$AUTH_DIR"

# login 工具：不存在才编译（源码改动后手动 go build -o login ./cmd/login）
# go 未必在 PATH 里（macOS 上 brew 装的 go 常缺 shellenv），由 goenv.sh 兜底定位。
source "./scripts/goenv.sh"
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" ]]; then
    require_go || exit 1
    echo "构建 login 工具（$("$GO_BIN" version)）..."
    "$GO_BIN" build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo "  WorkBuddy OAuth 登录"
echo "============================================================"
echo ""

# 告知 login 工具"已被 login.sh 编排"，避免它额外打印"请用 ./login.sh"的提示
export WB2A_LOGIN_ORCHESTRATED=1
AUTH_URL=$("$LOGIN_BIN" url)

echo "请在浏览器中打开以下链接完成登录："
echo ""
echo "  $AUTH_URL"
echo ""

# 授权 URL 尽量复制到剪贴板：macOS 用 pbcopy，Linux 用 xclip/xsel。
if command -v pbcopy &>/dev/null; then
    echo -n "$AUTH_URL" | pbcopy 2>/dev/null && echo "(已复制到剪贴板)"
elif command -v xclip &>/dev/null; then
    echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(已复制到剪贴板)"
elif command -v xsel &>/dev/null; then
    echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(已复制到剪贴板)"
fi

echo ""
echo "请在浏览器完成登录 —— 脚本会自动等待，无需按任何键（等待期间可按 Ctrl-C 中止）。"
echo "  ⚠️ 每次重跑都会签发新的 state，旧链接立即失效；请务必打开上面最新打印的那条链接。"
echo ""

echo "正在等待浏览器完成登录（最长 5 分钟）..."

RESULT=$("$LOGIN_BIN" poll) || {
    echo ""
    echo "获取 token 失败 —— 具体原因见上一行以 \"login:\" 开头的提示。常见情况："
    echo "  - 浏览器里没真正走完登录（授权页需要确认，或账号已被登出）"
    echo "  - 打开的是旧链接：每次重跑都会签发新 state，必须用最新打印的那条"
    echo "  - 登录的不是 CN 平台账号"
    exit 1
}

TOKEN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['access_token'])")
REFRESH=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['refresh_token'])")
EXPIRES_IN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['expires_in'])")
DOMAIN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('domain',''))")
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('uid',''))")
ENT_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('enterprise_id',''))")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('nickname',''))")

if [[ -z "$USER_ID" ]]; then
    echo "无法获取 uid，请检查 token 是否有效"
    exit 1
fi

EXPIRES_AT=$(( $(date +%s) + EXPIRES_IN ))

# ─── 落盘 auth 文件（与 internal/auth 读取格式一致）─────────────────
# 位置很关键：必须在签到与重启之前。state 是一次性的，poll 成功后凭证只存在于内存，
# 任何后续步骤中途失败（例如装饰性输出报错）都会永久丢掉这份 token。
AUTH_FILE="$AUTH_DIR/workbuddy-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
    ACTION="覆盖"
else
    ACTION="新增"
fi

# 变量值一律走环境变量传给 python，heredoc 用 <<'PYEOF' 禁止 shell 展开：
# nickname / domain 里若含引号、$ 或反引号，旧写法会破坏 python 字面量甚至注入代码。
# 注意 USER_ID="$USER_ID" 这类自引用前缀赋值取的是外层值（bash 在赋值前完成展开）。
AUTH_FILE="$AUTH_FILE" ACTION="$ACTION" \
USER_ID="$USER_ID" ENT_ID="$ENT_ID" NICKNAME="$NICKNAME" \
TOKEN="$TOKEN" REFRESH="$REFRESH" EXPIRES_AT="$EXPIRES_AT" DOMAIN="$DOMAIN" \
python3 - <<'PYEOF'
import json, os

env = os.environ
path = env["AUTH_FILE"]
auth = {
    "account": {
        "uid": env["USER_ID"],
        "enterpriseId": env["ENT_ID"],
        "nickname": env["NICKNAME"],
    },
    "auth": {
        "accessToken": env["TOKEN"],
        "refreshToken": env["REFRESH"],
        "expiresAt": int(env["EXPIRES_AT"]),
        "domain": env["DOMAIN"],
    },
}
with open(path, "w") as f:
    json.dump(auth, f, indent=1)
os.chmod(path, 0o600)
print(f"已保存（{env['ACTION']}）: {path}")
PYEOF
echo "账号${ACTION}：uid=${USER_ID}（${NICKNAME:-未获取到昵称}）"

# ─── 签到（CN：POST codebuddy.cn/v2/billing/meter/daily-checkin，幂等不阻塞）───
USER_ID="$USER_ID" ENT_ID="$ENT_ID" DOMAIN="$DOMAIN" TOKEN="$TOKEN" \
python3 - <<'PYEOF'
import json, os, urllib.request, urllib.error

env = os.environ
headers = {
    "Authorization": "Bearer " + env["TOKEN"],
    "Accept": "application/json",
    "Content-Type": "application/json",
    "X-User-Id": env["USER_ID"],
}
if env["ENT_ID"]:
    headers["X-Enterprise-Id"] = env["ENT_ID"]
    headers["X-Tenant-Id"] = env["ENT_ID"]
if env["DOMAIN"]:
    headers["X-Domain"] = env["DOMAIN"]

req = urllib.request.Request(
    "https://www.codebuddy.cn/v2/billing/meter/daily-checkin",
    method="POST", data=b"{}", headers=headers)
try:
    with urllib.request.urlopen(req, timeout=15) as r:
        body = json.loads(r.read().decode() or "{}")
    if body.get("code") == 0:
        data = body.get("data") or {}
        print(f"签到: 成功 {json.dumps(data, ensure_ascii=False)[:150]}")
    else:
        print(f"签到: {body.get('msg', json.dumps(body)[:150])}")
except urllib.error.HTTPError as e:
    # 已签到等业务错误也走 4xx（实测 code=10001 "今天已签到"）
    try:
        body = json.loads(e.read().decode() or "{}")
        print(f"签到: {body.get('msg', 'http %d' % e.code)}")
    except Exception:
        print(f"签到: http {e.code}")
except Exception as e:
    print(f"签到: {e}")
PYEOF

# ─── 重启服务加载新账号 ───────────────────────────────────
# 注意：账号目录只在进程启动时扫描一次（main.go 的 LoadDir + SyncToDir），
# 运行期不会热加载，所以新增凭证后必须重启才能生效。
echo ""
LAUNCH_LABEL="com.hubo.workbuddy2api"
RESTARTED=""
if docker ps --format '{{.Names}}' 2>/dev/null | grep -q "^${CONTAINER}$"; then
    echo "重启 $CONTAINER 加载新账号..."
    docker restart "$CONTAINER" >/dev/null
    RESTARTED="docker"
elif launchctl print "gui/$(id -u)/${LAUNCH_LABEL}" >/dev/null 2>&1; then
    echo "重启 launchd 服务 ${LAUNCH_LABEL} 加载新账号..."
    "$(dirname "$0")/service.sh" restart >/dev/null 2>&1 \
        || launchctl kickstart -k "gui/$(id -u)/${LAUNCH_LABEL}" >/dev/null 2>&1 \
        || true
    RESTARTED="launchd"
fi

if [[ -z "$RESTARTED" ]]; then
    echo "未检测到运行中的服务（docker 容器与 launchd 服务都没有）。"
    echo "  auth 文件已保存，下次启动会自动加载；本机可执行 ./service.sh install 开启开机自启。"
else
    sleep 2
    # 查账号数：key 依次取 API_KEY 环境变量 → config.json 的 api_key；
    # 本机回环无鉴权模式下两者都可能为空，此时直接不带 Authorization 头请求。
    # 绝不内置默认值：上游遗留的 tistzach 就是反面教材（既无实际作用又误导读者）。
    KEY="${API_KEY:-}"
    if [[ -z "$KEY" && -f config.json ]]; then
        KEY=$(python3 -c "import json;print(json.load(open('config.json')).get('api_key') or '')" 2>/dev/null || true)
    fi
    if [[ -n "$KEY" ]]; then
        RESP=$(curl -s -m 5 --noproxy '*' -H "Authorization: Bearer ${KEY}" http://127.0.0.1:7863/status 2>/dev/null || true)
    else
        RESP=$(curl -s -m 5 --noproxy '*' http://127.0.0.1:7863/status 2>/dev/null || true)
    fi
    # 响应里没有 accounts 键（例如鉴权失败）时报 "?"，不要误报成 0 个账号。
    COUNT=$(printf '%s' "$RESP" | python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d['accounts']) if 'accounts' in d else '?')" 2>/dev/null || echo "?")
    echo "服务已重启，当前账号数: $COUNT"
fi

echo ""
# 时间戳转可读时间：`date -d` 是 GNU 扩展，macOS 的 BSD date 不支持（也没有 -d）。
# 脚本本就依赖 python3，用它做跨平台格式化。
EXPIRES_HUMAN=$(python3 -c "import datetime;print(datetime.datetime.fromtimestamp($EXPIRES_AT).strftime('%Y-%m-%d %H:%M'))" 2>/dev/null || echo "$EXPIRES_AT")
echo "============================================================"
echo "  登录完成！"
echo "  UID: ${USER_ID}"
echo "  Nickname: ${NICKNAME:-（未获取到）}"
echo "  Token: ${TOKEN:0:30}..."
echo "  有效期: $EXPIRES_HUMAN"
echo "============================================================"
echo ""
echo "登录下一个账号：先在浏览器【退出当前账号】（或用无痕窗口打开新链接），再跑一次 ./login.sh。"
echo "否则浏览器会直接复用当前登录态、重复授权同一个账号（uid 相同 → 覆盖同一份 auth 文件）。"
