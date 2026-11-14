// @author: kevinkelly
// @license: Apache Licence
// @file: parse.go
// @time: 2026/10/28

package tsql

import (
	"fmt"
	"strings"

	"github.com/antlr4-go/antlr/v4"
	parser "github.com/bytebase/parser/tsql"
)

// SyntaxError 是一处语法错误，位置相对于被解析的文本（行从 1 开始，列从 0 开始）。
type SyntaxError struct {
	Line    int
	Column  int
	Message string
}

func (e SyntaxError) String() string {
	return fmt.Sprintf("第 %d 行第 %d 列附近：%s", e.Line, e.Column+1, e.Message)
}

// Parsed 是一段 T-SQL 的解析结果。
type Parsed struct {
	Tree   parser.ITsql_fileContext // 有语法错误时可能为 nil 或不完整
	Errors []SyntaxError
}

// OK 解析成功且没有语法错误。
func (p *Parsed) OK() bool { return p != nil && len(p.Errors) == 0 && p.Tree != nil }

type collector struct {
	*antlr.DefaultErrorListener
	errs []SyntaxError
}

func (c *collector) SyntaxError(_ antlr.Recognizer, _ interface{}, line, column int, msg string, _ antlr.RecognitionException) {
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if len(c.errs) < 20 {
		c.errs = append(c.errs, SyntaxError{Line: line, Column: column, Message: localize(msg)})
	}
}

// localize 把 ANTLR 的英文提示换成中文前缀，保留原文里的符号与期望项。
func localize(msg string) string {
	switch {
	case strings.HasPrefix(msg, "mismatched input"):
		return fmt.Sprintf("无法识别的输入 %s", strings.TrimPrefix(msg, "mismatched input"))
	case strings.HasPrefix(msg, "extraneous input"):
		return fmt.Sprintf("多余的输入 %s", strings.TrimPrefix(msg, "extraneous input"))
	case strings.HasPrefix(msg, "missing"):
		return fmt.Sprintf("缺少 %s", strings.TrimPrefix(msg, "missing"))
	case strings.HasPrefix(msg, "no viable alternative at input"):
		return fmt.Sprintf("语法不正确，位置在 %s", strings.TrimPrefix(msg, "no viable alternative at input"))
	case strings.HasPrefix(msg, "token recognition error"):
		return fmt.Sprintf("无法识别的字符 %s", strings.TrimPrefix(msg, "token recognition error at:"))
	}
	return msg
}

func newParser(sql string, c *collector) *parser.TSqlParser {
	lex := parser.NewTSqlLexer(antlr.NewInputStream(sql))
	lex.RemoveErrorListeners()
	if c != nil {
		lex.AddErrorListener(c)
	}
	p := parser.NewTSqlParser(antlr.NewCommonTokenStream(lex, antlr.TokenDefaultChannel))
	p.RemoveErrorListeners()
	if c != nil {
		p.AddErrorListener(c)
	}
	return p
}

// Parse 解析一段 T-SQL（一个或多个批），收集全部语法错误。
// 解析器内部异常（极端输入）按语法错误返回，不会 panic。
//
// 说明：ANTLR 推荐的“先 SLL 遇错即停、失败再 LL”两阶段做法在这个语法上拿不到错误（Go 运行时的 Bail 策略不会中止），
// 所以直接用 LL 模式；工单脚本的体量下耗时可以忽略。
func Parse(sql string) (res *Parsed) {
	defer func() {
		if r := recover(); r != nil {
			res = &Parsed{Errors: []SyntaxError{{Line: 1, Message: fmt.Sprintf("语法解析器内部错误：%v", r)}}}
		}
	}()
	c := &collector{DefaultErrorListener: antlr.NewDefaultErrorListener()}
	p := newParser(sql, c)
	tree := p.Tsql_file()
	return &Parsed{Tree: tree, Errors: c.errs}
}
