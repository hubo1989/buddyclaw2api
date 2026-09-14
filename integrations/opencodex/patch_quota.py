#!/usr/bin/env python3
"""把 workbuddy 配额支持打进 opencodex 的 quota.ts（幂等，锚点定位）。"""
import os, sys

QUOTA = os.environ["OCX_QUOTA"]
SNIPPET = os.environ["OCX_QUOTA_SNIPPET"]

text = open(QUOTA, encoding="utf-8").read()
changed = False

# ── 1) 白名单：explicitAccountReader 加 workbuddy ─────────────────────
anchor = 'return provider === "xai" || provider === "cursor" || provider === "kimi" || provider === "command-code";'
if 'provider === "workbuddy"' not in text:
    if text.count(anchor) != 1:
        sys.exit(f"锚点 explicitAccountReader 匹配 {text.count(anchor)} 次")
    text = text.replace(
        anchor,
        'return provider === "xai" || provider === "cursor" || provider === "kimi" || provider === "command-code"\n    || provider === "workbuddy";',
        1,
    )
    changed = True
    print("  已修改 explicitAccountReader（配额白名单）")

# ── 1b) 第二道门：explicitQuotaDestination 也放行 workbuddy ───────────
# 注意它和 explicitAccountReader 是两个独立 gate，都要过；此行在 1) 之后应只剩这一处。
anchor_dest = '  return provider === "xai" || provider === "cursor";'
if text.count(anchor_dest) == 1 and 'workbuddy' not in text.split(anchor_dest)[0][-400:]:
    text = text.replace(
        anchor_dest,
        '  // workbuddy2api overlay: fixed canonical billing origin (codebuddy.cn), never config.baseUrl.\n'
        '  return provider === "xai" || provider === "cursor" || provider === "workbuddy";',
        1,
    )
    changed = True
    print("  已修改 explicitQuotaDestination（第二道门）")

# ── 2) fetcher 分发：switch 加 workbuddy case ─────────────────────────
anchor2 = '    case "command-code": result = await fetchCommandCodeQuota(provider, config, accessToken); break;'
if 'case "workbuddy"' not in text:
    if text.count(anchor2) != 1:
        sys.exit(f"锚点 readExplicitAccountQuota switch 匹配 {text.count(anchor2)} 次")
    text = text.replace(
        anchor2,
        anchor2 + '\n    case "workbuddy": result = await fetchWorkbuddyQuotaReport(provider, accessToken); break;',
        1,
    )
    changed = True
    print("  已修改 readExplicitAccountQuota（fetcher 分发）")

# ── 3) 顶部 import ────────────────────────────────────────────────────
imp_anchor = 'import { getAccountCredential, getAccountSet, getCredential } from "../oauth/store";'
imp_new = imp_anchor + '\n// workbuddy2api overlay: quota fetcher for the built-in workbuddy provider.\nimport { fetchWorkbuddyQuota as fetchWorkbuddyQuotaRaw } from "./workbuddy-quota";'
if 'workbuddy-quota' not in text:
    if text.count(imp_anchor) != 1:
        # import 布局随版本可能变化；退而求其次：插到文件首个 import 块之后
        first_import_end = text.index('\n\n', text.index('import '))
        text = text[:first_import_end] + '\n// workbuddy2api overlay: quota fetcher for the built-in workbuddy provider.\nimport { fetchWorkbuddyQuota as fetchWorkbuddyQuotaRaw } from "./workbuddy-quota";' + text[first_import_end:]
        changed = True
        print("  已插入 import（fallback 位置）")
    else:
        text = text.replace(imp_anchor, imp_new, 1)
        changed = True
        print("  已插入 import")

# ── 4) 适配函数：转成 report() 需要的形状（以整块存在性判断，防重复插入）──
adapter_anchor = 'async function fetchExplicitCurrentQuota('
adapter_code = '''// workbuddy2api overlay: adapt the raw fetcher to ProviderQuotaReport via report().
async function fetchWorkbuddyQuotaReport(provider: string, accessToken: string): Promise<ProviderQuotaReport | null> {
  const fetched = await fetchWorkbuddyQuotaRaw(provider, accessToken);
  if (!fetched) return null;
  return report(provider, fetched.source, fetched.quota);
}

'''
if adapter_code.strip() not in text:
    if text.count(adapter_anchor) != 1:
        sys.exit(f"锚点 fetchExplicitCurrentQuota 匹配 {text.count(adapter_anchor)} 次")
    text = text.replace(adapter_anchor, adapter_code + adapter_anchor, 1)
    changed = True
    print("  已插入 fetchWorkbuddyQuotaReport 适配函数")

if changed:
    open(QUOTA, "w", encoding="utf-8").write(text)
    print("  quota.ts 已写入")
else:
    print("  全部已存在，跳过")
