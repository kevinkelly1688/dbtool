// @author: kevinkelly
// @license: Apache Licence
// @file: diagnose_test.go
// @time: 2026/10/28

package perf

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbtool/internal/model"
)

type fakeExplainer struct {
	res   *ExplainResult
	err   error
	calls []string
}

func (e *fakeExplainer) Explain(_ context.Context, _ *model.Instance, db, sql string, _ int) (*ExplainResult, error) {
	e.calls = append(e.calls, db+"|"+sql)
	return e.res, e.err
}

func diagnose(sql string, m Metrics, ex Explainer) DiagResult {
	return Diagnose(context.Background(), ex, DiagnoseInput{Instance: &model.Instance{}, Product: ProdMySQL, MinAvgMs: 1000, DB: "order", SQL: sql, Metrics: m,
		SampleReal: true, AutoExplain: true, TimeoutMs: 3000})
}

func TestDiagnoseTextRules(t *testing.T) {
	cases := []struct {
		sql      string
		category string
		side     string
		contains string
	}{
		{"select id from t where name like '%abc'", CatIndexInvalid, SideDev, "LIKE 以 % 开头"},
		{"select id from t where date(create_time) = '2024-01-01'", CatIndexInvalid, SideDev, "函数 DATE()"},
		{"select id from t where a = 1 and DATE_FORMAT(b, '%Y') = '2024'", CatIndexInvalid, SideDev, "DATE_FORMAT"},
		{"select id from t order by id limit 20000, 10", CatDeepPaging, SideDev, "offset=20000"},
		{"select id from t order by id limit 10 offset 50000", CatDeepPaging, SideDev, "offset=50000"},
		{"select id from t order by rand() limit 1", CatSortGroup, SideDev, "ORDER BY RAND()"},
		{"select id from t", CatFullScanBiz, SideDev, "没有 WHERE 也没有 LIMIT"},
		{"select 1 from dual", CatOther, SideDev, "未命中已知规则"},
		{"select id from t where a = 1", CatOther, SideDev, "未命中已知规则"},
	}
	for _, c := range cases {
		r := diagnose(c.sql, Metrics{}, nil)
		if r.Category != c.category || r.OwnerSide != c.side || !strings.Contains(r.Diagnosis, c.contains) {
			t.Errorf("%s\n got %s/%s %q\n want %s/%s %q", c.sql, r.Category, r.OwnerSide, r.Diagnosis, c.category, c.side, c.contains)
		}
	}
	// 超长 IN 列表
	in := "select id from t where id in (" + strings.Repeat("1,", 600) + "1)"
	if r := diagnose(in, Metrics{}, nil); !strings.Contains(r.Diagnosis, "IN 列表过长(601个值)") {
		t.Errorf("IN 列表: %s", r.Diagnosis)
	}
	// 建议：SELECT *、!= 、OR
	r := diagnose("select * from t where a != 1 or b = 2", Metrics{}, nil)
	if !strings.Contains(r.Suggestion, "避免 SELECT *") || !strings.Contains(r.Suggestion, "!= / <> / NOT IN") {
		t.Errorf("建议: %s", r.Suggestion)
	}
	// 主类别按优先级：索引失效 > 深分页
	r = diagnose("select id from t where date(a)='x' order by id limit 20000,10", Metrics{}, nil)
	if r.Category != CatIndexInvalid {
		t.Errorf("优先级: %s", r.Category)
	}
}

func TestDiagnoseMetricRules(t *testing.T) {
	r := diagnose("select id from t where a=1", Metrics{RowsSentAvg: 50000}, nil)
	if r.Category != CatLargeResult || !strings.Contains(r.Diagnosis, "平均每次返回 50000 行") {
		t.Errorf("大结果集: %+v", r)
	}
	r = diagnose("select id from t where a=1", Metrics{AvgMs: 200, PerDay: 200000}, nil)
	if r.Category != CatHighFrequency || !strings.Contains(r.Diagnosis, "日均执行 200,000 次") {
		t.Errorf("高频: %+v", r)
	}
	r = diagnose("select id from t where a=1", Metrics{RowsExaminedAvg: 500000, RowsSentAvg: 10, AvgMs: 5000}, nil)
	if !strings.Contains(r.Diagnosis, "扫描/返回比过高") {
		t.Errorf("扫描比: %+v", r)
	}
	// OLAP 产品
	r = Diagnose(context.Background(), nil, DiagnoseInput{Product: ProdSelectDB, MinAvgMs: 1000, SQL: "select a from t where dt='x'", Metrics: Metrics{RowsExaminedAvg: 2e8, AvgMs: 5000}, SampleReal: true})
	if r.Category != CatOLAPHeavy || !strings.Contains(r.Suggestion, "分区列") {
		t.Errorf("OLAP: %+v", r)
	}
	// PolarDB-X 的子查询
	r = Diagnose(context.Background(), nil, DiagnoseInput{Product: ProdPolarDBX, SQL: "select a from t where id in (select id from s)", SampleReal: true})
	if r.Category != CatSubqueryJoin || !strings.Contains(r.Suggestion, "分区键") {
		t.Errorf("PolarDB-X: %+v", r)
	}
}

func TestDiagnoseExplainMissingIndex(t *testing.T) {
	ex := &fakeExplainer{res: &ExplainResult{Plan: []map[string]any{
		{"id": 1, "select_type": "SIMPLE", "table": "t_order", "type": "ALL", "rows": int64(3000000), "extra": "Using where"},
	}}}
	r := diagnose("select * from t_order where user_id = 1 and status in (1,2) and created > '2024-01-01' and updated > 5", Metrics{}, ex)
	if r.Category != CatMissingIndex || r.OwnerSide != SideDBA {
		t.Fatalf("纯缺索引交给 DBA: %+v", r)
	}
	want := "ALTER TABLE `order`.`t_order` ADD INDEX idx_user_id_status_created (`user_id`, `status`, `created`);"
	if r.SuggestedDDL != want {
		t.Fatalf("DDL:\n got  %s\n want %s", r.SuggestedDDL, want)
	}
	if !strings.Contains(r.Diagnosis, "全表扫描约 3,000,000 行且无可用索引") || r.ExplainResult == "" || len(ex.calls) != 1 || ex.calls[0] != "order|select * from t_order where user_id = 1 and status in (1,2) and created > '2024-01-01' and updated > 5" {
		t.Fatalf("%+v %v", r, ex.calls)
	}
	// 详情页解析执行计划
	rows, cols := parseExplain(r.ExplainResult)
	if len(rows) != 1 || strings.Join(cols, ",") != "id,select_type,table,type,rows,extra" {
		t.Fatalf("解析执行计划: %v %v", rows, cols)
	}
}

func TestDiagnoseExplainOtherPlans(t *testing.T) {
	plan := func(m map[string]any) *fakeExplainer {
		return &fakeExplainer{res: &ExplainResult{Plan: []map[string]any{m}}}
	}
	// 有候选索引但没用上 → 索引失效，交给研发
	r := diagnose("select * from t where a = 1", Metrics{}, plan(map[string]any{"table": "t", "type": "ALL", "possible_keys": "idx_a", "rows": float64(50000)}))
	if r.Category != CatIndexInvalid || r.OwnerSide != SideDev || !strings.Contains(r.Diagnosis, "候选索引(idx_a)") {
		t.Errorf("候选索引: %+v", r)
	}
	// 全索引扫描
	r = diagnose("select a from t where a > 1", Metrics{}, plan(map[string]any{"table": "t", "type": "index", "key": "idx_a", "rows": int64(200000)}))
	if r.Category != CatFullScanBiz || !strings.Contains(r.Diagnosis, "全索引扫描约 200,000 行") {
		t.Errorf("全索引扫描: %+v", r)
	}
	// filesort
	r = diagnose("select a from t where b = 1 order by c", Metrics{}, plan(map[string]any{"table": "t", "type": "ref", "key": "idx_b", "rows": int64(20000), "extra": "Using where; Using filesort"}))
	if r.Category != CatSortGroup || !strings.Contains(r.Diagnosis, "Using filesort") {
		t.Errorf("filesort: %+v", r)
	}
	// 相关子查询
	r = diagnose("select a from t where b in (select c from s where s.x = t.x)", Metrics{}, plan(map[string]any{"table": "s", "select_type": "DEPENDENT SUBQUERY", "type": "ref", "key": "k", "rows": int64(5000)}))
	if r.Category != CatSubqueryJoin {
		t.Errorf("相关子查询: %+v", r)
	}
	// 派生表跳过；小表不报
	r = diagnose("select a from t where b = 1", Metrics{}, plan(map[string]any{"table": "<derived2>", "type": "ALL", "rows": int64(9999999)}))
	if r.Category != CatOther {
		t.Errorf("派生表: %+v", r)
	}
	r = diagnose("select a from t where b = 1", Metrics{}, plan(map[string]any{"table": "t", "type": "ALL", "rows": int64(500)}))
	if r.Category != CatOther {
		t.Errorf("小表: %+v", r)
	}
	// 隐式转换告警
	ex := &fakeExplainer{res: &ExplainResult{Plan: []map[string]any{{"table": "t", "type": "ref", "key": "k", "rows": int64(10)}},
		Warnings: []string{"Cannot use range access on index 'idx' due to type or collation conversion on field 'phone'"}}}
	r = diagnose("select a from t where phone = 138", Metrics{}, ex)
	if r.Category != CatIndexInvalid || !strings.Contains(r.Diagnosis, "隐式类型/字符集转换") {
		t.Errorf("隐式转换: %+v", r)
	}
}

func TestDiagnoseExplainSkipsAndFailures(t *testing.T) {
	ex := &fakeExplainer{err: errors.New("Unknown database")}
	r := diagnose("select * from t where a = 1", Metrics{}, ex)
	if !strings.Contains(r.Diagnosis, "EXPLAIN 诊断异常：Unknown database") {
		t.Errorf("EXPLAIN 失败应记录: %+v", r)
	}
	// 参数化文本不 EXPLAIN
	ex = &fakeExplainer{}
	r = Diagnose(context.Background(), ex, DiagnoseInput{Instance: &model.Instance{}, Product: ProdMySQL, SQL: "select * from t where a = ?", SampleReal: false, AutoExplain: true})
	if len(ex.calls) != 0 || !strings.Contains(r.Diagnosis, "仅有参数化SQL文本，未执行 EXPLAIN") {
		t.Errorf("参数化文本: %+v %v", r, ex.calls)
	}
	// 关闭自动 EXPLAIN / 非 MySQL 系 / 非查询语句 / 多语句 都不 EXPLAIN
	for name, in := range map[string]DiagnoseInput{
		"关闭":     {Product: ProdMySQL, SQL: "select 1", SampleReal: true, AutoExplain: false},
		"分析型":    {Product: ProdSelectDB, SQL: "select 1", SampleReal: true, AutoExplain: true},
		"insert": {Product: ProdMySQL, SQL: "insert into t values (1)", SampleReal: true, AutoExplain: true},
		"多语句":    {Product: ProdMySQL, SQL: "select 1; drop table t", SampleReal: true, AutoExplain: true},
	} {
		ex = &fakeExplainer{res: &ExplainResult{}}
		in.Instance = &model.Instance{}
		Diagnose(context.Background(), ex, in)
		if len(ex.calls) != 0 {
			t.Errorf("%s 不应 EXPLAIN", name)
		}
	}
	// 末尾分号去掉后再 EXPLAIN
	ex = &fakeExplainer{res: &ExplainResult{}}
	Diagnose(context.Background(), ex, DiagnoseInput{Instance: &model.Instance{}, Product: ProdMySQL, DB: "d", SQL: "select 1 from t where a=1;  ", SampleReal: true, AutoExplain: true})
	if len(ex.calls) != 1 || ex.calls[0] != "d|select 1 from t where a=1" {
		t.Errorf("%v", ex.calls)
	}
}

func TestCommaIntAndRound(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := commaInt(in); got != want {
			t.Errorf("commaInt(%d)=%s", in, got)
		}
	}
	if round(2.345, 2) != 2.35 || round(-1.5, 0) != -2 || round(0.04, 1) != 0 {
		t.Error("round")
	}
}
