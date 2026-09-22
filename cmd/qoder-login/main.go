// qoder-login Qoder（CN/国际双域）账号登录 CLI，落盘 auths/qoder-<realm>-<uid>.json。
//
// 两个模式：
//
//	qoder-login device --realm=global          # 设备流：打印授权 URL，轮询直至完成
//	qoder-login pat --realm=global --token=pt-… # PAT 导入（自动换 job token）
//
// 设备流协议：本地生成 PKCE+nonce+machineId → 浏览器打开 qoder.com（或 qoder.cn）
// /device/selectAccounts → 每 2s 轮询 openapi 设备 token 端点（202/404=未完成）。
// token 约 30 天，上游 refresh 对设备流 403（9router 实测同口径），过期需重登。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/qoder"
)

var exitFunc = os.Exit

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "qoder-login: "+format+"\n", args...)
	exitFunc(1)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	authDir := fs.String("auth-dir", "./auths", "凭据目录")
	realm := fs.String("realm", "global", "账号域：cn 或 global")
	pat := fs.String("token", "", "[pat 模式] Personal Access Token（pt-…）")
	persist := fs.Bool("persist", true, "[pat 模式] 是否落盘（false 仅打印）")
	_ = fs.Parse(os.Args[2:])

	mode := os.Args[1]
	r := qoder.RealmOf(*realm)
	client := qoder.NewClient()

	switch mode {
	case "device":
		start, err := client.DeviceFlowStartURL(r)
		if err != nil {
			fatal("生成设备流失败: %v", err)
		}
		fmt.Printf("realm:   %s\n授权 URL（浏览器打开并登录 Qoder 账号）:\n%s\n轮询中（最长 5 分钟，每 2s）…\n",
			r, start.AuthURL)
		deadline := time.Now().Add(5 * time.Minute)
		for {
			res, err := client.DeviceTokenPoll(r, start.Nonce, start.Verifier)
			if err != nil {
				fatal("轮询失败: %v", err)
			}
			if res.Pending {
				if time.Now().After(deadline) {
					fatal("超时（5 分钟）——重新运行并尽快完成浏览器授权")
				}
				time.Sleep(2 * time.Second)
				continue
			}
			info := client.UserInfo(r, res.Token)
			path := save(*authDir, r, res.Token, res.UserID, info, start.MachineID, "device")
			fmt.Printf("登录成功: uid=%s name=%q email=%q\n账号文件: %s\n", res.UserID, info.Name, info.Email, path)
			return
		}
	case "pat":
		if !strings.HasPrefix(*pat, "pt-") {
			fatal("--token 需要 pt- 前缀的 PAT（Account → Integrations 创建）")
		}
		job, err := client.ExchangeJobToken(r, *pat)
		if err != nil {
			fatal("PAT 换 job token 失败: %v", err)
		}
		info := client.UserInfo(r, job.Token)
		if *persist {
			path := save(*authDir, r, job.Token, info.UserID, info, "", "pat")
			fmt.Printf("PAT 导入成功: uid=%s name=%q（job token 至 %s）\n账号文件: %s\n",
				info.UserID, info.Name, job.ExpiresAt.Format(time.RFC3339), path)
		} else {
			fmt.Printf("job token=%s…（至 %s）uid=%s\n", job.Token[:8], job.ExpiresAt.Format(time.RFC3339), info.UserID)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, "用法: qoder-login {device|pat} [--realm=cn|global] [--auth-dir=./auths] [pat: --token=pt-…] [--persist=false]\n")
	exitFunc(2)
}

// save 构造凭据原子落盘（qoder-<realm>-<uid>.json，0600）。
func save(dir string, realm qoder.Realm, token, uid string, info qoder.UserInfo, machineID, method string) string {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fatal("创建 %s 失败: %v", dir, err)
	}
	cred := &qoder.Credentials{
		AccessToken: token,
		Realm:       string(realm),
		UserID:      uid,
		Name:        info.Name,
		Email:       info.Email,
		OrgID:       info.OrganizationID,
		MachineID:   machineID,
		AuthMethod:  method,
	}
	cred.FilePath = qoder.NewPath(dir, realm, uid)
	if err := cred.SaveAtomic(); err != nil {
		fatal("落盘失败: %v", err)
	}
	return cred.FilePath
}
