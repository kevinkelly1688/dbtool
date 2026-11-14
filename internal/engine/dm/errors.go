// @author: kevinkelly
// @license: Apache Licence
// @file: errors.go
// @time: 2026/10/28

package dm

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	dmdrv "gitee.com/chunanyong/dm"
)

// ServerError 是目标库 / 驱动返回的错误（口令已抹掉）。Code 是达梦的错误码（负数，如 -2106），没有时为 0；
// Infra 为真表示这是连接 / 会话层面的错误（建连失败、通信中断），不是用户语句的问题。
type ServerError struct {
	Code  int
	Msg   string
	Infra bool
	cause error
}

func (e *ServerError) Error() string { return e.Msg }
func (e *ServerError) Unwrap() error { return e.cause }

// dmCode 取驱动错误里的达梦错误码。
func dmCode(err error) int {
	var de *dmdrv.DmError
	if errors.As(err, &de) {
		return int(de.ErrCode)
	}
	return 0
}

// infraCodes 是驱动自己报的连接 / 通信类错误码（驱动本地的错误码是正数，服务端报的错误码是负数）。
var infraCodes = map[int]bool{
	6001: true, 6002: true, 6060: true, // 通信错误、报文校验错误、连接已关闭
	9007: true, 9008: true, // 不认识的网络类型、连接无效
	20000: true, 20001: true, 20002: true, 20004: true, // 连接已切换 / 切换失败、SSL 初始化失败、致命错误
}

// infraWords 是错误文本里表示连接 / 通信问题的词（没有错误码时的兜底）。
var infraWords = []string{"通信", "网络", "连接已", "连接失败", "无法连接", "登录失败", "用户名或密码", "connection", "refused", "timeout", "i/o", "broken pipe", "EOF"} // i18n-ignore 与驱动错误文本做匹配用，不展示

func looksInfra(msg string) bool {
	low := strings.ToLower(msg)
	for _, w := range infraWords {
		if strings.Contains(low, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

// mapError 抹掉错误文本里的口令，并包成 *ServerError（超时 / 取消 / 业务错误原样返回，方便上层判断）。
func mapError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	var ue *UserError
	if errors.As(err, &ue) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var se *ServerError
	if errors.As(err, &se) {
		return err
	}
	msg := err.Error()
	for _, s := range secrets {
		if len(s) >= 3 {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	code := dmCode(err)
	return &ServerError{Code: code, Msg: msg, Infra: infraCodes[code], cause: err}
}

// IsTimeout 判断是不是超时（客户端期限到了）。
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// IsCanceled 判断是不是用户 / 上层取消。
func IsCanceled(err error) bool { return errors.Is(err, context.Canceled) }

// IsInfra 判断是不是连接 / 实例层面的错误（建连失败、口令不对、通信中断……）。
func IsInfra(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false // 超时 / 取消是调用方的期限问题，不是实例故障（context.DeadlineExceeded 自己也满足 net.Error）
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var se *ServerError
	return errors.As(err, &se) && (se.Infra || looksInfra(se.Msg) && se.Code == 0)
}

// IsReadOnlyViolation 判断服务端是否因只读事务拒绝了写操作（错误文本里有“只读”或 read only）。
func IsReadOnlyViolation(err error) bool {
	var se *ServerError
	if !errors.As(err, &se) {
		return false
	}
	low := strings.ToLower(se.Msg)
	return strings.Contains(low, "只读") || strings.Contains(low, "read only") || strings.Contains(low, "read-only") // i18n-ignore 与服务端错误文本做匹配用，不展示
}

// connectError 把建连失败包成连接层面的错误（Infra）。
func connectError(err error, host string, port int, secrets ...string) error {
	e := mapError(err, secrets...)
	var se *ServerError
	if errors.As(e, &se) {
		se.Infra = true
		if IsTimeout(err) {
			se.Msg = fmt.Sprintf("连接达梦 %s:%d 超时：%s", host, port, se.Msg)
		}
		return se
	}
	if IsTimeout(err) {
		return fmt.Errorf("连接达梦 %s:%d 超时：%w", host, port, e)
	}
	return e
}
