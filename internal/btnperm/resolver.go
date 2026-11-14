// @author: kevinkelly
// @license: Apache Licence
// @file: resolver.go
// @time: 2026/10/28

package btnperm

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// 迁移状态（dbt_perm_setting 里 key=mode 的值）。运行时不再区分：权限一律只认按钮（权限组授予 + 单用户授权 / 禁用）。
// 取值只说明“原权限码有没有迁移成按钮授权”：compat = 还没迁移（没执行过 dbt-admin perm migrate），strict = 已迁移。
const (
	ModeCompat = "compat"
	ModeStrict = "strict"
)

// 无权限的按钮在页面上的呈现方式。
const (
	DenyHide    = "hide"
	DenyDisable = "disable"
)

// Setting 是全局设置（dbt_perm_setting）。Mode 只是迁移状态的标记（见上）。
type Setting struct {
	Mode        string `json:"mode"`
	DenyDisplay string `json:"deny_display"`
}

// DefaultSetting 是没有任何设置（或表还没创建）时的设置：未迁移、隐藏。
func DefaultSetting() Setting { return Setting{Mode: ModeCompat, DenyDisplay: DenyHide} }

// Normalize 把空值与非法值收敛成默认值。
func (s Setting) Normalize() Setting {
	if s.Mode != ModeStrict {
		s.Mode = ModeCompat
	}
	if s.DenyDisplay != DenyDisable {
		s.DenyDisplay = DenyHide
	}
	return s
}

// 单用户覆盖的效果。
const (
	EffectAllow = "allow"
	EffectDeny  = "deny"
)

// Override 是 dbt_user_perm 里的一条：单用户单独授权 / 禁用。
type Override struct {
	Code      string     `json:"code"`
	Effect    string     `json:"effect"`
	ExpireAt  *time.Time `json:"expire_at"`
	Reason    string     `json:"reason"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
}

// Expired 判断是否已到期。
func (o Override) Expired(now time.Time) bool { return o.ExpireAt != nil && !o.ExpireAt.After(now) }

// Profile 是一个用户与按钮权限相关的全部数据。
type Profile struct {
	Active      bool                // 用户存在且启用
	Super       bool                // 超级用户（仅供展示 / 诊断，调用方自己先行判断）
	GroupSource map[string][]string // 按钮码 → 授予它的权限组名
	Overrides   []Override
}

// Source 是解析器的数据来源：GORM 实现见 store.go，测试里用内存假实现。
type Source interface {
	Setting() (Setting, error)
	Profile(uid uint) (*Profile, error)
}

// Checker 是对外（server 等）的检查接口，*Resolver 实现它。
type Checker interface {
	// HasButton 判断用户是否拥有按钮（超级用户由调用方先行判断）。
	HasButton(uid uint, code string) (bool, error)
	// AllowedButtons 一次算出用户拥有的全部可授予按钮码。
	AllowedButtons(uid uint) (map[string]bool, error)
	Setting() (Setting, error)
}

// 缓存参数：数据库里的授权变更由 btnperm 自己的写操作立即失效；其他途径的变更（权限组成员、用户启停）最长 ttl 后生效。
const (
	DefaultTTL      = 15 * time.Second
	DefaultMaxUsers = 2000
)

type cachedProfile struct {
	p  *Profile
	at time.Time
}

// Resolver 把登记表和用户的按钮授权（权限组授予、单用户授权 / 禁用）合成最终判断，并带有上限与 TTL 的缓存。
type Resolver struct {
	src Source
	ttl time.Duration
	max int
	now func() time.Time

	mu         sync.Mutex
	gen        uint64 // 每次失效 +1：加载期间发生失效时，加载结果不入缓存
	profiles   map[uint]cachedProfile
	setting    Setting
	settingAt  time.Time
	settingSet bool
}

// New 创建解析器。
func New(src Source) *Resolver {
	return &Resolver{src: src, ttl: DefaultTTL, max: DefaultMaxUsers, now: time.Now, profiles: map[uint]cachedProfile{}}
}

// SetCache 调整缓存参数（测试用）；ttl<=0 表示不缓存。
func (r *Resolver) SetCache(ttl time.Duration, max int) {
	r.mu.Lock()
	r.ttl, r.max = ttl, max
	r.mu.Unlock()
	r.InvalidateAll()
}

// SetClock 替换时钟（测试用）。
func (r *Resolver) SetClock(f func() time.Time) { r.now = f }

// InvalidateUser 使某个用户的缓存失效。
func (r *Resolver) InvalidateUser(uid uint) {
	r.mu.Lock()
	delete(r.profiles, uid)
	r.gen++
	r.mu.Unlock()
}

// InvalidateAll 使全部缓存（含全局设置）失效。写操作之后调用。
func (r *Resolver) InvalidateAll() {
	r.mu.Lock()
	r.profiles = map[uint]cachedProfile{}
	r.settingSet = false
	r.gen++
	r.mu.Unlock()
}

// CacheSize 返回缓存的用户数（测试用）。
func (r *Resolver) CacheSize() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.profiles)
}

// Setting 返回全局设置（带缓存）。读取失败时返回默认设置与错误。
func (r *Resolver) Setting() (Setting, error) {
	r.mu.Lock()
	if r.settingSet && r.now().Sub(r.settingAt) < r.ttl {
		s := r.setting
		r.mu.Unlock()
		return s, nil
	}
	gen := r.gen
	r.mu.Unlock()
	s, err := r.src.Setting()
	if err != nil {
		return DefaultSetting(), err
	}
	s = s.Normalize()
	r.mu.Lock()
	if r.gen == gen && r.ttl > 0 {
		r.setting, r.settingAt, r.settingSet = s, r.now(), true
	}
	r.mu.Unlock()
	return s, nil
}

func (r *Resolver) profile(uid uint) (*Profile, error) {
	now := r.now()
	r.mu.Lock()
	if c, ok := r.profiles[uid]; ok && now.Sub(c.at) < r.ttl {
		r.mu.Unlock()
		return c.p, nil
	}
	gen := r.gen
	r.mu.Unlock()
	p, err := r.src.Profile(uid)
	if err != nil {
		return nil, err
	}
	if p == nil {
		p = &Profile{}
	}
	r.mu.Lock()
	if r.gen == gen && r.ttl > 0 {
		if len(r.profiles) >= r.max {
			for k, c := range r.profiles { // 先清过期的
				if now.Sub(c.at) >= r.ttl {
					delete(r.profiles, k)
				}
			}
			if len(r.profiles) >= r.max { // 仍然满：整体清空，宁可多查几次也不无限增长
				r.profiles = map[uint]cachedProfile{}
			}
		}
		r.profiles[uid] = cachedProfile{p: p, at: now}
	}
	r.mu.Unlock()
	return p, nil
}

// ---- 判定核心（纯函数，便于矩阵测试） ----

// Decision 是一个按钮对一个用户的判定结果与来源。
type Decision struct {
	Allowed bool     `json:"allowed"`
	Source  string   `json:"source"` // super / deny / allow / group / open / none / locked / inactive
	Groups  []string `json:"groups,omitempty"`
	Expire  *time.Time
}

// 判定来源。
const (
	SrcSuper    = "super"
	SrcDeny     = "deny"
	SrcAllow    = "allow"
	SrcGroup    = "group"
	SrcOpen     = "open"
	SrcNone     = "none"
	SrcLocked   = "locked"
	SrcInactive = "inactive"
)

// override 返回某按钮当前生效（未到期）的单用户覆盖。deny 优先于 allow。
func (p *Profile) override(code string, now time.Time) (Override, bool) {
	var allow *Override
	for i := range p.Overrides {
		o := p.Overrides[i]
		if o.Code != code || o.Expired(now) {
			continue
		}
		if o.Effect == EffectDeny {
			return o, true
		}
		allow = &p.Overrides[i]
	}
	if allow != nil {
		return *allow, true
	}
	return Override{}, false
}

func (p *Profile) denied(code string, now time.Time) bool {
	o, ok := p.override(code, now)
	return ok && o.Effect == EffectDeny
}

// granted 判断按钮是否被权限组或单独授权授予（不含禁用的判断）。
func (p *Profile) granted(code string, now time.Time) (Decision, bool) {
	if o, ok := p.override(code, now); ok && o.Effect == EffectAllow {
		return Decision{Allowed: true, Source: SrcAllow, Expire: o.ExpireAt}, true
	}
	if gs := p.GroupSource[code]; len(gs) > 0 {
		return Decision{Allowed: true, Source: SrcGroup, Groups: gs}, true
	}
	return Decision{}, false
}

// EvalButton 计算一个按钮对用户的判定：用户停用 → 无；单用户禁用 → 无（优先于授权）；仅超级用户的锁定按钮 → 无；
// 单用户授权 / 权限组授予 → 有；基础按钮（Open，登录即可）→ 有；其余 → 无。超级用户由调用方先行判断。
func EvalButton(b Button, p *Profile, now time.Time) Decision {
	if !p.Active {
		return Decision{Source: SrcInactive}
	}
	if p.denied(b.Code, now) {
		o, _ := p.override(b.Code, now)
		return Decision{Source: SrcDeny, Expire: o.ExpireAt}
	}
	if b.Locked {
		return Decision{Source: SrcLocked}
	}
	if d, ok := p.granted(b.Code, now); ok {
		return d
	}
	if b.Open {
		return Decision{Allowed: true, Source: SrcOpen}
	}
	return Decision{Source: SrcNone}
}

// ---- 对外方法 ----

// HasButton 判断用户是否拥有按钮；按钮码未登记返回 false。
func (r *Resolver) HasButton(uid uint, code string) (bool, error) {
	b, ok := idx.byCode[code]
	if !ok {
		return false, nil
	}
	p, err := r.profile(uid)
	if err != nil {
		return false, err
	}
	return EvalButton(*b, p, r.now()).Allowed, nil
}

// Explain 返回用户对每个按钮的判定与来源（权限组管理页“按用户查看有效权限”）。super 为 true 时全部放行。
func (r *Resolver) Explain(uid uint) (map[string]Decision, *Profile, error) {
	p, err := r.src.Profile(uid) // 诊断不走缓存
	if err != nil {
		return nil, nil, err
	}
	if p == nil {
		p = &Profile{}
	}
	now := r.now()
	out := make(map[string]Decision, len(idx.codes))
	for _, c := range idx.codes {
		if p.Super && p.Active {
			out[c] = Decision{Allowed: true, Source: SrcSuper}
			continue
		}
		out[c] = EvalButton(*idx.byCode[c], p, now)
	}
	return out, p, nil
}

// WhatIfGroup 假设权限组 groupName 的按钮授权换成 next，计算用户对按钮 code 的判定（不写库、不走缓存）。
// 权限组管理页保存前预检用（例如：这次保存会不会让某级审批流无人可审）。超级用户由调用方先行判断。
func (r *Resolver) WhatIfGroup(uid uint, groupName string, next map[string]bool, code string) (Decision, error) {
	b, ok := idx.byCode[code]
	if !ok {
		return Decision{}, nil
	}
	p, err := r.src.Profile(uid)
	if err != nil {
		return Decision{}, err
	}
	if p == nil {
		p = &Profile{}
	}
	cp := *p
	cp.GroupSource = make(map[string][]string, len(p.GroupSource))
	for k, v := range p.GroupSource {
		cp.GroupSource[k] = append([]string(nil), v...)
	}
	kept := cp.GroupSource[code][:0]
	for _, g := range cp.GroupSource[code] {
		if g != groupName {
			kept = append(kept, g)
		}
	}
	if next[code] {
		kept = append(kept, groupName)
	}
	cp.GroupSource[code] = kept
	return EvalButton(*b, &cp, r.now()), nil
}

// AllowedButtons 一次算出用户拥有的全部按钮码（含 Locked 以外的全部登记按钮）。
func (r *Resolver) AllowedButtons(uid uint) (map[string]bool, error) {
	p, err := r.profile(uid)
	if err != nil {
		return nil, err
	}
	now := r.now()
	out := map[string]bool{}
	for _, c := range idx.codes {
		if EvalButton(*idx.byCode[c], p, now).Allowed {
			out[c] = true
		}
	}
	return out, nil
}

// SortedKeys 返回 map 的键（排序）。
func SortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ---- 进程内默认解析器 ----

var def atomic.Pointer[Resolver]

// SetDefault 设置进程内默认解析器（store.Open 调用）。
func SetDefault(r *Resolver) { def.Store(r) }

// Default 返回默认解析器；没有设置（测试 / 演示）时为 nil，调用方改问仓库自己的 HasButton。
func Default() *Resolver { return def.Load() }
