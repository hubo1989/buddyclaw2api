/* providers-autoclaw-panel.js
 * 在 opencodex 面板里补齐 AutoClaw 的登录与账号管理入口。
 *
 * 面板原生账户区只提供「打开授权链接 + 粘贴回调」一种交互，且不区分登录方式、输入框很窄。
 * 本脚本不改 React 源码，而是在账户页注入一个**编号步骤式**的表单，底层复用面板自己的登录通道：
 *   POST /api/oauth/login       启动登录流程（→ 守护进程的 loginAutoclaw）
 *   POST /api/oauth/login/code  把一次输入喂给 onManualCodeInput（手机号 / 验证码 / zai|google）
 *   POST /api/oauth/login/cancel
 *   GET  /api/oauth/status?provider=autoclaw   读取当前状态与错误文本
 * 同时隐藏面板原生那套入口（登录按钮 / 复选框 / 错误行），避免"该点哪个"的困惑。
 *
 * 版式（重要，2026-09-14 修订）：账户页的主角色是**已登录的账号池**，所以列表必须排在前面，
 * 添加入口收成列表**最底部**的一个按钮（点开才展开步骤式表单）。旧实现把整张表单插在
 * 「可用账户」标题下面，一进页先看到一大块添加表单、账号列表被挤到屏幕外，与原生页面
 * （Workbuddy 等 provider）的版式相反。现在：列表在上，底部一个「＋ 添加 AutoClaw 账号」。
 */
(function () {
  if (window.__acPanelInjected) return;
  window.__acPanelInjected = true;

  var PROVIDER = "autoclaw";
  var PAGE = "/autoclaw-accounts.html";
  var FLOAT_BTN = null, MODAL = null, FRAME = null;
  var FORM = null;          // 注入容器（含折叠入口按钮 + 展开后的步骤式表单）
  var formOpen = false;     // 展开状态：React 重渲染会重建 DOM，重建后按它还原
  var flow = { vendor: "", smsSent: false, accountsBefore: 0 };
  var nativeKeep = false;   // 用户显式要求显示面板原生登录框（否则每轮都把它藏回去）

  /* ---------------- 小工具 ---------------- */

  function mk(tag, style, text) {
    var el = document.createElement(tag);
    if (style) for (var k in style) el.style[k] = style[k];
    if (text != null) el.textContent = text;
    return el;
  }

  function api(path, body, method) {
    return fetch(path, {
      method: method || "POST",
      headers: { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        return { ok: r.ok, status: r.status, body: j || {} };
      });
    });
  }

  function errText(res) {
    return (res.body && (res.body.error || res.body.message)) || ("HTTP " + res.status);
  }

  function btnStyle(kind) {
    var base = {
      padding: "7px 14px", borderRadius: "8px", cursor: "pointer",
      fontSize: "13px", fontWeight: "600", border: "1px solid transparent",
      whiteSpace: "nowrap",
    };
    if (kind === "primary") { base.background = "#2f6fed"; base.color = "#fff"; }
    else if (kind === "ghost") {
      base.background = "transparent"; base.color = "inherit";
      base.border = "1px solid rgba(128,128,128,.5)";
    }
    return base;
  }

  function inputStyle(width) {
    return {
      width: width, padding: "7px 10px", borderRadius: "8px",
      border: "1px solid rgba(128,128,128,.45)", background: "transparent",
      color: "inherit", fontSize: "13px", outline: "none", boxSizing: "border-box",
    };
  }

  function step(n) {
    return mk("span", {
      display: "inline-flex", alignItems: "center", justifyContent: "center",
      width: "18px", height: "18px", borderRadius: "50%", flex: "0 0 auto",
      background: "rgba(47,111,237,.14)", color: "#2f6fed",
      fontSize: "11.5px", fontWeight: "700",
    }, String(n));
  }

  /* ---------------- 浮动入口（完整管理页） ---------------- */

  function buildFloatButton() {
    FLOAT_BTN = mk("button", {
      position: "fixed", right: "20px", bottom: "20px", zIndex: "2147483000",
      padding: "9px 14px", borderRadius: "999px", cursor: "pointer",
      background: "#2f6fed", color: "#fff", border: "none",
      fontSize: "13px", fontWeight: "600", boxShadow: "0 4px 14px rgba(0,0,0,.28)",
      display: "none",
    }, "AutoClaw 账号管理");
    FLOAT_BTN.addEventListener("click", function () {
      MODAL.style.display = "flex";
      // 同理：只设一次，避免每次打开都重载（重载会清掉未完成的表单/滑块状态）。
      if (!FRAME.src || !FRAME.src.includes(PAGE)) {
        FRAME.src = PAGE;
      }
    });

    MODAL = mk("div", {
      position: "fixed", inset: "0", zIndex: "2147483001",
      background: "rgba(0,0,0,.55)", display: "none",
      alignItems: "center", justifyContent: "center",
    });
    var box = mk("div", {
      width: "92vw", height: "86vh", maxWidth: "1100px",
      background: "#15181f", borderRadius: "12px", overflow: "hidden",
      display: "flex", flexDirection: "column", boxShadow: "0 20px 60px rgba(0,0,0,.5)",
    });
    var bar = mk("div", {
      display: "flex", alignItems: "center", justifyContent: "space-between",
      padding: "10px 14px", background: "#1c212b", color: "#e6e8ee",
      fontSize: "13px", fontWeight: "600", flex: "0 0 auto",
    });
    bar.appendChild(mk("span", null, "AutoClaw 账号池 · 账号列表 / 删除 / 滑块 OAuth 登录"));
    var close = mk("button", {
      background: "transparent", border: "1px solid #3a4150", color: "#e6e8ee",
      borderRadius: "6px", padding: "4px 10px", cursor: "pointer", fontSize: "12px",
    }, "关闭");
    close.addEventListener("click", function () { MODAL.style.display = "none"; });
    bar.appendChild(close);

    FRAME = mk("iframe", { flex: "1 1 auto", width: "100%", border: "none", background: "#fff" });
    box.appendChild(bar);
    box.appendChild(FRAME);
    MODAL.appendChild(box);
    MODAL.addEventListener("click", function (e) { if (e.target === MODAL) MODAL.style.display = "none"; });
    document.addEventListener("keydown", function (e) { if (e.key === "Escape") MODAL.style.display = "none"; });

    document.body.appendChild(FLOAT_BTN);
    document.body.appendChild(MODAL);
  }

  function openCaptchaPage(vendor) {
    // 方案 B：国际版 OAuth（Google/Z.ai）统一跳到自建页面完成 —— 滑块、授权、回调粘贴
    // 都在那个独立文档里做（滑块在 iframe/React 环境里反复出问题，独立页面最稳）。
    // 本页面与面板同源，页内直接调面板登录 API，凭据照常进 opencodex 账号池。
    // 带 ?v= 缓存戳：保证每次跳转都拿到最新版登录页（浏览器对无缓存头的 HTML 会启发式缓存）
    window.open(PAGE + "?v=" + Date.now() + "#oauth-" + vendor, "_blank", "noopener");
  }

  /* ---- 方案 B 桥接：自建页(新标签)没有面板 SPA 的带凭据 fetch，经 postMessage 中转 ---- */
  /** 让面板重新拉取账号名册。
   *  面板只在「自己的登录流程走完 / 切页签 / 窗口重新可见」时拉名册；从自建页驱动的登录
   *  它不知道，于是新账号要等用户手动切页签才出现。
   *  实测最可靠的杠杆是派发 visibilitychange（面板用它做可见性轮询），fallback 派发 focus。
   *  两者都是「重新确认当前状态」的语义，不会改动任何账号数据。 */
  function notifyAccountsChanged() {
    try {
      document.dispatchEvent(new Event("visibilitychange"));
      window.dispatchEvent(new Event("focus"));
    } catch (_) {}
  }

  /* ---- 主动刷新账号池 ----
   * 面板把账号名册存在自己的 React store 里，常规节奏是 30 秒轮询一次（pollMs:3e4）。
   * 登录完成后必须让它**立刻**重拉，否则用户要等最多 30 秒才看到新账号，或得手动切页签。
   *
   * 面板为名册挂了 document 级 visibilitychange 处理：拿到该事件就会把各 provider 的
   * 名册重新拉一遍（实测确实会发出 /api/oauth/accounts?provider=… 请求）。
   * 这里多通知几次是因为：面板此刻可能正在别的请求上（inflight 时重入会被跳过），
   * 单次通知可能被吞掉；少量重试能让刷新确定性发生，代价可以忽略。
   */
  function refreshNativeRoster() {
    [0, 1500].forEach(function (delay) {
      setTimeout(notifyAccountsChanged, delay);
    });
  }

  // 跨标签页通道：自建页是用 window.open(..., "noopener") 打开的（还有弹层 iframe 场景），
  // 没有 opener 可回传，postMessage 到不了面板。BroadcastChannel 同源跨标签页可用，
  // 不依赖窗口引用，是这条通知的主通道；postMessage 仅作额外兜底。
  try {
    var acChan = new BroadcastChannel("ocx-autoclaw-accounts");
    acChan.addEventListener("message", function (ev) {
      if (ev && ev.data && ev.data.type === "ocx-accounts-changed") notifyAccountsChanged();
    });
  } catch (_) { /* 不支持 BroadcastChannel 时退回 postMessage */ }

  // 面板 SPA 启动时包装了 window.fetch，自动给 /api/* 附加 gui-session 凭据；
  // 这里只复用当前上下文的 fetch，不接触凭据本身。消息仅接受同源 + 自建页路径的 opener。
  window.addEventListener("message", function (ev) {
    if (ev.origin !== location.origin) return;
    var d = ev.data || {};
    // 自建页登录成功后通知面板立刻重拉账号名册。
    // 面板的名册只在少数几个动作时拉取（登录流程自己走完 / 切页签 / 窗口重新可见），
    // 从自建页驱动的登录面板并不知道 —— 不通知就要等用户手动切页签才看得到新账号。
    if (d.type === "ocx-accounts-changed") { notifyAccountsChanged(); return; }
    if (d.type !== "ocx-bridge-req") return;
    // 只接受我们的自建页发来的请求
    if (!ev.source || String(ev.source.location && ev.source.location.pathname || "") !== PAGE) return;
    var seq = d.seq;
    var finish = function (res) {
      try { ev.source.postMessage({ type: "ocx-bridge-resp", seq: seq, res: res }, location.origin); } catch (_) {}
    };
    fetch(d.path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: d.body === null || d.body === undefined ? undefined : JSON.stringify(d.body),
    })
      .then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (j) {
          finish({ ok: r.ok, status: r.status, body: j || {} });
        });
      })
      .catch(function (e) {
        finish({ ok: false, status: 0, body: { error: String(e && e.message || e) } });
      });
  });

  /* ---------------- 原生登录区处理 ---------------- */

  function nativeInputs() {
    var all = document.querySelectorAll("input");
    var out = [];
    for (var i = 0; i < all.length; i++) {
      var ph = all[i].getAttribute("placeholder") || "";
      if (/paste redirect|粘贴重定向/i.test(ph)) out.push(all[i]);
    }
    return out;
  }

  function nativeRoot() {
    var ins = nativeInputs();
    if (!ins.length) return null;
    var node = ins[0];
    for (var up = 0; up < 3 && node && node.parentElement; up++) {
      node = node.parentElement;
      if (node.querySelectorAll("input").length === 1) break;
    }
    return node || null;
  }

  /** 该元素是否属于我们注入的表单（hideNative 绝不能碰它，否则会把自己的按钮藏掉）。 */
  function isOurs(el) {
    return !!(FORM && FORM.contains(el)) || !!(el.closest && el.closest("[data-ac-form]"));
  }

  /** 隐藏面板原生那套（登录按钮 / 复选框 / 状态行 / 粘贴框）——我们的表单已完全取代它。 */
  function hideNative(enabled) {
    var root = nativeRoot();
    if (root) root.style.display = enabled ? "none" : "";

    var panel = accountsPanel();
    if (!panel) return;
    // 原生 Login / 添加账户按钮 —— 我们的表单已完整覆盖这两条入口（含手机验证码与 Google/Z.ai）。
    // 不藏「添加账户」会和底部我们自己的「＋ 添加 AutoClaw 账号」重复出现两个添加入口。
    var btns = panel.querySelectorAll("button");
    for (var i = 0; i < btns.length; i++) {
      if (isOurs(btns[i])) continue;
      var t = (btns[i].textContent || "").trim();
      if (t === "Login" || t === "登录" || t === "Add account" || t === "添加账户") {
        btns[i].style.display = enabled ? "none" : "";
      }
    }
    // "不要在运行代理的机器上打开浏览器" 复选框（对我们无用）
    var labels = panel.querySelectorAll("label");
    for (var j = 0; j < labels.length; j++) {
      if (isOurs(labels[j])) continue;
      var lt = labels[j].textContent || "";
      if (/打开浏览器|open a browser/i.test(lt)) labels[j].style.display = enabled ? "none" : "";
    }
    // 面板原生登录状态行（"Login cancelled" / "xxx login error: …"）——错误已搬进我们表单
    var statusEls = panel.querySelectorAll("[class*='auth-status'], .pwi-auth-status-text, span, div, p");
    for (var k = 0; k < statusEls.length; k++) {
      var el = statusEls[k];
      if (isOurs(el) || el.children.length !== 0) continue;
      var st = (el.textContent || "").trim();
      if (/^(login cancelled|login error)/i.test(st) || /(登录已取消|登录错误)/.test(st)) {
        el.style.display = enabled ? "none" : "";
      }
    }
  }

  /* ---------------- 表单 ---------------- */

  function setMsgIn(root, text, kind) {
    var el = root && root.querySelector("[data-ac-msg]");
    if (!el) return;
    el.textContent = text || "";
    el.style.color = kind === "bad" ? "#d9534f" : (kind === "ok" ? "#2e9e5b" : "inherit");
  }

  /** 守护进程/上游的业务错误单独一行，不覆盖本表单的操作指引。 */
  function setNativeErrIn(root, text) {
    var el = root && root.querySelector("[data-ac-native]");
    if (!el) return;
    el.textContent = text ? ("上游/守护进程： " + text) : "";
  }

  function buildForm() {
    // 外层容器：默认只有一个「＋ 添加 AutoClaw 账号」按钮，排在账号列表最底部。
    // 点开后展开面板内的步骤式表单；收起/展开不重载页面，未完成的滑块状态也保留。
    var wrap = mk("div", { margin: "10px 0 0", display: "flex", flexDirection: "column", gap: "10px" });
    wrap.setAttribute("data-ac-form", "1");

    // 折叠入口：直接复用面板自己的按钮类，视觉与原生「添加账户 / 刷新额度」完全一致
    // （不写内联配色，避免内联样式压过面板主题色；只兜底 cursor 与最小高度）。
    var openBtn = mk("button", { cursor: "pointer", alignSelf: "flex-start" }, "＋ 添加 AutoClaw 账号");
    openBtn.type = "button";
    openBtn.className = "btn btn-ghost btn-sm";
    openBtn.setAttribute("data-ac-open", "1");
    wrap.appendChild(openBtn);

    var panel = mk("div", {
      border: "1px solid rgba(128,128,128,.32)", borderRadius: "12px",
      padding: "16px 18px", display: "none", maxWidth: "640px",
      flexDirection: "column", gap: "12px",
    });
    panel.setAttribute("data-ac-panel", "1");
    wrap.appendChild(panel);

    /** 展开/收起步骤式表单（按钮文案跟随状态）。 */
    function setOpen(open) {
      formOpen = open;
      panel.style.display = open ? "flex" : "none";
      openBtn.textContent = open ? "收起添加表单" : "＋ 添加 AutoClaw 账号";
      if (open) {
        var smsBox = panel.querySelector("[data-ac-sms]");
        var first = panel.querySelector("[data-ac-phone]");
        if (first && smsBox && smsBox.style.display !== "none") first.focus();
      }
    }

    panel.appendChild(mk("div", { fontWeight: "700", fontSize: "14px" }, "添加 AutoClaw 账号"));
    panel.appendChild(mk("div", { fontSize: "12.5px", opacity: ".72", marginTop: "-6px" },
      "选一种方式，然后按编号一步步做即可。账号会加进 opencodex 的账号池。"));

    /* --- 方式选择：手机验证码 / OAuth（跳转登录页） --- */
    var modeRow = mk("div", { display: "flex", gap: "8px", flexWrap: "wrap" });
    var smsBtn = mk("button", btnStyle("primary"), "手机验证码");
    var oauthBtn = mk("button", btnStyle("ghost"), "Google / Z.ai（OAuth）");
    modeRow.appendChild(smsBtn);
    modeRow.appendChild(oauthBtn);
    panel.appendChild(modeRow);

    // OAuth 入口：vendor 在登录页上选（那里是唯一需要 Google/Z.ai 区分的地方）
    var oauthVendor = "zai";

    /* --- 手机验证码 --- */
    var smsBox = mk("div", { display: "flex", flexDirection: "column", gap: "10px" });
    smsBox.setAttribute("data-ac-sms", "1");

    var r1 = mk("div", { display: "flex", gap: "8px", alignItems: "center", flexWrap: "wrap" });
    r1.appendChild(step(1));
    r1.appendChild(mk("span", { fontSize: "12.5px", opacity: ".8" }, "填手机号，点右侧按钮收短信"));
    var phone = mk("input", inputStyle("200px"));
    phone.setAttribute("placeholder", "手机号（11 位）");
    phone.setAttribute("data-ac-phone", "1");
    phone.setAttribute("inputmode", "numeric");
    var sendBtn = mk("button", btnStyle("primary"), "发送验证码");
    r1.appendChild(phone);
    r1.appendChild(sendBtn);
    smsBox.appendChild(r1);

    var r2 = mk("div", { display: "flex", gap: "8px", alignItems: "center", flexWrap: "wrap" });
    r2.appendChild(step(2));
    r2.appendChild(mk("span", { fontSize: "12.5px", opacity: ".8" }, "填短信里收到的 6 位验证码，点「登录」"));
    var code = mk("input", inputStyle("140px"));
    code.setAttribute("placeholder", "短信验证码");
    code.setAttribute("data-ac-code", "1");
    code.setAttribute("inputmode", "numeric");
    var loginBtn = mk("button", btnStyle("primary"), "登录");
    loginBtn.disabled = true;
    loginBtn.style.opacity = ".5";
    loginBtn.style.cursor = "not-allowed";
    r2.appendChild(code);
    r2.appendChild(loginBtn);
    smsBox.appendChild(r2);
    panel.appendChild(smsBox);

    /* --- 状态与兜底 --- */
    var msg = mk("div", { fontSize: "12.5px", minHeight: "18px", whiteSpace: "pre-wrap" });
    msg.setAttribute("data-ac-msg", "1");
    panel.appendChild(msg);

    var nativeErr = mk("div", { fontSize: "12px", minHeight: "16px", color: "#d9534f", opacity: ".92" });
    nativeErr.setAttribute("data-ac-native", "1");
    panel.appendChild(nativeErr);

    var foot = mk("div", { fontSize: "12px", opacity: ".62", display: "flex", gap: "12px", flexWrap: "wrap" });
    var restore = mk("a", { cursor: "pointer", textDecoration: "underline" }, "用面板原生登录框");
    restore.addEventListener("click", function (e) {
      e.preventDefault();
      nativeKeep = !nativeKeep;
      hideNative(nativeKeep);
      restore.textContent = nativeKeep ? "回到步骤式表单" : "用面板原生登录框";
      setMsgIn(wrap, nativeKeep
        ? "已恢复面板原生登录框：在它的输入框里填 手机号 / 短信验证码 / zai / google 即可（提示文案是面板自带的，与上面步骤等价）。"
        : "已切回步骤式表单。");
    });
    var cancelLink = mk("a", { cursor: "pointer", textDecoration: "underline" }, "取消当前登录");
    cancelLink.addEventListener("click", function (e) {
      e.preventDefault();
      api("/api/oauth/login/cancel", { provider: PROVIDER }).then(function () {
        setMsgIn(wrap, "已取消当前登录流程。", "ok");
      }).catch(function () { setMsgIn(wrap, "取消失败（可能本来就没有进行中的流程）。"); });
    });
    var listLink = mk("a", { cursor: "pointer", textDecoration: "underline" }, "账号列表 / 删除");
    listLink.addEventListener("click", function (e) {
      e.preventDefault();
      MODAL.style.display = "flex";
      if (!FRAME.src || !FRAME.src.includes(PAGE)) {
        FRAME.src = PAGE;
      }
    });
    foot.appendChild(restore);
    foot.appendChild(cancelLink);
    foot.appendChild(listLink);
    panel.appendChild(foot);

    /* --- 交互 --- */

    function acBase() {
      return (localStorage.getItem("ac_api_base") || "http://127.0.0.1:7863").replace(/\/+$/, "");
    }

    function setMode(m) {
      flow.vendor = m === "sms" ? "" : flow.vendor;
      Object.assign(smsBtn.style, btnStyle(m === "sms" ? "primary" : "ghost"));
      Object.assign(oauthBtn.style, btnStyle(m === "oauth" ? "primary" : "ghost"));
      smsBox.style.display = m === "sms" ? "flex" : "none";
      if (m === "sms") {
        setMsgIn(wrap, "第 1 步：填手机号 → 点「发送验证码」；第 2 步：填收到的验证码 → 点「登录」。");
        // 只有表单已展开时才抢焦点：初次构建时会先 setMode("sms") 再收起，
        // 此时对隐藏元素调 focus() 会把焦点/视口从账号列表上拽走。
        if (panel.style.display !== "none") phone.focus();
      } else {
        // OAuth：新标签页打开登录页（滑块 → 授权 → 粘贴回跳，一站式），凭据直接进 opencodex 池
        setMsgIn(wrap, "已在新标签页打开登录页；在那里拖滑块 → 开始登录 → 粘贴回跳地址，完成后回到本页即可看到新账号。");
        openCaptchaPage(oauthVendor);
      }
    }

    function startFlow() {
      return api("/api/oauth/login/cancel", { provider: PROVIDER })
        .catch(function () { return { ok: true }; })
        .then(function () { return api("/api/oauth/login", { provider: PROVIDER, addAccount: true }); })
        .then(function (r) {
          if (!r.ok) throw new Error(errText(r));
          return r.body;
        });
    }

    function submitInput(value) {
      return api("/api/oauth/login/code", { provider: PROVIDER, input: value })
        .then(function (r) {
          if (!r.ok) throw new Error(errText(r));
          return r.body;
        });
    }

    /** 读一次登录状态（失败的读取退化成空对象，由调用方决定怎么处理）。 */
    function fetchStatus() {
      return fetch("/api/oauth/status?provider=" + PROVIDER)
        .then(function (r) { return r.json().catch(function () { return {}; }); })
        .catch(function () { return {}; });
    }

    /**
     * 等登录真正「落盘完成」再宣告成功。
     *
     * ⚠️ POST /api/oauth/login/code 只把输入**塞进**等待中的流程就立刻回 {ok:true}
     * （见 oauth-account-routes：submitManualLoginCode 是同步投递）。上游校验 + saveCredential
     * 都在后台 promise 里跑，凭据落盘前 /api/oauth/accounts 还是旧名册。老实现一看 ok 就报
     * 「登录成功」并立刻刷新，于是面板要么看不到新账号、要么仍显示旧的当前账户。
     *
     * 判定口径（与面板原生 loginOAuth 一致）：
     *   error         → 失败
     *   accounts 变多  → 成功（新账号已落盘）
     *   done === true  → 成功（本次登录已结算且没有 error；同一身份重登是「原地更新」，
     *                    账号数不变，所以不能把「没变多」当成失败）
     */
    // beforeCount < 0 表示基线没读到（读取失败）——此时「数量变多」不可作为成功依据，
    // 否则已有账号会让 n > 0 立刻成立，变成假成功。这种退化情形只认 done。
    function waitForLoginSettled(beforeCount) {
      var known = typeof beforeCount === "number" && beforeCount >= 0;
      var deadline = Date.now() + 90000;
      return new Promise(function (resolve, reject) {
        (function poll() {
          fetchStatus().then(function (d) {
            if (d && d.error) { reject(new Error(String(d.error))); return; }
            var n = (d && d.accounts ? d.accounts : []).length;
            if (known && n > beforeCount) { resolve(d); return; }
            if (d && d.done === true) { resolve(d); return; }
            if (Date.now() > deadline) {
              reject(new Error("等待上游确认超时（90 秒）——请稍后点「刷新额度」或切页签查看账号池。"));
              return;
            }
            setTimeout(poll, 2000);
          }).catch(function () {
            // 单次读取失败（面板重启等）不判死，继续轮询到超时
            if (Date.now() > deadline) { reject(new Error("等待登录结果超时（90 秒）")); return; }
            setTimeout(poll, 2000);
          });
        })();
      });
    }

    /**
     * 发码步骤同理：提交手机号后上游才真正去请求短信，失败（如「手机号码不合法」）
     * 会写进 loginState.error。这里给一个有限的确认窗口 —— 冒错立刻失败，
     * 窗口内没冒错才继续，避免「明明没发出去却说已发送」。
     */
    function waitForUpstreamSend(graceMs) {
      // 实测错误 ~0.4s 就写进状态，但上游/网络有抖动，留 2s 窗口足够可靠，
      // 又不至于让正常路径白等太久。轮询用 400ms：尽快失败，不白等整个窗口。
      var deadline = Date.now() + (graceMs || 2000);
      return new Promise(function (resolve, reject) {
        (function poll() {
          fetchStatus().then(function (d) {
            if (d && d.error) { reject(new Error(String(d.error))); return; }
            if (Date.now() >= deadline) { resolve(d); return; }
            setTimeout(poll, 400);
          }).catch(function () {
            if (Date.now() >= deadline) { resolve({}); return; }
            setTimeout(poll, 400);
          });
        })();
      });
    }

    sendBtn.addEventListener("click", function () {
      var v = (phone.value || "").trim();
      if (!v) { setMsgIn(wrap, "请先填手机号。", "bad"); phone.focus(); return; }
      sendBtn.disabled = true;
      setMsgIn(wrap, "正在发送验证码…");
     // 先记下发码前的账号数，再启动流程：登录完成后「多了一个」才算落盘成功。
     // 顺序不能反 —— 反过来读到的会是启动后的名册，基线就错了。
     fetchStatus()
        .then(function (d) {
          // 读不到就用 -1 标记基线未知（waitForLoginSettled 会退化成只认 done）
          flow.accountsBefore = d && d.accounts ? d.accounts.length : -1;
        })
        .then(function () { return startFlow(); })
        .then(function () { return submitInput(v); })
        // 手机号只是被投递给上游，真正发码在后台。给它一个确认窗口：
        // 上游若回「手机号码不合法」这类错误，这里就必须报失败，而不是说「已发送」。
        .then(function () { return waitForUpstreamSend(); })
        .then(function () {
          flow.smsSent = true;
          loginBtn.disabled = false;
          loginBtn.style.opacity = "1";
          loginBtn.style.cursor = "pointer";
          setMsgIn(wrap, "验证码已发送到 " + v.slice(0, 3) + "****" + v.slice(-4)
            + "。第 2 步：把短信里的验证码填进下面的框，点「登录」。", "ok");
          code.focus();
        })
        .catch(function (e) {
          setMsgIn(wrap, "发送失败：" + e.message, "bad");
          // 上游已失败 → 收回第 2 步放行，避免用注定失败的状态继续
          flow.smsSent = false;
          loginBtn.disabled = true;
          loginBtn.style.opacity = ".5";
          loginBtn.style.cursor = "not-allowed";
        })
        .then(function () { sendBtn.disabled = false; });
    });

    loginBtn.addEventListener("click", function () {
      var v = (code.value || "").trim();
      if (!v) { setMsgIn(wrap, "请填短信验证码。", "bad"); code.focus(); return; }
      loginBtn.disabled = true;
      setMsgIn(wrap, "正在登录，等上游确认…");
      submitInput(v)
        .then(function () { return waitForLoginSettled(flow.accountsBefore); })
        .then(function () {
          // 凭据已落盘 → 通知面板立刻重拉名册：新账号出现并标为「使用中」。
          // 早先的实现只能整页 reload，因为它在凭据落盘前就通知了 —— 名册还是旧的。
          setMsgIn(wrap, "登录成功，账号已加入账号池，正在刷新面板…", "ok");
          refreshNativeRoster();
        })
        .catch(function (e) {
          setMsgIn(wrap, "登录失败：" + e.message, "bad");
          loginBtn.disabled = false;
        });
    });

    openBtn.addEventListener("click", function () {
      var open = panel.style.display === "none";
      setOpen(open);
      if (open) setMode("sms");
    });
    smsBtn.addEventListener("click", function () { setMode("sms"); });
    oauthBtn.addEventListener("click", function () { setMode("oauth"); });
    phone.addEventListener("keydown", function (e) { if (e.key === "Enter") sendBtn.click(); });
    code.addEventListener("keydown", function (e) { if (e.key === "Enter" && !loginBtn.disabled) loginBtn.click(); });

    setMode("sms");
    setOpen(formOpen);
    return wrap;
  }

  /* ---------------- 挂载与状态同步 ---------------- */

  function accountsPanel() {
    var panels = document.querySelectorAll("[role=tabpanel]");
    for (var i = 0; i < panels.length; i++) {
      if (/AVAILABLE ACCOUNTS|可用账户/i.test(panels[i].innerText || "")) return panels[i];
    }
    return null;
  }

  function isAutoclawAccountsTab() {
    var panel = accountsPanel();
    if (!panel) return null;
    var sel = document.querySelector('[role=option][aria-selected="true"]');
    if (sel && (sel.textContent || "").toLowerCase().indexOf("autoclaw") === -1) return null;
    return panel;
  }

  /** 把面板 login 状态里的错误搬到我们表单里，用户只看这一处即可。 */
  function syncStatus() {
    if (!FORM) return;
    fetch("/api/oauth/status?provider=" + PROVIDER)
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (!d) return;
        var err = d.error ? String(d.error) : "";
        if (/cancelled|取消/i.test(err)) err = "";  // 用户主动取消，不算错误
        setNativeErrIn(FORM, err);
      })
      .catch(function () { /* 面板不可用时忽略 */ });
  }

  var lastSync = 0;
  /** 我们的入口该挂在哪：账号列表下面的原生操作行末尾（列表在上、添加入口在底部）。 */
  function mountPoint(panel) {
    var body = panel.querySelector(".pwi-auth-body") || panel;
    // ⚠️ 必须取 body 的**直接子级**操作行。.pwi-auth-actions 在状态行里还嵌套了一个同名 span
    // （里面是「退出登录」），用后代选择器会先命中它，把表单插到状态行内部、列表上方。
    var actions = body.querySelector(":scope > .pwi-auth-actions");
    return { parent: body, after: actions };
  }

  function ensureForm() {
    var panel = isAutoclawAccountsTab();
    if (!panel) {
      if (FORM && FORM.parentElement) FORM.parentElement.removeChild(FORM);
      FORM = null;
      hideNative(false);
      return;
    }
    hideNative(!nativeKeep);
    // React 重渲染会重建这一片 DOM，把我们的节点一起丢掉/挪走 —— 所以不仅验「还在文档里」，
    // 还要验「仍然是 body 的最后一个子节点」（即紧跟在账号列表下方）。任一不满足就重插。
    var mp = mountPoint(panel);
    if (FORM && FORM.parentElement === mp.parent && FORM === mp.parent.lastElementChild) return;
    if (!nativeKeep) hideNative(true);
    if (FORM && FORM.parentElement) FORM.parentElement.removeChild(FORM);
    FORM = buildForm();
    if (mp.after && mp.after.nextSibling) mp.parent.insertBefore(FORM, mp.after.nextSibling);
    else mp.parent.appendChild(FORM);
    syncStatus();
  }

  function syncFloat() {
    if (!FLOAT_BTN) return;
    var on = location.pathname.indexOf("/providers") === 0;
    FLOAT_BTN.style.display = on ? "block" : "none";
    if (!on) MODAL.style.display = "none";
  }

  function boot() {
    if (!document.body) { setTimeout(boot, 200); return; }
    buildFloatButton();
    syncFloat();
    window.addEventListener("popstate", syncFloat);
    setInterval(function () {
      syncFloat();
      try {
        ensureForm();
        var now = Date.now();
        if (FORM && now - lastSync > 4000) { lastSync = now; syncStatus(); }
      } catch (err) { /* 注入脚本绝不能影响面板本身 */ }
    }, 500);
  }

  boot();
})();
