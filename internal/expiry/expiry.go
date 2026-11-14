// @author: kevinkelly
// @license: Apache Licence
// @file: expiry.go
// @time: 2026/10/28

// Package expiry 给程序加固定的使用期限：到期后服务和管理命令不再启动，已在运行的服务到点自行优雅退出。
//
// 这里只是一个写死的截止日期，没有试用期、授权码或只读模式（授权体系尚未实现）。
// 要延期就改下面的 deadline 后重新打包；刻意不提供环境变量 / 配置项 / 启动参数来覆盖它。
package expiry

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// deadline 是使用期限的截止时刻：2030-01-01 00:00:00（北京时间，UTC+8）起不可用，2029-12-31 当天仍可用。
// 用固定时区而不是 time.Local，避免部署机器时区不同让截止时刻漂移，也不依赖目标机器有没有 tzdata。
var deadline = time.Date(2030, 1, 1, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))

// checkInterval 是运行期间复查的间隔。用墙上时钟轮询而不是 time.Timer：Timer 走单调时钟，机器休眠或被调时间后不会如期触发。
const checkInterval = time.Minute

// ExpiredError 表示已超过使用期限。
type ExpiredError struct{ Deadline time.Time }

func (e *ExpiredError) Error() string {
	return fmt.Sprintf("已超过使用期限（%s 起不可用），请联系提供方获取新版本", e.Deadline.Format("2006-01-02"))
}

// Check 在当前时间已到截止时刻时返回 *ExpiredError，否则返回 nil。
func Check() error { return check(time.Now()) }

func check(now time.Time) error {
	if now.Before(deadline) {
		return nil
	}
	return &ExpiredError{Deadline: deadline}
}

// Watch 在后台每分钟复查一次，发现到期后调用 onExpire（只调一次）并退出；ctx 结束时也退出。
// 传入的 onExpire 应触发服务自己的优雅停止流程（例如 signal.NotifyContext 返回的 cancel），而不是直接 os.Exit：
// 正在执行的 SQL 工单已经发到目标库，不能被中途丢掉。
func Watch(ctx context.Context, onExpire func()) {
	go watch(ctx, time.Now, checkInterval, onExpire)
}

func watch(ctx context.Context, now func() time.Time, every time.Duration, onExpire func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := check(now()); err != nil {
				slog.Error("使用期限已到，服务即将停止", "err", err)
				onExpire()
				return
			}
		}
	}
}
