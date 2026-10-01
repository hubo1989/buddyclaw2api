package scheduler

import (
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/autoclaw"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/qoder"
	"workbuddy2api/internal/upstream"
)

type fakeAutoclawCheckin struct{ calls atomic.Int32 }

func (f *fakeAutoclawCheckin) RunSignin() []autoclaw.SigninResult {
	f.calls.Add(1)
	return []autoclaw.SigninResult{{Phone: "138****0000", OK: true, Already: true, Balance: 100}}
}

type fakeQoderCampaign struct{ calls atomic.Int32 }

func (f *fakeQoderCampaign) RunCampaignCheck() []qoder.CampaignCheck {
	f.calls.Add(1)
	return []qoder.CampaignCheck{{Realm: "global", UserID: "u1", HasNumber: true, Number: 1}}
}

// TestRunCheckinNowIncludesProviders 锁定 AutoClaw/Qoder 归属签到任务：
// 空 WorkBuddy 池时仍执行 provider 每日动作，且不再依赖 activity 任务。
func TestRunCheckinNowIncludesProviders(t *testing.T) {
	ac := &fakeAutoclawCheckin{}
	qd := &fakeQoderCampaign{}
	s := New(Config{
		Pool:     pool.New(""),
		Upstream: &upstream.Client{},
		Autoclaw: ac,
		Qoder:    qd,
	})
	s.RunCheckinNow()
	if ac.calls.Load() != 1 || qd.calls.Load() != 1 {
		t.Fatalf("provider calls autoclaw=%d qoder=%d, want 1/1", ac.calls.Load(), qd.calls.Load())
	}
}

// TestStartupCheckinGate 启动补签受 checkin_on_start 与总签到开关双重控制。
func TestStartupCheckinGate(t *testing.T) {
	var calls atomic.Int32
	newScheduler := func(cfg Config) *Scheduler {
		s := &Scheduler{cfg: cfg}
		s.startupCheckin = func() { calls.Add(1) }
		return s
	}

	newScheduler(Config{CheckinOnStart: true}).runStartupCheckin()
	if calls.Load() != 1 {
		t.Fatalf("enabled startup calls=%d, want 1", calls.Load())
	}

	newScheduler(Config{CheckinOnStart: false}).runStartupCheckin()
	if calls.Load() != 1 {
		t.Fatalf("checkin_on_start=false must not run, calls=%d", calls.Load())
	}

	newScheduler(Config{CheckinOnStart: true, CheckinDisabled: true}).runStartupCheckin()
	if calls.Load() != 1 {
		t.Fatalf("checkin disabled must not run startup checkin, calls=%d", calls.Load())
	}
}

type nilAutoclawCheckin struct{}

func (*nilAutoclawCheckin) RunSignin() []autoclaw.SigninResult {
	panic("typed-nil provider must not be dispatched")
}

// TestRunCheckinNowSkipsTypedNilProviders 回归：接口内类型化 nil 指针本身非 nil，
// disabled provider 不能触发其方法。
func TestRunCheckinNowSkipsTypedNilProviders(t *testing.T) {
	s := New(Config{
		Pool:     pool.New(""),
		Upstream: &upstream.Client{},
		Autoclaw: (*nilAutoclawCheckin)(nil),
	})
	s.RunCheckinNow() // 未 panic 即通过。
}
