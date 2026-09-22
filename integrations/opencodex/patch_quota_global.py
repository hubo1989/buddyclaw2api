#!/usr/bin/env python3
"""把 workbuddy-global 配额支持打进 opencodex 配额层（幂等，锚点定位）。

依赖：apply_global.sh（workbuddy-global provider）已应用。结构仿 patch_quota_autoclaw.py：
  门① explicitAccountReader + 门② explicitQuotaDestination → providers/quota/account-cache.ts
  （2.58.0 拆包后位置；文件缺失回退 quota.ts）
  switch case + report 适配器 + import → providers/quota.ts

env:
  OCX_QUOTA                quota.ts 路径
  OCX_QUOTA_ACCOUNT_CACHE  account-cache.ts 路径（不存在则回退 quota.ts）
"""
import os
import sys

QUOTA = os.environ["OCX_QUOTA"]
GATES = os.environ.get("OCX_QUOTA_ACCOUNT_CACHE") or QUOTA
if not os.path.isfile(GATES):
    GATES = QUOTA

_texts = {}
_dirty = set()


def want(path):
    if path not in _texts:
        _texts[path] = open(path, encoding="utf-8").read()
    return _texts[path]


def put(path, text):
    _texts[path] = text
    _dirty.add(path)


def flush():
    for p in sorted(_dirty):
        open(p, "w", encoding="utf-8").write(_texts[p])


def patch(path, anchor, replacement, label):
    text = want(path)
    if replacement.rstrip("\n") and replacement.rstrip("\n") in text:
        print(f"  已存在，跳过：{label}")
        return
    n = text.count(anchor)
    if n != 1:
        sys.exit(f"patch_quota_global: {label} 锚点匹配 {n} 次（期望 1）。opencodex 结构可能已变：{path}")
    put(path, text.replace(anchor, replacement, 1))
    print(f"  已修改 {label}")


# ── 1) 门①：explicitAccountReader 放行 workbuddy-global ──────────────
# 锚点：现形态已含 workbuddy + autoclaw 两行 overlay（apply.sh/apply_autoclaw.sh 打的）。
g = want(GATES)
if 'provider === "workbuddy-global"' not in g:
    anchor1 = '    || provider === "workbuddy"\n    || provider === "autoclaw";\n'
    if g.count(anchor1) == 1:
        put(GATES, g.replace(anchor1,
            '    || provider === "workbuddy"\n    || provider === "autoclaw"\n'
            '    || provider === "workbuddy-global";\n', 1))
        print("  已修改 门① explicitAccountReader（接在 autoclaw 行后）")
    else:
        # 回退锚点：autoclaw 补丁未打时的形态
        patch(GATES,
              '    || provider === "workbuddy";\n',
              '    || provider === "workbuddy"\n    || provider === "workbuddy-global";\n',
              "门① explicitAccountReader")
else:
    print("  已存在，跳过：门①")

# ── 2) 门②：explicitQuotaDestination 放行 ────────────────────────────
g = want(GATES)
if g.count('provider === "workbuddy-global"') < 2:
    anchor2 = '  return provider === "xai" || provider === "cursor" || provider === "workbuddy"\n'
    n2 = g.count(anchor2)
    if n2 == 1:
        # 其后可能已接 autoclaw 注释行——找 workbuddy 行行尾追加
        put(GATES, g.replace(anchor2,
            anchor2.rstrip("\n") + '\n    // workbuddy2api overlay: global 同 CN：固定计费域（workbuddy.ai）。\n'
            '    || provider === "workbuddy-global"\n', 1))
        print("  已修改 门② explicitQuotaDestination")
    else:
        sys.exit(f"patch_quota_global: 门② 锚点匹配 {n2} 次。检查 account-cache.ts 的 explicitQuotaDestination")
else:
    print("  已存在，跳过：门②")

# ── 3) switch case：quota.ts 的 readExplicitAccountQuota ─────────────
patch(QUOTA,
      '    case "workbuddy": result = await fetchWorkbuddyQuotaReport(provider, accessToken); break;\n',
      '    case "workbuddy": result = await fetchWorkbuddyQuotaReport(provider, accessToken); break;\n'
      '    case "workbuddy-global": result = await fetchWorkbuddyGlobalQuotaReport(provider, accessToken); break;\n',
      "switch case workbuddy-global")

# ── 4) report 适配器 ──────────────────────────────────────────────────
ADAPTER = '''
// workbuddy2api overlay: adapt the global fetcher to ProviderQuotaReport via report().
async function fetchWorkbuddyGlobalQuotaReport(provider: string, accessToken: string): Promise<ProviderQuotaReport | null> {
  const fetched = await fetchWorkbuddyGlobalQuotaRaw(provider, accessToken);
  if (!fetched) return null;
  return buildWorkbuddyQuotaReport(provider, fetched.source, fetched.quota);
}
'''
patch(QUOTA,
      'async function fetchExplicitCurrentQuota(',
      ADAPTER + '\nasync function fetchExplicitCurrentQuota(',
      "report 适配器")

# ── 5) import ─────────────────────────────────────────────────────────
patch(QUOTA,
      'import { fetchWorkbuddyQuota as fetchWorkbuddyQuotaRaw } from "./workbuddy-quota";\n',
      'import { fetchWorkbuddyQuota as fetchWorkbuddyQuotaRaw } from "./workbuddy-quota";\n'
      '// workbuddy2api overlay: quota fetcher for the built-in workbuddy-global provider.\n'
      'import { fetchWorkbuddyGlobalQuota as fetchWorkbuddyGlobalQuotaRaw } from "./workbuddy-global-quota";\n',
      "import workbuddy-global-quota")

flush()
print("patch_quota_global 完成")
