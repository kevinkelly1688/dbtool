// @author: kevinkelly
// @license: Apache Licence
// @file: optimize_test.go
// @time: 2026/10/28

package sqlopt

import (
	"dbtool/internal/audit"
	"dbtool/internal/settings"
	"errors"
	"strings"
	"testing"
)

func startOpt(t *testing.T, e *env, a audit.Actor, req OptimizeReq) *ItemDetail {
	t.Helper()
	v, err := e.svc.StartOptimize(ctx(), alice, req)
	if err != nil {
		t.Fatal(err)
	}
	done := waitJob(t, e.svc, alice, v.ID)
	d, err := e.svc.JobItem(alice, v.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusDone {
		t.Fatalf("任务状态 %s：%+v", done.Status, done)
	}
	return d
}

func (o OptimizeReq) with(f func(*OptimizeReq)) OptimizeReq { f(&o); return o }

var advReq = OptimizeReq{Tool: ToolSOAR, InstanceID: 1, DB: "shop", SQL: "select * from orders where id = 1"}

func TestToolPerm(t *testing.T) {
	for tool, want := range map[string]string{ToolSOAR: "optimize.soar", ToolTuning: "optimize.tuning", "sqladvisor": "", "x": ""} {
		if got := ToolPerm(tool); got != want {
			t.Errorf("ToolPerm(%s)=%s", tool, got)
		}
	}
}

func TestOptimizeToolNotConfigured(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.StartOptimize(ctx(), alice, advReq)
	wantCode(t, err, "tool_not_configured")
	if !strings.Contains(err.Error(), "SOAR") {
		t.Fatalf("应明确说是哪个工具没配置：%v", err)
	}
	e.repo.config[settings.KeyOptimizerSoarPath] = "/no/such/soar"
	_, err = e.svc.StartOptimize(ctx(), alice, advReq)
	wantCode(t, err, "tool_missing")

	e.repo.config[settings.KeyOptimizerSoarPath] = testToolPath
	_, err = e.svc.StartOptimize(ctx(), alice, advReq)
	wantCode(t, err, "tool_not_configured") // 缺 soar_test_dsn
	if !strings.Contains(err.Error(), "测试环境") {
		t.Fatalf("%v", err)
	}
	if e.runner.count() != 0 || len(e.audit.all()) != 0 {
		t.Fatal("工具不可用时不应运行或审计")
	}
	// 页面据此显示的状态
	h := &handlers{svc: e.svc}
	st := h.toolStatus()
	if st[ToolSOAR].Ready || !st[ToolTuning].Ready || st[ToolSOAR].Message == "" {
		t.Fatalf("%+v", st)
	}
	e.configureTools()
	st = h.toolStatus()
	if !st[ToolSOAR].Ready {
		t.Fatalf("%+v", st)
	}
	if _, ok := st["sqladvisor"]; ok {
		t.Fatalf("SQLAdvisor 已下线，不应再出现在工具状态里：%+v", st)
	}
}

// SQLAdvisor 已下线：旧页面 / 脚本传 sqladvisor 与未知工具一样被拒绝，不会运行任何外部程序。
func TestOptimizeSQLAdvisorRemoved(t *testing.T) {
	e := newEnv(t)
	e.configureTools()
	_, err := e.svc.StartOptimize(ctx(), alice, advReq.with(func(r *OptimizeReq) { r.Tool = "sqladvisor" }))
	wantCode(t, err, "bad_request")
	if e.runner.count() != 0 {
		t.Fatal("不应运行外部程序")
	}
}

func TestOptimizeInputsAreSingleArgs(t *testing.T) {
	e := newEnv(t)
	e.configureTools()
	// SQL 里的引号、分号、美元符、反引号只会作为 -q 的单个参数，而且必须是单条语句
	evil := "select '; rm -rf / #' , `x`, $(id) from t where a = \"b\" && c"
	startOpt(t, e, alice, advReq.with(func(r *OptimizeReq) { r.SQL = evil }))
	a := e.runner.last().Args
	if a[len(a)-2] != "-query" || a[len(a)-1] != evil {
		t.Fatalf("SQL 应原样作为最后一个参数：%q", a)
	}
	for _, x := range a[:len(a)-1] {
		if strings.Contains(x, "rm -rf") {
			t.Fatalf("SQL 不应出现在其他参数里：%q", a)
		}
	}
	// 注入式的库名、路径式的 SQL：在运行外部程序之前就被拒绝
	n := e.runner.count()
	for _, db := range []string{"shop;rm -rf /", "-x", "a b", "shop`", "a/../b", "$(id)", strings.Repeat("a", 65)} {
		_, err := e.svc.StartOptimize(ctx(), alice, advReq.with(func(r *OptimizeReq) { r.DB = db }))
		wantCode(t, err, "bad_request")
	}
	for _, sql := range []string{"/etc/passwd", "../../etc/shadow", "soar.yaml", "-config /tmp/x", "show tables", "select 1; drop table t", ""} {
		_, err := e.svc.StartOptimize(ctx(), alice, advReq.with(func(r *OptimizeReq) { r.SQL = sql }))
		wantCode(t, err, "bad_request")
	}
	if e.runner.count() != n {
		t.Fatal("被拒绝的请求不能触发外部程序")
	}
}

func TestOptimizeToolFailures(t *testing.T) {
	e := newEnv(t)
	e.configureTools()
	run := func() *ItemDetail {
		v, err := e.svc.StartOptimize(ctx(), alice, advReq)
		if err != nil {
			t.Fatal(err)
		}
		done := waitJob(t, e.svc, alice, v.ID)
		if done.Status != StatusDone || done.Failed != 1 {
			t.Fatalf("条目失败不等于任务失败：%+v", done)
		}
		d, _ := e.svc.JobItem(alice, v.ID, 0)
		return d
	}
	e.runner.res = &ExecResult{ExitCode: 2, Stderr: "fatal: cannot connect with password s3cretPW"}
	d := run()
	if d.Result != nil || !strings.Contains(d.Error, "退出码 2") || strings.Contains(d.Error, "s3cretPW") {
		t.Fatalf("%+v", d)
	}
	e.runner.res = &ExecResult{TimedOut: true}
	if d = run(); !strings.Contains(d.Error, "运行超过") {
		t.Fatalf("%+v", d)
	}
	e.runner.res, e.runner.err = nil, errors.New("fork/exec /opt/soar: permission denied")
	if d = run(); !strings.Contains(d.Error, "启动 SOAR 失败") {
		t.Fatalf("%+v", d)
	}
	// 非零退出码但有输出：照样展示输出
	e.runner.res, e.runner.err = &ExecResult{ExitCode: 1, Stdout: "partial report"}, nil
	v, _ := e.svc.StartOptimize(ctx(), alice, advReq)
	waitJob(t, e.svc, alice, v.ID)
	if d, _ = e.svc.JobItem(alice, v.ID, 0); d.Result == nil || d.Result.Output != "partial report" {
		t.Fatalf("%+v", d)
	}
}

func TestOptimizeSOAR(t *testing.T) {
	e := newEnv(t)
	e.configureTools()
	e.repo.config[settings.KeyOptimizerSoarConfig] = "/etc/soar.yaml"
	e.runner.res = &ExecResult{Stdout: "# 综合评分: 70 分\n* ARG.001\n"}
	req := advReq
	d := startOpt(t, e, alice, req)
	if d.Result.Tool != ToolSOAR || !strings.Contains(d.Result.Output, "ARG.001") {
		t.Fatalf("%+v", d)
	}
	want := []string{"-config", "/etc/soar.yaml", "-report-type", "markdown", "-online-dsn", "dbuser:s3cretPW@10.0.0.1:3306/shop",
		"-test-dsn", "tester:tpass1234@10.9.9.9:3306/test", "-allow-online-as-test=false", "-query", "select * from orders where id = 1"}
	if got := e.runner.last().Args; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("参数模板：%q", got)
	}
	if e.runner.last().Timeout != soarTimeout {
		t.Fatal("超时")
	}
	if es := e.audit.all(); len(es) != 1 || es[0].Action != "SQL优化-SOAR" || strings.Contains(es[0].ExtraInfo, "s3cretPW") || strings.Contains(es[0].ExtraInfo, "tpass1234") {
		t.Fatalf("审计不能含密码：%+v", es)
	}
	// SOAR 必须选库
	_, err := e.svc.StartOptimize(ctx(), alice, req.with(func(r *OptimizeReq) { r.DB = "" }))
	wantCode(t, err, "bad_request")
	// 用户没有权限看到的库
	e.repo.instances[0].DeniedDBNameRegex = "shop"
	v, err := e.svc.StartOptimize(ctx(), alice, req)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, e.svc, alice, v.ID)
	n := e.runner.count()
	if d, _ = e.svc.JobItem(alice, v.ID, 0); d.Error == "" || e.runner.count() != n {
		t.Fatalf("不可见的库不应运行 SOAR：%+v", d)
	}
}

func TestOptimizeAccessControl(t *testing.T) {
	e := newEnv(t)
	e.configureTools()
	_, err := e.svc.StartOptimize(ctx(), alice, advReq.with(func(r *OptimizeReq) { r.InstanceID = 2 }))
	wantCode(t, err, "no_instance")
	_, err = e.svc.StartOptimize(ctx(), alice, advReq.with(func(r *OptimizeReq) { r.InstanceID = 3 }))
	wantCode(t, err, "unsupported") // doris 只有执行计划
	_, err = e.svc.StartOptimize(ctx(), alice, advReq.with(func(r *OptimizeReq) { r.InstanceID = 0 }))
	wantCode(t, err, "bad_request")
	_, err = e.svc.StartOptimize(ctx(), alice, advReq.with(func(r *OptimizeReq) { r.Tool = "rm" }))
	wantCode(t, err, "bad_request")
	if _, err = e.svc.StartOptimize(ctx(), root, advReq.with(func(r *OptimizeReq) { r.InstanceID = 2 })); err != nil {
		t.Fatalf("超级用户可见全部实例：%v", err)
	}
}

func TestOptimizeSQLTuning(t *testing.T) {
	e := newEnv(t)
	e.db.tables["orders"] = ordersFake()
	req := OptimizeReq{Tool: ToolTuning, InstanceID: 1, DB: "shop", SQL: "select * from orders o join shop.users u on u.id = o.user_id where o.id = 1",
		Options: []string{OptSysParm, OptSQLPlan, OptObjStat, OptSQLProfile}}
	d := startOpt(t, e, alice, req)
	tu := d.Result.Tuning
	if d.Result.Tool != ToolTuning || tu == nil {
		t.Fatalf("%+v", d.Result)
	}
	if tu.Basic == nil || tu.Basic.Rows[0][0] != "8.0.36" || tu.SysParams == nil || tu.OptimizerSwitch == nil || tu.Plan == nil || tu.Rewrite == nil {
		t.Fatalf("%+v", tu)
	}
	if len(tu.Objects) != 2 || tu.Objects[0].Table != "shop.orders" || tu.Objects[1].Table != "shop.users" || tu.Objects[0].Structure == "" || tu.Objects[0].Info == nil || tu.Objects[0].Indexes == nil {
		t.Fatalf("应覆盖语句里的所有表：%+v", tu.Objects)
	}
	p := tu.Profile
	if p == nil || p.Error != "" || p.Rows != 3 || p.Detail == nil || p.Summary == nil || p.StatusDiff == nil {
		t.Fatalf("%+v", p)
	}
	if len(e.db.drained) != 1 || e.db.drained[0] != "select * from orders o join shop.users u on u.id = o.user_id where o.id = 1" {
		t.Fatalf("PROFILE 应真实执行一次查询：%v", e.db.drained)
	}
	qs := strings.Join(e.db.queries(), "\n")
	for _, must := range []string{"SET SESSION TRANSACTION READ ONLY", "SET SESSION max_execution_time", "SET profiling = 1", "SHOW PROFILES", "SHOW CREATE TABLE `shop`.`orders`"} {
		if !strings.Contains(qs, must) {
			t.Errorf("缺少语句 %s", must)
		}
	}
	if strings.Contains(qs, "performance_schema") {
		t.Error("不应依赖 performance_schema")
	}
	// 状态差异：Handler_read_rnd 前后都是 10 不会出现，这里只验证结构
	if len(p.StatusDiff.Columns) != 4 {
		t.Fatalf("%+v", p.StatusDiff)
	}
	if es := e.audit.all(); len(es) != 1 || es[0].Action != "SQL优化-SQLTuning" {
		t.Fatalf("%+v", es)
	}
	if e.db.opened != e.db.closed {
		t.Fatalf("会话泄漏 opened=%d closed=%d", e.db.opened, e.db.closed)
	}
}

func TestOptimizeSQLTuningValidation(t *testing.T) {
	e := newEnv(t)
	base := OptimizeReq{Tool: ToolTuning, InstanceID: 1, DB: "shop", SQL: "select 1", Options: []string{OptSQLPlan}}
	for _, sql := range []string{"update t set a=1", "delete from t", "insert into t select 1", "show tables", "select sleep(5)", "select * from t into outfile '/tmp/x'", "select 1; select 2"} {
		_, err := e.svc.StartOptimize(ctx(), alice, base.with(func(r *OptimizeReq) { r.SQL = sql }))
		wantCode(t, err, "bad_request")
	}
	_, err := e.svc.StartOptimize(ctx(), alice, base.with(func(r *OptimizeReq) { r.Options = nil }))
	wantCode(t, err, "bad_request")
	_, err = e.svc.StartOptimize(ctx(), alice, base.with(func(r *OptimizeReq) { r.Options = []string{"drop"} }))
	wantCode(t, err, "bad_request")
	_, err = e.svc.StartOptimize(ctx(), alice, base.with(func(r *OptimizeReq) { r.DB = "" }))
	wantCode(t, err, "bad_request")
	// 引用了不可见库的语句：任务条目失败，且不会执行
	v, err := e.svc.StartOptimize(ctx(), alice, base.with(func(r *OptimizeReq) { r.SQL = "select * from mysql.user"; r.Options = []string{OptSQLProfile} }))
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, e.svc, alice, v.ID)
	if d, _ := e.svc.JobItem(alice, v.ID, 0); d.Error == "" || len(e.db.drained) != 0 {
		t.Fatalf("不应执行：%+v drained=%v", d, e.db.drained)
	}
}

func TestSQLTuningPartialFailures(t *testing.T) {
	e := newEnv(t)
	e.db.errs["SET profiling"] = errors.New("unknown system variable 'profiling'")
	e.db.errs["EXPLAIN"] = errors.New("Error 1064 syntax")
	req := OptimizeReq{Tool: ToolTuning, InstanceID: 1, DB: "shop", SQL: "select 1", Options: []string{OptSQLPlan, OptSQLProfile}}
	d := startOpt(t, e, alice, req)
	tu := d.Result.Tuning
	if tu.Basic == nil || tu.Plan != nil || len(tu.Errors) != 1 || !strings.Contains(tu.Errors[0], "获取执行计划") {
		t.Fatalf("%+v", tu)
	}
	if tu.Profile == nil || !strings.Contains(tu.Profile.Error, "profiling") || len(e.db.drained) != 0 {
		t.Fatalf("profiling 不可用时不应执行语句：%+v drained=%v", tu.Profile, e.db.drained)
	}
}

func TestFinishItemRespectsJobBudget(t *testing.T) {
	e := newEnv(t)
	j := e.svc.newJob(alice, KindAnalyze, "x")
	it := &Item{ID: "1", SQL: "select 1"}
	j.Items = []*Item{it}
	j.bytes = MaxJobBytes - 10
	rep := &Report{Findings: []Finding{{ID: "R001", Level: LevelLow}}, Plan: &Table{Columns: []string{"a"}}, Soar: strings.Repeat("x", 5000)}
	j.finishItem(it, rep, nil)
	if it.Report == nil || it.Report.Plan != nil || it.Report.Soar != "" || len(it.Report.Notes) == 0 || len(it.Report.Findings) != 1 || it.Status != StatusDone {
		t.Fatalf("超出预算时应保留建议、丢弃体积大的部分：%+v", it.Report)
	}
	it2 := &Item{ID: "2"}
	j.bytes = MaxJobBytes
	j.finishItem(it2, nil, &OptimizeResult{Tool: ToolSOAR, Output: strings.Repeat("y", 100)})
	if it2.Result != nil || it2.Status != StatusFailed {
		t.Fatalf("%+v", it2)
	}
}
