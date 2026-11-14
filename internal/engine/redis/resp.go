// @author: kevinkelly
// @license: Apache Licence
// @file: resp.go
// @time: 2026/10/28

package redis

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// RESP2 的最小实现：只支持客户端发数组命令、读五种回复（简单字符串、错误、整数、批量字符串、数组）。
// 回复值映射：简单字符串 / 批量字符串 → string；整数 → int64；空批量 / 空数组 → nil；数组 → []any；错误 → *ServerError（作为 error 返回）。

const (
	maxBulkLen  = 16 << 20 // 单个批量字符串上限 16MB：异常大的回复不读进内存
	maxArrayLen = 200000   // 数组元素上限
	maxDepth    = 6        // 嵌套深度上限（SLOWLOG / ROLE / COMMAND INFO 最深 3 层）
)

// ServerError 是服务端返回的错误回复（以 - 开头）。
type ServerError struct{ Msg string }

func (e *ServerError) Error() string { return e.Msg }

// Kind 返回错误码（第一个单词），如 ERR、NOAUTH、NOPERM、WRONGPASS。
func (e *ServerError) Kind() string {
	k, _, _ := strings.Cut(e.Msg, " ")
	return k
}

// writeCommand 把命令编码成 RESP 数组写出。参数不允许含 NUL 以外的限制（批量字符串是二进制安全的）。
func writeCommand(w *bufio.Writer, args []string) error {
	if _, err := fmt.Fprintf(w, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, a := range args {
		if _, err := fmt.Fprintf(w, "$%d\r\n", len(a)); err != nil {
			return err
		}
		if _, err := w.WriteString(a); err != nil {
			return err
		}
		if _, err := w.WriteString("\r\n"); err != nil {
			return err
		}
	}
	return w.Flush()
}

func readLine(r *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		line, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", err
		}
		sb.Write(line)
		if sb.Len() > 64<<10 {
			return "", errors.New("redis 回复的行过长")
		}
		if !isPrefix {
			return sb.String(), nil
		}
	}
}

// readReply 读取一个完整的回复；服务端错误回复作为 *ServerError 返回。
func readReply(r *bufio.Reader) (any, error) {
	v, err := readValue(r, 0)
	if err != nil {
		return nil, err
	}
	if se, ok := v.(*ServerError); ok {
		return nil, se
	}
	return v, nil
}

// readValue 读一个值；数组里的错误元素原样保留为 *ServerError（不当作整体失败）。
func readValue(r *bufio.Reader, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("redis 回复嵌套过深")
	}
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	if line == "" {
		return nil, errors.New("redis 回复为空行")
	}
	body := line[1:]
	switch line[0] {
	case '+':
		return body, nil
	case '-':
		return &ServerError{Msg: body}, nil
	case ':':
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis 整数回复不合法: %q", body)
		}
		return n, nil
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n < -1 {
			return nil, fmt.Errorf("redis 批量回复长度不合法: %q", body)
		}
		if n == -1 {
			return nil, nil
		}
		if n > maxBulkLen {
			// 丢弃这段内容，保持连接可继续使用，同时告诉调用方回复太大
			if _, err := io.CopyN(io.Discard, r, int64(n)+2); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("redis 回复过大（%d 字节，上限 %d）", n, maxBulkLen)
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil || n < -1 {
			return nil, fmt.Errorf("redis 数组回复长度不合法: %q", body)
		}
		if n == -1 {
			return nil, nil
		}
		if n > maxArrayLen {
			return nil, fmt.Errorf("redis 数组回复过长（%d 个元素，上限 %d）", n, maxArrayLen)
		}
		out := make([]any, 0, min(n, 1024))
		for i := 0; i < n; i++ {
			v, err := readValue(r, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, fmt.Errorf("无法识别的 redis 回复类型 %q", line[:1])
}
