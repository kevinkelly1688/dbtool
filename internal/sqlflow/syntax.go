// @author: kevinkelly
// @license: Apache Licence
// @file: syntax.go
// @time: 2026/10/28

// Package sqlflow 实现“SQL 上线工单”：提交与检查、审批、立即/定时/手工执行、终止、回滚语句、在线改表进度控制。
// 审批流程复用通用审批引擎 internal/audit，检查与执行优先交给 goInception，没有时降级为原生检查/执行。
package sqlflow

import (
	"regexp"
	"strings"

	"dbtool/internal/sqlx"
)

// 语句类型，与 dbt_sql_workflow.syntax_type 的取值一致。
const (
	SyntaxOther  = 0
	SyntaxDDL    = 1
	SyntaxDML    = 2
	SyntaxExport = 3 // 离线导出
)

var (
	ddlRe = regexp.MustCompile(`(?i)^(alter|create|drop|rename|truncate|delete)\b`)
	dmlRe = regexp.MustCompile(`(?i)^(call|do|handler|insert|load\s+data|load\s+xml|replace|select|update)\b`)
)

// StatementType 判断单条语句是 DDL 还是 DML：
// 注释先去掉；DELETE 强制归为 DDL（本站要求删数据走 DDL 上线流程）。
func StatementType(stmt string) int {
	s := strings.TrimSpace(sqlx.StripComments(stmt))
	switch {
	case ddlRe.MatchString(s):
		return SyntaxDDL
	case dmlRe.MatchString(s):
		return SyntaxDML
	}
	return SyntaxOther
}

// SetSyntaxType 按一组语句定下工单类型：只要有一条 DDL 就是 DDL，否则 DML。
func SetSyntaxType(stmts []string) int {
	t := SyntaxDML
	for _, s := range stmts {
		if StatementType(s) == SyntaxDDL {
			return SyntaxDDL
		}
	}
	return t
}

// SyntaxLabel 是类型的显示名。
func SyntaxLabel(t int) string {
	switch t {
	case SyntaxDDL:
		return "DDL"
	case SyntaxDML:
		return "DML"
	case 3:
		return "离线导出"
	}
	return "其他"
}
