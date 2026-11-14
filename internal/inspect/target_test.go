// @author: kevinkelly
// @license: Apache Licence
// @file: target_test.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbtool/internal/engine"
)

func TestCheckReadOnly(t *testing.T) {
	ok := []string{
		"SELECT 1",
		"  select * from t where a = 1;",
		"SHOW STATUS WHERE variable_name IN ('Threads_connected')",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"/* 说明 */ SELECT 1",
		"-- 注释\nSELECT 1",
		"SELECT /*+ SET_VAR(information_schema_stats_expiry=0) MAX_EXECUTION_TIME(120000) */ 1",
		"EXPLAIN SELECT 1",
		"DESC t",
		"show engine innodb status",
	}
	for _, q := range ok {
		if err := CheckReadOnly(q); err != nil {
			t.Errorf("应允许：%q，得到 %v", q, err)
		}
	}
	bad := []string{
		"",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"INSERT INTO t VALUES (1)",
		"DROP TABLE t",
		"SET GLOBAL max_connections = 1",
		"KILL 10",
		"GRANT ALL ON *.* TO x",
		"USE mysql",
		"EXEC xp_cmdshell 'dir'",
		"SELECT 1; DROP TABLE t",
		"SELECT * FROM t INTO OUTFILE '/tmp/x'",
		"SELECT a INTO #tmp FROM t",
		"SELECT * FROM t FOR UPDATE",
		"SELECT * FROM t LOCK IN SHARE MODE",
		"SELECT GET_LOCK('a', 10)",
		"SELECT SLEEP(100)",
		"/* x */ DELETE FROM t",
		"-- SELECT\nDROP TABLE t",
		"CALL p()",
		"TRUNCATE t",
	}
	for _, q := range bad {
		if err := CheckReadOnly(q); err == nil {
			t.Errorf("应拒绝：%q", q)
		}
	}
}

// 巡检里写死的所有语句都必须是只读的：假目标库会对每条查询做校验，这里再扫一遍 MSSQL 检查项的语句文本。
func TestInspectionStatementsAreReadOnly(t *testing.T) {
	ft := mssqlHappyTarget()
	for _, r := range ft.rules {
		if strings.TrimSpace(r.sub) == "" {
			t.Fatal("空规则")
		}
	}
	for _, ck := range msChecks {
		c := &msContext{ctx: t.Context(), t: &recordTarget{}, now: newClock(ts("2026-10-06 08:00:00")).Now, cpuCount: 8, version: 15, server: map[string]any{}}
		c.db = "ShopDB"
		_, _ = ck.Run(c) // 记录目标收到的语句；返回什么结果无所谓
		for _, q := range c.t.(*recordTarget).queries {
			if err := CheckReadOnly(q); err != nil {
				t.Errorf("检查项 %s 含非只读语句：%s", ck.Key, truncate(q, 100))
			}
		}
	}
}

type recordTarget struct{ queries []string }

func (r *recordTarget) Query(_ context.Context, db, q string, args ...any) (*Result, error) {
	r.queries = append(r.queries, q)
	return &Result{}, nil
}
func (r *recordTarget) Close() error { return nil }

func TestOpenSQLUnsupportedType(t *testing.T) {
	for _, db := range []string{"redis", "oracle", "mongo", "pgsql", ""} {
		if _, err := OpenSQL(context.Background(), db, engine.Conn{}); err == nil || !strings.Contains(err.Error(), "暂不支持巡检") {
			t.Errorf("%q 应明确提示暂不支持：%v", db, err)
		}
	}
}

func TestScrubAndQuote(t *testing.T) {
	msg := scrub("dial tcp 10.0.0.1:3306: access denied for user 'dbadmin' (using password: s3cretPW)", engine.Conn{User: "dbadmin", Password: "s3cretPW"})
	if strings.Contains(msg, "s3cretPW") || strings.Contains(msg, "dbadmin") {
		t.Errorf("错误信息里不能带账号密码：%s", msg)
	}
	if quoteIdent(KindMySQL, "a`b") != "`a``b`" || quoteIdent(KindMSSQL, "a]b") != "[a]]b]" {
		t.Error("标识符转义")
	}
}

// 目标库返回的查询错误（如 1142 “SELECT command denied to user 'x'@'host'”）会存进报告，必须抹掉账号密码；
// 上下文超时等不带账号的错误原样返回（errors.Is 仍然成立）。
func TestSQLTargetCleanQueryError(t *testing.T) {
	tg := &sqlTarget{conn: engine.Conn{User: "marcus", Password: "s3cretPW"}}
	err := tg.clean(errors.New("Error 1142 (42000): SELECT command denied to user 'marcus'@'47.1.2.3' for table 'user'"))
	if err == nil || strings.Contains(err.Error(), "marcus") || !strings.Contains(err.Error(), "SELECT command denied") {
		t.Errorf("账号名应被抹掉、其余信息保留：%v", err)
	}
	if got := tg.clean(context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("不含账号的错误应原样返回：%v", got)
	}
	if tg.clean(nil) != nil {
		t.Error("nil 应返回 nil")
	}
}
