// headers.go 两类请求头：业务签名头（task/wallet/登录/refresh）与对话头（model-proxy）。
package autoclaw

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

// randomHex 生成 n 字节的十六进制随机串（请求 ID / 设备 ID 用）。
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// commonBase 返回业务头公共部分（不含 Authorization）。
func commonBase() http.Header {
	h := http.Header{}
	ts := fmtUnix(now())
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("X-Version", ClientVersion)
	h.Set("X-Tm", PlatformTm)
	h.Set("X-Product", Product)
	h.Set("X-Auth-Appid", AppID)
	h.Set("X-Auth-TimeStamp", ts)
	h.Set("X-Auth-Sign", authSign(ts))
	h.Set("X-Trace-Id", randomHex(16))
	h.Set("X-Lang", "zh-CN")
	h.Set("X-Channel", Channel)
	// WAF 按 UA 放行：非 AutoClaw 客户端 UA（含 Go/bun 默认 UA）会被直接 405，拿不到业务信封。
	h.Set("User-Agent", UserAgent)
	h.Set("Origin", CallbackOrigin)
	h.Set("Referer", CallbackOrigin+"/")
	return h
}

// now 便于测试注入的时间源。
var now = time.Now

// BizHeaders 业务签名头 + Bearer（task/wallet/user-info）。
func BizHeaders(accessToken string) http.Header {
	h := commonBase()
	if accessToken != "" {
		h.Set("Authorization", "Bearer "+accessToken)
	}
	return h
}

// AnonHeaders 未登录业务头（send-code / agent-login）。
func AnonHeaders() http.Header { return commonBase() }

// ChatHeaders 对话头（model-proxy 形态：X-Authorization + 路由信息，无签名三件套）。
func ChatHeaders(accessToken, routeModelID string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("X-Authorization", "Bearer "+accessToken)
	h.Set("X-Request-Id", randomHex(16))
	h.Set("X-Request-Model", routeModelID)
	h.Set("X-Client-Type", "pc")
	h.Set("X-Product", Product)
	h.Set("X-Harness-Type", "zcode")
	h.Set("X-Tm", PlatformTm)
	h.Set("X-Version", ClientVersion)
	h.Set("X-Lang", "zh-CN")
	h.Set("x_trace_id", "autoclaw-desktop")
	h.Set("X-Channel", Channel)
	return h
}
