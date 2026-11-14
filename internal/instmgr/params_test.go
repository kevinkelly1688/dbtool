// @author: kevinkelly
// @license: Apache Licence
// @file: params_test.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func paramEnv() *env {
	e := newEnv()
	e.target.on("SHOW GLOBAL VARIABLES", rs([]string{"variable_name", "value"},
		[]any{"max_connections", "2000"}, []any{"slow_query_log", "ON"}, []any{"long_query_time", "10.000000"}, []any{"innodb_buffer_pool_size", "25769803776"},
		[]any{"init_connect", ""}, []any{"read_only", "OFF"}, []any{"sql_mode", "STRICT_TRANS_TABLES"}))
	e.repo.tpls = []ParamTemplate{
		{ID: 1, DBType: "mysql", VariableName: "max_connections", DefaultValue: "151", Editable: true, ValidValues: "[1-100000]", Description: "最大连接数"},
		{ID: 2, DBType: "mysql", VariableName: "slow_query_log", DefaultValue: "OFF", Editable: true, ValidValues: "[ON|OFF]"},
		{ID: 3, DBType: "mysql", VariableName: "long_query_time", DefaultValue: "10", Editable: true, ValidValues: "[0-31536000]"},
		{ID: 4, DBType: "mysql", VariableName: "innodb_buffer_pool_size", Editable: true, ValidValues: "[5242880-…]"},
		{ID: 5, DBType: "mysql", VariableName: "read_only", Editable: false},
		{ID: 6, DBType: "mysql", VariableName: "init_connect", Editable: true}, // 模板里写了可改，但服务端黑名单拒绝
		{ID: 7, DBType: "mysql", VariableName: "sql_mode", Editable: true},
		{ID: 8, DBType: "doris", VariableName: "exec_mem_limit", Editable: true},
	}
	return e
}

func TestParseValid(t *testing.T) {
	cases := []struct {
		in    string
		kind  string
		check map[string]bool // 值 -> 是否通过
	}{
		{"", "", map[string]bool{"anything": true}},
		{"自由文本说明", "", map[string]bool{"x": true}},
		{"[1-100000]", "range", map[string]bool{"1": true, "100000": true, "0": false, "100001": false, "abc": false, "1.5": true}},
		{"[5242880-…]", "range", map[string]bool{"5242880": true, "99999999999": true, "5242879": false}},
		{"[0-]", "range", map[string]bool{"0": true, "-1": false, "100": true}},
		{"[-1-100]", "range", map[string]bool{"-1": true, "100": true, "101": false, "-2": false}},
		{"[0.5-10.5]", "range", map[string]bool{"0.5": true, "10.5": true, "11": false}},
		{"[ON|OFF]", "enum", map[string]bool{"ON": true, "off": true, "1": false, "MAYBE": false}},
		{"[ ROW | STATEMENT |MIXED]", "enum", map[string]bool{"row": true, "MIXED": true, "x": false}},
	}
	for _, c := range cases {
		spec, err := parseValid(c.in)
		if err != nil || spec.kind != c.kind {
			t.Errorf("%q: kind=%q err=%v", c.in, spec.kind, err)
			continue
		}
		for v, want := range c.check {
			if _, err := spec.check(v); (err == nil) != want {
				t.Errorf("%q 校验 %q: 期望通过=%v，实际 err=%v", c.in, v, want, err)
			}
		}
	}
	enum, _ := parseValid("[ON|OFF]")
	if v, _ := enum.check("off"); v != "OFF" {
		t.Errorf("枚举应规范成模板里的写法: %q", v)
	}
	for _, bad := range []string{"[5-1]", "[abc]def", "[]", "[|]"} {
		if _, err := parseValid(bad); err == nil {
			t.Errorf("%q 应判为格式错误", bad)
		}
	}
}

func TestParamArg(t *testing.T) {
	if v := paramArg("3000"); v != int64(3000) {
		t.Errorf("%#v", v)
	}
	if v := paramArg("-1"); v != int64(-1) {
		t.Errorf("%#v", v)
	}
	if v := paramArg("1.5"); v != 1.5 {
		t.Errorf("%#v", v)
	}
	for _, s := range []string{"ON", "1G", "0x10", "12345678901234567890123", "a'b"} {
		if v := paramArg(s); v != s {
			t.Errorf("%q 应按字符串传: %#v", s, v)
		}
	}
}

func TestListParamsMergesTemplates(t *testing.T) {
	e := paramEnv()
	rows, err := e.svc.ListParams(context.Background(), bob, 1, "yes", "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]ParamRow{}
	for _, r := range rows {
		names[r.Name] = r
	}
	if len(rows) != 5 {
		t.Fatalf("允许修改的参数（read_only 未开、init_connect 被黑名单挡住）: %+v", rows)
	}
	if r := names["max_connections"]; r.RuntimeValue != "2000" || r.DefaultValue != "151" || r.ValidValues != "[1-100000]" || !r.Editable || r.TemplateID != 1 {
		t.Fatalf("%+v", r)
	}
	if _, ok := names["init_connect"]; ok {
		t.Fatal("黑名单参数不能显示为可修改")
	}
	all, _ := e.svc.ListParams(context.Background(), root, 1, "all", "")
	no, _ := e.svc.ListParams(context.Background(), root, 1, "no", "")
	if len(all) != 7 || len(no) != 2 {
		t.Fatalf("全部 %d / 不可改 %d", len(all), len(no))
	}
	if r, _ := e.svc.ListParams(context.Background(), root, 1, "all", "QUERY"); len(r) != 2 {
		t.Fatalf("搜索不区分大小写: %+v", r)
	}
	if r, _ := e.svc.ListParams(context.Background(), root, 1, "", ""); len(r) != 7 {
		t.Fatalf("默认显示全部: %d", len(r))
	}
	_, err = e.svc.ListParams(context.Background(), root, 1, "maybe", "")
	wantErr(t, err, 400, "editable")
	// 其他类型的模板不会混进来：doris 实例只看 doris 的模板
	e.target.handlers = nil
	e.target.on("SHOW GLOBAL VARIABLES", rs([]string{"variable_name", "value"}, []any{"exec_mem_limit", "2147483648"}, []any{"max_connections", "1"}))
	rows, _ = e.svc.ListParams(context.Background(), root, 4, "yes", "")
	if len(rows) != 1 || rows[0].Name != "exec_mem_limit" {
		t.Fatalf("%+v", rows)
	}
	_, err = e.svc.ListParams(context.Background(), root, 3, "yes", "") // mssql
	wantErr(t, err, 400, "暂不支持")
	_, err = e.svc.ListParams(context.Background(), bob, 2, "yes", "")
	wantErr(t, err, 403, "未关联")
}

func TestSetParam(t *testing.T) {
	e := paramEnv()
	ctx := context.Background()
	plan, err := e.svc.PreviewParam(ctx, root, 1, "MAX_CONNECTIONS", "3000")
	if err != nil || plan.SQL != "SET GLOBAL max_connections = 3000" || plan.OldValue != "2000" || plan.Name != "max_connections" {
		t.Fatalf("%+v %v", plan, err)
	}
	if len(e.target.execs()) != 0 {
		t.Fatal("预览不执行")
	}
	_, err = e.svc.SetParam(ctx, root, 1, "max_connections", "3000", false)
	wantErr(t, err, 400, "确认")
	plan, err = e.svc.SetParam(ctx, root, 1, "max_connections", "3000", true)
	if err != nil {
		t.Fatal(err)
	}
	ex := e.target.execs()
	if len(ex) != 1 || ex[0].sql != "SET GLOBAL max_connections = ?" || ex[0].args[0] != int64(3000) {
		t.Fatalf("数值参数按数字传: %+v", ex)
	}
	if len(e.repo.hist) != 1 {
		t.Fatal("应记录修改历史")
	}
	h := e.repo.hist[0]
	if h.InstanceID != 1 || h.VariableName != "max_connections" || h.OldVar != "2000" || h.NewVar != "3000" || h.SetSQL != "SET GLOBAL max_connections = 3000" || h.UserName != "root" || h.UserDisplay != "管理员" {
		t.Fatalf("%+v", h)
	}
	if !h.CreateTime.Equal(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("创建时间由服务写入: %v", h.CreateTime)
	}
	a := e.repo.lastAudit()
	if a.Action != "修改实例参数" || !strings.Contains(a.ExtraInfo, "SET GLOBAL max_connections = 3000") || !strings.Contains(a.ExtraInfo, "成功") {
		t.Fatalf("%+v", a)
	}
	if !strings.Contains(plan.Note, "my.cnf") {
		t.Fatalf("要提醒持久化: %+v", plan)
	}
	// 枚举：规范成模板写法，按字符串传
	e2 := paramEnv()
	if _, err := e2.svc.SetParam(ctx, root, 1, "slow_query_log", "off", true); err != nil {
		t.Fatal(err)
	}
	if ex := e2.target.execs(); ex[0].args[0] != "OFF" {
		t.Fatalf("%+v", ex)
	}
	// 小数
	e3 := paramEnv()
	if _, err := e3.svc.SetParam(ctx, root, 1, "long_query_time", "1.5", true); err != nil || e3.target.execs()[0].args[0] != 1.5 {
		t.Fatalf("%v %+v", err, e3.target.execs())
	}
}

func TestSetParamRejections(t *testing.T) {
	cases := []struct {
		name  string
		param string
		value string
		code  int
		want  string
	}{
		{"没有模板", "wait_timeout", "100", 400, "先在参数模板中配置"},
		{"模板未开启修改", "read_only", "ON", 403, "没有开启"},
		{"黑名单", "init_connect", "SET @a=1", 403, "不允许在线修改"},
		{"超出范围", "max_connections", "100001", 400, "超出允许范围"},
		{"不是数字", "max_connections", "many", 400, "必须是数字"},
		{"枚举不符", "slow_query_log", "maybe", 400, "之一"},
		{"与运行值相同", "max_connections", "2000", 409, "未调整"},
		{"空值", "max_connections", "  ", 400, "不能为空"},
		{"含换行", "sql_mode", "A\nB", 400, "控制字符"},
		{"参数名含注入", "max_connections; drop", "1", 400, "参数名"},
		{"参数名过长", strings.Repeat("a", 70), "1", 400, "参数名"},
	}
	for _, c := range cases {
		e := paramEnv()
		_, err := e.svc.SetParam(context.Background(), root, 1, c.param, c.value, true)
		if err == nil {
			t.Errorf("%s：应该失败", c.name)
			continue
		}
		wantErr(t, err, c.code, c.want)
		if len(e.target.execs()) != 0 || len(e.repo.hist) != 0 {
			t.Errorf("%s：被拒绝时不能执行也不能记历史", c.name)
		}
	}
	// 模板里有、实例上没有的参数
	e := paramEnv()
	e.repo.tpls = append(e.repo.tpls, ParamTemplate{ID: 9, DBType: "mysql", VariableName: "no_such_var", Editable: true})
	_, err := e.svc.SetParam(context.Background(), root, 1, "no_such_var", "1", true)
	wantErr(t, err, 404, "没有参数")
	// 自由文本值带引号：走参数，不拼进语句
	e = paramEnv()
	if _, err := e.svc.SetParam(context.Background(), root, 1, "sql_mode", "A','B'; drop", true); err != nil {
		t.Fatal(err)
	}
	if ex := e.target.execs(); ex[0].sql != "SET GLOBAL sql_mode = ?" || ex[0].args[0] != "A','B'; drop" {
		t.Fatalf("%+v", ex)
	}
	if h := e.repo.hist[0]; !strings.Contains(h.SetSQL, `'A\',\'B\'; drop'`) {
		t.Fatalf("历史里记录的是转义后的语句: %s", h.SetSQL)
	}
}

func TestSetParamExecFailureAndHistoryFailure(t *testing.T) {
	e := paramEnv()
	e.target.execErr["SET GLOBAL"] = errors.New("Error 1238: Variable 'max_connections' is a read only variable")
	_, err := e.svc.SetParam(context.Background(), root, 1, "max_connections", "3000", true)
	wantErr(t, err, 502, "1238")
	if len(e.repo.hist) != 0 {
		t.Fatal("失败不记历史")
	}
	if a := e.repo.lastAudit(); !strings.Contains(a.ExtraInfo, "失败") {
		t.Fatalf("失败也要审计: %+v", a)
	}
	// 修改成功但历史写失败：不能报失败（SET GLOBAL 已生效），在说明里提示
	e2 := paramEnv()
	e2.repo.failHist = true
	plan, err := e2.svc.SetParam(context.Background(), root, 1, "max_connections", "3000", true)
	if err != nil || !strings.Contains(plan.Note, "写入修改历史失败") {
		t.Fatalf("%+v %v", plan, err)
	}
}

func TestParamHistoryAndTemplates(t *testing.T) {
	e := paramEnv()
	for i := 0; i < 3; i++ {
		e.repo.hist = append(e.repo.hist, ParamHistory{InstanceID: 1, VariableName: "max_connections", NewVar: "1"})
	}
	e.repo.hist = append(e.repo.hist, ParamHistory{InstanceID: 1, VariableName: "long_query_time"}, ParamHistory{InstanceID: 2, VariableName: "other"})
	page, err := e.svc.ListParamChanges(bob, 1, "", 1, 3)
	if err != nil || page.Total != 4 || len(page.Rows) != 3 || page.Rows[0].InstanceName != "prod-mysql-01" {
		t.Fatalf("%+v %v", page, err)
	}
	if p, _ := e.svc.ListParamChanges(bob, 1, "long", 1, 20); p.Total != 1 {
		t.Fatalf("%+v", p)
	}
	if p, _ := e.svc.ListParamChanges(bob, 1, "", 9, 20); p.Rows == nil || len(p.Rows) != 0 {
		t.Fatalf("超出页数返回空数组: %+v", p)
	}
	_, err = e.svc.ListParamChanges(bob, 2, "", 1, 20)
	wantErr(t, err, 403, "未关联")

	list, err := e.svc.ListTemplates("mysql")
	if err != nil || len(list) != 7 {
		t.Fatalf("%d %v", len(list), err)
	}
	_, err = e.svc.ListTemplates("mssql")
	wantErr(t, err, 400, "暂不支持")
}

func TestSaveTemplate(t *testing.T) {
	e := paramEnv()
	in := TemplateInput{DBType: "mysql", VariableName: " Innodb_Io_Capacity ", DefaultValue: "200", Editable: true, ValidValues: "[100-20000]", Description: "IO 能力"}
	tpl, err := e.svc.SaveTemplate(root, 0, in)
	if err != nil || tpl.VariableName != "innodb_io_capacity" || tpl.ID == 0 || tpl.CreateTime.IsZero() {
		t.Fatalf("%+v %v", tpl, err)
	}
	if a := e.repo.lastAudit(); a.Action != "保存参数模板" || !strings.Contains(a.ExtraInfo, "innodb_io_capacity") {
		t.Fatalf("%+v", a)
	}
	_, err = e.svc.SaveTemplate(root, 0, in)
	wantErr(t, err, 409, "已有参数")
	// 修改：不能改类型与参数名
	upd := TemplateInput{DBType: "doris", VariableName: "hacked", DefaultValue: "300", Editable: false, ValidValues: "[ON|OFF]"}
	got, err := e.svc.SaveTemplate(root, tpl.ID, upd)
	if err != nil || got.DBType != "mysql" || got.VariableName != "innodb_io_capacity" || got.DefaultValue != "300" || got.Editable || got.ValidValues != "[ON|OFF]" {
		t.Fatalf("%+v %v", got, err)
	}
	if !got.CreateTime.Equal(tpl.CreateTime) {
		t.Fatal("修改不能动创建时间")
	}
	_, err = e.svc.SaveTemplate(root, 999, upd)
	wantErr(t, err, 404, "不存在")
	for name, mod := range map[string]func(*TemplateInput){
		"类型不支持":  func(i *TemplateInput) { i.DBType = "mssql" },
		"参数名非法":  func(i *TemplateInput) { i.VariableName = "a b" },
		"参数名空":   func(i *TemplateInput) { i.VariableName = "" },
		"可修改值格式": func(i *TemplateInput) { i.ValidValues = "[5-1]" },
		"黑名单不能开": func(i *TemplateInput) { i.VariableName, i.Editable = "init_connect", true },
		"默认值过长":  func(i *TemplateInput) { i.DefaultValue = strings.Repeat("x", 1025) },
	} {
		bad := in
		bad.VariableName = "some_var"
		mod(&bad)
		if _, err := e.svc.SaveTemplate(root, 0, bad); err == nil {
			t.Errorf("%s：应该失败", name)
		}
	}
}
