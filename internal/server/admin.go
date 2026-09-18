// admin.go 管理端点：账号池查看 + 登录会话（验证码 / OAuth）+ 删除账号。
// 供 opencodex 面板页面（跨源 fetch）与本地脚本调用；鉴权沿用 cfg.APIKey，全部带 CORS 头。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/autoclaw"
)

// corsMiddleware 面板跨源访问（面板 origin 与 7863 不同源）。
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// loginSession 一次进行中的登录（send-code 后等待 code；OAuth 后等待回调）。
// 全局仅允许一个进行中会话（简单互斥，避免 device_id 冲突）。
type loginSession struct {
	mu        sync.Mutex
	active    bool
	vendor    string // phone / google / zai
	deviceID  string
	phone     string
	startedAt time.Time
	respCh    chan *autoclaw.LoginResult
	errCh     chan error
}

// deviceIDFor 取当前进行中会话的 device_id（phone 流程且号码匹配时）。
func (s *loginSession) deviceIDFor(phone string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.deviceID == "" || s.vendor != "phone" {
		return ""
	}
	if phone != "" && s.phone != "" && s.phone != phone {
		return ""
	}
	return s.deviceID
}

// deviceIDForLogin 登录（第二步）使用的 device_id。
// 必须与发码时一致 —— 服务端把验证码与发码设备绑定，不一致即 400001「请求数据有问题」。
// 优先进行中会话，其次落盘的发码记录，最后复用已有凭据的设备号。
func (h *Handler) deviceIDForLogin(phone string) string {
	if id := loginSess.deviceIDFor(phone); id != "" {
		return id
	}
	if id := autoclaw.LoadPendingDevice(h.adminDeps.AuthDir, phone); id != "" {
		return id
	}
	return reuseDeviceID(h.adminDeps.AuthDir, phone)
}

var loginSess = &loginSession{
	respCh: make(chan *autoclaw.LoginResult, 1),
	errCh:  make(chan error, 1),
}

// begin 记录一次登录的设备信息。
// 不做互斥拒绝：phone 流程靠 pending 文件按手机号保持 device_id，OAuth 流程的 device_id
// 随响应回传给调用方，两者都不依赖会话独占。早期版本用互斥，结果一次中途放弃的登录会把
// 后续所有登录挡在 409 外面（还会连带把 OAuth 请求打到直连、报出误导性的 631002）。
func (s *loginSession) begin(vendor, phone, deviceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active, s.vendor, s.phone, s.deviceID, s.startedAt = true, vendor, phone, deviceID, time.Now()
	// 清空旧 channel
	select {
	case <-s.respCh:
	default:
	}
	select {
	case <-s.errCh:
	default:
	}
	return true
}

func (s *loginSession) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = false
}

// captchaReceipt 阿里云滑块回执缓存。
// opencodex 面板/守护进程无法渲染滑块（无浏览器、非 TTY），所以由用户在 7863 账号管理页拖动滑块，
// 页面经 captcha-param 端点上报回执，守护进程随后取用换取授权链接。短 TTL 避免回执长期驻留。
var captchaStore = struct {
	mu    sync.Mutex
	items map[string]captchaItem
}{items: map[string]captchaItem{}}

type captchaItem struct {
	Param string
	At    time.Time
}

const captchaTTL = 15 * time.Minute

func putCaptcha(vendor, param string) {
	captchaStore.mu.Lock()
	defer captchaStore.mu.Unlock()
	captchaStore.items[vendor] = captchaItem{Param: param, At: time.Now()}
}

func getCaptcha(vendor string) string {
	captchaStore.mu.Lock()
	defer captchaStore.mu.Unlock()
	it, ok := captchaStore.items[vendor]
	if !ok || time.Since(it.At) > captchaTTL {
		return ""
	}
	return it.Param
}

// captchaPageURL 引导用户去拖滑块的页面地址（面板静态页，带 vendor 锚点）。
func captchaPageURL(panelBase, vendor string) string {
	base := strings.TrimSpace(panelBase)
	if base == "" {
		base = "http://127.0.0.1:10100/autoclaw-accounts.html"
	}
	if vendor == "" {
		return base
	}
	return base + "#oauth-" + vendor
}

// AdminDeps 管理端点依赖。
type AdminDeps struct {
	Autoclaw   *autoclaw.Subsystem
	AuthDir    string
	Host       string // AutoClaw API host
	HTTPClient *http.Client
	PanelURL   string // 账号管理页地址（用于 need_captcha 引导）
}

// RegisterAdminRoutes 挂载 /admin/autoclaw/* 路由（cfg.Autoclaw 非 nil 时调用）。
func (h *Handler) RegisterAdminRoutes(d AdminDeps) {
	h.adminDeps = d
	// OPTIONS 预检统一处理（Go 1.22 mux 的方法匹配优先于 handler，需显式注册）。
	h.mux.HandleFunc("OPTIONS /admin/autoclaw/{path...}", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {}))
	h.mux.HandleFunc("GET /admin/autoclaw/accounts", h.withAuth(corsMiddleware(h.adminAccounts)))
	h.mux.HandleFunc("POST /admin/autoclaw/login/send-code", h.withAuth(corsMiddleware(h.adminSendCode)))
	h.mux.HandleFunc("POST /admin/autoclaw/login/verify", h.withAuth(corsMiddleware(h.adminVerify)))
	h.mux.HandleFunc("GET /admin/autoclaw/login/captcha-config", h.withAuth(corsMiddleware(h.adminCaptchaConfig)))
	h.mux.HandleFunc("POST /admin/autoclaw/login/captcha-param", h.withAuth(corsMiddleware(h.adminCaptchaParam)))
	h.mux.HandleFunc("POST /admin/autoclaw/login/oauth-url", h.withAuth(corsMiddleware(h.adminOAuthURL)))
	h.mux.HandleFunc("POST /admin/autoclaw/login/oauth-link", h.withAuth(corsMiddleware(h.adminOAuthLinkPut)))
	h.mux.HandleFunc("GET /admin/autoclaw/login/oauth-link", h.withAuth(corsMiddleware(h.adminOAuthLinkGet)))
	h.mux.HandleFunc("POST /admin/autoclaw/login/oauth-finish", h.withAuth(corsMiddleware(h.adminOAuthFinish)))
	h.mux.HandleFunc("POST /admin/autoclaw/login/reset", h.withAuth(corsMiddleware(h.adminLoginReset)))
	h.mux.HandleFunc("DELETE /admin/autoclaw/accounts/{uid}", h.withAuth(corsMiddleware(h.adminDeleteAccount)))
}

func writeAdminErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

// adminAccounts GET /admin/autoclaw/accounts
func (h *Handler) adminAccounts(w http.ResponseWriter, r *http.Request) {
	if h.adminDeps.Autoclaw == nil {
		writeAdminErr(w, http.StatusServiceUnavailable, "autoclaw 未启用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"enabled":  true,
		"accounts": h.cfg.Autoclaw.Status()["accounts"],
		"total":    h.cfg.Autoclaw.Count(),
	})
}

// adminSendCode POST {phone} → 发送短信验证码。
func (h *Handler) adminSendCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Phone) == "" {
		writeAdminErr(w, http.StatusBadRequest, "body 需为 {\"phone\":\"...\"}")
		return
	}
	deviceID := reuseDeviceID(h.adminDeps.AuthDir, req.Phone)
	loginSess.begin("phone", req.Phone, deviceID) // 记录当前流程的设备（不互斥）
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	// 发码固定走国内 host：国际 host 对 +86 号码在登录阶段会以 630015 拒绝。
	log.Printf("admin: autoclaw send-code host=%s phone=%s", h.autoclawClient().HostForPhone(), maskPhoneAdmin(req.Phone))
	if err := h.autoclawClient().SendCodePhone(ctx, req.Phone, deviceID); err != nil {
		loginSess.end()
		writeAdminErr(w, http.StatusBadGateway, "发送验证码失败: "+err.Error())
		return
	}
	// 记住发码设备：下一步 agent-login 必须用同一个 device_id。
	if perr := autoclaw.SavePendingDevice(h.adminDeps.AuthDir, req.Phone, deviceID); perr != nil {
		log.Printf("admin: 保存发码设备失败（不影响登录）: %v", perr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "验证码已发送，请查收手机短信"})
}

// adminVerify POST {phone, code, no_persist} → 登录。
// no_persist=true 时只返回 token 不落 7863 池（opencodex 守护进程借 7863 走完两步验证码登录，
// 凭据归 opencodex 自己的池）；这是面板内完成短信登录的关键 —— 同时由 7863 统一维护
// 发码/登录的 device_id 一致性。
func (h *Handler) adminVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone     string `json:"phone"`
		Code      string `json:"code"`
		NoPersist bool   `json:"no_persist"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Phone == "" || req.Code == "" {
		writeAdminErr(w, http.StatusBadRequest, "body 需为 {\"phone\":\"...\",\"code\":\"...\"}")
		return
	}
	deviceID := h.deviceIDForLogin(req.Phone)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	lr, usedHost, err := h.autoclawClient().LoginPhone(ctx, req.Phone, req.Code, deviceID)
	if err != nil {
		msg := "登录失败: " + err.Error()
		if strings.Contains(err.Error(), "400001") {
			msg += " — 常见原因：验证码过期/填错、手机号格式不对（请填 11 位大陆手机号，不带 +86），"
			msg += "或发码与登录不在同一设备（请重新点「发送验证码」后再登录）"
		}
		writeAdminErr(w, http.StatusBadGateway, msg)
		return
	}
	loginSess.end()
	_ = autoclaw.ClearPendingDevice(h.adminDeps.AuthDir, req.Phone)
	if req.NoPersist {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"uid":           lr.UID,
			"nickname":      lr.Nickname,
			"phone":         maskPhoneAdmin(req.Phone),
			"access_token":  lr.AccessToken,
			"refresh_token": lr.RefreshToken,
			"expires_at":    time.Now().Add(5 * time.Hour).Unix(),
			"device_id":     deviceID,
			"message":       "登录成功（凭据仅返回，未落 7863 账号池）",
		})
		return
	}
	fp, err := saveCred(h.adminDeps.AuthDir, req.Phone, deviceID, usedHost, lr)
	if err != nil {
		writeAdminErr(w, http.StatusInternalServerError, "保存凭据失败: "+err.Error())
		return
	}
	log.Printf("admin: autoclaw account added uid=%s phone=%s (%s) — 重启服务生效", lr.UID, maskPhoneAdmin(req.Phone), fp)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     lr.UID,
		"phone":   maskPhoneAdmin(req.Phone),
		"file":    fp,
		"message": "登录成功，凭据已落盘。需重启服务加载（见页面提示）",
	})
}

// adminCaptchaConfig GET → 滑块验证配置（面板据此渲染阿里云滑块）。
func (h *Handler) adminCaptchaConfig(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	cfg, err := h.autoclawClient().GetOAuthCaptchaConfig(ctx)
	if err != nil {
		writeAdminErr(w, http.StatusBadGateway, "获取验证码配置失败: "+err.Error())
		return
	}
	if cfg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": true, "captcha": cfg})
}

// adminCaptchaParam POST {vendor, captcha_verify_param} → 缓存滑块回执。
// 用于「面板/守护进程无法拖滑块」的场景：用户在账号管理页拖完后上报，随后由 opencodex 取用。
func (h *Handler) adminCaptchaParam(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Vendor             string `json:"vendor"`
		CaptchaVerifyParam string `json:"captcha_verify_param"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.CaptchaVerifyParam) == "" {
		writeAdminErr(w, http.StatusBadRequest, "body 需为 {\"vendor\":\"google|zai\",\"captcha_verify_param\":\"...\"}")
		return
	}
	vendor := strings.ToLower(strings.TrimSpace(req.Vendor))
	if vendor != "google" && vendor != "zai" {
		writeAdminErr(w, http.StatusBadRequest, "vendor 只支持 google 或 zai")
		return
	}
	putCaptcha(vendor, strings.TrimSpace(req.CaptchaVerifyParam))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"vendor":  vendor,
		"ttl_min": int(captchaTTL.Minutes()),
		"message": "滑块回执已就绪，请回到 opencodex 面板重新点击登录并选择 " + vendor,
	})
}

// oauthLinkStore 待授权链接中转。
// 面板的登录请求在 onAuth 首次触发时就已返回，之后的授权 URL 无处可展示；
// 守护进程把它 POST 到这里，注入面板的表单再 GET 取回并打开。
var oauthLinkStore = struct {
	mu    sync.Mutex
	items map[string]oauthLink
}{items: map[string]oauthLink{}}

type oauthLink struct {
	URL      string
	DeviceID string
	At       time.Time
}

const oauthLinkTTL = 15 * time.Minute

// adminOAuthLinkPut POST {vendor, url, device_id} → 暂存待授权链接。
func (h *Handler) adminOAuthLinkPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Vendor   string `json:"vendor"`
		URL      string `json:"url"`
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		writeAdminErr(w, http.StatusBadRequest, "body 需含 vendor/url")
		return
	}
	vendor := strings.ToLower(strings.TrimSpace(req.Vendor))
	if vendor == "" {
		vendor = "zai"
	}
	oauthLinkStore.mu.Lock()
	oauthLinkStore.items[vendor] = oauthLink{URL: req.URL, DeviceID: req.DeviceID, At: time.Now()}
	oauthLinkStore.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vendor": vendor})
}

// adminOAuthLinkGet GET /admin/autoclaw/login/oauth-link?vendor= → 取回待授权链接（一次性读，不删除）。
func (h *Handler) adminOAuthLinkGet(w http.ResponseWriter, r *http.Request) {
	vendor := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("vendor")))
	if vendor == "" {
		vendor = "zai"
	}
	oauthLinkStore.mu.Lock()
	it, ok := oauthLinkStore.items[vendor]
	oauthLinkStore.mu.Unlock()
	if !ok || time.Since(it.At) > oauthLinkTTL {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pending": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"pending":   true,
		"vendor":    vendor,
		"url":       it.URL,
		"device_id": it.DeviceID,
		"age_sec":   int(time.Since(it.At).Seconds()),
	})
}

// adminLoginReset 清空登录相关状态：进行中的会话、缓存的滑块回执、待授权链接。
func (h *Handler) adminLoginReset(w http.ResponseWriter, _ *http.Request) {
	loginSess.end()
	captchaStore.mu.Lock()
	captchaStore.items = map[string]captchaItem{}
	captchaStore.mu.Unlock()
	oauthLinkStore.mu.Lock()
	oauthLinkStore.items = map[string]oauthLink{}
	oauthLinkStore.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "登录会话、滑块回执缓存与待授权链接已清空",
	})
}

// adminOAuthURL POST {vendor} → 返回授权页 URL（vendor: google / zai）。
// 服务端开启滑块后必须携带回执：优先用请求体里的，其次用 captcha-param 缓存的；
// 两者皆无时返回 428 + need_captcha，由调用方引导用户去账号管理页拖滑块。
func (h *Handler) adminOAuthURL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Vendor             string `json:"vendor"`
		RedirectURI        string `json:"redirect_uri"`
		CaptchaVerifyParam string `json:"captcha_verify_param"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAdminErr(w, http.StatusBadRequest, "body 需为 {\"vendor\":\"google|zai\"}")
		return
	}
	vendor := strings.ToLower(strings.TrimSpace(req.Vendor))
	if vendor != "google" && vendor != "zai" {
		writeAdminErr(w, http.StatusBadRequest, "vendor 只支持 google 或 zai")
		return
	}
	redirect := strings.TrimSpace(req.RedirectURI)
	if redirect == "" {
		redirect = fmt.Sprintf("http://localhost:18432/auth/callback-%s", vendor)
	}
	param := strings.TrimSpace(req.CaptchaVerifyParam)
	if param == "" {
		param = getCaptcha(vendor)
	}
	deviceID := autoclaw.NewDeviceID()
	// 不占用登录会话：device_id 随响应回传，调用方 finish 时带回即可（无状态，避免互相阻塞）。
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	u, err := h.autoclawClient().GetOAuthURL(ctx, vendor, deviceID, redirect, param)
	if err != nil {
		// 无回执而上文要求滑块时，给调用方可行动的引导（而不是一句 631002）。
		if param == "" {
			if cfg, cerr := h.autoclawClient().GetOAuthCaptchaConfig(ctx); cerr == nil && cfg != nil && cfg.Enabled {
				writeJSON(w, http.StatusPreconditionRequired, map[string]any{
					"ok":           false,
					"need_captcha": true,
					"vendor":       vendor,
					"captcha":      cfg,
					"captcha_page": captchaPageURL(h.adminDeps.PanelURL, vendor),
					"error":        "该接口需要滑块验证回执：请在账号管理页拖动滑块后重试（上游: " + err.Error() + "）",
				})
				return
			}
		}
		writeAdminErr(w, http.StatusBadGateway, "获取授权 URL 失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"vendor":       vendor,
		"authorizeURL": u,
		"redirect_uri": redirect,
		"device_id":    deviceID,
		"message":      "请在浏览器打开授权页；授权后从跳转地址复制 code 与 state",
	})
}

// adminOAuthFinish POST {vendor, code, state, redirect_uri, device_id, no_persist} → 换 token。
// no_persist=true 时不落 7863 账号池，仅把 token 返回给调用方（opencodex 守护进程入自己的池），
// 避免同一账号在两边同时轮换 refresh 而互踢。
func (h *Handler) adminOAuthFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Vendor      string `json:"vendor"`
		Code        string `json:"code"`
		State       string `json:"state"`
		RedirectURI string `json:"redirect_uri"`
		DeviceID    string `json:"device_id"`
		NoPersist   bool   `json:"no_persist"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
		writeAdminErr(w, http.StatusBadRequest, "body 需含 vendor/code/state/redirect_uri/device_id")
		return
	}
	vendor := strings.ToLower(strings.TrimSpace(req.Vendor))
	if vendor != "google" && vendor != "zai" {
		writeAdminErr(w, http.StatusBadRequest, "vendor 只支持 google 或 zai")
		return
	}
	deviceID := req.DeviceID
	if deviceID == "" {
		deviceID = autoclaw.NewDeviceID()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	lr, err := h.autoclawClient().OAuthLogin(ctx, vendor, req.Code, req.State, req.RedirectURI, deviceID)
	if err != nil {
		writeAdminErr(w, http.StatusBadGateway, "OAuth 登录失败: "+err.Error())
		return
	}
	loginSess.end()
	if req.NoPersist {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"vendor":        vendor,
			"uid":           lr.UID,
			"nickname":      lr.Nickname,
			"access_token":  lr.AccessToken,
			"refresh_token": lr.RefreshToken,
			"expires_at":    time.Now().Add(5 * time.Hour).Unix(),
			"device_id":     deviceID,
			"message":       "登录成功（凭据仅返回，未落 7863 账号池）",
		})
		return
	}
	fp, err := saveCred(h.adminDeps.AuthDir, "", deviceID, h.adminDeps.Host, lr)
	if err != nil {
		writeAdminErr(w, http.StatusInternalServerError, "保存凭据失败: "+err.Error())
		return
	}
	log.Printf("admin: autoclaw oauth account added uid=%s vendor=%s (%s)", lr.UID, vendor, fp)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     lr.UID,
		"vendor":  vendor,
		"file":    fp,
		"message": "登录成功，凭据已落盘。需重启服务加载",
	})
}

// adminDeleteAccount DELETE /admin/autoclaw/accounts/{uid} → 删除凭据文件。
func (h *Handler) adminDeleteAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeAdminErr(w, http.StatusBadRequest, "缺少 uid")
		return
	}
	matches, err := filepath.Glob(filepath.Join(h.adminDeps.AuthDir, "autoclaw-"+uid+".json"))
	if err != nil || len(matches) == 0 {
		writeAdminErr(w, http.StatusNotFound, "未找到该账号的凭据文件")
		return
	}
	for _, f := range matches {
		if err := os.Remove(f); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "删除失败: "+err.Error())
			return
		}
	}
	log.Printf("admin: autoclaw account removed uid=%s (%s)", uid, matches[0])
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     uid,
		"message": "已删除凭据文件。需重启服务从内存池移除",
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (h *Handler) autoclawClient() *autoclaw.Client {
	if h.adminClient != nil {
		return h.adminClient
	}
	h.adminClient = autoclaw.NewClient(h.adminDeps.Host)
	return h.adminClient
}

// reuseDeviceID 复用同手机号已有凭据的 device_id。
func reuseDeviceID(authDir, phone string) string {
	creds, err := autoclaw.LoadDir(authDir)
	if err != nil {
		return autoclaw.NewDeviceID()
	}
	for _, c := range creds {
		if c.Phone == phone && c.DeviceID != "" {
			return c.DeviceID
		}
	}
	return autoclaw.NewDeviceID()
}

// saveCred 构造凭据并原子落盘（供 admin 端点）。
func saveCred(authDir, phone, deviceID, host string, lr *autoclaw.LoginResult) (string, error) {
	cred := &autoclaw.Credentials{
		AccessToken:  lr.AccessToken,
		RefreshToken: lr.RefreshToken,
		ExpiresAt:    time.Now().Add(5 * time.Hour).Unix(),
		DeviceID:     deviceID,
		Phone:        phone,
		UID:          lr.UID,
		Nickname:     lr.Nickname,
		Host:         host,
	}
	if cred.UID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		client := autoclaw.NewClient("")
		if uid, nick, err := client.UserInfo(ctx, cred.AccessToken, cred.DeviceID); err == nil && uid != "" {
			cred.UID = uid
			cred.Nickname = nick
		}
	}
	if cred.UID == "" {
		cred.UID = fmt.Sprintf("oa%d", time.Now().Unix())
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", err
	}
	cred.FilePath = filepath.Join(authDir, "autoclaw-"+cred.UID+".json")
	if err := cred.SaveAtomic(); err != nil {
		return "", err
	}
	return cred.FilePath, nil
}

func maskPhoneAdmin(p string) string {
	if len(p) < 7 {
		return "***"
	}
	return p[:3] + "****" + p[len(p)-4:]
}

// ──────────────────────────────────────────────────────────────────────
// 以下为上游 v1.0.0 的账号停用/恢复/复活管理端点（issue #138 / #118 / PR #166），
// 与上方 autoclaw 管理端点相互独立，共用 cfg.APIKey 鉴权。
// ──────────────────────────────────────────────────────────────────────

// admin.go 运维管理端点（issue #138 / #118）：账号的临时停用 / 恢复 / 复活。
//
// 设计要点（与维护者在 issue #118 预告的方案一致）：
//   - 手动停用是**独立状态位** manual_disabled，与自动禁用 disabled 并列、互不影响。
//     复用同一字段会让运维意图被签到解冻、refresh 成功等自动复活路径意外解除。
//   - 语义是「对话流量摘除」而非「账号冻结」：不碰冷却/熔断维度，签到与保活照常，
//     凭证和积分都是活的；恢复时拿到的是停用期间真实发生的状态。
//   - 默认关闭（config admin.enabled），开启后与 /status 共用同一把 api_key 鉴权。
//   - 幂等：面板重试不会报错；重复调用只更新原因文案。

// adminState 管理端点的统一响应体：回显操作后的双位状态，面板据此直接更新 UI，
// 不必再打一次 /status。
type adminState struct {
	UID            string `json:"uid"`
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason,omitempty"`
	// Disabled 保留在响应里让面板能区分「手动摘除」与「系统判定坏了」——
	// 恢复按钮的语义对两者不同（enable 解手动位，revive 解自动位）。
	Disabled bool `json:"disabled"`
	Changed  bool `json:"changed"`
}

// adminUID 提取并校验路径段 uid。返回 false 表示已写出响应（uid 为空 → 400），
// 调用方应直接 return。
// 开关判断不在这里：路由按 cfg.AdminEnabled 条件注册（见 NewHandler），
// 未开启时这些 handler 根本不可达——handler 内再判开关是多余的存在性泄露面。
func adminUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid is required")
		return "", false
	}
	return uid, true
}

// adminReasonFromBody 读可选 JSON 体里的 reason 字段。
// 空体/非 JSON/无该字段都返回空串（端点不因体格式拒绝——无体是最常见调用形态）。
func adminReasonFromBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	// 限制读取量：reason 是短文本，避免畸形大请求占用内存。
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

// adminAccountDisable 手动停用：把账号摘出选号池，但保留在池里
// （状态/冷却/成本台账继续归它管，签到与保活照常）。
func (h *Handler) adminAccountDisable(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	reason := adminReasonFromBody(r)
	if reason == "" {
		reason = "manual"
	}
	found, changed := h.cfg.Pool.SetManualDisabled(uid, true, reason)
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	stopped, stopReason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: stopReason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// adminAccountEnable 解除手动停用。若账号仍被系统自动禁用（disabled），它**不会**
// 因此回到选号池——那需要 revive。响应里的 disabled 字段就是给面板看的提示。
func (h *Handler) adminAccountEnable(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	found, changed := h.cfg.Pool.SetManualDisabled(uid, false, "")
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	stopped, reason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: reason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// adminAccountRevive 解除系统自动禁用（清 disabled + reason + 连续 12153 计数）。
// 不碰手动停用位：运维明确摘除的号不应被一次 revive 悄悄放回选号池。
func (h *Handler) adminAccountRevive(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	if _, _, found := h.cfg.Pool.ManualDisabledState(uid); !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	changed := h.cfg.Pool.ReviveDisabled(uid)
	stopped, reason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: reason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// accountAutoDisabled 读某账号当前的自动禁用位（供响应回显）。
// 复用 Pool.List 的单账号查询：轮询全部账号在小池下开销可忽略，
// 且避免为此在 pool 上再开一个只读访问器（保持接口面最小）。
func (h *Handler) accountAutoDisabled(uid string) bool {
	for _, st := range h.cfg.Pool.List() {
		if st.UID == uid {
			return st.Disabled
		}
	}
	return false
}
