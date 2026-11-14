// @author: kevinkelly
// @license: Apache Licence
// @file: prefix_lint_test.go
// @time: 2026/10/28

package bootstrapsql

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 前缀策略的回归保护（R1）：本系统所有表统一 dbt_ 前缀。001 / 002 / deploy/sql/*.sql 里不得再出现“未加前缀的基础表名”
// 作为 CREATE TABLE / ALTER TABLE / INSERT INTO / REFERENCES / FROM / JOIN / UPDATE / TRUNCATE 的目标，
// 也不得出现在 information_schema 的 table_name = '…' 探测里，以及 COMMENT 里的“旧表名.列”说明里。
//
// “旧表名”= 001 里每张表去掉 dbt_ 前缀后的名字（例外表还原）+ 被排除的表（expectedExcluded）。
// 说明性文字（注释）里出现的旧表名不在检查范围：只查语句里的目标。
// 数据值也不查：notify 的 kind 标签（sql_workflow）、留存键（query_log / audit_log）、通知目标类型（auth_group）等
// 只是恰好和旧表名同形的字符串（R3）。

var (
	legacyTargetFmt = "(?is)\\b(?:FROM|JOIN|INTO|UPDATE|TRUNCATE|REFERENCES|TABLE(?:\\s+IF\\s+(?:NOT\\s+)?EXISTS)?)\\s+`?(%s)`?(?:[^A-Za-z0-9_]|$)"
	tableNameProbe  = regexp.MustCompile("(?is)\\btable_name\\s*(?:=|(?:NOT\\s+)?IN\\s*\\()\\s*'([A-Za-z0-9_]+)'")
	probeListItem   = regexp.MustCompile(`'([A-Za-z0-9_]+)'`)
	probeInList     = regexp.MustCompile("(?is)\\btable_name\\s+(?:NOT\\s+)?IN\\s*\\(([^)]*)\\)")
	commentedStmtRe = regexp.MustCompile(`(?i)^(ALTER|CREATE|DROP|TRUNCATE|INSERT|REPLACE|UPDATE|DELETE|SELECT|SHOW|DESC|DESCRIBE|EXPLAIN)\b`)
)

// stripSQLComments 去掉 -- / # 行注释与 /* */ 块注释，字符串（单 / 双引号）和反引号标识符原样保留
// （PREPARE 用的动态 SQL 就在字符串里，必须检查）。
func stripSQLComments(src string) string {
	var b strings.Builder
	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < n {
				if src[j] == '\\' && c != '`' && j+1 < n {
					j += 2
					continue
				}
				if src[j] == c {
					if j+1 < n && src[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j >= n {
				j = n - 1
			}
			b.WriteString(src[i : j+1])
			i = j + 1
		case c == '#', c == '-' && i+1 < n && src[i+1] == '-' && (i+2 >= n || isSpace(src[i+2])):
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = n
			} else {
				i += 2 + end + 2
			}
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// legacyHits 返回 text 里“未加前缀的旧表名”的出现位置描述。legacy 是旧表名集合。
func legacyHits(text string, legacy map[string]bool) []string {
	names := make([]string, 0, len(legacy))
	for n := range legacy {
		names = append(names, regexp.QuoteMeta(n))
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) }) // 长名字优先
	alt := strings.Join(names, "|")
	var hits []string

	code := stripSQLComments(text)
	re := regexp.MustCompile(fmt.Sprintf(legacyTargetFmt, alt))
	for _, m := range re.FindAllStringSubmatch(code, -1) {
		hits = append(hits, "语句目标 "+m[1]+"：…"+strings.Join(strings.Fields(m[0]), " "))
	}
	for _, m := range tableNameProbe.FindAllStringSubmatch(code, -1) {
		if legacy[m[1]] {
			hits = append(hits, "information_schema 探测 table_name = '"+m[1]+"'")
		}
	}
	for _, m := range probeInList.FindAllStringSubmatch(code, -1) {
		for _, it := range probeListItem.FindAllStringSubmatch(m[1], -1) {
			if legacy[it[1]] {
				hits = append(hits, "information_schema 探测 table_name IN (…'"+it[1]+"'…)")
			}
		}
	}
	// 被注释掉的示例语句（“-- ALTER TABLE x …”“-- SHOW INDEX FROM x”）：以 SQL 动词开头的注释行也按语句目标检查
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "--") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(t, "--"))
		if !commentedStmtRe.MatchString(body) {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			hits = append(hits, "被注释掉的示例语句里的 "+m[1]+"：-- "+strings.Join(strings.Fields(body), " "))
		}
	}
	// COMMENT / 注释里的“旧表名.列”：说明里写的是本系统的表，必须是新名（注释行也检查）
	dotted := regexp.MustCompile("(?:^|[^A-Za-z0-9_.`])`?(" + alt + ")`?\\.`?[A-Za-z_][A-Za-z0-9_]*")
	for _, l := range strings.Split(text, "\n") {
		for _, m := range dotted.FindAllStringSubmatch(l, -1) {
			hits = append(hits, "说明里的 "+m[1]+".列："+strings.Join(strings.Fields(l), " "))
		}
	}
	return hits
}

func TestLegacyLintDetectsAndAllowsCorrectly(t *testing.T) {
	legacy := map[string]bool{"sql_users": true, "auth_group": true, "audit_log": true, "slow_logs": true, "django_content_type": true}
	bad := map[string]string{
		"CREATE TABLE":        "CREATE TABLE IF NOT EXISTS sql_users (id int)",
		"CREATE TABLE 反引号":    "CREATE TABLE `sql_users` (id int)",
		"INSERT INTO":         "INSERT INTO auth_group (id) VALUES (1)",
		"INSERT 反引号":          "INSERT INTO `django_content_type` (`id`) VALUES (1)",
		"FROM":                "SELECT id FROM sql_users WHERE 1",
		"JOIN":                "SELECT 1 FROM dbt_x a JOIN audit_log b ON 1",
		"REFERENCES":          "CONSTRAINT `c` FOREIGN KEY (`a`) REFERENCES `sql_users` (`id`)",
		"字符串里的动态 SQL":         "SET @s := 'INSERT INTO dbt_x (a) SELECT id FROM auth_group WHERE 1'",
		"ALTER TABLE":         "ALTER TABLE slow_logs ADD INDEX i (a)",
		"UPDATE":              "UPDATE sql_users SET a = 1",
		"换行":                  "SELECT 1\n  FROM\n  sql_users",
		"小写":                  "select 1 from sql_users",
		"table_name 探测":       "SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'sql_users'",
		"table_name IN 探测":    "WHERE table_name IN ('dbt_a', 'auth_group')",
		"COMMENT 里的 旧表名.列":    "id INT COMMENT 'sql_users.id'",
		"注释行里的 旧表名.列":         "-- 沿用 sql_users.failed_login_count",
		"被注释掉的 ALTER 示例":      "-- ALTER TABLE slow_logs ADD INDEX idx_inst_start (instance_id, start_time);",
		"被注释掉的 SHOW INDEX 示例": "-- SHOW INDEX FROM slow_logs",
	}
	for name, sql := range bad {
		if len(legacyHits(sql, legacy)) == 0 {
			t.Errorf("%s：应被检出但没有：%s", name, sql)
		}
	}
	good := map[string]string{
		"新名":                    "CREATE TABLE IF NOT EXISTS dbt_sql_users (id int)",
		"新名 INSERT":             "INSERT INTO `dbt_auth_group` (`id`) VALUES (1)",
		"新名 FROM":               "SELECT id FROM dbt_audit_log a JOIN dbt_slow_logs s ON 1",
		"前缀名里含旧名":               "SELECT 1 FROM dbt_auth_group_ext",
		"注释里的旧名描述":              "-- 与 Archery 的 audit_log 结构一一对应\nSELECT 1",
		"被注释掉的示例用新名":            "-- ALTER TABLE dbt_slow_logs ADD INDEX idx_inst_start (instance_id, start_time);",
		"块注释":                   "/* FROM sql_users */ SELECT 1",
		"数据值":                   "INSERT INTO dbt_notify_log (kind) VALUES ('sql_workflow'), ('auth_group')",
		"新名.列":                  "id INT COMMENT 'dbt_sql_users.id'",
		"information_schema 新名": "WHERE table_name = 'dbt_auth_group_ext'",
		"列名同旧表名":                "SELECT audit_log FROM dbt_x",
	}
	for name, sql := range good {
		if hits := legacyHits(sql, legacy); len(hits) != 0 {
			t.Errorf("%s：不应误报：%v\n  %s", name, hits, sql)
		}
	}
}

func TestNoUnprefixedBaseTableNamesInSQLFiles(t *testing.T) {
	root := repoRoot(t)
	legacy := legacySourceNames(tableNames(loadSchema(t)))
	if len(legacy) != 48+len(expectedExcluded) {
		t.Fatalf("旧表名集合有 %d 个，期望 %d：001 的表集合变了？", len(legacy), 48+len(expectedExcluded))
	}
	files := []string{schemaFile, dataFile}
	m, err := filepath.Glob(filepath.Join(root, "deploy", "sql", "*.sql"))
	if err != nil || len(m) < 20 {
		t.Fatalf("找不到 deploy/sql/*.sql：%v", err)
	}
	for _, f := range m {
		rel, _ := filepath.Rel(root, f)
		files = append(files, filepath.ToSlash(rel))
	}
	checked := 0
	for _, rel := range files {
		checked++
		for _, h := range legacyHits(readFile(t, rel), legacy) {
			t.Errorf("%s：未加 %s 前缀的旧表名：%s", rel, tablePrefix, h)
		}
	}
	t.Logf("检查了 %d 个 SQL 文件（001、002、deploy/sql/*.sql），旧表名集合 %d 个（57 张基础表的源名 + 7 张排除表）", checked, len(legacy))
}
