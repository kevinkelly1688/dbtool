// @author: kevinkelly
// @license: Apache Licence
// @file: navbadge.go
// @time: 2026/10/28

package server

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"dbtool/internal/audit"
)

// 侧栏角标：某些菜单条目（SQL 上线 / 权限管理）右侧显示“等我处理的数量”。
// 数据在服务端渲染页面时算好随 NavLink 下发；计算有短缓存与等待上限，任何一项失败 / 超时只是不显示（或沿用上一次的旧值），不会拖慢或拖垮页面。
// 性能工单的角标另由 web/static/js/perf/todo.js 异步补上（perf 包依赖 server，server 不能反向引用它）。

// NavBadgeFunc 计算某个菜单条目对用户 a 的角标：n 为数量（<=0 不显示），warn 为 true 时用橙色。
type NavBadgeFunc func(s *Server, a audit.Actor) (n int, warn bool, err error)

type navBadgeDef struct {
	title string // 鼠标悬停提示，%d 为数量
	fn    NavBadgeFunc
}

var navBadgeDefs = map[string]navBadgeDef{}

// AddNavBadge 给菜单条目（按 Key）登记角标计算函数；在 init() 里调用。
func AddNavBadge(key, title string, fn NavBadgeFunc) { navBadgeDefs[key] = navBadgeDef{title, fn} }

func init() {
	AddNavBadge("sql", "等待我审批的 SQL 工单 %d 张", func(s *Server, a audit.Actor) (int, bool, error) {
		if s.sql == nil {
			return 0, false, nil
		}
		n, err := s.sql.PendingReview(a, 0)
		return n, false, err
	})
	AddNavBadge("applies", "等待我审批的查询权限申请 %d 张", func(s *Server, a audit.Actor) (int, bool, error) {
		if s.apply == nil {
			return 0, false, nil
		}
		n, err := s.apply.PendingReview(a)
		return n, false, err
	})
}

const (
	badgeTTL       = 15 * time.Second // 角标缓存时间
	badgeWait      = 700 * time.Millisecond
	badgeStaleMax  = 10 * time.Minute // 超时 / 失败时最多沿用多久以前的旧值
	badgeMaxCached = 4000
	badgeCap       = 99 // 超过显示为 99+
)

type badgeKey struct {
	uid uint
	key string
}

type badgeEntry struct {
	n    int
	warn bool
	at   time.Time
}

// badgeCache 是进程内的角标缓存；零值可用。ttl / wait 为 0 时取默认值，ttl<0 表示不缓存（测试用）。
type badgeCache struct {
	mu   sync.Mutex
	m    map[badgeKey]badgeEntry
	ttl  time.Duration
	wait time.Duration
}

func (c *badgeCache) ttlV() time.Duration {
	if c.ttl == 0 {
		return badgeTTL
	}
	return c.ttl
}

func (c *badgeCache) waitV() time.Duration {
	if c.wait == 0 {
		return badgeWait
	}
	return c.wait
}

func (c *badgeCache) get(k badgeKey) (badgeEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	return e, ok
}

func (c *badgeCache) put(k badgeKey, e badgeEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= badgeMaxCached {
		c.m = map[badgeKey]badgeEntry{}
	}
	c.m[k] = e
}

// navBadges 并发计算 keys 对应的角标，返回有角标（n>0）的条目。
func (s *Server) navBadges(a audit.Actor, keys []string) map[string]badgeEntry {
	out := map[string]badgeEntry{}
	if len(keys) == 0 {
		return out
	}
	c := &s.nb
	type res struct {
		key string
		e   badgeEntry
		ok  bool
	}
	ch := make(chan res, len(keys))
	pending := 0
	settled := map[string]bool{} // 已有可信结果的条目（命中新鲜缓存或本次算成功，数量可以为 0）
	now := time.Now()
	for _, key := range keys {
		def, ok := navBadgeDefs[key]
		if !ok {
			continue
		}
		bk := badgeKey{a.ID, key}
		if e, ok := c.get(bk); ok && c.ttlV() > 0 && now.Sub(e.at) < c.ttlV() {
			settled[key] = true
			if e.n > 0 {
				out[key] = e
			}
			continue
		}
		pending++
		go func(key string, def navBadgeDef) {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("计算侧栏角标异常", "key", key, "panic", r)
					ch <- res{key: key}
				}
			}()
			n, warn, err := def.fn(s, a)
			if err != nil {
				slog.Warn("计算侧栏角标失败", "key", key, "err", err)
				ch <- res{key: key}
				return
			}
			e := badgeEntry{n: n, warn: warn, at: time.Now()}
			c.put(bk, e)
			ch <- res{key, e, true}
		}(key, def)
	}
	if pending == 0 {
		return out
	}
	timer := time.NewTimer(c.waitV())
	defer timer.Stop()
collect:
	for i := 0; i < pending; i++ {
		select {
		case r := <-ch:
			if r.ok {
				settled[r.key] = true
				if r.e.n > 0 {
					out[r.key] = r.e
				}
			}
		case <-timer.C:
			break collect
		}
	}
	// 失败 / 超时的条目：沿用不太旧的缓存值
	for _, key := range keys {
		if _, ok := navBadgeDefs[key]; !ok || settled[key] {
			continue
		}
		if e, ok := c.get(badgeKey{a.ID, key}); ok && e.n > 0 && time.Since(e.at) < badgeStaleMax {
			out[key] = e
		}
	}
	return out
}

func badgeText(n int) string {
	if n > badgeCap {
		return fmt.Sprintf("%d+", badgeCap)
	}
	return fmt.Sprintf("%d", n)
}
