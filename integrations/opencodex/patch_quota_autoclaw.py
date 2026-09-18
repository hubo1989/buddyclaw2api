#!/usr/bin/env python3
"""把 autoclaw 配额支持打进 opencodex 的配额层（幂等，锚点定位）。

依赖：先跑过 apply.sh（workbuddy 补丁）。锚点取自 workbuddy 补丁之后的形态；
若锚点失配会明确报错而不是写坏文件（配额只是展示层增强，失败不影响登录/对话）。

2.58.0 起上游拆包：
  · 门控函数 explicitAccountReader / explicitQuotaDestination -> providers/quota/account-cache.ts
  · report() 构造器                                           -> providers/quota/report-cache.ts
  · readExplicitAccountQuota / fetchExplicitCurrentQuota      -> 仍留在 providers/quota.ts

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
REVERT = "--revert" in sys.argv

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


if REVERT:
    # ── 门控文件：逐条还原（先处理损坏形态，再处理正常形态）────────────────
    g = want(GATES)
    gate_replacements = [
        # 损坏形态：分号卡在中间，留下孤立的 || 行
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
    ]
    for old, new in gate_replacements:
        if old in g:
            g = g.replace(old, new, 1)
            put(GATES, g)
    # 兜底：任何残留的 autoclaw 分支行
    g = want(GATES)
    for lo in ('    || provider === "autoclaw";\n', '    || provider === "autoclaw"\n'):
        while lo in g:
            g = g.replace(lo, "", 1)
            put(GATES, g)

    # ── quota.ts：分发 / import / 适配函数 ─────────────────────────────────
    text = want(QUOTA)
    quota_replacements = [
        ('\n    case "autoclaw": result = await fetchAutoclawQuotaReport(provider, accessToken); break;', ''),
        ('\n// workbuddy2api overlay: report() builder for the autoclaw quota adapter.\n'
         'import { report as buildAutoclawQuotaReport } from "./quota/report-cache";', ''),
        ('\n// workbuddy2api overlay: quota fetcher for the built-in autoclaw provider.\n'
         'import { fetchAutoclawQuota as fetchAutoclawQuotaRaw } from "./autoclaw-quota";', ''),
    ]
    for old, new in quota_replacements:
        if old in text:
            text = text.replace(old, new, 1)
            put(QUOTA, text)
    text = want(QUOTA)
    marker = "// workbuddy2api overlay: adapt the autoclaw fetcher to ProviderQuotaReport via report()."
    if marker in text:
        start = text.index(marker)
        end = text.index("async function fetchWorkbuddyQuotaReport(", start)
        text = text[:start] + text[end:]
        put(QUOTA, text)

    if _dirty:
        flush()
        print("  已移除 autoclaw 配额补丁：" + "、".join(os.path.basename(p) for p in sorted(_dirty)))
    else:
        print("  本就没有 autoclaw 配额补丁")
    sys.exit(0)

# ── 1) 门①：explicitAccountReader 放行 autoclaw ────────────────────────
# 锚点由 apply.sh 产出，已是「分号在末行」的形态，无需再摘分号。
anchor1 = ('return provider === "xai" || provider === "cursor" || provider === "kimi" || provider === "command-code"\n'
           '    || provider === "workbuddy";')
g = want(GATES)
if 'provider === "autoclaw"' not in g:
    if g.count(anchor1) != 1:
        sys.exit(f"锚点 explicitAccountReader 匹配 {g.count(anchor1)} 次（期望 1，请先跑 apply.sh）")
    put(GATES, g.replace(anchor1, anchor1[:-1] + '\n    || provider === "autoclaw";', 1))
    print("  已修改 explicitAccountReader（配额白名单）")

# ── 2) 门②：explicitQuotaDestination 放行 autoclaw ─────────────────────
anchor2 = '  return provider === "xai" || provider === "cursor" || provider === "workbuddy";'
g = want(GATES)
if g.count(anchor2) == 1 and '|| provider === "autoclaw";' not in g.split(anchor2)[0][-300:]:
    put(GATES, g.replace(
        anchor2,
        '  // workbuddy2api overlay: autoclaw 走自家签名端点，不受 config.baseUrl 约束。\n'
        '  return provider === "xai" || provider === "cursor" || provider === "workbuddy"\n'
        '    || provider === "autoclaw";',
        1,
    ))
    print("  已修改 explicitQuotaDestination（第二道门）")

# ── 3) fetcher 分发（quota.ts）─────────────────────────────────────────
anchor3 = '    case "workbuddy": result = await fetchWorkbuddyQuotaReport(provider, accessToken); break;'
text = want(QUOTA)
if 'case "autoclaw"' not in text:
    if text.count(anchor3) != 1:
        sys.exit(f"锚点 readExplicitAccountQuota switch 匹配 {text.count(anchor3)} 次（期望 1，请先跑 apply.sh）")
    put(QUOTA, text.replace(
        anchor3,
        anchor3 + '\n    case "autoclaw": result = await fetchAutoclawQuotaReport(provider, accessToken); break;',
        1,
    ))
    print("  已修改 readExplicitAccountQuota（fetcher 分发）")

# ── 4) import（quota.ts）───────────────────────────────────────────────
# report() 在 2.58.0 搬到了 quota/report-cache.ts，quota.ts 不再转引它 → 自己引一份。
imp_anchor = 'import { fetchWorkbuddyQuota as fetchWorkbuddyQuotaRaw } from "./workbuddy-quota";'
report_imp = 'import { report as buildAutoclawQuotaReport } from "./quota/report-cache";'
fetch_imp = ('// workbuddy2api overlay: quota fetcher for the built-in autoclaw provider.\n'
             'import { fetchAutoclawQuota as fetchAutoclawQuotaRaw } from "./autoclaw-quota";')
text = want(QUOTA)
if 'autoclaw-quota' not in text:
    if text.count(imp_anchor) == 1:
        put(QUOTA, text.replace(imp_anchor, imp_anchor + '\n' + report_imp + '\n' + fetch_imp, 1))
        print("  已插入 import（抓取器 + report 构造器）")
    else:
        sys.exit("找不到 workbuddy-quota 的 import 锚点（请先跑 apply.sh）")
elif report_imp not in want(QUOTA):
    # 升级路径：旧版只引了抓取器，补上 report 构造器
    text = want(QUOTA)
    put(QUOTA, text.replace(imp_anchor, imp_anchor + '\n' + report_imp, 1))
    print("  已插入 report 构造器 import（升级）")

# ── 5) 适配函数（quota.ts）─────────────────────────────────────────────
adapter_anchor = 'async function fetchWorkbuddyQuotaReport('
adapter_code = '''// workbuddy2api overlay: adapt the autoclaw fetcher to ProviderQuotaReport via report().
async function fetchAutoclawQuotaReport(provider: string, accessToken: string): Promise<ProviderQuotaReport | null> {
  const fetched = await fetchAutoclawQuotaRaw(provider, accessToken);
  if (!fetched) return null;
  return buildAutoclawQuotaReport(provider, fetched.source, fetched.quota);
}

'''
text = want(QUOTA)
if '  return report(provider, fetched.source, fetched.quota);' in text:
    # 升级路径：旧版直接调用 quota.ts 本地的 report()（2.58.0 已不存在）
    put(QUOTA, text.replace(
        '  return report(provider, fetched.source, fetched.quota);',
        '  return buildAutoclawQuotaReport(provider, fetched.source, fetched.quota);',
        1,
    ))
    print("  已升级适配函数（report -> buildAutoclawQuotaReport）")
elif adapter_code.strip() not in text:
    if text.count(adapter_anchor) != 1:
        sys.exit(f"锚点 fetchWorkbuddyQuotaReport 匹配 {text.count(adapter_anchor)} 次（期望 1，请先跑 apply.sh）")
    put(QUOTA, text.replace(adapter_anchor, adapter_code + adapter_anchor, 1))
    print("  已插入 fetchAutoclawQuotaReport 适配函数")

if _dirty:
    flush()
    print("  已写入：" + "、".join(os.path.basename(p) for p in sorted(_dirty)))
else:
    print("  全部已存在，跳过")
