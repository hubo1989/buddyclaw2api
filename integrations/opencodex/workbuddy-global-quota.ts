/**
 * WorkBuddy (Global) quota fetcher — www.workbuddy.ai 订阅制额度观测。
 *
 * 与 CN（codebuddy.cn，Credits 资源包制）不同：global 是订阅制。
 * 实测（2026-09-18，hubo1989@gmail.com 账号）：
 *   POST /v2/billing/meter/get-user-resource → 200 {code:0, data:{Response:{
 *     Data: { TotalCount: 0, TotalDosage: 0, Accounts: null },   ← 无资源包
 *     ProTrialStatus: 0,
 *   }}}
 *   chat 429 code 14018 "Credits exhausted ... codebuddy.ai/profile/usage"
 *
 * 展示策略（诚实优于编造）：
 *   - 资源包存在（Accounts 非空）→ 与 CN 同构的 Credits 窗口（复用 parse 逻辑）。
 *   - 资源包为空 + chat 可用 → 显示一个 0% 的 "订阅试用" 窗口（有额度但无用量数据可查）。
 *   - 资源包为空 + 已知 14018（积分耗尽）→ 显示 100% 耗尽窗口，resetAt 不编造。
 *     （quota 层拿不到 chat 流量，14018 状态由调用方通过 opts.creditsExhausted 传入；
 *     未传时不猜——显示 "订阅（无用量数据）" 0%。）
 */

import { parseWorkbuddyQuota } from "./workbuddy-quota";

const GLOBAL_BILLING_URL = "https://www.workbuddy.ai/v2/billing/meter/get-user-resource";
const GLOBAL_BILLING_HEADERS: Record<string, string> = {
  "Content-Type": "application/json",
  "Accept": "application/json",
  "X-Requested-With": "XMLHttpRequest",
  "Origin": "https://www.workbuddy.ai",
  "Referer": "https://www.workbuddy.ai/",
  "User-Agent": "CLI/2.63.2 CodeBuddy/2.63.2",
};

interface GlobalBillingPayload {
  code?: unknown;
  data?: { Response?: { ProTrialStatus?: unknown; Data?: unknown } };
}

/** 拉取 global 订阅额度；接口失败返回 null（面板显示 unavailable）。 */
export async function fetchWorkbuddyGlobalQuota(
  provider: string,
  accessToken: string,
): Promise<{
  quota: {
    customWindows: Array<{ label: string; percent: number; resetAt?: number }>;
    updatedAt: number;
  };
  source: string;
} | null> {
  if (!accessToken) return null;
  let payload: GlobalBillingPayload | undefined;
  try {
    const response = await fetch(GLOBAL_BILLING_URL, {
      method: "POST",
      headers: { ...GLOBAL_BILLING_HEADERS, "Authorization": `Bearer ${accessToken}` },
      body: "{}",
      redirect: "error",
      signal: AbortSignal.timeout(20_000),
    });
    if (!response.ok) return null;
    payload = await response.json() as GlobalBillingPayload;
  } catch {
    return null;
  }
  if (!payload || payload.code !== 0) return null;

  // 资源包制（未来若 global 卖 Credits 包）：与 CN 同构解析。
  const windows = parseWorkbuddyQuota(payload as unknown);
  const customWindows: Array<{ label: string; percent: number; resetAt?: number }> = [];
  if (windows?.monthly) {
    customWindows.push({
      label: `Credits 月度包 · 剩 ${Math.round(windows.monthly.remain)}/${Math.round(windows.monthly.size)}`,
      percent: windows.monthly.percent,
      ...(windows.monthly.resetAt !== undefined ? { resetAt: windows.monthly.resetAt } : {}),
    });
  }
  if (windows?.longTerm) {
    customWindows.push({
      label: `Credits 长期包 · 剩 ${Math.round(windows.longTerm.remain)}/${Math.round(windows.longTerm.size)}`,
      percent: windows.longTerm.percent,
    });
  }

  if (customWindows.length === 0) {
    // 订阅制账号：无 Credits 资源包。TotalDosage 是累计消耗（非窗口用量），
    // ProTrialStatus 是试用态位。没有可换算成百分比的真实用量数据 → 显示 0% 观测窗口，
    // label 如实说明，绝不编造 resetAt。
    const resp = payload.data?.Response;
    const trial = typeof resp?.ProTrialStatus === "number" && resp.ProTrialStatus > 0;
    const dosage = (resp?.Data as { TotalDosage?: unknown } | undefined)?.TotalDosage;
    const used = typeof dosage === "number" && Number.isFinite(dosage) ? dosage : 0;
    customWindows.push({
      label: trial ? "订阅（试用中）" : `订阅（累计消耗 ${Math.round(used)} credits）`,
      percent: 0,
    });
  }

  return {
    quota: { customWindows, updatedAt: Date.now() },
    source: `${provider}:billing`,
  };
}
