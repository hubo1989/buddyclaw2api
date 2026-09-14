// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json
	Region    string `json:"region"`     // 只收 "cn"

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "60s"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
	} `json:"schedule"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 默认 120
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int     `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string  `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	Autoclaw struct {
		Enabled            bool   `json:"enabled"`              // 默认 false，零影响
		Host               string `json:"host"`                 // 默认官方生产 host
		AuthDir            string `json:"auth_dir"`             // 默认同顶层 auth_dir（autoclaw-*.json）
		TimeoutSeconds     int    `json:"timeout_seconds"`      // 上游 HTTP 超时，默认 120
		RefreshSkew        string `json:"refresh_skew"`         // token 提前刷新窗口，默认 "30m"
		SoftRate           string `json:"soft_rate"`            // 429 冷却，默认 "60s"
		BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败熔断阈值，默认 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // 熔断基础时长，默认 "1m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // 熔断封顶，默认 "30m"
		DefaultModel       string `json:"default_model"`        // 预留：默认路由模型
	} `json:"autoclaw"`

	// 解析后
	SoftRateDur         time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
	AutoclawRefreshSkew time.Duration `json:"-"`
	AutoclawSoftDur     time.Duration `json:"-"`
	AutoclawBreakerDur  time.Duration `json:"-"`
	AutoclawBreakerMax  time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
		Region:    "cn",
	}
	c.Cooldown.SoftRate = "60s"
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.KeepaliveHours = []int{22}
	c.Upstream.TimeoutSeconds = 120
	c.Features.SanitizeBlacklistFingerprints = true
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	c.Autoclaw.Host = ""
	c.Autoclaw.TimeoutSeconds = 120
	c.Autoclaw.RefreshSkew = "30m"
	c.Autoclaw.SoftRate = "60s"
	c.Autoclaw.BreakerThreshold = 3
	c.Autoclaw.BreakerCooldown = "1m"
	c.Autoclaw.BreakerCooldownMax = "30m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_REGION"); v != "" {
		c.Region = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_AUTOCLAW_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Autoclaw.Enabled = b
		}
	}
	if v := os.Getenv("WB2A_AUTOCLAW_HOST"); v != "" {
		c.Autoclaw.Host = v
	}
	if v := os.Getenv("WB2A_AUTOCLAW_AUTH_DIR"); v != "" {
		c.Autoclaw.AuthDir = v
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.Region == "" {
		c.Region = "cn"
	}
	c.Region = strings.ToLower(c.Region)
	if c.Region != "cn" && c.Region != "global" {
		return fmt.Errorf("region must be cn or global, got %q", c.Region)
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// autoclaw 时长字段（enabled=false 也解析，配置错误尽早暴露）
	if c.AutoclawRefreshSkew, err = c.durOr(c.Autoclaw.RefreshSkew, "30m", "autoclaw.refresh_skew"); err != nil {
		return err
	}
	if c.AutoclawSoftDur, err = c.durOr(c.Autoclaw.SoftRate, "60s", "autoclaw.soft_rate"); err != nil {
		return err
	}
	if c.AutoclawBreakerDur, err = c.durOr(c.Autoclaw.BreakerCooldown, "1m", "autoclaw.breaker_cooldown"); err != nil {
		return err
	}
	if c.AutoclawBreakerMax, err = c.durOr(c.Autoclaw.BreakerCooldownMax, "30m", "autoclaw.breaker_cooldown_max"); err != nil {
		return err
	}
	if c.Autoclaw.BreakerThreshold <= 0 {
		c.Autoclaw.BreakerThreshold = 3
	}
	if c.Autoclaw.TimeoutSeconds <= 0 {
		c.Autoclaw.TimeoutSeconds = 120
	}
	return nil
}

// durOr 解析 duration 字符串，空值取 def，非法值返回带字段名的错误。
func (c *Config) durOr(v, def, field string) (time.Duration, error) {
	if v == "" {
		v = def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return d, nil
}

// placeholderAPIKeys 是 config.example.json / README 样例里出现的示例占位值。
// 用户 `cp config.example.json config.json` 后忘记修改，就会拿到一个"非空但众所周知"
// 的 key —— 那等于把鉴权做成摆设，还会绕过下方的空 key 检查。因此一律视同未设置。
var placeholderAPIKeys = map[string]bool{
	"your-api-key-here": true,
	"your-api-key":      true,
	"changeme":          true,
	"change-me":         true,
}

// ValidateForServe 是"即将监听端口"之前的安全闸门，与 Load/normalize 的纯解析职责分离。
//
// 不变量：APIKey == ""（或等于示例占位值）等价于完全不做鉴权。此时若监听地址不是回环，
// 则任何网络可达者都能：
//   - 调用 /v1/chat/completions 白嫖并烧掉账号积分；
//   - 读取 /status 拿到全部账号的 uid / 昵称 / 积分 / 错误原因。
//
// 因此：空 key + 非回环监听 = 拒绝启动（fail closed）。回环监听只告警（本机自用是合理场景）。
// 若确需在非回环地址上裸跑（例如前面挂了自带鉴权的反向代理），必须显式设置
// WB2A_ALLOW_INSECURE_LISTEN=1 才降级为醒目告警。
func (c *Config) ValidateForServe() error {
	if !c.hasUsableAPIKey() {
		if isLoopbackListen(c.Listen) {
			log.Printf("WARN: api_key 为空或仍是示例占位值 — /v1/* 与 /status 形同不鉴权（listen=%s 仅回环可达，风险可控）", c.Listen)
			return nil
		}
		if envTruthy(os.Getenv("WB2A_ALLOW_INSECURE_LISTEN")) {
			log.Printf("WARN: api_key 为空或仍是示例占位值，且 listen=%s 非回环 — 服务对网络基本开放；"+
				"已检测到 WB2A_ALLOW_INSECURE_LISTEN，按显式授权继续启动", c.Listen)
			return nil
		}
		return fmt.Errorf("拒绝启动：api_key 为空或仍是示例占位值（等于不鉴权），而 listen=%q 不是回环地址，会把 /v1/chat/completions 与 /status 暴露给任意网络可达者。"+
			"请设置 config 的 api_key 或环境变量 WB2A_API_KEY；或改为监听 127.0.0.1；"+
			"确需无鉴权监听时显式设置 WB2A_ALLOW_INSECURE_LISTEN=1", c.Listen)
	}
	return nil
}

// hasUsableAPIKey 判断 key 是否真的能起鉴权作用（非空且不是示例占位值）。
func (c *Config) hasUsableAPIKey() bool {
	k := strings.TrimSpace(c.APIKey)
	if k == "" {
		return false
	}
	return !placeholderAPIKeys[strings.ToLower(k)]
}

// isLoopbackListen 判断监听地址是否仅本机可达。
// ":7863" / "0.0.0.0:7863" / "[::]:7863" 均表示全部网卡 → false。
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	// 空 host（如 ":7863"）表示绑定所有网卡，不是回环。
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
