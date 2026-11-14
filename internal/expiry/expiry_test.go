// @author: kevinkelly
// @license: Apache Licence
// @file: expiry_test.go
// @time: 2026/10/28

package expiry

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckBoundary(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	cases := []struct {
		name    string
		now     time.Time
		expired bool
	}{
		{"现在", time.Date(2026, 10, 16, 12, 0, 0, 0, cst), false},
		{"截止前一秒（北京时间）", time.Date(2029, 12, 31, 23, 59, 59, 0, cst), false},
		{"截止时刻（北京时间）", time.Date(2030, 1, 1, 0, 0, 0, 0, cst), true},
		{"截止时刻（UTC 表示）", time.Date(2029, 12, 31, 16, 0, 0, 0, time.UTC), true},
		{"截止前一秒（UTC 表示）", time.Date(2029, 12, 31, 15, 59, 59, 0, time.UTC), false},
		{"截止后一年", time.Date(2031, 1, 1, 0, 0, 0, 0, cst), true},
	}
	for _, c := range cases {
		err := check(c.now)
		if got := err != nil; got != c.expired {
			t.Errorf("%s: check(%v) 过期=%v，期望 %v", c.name, c.now, got, c.expired)
		}
	}
}

func TestExpiredErrorMessage(t *testing.T) {
	err := check(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC))
	var ee *ExpiredError
	if !errors.As(err, &ee) {
		t.Fatalf("期望 *ExpiredError，得到 %T", err)
	}
	if !strings.Contains(err.Error(), "2030-01-01") {
		t.Errorf("提示里应写明截止日期，得到 %q", err.Error())
	}
}

func TestWatchFiresOnceAfterDeadline(t *testing.T) {
	var past atomic.Bool // false=还没到期；置 true 模拟时钟越过截止时刻
	now := func() time.Time {
		if past.Load() {
			return deadline.Add(time.Second)
		}
		return deadline.Add(-time.Hour)
	}
	fired := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watch(ctx, now, 5*time.Millisecond, func() { fired <- struct{}{} })

	select {
	case <-fired:
		t.Fatal("未到期就触发了 onExpire")
	case <-time.After(50 * time.Millisecond):
	}
	past.Store(true)
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("到期后没有触发 onExpire")
	}
	select {
	case <-fired:
		t.Fatal("onExpire 被触发了不止一次")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWatchStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watch(ctx, func() time.Time { return deadline.Add(-time.Hour) }, 5*time.Millisecond, func() { t.Error("不应触发") })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 watch 没有退出")
	}
}
