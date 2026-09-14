// phone.go 手机验证码登录流程。
package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"workbuddy2api/internal/autoclaw"
)

func phoneCodeFlow(c *autoclaw.Client, phone, code, authDir, host string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// device_id 必须与发码时一致（服务端把验证码与发码设备绑定，否则 agent-login 报 400001）。
	// CLI 是两次独立执行，所以发码后落盘、登录时读回。
	deviceID := autoclaw.NewDeviceID()
	if creds, err := autoclaw.LoadDir(authDir); err == nil {
		for _, cr := range creds {
			if cr.Phone == phone && cr.DeviceID != "" {
				deviceID = cr.DeviceID
			}
		}
	}
	if pending := autoclaw.LoadPendingDevice(authDir, phone); pending != "" {
		deviceID = pending
	}

	if code == "" {
		if err := c.SendCodePhone(ctx, phone, deviceID); err != nil {
			log.Fatalf("send-code: %v", err)
		}
		if err := autoclaw.SavePendingDevice(authDir, phone, deviceID); err != nil {
			log.Printf("WARN: 保存发码设备失败（可能导致下一步 400001）: %v", err)
		}
		fmt.Println("验证码已发送到手机短信，收到后执行:")
		fmt.Printf("  wb2api-autoclaw-login -phone %s -code <验证码>\n", phone)
		return
	}

	lr, usedHost, err := c.LoginPhone(ctx, phone, code, deviceID)
	if err != nil {
		if strings.Contains(err.Error(), "400001") {
			log.Printf("提示: 400001 常见原因 — 验证码过期/填错、手机号需为 11 位大陆号码（不带 +86）、或发码与登录设备不一致（重新执行发码步骤）")
		}
		log.Fatalf("login: %v", err)
	}
	fp, err := saveCredential(c, authDir, phone, deviceID, usedHost, lr)
	if err != nil {
		log.Fatalf("save: %v", err)
	}
	fmt.Printf("登录成功: uid=%s phone=%s\n凭据: %s\n", lr.UID, autoclaw.MaskPhoneExport(phone), fp)
	fmt.Println("重启 wb2api 服务后生效（./service.sh restart）")
}
