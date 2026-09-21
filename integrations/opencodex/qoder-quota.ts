/**
 * Qoder quota fetcher — openapi.qoder.sh/api/v2/quota/usage（Bearer 设备 token）。
 *
 * 实测（2026-09-20，qoder-global 账号）：
 *   { userType:"personal_standard", usageType:"credits",
 *     totalUsagePercentage:0.01, isQuotaExceeded:false, expiresAt:253402214400000,
 *     userQuota:{total:0,used:0,remaining:0,percentage:0,unit:"credits"},
 *     addOnQuota:{total:100,used:0,remaining:100,percentage:0.01,unit:"credits"} }
 *
 * 展示策略（诚实优于编造）：
 *   - userQuota.total > 0 → 订阅额度窗口（percentage 直接用上游值，0-100）。
 *   - addOnQuota.total > 0 → Credits 附加包窗口。
 *   - isQuotaExceeded → 窗口标 100%（已耗尽）。
 *   - 两者皆无 → 0% 观测窗口，如实标注。
 *   - expiresAt = 253402214400000（9999 年）视作无过期，不编造 resetAt。
 */

// 上游对 bun/浏览器类 TLS 指纹返回 401 TOKEN_INVALID，因此经本机网关（Go http
// 客户端指纹可用）代理拉取；网关按 X-Authorization token 找账号后转发。
const QUOTA_PROXY = (process.env.QODER_PROXY_BASE ?? "http://127.0.0.1:7863").trim().replace(/\/+$/, "");

interface QoderQuotaBucket {
  total?: unknown;
  used?: unknown;
  remaining?: unknown;
  percentage?: unknown;
  unit?: unknown;
}

interface QoderQuotaPayload {
  userQuota?: QoderQuotaBucket;
  addOnQuota?: QoderQuotaBucket;
  totalUsagePercentage?: unknown;
  isQuotaExceeded?: unknown;
  expiresAt?: unknown;
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

export async function fetchQoderQuota(
  provider: string,
  accessToken: string,
): Promise<{
  quota: {
    customWindows: Array<{ label: string; percent: number; resetAt?: number }>;
    updatedAt: number;
  };
  source: string;
} | null> {
  const realm = provider === "qoder-cn-oauth" ? "cn" : "global";
  if (!accessToken) return null;
  let payload: QoderQuotaPayload | undefined;
  try {
    const response = await fetch(`${QUOTA_PROXY}/admin/qoder/quota?realm=${realm}`, {
      headers: {
        "X-Authorization": accessToken,
        "Accept": "application/json",
      },
      redirect: "error",
      signal: AbortSignal.timeout(20_000),
    });
    if (!response.ok) return null;
    payload = await response.json() as QoderQuotaPayload;
  } catch {
    return null;
  }
  if (!payload) return null;

  const exhausted = payload.isQuotaExceeded === true;
  const customWindows: Array<{ label: string; percent: number; resetAt?: number }> = [];
  const unit = typeof payload.userQuota?.unit === "string" && payload.userQuota.unit
    ? payload.userQuota.unit
    : "credits";

  const plan = payload.userQuota;
  if (plan && num(plan.total) > 0) {
    const percent = exhausted ? 100 : num(plan.percentage);
    customWindows.push({
      label: `订阅额度 · 剩 ${Math.round(num(plan.remaining))}/${Math.round(num(plan.total))} ${unit}`,
      percent: Math.min(100, Math.max(0, percent)),
    });
  }

  const addon = payload.addOnQuota;
  if (addon && num(addon.total) > 0) {
    const percent = exhausted ? 100 : num(addon.percentage);
    customWindows.push({
      label: `Credits 附加包 · 剩 ${Math.round(num(addon.remaining))}/${Math.round(num(addon.total))} ${unit}`,
      percent: Math.min(100, Math.max(0, percent)),
    });
  }

  if (customWindows.length === 0) {
    customWindows.push({
      label: exhausted ? "配额已耗尽" : "订阅（无配额用量数据）",
      percent: exhausted ? 100 : 0,
    });
  }

  // 9999 年占位时间戳视作无过期：仅在有意义的过期时间上给 resetAt。
  let resetAt: number | undefined;
  if (typeof payload.expiresAt === "number" && payload.expiresAt > 0 && payload.expiresAt < 4102444800000) {
    resetAt = payload.expiresAt;
  }
  for (const w of customWindows) {
    if (resetAt !== undefined) w.resetAt = resetAt;
  }

  return {
    quota: { customWindows, updatedAt: Date.now() },
    source: `${provider}:quota-usage`,
  };
}
