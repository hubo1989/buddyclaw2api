/**
 * WorkBuddy (Tencent CodeBuddy CN) 剩余积分 quota fetcher。
 *
 * 上游：POST https://www.codebuddy.cn/v2/billing/meter/get-user-resource
 * 请求头：Bearer accessToken + 常规 CN 站头；body `{}`。
 * 响应：{code, msg, data:{Response:{Data:{
 *   TotalCount,            // 资源包个数
 *   Accounts:[{            // 每个资源包
 *     PackageName, CapacityUnit,
 *     CycleCapacityRemain, CycleCapacitySize,   // 本周期剩余/总量（裂变包按月滚动）
 *     CapacityRemain, CapacitySize,             // 包总量口径
 *     CycleEndTime,                             // "YYYY-MM-DD HH:mm:ss"（北京时间）
 *   }]
 * }}}}
 *
 * 展示策略（与 opencodex 的 ProviderQuota 对齐）：
 *   - 所有资源包按 CycleCapacitySize 汇总为两个自定义窗口：
 *       "Credits (月度包)"  = 本月到期的那批（CycleEndTime 在 ~35 天内）
 *       "Credits (长期包)"  = 其余（多周期/无限期）
 *   - percent = 已用占比（100 - 剩余%），面板按 opencodex 惯例显示用量条。
 *   - 无可用数据时返回 null（面板显示 unavailable，而不是误导性的 0%）。
 */

const WORKBUDDY_BILLING_URL = "https://www.codebuddy.cn/v2/billing/meter/get-user-resource";
const WORKBUDDY_BILLING_HEADERS: Record<string, string> = {
  "Content-Type": "application/json",
  "Accept": "application/json",
  "X-Requested-With": "XMLHttpRequest",
  "Origin": "https://www.codebuddy.cn",
  "Referer": "https://www.codebuddy.cn/",
  "User-Agent": "CLI/2.63.2 CodeBuddy/2.63.2",
};
/** 月度包判定：周期结束在 35 天内视为"本月滚动包"。 */
const MONTHLY_WINDOW_MS = 35 * 24 * 60 * 60 * 1000;

interface WorkbuddyPackageRow {
  PackageName?: unknown;
  CapacityUnit?: unknown;
  CycleCapacityRemain?: unknown;
  CycleCapacitySize?: unknown;
  CapacityRemain?: unknown;
  CapacitySize?: unknown;
  CycleEndTime?: unknown;
}

function num(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

/** "2026-09-30 23:59:59"（北京时间）-> epoch ms。解析失败返回 undefined。 */
function parseCnDate(value: unknown): number | undefined {
  if (typeof value !== "string") return undefined;
  const m = /^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})/.exec(value.trim());
  if (!m) return undefined;
  return Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4] - 8, +m[5], +m[6]);
}

function toPercent(remain: number, size: number): number {
  if (size <= 0) return 0;
  const used = Math.max(0, Math.min(100, ((size - remain) / size) * 100));
  return Math.round(used * 10) / 10;
}

export interface WorkbuddyQuotaWindows {
  monthly?: { percent: number; resetAt?: number; remain: number; size: number };
  longTerm?: { percent: number; remain: number; size: number };
  totalRemain: number;
  totalSize: number;
}

export function parseWorkbuddyQuota(payload: unknown, now = Date.now()): WorkbuddyQuotaWindows | null {
  const data = (payload as { data?: { Response?: { Data?: { Accounts?: unknown } } } } | undefined)
    ?.data?.Response?.Data;
  const rows = Array.isArray(data?.Accounts) ? (data!.Accounts as WorkbuddyPackageRow[]) : [];
  if (rows.length === 0) return null;

  let monthlyRemain = 0, monthlySize = 0, longRemain = 0, longSize = 0;
  let monthlyReset: number | undefined;
  for (const row of rows) {
    const size = num(row.CycleCapacitySize);
    const remain = num(row.CycleCapacityRemain);
    if (size === undefined || remain === undefined || size <= 0) continue;
    const cycleEnd = parseCnDate(row.CycleEndTime);
    if (cycleEnd !== undefined && cycleEnd - now < MONTHLY_WINDOW_MS) {
      monthlyRemain += remain;
      monthlySize += size;
      if (monthlyReset === undefined || cycleEnd > monthlyReset) monthlyReset = cycleEnd;
    } else {
      longRemain += remain;
      longSize += size;
    }
  }
  if (monthlySize === 0 && longSize === 0) return null;

  const out: WorkbuddyQuotaWindows = {
    totalRemain: monthlyRemain + longRemain,
    totalSize: monthlySize + longSize,
  };
  if (monthlySize > 0) {
    out.monthly = {
      percent: toPercent(monthlyRemain, monthlySize),
      remain: monthlyRemain,
      size: monthlySize,
      ...(monthlyReset !== undefined ? { resetAt: monthlyReset } : {}),
    };
  }
  if (longSize > 0) {
    out.longTerm = { percent: toPercent(longRemain, longSize), remain: longRemain, size: longSize };
  }
  return out;
}

/** 拉取并转换为 opencodex 的 ProviderQuota；失败/无数据返回 null。 */
export async function fetchWorkbuddyQuota(
  provider: string,
  accessToken: string,
): Promise<{
  quota: {
    customWindows: Array<{ label: string; percent: number; resetAt?: number }>;
    monthlyPercent?: number;
    updatedAt: number;
  };
  source: string;
} | null> {
  if (!accessToken) return null;
  let payload: unknown;
  try {
    const response = await fetch(WORKBUDDY_BILLING_URL, {
      method: "POST",
      headers: { ...WORKBUDDY_BILLING_HEADERS, "Authorization": `Bearer ${accessToken}` },
      body: "{}",
      redirect: "error",
      signal: AbortSignal.timeout(20_000),
    });
    if (!response.ok) return null;
    payload = await response.json();
  } catch {
    return null;
  }
  const envelope = payload as { code?: unknown };
  if (envelope.code !== 0) return null;
  const windows = parseWorkbuddyQuota(payload);
  if (!windows) return null;

  const customWindows: Array<{ label: string; percent: number; resetAt?: number }> = [];
  if (windows.monthly) {
    customWindows.push({
      label: `Credits 月度包 · 剩 ${Math.round(windows.monthly.remain)}/${Math.round(windows.monthly.size)}`,
      percent: windows.monthly.percent,
      ...(windows.monthly.resetAt !== undefined ? { resetAt: windows.monthly.resetAt } : {}),
    });
  }
  if (windows.longTerm) {
    customWindows.push({
      label: `Credits 长期包 · 剩 ${Math.round(windows.longTerm.remain)}/${Math.round(windows.longTerm.size)}`,
      percent: windows.longTerm.percent,
    });
  }
  if (customWindows.length === 0) return null;
  return {
    quota: {
      customWindows,
      ...(windows.monthly ? { monthlyPercent: windows.monthly.percent } : {}),
      updatedAt: Date.now(),
    },
    source: `${provider}:billing`,
  };
}
