// Package autoclaw 实现智谱 AutoClaw（澳龙）云端账号体系的上游接入。
//
// 与 workbuddy 上游（CodeBuddy 协议）并存，提供：
//   - 手机验证码登录（send-code / agent-login），每账号一个凭据文件 auths/autoclaw-<uid>.json
//   - 自管 token 刷新（/userapi/v1/refresh，refresh token 轮换，服务独占账号不与桌面端共享）
//   - 每日签到（autoclaw-task-list / task-complete, task_id=daily_signin，幂等）
//   - 积分余额（/agent-assetmgr/api/v2/wallets）
//   - OpenAI 兼容 chat 代理（/autoclaw-proxy/proxy/autoclaw/v1/chat/completions，流式+非流式）
//
// 协议来自 AutoClaw.app 1.18.1 客户端逆向 + 实测（2026-09-11），见 docs/specs/2026-09-11-autoclaw-provider.md。
// 协议私有且可能随客户端版本漂移：所有常量集中于此，失效时优先怀疑版本升级。
package autoclaw

import (
	"crypto/md5"
	"encoding/hex"
	"strconv"
	"time"
)

// 协议常量（客户端公开内置值，随安装包分发；AutoClaw 1.18.1 实测）。
const (
	// AppID 客户端签名 appid。
	AppID = "100003"
	// AppKey 客户端签名密钥（asar 内明文常量，非机密）。
	AppKey = "38d2391985e2369a5fb8227d8e6cd5e5"
	// Product 固定产品标识。
	Product = "autoclaw"
	// Channel 安装渠道（客户端 channel.json 默认值）。
	Channel = "AutoClaw4"
	// ClientVersion 上报的客户端版本（X-Version）。
	ClientVersion = "1.18.1"
	// UserAgent 客户端 UA：WAF 按 UA 放行，缺失或非 AutoClaw UA 会直接 405。
	UserAgent = "AutoClaw/1.18.1 (mac-arm64)"
	// CallbackOrigin OAuth 回调来源（客户端回环 TokenServer 端口池首端口）。
	CallbackOrigin = "http://localhost:18432"
	// PlatformTm X-Tm 平台标识（darwin=mac）。
	PlatformTm = "mac"

	// DefaultHost 生产 API host（国际版，客户端 Oversea 构建使用）。
	DefaultHost = "https://autoglm-api.autoglm.ai"
	// CNHost 国内版 API host。同一 appid，但手机号注册/登录的地区策略不同：
	// 国际 host 对未注册的 +86 号码返回 630015「当前地区暂不支持手机号注册」。
	CNHost = "https://autoglm-api.zhipuai.cn"
	// AccelHost 国内加速 host（客户端亦使用；实测发码频率限制更严）。
	AccelHost = "https://autoglm-acceleration-api.zhipuai.cn"

	// PathChat 对话端点（OpenAI 兼容）。
	PathChat = "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions"
	// PathTaskList 任务列表（含 daily_signin 状态）。
	PathTaskList = "/autoclaw-proxy/proxy/autoclaw-task-list"
	// PathTaskComplete 任务完成（签到执行）。
	PathTaskComplete = "/autoclaw-proxy/proxy/autoclaw-task-complete"
	// PathWallets 积分钱包。
	PathWallets = "/agent-assetmgr/api/v2/wallets?biz_app_id=" + Product
	// PathSendCode 发送手机验证码。
	PathSendCode = "/userapi/v1/agent-send-code"
	// PathLogin 手机验证码登录。
	PathLogin = "/userapi/v1/agent-login/"
	// PathRefresh token 刷新（refresh token 轮换）。
	PathRefresh = "/userapi/v1/refresh"
	// PathRefreshFallback 刷新降级端点（签名校验失败 code=400002 时客户端用它）。
	PathRefreshFallback = "/userapi/v1/agent-refresh"
	// PathUserInfo 用户信息（登录态有效性探测）。
	PathUserInfo = "/userapi/v1/user-info"

	// PathOverseaGoogleOAuthURL 海外 Google OAuth：获取授权页 URL。
	PathOverseaGoogleOAuthURL = "/userapi/overseasv1/google-oauth-url"
	// PathOverseaZaiOAuthURL 海外 zai OAuth：获取授权页 URL。
	PathOverseaZaiOAuthURL = "/userapi/overseasv1/zai-oauth-url"
	// PathOverseaGoogleLogin 海外 Google OAuth：code 换 token。
	PathOverseaGoogleLogin = "/userapi/overseasv1/google-oauth-login"
	// PathOverseaZaiLogin 海外 zai OAuth：code 换 token。
	PathOverseaZaiLogin = "/userapi/overseasv1/zai-oauth-login"
	// PathOverseaOAuthCaptchaConfig 海外 OAuth 人机验证配置（aliyun 滑块参数）。
	PathOverseaOAuthCaptchaConfig = "/userapi/overseasv1/oauth-captcha-config"

	// TaskDailySignin 每日签到任务 ID。
	TaskDailySignin = "daily_signin"
)

// authSign 计算业务签名：md5("{appid}&{timestamp}&{appkey}")。
func authSign(timestamp string) string {
	sum := md5.Sum([]byte(AppID + "&" + timestamp + "&" + AppKey))
	return hex.EncodeToString(sum[:])
}

// newDeviceID 生成 64 位十六进制设备标识（形态对齐客户端 identity/device.json）。
// 每个凭据文件持有独立的 device_id，避免多账号共用设备指纹。
func newDeviceID() string { return randomHex(32) }

// fmtUnix 格式化 Unix 秒。
func fmtUnix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
