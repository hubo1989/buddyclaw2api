// main.go 登录 CLI：支持手机验证码与海外网页 OAuth（Google / zai）两种模式。
//
// 手机验证码（两步）:
//
//	wb2api-autoclaw-login -phone 138xxxxxxxx
//	wb2api-autoclaw-login -phone 138xxxxxxxx -code 123456
//
// 海外 OAuth（浏览器授权）:
//
//	wb2api-autoclaw-login -oauth google          # 自动回调模式（默认端口 18432）
//	wb2api-autoclaw-login -oauth zai -manual     # 手动粘贴 code/state
//
// 注意：登录的账号必须是「服务独占」——不要在 AutoClaw 桌面端登录同一账号，
// 双方各自的 refresh token 轮换会互踢。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"workbuddy2api/internal/autoclaw"
)

var stdin = bufio.NewReader(os.Stdin)

func readLine() string {
	s, _ := stdin.ReadString('\n')
	return strings.TrimSpace(s)
}

// osExit 便于测试替换。
var osExit = os.Exit

func main() {
	phone := flag.String("phone", "", "[验证码模式] 手机号")
	code := flag.String("code", "", "[验证码模式] 6 位验证码（空=仅发送）")
	vendor := flag.String("oauth", "", "[OAuth 模式] google 或 zai（网页授权，无需手机短信）")
	manual := flag.Bool("manual", false, "[OAuth 模式] 手动粘贴 code/state（回调端口被占用/远程机时用）")
	port := flag.Int("port", 18432, "[OAuth 模式] 本地回调端口")
	authDir := flag.String("auth-dir", "./auths", "凭据目录")
	host := flag.String("host", autoclaw.DefaultHost, "API host")
	flag.Parse()

	c := autoclaw.NewClient(*host)

	switch {
	case *vendor != "":
		v := strings.ToLower(strings.TrimSpace(*vendor))
		if v != "google" && v != "zai" {
			fmt.Fprintln(os.Stderr, "-oauth 只支持 google 或 zai")
			osExit(2)
		}
		deviceID := findDeviceIDForVendor(*authDir, v)
		if deviceID == "" {
			deviceID = autoclaw.NewDeviceID()
		}
		oauthLoginFlow(c, *host, v, *authDir, *port, *manual, deviceID)

	case *phone != "":
		phoneCodeFlow(c, *phone, *code, *authDir, *host)

	default:
		fmt.Fprintln(os.Stderr, `用法:
  # 手机验证码（两步）
  wb2api-autoclaw-login -phone 138xxxxxxxx
  wb2api-autoclaw-login -phone 138xxxxxxxx -code 123456

  # 海外网页 OAuth（Google / zai 账号）
  wb2api-autoclaw-login -oauth google            # 起本地回调，自动接收
  wb2api-autoclaw-login -oauth zai -manual       # 手动粘贴 code/state`)
		osExit(2)
	}
}

// findDeviceIDForVendor 复用已有同 vendor OAuth 凭据的 device_id（保持设备指纹一致）。
func findDeviceIDForVendor(authDir, vendor string) string {
	creds, err := autoclaw.LoadDir(authDir)
	if err != nil {
		return ""
	}
	for _, cr := range creds {
		// OAuth 账号 phone 为空；用 nickname 前缀或 device 复用最后一个 OAuth 账号
		if cr.Phone == "" && cr.DeviceID != "" {
			return cr.DeviceID
		}
	}
	_ = vendor
	return ""
}
