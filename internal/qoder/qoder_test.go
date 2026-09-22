// qoder_test.go 协议核心测试。EncodeBody 固定向量直接取自 9router 原版 JS 实现
// （node 直跑 shared/qoder/encoding.js 生成），保证跨语言一致；COSY 用签名输入
// 重建 md5 做自洽校验；unwrapSSE 覆盖信封解包/错误帧/keepalive 截断。
package qoder

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestEncodeBody_ReferenceVectors 向量来自 9router JS 原版实现直跑输出。
func TestEncodeBody_ReferenceVectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abcdef", "FOKMDH#S"},
		{"hello", "q$FruHPH"},
		{"", ""},
		{"{messages:[{role:user,content:hi}],stream:true}",
			"Wx,%^JZKL#SwyJZK.Dxw$@).N(FYGHjbuECLuEpyPHLm(.LNkjiD(F^lLtXNOWrD"},
	}
	for _, c := range cases {
		got := string(EncodeBody([]byte(c.in)))
		if got != c.want {
			t.Errorf("EncodeBody(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEncodeBody_LengthPreserved base64 长度保持（含 padding 边界）。
func TestEncodeBody_LengthPreserved(t *testing.T) {
	for _, in := range []string{"abcdef", "hello", "x", "hello world this is a longer string 0123456789"} {
		if got := len(EncodeBody([]byte(in))); got != base64.StdEncoding.EncodedLen(len(in)) {
			t.Errorf("EncodeBody(%q) length = %d, want base64 length %d", in, got, base64.StdEncoding.EncodedLen(len(in)))
		}
	}
}

// TestEncodeBody_CustomAlphabetOnly 输出只含自定义字母表 + '$'。
func TestEncodeBody_CustomAlphabetOnly(t *testing.T) {
	allowed := qoderCustomAlphabet + "$"
	out := string(EncodeBody([]byte("hello world this is a longer string for testing 0123456789")))
	for _, ch := range out {
		if !strings.ContainsRune(allowed, ch) {
			t.Fatalf("unexpected char %q in output %q", ch, out)
		}
	}
}

// TestComputeSigPath 剥 /algo 前缀；其余路径原样。
func TestComputeSigPath(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?x=1", "/api/v2/service/pro/sse/agent_chat_generation"},
		{"https://api3.qoder.sh/algo/api/v2/model/list", "/api/v2/model/list"},
		{"https://api3.qoder.com.cn/api/v1/other", "/api/v1/other"},
	}
	for _, c := range cases {
		if got := ComputeSigPath(c.url); got != c.want {
			t.Errorf("ComputeSigPath(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// TestBuildCosyHeaders_SelfConsistent 签名可由返回头重建验证：
// sig == md5(payloadB64 LF cosyKey LF ts LF body LF sigPath)。
func TestBuildCosyHeaders_SelfConsistent(t *testing.T) {
	body := []byte("Wx,%^JZKL#SwyJZK.Dxw$@).N(FYGHjbuECLuEpyPHLm(.LNkjiD(F^lLtXNOWrD")
	u := "https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	h, err := BuildCosyHeaders(body, u, CosyCredentials{UserID: "u1", AuthToken: "dt-abc", MachineID: "m-1"})
	if err != nil {
		t.Fatal(err)
	}
	auth := h["Authorization"]
	if !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Fatalf("Authorization shape: %q", auth)
	}
	rest := strings.TrimPrefix(auth, "Bearer COSY.")
	parts := strings.SplitN(rest, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("Authorization payload.sig split failed: %q", auth)
	}
	sigInput := parts[0] + "\n" + h["Cosy-Key"] + "\n" + h["Cosy-Date"] + "\n" + string(body) + "\n" + h["Cosy-Sigpath"]
	want := hex.EncodeToString(mustMD5(sigInput))
	if parts[1] != want {
		t.Errorf("sig mismatch: got %s want %s", parts[1], want)
	}
	if bh := hex.EncodeToString(mustMD5(string(body))); h["Cosy-Bodyhash"] != bh {
		t.Errorf("Cosy-Bodyhash mismatch")
	}
	if h["Cosy-Bodylength"] != itoa(len(body)) {
		t.Errorf("Cosy-Bodylength = %s, want %d", h["Cosy-Bodylength"], len(body))
	}
	for _, k := range []string{"Cosy-User", "Cosy-Version", "Cosy-Machineid", "Cosy-Machinetoken",
		"Cosy-Machinetype", "Cosy-Machineos", "Cosy-Clienttype", "Cosy-Clientip", "Cosy-Sigpath",
		"Cosy-Data-Policy", "Login-Version", "X-Request-Id"} {
		if v, ok := h[k]; !ok || v == "" {
			t.Errorf("missing/empty header %s", k)
		}
	}
	if h["Cosy-Machineid"] != "m-1" {
		t.Errorf("MachineID not honored: %s", h["Cosy-Machineid"])
	}
}

func mustMD5(s string) []byte {
	sum := md5.Sum([]byte(s))
	return sum[:]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// TestUnwrapSSE_Envelope 信封解包：inner 直透、[DONE] 终止、终止帧后 keepalive 不转发。
func TestUnwrapSSE_Envelope(t *testing.T) {
	upstream := strings.Join([]string{
		"data: " + `{"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}"}`,
		"",
		"data: " + `{"statusCodeValue":200,"body":"[DONE]"}`,
		"",
		"data: " + `{"statusCodeValue":200,"body":"keepalive-after-done"}`,
		"",
	}, "\n")
	rc := unwrapSSE(io.NopCloser(strings.NewReader(upstream)), "qoder/auto")
	defer rc.Close()
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "content\":\"hi") {
		t.Errorf("inner chunk not forwarded: %q", s)
	}
	if strings.Count(s, "data: [DONE]") != 1 {
		t.Errorf("exactly one [DONE] expected, got: %q", s)
	}
	if strings.Contains(s, "keepalive-after-done") {
		t.Errorf("keepalive after [DONE] leaked: %q", s)
	}
}

// TestUnwrapSSE_ErrorFrame statusCodeValue != 200 → 合成 OpenAI 错误 chunk + [DONE]。
func TestUnwrapSSE_ErrorFrame(t *testing.T) {
	upstream := "data: " + `{"statusCodeValue":403,"body":"{\"code\":\"112\"}"}` + "\n\n"
	rc := unwrapSSE(io.NopCloser(strings.NewReader(upstream)), "qoder/auto")
	defer rc.Close()
	out, _ := io.ReadAll(rc)
	s := string(out)
	if !strings.Contains(s, "[qoder error 403:") || !strings.Contains(s, "data: [DONE]") {
		t.Errorf("error frame not synthesized: %q", s)
	}
}

// TestRealmOf / TestNewEndpoints 域解析与 CN fail-fast。
func TestRealmOf(t *testing.T) {
	if RealmOf("cn") != RealmCN || RealmOf("global") != RealmGlobal || RealmOf("") != RealmGlobal {
		t.Fatal("RealmOf semantics broken")
	}
}

func TestNewEndpoints(t *testing.T) {
	g := NewEndpoints(RealmGlobal)
	if g.ChatBase != "https://api3.qoder.sh" || g.ChatBaseAlt != "https://api2.qoder.sh" {
		t.Errorf("global endpoints wrong: %+v", g)
	}
	c := NewEndpoints(RealmCN)
	if c.ChatBase != "https://gateway.qoder.com.cn" || c.OpenAPIBase != "https://openapi.qoder.com.cn" {
		t.Errorf("CN chat base wrong: %s", c.ChatBase)
	}
	if u, err := ChatUrls(RealmCN, "dt-x"); err != nil || !strings.Contains(u, "gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation") {
		t.Errorf("CN chat URL wrong: %s err=%v", u, err)
	}
	if _, err := ChatUrls(RealmGlobal, "dt-x"); err != nil {
		t.Errorf("global chat URL should resolve: %v", err)
	}
	if _, err := ChatUrls(RealmGlobal, "jt-x"); err != nil {
		t.Errorf("jt token chat URL should resolve to alt: %v", err)
	}
}

// TestStableHash 稳定且 16 hex。
func TestStableHash(t *testing.T) {
	a := stableHash("p", "x", "y")
	b := stableHash("p", "x", "y")
	if a != b || len(a) != 16 {
		t.Fatalf("stableHash unstable or wrong length: %q vs %q", a, b)
	}
	if a == stableHash("p", "y", "x") {
		t.Fatal("order matters")
	}
}

// TestLimitedNumberManual 真实账号手动验证（默认跳过）：
//
//	QODER_TEST_TOKEN_GLOBAL=dt-… QODER_TEST_TOKEN_CN=dt-… go test ./internal/qoder/ -run TestLimitedNumberManual -v
func TestLimitedNumberManual(t *testing.T) {
	for _, tc := range []struct{ realm, token string }{
		{string(RealmGlobal), os.Getenv("QODER_TEST_TOKEN_GLOBAL")},
		{string(RealmCN), os.Getenv("QODER_TEST_TOKEN_CN")},
	} {
		if tc.token == "" {
			continue
		}
		res, err := NewClient().LimitedNumber(RealmOf(tc.realm), tc.token)
		if err != nil {
			t.Errorf("%s: %v", tc.realm, err)
			continue
		}
		t.Logf("%s: hasNumber=%v number=%d createdAt=%s", tc.realm, res.HasNumber, res.Number, res.CreatedAt)
	}
}

// TestParseExpiry 各口径。
func TestParseExpiry(t *testing.T) {
	now := time.Now()
	if got := parseExpiry([]byte("1781594470000"), nil); got.UnixMilli() != 1781594470000 {
		t.Errorf("numeric string: %v", got)
	}
	if got := parseExpiry(nil, []byte("3600")); now.Add(59 * time.Minute).After(got) {
		t.Errorf("expires_in seconds: %v", got)
	}
	if got := parseExpiry(nil, nil); got.Before(now.Add(29 * 24 * time.Hour)) {
		t.Errorf("fallback 30d: %v", got)
	}
}
