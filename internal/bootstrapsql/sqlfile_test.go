// @author: kevinkelly
// @license: Apache Licence
// @file: sqlfile_test.go
// @time: 2026/10/28

package bootstrapsql

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"
	mysql "github.com/bytebase/parser/mysql"
)

// ---- 仓库路径与文件读取 ----

// repoRoot 从当前包目录向上找到含 go.mod 的目录。
func repoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			t.Fatalf("从 %s 向上找不到 go.mod", dir)
		}
		d = p
	}
}

func readFile(t testing.TB, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取 %s：%v", rel, err)
	}
	return string(b)
}

const (
	schemaFile = "deploy/bootstrap/001_base_schema.sql"
	dataFile   = "deploy/bootstrap/002_base_data.sql"
)

// ---- 语句切分（规则与 mysql 客户端、internal/dbtadmin.SplitSQL 对这类脚本的处理一致） ----

type stmt struct {
	Text string // 不含结尾分号
	Line int    // 第一个有效字符所在行（从 1 起）
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v'
}

func skipLine(s string, i int) int {
	for i < len(s) && s[i] != '\n' {
		i++
	}
	return i
}

// splitSQL 按分号切分语句：引号（' " 支持反斜杠转义，` 不支持）与注释里的分号不算；语句开头的注释被丢弃；
// 不支持 DELIMITER。引号或块注释没有闭合返回错误。
func splitSQL(src string) ([]stmt, error) {
	var out []stmt
	n, i, line := len(src), 0, 1
	start, startLine := -1, 0
	begin := func() {
		if start < 0 {
			start, startLine = i, line
		}
	}
	emit := func(end int) {
		if start < 0 {
			return
		}
		if txt := strings.TrimSpace(src[start:end]); txt != "" {
			out = append(out, stmt{Text: txt, Line: startLine})
		}
		start = -1
	}
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case isSpace(c):
			i++
		case c == ';':
			emit(i)
			i++
		case c == '#':
			i = skipLine(src, i)
		case c == '-' && i+1 < n && src[i+1] == '-' && (i+2 >= n || isSpace(src[i+2])):
			i = skipLine(src, i)
		case c == '/' && i+1 < n && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("第 %d 行：块注释没有闭合", line)
			}
			body := src[i : i+2+end+2]
			if len(body) > 2 && (body[2] == '!' || body[2] == '+') {
				begin()
			}
			line += strings.Count(body, "\n")
			i += len(body)
		case c == '\'' || c == '"' || c == '`':
			begin()
			q := c
			j := i + 1
			for ; j < n; j++ {
				if src[j] == '\n' {
					line++
				}
				if src[j] == '\\' && q != '`' && j+1 < n {
					if src[j+1] == '\n' {
						line++
					}
					j++
					continue
				}
				if src[j] == q {
					break
				}
			}
			if j >= n {
				return nil, fmt.Errorf("第 %d 行：引号 %c 没有闭合", startLine, q)
			}
			i = j + 1
		default:
			begin()
			i++
		}
	}
	emit(n)
	return out, nil
}

// ---- 用仓库已有的 MySQL 语法（github.com/bytebase/parser/mysql）解析 ----

type syntaxErrs struct {
	*antlr.DefaultErrorListener
	msgs []string
}

func (e *syntaxErrs) SyntaxError(_ antlr.Recognizer, _ interface{}, line, col int, msg string, _ antlr.RecognitionException) {
	if len(e.msgs) < 5 {
		if len(msg) > 160 {
			msg = msg[:160] + "…"
		}
		e.msgs = append(e.msgs, fmt.Sprintf("%d:%d %s", line, col, msg))
	}
}

// mysqlSyntaxErrors 解析一条语句（不含分号），返回语法错误；空表示语法正确。
func mysqlSyntaxErrors(text string) (errs []string) {
	defer func() {
		if r := recover(); r != nil {
			errs = []string{fmt.Sprintf("解析器内部错误：%v", r)}
		}
	}()
	el := &syntaxErrs{DefaultErrorListener: antlr.NewDefaultErrorListener()}
	lex := mysql.NewMySQLLexer(antlr.NewInputStream(text + ";"))
	lex.RemoveErrorListeners()
	lex.AddErrorListener(el)
	p := mysql.NewMySQLParser(antlr.NewCommonTokenStream(lex, antlr.TokenDefaultChannel))
	p.RemoveErrorListeners()
	p.AddErrorListener(el)
	p.Script()
	return el.msgs
}

// ---- INSERT 语句的最小解析（只支持这两个文件用到的形式） ----

type sqlVal struct {
	S     string // 字符串（已反转义）或原样的非字符串记号（数字、NULL、NOW(6)）
	IsStr bool
}

type insertStmt struct {
	Table      string
	Cols       []string
	Rows       [][]sqlVal
	Ignore     bool   // INSERT IGNORE
	OnDup      string // ON DUPLICATE KEY UPDATE 之后的文本，没有为空
	Line       int
	RawPreview string
}

var insertHead = regexp.MustCompile("(?is)^INSERT\\s+(IGNORE\\s+)?INTO\\s+`?([A-Za-z0-9_]+)`?\\s*\\(([^)]*)\\)\\s*VALUES\\s*(.*)$")

func parseInsert(s stmt) (*insertStmt, error) {
	m := insertHead.FindStringSubmatch(s.Text)
	if m == nil {
		return nil, fmt.Errorf("第 %d 行：不是 INSERT … (列) VALUES … 形式", s.Line)
	}
	ins := &insertStmt{Table: m[2], Ignore: m[1] != "", Line: s.Line, RawPreview: preview(s.Text)}
	for _, c := range strings.Split(m[3], ",") {
		ins.Cols = append(ins.Cols, strings.Trim(strings.TrimSpace(c), "`"))
	}
	rest := m[4]
	if loc := regexp.MustCompile(`(?is)\s+ON\s+DUPLICATE\s+KEY\s+UPDATE\s+`).FindStringIndex(rest); loc != nil {
		ins.OnDup = strings.TrimSpace(rest[loc[1]:])
		rest = rest[:loc[0]]
	}
	// 逐个元组解析
	i := 0
	skipWS := func() {
		for i < len(rest) && isSpace(rest[i]) {
			i++
		}
	}
	for {
		skipWS()
		if i >= len(rest) {
			break
		}
		if rest[i] != '(' {
			return nil, fmt.Errorf("第 %d 行：VALUES 里期望 (，实际 %q", s.Line, rest[i:min(i+10, len(rest))])
		}
		i++
		var row []sqlVal
		for {
			skipWS()
			if i >= len(rest) {
				return nil, fmt.Errorf("第 %d 行：元组没有闭合", s.Line)
			}
			switch c := rest[i]; {
			case c == '\'':
				var b strings.Builder
				i++
				for {
					if i >= len(rest) {
						return nil, fmt.Errorf("第 %d 行：字符串没有闭合", s.Line)
					}
					c := rest[i]
					if c == '\\' && i+1 < len(rest) {
						b.WriteByte(rest[i+1])
						i += 2
						continue
					}
					if c == '\'' {
						if i+1 < len(rest) && rest[i+1] == '\'' {
							b.WriteByte('\'')
							i += 2
							continue
						}
						i++
						break
					}
					b.WriteByte(c)
					i++
				}
				row = append(row, sqlVal{S: b.String(), IsStr: true})
			default:
				j := i
				depth := 0
				for j < len(rest) {
					ch := rest[j]
					if ch == '(' {
						depth++
					} else if ch == ')' {
						if depth == 0 {
							break
						}
						depth--
					} else if ch == ',' && depth == 0 {
						break
					}
					j++
				}
				row = append(row, sqlVal{S: strings.TrimSpace(rest[i:j])})
				i = j
			}
			skipWS()
			if i < len(rest) && rest[i] == ',' {
				i++
				continue
			}
			if i < len(rest) && rest[i] == ')' {
				i++
				break
			}
			return nil, fmt.Errorf("第 %d 行：元组里期望 , 或 )", s.Line)
		}
		if len(row) != len(ins.Cols) {
			return nil, fmt.Errorf("第 %d 行：元组有 %d 个值，列有 %d 个", s.Line, len(row), len(ins.Cols))
		}
		ins.Rows = append(ins.Rows, row)
		skipWS()
		if i < len(rest) && rest[i] == ',' {
			i++
		}
	}
	return ins, nil
}

func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

func (ins *insertStmt) col(row []sqlVal, name string) sqlVal {
	for i, c := range ins.Cols {
		if c == name {
			return row[i]
		}
	}
	panic("列不存在：" + name)
}

func (ins *insertStmt) intCol(t testing.TB, row []sqlVal, name string) int {
	t.Helper()
	v := ins.col(row, name)
	n, err := strconv.Atoi(v.S)
	if err != nil || v.IsStr {
		t.Fatalf("%s.%s 期望整数，实际 %q", ins.Table, name, v.S)
	}
	return n
}

// ---- 解析并返回两个文件里的全部语句 ----

func loadStmts(t testing.TB, rel string) []stmt {
	t.Helper()
	text := readFile(t, rel)
	ss, err := splitSQL(text)
	if err != nil {
		t.Fatalf("%s：%v", rel, err)
	}
	if len(ss) == 0 {
		t.Fatalf("%s 没有任何语句", rel)
	}
	return ss
}

func loadInserts(t testing.TB) []*insertStmt {
	t.Helper()
	var out []*insertStmt
	for _, s := range loadStmts(t, dataFile) {
		if !strings.HasPrefix(strings.ToUpper(s.Text), "INSERT") {
			continue
		}
		ins, err := parseInsert(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ins)
	}
	return out
}

func insertsOf(all []*insertStmt, table string) []*insertStmt {
	var out []*insertStmt
	for _, i := range all {
		if i.Table == table {
			out = append(out, i)
		}
	}
	return out
}
