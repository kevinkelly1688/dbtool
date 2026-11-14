// @author: kevinkelly
// @license: Apache Licence
// @file: term.go
// @time: 2026/10/28

package dbtadmin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// errNoEcho 表示当前平台没有实现“终端无回显读密码”。
var errNoEcho = errors.New("本平台没有实现终端无回显输入（需要 golang.org/x/term，本项目未引入该依赖）；请改用环境变量 " + PasswordEnv)

// stdinIsTerminal 报告标准输入是否是字符设备（终端）。只用标准库，不依赖 x/term。
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// readPasswordNoEcho 在终端里读一行密码：先关掉回显（平台实现见 term_*.go），读完恢复。
// 没有终端、或平台不支持关闭回显时返回错误，绝不回退成“有回显”读取。
func readPasswordNoEcho(prompt string) (string, error) {
	if !stdinIsTerminal() {
		return "", errors.New("标准输入不是终端")
	}
	restore, err := disableEcho(os.Stdin)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, prompt)
	line, rerr := readLineRaw(os.Stdin)
	restore()
	fmt.Fprintln(os.Stderr)
	if rerr != nil {
		return "", rerr
	}
	return line, nil
}

// readLineRaw 逐字节读到换行（不经缓冲，避免吞掉后续输入）。
func readLineRaw(r io.Reader) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			if err == io.EOF && sb.Len() > 0 {
				break
			}
			return "", err
		}
	}
	return strings.TrimRight(sb.String(), "\r"), nil
}
