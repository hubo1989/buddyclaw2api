/**
 * AutoClaw（智谱澳龙）积分 quota fetcher。
 *
 * 上游：GET {host}/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw
 * 请求头：业务签名（md5(appid & ts & APP_KEY) → X-Auth-Sign）+ `Authorization: Bearer <access>`。
 *
 * 响应：{code:0, data:{schema_version:"wallets.v2", total_balance, wallets:[{
 *   public_wallet_type, display_name, balance, balance_view, display, priority }]}}
 *
 * 关键点：
 *   1. **host 必须与账号所在地一致** —— 手机号账号落在国内 host（autoglm-api.zhipuai.cn），
 *      拿它的 token 打国际 host 会得到 `410000 用户未登录`。所以这里依次尝试国内 → 国际。
 *   2. AutoClaw 是**纯积分制**：响应里只有余额，没有「总量/上限」，因此算不出已用百分比。
 *      面板的 ProviderQuota 又要求 customWindows 里带 percent（否则整行不展示），
 *      所以 percent 固定填 0（表示未耗尽 —— 这也是 combos 判断额度耗尽的依据，安全），
 *      真正的信息放在 label 里（沿用 workbuddy 的做法：label 直接带数字）。
 */

import { createHash, randomUUID } from "node:crypto";

const APP_ID = "100003";
const APP_KEY = "38d2391985e2369a5fb8227d8e6cd5e5";
const PRODUCT = "autoclaw";
const WALLETS_PATH = `/agent-assetmgr/api/v2/wallets?biz_app_id=${PRODUCT}`;
/** 先国内（手机号账号），再国际（z.ai / Google 账号）。 */
const QUOTA_HOSTS = ["https://autoglm-api.zhipuai.cn", "https://autoglm-api.autoglm.ai"];

interface WalletRow {
  public_wallet_type?: unknown;
  display_name?: unknown;
  balance?: unknown;
  display?: unknown;
}

function walletHeaders(accessToken: string): Record<string, string> {
  const ts = String(Math.floor(Date.now() / 1000));
  return {
    "Content-Type": "application/json",
    "Accept": "*/*",
    "X-Version": "1.18.1",
    "X-Tm": process.platform === "darwin" ? "mac" : process.platform === "win32" ? "win" : "linux",
    "X-Product": PRODUCT,
    "X-Auth-Appid": APP_ID,
    "X-Auth-TimeStamp": ts,
    "X-Auth-Sign": createHash("md5").update(`${APP_ID}&${ts}&${APP_KEY}`).digest("hex"),
    "X-Trace-Id": randomUUID(),
    "X-Lang": "zh-CN",
    "X-Channel": "AutoClaw4",
    // WAF 按 UA 放行：非 AutoClaw UA 会被直接 405/拦截。
    "User-Agent": "AutoClaw/1.18.1 (mac-arm64)",
    "Authorization": `Bearer ${accessToken}`,
  };
}

/** 拉取并转换为 opencodex 的 ProviderQuota；失败/无数据返回 null。 */
export async function fetchAutoclawQuota(
  provider: string,
  accessToken: string,
): Promise<{
  quota: {
    customWindows: Array<{ label: string; percent: number; resetAt?: number }>;
    updatedAt: number;
  };
  source: string;
} | null> {
  const token = (accessToken ?? "").replace(/^Bearer\s+/i, "").trim();
  if (!token) return null;

  for (const host of QUOTA_HOSTS) {
    let payload: { code?: unknown; data?: { total_balance?: unknown; wallets?: unknown } } | null = null;
    try {
      const response = await fetch(host + WALLETS_PATH, {
        method: "GET",
        headers: walletHeaders(token),
        redirect: "error",
        signal: AbortSignal.timeout(20_000),
      });
      if (!response.ok) continue;
      payload = await response.json();
    } catch {
      continue; // 换下一个 host
    }
    if (!payload || payload.code !== 0 || !payload.data) continue;

    const total = Number(payload.data.total_balance);
    if (!Number.isFinite(total)) continue;

    const rows: WalletRow[] = Array.isArray(payload.data.wallets) ? (payload.data.wallets as WalletRow[]) : [];
    const named = rows
      .filter(r => r.display !== false && Number(r.balance) > 0)
      .map(r => ({ name: String(r.display_name ?? r.public_wallet_type ?? "积分"), balance: Number(r.balance) }));

    // percent 固定 0：纯积分制没有上限可比，0 同时表示「未耗尽」（安全的中性值）。
    const customWindows: Array<{ label: string; percent: number }> = [
      { label: `积分余额 ${Math.round(total)}`, percent: 0 },
    ];
    // 明细里与总额相同的钱包不重复展示（AutoClaw 常见：total == 唯一非零钱包）
    for (const w of named) {
      if (Math.round(w.balance) === Math.round(total)) continue;
      customWindows.push({ label: `${w.name} ${Math.round(w.balance)}`, percent: 0 });
      if (customWindows.length >= 4) break;
    }

    return {
      quota: { customWindows, updatedAt: Date.now() },
      source: `${provider}:wallets`,
    };
  }
  return null;
}
