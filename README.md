# WorkBuddy2API

> WorkBuddy CN（CodeBuddy / copilot.tencent.com）的 OpenAI 兼容反向代理，支持 OAuth 登录、多账号轮转、工具调用与流式响应。可选集成智谱 AutoClaw（澳龙）云端账号体系作为第二上游。

## 功能特性

- 🔐 **OAuth 登录** — 通过 `/v2/plugin/auth/state` 设备授权流程获取凭证，支持 token 自动刷新
- 🔄 **多账号轮转** — 三因子加权随机选号（credits ×闲置×成功率），防热点 + 防惊群（100ms 窗口）
- 🛠 **工具调用** — 完整支持 OpenAI tools/tool_choice，流式 `tool_calls` 按 index 合并
- 📡 **流式 + 非流式** — 上游 SSE 透传；非流式本地聚合（上游拒绝非流式请求）
- ⏰ **定时签到** — 每日 09:00 / 21:00 自动签到 + 积分查询，积分耗尽账号次日 04:00 自动恢复
- 🦞 **AutoClaw 上游（可选）** — `autoclaw/*` 模型前缀路由到智谱 AutoClaw 云端；手机验证码登录、独立 token 刷新（轮换写回）、每日签到（400 分/次，幂等）、积分钱包查询
- 📊 **积分监控** — `credit.sh` 一键查询全部账号剩余/总量/百分比
- 🔑 **登录工具** — `login.sh` 交互式登录，落盘即生效
- 🏗 **Docker 部署** — 一键 `docker compose up`，healthcheck 常驻
- 📈 **请求级日志** — 每个 `/v1/chat/completions` 请求打表格日志（seq/TTFB/uid/tokens/latency）
- 🏥 **健康检查** — `/healthz` 无健康账号时返回 503，可接负载均衡器
- 📉 **状态汇总** — `/status` 返回 total/healthy/cooling/disabled 计数 + 每账号完整画像

## 快速开始

### 1. 克隆 & 配置

```bash
git clone https://github.com/Sliverkiss/workbuddy2api.git
cd workbuddy2api
cp config.example.json config.json
# 编辑 config.json，把 api_key 换成自己生成的长随机串（示例里的占位值会被拒绝启动）
#   openssl rand -hex 32
```

### 2. 添加账号

```bash
./login.sh
# 打开浏览器登录 → 脚本自动等待并落盘 auths/ → 自动重启服务加载新账号
```

> ⚠️ **账号目录不做运行期热加载**：`LoadDir` + `SyncToDir` 只在进程启动时各执行一次，所以**新增账号后必须重启服务**才生效。
> `login.sh` 已自动处理这一步（依次尝试 docker 容器 → launchd 服务）；手动重启用 `./service.sh restart`。

### 3. 启动服务

```bash
docker compose up -d --build
```

### 4. 验证

```bash
# 模型列表
curl -s http://localhost:7863/v1/models -H "Authorization: Bearer your-api-key"

# 账号状态（汇总 + 每账号详情）
curl -s http://localhost:7863/status -H "Authorization: Bearer your-api-key"

# 健康检查（无健康账号时 503）
curl -s http://localhost:7863/healthz

# 聊天补全（流式）
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'

# 聊天补全（非流式，本地聚合）
curl -s http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 只在本机跑（不用 Docker）

本机自用推荐直接绑回环 —— 此时**不需要设置 `api_key`**（仅本机可达，安全闸门只告警不拦）：

```bash
# 零配置：不需要 config.json，也不需要先建 auths/（先跑起来再加账号）
WB2A_LISTEN=127.0.0.1:7863 go run ./cmd/server
```

若要保持默认的 `:7863`（全网卡）监听，则**必须**给一个真 key：

```bash
cp config.example.json config.json
python3 -c "import json,secrets;p='config.json';d=json.load(open(p));d['api_key']=secrets.token_hex(32);json.dump(d,open(p,'w'),indent=2);print('api_key 已写入 config.json')"
go run ./cmd/server
# 之后所有请求都要带 -H "Authorization: Bearer <config.json 里的 api_key>"
```

> ⚠️ **Docker 场景下不要把容器内 `listen` 改成 `127.0.0.1`**：端口映射是从宿主机打到容器 IP，绑容器回环会让 `7863:7863` 映射不通。
> 要收紧暴露面请改 compose 的宿主机侧 —— `"127.0.0.1:7863:7863"`。

### 开机自启（macOS launchd）

```bash
./service.sh install     # 生成 plist → 加载 → 开机自启（幂等，可反复执行）
./service.sh status      # 服务状态 + 端口探测
./service.sh restart     # 重启（登录新账号后加载凭证用）
./service.sh logs        # 跟踪日志
./service.sh uninstall   # 停止并关闭开机自启
```

- plist 写入 `~/Library/LaunchAgents/com.hubo.workbuddy2api.plist`，其中路径按仓库实际位置生成，可整目录搬迁后重跑 `install`。
- `RunAtLoad` + `KeepAlive`：登录即启动、崩溃自动拉起；`ThrottleInterval=10` 避免配置错误时打爆日志。
- 环境变量写死 `WB2A_LISTEN=127.0.0.1:7863` —— **即使 `config.json` 里写了 `:7863`，也会被 env 覆盖回回环**，不会因改配置意外暴露到全网卡。
- 日志落在 `logs/wb2api.{out,err}.log`（已 gitignore）。

> 本机自用走"回环 + 不设 key"，所以 `install` 不需要任何密钥。若你的终端被本工具之外的环境限制（无法写 launchd 域），`install` 会明确提示手动执行的那一条命令；plist 本身放在 `~/Library/LaunchAgents/` 下，**下一次登录也会自动加载**。

### 登录账号

服务启动后 `auths/` 是空的（0 账号，`/healthz` 会返回 503），需要登录至少一个账号：

> ⚠️ **务必用 `./login.sh`，不要直接跑 `./login url`。** `url` 只是「取授权链接」的底层子命令 ——
> 它不轮询、不落盘，打印完链接就结束；而且每次都会覆盖 `/tmp/wb2api-login-state.json`，
> 让**上一个链接立即失效**。单独跑它 = 「浏览器里登录了，但什么都没保存」。

```bash
cd /Users/hubo/Tools/workbuddy2api
./login.sh
```

1. 脚本打印授权 URL（**并自动复制到剪贴板**）。在浏览器打开该链接，登录你的 **WorkBuddy CN / CodeBuddy CN 账号**，并在**授权页点确认**（`copilot.tencent.com` 设备授权流程，无 PKCE）。
2. **不需要按任何键**：脚本会自动轮询等你完成登录（默认最长 5 分钟，可用 `LOGIN_POLL_TIMEOUT=<秒>` 调整）。
3. 登录完成后脚本自动继续：**先把凭证落盘** `auths/workbuddy-<uid>.json`（先保住 token，再做其余步骤）→ 上游每日签到（幂等，失败不阻塞）→ 重启服务加载新账号 → 打印当前账号数。

**多账号（关键）**：每次登录前必须让浏览器处于**目标账号**的登录态 —— 先退出当前账号，或用**无痕窗口**打开新链接。
否则浏览器会复用当前登录态、**重复授权同一个账号**（uid 相同 → 覆盖同一份 auth 文件），跑几次都还是同一个账号。

> ⚠️ 账号目录只在**进程启动时**扫描一次（`auth.LoadDir` + `pool.SyncToDir`），运行期不热加载 —— 所以手工新增或替换 auth 文件后必须 `./service.sh restart`。
> ⚠️ **state 是一次性的，且每次 `login url` 都会签发新 state、让旧的立即失效**（state 落在 `/tmp/wb2api-login-state.json`，同一时刻只能有一个登录流程）。所以重跑 `./login.sh` 后**必须打开最新打印的那条链接** —— 你若用旧链接完成授权，脚本等待的却是新 state，会一直等不到而超时。
> ⚠️ 登录在浏览器侧完成，**授权页需要点确认**；仅"浏览器里已登录"并不等于授权完成。

## AutoClaw 上游（可选）

第二上游：智谱 [AutoClaw（澳龙）](https://autoglm.zhipuai.cn/autoclaw) 云端账号体系。`model` 以 `autoclaw/` 前缀路由到该上游，其余模型继续走 WorkBuddy 池，两者互不影响。

### 启用步骤

```bash
# 1. 编译登录工具
export PATH="/opt/homebrew/bin:$PATH"
go build -o wb2api-autoclaw-login ./cmd/autoclaw-login

# 2. 手机验证码登录（两步）
./wb2api-autoclaw-login -phone 138xxxxxxxx          # 发送验证码
./wb2api-autoclaw-login -phone 138xxxxxxxx -code 123456   # 登录 → 落盘 auths/autoclaw-<uid>.json

# 3. 配置启用（config.json 或 env）
#   config.json: { "autoclaw": { "enabled": true } }
#   或 env:      WB2A_AUTOCLAW_ENABLED=1
./service.sh restart   # auth 目录启动时扫描一次，必须重启

# 4. 使用
curl http://127.0.0.1:7863/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"autoclaw/zai_auto","messages":[{"role":"user","content":"hi"}]}'
```

### 模型列表（实测可用）

| 模型 | 说明 |
|---|---|
| `autoclaw/zai_auto` | 官方自动路由（推荐默认） |
| `autoclaw/zai_auto-fast` | 自动路由-快速档 |
| `autoclaw/zai_glm-5.3-flash` | GLM-5.3-Flash（1M 上下文） |
| `autoclaw/zai_glm-5-turbo` | GLM-5-Turbo |
| `autoclaw/zaicoding_glm-5.3` | GLM-5.3（Coding Plan） |
| `autoclaw/tdpsk_deepseek-v4-flash-202605` | DeepSeek-V4.1-Flash |
| `autoclaw/tdpsk_deepseek-v4-pro-202606` | DeepSeek-V4-Pro |

> 注：`tdpsk_deepseek-v4-flash-202605` 是**路由 ID**（即请求头 `X-Request-Model` 的值），AutoClaw 客户端界面把它显示为
> **Deepseek-V4.1-Flash** —— ID 里不含小版本号，别把它误读成 4.0。

### 行为说明

- **token 自管**：服务独立持有 refresh token（`/userapi/v1/refresh`，轮换写回凭据文件），临期自动刷新；与 WorkBuddy 上游同语义。
- **每日签到**：调度器 09/21 点对 autoclaw 账号执行 `daily_signin`（幂等，400 分/次），并刷新积分余额进 `/status`。
- **账号独占**：⚠️ 同一手机号**不要**同时在 AutoClaw 桌面端登录 —— 双方各自轮换 refresh token 会互踢，表现为其中一方频繁 401。
- **错误处理**：401 → 自动刷新重试一次，仍失败标记 `needs_relogin`（`/status` 可见，需重新运行登录命令）；429 → 60s 软冷却；积分不足 → 冷却到次日 04:00；非法模型 → 400 原样透传。
- **协议来源**：AutoClaw.app 1.18.1 客户端逆向 + 实测（详见 `docs/specs/2026-09-11-autoclaw-provider.md`）。**非官方 API**，客户端版本升级可能导致协议漂移（签名/端点/模型白名单变化），届时需更新 `internal/autoclaw` 的常量。

> 想把 `autoclaw/*` 模型接进 Codex CLI / Claude Code，或在 opencodex 面板里直接管理 AutoClaw 账号池，
> 见下文「接入 opencodex」中的 **把 workbuddy / autoclaw 内置进 opencodex** 一节。

## 配置说明

> **权威字段定义见 [`config.example.json`](config.example.json)**：它是当前 schema 的唯一权威，下方样例与之保持一致。`cp config.example.json config.json` 即可得到完整默认配置。

```json
{
  "listen": ":7863",
  "api_key": "your-api-key-here",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "region": "cn",
  "cooldown": {
    "soft_rate": "60s"
  },
  "schedule": {
    "checkin_hours": [9, 21],
    "keepalive_hours": [22]
  },
  "upstream": {
    "timeout_seconds": 120
  },
  "features": {
    "sanitize_blacklist_fingerprints": true
  },
  "upstash": {
    "url": "",
    "token": ""
  },
  "pool": {
    "max_in_flight": 3,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m",
    "breaker_cooldown_max": "6h",
    "idle_weight_per_hour": 0.5,
    "idle_weight_max": 5.0
  },
  "session_sticky": {
    "enabled": true,
    "ttl": "30m",
    "gc_interval": "5m"
  }
}
```

**注意**：`cooldown.hard_credit` / `cooldown.err_threshold` / `cooldown.err_cooldown` 三个历史键已退役。硬冷却固定为**次日 04:00**（本地时区，`CooldownUntilTomorrow4AM`），连续错误语义并入熔断器（`pool.breaker_threshold` 触发指数退避）。旧配置中的这些键因 JSON 未知字段被自然忽略，不报错。

## 安全

### 启动闸门（fail closed）

`api_key` 为空等价于**完全不做鉴权**：任何网络可达者都能调 `/v1/chat/completions` 烧账号积分，或读 `/status` 拿到全部账号的 uid / 昵称 / 积分 / 错误原因。因此启动时有一道硬闸门（`Config.ValidateForServe`）：

| 监听地址 | `api_key` | 行为 |
|---|---|---|
| 非回环（`:7863` / `0.0.0.0` / 实际网卡 IP） | 空或示例占位值 | **拒绝启动**（`log.Fatalf`） |
| 非回环 | 已设置 | 正常启动 |
| 回环（`127.0.0.1` / `::1` / `localhost`） | 空或示例占位值 | 打醒目 WARN 后启动（本机自用合理） |

- "示例占位值"指 `your-api-key-here` / `your-api-key` / `changeme` 等文档样例值——`cp config.example.json config.json` 后忘记改也会被拦下，不会带着众所周知的 key 裸跑。
- 确需在非回环地址无鉴权运行（例如前面已经挂了自带鉴权的反向代理）时，必须**显式**设置 `WB2A_ALLOW_INSECURE_LISTEN=1`，闸门才降级为 WARN。

### API key 校验

- 常量时间比较：用 `crypto/subtle.ConstantTimeCompare` 比较 key 的 **SHA-256 摘要**，避免 `!=` 原文比较带来的逐字节时序侧信道；摘要定长，长度维度同样不泄露。
- `login.sh` 查询账号数用的 key，按 **`API_KEY` 环境变量 → `config.json` 的 `api_key`** 顺序获取，**无内置默认值**——密钥不进入版本库，也不会拿一个写死的值去猜。

### 凭据与历史（运维待办）

- `config.json` / `auths/` / `data/` 已由 `.gitignore` 排除，请勿入库。
- 若曾把真实 key 提交过，**轮换该 key 是必须的**；清理历史需 `git filter-repo` 改写并强推，须协调所有 clone。轮换 + 清理一起做才算完整闭环。
- 建议：`config.json` 权限 `chmod 600`；对外只暴露反向代理，端口（docker-compose 的 `7863:7863`）如需公网访问，优先改为 `127.0.0.1:7863:7863`。

## 账号轮换与冷却策略

### 状态机

```
Healthy → Cooling → (签到恢复) → Healthy
   ↓           ↑
Disabled ←────┘ (session 死亡，永久)
```

### 错误分类

| 错误类型 | 冷却策略 | 恢复方式 |
|---|---|---|
| **402 + 余额关键词** | 冷却到**次日 04:00** | 签到任务（09:00/21:00）自动恢复 |
| **429 限流** | 60s 短冷却 | 到期自动恢复 |
| **401 + session 死亡** | **永久禁用** | 人工重新登录 |
| **404 上游偶发** | 60s 短冷却（不累计错误计数） | 到期自动恢复 |
| **5xx 上游故障** | 喂熔断计数（`pool.breaker_threshold` 触发指数退避熔断） | 熔断到期自动恢复 / 成功清零 |
| **网络抖动** | **不计失败**，立即换号重试 | 即时 |

### 挑选策略

1. **状态过滤**：Disabled / Cooling / 熔断 / 在途占满 不选
2. **Top-5 候选**：按三因子权重降序取前 5（credits 只是权重的一个因子，闲置补偿与成功率同样决定谁进短名单）
3. **三因子加权随机**：权重 = credits 比例 ×10 + 闲置补偿 + 成功率 ×3（credits 全 0 仍按闲置+成功率加权）
4. **防惊群**：跳过 100ms 内刚被选中的账号（除非 top5 全部刚被用过，退回 LRU）

## 账号池 v3

在 v2 基础上吸收外部项目成熟设计，引入四块能力：

- **熔断器（指数退避）**：连续 `pool.breaker_threshold` 次失败熔断，退避 `breaker_cooldown × 2^retryCount` 封顶 `breaker_cooldown_max`；成功清零。单一连续失败计数器 `fails`，签到解冻只清冷却（余额恢复）不动熔断——熔断作为"连续 5xx"信号要到退避到期或下次 chat 成功才恢复。
- **三因子加权选取**：`credits 比例 ×10 + idleWeight + successRate ×3`。闲置补偿每小时 `+idle_weight_per_hour`（封顶 `idle_weight_max`），成功率无记录给中性 1.5。
- **在途租约**：单账号并发上限 `pool.max_in_flight`（0 = 不限），`Pick` 跳过占满账号。
- **会话粘性路由**：同一 `metadata.conversation_id`/`conversation_id`/`metadata.user_id` 尽量绑定同一账号，TTL 滚动续期；请求失败自动解绑回落轮换，请求成功后会话绑定**跟随最终成功号**。
- **全冷却兜底**：无 healthy 账号时从冷却账号选最早到期者顶班（禁用与余额耗尽号永不参与）。

### Redis（Upstash）镜像

- 配置 `upstash.url/token`（空 = 纯内存模式，一切功能照常，只打一条启动警告）。
- Redis 仅做异步镜像（粘性会话映射防重启丢失 + 池状态快照恢复备份），**不在请求热路径同步调用**。
- 池状态快照：每次本地 `state.json` 落盘同步镜像一份到 Redis（带 `saved_at`）；启动时**择新恢复**——Redis 快照比本地新才采用，否则本地优先。
- `/status` 透出 `redis_mode`（`upstash`/`noop`）与池级 `sticky_sessions`。

### 请求级日志

每个 `/v1/chat/completions` 请求结束后打一行表格日志到 stdout：

```
| #001 | 18:31:31 | deepseek-v4 | stream | 200 | uid=0851ce35 | TTFB=801ms | tok=60 | 23.5tok/s | total=2.6s |
```

字段说明：
- `#001`：请求序号（进程级 atomic counter）
- `TTFB`：首 token 到达时间（stream 模式）
- `tok`：输出 token 数（从上游 usage.completion_tokens 精确读取，非估算）
- `uid`：账号 UID 前 8 位

## 工具脚本

| 脚本 | 用途 |
|---|---|
| `./login.sh` | OAuth 登录 → 落盘 auth → 重启服务；key 依次取 `API_KEY` 环境变量 → `config.json` 的 `api_key`，本机回环无鉴权模式下则不带鉴权头直接查 |
| `./service.sh` | macOS 开机自启 / 停止 / 重启 / 状态 / 日志（见上方「开机自启」） |
| `./credit.sh` | 积分日报（美化输出） |
| `./credit.sh -json` | 积分原始 JSON |
| `./signin.sh` | 批量签到（遍历 auths/ 下所有账号） |

## API 端点

除 `/healthz` 外，所有端点都受 `api_key` 保护（配置了 key 就必须带 `Authorization: Bearer <api_key>`）：

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | Bearer | OpenAI 兼容聊天补全（流式/非流式） |
| `GET /v1/models` | Bearer | 模型列表（动态拉取 + 静态兜底） |
| `GET /status` | Bearer | 账号状态汇总（total/healthy/cooling/disabled + 每账号详情） |
| `GET /healthz` | 无 | 健康检查（无健康账号时 503） |

## 接入 opencodex（Codex CLI / Claude Code 获得 WebUI 与统一入口）

本项目**自身没有 WebUI**，只有上面这 4 个 JSON 端点。若想有界面、或把 WorkBuddy CN 账号的额度接进 Codex CLI / Claude Code / Claude Desktop，可把它作为一个 **OpenAI 兼容 provider** 挂到 [opencodex](https://github.com/lidge-jun/opencodex)（本地代理 + Dashboard，默认 `http://localhost:10100`）。

opencodex 侧用 `openai-chat` 适配器即可，因为它本来就是 Chat Completions：

```bash
ocx provider add workbuddy \
  --adapter openai-chat \
  --base-url http://127.0.0.1:7863/v1 \
  --allow-private-network \
  --default-model hy3
ocx sync        # 触发模型发现（会调 /v1/models，把模型写进 Codex 目录）
```

之后 `codex -m "workbuddy/hy3" "..."` 即可走本项目的账号池。

> ⚠️ **`--allow-private-network` 是必需的**：opencodex 默认对私网/回环目标做 SSRF 拦截，缺了它 provider 能在配置里出现但请求永远失败。

> ⚠️ **若你的环境设了 `http_proxy`/`HTTP_PROXY`，必须同时设置 `NO_PROXY` 含 `127.0.0.1,localhost`**，否则模型发现会失败且**没有任何请求日志**：
> ```
> Provider model discovery for "workbuddy" threw Error [urlClass=provider-models, fallback=configured].
> ```
> 原因是 opencodex 在 `provider-outbound.ts` 里对"走代理 + 私网目标 + NO_PROXY 不匹配"直接抛错（错误类名是裸 `Error`，区别于策略拦截的 `ProviderOutboundPolicyError`）。launchd 方式托管时把 NO_PROXY 写进 plist 的 `EnvironmentVariables`，再重启生效。

验证（`<key>` 取 `~/.opencodex/config.json` 的 `apiKeys[0].key`）：

```bash
curl -s http://127.0.0.1:10100/v1/chat/completions \
  -H "Authorization: Bearer <key>" -H 'Content-Type: application/json' \
  -d '{"model":"workbuddy/hy3","messages":[{"role":"user","content":"hi"}]}'
```

撤销：`ocx provider remove workbuddy`。

### 把 workbuddy / autoclaw 内置进 opencodex（OAuth 账号池）

上面 `ocx provider add` 那条路只能拿到 **API-key 池**（且换号只在 429 时触发）。原因是 opencodex 的 OAuth 账号池 ——
`~/.opencodex/auth.json` 里的 `accounts[]`、额度窗口、后台续期 —— **只对内置 provider 开放**：
要求 provider 在 registry 里带 `authKind: "oauth"`，并有内置的登录/刷新实现。

本仓库用**幂等补丁脚本**（[`integrations/opencodex/`](integrations/opencodex/)）把 `workbuddy` 与 `autoclaw`
变成真正的内置 provider，从而拿到完整的 OAuth 池能力：

| 脚本 | 作用 |
|---|---|
| `apply.sh` | 注入 `workbuddy` 内置 provider（registry 条目 + `OAUTH_PROVIDERS` 注册 + 额度显示） |
| `apply_autoclaw.sh` | 注入 `autoclaw` 内置 provider，含 `authHeaderName: X-Authorization` 适配（AutoClaw 网关只认这个头） |
| `apply_gui.sh` | 注入 AutoClaw 账号管理页（静态页 + `/providers` 浮动入口 + iframe 例外） |

```bash
cd integrations/opencodex
./apply.sh && ./apply_autoclaw.sh && ./apply_gui.sh
ocx restart                  # 账号池不进运行时，必须重启
ocx login workbuddy          # 浏览器授权；要加【另一个】账号就用无痕窗口打开打印的 URL
ocx login autoclaw           # 手机验证码 / Google / Z.ai 三种方式
ocx account list workbuddy   # 查看账号池
```

> ⚠️ **每次 `ocx update` 之后都要重跑这三个脚本。** opencodex 的自更新会整体替换 `src/`，补丁会被抹掉，
> 表现为面板里 provider 消失、`ocx login workbuddy` 提示未知 provider。三个脚本都是**幂等**的，重复跑安全；
> 每个都自带备份，`./apply.sh --revert` / `./apply_autoclaw.sh --revert` 可回滚。

> ⚠️ **`./apply.sh --check` 的冒烟校验别跳过。** 它会真正加载模块并断言 `listOAuthProviders()` 含目标 provider ——
> 「补丁文本在位、但注册没生效」（升级后常见的静默失败）只有这一步能抓到。

内置化之后得到的能力：

- **多账号池 + 自动切换**：复用 opencodex 的 `generic-account-failover.ts`，账号可在面板里直接增删
- **后台 token 续期**：`token-guardian.ts` 定期刷新，无需人工干预
- **AutoClaw 账号管理页**：<http://127.0.0.1:10100/autoclaw-accounts.html>
  —— 手机验证码登录 / Google / Z.ai 网页 OAuth（含阿里云滑块），`/providers` 页右下角也有浮动入口
- **额度显示**：provider 卡片上直接显示 WorkBuddy 积分余额 / AutoClaw 积分

两个必踩的配置点：

| 项 | 必须是 | 否则 |
|---|---|---|
| `autoclaw` 的 `baseUrl` | `http://0.0.0.0:7863/v1/autoclaw` | 写 `127.0.0.1` / `localhost` 会被 opencodex 判成「本地」provider → **账户 tab 根本不渲染**（`0.0.0.0` 同样可达本机端口，且不在它的 loopback 名单里） |
| `autoclaw` 的 `authHeaderName` | `X-Authorization` | AutoClaw 网关不认 `Authorization` → 401 |

> 补丁靠**锚点**定位 opencodex 源码。上游重构会让锚点失配，此时脚本会**显式报错并自动回滚**，不会写坏文件。
> 已知 2.58.0 的漂移点：`OAUTH_PROVIDERS` 之后新增了 `DEPRECATED_OAUTH_PROVIDER_ALIASES` 块、
> `providers/quota.ts` 拆包出 `quota/account-cache.ts` + `quota/report-cache.ts`、
> `adapters/openai-chat.ts` 拆包出 `openai-chat/wire.ts`。脚本已改为**结构化定位**（花括号配对）以适配这类变化。
> 若报「锚点匹配 0 次」，说明上游结构又变了，改锚点即可（脚本会指出是哪个文件）。

### 在 Dashboard 里改过 provider 后「模型全部消失」

在 opencodex Dashboard 编辑过这个 provider 后，若模型列表整块变空（`ocx models live --provider workbuddy` 返回 `[]`、Codex 里也看不到），先查这两项：

| 字段 | 必须是 | 若错了会怎样 |
|---|---|---|
| `adapter` | `openai-chat` | 改成 `openai-responses` 后 opencodex 会向上游请求 `/v1/responses`，本项目没这个路由 → 调用返回 `upstream error (404)` |
| `authMode` | **不设**（默认 `key`） | 设成 `oauth` 且没有凭证时，opencodex 直接拒绝发请求：模型发现**静默返回空**（只在 `provider-fetch.ts` 里 `return observed(configured, "degraded")`，**日志一行都不打**），调用则报 `OAuth authentication failed` |

判断依据（直接问运行时，比看配置文件可靠）：

```bash
TOKEN=$(cat ~/.opencodex/admin-api-token)
curl -s "http://127.0.0.1:10100/api/models" -H "Authorization: Bearer $TOKEN" \
  | python3 -c "import json,sys;print(len([r for r in json.load(sys.stdin) if r['provider']=='workbuddy']),'workbuddy rows')"
```

修复（字段掩码 PATCH，改完立即生效，不必重启）：

```bash
curl -s -X PATCH "http://127.0.0.1:10100/api/providers?name=workbuddy" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"adapter":"openai-chat","authMode":""}'      # authMode 传空串 = 清除
ocx sync
```

> `authMode` 的合法值是 `key` / `forward` / `oauth` / `local`（默认 `key`）。本项目是本机回环、无鉴权，**不要设 `oauth`**。
> 在 Dashboard 里「隐藏模型」只会往 `disabledModels` 写一条 `workbuddy/<id>`，**不会影响其它模型**；隐藏某个模型后发现整块消失，一定是上面两项之一的配置问题。

### 账号池放哪边管？

opencodex 有两套池机制，**只有内置 provider 能享受 OAuth 池**：

| | OAuth 池 | API-key 池 |
|---|---|---|
| 代表 | xai、google-antigravity、chatgpt、**（经补丁内置化的）workbuddy / autoclaw** | zai |
| 存储 | `~/.opencodex/auth.json`（含 `accounts[]`、额度窗口、priority） | `provider.apiKeyPool: [{id,key,label}]` |
| 前提 | registry 里 `authKind: "oauth"` + 内置登录/刷新实现 | 任意 provider（含自定义） |
| 自定义 provider | ❌ 默认做不到（需上一节的补丁把它变成内置 provider） | ✅ 可以：`printf '<key>' \| ocx account add-key workbuddy --label X` |
| 轮转 | 按 priority / 额度 / 429 自动切换 | 仅 **429** 触发 failover（冷却 60s，上限 10min） |

两条路线怎么选：

| 你的需求 | 建议 |
|---|---|
| 只想把额度接进 Codex CLI / Claude Code，顺带要个界面 | 用上面的 `ocx provider add`，一条命令 |
| 想要多账号池、429 自动换号、在面板里直接管账号 | 跑 `integrations/opencodex/` 那三个脚本（见上一节）；代价是每次 `ocx update` 后要重跑 |

> 本项目自带的池仍然更强一些：积分加权选择、粘性会话、失败冷却到次日 04:00、熔断指数退避。
> 若你只用本项目、不需要 WebUI，**轮转留在本项目更划算**，opencodex 只当一个 provider 用即可。
> 反过来，想让 opencodex 侧驱动本项目的账号轮转，需要先给本项目加「每个账号一个 api key」的映射（key 钉住账号）。
> 另外 opencodex 内置的 `codebuddy-cn` provider 也支持 key 池，但它走 `codebuddy` **CLI 适配器**（`--tools ""`，
> 不能带工具，需 `npm i -g @tencent-ai/codebuddy-code`），凭证是 `copilot.tencent.com/profile/keys` 的官方 API key ——
> 与 desktop OAuth 会话不是同一套，不可混用。

## 稳定性设计

- **防雪崩**：上游 4xx/5xx 轮转重试（不直接返回），404 短冷却 60s 不累计失败
- **错误分流**：网络层错误不计失败（避免抖动连坐）；HTTP 5xx 喂单一连续失败计数器，达 `breaker_threshold`（默认 3）触发指数退避熔断
- **请求日志**：表格日志（seq/TTFB/uid/tokens/latency）便于排查慢请求
- **连接池**：`MaxIdleConnsPerHost=20` 减少 TLS 握手
- **凭证续期**：token 临近过期自动 refresh，失败禁用账号
- **状态持久化**：`data/state.json` dirty flag + 5s 周期异步落盘，进程退出前强制 flush
- **防惊群**：100ms 窗口内不重复选中同一账号（高并发时打散热点）

## 开发

### 测试

```bash
go build ./...
go test ./... -count=20  # 20 次全绿（无 flake）
go vet ./...
gofmt -l .  # 应为空
```

### 代码结构

```
cmd/
  server/     # 主服务入口
  login/      # OAuth 登录工具
  credit/     # 积分查询工具
  signin/     # 批量签到工具
internal/
  auth/       # auth 文件解析 + token 刷新
  pool/       # 账号池（状态机 + 冷却 + 持久化）
  scheduler/  # 定时签到 + 积分查询
  server/     # HTTP handler + 请求日志
  upstream/   # 上游 API 封装（chat/billing/auth）
```

## 免责声明

本项目仅供学习和研究使用。使用者需遵守 WorkBuddy / CodeBuddy 的服务条款，自行承担使用风险。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

MIT
