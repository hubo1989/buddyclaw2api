// client_test.go httptest 桩：覆盖登录/刷新/签到/积分/chat 的正常与错误路径。
package autoclaw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.Handler) (*Client, *Credentials) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL)
	cred := &Credentials{
		AccessToken:  "tok-access",
		RefreshToken: "tok-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		DeviceID:     newDeviceID(),
		Phone:        "13800001234",
		UID:          "u-test",
	}
	return c, cred
}

func env(code int, data string) string {
	if data == "" {
		data = "null"
	}
	return `{"code":` + itoa(code) + `,"msg":"SUCCESS","data":` + data + `}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func TestSendCodeAndLogin(t *testing.T) {
	mux := http.NewServeMux()
	var gotSend, gotLogin map[string]any
	mux.HandleFunc("/userapi/v1/agent-send-code", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotSend)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(env(0, `{"result":true}`)))
	})
	mux.HandleFunc("/userapi/v1/agent-login/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotLogin)
		// code 必须是 JSON 数字（对齐官方客户端 Number() 转换），发字符串会被服务端判 400001。
		if _, ok := gotLogin["code"].(float64); !ok {
			t.Errorf("code must be sent as a JSON number, got %T (%v)", gotLogin["code"], gotLogin["code"])
		}
		if gotLogin["code"] != float64(1234) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Invalid token"}`))
			return
		}
		_, _ = w.Write([]byte(env(0, `{"access_token":"at","refresh_token":"rt","user_id":"u1","user_name":"tester"}`)))
	})
	c, _ := newTestClient(t, mux)
	ctx := context.Background()

	if err := c.SendCode(ctx, "13800001234", "dev"); err != nil {
		t.Fatalf("send-code: %v", err)
	}
	if gotSend["phone"] != "13800001234" || gotSend["source_id"] != Product {
		t.Fatalf("send-code body mismatch: %v", gotSend)
	}
	if _, err := c.Login(ctx, "13800001234", "9999", "dev"); err == nil {
		t.Fatal("expected 401 error for wrong code")
	}
	lr, err := c.Login(ctx, "13800001234", "1234", "dev")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if lr.AccessToken != "at" || lr.RefreshToken != "rt" || lr.UID != "u1" {
		t.Fatalf("login result mismatch: %+v", lr)
	}
}

func TestRefreshRotation(t *testing.T) {
	mux := http.NewServeMux()
	var gotRefresh map[string]any
	mux.HandleFunc(PathRefresh, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotRefresh)
		_, _ = w.Write([]byte(env(0, `{"access_token":"at2","refresh_token":"rt2","expires_in":3600}`)))
	})
	c, cred := newTestClient(t, mux)
	if err := c.Refresh(context.Background(), cred); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if gotRefresh["refresh_token"] != "tok-refresh" {
		t.Fatalf("refresh body mismatch: %v", gotRefresh)
	}
	cred.mu.Lock()
	at, rt := cred.AccessToken, cred.RefreshToken
	exp := cred.ExpiresAt
	cred.mu.Unlock()
	if at != "at2" || rt != "rt2" {
		t.Fatalf("rotation failed: at=%s rt=%s", at, rt)
	}
	if exp <= time.Now().Unix() {
		t.Fatalf("expires not advanced: %d", exp)
	}
}

func TestTaskSigninIdempotent(t *testing.T) {
	mux := http.NewServeMux()
	var completed bool
	mux.HandleFunc(PathTaskList, func(w http.ResponseWriter, r *http.Request) {
		status := "incomplete"
		if completed {
			status = "completed"
		}
		_, _ = w.Write([]byte(env(0, `[{"task_id":"daily_signin","status":"`+status+`","reward_points":400}]`)))
	})
	mux.HandleFunc(PathTaskComplete, func(w http.ResponseWriter, r *http.Request) {
		completed = true
		_, _ = w.Write([]byte(env(0, `{"success":true,"already_completed":false,"reward_points":400}`)))
	})
	c, cred := newTestClient(t, mux)
	ctx := context.Background()

	st, err := c.TaskList(ctx, cred.AccessToken, cred.DeviceID)
	if err != nil {
		t.Fatalf("task-list: %v", err)
	}
	if st.Status != "incomplete" || st.Reward != 400 {
		t.Fatalf("task status mismatch: %+v", st)
	}
	res, err := c.TaskComplete(ctx, cred.AccessToken, cred.DeviceID)
	if err != nil {
		t.Fatalf("task-complete: %v", err)
	}
	if !res.Success || res.RewardPoints != 400 {
		t.Fatalf("complete result mismatch: %+v", res)
	}
	st2, _ := c.TaskList(ctx, cred.AccessToken, cred.DeviceID)
	if st2.Status != "completed" {
		t.Fatalf("expected completed after signin")
	}
}

func TestWallets(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/agent-assetmgr/api/v2/wallets", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("biz_app_id") != Product {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(env(0, `{"schema_version":"wallets.v2","total_balance":95100,"wallets":[{"public_wallet_type":"reward","display_name":"奖励积分","balance":95100,"balance_view":"9.5w","display":true,"priority":10}]}`)))
	})
	c, cred := newTestClient(t, mux)
	w, err := c.Wallets(context.Background(), cred.AccessToken, cred.DeviceID)
	if err != nil {
		t.Fatalf("wallets: %v", err)
	}
	if w.Total != 95100 || len(w.Wallets) != 1 || w.Wallets[0].Type != "reward" {
		t.Fatalf("wallets mismatch: %+v", w)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{http.StatusUnauthorized, `{"error":"Invalid token"}`, ErrAuth},
		{http.StatusTooManyRequests, "rate limited", ErrSoftRate},
		{http.StatusInternalServerError, "boom", ErrServer},
		{http.StatusBadRequest, `{"message":"非法模型"}`, ErrClient},
	}
	for _, tc := range cases {
		mux := http.NewServeMux()
		mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		c, cred := newTestClient(t, mux)
		_, err := c.callBiz(context.Background(), http.MethodGet, "/boom", BizHeaders(cred.AccessToken), nil)
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		var ue *Error
		if !asError(err, &ue) || ue.Kind != tc.want {
			t.Fatalf("status %d: got %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestChatStreamAndNonStream(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/autoclaw-proxy/proxy/autoclaw/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Authorization") != "Bearer tok-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Request-Model") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	c, cred := newTestClient(t, mux)
	body := []byte(`{"model":"zai_auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	status, _, rc, err := c.ChatStream(context.Background(), cred.AccessToken, "zai_auto", body)
	if err != nil || status != 200 || rc == nil {
		t.Fatalf("chat stream: status=%d err=%v", status, err)
	}
	defer rc.Close()
	buf := make([]byte, 512)
	n, _ := rc.Read(buf)
	if !strings.Contains(string(buf[:n]), "chat.completion.chunk") {
		t.Fatalf("unexpected stream: %s", string(buf[:n]))
	}
	// 401 路径（测试桩未写 body，raw 允许为空）
	status, raw, rc2, err := c.ChatStream(context.Background(), "bad-token", "zai_auto", body)
	if err != nil || status != http.StatusUnauthorized || rc2 != nil {
		t.Fatalf("chat 401: status=%d rc=%v err=%v", status, rc2, err)
	}
	_ = raw
}

func TestChatErrorStatusClassified(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/autoclaw-proxy/proxy/autoclaw/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"非法模型"}`))
	})
	c, cred := newTestClient(t, mux)
	status, raw, rc, err := c.ChatStream(context.Background(), cred.AccessToken, "bogus", []byte(`{"model":"bogus"}`))
	if err != nil {
		t.Fatalf("transport err: %v", err)
	}
	if status != 400 || rc != nil {
		t.Fatalf("expected 400 without body reader, got %d", status)
	}
	if kind := ClassifyHTTP(status, string(raw)); kind == ErrServer {
		t.Fatalf("400 should not classify as server error")
	}
}

// 地区拒绝时换 host：国际 host 回 630015（当前地区暂不支持手机号注册）→ 国内 host 成功。
func TestLoginAnyHostFallsBackOnRegionReject(t *testing.T) {
	intl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":630015,"msg":"当前地区暂不支持手机号注册，请使用其他方式登录","data":null}`))
	}))
	defer intl.Close()
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		if _, ok := got["code"].(float64); !ok {
			t.Errorf("code must be a JSON number, got %T", got["code"])
		}
		_, _ = w.Write([]byte(env(0, `{"access_token":"at-cn","refresh_token":"rt-cn","user_id":"u-cn","user_name":"cn"}`)))
	}))
	defer cn.Close()

	c := NewClient(intl.URL)
	lr, host, err := c.LoginAnyHost(context.Background(), "13800001234", "123456", "dev", cn.URL)
	if err != nil {
		t.Fatalf("expected fallback success, got %v", err)
	}
	if host != cn.URL {
		t.Fatalf("expected CN host %s, got %s", cn.URL, host)
	}
	if lr.AccessToken != "at-cn" || lr.UID != "u-cn" {
		t.Fatalf("unexpected login result: %+v", lr)
	}
}

// 验证码错误（630202）不该触发换 host —— 换 host 也救不了，避免无谓请求与误导。
func TestLoginAnyHostStopsOnVerificationError(t *testing.T) {
	var cnHits int
	intl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":630202,"msg":"抱歉,验证码错误，请输入正确验证码！","data":null}`))
	}))
	defer intl.Close()
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnHits++
		_, _ = w.Write([]byte(env(0, `{"access_token":"at","refresh_token":"rt"}`)))
	}))
	defer cn.Close()

	c := NewClient(intl.URL)
	_, _, err := c.LoginAnyHost(context.Background(), "13800001234", "000000", intl.URL, cn.URL)
	if err == nil {
		t.Fatal("expected verification error to surface")
	}
	if !strings.Contains(err.Error(), "630202") {
		t.Fatalf("expected 630202 in error, got %v", err)
	}
	if cnHits != 0 {
		t.Fatalf("must not try the other host on a verification error, hits=%d", cnHits)
	}
}

// 凭据记录了 host 时，ForCred 生成的 client 指向该 host。
func TestForCredUsesCredentialHost(t *testing.T) {
	c := NewClient("")
	plain := c.ForCred(&Credentials{})
	if plain.Host != DefaultHost {
		t.Fatalf("empty host should stay on default, got %s", plain.Host)
	}
	cnClient := c.ForCred(&Credentials{Host: CNHost})
	if cnClient.Host != CNHost {
		t.Fatalf("expected %s, got %s", CNHost, cnClient.Host)
	}
	if c.Host != DefaultHost {
		t.Fatalf("original client must not be mutated, got %s", c.Host)
	}
}

// 手机验证码登录固定走国内 host（不做"先试国际"的无谓请求）。
func TestHostForPhoneForcesCN(t *testing.T) {
	c := NewClient("") // 默认国际 host
	if got := c.HostForPhone(); got != CNHost {
		t.Fatalf("phone login must target the CN host, got %s", got)
	}
	// 部署方显式配置了自定义 host 时尊重配置
	custom := NewClient("https://example.internal")
	if got := custom.HostForPhone(); got != "https://example.internal" {
		t.Fatalf("explicit host must win, got %s", got)
	}
}

// LoginPhone 打到国内 host，并把 host 一并返回给凭据。
func TestLoginPhoneTargetsCNHost(t *testing.T) {
	var hitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		_, _ = w.Write([]byte(env(0, `{"access_token":"at","refresh_token":"rt","user_id":"u1"}`)))
	}))
	defer srv.Close()

	c := NewClient("") // 默认国际
	c = c.WithHost("") // 保持默认；用替换 CN 的方式验证（测试里 CN 指向桩）
	// 直接验证 LoginPhone 用的是 HostForPhone() 的结果
	cn := c.WithHost(srv.URL)
	lr, host, err := cn.LoginPhone(context.Background(), "13800001234", "123456", "dev")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if host != srv.URL {
		t.Fatalf("returned host should be the CN target, got %s", host)
	}
	if lr.AccessToken != "at" || hitPath != PathLogin {
		t.Fatalf("unexpected result: %+v path=%s", lr, hitPath)
	}
}
