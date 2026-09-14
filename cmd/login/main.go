// login.go — WorkBuddy CN OAuth 登录（与 CPA 插件 /root/qoderwork/workbuddy/oauth.go
// 的 handleStartLogin + handlePollLogin 逐字一致的实现，CN realm only）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login url   → POST /v2/plugin/auth/state?platform=CLI 拿 state+authUrl，
//	              state 落 /tmp/wb2api-login-state.json，stdout 打印授权 URL
//	login poll  → 读 state，轮询 GET /v2/plugin/auth/token?state= 直到登录完成（默认最长 5 分钟），
//	              成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	              stdout 打印完整 token+account JSON（进度写 stderr）
//
// 无 PKCE（workbuddy 设备流由服务端签发 state，与 qoderwork 不同）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strconv"
	"time"
)

// 与 /root/qoderwork/workbuddy/main.go:82-96 完全一致的常量（CN only）
const (
	upstreamBaseCN    = "https://copilot.tencent.com"
	clientUA          = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer     = "https://www.codebuddy.cn"
	endpointAuthState = upstreamBaseCN + "/v2/plugin/auth/state?platform=CLI"
	endpointLoginAcct = upstreamBaseCN + "/v2/plugin/login/account?state="
	endpointAuthToken = upstreamBaseCN + "/v2/plugin/auth/token?state="
	stateFile         = "/tmp/wb2api-login-state.json"
)

// commonHeaders 与 main.go:496-503 一致
func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
}

// apiEnvelope 与 main.go:429-433 一致
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 与 oauth.go:33-66 一致：{code,msg,data} 信封，code!=0 → error
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

type loginState struct {
	State string `json:"state"`
}

// pollToken 轮询 auth/token 直到登录完成或超时。
//
// 为什么必须轮询：登录完成发生在浏览器侧，时机不可控。早先的实现只请求一次，
// 于是"用户在浏览器点完授权"与"脚本发起 poll"之间只要有一点不同步（按 y 太早、
// 网络往返、上游状态同步延迟），就会直接判定失败——实测中连续 4 个账号全部卡在
// 这一步、一份凭证都没落盘，而事后补 poll 同一 state 却一次就成功。
//
// 每次尝试间隔 2s，默认最长 5 分钟；可用 LOGIN_POLL_TIMEOUT（秒）覆盖。
// 进度只写 stderr，stdout 保持"仅最终 JSON"的契约不变。
func pollToken(client *http.Client, state string) (json.RawMessage, error) {
	timeout := 300 * time.Second
	if v := os.Getenv("LOGIN_POLL_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	const interval = 2 * time.Second
	start := time.Now()
	var lastErr error
	for attempt := 1; ; attempt++ {
		tokRaw, _, err := doJSON(client, http.MethodGet, endpointAuthToken+state, nil, nil)
		if err == nil {
			return tokRaw, nil
		}
		lastErr = err
		// pending 时上游返回业务 code 非 0（"login ing"），HTTP 可能是 200 或 4xx；
		// 网络错误/5xx 也可能是瞬时的 —— 一律重试到超时，再由下面统一汇报。
		if time.Since(start) >= timeout {
			return nil, fmt.Errorf("等待登录超时（已等 %s，共尝试 %d 次），最后一次错误：%v\n"+
				"  请在浏览器打开 ./login.sh 最新打印的那个链接完成登录后重跑", timeout, attempt, lastErr)
		}
		if attempt == 1 || attempt%5 == 0 {
			fmt.Fprintf(os.Stderr, "  等待浏览器完成登录… %ds\n", int(time.Since(start).Seconds()))
		}
		time.Sleep(interval)
	}
}

func main() {
	if len(os.Args) < 2 {
		fatal("这是 login.sh 的底层工具，本身不保存账号。\n" +
			"  要完成登录，请直接运行：./login.sh\n" +
			"  （子命令：url 只取授权链接；poll 轮询换取 token —— 二者由 login.sh 编排，单独跑 url 不会有任何落盘）")
	}
	// 每个流程独立 cookie jar（oauth.go:22-29：多账号登录互不串会话）
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	switch os.Args[1] {
	case "url":
		// handleStartLogin (oauth.go:68-87)
		data, _, err := doJSON(client, http.MethodPost, endpointAuthState, nil, bytes.NewReader([]byte("{}")))
		if err != nil {
			fatal("auth state failed: %v", err)
		}
		var st struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		}
		if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
			fatal("auth state: missing state or authUrl")
		}
		raw, _ := json.Marshal(loginState{State: st.State})
		if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
			fatal("write state: %v", err)
		}
		fmt.Println(st.AuthURL)
		// 单跑 `login url` 是最常见的误用：拿到链接就以为登录完成，实则没有任何落盘。
		// login.sh 会设 WB2A_LOGIN_ORCHESTRATED=1 来抑制这句提示。
		if os.Getenv("WB2A_LOGIN_ORCHESTRATED") == "" {
			fmt.Fprintln(os.Stderr, "提示：`login url` 只负责取授权链接，不会保存账号。"+
				"要完成登录并落盘凭证，请直接运行 ./login.sh")
		}

	case "poll":
		raw, err := os.ReadFile(stateFile)
		if err != nil {
			fatal("read state: %v (先跑 login url)", err)
		}
		var ls loginState
		if err := json.Unmarshal(raw, &ls); err != nil {
			fatal("parse state: %v", err)
		}
		// handlePollLogin (oauth.go:108-162)：auth/token 是权威登录状态端点，
		// pending 时业务 code 非 0（"login ing"），完成时 code=0 + token bundle。
		// pollToken 会一直等到登录完成（详见该函数注释）。
		tokRaw, errTok := pollToken(client, ls.State)
		if errTok != nil {
			fatal("%v", errTok)
		}
		var tok struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		}
		if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
			fatal("token 响应里没有 accessToken（上游返回异常），请重跑 ./login.sh")
		}
		// login/account 拿 uid/nickname（带 Bearer）
		var acct struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		acctHeaders := func(r *http.Request) {
			commonHeaders(r)
			r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}
		if acctRaw, _, errAcct := doJSON(client, http.MethodGet, endpointLoginAcct+ls.State, acctHeaders, nil); errAcct == nil {
			_ = json.Unmarshal(acctRaw, &acct)
		}
		out := map[string]any{
			"access_token":  tok.AccessToken,
			"refresh_token": tok.RefreshToken,
			"expires_in":    tok.ExpiresIn,
			"domain":        tok.Domain,
			"uid":           acct.UID,
			"enterprise_id": acct.EnterpriseID,
			"nickname":      acct.Nickname,
		}
		oraw, _ := json.Marshal(out)
		fmt.Println(string(oraw))
		os.Remove(stateFile)

	default:
		fatal("unknown subcommand %q（want url|poll）—— 完整登录请直接运行 ./login.sh", os.Args[1])
	}
}
