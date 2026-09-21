// client.go Qoder 上游 HTTP 客户端：设备流登录、PAT 换 job token、userinfo。
// 端点契约移植自 9router src/lib/oauth/services/qoder.js（device 流）与
// open-sse/services/qoderModels.js（PAT 交换）。
package qoder

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const clientHTTPTimeout = 15 * time.Second

// Client 按域隔离的 HTTP 客户端（复用连接池）。
type Client struct {
	HTTP *http.Client
}

// NewClient 构造默认客户端。
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: clientHTTPTimeout}}
}

// DeviceFlowStart 本地生成 PKCE+nonce+machineId，返回浏览器授权 URL。
// 轮询端点用 nonce+verifier 定位会话（上游不签发 device_code）。
type DeviceFlowStart struct {
	AuthURL   string // 浏览器打开
	Nonce     string
	Verifier  string
	Challenge string
	MachineID string
}

func (c *Client) DeviceFlowStartURL(realm Realm) (*DeviceFlowStart, error) {
	verifier, challenge, err := pkcePair()
	if err != nil {
		return nil, err
	}
	fs := &DeviceFlowStart{
		Nonce:     newUUIDv4(),
		Verifier:  verifier,
		Challenge: challenge,
		MachineID: NewMachineID(),
	}
	q := url.Values{
		"challenge":        {fs.Challenge},
		"challenge_method": {"S256"},
		"machine_id":       {fs.MachineID},
		"nonce":            {fs.Nonce},
	}
	if realm == RealmCN {
		q.Set("client_id", CNClientID)
	}
	fs.AuthURL = NewEndpoints(realm).LoginPage + "?" + q.Encode()
	return fs, nil
}

// DeviceTokenPollResult 单次轮询结果。
type DeviceTokenPollResult struct {
	Pending      bool
	Token        string // dt-...
	RefreshToken string
	UserID       string
	ExpiresAt    time.Time
}

// DeviceTokenPoll 单次轮询设备 token。202/404 = 未完成；200+token = 完成；
// 其余状态为终态错误。逐字对照 9router pollDeviceToken 的状态机。
func (c *Client) DeviceTokenPoll(realm Realm, nonce, verifier string) (*DeviceTokenPollResult, error) {
	ep := NewEndpoints(realm)
	u := ep.OpenAPIBase + DeviceTokenPollPath +
		"?nonce=" + url.QueryEscape(nonce) +
		"&verifier=" + url.QueryEscape(verifier) +
		"&challenge_method=S256"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// UA 与上游一致（qodercli 的 Go http 口径）。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return &DeviceTokenPollResult{Pending: true}, nil
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("poll read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device token poll failed: HTTP %d: %.200s", resp.StatusCode, string(raw))
	}
	var body struct {
		Token        string          `json:"token"`
		RefreshToken string          `json:"refresh_token"`
		UserID       string          `json:"user_id"`
		ExpiresAt    json.RawMessage `json:"expires_at"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("device token poll: invalid JSON: %w", err)
	}
	if body.Token == "" {
		return nil, fmt.Errorf("device token poll returned 200 but no token")
	}
	return &DeviceTokenPollResult{
		Token:        body.Token,
		RefreshToken: body.RefreshToken,
		UserID:       body.UserID,
		ExpiresAt:    parseExpiry(body.ExpiresAt, body.ExpiresIn),
	}, nil
}

// parseExpiry 上游过期口径宽容解析：ms 纪元（数字/纯数字串）、RFC3339、
// 相对秒（expires_in）；全部缺失回落 now+30 天（设备 token 实测约 30 天）。
func parseExpiry(expiresAt, expiresInSeconds json.RawMessage) time.Time {
	if v, ok := asFloat(expiresAt); ok && v > 1e12 {
		return time.UnixMilli(int64(v))
	}
	if s, ok := asString(expiresAt); ok && s != "" {
		if ms, err := strconv.ParseFloat(s, 64); err == nil && ms > 1e12 {
			return time.UnixMilli(int64(ms))
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	if v, ok := asFloat(expiresInSeconds); ok && v >= 0 {
		return time.Now().Add(time.Duration(v) * time.Second)
	}
	return time.Now().Add(30 * 24 * time.Hour)
}

func asFloat(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	return 0, false
}

func asString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}

// UserInfo 登录完成后拉取展示资料（name/username、email、organization_id）。
// 失败不阻塞登录——返回空串。
type UserInfo struct {
	Name           string
	Email          string
	OrganizationID string
	UserID         string
}

func (c *Client) UserInfo(realm Realm, accessToken string) UserInfo {
	ep := NewEndpoints(realm)
	req, err := http.NewRequest(http.MethodGet, ep.OpenAPIBase+UserInfoPath, nil)
	if err != nil {
		return UserInfo{}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return UserInfo{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return UserInfo{}
	}
	var body struct {
		ID             string `json:"id"`
		UserID         string `json:"userId"`
		UserIDSnake    string `json:"user_id"`
		Name           string `json:"name"`
		Username       string `json:"username"`
		Email          string `json:"email"`
		OrganizationID string `json:"organization_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return UserInfo{}
	}
	return UserInfo{
		UserID:         firstNonEmpty(body.ID, body.UserID, body.UserIDSnake),
		Name:           strings.TrimSpace(firstNonEmpty(body.Name, body.Username)),
		Email:          strings.TrimSpace(body.Email),
		OrganizationID: strings.TrimSpace(body.OrganizationID),
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// QuotaUsageRaw 拉取配额用量原始 JSON（openapi 域 /api/v2/quota/usage，Bearer 设备
// token）。注意：该端点对 bun fetch 的 TLS 指纹返回 401 TOKEN_INVALID，curl/Go 均
// 200——因此生产路径由 Go 网关代理，opencodex 侧不直连。
func (c *Client) QuotaUsageRaw(realm Realm, accessToken string) ([]byte, int, error) {
	ep := NewEndpoints(realm)
	req, err := http.NewRequest(http.MethodGet, ep.OpenAPIBase+QuotaUsagePath, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, err
}

// LimitedNumberResult 每日限量名额（client_launch_26 活动：名额持有者每日获
// Credits 附加包 +100）。实测（2026-09-20，双域账号）：hasNumber=true 且
// number/createdAt 为 9-18 领取时的值。
type LimitedNumberResult struct {
	HasNumber bool   `json:"hasNumber"`
	Number    int64  `json:"number"`
	CreatedAt string `json:"createdAt"`
}

// LimitedNumber 查询/领取每日限量名额（app 端「每天领 100」入口即轮询此端点）。
const (
	CampaignPath        = "/sash/api/v1/me/campaigns"
	CampaignLimitedPath = "/sash/api/v1/me/campaigns/client_launch_26/limited-number"
)

func (c *Client) LimitedNumber(realm Realm, accessToken string) (*LimitedNumberResult, error) {
	ep := NewEndpoints(realm)
	req, err := http.NewRequest(http.MethodGet, ep.OpenAPIBase+CampaignLimitedPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("limited-number: HTTP %d: %.200s", resp.StatusCode, string(raw))
	}
	var out LimitedNumberResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("limited-number: invalid JSON: %w", err)
	}
	return &out, nil
}

// CampaignsResult 营销活动观测（/sash/api/v1/me/campaigns）。
// 实测（2026-09-20，双域账号）：无活动时 showCampaign=false、claimable=false、
// campaigns=[]。签到/赠送类活动上线时 claimable=true（领取端点形态未知，
// 需抓到真实 campaign 样本后再实现自动领取）。
type CampaignsResult struct {
	UID          string           `json:"uid"`
	ShowCampaign bool             `json:"showCampaign"`
	Claimable    bool             `json:"claimable"`
	CampaignURL  string           `json:"campaignUrl"`
	Campaigns    []map[string]any `json:"campaigns"`
}

// Campaigns 拉取账号可见的运营活动列表（普通 JSON，无 COSY）。
func (c *Client) Campaigns(realm Realm, accessToken string) (*CampaignsResult, error) {
	ep := NewEndpoints(realm)
	req, err := http.NewRequest(http.MethodGet, ep.OpenAPIBase+"/sash/api/v1/me/campaigns", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return &CampaignsResult{}, nil // 无活动：等价空列表
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("campaigns: HTTP %d: %.200s", resp.StatusCode, string(raw))
	}
	var out CampaignsResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("campaigns: invalid JSON: %w", err)
	}
	return &out, nil
}

// JobToken PAT 换出的 job token（jt-，约 24h）。
type JobToken struct {
	Token        string
	RefreshToken string
	ExpiresAt    time.Time
}

// ExchangeJobToken PAT（pt-）→ job token（jt-）。普通 JSON POST，不做 COSY 签名。
// PAT 无法直接签名 COSY 请求（9router qoderModels.js 同口径）。
func (c *Client) ExchangeJobToken(realm Realm, personalToken string) (*JobToken, error) {
	ep := NewEndpoints(realm)
	body := `{"personal_token":` + jsonQuote(personalToken) + `}`
	req, err := http.NewRequest(http.MethodPost, ep.OpenAPIBase+JobTokenExchangePath, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "qodercli/1.0.0")
	req.Header.Set("Cosy-Version", CosyIDEVersion)
	req.Header.Set("Cosy-ClientType", CosyClientType)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("qoder PAT exchange failed: HTTP %d: %.200s", resp.StatusCode, string(raw))
	}
	var out struct {
		Token        string          `json:"token"`
		RefreshToken string          `json:"refresh_token"`
		ExpiresAt    json.RawMessage `json:"expires_at"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("qoder PAT exchange: invalid JSON: %w", err)
	}
	if out.Token == "" {
		return nil, fmt.Errorf("qoder PAT exchange returned no job token")
	}
	return &JobToken{
		Token:        out.Token,
		RefreshToken: out.RefreshToken,
		ExpiresAt:    parseExpiry(out.ExpiresAt, out.ExpiresIn),
	}, nil
}

// jsonQuote 极薄 JSON 字符串包装（避免为单字段引入 map 分配）。
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
