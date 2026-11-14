// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package sqlopt

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/audit"
)

func ctx() context.Context { return context.Background() }

// ---- 实例范围 ----

func TestInstancesVisibility(t *testing.T) {
	e := newEnv(t)
	list, err := e.svc.Instances(root)
	if err != nil || len(list) != 3 { // oracle 不在支持范围
		t.Fatalf("超级用户应看到 3 个（mysql×2 + doris）：%+v %v", list, err)
	}
	list, _ = e.svc.Instances(alice)
	if len(list) != 2 || list[0].Name != "prod-mysql" || list[1].Name != "report-doris" {
		t.Fatalf("alice 只能看到资源组内的：%+v", list)
	}
	if !list[0].Tools || !list[0].Explain || list[1].Tools || !list[1].Explain {
		t.Fatalf("doris 只支持执行计划：%+v", list)
	}
	if list, _ = e.svc.Instances(bob); len(list) != 0 {
		t.Fatalf("bob 没有关联任何实例：%+v", list)
	}
	e.repo.perms[bob.ID] = map[string]bool{"query.admin_run": true}
	if list, _ = e.svc.Instances(bob); len(list) != 3 {
		t.Fatalf("query.admin_run 可见全部：%+v", list)
	}
}

func TestDatabases(t *testing.T) {
	e := newEnv(t)
	e.repo.instances[0].DeniedDBNameRegex = "secret"
	dbs, err := e.svc.Databases(ctx(), alice, 1)
	if err != nil || strings.Join(dbs, ",") != "report,shop" {
		t.Fatalf("应去掉系统库与被屏蔽的库：%v %v", dbs, err)
	}
	e.repo.instances[0].ShowDBNameRegex = "sh"
	if dbs, _ = e.svc.Databases(ctx(), alice, 1); strings.Join(dbs, ",") != "shop" {
		t.Fatalf("show 正则从开头匹配：%v", dbs)
	}
	e.repo.instances[0].ShowDBNameRegex = "("
	_, err = e.svc.Databases(ctx(), alice, 1)
	wantCode(t, err, "bad_config")
	_, err = e.svc.Databases(ctx(), alice, 2)
	wantCode(t, err, "no_instance")
	_, err = e.svc.Databases(ctx(), alice, 99)
	wantCode(t, err, "not_found")
	_, err = e.svc.Databases(ctx(), root, 4)
	wantCode(t, err, "unsupported")
	e.db.openErr = errors.New("dial tcp 10.0.0.1:3306: connect: refused (password s3cretPW)")
	_, err = e.svc.Databases(ctx(), alice, 1)
	wantCode(t, err, "connect_failed")
	if strings.Contains(err.Error(), "s3cretPW") {
		t.Fatalf("错误信息里不能出现密码：%v", err)
	}
	if e.db.opened != e.db.closed {
		t.Fatalf("会话泄漏 opened=%d closed=%d", e.db.opened, e.db.closed)
	}
}

// ---- 执行计划 ----

func TestExplain(t *testing.T) {
	e := newEnv(t)
	res, err := e.svc.Explain(ctx(), alice, ExplainReq{InstanceID: 1, DB: "shop", SQL: "select * from orders where user_id = 1;"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != "traditional" || res.Plan == nil || len(res.Plan.Rows) != 1 || !hasID(res.Findings, "P001") || !hasID(res.Findings, "P003") {
		t.Fatalf("%+v", res)
	}
	qs := e.db.queries()
	if qs[len(qs)-1] != "EXPLAIN select * from orders where user_id = 1" {
		t.Fatalf("发给目标库的语句：%v", qs)
	}
	var sawUse bool
	for _, q := range qs {
		sawUse = sawUse || q == "USE `shop`"
	}
	if !sawUse {
		t.Fatalf("应先切换库：%v", qs)
	}
	entries := e.audit.all()
	if len(entries) != 1 || entries[0].Action != "SQL优化-执行计划" || entries[0].UserName != "alice" || !strings.Contains(entries[0].ExtraInfo, "prod-mysql") {
		t.Fatalf("审计：%+v", entries)
	}
	if e.db.opened != e.db.closed {
		t.Fatal("会话没有关闭")
	}

	// 用户自己写了 explain：去掉前缀，只发一个 EXPLAIN
	if _, err = e.svc.Explain(ctx(), alice, ExplainReq{InstanceID: 1, DB: "shop", SQL: "explain select 1"}); err != nil {
		t.Fatal(err)
	}
	qs = e.db.queries()
	if qs[len(qs)-1] != "EXPLAIN select 1" {
		t.Fatalf("%v", qs)
	}

	// JSON 格式
	e.db.plan = tbl([]string{"EXPLAIN"}, []any{`{"query_block":{"select_id":1}}`})
	res, err = e.svc.Explain(ctx(), alice, ExplainReq{InstanceID: 1, DB: "shop", SQL: "select 1", Format: "json"})
	if err != nil || !strings.Contains(res.JSON, "\n  \"query_block\"") || res.Plan != nil {
		t.Fatalf("%+v %v", res, err)
	}
	qs = e.db.queries()
	if qs[len(qs)-1] != "EXPLAIN FORMAT=JSON select 1" {
		t.Fatalf("%v", qs)
	}
}

func TestExplainRejects(t *testing.T) {
	e := newEnv(t)
	run := func(id uint, db, sql string) error {
		_, err := e.svc.Explain(ctx(), alice, ExplainReq{InstanceID: id, DB: db, SQL: sql})
		return err
	}
	wantCode(t, run(1, "shop", "drop table orders"), "bad_request")
	wantCode(t, run(1, "shop", "select 1; select 2"), "bad_request")
	wantCode(t, run(1, "shop", "explain analyze select 1"), "bad_request")
	wantCode(t, run(1, "shop", "select sleep(10)"), "bad_request")
	wantCode(t, run(1, "shop", ""), "bad_request")
	wantCode(t, run(0, "shop", "select 1"), "bad_request")
	wantCode(t, run(2, "shop", "select 1"), "no_instance") // 不在 alice 的资源组
	wantCode(t, run(99, "shop", "select 1"), "not_found")
	wantCode(t, run(4, "", "select 1"), "unsupported") // oracle
	if _, err := e.svc.Explain(ctx(), alice, ExplainReq{InstanceID: 1, SQL: "select 1", Format: "yaml"}); err == nil {
		t.Fatal("未知格式应拒绝")
	}
	if _, err := e.svc.Explain(ctx(), alice, ExplainReq{InstanceID: 3, SQL: "select 1", Format: "json"}); err == nil {
		t.Fatal("doris 不支持 JSON 格式")
	}

	// 库名：不存在 / 被实例规则屏蔽 / 系统库 / 不合法
	e.repo.instances[0].DeniedDBNameRegex = "secret"
	wantCode(t, run(1, "secret", "select 1"), "no_database")
	wantCode(t, run(1, "mysql", "select 1"), "no_database")
	wantCode(t, run(1, "nope", "select 1"), "no_database")
	wantCode(t, run(1, "bad\x00db", "select 1"), "bad_request")
	// 语句里带库名引用被屏蔽的库
	wantCode(t, run(1, "shop", "select * from secret.users"), "no_database")
	wantCode(t, run(1, "shop", "select * from mysql.user"), "no_database")
	wantCode(t, run(1, "shop", "update secret.t set a=1 where id=1"), "no_database")
	if err := run(1, "shop", "select * from shop.orders o join report.x r on r.id=o.id"); err != nil {
		t.Fatalf("可见库的限定名应放行：%v", err)
	}
	for _, q := range e.db.queries() {
		if strings.Contains(q, "secret") && strings.HasPrefix(q, "EXPLAIN") {
			t.Fatalf("被拒绝的语句不应发给目标库：%s", q)
		}
	}
	if e.db.opened != e.db.closed {
		t.Fatalf("会话泄漏 opened=%d closed=%d", e.db.opened, e.db.closed)
	}
}

func TestExplainDBErrors(t *testing.T) {
	e := newEnv(t)
	run := func() error {
		_, err := e.svc.Explain(ctx(), alice, ExplainReq{InstanceID: 1, DB: "shop", SQL: "select * from nope"})
		return err
	}
	e.db.errs["EXPLAIN"] = errors.New("Error 1146: Table 'shop.nope' doesn't exist")
	err := run()
	wantCode(t, err, "db_error")
	if !strings.Contains(err.Error(), "doesn't exist") {
		t.Fatalf("应把目标库的错误告诉用户：%v", err)
	}
	e.db.errs["EXPLAIN"] = errors.New("access denied for user dbuser password s3cretPW")
	if err = run(); strings.Contains(err.Error(), "s3cretPW") {
		t.Fatalf("密码不能出现在错误里：%v", err)
	}
	// 超时
	delete(e.db.errs, "EXPLAIN")
	e.db.delay = 300 * time.Millisecond
	c, cancel := context.WithTimeout(ctx(), 50*time.Millisecond)
	defer cancel()
	_, err = e.svc.Explain(c, alice, ExplainReq{InstanceID: 1, DB: "shop", SQL: "select 1"})
	if err == nil {
		t.Fatal("应超时")
	}
	if code := codeOf(t, err); code != "timeout" && code != "canceled" {
		t.Fatalf("code=%s", code)
	}
}

func TestExplainBusy(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < DefaultConcurrency; i++ {
		e.svc.syncSem <- struct{}{}
	}
	c, cancel := context.WithTimeout(ctx(), 200*time.Millisecond)
	defer cancel()
	_, err := e.svc.Explain(c, alice, ExplainReq{InstanceID: 1, DB: "shop", SQL: "select 1"})
	if err == nil {
		t.Fatal("并发名额用完应拒绝或等待超时")
	}
	if code := codeOf(t, err); code != "busy" && code != "canceled" {
		t.Fatalf("code=%s", code)
	}
}

// ---- 内置建议 ----

func ordersFake() fakeTable {
	return fakeTable{
		cols:    [][3]string{{"id", "bigint", "bigint(20)"}, {"user_id", "bigint", "bigint(20)"}, {"phone", "varchar", "varchar(20)"}, {"created_at", "datetime", "datetime"}},
		indexes: [][4]string{{"PRIMARY", "0", "1", "id"}, {"idx_user", "1", "1", "user_id"}},
		rows:    "8000000",
	}
}

func TestAdvise(t *testing.T) {
	e := newEnv(t)
	// 不选实例：只有静态规则，不连库
	rep, err := e.svc.Advise(ctx(), alice, AdviseReq{SQL: "select * from orders where date(created_at) = '2026-10-05' limit 20000, 20"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"R001", "R003", "R005"} {
		if !hasID(rep.Findings, id) {
			t.Errorf("应命中 %s：%s", id, ids(rep.Findings))
		}
	}
	if rep.Plan != nil || e.db.opened != 0 || rep.Score >= 100 || rep.Highest == "" || rep.Type != TypeSelect || len(rep.Tables) != 1 {
		t.Fatalf("%+v opened=%d", rep, e.db.opened)
	}
	if len(e.audit.all()) != 0 {
		t.Fatal("没有访问目标库不需要审计")
	}
	if rep.FPID != FingerprintID("select * from orders where date(created_at) = ? limit ?") {
		t.Fatalf("指纹 %s", rep.Fingerprint)
	}

	// 选了实例：表结构 + 执行计划
	e.db.tables["orders"] = ordersFake()
	rep, err = e.svc.Advise(ctx(), alice, AdviseReq{InstanceID: 1, DB: "shop", SQL: "select * from orders where phone = 13800000000 order by created_at desc limit 10"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Plan == nil || !hasID(rep.Findings, "P001") || !hasID(rep.Findings, "S001") || !hasID(rep.Findings, "S002") || len(rep.IndexDDL) != 1 {
		t.Fatalf("%s notes=%v planerr=%s", ids(rep.Findings), rep.Notes, rep.PlanError)
	}
	for i := 1; i < len(rep.Findings); i++ { // 级别从高到低
		if levelRank[rep.Findings[i].Level] > levelRank[rep.Findings[i-1].Level] {
			t.Fatalf("建议应按级别从高到低：%+v", rep.Findings)
		}
	}
	if es := e.audit.all(); len(es) != 1 || es[0].Action != "SQL优化-内置建议" {
		t.Fatalf("审计：%+v", es)
	}

	// DDL：没有执行计划，也不报错
	rep, err = e.svc.Advise(ctx(), alice, AdviseReq{InstanceID: 1, DB: "shop", SQL: "alter table orders add index i(a)"})
	if err != nil || rep.Plan != nil || rep.PlanError != "" {
		t.Fatalf("%+v %v", rep, err)
	}
	// 执行计划失败只记在报告里
	e.db.errs["EXPLAIN"] = errors.New("Error 1146 table missing")
	rep, err = e.svc.Advise(ctx(), alice, AdviseReq{InstanceID: 1, DB: "shop", SQL: "select * from orders where id = 1"})
	if err != nil || rep.PlanError == "" || rep.Plan != nil {
		t.Fatalf("%+v %v", rep, err)
	}
	// 权限与类型
	_, err = e.svc.Advise(ctx(), alice, AdviseReq{InstanceID: 2, SQL: "select 1"})
	wantCode(t, err, "no_instance")
	_, err = e.svc.Advise(ctx(), alice, AdviseReq{InstanceID: 3, SQL: "select 1"})
	wantCode(t, err, "unsupported")
	_, err = e.svc.Advise(ctx(), alice, AdviseReq{SQL: "select 1; select 2"})
	wantCode(t, err, "bad_request")
	_, err = e.svc.Advise(ctx(), alice, AdviseReq{InstanceID: 1, DB: "shop", SQL: "select * from hidden.t"})
	wantCode(t, err, "no_database") // hidden 不在实例的库列表里
}

// ---- 分析任务 ----

const analyzeText = "select * from orders where phone = 13800000000;\nupdate stock set qty = 1;\nselect 1;"

func TestAnalyzeJobStaticOnly(t *testing.T) {
	e := newEnv(t)
	v, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: analyzeText})
	if err != nil {
		t.Fatal(err)
	}
	if v.Total != 3 || v.Kind != KindAnalyze || len(v.Items) != 3 || v.ID == "" {
		t.Fatalf("%+v", v)
	}
	if !strings.Contains(v.Notice, "SOAR 不可用") || !strings.Contains(v.Notice, "没有选择实例") {
		t.Fatalf("应说明 SOAR 未配置与未选实例：%q", v.Notice)
	}
	done := waitJob(t, e.svc, alice, v.ID)
	if done.Status != StatusDone || done.Done != 3 || done.Failed != 0 {
		t.Fatalf("%+v", done)
	}
	for _, it := range done.Items {
		if it.Status != StatusDone {
			t.Fatalf("%+v", it)
		}
	}
	if done.Items[1].Level != LevelHigh { // update 没有 where
		t.Fatalf("第 2 条应有高危建议：%+v", done.Items[1])
	}
	if done.Summary == nil || done.Summary.Total != 3 || done.Summary.Unique != 3 || done.Summary.ByType[TypeSelect] != 2 || done.Summary.ByType[TypeUpdate] != 1 || done.Summary.ByLevel[LevelHigh] != 1 {
		t.Fatalf("%+v", done.Summary)
	}
	d, err := e.svc.JobItem(alice, v.ID, 0)
	if err != nil || d.Report == nil || !hasID(d.Report.Findings, "R001") || d.FullSQL != "select * from orders where phone = 13800000000" || d.Fingerprint == "" {
		t.Fatalf("%+v %v", d, err)
	}
	if e.db.opened != 0 || e.runner.count() != 0 {
		t.Fatal("静态分析不应连库或调用外部程序")
	}
	es := e.audit.all()
	if len(es) != 1 || es[0].Action != "SQL分析" || es[0].UserName != "alice" {
		t.Fatalf("审计：%+v", es)
	}
}

func TestAnalyzeJobWithInstanceAndSoar(t *testing.T) {
	e := newEnv(t)
	e.configureTools()
	e.db.tables["orders"] = ordersFake()
	e.runner.res = &ExecResult{Stdout: "# Query: abc\n\n* **Item:** ARG.001 password s3cretPW leaked? tpass1234\n"}
	v, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select * from orders where phone = 1; select 1", InstanceID: 1, DB: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitJob(t, e.svc, alice, v.ID)
	if done.Status != StatusDone || done.Done != 2 {
		t.Fatalf("%+v", done)
	}
	d, _ := e.svc.JobItem(alice, v.ID, 0)
	if d.Report.Plan == nil || !hasID(d.Report.Findings, "S001") || !hasID(d.Report.Findings, "P001") {
		t.Fatalf("应有表结构与执行计划分析：%s", ids(d.Report.Findings))
	}
	if !strings.Contains(d.Report.Soar, "ARG.001") || strings.Contains(d.Report.Soar, "s3cretPW") || strings.Contains(d.Report.Soar, "tpass1234") {
		t.Fatalf("SOAR 报告里的密码必须被遮盖：%q", d.Report.Soar)
	}
	if e.runner.count() != 2 {
		t.Fatalf("每条 SQL 一次 SOAR：%d", e.runner.count())
	}
	spec := e.runner.calls[0]
	want := []string{"-report-type", "markdown", "-online-dsn", "dbuser:s3cretPW@10.0.0.1:3306/shop", "-test-dsn", "tester:tpass1234@10.9.9.9:3306/test",
		"-allow-online-as-test=false", "-query", "select * from orders where phone = 1"}
	if strings.Join(spec.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("SOAR 参数模板：\n%q\n%q", spec.Args, want)
	}
	if spec.Path != testToolPath || spec.Timeout <= 0 || spec.MaxOutput <= 0 {
		t.Fatalf("%+v", spec)
	}
	if e.db.opened != e.db.closed || e.db.opened != 1 {
		t.Fatalf("整个任务只应开一个会话并在结束时关闭 opened=%d closed=%d", e.db.opened, e.db.closed)
	}
	if strings.Contains(done.Notice, "SOAR 不可用") {
		t.Fatalf("SOAR 可用时不应提示不可用：%q", done.Notice)
	}
}

func TestAnalyzeJobDegradesWhenInstanceUnreachable(t *testing.T) {
	e := newEnv(t)
	e.db.openErr = errors.New("connection refused")
	v, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select * from t", InstanceID: 1, DB: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitJob(t, e.svc, alice, v.ID)
	if done.Status != StatusDone || !strings.Contains(done.Notice, "降级为静态分析") {
		t.Fatalf("%+v", done)
	}
	d, _ := e.svc.JobItem(alice, v.ID, 0)
	if d.Report == nil || !hasID(d.Report.Findings, "R001") {
		t.Fatalf("%+v", d)
	}
}

func TestAnalyzeValidation(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "   "})
	wantCode(t, err, "bad_request")
	_, err = e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select \x00"})
	wantCode(t, err, "bad_request")
	_, err = e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 1", InstanceID: 2})
	wantCode(t, err, "no_instance")
	_, err = e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 1", InstanceID: 3})
	wantCode(t, err, "unsupported")
	_, err = e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 1", InstanceID: 1, DB: "a\x01b"})
	wantCode(t, err, "bad_request")
	if len(e.audit.all()) != 0 {
		t.Fatal("被拒绝的请求不应有审计")
	}
}

func TestAnalyzeItemFailuresAndOversize(t *testing.T) {
	e := newEnv(t)
	long := "select '" + strings.Repeat("x", MaxSQLBytes) + "'"
	v, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: long + ";\nselect 2"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitJob(t, e.svc, alice, v.ID)
	if done.Status != StatusDone || done.Failed != 1 || done.Items[0].Status != StatusFailed || done.Items[0].Error == "" || done.Items[1].Status != StatusDone {
		t.Fatalf("%+v", done)
	}
}

func TestJobIsolationAndExpiry(t *testing.T) {
	e := newEnv(t)
	v, _ := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 1"})
	waitJob(t, e.svc, alice, v.ID)
	if _, err := e.svc.Job(bob, v.ID); err == nil {
		t.Fatal("别人的任务应不可见")
	} else {
		wantCode(t, err, "not_found")
	}
	if _, err := e.svc.JobItem(bob, v.ID, 0); err == nil {
		t.Fatal("别人的任务条目应不可见")
	}
	if err := e.svc.CancelJob(bob, v.ID); err == nil {
		t.Fatal("不能取消别人的任务")
	}
	if _, err := e.svc.Job(root, v.ID); err != nil {
		t.Fatalf("超级用户可以看：%v", err)
	}
	if _, err := e.svc.Job(alice, "nope"); err == nil {
		t.Fatal("不存在的任务")
	}
	if _, err := e.svc.JobItem(alice, v.ID, 5); err == nil {
		t.Fatal("条目越界")
	}
	// 过期清理
	e.svc.now = func() time.Time { return time.Now().Add(jobTTL + time.Minute) }
	e.svc.gc()
	if _, err := e.svc.Job(alice, v.ID); err == nil {
		t.Fatal("过期任务应被清理")
	}
}

// S1-A6：取消分析任务写审计（含任务号与标题），取消失败（别人的 / 已结束）不记；超级用户代取消记的是操作人。
func TestCancelJobAudited(t *testing.T) {
	e := newEnv(t)
	e.configureTools()
	e.runner.block = make(chan struct{})
	defer close(e.runner.block)
	v, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 1"})
	if err != nil {
		t.Fatal(err)
	}
	n0 := len(e.audit.all())
	if err := e.svc.CancelJob(bob, v.ID); err == nil {
		t.Fatal("不能取消别人的任务")
	}
	if len(e.audit.all()) != n0 {
		t.Fatalf("取消失败不应写审计: %+v", e.audit.all())
	}
	if err := e.svc.CancelJob(root, v.ID); err != nil {
		t.Fatal(err)
	}
	es := e.audit.all()
	if len(es) != n0+1 {
		t.Fatalf("取消应写 1 条审计: %+v", es)
	}
	last := es[len(es)-1]
	if last.Action != "SQL分析-取消" || last.UserName != root.Username || !strings.Contains(last.ExtraInfo, v.ID) || !strings.Contains(last.ExtraInfo, alice.Username) {
		t.Errorf("审计内容: %+v", last)
	}
	if err := e.svc.CancelJob(root, v.ID); err == nil {
		t.Fatal("已结束的任务不能再取消")
	}
	if len(e.audit.all()) != n0+1 {
		t.Errorf("重复取消不应再写审计: %+v", e.audit.all())
	}
	if c := audit.Classify(last.Action, last.ExtraInfo); c.Code != "sqlanalyze.run" || c.Category != audit.CatSQLFlow {
		t.Errorf("归类: %+v", c)
	}
}

func TestJobLimitsAndCancel(t *testing.T) {
	e := newEnv(t)
	e.configureTools()
	e.runner.block = make(chan struct{})
	released := false
	release := func() {
		if !released {
			released = true
			close(e.runner.block)
		}
	}
	defer release()
	v1, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 1"})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 2"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 3"})
	wantCode(t, err, "too_many_jobs")
	// 别的用户不受影响
	v3, err := e.svc.StartAnalyze(ctx(), bob, AnalyzeReq{Text: "select 3"})
	if err != nil {
		t.Fatalf("其他用户不应被限制：%v", err)
	}
	// 取消一个后，alice 又可以提交
	if err := e.svc.CancelJob(alice, v1.ID); err != nil {
		t.Fatal(err)
	}
	c1 := waitJob(t, e.svc, alice, v1.ID)
	if c1.Status != StatusCanceled || c1.Items[0].Status != StatusCanceled {
		t.Fatalf("%+v", c1)
	}
	if err := e.svc.CancelJob(alice, v1.ID); err == nil {
		t.Fatal("已结束的任务不能再取消")
	} else {
		wantCode(t, err, "conflict")
	}
	v4, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 4"})
	if err != nil {
		t.Fatalf("取消后应能再提交：%v", err)
	}
	release()
	waitJob(t, e.svc, alice, v2.ID)
	waitJob(t, e.svc, alice, v4.ID)
	waitJob(t, e.svc, bob, v3.ID)
}

type runnerFunc func(ctx context.Context, spec ExecSpec) (*ExecResult, error)

func (f runnerFunc) Run(ctx context.Context, spec ExecSpec) (*ExecResult, error) { return f(ctx, spec) }

func TestGlobalConcurrencyBound(t *testing.T) {
	e := newEnv(t)
	e.configureTools()
	var mu sync.Mutex
	cur, peak := 0, 0
	e.svc.runner = runnerFunc(func(ctx context.Context, spec ExecSpec) (*ExecResult, error) {
		mu.Lock()
		cur++
		if cur > peak {
			peak = cur
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		cur--
		mu.Unlock()
		return &ExecResult{Stdout: "ok"}, nil
	})
	var ids []string
	for i := 0; i < 6; i++ {
		a := alice
		a.ID = uint(100 + i) // 6 个不同用户各提交一个 5 条 SQL 的任务
		v, err := e.svc.StartAnalyze(ctx(), a, AnalyzeReq{Text: "select 1;select 2;select 3;select 4;select 5"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, v.ID)
	}
	for _, id := range ids {
		waitJob(t, e.svc, root, id)
	}
	if peak > DefaultConcurrency || peak == 0 {
		t.Fatalf("同时运行的外部程序数应不超过 %d，实际峰值 %d", DefaultConcurrency, peak)
	}
}

func TestJobStoreLimits(t *testing.T) {
	e := newEnv(t)
	// 全局进行中的任务上限
	for i := 0; i < maxActiveAll; i++ {
		j := e.svc.newJob(alice, KindAnalyze, "x")
		j.UserID = uint(1000 + i)
		if err := e.svc.jobs.add(j, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	j := e.svc.newJob(alice, KindAnalyze, "x")
	j.UserID = 5000
	wantCode(t, e.svc.jobs.add(j, time.Now()), "busy")
}

// 没有任何建议的语句（如 select 1）：报告与汇总里的列表必须序列化成 []，页面直接读 findings.length / 遍历 tables，null 会让页面报错。
func TestEmptyListsSerializeAsArrays(t *testing.T) {
	e := newEnv(t)
	rep, err := e.svc.Advise(ctx(), alice, AdviseReq{SQL: "select 1"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rep)
	if !strings.Contains(string(b), `"findings":[]`) {
		t.Fatalf("findings 应为 []：%s", b)
	}
	v, err := e.svc.StartAnalyze(ctx(), alice, AnalyzeReq{Text: "select 1;"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitJob(t, e.svc, alice, v.ID)
	b, _ = json.Marshal(done.Summary)
	if !strings.Contains(string(b), `"tables":[]`) || strings.Contains(string(b), "null") {
		t.Fatalf("汇总里的列表应为 []：%s", b)
	}
}

// 库列表为空时要能说明原因：总数、被系统库 / 显示规则 / 屏蔽规则各排除了几个。
func TestDatabasesDetailExplainsHidden(t *testing.T) {
	e := newEnv(t)
	d, err := e.svc.DatabasesDetail(ctx(), alice, 1)
	if err != nil || d.Total != 5 || d.HiddenSystem != 2 || d.HiddenShow != 0 || d.HiddenDenied != 0 || strings.Join(d.Databases, ",") != "report,secret,shop" {
		t.Fatalf("%+v %v", d, err)
	}
	e.repo.instances[0].ShowDBNameRegex = "zzz"
	e.repo.instances[0].DeniedDBNameRegex = "secret"
	d, err = e.svc.DatabasesDetail(ctx(), alice, 1)
	if err != nil || len(d.Databases) != 0 || d.Total != 5 || d.HiddenSystem != 2 || d.HiddenShow != 3 || d.HiddenDenied != 0 {
		t.Fatalf("显示规则优先于屏蔽规则统计：%+v %v", d, err)
	}
	e.db.dbs = nil
	d, err = e.svc.DatabasesDetail(ctx(), alice, 1)
	if err != nil || d.Total != 0 || len(d.Databases) != 0 {
		t.Fatalf("账号什么库都看不到：%+v %v", d, err)
	}
	if e.db.opened != e.db.closed {
		t.Fatalf("会话泄漏 opened=%d closed=%d", e.db.opened, e.db.closed)
	}
}
