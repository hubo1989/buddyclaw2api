// qoder.go Qoder 上游的网关出站：/v1/qoder/{realm}/chat/completions 直通转换
// （OpenAI 入 → Qoder COSY 出 → OpenAI SSE 回），以及 /admin/qoder/* 登录管理端点
// （设备流 start/poll、PAT 导入、账号列表/删除）。autoclaw 同款接线。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	"workbuddy2api/internal/qoder"
	"workbuddy2api/internal/upstream"
)

// qoderChat POST /v1/qoder/{realm}/chat/completions
//
// 鉴权：X-Authorization/Authorization 携带登录时落盘的 Qoder access token（dt-/jt-），
// 按 token 精确找账号——COSY 签名需要 userId+machineId，纯 token 不够，必须查池。
// 不做本服务 APIKey 校验（与 /v1/autoclaw 同口径，回环使用）。
func (h *Handler) qoderChat(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Qoder == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "qoder 未启用", "type": "service_unavailable"},
		})
		return
	}
	realm := qoder.RealmOf(r.PathValue("realm"))
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "读取请求体失败: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	token := strings.TrimSpace(r.Header.Get("X-Authorization"))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("Authorization"))
		for len(token) > 7 && strings.EqualFold(token[:7], "bearer ") {
			token = strings.TrimSpace(token[7:])
		}
	}
	cred := h.cfg.Qoder.FindByToken(token)
	if cred == nil || cred.RealmOf() != realm {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{
				"message": "qoder " + string(realm) + " 账号未登录或 token 不匹配——先经面板/CLI 登录",
				"type":    "authentication_error",
			},
		})
		return
	}

	var in struct {
		Model               string              `json:"model"`
		Stream              *bool               `json:"stream"`
		Messages            []qoder.ChatMessage `json:"messages"`
		Tools               []qoder.Tool        `json:"tools"`
		MaxTokens           int                 `json:"max_tokens"`
		MaxCompletionTokens int                 `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "请求体不是合法 JSON: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	modelKey := strings.TrimPrefix(strings.TrimSpace(in.Model), "qoder/")
	// 容错：qoder/<realm>/<key> 双前缀形态也接受（realm 已由路径决定）。
	for _, p := range []string{"global/", "cn/"} {
		modelKey = strings.TrimPrefix(modelKey, p)
	}
	if modelKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "model 不能为空", "type": "invalid_request_error"},
		})
		return
	}
	maxTokens := in.MaxTokens
	if in.MaxCompletionTokens > 0 && (maxTokens == 0 || in.MaxCompletionTokens < maxTokens) {
		maxTokens = in.MaxCompletionTokens
	}
	// 双域同协议：system 提升等请求整形对 CN/国际一致。
	systemText, msgs := hoistSystem(in.Messages)
	wantStream := in.Stream == nil || *in.Stream

	creds := cred.CosyCreds()
	modelConfig, err := h.cfg.Qoder.Client().ModelConfig(realm, creds, modelKey, false)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "api_error", "code": "qoder_model_config"},
		})
		return
	}
	rc, err := h.cfg.Qoder.Client().Chat(realm, creds, qoder.ChatRequest{
		ModelKey:   modelKey,
		SystemText: systemText,
		Messages:   msgs,
		Tools:      in.Tools,
		MaxTokens:  maxTokens,
		UserID:     cred.UserID,
	}, modelConfig)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "api_error", "code": "qoder_upstream"},
		})
		return
	}
	defer rc.Close()
	if wantStream {
		_ = upstream.Stream(w, rc)
		return
	}
	resp, err := upstream.Aggregate(rc)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "api_error", "code": "qoder_parse"},
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// hoistSystem 把 role=system 的消息提升为 System 文本（Qoder 拒绝 messages 里带 system）。
func hoistSystem(msgs []qoder.ChatMessage) (string, []qoder.ChatMessage) {
	var parts []string
	var out []qoder.ChatMessage
	for _, m := range msgs {
		if m.Role == "system" {
			if t := qoderContentText(m.Content); t != "" {
				parts = append(parts, t)
			}
			continue
		}
		out = append(out, m)
	}
	return strings.Join(parts, "\n\n"), out
}

// qoderContentText 与 internal/qoder.contentText 同口径的文本化（跨包薄复制）。
func qoderContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			} else if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n\n")
	case nil:
		return ""
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

func writeQoderErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error", "code": code},
	})
}

// qoderQuota GET /admin/qoder/quota —— 按 token 找账号，代理上游配额 JSON。
// 面板侧（bun fetch）直连 openapi 域会被 TLS 指纹拒（401 TOKEN_INVALID），
// 必须由 Go 网关代取。回环使用，无网关 key 校验（与 chat 路由同口径）。
func (h *Handler) qoderQuota(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Qoder == nil {
		writeQoderErr(w, http.StatusServiceUnavailable, "qoder_quota", "qoder 未启用")
		return
	}
	realm := qoder.RealmOf(r.URL.Query().Get("realm"))
	token := strings.TrimSpace(r.Header.Get("X-Authorization"))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("Authorization"))
		for len(token) > 7 && strings.EqualFold(token[:7], "bearer ") {
			token = strings.TrimSpace(token[7:])
		}
	}
	cred := h.cfg.Qoder.FindByToken(token)
	if cred == nil || cred.RealmOf() != realm {
		writeQoderErr(w, http.StatusUnauthorized, "qoder_quota", "账号未登录或 token 不匹配")
		return
	}
	raw, status, err := h.cfg.Qoder.Client().QuotaUsageRaw(realm, cred.AccessToken)
	if err != nil {
		writeQoderErr(w, http.StatusBadGateway, "qoder_quota", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// RegisterQoderAdminRoutes 挂载 /admin/qoder/*（cfg.Qoder 非 nil 且 EnableAdmin 时调用）。
func (h *Handler) RegisterQoderAdminRoutes() {
	h.mux.HandleFunc("OPTIONS /admin/qoder/{path...}", func(w http.ResponseWriter, r *http.Request) {})
	h.mux.HandleFunc("POST /admin/qoder/login/device/start", h.withAuth(h.qoderDeviceStart))
	h.mux.HandleFunc("POST /admin/qoder/login/device/poll", h.withAuth(h.qoderDevicePoll))
	h.mux.HandleFunc("POST /admin/qoder/login/pat", h.withAuth(h.qoderPatImport))
	h.mux.HandleFunc("GET /admin/qoder/quota", h.qoderQuota)
	h.mux.HandleFunc("GET /admin/qoder/accounts", h.withAuth(h.qoderAccounts))
	h.mux.HandleFunc("DELETE /admin/qoder/accounts/{uid}", h.withAuth(h.qoderDeleteAccount))
}

type qoderDeviceStartReq struct {
	Realm string `json:"realm"`
}

// qoderDeviceStart 本地生成 PKCE+nonce+machineId，返回浏览器授权 URL。
// 无服务端状态：poll 时调用方回传 nonce/verifier/machineId（与上游设备流一致，
// 轮询端点用 nonce+verifier 定位会话，上游不签发 device_code）。
func (h *Handler) qoderDeviceStart(w http.ResponseWriter, r *http.Request) {
	var req qoderDeviceStartReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	realm := qoder.RealmOf(req.Realm)
	start, err := h.cfg.Qoder.Client().DeviceFlowStartURL(realm)
	if err != nil {
		writeQoderErr(w, http.StatusInternalServerError, "qoder_start", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"realm":     string(realm),
		"authUrl":   start.AuthURL,
		"nonce":     start.Nonce,
		"verifier":  start.Verifier,
		"machineId": start.MachineID,
		"expiresIn": 300,
		"interval":  2,
	})
}

type qoderDevicePollReq struct {
	Realm     string `json:"realm"`
	Nonce     string `json:"nonce"`
	Verifier  string `json:"verifier"`
	MachineID string `json:"machineId"`
	Persist   *bool  `json:"persist"`
	Label     string `json:"label"`
}

// qoderDevicePoll 单次轮询。pending → {pending:true}；成功 → 落盘（默认 persist=true）
// 并热加载进池，返回账号视图。
func (h *Handler) qoderDevicePoll(w http.ResponseWriter, r *http.Request) {
	var req qoderDevicePollReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Nonce == "" || req.Verifier == "" {
		writeQoderErr(w, http.StatusBadRequest, "qoder_poll", "nonce/verifier 必填")
		return
	}
	realm := qoder.RealmOf(req.Realm)
	res, err := h.cfg.Qoder.Client().DeviceTokenPoll(realm, req.Nonce, req.Verifier)
	if err != nil {
		writeQoderErr(w, http.StatusBadGateway, "qoder_poll", err.Error())
		return
	}
	if res.Pending {
		writeJSON(w, http.StatusOK, map[string]any{"pending": true})
		return
	}
	info := h.cfg.Qoder.Client().UserInfo(realm, res.Token)
	cred := h.qoderSaveAccount(w, realm, res.Token, res.UserID, info, req.MachineID, "device", req.Persist)
	if cred == nil {
		return
	}
	writeJSON(w, http.StatusOK, qoderAccountView(cred))
}

type qoderPatReq struct {
	Realm         string `json:"realm"`
	PersonalToken string `json:"personalToken"`
	Persist       *bool  `json:"persist"`
}

// qoderPatImport PAT（pt-）导入：换 job token（jt-）+ 拉 userid 后按账号落盘。
// 聊天时 jt- 自动走 alt 推理域（api3 拒绝 jt-）。
func (h *Handler) qoderPatImport(w http.ResponseWriter, r *http.Request) {
	var req qoderPatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.PersonalToken) == "" {
		writeQoderErr(w, http.StatusBadRequest, "qoder_pat", "personalToken 必填")
		return
	}
	realm := qoder.RealmOf(req.Realm)
	if !strings.HasPrefix(req.PersonalToken, "pt-") {
		writeQoderErr(w, http.StatusBadRequest, "qoder_pat", "PAT 以 pt- 开头（qoder.com / qoder.cn → Account → Integrations 创建）")
		return
	}
	job, err := h.cfg.Qoder.Client().ExchangeJobToken(realm, req.PersonalToken)
	if err != nil {
		writeQoderErr(w, http.StatusBadGateway, "qoder_pat", err.Error())
		return
	}
	info := h.cfg.Qoder.Client().UserInfo(realm, job.Token)
	cred := h.qoderSaveAccount(w, realm, job.Token, info.UserID, info, "", "pat", req.Persist)
	if cred == nil {
		return
	}
	writeJSON(w, http.StatusOK, qoderAccountView(cred))
}

// qoderSaveAccount 落盘 + 热加载；persist=false 时仅入内存池（不写 auths/）。
func (h *Handler) qoderSaveAccount(w http.ResponseWriter, realm qoder.Realm, token, uid string, info qoder.UserInfo, machineID, method string, persist *bool) *qoder.Credentials {
	cred := &qoder.Credentials{
		AccessToken: token,
		Realm:       string(realm),
		UserID:      uid,
		Name:        info.Name,
		Email:       info.Email,
		OrgID:       info.OrganizationID,
		AuthMethod:  method,
	}
	if machineID != "" {
		cred.MachineID = machineID
	}
	if persist == nil || *persist {
		cred.FilePath = qoder.NewPath(h.cfg.QoderAuthDir, realm, uid)
		if err := cred.SaveAtomic(); err != nil {
			writeQoderErr(w, http.StatusInternalServerError, "qoder_save", err.Error())
			return nil
		}
	}
	h.cfg.Qoder.Add(cred)
	return cred
}

func qoderAccountView(c *qoder.Credentials) map[string]any {
	tokenPrefix := c.AccessToken
	if len(tokenPrefix) > 8 {
		tokenPrefix = tokenPrefix[:8] + "…"
	}
	return map[string]any{
		"realm":       c.Realm,
		"userId":      c.UserID,
		"name":        c.Name,
		"email":       c.Email,
		"authMethod":  c.AuthMethod,
		"expired":     c.NeedsRefresh(0),
		"expiresAt":   c.ExpiresAt,
		"tokenPrefix": tokenPrefix,
		// 面板/CLI 是凭据的所有者（loopback + withAuth 调用），回传完整 token
		// 供 opencodex 账号池保存——聊天时经 X-Authorization 带回，网关按 token 查池签名。
		"accessToken": c.AccessToken,
	}
}

// qoderAccounts GET /admin/qoder/accounts。
func (h *Handler) qoderAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := h.cfg.Qoder.Accounts()
	out := make([]map[string]any, 0, len(accounts))
	for _, c := range accounts {
		out = append(out, qoderAccountView(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// qoderDeleteAccount DELETE /admin/qoder/accounts/{uid}?realm= —— 删文件 + 摘池。
func (h *Handler) qoderDeleteAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	realm := qoder.RealmOf(r.URL.Query().Get("realm"))
	var target *qoder.Credentials
	for _, c := range h.cfg.Qoder.Accounts() {
		if c.UserID == uid && c.RealmOf() == realm {
			target = c
			break
		}
	}
	if target == nil {
		writeQoderErr(w, http.StatusNotFound, "qoder_account", "账号不存在")
		return
	}
	h.cfg.Qoder.RemoveByToken(target.AccessToken)
	if target.FilePath != "" {
		if err := os.Remove(target.FilePath); err != nil && !os.IsNotExist(err) {
			writeQoderErr(w, http.StatusInternalServerError, "qoder_delete", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": uid})
}
