#!/usr/bin/env python3
"""把 autoclaw 配额支持打进 opencodex 的 quota.ts（幂等，锚点定位）。

依赖：先跑过 apply.sh（workbuddy 补丁）。锚点取自 workbuddy 补丁之后的形态；
若锚点失配会明确报错而不是写坏文件（配额只是展示层增强，失败不影响登录/对话）。

env:
  OCX_QUOTA    quota.ts 路径
"""
import os
import sys

QUOTA = os.environ["OCX_QUOTA"]
REVERT = "--revert" in sys.argv
text = open(QUOTA, encoding="utf-8").read()
changed = False

if REVERT:
    # 逐条还原（先处理可能存在的损坏形态，再处理正常形态）
    replacements = [
        # 损坏形态：分号被卡在中间，留下孤立的 || 行
        ('    || provider === "workbuddy";\n    || provider === "autoclaw";\n',
         '    || provider === "workbuddy";\n'),
        # 正常形态：门①
        ('    || provider === "workbuddy"\n    || provider === "autoclaw";',
         '    || provider === "workbuddy";'),
        # 正常形态：门②
        ('  // workbuddy2api overlay: autoclaw 走自家签名端点，不受 config.baseUrl 约束。\n'
         '  return provider === "xai" || provider === "cursor" || provider === "workbuddy"\n'
         '    || provider === "autoclaw";',
         '  return provider === "xai" || provider === "cursor" || provider === "workbuddy";'),
        # switch case
        ('\n    case "autoclaw": result = await fetchAutoclawQuotaReport(provider, accessToken); break;', ''),
        # import
        ('\n// workbuddy2api overlay: quota fetcher for the built-in autoclaw provider.\n'
         'import { fetchAutoclawQuota as fetchAutoclawQuotaRaw } from "./autoclaw-quota";', ''),
    ]
    for old, new in replacements:
        if old in text:
            text = text.replace(old, new, 1)
            changed = True
    # adapter 函数整块移除
    marker = "// workbuddy2api overlay: adapt the autoclaw fetcher to ProviderQuotaReport via report()."
    if marker in text:
        start = text.index(marker)
        end = text.index("async function fetchWorkbuddyQuotaReport(", start)
        text = text[:start] + text[end:]
        changed = True
    # 兜底：任何残留的 autoclaw 补丁行
    leftovers = [
        '    || provider === "autoclaw";\n',
        '    || provider === "autoclaw"\n',
    ]
    for lo in leftovers:
        while lo in text:
            text = text.replace(lo, "", 1)
            changed = True
    if changed:
        open(QUOTA, "w", encoding="utf-8").write(text)
        print("  quota.ts 已移除 autoclaw 补丁")
    else:
        print("  quota.ts 中本就没有 autoclaw 补丁")
    sys.exit(0)

# ── 1) 白名单门①：explicitAccountReader 放行 autoclaw ─────────────────
anchor1 = ('return provider === "xai" || provider === "cursor" || provider === "kimi" || provider === "command-code"\n'
           '    || provider === "workbuddy";')
if 'provider === "autoclaw"' not in text:
    if text.count(anchor1) != 1:
        sys.exit(f"锚点 explicitAccountReader 匹配 {text.count(anchor1)} 次（期望 1，请先跑 apply.sh）")
    # 锚点以 ';' 结尾：先摘掉分号，追加新分支后再补回（否则分号卡在中间 → 语法错误）
    text = text.replace(anchor1, anchor1[:-1] + '\n    || provider === "autoclaw";', 1)
    changed = True
    print("  已修改 explicitAccountReader（配额白名单）")

# ── 2) 门②：explicitQuotaDestination 放行 autoclaw ────────────────────
anchor2 = '  return provider === "xai" || provider === "cursor" || provider === "workbuddy";'
if text.count(anchor2) == 1 and '|| provider === "autoclaw";' not in text.split(anchor2)[0][-300:]:
    text = text.replace(
        anchor2,
        '  // workbuddy2api overlay: autoclaw 走自家签名端点，不受 config.baseUrl 约束。\n'
        '  return provider === "xai" || provider === "cursor" || provider === "workbuddy"\n'
        '    || provider === "autoclaw";',
        1,
    )
    changed = True
    print("  已修改 explicitQuotaDestination（第二道门）")

# ── 3) fetcher 分发 ───────────────────────────────────────────────────
anchor3 = '    case "workbuddy": result = await fetchWorkbuddyQuotaReport(provider, accessToken); break;'
if 'case "autoclaw"' not in text:
    if text.count(anchor3) != 1:
        sys.exit(f"锚点 readExplicitAccountQuota switch 匹配 {text.count(anchor3)} 次（期望 1，请先跑 apply.sh）")
    text = text.replace(
        anchor3,
        anchor3 + '\n    case "autoclaw": result = await fetchAutoclawQuotaReport(provider, accessToken); break;',
        1,
    )
    changed = True
    print("  已修改 readExplicitAccountQuota（fetcher 分发）")

# ── 4) import ─────────────────────────────────────────────────────────
imp_anchor = 'import { fetchWorkbuddyQuota as fetchWorkbuddyQuotaRaw } from "./workbuddy-quota";'
if 'autoclaw-quota' not in text:
    if text.count(imp_anchor) == 1:
        text = text.replace(
            imp_anchor,
            imp_anchor + '\n// workbuddy2api overlay: quota fetcher for the built-in autoclaw provider.\n'
            'import { fetchAutoclawQuota as fetchAutoclawQuotaRaw } from "./autoclaw-quota";',
            1,
        )
        changed = True
        print("  已插入 import")
    else:
        sys.exit("找不到 workbuddy-quota 的 import 锚点（请先跑 apply.sh）")

# ── 5) 适配函数 ───────────────────────────────────────────────────────
adapter_anchor = 'async function fetchWorkbuddyQuotaReport('
adapter_code = '''// workbuddy2api overlay: adapt the autoclaw fetcher to ProviderQuotaReport via report().
async function fetchAutoclawQuotaReport(provider: string, accessToken: string): Promise<ProviderQuotaReport | null> {
  const fetched = await fetchAutoclawQuotaRaw(provider, accessToken);
  if (!fetched) return null;
  return report(provider, fetched.source, fetched.quota);
}

'''
if adapter_code.strip() not in text:
    if text.count(adapter_anchor) != 1:
        sys.exit(f"锚点 fetchWorkbuddyQuotaReport 匹配 {text.count(adapter_anchor)} 次（期望 1，请先跑 apply.sh）")
    text = text.replace(adapter_anchor, adapter_code + adapter_anchor, 1)
    changed = True
    print("  已插入 fetchAutoclawQuotaReport 适配函数")

if changed:
    open(QUOTA, "w", encoding="utf-8").write(text)
    print("  quota.ts 已写入")
else:
    print("  全部已存在，跳过")
