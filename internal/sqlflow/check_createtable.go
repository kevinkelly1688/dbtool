// @author: kevinkelly
// @license: Apache Licence
// @file: check_createtable.go
// @time: 2026/10/31

package sqlflow

import (
	"fmt"
	"strconv"
	"strings"

	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/sqlx"
)

// SourceCreateTable 是“只做语法检查、原生执行”的建表工单的检查来源。
const SourceCreateTable = "create_table"

// nativeCreateTableTypes 这些实例类型的 CREATE TABLE 工单不走 goInception：
// 它们的建表语法（ENGINE=OLAP、DISTRIBUTED BY、PROPERTIES、DBPARTITION BY …）goInception 的解析器认不全，
// 所以检查只做语法检查（不连目标实例），执行用原生方式（直连逐条执行）。
var nativeCreateTableTypes = map[string]bool{"polardb-x": true, "polardbx": true, "selectdb": true, "doris": true, "adb": true}

// nativeCreateTableType 实例类型是不是上面那几类（不看语句内容）。
func nativeCreateTableType(dbType string) bool { return nativeCreateTableTypes[dbType] }

// nativeCreateTable 这批语句是不是“这几类实例上的 CREATE TABLE 工单”：每条都是 CREATE [TEMPORARY|EXTERNAL] TABLE。
// 混了别的语句（ALTER、INSERT …）的工单仍按原来的方式处理。
func nativeCreateTable(dbType, sqlText string) bool {
	if !nativeCreateTableType(dbType) {
		return false
	}
	stmts := sqlx.Split(sqlText)
	if len(stmts) == 0 {
		return false
	}
	for _, st := range stmts {
		if !isCreateTable(st) {
			return false
		}
	}
	return true
}

// isCreateTable 语句是不是 CREATE [TEMPORARY | EXTERNAL] TABLE …。
func isCreateTable(stmt string) bool {
	toks := sqlx.Significant(sqlx.Tokenize(stmt))
	i := 0
	word := func(w string) bool {
		if i < len(toks) && toks[i].Kind == sqlx.Word && strings.EqualFold(toks[i].Text, w) {
			i++
			return true
		}
		return false
	}
	if !word("create") {
		return false
	}
	if !word("temporary") {
		word("external")
	}
	return word("table")
}

const createTableNotice = "该类型实例的 CREATE TABLE 工单只做语法检查（不连接目标实例，不校验库表是否已存在），审批通过后由原生方式执行；不备份、不支持回滚，语句的正确性请自行保证。"

// checkCreateTable 对每条 CREATE TABLE 语句只做语法检查：不连目标实例，也不校验库表是否存在。
func (s *Service) checkCreateTable(stmts []string) *CheckResult {
	res := &CheckResult{Source: SourceCreateTable, Notice: createTableNotice}
	res.FullSQL = strings.Join(stmts, ";\n")
	for i, st := range stmts {
		r := review.Row{ID: review.Flex(i + 1), Stage: "CHECKED", StageStatus: "Audit completed", ErrorMessage: "None", SQL: st, Sequence: "0_0_" + strconv.Itoa(i), BackupDBName: "None"}
		if msg := createTableSyntaxError(st); msg != "" {
			markError(&r, "驳回语法错误", msg)
		}
		res.Rows = append(res.Rows, r)
	}
	res.SyntaxType = SyntaxDDL
	s.applyRules(res)
	return res
}

// createTableSyntaxError 检查一条 CREATE TABLE 语句的结构，返回第一处语法问题（空串 = 没问题）。
// 只看结构，不认具体的表选项，所以 MySQL / Doris / SelectDB / PolarDB-X / ADB 各自的扩展写法（ENGINE=OLAP、DISTRIBUTED BY HASH(..) BUCKETS n、
// PROPERTIES (...)、DBPARTITION BY ... 等）都能通过：
//   - 语句头：CREATE [TEMPORARY | EXTERNAL] TABLE [IF NOT EXISTS] 表名（可带库名）；
//   - 引号、括号成对；
//   - 表名后面是列定义 ( ... )、LIKE 其他表，或 [表选项] AS SELECT …；
//   - 列定义里每一项都不为空（没有多余 / 缺少的逗号），列要有名字和类型，约束 / 索引项要有内容，至少有一列。
func createTableSyntaxError(stmt string) string {
	toks := sqlx.Significant(sqlx.Tokenize(stmt))

	for _, t := range toks {
		switch t.Kind {
		case sqlx.String:
			if len(t.Text) < 2 || t.Text[len(t.Text)-1] != '\'' {
				return "字符串缺少结束的单引号"
			}
		case sqlx.Quoted:
			if q := t.Text[0]; len(t.Text) < 2 || t.Text[len(t.Text)-1] != q {
				return fmt.Sprintf("标识符缺少结束的引号 %c", q)
			}
		}
	}
	if depth, bad := parenDepth(toks); bad {
		return "括号不匹配：多了一个右括号"
	} else if depth > 0 {
		return "括号不匹配：缺少右括号"
	}

	i := 0
	isWord := func(w string) bool {
		return i < len(toks) && toks[i].Kind == sqlx.Word && strings.EqualFold(toks[i].Text, w)
	}
	eat := func(w string) bool {
		if isWord(w) {
			i++
			return true
		}
		return false
	}
	if !eat("create") {
		return "语句必须以 CREATE TABLE 开头"
	}
	if !eat("temporary") {
		eat("external")
	}
	if !eat("table") {
		return "CREATE 后面缺少 TABLE"
	}
	if eat("if") {
		if !eat("not") || !eat("exists") {
			return "IF NOT EXISTS 写得不完整"
		}
	}
	// 表名：名字 [. 名字]
	name := func() bool {
		if i < len(toks) && (toks[i].Kind == sqlx.Word || toks[i].Kind == sqlx.Quoted) {
			i++
			return true
		}
		return false
	}
	if !name() {
		return "缺少表名"
	}
	for i < len(toks) && toks[i].Kind == sqlx.Punct && toks[i].Text == "." {
		i++
		if !name() {
			return "表名里的“.”后面缺少名字"
		}
	}
	if i >= len(toks) {
		return "表名后面缺少列定义 ( ... )"
	}

	switch {
	case isWord("like"):
		i++
		if !name() {
			return "LIKE 后面缺少表名"
		}
		for i < len(toks) && toks[i].Kind == sqlx.Punct && toks[i].Text == "." {
			i++
			if !name() {
				return "表名里的“.”后面缺少名字"
			}
		}
		if i < len(toks) {
			return fmt.Sprintf("LIKE 其他表之后不能再写别的内容：%s", snippet(stmt, toks[i]))
		}
		return ""
	case toks[i].Kind == sqlx.Punct && toks[i].Text == "(":
		end := matchParen(toks, i)
		if msg := columnListError(toks[i+1 : end]); msg != "" {
			return msg
		}
		return ""
	}
	// 没有列定义：只允许 CREATE TABLE 表名 [表选项] AS SELECT …（建表同时导入数据）
	depth := 0
	for _, t := range toks[i:] {
		switch {
		case t.Kind == sqlx.Punct && t.Text == "(":
			depth++
		case t.Kind == sqlx.Punct && t.Text == ")":
			depth--
		case depth == 0 && t.Kind == sqlx.Word && (strings.EqualFold(t.Text, "as") || strings.EqualFold(t.Text, "select")):
			return ""
		}
	}
	return fmt.Sprintf("表名后面应是列定义 ( ... )、LIKE 其他表，或 AS SELECT …：%s", snippet(stmt, toks[i]))
}

// parenDepth 返回括号的最终深度；bad = 中途出现过多余的右括号。
func parenDepth(toks []sqlx.Token) (depth int, bad bool) {
	for _, t := range toks {
		if t.Kind != sqlx.Punct {
			continue
		}
		switch t.Text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return depth, true
			}
		}
	}
	return depth, false
}

// matchParen 返回与 toks[open] 这个左括号配对的右括号的下标（调用前已确认括号成对）。
func matchParen(toks []sqlx.Token, open int) int {
	depth := 0
	for j := open; j < len(toks); j++ {
		if toks[j].Kind != sqlx.Punct {
			continue
		}
		switch toks[j].Text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return len(toks) - 1
}

// constraintStart 列定义里这些词开头的是约束 / 索引项，不是列。
var constraintStart = map[string]bool{"primary": true, "unique": true, "key": true, "index": true, "constraint": true, "foreign": true,
	"check": true, "fulltext": true, "spatial": true, "projection": true}

// columnListError 检查括号里的列定义列表（不含最外层括号）。
func columnListError(inner []sqlx.Token) string {
	if len(inner) == 0 {
		return "列定义是空的，至少要有一列"
	}
	var items [][]sqlx.Token
	depth, start := 0, 0
	for j, t := range inner {
		if t.Kind != sqlx.Punct {
			continue
		}
		switch t.Text {
		case "(":
			depth++
		case ")":
			depth--
		case ",":
			if depth == 0 {
				items = append(items, inner[start:j])
				start = j + 1
			}
		}
	}
	items = append(items, inner[start:])
	for n, it := range items {
		if len(it) == 0 {
			if n == len(items)-1 {
				return "列定义最后多了一个逗号"
			}
			return "列定义里有多余的逗号（两个逗号之间是空的）"
		}
		first := it[0]
		if first.Kind != sqlx.Word && first.Kind != sqlx.Quoted {
			return fmt.Sprintf("列定义必须以列名开头：%s", first.Text)
		}
		if first.Kind == sqlx.Word && constraintStart[strings.ToLower(first.Text)] {
			if len(it) < 2 {
				return fmt.Sprintf("约束 / 索引定义不完整：%s", first.Text)
			}
			continue
		}
		if len(it) < 2 {
			return fmt.Sprintf("列 %s 缺少数据类型", strings.Trim(first.Text, "`\""))
		}
		if it[1].Kind != sqlx.Word {
			return fmt.Sprintf("列 %s 的数据类型写法不对：%s", strings.Trim(first.Text, "`\""), it[1].Text)
		}
	}
	return ""
}

// snippet 取出错位置附近的一小段原文，方便定位。
func snippet(stmt string, t sqlx.Token) string {
	s := stmt[t.Start:]
	if r := []rune(s); len(r) > 24 {
		s = string(r[:24]) + "…"
	}
	return s
}

// createTableSQL 取工单的 SQL 正文，只在目标实例属于 nativeCreateTableTypes 时才去读（其他工单用不到，不多一次查询）。
func (s *Service) createTableSQL(r Repo, w *model.SqlWorkflow) string {
	if w.Instance == nil || !nativeCreateTableType(w.Instance.DBType) {
		return ""
	}
	if c, err := r.GetContent(w.ID); err == nil && c != nil {
		return c.SQLContent
	}
	return ""
}
