// oauth.go 海外网页 OAuth 登录（Google / zai）。
//
// 两种模式：
//  1. 自动回调（默认）：-port 起本地 HTTP 服务，打印授权 URL 并尝试 open 浏览器；
//     授权完成后服务端回调 http://localhost:<port>/auth/callback-<vendor>?code=..&state=..，自动换 token。
//  2. 手动粘贴（--manual 或回调不可达时）：打印授权 URL（navigate_uri 指向占位地址），
//     用户把浏览器最终跳转地址（或其中的 code/state）粘贴回来。
package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"workbuddy2api/internal/autoclaw"
)

const callbackPathPrefix = "/auth/callback-"

// openBrowser 尽力打开默认浏览器（失败不影响手动模式）。
func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

// waitForCallback 起 http://localhost:<port>/auth/callback-<vendor> 接收回调，返回 code/state。
func waitForCallback(port int, vendor string, timeout time.Duration) (code, state string, err error) {
	path := callbackPathPrefix + vendor
	done := make(chan struct{})
	var gotCode, gotState string
	var srvErr error

	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotCode = q.Get("code")
		gotState = q.Get("state")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!DOCTYPE html><html><body style='font-family:sans-serif;padding:2em'>"+
			"<h2>AutoClaw 登录回调已收到</h2><p>code=%s</p><p>可以关闭此页面，回到终端查看结果。</p></body></html>",
			html.EscapeString(short(gotCode)))
		close(done)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, path+"?"+r.URL.RawQuery, http.StatusFound)
	})
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: mux}
	go func() {
		if e := srv.ListenAndServe(); e != nil && e != http.ErrServerClosed {
			srvErr = e
			close(done)
		}
	}()

	select {
	case <-done:
		_ = srv.Shutdown(context.Background())
		return gotCode, gotState, srvErr
	case <-time.After(timeout):
		_ = srv.Shutdown(context.Background())
		return "", "", fmt.Errorf("等待回调超时（%s）", timeout)
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "..."
	}
	return s
}

// oauthLoginFlow 执行海外 OAuth 登录。
func oauthLoginFlow(c *autoclaw.Client, host, vendor, authDir string, port int, manual bool, deviceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	redirectURI := fmt.Sprintf("http://localhost:%d%s%s", port, callbackPathPrefix, vendor)
	if manual {
		// 手动模式：回调地址只是占位（服务端可能校验与 authorize 一致，故仍带同一地址）
		redirectURI = fmt.Sprintf("http://localhost:%d%s%s", port, callbackPathPrefix, vendor)
	}

	authorizeURL, err := c.GetOAuthURL(ctx, vendor, deviceID, redirectURI, "")
	if err != nil {
		log.Fatalf("获取授权 URL 失败: %v\n（若返回 631002/631000 等业务错误，说明该 host 对当前环境未开放 oversea OAuth；"+
			"可尝试 --host 指定其他官方端点）", err)
	}
	fmt.Println("请在浏览器中打开以下链接完成授权：")
	fmt.Println()
	fmt.Println("  " + authorizeURL)
	fmt.Println()
	if !manual {
		openBrowser(authorizeURL)
		fmt.Printf("已尝试打开浏览器。等待回调 http://localhost:%d%s%s ...\n", port, callbackPathPrefix, vendor)
		code, state, err := waitForCallback(port, vendor, 10*time.Minute)
		if err != nil {
			fmt.Println("自动回调失败：", err)
			fmt.Println("可改用手动模式：--manual，然后从浏览器地址栏复制 code/state 粘贴。")
			osExit(1)
		}
		finishOAuth(c, host, vendor, authDir, deviceID, redirectURI, code, state)
		return
	}

	// 手动模式
	fmt.Println("授权完成后，浏览器会跳转到一个包含 code= 与 state= 的地址。")
	fmt.Printf("把完整跳转地址粘贴到这里（回车确认）:\n> ")
	var line string
	if _, err := fmt.Scanln(&line); err != nil {
		// 允许粘贴不含空格的 URL；Scanln 对超长行可能截断，改用 bufio 更稳
		line = readLine()
	}
	line = strings.TrimSpace(line)
	u, err := url.Parse(line)
	if err != nil {
		log.Fatalf("无法解析输入: %v", err)
	}
	q := u.Query()
	code, state := q.Get("code"), q.Get("state")
	if code == "" {
		// 也许用户只粘了 code 本身
		code = line
	}
	finishOAuth(c, host, vendor, authDir, deviceID, redirectURI, code, state)
}

func finishOAuth(c *autoclaw.Client, host, vendor, authDir, deviceID, redirectURI, code, state string) {
	if code == "" {
		log.Fatal("code 为空，登录中止")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lr, err := c.OAuthLogin(ctx, vendor, code, state, redirectURI, deviceID)
	if err != nil {
		log.Fatalf("OAuth 登录失败: %v", err)
	}
	fp, err := saveCredential(c, authDir, "", deviceID, c.Host, lr)
	if err != nil {
		log.Fatalf("保存凭据失败: %v", err)
	}
	fmt.Printf("登录成功（%s OAuth）: uid=%s nickname=%s\n凭据: %s\n", vendor, lr.UID, lr.Nickname, fp)
	fmt.Println("重启 wb2api 服务后生效（./service.sh restart）")
}
