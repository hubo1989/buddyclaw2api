// client.go AutoClaw 云端 HTTP 客户端：登录/刷新/签到/积分/对话。
package autoclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// numericCodeValue 验证码按数字发送（对齐官方客户端 normalizeCode 的 Number() 转换）；
// 非纯数字时原样返回，避免把异常输入变成 0。
func numericCodeValue(code string) any {
	s := strings.TrimSpace(code)
	if s == "" {
		return code
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return s
}

// HTTP doJSON 的可注入传输层（测试用）。生产为 nil，走内部 client。
type HTTP interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client AutoClaw 云端客户端。Host 可覆盖便于测试。
type Client struct {
	Host string
	HTTP HTTP

	hc *http.Client
}

// NewClient 生产构造。
func NewClient(host string) *Client {
	if host == "" {
		host = DefaultHost
	}
	return &Client{
		Host: strings.TrimRight(host, "/"),
		HTTP: &http.Client{Timeout: 60 * time.Second},
	}
}

// WithHost 返回一个指向另一 host 的浅拷贝（共享同一条 HTTP 传输层）。
// 用于「凭据与 host 绑定」的场景：手机号账号可能落在国内 host。
func (c *Client) WithHost(host string) *Client {
	h := strings.TrimRight(strings.TrimSpace(host), "/")
	if h == "" || h == c.Host {
		return c
	}
	cp := *c
	cp.Host = h
	return &cp
}

// ForCred 返回该凭据应该使用的 client（凭据记录了登录时的 host）。
func (c *Client) ForCred(cred *Credentials) *Client {
	if cred == nil || cred.Host == "" {
		return c
	}
	return c.WithHost(cred.Host)
}

// HostForPhone 手机验证码登录使用的 host：默认**强制国内**。
// 国际 host 对 +86 号码会以 630015「当前地区暂不支持手机号注册」拒绝，且这个拒绝在验证码校验
// 之后才出现 —— 先试国际再回退只会白白多发一次必然失败的请求，所以直接锁国内。
// 例外：若部署方显式配置了自定义 host（非默认国际 host），则尊重该配置。
func (c *Client) HostForPhone() string {
	if c.Host != "" && c.Host != DefaultHost {
		return c.Host
	}
	return CNHost
}

// SendCodePhone 手机验证码发码：与 LoginPhone 使用同一个 host。
func (c *Client) SendCodePhone(ctx context.Context, phone, deviceID string) error {
	return c.WithHost(c.HostForPhone()).SendCode(ctx, phone, deviceID)
}

// LoginPhone 手机验证码登录（强制国内 host），返回实际使用的 host 供凭据记录。
func (c *Client) LoginPhone(ctx context.Context, phone, code, deviceID string) (*LoginResult, string, error) {
	host := c.HostForPhone()
	lr, err := c.WithHost(host).Login(ctx, phone, code, deviceID)
	return lr, host, err
}

// LoginAnyHost 依次尝试多个 host 完成手机验证码登录，返回成功的 host。
// 主要留给需要跨区兜底的场景（当前手机流程默认走 LoginPhone 的固定国内 host）。
func (c *Client) LoginAnyHost(ctx context.Context, phone, code, deviceID string, hosts ...string) (*LoginResult, string, error) {
	cands := dedupeHosts(append([]string{c.Host}, hosts...))
	if len(cands) == 0 {
		cands = []string{DefaultHost, CNHost}
	}
	var lastErr error
	for _, h := range cands {
		cl := c.WithHost(h)
		lr, err := cl.Login(ctx, phone, code, deviceID)
		if err == nil {
			return lr, h, nil
		}
		lastErr = err
		// 只有「地区不支持 / 需要走另一侧」这类业务错误才值得换 host；
		// 验证码错误（630202）、签名问题等换 host 也一样，直接返回，避免无谓请求。
		if !isHostSwitchableErr(err) {
			return nil, h, err
		}
	}
	return nil, "", lastErr
}

// isHostSwitchableErr 判断该错误是否可能因 host 不同而改变。
func isHostSwitchableErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, code := range []string{"630015", "630014", "630013", "631002", "631000"} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

func dedupeHosts(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, h := range in {
		h = strings.TrimRight(strings.TrimSpace(h), "/")
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

func (c *Client) do(method, path string, header http.Header, body []byte) (int, []byte, error) {
	if c.hc == nil {
		if h, ok := c.HTTP.(*http.Client); ok && h != nil {
			c.hc = h
		} else if c.HTTP != nil {
			c.hc = &http.Client{Transport: transportOf(c.HTTP)}
		} else {
			c.hc = &http.Client{Timeout: 60 * time.Second}
		}
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.Host+path, rd)
	if err != nil {
		return 0, nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// transportOf 兼容自定义 HTTP 实现的传输层注入（测试）。
func transportOf(h HTTP) http.RoundTripper {
	if t, ok := h.(interface{ RoundTrip() http.RoundTripper }); ok {
		return t.RoundTrip()
	}
	return http.DefaultTransport
}

// callBiz 业务接口：期望 HTTP 200 + 信封 code=0；否则返回携带分类的 *Error。
func (c *Client) callBiz(ctx context.Context, method, path string, header http.Header, body []byte) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Host+path, bodyReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Kind: ErrServer, Msg: "transport: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &Error{Kind: ErrAuth, Status: 401, Msg: "Invalid token"}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &Error{Kind: ErrSoftRate, Status: 429, Msg: truncateStr(string(raw), 160)}
	}
	if resp.StatusCode >= 500 {
		return nil, &Error{Kind: ErrServer, Status: resp.StatusCode, Msg: truncateStr(string(raw), 160)}
	}
	if resp.StatusCode >= 400 {
		return nil, &Error{Kind: ErrClient, Status: resp.StatusCode, Msg: truncateStr(string(raw), 200)}
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &Error{Kind: ErrClient, Status: resp.StatusCode, Msg: "bad envelope: " + truncateStr(string(raw), 120)}
	}
	if env.Code != 0 {
		kind := ErrClient
		if isCreditMsg(env.Msg) {
			kind = ErrHardCredit
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, env.Msg)}
	}
	return env.Data, nil
}

func bodyReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return bytes.NewReader(b)
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------------------------------------------------------------------------
// 登录 / 刷新
// ---------------------------------------------------------------------------

// SendCode 向手机号发送验证码。
func (c *Client) SendCode(ctx context.Context, phone, deviceID string) error {
	body, _ := json.Marshal(map[string]string{
		"source_id": Product,
		"device_id": deviceID,
		"phone":     phone,
	})
	_, err := c.callBiz(ctx, http.MethodPost, PathSendCode, AnonHeaders(), body)
	return err
}

// LoginResult 登录响应中服务需要的字段。
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	UID          string
	Nickname     string
}

// Login 手机验证码登录。成功返回 token 对；调用方负责构造 Credentials 并 SaveAtomic。
//
// 注意：`code` 必须按**数字**发送。官方客户端的 normalizeCode 会把验证码
// `Number(...)` 后再放进请求体，服务端按类型校验；发字符串会被判为
// 400001「请求数据有问题」（实测踩过）。
func (c *Client) Login(ctx context.Context, phone, code, deviceID string) (*LoginResult, error) {
	body, _ := json.Marshal(map[string]any{
		"source_id": Product,
		"device_id": deviceID,
		"phone":     phone,
		"code":      numericCodeValue(code),
	})
	data, err := c.callBiz(ctx, http.MethodPost, PathLogin, AnonHeaders(), body)
	if err != nil {
		return nil, err
	}
	var d struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		UserName     string `json:"user_name"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, &Error{Kind: ErrClient, Msg: "login parse: " + err.Error()}
	}
	if d.AccessToken == "" || d.RefreshToken == "" {
		return nil, &Error{Kind: ErrClient, Msg: "login: empty token in response"}
	}
	return &LoginResult{AccessToken: d.AccessToken, RefreshToken: d.RefreshToken, UID: d.UserID, Nickname: d.UserName}, nil
}

// Refresh 刷新 access token（refresh token 轮换：成功后必须 SaveAtomic）。
// 与客户端一致：/userapi/v1/refresh 失败（信封 code=400002 签名类错误）降级 /agent-refresh。
func (c *Client) Refresh(ctx context.Context, cred *Credentials) error {
	cred.Lock()
	refreshToken := cred.RefreshToken
	deviceID := cred.DeviceID
	cred.Unlock()
	if strings.TrimSpace(refreshToken) == "" {
		return &Error{Kind: ErrAuth, Msg: "no refreshToken"}
	}
	body, _ := json.Marshal(map[string]string{
		"source_id":     Product,
		"device_id":     deviceID,
		"refresh_token": refreshToken,
	})
	data, err := c.callBiz(ctx, http.MethodPost, PathRefresh, AnonHeaders(), body)
	if err != nil {
		var ue *Error
		if asError(err, &ue) && ue.Kind == ErrClient && strings.Contains(ue.Msg, "400002") {
			// 降级端点
			data, err = c.callBiz(ctx, http.MethodPost, PathRefreshFallback, AnonHeaders(), body)
		}
		if err != nil {
			return err
		}
	}
	var d struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.AccessToken == "" {
		return &Error{Kind: ErrAuth, Msg: "refresh: no access_token in response"}
	}
	cred.Lock()
	cred.AccessToken = d.AccessToken
	if d.RefreshToken != "" {
		cred.RefreshToken = d.RefreshToken // 轮换
	}
	if d.ExpiresIn > 0 {
		cred.ExpiresAt = time.Now().Add(time.Duration(d.ExpiresIn) * time.Second).Unix()
	} else {
		// 响应缺 expires_in：按实测有效期的保守值
		cred.ExpiresAt = time.Now().Add(5 * time.Hour).Unix()
	}
	cred.Unlock()
	return nil
}

// UserInfo 校验登录态并补全 uid/昵称（登录后首次调用）。
func (c *Client) UserInfo(ctx context.Context, accessToken, deviceID string) (uid, nickname string, err error) {
	body, _ := json.Marshal(map[string]string{"source_id": Product, "device_id": deviceID})
	h := BizHeaders(accessToken)
	data, err := c.callBiz(ctx, http.MethodPost, PathUserInfo, h, body)
	if err != nil {
		return "", "", err
	}
	var d struct {
		UserID   string `json:"user_id"`
		UserName string `json:"user_name"`
		NickName string `json:"nickname"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return "", "", nil // 用户信息非关键，解析失败不阻断
	}
	if d.NickName != "" {
		return d.UserID, d.NickName, nil
	}
	return d.UserID, d.UserName, nil
}

// ---------------------------------------------------------------------------
// 海外 OAuth（Google / zai 网页授权）
//
// 流程：GetOAuthURL 拿授权页 → 用户浏览器完成授权 → 服务回调 redirect_uri?code=..&state=..
// → 用 code/state 调 OverreaLogin 换 access/refresh token（与手机验证码登录同构）。
// redirect_uri 由调用方决定（客户端用 http://localhost:<port>/auth/callback-<vendor>）。
// ---------------------------------------------------------------------------

// OAuthURLResult 授权页信息。URL 字段名以服务端实际返回为准（客户端里 data 直接透传给渲染层打开）。
type OAuthURLResult struct {
	OAuthURL  string `json:"oauth_url"` // 上游实际字段名（google/zai-oauth-url 实测返回）
	URL       string `json:"url"`       // 常见命名兜底
	LoginURL  string `json:"login_url"` // 备选命名
	AuthURL   string `json:"auth_url"`  // 备选命名
	VerifyURL string `json:"verify_url"`
	State     string `json:"state"`
}

// CaptchaConfig 海外 OAuth 人机验证配置（aliyun 滑块）。
// enabled=true 时，oauth-url 请求必须携带滑块回执（ali_captcha_verify_param）。
type CaptchaConfig struct {
	Enabled         bool   `json:"enabled"`
	Region          string `json:"region"`
	Prefix          string `json:"prefix"`
	SceneID         string `json:"scene_id"`
	CaptchaSupplier string `json:"captcha_supplier"`
}

// GetOAuthCaptchaConfig 拉取滑块验证配置；未启用时返回 nil。
func (c *Client) GetOAuthCaptchaConfig(ctx context.Context) (*CaptchaConfig, error) {
	data, err := c.callBiz(ctx, http.MethodPost, PathOverseaOAuthCaptchaConfig, AnonHeaders(), []byte("{}"))
	if err != nil {
		return nil, err
	}
	var cfg CaptchaConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, &Error{Kind: ErrClient, Msg: "captcha-config parse: " + err.Error()}
	}
	if !cfg.Enabled {
		return nil, nil
	}
	return &cfg, nil
}

// rawURL 返回第一个非空 URL 字段。
func (r *OAuthURLResult) rawURL() string {
	switch {
	case r.OAuthURL != "":
		return r.OAuthURL
	case r.URL != "":
		return r.URL
	case r.LoginURL != "":
		return r.LoginURL
	case r.AuthURL != "":
		return r.AuthURL
	case r.VerifyURL != "":
		return r.VerifyURL
	}
	return ""
}

// GetOAuthURL 获取海外 OAuth 授权页地址。vendor: "google" 或 "zai"。
// captchaVerifyParam 非空时作为 ali_captcha_verify_param 携带（服务端 enabled 滑块后必需）。
func (c *Client) GetOAuthURL(ctx context.Context, vendor, deviceID, redirectURI, captchaVerifyParam string) (string, error) {
	urlPath := PathOverseaZaiOAuthURL
	if vendor == "google" {
		urlPath = PathOverseaGoogleOAuthURL
	}
	body := map[string]string{
		"source_id":    Product,
		"device_id":    deviceID,
		"navigate_uri": redirectURI,
	}
	if captchaVerifyParam != "" {
		body["ali_captcha_verify_param"] = captchaVerifyParam
	}
	raw, _ := json.Marshal(body)
	data, err := c.callBiz(ctx, http.MethodPost, urlPath, AnonHeaders(), raw)
	if err != nil {
		return "", err
	}
	var r OAuthURLResult
	if err := json.Unmarshal(data, &r); err != nil {
		// data 可能直接是字符串 URL
		var s string
		if json.Unmarshal(data, &s) == nil && strings.HasPrefix(s, "http") {
			return s, nil
		}
		return "", &Error{Kind: ErrClient, Msg: "oauth-url parse: " + err.Error()}
	}
	if u := r.rawURL(); u != "" {
		return u, nil
	}
	return "", &Error{Kind: ErrClient, Msg: "oauth-url: no url in response (raw=" + truncateStr(string(data), 120) + ")"}
}

// OAuthLogin 用回调 code/state 换 token。vendor: "google" 或 "zai"。
func (c *Client) OAuthLogin(ctx context.Context, vendor, code, state, redirectURI, deviceID string) (*LoginResult, error) {
	loginPath := PathOverseaZaiLogin
	if vendor == "google" {
		loginPath = PathOverseaGoogleLogin
	}
	body, _ := json.Marshal(map[string]string{
		"source_id":    Product,
		"device_id":    deviceID,
		"code":         code,
		"state":        state,
		"navigate_uri": redirectURI,
	})
	data, err := c.callBiz(ctx, http.MethodPost, loginPath, AnonHeaders(), body)
	if err != nil {
		return nil, err
	}
	var d struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		UserName     string `json:"user_name"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, &Error{Kind: ErrClient, Msg: "oauth-login parse: " + err.Error()}
	}
	if d.AccessToken == "" || d.RefreshToken == "" {
		return nil, &Error{Kind: ErrClient, Msg: "oauth-login: empty token in response"}
	}
	return &LoginResult{AccessToken: d.AccessToken, RefreshToken: d.RefreshToken, UID: d.UserID, Nickname: d.UserName}, nil
}

// ---------------------------------------------------------------------------
// 签到 / 积分
// ---------------------------------------------------------------------------

// TaskStatus daily_signin 任务状态。
type TaskStatus struct {
	TaskID     string `json:"task_id"`
	Status     string `json:"status"` // completed / incomplete
	StatusDesc string `json:"status_description"`
	Reward     int64  `json:"reward_points"`
}

// TaskList 拉取任务列表（关注 daily_signin）。
func (c *Client) TaskList(ctx context.Context, accessToken, deviceID string) (*TaskStatus, error) {
	h := BizHeaders(accessToken)
	data, err := c.callBiz(ctx, http.MethodGet, PathTaskList, h, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Tasks []TaskStatus `json:"data"`
	}
	// data 直接是数组
	if err := json.Unmarshal(data, &d.Tasks); err != nil {
		var arr []TaskStatus
		if err2 := json.Unmarshal(data, &arr); err2 != nil {
			return nil, &Error{Kind: ErrClient, Msg: "task-list parse: " + err.Error()}
		}
		d.Tasks = arr
	}
	for _, t := range d.Tasks {
		if t.TaskID == TaskDailySignin {
			return &t, nil
		}
	}
	return nil, &Error{Kind: ErrClient, Msg: "task-list: no " + TaskDailySignin + " task"}
}

// TaskCompleteResult 签到执行结果。
type TaskCompleteResult struct {
	Success        bool  `json:"success"`
	AlreadyDone    bool  `json:"already_completed"`
	RewardPoints   int64 `json:"reward_points"`
	ContinuousDays int64 `json:"continuous_days"`
}

// TaskComplete 执行签到。幂等：already_completed 也返回成功。
func (c *Client) TaskComplete(ctx context.Context, accessToken, deviceID string) (*TaskCompleteResult, error) {
	body, _ := json.Marshal(map[string]string{
		"task_id":   TaskDailySignin,
		"source_id": Product,
	})
	h := BizHeaders(accessToken)
	data, err := c.callBiz(ctx, http.MethodPost, PathTaskComplete, h, body)
	if err != nil {
		return nil, err
	}
	var r TaskCompleteResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, &Error{Kind: ErrClient, Msg: "task-complete parse: " + err.Error()}
	}
	return &r, nil
}

// Wallet 积分钱包分项。
type Wallet struct {
	Type    string `json:"public_wallet_type"`
	Name    string `json:"display_name"`
	Balance int64  `json:"balance"`
}

// WalletsResult 积分汇总。
type WalletsResult struct {
	Total   int64    `json:"total_balance"`
	Wallets []Wallet `json:"wallets"`
}

// Wallets 查询积分余额。
func (c *Client) Wallets(ctx context.Context, accessToken, deviceID string) (*WalletsResult, error) {
	h := BizHeaders(accessToken)
	data, err := c.callBiz(ctx, http.MethodGet, PathWallets, h, nil)
	if err != nil {
		return nil, err
	}
	var r WalletsResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, &Error{Kind: ErrClient, Msg: "wallets parse: " + err.Error()}
	}
	return &r, nil
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// ChatStream 发起对话：强制读原始响应，调用方处理 SSE 或 JSON。
// routeModelID 是带前缀的路由模型（如 zai_auto）；非法模型上游返回 400。
// 返回 status 与 body：status!=200 时 body 是错误响应（调用方分类），仅传输层错误返回 err。
func (c *Client) ChatStream(ctx context.Context, accessToken, routeModelID string, openaiBody []byte) (int, []byte, io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Host+PathChat, bytes.NewReader(openaiBody))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, vs := range ChatHeaders(accessToken, routeModelID) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return resp.StatusCode, raw, nil, nil
	}
	return resp.StatusCode, nil, resp.Body, nil
}
