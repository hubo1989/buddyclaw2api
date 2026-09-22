// login_cli.go 登录模式公用逻辑：构造凭据并落盘。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"workbuddy2api/internal/autoclaw"
)

// saveCredential 构造凭据、补全 uid、原子落盘，返回文件路径。
func saveCredential(c *autoclaw.Client, authDir, phone, deviceID, host string, lr *autoclaw.LoginResult) (string, error) {
	cred := &autoclaw.Credentials{
		AccessToken:  lr.AccessToken,
		RefreshToken: lr.RefreshToken,
		ExpiresAt:    time.Now().Add(5 * time.Hour).Unix(),
		DeviceID:     deviceID,
		Phone:        phone, // OAuth 模式时为空（可留空）
		UID:          lr.UID,
		Nickname:     lr.Nickname,
		Host:         host,
	}
	if cred.UID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if uid, nick, err := c.UserInfo(ctx, cred.AccessToken, cred.DeviceID); err == nil && uid != "" {
			cred.UID = uid
			cred.Nickname = nick
		}
	}
	if cred.UID == "" {
		cred.UID = fmt.Sprintf("oa%d", time.Now().Unix())
		log.Printf("warn: user_id 未取到，用占位 %s", cred.UID)
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
