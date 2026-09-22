/**
 * WorkBuddy (Tencent CodeBuddy) browser OAuth + token refresh — GLOBAL realm.
 *
 * 与 ./workbuddy.ts（CN realm）同协议、不同 host/Origin：
 *   CN     → https://copilot.tencent.com  （Origin/Referer https://www.codebuddy.cn）
 *   GLOBAL → https://www.workbuddy.ai     （Origin/Referer https://www.workbuddy.ai）
 *
 * 上游把 token/账号/chat 端点在两个域上同构部署（上游 workbuddy2api 的 login.go
 * 双域实现已实测：global 的 auth/state 200 正常签发授权链接）。差别只有三处常量：
 *   1. OAuth base
 *   2. Origin/Referer（请求头校验按域）
 *   3. 鉴权后的账号端点 token 同构
 *
 * chat 出站：global 固定走 {base}/v2/chat/completions（上游 #119 实测：/console 挂
 * 腾讯云 WAF 内容规则会 403，/v2 同 base 不挂规则）——与 registry 条目的 baseUrl 一致。
 *
 * 本文件由 integrations/opencodex/apply_global.sh 落到 $PKG/src/oauth/workbuddy-global.ts，
 * 并在 OAUTH_PROVIDERS 注册为 "workbuddy-global"。账号池/429 failover/token-guardian
 * 等能力随注册自动获得（与 workbuddy CN 相同）。
 */

import type { OAuthController, OAuthCredentials } from "./types";

const DEFAULT_OAUTH_BASE = "https://www.workbuddy.ai";
const ORIGIN_REFERER = "https://www.workbuddy.ai";
const CLIENT_UA = "CLI/2.63.2 CodeBuddy/2.63.2";

/** Refresh this long before the stated expiry so a request never rides an about-to-die token. */
const OAUTH_EXPIRY_SKEW_MS = 5 * 60 * 1000;
const POLL_INTERVAL_MS = 2_000;
/** 与 CN 侧一致的轮询上限（workbuddy2api login poll 的 5 分钟）。 */
const LOGIN_TIMEOUT_MS = 5 * 60 * 1000;
const FALLBACK_EXPIRES_IN_S = 3600;

interface ApiEnvelope<T> {
  code?: number;
  msg?: string;
  data?: T;
}

interface StateData {
  state?: string;
  authUrl?: string;
}

interface TokenData {
  accessToken?: string;
  refreshToken?: string;
  expiresIn?: number;
  domain?: string;
}

interface AccountData {
  uid?: string;
  enterpriseId?: string;
  nickname?: string;
}

interface EnvelopeResult<T> {
  status: number;
  code: number | undefined;
  msg: string | undefined;
  data: T | undefined;
}

/**
 * Refresh failure with the OAuth error code retained（与 CN 侧 WorkbuddyTokenError 同契约：
 * opencodex 的 terminal() 分类按 message 子串判 invalid_grant/revoked 等，
 * 瞬时失败不含这些词 → 保持可重试）。
 */
export class WorkbuddyGlobalTokenError extends Error {
  public readonly httpStatus: number | undefined;
  public readonly oauthError: string | undefined;

  constructor(message: string, options?: { cause?: unknown; httpStatus?: number; oauthError?: string }) {
    super(message, options?.cause !== undefined ? { cause: options.cause } : undefined);
    this.name = "WorkbuddyGlobalTokenError";
    this.httpStatus = options?.httpStatus;
    this.oauthError = options?.oauthError;
  }
}

function resolveOAuthBase(): string {
  const raw = process.env.WORKBUDDY_GLOBAL_OAUTH_BASE
    || process.env.WORKBUDDY_GLOBAL_BASE_URL
    || DEFAULT_OAUTH_BASE;
  return raw.trim().replace(/\/+$/, "");
}

function commonHeaders(): Record<string, string> {
  return {
    "Content-Type": "application/json",
    "Accept": "application/json, text/plain, */*",
    "X-Requested-With": "XMLHttpRequest",
    "Origin": ORIGIN_REFERER,
    "Referer": `${ORIGIN_REFERER}/`,
    "User-Agent": CLIENT_UA,
  };
}

function nonEmptyString(value: unknown): string | undefined {
  return typeof value === "string" && value.length > 0 ? value : undefined;
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
  init: { method: "GET" | "POST"; body?: string; headers?: Record<string, string>; signal?: AbortSignal },
): Promise<EnvelopeResult<T>> {
  const endpoint = path.split("?")[0];
  let response: Response;
  let text: string;
  try {
    response = await fetch(`${resolveOAuthBase()}${path}`, {
      method: init.method,
      headers: { ...commonHeaders(), ...(init.headers ?? {}) },
      ...(init.body !== undefined ? { body: init.body } : {}),
      ...(init.signal ? { signal: init.signal } : {}),
    });
    text = await response.text();
  } catch (cause) {
    // 与 CN 侧同理：transport 失败重抛为自有错误类，否则会被 opencodex 的
    // publicOAuthAuthenticationErrorMessage 压成通用文案，无法排查。
    if (cause instanceof WorkbuddyGlobalTokenError) throw cause;
    throw new WorkbuddyGlobalTokenError(
      `WorkBuddy-Global ${endpoint} request failed: ${cause instanceof Error ? cause.message : String(cause)}`,
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
    throw new WorkbuddyGlobalTokenError(
      `WorkBuddy-Global ${endpoint} returned a non-envelope response (HTTP ${response.status})`,
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
  return typeof msg === "string" && /invalid[_ ]?grant|refresh[_ ]?token[_ ]?reuse|revoked|expired/i.test(msg);
}

function credentialsFromTokenData(
  data: TokenData,
  refreshFallback: string | undefined,
  identity: { accountId?: string; email?: string },
): OAuthCredentials {
  const access = nonEmptyString(data.accessToken);
  if (!access) throw new WorkbuddyGlobalTokenError("WorkBuddy-Global token response contained no accessToken");
  const refresh = nonEmptyString(data.refreshToken) ?? refreshFallback;
  if (!refresh) throw new WorkbuddyGlobalTokenError("WorkBuddy-Global token response contained no refreshToken");
  const expiresInS = typeof data.expiresIn === "number" && Number.isFinite(data.expiresIn) && data.expiresIn > 0
    ? data.expiresIn
    : FALLBACK_EXPIRES_IN_S;
  const expires = Date.now() + expiresInS * 1000 - OAUTH_EXPIRY_SKEW_MS;
  if (!Number.isFinite(expires)) {
    throw new WorkbuddyGlobalTokenError("WorkBuddy-Global token response had an unusable expiresIn");
  }
  return {
    access,
    refresh,
    expires,
    ...(identity.accountId ? { accountId: identity.accountId } : {}),
    ...(identity.email ? { email: identity.email } : {}),
    source: "oauth",
  };
}

export async function loginWorkbuddyGlobal(ctrl: OAuthController): Promise<OAuthCredentials> {
  const state = await callEnvelope<StateData>("/v2/plugin/auth/state?platform=CLI", {
    method: "POST",
    body: "{}",
    ...(ctrl.signal ? { signal: ctrl.signal } : {}),
  });
  if (state.code !== 0 || !state.data) {
    throw new WorkbuddyGlobalTokenError(
      `WorkBuddy-Global auth state request failed: ${state.msg ?? `code ${state.code}`}`,
      { httpStatus: state.status },
    );
  }
  const stateToken = nonEmptyString(state.data.state);
  const authUrl = nonEmptyString(state.data.authUrl);
  if (!stateToken || !authUrl) {
    throw new WorkbuddyGlobalTokenError("WorkBuddy-Global auth state response was missing state or authUrl");
  }

  ctrl.onAuth?.({
    url: authUrl,
    instructions: "Authorize WorkBuddy (Global) in the browser. This window keeps polling until it completes.",
  });

  const deadline = Date.now() + LOGIN_TIMEOUT_MS;
  let announced = Date.now();
  let lastMsg: string | undefined;
  for (;;) {
    if (ctrl.signal?.aborted) throw new Error("Login cancelled");
    const poll = await callEnvelope<TokenData>(
      `/v2/plugin/auth/token?state=${encodeURIComponent(stateToken)}`,
      { method: "GET", ...(ctrl.signal ? { signal: ctrl.signal } : {}) },
    );
    if (poll.code === 0 && poll.data?.accessToken) {
      const identity = await fetchAccountIdentity(stateToken, poll.data.accessToken);
      return credentialsFromTokenData(poll.data, undefined, identity);
    }
    lastMsg = poll.msg ?? lastMsg;
    if (Date.now() >= deadline) {
      throw new WorkbuddyGlobalTokenError(
        `WorkBuddy-Global login timed out after ${Math.round(LOGIN_TIMEOUT_MS / 1000)}s`
        + `${lastMsg ? ` (last upstream status: ${lastMsg})` : ""}`
        + " — re-run `ocx login workbuddy-global` and finish the browser authorization promptly.",
      );
    }
    if (Date.now() - announced >= 10_000) {
      announced = Date.now();
      ctrl.onProgress?.(`still waiting for the browser authorization… ${lastMsg ?? ""}`.trim());
    }
    await sleep(POLL_INTERVAL_MS, ctrl.signal);
  }
}

async function fetchAccountIdentity(
  stateToken: string,
  accessToken: string,
): Promise<{ accountId?: string; email?: string }> {
  try {
    const account = await callEnvelope<AccountData>(
      `/v2/plugin/login/account?state=${encodeURIComponent(stateToken)}`,
      { method: "GET", headers: { "Authorization": `Bearer ${accessToken}` } },
    );
    const accountId = nonEmptyString(account.data?.uid);
    const label = nonEmptyString(account.data?.nickname) ?? accountId;
    return {
      ...(accountId ? { accountId } : {}),
      ...(label ? { email: label } : {}),
    };
  } catch {
    return {};
  }
}

export async function refreshWorkbuddyGlobalToken(
  refreshToken: string,
  signal?: AbortSignal,
): Promise<OAuthCredentials> {
  const result = await callEnvelope<TokenData>("/v2/plugin/auth/token/refresh", {
    method: "POST",
    body: "{}",
    headers: {
      "X-Refresh-Token": refreshToken,
      "X-Auth-Refresh-Source": "workbuddy",
    },
    ...(signal ? { signal } : {}),
  });
  if (result.code !== 0 || !result.data) {
    const dead = isDeadGrant(result.status, result.msg);
    throw new WorkbuddyGlobalTokenError(
      `WorkBuddy-Global token refresh failed (HTTP ${result.status}): ${result.msg ?? `code ${result.code}`}`
      + (dead ? " [invalid_grant]" : ""),
      { httpStatus: result.status, oauthError: dead ? "invalid_grant" : undefined },
    );
  }
  return credentialsFromTokenData(result.data, refreshToken, {});
}
