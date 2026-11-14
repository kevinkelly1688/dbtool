// @author: kevinkelly
// @license: Apache Licence
// @file: readonly.go
// @time: 2026/10/28

package tsql

import (
	"fmt"
	"strings"
)

// 只读查询里一律不允许出现的关键字。
// 整段脚本逐词检查：不能只靠“第一个词是 SELECT”，否则 `select 1\nupdate t set c=1` 会把写语句夹带进去。
// INSERT 不在这张表里，单独判断：只放行写表变量（见 ReadOnlyViolation）。
var readOnlyForbidden = map[string]bool{
	"UPDATE": true, "DELETE": true, "MERGE": true, "TRUNCATE": true, "DROP": true, "CREATE": true, "ALTER": true,
	"EXEC": true, "EXECUTE": true, "SP_EXECUTESQL": true, "GRANT": true, "REVOKE": true, "DENY": true,
	"OPENQUERY": true, "OPENROWSET": true, "OPENDATASOURCE": true, "DBCC": true, "SHUTDOWN": true, "KILL": true,
	"RECONFIGURE": true, "BACKUP": true, "RESTORE": true, "WAITFOR": true, "WHILE": true, "GOTO": true, "CURSOR": true, "USE": true,
	"INTO": true, // SELECT … INTO / OUTPUT … INTO 会建表写数据（只放行紧跟在 INSERT 后面的 INTO，见 ReadOnlyViolation）
	// INSERT … OUTPUT 会把写入的行作为一个结果集返回，而且排在批处理最后那条 SELECT 的结果集前面：在线查询只取第一个结果集，
	// 脱敏又只按 SELECT 列表匹配列名，OUTPUT 子句里改个别名（`OUTPUT inserted.x AS y`）就能绕过脱敏。只读查询用不着它。
	"OUTPUT": true,
	// 事务控制：在线查询用不到；开了事务不提交会一直占着锁和日志（BEGIN 本身要留着，IF … BEGIN … END 要用）
	"TRAN": true, "TRANSACTION": true, "COMMIT": true, "ROLLBACK": true,
	// 写服务端错误日志 / 自定义报错（THROW 另有常量参数的限制，见下）
	"RAISERROR": true,
	// 排他 / 更新锁、表级锁与持有读锁的提示：会阻塞业务；在线查询与导出已经是 READ UNCOMMITTED，用不着。
	// READCOMMITTED / READCOMMITTEDLOCK 会覆盖会话的 READ UNCOMMITTED，改回读时加共享锁，查询反过来会被写事务堵住；
	// TABLOCK 对整张表加共享锁、REPEATABLEREAD 持有读锁到语句结束，写入都要等。NOLOCK / READUNCOMMITTED 不受影响。
	"UPDLOCK": true, "XLOCK": true, "TABLOCKX": true, "HOLDLOCK": true, "SERIALIZABLE": true,
	"TABLOCK": true, "REPEATABLEREAD": true, "READCOMMITTED": true, "READCOMMITTEDLOCK": true,
}

// ReadOnlyViolation 检查一段 T-SQL 是不是“只读查询”：以 SELECT / WITH 开头，或是 DECLARE @变量 / SET @变量 打头、最后 SELECT 出结果的批处理，
// 且全程没有写入、执行、危险语句。返回空串表示通过，否则返回违规原因。字符串、注释、[标识符] 里的同名单词不算。
//
// 唯一放行的写入是往表变量里攒中间结果：`DECLARE @t TABLE (…); INSERT [INTO] @t SELECT … / VALUES (…)`。表变量只活在这个批处理里，
// 不碰库里的数据；写表、#临时表、没用 DECLARE … TABLE 声明的变量、INSERT … EXEC 仍然拒绝（UPDATE / DELETE @t 也不放行）。
// 表变量带出的敏感字段由脱敏的污染传播处理（masking.tableVarInserts）。
func ReadOnlyViolation(sqlText string) string {
	sig := Significant(Tokenize(sqlText))
	if len(sig) == 0 {
		return "SQL 内容为空"
	}
	first := sig[0].Upper()
	switch first {
	case "SELECT", "WITH", "DECLARE", "SET", "(":
	default:
		return fmt.Sprintf("只允许 SELECT / WITH 查询（或 DECLARE @变量 / SET @变量 开头的只读批处理），不允许 %s", strings.ToLower(first))
	}
	tableVars := tableVariables(sig)
	for i, t := range sig {
		if t.Kind != Word {
			continue
		}
		up := strings.ToUpper(t.Text)
		switch {
		case up == "INSERT":
			if !insertsIntoTableVar(sig, i, tableVars) {
				return "INSERT 只能写入 DECLARE @变量 TABLE (…) 声明的表变量，不能写入表或临时表"
			}
			continue
		case up == "INTO" && i > 0 && sig[i-1].Kind == Word && sig[i-1].Upper() == "INSERT":
			continue // 上面已确认这条 INSERT 写的是表变量
		}
		if readOnlyForbidden[up] {
			return fmt.Sprintf("只读查询里不允许出现 %s", strings.ToLower(up))
		}
		switch up {
		case "DECLARE":
			// 只放行 DECLARE @变量（DECLARE x CURSOR 被 CURSOR 关键字拦掉）
			if i+1 >= len(sig) || sig[i+1].Kind != Variable {
				return "只允许 DECLARE @变量"
			}
		case "SET":
			// SET @x = …；SET NOCOUNT ON、SET IMPLICIT_TRANSACTIONS 等有副作用，不放行
			if i+1 >= len(sig) || sig[i+1].Kind != Variable {
				return "只允许 SET @变量"
			}
		case "THROW":
			// THROW 的消息和错误号只能是常量：用变量会把查到的值原样带进错误消息，绕开结果列脱敏
			for j := i + 1; j < len(sig) && j <= i+5; j++ {
				if sig[j].Kind == Punct && sig[j].Text == ";" {
					break
				}
				if sig[j].Kind == Variable {
					return "THROW 的参数只能是常量（错误号、消息文本、状态），不能用变量"
				}
			}
		case "NEXT":
			// NEXT VALUE FOR 会推进序列，是带副作用的 SELECT
			if i+2 < len(sig) && sig[i+1].Upper() == "VALUE" && sig[i+2].Upper() == "FOR" {
				return "不允许 NEXT VALUE FOR（会推进序列）"
			}
		}
	}
	return ""
}

// tableVariables 返回脚本里用 `DECLARE @名字 [AS] TABLE` 声明的表变量（小写名字）。
func tableVariables(sig []Token) map[string]bool {
	vars := map[string]bool{}
	for i := 0; i+2 < len(sig); i++ {
		if sig[i].Kind != Word || sig[i].Upper() != "DECLARE" || sig[i+1].Kind != Variable {
			continue
		}
		j := i + 2
		if sig[j].Kind == Word && sig[j].Upper() == "AS" && j+1 < len(sig) {
			j++
		}
		if sig[j].Kind == Word && sig[j].Upper() == "TABLE" {
			vars[strings.ToLower(sig[i+1].Text)] = true
		}
	}
	return vars
}

// insertsIntoTableVar 判断 sig[i]（INSERT）写的是不是已声明的表变量：INSERT [INTO] @t。
// 写真表、#临时表、没声明的变量，以及 INSERT TOP (n) INTO … 这类带修饰的写法都不算。
func insertsIntoTableVar(sig []Token, i int, tableVars map[string]bool) bool {
	j := i + 1
	if j < len(sig) && sig[j].Kind == Word && sig[j].Upper() == "INTO" {
		j++
	}
	return j < len(sig) && sig[j].Kind == Variable && tableVars[strings.ToLower(sig[j].Text)]
}

// withMainVerb 返回从 sig[i]（WITH）起，括号外第一个 SELECT / INSERT / UPDATE / DELETE / MERGE（大写）；找不到返回空串。
func withMainVerb(sig []Token, i int) string {
	depth := 0
	for _, t := range sig[i+1:] {
		switch {
		case t.Kind == Punct && t.Text == "(":
			depth++
		case t.Kind == Punct && t.Text == ")":
			depth--
		case depth == 0 && t.Kind == Word:
			switch up := t.Upper(); up {
			case "SELECT", "INSERT", "UPDATE", "DELETE", "MERGE":
				return up
			}
		}
	}
	return ""
}

// 新语句的开头词：INSERT … SELECT 的“语句”到这里结束（没写分号的批处理靠它判断）。
var stmtOpeners = map[string]bool{"DECLARE": true, "SET": true, "IF": true, "PRINT": true, "THROW": true, "BEGIN": true, "RETURN": true, "UPDATE": true, "DELETE": true}

// MainQueryStart 返回有效词序列 sig 里“产出结果的那条查询”的起点下标；找不到返回 -1。
// SELECT / WITH 打头的语句就是 0；DECLARE @变量 / SET @变量 / IF … THROW 打头的批处理，取括号外第一个 SELECT 或 WITH
// （`SELECT @a = …` 这种只给变量赋值、不产出结果的 SELECT 跳过；`INSERT INTO @t SELECT …` 和 `WITH … INSERT INTO @t …`
// 是往表变量写数据，不产出结果，它们的 SELECT / WITH 也跳过）。
func MainQueryStart(sig []Token) int {
	if len(sig) == 0 {
		return -1
	}
	switch sig[0].Upper() {
	case "SELECT", "WITH":
		return 0
	}
	depth := 0
	inInsert, insertSelect := false, false // 正在 INSERT 语句里；它自己的 SELECT 已经见过
	for i, t := range sig {
		switch {
		case t.Kind == Punct && t.Text == "(":
			depth++
		case t.Kind == Punct && t.Text == ")":
			if depth > 0 {
				depth--
			}
		case depth == 0 && t.Kind == Punct && t.Text == ";":
			inInsert = false
		case depth == 0 && t.Kind == Word && t.Upper() == "INSERT":
			inInsert, insertSelect = true, false
		case depth == 0 && t.Kind == Word && inInsert && stmtOpeners[t.Upper()]:
			inInsert = false
		case depth == 0 && t.Kind == Word && t.Upper() == "WITH":
			if inInsert || withMainVerb(sig, i) == "INSERT" {
				continue // INSERT 的表提示 / INSERT 前面的 CTE
			}
			return i
		case depth == 0 && t.Kind == Word && t.Upper() == "SELECT":
			if inInsert {
				prev := ""
				if i > 0 && sig[i-1].Kind == Word {
					prev = sig[i-1].Upper()
				}
				// INSERT 自己的 SELECT 和它的 UNION 分支跳过；之后没有分号直接跟的 SELECT 是新语句
				if !insertSelect || prev == "UNION" || prev == "ALL" || prev == "INTERSECT" || prev == "EXCEPT" {
					insertSelect = true
					continue
				}
				inInsert = false
			}
			if i+2 < len(sig) && sig[i+1].Kind == Variable && sig[i+2].Kind == Punct && sig[i+2].Text == "=" {
				continue // SELECT @a = … 只赋值
			}
			return i
		}
	}
	return -1
}

// StripTrailingOrderBy 去掉最外层（括号深度 0）的最后一个 ORDER BY。
// 统计导出行数时会把用户 SQL 包成派生表 SELECT COUNT(*) FROM (…) t，而 SQL Server 禁止派生表里出现不带 TOP/OFFSET/FOR XML 的 ORDER BY（错误 1033）；
// 排序不影响行数。OFFSET/FETCH 依赖 ORDER BY 且会改变行数，这种情况原样返回。
func StripTrailingOrderBy(sqlText string) string {
	toks := Tokenize(sqlText)
	depth, idx := 0, -1
	for i, t := range toks {
		switch {
		case t.Kind == Punct && t.Text == "(":
			depth++
		case t.Kind == Punct && t.Text == ")":
			depth--
		case t.Kind == Word && depth == 0 && strings.EqualFold(t.Text, "ORDER"):
			// 下一个有效词是 BY
			for j := i + 1; j < len(toks); j++ {
				if toks[j].Kind == Space || toks[j].Kind == Comment {
					continue
				}
				if toks[j].Kind == Word && strings.EqualFold(toks[j].Text, "BY") {
					idx = i
				}
				break
			}
		}
	}
	if idx < 0 {
		return sqlText
	}
	tail := strings.ToUpper(StripComments(sqlText[toks[idx].Start:]))
	if strings.Contains(tail, "OFFSET") || strings.Contains(tail, "FETCH") {
		return sqlText
	}
	return strings.TrimRight(strings.TrimSpace(sqlText[:toks[idx].Start]), "; \t\r\n")
}
