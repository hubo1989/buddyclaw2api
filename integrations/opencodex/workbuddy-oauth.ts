/**
 * WorkBuddy (Tencent CodeBuddy CN) browser OAuth + token refresh.
 *
 * Why this file exists: `codebuddy` / `codebuddy-cn` authenticate with an official
 * `CODEBUDDY_API_KEY` against the headless CLI surface. WorkBuddy accounts obtained from the
 * web/desktop product instead issue a rotating token pair through a server-driven state flow,
 * which is what makes a multi-account pool (`ocx login workbuddy` -> N accounts -> failover)
 * possible. That flow is what this module implements.
 *
 * Wire contract (CN realm, verified against the live service):
 *
 *   1. POST {base}/v2/plugin/auth/state?platform=CLI   body {}
 *        -> {code:0, data:{state, authUrl}}
 *   2. the user authorizes `authUrl` in a browser
 *   3. GET  {base}/v2/plugin/auth/token?state=<state>  (polled)
 *        -> pending: {code:<non-zero>, msg:"login ing"}
 *        -> done:    {code:0, data:{accessToken, refreshToken, expiresIn, domain}}
 *   4. GET  {base}/v2/plugin/login/account?state=<state>   Authorization: Bearer <accessToken>
 *        -> {code:0, data:{uid, enterpriseId, nickname}}   (best effort, for the account label)
 *   5. POST {base}/v2/plugin/auth/token/refresh   X-Refresh-Token: <refreshToken>   body {}
 *        -> {code:0, data:{accessToken, refreshToken, expiresIn, ...}}
 *
 * Notes that shaped the implementation:
 *   - There is no PKCE and no client-controlled redirect: the server mints `state` and owns the
 *     association, so the poll loop is the only way to observe completion.
 *   - `expiresIn` is ~60 days, but the refresh endpoint ROTATES the refresh token, so the caller
 *     must persist the returned refresh token and concurrent refreshes must be avoided
 *     (`refreshPolicy: "lazy-only"` on the registry entry).
 *   - Every response is wrapped in a `{code,msg,data}` envelope where a non-zero `code` is a
 *     business status rather than an HTTP failure; `code != 0` while polling simply means
 *     "not yet", so the envelope is decoded before any error is raised.
 */

import type { OAuthController, OAuthCredentials } from "./types";

const DEFAULT_OAUTH_BASE = "https://copilot.tencent.com";
const ORIGIN_REFERER = "https://www.codebuddy.cn";
const CLIENT_UA = "CLI/2.63.2 CodeBuddy/2.63.2";

/** Refresh this long before the stated expiry so a request never rides an about-to-die token. */
const OAUTH_EXPIRY_SKEW_MS = 5 * 60 * 1000;
const POLL_INTERVAL_MS = 2_000;
/** Matches the CLI-side default in workbuddy2api's `login poll` (5 minutes). */
const LOGIN_TIMEOUT_MS = 5 * 60 * 1000;
/** Fallback lifetime when the upstream omits `expiresIn`; short on purpose so we re-check soon. */
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
 * Refresh failure with the OAuth error code retained.
 *
 * `terminal` classification in `src/oauth/index.ts` keys off the error MESSAGE
 * (`invalid_grant` / `refresh_token_reused` / `revoked` / `expired_token`), so a dead grant is
 * spelled out explicitly while transient failures are left without those tokens and therefore
 * stay retryable instead of forcing the account through a needless re-login.
 */
export class WorkbuddyTokenError extends Error {
  public readonly httpStatus: number | undefined;
  public readonly oauthError: string | undefined;

  constructor(message: string, options?: { cause?: unknown; httpStatus?: number; oauthError?: string }) {
    super(message, options?.cause !== undefined ? { cause: options.cause } : undefined);
    this.name = "WorkbuddyTokenError";
    this.httpStatus = options?.httpStatus;
    this.oauthError = options?.oauthError;
  }
}

function resolveOAuthBase(): string {
  const raw = process.env.WORKBUDDY_OAUTH_BASE
    || process.env.WORKBUDDY_BASE_URL
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

/**
 * One request against an envelope-shaped endpoint.
 *
 * Resolves for any HTTP status the server managed to return — the caller inspects `code`.
 * Rejects only when the request never produced a decodable envelope (network fault, timeout,
 * non-JSON body), which the callers treat as retryable.
 */
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
    // A transport failure (DNS, TLS, refused connection, a destination the runtime declines to
    // reach) rejects BEFORE any envelope exists. Rethrow as our own error type on purpose:
    // opencodex's `publicOAuthAuthenticationErrorMessage` only passes through errors it
    // recognizes and collapses everything else into "OAuth authentication failed", which turns
    // an unreachable network into an indistinguishable "not logged in".
    if (cause instanceof WorkbuddyTokenError) throw cause;
    throw new WorkbuddyTokenError(
      `WorkBuddy ${endpoint} request failed: ${cause instanceof Error ? cause.message : String(cause)}`,
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
    throw new WorkbuddyTokenError(
      `WorkBuddy ${endpoint} returned a non-envelope response (HTTP ${response.status})`,
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

/** True when a refresh rejection means the grant is gone and retrying cannot help. */
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
  if (!access) throw new WorkbuddyTokenError("WorkBuddy token response contained no accessToken");
  const refresh = nonEmptyString(data.refreshToken) ?? refreshFallback;
  if (!refresh) throw new WorkbuddyTokenError("WorkBuddy token response contained no refreshToken");
  const expiresInS = typeof data.expiresIn === "number" && Number.isFinite(data.expiresIn) && data.expiresIn > 0
    ? data.expiresIn
    : FALLBACK_EXPIRES_IN_S;
  const expires = Date.now() + expiresInS * 1000 - OAUTH_EXPIRY_SKEW_MS;
  if (!Number.isFinite(expires)) {
    throw new WorkbuddyTokenError("WorkBuddy token response had an unusable expiresIn");
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

/**
 * Browser OAuth login.
 *
 * Hands the server-minted `authUrl` to the host (the CLI opens it via `openUrl`), then polls
 * until the user completes authorization. Rejects nothing on a non-zero business code: while the
 * user is still in the browser the upstream answers `code != 0` on every attempt.
 */
export async function loginWorkbuddy(ctrl: OAuthController): Promise<OAuthCredentials> {
  const state = await callEnvelope<StateData>("/v2/plugin/auth/state?platform=CLI", {
    method: "POST",
    body: "{}",
    ...(ctrl.signal ? { signal: ctrl.signal } : {}),
  });
  if (state.code !== 0 || !state.data) {
    throw new WorkbuddyTokenError(
      `WorkBuddy auth state request failed: ${state.msg ?? `code ${state.code}`}`,
      { httpStatus: state.status },
    );
  }
  const stateToken = nonEmptyString(state.data.state);
  const authUrl = nonEmptyString(state.data.authUrl);
  if (!stateToken || !authUrl) {
    throw new WorkbuddyTokenError("WorkBuddy auth state response was missing state or authUrl");
  }

  ctrl.onAuth?.({
    url: authUrl,
    instructions: "Authorize WorkBuddy in the browser. This window keeps polling until it completes.",
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
      throw new WorkbuddyTokenError(
        `WorkBuddy login timed out after ${Math.round(LOGIN_TIMEOUT_MS / 1000)}s`
        + `${lastMsg ? ` (last upstream status: ${lastMsg})` : ""}`
        + " — re-run `ocx login workbuddy` and finish the browser authorization promptly.",
      );
    }
    if (Date.now() - announced >= 10_000) {
      announced = Date.now();
      ctrl.onProgress?.(`still waiting for the browser authorization… ${lastMsg ?? ""}`.trim());
    }
    await sleep(POLL_INTERVAL_MS, ctrl.signal);
  }
}

/**
 * Account label for the pool UI. Entirely best effort: an account whose identity lookup fails is
 * still a usable account, so this never rejects the login.
 *
 * `email` is the store's display slot across every provider; the WorkBuddy nickname is the
 * closest human-readable equivalent, with the uid as the fallback so two accounts in the pool
 * are still distinguishable.
 */
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

/**
 * Exchange a refresh token for a fresh pair.
 *
 * The upstream rotates `refreshToken` on every call; the returned one is always preferred and the
 * caller's value is only used as a fallback, so a non-rotating response cannot blank the grant.
 */
export async function refreshWorkbuddyToken(
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
    throw new WorkbuddyTokenError(
      `WorkBuddy token refresh failed (HTTP ${result.status}): ${result.msg ?? `code ${result.code}`}`
      + (dead ? " [invalid_grant]" : ""),
      { httpStatus: result.status, oauthError: dead ? "invalid_grant" : undefined },
    );
  }
  // The refresh response carries no identity fields, and the caller reuses the stored
  // accountId/email, so none is passed here.
  return credentialsFromTokenData(result.data, refreshToken, {});
}
