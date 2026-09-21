// chat.go Qoder 推理调用：模型目录（model_config 必须逐账号 live 拉取——发错
// model_config 上游会静默降级到别的模型）、Qoder 请求体构造、COSY 签名出站、
// {statusCodeValue, body} SSE 信封解包为标准 OpenAI SSE。
//
// 请求体形状移植自 9router executors/qoder.js buildQoderRequestBody。
package qoder

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ChatMessage 出站消息（OpenAI 兼容形态；system 由网关层提升到 System 字段）。
type ChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// Tool 定义透传（Qoder 支持 function tool 形状）。
type Tool = map[string]any

// ChatRequest 网关侧收集到的对话要素。
type ChatRequest struct {
	ModelKey   string // Qoder 规范模型键（auto/ultimate/...）
	SystemText string // 提升后的 system 文本
	Messages   []ChatMessage
	Tools      []Tool
	MaxTokens  int
	UserID     string // 会话/记录 id 稳定化用
}

type chatContextExtra struct {
	Context         []any       `json:"context"`
	ModelConfigInfo modelCfgRef `json:"modelConfig"`
	OriginalContent string      `json:"originalContent"`
}

type modelCfgRef struct {
	Key         string `json:"key"`
	IsReasoning bool   `json:"is_reasoning"`
}

type chatContext struct {
	ChatPrompt string           `json:"chatPrompt"`
	ImageUrls  any              `json:"imageUrls"`
	Extra      chatContextExtra `json:"extra"`
	Features   []any            `json:"features"`
	Text       string           `json:"text"`
}

type chatBusiness struct {
	Product string `json:"product"`
	Version string `json:"version"`
	Type    string `json:"type"`
	Stage   string `json:"stage"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	BeginAt int64  `json:"begin_at"`
}

type chatPayload struct {
	RequestID      string         `json:"request_id"`
	RequestSetID   string         `json:"request_set_id"`
	ChatRecordID   string         `json:"chat_record_id"`
	SessionID      string         `json:"session_id"`
	Stream         bool           `json:"stream"`
	ChatTask       string         `json:"chat_task"`
	IsReply        bool           `json:"is_reply"`
	IsRetry        bool           `json:"is_retry"`
	Source         int            `json:"source"`
	Version        string         `json:"version"`
	SessionType    string         `json:"session_type"`
	AgentID        string         `json:"agent_id"`
	TaskID         string         `json:"task_id"`
	CodeLanguage   string         `json:"code_language"`
	ChatPrompt     string         `json:"chat_prompt"`
	ImageUrls      any            `json:"image_urls"`
	AliyunUserType string         `json:"aliyun_user_type"`
	System         string         `json:"system"`
	Messages       []ChatMessage  `json:"messages"`
	Tools          []Tool         `json:"tools"`
	Parameters     map[string]int `json:"parameters"`
	ChatContext    chatContext    `json:"chat_context"`
	ModelConfig    map[string]any `json:"model_config"`
	Business       chatBusiness   `json:"business"`
}

// stableHash 上游 9router stableHash 口径：sha256(prefix + 分段 NUL 连接) 取前 16 hex。
// 会话/记录 id 用它跨请求稳定（上游业务块要求稳定 ID）。
func stableHash(prefix string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(prefix))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// buildChatPayload 构造 Qoder 原生请求体。
func buildChatPayload(req ChatRequest, modelConfig map[string]any) chatPayload {
	maxTokens := 32768
	if v, ok := modelConfig["max_output_tokens"].(float64); ok && v > 0 {
		maxTokens = int(v)
	}
	if req.MaxTokens > 0 && req.MaxTokens < maxTokens {
		maxTokens = req.MaxTokens
	}
	isReasoning := false
	if v, ok := modelConfig["is_reasoning"].(bool); ok {
		isReasoning = v
	}
	lastUser := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUser = contentText(req.Messages[i].Content)
			break
		}
	}
	recordID := stableHash("qoder-record", req.ModelKey,
		messagesFingerprint(req.Messages), fmt.Sprintf("mt=%d", maxTokens))
	sessionID := stableHash("qoder-session", req.UserID, req.ModelKey)

	return chatPayload{
		RequestID:      newUUIDv4(),
		RequestSetID:   recordID,
		ChatRecordID:   recordID,
		SessionID:      sessionID,
		Stream:         true,
		ChatTask:       "FREE_INPUT",
		IsReply:        true,
		IsRetry:        false,
		Source:         1,
		Version:        "3",
		SessionType:    "qodercli",
		AgentID:        "agent_common",
		TaskID:         "common",
		CodeLanguage:   "",
		ChatPrompt:     "",
		ImageUrls:      nil,
		AliyunUserType: "",
		System:         req.SystemText,
		Messages:       req.Messages,
		Tools:          req.Tools,
		Parameters:     map[string]int{"max_tokens": maxTokens},
		ChatContext: chatContext{
			Extra: chatContextExtra{
				Context:         []any{},
				ModelConfigInfo: modelCfgRef{Key: req.ModelKey, IsReasoning: isReasoning},
				OriginalContent: lastUser,
			},
			Features: []any{},
			Text:     lastUser,
		},
		ModelConfig: modelConfig,
		Business: chatBusiness{
			Product: "cli", Version: CosyIDEVersion, Type: "agent", Stage: "start",
			ID: newUUIDv4(), Name: truncateRunes(lastUser, 30), BeginAt: time.Now().UnixMilli(),
		},
	}
}

// messagesFingerprint 消息列表指纹（role+content 摘要），进 record id 稳定化。
func messagesFingerprint(msgs []ChatMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteByte(0)
		b.WriteString(contentText(m.Content))
		b.WriteByte(0)
	}
	return stableHash("msgs", b.String())
}

// contentText content 的最佳文本化：字符串直取；数组取 text 字段拼接；其余 JSON 摘要。
func contentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok && t != "" {
					parts = append(parts, t)
					continue
				}
			}
			if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
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

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// ── 模型目录（每账号缓存 1h；model_config 缺失 = 硬错误，静默降级不可接受）──

type catalogEntry struct {
	ExpiresAt  time.Time
	RawConfigs map[string]map[string]any
}

type catalogCache struct {
	mu    sync.Mutex
	items map[string]*catalogEntry
}

var catalogs = &catalogCache{items: map[string]*catalogEntry{}}

func catalogCacheKey(realm Realm, creds CosyCredentials) string {
	return stableHash("qoder-catalog", string(realm), creds.UserID, creds.AuthToken)
}

// InvalidateCatalog 账号 token 变化后由调用方清缓存。
func InvalidateCatalog(realm Realm, creds CosyCredentials) {
	catalogs.mu.Lock()
	delete(catalogs.items, catalogCacheKey(realm, creds))
	catalogs.mu.Unlock()
}

// ModelConfig 取指定模型的 model_config 块；缓存 1h。缺失即硬错误（错误信息
// 与上游口径一致：提示先拉目录）。
func (c *Client) ModelConfig(realm Realm, creds CosyCredentials, modelKey string, forceRefresh bool) (map[string]any, error) {
	key := catalogCacheKey(realm, creds)
	catalogs.mu.Lock()
	entry := catalogs.items[key]
	catalogs.mu.Unlock()
	if entry == nil || time.Now().After(entry.ExpiresAt) || forceRefresh {
		configs, err := c.fetchCatalog(realm, creds)
		if err != nil {
			return nil, err
		}
		entry = &catalogEntry{ExpiresAt: time.Now().Add(time.Hour), RawConfigs: configs}
		catalogs.mu.Lock()
		catalogs.items[key] = entry
		catalogs.mu.Unlock()
	}
	cfg, ok := entry.RawConfigs[modelKey]
	if !ok {
		return nil, fmt.Errorf("qoder: model_config for %q not yet known (run a model list fetch or check upstream connectivity)", modelKey)
	}
	return cfg, nil
}

// fetchCatalog COSY 签名 GET /algo/api/v2/model/list，展开为 key→model_config。
// 返回结构兼容 {data:{models:[...]}}、{body:{models:[...]}}、顶层数组三种形态。
func (c *Client) fetchCatalog(realm Realm, creds CosyCredentials) (map[string]map[string]any, error) {
	ep := NewEndpoints(realm)
	if ep.ChatBase == "" {
		return nil, fmt.Errorf("qoder %s: chat base 未配置（直连端点未验证）——设置 %s 后重试", realm, EnvCNChat)
	}
	u := ep.ChatBase + ModelListPath
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	headers, err := BuildCosyHeaders(nil, u, creds)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, headers)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity") // gzip 触发 CDN 签名校验失败
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qoder model list: HTTP %d: %.200s", resp.StatusCode, string(raw))
	}
	var env struct {
		StatusCodeValue int             `json:"statusCodeValue"`
		Body            json.RawMessage `json:"body"`
		Data            json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("qoder model list: invalid JSON: %w", err)
	}
	inner := env.Data
	if len(inner) == 0 {
		inner = env.Body
	}
	if len(inner) == 0 {
		// 非 {statusCodeValue,data|body} 信封：响应体本身就是目录 JSON（实测
		// /algo/api/v2/model/list 返回裸 {"chat":[...]}）。
		inner = raw
	}
	var list []map[string]any
	if len(inner) > 0 {
		var wrap struct {
			Models    []map[string]any `json:"models"`
			ModelList []map[string]any `json:"model_list"`
			List      []map[string]any `json:"list"`
			Chat      []map[string]any `json:"chat"`
		}
		if err := json.Unmarshal(inner, &wrap); err == nil && (wrap.Models != nil || wrap.ModelList != nil || wrap.List != nil || wrap.Chat != nil) {
			for _, candidate := range [][]map[string]any{wrap.Models, wrap.Chat, wrap.ModelList, wrap.List} {
				if candidate != nil {
					list = candidate
					break
				}
			}
		} else if err := json.Unmarshal(inner, &list); err != nil {
			return nil, fmt.Errorf("qoder model list: unexpected shape: %w", err)
		}
	}
	configs := map[string]map[string]any{}
	for _, m := range list {
		k, _ := m["key"].(string)
		if k == "" {
			k, _ = m["model_key"].(string)
		}
		if k == "" {
			if name, ok := m["model"].(string); ok {
				k = name
			}
		}
		if k == "" {
			continue
		}
		configs[k] = m
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("qoder model list: parsed 0 models from %.400s", string(raw))
	}
	return configs, nil
}

func applyHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		req.Header.Set(k, v)
	}
}

// ChatUrls 计算某凭据应使用的 chat URL（jt- job token 走 alt 域）。
func ChatUrls(realm Realm, authToken string) (string, error) {
	ep := NewEndpoints(realm)
	base := ep.ChatBase
	if strings.HasPrefix(authToken, "jt-") {
		base = ep.ChatBaseAlt
	}
	if base == "" {
		return "", fmt.Errorf("qoder %s: chat base 未配置（直连端点未验证）——设置 %s 后重试", realm, EnvCNChat)
	}
	_ = realm // CN 与国际版同协议同路径（已实测），仅 base 不同
	return base + "/algo" + ChatSigPath + "?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1", nil
}

// Chat 执行一次对话：构造 payload → Encode → COSY 签名 → POST → 解包 SSE。
// 返回标准 OpenAI SSE 流（逐帧 data: {...}，终止 [DONE]）。
func (c *Client) Chat(realm Realm, creds CosyCredentials, req ChatRequest, modelConfig map[string]any) (io.ReadCloser, error) {
	payload := buildChatPayload(req, modelConfig)
	plain, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	encoded := EncodeBody(plain)
	u, err := ChatUrls(realm, creds.AuthToken)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	headers, err := BuildCosyHeaders(encoded, u, creds)
	if err != nil {
		return nil, err
	}
	applyHeaders(httpReq, headers)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	httpReq.Header.Set("Accept-Encoding", "identity") // gzip 会破坏 CDN 的签名校验
	httpReq.Header.Set("X-Model-Key", req.ModelKey)
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("qoder chat: HTTP %d: %.300s", resp.StatusCode, string(raw))
	}
	return unwrapSSE(resp.Body, req.ModelKey), nil
}

// unwrapSSE 把 Qoder {statusCodeValue, body} 信封流转成标准 OpenAI SSE 流。
// 终止帧（[DONE] 或错误帧）出现后主动关闭上游——上游在终止帧后保持 keepalive，
// 不关闭会挂住非流式消费方。statusCodeValue != 200 时合成 OpenAI 错误帧。
func unwrapSSE(src io.ReadCloser, model string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer src.Close()
		scanner := bufio.NewScanner(src)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		done := false
		for scanner.Scan() && !done {
			line := strings.TrimRight(scanner.Text(), "\r")
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				pw.Write([]byte("data: [DONE]\n\n"))
				done = true
				break
			}
			var env struct {
				StatusCodeValue int             `json:"statusCodeValue"`
				Body            json.RawMessage `json:"body"`
			}
			if err := json.Unmarshal([]byte(data), &env); err != nil {
				continue // 非 JSON 心跳帧忽略
			}
			// statusCodeValue 缺失的帧按 200 处理（上游 9router 同口径）。
			if env.StatusCodeValue == 0 {
				env.StatusCodeValue = 200
			}
			// body 是 JSON 字符串字面量（内嵌转义的 OpenAI chunk JSON）——先去引号；
			// 若上游改发对象则原样保留（已是 JSON 文本）。与 9router 的解包口径一致。
			inner := ""
			if len(env.Body) > 0 {
				var s string
				if err := json.Unmarshal(env.Body, &s); err == nil {
					inner = s
				} else {
					inner = strings.TrimSpace(string(env.Body))
				}
			}
			if env.StatusCodeValue != 200 {
				errChunk := map[string]any{
					"id":      fmt.Sprintf("qoder-error-%d", time.Now().UnixMilli()),
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []any{map[string]any{
						"index":         0,
						"delta":         map[string]any{"content": fmt.Sprintf("\n[qoder error %d: %.200s]", env.StatusCodeValue, inner)},
						"finish_reason": "stop",
					}},
				}
				raw, _ := json.Marshal(errChunk)
				pw.Write([]byte("data: " + string(raw) + "\n\n"))
				pw.Write([]byte("data: [DONE]\n\n"))
				done = true
				break
			}
			if inner == "" || inner == "null" {
				continue
			}
			if inner == "[DONE]" {
				pw.Write([]byte("data: [DONE]\n\n"))
				done = true
				break
			}
			pw.Write([]byte("data: " + inner + "\n\n"))
		}
		pw.CloseWithError(scanner.Err())
	}()
	return pr
}
