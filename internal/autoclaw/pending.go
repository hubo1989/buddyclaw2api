// pending.go 「已发码、待验证」阶段的 device_id 持久化。
//
// agent-send-code 与 agent-login 必须使用同一个 device_id：服务端把验证码与发码时的设备
// 绑定，两次调用若 device_id 不同，agent-login 直接返回 400001「请求数据有问题」。
// 而这两步天然跨进程/跨调用（CLI 两次执行、面板两次点击、服务可能重启），内存状态会丢，
// 因此落到 auth 目录下一个小文件里。文件名刻意不匹配 `autoclaw-*.json`，避免被 LoadDir
// 当成凭据加载。
package autoclaw

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// pendingFileName 发码态文件名。
const pendingFileName = "pending-device.json"

// pendingEntry 一条发码记录。
type pendingEntry struct {
	DeviceID string `json:"device_id"`
	At       int64  `json:"at"`
}

// pendingMaxAge 发码记录有效期（超过即视为过期，重新发码）。
const pendingMaxAge = 30 * time.Minute

var pendingMu sync.Mutex

func pendingPath(authDir string) string {
	if strings.TrimSpace(authDir) == "" {
		return ""
	}
	return filepath.Join(authDir, pendingFileName)
}

func loadPending(authDir string) map[string]pendingEntry {
	out := map[string]pendingEntry{}
	p := pendingPath(authDir)
	if p == "" {
		return out
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func writePending(authDir string, m map[string]pendingEntry) error {
	p := pendingPath(authDir)
	if p == "" {
		return nil
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// SavePendingDevice 记录某手机号发码时使用的 device_id。
func SavePendingDevice(authDir, phone, deviceID string) error {
	key := strings.TrimSpace(phone)
	if key == "" || strings.TrimSpace(deviceID) == "" {
		return nil
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	m := loadPending(authDir)
	// 顺带清理过期项，避免文件无限增长
	now := time.Now().Unix()
	for k, v := range m {
		if now-v.At > int64(pendingMaxAge.Seconds()) {
			delete(m, k)
		}
	}
	m[key] = pendingEntry{DeviceID: deviceID, At: now}
	return writePending(authDir, m)
}

// LoadPendingDevice 取回某手机号发码时的 device_id；无记录或已过期返回空串。
func LoadPendingDevice(authDir, phone string) string {
	key := strings.TrimSpace(phone)
	if key == "" {
		return ""
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	e, ok := loadPending(authDir)[key]
	if !ok {
		return ""
	}
	if time.Now().Unix()-e.At > int64(pendingMaxAge.Seconds()) {
		return ""
	}
	return e.DeviceID
}

// ClearPendingDevice 登录成功后清掉发码记录。
func ClearPendingDevice(authDir, phone string) error {
	key := strings.TrimSpace(phone)
	if key == "" {
		return nil
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	m := loadPending(authDir)
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	return writePending(authDir, m)
}
