  {
    // AutoClaw — 智谱澳龙（OpenClaw 桌面版的云端账号体系）。
    // 与 workbuddy 同构：手机验证码 / 海外网页 OAuth 登录、refresh 轮换（lazy-only）、
    // 多账号池 + 429 failover + 后台续期（注册进 OAUTH_PROVIDERS 后全部自动获得）。
    // 模型为带前缀的 routeModelId（zai_/zaicoding_/tdpsk_），服务端白名单校验，实测 2026-09-11。
    id: "autoclaw",
    label: "AutoClaw (Zhipu)",
    adapter: "openai-chat",
    // 指向本机 workbuddy2api 的带外转发端点（经 tailscale 域名，tailnet 内可达），
    // 而不是直连上游：AutoClaw 的 token 与 host 绑定（手机号账号在国内 host、
    // z.ai/Google 账号在国际 host），而 provider 只有一个 baseUrl —— 由 7863 按
    // token 自动挑 host。端点路径 = ${baseUrl}/chat/completions。
    // ⚠️ 不能用 0.0.0.0：新版 opencodex 目标策略把 unspecified 地址判死；
    // 127.0.0.1/localhost 虽可行但会打「本地」徽标。tailscale 域名解析到
    // tailnet IP（100.64/10，private 类），allowPrivateNetwork:true 放行且无徽标。
    baseUrl: "https://m4-pro.tailc81e73.ts.net:8443/v1/autoclaw",
    allowPrivateNetwork: true,
    authKind: "oauth",
    preserveCustomDestination: true,
    // AutoClaw chat authenticates via X-Authorization (standard Authorization is 401-rejected).
    // Requires the adapter patch in apply_autoclaw.sh (types + openai-chat consume this field).
    authHeaderName: "X-Authorization",
    staticHeaders: {
      "X-Client-Type": "pc",
      "X-Product": "autoclaw",
      "X-Harness-Type": "zcode",
      "X-Tm": "mac",
      "X-Version": "1.18.1",
      "X-Lang": "zh-CN",
      "X-Channel": "AutoClaw4",
      "x_trace_id": "autoclaw-desktop",
    },
    defaultModel: "zai_auto",
    models: [
      "zai_auto", "zai_auto-fast", "zai_glm-5.3-flash", "zai_glm-5-turbo",
      "zaicoding_glm-5.3", "tdpsk_deepseek-v4-flash-202605", "tdpsk_deepseek-v4-pro-202606",
    ],
    liveModels: false,
    modelContextWindows: {
      "zai_auto": 200000,
      "zai_auto-fast": 200000,
      "zai_glm-5.3-flash": 1000000,
      "zai_glm-5-turbo": 200000,
      "zaicoding_glm-5.3": 1000000,
      "tdpsk_deepseek-v4-flash-202605": 1000000,
      "tdpsk_deepseek-v4-pro-202606": 1000000,
    },
    modelMaxInputTokens: {
      "zai_auto": 200000,
      "zai_auto-fast": 200000,
      "zai_glm-5.3-flash": 1000000,
      "zai_glm-5-turbo": 200000,
      "zaicoding_glm-5.3": 1000000,
      "tdpsk_deepseek-v4-flash-202605": 1000000,
      "tdpsk_deepseek-v4-pro-202606": 1000000,
    },
    modelMaxOutputTokens: {
      "zai_auto": 32000,
      "zai_auto-fast": 32000,
      "zai_glm-5.3-flash": 32000,
      "zai_glm-5-turbo": 32000,
      "zaicoding_glm-5.3": 131072,
      "tdpsk_deepseek-v4-flash-202605": 384000,
      "tdpsk_deepseek-v4-pro-202606": 384000,
    },
    reasoningEfforts: ["low", "high"],
    note: "AutoClaw (Zhipu OpenClaw) via phone-SMS or oversea web OAuth. `ocx login autoclaw` prompts for the mode; refresh tokens rotate (lazy-only). Chat needs X-Authorization + X-Request-Model which the openai-chat adapter sends via staticHeaders/per-request model pass-through.",
  },
