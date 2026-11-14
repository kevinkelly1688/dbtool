// @author: kevinkelly
// @license: Apache Licence
// @file: errors.go
// @time: 2026/10/28

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// UserError 是语句检查 / 服务端执行报错里“可以直接展示给用户”的业务原因（上层按 bad_query 处理）。
// Code 为空表示静态检查拒绝；非空时是服务端返回的 SQLSTATE（如 42P01 表不存在、42601 语法错误）。
type UserError struct {
	Msg  string
	Code string
}

func (e *UserError) Error() string { return e.Msg }

func uerr(format string, a ...any) error { return &UserError{Msg: fmt.Sprintf(format, a...)} }

// ErrTimeout 表示语句超过执行时限被终止（服务端 statement_timeout 或客户端超时）。可用 errors.Is 判断。
var ErrTimeout = errors.New("查询超过执行时限已终止")

// IsTimeout 判断 err 是否是超时。
func IsTimeout(err error) bool { return errors.Is(err, ErrTimeout) }

// redact 把错误文本里出现的密码抹掉（正常情况下驱动的错误信息不含密码，这里是兜底）。
func redact(msg, password string) string {
	if len(password) >= 4 {
		msg = strings.ReplaceAll(msg, password, "******")
	}
	return msg
}

// mapError 把驱动 / 服务端错误转成业务错误：
//   - 服务端的 SQL 错误（语法、不存在、权限…）→ *UserError，带 SQLSTATE；
//   - 57014（statement_timeout / 被取消）→ ErrTimeout 或 context.Canceled；
//   - 连接 / 网络错误 → 普通 error（已抹掉密码）。
func mapError(err error, password string) error {
	if err == nil {
		return nil
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "57014":
			if strings.Contains(pe.Message, "statement timeout") {
				return fmt.Errorf("%w（超过设定的最大执行时间）", ErrTimeout)
			}
			return fmt.Errorf("查询已被取消：%w", context.Canceled)
		case "25006":
			return &UserError{Code: pe.Code, Msg: fmt.Sprintf("只读事务内不允许执行会修改数据的操作：%s", redact(pe.Message, password))}
		case "28P01", "28000":
			return &UserError{Code: pe.Code, Msg: "PostgreSQL 认证失败：账号或密码错误，或 pg_hba.conf 不允许该来源连接"}
		case "3D000":
			return &UserError{Code: pe.Code, Msg: fmt.Sprintf("数据库不存在或不可连接：%s", redact(pe.Message, password))}
		case "55P03":
			return &UserError{Code: pe.Code, Msg: fmt.Sprintf("等待锁超时（lock_timeout），请稍后重试：%s", redact(pe.Message, password))}
		}
		// 整句格式串（提示 / 位置各有有无两种），目录里按整句翻译，不用拼接。
		text := redact(pe.Message, password)
		if pe.Detail != "" {
			text += "；" + redact(pe.Detail, password)
		}
		var msg string
		switch {
		case pe.Hint != "" && pe.Position > 0:
			msg = fmt.Sprintf("PostgreSQL 报错：%s（提示：%s）（位置 %d） [%s]", text, redact(pe.Hint, password), pe.Position, pe.Code)
		case pe.Hint != "":
			msg = fmt.Sprintf("PostgreSQL 报错：%s（提示：%s） [%s]", text, redact(pe.Hint, password), pe.Code)
		case pe.Position > 0:
			msg = fmt.Sprintf("PostgreSQL 报错：%s（位置 %d） [%s]", text, pe.Position, pe.Code)
		default:
			msg = fmt.Sprintf("PostgreSQL 报错：%s [%s]", text, pe.Code)
		}
		return &UserError{Code: pe.Code, Msg: msg}
	}
	var ue *UserError
	if errors.As(err, &ue) {
		return err
	}
	var big *pgproto3.ExceededMaxBodyLenErr
	if errors.As(err, &big) {
		return uerr("单行结果超过 %d MB，已中止并断开连接；请不要在一个单元格里返回超大的值（可用 left() / substring() 截取）", maxMessageBytes>>20)
	}
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return fmt.Errorf("%w（客户端等待超时）", ErrTimeout)
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return errors.New(redact(err.Error(), password))
}
