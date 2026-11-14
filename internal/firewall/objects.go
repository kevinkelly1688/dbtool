// @author: kevinkelly
// @license: Apache Licence
// @file: objects.go
// @time: 2026/10/28

package firewall

import (
	"strings"

	"dbtool/internal/instext"
	"dbtool/internal/model"
	"dbtool/internal/sqlx"
)

// EnvOf 由实例标签推断环境（prod / test / 空）：标签编码或名称含 prod / 生产 视为生产，含 test / 测试 视为测试。
// 环境只能靠标签推断；没打标签的实例没有环境，只被“全部实例”的规则覆盖。
// 在线查询、SQL 工单、数据导出与管理页用同一个函数，规则的“实例环境”范围才不会各算各的。
func EnvOf(tags []model.InstanceTag) string {
	env := ""
	for _, t := range tags {
		s := strings.ToLower(t.TagCode + " " + t.TagName)
		switch {
		case strings.Contains(s, "prod") || strings.Contains(s, "生产"): // i18n-ignore 与实例标签里的中文名称匹配
			return "prod"
		case strings.Contains(s, "test") || strings.Contains(s, "测试"): // i18n-ignore 与实例标签里的中文名称匹配
			env = "test"
		}
	}
	return env
}

// EnvOfInstance 是实例的有效环境：实例管理里设置的环境（环境字典里的编码）优先，没设置时才由标签推断。
// 在线查询、SQL 工单、数据导出与管理页都用它，规则的“实例环境”范围才不会各算各的。
func EnvOfInstance(id uint, tags []model.InstanceTag) string {
	return instext.InstanceEnvOf(id, EnvOf(tags))
}

// LexicalObjects 用词法扫描取出语句涉及的库表：SELECT 的 FROM / JOIN（sqlx.ExtractTables），再补上
// UPDATE / INSERT INTO / REPLACE INTO / DELETE FROM / ALTER|DROP|TRUNCATE|CREATE TABLE 后面的目标表。
// 拿不准时宁多勿漏。用于没有 goInception 语法树的场景（工单检查结果里没有表信息、测试台、历史回放）。
func LexicalObjects(stmt string) []Object {
	seen := map[Object]bool{}
	var out []Object
	add := func(schema, name string) {
		if i := strings.LastIndex(name, "."); i >= 0 {
			schema, name = name[:i], name[i+1:]
		}
		o := Object{Schema: schema, Table: name}
		if name != "" && !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	for _, r := range sqlx.ExtractTables(stmt) {
		add(r.Schema, r.Name)
	}
	sig := sqlx.Significant(sqlx.Tokenize(stmt))
	ident := func(t sqlx.Token) bool { return t.Kind == sqlx.Word || t.Kind == sqlx.Quoted }
	text := func(t sqlx.Token) string { return strings.Trim(t.Text, "`\"") }
	skip := map[string]bool{"if": true, "not": true, "exists": true, "ignore": true, "low_priority": true, "delayed": true, "high_priority": true}
	for i := 0; i < len(sig); i++ {
		switch sig[i].Lower() {
		case "update", "into", "table":
			j := i + 1
			for j < len(sig) && ident(sig[j]) && skip[sig[j].Lower()] {
				j++
			}
			if j >= len(sig) || !ident(sig[j]) {
				continue
			}
			name := text(sig[j])
			if j+2 < len(sig) && sig[j+1].Kind == sqlx.Punct && sig[j+1].Text == "." && ident(sig[j+2]) {
				add(name, text(sig[j+2]))
			} else {
				add("", name)
			}
		}
	}
	return out
}
