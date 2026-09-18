#!/usr/bin/env bash
# apply_global.sh — 给 opencodex 增加 workbuddy-global（CodeBuddy 国际版）OAuth provider。
# 同时打配额补丁（patch_quota_global.py）：门①②+case+适配器+import 五处，
# global 订阅制账号显示「订阅（累计消耗 N credits）」观测窗口。
#
# 与 apply.sh（workbuddy CN）同构：registry 条目 + oauth 模块 + OAUTH_PROVIDERS 注册 +
# 错误透传。CN 与 Global 是两个独立 provider、独立账号池，互不影响。
#
# 用法：
#   ./apply_global.sh            # 打补丁（幂等）
#   ./apply_global.sh --check    # 检查在位（含冒烟）
#   ./apply_global.sh --revert   # 从备份恢复
set -uo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKUP_ROOT="${HOME}/.opencodex-provider-workbuddy-backups"

die() { printf 'apply_global: %s\n' "$1" >&2; exit 1; }

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
  PKG_DIR="$(resolve_pkg_dir)" || die "找不到 opencodex 包目录。可用 OCX_PKG_DIR=<路径> 显式指定。"
fi

REGISTRY="${PKG_DIR}/src/providers/registry.ts"
OAUTH_INDEX="${PKG_DIR}/src/oauth/index.ts"
MODULE="${PKG_DIR}/src/oauth/workbuddy-global.ts"

for f in "${REGISTRY}" "${OAUTH_INDEX}"; do
  [ -f "${f}" ] || die "缺少文件：${f}"
done
[ -f "${SRC_DIR}/workbuddy-global-oauth.ts" ] || die "缺少 ${SRC_DIR}/workbuddy-global-oauth.ts"
[ -f "${SRC_DIR}/workbuddy-global-entry.json" ] || die "缺少 ${SRC_DIR}/workbuddy-global-entry.json"

MODE="${1:-apply}"

verify_patch() {
  local bun_bin="${OCX_BUN:-}"
  if [ -z "${bun_bin}" ]; then
    bun_bin="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("bunPath",""))' "${HOME}/.opencodex/service-state.json" 2>/dev/null)"
  fi
  if [ -z "${bun_bin}" ] || [ ! -x "${bun_bin}" ]; then
    printf '  跳过冒烟校验（找不到 bun）\n'
    return 0
  fi
  local probe="${TMPDIR:-/tmp}/ocx-wbg-verify.mjs"
  cat > "${probe}" <<'PROBE'
const pkg = process.env.OCX_PKG;
const oauth = await import(`${pkg}/src/oauth/index.ts`);
const reg = await import(`${pkg}/src/providers/registry.ts`);
const entry = reg.PROVIDER_REGISTRY.find(e => e.id === "workbuddy-global");
const ok = oauth.listOAuthProviders().includes("workbuddy-global") && entry?.authKind === "oauth";
console.log(JSON.stringify({
  ok,
  authKind: entry?.authKind ?? null,
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
  printf '\n  ⚠️ 冒烟校验失败：\n%s\n\n  可执行 ./apply_global.sh --revert 回滚。\n' "${out}"
  return 1
}

case "${MODE}" in
  --check)
    if grep -q 'id: "workbuddy-global"' "${REGISTRY}" && grep -q '^  "workbuddy-global": {' "${OAUTH_INDEX}"; then
      printf '已打补丁：%s\n' "${PKG_DIR}"
      verify_patch
      exit $?
    fi
    printf '未打补丁：%s\n' "${PKG_DIR}"
    exit 1
    ;;
  --revert)
    latest="$(ls -1dt "${BACKUP_ROOT}"/* 2>/dev/null | head -1)"
    [ -n "${latest}" ] || die "没有备份"
    for name in registry.ts oauth-index-global.ts; do
      [ -f "${latest}/${name}" ] || die "备份不完整：${latest}/${name}"
    done
    cp "${latest}/registry.ts" "${REGISTRY}"
    cp "${latest}/oauth-index-global.ts" "${OAUTH_INDEX}"
    rm -f "${MODULE}"
    printf '已从 %s 恢复\n' "${latest}"
    exit 0
    ;;
  apply) ;;
  *) die "未知参数 ${MODE}" ;;
esac

# 模块每次刷新（自研文件幂等）
cp "${SRC_DIR}/workbuddy-global-oauth.ts" "${MODULE}"
QUOTA_MODULE_G="${PKG_DIR}/src/providers/workbuddy-global-quota.ts"
QUOTA_TS="${PKG_DIR}/src/providers/quota.ts"
QUOTA_ACCOUNT_CACHE="${PKG_DIR}/src/providers/quota/account-cache.ts"
cp "${SRC_DIR}/workbuddy-global-quota.ts" "${QUOTA_MODULE_G}"

if grep -q 'id: "workbuddy-global"' "${REGISTRY}" && grep -q '^  "workbuddy-global": {' "${OAUTH_INDEX}"; then
  printf '已打过补丁（模块已刷新）：%s\n' "${PKG_DIR}"
  verify_patch
  exit $?
fi

STAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="${BACKUP_ROOT}/${STAMP}"
mkdir -p "${BACKUP_DIR}"
cp "${REGISTRY}" "${BACKUP_DIR}/registry.ts"
cp "${OAUTH_INDEX}" "${BACKUP_DIR}/oauth-index-global.ts"
printf '备份：%s\n' "${BACKUP_DIR}"

OCX_REGISTRY="${REGISTRY}" \
OCX_OAUTH_INDEX="${OAUTH_INDEX}" \
OCX_MODULE="${MODULE}" \
OCX_ENTRY_FILE="${SRC_DIR}/workbuddy-global-entry.json" \
OCX_SRC_MODULE="${SRC_DIR}/workbuddy-global-oauth.ts" \
python3 - <<'PYEOF'
import os, sys, json, re

registry_path = os.environ["OCX_REGISTRY"]
oauth_index_path = os.environ["OCX_OAUTH_INDEX"]
module_path = os.environ["OCX_MODULE"]

# registry 条目：从 JSON 生成 TS 字面量（保持与 apply.sh 注入的 workbuddy 条目同构）
entry_obj = json.load(open(os.environ["OCX_ENTRY_FILE"], encoding="utf-8"))

def ts_value(v):
    if isinstance(v, bool): return "true" if v else "false"
    if isinstance(v, (int, float)): return json.dumps(v)
    if isinstance(v, list): return "[" + ", ".join(ts_value(x) for x in v) + "]"
    if isinstance(v, dict):
        return "{\n" + ",\n".join(f'      {json.dumps(k)}: {ts_value(val)}' for k, val in v.items()) + ",\n    }"
    return json.dumps(v, ensure_ascii=False)

fields = []
for k, v in entry_obj.items():
    fields.append(f'    {k}: {ts_value(v)},')
entry = (
    "  // WorkBuddy (Global) — CodeBuddy 国际版（www.workbuddy.ai）OAuth provider。\n"
    "  // 与 workbuddy（CN）同协议不同域：独立 provider、独立账号池。chat 固定走 /v2\n"
    "  //（/console 挂腾讯云 WAF 内容规则，上游 issue #119 实测 403）。\n"
    + "\n".join(fields) + "\n"
)

PROVIDER_DEF = '''  "workbuddy-global": {
    login: (ctrl) => loginWorkbuddyGlobal(ctrl),
    refresh: (rt, signal) => refreshWorkbuddyGlobalToken(rt, signal),
    providerConfig: oauthConfig("workbuddy-global"),
    defaultModel: oauthDefaultModel("workbuddy-global"),
    // 与 CN 侧同理：上游轮换 refresh token，并发主动刷新会触发复用吊销。
    defaultRefreshPolicy: "lazy-only",
  },
'''
IMPORT_LINE = 'import { loginWorkbuddyGlobal, refreshWorkbuddyGlobalToken, WorkbuddyGlobalTokenError } from "./workbuddy-global";\n'
ERROR_MARKER = 'error instanceof WorkbuddyGlobalTokenError'


def patch_once(marker, path, anchor, replacement, label):
    text = open(path, encoding="utf-8").read()
    if marker in text:
        print(f"  已存在，跳过：{label}")
        return
    count = text.count(anchor)
    if count != 1:
        sys.exit(f"apply_global: {label} 锚点匹配 {count} 次（期望 1）。opencodex 结构可能已变：{path}")
    open(path, "w", encoding="utf-8").write(text.replace(anchor, replacement, 1))
    print(f"  已修改 {label}")


# 1) registry：PROVIDER_REGISTRY 结尾插入
patch_once(
    'id: "workbuddy-global"',
    registry_path,
    '\n];\n\nexport function providerRegistryFastWireError(',
    '\n  {\n' + entry + '  },\n];\n\nexport function providerRegistryFastWireError(',
    "registry.ts 条目",
)

# 2) import
patch_once(
    'from "./workbuddy-global";',
    oauth_index_path,
    'import { randomUUID } from "node:crypto";\n',
    'import { randomUUID } from "node:crypto";\n' + IMPORT_LINE,
    "oauth/index.ts import",
)

# 3) OAUTH_PROVIDERS 注册——锚点用 autoclaw 条目的结构化结尾。
# ⚠️ 别用 '\n};\n\nexport function isOAuthProvider('：opencodex 2.5x 在 OAUTH_PROVIDERS
# 之后加了 DEPRECATED_OAUTH_PROVIDER_ALIASES 表，旧锚点会命中**别名表**结尾把
# provider 定义插进 Record<string,string>（listOAuthProviders 不含它、面板不显示）。
ANCHOR_OAUTH_TAIL = (
    '    defaultRefreshPolicy: "lazy-only",\n'
    '  },\n'
    '};\n'
)
REPL_OAUTH_TAIL = (
    '    defaultRefreshPolicy: "lazy-only",\n'
    '  },\n'
    + PROVIDER_DEF
    + '};\n'
)
patch_once(
    '  "workbuddy-global": {',
    oauth_index_path,
    ANCHOR_OAUTH_TAIL,
    REPL_OAUTH_TAIL,
    "OAUTH_PROVIDERS 注册",
)

# 4) 错误透传（WorkbuddyGlobalTokenError → 原始 message）
patch_once(
    ERROR_MARKER,
    oauth_index_path,
    '  if (error instanceof WorkbuddyTokenError) return error.message;\n',
    '  if (error instanceof WorkbuddyTokenError) return error.message;\n'
    '  if (error instanceof WorkbuddyGlobalTokenError) return error.message;\n',
    "错误透传",
)

# 5) 模块落盘
src = open(os.environ["OCX_SRC_MODULE"], encoding="utf-8").read()
open(module_path, "w", encoding="utf-8").write(src)
print(f"  已写入 {os.path.relpath(module_path)}")
PYEOF

STATUS=$?
if [ "${STATUS}" -ne 0 ]; then
  printf '打补丁失败，回滚…\n'
  cp "${BACKUP_DIR}/registry.ts" "${REGISTRY}"
  cp "${BACKUP_DIR}/oauth-index-global.ts" "${OAUTH_INDEX}"
  rm -f "${MODULE}"
  die "已回滚"
fi

verify_patch || exit 1

# ── 配额支持（幂等；失败不回滚 provider 主功能——配额只是展示层）──
if [ -f "${QUOTA_TS}" ]; then
  OCX_QUOTA="${QUOTA_TS}" \
    $([ -f "${QUOTA_ACCOUNT_CACHE}" ] && printf 'OCX_QUOTA_ACCOUNT_CACHE=%s ' "${QUOTA_ACCOUNT_CACHE}") \
    python3 "${SRC_DIR}/patch_quota_global.py" || printf '⚠️ 配额补丁失败（不影响登录/对话）：见上方报错\n'
else
  printf '⚠️ 未找到 %s，跳过配额补丁\n' "${QUOTA_TS}"
fi

printf '\n完成：\n  ocx restart\n  ocx login workbuddy-global    # 浏览器授权 workbuddy.ai\n  ocx account list workbuddy-global\n'
