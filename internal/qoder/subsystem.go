// subsystem.go Qoder 账号池：按 realm 分池轮换选号。与 internal/autoclaw.Subsystem
// 同构但更薄——Qoder 无 refresh（设备流 token 到期重登），池只负责选号与状态观测。
package qoder

import (
	"net/http"
	"sync"
	"time"
)

// Subsystem 持有双域账号池与共享 HTTP 客户端。
type Subsystem struct {
	mu            sync.RWMutex
	client        *Client
	rr            int
	pools         map[Realm][]*Credentials
	lastCampaigns []CampaignCheck
}

// SubsystemConfig 构造参数（Timeout 秒；0 取默认 15s）。
type SubsystemConfig struct {
	Timeout time.Duration
}

// NewSubsystem 从凭据列表建池。
func NewSubsystem(cfg SubsystemConfig, creds []*Credentials) *Subsystem {
	s := &Subsystem{
		client: NewClient(),
		pools:  map[Realm][]*Credentials{RealmGlobal: {}, RealmCN: {}},
	}
	if cfg.Timeout > 0 {
		s.client.HTTP.Timeout = cfg.Timeout
	}
	for _, c := range creds {
		s.pools[c.RealmOf()] = append(s.pools[c.RealmOf()], c)
	}
	return s
}

// Count 池内账号总数（两域合计）。
func (s *Subsystem) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, pool := range s.pools {
		n += len(pool)
	}
	return n
}

// CountByRealm 单域账号数。
func (s *Subsystem) CountByRealm(realm Realm) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.pools[realm])
}

// Pick 按 realm 轮换选号；池空返回 nil（调用方给 503）。
func (s *Subsystem) Pick(realm Realm) *Credentials {
	s.mu.Lock()
	defer s.mu.Unlock()
	pool := s.pools[realm]
	if len(pool) == 0 {
		return nil
	}
	c := pool[s.rr%len(pool)]
	s.rr++
	return c
}

// Client 暴露共享 HTTP 客户端（handler 层构造请求用）。
func (s *Subsystem) Client() *Client { return s.client }

// HTTP 透传给管理端点做登录流程。
func (s *Subsystem) HTTP() *http.Client { return s.client.HTTP }

// Status 观测快照（/status 面板用）。
func (s *Subsystem) Status() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []map[string]any
	for _, realm := range []Realm{RealmGlobal, RealmCN} {
		for _, c := range s.pools[realm] {
			out = append(out, map[string]any{
				"realm":      string(realm),
				"userId":     c.UserID,
				"name":       c.Name,
				"email":      c.Email,
				"authMethod": c.AuthMethod,
				"expired":    c.NeedsRefresh(0),
			})
		}
	}
	return out
}

// Add 热加载新账号（登录落盘后调用；同 uid+token 幂等）。
func (s *Subsystem) Add(c *Credentials) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	pool := s.pools[c.RealmOf()]
	for _, e := range pool {
		if e.UserID == c.UserID && e.AccessToken == c.AccessToken {
			return false
		}
	}
	s.pools[c.RealmOf()] = append(pool, c)
	return true
}

// RemoveByToken 按 token 摘除账号（管理端点删除用）。
func (s *Subsystem) RemoveByToken(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for realm, pool := range s.pools {
		for i, e := range pool {
			if e.AccessToken == token {
				s.pools[realm] = append(pool[:i], pool[i+1:]...)
				return true
			}
		}
	}
	return false
}

// FindByToken 按 access token 精确找账号（聊天路由鉴权用）。
func (s *Subsystem) FindByToken(token string) *Credentials {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, pool := range s.pools {
		for _, e := range pool {
			if e.AccessToken == token {
				return e
			}
		}
	}
	return nil
}

// Accounts 全量账号视图（管理端点列表用）。
func (s *Subsystem) Accounts() []*Credentials {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Credentials
	for _, realm := range []Realm{RealmGlobal, RealmCN} {
		out = append(out, s.pools[realm]...)
	}
	return out
}

// CampaignCheck 单账号活动观测结果。
type CampaignCheck struct {
	Realm        string
	UserID       string
	Name         string
	Claimable    bool
	ShowCampaign bool
	CampaignURL  string
	Count        int
	HasNumber    bool
	Number       int64
	NumberAt     string
	Err          string
}

// RunCampaignCheck 对全部账号做活动观测（scheduler 每日调用）。
// Qoder 无签到机制（CLI/双域 API 均无领取端点）；价值在于平台上线
// 可领取活动（claimable=true）时日志立刻暴露，提醒人工领取。
func (s *Subsystem) RunCampaignCheck() []CampaignCheck {
	var out []CampaignCheck
	for _, c := range s.Accounts() {
		r := CampaignCheck{Realm: string(c.RealmOf()), UserID: c.UserID, Name: c.Name}
		res, err := s.client.Campaigns(c.RealmOf(), c.AccessToken)
		if err != nil {
			r.Err = err.Error()
		} else {
			r.Claimable = res.Claimable
			r.ShowCampaign = res.ShowCampaign
			r.CampaignURL = res.CampaignURL
			r.Count = len(res.Campaigns)
		}
		if ln, err := s.client.LimitedNumber(c.RealmOf(), c.AccessToken); err != nil {
			if r.Err == "" {
				r.Err = "limited-number: " + err.Error()
			}
		} else {
			r.HasNumber = ln.HasNumber
			r.Number = ln.Number
			r.NumberAt = ln.CreatedAt
		}
		out = append(out, r)
	}
	s.mu.Lock()
	s.lastCampaigns = out
	s.mu.Unlock()
	return out
}

// LastCampaigns 最近一次活动观测（/status 用）。
func (s *Subsystem) LastCampaigns() []CampaignCheck {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]CampaignCheck(nil), s.lastCampaigns...)
}
