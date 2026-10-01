// opencodex-checkin 直接读取 OpenCodeX OAuth 账号池执行每日任务：
// WorkBuddy CN 签到、AutoClaw 签到、Qoder 名额/活动观测。定时器只在本机
// OpenCodeX service 存活时运行；token 刷新会按账号写回 auth.json。
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/autoclaw"
	"workbuddy2api/internal/qoder"
	"workbuddy2api/internal/upstream"
)

type credential map[string]any

type accountRow struct {
	ID         string     `json:"id"`
	Credential credential `json:"credential"`
}

type providerSet struct {
	Accounts []accountRow `json:"accounts"`
}

type authStore map[string]providerSet

type runState struct {
	Accounts map[string]string `json:"accounts"`
}

type tokenUpdate struct {
	provider, id, oldAccess, access, refresh string
	expiresMs                                int64
}

func main() {
	log.SetFlags(log.LstdFlags)
	home, _ := os.UserHomeDir()
	authPath := flag.String("auth", filepath.Join(home, ".opencodex", "auth.json"), "OpenCodeX auth.json")
	statePath := flag.String("state", filepath.Join(home, "Library", "Application Support", "workbuddy2api", "opencodex-checkin-state.json"), "daily state")
	ocxHome := flag.String("opencodex-home", filepath.Join(home, ".opencodex"), "OpenCodeX home")
	force := flag.Bool("force", false, "ignore OpenCodeX liveness and daily state")
	flag.Parse()

	if !*force {
		if !opencodexAlive(*ocxHome) {
			log.Printf("skip: OpenCodeX service is not running")
			return
		}
	}
	raw, err := os.ReadFile(*authPath)
	if err != nil {
		log.Fatalf("read auth: %v", err)
	}
	var store authStore
	if err := json.Unmarshal(raw, &store); err != nil {
		log.Fatalf("parse auth: %v", err)
	}
	state, err := loadState(*statePath)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}
	if *force {
		state.Accounts = map[string]string{}
	}
	day := time.Now().In(time.Local).Format("2006-01-02")
	fail := 0
	fail += runWorkbuddy(store, state, day, *authPath)
	fail += runAutoclaw(store, state, day, *authPath)
	fail += runQoder(store, state, day)
	if err := saveState(*statePath, state); err != nil {
		log.Printf("WARN: save state: %v", err)
		fail++
	}
	if fail != 0 {
		os.Exit(1)
	}
}

func opencodexAlive(home string) bool {
	raw, err := os.ReadFile(filepath.Join(home, "runtime-port.json"))
	if err != nil {
		return false
	}
	var rt struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(raw, &rt) != nil || rt.PID <= 0 {
		return false
	}
	return syscall.Kill(rt.PID, 0) == nil
}

func loadState(path string) (*runState, error) {
	s := &runState{Accounts: map[string]string{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, err
	}
	if s.Accounts == nil {
		s.Accounts = map[string]string{}
	}
	return s, nil
}

func saveState(path string, state *runState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicWrite(path, state)
}

func done(state *runState, provider, id, day string) bool {
	return state.Accounts[provider+"/"+id] == day
}

func mark(state *runState, provider, id, day string) {
	if state.Accounts == nil {
		state.Accounts = map[string]string{}
	}
	state.Accounts[provider+"/"+id] = day
}

func runWorkbuddy(store authStore, state *runState, day, authPath string) int {
	fail := 0
	for _, provider := range []string{"workbuddy", "workbuddy-global"} {
		for _, row := range store[provider].Accounts {
			if done(state, provider, row.ID, day) {
				continue
			}
			if provider == "workbuddy-global" {
				log.Printf("workbuddy-global/%s: skipped (global check-in endpoint unverified)", row.ID)
				mark(state, provider, row.ID, day)
				continue
			}
			if err := workbuddyCheckin(provider, row, authPath); err != nil {
				log.Printf("workbuddy/%s: %v", row.ID, err)
				fail++
				continue
			}
			mark(state, provider, row.ID, day)
		}
	}
	return fail
}

func workbuddyCheckin(provider string, row accountRow, authPath string) error {
	access := stringField(row.Credential, "access")
	refresh := stringField(row.Credential, "refresh")
	uid := stringField(row.Credential, "accountId")
	if access == "" || refresh == "" || uid == "" {
		return fmt.Errorf("incomplete credential")
	}
	nested, _ := json.Marshal(map[string]any{
		"auth": map[string]any{
			"accessToken": access, "refreshToken": refresh,
			"expiresAt": int64Field(row.Credential, "expires") / 1000,
			"domain":    "copilot.tencent.com", "realm": "cn",
		},
		"account": map[string]any{"uid": uid, "nickname": stringField(row.Credential, "email")},
	})
	a, err := auth.Parse(nested)
	if err != nil {
		return err
	}
	client := upstream.New()
	if a.NeedsRefresh(2 * time.Minute) {
		if err := client.RefreshToken(a); err != nil {
			return fmt.Errorf("refresh: %w", err)
		}
		if err := saveAuthUpdates(authPath, []tokenUpdate{{
			provider: provider, id: row.ID, oldAccess: access,
			access: a.AccessToken, refresh: a.RefreshToken,
			expiresMs: a.ExpiresAt * 1000,
		}}); err != nil {
			return fmt.Errorf("save refreshed credential: %w", err)
		}
	}
	if err := client.DailyCheckin(a); err != nil {
		if upstream.IsAlreadyCheckin(err) {
			log.Printf("workbuddy/%s: already signed", row.ID)
			return nil
		}
		return fmt.Errorf("checkin: %w", err)
	}
	log.Printf("workbuddy/%s: signed", row.ID)
	return nil
}

func runAutoclaw(store authStore, state *runState, day, authPath string) int {
	fail := 0
	ctx := context.Background()
	for _, row := range store["autoclaw"].Accounts {
		if done(state, "autoclaw", row.ID, day) {
			continue
		}
		refresh := stringField(row.Credential, "refresh")
		// 任务/钱包接口只接受 agent*_token（手机验证码登录）。OpenCodeX 网页 OAuth
		// token 是 autoclaw*_token，聊天可用但 task-complete 会 401；标记当日已处理，
		// 避免 5 分钟调度反复重放必然失败的写接口。
		if source := jwtStringClaim(refresh, "source_id"); !strings.HasPrefix(source, "agent") {
			log.Printf("autoclaw/%s: skipped (web OAuth token cannot call daily task API; use gateway phone account)", row.ID)
			mark(state, "autoclaw", row.ID, day)
			continue
		}
		cred := &autoclaw.Credentials{
			AccessToken:  stringField(row.Credential, "access"),
			RefreshToken: refresh,
			ExpiresAt:    int64Field(row.Credential, "expires") / 1000,
			DeviceID:     jwtStringClaim(refresh, "device_id"),
			UID:          stringField(row.Credential, "accountId"),
			Nickname:     stringField(row.Credential, "email"),
		}
		if cred.AccessToken == "" || cred.RefreshToken == "" || cred.UID == "" {
			log.Printf("autoclaw/%s: incomplete credential", row.ID)
			fail++
			continue
		}
		client := autoclaw.NewClient("")
		if cred.NeedsRefresh(time.Minute) {
			if err := client.ForCred(cred).Refresh(ctx, cred); err != nil {
				log.Printf("autoclaw/%s: refresh: %v", row.ID, err)
				fail++
				continue
			}
			if nextDevice := jwtStringClaim(cred.RefreshToken, "device_id"); nextDevice != "" {
				cred.DeviceID = nextDevice
			}
			if err := saveAuthUpdates(authPath, []tokenUpdate{{
				provider: "autoclaw", id: row.ID, oldAccess: stringField(row.Credential, "access"),
				access: cred.AccessToken, refresh: cred.RefreshToken, expiresMs: cred.ExpiresAt * 1000,
			}}); err != nil {
				log.Printf("autoclaw/%s: save refreshed credential: %v", row.ID, err)
				fail++
				continue
			}
		}
		res, err := client.ForCred(cred).TaskComplete(ctx, cred.AccessToken, cred.DeviceID)
		if err != nil {
			log.Printf("autoclaw/%s: checkin: %v", row.ID, err)
			fail++
			continue
		}
		if res.AlreadyDone {
			log.Printf("autoclaw/%s: already signed", row.ID)
		} else {
			log.Printf("autoclaw/%s: +%d points", row.ID, res.RewardPoints)
		}
		mark(state, "autoclaw", row.ID, day)
	}
	return fail
}

func runQoder(store authStore, state *runState, day string) int {
	fail := 0
	for provider, realm := range map[string]qoder.Realm{"qoder-global": qoder.RealmGlobal, "qoder-cn-oauth": qoder.RealmCN} {
		for _, row := range store[provider].Accounts {
			if done(state, provider, row.ID, day) {
				continue
			}
			tok := stringField(row.Credential, "access")
			if tok == "" {
				log.Printf("%s/%s: incomplete credential", provider, row.ID)
				fail++
				continue
			}
			client := qoder.NewClient()
			camp, err := client.Campaigns(realm, tok)
			var limited *qoder.LimitedNumberResult
			if err == nil {
				limited, err = client.LimitedNumber(realm, tok)
			}
			if err != nil {
				log.Printf("%s/%s: %v", provider, row.ID, err)
				fail++
				continue
			}
			log.Printf("%s/%s: campaigns=%d claimable=%v limited=%v number=%d createdAt=%s", provider, row.ID, len(camp.Campaigns), camp.Claimable, limited.HasNumber, limited.Number, limited.CreatedAt)
			mark(state, provider, row.ID, day)
		}
	}
	return fail
}

func saveAuthUpdates(path string, updates []tokenUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := acquireAuthStoreLock(path)
	if err != nil {
		return err
	}
	defer lock.release()

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var current authStore
	if err := json.Unmarshal(raw, &current); err != nil {
		return err
	}
	var conflict error
	for _, u := range updates {
		set := current[u.provider]
		for i := range set.Accounts {
			row := &set.Accounts[i]
			if row.ID != u.id {
				continue
			}
			if stringField(row.Credential, "access") != u.oldAccess {
				conflict = fmt.Errorf("credential changed concurrently: %s/%s", u.provider, u.id)
				continue
			}
			row.Credential["access"] = u.access
			row.Credential["refresh"] = u.refresh
			row.Credential["expires"] = u.expiresMs
		}
		current[u.provider] = set
	}
	if err := atomicWrite(path, current); err != nil {
		return err
	}
	return conflict
}

// acquireAuthStoreLock implements the same cooperative auth.store.lock protocol as
// OpenCodeX. OpenCodeX mutations hold this lock while loading, changing, and renaming
// auth.json, so refreshing under it prevents a stale whole-store replacement.
func acquireAuthStoreLock(authPath string) (*authStoreLock, error) {
	lockPath := filepath.Join(filepath.Dir(authPath), "auth.store.lock")
	deadline := time.Now().Add(5 * time.Second)
	owner := randomOwner()
	for {
		fd, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			meta := map[string]any{"version": 1, "ownerId": owner, "pid": os.Getpid(), "createdAt": time.Now().UnixMilli()}
			raw, _ := json.Marshal(meta)
			raw = append(raw, '\n')
			if _, err := fd.Write(raw); err != nil {
				_ = fd.Close()
				return nil, err
			}
			if err := fd.Close(); err != nil {
				return nil, err
			}
			return &authStoreLock{path: lockPath, owner: owner}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		raw, statErr := os.ReadFile(lockPath)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return nil, statErr
		}
		var meta struct {
			OwnerID   string `json:"ownerId"`
			CreatedAt int64  `json:"createdAt"`
		}
		if json.Unmarshal(raw, &meta) != nil || meta.CreatedAt <= 0 {
			meta.CreatedAt = time.Now().Add(-time.Hour).UnixMilli()
		}
		if time.Since(time.UnixMilli(meta.CreatedAt)) > 30*time.Second && removeUnchangedLock(lockPath, raw) {
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for %s", lockPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type authStoreLock struct {
	path, owner string
}

func (l *authStoreLock) release() {
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	var meta struct {
		OwnerID string `json:"ownerId"`
	}
	if json.Unmarshal(raw, &meta) != nil || meta.OwnerID != l.owner {
		return
	}
	_ = os.Remove(l.path)
}

func removeUnchangedLock(path string, expected []byte) bool {
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != string(expected) {
		return false
	}
	return os.Remove(path) == nil
}

func randomOwner() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("fallback-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(raw)
}

func atomicWrite(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func int64Field(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func jwtStringClaim(token, key string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	s, _ := claims[key].(string)
	return s
}

func stableDeviceID(id string) string {
	sum := sha256.Sum256([]byte("opencodex-workbuddy2api:" + id))
	return hex.EncodeToString(sum[:])
}
