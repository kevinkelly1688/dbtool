// @author: kevinkelly
// @license: Apache Licence
// @file: view_mask_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"dbtool/internal/engine"
	"dbtool/internal/engine/mssql"
	"dbtool/internal/engine/postgres/pgtest"
	"dbtool/internal/goinception"
	"dbtool/internal/masking"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 视图的数据脱敏：配置写在基表字段上，经过视图改名、套表达式、套另一个视图之后，要顺着视图定义追到基表字段（见 viewexp.go）。

// viewEngine 是能读视图定义的 MySQL 系引擎替身：defs 按小写视图名给出定义，calls 记下每次目录查询带的名字。
type viewEngine struct {
	maskEngine
	defs  map[string]engine.ViewDef
	err   error
	calls [][]engine.ViewRef
	// unsupportedFrom > 0：第 N 次及以后的目录查询报“没有视图定义目录”
	unsupportedFrom int
}

func (e *viewEngine) ViewDefs(_ context.Context, _ string, refs []engine.ViewRef) ([]engine.ViewDef, error) {
	e.calls = append(e.calls, append([]engine.ViewRef(nil), refs...))
	if e.unsupportedFrom > 0 && len(e.calls) >= e.unsupportedFrom {
		return nil, engine.ErrViewDefsUnsupported
	}
	if e.err != nil {
		return nil, e.err
	}
	return lookupViews(e.defs, refs), nil
}

func lookupViews(defs map[string]engine.ViewDef, refs []engine.ViewRef) []engine.ViewDef {
	var out []engine.ViewDef
	for _, r := range refs {
		d, ok := defs[strings.ToLower(r.Name)]
		if ok && (r.Schema == "" || strings.EqualFold(r.Schema, d.Schema)) && (r.Catalog == "" || strings.EqualFold(r.Catalog, d.Catalog)) {
			out = append(out, d)
		}
	}
	return out
}

func viewDefsOf(ds ...engine.ViewDef) map[string]engine.ViewDef {
	m := map[string]engine.ViewDef{}
	for _, d := range ds {
		m[strings.ToLower(d.Name)] = d
	}
	return m
}

func viewDef(schema, name, text string) engine.ViewDef {
	return engine.ViewDef{Schema: schema, Name: name, Definition: text}
}

// newViewSvc 同 newMaskSvc，但引擎可以是任意替身。
func newViewSvc(r *fakeRepo, e engine.Engine, c *fakeTreeClient) *Service {
	ext := NewAutoExtractor(r)
	if c != nil {
		r.cfg[settings.KeyGoInceptionHost] = "10.1.1.1"
		ext.NewClient = func(goinception.Config) InceptionClient { return c }
	}
	s := NewService(r, func(string, engine.Conn) (engine.Engine, error) { return e, nil }, NewPrivAuthorizer(r, ext))
	s.SetSelectListProvider(ext)
	return s
}

// viewRepo 的脱敏配置：shop.customers.phone，保留前 3 后 4（13812345678 → 138***5678）。
func viewRepo() *fakeRepo {
	r := maskRepo()
	r.maskCols = []model.DataMaskingColumn{keepCfg("shop", "customers", "phone", 3, 4)}
	return r
}

// MySQL 存的视图定义：规范化的 SELECT，列带 AS 别名、对象名带库名。
const (
	mobileViewDef = "select `shop`.`customers`.`id` AS `id`,`shop`.`customers`.`phone` AS `mobile` from `shop`.`customers`"
	phoneViewDef  = "select `shop`.`customers`.`id` AS `id`,`shop`.`customers`.`phone` AS `phone` from `shop`.`customers`"
)

func viewSources(table string, fields ...string) []masking.SelectColumn {
	out := make([]masking.SelectColumn, len(fields))
	for i, f := range fields {
		out[i] = masking.SelectColumn{Index: i, Field: f, Table: table, Schema: "shop"}
	}
	return out
}

func TestMaskViewRenamedColumn(t *testing.T) {
	cases := []struct {
		name string
		tree *fakeTreeClient
		want string // 脱敏模式
	}{
		{"goInception 把来源报成视图本身", &fakeTreeClient{list: viewSources("v_customers", "id", "mobile")}, "syntax-tree"},
		{"没有 goInception，按列名保守匹配", nil, "column-name"},
	}
	for _, c := range cases {
		r := viewRepo()
		e := &viewEngine{maskEngine: maskEngine{cols: []string{"id", "mobile"}, data: [][]any{{201, "13812345678"}}}, defs: viewDefsOf(viewDef("shop", "v_customers", mobileViewDef))}
		res, err := newViewSvc(r, e, c.tree).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id, mobile from v_customers"})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := []any{201, "138***5678"}; !reflect.DeepEqual(res.Rows[0], want) || !reflect.DeepEqual(res.MaskedColumns, []string{"mobile"}) || res.MaskMode != c.want {
			t.Errorf("%s: row=%v masked=%v mode=%q", c.name, res.Rows[0], res.MaskedColumns, res.MaskMode)
		}
		if want := [][]engine.ViewRef{{{Name: "v_customers"}}, {{Schema: "shop", Name: "customers"}}}; !reflect.DeepEqual(e.calls, want) {
			t.Errorf("%s: 先带着语句里的名字查，再查视图定义里引用的名字（看有没有嵌套视图）: %+v", c.name, e.calls)
		}
		if l := r.logs[0]; !l.HitRule || !l.Masking || strings.Contains(res.Notice, "视图") {
			t.Errorf("%s: 日志 %+v 提示 %q", c.name, l, res.Notice)
		}
	}
}

// 来源报成视图本身、视图列与基表字段同名：只看精确来源会把它当成一张叫 v_customers 的普通表，配置对不上而漏掉。
func TestMaskViewSameNameColumn(t *testing.T) {
	r := viewRepo()
	e := &viewEngine{maskEngine: maskEngine{cols: []string{"id", "phone"}, data: [][]any{{201, "13812345678"}}}, defs: viewDefsOf(viewDef("shop", "v_customers", phoneViewDef))}
	c := &fakeTreeClient{list: viewSources("v_customers", "id", "phone")}
	res, err := newViewSvc(r, e, c).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id, phone from v_customers"})
	if err != nil || !reflect.DeepEqual(res.Rows[0], []any{201, "138***5678"}) {
		t.Fatalf("视图里的同名列应脱敏: %v %v", err, res)
	}
	// 查的是普通表（目录里不是视图）：来源精确，行为与以前完全一样，id 不会被误伤
	e2 := &viewEngine{maskEngine: maskEngine{cols: []string{"id", "phone"}, data: [][]any{{201, "13812345678"}}}}
	c2 := &fakeTreeClient{list: viewSources("customers", "id", "phone")}
	res, err = newViewSvc(viewRepo(), e2, c2).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id, phone from customers"})
	if err != nil || !reflect.DeepEqual(res.Rows[0], []any{201, "138***5678"}) || !reflect.DeepEqual(res.MaskedColumns, []string{"phone"}) {
		t.Errorf("普通表: %v %v", err, res)
	}
}

func TestMaskViewNestedExpressionAndFunction(t *testing.T) {
	cases := []struct {
		name  string
		defs  []engine.ViewDef
		sql   string
		cols  []string
		data  []any
		want  []any
		calls int
	}{
		{"视图套视图", []engine.ViewDef{
			viewDef("shop", "v2", "select `shop`.`v1`.`mobile` AS `m2`,`shop`.`v1`.`id` AS `id` from `shop`.`v1`"),
			viewDef("shop", "v1", mobileViewDef),
		}, "select id, m2 from v2", []string{"id", "m2"}, []any{201, "13812345678"}, []any{201, "138***5678"}, 2},
		{"视图里的表达式列", []engine.ViewDef{
			viewDef("shop", "v", "select `shop`.`customers`.`id` AS `id`,concat(`shop`.`customers`.`phone`,'') AS `p2` from `shop`.`customers`"),
		}, "select id, p2 from v", []string{"id", "p2"}, []any{201, "13812345678"}, []any{201, "138***5678"}, 1},
		{"视图里调用自定义函数", []engine.ViewDef{
			viewDef("shop", "v", "select `shop`.`customers`.`id` AS `id`,`shop`.`fn_get_phone`(`shop`.`customers`.`id`) AS `p` from `shop`.`customers`"),
		}, "select id, p from v", []string{"id", "p"}, []any{201, "13812345678"}, []any{201, "138***5678"}, 1},
		{"视图不含敏感字段不误伤", []engine.ViewDef{
			viewDef("shop", "v", "select `shop`.`customers`.`id` AS `id`,`shop`.`customers`.`age` AS `age` from `shop`.`customers`"),
		}, "select id, age from v", []string{"id", "age"}, []any{201, "13812345678"}, []any{201, "13812345678"}, 1},
		{"视图名限定列、select *、JOIN 另一张表", []engine.ViewDef{viewDef("shop", "v_customers", mobileViewDef)},
			"select v_customers.mobile, o.id from v_customers join orders o on o.cid = v_customers.id", []string{"mobile", "id"}, []any{"13812345678", 9}, []any{"138***5678", 9}, 1},
	}
	for _, c := range cases {
		e := &viewEngine{maskEngine: maskEngine{cols: c.cols, data: [][]any{c.data}}, defs: viewDefsOf(c.defs...)}
		res, err := newViewSvc(viewRepo(), e, nil).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: c.sql})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(res.Rows[0], c.want) {
			t.Errorf("%s: row=%v masked=%v，期望 %v", c.name, res.Rows[0], res.MaskedColumns, c.want)
		}
		if len(e.calls) < c.calls {
			t.Errorf("%s: 目录查询 %d 次，期望至少 %d 次: %+v", c.name, len(e.calls), c.calls, e.calls)
		}
	}
	// 嵌套视图第二轮带着库名查（视图定义里的名字是 `shop`.`v1`）
	e := &viewEngine{maskEngine: maskEngine{cols: []string{"m2"}, data: [][]any{{"13812345678"}}}, defs: viewDefsOf(
		viewDef("shop", "v2", "select `shop`.`v1`.`mobile` AS `m2` from `shop`.`v1`"), viewDef("shop", "v1", mobileViewDef))}
	if _, err := newViewSvc(viewRepo(), e, nil).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select m2 from v2"}); err != nil {
		t.Fatal(err)
	}
	if want := [][]engine.ViewRef{{{Name: "v2"}}, {{Schema: "shop", Name: "v1"}}, {{Schema: "shop", Name: "customers"}}}; !reflect.DeepEqual(e.calls, want) {
		t.Errorf("嵌套视图每深一层查一轮: %+v", e.calls)
	}
}

// 视图互相引用（定义里成环）：每个名字只查一次，不会死循环，也不算失败。
func TestMaskViewCycle(t *testing.T) {
	e := &viewEngine{maskEngine: maskEngine{cols: []string{"a"}, data: [][]any{{"x"}}}, defs: viewDefsOf(
		viewDef("shop", "v1", "select `shop`.`v2`.`a` AS `a` from `shop`.`v2`"),
		viewDef("shop", "v2", "select `shop`.`v1`.`a` AS `a` from `shop`.`v1`"))}
	res, err := newViewSvc(viewRepo(), e, nil).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select a from v1"})
	if err != nil || res.Rows[0][0] != "x" || strings.Contains(res.Notice, "视图") {
		t.Fatalf("成环的视图: %v %v", err, res)
	}
	if len(e.calls) != 2 {
		t.Errorf("成环时每个名字只查一次: %+v", e.calls)
	}
}

// 视图定义读不到（没有 SHOW VIEW 权限）按脱敏失败处理：默认放行并提示（其余的列已按配置脱敏），开启 query.block_on_mask_error 阻断。
func TestMaskViewDefinitionUnreadable(t *testing.T) {
	newEnv := func() (*fakeRepo, *viewEngine, *fakeTreeClient) {
		r := viewRepo()
		e := &viewEngine{maskEngine: maskEngine{cols: []string{"phone", "mobile"}, data: [][]any{{"13812345678", "13812345678"}}},
			defs: viewDefsOf(viewDef("shop", "v_customers", ""))}
		return r, e, &fakeTreeClient{list: viewSources("v_customers", "phone", "mobile")}
	}
	req := Request{InstanceID: 1, DB: "shop", SQL: "select phone, mobile from v_customers"}

	r, e, c := newEnv()
	res, err := newViewSvc(r, e, c).Execute(context.Background(), admin, req)
	if err != nil {
		t.Fatalf("默认应放行: %v", err)
	}
	if res.Rows[0][0] != "138***5678" {
		t.Errorf("同名列仍应按配置脱敏（经过读不到定义的视图也不能当普通表）: %v", res.Rows[0])
	}
	if res.Rows[0][1] != "13812345678" {
		t.Errorf("定义读不到，改了名的列没法确定: %v", res.Rows[0])
	}
	if !strings.Contains(res.Notice, "v_customers") || !strings.Contains(res.Notice, "block_on_mask_error") {
		t.Errorf("应提示哪个视图读不到定义: %q", res.Notice)
	}
	if l := r.logs[0]; !l.Masking || !l.HitRule {
		t.Errorf("日志应记录已做的脱敏: %+v", l)
	}
	if !reflect.DeepEqual(res.MaskedColumns, []string{"phone"}) {
		t.Errorf("MaskedColumns=%v", res.MaskedColumns)
	}

	r, e, c = newEnv()
	r.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	if _, err := newViewSvc(r, e, c).Execute(context.Background(), admin, req); code(err) != "masking_error" {
		t.Errorf("开启 query.block_on_mask_error 应阻断: %v", err)
	}
	if len(r.logs) != 0 {
		t.Error("被阻断的查询不写 dbt_query_log")
	}
}

func TestMaskViewCatalogErrors(t *testing.T) {
	req := Request{InstanceID: 1, DB: "shop", SQL: "select phone from customers"}
	newEnv := func(err error) (*fakeRepo, *viewEngine, *fakeTreeClient) {
		return viewRepo(), &viewEngine{maskEngine: maskEngine{cols: []string{"phone"}, data: [][]any{{"13812345678"}}}, err: err},
			&fakeTreeClient{list: viewSources("customers", "phone")}
	}
	// 目录查询失败（权限不足、超时、连接错误…）：同样按脱敏失败处理，普通列照常脱敏
	r, e, c := newEnv(errors.New("SELECT command denied to user 'q'@'%' for table 'VIEWS'"))
	res, err := newViewSvc(r, e, c).Execute(context.Background(), admin, req)
	if err != nil || res.Rows[0][0] != "138***5678" || !strings.Contains(res.Notice, "读取视图定义失败") {
		t.Errorf("默认应放行并提示: %v %v", err, res)
	}
	r, e, c = newEnv(errors.New("boom"))
	r.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	if _, err := newViewSvc(r, e, c).Execute(context.Background(), admin, req); code(err) != "masking_error" {
		t.Errorf("开启 query.block_on_mask_error 应阻断: %v", err)
	}
	// 这个库没有视图定义目录：不展开，不算失败，也没有提示
	r, e, c = newEnv(fmt.Errorf("%w: 表不存在", engine.ErrViewDefsUnsupported))
	r.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	res, err = newViewSvc(r, e, c).Execute(context.Background(), admin, req)
	if err != nil || res.Rows[0][0] != "138***5678" || res.Notice != "" {
		t.Errorf("目录不可用不应当失败: %v %v", err, res)
	}
}

// 没有配置任何脱敏字段、或脱敏总开关关闭：不去目标库查视图目录。
func TestMaskViewNoLookupWithoutMasking(t *testing.T) {
	req := Request{InstanceID: 1, DB: "shop", SQL: "select phone from v_customers"}
	newEnv := func() *viewEngine {
		return &viewEngine{maskEngine: maskEngine{cols: []string{"phone"}, data: [][]any{{"13812345678"}}}, defs: viewDefsOf(viewDef("shop", "v_customers", phoneViewDef))}
	}
	r := viewRepo()
	r.maskCols = nil
	e := newEnv()
	if _, err := newViewSvc(r, e, nil).Execute(context.Background(), admin, req); err != nil || len(e.calls) != 0 {
		t.Errorf("没有脱敏配置不该查视图目录: %v %+v", err, e.calls)
	}
	r = viewRepo()
	r.cfg[settings.KeyQueryMaskEnabled] = "false"
	e = newEnv()
	if _, err := newViewSvc(r, e, nil).Execute(context.Background(), admin, req); err != nil || len(e.calls) != 0 {
		t.Errorf("脱敏总开关关闭不该查视图目录: %v %+v", err, e.calls)
	}
}

// 前置 SET 把视图里的值带进变量：变量要像别名一样被污染。
func TestMaskViewThroughUserVariable(t *testing.T) {
	e := &viewEngine{maskEngine: maskEngine{cols: []string{"p"}, data: [][]any{{"13812345678"}}}, defs: viewDefsOf(viewDef("shop", "v_customers", mobileViewDef))}
	res, err := newViewSvc(viewRepo(), e, nil).Execute(context.Background(), admin,
		Request{InstanceID: 1, DB: "shop", SQL: "set @m := (select mobile from v_customers limit 1); select @m as p"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][0] != "138***5678" {
		t.Errorf("变量带出视图里的值: %v mode=%q", res.Rows[0], res.MaskMode)
	}
}

// 导出工单走同一套脱敏：视图读不到定义时失败，导出中止（不管 query.block_on_mask_error）。
func TestMaskExportViewUnreadable(t *testing.T) {
	r := viewRepo()
	e := &viewEngine{maskEngine: maskEngine{}, defs: viewDefsOf(viewDef("shop", "v_customers", ""))}
	s := newViewSvc(r, e, nil)
	rs := &engine.ResultSet{ColumnList: []string{"mobile"}, Rows: [][]any{{"13812345678"}}}
	if _, err := s.MaskExport(context.Background(), r.insts[1], "shop", "select mobile from v_customers", e, rs); err == nil {
		t.Fatal("导出遇到读不到定义的视图应失败，不能导出未脱敏的数据")
	}
	// 读得到定义：正常脱敏
	e.defs = viewDefsOf(viewDef("shop", "v_customers", mobileViewDef))
	rs = &engine.ResultSet{ColumnList: []string{"mobile"}, Rows: [][]any{{"13812345678"}}}
	cols, err := s.MaskExport(context.Background(), r.insts[1], "shop", "select mobile from v_customers", e, rs)
	if err != nil || !reflect.DeepEqual(cols, []string{"mobile"}) || rs.Rows[0][0] != "138***5678" {
		t.Errorf("导出脱敏: %v %v %v", cols, err, rs.Rows)
	}
}

// ---- 展开本身的上限与细节 ----

func TestExpandViewsLimits(t *testing.T) {
	s := newViewSvc(viewRepo(), &viewEngine{}, nil)
	run := func(defs map[string]engine.ViewDef, sql string) (*viewExpansion, error, *viewEngine) {
		e := &viewEngine{defs: defs}
		x, err := s.expandViews(context.Background(), e, "mysql", "shop", sql)
		return x, err, e
	}
	// 嵌套超过上限：失败，已展开的部分仍带回来
	chain := map[string]engine.ViewDef{}
	for i := 0; i < maxViewDepth+2; i++ {
		chain[fmt.Sprintf("v%d", i)] = viewDef("shop", fmt.Sprintf("v%d", i), fmt.Sprintf("select `a` AS `a` from `shop`.`v%d`", i+1))
	}
	x, err, _ := run(chain, "select a from v0")
	var ve *viewDefError
	if !errors.As(err, &ve) || ve.Reason == "" || x == nil || len(x.bodies) != maxViewDepth {
		t.Errorf("嵌套超过 %d 层应失败并保留已展开的部分: x=%+v err=%v", maxViewDepth, x, err)
	}
	// 语句里的对象名太多：不查目录，直接失败
	var names []string
	for i := 0; i <= maxViewRefs; i++ {
		names = append(names, fmt.Sprintf("t%d", i))
	}
	x, err, e := run(nil, "select 1 from "+strings.Join(names, ", "))
	if !errors.As(err, &ve) || ve.Reason == "" || x != nil || len(e.calls) != 0 {
		t.Errorf("对象名超过 %d 个: x=%+v err=%v calls=%d", maxViewRefs, x, err, len(e.calls))
	}
	// 展开的视图太多
	many := map[string]engine.ViewDef{}
	names = names[:0]
	for i := 0; i <= maxViewCount; i++ {
		n := fmt.Sprintf("v%d", i)
		many[n] = viewDef("shop", n, "select `a` AS `a` from `shop`.`customers`")
		names = append(names, n)
	}
	x, err, e = run(many, "select 1 from "+strings.Join(names, ", "))
	if !errors.As(err, &ve) || ve.Reason == "" || x == nil || len(x.bodies) != maxViewCount {
		t.Errorf("视图超过 %d 个: err=%v", maxViewCount, err)
	}
	if len(e.calls) != 2 { // 101 个名字分两批查
		t.Errorf("名字应按 %d 个一批查: %d 批", viewRefChunk, len(e.calls))
	}
	// 引擎不提供视图定义：不展开，不算失败
	if x, err := s.expandViews(context.Background(), &maskEngine{}, "mysql", "shop", "select 1 from v"); x != nil || err != nil {
		t.Errorf("没有实现 engine.ViewDefiner: %v %v", x, err)
	}
	// 看不懂的定义（引号没结束）算读不到，其余的视图照常展开
	x, err, _ = run(viewDefsOf(viewDef("shop", "bad", "select 'x from t"), viewDef("shop", "good", mobileViewDef)), "select 1 from bad, good")
	if !errors.As(err, &ve) || !reflect.DeepEqual(ve.Views, []string{"shop.bad"}) || x == nil || len(x.bodies) != 1 || !x.isView("shop", "bad") || !x.isView("", "GOOD") {
		t.Errorf("看不懂的定义: x=%+v err=%v", x, err)
	}
}

// 目录在后面一轮才报“不可用”：前面已经发现的读不到定义的视图仍然要报失败。
func TestExpandViewsUnsupportedKeepsEarlierFailure(t *testing.T) {
	e := &viewEngine{unsupportedFrom: 2, defs: viewDefsOf(viewDef("shop", "v_bad", ""), viewDef("shop", "v_ok", mobileViewDef))}
	s := newViewSvc(viewRepo(), e, nil)
	x, err := s.expandViews(context.Background(), e, "mysql", "shop", "select 1 from v_bad, v_ok")
	var ve *viewDefError
	if !errors.As(err, &ve) || !reflect.DeepEqual(ve.Views, []string{"shop.v_bad"}) || ve.Reason != "" {
		t.Fatalf("应只报读不到定义的视图: %v", err)
	}
	if x == nil || len(x.bodies) != 1 || len(e.calls) != 2 {
		t.Errorf("读得到的视图仍要展开: x=%+v calls=%d", x, len(e.calls))
	}
}

func TestViewDefErrorNotice(t *testing.T) {
	e := &viewDefError{Views: []string{"shop.v1", "shop.v2"}}
	if n := e.notice(); !strings.Contains(n, "shop.v1, shop.v2") || strings.Contains(n, "读取视图定义失败") {
		t.Errorf("notice=%q", n)
	}
	e = &viewDefError{Reason: "boom"}
	if n := e.notice(); !strings.Contains(n, "读取视图定义失败") || strings.Contains(n, "boom") {
		t.Errorf("原因细节只进日志，不给用户: %q", n)
	}
	if !strings.Contains((&viewDefError{Views: []string{"v"}, Reason: "r"}).Error(), "r") {
		t.Error("Error() 要带上原因")
	}
}

func TestMSSQLViewBody(t *testing.T) {
	ok := []struct {
		name, def, body string
		cols            []string
	}{
		{"基本", "CREATE VIEW dbo.v AS SELECT a FROM t", "SELECT a FROM t", nil},
		{"方括号、OR ALTER、结尾分号", "CREATE OR ALTER VIEW [crm].[v_buyer] AS\r\nSELECT a FROM t;\r\n", "SELECT a FROM t", nil},
		{"WITH 选项", "create view v with schemabinding, encryption as select a from dbo.t", "select a from dbo.t", nil},
		{"列清单", "CREATE VIEW dbo.v ([Mobile], Id2) WITH SCHEMABINDING AS SELECT phone, id FROM dbo.t", "SELECT phone, id FROM dbo.t", []string{"Mobile", "Id2"}},
		{"头里的注释", "/* 说明 */ CREATE VIEW v -- 备注\n AS SELECT 1 AS a", "SELECT 1 AS a", nil},
		{"主体以 WITH 开头（CTE）", "CREATE VIEW v AS WITH c AS (SELECT 1 AS a) SELECT a FROM c", "WITH c AS (SELECT 1 AS a) SELECT a FROM c", nil},
	}
	for _, c := range ok {
		got, err := mssqlViewBody(c.def)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !strings.EqualFold(strings.Join(strings.Fields(got.Body), " "), c.body) || !reflect.DeepEqual(got.Columns, c.cols) {
			t.Errorf("%s: body=%q cols=%v，期望 %q %v", c.name, got.Body, got.Columns, c.body, c.cols)
		}
	}
	for _, def := range []string{"SELECT 1", "CREATE VIEW v", "CREATE PROC p AS SELECT 1", ""} {
		if _, err := viewBodyOf("mssql", viewDef("dbo", "v", def)); err == nil {
			t.Errorf("%q 应认不出", def)
		}
	}
	// 其它引擎的定义只有 SELECT：去掉结尾分号
	if b, err := viewBodyOf("pgsql", viewDef("public", "v", " SELECT 1;\n")); err != nil || b.Body != "SELECT 1" {
		t.Errorf("PostgreSQL: %+v %v", b, err)
	}
}

// ---- PostgreSQL ----

// pgViewHandler：视图 reporting.v_account 把基表的 email 改名为 contact（定义由 def 给出，nil 表示读不到）。
func pgViewHandler(def *string, gotParams *[][]string) pgtest.Handler {
	return func(sql string, params []string) *pgtest.Result {
		low := strings.ToLower(sql)
		switch {
		case strings.Contains(low, "pg_get_viewdef"):
			if gotParams != nil {
				*gotParams = append(*gotParams, params)
			}
			return &pgtest.Result{Cols: pgtest.TextCols("nspname", "relname", "def"), Rows: [][]*string{{pgtest.Str("reporting"), pgtest.Str("v_account"), def}}}
		case strings.Contains(sql, "pg_catalog.pg_attribute"):
			return &pgtest.Result{Cols: pgtest.TextCols("rel", "att", "nsp", "tbl", "col", "kind"), Rows: [][]*string{
				pgtest.Cell("16600", "1", "reporting", "v_account", "contact", "v"),
				pgtest.Cell("16600", "2", "reporting", "v_account", "id", "v"),
			}}
		case strings.Contains(low, "from reporting.v_account"):
			return &pgtest.Result{
				Cols:    []pgtest.Col{{Name: "contact", OID: 25}, {Name: "id", OID: 25}},
				Rows:    [][]*string{pgtest.Cell("ab@corp.com", "7")},
				Origins: []pgtest.Origin{{Table: 16600, Att: 1}, {Table: 16600, Att: 2}},
			}
		}
		return nil
	}
}

func TestPGMaskViewRenamedColumn(t *testing.T) {
	var params [][]string
	def := " SELECT account.email AS contact,\n    account.id\n   FROM account;"
	e := newMaskEnv(t, pgViewHandler(&def, &params), keepCfg("shop", "public.account", "email", 2, 0)) // 配置写在基表上
	res := e.run(t, pgID, "shop", "select contact, id from reporting.v_account")
	if row := res.Rows[0]; row[0] != "ab***" || row[1] != "7" {
		t.Errorf("视图把 email 改名为 contact 也应脱敏: row=%v masked=%v mode=%q notice=%v", row, res.MaskedColumns, res.MaskMode, res.Warnings)
	}
	if want := []string{`{"v_account"}`}; len(params) == 0 || !reflect.DeepEqual(params[0], want) {
		t.Errorf("目录查询的参数（数组字面量）: %q", params)
	}

	// 定义读不到（NULL）：默认放行并提示；开启 query.block_on_mask_error 阻断
	e = newMaskEnv(t, pgViewHandler(nil, nil), keepCfg("shop", "public.account", "email", 2, 0))
	res = e.run(t, pgID, "shop", "select contact, id from reporting.v_account")
	if res.Rows[0][0] != "ab@corp.com" || !strings.Contains(strings.Join(res.Warnings, "\n"), "reporting.v_account") {
		t.Errorf("定义读不到应放行并提示: row=%v warnings=%v", res.Rows[0], res.Warnings)
	}
	e.repo.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	if _, err := e.svc.Execute(context.Background(), admin, Request{InstanceID: pgID, DB: "shop", SQL: "select contact, id from reporting.v_account"}); code(err) != "masking_error" {
		t.Errorf("应阻断: %v", err)
	}
}

// ---- SQL Server ----

type mssqlViewEngine struct {
	mssqlMaskEngine
	defs  map[string]engine.ViewDef
	calls [][]engine.ViewRef
}

func (e *mssqlViewEngine) ViewDefs(_ context.Context, _ string, refs []engine.ViewRef) ([]engine.ViewDef, error) {
	e.calls = append(e.calls, append([]engine.ViewRef(nil), refs...))
	return lookupViews(e.defs, refs), nil
}

func TestMSSQLMaskViewExpression(t *testing.T) {
	src := func(i int, db, sch, tbl, col string) mssql.ColumnSource {
		return mssql.ColumnSource{Ordinal: i, Name: col, DB: db, Schema: sch, Table: tbl, Column: col}
	}
	newEnv := func(cols []string, data []any, base, views []mssql.ColumnSource, defs ...engine.ViewDef) (*fakeRepo, *mssqlViewEngine) {
		eng := &mssqlViewEngine{mssqlMaskEngine: mssqlMaskEngine{maskEngine: maskEngine{cols: cols, data: [][]any{data}}, base: base, views: views}, defs: viewDefsOf(defs...)}
		r := newRepo()
		r.cfg[settings.KeyQueryMaskEnabled] = "true"
		r.insts = map[uint]*model.Instance{5: {ID: 5, InstanceName: "erp-01", DBType: "mssql", Type: "master", Tags: canRead()}}
		r.maskCols = []model.DataMaskingColumn{keepCfg("ERP", "dbo.Buyer", "Mobile", 3, 4)}
		return r, eng
	}
	run := func(r *fakeRepo, eng *mssqlViewEngine, sql string) (*Result, error) {
		s := NewService(r, func(string, engine.Conn) (engine.Engine, error) { return eng, nil }, NewPrivAuthorizer(r, NewAutoExtractor(r)))
		return s.Execute(context.Background(), admin, Request{InstanceID: 5, DB: "ERP", SQL: sql})
	}
	cols := []string{"BuyerId", "Head"}
	base := []mssql.ColumnSource{src(1, "ERP", "dbo", "Buyer", "BuyerId"), {Ordinal: 2, Name: "Head"}} // 视图里的表达式列：基表层没有来源
	views := []mssql.ColumnSource{src(1, "ERP", "crm", "v_buyer", "BuyerId"), src(2, "ERP", "crm", "v_buyer", "Head")}

	// 视图里 Head = LEFT(Mobile, 3)：服务端只能说它来自视图自己，要靠视图定义才知道是 Mobile 的前三位
	r, eng := newEnv(cols, []any{1, "138"}, base, views, viewDef("crm", "v_buyer", "CREATE VIEW [crm].[v_buyer] AS SELECT BuyerId, Head = LEFT(Mobile, 3) FROM dbo.Buyer"))
	res, err := run(r, eng, "select BuyerId, Head from crm.v_buyer")
	if err != nil || res.Rows[0][1] != "13***" || res.Rows[0][0] != 1 {
		t.Fatalf("视图里的表达式列: %v %v masked=%v", err, res, res.MaskedColumns)
	}
	if want := []engine.ViewRef{{Schema: "crm", Name: "v_buyer"}}; len(eng.calls) == 0 || !reflect.DeepEqual(eng.calls[0], want) {
		t.Errorf("目录查询: %+v", eng.calls)
	}

	// 视图带显式列清单：输出列名取自清单，按位置对应
	r, eng = newEnv([]string{"Id2", "Mobile3"}, []any{1, "138"}, []mssql.ColumnSource{src(1, "ERP", "dbo", "Buyer", "BuyerId"), {Ordinal: 2, Name: "Mobile3"}},
		[]mssql.ColumnSource{src(1, "ERP", "crm", "v_buyer", "Id2"), src(2, "ERP", "crm", "v_buyer", "Mobile3")},
		viewDef("crm", "v_buyer", "CREATE VIEW crm.v_buyer (Id2, Mobile3) WITH SCHEMABINDING AS SELECT BuyerId, LEFT(Mobile, 3) FROM dbo.Buyer"))
	res, err = run(r, eng, "select Id2, Mobile3 from crm.v_buyer")
	if err != nil || res.Rows[0][1] != "13***" || res.Rows[0][0] != 1 {
		t.Errorf("带列清单的视图: %v %v", err, res)
	}

	// 三段名：到那个库里查视图；视图定义里没写库名的嵌套视图也属于那个库
	r, eng = newEnv([]string{"Head"}, []any{"138"}, []mssql.ColumnSource{{Ordinal: 1, Name: "Head"}}, []mssql.ColumnSource{src(1, "ERP2", "crm", "v_buyer", "Head")},
		engine.ViewDef{Catalog: "ERP2", Schema: "crm", Name: "v_buyer", Definition: "CREATE VIEW crm.v_buyer AS SELECT Head FROM dbo.v_inner"},
		engine.ViewDef{Catalog: "ERP2", Schema: "dbo", Name: "v_inner", Definition: "CREATE VIEW dbo.v_inner AS SELECT Head = LEFT(Mobile, 3) FROM dbo.Buyer"})
	res, err = run(r, eng, "select Head from ERP2.crm.v_buyer")
	if err != nil || res.Rows[0][0] != "13***" {
		t.Fatalf("三段名: %v %v", err, res)
	}
	want := [][]engine.ViewRef{{{Catalog: "ERP2", Schema: "crm", Name: "v_buyer"}}, {{Catalog: "ERP2", Schema: "dbo", Name: "v_inner"}}}
	if len(eng.calls) < 2 || !reflect.DeepEqual(eng.calls[:2], want) {
		t.Errorf("三段名与嵌套视图的目录查询: %+v", eng.calls)
	}

	// 加密视图（definition 为 NULL）：默认放行并提示，开启 query.block_on_mask_error 阻断
	r, eng = newEnv(cols, []any{1, "138"}, base, views, viewDef("crm", "v_buyer", ""))
	res, err = run(r, eng, "select BuyerId, Head from crm.v_buyer")
	if err != nil || !strings.Contains(res.Notice, "crm.v_buyer") {
		t.Errorf("定义读不到应放行并提示: %v %q", err, res.Notice)
	}
	r, eng = newEnv(cols, []any{1, "138"}, base, views, viewDef("crm", "v_buyer", ""))
	r.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	if _, err := run(r, eng, "select BuyerId, Head from crm.v_buyer"); code(err) != "masking_error" {
		t.Errorf("应阻断: %v", err)
	}
}
