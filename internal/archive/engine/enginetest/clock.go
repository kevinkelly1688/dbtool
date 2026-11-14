// @author: kevinkelly
// @license: Apache Licence
// @file: clock.go
// @time: 2026/10/28

// Package enginetest 是归档引擎的内存测试替身：可编程的 SourceDB / DestDB / FileStore / LoadProbe、假时钟、事件收集器，
// 以及把它们装配成 engine.Env 的 Harness。引擎自己的测试和上层服务的测试都可以用它，不连任何真实库。
package enginetest

import (
	"context"
	"sync"
	"time"
)

// Clock 是假时钟：Sleep 只推进时间、不真睡。可并发使用。
type Clock struct {
	mu      sync.Mutex
	t       time.Time
	slept   []time.Duration
	OnSleep func(d time.Duration) // 每次 Sleep 前调用（可用来让负载随“时间”恢复）
}

func NewClock(start time.Time) *Clock { return &Clock{t: start} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Sleep 符合 engine.Env.Sleep 的签名。ctx 已取消则返回错误。
func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.OnSleep != nil {
		c.OnSleep(d)
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.slept = append(c.slept, d)
	c.mu.Unlock()
	return ctx.Err()
}

// Slept 返回迄今所有 Sleep 的时长。
func (c *Clock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}
