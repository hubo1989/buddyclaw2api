#!/usr/bin/env python3
"""把 qoder-global 配额支持打进 opencodex 配额层（幂等）。

依赖：apply_qoder.sh（qoder-global provider）已应用；patch_quota_global.py 已应用
（gates 里已有 workbuddy-global 行）。结构仿 patch_quota_global.py。

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

def marker_hit(text, label):
    return label in ("门① explicitAccountReader", "门② explicitQuotaDestination") and 'provider === "qoder-global"' in text

def patch(path, anchor, replacement, label):
    text = open(path, encoding="utf-8").read()
    if marker_hit(text, label):
        print(f"  已存在，跳过：{label}")
        return
    n = text.count(anchor)
    if n != 1:
        sys.exit(f"patch_quota_qoder: {label} 锚点匹配 {n} 次（期望 1）。opencodex 结构可能已变：{path}")
    open(path, "w", encoding="utf-8").write(text.replace(anchor, replacement, 1))
    print(f"  已修改 {label}")

# ── 门①：explicitAccountReader 追加 qoder-global ─────────────────────
g = GATES
text = open(g, encoding="utf-8").read()
if 'provider === "qoder-global"' not in text:
    anchor1 = '    || provider === "workbuddy-global";\n'
    if text.count(anchor1) == 1:
        open(g, "w", encoding="utf-8").write(text.replace(
            anchor1, '    || provider === "workbuddy-global"\n    || provider === "qoder-global";\n', 1))
        print("  已修改 门① explicitAccountReader（接在 workbuddy-global 行后）")
    else:
        sys.exit("patch_quota_qoder: 门① 锚点缺失——先运行 patch_quota_global.py")
else:
    print("  已存在，跳过：门①")

# ── 门②：explicitQuotaDestination 追加 qoder-global ──────────────────
text = open(g, encoding="utf-8").read()
hits = text.count('provider === "qoder-global"')
if hits < 2:
    anchor2 = '    || provider === "autoclaw";\n'
    if text.count(anchor2) == 1:
        open(g, "w", encoding="utf-8").write(text.replace(
            anchor2, '    || provider === "autoclaw"\n    || provider === "qoder-global";', 1))
        print("  已修改 门② explicitQuotaDestination")
    else:
        sys.exit("patch_quota_qoder: 门② 锚点缺失")
else:
    print("  已存在，跳过：门②")

# ── switch case ──────────────────────────────────────────────────────
text = open(QUOTA, encoding="utf-8").read()
if 'case "qoder-global":' not in text:
    anchor3 = '    case "workbuddy-global": result = await fetchWorkbuddyGlobalQuotaReport(provider, accessToken); break;\n'
    if text.count(anchor3) == 1:
        open(QUOTA, "w", encoding="utf-8").write(text.replace(
            anchor3,
            anchor3 + '    case "qoder-global": result = await fetchQoderQuotaReport(provider, accessToken); break;\n', 1))
        print("  已修改 switch case qoder-global")
    else:
        sys.exit("patch_quota_qoder: switch case 锚点缺失——先运行 patch_quota_global.py")
else:
    print("  已存在，跳过：switch case")

# ── report 适配器 ────────────────────────────────────────────────────
text = open(QUOTA, encoding="utf-8").read()
if "async function fetchQoderQuotaReport" not in text:
    ADAPTER = (
        "\n// workbuddy2api overlay: adapt the qoder fetcher to ProviderQuotaReport via report().\n"
        "async function fetchQoderQuotaReport(provider: string, accessToken: string): Promise<ProviderQuotaReport | null> {\n"
        "  const fetched = await fetchQoderQuotaRaw(provider, accessToken);\n"
        "  if (!fetched) return null;\n"
        "  return buildWorkbuddyQuotaReport(provider, fetched.source, fetched.quota);\n"
        "}\n"
    )
    anchor4 = "async function fetchExplicitCurrentQuota("
    if text.count(anchor4) != 1:
        sys.exit("patch_quota_qoder: 适配器锚点缺失")
    open(QUOTA, "w", encoding="utf-8").write(text.replace(anchor4, ADAPTER + "\n" + anchor4, 1))
    print("  已修改 report 适配器")
else:
    print("  已存在，跳过：report 适配器")

# ── import ───────────────────────────────────────────────────────────
text = open(QUOTA, encoding="utf-8").read()
if 'from "./qoder-quota"' not in text:
    anchor5 = 'import { fetchWorkbuddyGlobalQuota as fetchWorkbuddyGlobalQuotaRaw } from "./workbuddy-global-quota";\n'
    if text.count(anchor5) != 1:
        sys.exit("patch_quota_qoder: import 锚点缺失——先运行 patch_quota_global.py")
    open(QUOTA, "w", encoding="utf-8").write(text.replace(
        anchor5,
        anchor5 + '// workbuddy2api overlay: quota fetcher for the qoder-global provider.\n'
        'import { fetchQoderQuota as fetchQoderQuotaRaw } from "./qoder-quota";\n', 1))
    print("  已修改 import qoder-quota")
else:
    print("  已存在，跳过：import")

print("patch_quota_qoder 完成")
