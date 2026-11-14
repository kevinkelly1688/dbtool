// @author: kevinkelly
// @license: Apache Licence
// @file: mask_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"dbtool/internal/engine"
	"dbtool/internal/goinception"
	"dbtool/internal/masking"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 结果集：id, name, p(来自 customers.phone 的别名)
type maskEngine struct {
	fakeEngine
	cols []string
	data [][]any
}

func (e *maskEngine) Query(_ context.Context, sql string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	e.gotSQL, e.gotOpt = sql, opt
	rows := make([][]any, len(e.data))
	for i := range e.data { // 每次返回副本，避免测试间互相污染
		rows[i] = append([]any(nil), e.data[i]...)
	}
	return &engine.ResultSet{ColumnList: e.cols, Rows: rows}, nil
}

func maskRepo() *fakeRepo {
	r := newRepo()
	r.cfg[settings.KeyQueryMaskEnabled] = "true"
	r.maskCols = []model.DataMaskingColumn{{ColumnID: 1, Active: true, InstanceID: 1, TableSchema: "shop", TableName_: "customers", ColumnName: "phone",
		Kind: "regex", RuleRegex: `^(\d{3})(\d{4})(\d{4})$`, HideGroup: 2}}
	return r
}

func newMaskSvc(r *fakeRepo, e *maskEngine, c *fakeTreeClient) *Service {
	ext := NewAutoExtractor(r)
	if c != nil {
		r.cfg[settings.KeyGoInceptionHost] = "10.1.1.1"
		ext.NewClient = func(goinception.Config) InceptionClient { return c }
	}
	s := NewService(r, func(string, engine.Conn) (engine.Engine, error) { return e, nil }, NewPrivAuthorizer(r, ext))
	s.SetSelectListProvider(ext)
	return s
}

var customerData = [][]any{{201, "王小明", "13812345678"}, {202, "李华", "13987654321"}, {203, "无电话", nil}}

func TestMaskSyntaxTree(t *testing.T) {
	r := maskRepo()
	e := &maskEngine{cols: []string{"id", "name", "p"}, data: customerData}
	c := &fakeTreeClient{list: []masking.SelectColumn{
		{Index: 0, Field: "id", Table: "customers", Schema: "shop"},
		{Index: 1, Field: "name", Table: "customers", Schema: "shop"},
		{Index: 2, Field: "phone", Table: "customers", Schema: "shop", Alias: "p"}, // 别名 p 来自 phone
	}}
	res, err := newMaskSvc(r, e, c).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id, name, phone as p from customers"})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{{201, "王小明", "138****5678"}, {202, "李华", "139****4321"}, {203, "无电话", nil}}
	if !reflect.DeepEqual(res.Rows, want) || !reflect.DeepEqual(res.MaskedColumns, []string{"p"}) || res.MaskMode != "syntax-tree" {
		t.Errorf("语法树脱敏: rows=%v masked=%v mode=%q", res.Rows, res.MaskedColumns, res.MaskMode)
	}
	// 交给 goInception 的是实际执行的语句（已改写 LIMIT）
	if c.gotSQL != e.gotSQL {
		t.Errorf("应解析实际执行的语句: %q vs %q", c.gotSQL, e.gotSQL)
	}
	if l := r.logs[0]; !l.HitRule || !l.Masking {
		t.Errorf("dbt_query_log 应记录命中规则与已脱敏: %+v", l)
	}
}

func TestMaskNoHitStillRecorded(t *testing.T) {
	r := maskRepo()
	e := &maskEngine{cols: []string{"id", "name"}, data: [][]any{{1, "a"}}}
	c := &fakeTreeClient{list: []masking.SelectColumn{{Index: 0, Field: "id", Table: "customers", Schema: "shop"}, {Index: 1, Field: "name", Table: "customers", Schema: "shop"}}}
	res, err := newMaskSvc(r, e, c).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id, name from customers"})
	if err != nil || len(res.MaskedColumns) != 0 {
		t.Fatalf("没有命中不应脱敏: %v %v", err, res)
	}
	if l := r.logs[0]; l.HitRule || !l.Masking {
		t.Errorf("未命中: hit_rule=false masking=true, 实际 %+v", l)
	}
}

func TestMaskFallbackColumnName(t *testing.T) {
	r := maskRepo()
	e := &maskEngine{cols: []string{"id", "phone"}, data: [][]any{{201, "13812345678"}}}
	res, err := newMaskSvc(r, e, nil).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id, phone from customers"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][1] != "138****5678" || res.MaskMode != "column-name" || res.Notice == "" {
		t.Errorf("未配置 goInception 应降级为列名脱敏并提示: %v mode=%q notice=%q", res.Rows, res.MaskMode, res.Notice)
	}
}

// 存储函数 / 自定义函数的返回值可能来自函数体里读到的任何字段，语句文本看不出来：结果列没有来源时，整列按最严规则脱敏。
// 内置函数（concat 等）不受影响。语法树方式和降级的列名方式结果一致。
func TestMaskUserFunction(t *testing.T) {
	const sql = "select id, fn_get_phone(1) as p, concat(name, 'x') as n from customers"
	for _, withTree := range []bool{true, false} {
		r := maskRepo()
		e := &maskEngine{cols: []string{"id", "p", "n"}, data: [][]any{{201, "13812345678", "王小明x"}}}
		var c *fakeTreeClient
		if withTree {
			c = &fakeTreeClient{list: []masking.SelectColumn{{Index: 0, Field: "id", Table: "customers", Schema: "shop"}}} // 函数列与表达式列没有来源
		}
		res, err := newMaskSvc(r, e, c).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: sql})
		if err != nil {
			t.Fatalf("withTree=%v: %v", withTree, err)
		}
		want := []any{201, "138****5678", "王小明x"}
		if !reflect.DeepEqual(res.Rows[0], want) || !reflect.DeepEqual(res.MaskedColumns, []string{"p"}) {
			t.Errorf("withTree=%v: row=%v masked=%v mode=%q", withTree, res.Rows[0], res.MaskedColumns, res.MaskMode)
		}
		if l := r.logs[0]; !l.HitRule || !l.Masking {
			t.Errorf("withTree=%v: dbt_query_log 应记录命中规则与已脱敏: %+v", withTree, l)
		}
	}
	// 实例没有配置任何脱敏字段：不受这条规则影响
	r := maskRepo()
	r.maskCols = nil
	e := &maskEngine{cols: []string{"p"}, data: [][]any{{"13812345678"}}}
	res, err := newMaskSvc(r, e, nil).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select fn_get_phone(1) as p"})
	if err != nil || res.Rows[0][0] != "13812345678" || len(res.MaskedColumns) != 0 {
		t.Errorf("没有脱敏配置不应脱敏: %v %v", err, res)
	}
}

// PolarDB-X 不调用 goInception（即使已配置，调用即报错）：普通列与表达式列都按列名 / 表达式引用保守匹配，且没有降级提示。
func TestMaskPolarDBXConservative(t *testing.T) {
	for _, tp := range []string{"polardb-x", "polardbx"} {
		r := maskRepo()
		r.insts[1].DBType = tp
		e := &maskEngine{cols: []string{"id", "phone", "c"}, data: [][]any{{201, "13812345678", "13812345678"}}}
		c := &fakeTreeClient{listEr: errors.New("PolarDB-X 不应调用 goInception")}
		res, err := newMaskSvc(r, e, c).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id, phone, concat(phone, '') as c from customers"})
		if err != nil {
			t.Fatalf("%s: %v", tp, err)
		}
		if res.MaskMode != "column-name" || res.Rows[0][0] != 201 || res.Rows[0][1] != "138****5678" || res.Rows[0][2] != "138****5678" {
			t.Errorf("%s: 应按列名保守匹配: rows=%v mode=%q masked=%v", tp, res.Rows, res.MaskMode, res.MaskedColumns)
		}
		if res.Notice != "" {
			t.Errorf("%s: 保守匹配是预期行为，不应带降级提示: %q", tp, res.Notice)
		}
		if l := r.logs[0]; !l.HitRule || !l.Masking {
			t.Errorf("%s: dbt_query_log 应记录命中规则与已脱敏: %+v", tp, l)
		}
	}
}

func TestMaskFailureHandling(t *testing.T) {
	list := []masking.SelectColumn{{Index: 0, Field: "phone", Table: "customers", Schema: "shop"}}
	e := func() *maskEngine { return &maskEngine{cols: []string{"phone"}, data: [][]any{{"13812345678"}}} }
	req := Request{InstanceID: 1, DB: "shop", SQL: "select phone from customers"}

	// goInception 出错 + query_check 开启：阻断，不返回数据
	r := maskRepo()
	r.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	_, err := newMaskSvc(r, e(), &fakeTreeClient{listEr: errors.New("boom")}).Execute(context.Background(), admin, req)
	if code(err) != "masking_error" {
		t.Errorf("query_check 开启应阻断: %v", err)
	}
	if len(r.logs) != 0 {
		t.Error("被阻断的查询不应写 dbt_query_log")
	}

	// query_check 关闭：放行未脱敏数据并提示，日志标记未脱敏
	r = maskRepo()
	res, err := newMaskSvc(r, e(), &fakeTreeClient{listEr: errors.New("boom")}).Execute(context.Background(), admin, req)
	if err != nil || res.Rows[0][0] != "13812345678" || res.Notice == "" || r.logs[0].Masking {
		t.Errorf("query_check 关闭应放行并标记未脱敏: %v %v", err, res)
	}

	// 列序号越界：脱敏没法定位列，必须当作失败处理
	r = maskRepo()
	r.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	bad := &fakeTreeClient{list: []masking.SelectColumn{{Index: 7, Field: "phone", Table: "customers", Schema: "shop"}}}
	if _, err := newMaskSvc(r, e(), bad).Execute(context.Background(), admin, req); code(err) != "masking_error" {
		t.Errorf("越界序号应阻断: %v", err)
	}
	_ = list
}

func TestMaskStrictModeBlocksWithoutInception(t *testing.T) {
	r := maskRepo() // 未配置 goInception，query_check 关闭
	e := &maskEngine{cols: []string{"phone"}, data: [][]any{{"13812345678"}}}
	ext := NewAutoExtractor(r)
	ext.Strict = true
	s := NewService(r, func(string, engine.Conn) (engine.Engine, error) { return e, nil }, NewPrivAuthorizer(r, ext))
	s.SetSelectListProvider(ext)
	if _, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select phone from customers"}); code(err) != "masking_error" {
		t.Errorf("严格模式未配置 goInception 应阻断（不论 query_check）: %v", err)
	}
}

func TestMaskSkipped(t *testing.T) {
	list := []masking.SelectColumn{{Index: 0, Field: "phone", Table: "customers", Schema: "shop"}}
	req := Request{InstanceID: 1, DB: "shop", SQL: "select phone from customers"}
	e := func() *maskEngine { return &maskEngine{cols: []string{"phone"}, data: [][]any{{"13812345678"}}} }

	// 总开关关闭
	r := maskRepo()
	r.cfg[settings.KeyQueryMaskEnabled] = "false"
	res, _ := newMaskSvc(r, e(), &fakeTreeClient{list: list}).Execute(context.Background(), admin, req)
	if res.Rows[0][0] != "13812345678" || r.logs[0].Masking {
		t.Errorf("data_masking 关闭不应脱敏: %v", res.Rows)
	}
	// 没有配置任何脱敏字段：不调用 goInception，也不报错
	r = maskRepo()
	r.maskCols = nil
	c := &fakeTreeClient{listEr: errors.New("不应被调用")}
	res, err := newMaskSvc(r, e(), c).Execute(context.Background(), admin, req)
	if err != nil || res.Rows[0][0] != "13812345678" || !r.logs[0].Masking || r.logs[0].HitRule {
		t.Errorf("无配置: %v %v", err, res)
	}
	// 非 SELECT（EXPLAIN）不脱敏
	r = maskRepo()
	if _, err := newMaskSvc(r, e(), &fakeTreeClient{listEr: errors.New("不应被调用")}).Execute(context.Background(), admin,
		Request{InstanceID: 1, DB: "shop", SQL: "explain select phone from customers"}); err != nil {
		t.Errorf("EXPLAIN 不应触发脱敏: %v", err)
	}
	if r.logs[0].Masking {
		t.Error("EXPLAIN 的 masking 应为 false")
	}
}

func TestMaskDefaultThreeSegmentRule(t *testing.T) {
	r := maskRepo()
	r.maskCols[0].Kind, r.maskCols[0].RuleRegex, r.maskCols[0].HideGroup = "", "", 0
	r.maskCols[0].RuleType = 100 // 内置默认三段式规则（隐藏中段）
	e := &maskEngine{cols: []string{"phone"}, data: [][]any{{"13812345678"}}}
	c := &fakeTreeClient{list: []masking.SelectColumn{{Index: 0, Field: "phone", Table: "customers", Schema: "shop"}}}
	res, err := newMaskSvc(r, e, c).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select phone from customers"})
	if err != nil || res.Rows[0][0] != "138****5678" {
		t.Errorf("默认三段式规则: %v %v", err, res)
	}
}
