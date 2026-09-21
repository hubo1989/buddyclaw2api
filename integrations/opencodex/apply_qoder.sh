#!/usr/bin/env bash
# apply_qoder.sh — 给 opencodex 增加 qoder-global / qoder-cn-oauth 两个 OAuth provider。
#
# 与 apply_global.sh（workbuddy-global）同构：registry 条目 + oauth 模块 +
# OAUTH_PROVIDERS 注册 + 错误透传。两个 provider 共用 qoder-oauth.ts 模块、
# 独立账号池，指向本机 workbuddy2api 网关（7863）——COSY 签名/WAF 编码/SSE
# 解包都在网关侧完成，opencodex 侧只做设备流登录与 token 保管。
#
# 前置：网关已启用 qoder（config.json qoder.enabled=true）并重启。
#
# 用法：
#   ./apply_qoder.sh            # 打补丁（幂等）
#   ./apply_qoder.sh --check    # 检查在位（含冒烟）
#   ./apply_qoder.sh --revert   # 从备份恢复
set -uo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKUP_ROOT="${HOME}/.opencodex-provider-qoder-backups"

die() { printf 'apply_qoder: %s\n' "$1" >&2; exit 1; }

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
MODULE="${PKG_DIR}/src/oauth/qoder.ts"

for f in "${REGISTRY}" "${OAUTH_INDEX}"; do
  [ -f "${f}" ] || die "缺少文件：${f}"
done
[ -f "${SRC_DIR}/qoder-oauth.ts" ] || die "缺少 ${SRC_DIR}/qoder-oauth.ts"
[ -f "${SRC_DIR}/qoder-global-entry.json" ] || die "缺少 ${SRC_DIR}/qoder-global-entry.json"
[ -f "${SRC_DIR}/qoder-cn-entry.json" ] || die "缺少 ${SRC_DIR}/qoder-cn-entry.json"

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
  local probe="${TMPDIR:-/tmp}/ocx-qoder-verify.mjs"
  cat > "${probe}" <<'PROBE'
const pkg = process.env.OCX_PKG;
const oauth = await import(`${pkg}/src/oauth/index.ts`);
const reg = await import(`${pkg}/src/providers/registry.ts`);
const okG = oauth.listOAuthProviders().includes("qoder-global");
const okC = oauth.listOAuthProviders().includes("qoder-cn-oauth");
const eG = reg.PROVIDER_REGISTRY.find(e => e.id === "qoder-global");
const eC = reg.PROVIDER_REGISTRY.find(e => e.id === "qoder-cn-oauth");
console.log(JSON.stringify({ okG, okC, baseUrlG: eG?.baseUrl ?? null, baseUrlC: eC?.baseUrl ?? null }));
process.exit(okG && okC ? 0 : 1);
PROBE
  local out
  if out="$(OCX_PKG="${PKG_DIR}" "${bun_bin}" "${probe}" 2>&1)"; then
    rm -f "${probe}"
    printf '  冒烟校验通过：%s\n' "${out}"
    return 0
  fi
  rm -f "${probe}"
  printf '\n  ⚠️ 冒烟校验失败：\n%s\n\n  可执行 ./apply_qoder.sh --revert 回滚。\n' "${out}"
  return 1
}

case "${MODE}" in
  --check)
    if grep -Eq '"?id"?: "qoder-global"' "${REGISTRY}" && grep -q '^  "qoder-global": {' "${OAUTH_INDEX}"; then
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
    for name in registry.ts oauth-index-qoder.ts; do
      [ -f "${latest}/${name}" ] || die "备份不完整：${latest}/${name}"
    done
    cp "${latest}/registry.ts" "${REGISTRY}"
    cp "${latest}/oauth-index-qoder.ts" "${OAUTH_INDEX}"
    rm -f "${MODULE}"
    printf '已从 %s 恢复\n' "${latest}"
    exit 0
    ;;
  apply) ;;
  *) die "未知参数 ${MODE}" ;;
esac

# 模块每次刷新（自研文件幂等）
cp "${SRC_DIR}/qoder-oauth.ts" "${MODULE}"

# 登录持久化的 provider 行需携带 allowPrivateNetwork（缺了 config 校验会判死
# 回环/tailnet baseUrl）。幂等：字段已存在则跳过。
if ! grep -q '"allowPrivateNetwork",' "${OAUTH_INDEX}"; then
  OAUTH_INDEX_FILE="${OAUTH_INDEX}" python3 - <<'FIELDSEP'
import os
p = os.environ["OAUTH_INDEX_FILE"]
s = open(p, encoding="utf-8").read()
anchor = '  "keyOptional",\n] as const satisfies readonly (keyof OcxProviderConfig)[];'
assert s.count(anchor) == 1, "OAUTH field anchor mismatch: " + p
open(p, "w", encoding="utf-8").write(
    s.replace(anchor,
              '  "keyOptional",\n  "allowPrivateNetwork",\n] as const satisfies readonly (keyof OcxProviderConfig)[];', 1))
print("  已修改 OAUTH_LOGIN_OWNED_PROVIDER_FIELDS 字段表")
FIELDSEP
fi

if grep -Eq '"?id"?: "qoder-global"' "${REGISTRY}" && grep -q '^  "qoder-global": {' "${OAUTH_INDEX}"; then
  printf '已打过补丁（模块已刷新）：%s\n' "${PKG_DIR}"
  verify_patch
  exit $?
fi

STAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="${BACKUP_ROOT}/${STAMP}"
mkdir -p "${BACKUP_DIR}"
cp "${REGISTRY}" "${BACKUP_DIR}/registry.ts"
cp "${OAUTH_INDEX}" "${BACKUP_DIR}/oauth-index-qoder.ts"
printf '备份：%s\n' "${BACKUP_DIR}"

OCX_REGISTRY="${REGISTRY}" \
OCX_OAUTH_INDEX="${OAUTH_INDEX}" \
OCX_MODULE="${MODULE}" \
OCX_SRC_DIR="${SRC_DIR}" \
python3 - <<'PYEOF'
import os, sys, json, re

registry_path = os.environ["OCX_REGISTRY"]
oauth_index_path = os.environ["OCX_OAUTH_INDEX"]
module_path = os.environ["OCX_MODULE"]
src_dir = os.environ["OCX_SRC_DIR"]

def ts_value(v):
    if isinstance(v, bool): return "true" if v else "false"
    if isinstance(v, (int, float)): return json.dumps(v)
    if isinstance(v, list): return "[" + ", ".join(ts_value(x) for x in v) + "]"
    if isinstance(v, dict):
        return "{\n" + ",\n".join(f'      {json.dumps(k)}: {ts_value(val)}' for k, val in v.items()) + ",\n    }"
    return json.dumps(v, ensure_ascii=False)

def entry_block(entry_file, comment):
    obj = json.load(open(entry_file, encoding="utf-8"))
    fields = [f"    {json.dumps(k)}: {ts_value(v)}," for k, v in obj.items()]
    return "  // " + comment + "\n" + "\n".join(fields) + "\n"

PROVIDER_DEFS = (
'  "qoder-global": {\n'
'    login: (ctrl) => loginQoderGlobal(ctrl),\n'
'    refresh: (rt, signal) => refreshQoderToken(rt, signal),\n'
'    providerConfig: oauthConfig("qoder-global"),\n'
'    defaultModel: oauthDefaultModel("qoder-global"),\n'
'    // 设备流 token 上游 refresh 403（实测）——失效即提示重登。\n'
'    defaultRefreshPolicy: "lazy-only",\n'
'  },\n'
'  "qoder-cn-oauth": {\n'
'    login: (ctrl) => loginQoderCN(ctrl),\n'
'    refresh: (rt, signal) => refreshQoderToken(rt, signal),\n'
'    providerConfig: oauthConfig("qoder-cn-oauth"),\n'
'    defaultModel: oauthDefaultModel("qoder-cn-oauth"),\n'
'    defaultRefreshPolicy: "lazy-only",\n'
'  },\n'
)
IMPORT_LINE = 'import { loginQoderGlobal, loginQoderCN, refreshQoderToken, QoderTokenError } from "./qoder";\n'

def patch_once(marker, path, anchor, replacement, label, marker_re=None):
    text = open(path, encoding="utf-8").read()
    if (re.search(marker_re, text) if marker_re else marker in text):
        print(f"  已存在，跳过：{label}")
        return
    count = text.count(anchor)
    if count != 1:
        sys.exit(f"apply_qoder: {label} 锚点匹配 {count} 次（期望 1）。opencodex 结构可能已变：{path}")
    open(path, "w", encoding="utf-8").write(text.replace(anchor, replacement, 1))
    print(f"  已修改 {label}")

# 1) registry：两个条目插到 PROVIDER_REGISTRY 结尾
entry_g = entry_block(os.path.join(src_dir, "qoder-global-entry.json"),
                      "Qoder (Global) — qoder.com，登录/对话经本机网关 7863 代理（COSY 协议在网关侧）。")
entry_c = entry_block(os.path.join(src_dir, "qoder-cn-entry.json"),
                      "Qoder (CN) — qoder.cn 独立部署（阿里云，账号体系独立）；CN 直连推理端点未公开验证，网关侧 fail-fast。")
patch_once(
    'id: "qoder-global"',
    registry_path,
    "\n];\n\nexport function providerRegistryFastWireError(",
    "\n  {\n" + entry_g + "  },\n  {\n" + entry_c + "  },\n];\n\nexport function providerRegistryFastWireError(",
    "registry.ts 条目",
    marker_re=r'"?id"?:\s*"qoder-global"',
)

# 2) import
patch_once(
    'from "./qoder";',
    oauth_index_path,
    'import { randomUUID } from "node:crypto";\n',
    'import { randomUUID } from "node:crypto";\n' + IMPORT_LINE,
    "oauth/index.ts import",
)

# 3) OAUTH_PROVIDERS 注册——锚点用 workbuddy-global 条目的结构化结尾（与 apply_global.sh 同款避坑）。
ANCHOR_OAUTH_TAIL = (
'    defaultRefreshPolicy: "lazy-only",\n'
'  },\n'
'};\n'
)
REPL_OAUTH_TAIL = (
'    defaultRefreshPolicy: "lazy-only",\n'
'  },\n'
+ PROVIDER_DEFS
+ '};\n'
)
patch_once(
  '  "qoder-global": {',
  oauth_index_path,
  ANCHOR_OAUTH_TAIL,
  REPL_OAUTH_TAIL,
  "OAUTH_PROVIDERS 注册",
)

# 4) 错误透传（QoderTokenError → 原始 message）
patch_once(
    "error instanceof QoderTokenError",
    oauth_index_path,
    '  if (error instanceof WorkbuddyGlobalTokenError) return error.message;\n',
    '  if (error instanceof WorkbuddyGlobalTokenError) return error.message;\n'
    '  if (error instanceof QoderTokenError) return error.message;\n',
    "错误透传",
)

# 5) 模块落盘
src = open(os.path.join(src_dir, "qoder-oauth.ts"), encoding="utf-8").read()
open(module_path, "w", encoding="utf-8").write(src)
print(f"  已写入 {os.path.relpath(module_path)}")

# 6) 登录持久化的 provider 行需携带 allowPrivateNetwork，否则 config 校验会拒绝
# 回环/tailnet baseUrl（login 路径 upsert 行不带该标志 → 整份 config 判无效）。
patch_once(
    "allowPrivateNetwork",
    oauth_index_path,
    '  "keyOptional",\n] as const satisfies readonly (keyof OcxProviderConfig)[];',
    '  "keyOptional",\n  "allowPrivateNetwork",\n] as const satisfies readonly (keyof OcxProviderConfig)[];',
    "OAUTH_LOGIN_OWNED_PROVIDER_FIELDS 字段表",
    marker_re=r'"allowPrivateNetwork",',
)
PYEOF

STATUS=$?
if [ "${STATUS}" -ne 0 ]; then
  printf '打补丁失败，回滚…\n'
  cp "${BACKUP_DIR}/registry.ts" "${REGISTRY}"
  cp "${BACKUP_DIR}/oauth-index-qoder.ts" "${OAUTH_INDEX}"
  rm -f "${MODULE}"
  die "已回滚"
fi

verify_patch || exit 1

# 配额展示（幂等；失败不影响登录/对话）
QUOTA_TS="${PKG_DIR}/src/providers/quota.ts"
QUOTA_ACCOUNT_CACHE="${PKG_DIR}/src/providers/quota/account-cache.ts"
cp "${SRC_DIR}/qoder-quota.ts" "${PKG_DIR}/src/providers/qoder-quota.ts"
OCX_QUOTA="${QUOTA_TS}" \
  $([ -f "${QUOTA_ACCOUNT_CACHE}" ] && printf 'OCX_QUOTA_ACCOUNT_CACHE=%s ' "${QUOTA_ACCOUNT_CACHE}") \
  python3 "${SRC_DIR}/patch_quota_qoder.py" || printf 'quota patch failed (login/chat unaffected)\n'

printf '\n完成：\n  ocx restart\n  ocx login qoder-global    # 浏览器授权 qoder.com（经网关 7863）\n  ocx login qoder-cn-oauth        # 浏览器授权 qoder.cn\n  ocx account list qoder-global\n'
