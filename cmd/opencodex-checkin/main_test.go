package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDailyState(t *testing.T) {
	s := &runState{Accounts: map[string]string{}}
	if done(s, "autoclaw", "a", "2026-10-01") {
		t.Fatal("empty state must not be done")
	}
	mark(s, "autoclaw", "a", "2026-10-01")
	if !done(s, "autoclaw", "a", "2026-10-01") {
		t.Fatal("marked state must be done")
	}
	if done(s, "autoclaw", "a", "2026-10-02") {
		t.Fatal("next day must reset")
	}
}

func TestSaveAuthUpdatesUsesAccessCAS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	write := func(access string) {
		raw, _ := json.Marshal(authStore{"autoclaw": {Accounts: []accountRow{{
			ID: "a", Credential: credential{"access": access, "refresh": "old", "expires": float64(1), "email": "u"},
		}}}})
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("old")
	err := saveAuthUpdates(path, []tokenUpdate{{
		provider: "autoclaw", id: "a", oldAccess: "changed", access: "new", refresh: "new-refresh", expiresMs: 2,
	}})
	if err == nil {
		t.Fatal("stale CAS must return an error")
	}
	var s authStore
	if err := json.Unmarshal(read(t, path), &s); err != nil {
		t.Fatal(err)
	}
	if got := stringField(s["autoclaw"].Accounts[0].Credential, "access"); got != "old" {
		t.Fatalf("stale CAS must not overwrite, got %q", got)
	}

	err = saveAuthUpdates(path, []tokenUpdate{{
		provider: "autoclaw", id: "a", oldAccess: "old", access: "new", refresh: "new-refresh", expiresMs: 2,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(read(t, path), &s); err != nil {
		t.Fatal(err)
	}
	c := s["autoclaw"].Accounts[0].Credential
	if stringField(c, "access") != "new" || stringField(c, "refresh") != "new-refresh" || int64Field(c, "expires") != 2 {
		t.Fatalf("matching CAS did not update credential: %#v", c)
	}
	if stringField(c, "email") != "u" {
		t.Fatalf("unknown credential metadata must be preserved: %#v", c)
	}
}

func TestAuthStoreLockCooperativeHandoff(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireAuthStoreLock(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	second := make(chan *authStoreLock, 1)
	errCh := make(chan error, 1)
	go func() {
		lock, err := acquireAuthStoreLock(filepath.Join(dir, "auth.json"))
		if err != nil {
			errCh <- err
			return
		}
		second <- lock
	}()
	time.Sleep(20 * time.Millisecond)
	first.release()
	select {
	case lock := <-second:
		lock.release()
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("lock was not handed off after release")
	}
	if _, err := os.Stat(filepath.Join(dir, "auth.store.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released lock still exists: %v", err)
	}
}

func TestJWTStringClaim(t *testing.T) {
	// JWT signature isn't verified for local metadata extraction; only the payload matters here.
	token := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"device_id":"dev"}`)) + ".y"
	if got := jwtStringClaim(token, "device_id"); got != "dev" {
		t.Fatalf("device_id=%q want dev", got)
	}
	if got := jwtStringClaim("not-jwt", "device_id"); got != "" {
		t.Fatalf("invalid JWT must return empty, got %q", got)
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
