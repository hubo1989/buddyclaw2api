// errors.go 错误分类：驱动单账号冷却与 /status 观测。
package autoclaw

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrKind 错误分类。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 积分不足 → 长冷却（到次日 04:00）
	ErrSoftRate                   // 429 → 短冷却
	ErrAuth                       // 401 → token 失效（刷新一次，仍失败则标记 needs_relogin）
	ErrNotFound                   // 404 → 协议漂移嫌疑
	ErrServer                     // 5xx / 网络错
	ErrClient                     // 其他 4xx / 业务错误
	ErrDesktopDown                // 桌面端模式专用（S2 不用，保留枚举对齐）
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrAuth:
		return "auth"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrDesktopDown:
		return "desktop_down"
	default:
		return "none"
	}
}

// Error 带分类的错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("autoclaw %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// asError errors.As 的包内封装。
func asError(err error, target **Error) bool { return errors.As(err, target) }

// creditMarkers 积分不足关键词（中英双语，对齐 workbuddy 上游的 hardMarkers 思路）。
var creditMarkers = []string{
	"insufficient", "not enough", "balance", "quota exceeded", "point",
	"积分不足", "余额不足", "额度不足", "积分用完", "点数不足",
}

// isCreditMsg 判断业务 msg 是否积分不足。
func isCreditMsg(msg string) bool {
	lower := strings.ToLower(msg)
	for _, m := range creditMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// ClassifyHTTP 按 HTTP 状态码与 body 分类（chat 非 2xx 路径）。
func ClassifyHTTP(status int, body string) ErrKind {
	switch {
	case status == http.StatusUnauthorized:
		return ErrAuth
	case status == http.StatusPaymentRequired:
		return ErrHardCredit
	case status == http.StatusTooManyRequests:
		return ErrSoftRate
	case status == http.StatusNotFound:
		return ErrNotFound
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrClient
	}
	if isCreditMsg(body) {
		return ErrHardCredit
	}
	return ErrNone
}
