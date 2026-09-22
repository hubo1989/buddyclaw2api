package autoclaw

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPendingDeviceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := LoadPendingDevice(dir, "13800000000"); got != "" {
		t.Fatalf("empty dir should return empty, got %q", got)
	}
	if err := SavePendingDevice(dir, "13800000000", "dev-a"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := SavePendingDevice(dir, "13900000000", "dev-b"); err != nil {
		t.Fatalf("save second: %v", err)
	}
	if got := LoadPendingDevice(dir, "13800000000"); got != "dev-a" {
		t.Fatalf("want dev-a, got %q", got)
	}
	if got := LoadPendingDevice(dir, "13900000000"); got != "dev-b" {
		t.Fatalf("want dev-b, got %q", got)
	}
	if got := LoadPendingDevice(dir, "13700000000"); got != "" {
		t.Fatalf("unknown phone should be empty, got %q", got)
	}
	// 文件不应被 LoadDir 当成凭据（命名不匹配 autoclaw-*.json）
	names, _ := filepath.Glob(filepath.Join(dir, "autoclaw-*.json"))
	if len(names) != 0 {
		t.Fatalf("pending file must not match autoclaw-*.json, found %v", names)
	}
	if err := ClearPendingDevice(dir, "13800000000"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := LoadPendingDevice(dir, "13800000000"); got != "" {
		t.Fatalf("after clear should be empty, got %q", got)
	}
}

func TestPendingDeviceExpiry(t *testing.T) {
	dir := t.TempDir()
	if err := SavePendingDevice(dir, "13800000000", "dev-a"); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 手动把时间戳推到过期之前
	raw, err := os.ReadFile(pendingPath(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	m := map[string]pendingEntry{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	e := m["13800000000"]
	e.At -= int64(pendingMaxAge.Seconds()) * 2
	m["13800000000"] = e
	pendingMu.Lock()
	_ = writePending(dir, m)
	pendingMu.Unlock()
	if got := LoadPendingDevice(dir, "13800000000"); got != "" {
		t.Fatalf("expired entry should be ignored, got %q", got)
	}
}
