// @author: kevinkelly
// @license: Apache Licence
// @file: navbadge_test.go
// @time: 2026/10/28

package server

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"dbtool/internal/audit"
)

func withBadge(t *testing.T, key string, fn NavBadgeFunc) {
	t.Helper()
	old, had := navBadgeDefs[key]
	AddNavBadge(key, "测试角标 %d", fn)
	t.Cleanup(func() {
		if had {
			navBadgeDefs[key] = old
		} else {
			delete(navBadgeDefs, key)
		}
	})
}

func TestNavBadgesCountAndCache(t *testing.T) {
	var calls atomic.Int32
	withBadge(t, "t-count", func(*Server, audit.Actor) (int, bool, error) { calls.Add(1); return 3, true, nil })
	withBadge(t, "t-zero", func(*Server, audit.Actor) (int, bool, error) { return 0, false, nil })
	s := &Server{}
	a := audit.Actor{ID: 7, Username: "u7"}

	got := s.navBadges(a, []string{"t-count", "t-zero", "no-such"})
	if len(got) != 1 || got["t-count"].n != 3 || !got["t-count"].warn {
		t.Fatalf("角标: %+v", got)
	}
	s.navBadges(a, []string{"t-count"})
	if calls.Load() != 1 {
		t.Errorf("TTL 内应命中缓存，实际计算 %d 次", calls.Load())
	}
	// 不同用户互不影响
	s.navBadges(audit.Actor{ID: 8}, []string{"t-count"})
	if calls.Load() != 2 {
		t.Errorf("不同用户应各算一次: %d", calls.Load())
	}
}

func TestNavBadgesFailureKeepsStaleValue(t *testing.T) {
	var fail atomic.Bool
	withBadge(t, "t-flaky", func(*Server, audit.Actor) (int, bool, error) {
		if fail.Load() {
			return 0, false, errors.New("db down")
		}
		return 5, false, nil
	})
	s := &Server{}
	s.nb.ttl = -1 // 不使用新鲜缓存：每次都重算
	a := audit.Actor{ID: 1}
	if got := s.navBadges(a, []string{"t-flaky"}); got["t-flaky"].n != 5 {
		t.Fatalf("首次: %+v", got)
	}
	fail.Store(true)
	if got := s.navBadges(a, []string{"t-flaky"}); got["t-flaky"].n != 5 {
		t.Errorf("失败时应沿用上一次的值: %+v", got)
	}
	// 从没成功过的用户：失败只是不显示
	if got := s.navBadges(audit.Actor{ID: 2}, []string{"t-flaky"}); len(got) != 0 {
		t.Errorf("没有旧值时不应显示角标: %+v", got)
	}
	// 成功算出 0：不能再沿用旧值
	fail.Store(false)
	withBadge(t, "t-flaky", func(*Server, audit.Actor) (int, bool, error) { return 0, false, nil })
	if got := s.navBadges(a, []string{"t-flaky"}); len(got) != 0 {
		t.Errorf("待办清零后角标应消失: %+v", got)
	}
}

func TestNavBadgesSlowAndPanicDoNotBlock(t *testing.T) {
	release := make(chan struct{})
	withBadge(t, "t-slow", func(*Server, audit.Actor) (int, bool, error) { <-release; return 9, false, nil })
	withBadge(t, "t-panic", func(*Server, audit.Actor) (int, bool, error) { panic("boom") })
	withBadge(t, "t-ok", func(*Server, audit.Actor) (int, bool, error) { return 1, false, nil })
	s := &Server{}
	s.nb.wait = 30 * time.Millisecond
	start := time.Now()
	got := s.navBadges(audit.Actor{ID: 1}, []string{"t-slow", "t-panic", "t-ok"})
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("慢角标不应拖住页面: %v", d)
	}
	if len(got) != 1 || got["t-ok"].n != 1 {
		t.Errorf("只应显示算出来的角标: %+v", got)
	}
	close(release) // 放行后台计算，让 goroutine 结束
}

func TestBadgeText(t *testing.T) {
	for n, want := range map[int]string{1: "1", 99: "99", 100: "99+", 5000: "99+"} {
		if got := badgeText(n); got != want {
			t.Errorf("badgeText(%d)=%q, want %q", n, got, want)
		}
	}
}
