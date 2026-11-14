// @author: kevinkelly
// @license: Apache Licence
// @file: split.go
// @time: 2026/10/28

// Package dbtadmin 是命令行工具 dbt-admin 的实现：初始化元数据库（bootstrap）、创建 / 重置管理员、生成密钥。
// 入口见 cmd/dbt-admin，用法与安全设计见 docs/admin-cli.md。
//
// 数据库访问全部经过 Store / Tx 接口，测试里用内存假实现替换；真实实现在 mysqlstore.go。
package dbtadmin

import (
	"fmt"
	"strings"
)

// Stmt 是从 SQL 脚本里切分出的一条语句。
type Stmt struct {
	Text string // 不含结尾的分号；开头的注释已去掉
	Line int    // 语句第一个有效字符所在行（从 1 起）
}

// SplitOptions 是切分选项。
type SplitOptions struct {
	// MaxStmtBytes 是单条语句的字节数上限，0 表示用默认值（DefaultMaxStmtBytes）。
	MaxStmtBytes int
}

// DefaultMaxStmtBytes 是单条语句的默认上限：这些脚本是建表与基础数据，没有理由出现更大的语句；
// 超过说明文件被改坏（比如引号没闭合把后半个文件吞进了一条语句）。
const DefaultMaxStmtBytes = 8 << 20

// SplitError 是切分失败：带行号，便于定位。
type SplitError struct {
	Line int
	Msg  string
}

func (e *SplitError) Error() string { return fmt.Sprintf("第 %d 行：%s", e.Line, e.Msg) }

// SplitSQL 把一个 SQL 脚本切成语句。规则（与 mysql 客户端对这类脚本的行为一致）：
//   - ; 结束一条语句；引号 '…' "…"（支持 \ 转义与重复引号）、反引号 `…`（重复反引号转义）、注释里的 ; 不算；
//   - 注释：-- （后面必须是空白、控制字符或文件结尾）、# 到行尾、/* … */；/*! … */ 与 /*+ … */ 是带内容的注释，原样保留在语句里；
//   - 只含注释 / 空白的“语句”会被丢弃；最后一条语句可以没有结尾分号；
//   - 不支持 DELIMITER（存储过程 / 触发器脚本需要它）：遇到直接报错，不猜。
//
// 不处理 sql_mode=NO_BACKSLASH_ESCAPES（这些脚本不会设置它）。
func SplitSQL(src string, opt SplitOptions) ([]Stmt, error) {
	max := opt.MaxStmtBytes
	if max <= 0 {
		max = DefaultMaxStmtBytes
	}
	src = strings.TrimPrefix(src, string(rune(0xFEFF))) // UTF-8 BOM
	var (
		out      []Stmt
		n        = len(src)
		i        = 0
		line     = 1
		start    = -1 // 当前语句第一个有效字符的偏移，-1 = 还没开始
		stmtLine = 0
	)
	begin := func() {
		if start < 0 {
			start, stmtLine = i, line
		}
	}
	emit := func(end int) error {
		if start < 0 {
			return nil
		}
		text := strings.TrimSpace(src[start:end])
		if len(text) > max {
			return &SplitError{Line: stmtLine, Msg: fmt.Sprintf("语句过长（%d 字节，上限 %d）；通常是引号或注释没有闭合", len(text), max)}
		}
		if text != "" {
			out = append(out, Stmt{Text: text, Line: stmtLine})
		}
		start = -1
		return nil
	}
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == ';':
			if err := emit(i); err != nil {
				return nil, err
			}
			i++
		case c == '#':
			i = skipLine(src, i)
		case c == '-' && i+1 < n && src[i+1] == '-' && (i+2 >= n || isSpaceOrCtl(src[i+2])):
			i = skipLine(src, i)
		case c == '/' && i+1 < n && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, &SplitError{Line: line, Msg: "注释 /* 没有闭合"}
			}
			body := src[i : i+2+end+2]
			if i+2 < n && (src[i+2] == '!' || src[i+2] == '+') { // 可执行注释 / 优化器提示：属于语句
				begin()
			}
			line += strings.Count(body, "\n")
			i += len(body)
		case c == '\'' || c == '"' || c == '`':
			begin()
			j, nl, err := skipQuoted(src, i, line)
			if err != nil {
				return nil, err
			}
			line += nl
			i = j
		default:
			if start < 0 {
				// 语句开头：DELIMITER 是客户端命令，不是 SQL
				if hasWordPrefix(src[i:], "delimiter") {
					return nil, &SplitError{Line: line, Msg: "不支持 DELIMITER（含存储过程 / 触发器的脚本请用 mysql 客户端执行）"}
				}
			}
			begin()
			i++
		}
	}
	if err := emit(n); err != nil {
		return nil, err
	}
	return out, nil
}

// skipLine 返回行尾（换行符本身不消耗，交给主循环计行号）。
func skipLine(s string, i int) int {
	for i < len(s) && s[i] != '\n' {
		i++
	}
	return i
}

func isSpaceOrCtl(b byte) bool { return b <= ' ' || b == 0x7f }

// hasWordPrefix 判断 s 是否以 word（不区分大小写）开头，且其后是空白或结尾。
func hasWordPrefix(s, word string) bool {
	if len(s) < len(word) || !strings.EqualFold(s[:len(word)], word) {
		return false
	}
	return len(s) == len(word) || isSpaceOrCtl(s[len(word)])
}

// skipQuoted 从 s[i]（引号字符）开始跳过一个带引号的片段，返回结束后的偏移与经过的换行数。
func skipQuoted(s string, i, line int) (next, newlines int, err error) {
	q := s[i]
	j := i + 1
	for j < len(s) {
		c := s[j]
		switch {
		case c == '\n':
			newlines++
			j++
		case c == '\\' && q != '`':
			if j+1 < len(s) && s[j+1] == '\n' {
				newlines++
			}
			j += 2
		case c == q:
			if j+1 < len(s) && s[j+1] == q { // 重复引号 = 转义
				j += 2
				continue
			}
			return j + 1, newlines, nil
		default:
			j++
		}
	}
	what := "字符串"
	if q == '`' {
		what = "反引号标识符"
	}
	return 0, 0, &SplitError{Line: line, Msg: what + "没有闭合"}
}

// Redact 把语句里的字符串字面量换成 '***' 并截断到 max 个字符，用于错误信息（语句里可能含密码哈希等值）。
func Redact(stmt string, max int) string {
	var b strings.Builder
	for i := 0; i < len(stmt); {
		c := stmt[i]
		if c == '\'' || c == '"' {
			j, _, err := skipQuoted(stmt, i, 1)
			if err != nil { // 没闭合：后面的内容都当作字面量
				b.WriteString("'***'")
				break
			}
			b.WriteString("'***'")
			i = j
			continue
		}
		b.WriteByte(c)
		i++
	}
	s := strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
