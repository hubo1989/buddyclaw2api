#!/usr/bin/env bash
# apply.sh — 给 opencodex（@bitkyc08/opencodex）加一个内置 provider = workbuddy。
#
# 为什么需要这个脚本：opencodex 的内置 provider 与 OAuth 后端是**源码内置**的：
#   - 内置 provider 列表        -> src/providers/registry.ts  (PROVIDER_REGISTRY)
#   - OAuth 登录/续期实现       -> src/oauth/<provider>.ts + src/oauth/index.ts (OAUTH_PROVIDERS)
# 注册进 OAUTH_PROVIDERS 之后，下面这些能力会自动获得（无需再改别处）：
#   - `ocx login workbuddy`         （login-cli.ts 用 listOAuthProviders() 驱动）
#   - 多账号账号池 + 429 自动切换    （oauth/generic-account-failover.ts，authMode==="oauth" 即生效）
#   - 后台 token 续期                （oauth/token-guardian.ts）
#   - 账号池管理 API                 （server/management/oauth-account-routes.ts）
#
# ⚠️ opencodex 通过 npm 自更新（`ocx update` → npm i -g @bitkyc08/opencodex@latest），
#    会整体替换 src/，所以**每次升级后都要重跑本脚本**。本脚本幂等：
#    已打过补丁会直接跳过，不会重复插入。
#
# 用法：
#   ./apply.sh            # 打补丁
#   ./apply.sh --check    # 检查补丁在位 + 冒烟校验（真正加载模块断言注册生效）
#   ./apply.sh --verify   # 只跑冒烟校验
#   ./apply.sh --revert    # 从最近一次备份恢复（撤销补丁）
#
# ── 日常使用 runbook（均已实测）────────────────────────────────────────
#   加账号   : ocx login workbuddy
#              · 浏览器已有 WorkBuddy 登录态时会自动 SSO 同意，几秒即完成
#              · 要加【另一个】账号：把打印出的 URL 复制到【无痕窗口】打开，
#                否则会重复授权当前账号
#   必须重启 : ocx restart
#              · 登录只 live-reload 了 provider 配置，**auth 账号池不进运行时**；
#                不重启就调用会得到极具误导性的 400：
#                "model is not supported when using Codex with a ChatGPT account"
#                （真实原因是被兜底路由到了 openai provider）
#   刷目录   : ocx sync
#              · Codex 的模型目录是 ~/.codex/cc-switch-model-catalog.json，由 opencodex 生成
#   查账号池 : ocx account list workbuddy      （面板：http://localhost:10100/）
#   升级之后 : ocx update  →  ./apply.sh  →  ocx restart
#              · ocx 自更新会整体替换 src/，这一步不能省
#   已知陷阱 : 思考模型（hy3 / glm-5.3 等）的 max_tokens 别给小。
#              hy3 在 max_tokens=24 时输出会被思考 token 吃光（reasoning_tokens=60），
#              客户端只看到空文本；≥256 才稳定出文本。
set -uo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKUP_ROOT="${HOME}/.opencodex-provider-workbuddy-backups"

die() { printf 'apply: %s\n' "$1" >&2; exit 1; }

# ── 定位 opencodex 包目录 ─────────────────────────────────────────────
# 优先用运行态记录的 cliPath（与实际正在跑的安装一致），其次 npm root -g。
resolve_pkg_dir() {
  local state="${HOME}/.opencodex/service-state.json"
  if [ -f "${state}" ]; then
    local cli_path
    cli_path="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("cliPath",""))' "${state}" 2>/dev/null)"
    if [ -n "${cli_path}" ]; then
      # <pkg>/src/cli/index.ts -> <pkg>
      local d
      d="$(cd "$(dirname "${cli_path}")/../.." 2>/dev/null && pwd)"
      if [ -n "${d}" ] && [ -d "${d}/src/providers" ]; then printf '%s' "${d}"; return 0; fi
    fi
  fi
  local root
  root="$(npm root -g 2>/dev/null)"
  if [ -n "${root}" ] && [ -d "${root}/@bitkyc08/opencodex/src/providers" ]; then
    printf '%s' "${root}/@bitkyc08/opencodex"; return 0
  fi
  return 1
}

PKG_DIR="${OCX_PKG_DIR:-}"
if [ -z "${PKG_DIR}" ]; then
  PKG_DIR="$(resolve_pkg_dir)" || die "找不到 opencodex 包目录。可用 OCX_PKG_DIR=<路径> 显式指定。"
fi

REGISTRY="${PKG_DIR}/src/providers/registry.ts"
OAUTH_INDEX="${PKG_DIR}/src/oauth/index.ts"
OAUTH_MODULE="${PKG_DIR}/src/oauth/workbuddy.ts"
QUOTA_TS="${PKG_DIR}/src/providers/quota.ts"
QUOTA_MODULE="${PKG_DIR}/src/providers/workbuddy-quota.ts"

for f in "${REGISTRY}" "${OAUTH_INDEX}"; do
  [ -f "${f}" ] || die "缺少文件：${f}（opencodex 版本结构可能已变）"
done
[ -f "${SRC_DIR}/workbuddy-oauth.ts" ] || die "缺少补丁源文件：${SRC_DIR}/workbuddy-oauth.ts"
[ -f "${SRC_DIR}/registry-entry.ts" ] || die "缺少补丁源文件：${SRC_DIR}/registry-entry.ts"

# ── 冒烟校验：真正加载模块，断言注册生效 ─────────────────────────────
# 只检查「文本插入成功」不够——锚点可能匹配上但插入内容语法有误，那时补丁在位却会让代理起不来。
# 这里用包自带的 bun 直接 import 两个模块：既验证 TS 语法，也验证注册真的可见。
verify_patch() {
  local bun_bin="${OCX_BUN:-}"
  if [ -z "${bun_bin}" ]; then
    bun_bin="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("bunPath",""))' "${HOME}/.opencodex/service-state.json" 2>/dev/null)"
  fi
  if [ -z "${bun_bin}" ] || [ ! -x "${bun_bin}" ]; then
    printf '  跳过冒烟校验（找不到 bun；可用 OCX_BUN=<路径> 指定）\n'
    return 0
  fi

  local probe="${TMPDIR:-/tmp}/ocx-workbuddy-verify.mjs"
  cat > "${probe}" <<'PROBE'
const pkg = process.env.OCX_PKG;
const oauth = await import(`${pkg}/src/oauth/index.ts`);
const reg = await import(`${pkg}/src/providers/registry.ts`);
const entry = reg.PROVIDER_REGISTRY.find(e => e.id === "workbuddy");
const ok = oauth.listOAuthProviders().includes("workbuddy") && entry?.authKind === "oauth";
console.log(JSON.stringify({
  ok,
  oauthRegistered: oauth.listOAuthProviders().includes("workbuddy"),
  authKind: entry?.authKind ?? null,
  adapter: entry?.adapter ?? null,
  baseUrl: entry?.baseUrl ?? null,
  modelCount: entry?.models?.length ?? 0,
}));
process.exit(ok ? 0 : 1);
PROBE

  local out
  if out="$(OCX_PKG="${PKG_DIR}" "${bun_bin}" "${probe}" 2>&1)"; then
    rm -f "${probe}"
    printf '  冒烟校验通过：%s\n' "${out}"
    return 0
  fi
  rm -f "${probe}"
  printf '\n  ⚠️ 冒烟校验失败——补丁在位但模块加载/注册异常：\n%s\n\n' "${out}"
  printf '  opencodex 可能无法启动。请执行 ./apply.sh --revert 回滚后排查。\n'
  return 1
}

MODE="${1:-apply}"

case "${MODE}" in
  --check)
    if grep -q 'id: "workbuddy"' "${REGISTRY}" \
       && grep -q '^  workbuddy: {' "${OAUTH_INDEX}" \
       && grep -q 'error instanceof WorkbuddyTokenError' "${OAUTH_INDEX}"; then
      printf '已打补丁：%s\n' "${PKG_DIR}"
      # 文本在位 ≠ 可用：再确认模块能加载、注册真的生效（升级后结构漂移靠这步兜住）。
      verify_patch
      exit $?
    fi
    printf '未打补丁：%s\n' "${PKG_DIR}"
    exit 1
    ;;
  --revert)
    latest="$(ls -1dt "${BACKUP_ROOT}"/* 2>/dev/null | head -1)"
    [ -n "${latest}" ] || die "没有可用备份（${BACKUP_ROOT}）"
    for name in registry.ts oauth-index.ts quota.ts; do
      [ -f "${latest}/${name}" ] || die "备份不完整：${latest}/${name}"
    done
    cp "${latest}/registry.ts" "${REGISTRY}"
    cp "${latest}/oauth-index.ts" "${OAUTH_INDEX}"
    cp "${latest}/quota.ts" "${QUOTA_TS}"
    rm -f "${OAUTH_MODULE}" "${QUOTA_MODULE}"
    printf '已从 %s 恢复（并删除 workbuddy.ts / workbuddy-quota.ts）\n' "${latest}"
    exit 0
    ;;
  apply) ;;
  --verify)
    verify_patch
    exit $?
    ;;
  *) die "未知参数：${MODE}（支持 --check / --revert）" ;;
esac

# 模块文件（workbuddy.ts / workbuddy-quota.ts）是**我们自己的**，不是 vendor 代码，且落盘幂等 ——
# 每次都刷新，这样修 bug 后只要重跑 apply.sh 就能生效，不必先 --revert。
cp "${SRC_DIR}/workbuddy-oauth.ts" "${OAUTH_MODULE}"
cp "${SRC_DIR}/workbuddy-quota.ts" "${QUOTA_MODULE}"

if grep -q 'id: "workbuddy"' "${REGISTRY}" \
   && grep -q '  workbuddy: {' "${OAUTH_INDEX}" \
   && grep -q 'error instanceof WorkbuddyTokenError' "${OAUTH_INDEX}" \
   && grep -q 'provider === "workbuddy"' "${QUOTA_TS}"; then
  printf '已打过补丁（模块已刷新）。%s\n' "${PKG_DIR}"
  verify_patch
  exit $?
fi

# ── 备份 ──────────────────────────────────────────────────────────────
STAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="${BACKUP_ROOT}/${STAMP}"
mkdir -p "${BACKUP_DIR}"
cp "${REGISTRY}" "${BACKUP_DIR}/registry.ts"
cp "${OAUTH_INDEX}" "${BACKUP_DIR}/oauth-index.ts"
cp "${QUOTA_TS}" "${BACKUP_DIR}/quota.ts"
printf '备份：%s\n' "${BACKUP_DIR}"

# ── 打补丁（字符串手术交给 python3，避开 BSD sed 的坑） ───────────────
OCX_REGISTRY="${REGISTRY}" \
OCX_OAUTH_INDEX="${OAUTH_INDEX}" \
OCX_OAUTH_MODULE="${OAUTH_MODULE}" \
OCX_ENTRY_FILE="${SRC_DIR}/registry-entry.ts" \
OCX_SRC_MODULE="${SRC_DIR}/workbuddy-oauth.ts" \
python3 - <<'PYEOF'
import os, sys

registry_path = os.environ["OCX_REGISTRY"]
oauth_index_path = os.environ["OCX_OAUTH_INDEX"]
module_path = os.environ["OCX_OAUTH_MODULE"]
entry = open(os.environ["OCX_ENTRY_FILE"], encoding="utf-8").read()
module_src = open(os.environ["OCX_SRC_MODULE"], encoding="utf-8").read()

if not entry.endswith("\n"):
    entry += "\n"

PROVIDER_DEF = '''  workbuddy: {
    login: (ctrl) => loginWorkbuddy(ctrl),
    refresh: (rt, signal) => refreshWorkbuddyToken(rt, signal),
    providerConfig: oauthConfig("workbuddy"),
    defaultModel: oauthDefaultModel("workbuddy"),
    // The refresh endpoint ROTATES the refresh token on every call, so concurrent proactive
    // refreshes could trip reuse revocation. Stay lazy-only, like the other rotating providers.
    defaultRefreshPolicy: "lazy-only",
  },
'''

IMPORT_LINE = 'import { loginWorkbuddy, refreshWorkbuddyToken, WorkbuddyTokenError } from "./workbuddy";\n'
# 上一版补丁插的 import（少了错误类）。升级路径要把它替换掉，而不是重复插一行。
LEGACY_IMPORT_LINE = 'import { loginWorkbuddy, refreshWorkbuddyToken } from "./workbuddy";\n'

# 错误透传：publicOAuthAuthenticationErrorMessage 只放行它认识的那几类错误，
# 其余一律压成通用文案。我们自己的 WorkbuddyTokenError 不认识 → 登录/续期的真实原因被吞掉，
# 实测排查时极难定位（面板只显示 "OAuth authentication failed…"）。这里把它的 message 放行。
ERROR_PASSTHROUGH_ANCHOR = '\n  return "OAuth authentication failed. Check the OpenCodex account status and retry.";'
ERROR_PASSTHROUGH_MARKER = 'error instanceof WorkbuddyTokenError'


def patch_once(marker, path, anchor, replacement, label):
    text = open(path, encoding="utf-8").read()
    if marker in text:
        print(f"  已存在，跳过：{label}")
        return
    count = text.count(anchor)
    if count != 1:
        sys.exit(f"apply: {label} 锚点匹配 {count} 次（期望 1）。opencodex 结构可能已变，请检查 {path}")
    open(path, "w", encoding="utf-8").write(text.replace(anchor, replacement, 1))
    print(f"  已修改 {label}")


# 1) registry.ts: 在 PROVIDER_REGISTRY 数组结尾前插入条目
patch_once(
    'id: "workbuddy"',
    registry_path,
    '\n];\n\nexport function providerRegistryFastWireError(',
    '\n' + entry + '];\n\nexport function providerRegistryFastWireError(',
    "src/providers/registry.ts (内置 provider 条目)",
)

# 2) oauth/index.ts: 补 import
patch_once(
    'from "./workbuddy";',
    oauth_index_path,
    'import { randomUUID } from "node:crypto";\n',
    'import { randomUUID } from "node:crypto";\n' + IMPORT_LINE,
    "src/oauth/index.ts (import)",
)

# 2b) 升级路径：把旧 import 换成带错误类的新 import
_index_text = open(oauth_index_path, encoding="utf-8").read()
if ERROR_PASSTHROUGH_MARKER not in _index_text and LEGACY_IMPORT_LINE in _index_text:
    open(oauth_index_path, "w", encoding="utf-8").write(
        _index_text.replace(LEGACY_IMPORT_LINE, IMPORT_LINE, 1)
    )
    print("  已修改 src/oauth/index.ts (import 升级：补上 WorkbuddyTokenError)")

# 3) oauth/index.ts: 注册 OAuth 后端
patch_once(
    '  workbuddy: {',
    oauth_index_path,
    '\n};\n\nexport function isOAuthProvider(',
    '\n' + PROVIDER_DEF + '};\n\nexport function isOAuthProvider(',
    "src/oauth/index.ts (OAUTH_PROVIDERS)",
)

# 4) oauth/index.ts: 让 WorkbuddyTokenError 的真实 message 透传到 UI
patch_once(
    ERROR_PASSTHROUGH_MARKER,
    oauth_index_path,
    ERROR_PASSTHROUGH_ANCHOR,
    "\n  // WorkBuddy: surface the provider's own diagnostic instead of the generic fallback, so a\n"
    "  // failed login/refresh can be told apart from \"not logged in\".\n"
    "  if (error instanceof WorkbuddyTokenError) return error.message;\n"
    + ERROR_PASSTHROUGH_ANCHOR,
    "src/oauth/index.ts (WorkbuddyTokenError 透传)",
)

# 5) 落 OAuth 模块
open(module_path, "w", encoding="utf-8").write(module_src)
print(f"  已写入 {os.path.relpath(module_path)}")

PYEOF

STATUS=$?
if [ "${STATUS}" -ne 0 ]; then
  printf '打补丁失败，正在回滚…\n'
  cp "${BACKUP_DIR}/registry.ts" "${REGISTRY}"
  cp "${BACKUP_DIR}/oauth-index.ts" "${OAUTH_INDEX}"
  rm -f "${OAUTH_MODULE}"
  die "已回滚到补丁前状态"
fi

# ── 配额支持（quota.ts 4 处改动 + 独立模块）───────────────────────────
# 幂等；锚点失配会报错但不回滚 OAuth 部分（quota 只是展示层增强）。
OCX_QUOTA="${QUOTA_TS}" OCX_QUOTA_SNIPPET="${SRC_DIR}/workbuddy-quota.ts" \
  python3 "${SRC_DIR}/patch_quota.py" || printf '⚠️ 配额补丁失败（不影响 OAuth 主功能）：见上方报错\n'

verify_patch || exit 1

printf '\n完成。下一步：\n'
printf '  1) ./apply.sh --check            # 确认补丁在位（含冒烟校验）\n'
printf '  2) ocx restart                   # 让内置 provider 生效\n'
printf '  3) ocx login workbuddy           # 浏览器授权；重复执行可加更多账号\n'
printf '  4) ocx account list              # 查看账号池\n'
