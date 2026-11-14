// @author: kevinkelly
// @license: Apache Licence
// @file: sqlprep.go
// @time: 2026/10/28

package export

import (
	"errors"
	"fmt"
	"strings"

	"dbtool/internal/sqlx"
	"dbtool/internal/tsql"
)

// Prepared 是校验并整理后的导出查询。
type Prepared struct {
	SQL   string // 要执行的查询
	Batch bool   // 是 T-SQL 的 DECLARE/SET @ 批处理：无法包成派生表统计行数，行数上限只能在执行时兜底
}

// Prepare 校验并整理导出用的 SQL，按实例类型区分。导出只允许一条只读查询：
//   - MySQL 系：必须是单条 SELECT / WITH；不允许 INTO OUTFILE/DUMPFILE、加锁读取、读服务器文件的函数、查 mysql.user；
//   - SQL Server：SELECT / WITH，或 DECLARE @ / SET @ 开头的只读批处理（见 tsql.ReadOnlyViolation）；GO 行会被去掉。
//
// 多条语句直接报错，避免用户以为都导出了。
func Prepare(dbType, sqlText string) (*Prepared, error) {
	if strings.TrimSpace(sqlText) == "" {
		return nil, errors.New("SQL 内容不能为空")
	}
	if dbType == "mssql" {
		units := tsql.Units(sqlText)
		if len(units) == 0 {
			return nil, errors.New("SQL 内容不能为空")
		}
		// 先逐个单元查只读（原因比“语句太多”更有用），再要求只有一个单元
		for _, u := range units {
			if why := tsql.ReadOnlyViolation(u.SQL); why != "" {
				return nil, errors.New(why)
			}
		}
		if len(units) > 1 {
			return nil, fmt.Errorf("导出只支持一条查询语句（或 DECLARE @ / SET @ 开头的只读批处理），检测到 %d 个语句", len(units))
		}
		stmt := strings.TrimSpace(tsql.StripComments(units[0].SQL))
		batch := false
		switch firstWord(stmt) {
		case "declare", "set":
			batch = true
		}
		return &Prepared{SQL: strings.TrimRight(stmt, "; \t\r\n"), Batch: batch}, nil
	}
	stmts := sqlx.Split(sqlText)
	switch len(stmts) {
	case 0:
		return nil, errors.New("没有有效的 SQL 语句")
	case 1:
	default:
		return nil, fmt.Errorf("导出只支持一条查询语句，检测到 %d 条", len(stmts))
	}
	stmt := stmts[0]
	switch kw := sqlx.FirstKeyword(stmt); kw {
	case "select", "with":
	default:
		return nil, errors.New("违规语句：数据导出只允许 SELECT / WITH 查询")
	}
	if sqlx.ContainsSequence(stmt, "into", "outfile") || sqlx.ContainsSequence(stmt, "into", "dumpfile") ||
		sqlx.ContainsSequence(stmt, "for", "update") || sqlx.ContainsSequence(stmt, "lock", "in", "share", "mode") {
		return nil, errors.New("数据导出不允许加锁读取或写出文件")
	}
	if fns := sqlx.DangerousFunctions(stmt); len(fns) > 0 {
		return nil, fmt.Errorf("数据导出不允许使用函数：%s", strings.Join(fns, "、"))
	}
	if sqlx.TouchesMySQLUser(stmt, "") {
		return nil, errors.New("不允许查询 mysql.user 表")
	}
	return &Prepared{SQL: stmt}, nil
}

func firstWord(s string) string {
	for _, t := range tsql.Significant(tsql.Tokenize(s)) {
		return strings.ToLower(t.Text)
	}
	return ""
}

// CountSQL 生成统计行数的语句：把查询包成派生表。SQL Server 要先去掉最外层 ORDER BY。批处理没有这种语句（返回空串）。
func (p *Prepared) CountSQL(dbType string) string {
	if p.Batch {
		return ""
	}
	q := p.SQL
	if dbType == "mssql" {
		q = tsql.StripTrailingOrderBy(q)
	}
	return "SELECT COUNT(*) FROM (" + strings.TrimRight(q, "; \t\r\n") + ") t"
}

// ProbeSQL 生成只取列名的语句（合并导出提交前核对各实例的列是否一致）：把查询包成派生表后一行都不取。批处理没有这种语句（返回空串）。
func (p *Prepared) ProbeSQL(dbType string) string {
	if p.Batch {
		return ""
	}
	q := p.SQL
	if dbType == "mssql" {
		return "SELECT TOP 0 * FROM (" + strings.TrimRight(tsql.StripTrailingOrderBy(q), "; \t\r\n") + ") t"
	}
	return "SELECT * FROM (" + strings.TrimRight(q, "; \t\r\n") + ") t LIMIT 0"
}
