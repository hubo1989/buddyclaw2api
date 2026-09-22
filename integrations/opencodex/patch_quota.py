#!/usr/bin/env python3
"""把 workbuddy 配额支持打进 opencodex 的配额层（幂等，锚点定位）。

2.58.0 起上游拆了包：
  · 门控函数 explicitAccountReader / explicitQuotaDestination -> providers/quota/account-cache.ts
  · report() 构造器                                           -> providers/quota/report-cache.ts
  · readExplicitAccountQuota / fetchExplicitCurrentQuota      -> 仍留在 providers/quota.ts
所以按「文件是否存在」自适应：拆包版改两个文件，旧版布局只改 quota.ts。
"""
import os, sys

QUOTA = os.environ["OCX_QUOTA"]
# 拆包后的门控文件；不存在（旧版布局）则回退到 quota.ts 本身
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


# ── 1) 白名单：explicitAccountReader 放行 workbuddy ──────────────────────
# ⚠️ 锚点自带行尾分号，替换时必须把分号一并搬到末行，否则第一行被 ';' 提前终止、
#    第二行以 '||' 开头 —— 直接是语法错误。
ANCHOR_READER = 'return provider === "xai" || provider === "cursor" || provider === "kimi" || provider === "command-code";'
MARKER_READER = (
    'return provider === "xai" || provider === "cursor" || provider === "kimi" || provider === "command-code"\n'
    '    || provider === "workbuddy";'
)
g = want(GATES)
if MARKER_READER not in g:
    n = g.count(ANCHOR_READER)
    if n != 1:
        sys.exit(f"锚点 explicitAccountReader 匹配 {n} 次（期望 1）：{GATES}")
    put(GATES, g.replace(ANCHOR_READER, MARKER_READER, 1))
    print("  已修改 explicitAccountReader（配额白名单）")

# ── 1b) 第二道门：explicitQuotaDestination 也放行 workbuddy ──────────────
# 注意它和 explicitAccountReader 是两个独立 gate，都要过。
ANCHOR_DEST = '  return provider === "xai" || provider === "cursor";'
MARKER_DEST = '  return provider === "xai" || provider === "cursor" || provider === "workbuddy";'
g = want(GATES)
if MARKER_DEST not in g:
    n = g.count(ANCHOR_DEST)
    if n != 1:
        sys.exit(f"锚点 explicitQuotaDestination 匹配 {n} 次（期望 1）：{GATES}")
    put(GATES, g.replace(ANCHOR_DEST, MARKER_DEST, 1))
    print("  已修改 explicitQuotaDestination（第二道门）")

# ── 2) fetcher 分发：readExplicitAccountQuota 的 switch 加 workbuddy case ─
ANCHOR_SWITCH = '    case "command-code": result = await fetchCommandCodeQuota(provider, config, accessToken); break;'
text = want(QUOTA)
if '    case "workbuddy":' not in text:
    n = text.count(ANCHOR_SWITCH)
    if n != 1:
        sys.exit(f"锚点 readExplicitAccountQuota switch 匹配 {n} 次（期望 1）：{QUOTA}")
    put(QUOTA, text.replace(
        ANCHOR_SWITCH,
        ANCHOR_SWITCH + '\n    case "workbuddy": result = await fetchWorkbuddyQuotaReport(provider, accessToken); break;',
        1,
    ))
    print("  已修改 readExplicitAccountQuota（fetcher 分发）")

# ── 3) import：抓取器 + report 构造器 ───────────────────────────────────
# report() 在 2.58.0 搬到了 quota/report-cache.ts，且 quota.ts 不再转引它，必须自己引。
ANCHOR_STORE = 'import { getAccountCredential, getAccountSet } from "../oauth/store";'
OVERLAY_IMPORTS = (
    '// workbuddy2api overlay: 内置 workbuddy provider 的配额抓取器 + report 构造器。\n'
    'import { fetchWorkbuddyQuota as fetchWorkbuddyQuotaRaw } from "./workbuddy-quota";\n'
    'import { report as buildWorkbuddyQuotaReport } from "./quota/report-cache";'
)
LEGACY_FETCH_IMPORT = 'import { fetchWorkbuddyQuota as fetchWorkbuddyQuotaRaw } from "./workbuddy-quota";'
FETCH_IMPORT = 'import { fetchWorkbuddyQuota as fetchWorkbuddyQuotaRaw } from "./workbuddy-quota";'
REPORT_IMPORT = 'import { report as buildWorkbuddyQuotaReport } from "./quota/report-cache";'
NOTE_LINE = '// workbuddy2api overlay: 内置 workbuddy provider 的配额抓取器 + report 构造器。'

text = want(QUOTA)
if REPORT_IMPORT not in text:
    if LEGACY_FETCH_IMPORT in text:
        # 升级路径：旧版只引了抓取器，补上 report 构造器
        put(QUOTA, text.replace(LEGACY_FETCH_IMPORT, LEGACY_FETCH_IMPORT + '\n' + REPORT_IMPORT, 1))
        print("  已插入 report 构造器 import（升级）")
    elif text.count(ANCHOR_STORE) == 1:
        put(QUOTA, text.replace(ANCHOR_STORE, ANCHOR_STORE + '\n' + OVERLAY_IMPORTS, 1))
        print("  已插入 import（抓取器 + report 构造器）")
    else:
        # import 布局随版本可能变化；退而求其次：插到首个 import 块之后
        first_import_end = text.index('\n\n', text.index('import '))
        put(QUOTA, text[:first_import_end] + '\n' + OVERLAY_IMPORTS + text[first_import_end:])
        print("  已插入 import（fallback 位置）")

# ── 4) 适配函数：把抓取结果转成 report() 需要的形状 ─────────────────────
ADAPTER_FN = 'async function fetchWorkbuddyQuotaReport('
ADAPTER_ANCHOR = 'async function fetchExplicitCurrentQuota('
ADAPTER_CODE = '''// workbuddy2api overlay: adapt the raw fetcher to ProviderQuotaReport via report().
async function fetchWorkbuddyQuotaReport(provider: string, accessToken: string): Promise<ProviderQuotaReport | null> {
  const fetched = await fetchWorkbuddyQuotaRaw(provider, accessToken);
  if (!fetched) return null;
  return buildWorkbuddyQuotaReport(provider, fetched.source, fetched.quota);
}

'''
OLD_ADAPTER_BODY = '  return report(provider, fetched.source, fetched.quota);'
NEW_ADAPTER_BODY = '  return buildWorkbuddyQuotaReport(provider, fetched.source, fetched.quota);'

text = want(QUOTA)
if OLD_ADAPTER_BODY in text:
    # 升级路径：旧版直接调用 quota.ts 本地的 report()，现在要显式走 import 进来的构造器
    put(QUOTA, text.replace(OLD_ADAPTER_BODY, NEW_ADAPTER_BODY, 1))
    print("  已升级适配函数（report -> buildWorkbuddyQuotaReport）")
elif ADAPTER_FN not in text:
    n = text.count(ADAPTER_ANCHOR)
    if n != 1:
        sys.exit(f"锚点 fetchExplicitCurrentQuota 匹配 {n} 次（期望 1）：{QUOTA}")
    put(QUOTA, text.replace(ADAPTER_ANCHOR, ADAPTER_CODE + ADAPTER_ANCHOR, 1))
    print("  已插入 fetchWorkbuddyQuotaReport 适配函数")

if not _dirty:
    print("  全部已存在，跳过")
else:
    for path in _dirty:
        open(path, "w", encoding="utf-8").write(_texts[path])
    print("  已写入：" + "、".join(os.path.basename(p) for p in sorted(_dirty)))
