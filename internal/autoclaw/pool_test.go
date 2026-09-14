// pool_test.go 单账号状态机与凭据落盘测试。
package autoclaw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCredentialsSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "autoclaw-u1.json")
	c := &Credentials{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		Phone:        "13800001234",
		UID:          "u1",
		FilePath:     fp,
	}
	if err := c.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 0600
	info, _ := os.Stat(fp)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", info.Mode().Perm())
	}
	loaded, err := LoadDir(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load: %v n=%d", err, len(loaded))
	}
	if loaded[0].RefreshToken != "rt" || loaded[0].UID != "u1" {
		t.Fatalf("loaded mismatch: %+v", loaded[0])
	}
	// 缺 refresh 的残档被跳过
	_ = os.WriteFile(filepath.Join(dir, "autoclaw-bad.json"), []byte(`{"accessToken":"x"}`), 0o600)
	loaded2, err := LoadDir(dir)
	if err != nil || len(loaded2) != 1 {
		t.Fatalf("bad file should be skipped: %v n=%d", err, len(loaded2))
	}
}

func TestPoolCooldownAndRelogin(t *testing.T) {
	cred := &Credentials{RefreshToken: "rt", Phone: "13800001234", UID: "u1", DeviceID: newDeviceID()}
	p := NewPool([]*Credentials{cred})
	a := p.Pick(nil)
	if a == nil {
		t.Fatal("expected usable account")
	}
	// 429 软冷却
	p.NoteError(a, ErrSoftRate, "rate", 3, time.Minute, 30*time.Minute)
	if s := a.snapshot(); !s.cooling || s.usable {
		t.Fatalf("expected cooling after 429: %+v", s)
	}
	// 401 → needs_relogin
	cred2 := &Credentials{RefreshToken: "rt", Phone: "13900001234", UID: "u2", DeviceID: newDeviceID()}
	p2 := NewPool([]*Credentials{cred2})
	a2 := p2.Pick(nil)
	p2.NoteError(a2, ErrAuth, "Invalid token", 3, time.Minute, 30*time.Minute)
	if s := a2.snapshot(); !s.relogin || s.usable {
		t.Fatalf("expected needs_relogin: %+v", s)
	}
	// 连续 3 次 5xx → 熔断
	cred3 := &Credentials{RefreshToken: "rt", Phone: "13700001234", UID: "u3", DeviceID: newDeviceID()}
	p3 := NewPool([]*Credentials{cred3})
	a3 := p3.Pick(nil)
	for i := 0; i < 3; i++ {
		p3.NoteError(a3, ErrServer, "boom", 3, time.Minute, 30*time.Minute)
	}
	if s := a3.snapshot(); !s.broken || s.usable {
		t.Fatalf("expected breaker open: %+v", s)
	}
	// 客户端错误不冷却
	cred4 := &Credentials{RefreshToken: "rt", Phone: "13600001234", UID: "u4", DeviceID: newDeviceID()}
	p4 := NewPool([]*Credentials{cred4})
	a4 := p4.Pick(nil)
	p4.NoteError(a4, ErrClient, "非法模型", 3, time.Minute, 30*time.Minute)
	if s := a4.snapshot(); !s.usable {
		t.Fatalf("client error should not cool: %+v", s)
	}
}

func TestPoolRoundRobin(t *testing.T) {
	var creds []*Credentials
	for i := 0; i < 3; i++ {
		creds = append(creds, &Credentials{
			RefreshToken: "rt", UID: string(rune('a'+i)) + "-uid",
			Phone: "1380000123" + string(rune('0'+i)), DeviceID: newDeviceID(),
		})
	}
	p := NewPool(creds)
	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		a := p.Pick(nil)
		if a == nil {
			t.Fatal("expected usable account")
		}
		seen[a.Cred.UID]++
	}
	for uid, n := range seen {
		if n != 3 {
			t.Fatalf("uid %s picked %d times, want 3 (round robin)", uid, n)
		}
	}
	// exclude 生效：排除两个后只剩一个
	ex := map[string]bool{"a-uid": true, "b-uid": true}
	for i := 0; i < 3; i++ {
		a := p.Pick(ex)
		if a == nil || a.Cred.UID != "c-uid" {
			t.Fatalf("expected c-uid only, got %+v", a)
		}
	}
	// 全排除 → nil
	if a := p.Pick(map[string]bool{"a-uid": true, "b-uid": true, "c-uid": true}); a != nil {
		t.Fatal("expected nil when all excluded")
	}
}

func TestHardCreditCooldownTo4AM(t *testing.T) {
	cred := &Credentials{RefreshToken: "rt", Phone: "13800001234", UID: "u1", DeviceID: newDeviceID()}
	p := NewPool([]*Credentials{cred})
	a := p.Pick(nil)
	p.NoteError(a, ErrHardCredit, "积分不足", 3, time.Minute, 30*time.Minute)
	s := a.snapshot()
	if !s.cooling {
		t.Fatal("expected hard cooling")
	}
	// 冷却截止应是未来的 04:00
	a.mu.Lock()
	until := a.until
	a.mu.Unlock()
	now := time.Now()
	target := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	if !until.Equal(target) {
		t.Fatalf("until = %v, want %v", until, target)
	}
}

func TestDeviceIDFormat(t *testing.T) {
	d := newDeviceID()
	if len(d) != 64 {
		t.Fatalf("device id len = %d, want 64", len(d))
	}
	for _, c := range d {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("non-hex char %q", c)
		}
	}
	if d == newDeviceID() {
		t.Fatal("device ids should be unique")
	}
}
