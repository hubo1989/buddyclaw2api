/**
 * Qoder（阿里 AI IDE）device 流 + PAT 登录 —— CN / GLOBAL 双 provider 共用模块。
 *
 * 登录经本机 workbuddy2api 网关（7863）走两步：device/start 本地生成 PKCE+nonce+
 * machineId 并返回授权 URL；浏览器完成授权后 device/poll 每 2s 轮询（与上游
 * openapi.qoder.sh 设备流同构，202/404=未完成）。PAT 模式直接 /admin/qoder/login/pat
 * 换 job token。
 *
 * 成功后账号同时存在于两处：
 *   - 网关 auths/qoder-<realm>-<uid>.json（COSY 签名需要 userId+machineId，
 *     纯 token 不够——聊天时网关按 X-Authorization token 查池签名）
 *   - opencodex 自己的账号池（本模块返回的 OAuthCredentials）
 *
 * 聊天出站由 registry 条目指向 https://<tailscale 域名>:8443/v1/qoder/<realm>/chat/completions
 *（0.0.0.0 已被新版目标策略判死；tailscale 域名无「本地」徽标且 tailnet 内可达）。
 * 设备流 token 约 30 天，上游 refresh 端点对设备流返回 403（实测），过期即重登；
 * 因此 refresh 总是报 invalid_grant，让 opencodex 把账号标成需要重新登录。
 */

import type { OAuthController, OAuthCredentials } from "./types";

const PROXY_BASE = (process.env.QODER_PROXY_BASE ?? "http://127.0.0.1:7863").trim().replace(/\/+$/, "");
// 网关设了 api_key 的部署：在 opencodex 服务环境里注入 QODER_GATEWAY_KEY；
// 回环空 key 部署（默认）无需设置。
const GATEWAY_KEY = (process.env.QODER_GATEWAY_KEY ?? "").trim();
const POLL_INTERVAL_MS = 2_000;
const LOGIN_TIMEOUT_MS = 5 * 60 * 1000;
const OAUTH_EXPIRY_SKEW_MS = 10 * 60 * 1000;

export class QoderTokenError extends Error {
  public readonly oauthError: string | undefined;

  constructor(message: string, options?: { oauthError?: string }) {
    super(message);
    this.name = "QoderTokenError";
    this.oauthError = options?.oauthError;
  }
}

type Realm = "cn" | "global";

interface QoderAccountView {
  realm: string;
  userId: string;
  name: string;
  email: string;
  authMethod: string;
  expired: boolean;
  expiresAt: number;
  accessToken: string;
}

async function proxyPost(path: string, body: unknown, signal?: AbortSignal): Promise<any> {
  let response: Response;
  try {
    response = await fetch(`${PROXY_BASE}${path}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(GATEWAY_KEY ? { Authorization: `Bearer ${GATEWAY_KEY}` } : {}),
      },
      body: JSON.stringify(body),
      ...(signal ? { signal } : {}),
    });
  } catch (cause) {
    throw new QoderTokenError(
      `Qoder gateway ${path} request failed: ${cause instanceof Error ? cause.message : String(cause)} — 网关 7863 未启动？`,
      { cause },
    );
  }
  const text = await response.text();
  let parsed: any;
  try {
    parsed = text ? JSON.parse(text) : undefined;
  } catch {
    parsed = undefined;
  }
  if (!response.ok) {
    const msg = parsed?.error?.message ?? `HTTP ${response.status}: ${text.slice(0, 200)}`;
    throw new QoderTokenError(`Qoder gateway ${path}: ${msg}`);
  }
  return parsed;
}

function credentialsFromView(view: QoderAccountView): OAuthCredentials {
  const access = typeof view.accessToken === "string" ? view.accessToken : "";
  if (!access) throw new QoderTokenError("Qoder gateway returned no accessToken");
  // 上游 expiresAt 为 Unix 秒；缺省按 30 天设备流口径兜底，再提前 10 分钟过期。
  const expiresMs = (view.expiresAt > 0 ? view.expiresAt * 1000 : Date.now() + 30 * 24 * 3600 * 1000)
    - OAUTH_EXPIRY_SKEW_MS;
  return {
    access,
    // 设备流 refresh 是 no-op（上游 403）；占位 refresh 让 opencodex 凭据结构完整。
    refresh: access,
    expires: expiresMs,
    ...(view.userId ? { accountId: view.userId } : {}),
    email: view.email || view.name || (view.userId ? `qoder-user-${view.userId}` : "qoder-account"),
    source: "oauth",
  };
}

async function loginQoder(ctrl: OAuthController, realm: Realm): Promise<OAuthCredentials> {
  const start = await proxyPost("/admin/qoder/login/device/start", { realm }, ctrl.signal);
  if (!start?.authUrl || !start?.nonce || !start?.verifier) {
    throw new QoderTokenError("Qoder device/start returned an incomplete payload");
  }
  ctrl.onAuth?.({
    url: start.authUrl,
    instructions: realm === "cn"
      ? "在浏览器完成 Qoder CN（qoder.cn）授权，本窗口持续轮询直至完成。"
      : "Authorize Qoder (Global) in the browser. This window keeps polling until it completes.",
  });

  const deadline = Date.now() + LOGIN_TIMEOUT_MS;
  let announced = Date.now();
  for (;;) {
    if (ctrl.signal?.aborted) throw new Error("Login cancelled");
    const poll = await proxyPost("/admin/qoder/login/device/poll", {
      realm,
      nonce: start.nonce,
      verifier: start.verifier,
      machineId: start.machineId,
      persist: true,
    }, ctrl.signal);
    if (poll?.pending) {
      if (Date.now() >= deadline) {
        throw new QoderTokenError(
          `Qoder ${realm} login timed out after ${Math.round(LOGIN_TIMEOUT_MS / 1000)}s — re-run ocx login qoder-${realm} and finish the browser authorization promptly.`,
        );
      }
      if (Date.now() - announced >= 10_000) {
        announced = Date.now();
        ctrl.onProgress?.("still waiting for the browser authorization…");
      }
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(resolve, POLL_INTERVAL_MS);
        ctrl.signal?.addEventListener("abort", () => { clearTimeout(timer); reject(new Error("Login cancelled")); }, { once: true });
      });
      continue;
    }
    if (poll?.accessToken || poll?.userId) {
      return credentialsFromView(poll as QoderAccountView);
    }
    throw new QoderTokenError(`Qoder ${realm} device poll returned an unexpected payload`);
  }
}

export async function loginQoderGlobal(ctrl: OAuthController): Promise<OAuthCredentials> {
  return loginQoder(ctrl, "global");
}

export async function loginQoderCN(ctrl: OAuthController): Promise<OAuthCredentials> {
  return loginQoder(ctrl, "cn");
}

/**
 * 设备流 token 上游 refresh 实测 403（9router 同口径）——报 invalid_grant 让
 * opencodex 把账号归入「需重登」而不是无限重试。
 */
export async function refreshQoderToken(_refreshToken: string, _signal?: AbortSignal): Promise<OAuthCredentials> {
  throw new QoderTokenError(
    "Qoder device tokens cannot be refreshed upstream (403) — re-run `ocx login qoder-global` / `ocx login qoder-cn`. [invalid_grant]",
    { oauthError: "invalid_grant" },
  );
}
