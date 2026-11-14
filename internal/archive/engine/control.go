// @author: kevinkelly
// @license: Apache Licence
// @file: control.go
// @time: 2026/10/28

package engine

import "context"

// NewControl 创建运行控制器。
func NewControl() *Control { return &Control{wake: make(chan struct{}), done: make(chan struct{})} }

func (c *Control) init() {
	if c.wake == nil {
		c.wake = make(chan struct{})
	}
	if c.done == nil {
		c.done = make(chan struct{})
	}
}

// notify 必须在持锁时调用：唤醒所有等待者并重建通道。
func (c *Control) notify() {
	close(c.wake)
	c.wake = make(chan struct{})
}

// Pause 请求暂停：当前批做完后，引擎阻塞在批边界，直到 Resume / Stop / ctx 取消。
func (c *Control) Pause() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	if !c.paused && !c.stopped {
		c.paused = true
		c.notify()
	}
}

// Resume 恢复被暂停的运行。
func (c *Control) Resume() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	if c.paused {
		c.paused = false
		c.notify()
	}
}

// Stop 请求终止：当前批做完后结束，Result=stopped。幂等；同时唤醒暂停和等待中的运行。
func (c *Control) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	if !c.stopped {
		c.stopped = true
		c.paused = false
		close(c.done)
		c.notify()
	}
}

func (c *Control) Paused() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

func (c *Control) Stopped() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

// snapshot 返回当前状态和“下次状态变化”的通知通道。
func (c *Control) snapshot() (paused, stopped bool, changed <-chan struct{}) {
	if c == nil {
		return false, false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	return c.paused, c.stopped, c.wake
}

// stopCtx 返回一个在 ctx 取消或 Stop 时取消的上下文，仅用于“等待”（休眠、负载等待、暂停），
// 数据库操作仍使用调用方的 ctx，所以 Stop 不会打断正在执行的批。
func (c *Control) stopCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	wctx, cancel := context.WithCancel(ctx)
	if c == nil {
		return wctx, cancel
	}
	c.mu.Lock()
	c.init()
	done := c.done
	c.mu.Unlock()
	go func() {
		select {
		case <-done:
			cancel()
		case <-wctx.Done():
		}
	}()
	return wctx, cancel
}
