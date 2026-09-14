// credentials.go AutoClaw 账号凭据：手机验证码登录、多账号落盘、原子写回。
package autoclaw

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Credentials 单账号凭据（auths/autoclaw-<uid>.json，0600）。
// 服务独占该账号的 refresh：不要与 AutoClaw 桌面端登录同一账号，refresh token 轮换会互踢。
type Credentials struct {
	mu sync.Mutex

	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // Unix 秒；access token 实测约 6h
	DeviceID     string `json:"deviceId"`  // 每账号独立 64hex 设备标识
	Phone        string `json:"phone"`     // 登录手机号（明文，本地文件）
	UID          string `json:"uid"`       // 服务端 user_id
	Nickname     string `json:"nickname"`
	// Host 该账号登录时使用的 API host（空 = 默认国际 host）。
	// 手机号登录在国际 host 上可能被「当前地区暂不支持手机号注册」(630015) 拒绝，
	// 需要落到国内 host；token 与 host 绑定，所以后续 refresh/chat 必须走同一个 host。
	Host     string `json:"host,omitempty"`
	FilePath string `json:"-"`
}

// Lock/Unlock 供 upstream 刷新期间持锁（与 workbuddy auth.Auth 同构）。
func (c *Credentials) Lock()   { c.mu.Lock() }
func (c *Credentials) Unlock() { c.mu.Unlock() }

// NeedsRefresh 报告 token 是否将在 within 内过期。
func (c *Credentials) NeedsRefresh(within time.Duration) bool {
	if c.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= c.ExpiresAt
}

// SaveAtomic 原子写回（tmp+rename，0600）。
func (c *Credentials) SaveAtomic() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(c.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s phone=%s)", c.UID, maskPhone(c.Phone))
	}
	if c.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	c.DeviceID = strings.TrimSpace(c.DeviceID)
	if c.DeviceID == "" {
		c.DeviceID = newDeviceID()
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.FilePath)
}

// maskPhone 手机号脱敏（仅日志用）。
func maskPhone(p string) string {
	if len(p) < 7 {
		return "***"
	}
	return p[:3] + "****" + p[len(p)-4:]
}

// NewDeviceID 导出的设备 ID 生成器（login CLI 用）。
func NewDeviceID() string { return newDeviceID() }

// MaskPhoneExport 导出的手机号脱敏（login CLI 用）。
func MaskPhoneExport(p string) string { return maskPhone(p) }

// LoadDir 扫描 dir 下全部 autoclaw-*.json 凭据。
// 文件损坏的静默跳过（由调用方统计并告警）。
func LoadDir(dir string) ([]*Credentials, error) {
	files, err := filepath.Glob(filepath.Join(dir, "autoclaw-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Credentials
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var c Credentials
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		if strings.TrimSpace(c.RefreshToken) == "" {
			continue // 无 refresh 的残档不可用
		}
		c.FilePath = f
		if strings.TrimSpace(c.DeviceID) == "" {
			c.DeviceID = newDeviceID()
		}
		out = append(out, &c)
	}
	return out, nil
}
