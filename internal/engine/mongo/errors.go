// @author: kevinkelly
// @license: Apache Licence
// @file: errors.go
// @time: 2026/10/28

package mongo

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// UserError 是语句检查 / 执行不通过的业务原因，原文可直接展示给用户（上层按 bad_query 处理，与 redis 引擎的 UserError 同义）。
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func uerr(format string, a ...any) error { return &UserError{Msg: fmt.Sprintf(format, a...)} }

// codeError 由驱动的 CommandError / WriteException 等实现（HasErrorCode），用于不依赖具体驱动类型识别服务端错误码。
type codeError interface{ HasErrorCode(int) bool }

// 服务端错误码（只列用到的）。
const (
	codeUnauthorized          = 13
	codeMaxTimeMSExpired      = 50
	codeInterrupted           = 11601
	codeExceededTimeLimit     = 262
	codeQueryMemoryNoDisk     = 292
	codeSortMemoryLimit       = 16819
	codeNamespaceNotFound     = 26
	codeAuthenticationFailed  = 18
	codeCursorNotFound        = 43
	codeBadValue              = 2
	codeFailedToParse         = 9
	codeOperationNotSupported = 115
)

// friendlyError 把驱动 / 服务端错误转成可展示的中文业务错误，并抹掉可能出现在错误文本里的凭据。
// ctx 用来区分“用户取消”和“超时”：取消原样返回 context.Canceled（由上层映射为 canceled），超时返回带说明的 UserError。
func (e *Engine) friendlyError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	var ue *UserError
	if errors.As(err, &ue) {
		return err
	}
	if strings.Contains(err.Error(), "server selection error") {
		return fmt.Errorf("无法连接 MongoDB 实例：%s", e.scrub(err.Error()))
	}
	var to interface{ Timeout() bool }
	if errors.As(err, &to) && to.Timeout() {
		return ErrTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	var ce codeError
	if errors.As(err, &ce) {
		switch {
		case ce.HasErrorCode(codeMaxTimeMSExpired), ce.HasErrorCode(codeExceededTimeLimit), ce.HasErrorCode(codeInterrupted):
			return ErrTimeout
		case ce.HasErrorCode(codeQueryMemoryNoDisk), ce.HasErrorCode(codeSortMemoryLimit):
			return uerr("服务端内存不足以完成排序 / 分组（在线查询禁止使用磁盘临时文件）：请给排序字段建索引、加更严格的过滤条件或先 $match / $limit 缩小数据量。")
		case ce.HasErrorCode(codeUnauthorized):
			return uerr("连接账号没有执行该查询所需的权限（MongoDB 返回 Unauthorized）：%s", e.scrub(err.Error()))
		case ce.HasErrorCode(codeAuthenticationFailed):
			return errors.New("MongoDB 认证失败：请检查实例的账号 / 密码 / 认证库（认证库固定为 admin）")
		}
	}
	return errors.New(e.scrub(err.Error()))
}

// ErrTimeout 表示查询超过了执行时间上限。
var ErrTimeout = errors.New("查询超过执行时间上限已终止，请缩小范围（加过滤条件 / limit / 走索引）")

// scrub 去掉文本里的密码，并折叠 mongodb:// 连接串里的认证信息；长度截到 600 字符。
func (e *Engine) scrub(s string) string {
	if e != nil && e.password != "" {
		s = strings.ReplaceAll(s, e.password, "***")
	}
	for {
		i := strings.Index(s, "mongodb://")
		if j := strings.Index(s, "mongodb+srv://"); j >= 0 && (i < 0 || j < i) {
			i = j
		}
		if i < 0 {
			break
		}
		start := i + strings.Index(s[i:], "://") + 3
		end := start
		for end < len(s) && s[end] != '/' && s[end] != ' ' && s[end] != '"' && s[end] != '\'' {
			end++
		}
		host := s[start:end]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		s = s[:i] + "mongodb-uri://" + host + s[end:]
	}
	if r := []rune(s); len(r) > 600 {
		s = string(r[:600]) + "…"
	}
	return s
}
