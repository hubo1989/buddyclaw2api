// credentials.go Qoder 账号凭据：auths/qoder-<realm>-<uid>.json（0600），原子写回。
// 与 internal/autoclaw 的 Credentials 同构；多账号按 realm 分池。
package qoder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Credentials 单账号凭据。
// 设备流 token（dt-）约 30 天有效且上游 refresh 端点对设备流返回 403（实测，
// 9router 同口径）——过期即需重登。PAT 登录（pt-）换出的 job token（jt-）24h，
// 由 Client 按需重换，不落盘。
type Credentials struct {
	mu sync.Mutex

	AccessToken string `json:"accessToken"`
	Realm       string `json:"realm"`     // cn | global
	UserID      string `json:"userId"`    // COSY 签名必需
	MachineID   string `json:"machineId"` // 每账号固定机器 UUID
	Name        string `json:"name,omitempty"`
	Email       string `json:"email,omitempty"`
	OrgID       string `json:"organizationId,omitempty"`
	AuthMethod  string `json:"authMethod"` // device | pat
	ExpiresAt   int64  `json:"expiresAt"`  // Unix 秒
	FilePath    string `json:"-"`
}

// Lock/Unlock 供子系统刷新期间持锁。
func (c *Credentials) Lock()   { c.mu.Lock() }
func (c *Credentials) Unlock() { c.mu.Unlock() }

// RealmOf 该账号的域。
func (c *Credentials) RealmOf() Realm { return RealmOf(c.Realm) }

// NeedsRefresh 设备流 token 不可 refresh；这里只报告过期（供面板/状态展示）。
func (c *Credentials) NeedsRefresh(within time.Duration) bool {
	if c.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= c.ExpiresAt
}

// CosyCreds COSY 签名要素视图。
func (c *Credentials) CosyCreds() CosyCredentials {
	return CosyCredentials{
		UserID:    c.UserID,
		AuthToken: c.AccessToken,
		Name:      c.Name,
		Email:     c.Email,
		MachineID: c.MachineID,
	}
}

// SaveAtomic 原子写回（tmp+rename，0600）。拒绝空 token 落盘。
func (c *Credentials) SaveAtomic() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(c.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s realm=%s)", c.UserID, c.Realm)
	}
	if c.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	if c.MachineID == "" {
		c.MachineID = NewMachineID()
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.FilePath)
}

// LoadDir 从目录加载全部 qoder-*.json（文件名前缀区分，防混入其他上游账号）。
func LoadDir(dir string) ([]*Credentials, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Credentials
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "qoder-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		var cred Credentials
		if err := json.Unmarshal(raw, &cred); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		cred.FilePath = path
		out = append(out, &cred)
	}
	return out, nil
}

// NewPath 账号文件路径：qoder-<realm>-<uid>.json；uid 为空时用机器 id 兜底。
func NewPath(dir string, realm Realm, uid string) string {
	if uid == "" {
		uid = strings.ReplaceAll(NewMachineID(), "-", "")[:12]
	}
	return filepath.Join(dir, fmt.Sprintf("qoder-%s-%s.json", realm, uid))
}
