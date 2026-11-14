// @author: kevinkelly
// @license: Apache Licence
// @file: mssql_test.go
// @time: 2026/10/28

package mssql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/engine"
)

type fakeRunner struct {
	calls []string
	run   func(unit string) (UnitOutcome, error)
}

func (f *fakeRunner) Run(_ context.Context, unit string) (UnitOutcome, error) {
	f.calls = append(f.calls, unit)
	return f.run(unit)
}

func TestExecUnitsStopsAtFirstFailure(t *testing.T) {
	f := &fakeRunner{run: func(u string) (UnitOutcome, error) {
		if strings.Contains(u, "boom") {
			return UnitOutcome{Affected: -1}, errors.New("Invalid object name 'x'.")
		}
		return UnitOutcome{Affected: 3}, nil
	}}
	res := ExecUnits(context.Background(), f, []string{"update a set x=1", "update boom set x=1", "update c set x=1"})
	if len(res) != 3 || len(f.calls) != 2 {
		t.Fatalf("失败后不应再执行: %+v calls=%v", res, f.calls)
	}
	if res[0].Err != "" || res[0].Affected != 3 {
		t.Errorf("第一条: %+v", res[0])
	}
	if !strings.Contains(res[1].Err, "Invalid object name") || res[1].Skipped {
		t.Errorf("第二条: %+v", res[1])
	}
	if !res[2].Skipped || res[2].Err != "" {
		t.Errorf("第三条应标记未执行: %+v", res[2])
	}
}

func printRunner() *fakeRunner {
	return &fakeRunner{run: func(u string) (UnitOutcome, error) {
		if strings.Contains(u, "print") {
			return UnitOutcome{Affected: -1, Messages: []Message{{Number: 0, Text: "sp_GenerateChart 创建完成。"}}}, nil
		}
		return UnitOutcome{Affected: 1}, nil
	}}
}

// 默认：PRINT 只是信息，显示在该语句的信息里，语句照常成功，后面的单元继续执行。
func TestExecUnitsPrintIsInformationByDefault(t *testing.T) {
	res := ExecUnits(context.Background(), printRunner(), []string{"print 'x'", "update t set a=1"})
	if res[0].Err != "" || res[0].Skipped || res[0].Note != "sp_GenerateChart 创建完成。" {
		t.Errorf("PRINT 应作为信息显示、不算失败: %+v", res[0])
	}
	if res[1].Err != "" || res[1].Skipped || res[1].Affected != 1 {
		t.Errorf("PRINT 之后的单元应继续执行: %+v", res[1])
	}
}

// 开了 mssql_print_as_error：PRINT 按失败处理并停止。
func TestExecUnitsPrintIsFailureWhenConfigured(t *testing.T) {
	res := ExecUnitsWith(context.Background(), printRunner(), []string{"print 'x'", "update t set a=1"}, true)
	if res[0].Err != "sp_GenerateChart 创建完成。" || !res[1].Skipped {
		t.Errorf("PRINT 应按失败处理并停止: %+v", res)
	}
}

func TestExecUnitsMessagesAndResultSets(t *testing.T) {
	f := &fakeRunner{run: func(u string) (UnitOutcome, error) {
		switch {
		case strings.HasPrefix(u, "raiserror"):
			return UnitOutcome{Affected: -1, Messages: []Message{{Number: 50000, Text: "进度 50%"}}}, nil
		case strings.HasPrefix(u, "select"):
			return UnitOutcome{Affected: -1, ResultSets: 1, FetchRows: 42}, nil
		case strings.HasPrefix(u, "mixed"):
			return UnitOutcome{Affected: 5, ResultSets: 1, FetchRows: 9}, nil
		}
		return UnitOutcome{Affected: 0}, nil
	}}
	res := ExecUnits(context.Background(), f, []string{"raiserror ('x',1,1)", "select 1", "mixed", "create table t(id int)"})
	if res[0].Err != "" || res[0].Note != "进度 50%" {
		t.Errorf("低严重度 RAISERROR 只展示: %+v", res[0])
	}
	if res[1].Affected != 42 || !strings.Contains(res[1].Note, "查询返回 42 行") {
		t.Errorf("纯查询把返回行数填进影响行数: %+v", res[1])
	}
	if res[2].Affected != 5 || !strings.Contains(res[2].Note, "查询返回 9 行") {
		t.Errorf("有 DML 计数时保留 DML 行数: %+v", res[2])
	}
	if res[3].Affected != 0 || res[3].Note != "" || res[3].Err != "" {
		t.Errorf("DDL: %+v", res[3])
	}
}

func TestExecUnitsErrorKeepsEarlierMessages(t *testing.T) {
	f := &fakeRunner{run: func(string) (UnitOutcome, error) {
		return UnitOutcome{Affected: -1, Messages: []Message{{Number: 50000, Text: "开始处理"}}}, errors.New("违反 PRIMARY KEY 约束")
	}}
	res := ExecUnits(context.Background(), f, []string{"insert t values (1)"})
	if !strings.HasPrefix(res[0].Err, "开始处理\n") || !strings.Contains(res[0].Err, "违反 PRIMARY KEY") {
		t.Errorf("失败信息带上已收到的消息: %q", res[0].Err)
	}
}

func TestDSNAndQuote(t *testing.T) {
	d := dsn(engine.Conn{Host: "10.0.0.8", Port: 1433, User: "sa", Password: "p@ss:w/rd?", DBName: "shop"})
	for _, want := range []string{"sqlserver://sa:", "@10.0.0.8:1433", "database=shop", "TrustServerCertificate=true", "p%40ss%3Aw%2Frd%3F"} {
		if !strings.Contains(d, want) {
			t.Errorf("DSN 缺少 %q: %s", want, d)
		}
	}
	if QuoteIdent("a]b") != "[a]]b]" {
		t.Error("QuoteIdent")
	}
	if _, err := New(engine.Conn{Host: "127.0.0.1", Port: 1, User: "u", Password: "p"}); err != nil {
		t.Errorf("构造引擎不应连接: %v", err)
	}
}

func TestNormalizeValue(t *testing.T) {
	d := time.Date(2026, 10, 6, 9, 30, 1, 123456000, time.UTC)
	cases := []struct {
		v    any
		typ  string
		want any
	}{
		{[]byte("12.50"), "DECIMAL", "12.50"},
		{[]byte("abc"), "NVARCHAR", "abc"},
		{[]byte{0xDE, 0xAD}, "VARBINARY", "0xDEAD"},
		{d, "DATE", "2026-10-06"},
		{d, "DATETIME2", "2026-10-06 09:30:01.123456"},
		{time.Date(2026, 10, 6, 9, 30, 1, 0, time.UTC), "DATETIME", "2026-10-06 09:30:01"},
		{d, "TIME", "09:30:01.123456"},
		{int64(7), "INT", int64(7)},
		{nil, "INT", nil},
		{"x", "NVARCHAR", "x"},
	}
	for _, c := range cases {
		if got := NormalizeValue(c.v, c.typ); got != c.want {
			t.Errorf("%v(%s) = %v, 期望 %v", c.v, c.typ, got, c.want)
		}
	}
	// GUID：SQL Server 以混合字节序传输，驱动给出 16 字节，要按 GUID 格式还原
	g := NormalizeValue([]byte{0x78, 0x56, 0x34, 0x12, 0xBC, 0x9A, 0xF0, 0xDE, 0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF}, "UNIQUEIDENTIFIER")
	if g != "12345678-9ABC-DEF0-0123-456789ABCDEF" {
		t.Errorf("GUID: %v", g)
	}
}

// 脚本自己检查 @@TRANCOUNT / 自己管理事务的单元不能再被包进外层事务（外层 BEGIN TRAN 会让 @@TRANCOUNT 永远 >= 1，
// 脚本里“有未提交事务就拒绝执行”的检查必然命中）；注释、字符串里出现关键字不算。
func TestUsesTxnControl(t *testing.T) {
	yes := []string{
		"IF @@TRANCOUNT > 0 BEGIN RAISERROR(N'当前会话有未提交的事务', 16, 1); RETURN; END",
		"if @@trancount>0 rollback",
		"BEGIN TRAN\nUPDATE t SET a = 1\nCOMMIT TRAN",
		"begin transaction; delete from t; commit;",
		"COMMIT",
		"ROLLBACK;",
		"SAVE TRANSACTION sp1",
		"UPDATE t SET a = 1\nCOMMIT",
	}
	for _, u := range yes {
		if !usesTxnControl(u) {
			t.Errorf("应判为自管事务：%q", u)
		}
	}
	no := []string{
		"UPDATE t SET note = 'please commit later' WHERE id = 1",
		"-- @@TRANCOUNT 检查在下面\nUPDATE t SET a = 1",
		"/* begin tran */ DELETE FROM t WHERE id = 1",
		"SELECT [commit], [rollback] FROM log",
		"INSERT INTO t(a) VALUES (N'rollback')",
		"ALTER TABLE t ADD c int",
		"UPDATE committed_orders SET a = 1",
	}
	for _, u := range no {
		if usesTxnControl(u) {
			t.Errorf("不应判为自管事务：%q", u)
		}
	}
}
