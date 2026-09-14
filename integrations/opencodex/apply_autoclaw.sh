#!/usr/bin/env bash
# apply_autoclaw.sh — 给 opencodex 加第二个内置 provider = autoclaw（智谱澳龙）。
#
# 与 apply.sh（workbuddy）同构，额外多一处 adapter 补丁：
#   AutoClaw 的 chat 端点只认 `X-Authorization: Bearer <token>`（标准 Authorization 实测 401），
#   而 openai-chat adapter 硬编码标准头。因此：
#     1) types/provider.ts     加可选字段 authHeaderName
#     2) adapters/openai-chat  有 authHeaderName 时用它替代 Authorization
#     3) registry 条目声明 authHeaderName: "X-Authorization"
#
# 补丁点（在 apply.sh 的 5 处之外新增 3 处）：
#   A. src/providers/registry.ts        autoclaw 条目
#   B. src/oauth/index.ts               import + OAUTH_PROVIDERS 注册 + 错误透传
#   C. src/oauth/autoclaw.ts            登录/续期模块（本目录 autoclaw-oauth.ts）
#   D. src/types/provider.ts            authHeaderName 字段
#   E. src/adapters/openai-chat.ts      authHeaderName 消费
#
# ⚠️ ocx update 会整体替换 src/ → 升级后需重跑本脚本（幂等）。
#
# 用法：./apply_autoclaw.sh [--check|--revert]
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKUP_ROOT="${HOME}/.opencodex-provider-autoclaw-backups"

die() { printf 'apply_autoclaw: %s\n' "$1" >&2; exit 1; }

resolve_pkg_dir() {
  local state="${HOME}/.opencodex/service-state.json"
  if [ -f "${state}" ]; then
    local cli_path
    cli_path="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("cliPath",""))' "${state}" 2>/dev/null)"
    if [ -n "${cli_path}" ]; then
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
  PKG_DIR="$(resolve_pkg_dir)" || die "找不到 opencodex 包目录（可用 OCX_PKG_DIR= 指定）"
fi

REGISTRY="${PKG_DIR}/src/providers/registry.ts"
OAUTH_INDEX="${PKG_DIR}/src/oauth/index.ts"
OAUTH_MODULE="${PKG_DIR}/src/oauth/autoclaw.ts"
QUOTA_TS="${PKG_DIR}/src/providers/quota.ts"
QUOTA_MODULE="${PKG_DIR}/src/providers/autoclaw-quota.ts"
TYPES_PROVIDER="${PKG_DIR}/src/types/provider.ts"
ADAPTER_OPENAI_CHAT="${PKG_DIR}/src/adapters/openai-chat.ts"

for f in "${REGISTRY}" "${OAUTH_INDEX}" "${TYPES_PROVIDER}" "${ADAPTER_OPENAI_CHAT}"; do
  [ -f "${f}" ] || die "缺少文件：${f}（opencodex 结构可能已变）"
done
[ -f "${SCRIPT_DIR}/autoclaw-oauth.ts" ] || die "缺少补丁源文件：autoclaw-oauth.ts"
[ -f "${SCRIPT_DIR}/autoclaw-registry-entry.ts" ] || die "缺少补丁源文件：autoclaw-registry-entry.ts"
[ -f "${SCRIPT_DIR}/autoclaw-quota.ts" ] || die "缺少补丁源文件：autoclaw-quota.ts"

# 先确保 workbuddy 补丁在位（共享 verify/patch 基础设施与备份语义）
if ! grep -q 'id: "workbuddy"' "${REGISTRY}"; then
  die "workbuddy 补丁不在位——先跑 ./apply.sh 再跑本脚本"
fi

verify_patch() {
  local bun_bin="${OCX_BUN:-}"
  if [ -z "${bun_bin}" ]; then
    bun_bin="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("bunPath",""))' "${HOME}/.opencodex/service-state.json" 2>/dev/null)"
  fi
  if [ -z "${bun_bin}" ] || [ ! -x "${bun_bin}" ]; then
    printf '  跳过冒烟校验（找不到 bun）\n'; return 0
  fi
  local probe="${TMPDIR:-/tmp}/ocx-autoclaw-verify.mjs"
  cat > "${probe}" <<'PROBE'
const pkg = process.env.OCX_PKG;
const oauth = await import(`${pkg}/src/oauth/index.ts`);
const reg = await import(`${pkg}/src/providers/registry.ts`);
const entry = reg.PROVIDER_REGISTRY.find(e => e.id === "autoclaw");
const ok = oauth.listOAuthProviders().includes("autoclaw") && entry?.authKind === "oauth"
  && entry?.authHeaderName === "X-Authorization";
console.log(JSON.stringify({
  ok,
  oauthRegistered: oauth.listOAuthProviders().includes("autoclaw"),
  authKind: entry?.authKind ?? null,
  authHeaderName: entry?.authHeaderName ?? null,
  baseUrl: entry?.baseUrl ?? null,
  modelCount: entry?.models?.length ?? 0,
}));
process.exit(ok ? 0 : 1);
PROBE
  local out
  if out="$(OCX_PKG="${PKG_DIR}" "${bun_bin}" "${probe}" 2>&1)"; then
    rm -f "${probe}"; printf '  冒烟校验通过：%s\n' "${out}"; return 0
  fi
  rm -f "${probe}"
  printf '\n  ⚠️ 冒烟校验失败：\n%s\n\n  请 ./apply_autoclaw.sh --revert 回滚后排查。\n' "${out}"
  return 1
}

MODE="${1:-apply}"
case "${MODE}" in
  --check)
    if grep -q 'id: "autoclaw"' "${REGISTRY}" \
       && grep -q '^  autoclaw: {' "${OAUTH_INDEX}" \
       && grep -q 'authHeaderName' "${TYPES_PROVIDER}" \
       && grep -q 'authHeaderName' "${ADAPTER_OPENAI_CHAT}"; then
      if grep -q 'case "autoclaw"' "${QUOTA_TS}" 2>/dev/null && [ -f "${QUOTA_MODULE}" ]; then
        printf '已打补丁（含配额）：%s\n' "${PKG_DIR}"
      else
        printf '⚠️ OAuth 补丁在位但配额补丁缺失（重跑 apply 即可）\n'
      fi
      verify_patch; exit $?
    fi
    printf '未打补丁：%s\n' "${PKG_DIR}"; exit 1
    ;;
  --revert)
    latest="$(ls -1dt "${BACKUP_ROOT}"/* 2>/dev/null | head -1)"
    [ -n "${latest}" ] || die "没有可用备份（${BACKUP_ROOT}）"
    for name in registry.ts oauth-index.ts types-provider.ts openai-chat.ts; do
      [ -f "${latest}/${name}" ] || die "备份不完整：${latest}/${name}"
    done
    cp "${latest}/registry.ts" "${REGISTRY}"
    cp "${latest}/oauth-index.ts" "${OAUTH_INDEX}"
    cp "${latest}/types-provider.ts" "${TYPES_PROVIDER}"
    cp "${latest}/openai-chat.ts" "${ADAPTER_OPENAI_CHAT}"
    rm -f "${OAUTH_MODULE}"
    rm -f "${QUOTA_MODULE}"
    if [ -f "${QUOTA_TS}" ]; then
      OCX_QUOTA="${QUOTA_TS}" python3 "${SCRIPT_DIR}/patch_quota_autoclaw.py" --revert || true
    fi
    printf '已从 %s 恢复（并移除 autoclaw 配额模块）\n' "${latest}"
    exit 0
    ;;
  apply) ;;
  *) die "未知参数：${MODE}" ;;
esac

cp "${SCRIPT_DIR}/autoclaw-oauth.ts" "${OAUTH_MODULE}"
cp "${SCRIPT_DIR}/autoclaw-quota.ts" "${QUOTA_MODULE}"

# 配额支持（quota.ts 5 处 + 独立模块）；幂等。失败只警告——登录与对话不受影响。
if [ -f "${QUOTA_TS}" ]; then
  OCX_QUOTA="${QUOTA_TS}" python3 "${SCRIPT_DIR}/patch_quota_autoclaw.py" \
    || printf '⚠️ 配额补丁失败（不影响登录与对话）：见上方报错\n'
fi

if grep -q 'id: "autoclaw"' "${REGISTRY}" \
   && grep -q '  autoclaw: {' "${OAUTH_INDEX}" \
   && grep -q 'authHeaderName' "${TYPES_PROVIDER}" \
   && grep -q 'authHeaderName' "${ADAPTER_OPENAI_CHAT}"; then
  printf '已打过补丁（模块已刷新）。%s\n' "${PKG_DIR}"
  verify_patch; exit $?
fi

STAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="${BACKUP_ROOT}/${STAMP}"
mkdir -p "${BACKUP_DIR}"
cp "${REGISTRY}" "${BACKUP_DIR}/registry.ts"
cp "${OAUTH_INDEX}" "${BACKUP_DIR}/oauth-index.ts"
cp "${TYPES_PROVIDER}" "${BACKUP_DIR}/types-provider.ts"
cp "${ADAPTER_OPENAI_CHAT}" "${BACKUP_DIR}/openai-chat.ts"
printf '备份：%s\n' "${BACKUP_DIR}"

OCX_REGISTRY="${REGISTRY}" \
OCX_OAUTH_INDEX="${OAUTH_INDEX}" \
OCX_TYPES="${TYPES_PROVIDER}" \
OCX_ADAPTER="${ADAPTER_OPENAI_CHAT}" \
OCX_ENTRY_FILE="${SCRIPT_DIR}/autoclaw-registry-entry.ts" \
python3 - <<'PYEOF'
import os, sys

registry_path = os.environ["OCX_REGISTRY"]
oauth_index_path = os.environ["OCX_OAUTH_INDEX"]
types_path = os.environ["OCX_TYPES"]
adapter_path = os.environ["OCX_ADAPTER"]
entry = open(os.environ["OCX_ENTRY_FILE"], encoding="utf-8").read()
if not entry.endswith("\n"):
    entry += "\n"


def patch_once(marker, path, anchor, replacement, label):
    text = open(path, encoding="utf-8").read()
    if marker in text:
        print(f"  已存在，跳过：{label}")
        return
    count = text.count(anchor)
    if count != 1:
        sys.exit(f"apply_autoclaw: {label} 锚点匹配 {count} 次（期望 1）。结构可能已变：{path}")
    open(path, "w", encoding="utf-8").write(text.replace(anchor, replacement, 1))
    print(f"  已修改 {label}")


# A) registry：autoclaw 条目（插在 workbuddy 条目之后——数组结尾锚点已被 workbuddy 补丁占用过一次，
#    但那个补丁把条目插在 `];` 前，所以这里的锚点仍是数组结尾）
patch_once(
    'id: "autoclaw"',
    registry_path,
    '\n];\n\nexport function providerRegistryFastWireError(',
    '\n' + entry + '];\n\nexport function providerRegistryFastWireError(',
    "registry.ts (autoclaw 条目)",
)

# B) oauth/index.ts：import + 注册 + 错误透传
patch_once(
    'from "./autoclaw";',
    oauth_index_path,
    'import { randomUUID } from "node:crypto";\n',
    'import { randomUUID } from "node:crypto";\n'
    'import { loginAutoclaw, refreshAutoclawToken, AutoclawTokenError } from "./autoclaw";\n',
    "oauth/index.ts (import)",
)

PROVIDER_DEF = '''  autoclaw: {
    login: (ctrl) => loginAutoclaw(ctrl),
    refresh: (rt, signal) => refreshAutoclawToken(rt, signal),
    providerConfig: oauthConfig("autoclaw"),
    defaultModel: oauthDefaultModel("autoclaw"),
    // AutoClaw rotates refresh tokens on every call (same as workbuddy) → lazy-only.
    defaultRefreshPolicy: "lazy-only",
  },
'''
patch_once(
    '  autoclaw: {',
    oauth_index_path,
    '\n};\n\nexport function isOAuthProvider(',
    '\n' + PROVIDER_DEF + '};\n\nexport function isOAuthProvider(',
    "oauth/index.ts (OAUTH_PROVIDERS)",
)

ANCHOR = '\n  return "OAuth authentication failed. Check the OpenCodex account status and retry.";'
patch_once(
    'error instanceof AutoclawTokenError',
    oauth_index_path,
    ANCHOR,
    "\n  // AutoClaw: surface real diagnostics (same rationale as the WorkBuddy passthrough).\n"
    "  if (error instanceof AutoclawTokenError) return error.message;\n" + ANCHOR,
    "oauth/index.ts (AutoclawTokenError 透传)",
)

# D) types/provider.ts：authHeaderName 可选字段
patch_once(
    'authHeaderName?:',
    types_path,
    '  apiKeyTransport?: "x-api-key" | "bearer";',
    '  apiKeyTransport?: "x-api-key" | "bearer";\n'
    '  /** Custom auth header name (openai-chat): replaces `Authorization` when set (e.g. X-Authorization for AutoClaw). */\n'
    '  authHeaderName?: string;',
    "types/provider.ts (authHeaderName 字段)",
)

# E) adapters/openai-chat.ts：消费 authHeaderName
patch_once(
    'authHeaderName',
    adapter_path,
    '  if (hasCredential) headers.Authorization = `Bearer ${provider.apiKey}`;',
    '  if (hasCredential) {\n'
    '    const authHeader = (provider as { authHeaderName?: string }).authHeaderName || "Authorization";\n'
    '    headers[authHeader] = `Bearer ${provider.apiKey}`;\n'
    '  }',
    "adapters/openai-chat.ts (authHeaderName 消费)",
)

PYEOF

STATUS=$?
if [ "${STATUS}" -ne 0 ]; then
  printf '打补丁失败，回滚…\n'
  cp "${BACKUP_DIR}/registry.ts" "${REGISTRY}"
  cp "${BACKUP_DIR}/oauth-index.ts" "${OAUTH_INDEX}"
  cp "${BACKUP_DIR}/types-provider.ts" "${TYPES_PROVIDER}"
  cp "${BACKUP_DIR}/openai-chat.ts" "${ADAPTER_OPENAI_CHAT}"
  rm -f "${OAUTH_MODULE}"
  die "已回滚"
fi

verify_patch || exit 1

printf '\n完成。下一步：\n'
printf '  1) ocx restart                 # 让内置 provider 生效\n'
printf '  2) ocx login autoclaw          # 交互选择：手机验证码 / Google / Z.ai\n'
printf '  3) 面板 → 提供方 → AutoClaw → 账户（与 workbuddy 同款账户管理）\n'
printf '  4) ocx account list autoclaw   # 查看账号池\n'
