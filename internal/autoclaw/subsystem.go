// subsystem.go autoclaw 上游子系统：对 server.Handler 暴露 Chat / Status / ModelList，
// 内部封装轮换挑号、token 刷新、错误分类与签到/积分任务。
package autoclaw

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// SubsystemConfig 装配参数（来自 cmd/server/config）。
type SubsystemConfig struct {
	Host             string
	Timeout          time.Duration
	RefreshSkew      time.Duration // token 提前刷新窗口
	SoftCooldown     time.Duration // 429 冷却
	BreakerThreshold int
	BreakerCooldown  time.Duration
	BreakerMax       time.Duration
}

// Subsystem 对外子系统。
type Subsystem struct {
	cfg  SubsystemConfig
	cl   *Client
	pool *Pool

	// signinMu 串行化签到（多调度触发/手动并发防抖）。
	signinMu chan struct{}
}

// NewSubsystem 装配（creds 已由调用方从 auth 目录加载）。
func NewSubsystem(cfg SubsystemConfig, creds []*Credentials) *Subsystem {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 30 * time.Minute
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.BreakerThreshold <= 0 {
		cfg.BreakerThreshold = 3
	}
	if cfg.BreakerCooldown <= 0 {
		cfg.BreakerCooldown = time.Minute
	}
	if cfg.BreakerMax <= 0 {
		cfg.BreakerMax = 30 * time.Minute
	}
	cl := NewClient(cfg.Host)
	cl.HTTP.(*http.Client).Timeout = cfg.Timeout
	return &Subsystem{
		cfg:      cfg,
		cl:       cl,
		pool:     NewPool(creds),
		signinMu: make(chan struct{}, 1),
	}
}

// Count 账号数。
func (s *Subsystem) Count() int { return s.pool.Count() }

// Status /status 观测。
func (s *Subsystem) Status() map[string]any {
	return map[string]any{
		"enabled":  true,
		"total":    s.pool.Count(),
		"healthy":  s.pool.Healthy(),
		"accounts": s.pool.Status(),
	}
}

// staticAutoclawModels 静态模型表（实测 200 的路由模型；context 来自 third-party-provider-config）。
// 上游白名单可能随版本变化，请求透传为准 —— 列表仅供 /v1/models 展示。
var staticAutoclawModels = []map[string]any{
	{"id": "autoclaw/zai_auto", "object": "model", "created": 1753600000, "owned_by": "autoclaw", "context_length": 131072},
	{"id": "autoclaw/zai_auto-fast", "object": "model", "created": 1753600000, "owned_by": "autoclaw", "context_length": 131072},
	{"id": "autoclaw/zai_glm-5.3-flash", "object": "model", "created": 1753600000, "owned_by": "autoclaw", "context_length": 1000000},
	{"id": "autoclaw/zai_glm-5-turbo", "object": "model", "created": 1753600000, "owned_by": "autoclaw", "context_length": 200000},
	{"id": "autoclaw/zaicoding_glm-5.3", "object": "model", "created": 1753600000, "owned_by": "autoclaw", "context_length": 1000000},
	{"id": "autoclaw/tdpsk_deepseek-v4-flash-202605", "object": "model", "created": 1753600000, "owned_by": "autoclaw", "context_length": 1000000},
	{"id": "autoclaw/tdpsk_deepseek-v4-pro-202606", "object": "model", "created": 1753600000, "owned_by": "autoclaw", "context_length": 1000000},
}

// ModelList /v1/models 的 autoclaw 段。
func (s *Subsystem) ModelList() []map[string]any { return staticAutoclawModels }

// ensureToken 取账号的可用 access token：临近过期先刷新（轮换写回），失败分类上抛。
func (s *Subsystem) ensureToken(a *Account) (string, *Error) {
	cred := a.Cred
	if cred.NeedsRefresh(s.cfg.RefreshSkew) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.cl.ForCred(cred).Refresh(ctx, cred); err != nil {
			var ue *Error
			if asError(err, &ue) {
				s.pool.NoteError(a, ue.Kind, ue.Msg, s.cfg.BreakerThreshold, s.cfg.BreakerCooldown, s.cfg.BreakerMax)
				return "", ue
			}
			return "", &Error{Kind: ErrServer, Msg: err.Error()}
		}
		if err := cred.SaveAtomic(); err != nil {
			log.Printf("autoclaw refresh save uid=%s: %v", cred.UID, err)
		}
	}
	cred.mu.Lock()
	tok := cred.AccessToken
	cred.mu.Unlock()
	if strings.TrimSpace(tok) == "" {
		return "", &Error{Kind: ErrAuth, Msg: "empty accessToken"}
	}
	return tok, nil
}

// Chat 处理 autoclaw 上游的对话请求。写回 OpenAI 错误格式；成功时透传 SSE 或聚合 JSON。
// model 可带 "autoclaw/" 前缀（本服务直连用法），也可为裸路由模型（opencodex 独立 provider 用法）。
// ChatWithToken 用调用方自带的 access token 直接对话（不走本地账号池）。
//
// 存在的理由：opencodex 的内置 provider 只有一个 baseUrl，而 AutoClaw 的 token 与 host 绑定
// （手机号账号在国内 host、z.ai/Google 账号在国际 host）。让 opencodex 指向本服务、
// 由这里按 token 自动挑 host，就把「一个 baseUrl 服务两种账号」这件事解决在了一处。
func (s *Subsystem) ChatWithToken(w http.ResponseWriter, clientBody []byte, accessToken string, wantStream bool) {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(clientBody, &probe)
	routeModel := strings.TrimPrefix(probe.Model, "autoclaw/")
	if routeModel == "" {
		writeAutoErr(w, http.StatusBadRequest, "invalid_model", "model 不能为空")
		return
	}
	if strings.TrimSpace(accessToken) == "" {
		writeAutoErr(w, http.StatusUnauthorized, "missing_token", "缺少 X-Authorization / Authorization access token")
		return
	}

	// 统一向上游要流式：上游对非流式请求返回普通 JSON，而本服务按客户端需求
	// 决定透传 SSE 还是本地聚合（workbuddy 上游同款处理方式）。
	upBody := clientBody
	var m map[string]any
	if err := json.Unmarshal(clientBody, &m); err == nil {
		m["stream"] = true
		if b, err := json.Marshal(m); err == nil {
			upBody = b
		}
	}

	// 国内优先（手机号账号占多数），再国际；401/403 视为「token 不属于该 host」→ 换下一侧。
	hosts := dedupeHosts([]string{CNHost, DefaultHost})
	var lastStatus int
	var lastBody []byte
	for _, host := range hosts {
		cl := s.cl.WithHost(host)
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
		status, respBody, rc, err := cl.ChatStream(ctx, accessToken, routeModel, upBody)
		if err != nil {
			cancel()
			lastStatus, lastBody = http.StatusBadGateway, []byte(err.Error())
			continue
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			// token 与 host 不匹配（另一侧的账号）→ 试下一个 host。
			// 注意：ChatStream 在 >=400 时已把响应体读进 respBody，rc 为 nil，不要再读它。
			cancel()
			lastStatus, lastBody = status, respBody
			continue
		}
		if status >= 400 {
			cancel()
			writeAutoErr(w, status, "autoclaw_upstream", truncateStr(string(respBody), 300))
			return
		}
		s.serveStreamOrJSON(w, rc, wantStream)
		cancel()
		return
	}
	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	writeAutoErr(w, lastStatus, "autoclaw_host_unresolved",
		"两个 host 都拒绝了该 token："+truncateStr(string(lastBody), 200))
}

func (s *Subsystem) Chat(w http.ResponseWriter, clientBody []byte, wantStream bool) {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(clientBody, &probe)
	routeModel := strings.TrimPrefix(probe.Model, "autoclaw/")
	if routeModel == "" {
		writeAutoErr(w, http.StatusBadRequest, "invalid_model", "model 不能为空")
		return
	}

	// 多账号轮转 + 同请求 failover：最多尝试全部可用账号，失败者进排除表。
	tried := map[string]bool{}
	maxTries := s.pool.UsableCount(nil)
	if maxTries == 0 {
		writeAutoErr(w, http.StatusServiceUnavailable, "no_healthy_account",
			"autoclaw: no usable account (cooling/needs_relogin) — 检查 /status 或重新登录")
		return
	}
	if maxTries > 3 {
		maxTries = 3 // 单请求轮换上限（对齐 workbuddy handler 的 MaxRotate=3）
	}

	// 请求体按需保留：每轮可能不同账号 token，但 body 与 routeModel 不变。
	var lastStatus int
	var lastBody []byte
	for attempt := 0; attempt < maxTries; attempt++ {
		a := s.pool.Pick(tried)
		if a == nil {
			break
		}
		tried[a.Cred.UID] = true

		tok, terr := s.ensureToken(a)
		if terr != nil {
			lastStatus, lastBody = http.StatusServiceUnavailable, []byte(terr.Error())
			continue // 换下一个账号
		}

		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
		status, respBody, rc, err := s.cl.ForCred(a.Cred).ChatStream(ctx, tok, routeModel, clientBody)
		if err != nil {
			cancel()
			s.pool.NoteError(a, ErrServer, err.Error(), s.cfg.BreakerThreshold, s.cfg.BreakerCooldown, s.cfg.BreakerMax)
			lastStatus, lastBody = http.StatusBadGateway, []byte(err.Error())
			continue // 网络错：换号重试
		}
		if status >= 400 {
			kind := ClassifyHTTP(status, string(respBody))
			s.pool.NoteError(a, kind, truncateStr(string(respBody), 200), s.cfg.BreakerThreshold, s.cfg.BreakerCooldown, s.cfg.BreakerMax)
			cancel()
			// 客户端错误（非法模型等）换号无意义，直接透传
			if kind == ErrClient || kind == ErrNotFound {
				writeAutoErr(w, status, "autoclaw_upstream", truncateStr(string(respBody), 300))
				return
			}
			// token 失效：本轮先强刷重试一次同一账号，仍失败才换号
			if kind == ErrAuth {
				if retok, terr2 := s.ensureToken(a); terr2 == nil {
					status2, respBody2, rc2, err2 := s.cl.ForCred(a.Cred).ChatStream(ctx, retok, routeModel, clientBody)
					if err2 == nil && status2 < 400 && rc2 != nil {
						s.serveStreamOrJSON(w, rc2, wantStream)
						s.pool.NoteSuccess(a)
						cancel()
						return
					}
					if status2 >= 400 {
						respBody, status = respBody2, status2
					}
				}
			}
			lastStatus, lastBody = upstreamStatus(kind, status), respBody
			continue // 换下一个账号
		}
		s.serveStreamOrJSON(w, rc, wantStream)
		s.pool.NoteSuccess(a)
		cancel()
		return
	}
	if lastStatus == 0 {
		lastStatus = http.StatusServiceUnavailable
	}
	msg := truncateStr(string(lastBody), 300)
	if msg == "" {
		msg = "all autoclaw accounts failed"
	}
	writeAutoErr(w, lastStatus, "autoclaw_upstream", msg)
}

// serveStreamOrJSON 按客户端意愿透传 SSE 或聚合为单个 JSON。
func (s *Subsystem) serveStreamOrJSON(w http.ResponseWriter, rc io.ReadCloser, wantStream bool) {
	defer rc.Close()
	if wantStream {
		_ = StreamSSE(w, rc)
		return
	}
	resp, err := AggregateJSON(rc)
	if err != nil {
		writeAutoErr(w, http.StatusBadGateway, "autoclaw_parse", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// upstreamStatus 映射错误分类到对外 HTTP 状态。
func upstreamStatus(kind ErrKind, origin int) int {
	switch kind {
	case ErrAuth:
		return http.StatusBadGateway // 凭据问题是服务侧状态，不对外伪装成 401
	case ErrSoftRate:
		return http.StatusTooManyRequests
	case ErrHardCredit:
		return http.StatusPaymentRequired
	}
	return origin
}

func writeAutoErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error", "code": code},
	})
}

// ---------------------------------------------------------------------------
// 签到 / 积分（供 scheduler 调用）
// ---------------------------------------------------------------------------

// SigninResult 单账号签到结果。
type SigninResult struct {
	Phone   string `json:"phone"`
	OK      bool   `json:"ok"`
	Already bool   `json:"already"`
	Reward  int64  `json:"reward_points"`
	Balance int64  `json:"balance"`
	Skipped bool   `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
}

// RunSignin 对所有账号执行幂等签到并刷新积分余额（scheduler 09/21 点与手动触发共用）。
func (s *Subsystem) RunSignin() []SigninResult {
	select {
	case s.signinMu <- struct{}{}:
		defer func() { <-s.signinMu }()
	default:
		return nil // 已有签到在跑
	}
	var out []SigninResult
	for _, a := range s.poolAccounts() {
		cred := a.Cred
		res := SigninResult{Phone: maskPhone(cred.Phone)}
		if snap := a.snapshot(); snap.relogin {
			res.Skipped, res.Error = true, "needs_relogin"
			out = append(out, res)
			continue
		}
		tok, terr := s.ensureToken(a)
		if terr != nil {
			res.Skipped, res.Error = true, terr.Error()
			out = append(out, res)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		st, err := s.cl.ForCred(cred).TaskList(ctx, tok, cred.DeviceID)
		if err == nil && st.Status == "completed" {
			res.OK, res.Already = true, true
		} else if err == nil {
			cr, err := s.cl.ForCred(cred).TaskComplete(ctx, tok, cred.DeviceID)
			switch {
			case err == nil:
				res.OK, res.Reward = true, cr.RewardPoints
			case strings.Contains(err.Error(), "already_completed"):
				res.OK, res.Already = true, true
			default:
				res.Error = err.Error()
			}
		} else {
			res.Error = err.Error()
			var ue *Error
			if asError(err, &ue) {
				s.pool.NoteError(a, ue.Kind, ue.Msg, s.cfg.BreakerThreshold, s.cfg.BreakerCooldown, s.cfg.BreakerMax)
			}
		}
		cancel()
		// 积分余额刷新（失败不阻断）
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		if w, err := s.cl.ForCred(cred).Wallets(ctx2, tok, cred.DeviceID); err == nil {
			s.pool.SetBalance(a, w.Total)
			res.Balance = w.Total
		} else {
			cancel2()
			continue
		}
		cancel2()
		out = append(out, res)
	}
	return out
}

// poolAccounts 快照当前账号列表。
func (s *Subsystem) poolAccounts() []*Account {
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	out := make([]*Account, len(s.pool.accts))
	copy(out, s.pool.accts)
	return out
}
