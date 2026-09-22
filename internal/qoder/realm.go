// Package qoder 实现 Qoder（阿里 AI IDE）上游的直连协议：
// 设备流登录（CN/国际双域）、PAT 换 job token、COSY 签名、WAF-bypass body 编码、
// SSE 信封解包与 OpenAI 兼容出站。
//
// 协议来源：9router（github.com/decolua/9router，MIT）open-sse/shared/qoder/* 与
// src/lib/oauth/services/qoder.js，逐常量核对移植；国际版端点已在 9router 实测。
// Qoder CN 为独立部署（qoder.cn / api.qoder.com.cn，独立账号体系），直连聊天端点
// 尚无公开逆向——CN 域名可经环境变量覆盖，聊天端点未验证前 fail-fast 拒绝。
package qoder

import "os"

// Realm 账号域：国际版（Global）或国内版（CN）。
type Realm string

const (
	RealmGlobal Realm = "global"
	RealmCN     Realm = "cn"
)

// RealmOf 解析 realm 字符串；非法值回落 global（与 login --realm 语义一致）。
func RealmOf(s string) Realm {
	if s == string(RealmCN) {
		return RealmCN
	}
	return RealmGlobal
}

// Endpoints 单域端点集合。
type Endpoints struct {
	// OpenAPI：设备流轮询、userinfo、配额、PAT 换 job token（普通 JSON，不签名）。
	OpenAPIBase string
	// Center：token 刷新（国际版设备流实测 403，refresh 为 no-op）。
	CenterBase string
	// ChatBase/ChatBaseAlt：推理域。dt- 设备 token 走 ChatBase；jt- job token
	// 被国际版 ChatBase 以 "Login expired" 403 拒绝，必须走 ChatBaseAlt。
	ChatBase    string
	ChatBaseAlt string
	// LoginPage 浏览器授权落地页（device 流把 challenge/nonce 拼到 query 上）。
	LoginPage string
}

// 环境变量覆盖开关：CN 端点未固化，允许实测抓包后不改代码切换。
const (
	EnvCNOpenAPI   = "QODER_CN_OPENAPI_BASE"
	EnvCNChat      = "QODER_CN_CHAT_BASE"
	EnvCNLoginPage = "QODER_CN_LOGIN_PAGE"
)

// 国际版端点（9router 常量逐字移植）。
const (
	globalOpenAPI   = "https://openapi.qoder.sh"
	globalCenter    = "https://center.qoder.sh"
	globalChat      = "https://api3.qoder.sh"
	globalChatAlt   = "https://api2.qoder.sh"
	globalLoginPage = "https://qoder.com/device/selectAccounts"

	cnLoginPageDefault = "https://qoder.cn/device/selectAccounts"
)

// NewEndpoints 按 realm 返回端点集合。
func NewEndpoints(realm Realm) Endpoints {
	if realm == RealmCN {
		return Endpoints{
			OpenAPIBase: envOr(EnvCNOpenAPI, "https://openapi.qoder.com.cn"),
			CenterBase:  envOr(EnvCNOpenAPI, "https://openapi.qoder.com.cn"),
			ChatBase:    envOr(EnvCNChat, "https://gateway.qoder.com.cn"),
			ChatBaseAlt: envOr(EnvCNChat, ""),
			LoginPage:   envOr(EnvCNLoginPage, cnLoginPageDefault),
		}
	}
	return Endpoints{
		OpenAPIBase: globalOpenAPI,
		CenterBase:  globalCenter,
		ChatBase:    globalChat,
		ChatBaseAlt: globalChatAlt,
		LoginPage:   globalLoginPage,
	}
}

// openapi/center 域上的固定路径。CN 路径经 qoderclicn 1.1.58 二进制字符串提取核实：
// openapi.qoder.com.cn 轮询已实测结构化响应；gateway 推理域对无认证流量 ALB 503，
// 携带真实 token 的行为待登录后验证。
const (
	DeviceTokenPollPath    = "/api/v1/deviceToken/poll"
	DeviceTokenRefreshPath = "/api/v1/deviceToken/refresh"
	UserInfoPath           = "/api/v1/userinfo"
	QuotaUsagePath         = "/api/v2/quota/usage"
	JobTokenExchangePath   = "/api/v1/jobToken/exchange"
	RefreshTokenPath       = "/algo/api/v3/user/refresh_token"
	// ModelListPath / ChatSigPath：推理域路径（COSY 签名）。
	ModelListPath = "/algo/api/v2/model/list"
	ChatSigPath   = "/api/v2/service/pro/sse/agent_chat_generation"
)

// CNClientID CN CLI 设备流登录 URL 的 client_id（qoderclicn 1.1.58 内嵌常量默认
// 分支；仅登录页 URL 使用，轮询端不校验）。
const CNClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"

// envOr 读环境变量，非空则用之，否则回落 def。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
