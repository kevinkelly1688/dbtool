// @author: kevinkelly
// @license: Apache Licence
// @file: mssql.go
// @time: 2026/10/28

package query

import (
	"fmt"
	"strconv"
	"strings"

	"dbtool/internal/sqlx"
	"dbtool/internal/tsql"
)

// SQL Server 的在线查询：语句检查、库表解析、TOP n 改写。
//
// 只放行单条 SELECT / WITH 查询，或 DECLARE @变量 / SET @变量 打头、最后用 SELECT 输出结果的只读批处理
// （tsql.ReadOnlyViolation 逐词排除写入、EXEC、事务控制、OPENROWSET、SELECT INTO 等）；
// 权限按库校验（SQL Server 没有表级查询授权）；行数上限用 TOP n 改写，同时由引擎在客户端兜底截断。

const dbTypeMSSQL = "mssql"

func isMSSQL(dbType string) bool { return dbType == dbTypeMSSQL }

// checkMSSQL 是 Check 的 T-SQL 版本。
func (s *Service) checkMSSQL(sql string, pol Policy) (stmt, notice string, err error) {
	units := tsql.Units(sql)
	var texts []string
	for _, u := range units {
		if t := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(tsql.StripComments(u.SQL)), ";")); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		return "", "", newErr(400, "bad_query", "没有有效的SQL语句")
	}
	stmt = texts[0]
	if len(texts) > 1 {
		notice = fmt.Sprintf("检测到 %d 条语句，本次只执行第 1 条；可选中某条语句后再运行。", len(texts))
	}
	sig := tsql.Significant(tsql.Tokenize(stmt))
	kw := ""
	if len(sig) > 0 {
		kw = strings.ToLower(sig[0].Text)
	}
	batch := kw == "declare" || kw == "set"
	if kw != "select" && kw != "with" && !batch {
		return "", "", newErr(400, "bad_query", "SQL Server 在线查询仅支持 SELECT / WITH 查询（或 DECLARE @变量 / SET @变量 打头、最后用 SELECT 输出结果的只读批处理）；数据变更请提交 SQL 工单。")
	}
	if why := tsql.ReadOnlyViolation(stmt); why != "" {
		return "", "", newErr(400, "bad_query", fmt.Sprintf("不允许的 T-SQL：%s", why))
	}
	if batch && tsql.MainQueryStart(sig) < 0 {
		return "", "", newErr(400, "bad_query", "这段批处理没有输出结果的 SELECT：DECLARE / SET 之后需要一条 SELECT（或 WITH … SELECT）查询。")
	}
	if kw == "with" && mssqlMainKeyword(sig) != "select" {
		return "", "", newErr(400, "bad_query", "WITH 语句的主语句必须是 SELECT，只允许纯查询")
	}
	if _, linked := mssqlTables(stmt, ""); linked {
		return "", "", newErr(400, "bad_query", "不允许通过链接服务器（四部分名称）查询")
	}
	if pol.DisableStar && mssqlHasStar(sig) {
		return "", "", &Error{Code: "star_forbidden", HTTP: 400,
			Message: "系统已禁用 SELECT *，请列出需要的字段。在左侧点击表名可自动展开字段。"}
	}
	return stmt, notice, nil
}

func isIdentTok(t tsql.Token) bool {
	return t.Kind == tsql.Ident || (t.Kind == tsql.Word && !strings.HasPrefix(t.Text, "@"))
}

// mssqlMainKeyword 返回 WITH 语句的主语句关键字（小写），无法识别返回空串。
func mssqlMainKeyword(sig []tsql.Token) string {
	depth := 0
	for _, t := range sig[1:] {
		switch {
		case t.Kind == tsql.Punct && t.Text == "(":
			depth++
		case t.Kind == tsql.Punct && t.Text == ")":
			depth--
		case depth == 0 && t.Kind == tsql.Word:
			switch lw := strings.ToLower(t.Text); lw {
			case "select", "insert", "update", "delete", "merge":
				return lw
			}
		}
	}
	return ""
}

// mssqlHasStar 判断是否有 `SELECT *`、`t.*`、`, *` 形式的通配列（COUNT(*)、a * b 不算）。
func mssqlHasStar(sig []tsql.Token) bool {
	for i, t := range sig {
		if t.Kind != tsql.Punct || t.Text != "*" || i == 0 {
			continue
		}
		p := sig[i-1]
		switch {
		case p.Kind == tsql.Punct && (p.Text == "," || p.Text == "."):
			return true
		case p.Kind == tsql.Word:
			switch strings.ToLower(p.Text) {
			case "select", "distinct", "all", "percent", "ties":
				return true
			}
		case p.Kind == tsql.Number || (p.Kind == tsql.Punct && p.Text == ")"):
			// TOP n * / TOP (n) *：往前几个词里有 TOP
			for j := i - 2; j >= 0 && j >= i-5; j-- {
				if sig[j].Kind == tsql.Word && strings.EqualFold(sig[j].Text, "top") {
					return true
				}
			}
		}
	}
	return false
}

// mssqlTables 提取语句引用的 库.表。一部分/两部分名称（表、架构.表）属于当前库 db；三部分 库.架构.表 取其库名；
// 四部分（链接服务器）返回 linked=true。临时表（#t）、表变量（@t）不算。词法级、宁可多报。
func mssqlTables(stmt, db string) (refs []sqlx.TableRef, linked bool) {
	sig := tsql.Significant(tsql.Tokenize(stmt))
	seen := map[sqlx.TableRef]bool{}
	type level struct{ inFrom, expect bool }
	stack := []level{{}}
	top := func() *level { return &stack[len(stack)-1] }
	terminators := map[string]bool{"where": true, "group": true, "having": true, "order": true, "union": true, "intersect": true,
		"except": true, "option": true, "for": true, "offset": true, "select": true, "into": true, "window": true}
	for i := 0; i < len(sig); i++ {
		t := sig[i]
		lw := strings.ToLower(t.Text)
		switch {
		case t.Kind == tsql.Punct && t.Text == "(":
			derived := false
			if top().expect && i+1 < len(sig) {
				n := strings.ToLower(sig[i+1].Text)
				derived = n == "select" || n == "with" || n == "values"
			}
			nested := top().expect && !derived
			top().expect = false
			stack = append(stack, level{inFrom: nested, expect: nested})
		case t.Kind == tsql.Punct && t.Text == ")":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case t.Kind == tsql.Punct && t.Text == ",":
			if top().inFrom {
				top().expect = true
			}
		case t.Kind == tsql.Word && (lw == "from" || lw == "join" || lw == "apply"):
			top().inFrom, top().expect = true, true
		case t.Kind == tsql.Word && top().inFrom && terminators[lw]:
			top().inFrom, top().expect = false, false
		case top().expect && isIdentTok(t):
			parts := []string{tsql.Unquote(t.Text)}
			j := i
			for j+2 < len(sig) && sig[j+1].Kind == tsql.Punct && sig[j+1].Text == "." {
				if isIdentTok(sig[j+2]) {
					parts = append(parts, tsql.Unquote(sig[j+2].Text))
					j += 2
				} else if sig[j+2].Kind == tsql.Punct && sig[j+2].Text == "." { // a..b（省略架构）
					parts = append(parts, "")
					j++
				} else {
					break
				}
			}
			i = j
			top().expect = false
			name := parts[len(parts)-1]
			if name == "" || strings.HasPrefix(name, "#") || strings.HasPrefix(name, "@") {
				continue
			}
			ref := sqlx.TableRef{Schema: db, Name: name}
			switch {
			case len(parts) >= 4:
				linked = true
				continue
			case len(parts) == 3:
				ref.Schema = parts[0]
			}
			if !seen[ref] {
				seen[ref] = true
				refs = append(refs, ref)
			}
		}
	}
	return refs, linked
}

// mssqlRewriteTop 给 SELECT 施加最大行数：没有 TOP 时在 SELECT [DISTINCT|ALL] 后插入 TOP max；已有 TOP n 且 n>max 时改成 max。
// DECLARE / SET 打头的批处理改写其中产出结果的那条查询（括号外第一个不是 `SELECT @a = …` 的 SELECT / WITH）。
// 含 UNION/INTERSECT/EXCEPT、OFFSET…FETCH、TOP … PERCENT、TOP (表达式) 的语句不改写（由引擎在客户端兜底截断到 max 行）。
func mssqlRewriteTop(stmt string, max int) string {
	stmt = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(stmt), ";"))
	if max <= 0 {
		return stmt
	}
	sig := tsql.Significant(tsql.Tokenize(stmt))
	if len(sig) == 0 {
		return stmt
	}
	// DECLARE / SET 打头的批处理：改写的是产出结果的那条查询，不是第一个词
	base := tsql.MainQueryStart(sig)
	if base < 0 {
		return stmt
	}
	k := base
	if strings.EqualFold(sig[base].Text, "with") {
		depth := 0
		k = -1
		for i := base + 1; i < len(sig); i++ {
			t := sig[i]
			switch {
			case t.Kind == tsql.Punct && t.Text == "(":
				depth++
			case t.Kind == tsql.Punct && t.Text == ")":
				depth--
			case depth == 0 && t.Kind == tsql.Word && strings.EqualFold(t.Text, "select"):
				k = i
			}
			if k >= 0 {
				break
			}
		}
		if k < 0 {
			return stmt
		}
	}
	depth := 0
	for i := k; i < len(sig); i++ {
		t := sig[i]
		switch {
		case t.Kind == tsql.Punct && t.Text == "(":
			depth++
		case t.Kind == tsql.Punct && t.Text == ")":
			depth--
		case depth == 0 && t.Kind == tsql.Word:
			switch strings.ToLower(t.Text) {
			case "union", "intersect", "except", "offset":
				return stmt
			}
		}
	}
	end := func(t tsql.Token) int { return t.Start + len(t.Text) }
	pos := k + 1
	for pos < len(sig) && sig[pos].Kind == tsql.Word && (strings.EqualFold(sig[pos].Text, "distinct") || strings.EqualFold(sig[pos].Text, "all")) {
		pos++
	}
	if pos < len(sig) && strings.EqualFold(sig[pos].Text, "top") {
		// TOP n / TOP (n) [PERCENT] [WITH TIES]
		a := pos + 1
		paren := a < len(sig) && sig[a].Text == "("
		if paren {
			a++
		}
		if a >= len(sig) || sig[a].Kind != tsql.Number {
			return stmt
		}
		after := a + 1
		if paren {
			if after >= len(sig) || sig[after].Text != ")" {
				return stmt
			}
			after++
		}
		if after < len(sig) && strings.EqualFold(sig[after].Text, "percent") {
			return stmt
		}
		n, err := strconv.Atoi(sig[a].Text)
		if err != nil || n > max {
			return stmt[:sig[a].Start] + strconv.Itoa(max) + stmt[end(sig[a]):]
		}
		return stmt
	}
	at := end(sig[pos-1])
	return stmt[:at] + " TOP " + strconv.Itoa(max) + stmt[at:]
}
