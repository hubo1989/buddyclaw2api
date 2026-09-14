/**
 * AutoClaw (智谱澳龙) browser OAuth + token refresh.
 *
 * Mirrors `workbuddy.ts`: registering this module in `OAUTH_PROVIDERS` gives autoclaw the full
 * built-in account experience — `ocx login autoclaw`, the multi-account pool with 429 failover,
 * background token renewal, and the management-API account tab in the dashboard.
 *
 * Wire contract (reverse-engineered from AutoClaw.app 1.18.1 + verified against the live service,
 * see workbuddy2api docs/specs/2026-09-11-autoclaw-provider.md):
 *
 *   Login (phone SMS — the CN client's native flow; there is no browser-consent page):
 *     1. POST {base}/userapi/v1/agent-send-code   {source_id, device_id, phone}
 *     2. POST {base}/userapi/v1/agent-login/      {source_id, device_id, phone, code}
 *          -> {code:0, data:{access_token, refresh_token, user_id, user_name, ...}}
 *     (The interactive prompt asks for phone + code; `ocx login autoclaw` drives this over stdin.)
 *
 *   Login (oversea web OAuth — google / zai, only reachable from allowed networks):
 *     1. POST {base}/userapi/overseasv1/{vendor}-oauth-url   {source_id, device_id, navigate_uri}
 *     2. POST {base}/userapi/overseasv1/{vendor}-oauth-login {source_id, device_id, code, state, navigate_uri}
 *          -> same token payload as agent-login
 *
 *   Refresh:
 *     POST {base}/userapi/v1/refresh   {source_id, device_id, refresh_token}
 *       -> {code:0, data:{access_token, refresh_token, expires_in}}
 *       On business code 400002 (signature check) the client falls back to /userapi/v1/agent-refresh —
 *       we mirror that fallback.
 *     The refresh token ROTATES on every call → `defaultRefreshPolicy: "lazy-only"`.
 *
 *   Business headers: every userapi call carries md5(appid & timestamp & APP_KEY) as X-Auth-Sign
 *   (APP_ID="100003", APP_KEY is the client-bundled public constant). Chat goes through a
 *   different surface (X-Authorization, no signature) and is handled by the adapter, not here.
 */

import type { OAuthController, OAuthCredentials } from "./types";

const DEFAULT_OAUTH_BASE = "https://autoglm-api.autoglm.ai";
/** 国内 host：手机号/短信账号（agent*_token）只在这里认。与 autoclaw-quota.ts 的 QUOTA_HOSTS 一致。 */
const CN_OAUTH_BASE = "https://autoglm-api.zhipuai.cn";
const APP_ID = "100003";
const APP_KEY = "38d2391985e2369a5fb8227d8e6cd5e5";
const PRODUCT = "autoclaw";
const CHANNEL = "AutoClaw4";
const CLIENT_VERSION = "1.18.1";
const PLATFORM_TM = process.platform === "darwin" ? "mac" : process.platform === "win32" ? "win" : "linux";

const OAUTH_EXPIRY_SKEW_MS = 10 * 60 * 1000;
const FALLBACK_EXPIRES_IN_S = 5 * 3600;
/** 账号管理页：支持手机验证码与滑块验证登录，作为面板登录的落地页。 */
const ACCOUNTS_PAGE_URL = "http://127.0.0.1:10100/autoclaw-accounts.html";

interface ApiEnvelope<T> {
  code?: number;
  msg?: string;
  data?: T;
}

interface TokenData {
  access_token?: string;
  refresh_token?: string;
  expires_in?: number;
  user_id?: string;
  user_name?: string;
}

interface EnvelopeResult<T> {
  status: number;
  code: number | undefined;
  msg: string | undefined;
  data: T | undefined;
}

export class AutoclawTokenError extends Error {
  public readonly httpStatus: number | undefined;
  public readonly oauthError: string | undefined;

  constructor(message: string, options?: { cause?: unknown; httpStatus?: number; oauthError?: string }) {
    super(message, options?.cause !== undefined ? { cause: options.cause } : undefined);
    this.name = "AutoclawTokenError";
    this.httpStatus = options?.httpStatus;
    this.oauthError = options?.oauthError;
  }
}

function resolveOAuthBase(): string {
  const raw = process.env.AUTOCLAW_OAUTH_BASE
    || process.env.AUTOCLAW_BASE_URL
    || DEFAULT_OAUTH_BASE;
  return raw.trim().replace(/\/+$/, "");
}

function md5Hex(input: string): string {
  // Bun exposes node:crypto; keep the import inline so the module stays dependency-free otherwise.
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  const { createHash } = require("node:crypto") as typeof import("node:crypto");
  return createHash("md5").update(input).digest("hex");
}

function commonHeaders(): Record<string, string> {
  const ts = String(Math.floor(Date.now() / 1000));
  return {
    "Content-Type": "application/json",
    "Accept": "*/*",
    // WAF gate: requests without a client UA are 405-rejected before reaching the API.
    "User-Agent": `AutoClaw/${CLIENT_VERSION} (${PLATFORM_TM}-arm64)`,
    "X-Version": CLIENT_VERSION,
    "X-Tm": PLATFORM_TM,
    "X-Product": PRODUCT,
    "X-Auth-Appid": APP_ID,
    "X-Auth-TimeStamp": ts,
    "X-Auth-Sign": md5Hex(`${APP_ID}&${ts}&${APP_KEY}`),
    "X-Trace-Id": crypto.randomUUID(),
    "X-Lang": "zh-CN",
    "X-Channel": CHANNEL,
    "Origin": "http://localhost:18432",
    "Referer": "http://localhost:18432/",
  };
}

function nonEmptyString(value: unknown): string | undefined {
  return typeof value === "string" && value.length > 0 ? value : undefined;
}

/** 去重并保持顺序（host 回退列表用）。 */
function dedupe(values: string[]): string[] {
  return [...new Set(values.filter(Boolean))];
}

/** 读 AutoClaw token（JWT）payload 里的某个字段；解析失败返回 undefined。
 *  payload 是明文 base64url，不需要校验签名 —— 我们只用它取自己账号的 device_id / exp。 */
function jwtClaim(token: string, key: string): unknown {
  const raw = (token ?? "").replace(/^Bearer\s+/i, "").trim();
  const part = raw.split(".")[1];
  if (!part) return undefined;
  try {
    const json = Buffer.from(part.replace(/-/g, "+").replace(/_/g, "/"), "base64").toString("utf8");
    return (JSON.parse(json) as Record<string, unknown>)[key];
  } catch {
    return undefined;
  }
}

/** token 的真实过期时间（epoch ms）；取不到时 undefined。
 *  上游登录/refresh 响应里的 expires_in 常缺省，老代码一律按 5h 记 —— 而 JWT 的 exp 才是事实，
 *  记短了会让额度探测每次都误判「已过期」并触发一次 refresh（refresh token 轮换，有吊销风险）。 */
function jwtExpiresAtMs(token: string): number | undefined {
  const exp = jwtClaim(token, "exp");
  return typeof exp === "number" && Number.isFinite(exp) && exp > 0 ? exp * 1000 : undefined;
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new Error("Login cancelled"));
      return;
    }
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener("abort", () => {
      clearTimeout(timer);
      reject(new Error("Login cancelled"));
    }, { once: true });
  });
}

async function callEnvelope<T>(
  path: string,
  init: { method: "GET" | "POST"; body?: string; signal?: AbortSignal; base?: string },
): Promise<EnvelopeResult<T>> {
  const endpoint = path.split("?")[0];
  let response: Response;
  let text: string;
  try {
    response = await fetch(`${init.base ?? resolveOAuthBase()}${path}`, {
      method: init.method,
      headers: { ...commonHeaders() },
      ...(init.body !== undefined ? { body: init.body } : {}),
      ...(init.signal ? { signal: init.signal } : {}),
    });
    text = await response.text();
  } catch (cause) {
    if (cause instanceof AutoclawTokenError) throw cause;
    throw new AutoclawTokenError(
      `AutoClaw ${endpoint} request failed: ${cause instanceof Error ? cause.message : String(cause)}`,
      { cause },
    );
  }
  let parsed: ApiEnvelope<T> | undefined;
  try {
    parsed = text ? JSON.parse(text) as ApiEnvelope<T> : undefined;
  } catch {
    parsed = undefined;
  }
  if (!parsed || (parsed.code === undefined && response.status >= 400)) {
    throw new AutoclawTokenError(
      `AutoClaw ${endpoint} returned a non-envelope response (HTTP ${response.status})`,
      { httpStatus: response.status },
    );
  }
  return {
    status: response.status,
    code: typeof parsed.code === "number" ? parsed.code : undefined,
    msg: nonEmptyString(parsed.msg),
    data: parsed.data,
  };
}

function isDeadGrant(status: number, msg: string | undefined): boolean {
  if (status === 401 || status === 403) return true;
  return typeof msg === "string" && /invalid[_ ]?grant|refresh[_ ]?token[_ ]?reuse|revoked|expired|登录已失效|token 失效/i.test(msg);
}

function credentialsFromTokenData(
  data: TokenData,
  refreshFallback: string | undefined,
  identity: { accountId?: string; email?: string },
): OAuthCredentials {
  const access = nonEmptyString(data.access_token);
  if (!access) throw new AutoclawTokenError("AutoClaw token response contained no access_token");
  const refresh = nonEmptyString(data.refresh_token) ?? refreshFallback;
  if (!refresh) throw new AutoclawTokenError("AutoClaw token response contained no refresh_token");
  // 过期时间优先级：响应里的 expires_in（一般缺省）> access token 的 JWT exp（事实来源）> 5h 保守兜底。
  // 老代码缺省就按 5h 记，而 AutoClaw 的 JWT 实际 24h —— 记短 19h 会让额度探测每次都误判「已过期」，
  // 于是每次刷新额度都白跑一次 refresh（refresh token 轮换，高频有吊销风险）。
  const jwtExpires = jwtExpiresAtMs(access);
  const expiresInS = typeof data.expires_in === "number" && Number.isFinite(data.expires_in) && data.expires_in > 0
    ? data.expires_in
    : undefined;
  const expires = jwtExpires !== undefined
    ? jwtExpires - OAUTH_EXPIRY_SKEW_MS
    : Date.now() + (expiresInS ?? FALLBACK_EXPIRES_IN_S) * 1000 - OAUTH_EXPIRY_SKEW_MS;
  return {
    access,
    refresh,
    expires,
    ...(identity.accountId ? { accountId: identity.accountId } : {}),
    ...(identity.email ? { email: identity.email } : {}),
    source: "oauth",
  };
}

function interactiveLine(prompt: string): string {
  const line = promptLineSync(prompt);
  const trimmed = line.trim();
  if (!trimmed) throw new AutoclawTokenError(`AutoClaw login requires ${prompt.trim()}`);
  return trimmed;
}

/** Minimal synchronous stdin reader (the login CLI runs before any async IO races matter). */
function promptLineSync(prompt: string): string {
  process.stdout.write(prompt);
  const buf = new Uint8Array(1024);
  const n = require("node:fs").readSync(0, buf) as number;
  return new TextDecoder().decode(buf.subarray(0, n));
}

/**
 * Phone-SMS login (the CN client's native flow).
 *
 * Two transports:
 *   - CLI (TTY): interactive prompts — phone, then SMS code.
 *   - GUI (no TTY): two paste rounds through `ctrl.onManualCodeInput`
 *     (round 1: phone number → triggers the SMS; round 2: the received code),
 *     mirroring the re-prompt loop pattern from command-code.ts. This keeps the
 *     dashboard login button fully self-serve — oversea web-OAuth is server-disabled
 *     (631002) for this appid, so phone SMS is the only working path.
 */
async function loginAutoclawPhone(ctrl: OAuthController, presetPhone?: string): Promise<OAuthCredentials> {
  const deviceId = crypto.randomUUID().replace(/-/g, "").repeat(2).slice(0, 64);
  let phone: string;
  let smsCode: string;
  if (process.stdin.isTTY) {
    phone = interactiveLine("AutoClaw 手机号: ");
    const sendRes = await callEnvelope<{ result?: boolean }>(
      "/userapi/v1/agent-send-code",
      { method: "POST", body: JSON.stringify({ source_id: PRODUCT, device_id: deviceId, phone }) },
    );
    if (sendRes.code !== 0) {
      throw new AutoclawTokenError(
        `AutoClaw send-code failed: ${sendRes.msg ?? `code ${sendRes.code}`}`,
        { httpStatus: sendRes.status },
      );
    }
    ctrl.onProgress?.("验证码已发送到手机短信，请在终端输入收到的验证码");
    smsCode = interactiveLine("短信验证码: ");
  } else {
    // GUI: two paste rounds through the dashboard's manual-input dialog.
    //
    // The dashboard's async login contract ONLY resolves its HTTP request when `onAuth` fires
    // (see startLoginFlow: onAuth resolves, onProgress is deliberately a no-op). A flow that
    // never calls onAuth leaves the login request pending forever — which is exactly the
    // "spinner never finishes" symptom, later reported as "Login cancelled" when the abort
    // signal reaches us. So: fire onAuth up front with a clickable page + instructions
    // (instructions is the only text the UI surfaces), then collect phone/code by paste.
    // `onAuth` must fire (it is the only thing that resolves the dashboard's login request), but
    // we deliberately pass an EMPTY url: the dashboard auto-opens whatever url we return, and for
    // the SMS flow there is nothing to open in a browser — everything happens in the paste dialog.
    if (!presetPhone) {
      ctrl.onAuth?.({
        url: "",
        instructions: "AutoClaw 登录（两步，全程在本面板完成）：① 在下方输入框输入手机号（11 位，不带 +86）并提交 → 会收到短信验证码；"
          + "② 再在输入框输入短信里的验证码即可完成。",
      });
    }
    phone = (presetPhone ?? (await guiPrompt(ctrl, "手机号")))?.trim() ?? "";
    if (!phone) throw new AutoclawTokenError("手机号不能为空");

    // 经 7863 走两步：它统一维护发码/登录的 device_id 一致性，并用 no_persist 把凭据
    // 交还 opencodex 自己的账号池（不写入 7863 池）。
    if (proxyBase()) {
      const send = await proxyPost("/admin/autoclaw/login/send-code", { phone });
      if (!send.ok) {
        throw new AutoclawTokenError(
          `发送验证码失败: ${send.body?.error ?? `HTTP ${send.status}`}`,
        );
      }
      smsCode = ((await guiPrompt(ctrl, "短信验证码")) ?? "").trim();
      if (!smsCode) throw new AutoclawTokenError("验证码不能为空");
      const fin = await proxyPost("/admin/autoclaw/login/verify", {
        phone, code: smsCode, no_persist: true,
      });
      if (!fin.ok || !fin.body?.access_token) {
        throw new AutoclawTokenError(
          `AutoClaw 登录失败: ${fin.body?.error ?? `HTTP ${fin.status}`}`,
        );
      }
      return credsFromProxyToken(fin.body, "phone");
    }
    const sendRes = await callEnvelope<{ result?: boolean }>(
      "/userapi/v1/agent-send-code",
      { method: "POST", body: JSON.stringify({ source_id: PRODUCT, device_id: deviceId, phone }) },
    );
    if (sendRes.code !== 0) {
      throw new AutoclawTokenError(
        `AutoClaw send-code failed: ${sendRes.msg ?? `code ${sendRes.code}`}`,
        { httpStatus: sendRes.status },
      );
    }
    ctrl.onProgress?.("验证码已发送到手机短信，请在输入框中输入收到的验证码");
    smsCode = await guiPrompt(ctrl, "请输入收到的短信验证码:");
    if (!smsCode) throw new AutoclawTokenError("验证码不能为空");
  }
  return finishLogin(
    "/userapi/v1/agent-login/",
    { source_id: PRODUCT, device_id: deviceId, phone, code: codeValue(smsCode) },
    deviceId,
  );
}

/**
 * 验证码必须按数字发送：官方客户端的 normalizeCode 会把验证码 `Number(...)` 后再入 body，
 * 服务端按类型校验，发字符串会得到 400001「请求数据有问题」。
 */
function codeValue(code: string): string | number {
  const s = (code ?? "").trim();
  return /^\d+$/.test(s) ? Number(s) : s;
}

/** GUI paste prompt via onManualCodeInput; empty input re-prompts (bounded to 5 rounds). */
async function guiPrompt(ctrl: OAuthController, message: string): Promise<string> {
  for (let i = 0; i < 5; i++) {
    if (ctrl.signal?.aborted) throw new Error("Login cancelled");
    const pasted = await ctrl.onManualCodeInput?.();
    const value = (pasted ?? "").trim();
    if (value) return value;
  }
  return "";
}

/** 7863 管理 API 基址：守护进程无法渲染滑块，借助它缓存的回执完成 oversea OAuth。 */
function proxyBase(): string {
  const raw = (process.env.AUTOCLAW_PROXY_BASE ?? "http://127.0.0.1:7863").trim();
  if (!raw || raw === "0" || raw === "off") return "";
  return raw.replace(/\/+$/, "");
}

async function proxyPost(
  path: string,
  body: Record<string, unknown>,
): Promise<{ ok: boolean; status: number; body: any }> {
  const res = await fetch(`${proxyBase()}${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const json = await res.json().catch(() => ({}));
  return { ok: res.ok, status: res.status, body: json };
}

/** 7863 no_persist 返回的裸 token → OAuthCredentials。 */
function credsFromProxyToken(d: any, vendor: string): OAuthCredentials {
  const access = nonEmptyString(d?.access_token);
  if (!access) throw new AutoclawTokenError("AutoClaw token response contained no access_token");
  const refresh = nonEmptyString(d?.refresh_token);
  if (!refresh) throw new AutoclawTokenError("AutoClaw token response contained no refresh_token");
  // 过期时间以 access token 的 JWT exp 为准：7863 的 /admin/autoclaw/login/oauth-finish 回的是
  // 硬编码 now+5h，而 JWT 实际 24h。用 5h 会让每个新账号一登录就带着「少 19 小时」的过期时间
  // 落库 → 额度探测每次都误判已过期、每次刷新都白跑一遍 refresh。
  const jwtExpires = jwtExpiresAtMs(access);
  let expiresIn = FALLBACK_EXPIRES_IN_S;
  if (typeof d?.expires_at === "number" && d.expires_at > 0) {
    expiresIn = Math.max(60, Math.floor(d.expires_at - Date.now() / 1000));
  }
  const uid = nonEmptyString(d?.uid) ?? `${vendor}-${Date.now()}`;
  const email = nonEmptyString(d?.nickname) ?? uid;
  return {
    access,
    refresh,
    expires: jwtExpires !== undefined
      ? jwtExpires - OAUTH_EXPIRY_SKEW_MS
      : Date.now() + expiresIn * 1000 - OAUTH_EXPIRY_SKEW_MS,
    accountId: uid,
    email,
    source: "oauth",
  };
}

/**
 * Oversea web-OAuth login (google / zai), routed through the local workbuddy2api admin API.
 *
 * Why the proxy: upstream `{vendor}-oauth-url` REQUIRES an Aliyun slider-captcha receipt
 * (`ali_captcha_verify_param`) — without one it answers 631002 "version no longer supported".
 * The daemon has no browser and cannot solve a slider, so workbuddy2api caches the receipt after
 * the user drags it once in the account-management page and replays it here. When nothing is
 * cached the proxy answers 428 need_captcha; we then surface the preparation page through
 * `onAuth` — the only URL/text channel the dashboard actually renders.
 *
 * Two input transports, chosen by environment:
 *   - GUI (no TTY): `ctrl.onAuth` hands the authorize URL to the dashboard, and the final
 *     `code` arrives via `ctrl.onManualCodeInput` — the same paste channel other providers use.
 *     No stdin is touched, so this works inside the launchd daemon.
 *   - CLI (TTY): prints the URL, reads code/state from stdin.
 */
async function loginAutoclawOAuth(vendor: "google" | "zai", ctrl: OAuthController): Promise<OAuthCredentials> {
  if (proxyBase()) {
    const urlRes = await proxyPost("/admin/autoclaw/login/oauth-url", { vendor });
    if (urlRes.status === 428 && urlRes.body?.need_captcha) {
      const page = nonEmptyString(urlRes.body.captcha_page) ?? `${ACCOUNTS_PAGE_URL}#oauth-${vendor}`;
      ctrl.onAuth?.({
        url: page,
        instructions: `AutoClaw ${vendor} 登录需要先过一次滑块验证（守护进程没有浏览器，无法自己拖）。`
          + `请打开此链接 → 在「添加账号 · 网页 OAuth」处选 ${vendor}，勾选「仅为 opencodex 面板准备」→ 拖动滑块 →`
          + `看到“已就绪”后回到面板重新登录并选择 ${vendor}。回执有效期 15 分钟。`,
      });
      throw new AutoclawTokenError(
        `AutoClaw ${vendor} 登录需要滑块验证回执：请点右下角「AutoClaw 账号管理」→`
        + `「添加账号 · 网页 OAuth」选 ${vendor} → 拖动滑块 → 回到这里重试。`,
      );
    }
    if (urlRes.ok && nonEmptyString(urlRes.body?.authorizeURL)) {
      const authorizeUrl = String(urlRes.body.authorizeURL);
      const deviceId = nonEmptyString(urlRes.body.device_id) ?? "";
      const redirectUri = nonEmptyString(urlRes.body.redirect_uri) ?? "";
      let pasted: string | undefined;
      if (!process.stdin.isTTY) {
        // 面板的登录请求在首次 onAuth 时就已返回，这里的 authorizeUrl 没有展示通道 ——
        // 放到 7863 中转，注入面板的表单会轮询取回并打开授权页。
        if (proxyBase()) {
          await proxyPost("/admin/autoclaw/login/oauth-link", {
            vendor, url: authorizeUrl, device_id: deviceId,
          }).catch(() => undefined);
        }
        pasted = await guiPrompt(ctrl, `粘贴 ${vendor} 跳转地址中的 code（或完整地址）:`);
      } else {
        ctrl.onAuth?.({
          url: authorizeUrl,
          instructions: `在浏览器打开授权页；完成后从跳转地址复制 code（和 state）。此窗口在等待输入。`,
        });
        pasted = interactiveLine("粘贴回调地址中的 code 或完整跳转地址: ");
      }
      const code = extractCode(pasted);
      if (!code) throw new AutoclawTokenError("未能从输入中解析出 code — 请粘贴包含 code= 的完整跳转地址");
      const state = extractState(pasted) ?? "";
      const fin = await proxyPost("/admin/autoclaw/login/oauth-finish", {
        vendor, code, state, device_id: deviceId, redirect_uri: redirectUri, no_persist: true,
      });
      if (!fin.ok || !fin.body?.access_token) {
        throw new AutoclawTokenError(`AutoClaw ${vendor} 登录失败: ${fin.body?.error ?? `HTTP ${fin.status}`}`);
      }
      return credsFromProxyToken(fin.body, vendor);
    }
    // 代理返回了其他错误（409 会话占用 / 502 上游拒绝等）：原样带给用户。
    // 绝不回退直连 —— 直连必然得到误导性的 631002「版本已停止服务」，会让人以为这条路走不通。
    throw new AutoclawTokenError(
      `AutoClaw ${vendor} 登录未启动：${nonEmptyString(urlRes.body?.error) ?? `HTTP ${urlRes.status}`}`,
    );
  }

  // Fallback: 仅在未配置代理（AUTOCLAW_PROXY_BASE=off）时直连上游。
  const deviceId = crypto.randomUUID().replace(/-/g, "").repeat(2).slice(0, 64);
  const navigateUri = `http://localhost:18432/auth/callback-${vendor}`;
  const urlRes = await callEnvelope<{ url?: string; login_url?: string; auth_url?: string }>(
    `/userapi/overseasv1/${vendor}-oauth-url`,
    { method: "POST", body: JSON.stringify({ source_id: PRODUCT, device_id: deviceId, navigate_uri: navigateUri }) },
  );
  if (urlRes.code !== 0 || !urlRes.data) {
    throw new AutoclawTokenError(
      `AutoClaw ${vendor} oauth-url failed: ${urlRes.msg ?? `code ${urlRes.code}`}`
      + "（若为 631002/631000，说明当前网络对该 host 未开放 oversea OAuth；可设 AUTOCLAW_OAUTH_BASE 换端点）",
      { httpStatus: urlRes.status },
    );
  }
  const authorizeUrl = urlRes.data.url ?? urlRes.data.login_url ?? urlRes.data.auth_url ?? "";
  if (!authorizeUrl) {
    throw new AutoclawTokenError(`AutoClaw ${vendor} oauth-url response contained no url`);
  }
  const isGui = !process.stdin.isTTY;
  if (isGui) {
    // GUI path: hand the URL to the dashboard, then wait for the pasted code via the
    // standard manual-input channel (the panel prompts the user and POSTs it).
    ctrl.onAuth?.({
      url: authorizeUrl,
      instructions: `在浏览器打开授权页完成 ${vendor} 登录；然后把跳转地址中的 code 粘贴到面板输入框。`,
    });
    ctrl.onProgress?.(`等待 ${vendor} 授权 code（从浏览器跳转地址复制）…`);
    const pasted = await ctrl.onManualCodeInput?.();
    const code = extractCode(pasted);
    if (!code) {
      throw new AutoclawTokenError("未能从粘贴内容中解析出 code — 请粘贴包含 code= 的完整跳转地址");
    }
    const state = extractState(pasted) ?? "";
    return finishLogin(
      `/userapi/overseasv1/${vendor}-oauth-login`,
      { source_id: PRODUCT, device_id: deviceId, code, state, navigate_uri: navigateUri },
      deviceId,
    );
  }
  // CLI path: print + stdin
  ctrl.onAuth?.({
    url: authorizeUrl,
    instructions: `在浏览器打开授权页；完成后从跳转地址复制 code（和 state）。此窗口在等待输入。`,
  });
  const code = interactiveLine("粘贴回调地址中的 code: ");
  const state = promptLineSync("粘贴 state（可留空直接回车）: ").trim();
  return finishLogin(
    `/userapi/overseasv1/${vendor}-oauth-login`,
    { source_id: PRODUCT, device_id: deviceId, code, state, navigate_uri: navigateUri },
    deviceId,
  );
}

/** Pull `code` / `state` query params out of a pasted URL (or a bare code). */
function extractCode(pasted: string | undefined): string | undefined {
  if (!pasted) return undefined;
  const trimmed = pasted.trim();
  if (!trimmed.includes("?")) return trimmed || undefined; // bare code
  try {
    const q = new URL(trimmed).searchParams;
    return q.get("code") ?? undefined;
  } catch {
    const m = trimmed.match(/code=([^&]+)/);
    return m ? decodeURIComponent(m[1]) : undefined;
  }
}

function extractState(pasted: string | undefined): string | undefined {
  if (!pasted || !pasted.includes("?")) return undefined;
  try {
    return new URL(pasted).searchParams.get("state") ?? undefined;
  } catch {
    const m = pasted.match(/state=([^&]+)/);
    return m ? decodeURIComponent(m[1]) : undefined;
  }
}

async function finishLogin(
  path: string,
  body: Record<string, unknown>,
  deviceId: string,
): Promise<OAuthCredentials> {
  const res = await callEnvelope<TokenData>(path, { method: "POST", body: JSON.stringify(body) });
  if (res.code !== 0 || !res.data) {
    throw new AutoclawTokenError(
      `AutoClaw login failed: ${res.msg ?? `code ${res.code}`}`,
      { httpStatus: res.status },
    );
  }
  const identity = {
    accountId: nonEmptyString(res.data.user_id),
    email: nonEmptyString(res.data.user_name) ?? nonEmptyString(res.data.user_id),
  };
  const creds = credentialsFromTokenData(res.data, undefined, identity);
  void deviceId;
  return creds;
}

export async function loginAutoclaw(ctrl: OAuthController): Promise<OAuthCredentials> {
  // CLI: the full menu — phone SMS, Google OAuth, Z.ai OAuth.
  if (process.stdin.isTTY) {
    process.stdout.write("AutoClaw 登录方式: 1) 手机验证码  2) Google OAuth  3) Z.ai OAuth — 选择 [1/2/3，默认 1]: ");
    const choice = promptLineSync("").trim();
    if (choice === "2") return loginAutoclawOAuth("google", ctrl);
    if (choice === "3") return loginAutoclawOAuth("zai", ctrl);
    return loginAutoclawPhone(ctrl);
  }
  // GUI: `onAuth` must fire first (it is the only thing that resolves the dashboard's login
  // request — see startLoginFlow), but the url is left EMPTY on purpose: the dashboard
  // auto-opens whatever we return, and neither flow should yank the user out of the panel.
  // The paste dialog doubles as the mode picker: a phone number starts the SMS flow,
  // "zai" / "google" starts oversea web-OAuth (which opens the authorize page itself).
  ctrl.onAuth?.({
    url: "",
    instructions: "AutoClaw 登录（在本面板输入框完成）：① 输入手机号（11 位，不带 +86）并提交 → 收到短信验证码；"
      + "② 再输入短信里的验证码即完成。"
      + "要用 z.ai / Google 网页登录：输入 zai 或 google（会弹出授权页；首次需先在「AutoClaw 账号管理」里拖一次滑块）。",
  });
  const first = ((await guiPrompt(ctrl, "手机号，或输入 zai / google 选择网页登录")) ?? "").trim();
  const pick = first.toLowerCase();
  if (pick === "zai" || pick === "z.ai" || pick === "3") return loginAutoclawOAuth("zai", ctrl);
  if (pick === "google" || pick === "2") return loginAutoclawOAuth("google", ctrl);
  return loginAutoclawPhone(ctrl, first);
}

/**
 * Exchange a refresh token for a fresh pair. The upstream ROTATES the refresh token; the returned
 * value is always preferred. Business code 400002 on /refresh mirrors the client's fallback to
 * /agent-refresh.
 */
export async function refreshAutoclawToken(
  refreshToken: string,
  signal?: AbortSignal,
): Promise<OAuthCredentials> {
  // device_id 必填：传空串上游会答 400001「请求数据有问题」，refresh 永远失败
  //  → 过期账号拿不到新 token → 额度探测报「额度刷新失败」。
  // 它就在 refresh token 自己的 JWT payload 里，直接读出来回填。
  const deviceId = nonEmptyString(jwtClaim(refreshToken, "device_id")) ?? "";
  const body = JSON.stringify({ source_id: PRODUCT, device_id: deviceId, refresh_token: refreshToken });
  // host 要与账号来源匹配：手机号账号（agent*_token）只在国内 host 有效，
  // 网页 OAuth 账号（autoclaw*_token）只在国际 host 有效 —— 打错会得到 400000「用户未登录」。
  // 光靠 source_id 判断不可靠（老账号可能缺字段），所以按「默认 host 优先、换 host 兜底」逐个试。
  const isSmsAccount = /^agent/i.test(String(jwtClaim(refreshToken, "source_id") ?? ""));
  const hosts = isSmsAccount
    ? [CN_OAUTH_BASE, resolveOAuthBase()]
    : [resolveOAuthBase(), CN_OAUTH_BASE];
  let result: EnvelopeResult<TokenData> | null = null;
  for (const base of dedupe([...hosts, resolveOAuthBase()])) {
    result = await callEnvelope<TokenData>("/userapi/v1/refresh", {
      method: "POST", body, base, ...(signal ? { signal } : {}),
    });
    if (result.code !== 0 && result.code === 400002) {
      result = await callEnvelope<TokenData>("/userapi/v1/agent-refresh", {
        method: "POST", body, base, ...(signal ? { signal } : {}),
      });
    }
    // 400000 = 这个 host 不认该账号；换下一个 host。其余（含成功）就地判定。
    if (result.code !== 400000) break;
  }
  if (!result) throw new AutoclawTokenError("AutoClaw token refresh failed: no host attempted");
  if (result.code !== 0 || !result.data) {
    const dead = isDeadGrant(result.status, result.msg);
    throw new AutoclawTokenError(
      `AutoClaw token refresh failed (HTTP ${result.status}): ${result.msg ?? `code ${result.code}`}`
      + (dead ? " [invalid_grant]" : ""),
      { httpStatus: result.status, oauthError: dead ? "invalid_grant" : undefined },
    );
  }
  return credentialsFromTokenData(result.data, refreshToken, {});
}
