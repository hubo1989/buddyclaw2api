// pool.go autoclaw 单账号状态机（极简版）：健康 + 冷却 + 熔断 + 观测。
// 与 internal/pool 的区别：无加权轮换（单账号）、无在途上限（上游网关自身限流）、无 state 快照
// （凭据即状态，重启后从 auths/autoclaw-*.json 重建）。
package autoclaw

import (
	"log"
	"sync"
	"time"
)

// Account 池中的单账号。
type Account struct {
	Cred *Credentials

	mu           sync.Mutex
	healthy      bool
	until        time.Time // 冷却截止
	breaker      time.Time // 熔断截止
	fails        int       // 连续失败
	errTotal     int64     // 累计错误
	success      int64     // 累计成功
	lastErr      string    // 最近错误摘要
	needsRelogin bool      // refresh 也失败（refresh token 死亡），人工重登
	balance      int64     // 最近一次积分余额（观测）
	balanceAt    time.Time
}

// Pool autoclaw 账号集合。
type Pool struct {
	mu    sync.Mutex
	accts []*Account
	rr    int // 轮转指针（多账号均匀分摊）
}

// NewPool 从凭据构建池。
func NewPool(creds []*Credentials) *Pool {
	p := &Pool{}
	for _, c := range creds {
		p.accts = append(p.accts, &Account{Cred: c, healthy: true})
	}
	return p
}

// Count 账号数。
func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accts)
}

// Healthy 可服务账号数（健康且未熔断/冷却/禁登录）。
func (p *Pool) Healthy() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.accts {
		if a.snapshot().usable {
			n++
		}
	}
	return n
}

type acctSnap struct {
	uid      string
	phone    string
	usable   bool
	cooling  bool
	broken   bool
	relogin  bool
	fails    int
	success  int64
	errTotal int64
	lastErr  string
	balance  int64
}

func (a *Account) snapshot() acctSnap {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	return acctSnap{
		uid:      a.Cred.UID,
		phone:    maskPhone(a.Cred.Phone),
		usable:   a.healthy && !a.needsRelogin && now.After(a.until) && now.After(a.breaker),
		cooling:  now.Before(a.until),
		broken:   now.Before(a.breaker),
		relogin:  a.needsRelogin,
		fails:    a.fails,
		success:  a.success,
		errTotal: a.errTotal,
		lastErr:  a.lastErr,
		balance:  a.balance,
	}
}

// Pick 返回一个可用账号：从上次位置轮转（多账号均匀分摊），排除排除表。
// exclude 非空时跳过其中 uid（chat 轮换 failover 用）；可能返回 nil（全部不可用）。
func (p *Pool) Pick(exclude map[string]bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.accts)
	if n == 0 {
		return nil
	}
	for i := 0; i < n; i++ {
		a := p.accts[(p.rr+i)%n]
		if exclude != nil && exclude[a.Cred.UID] {
			continue
		}
		if a.snapshot().usable {
			p.rr = (p.rr + i + 1) % n // 下次从下一个开始
			return a
		}
	}
	return nil
}

// Count 可用账号数（不含 exclude）。
func (p *Pool) UsableCount(exclude map[string]bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.accts {
		if exclude != nil && exclude[a.Cred.UID] {
			continue
		}
		if a.snapshot().usable {
			n++
		}
	}
	return n
}

// NoteSuccess 记成功并清失败计数。
func (p *Pool) NoteSuccess(a *Account) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fails = 0
	a.success++
	a.healthy = true
}

// NoteError 记失败；连续 threshold 次触发熔断（指数退避 base→cap）。
func (p *Pool) NoteError(a *Account, kind ErrKind, msg string, threshold int, base, cap time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errTotal++
	a.fails++
	a.lastErr = kind.String() + ": " + truncateStr(msg, 120)
	switch kind {
	case ErrSoftRate:
		a.until = time.Now().Add(base)
		a.fails = 0 // 429 单独冷却，不进熔断计数
	case ErrHardCredit:
		a.until = cooldownUntilTomorrow4AM()
		a.fails = 0
	case ErrAuth:
		// token 刷新失败后的 401：标 needs_relogin，等待人工
		a.needsRelogin = true
	case ErrServer:
		if a.fails >= threshold {
			backoff := base << uint(min64(int64(a.fails-threshold), 6))
			if backoff > cap {
				backoff = cap
			}
			a.breaker = time.Now().Add(backoff)
			log.Printf("autoclaw account %s breaker open for %s (fails=%d)", maskPhone(a.Cred.Phone), backoff, a.fails)
		}
	default:
		// ErrClient/ErrNotFound：只记错误，不冷却（防雪崩）
	}
}

// NoteSuccessFull 成功后清除 relogin（refresh 恢复时调用）。
func (p *Pool) NoteSuccessFull(a *Account) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.needsRelogin = false
}

// SetBalance 更新积分观测。
func (p *Pool) SetBalance(a *Account, total int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.balance = total
	a.balanceAt = time.Now()
}

// Status 返回观测快照。
func (p *Pool) Status() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.accts))
	for _, a := range p.accts {
		s := a.snapshot()
		out = append(out, map[string]any{
			"uid":           s.uid,
			"phone":         s.phone,
			"usable":        s.usable,
			"cooling":       s.cooling,
			"breaker":       s.broken,
			"needs_relogin": s.relogin,
			"fails":         s.fails,
			"success":       s.success,
			"err_total":     s.errTotal,
			"last_error":    s.lastErr,
			"balance":       s.balance,
		})
	}
	return out
}

// cooldownUntilTomorrow4AM 与 workbuddy 上游同语义：冷却到次日 04:00（等签到恢复）。
func cooldownUntilTomorrow4AM() time.Time {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
