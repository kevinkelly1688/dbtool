// @author: kevinkelly
// @license: Apache Licence
// @file: userstatus.go
// @time: 2026/10/28

package server

import (
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/model"
)

// 会话 / JWT 撤销。
//
// 会话 Cookie 与 JWT 都是无状态签名，签发后在有效期内不会因为账号被停用 / 取消超级用户而失效。
// 认证中间件（requirePage / requireAPI）因此在每次请求里按用户 ID 回库确认 is_active / is_superuser，
// 结果带 userStatusTTL 的短缓存（按用户 ID，并发安全，同一用户并发未命中只查一次库）：
//   - 用户被停用或已不存在 → 401（页面跳登录页），并清掉会话 Cookie；
//   - 超级用户标记以库里最新值为准（覆盖会话 / JWT 里的旧值）；
//   - 库短暂不可用时沿用缓存（过期也用）或会话 / JWT 里的值，不因库抖动把所有人踢出。
//
// 缓存最长让撤销延迟 userStatusTTL 才生效；同进程内的管理操作可调用 Server.InvalidateUserStatus 立即生效。

// UserStatus 是认证中间件需要的用户当前状态。
type UserStatus struct {
	Exists    bool
	Active    bool
	Superuser bool
}

// UserStatusSource 由仓库实现（可选）：按用户 ID 读取当前状态，用户不存在时 Exists=false 且不报错。
// 仓库没有实现它但提供了元数据库句柄（DBProvider，即 store.Store）时，改为直接读 dbt_sql_users；两者都没有（测试 / 演示）则不做校验。
type UserStatusSource interface {
	UserStatus(id uint) (UserStatus, error)
}

var (
	userStatusTTL     = 10 * time.Second // 缓存有效期，必须 ≤ 15 秒
	userStatusBackoff = 3 * time.Second  // 库出错后，同一用户多久内不再重试（期间沿用缓存 / 会话值）
	statusClock       = time.Now
)

const userStatusMaxEntries = 20000

type statusEntry struct {
	st      UserStatus
	loaded  time.Time     // 最近一次成功读库的时间；零值表示还没有成功读过
	failAt  time.Time     // 最近一次读库失败的时间
	loading chan struct{} // 非 nil 表示有 goroutine 正在读库
}

// statusCache 的零值可用。
type statusCache struct {
	mu sync.Mutex
	m  map[uint]*statusEntry
}

// get 返回用户状态；known=false 表示没有可用的库里状态（库不可用且从未成功读过），调用方应沿用会话 / JWT 里的值。
func (c *statusCache) get(id uint, load func(uint) (UserStatus, error)) (st UserStatus, known bool) {
	for {
		now := statusClock()
		c.mu.Lock()
		if c.m == nil {
			c.m = map[uint]*statusEntry{}
		}
		e := c.m[id]
		if e != nil && !e.loaded.IsZero() && now.Sub(e.loaded) < userStatusTTL && !now.Before(e.loaded) {
			st = e.st
			c.mu.Unlock()
			return st, true
		}
		if e != nil && e.loading != nil { // 别的请求正在读：等它的结果
			ch := e.loading
			c.mu.Unlock()
			<-ch
			continue
		}
		if e != nil && !e.failAt.IsZero() && now.Sub(e.failAt) < userStatusBackoff && !now.Before(e.failAt) {
			st, known = e.st, !e.loaded.IsZero() // 刚失败过：不再打库
			c.mu.Unlock()
			return st, known
		}
		if e == nil {
			if len(c.m) >= userStatusMaxEntries {
				c.evictLocked(now)
			}
			e = &statusEntry{}
			c.m[id] = e
		}
		ch := make(chan struct{})
		e.loading = ch
		c.mu.Unlock()

		got, err := safeLoad(load, id)

		c.mu.Lock()
		e.loading = nil
		if err != nil {
			e.failAt = statusClock()
			st, known = e.st, !e.loaded.IsZero()
		} else {
			e.st, e.loaded, e.failAt = got, statusClock(), time.Time{}
			st, known = got, true
		}
		c.mu.Unlock()
		close(ch)
		if err != nil {
			slog.Warn("会话校验读取用户状态失败，沿用缓存 / 会话中的值", "uid", id, "err", err)
		}
		return st, known
	}
}

// evictLocked 在条目过多时清掉过期的（调用方持锁）。
func (c *statusCache) evictLocked(now time.Time) {
	for id, e := range c.m {
		if e.loading == nil && now.Sub(e.loaded) >= userStatusTTL {
			delete(c.m, id)
		}
	}
	if len(c.m) >= userStatusMaxEntries { // 仍然太多：整体丢弃（只会让下一轮请求多查一次库）
		for id, e := range c.m {
			if e.loading == nil {
				delete(c.m, id)
			}
		}
	}
}

func (c *statusCache) invalidate(id uint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.m[id]; e != nil && e.loading == nil {
		delete(c.m, id)
	}
}

func safeLoad(load func(uint) (UserStatus, error), id uint) (st UserStatus, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("读取用户状态 panic", "uid", id, "panic", r)
			err = errStatusPanic
		}
	}()
	return load(id)
}

type statusErr string

func (e statusErr) Error() string { return string(e) }

const errStatusPanic = statusErr("读取用户状态异常") // i18n-ignore

// gormStatusSource 直接读元数据库 dbt_sql_users（只读）。
type gormStatusSource struct{ db *gorm.DB }

func (g gormStatusSource) UserStatus(id uint) (UserStatus, error) {
	var rows []struct {
		IsActive    bool
		IsSuperuser bool
	}
	if err := g.db.Model(&model.User{}).Select("is_active", "is_superuser").Where("id = ?", id).Limit(1).Scan(&rows).Error; err != nil {
		return UserStatus{}, err
	}
	if len(rows) == 0 {
		return UserStatus{}, nil
	}
	return UserStatus{Exists: true, Active: rows[0].IsActive, Superuser: rows[0].IsSuperuser}, nil
}

// statusSource 决定用哪个来源；都没有时返回 nil（不做撤销校验）。
func (s *Server) statusSource() UserStatusSource {
	if src, ok := s.repo.(UserStatusSource); ok {
		return src
	}
	if p, ok := s.repo.(DBProvider); ok {
		if db := p.GormDB(); db != nil {
			return gormStatusSource{db}
		}
	}
	return nil
}

// liveStatus 对一个已通过签名校验的会话 / JWT 回库确认用户仍然可用。
// ok=false 表示用户已被停用或不存在（应 401）；super 是应当采用的超级用户标记（库里的最新值，库不可用时用传入值）。
func (s *Server) liveStatus(uid uint, tokenSuper bool) (ok, super bool) {
	src := s.statusSource()
	if src == nil {
		return true, tokenSuper
	}
	st, known := s.ustat.get(uid, src.UserStatus)
	if !known {
		return true, tokenSuper
	}
	if !st.Exists || !st.Active {
		return false, false
	}
	return true, st.Superuser
}

// InvalidateUserStatus 让某个用户的缓存状态立即失效（同进程内停用 / 改超级用户标记后调用，下一次请求即重新读库）。
func (s *Server) InvalidateUserStatus(uid uint) { s.ustat.invalidate(uid) }
