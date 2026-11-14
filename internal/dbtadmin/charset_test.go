// @author: kevinkelly
// @license: Apache Licence
// @file: charset_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

func TestSuggestCollation(t *testing.T) {
	for v, want := range map[string]string{
		"8.0.36":              "utf8mb4_0900_ai_ci",
		"8.4.0":               "utf8mb4_0900_ai_ci",
		"9.0.1":               "utf8mb4_0900_ai_ci",
		"5.7.44-log":          "utf8mb4_general_ci",
		"10.6.12-MariaDB-log": "utf8mb4_general_ci",
		"8.0.30-MariaDB":      "utf8mb4_general_ci",
		"":                    "utf8mb4_general_ci",
		"not-a-version":       "utf8mb4_general_ci",
	} {
		if got := suggestCollation(v); got != want {
			t.Errorf("%q: %s，期望 %s", v, got, want)
		}
	}
}

func TestEvaluateCharset(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cs        DBCharset
		version   string
		want      charsetLevel
		mustHave  []string
		mustAvoid []string
	}{
		{"utf8mb4 + general_ci", DBCharset{"utf8mb4", "utf8mb4_general_ci"}, "8.0.36", charsetOK, nil, nil},
		{"utf8mb4 + 0900_ai_ci", DBCharset{"utf8mb4", "utf8mb4_0900_ai_ci"}, "8.0.36", charsetOK, nil, nil},
		{"utf8mb4 + unicode_ci", DBCharset{"utf8mb4", "utf8mb4_unicode_ci"}, "5.7.44", charsetOK, nil, nil},
		{"大小写不敏感写法", DBCharset{"UTF8MB4", "UTF8MB4_GENERAL_CI"}, "8.0.36", charsetOK, nil, nil},
		{"utf8mb4 + bin", DBCharset{"utf8mb4", "utf8mb4_bin"}, "8.0.36", charsetWarn,
			[]string{"utf8mb4_bin", "二进制比较", "ALTER DATABASE `appdb` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"}, nil},
		{"utf8mb4 + 0900_as_cs", DBCharset{"utf8mb4", "utf8mb4_0900_as_cs"}, "8.0.36", charsetWarn, []string{"区分大小写"}, nil},
		{"utf8mb4 + 0900_as_ci", DBCharset{"utf8mb4", "utf8mb4_0900_as_ci"}, "8.0.36", charsetWarn, []string{"区分重音"}, nil},
		{"utf8mb4 + 语言排序", DBCharset{"utf8mb4", "utf8mb4_croatian_ci"}, "5.7.44", charsetWarn,
			[]string{"语言", "COLLATE utf8mb4_general_ci"}, nil},
		{"utf8mb3", DBCharset{"utf8mb3", "utf8mb3_general_ci"}, "8.0.36", charsetRefuse,
			[]string{"utf8mb3", "不是 utf8mb4", "ALTER DATABASE `appdb` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci", "--skip-charset-check", "不修改任何已有表"}, nil},
		{"旧别名 utf8", DBCharset{"utf8", "utf8_general_ci"}, "5.7.44", charsetRefuse,
			[]string{"ALTER DATABASE `appdb` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci"}, nil},
		{"latin1", DBCharset{"latin1", "latin1_swedish_ci"}, "8.0.36", charsetRefuse, []string{"latin1"}, nil},
		{"空值", DBCharset{}, "8.0.36", charsetRefuse, []string{"未知"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			level, lines := EvaluateCharset(tc.cs, tc.version, "appdb")
			if level != tc.want {
				t.Fatalf("级别 %d，期望 %d：%v", level, tc.want, lines)
			}
			joined := strings.Join(lines, "\n")
			mustContain(t, "说明", joined, tc.mustHave...)
			mustNotContain(t, "说明", joined, tc.mustAvoid...)
			if level == charsetOK && len(lines) != 0 {
				t.Errorf("OK 不应有输出: %v", lines)
			}
		})
	}
}

func TestBootstrapDryRunShowsCharsetAndNeverAltersExisting(t *testing.T) {
	st := newMemStore()
	te := newTestEnv(t, st)
	if code := te.run("bootstrap", "--sql-root", sample(t)); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	mustContain(t, "stdout", te.out.String(),
		"库默认字符集 / 校验规则：utf8mb4 / utf8mb4_general_ci", "都不指定", "不会修改已存在的表", "CREATE TABLE IF NOT EXISTS")
	for _, e := range st.execs {
		if strings.Contains(strings.ToUpper(e), "ALTER") {
			t.Errorf("不得执行 ALTER: %s", e)
		}
	}
}

func TestBootstrapRefusesNonUtf8mb4Default(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		charset string
		coll    string
		args    []string
		fix     string
	}{
		{"演练", "8.0.36", "latin1", "latin1_swedish_ci", nil, "ALTER DATABASE `db_tool` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"},
		{"应用", "5.7.44-log", "utf8mb3", "utf8mb3_general_ci", []string{"--apply", "--yes"}, "ALTER DATABASE `db_tool` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;"},
		{"only-sql", "8.0.36", "utf8", "utf8_general_ci", []string{"--only-sql", "--apply", "--yes"}, "ALTER DATABASE `db_tool` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newMemStore()
			st.version, st.charset, st.collation = tc.version, tc.charset, tc.coll
			te := newTestEnv(t, st)
			args := append([]string{"bootstrap", "--sql-root", sample(t)}, tc.args...)
			if code := te.run(args...); code != ExitFailed {
				t.Fatalf("应拒绝，exit %d\n%s", code, te.all())
			}
			if len(st.execs) != 0 || len(st.applied) != 0 || st.tables[TblApplied] {
				t.Errorf("拒绝时不得写任何东西: execs=%d applied=%v", len(st.execs), st.applied)
			}
			mustContain(t, "stderr", te.errb.String(), "不是 utf8mb4", tc.fix, "--skip-charset-check", "不修改任何已有表")
			mustNotContain(t, "输出", te.all(), "S3cretDb", "Pw%40")
		})
	}
}

func TestBootstrapWarnsOnUnusualCollation(t *testing.T) {
	st := newMemStore()
	st.collation = "utf8mb4_bin"
	te := newTestEnv(t, st)
	if code := te.run("bootstrap", "--sql-root", sample(t), "--apply", "--yes"); code != ExitOK {
		t.Fatalf("警告不应阻止执行，exit %d\n%s", code, te.all())
	}
	mustContain(t, "stdout", te.out.String(), "警告", "utf8mb4_bin", "二进制比较", "ALTER DATABASE `db_tool`")
	if len(st.execs) == 0 {
		t.Error("应继续执行")
	}
}

func TestSkipCharsetCheckNeedsYesAndPrintsRisk(t *testing.T) {
	st := newMemStore()
	st.charset, st.collation = "latin1", "latin1_swedish_ci"
	te := newTestEnv(t, st)
	root := sample(t)
	// 没有 --yes：参数错误，不碰库
	if code := te.run("bootstrap", "--sql-root", root, "--skip-charset-check"); code != ExitUsage {
		t.Fatalf("缺 --yes 应返回 2，得到 %d\n%s", code, te.all())
	}
	mustContain(t, "stderr", te.errb.String(), "--skip-charset-check 必须同时加 --yes")
	if len(st.execs) != 0 {
		t.Fatal("不应执行任何语句")
	}
	// 带 --yes：打印风险并继续
	te.reset()
	if code := te.run("bootstrap", "--sql-root", root, "--skip-charset-check", "--yes", "--apply"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	mustContain(t, "stdout", te.out.String(), "已按 --skip-charset-check 跳过检查", "风险", "latin1 / latin1_swedish_ci")
	if len(st.execs) == 0 {
		t.Error("跳过检查后应继续执行")
	}
}

func TestBootstrapCharsetReadFailure(t *testing.T) {
	st := newMemStore()
	st.charsetErr = errors.New("Error 1142: SELECT command denied to user 'dbtool'@'h' for table 'SCHEMATA'")
	te := newTestEnv(t, st)
	root := sample(t)
	if code := te.run("bootstrap", "--sql-root", root, "--apply", "--yes"); code != ExitFailed {
		t.Fatalf("读不到默认字符集应拒绝，exit %d\n%s", code, te.all())
	}
	mustContain(t, "stderr", te.errb.String(), "读取库默认字符集失败", "--skip-charset-check")
	if len(st.execs) != 0 {
		t.Error("不应执行任何语句")
	}
	te.reset()
	if code := te.run("bootstrap", "--sql-root", root, "--apply", "--yes", "--skip-charset-check"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	mustContain(t, "stdout", te.out.String(), "无法确认新建的表是 utf8mb4")
}

func TestStatusReportsCharset(t *testing.T) {
	st := newMemStore()
	st.version, st.collation = "8.0.36", "utf8mb4_0900_ai_ci"
	te := newTestEnv(t, st)
	if code := te.run("status", "--porcelain"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, te.all())
	}
	mustContain(t, "stdout", te.out.String(), "charset=utf8mb4\n", "collation=utf8mb4_0900_ai_ci\n", "charset_check=ok\n")

	te.reset()
	if code := te.run("status"); code != ExitOK {
		t.Fatal(code)
	}
	mustContain(t, "stdout", te.out.String(), "库默认字符集 / 校验规则：utf8mb4 / utf8mb4_0900_ai_ci", "已存在的表不会被本工具修改")

	// 不常见的校验规则：警告，退出码 0
	st.collation = "utf8mb4_bin"
	te.reset()
	if code := te.run("status", "--porcelain"); code != ExitOK {
		t.Fatalf("警告不应改变退出码: %d", code)
	}
	mustContain(t, "stdout", te.out.String(), "charset_check=warn\n")

	// 非 utf8mb4：退出码 1，输出仍完整，修复命令在 stderr
	st.charset, st.collation = "latin1", "latin1_swedish_ci"
	te.reset()
	if code := te.run("status", "--porcelain"); code != ExitFailed {
		t.Fatalf("非 utf8mb4 应返回 1，得到 %d\n%s", code, te.all())
	}
	mustContain(t, "stdout", te.out.String(), "db=db_tool\n", "charset=latin1\n", "charset_check=fail\n")
	mustContain(t, "stderr", te.errb.String(), "ALTER DATABASE `db_tool` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;")

	te.reset()
	if code := te.run("status"); code != ExitFailed {
		t.Fatalf("exit %d", code)
	}
	mustContain(t, "stdout", te.out.String(), "latin1 / latin1_swedish_ci", "结论：")
	mustContain(t, "stderr", te.errb.String(), "ALTER DATABASE")

	// --skip-charset-check 需要 --yes
	te.reset()
	if code := te.run("status", "--skip-charset-check"); code != ExitUsage {
		t.Fatalf("缺 --yes: %d", code)
	}
	te.reset()
	if code := te.run("status", "--skip-charset-check", "--yes", "--porcelain"); code != ExitOK {
		t.Fatalf("跳过后应返回 0: %d\n%s", code, te.all())
	}
	mustContain(t, "stdout", te.out.String(), "charset_check=fail\n")

	// 读取失败：状态未知，不当作失败
	st.charsetErr = errors.New("denied")
	te.reset()
	if code := te.run("status", "--porcelain"); code != ExitOK {
		t.Fatalf("读取失败不应让 status 失败: %d", code)
	}
	mustContain(t, "stdout", te.out.String(), "charset_check=unknown\n")
}

func TestBootstrapHelpMentionsSkipCharsetCheck(t *testing.T) {
	te := newTestEnv(t, newMemStore())
	if code := te.run("bootstrap", "--help"); code != ExitOK {
		t.Fatal(code)
	}
	mustContain(t, "help", te.out.String(), "--skip-charset-check", "--yes")
}

// 真实 Store 实现：读 information_schema.SCHEMATA 里“当前库”的默认字符集与校验规则（只读查询）。
func TestMySQLStoreDatabaseCharset(t *testing.T) {
	st, f := newFakeStore(t, func(q string, _ []driver.Value) *fakeResult {
		if strings.Contains(q, "information_schema.SCHEMATA") {
			return &fakeResult{cols: []string{"a", "b"}, rows: [][]driver.Value{{"utf8mb4", "utf8mb4_0900_ai_ci"}}}
		}
		return nil
	})
	cs, err := st.DatabaseCharset(context.Background())
	if err != nil || cs.Charset != "utf8mb4" || cs.Collation != "utf8mb4_0900_ai_ci" {
		t.Fatalf("%+v %v", cs, err)
	}
	qs := f.queries("information_schema.SCHEMATA")
	if len(qs) != 1 || !strings.Contains(qs[0].sql, "SCHEMA_NAME = DATABASE()") || !strings.HasPrefix(strings.TrimSpace(qs[0].sql), "SELECT") {
		t.Errorf("查询不对: %+v", qs)
	}
}

func TestMySQLStoreDatabaseCharsetNoRow(t *testing.T) {
	st, _ := newFakeStore(t)
	if _, err := st.DatabaseCharset(context.Background()); err == nil {
		t.Fatal("没有行应返回错误")
	}
}

// dbt_bootstrap_applied 由工具自己建表：同样不指定字符集 / 校验规则。
func TestAppliedTableDDLHasNoCharset(t *testing.T) {
	up := strings.ToUpper(createAppliedSQL)
	for _, bad := range []string{"CHARSET", "CHARACTER SET", "COLLATE"} {
		if strings.Contains(up, bad) {
			t.Errorf("createAppliedSQL 不应含 %s：\n%s", bad, createAppliedSQL)
		}
	}
	if !strings.Contains(up, "CREATE TABLE IF NOT EXISTS") {
		t.Error("应保持幂等")
	}
}
