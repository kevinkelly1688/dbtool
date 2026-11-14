// @author: kevinkelly
// @license: Apache Licence
// @file: errors.go
// @time: 2026/10/28

package oracle

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/sijms/go-ora/v2/network"
)

// ServerError 是目标库 / 驱动返回的错误（口令已抹掉）。Code 是 ORA-NNNNN 的数字，没有时为 0。
type ServerError struct {
	Code  int
	Msg   string
	cause error
}

func (e *ServerError) Error() string { return e.Msg }
func (e *ServerError) Unwrap() error { return e.cause }

var oraCodeRe = regexp.MustCompile(`ORA-(\d{5})`)

// oraCode 取错误里的 ORA 错误号：先看驱动的错误类型，没有就从文本里找。
func oraCode(err error) int {
	var oe *network.OracleError
	if errors.As(err, &oe) && oe.ErrCode != 0 {
		return oe.ErrCode
	}
	if m := oraCodeRe.FindStringSubmatch(err.Error()); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// infraCodes 是连接 / 会话 / 实例层面的错误号：账号口令、监听、通信中断、实例状态……不是用户语句的问题。
var infraCodes = map[int]bool{
	1017: true, 1033: true, 1034: true, 1035: true, 1089: true, 1090: true, 1092: true, 3113: true, 3114: true, 3135: true, 4031: true, 28000: true,
	28001: true, 28002: true, 28009: true, 28040: true, 12154: true, 12170: true, 12514: true, 12516: true, 12518: true, 12519: true, 12520: true,
	12521: true, 12523: true, 12525: true, 12526: true, 12527: true, 12528: true, 12537: true, 12541: true, 12543: true, 12545: true, 12560: true,
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
	return &ServerError{Code: oraCode(err), Msg: msg, cause: err}
}

// IsTimeout 判断是不是超时（客户端期限到了，或服务端因期限中断了语句）。
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// IsCanceled 判断是不是用户 / 上层取消（ORA-01013 是驱动按取消发出中断后服务端的回应）。
func IsCanceled(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	var se *ServerError
	return errors.As(err, &se) && se.Code == 1013
}

// IsInfra 判断是不是连接 / 实例层面的错误（账号口令不对、监听没起、通信中断……）。
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
	return errors.As(err, &se) && infraCodes[se.Code]
}

// IsReadOnlyViolation 判断服务端是否因只读事务拒绝了写操作（ORA-01456）。
func IsReadOnlyViolation(err error) bool {
	var se *ServerError
	return errors.As(err, &se) && se.Code == 1456
}

func connectError(err error, host string, port int, secrets ...string) error {
	e := mapError(err, secrets...)
	if IsTimeout(err) {
		return fmt.Errorf("连接 Oracle %s:%d 超时：%w", host, port, e)
	}
	return e
}
