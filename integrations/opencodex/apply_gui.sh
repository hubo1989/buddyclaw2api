#!/bin/bash
# apply_gui.sh 将「AutoClaw 账号池」页面注入 opencodex 面板（幂等，支持 --check / --revert）。
#
# 原理：向 opencodex 的 gui/dist/ 复制独立页面 autoclaw-accounts.html。
# 该页面不进 SPA 路由（opencodex GUI 是 React SPA，静态文件按路径直接服务），
# 访问 http://127.0.0.1:10100/autoclaw-accounts.html 即可使用。
# 页面直接 fetch http://127.0.0.1:7863/admin/autoclaw/*（workbuddy2api 管理端点，已带 CORS）。
#
# 用法:
#   ./apply_gui.sh            # 注入/更新
#   ./apply_gui.sh --check    # 检查注入状态
#   ./apply_gui.sh --revert   # 移除页面

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_HTML="${SCRIPT_DIR}/gui/autoclaw-accounts.html"
PAGE_NAME="autoclaw-accounts.html"
# 面板内嵌入口：在 /providers 页面挂一个浮动按钮，点开 iframe 加载账号管理页
SRC_JS="${SCRIPT_DIR}/gui/providers-autoclaw-panel.js"
JS_NAME="providers-autoclaw-panel.js"
# 阿里云滑块脚本本地副本：外网 o.alicdn.com 慢/被广告插件拦时滑块加载不出来
SRC_CAPTCHA="${SCRIPT_DIR}/gui/AliyunCaptcha.js"
CAPTCHA_NAME="AliyunCaptcha.js"
MARKER="<!-- autoclaw-panel-inject -->"
TAG="<script src=\"/${JS_NAME}\" defer></script>"

# 在 index.html 注入 script 标签（幂等）
inject_index() {
  python3 - "$1" "$MARKER" "$TAG" <<'PY'
import sys, pathlib
p = pathlib.Path(sys.argv[1]); marker = sys.argv[2]; tag = sys.argv[3]
s = p.read_text(encoding="utf-8")
if marker in s:
    print("  index.html 已注入（跳过）")
    sys.exit(0)
if "</body>" in s:
    s = s.replace("</body>", f"{marker}\n{tag}\n</body>")
else:
    s = s + f"\n{marker}\n{tag}\n"
p.write_text(s, encoding="utf-8")
print("  index.html 已注入浮动入口")
PY
}

# 从 index.html 移除注入
strip_index() {
  python3 - "$1" "$MARKER" "$TAG" <<'PY'
import sys, pathlib, re
p = pathlib.Path(sys.argv[1]); marker = sys.argv[2]; tag = sys.argv[3]
s = p.read_text(encoding="utf-8")
if marker not in s:
    print("  index.html 本就未注入")
    sys.exit(0)
s = s.replace(marker, "").replace(tag + "\n", "").replace(tag, "")
s = re.sub(r"\n{3,}", "\n\n", s)
p.write_text(s, encoding="utf-8")
print("  index.html 已还原")
PY
}

# 面板对所有静态响应都发 X-Frame-Options: DENY / frame-ancestors 'none'，
# 同源 iframe 也会被拒 → 右下角弹层白屏。给账号管理页开一个例外。
IFRAME_MARK="// [autoclaw-panel] allow same-origin iframe embedding"
patch_iframe_header() {
  python3 - "$1" "$IFRAME_MARK" <<'PY'
import sys, pathlib
p = pathlib.Path(sys.argv[1]); mark = sys.argv[2]
if not p.exists():
    print("  gui-static.ts 不存在，跳过"); sys.exit(0)
s = p.read_text(encoding="utf-8")
if mark in s:
    print("  gui-static.ts 已打过 iframe 例外"); sys.exit(0)
anchor = '  if (ext === ".html") return htmlResponse(filePath, session, runtimeRole, managementAuthRequired);'
if anchor not in s:
    print("  ✗ 未找到插入锚点，gui-static.ts 结构可能已变"); sys.exit(1)
block = (mark + "\n"
         + '  if (filePath.endsWith("autoclaw-accounts.html")) {\n'
         + '    return new Response(readFileSync(filePath), {\n'
         + '      headers: { "Content-Type": "text/html", "Cache-Control": "no-store" },\n'
         + '    });\n'
         + '  }\n'
         + anchor)
p.write_text(s.replace(anchor, block, 1), encoding="utf-8")
print("  gui-static.ts 已加 iframe 例外（需重启守护进程生效）")
PY
}
unpatch_iframe_header() {
  python3 - "$1" "$IFRAME_MARK" <<'PY'
import sys, pathlib, re
p = pathlib.Path(sys.argv[1]); mark = sys.argv[2]
if not p.exists() or mark not in p.read_text(encoding="utf-8"):
    print("  gui-static.ts 本就未打补丁"); sys.exit(0)
s = p.read_text(encoding="utf-8")
i = s.index(mark)
j = s.index('  if (ext === ".html")', i)
s = s[:i] + s[j:]
p.write_text(s, encoding="utf-8")
print("  gui-static.ts 已还原")
PY
}

# 定位 opencodex 安装目录
find_dist() {
  local nvm_root
  nvm_root="$(ls -d "${HOME}"/.nvm/versions/node/*/lib/node_modules/@bitkyc08/opencodex/gui/dist 2>/dev/null | sort -V | tail -1 || true)"
  local cands=(
    "${nvm_root:-/nonexistent}"
    "$(npm root -g 2>/dev/null)/@bitkyc08/opencodex/gui/dist"
    "/opt/homebrew/lib/node_modules/@bitkyc08/opencodex/gui/dist"
  )
  for c in "${cands[@]}"; do
    if [ -f "${c}/index.html" ]; then
      echo "${c}"
      return 0
    fi
  done
  return 1
}

mode="${1:-apply}"
case "${mode}" in
  --check)
    DIST="$(find_dist)" || { echo "✗ 未找到 opencodex gui/dist"; exit 1; }
    if [ -f "${DIST}/${PAGE_NAME}" ]; then
      if cmp -s "${SRC_HTML}" "${DIST}/${PAGE_NAME}"; then
        echo "✓ 页面已注入且为最新: ${DIST}/${PAGE_NAME}"
      else
        echo "! 页面已注入但版本较旧（重新运行 apply 更新）"
      fi
    else
      echo "✗ 未注入"
    fi
    [ -f "${DIST}/${JS_NAME}" ] && echo "✓ 面板入口脚本: ${DIST}/${JS_NAME}" || echo "✗ 面板入口脚本缺失"
    grep -q "${MARKER}" "${DIST}/index.html" && echo "✓ index.html 已挂入口" || echo "✗ index.html 未挂入口"
    SRC_TS="$(dirname "${DIST}")/../src/server/gui-static.ts"
    # -F：IFRAME_MARK 里带 "["，按正则匹配会被当成字符范围，导致这里误报「未打」
    [ -f "${SRC_TS}" ] && grep -qF "${IFRAME_MARK}" "${SRC_TS}" && echo "✓ iframe 例外已打" || echo "✗ iframe 例外未打（弹层会白屏）"
    ;;
  --revert)
    DIST="$(find_dist)" || { echo "✗ 未找到 opencodex gui/dist"; exit 1; }
    if [ -f "${DIST}/${PAGE_NAME}" ]; then
      rm "${DIST}/${PAGE_NAME}"
      echo "✓ 已移除 ${DIST}/${PAGE_NAME}"
    else
      echo "本就未注入"
    fi
    rm -f "${DIST}/${JS_NAME}"
    strip_index "${DIST}/index.html"
    unpatch_iframe_header "$(dirname "${DIST}")/../src/server/gui-static.ts"
    ;;
  apply)
    [ -f "${SRC_HTML}" ] || { echo "✗ 源文件缺失: ${SRC_HTML}"; exit 1; }
    DIST="$(find_dist)" || { echo "✗ 未找到 opencodex gui/dist（opencodex 未安装?）"; exit 1; }
    cp "${SRC_HTML}" "${DIST}/${PAGE_NAME}"
    cp "${SRC_JS}" "${DIST}/${JS_NAME}"
    if [ -f "${SRC_CAPTCHA}" ]; then
      cp "${SRC_CAPTCHA}" "${DIST}/${CAPTCHA_NAME}"
    fi
    inject_index "${DIST}/index.html"
    PKG_ROOT="$(dirname "${DIST}")/.."
    patch_iframe_header "${PKG_ROOT}/src/server/gui-static.ts"
    echo "✓ 已注入 ${DIST}/${PAGE_NAME}"
    echo "  独立入口: http://127.0.0.1:10100/${PAGE_NAME}"
    echo "  面板内入口: 提供方页面右下角「AutoClaw 账号管理」按钮"
    ;;
  *)
    echo "用法: $0 [--check|--revert]"
    exit 2
    ;;
esac
