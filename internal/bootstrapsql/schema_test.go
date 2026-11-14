// @author: kevinkelly
// @license: Apache Licence
// @file: schema_test.go
// @time: 2026/10/28

package bootstrapsql

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// expectedExcluded 是 001 排除的表（与 docs/bootstrap-design.md 的“排除表”
// 一致）。改这里等于改取舍：必须同时改设计文档。
// 它们用的是未加前缀的旧名：这些表在本系统里根本不建，所以没有对应的 dbt_ 新名。
var expectedExcluded = []string{
	"aliyun_rds_config",
	"archive_config",
	"archive_log",
	"cloud_access_key",
	"data_masking_columns",
	"data_masking_rules",
	"django_admin_log",
	"django_cas_ng_proxygrantingticket",
	"django_cas_ng_sessionticket",
	"django_migrations",
	"django_q_ormq",
	"django_q_schedule",
	"django_q_task",
	"django_session",
	"sql_config",
	"sql_permission",
}

type tableDef struct {
	Name string
	Body string
	Opts string
	Line int
	FKs  []string // 引用的其它表（去重、不含自身）
}

var (
	createRe = regexp.MustCompile("(?s)^CREATE TABLE IF NOT EXISTS `([^`]+)` \\(\n(.*)\n\\) (ENGINE=[^\n]*)$")
	fkRefRe  = regexp.MustCompile("REFERENCES `([^`]+)`")
)

func loadSchema(t testing.TB) []tableDef {
	t.Helper()
	var out []tableDef
	for _, s := range loadStmts(t, schemaFile) {
		if !strings.HasPrefix(s.Text, "CREATE") {
			continue
		}
		m := createRe.FindStringSubmatch(s.Text)
		if m == nil {
			t.Fatalf("%s 第 %d 行：CREATE 语句不是“CREATE TABLE IF NOT EXISTS `表` (…) ENGINE=…”的形式：%s", schemaFile, s.Line, preview(s.Text))
		}
		d := tableDef{Name: m[1], Body: m[2], Opts: m[3], Line: s.Line}
		seen := map[string]bool{}
		for _, r := range fkRefRe.FindAllStringSubmatch(m[2], -1) {
			if r[1] != d.Name && !seen[r[1]] {
				seen[r[1]] = true
				d.FKs = append(d.FKs, r[1])
			}
		}
		sort.Strings(d.FKs)
		out = append(out, d)
	}
	return out
}

func tableNames(defs []tableDef) []string {
	var n []string
	for _, d := range defs {
		n = append(n, d.Name)
	}
	return n
}

func setOf(l []string) map[string]bool {
	m := make(map[string]bool, len(l))
	for _, s := range l {
		m[s] = true
	}
	return m
}

// ---- 文件形态 ----

func TestFilesAreLFAndHaveProvenanceHeader(t *testing.T) {
	for _, f := range []string{schemaFile, dataFile} {
		text := readFile(t, f)
		if strings.Contains(text, "\r") {
			t.Errorf("%s 含 CR：必须是 LF 行尾", f)
		}
		if strings.HasPrefix(text, "\ufeff") {
			t.Errorf("%s 带 BOM", f)
		}
		if !strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\n\n") {
			t.Errorf("%s 应恰好以一个换行结尾", f)
		}
		head := text
		if i := strings.Index(text, "\n\n"); i > 0 {
			head = text[:i]
		}
		for _, want := range []string{"生成日期：", "空库"} {
			if !strings.Contains(head, want) {
				t.Errorf("%s 文件头缺少 %q", f, want)
			}
		}
	}
}

// ---- 语法 ----

func TestEveryStatementParsesWithMySQLGrammar(t *testing.T) {
	for _, f := range []string{schemaFile, dataFile} {
		ss := loadStmts(t, f)
		for _, s := range ss {
			if errs := mysqlSyntaxErrors(s.Text); len(errs) > 0 {
				t.Errorf("%s 第 %d 行语法错误：%s\n  语句：%s", f, s.Line, strings.Join(errs, "；"), preview(s.Text))
			}
		}
		t.Logf("%s：%d 条语句全部通过 MySQL 语法解析", f, len(ss))
	}
}

func TestStatementKindsAreWhitelisted(t *testing.T) {
	setStmt := regexp.MustCompile(`(?i)^SET\s+(NAMES\s+utf8mb4|FOREIGN_KEY_CHECKS\s*=\s*[01])$`)
	for _, s := range loadStmts(t, schemaFile) {
		if setStmt.MatchString(s.Text) || strings.HasPrefix(s.Text, "CREATE TABLE IF NOT EXISTS ") {
			continue
		}
		t.Errorf("%s 第 %d 行：001 里只允许 SET NAMES / SET FOREIGN_KEY_CHECKS 与 CREATE TABLE IF NOT EXISTS：%s", schemaFile, s.Line, preview(s.Text))
	}
	for _, s := range loadStmts(t, dataFile) {
		if setStmt.MatchString(s.Text) || strings.HasPrefix(s.Text, "INSERT ") {
			continue
		}
		t.Errorf("%s 第 %d 行：002 里只允许 SET NAMES 与 INSERT：%s", dataFile, s.Line, preview(s.Text))
	}
}

// ---- 001 的结构约束 ----

func TestSchemaShape(t *testing.T) {
	// 只检查语句本身（文件头注释里会提到被去掉的 DROP / AUTO_INCREMENT 等字样）
	for _, bad := range []struct{ re, why string }{
		{`(?i)\bDROP\s+(TABLE|DATABASE|VIEW|TRIGGER)`, "不允许 DROP"},
		{`(?i)AUTO_INCREMENT\s*=\s*\d+`, "不允许表选项 AUTO_INCREMENT=<数字>"},
		{`(?i)\bDEFINER\b`, "不允许 DEFINER"},
		{`/\*!`, "不允许 mysqldump 的版本注释 /*! … */"},
		{`(?i)\b(CREATE\s+(OR\s+REPLACE\s+)?(VIEW|TRIGGER|PROCEDURE|FUNCTION|EVENT)|TRUNCATE|DELETE\s+FROM|INSERT\s+INTO)\b`, "001 只含建表语句"},
	} {
		re := regexp.MustCompile(bad.re)
		for _, s := range loadStmts(t, schemaFile) {
			if re.MatchString(s.Text) {
				t.Errorf("%s 第 %d 行：%s", schemaFile, s.Line, bad.why)
			}
		}
	}
	text := readFile(t, schemaFile)
	// 每个 CREATE TABLE 都带 IF NOT EXISTS（loadSchema 已要求形式；这里再数一遍总数）
	if n, m := len(regexp.MustCompile(`(?m)^CREATE TABLE `).FindAllString(text, -1)),
		len(regexp.MustCompile("(?m)^CREATE TABLE IF NOT EXISTS `").FindAllString(text, -1)); n != m || n == 0 {
		t.Errorf("CREATE TABLE %d 条，其中带 IF NOT EXISTS 的 %d 条", n, m)
	}
	// 表选项：只允许 InnoDB，可带 COMMENT；字符集 / 校验规则一律不指定（docs/charset-policy.md；charset_lint_test.go 用语法树再查一遍）
	optRe := regexp.MustCompile(`^ENGINE=InnoDB( COMMENT='[^']*')?$`)
	for _, d := range loadSchema(t) {
		if !optRe.MatchString(d.Opts) {
			t.Errorf("表 %s 的表选项不在预期范围内：%s", d.Name, d.Opts)
		}
		if !strings.Contains(d.Body, "PRIMARY KEY") {
			t.Errorf("表 %s 没有主键", d.Name)
		}
	}
}

func TestSchemaTablesAreUniqueAndForeignKeyOrdered(t *testing.T) {
	defs := loadSchema(t)
	if len(defs) < 40 {
		t.Fatalf("表太少：%d", len(defs))
	}
	pos := map[string]int{}
	for i, d := range defs {
		if _, dup := pos[d.Name]; dup {
			t.Errorf("表 %s 定义了两次", d.Name)
		}
		pos[d.Name] = i
	}
	for i, d := range defs {
		for _, ref := range d.FKs {
			j, ok := pos[ref]
			switch {
			case !ok:
				t.Errorf("表 %s 的外键引用了文件里没有的表 %s", d.Name, ref)
			case j > i:
				t.Errorf("表 %s（第 %d 张）引用的 %s 排在它后面（第 %d 张）：被引用的表必须先建", d.Name, i+1, ref, j+1)
			}
		}
	}
	// 外键约束名唯一（MySQL 要求同一库内唯一）、带 dbt_ 前缀、不超过 64 个字符（R2）
	cons := map[string]string{}
	for _, d := range defs {
		for _, m := range fkNameRe.FindAllStringSubmatch(d.Body, -1) {
			if prev, ok := cons[m[1]]; ok {
				t.Errorf("外键约束名 %s 在 %s 与 %s 里重复", m[1], prev, d.Name)
			}
			cons[m[1]] = d.Name
			if !strings.HasPrefix(m[1], tablePrefix) {
				t.Errorf("表 %s 的外键约束名 %s 没有 %s 前缀（同库安装时会与 Archery 的同名约束冲突）", d.Name, m[1], tablePrefix)
			}
			if len(m[1]) > identMax {
				t.Errorf("表 %s 的外键约束名 %s 长度 %d，超过 %d", d.Name, m[1], len(m[1]), identMax)
			}
		}
	}
	if len(cons) < 40 {
		t.Errorf("外键约束只有 %d 个，解析可能失效", len(cons))
	}
}

// ---- 命名规则（R1 / R2） ----

var (
	tableCommentRe = regexp.MustCompile("(?m)^-- 表 `([^`]+)`（源表 `([^`]+)`）$")
	truncatedFKRe  = regexp.MustCompile(`(?m)^--\s+(\S+) -> (dbt_\S+)$`)
)

func TestTableNamesFollowPrefixRule(t *testing.T) {
	defs := loadSchema(t)
	text := readFile(t, schemaFile)
	declared := map[string]string{} // 新表名 -> 文件里“表 `新`（源表 `旧`）”注释写的源表名
	for _, m := range tableCommentRe.FindAllStringSubmatch(text, -1) {
		declared[m[1]] = m[2]
	}
	if len(declared) != len(defs) {
		t.Errorf("“-- 表 `新`（源表 `旧`）”注释 %d 条，表 %d 张：每张表前都应写明源表名", len(declared), len(defs))
	}
	ext := deploySQLTables(t)
	seen := map[string]bool{}
	for _, d := range defs {
		if seen[d.Name] {
			t.Errorf("表 %s 重复", d.Name)
		}
		seen[d.Name] = true
		if len(d.Name) > identMax {
			t.Errorf("表名 %s 超过 %d 个字符", d.Name, identMax)
		}
		src, ok := sourceName(d.Name)
		if !ok {
			t.Errorf("表 %s 不符合命名规则（应为 dbt_ + 源表名，或两个例外之一）", d.Name)
			continue
		}
		if targetName(src) != d.Name {
			t.Errorf("表 %s：由源表名 %s 推出的新名是 %s", d.Name, src, targetName(src))
		}
		if strings.HasPrefix(src, tablePrefix) {
			t.Errorf("表 %s 的源名 %s 已带前缀：不应再加一层", d.Name, src)
		}
		if got := declared[d.Name]; got != src {
			t.Errorf("表 %s 前的注释写的源表名是 %q，规则推出的是 %q", d.Name, got, src)
		}
		if _, clash := ext[d.Name]; clash {
			t.Errorf("表 %s 与 deploy/sql 里的扩展表重名", d.Name)
		}
		for _, e := range expectedExcluded {
			if src == e {
				t.Errorf("表 %s 的源表 %s 是被排除的表", d.Name, e)
			}
		}
	}
}

// 001 文件头声明的例外、外键统计、被截断的外键约束名，都要与规则（含 sha1）一致。
func TestRenameRuleMatchesGeneratedHeader(t *testing.T) {
	text := readFile(t, schemaFile)
	head := text
	if i := strings.Index(text, "\n\n"); i > 0 {
		head = text[:i]
	}
	// 例外：“-- 表名：…” 到 “-- 外键约束名：” 之间的 “a -> b”
	i, j := strings.Index(head, "-- 表名："), strings.Index(head, "-- 外键约束名：")
	if i < 0 || j < i {
		t.Fatalf("%s 文件头没有“表名：…”与“外键约束名：…”两段规则说明", schemaFile)
	}
	got := map[string]string{}
	for _, m := range regexp.MustCompile(`([a-z0-9_]+) -> ([a-z0-9_]+)`).FindAllStringSubmatch(head[i:j], -1) {
		got[m[1]] = m[2]
	}
	if len(got) != len(renameExceptions) {
		t.Errorf("文件头声明的例外 %v 与测试里的 renameExceptions %v 不一致", got, renameExceptions)
	}
	for k, v := range renameExceptions {
		if got[k] != v {
			t.Errorf("文件头声明的例外里 %s -> %q，测试里是 %q", k, got[k], v)
		}
	}
	// 外键统计与被截断的名字
	defs := loadSchema(t)
	actual := map[string]bool{}
	for _, d := range defs {
		for _, m := range fkNameRe.FindAllStringSubmatch(d.Body, -1) {
			actual[m[1]] = true
		}
	}
	m := regexp.MustCompile(`本文件共 (\d+) 个外键约束，其中 (\d+) 个因超长`).FindStringSubmatch(head)
	if m == nil {
		t.Fatalf("%s 文件头没有“本文件共 N 个外键约束，其中 K 个因超长…”", schemaFile)
	}
	if n, _ := strconv.Atoi(m[1]); n != len(actual) {
		t.Errorf("文件头声明 %d 个外键约束，实际 %d 个", n, len(actual))
	}
	k := 0
	for _, tm := range truncatedFKRe.FindAllStringSubmatch(head[strings.Index(head, "因超长"):], -1) {
		old, nw := tm[1], tm[2]
		k++
		if want := fkTargetName(old); want != nw {
			t.Errorf("超长外键约束名 %s：文件头写的新名 %s，规则（含 sha1）推出的是 %s", old, nw, want)
		}
		if len(nw) != identMax {
			t.Errorf("截断后的外键约束名 %s 长度 %d，应恰好 %d", nw, len(nw), identMax)
		}
		if !actual[nw] {
			t.Errorf("文件头列出的新外键约束名 %s 不在表定义里", nw)
		}
	}
	if want, _ := strconv.Atoi(m[2]); k != want {
		t.Errorf("文件头声明 %d 个超长外键约束，列出了 %d 个", want, k)
	}
	if k == 0 {
		t.Error("没有任何被截断的外键约束名：规则里的 sha1 分支没有被覆盖到，请确认源文件是否变了")
	}
}

func TestRenameRuleUnits(t *testing.T) {
	for src, want := range map[string]string{
		"sql_users": "dbt_sql_users", "2fa_config": "dbt_2fa_config", "audit_log": "dbt_audit_log",
		"django_content_type": "dbt_content_type",
		"dbt_notify_channel":  "dbt_notify_channel",
	} {
		if got := targetName(src); got != want {
			t.Errorf("targetName(%q) = %q，期望 %q", src, got, want)
		}
	}
	for dst, want := range map[string]string{"dbt_sql_users": "sql_users", "dbt_content_type": "django_content_type"} {
		if got, ok := sourceName(dst); !ok || got != want {
			t.Errorf("sourceName(%q) = %q,%v，期望 %q", dst, got, ok, want)
		}
	}
	if _, ok := sourceName("dbt_django_content_type"); ok {
		t.Error("dbt_django_content_type 不是合法的新名（例外表必须用 dbt_content_type）")
	}
	if _, ok := sourceName("sql_users"); ok {
		t.Error("没有 dbt_ 前缀的名字不是新名")
	}
	// 外键约束名：恰好 64 不截断；65 起截断到 64
	n60 := strings.Repeat("a", 60)
	if got := fkTargetName(n60); got != "dbt_"+n60 || len(got) != identMax {
		t.Errorf("60 字符的原名：%q", got)
	}
	n61 := strings.Repeat("b", 61)
	got := fkTargetName(n61)
	if len(got) != identMax || !strings.HasPrefix(got, "dbt_"+strings.Repeat("b", fkKeep)+"_") {
		t.Errorf("61 字符的原名：%q（长度 %d）", got, len(got))
	}
	if h := sha1.Sum([]byte(n61)); !strings.HasSuffix(got, hex.EncodeToString(h[:])[:4]) {
		t.Errorf("截断后的后缀应是 sha1(原名) 的前 4 位：%q", got)
	}
	if fkKeep != 55 {
		t.Errorf("fkKeep=%d，R2 规定保留原名前 55 个字符", fkKeep)
	}
}

// ---- 文件头声明 ----

// 文件头声明的范围：(001 的表数, 源文件表数, 排除表数) 与排除名单。
func headerScope(t testing.TB) (kept, total, excluded int, names []string) {
	t.Helper()
	text := readFile(t, schemaFile)
	m := regexp.MustCompile(`范围：(\d+) 张表（源文件共 (\d+) 张，排除 (\d+) 张`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("%s 文件头没有“范围：N 张表（源文件共 M 张，排除 K 张 …”", schemaFile)
	}
	kept, _ = strconv.Atoi(m[1])
	total, _ = strconv.Atoi(m[2])
	excluded, _ = strconv.Atoi(m[3])
	if m := regexp.MustCompile(`(?m)^-- 排除：(.+)$`).FindStringSubmatch(text); m != nil {
		for _, n := range strings.Split(m[1], "、") {
			names = append(names, strings.TrimSpace(n))
		}
	}
	sort.Strings(names)
	return
}

// 文件头声明、实际表数、排除名单三者自洽。
func TestSchemaHeaderIsConsistentWithBody(t *testing.T) {
	kept, total, excluded, names := headerScope(t)
	defs := loadSchema(t)
	if len(defs) != kept {
		t.Errorf("文件头声明 %d 张表，实际 %d 张", kept, len(defs))
	}
	if kept+excluded != total {
		t.Errorf("文件头：%d + %d != %d", kept, excluded, total)
	}
	want := append([]string(nil), expectedExcluded...)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("文件头的排除名单 %v 与测试里的 expectedExcluded %v 不一致", names, want)
	}
	if len(want) != excluded {
		t.Errorf("排除数量 %d 与文件头 %d 不一致", len(want), excluded)
	}
	// 被排除的表不能出现在 001 里，也不能被保留的表的外键引用（loadSchema 的顺序测试已覆盖后者）
	for _, e := range expectedExcluded {
		for _, n := range tableNames(defs) {
			if n == e {
				t.Errorf("被排除的表 %s 出现在 001 里", e)
			}
		}
	}
}

var charsetClauseRe = regexp.MustCompile(`(?i)\s+(?:DEFAULT\s+)?(?:CHARACTER\s+SET|CHARSET|COLLATE)(?:\s*=\s*|\s+)[A-Za-z0-9_]+\b`)

// stripCharsetClauses 去掉引号之外的字符集 / 校验规则子句（反引号标识符、单 / 双引号字符串里的同名文字原样保留），
// 返回结果与去掉的个数。
func stripCharsetClauses(s string) (string, int) {
	var out strings.Builder
	n := 0
	flush := func(seg string) {
		out.WriteString(charsetClauseRe.ReplaceAllStringFunc(seg, func(string) string { n++; return "" }))
	}
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\'' && c != '"' && c != '`' {
			i++
			continue
		}
		flush(s[start:i])
		j := i + 1
		for j < len(s) {
			if s[j] == '\\' && c != '`' {
				j += 2
				continue
			}
			if s[j] == c {
				if j+1 < len(s) && s[j+1] == c {
					j += 2
					continue
				}
				break
			}
			j++
		}
		if j >= len(s) {
			j = len(s) - 1
		}
		out.WriteString(s[i : j+1])
		i = j + 1
		start = i
	}
	if start < len(s) {
		flush(s[start:])
	}
	return out.String(), n
}

func TestStripCharsetClauses(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		n        int
	}{
		{"`a` varchar(10) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL", "`a` varchar(10) NOT NULL", 2},
		{"`charset` varchar(20) COLLATE utf8mb4_general_ci DEFAULT ''", "`charset` varchar(20) DEFAULT ''", 1},
		{"`a` int COMMENT 'COLLATE utf8mb4_bin x' ", "`a` int COMMENT 'COLLATE utf8mb4_bin x' ", 0},
		{"`a` int COMMENT 'it''s CHARSET=x'", "`a` int COMMENT 'it''s CHARSET=x'", 0},
		{"`a` int COMMENT 'it\\'s COLLATE x'", "`a` int COMMENT 'it\\'s COLLATE x'", 0},
		{"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='x COLLATE=y'", "ENGINE=InnoDB COMMENT='x COLLATE=y'", 2},
		{"ENGINE=InnoDB DEFAULT CHARSET=utf8mb3", "ENGINE=InnoDB", 1},
		{"`a` int DEFAULT NULL", "`a` int DEFAULT NULL", 0},
	} {
		got, n := stripCharsetClauses(tc.in)
		if got != tc.want || n != tc.n {
			t.Errorf("%q\n  得到 %q (%d)\n  期望 %q (%d)", tc.in, got, n, tc.want, tc.n)
		}
	}
}
