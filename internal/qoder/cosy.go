// cosy.go COSY 混合签名（RSA + AES + MD5）——Qoder 所有推理域请求的鉴权方式。
//
// 移植自 9router open-sse/shared/qoder/cosy.js（其注明源自 CLIProxyAPIPlus
// qoder-provider 分支并与 live qodercli 流量核对）：
//   - AES-128-CBC 加密 userinfo JSON（uid/security_oauth_token/name/aid/email），
//     key 取新 UUID v4 的前 16 字符（含连字符）；IV 复用 key 字节（上游约定）。
//   - AES key 用内置 RSA 公钥 PKCS1 加密 → Cosy-Key。
//   - payload = base64(JSON{version,requestId,info,cosyVersion,ideVersion})。
//   - sig = md5(payloadB64 LF cosyKey LF ts LF body LF sigPath)，均 latin1 字节口径。
//   - Authorization: Bearer COSY.<payloadB64>.<sig>，另附 17 个 Cosy-*/X-* 头。
//
// 请求头大小写与 qodercli 一致：Cosy-Machineid（非 MachineID）等。上游签名校验
// 会把这些值与签名时刻的输入比对，不可改动。
package qoder

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// cosyRSAPublicKey 提取自 Qoder IDE v0.9（上游公开常量，所有客户端共用）。
const cosyRSAPublicKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// COSY 客户端指纹常量（上游签名校验逐值比对，不可改）。
const (
	CosyIDEVersion  = "1.0.0"
	CosyClientType  = "5"
	CosyDataPolicy  = "disagree"
	CosyLoginVer    = "v2"
	CosyMachineOS   = "x86_64_windows"
	CosyMachineType = "5"
)

// CosyCredentials 参与签名的一组身份要素。
type CosyCredentials struct {
	UserID    string // Qoder 用户 id（userinfo / 设备流返回）
	AuthToken string // dt- 设备 token 或 jt- job token
	Name      string
	Email     string
	MachineID string // 持久化的机器 UUID（每账号固定）
}

var cosyRSAKey *rsa.PublicKey

func cosyPubKey() (*rsa.PublicKey, error) {
	if cosyRSAKey != nil {
		return cosyRSAKey, nil
	}
	block, _ := pem.Decode([]byte(cosyRSAPublicKey))
	if block == nil {
		return nil, errors.New("cosy: decode RSA public key failed")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cosy: parse RSA public key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("cosy: key is not RSA")
	}
	cosyRSAKey = rsaKey
	return rsaKey, nil
}

// newUUIDv4 RFC 4122 v4 UUID（无第三方依赖）。
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("qoder: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewMachineID 生成新的持久化机器 UUID。
func NewMachineID() string { return newUUIDv4() }

// aesEncryptCBCBase64 AES-128-CBC + PKCS7，IV=key 前 16 字节（上游约定）。
func aesEncryptCBCBase64(plaintext, keyStr string) (string, error) {
	key := []byte(keyStr)
	if len(key) != 16 {
		return "", fmt.Errorf("cosy: aes key must be 16 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad([]byte(plaintext), aes.BlockSize)
	enc := cipher.NewCBCEncrypter(block, key[:aes.BlockSize])
	out := make([]byte, len(padded))
	enc.CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

// rsaEncryptBase64 PKCS1 v1.5 公钥加密 → base64。
func rsaEncryptBase64(data string) (string, error) {
	key, err := cosyPubKey()
	if err != nil {
		return "", err
	}
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, key, []byte(data))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

type cosyUserInfo struct {
	UID                string `json:"uid"`
	SecurityOauthToken string `json:"security_oauth_token"`
	Name               string `json:"name"`
	Aid                string `json:"aid"`
	Email              string `json:"email"`
}

type cosyPayload struct {
	Version     string `json:"version"`
	RequestID   string `json:"requestId"`
	Info        string `json:"info"`
	CosyVersion string `json:"cosyVersion"`
	IdeVersion  string `json:"ideVersion"`
}

// ComputeSigPath 剥掉路径前缀 "/algo"（qodercli 约定：签名输入用剥前缀后的路径）。
func ComputeSigPath(requestURL string) string {
	u, err := url.Parse(requestURL)
	if err != nil {
		return ""
	}
	p := u.Path
	if len(p) >= 5 && p[:5] == "/algo" {
		return p[5:]
	}
	return p
}

// BuildCosyHeaders 对 body（签名前已 Encode 的最终字节）+ requestURL 生成全部请求头。
// GET 请求 body 传 nil/空。返回的 map 直接并入 http.Request.Header。
func BuildCosyHeaders(body []byte, requestURL string, creds CosyCredentials) (map[string]string, error) {
	if creds.UserID == "" {
		return nil, errors.New("cosy: user id is empty")
	}
	if creds.AuthToken == "" {
		return nil, errors.New("cosy: auth token is empty")
	}
	if creds.MachineID == "" {
		creds.MachineID = newUUIDv4()
	}

	aesKey := newUUIDv4()[:16]
	infoPlain, err := json.Marshal(cosyUserInfo{
		UID: creds.UserID, SecurityOauthToken: creds.AuthToken,
		Name: creds.Name, Aid: "", Email: creds.Email,
	})
	if err != nil {
		return nil, err
	}
	infoB64, err := aesEncryptCBCBase64(string(infoPlain), aesKey)
	if err != nil {
		return nil, err
	}
	cosyKey, err := rsaEncryptBase64(aesKey)
	if err != nil {
		return nil, err
	}

	payloadJSON, err := json.Marshal(cosyPayload{
		Version: "v1", RequestID: newUUIDv4(), Info: infoB64,
		CosyVersion: CosyIDEVersion, IdeVersion: "",
	})
	if err != nil {
		return nil, err
	}
	payloadB64 := base64.StdEncoding.EncodeToString(payloadJSON)

	sigPath := ComputeSigPath(requestURL)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sigInput := payloadB64 + "\n" + cosyKey + "\n" + ts + "\n" + string(body) + "\n" + sigPath
	sigSum := md5.Sum([]byte(sigInput))
	sig := hex.EncodeToString(sigSum[:])

	bodySum := md5.Sum(body)
	return map[string]string{
		"Authorization":          "Bearer COSY." + payloadB64 + "." + sig,
		"Cosy-Key":               cosyKey,
		"Cosy-User":              creds.UserID,
		"Cosy-Date":              ts,
		"Cosy-Version":           CosyIDEVersion,
		"Cosy-Machineid":         creds.MachineID,
		"Cosy-Machinetoken":      creds.MachineID,
		"Cosy-Machinetype":       CosyMachineType,
		"Cosy-Machineos":         CosyMachineOS,
		"Cosy-Clienttype":        CosyClientType,
		"Cosy-Clientip":          "127.0.0.1",
		"Cosy-Bodyhash":          hex.EncodeToString(bodySum[:]),
		"Cosy-Bodylength":        strconv.Itoa(len(body)),
		"Cosy-Sigpath":           sigPath,
		"Cosy-Data-Policy":       CosyDataPolicy,
		"Cosy-Organization-Id":   "",
		"Cosy-Organization-Tags": "",
		"Login-Version":          CosyLoginVer,
		"X-Request-Id":           newUUIDv4(),
	}, nil
}

// pkcePair S256 PKCE 对（32 随机字节，与 qodercli/Veria 一致）。
func pkcePair() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}
