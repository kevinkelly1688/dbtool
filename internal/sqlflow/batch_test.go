// @author: kevinkelly
// @license: Apache Licence
// @file: batch_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/review"
	"dbtool/internal/settings"
	"dbtool/internal/sqlflow"
)

// ---- SQL Server / 批量上线的测试场景 ----
//
// 在 setup 的基础上增加 3 个 SQL Server 实例（id 11、12、13，主机名 ms11…ms13，带 can_write，属于资源组 1），
// 每台实例有自己的假引擎状态；dev 另外拥有 sqlworkflow.submit_batch。

type msEngine struct {
	engine.Engine
	host  string
	state *msState
}

type msState struct {
	mu      sync.Mutex
	ran     []string
	failOn  string // 语句里含这个文本时失败
	dbs     []string
	syntax  func(unit string) string // 服务端语法检查：返回错误文本，空为通过
	noConn  bool                     // 连不上
	parseNo bool                     // 引擎不支持语法检查
}

func (m *msEngine) Close() error { return nil }
func (m *msEngine) AllDatabases(context.Context) ([]string, error) {
	if m.state.noConn {
		return nil, fmt.Errorf("dial tcp %s:1433: connection refused", m.host)
	}
	return m.state.dbs, nil
}
func (m *msEngine) ReadOnly(context.Context) (bool, error) { return false, nil }
func (m *msEngine) ExecEach(_ context.Context, db string, units []string) ([]engine.ExecStmtResult, error) {
	if m.state.noConn {
		return nil, fmt.Errorf("dial tcp %s:1433: connection refused", m.host)
	}
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	out := make([]engine.ExecStmtResult, len(units))
	failed := false
	for i, u := range units {
		out[i].SQL = u
		switch {
		case failed:
			out[i].Skipped = true
		case m.state.failOn != "" && strings.Contains(u, m.state.failOn):
			out[i].Err = "Invalid object name 'Orders'."
			failed = true
		default:
			out[i].Affected = 2
			m.state.ran = append(m.state.ran, db+": "+u)
		}
	}
	return out, nil
}

// syntaxEngine 在 msEngine 之上实现 engine.SyntaxChecker。
type syntaxEngine struct{ *msEngine }

func (s syntaxEngine) SyntaxCheck(_ context.Context, _ string, units []string) ([]string, error) {
	if s.state.noConn {
		return nil, fmt.Errorf("connection refused")
	}
	out := make([]string, len(units))
	for i, u := range units {
		if s.state.syntax != nil {
			out[i] = s.state.syntax(u)
		}
	}
	return out, nil
}

type batchEnv struct {
	*env
	hosts map[string]*msState
}

func setupBatch(t *testing.T) *batchEnv {
	t.Helper()
	e := setup(t)
	tag := []model.InstanceTag{{TagCode: "can_write", Active: true}}
	be := &batchEnv{env: e, hosts: map[string]*msState{}}
	for _, id := range []uint{11, 12, 13} {
		host := fmt.Sprintf("ms%d", id)
		e.repo.Instances = append(e.repo.Instances, model.Instance{ID: id, InstanceName: fmt.Sprintf("mssql-%d", id), DBType: "mssql", Host: host, Port: 1433, Tags: tag})
		e.repo.InstRGs[id] = []uint{1}
		be.hosts[host] = &msState{dbs: []string{"ShopDB", "Report"}}
	}
	e.repo.Perms[1]["sqlworkflow.submit_batch"] = true
	s2 := sqlflow.NewService(e.repo, sqlflow.Options{Notifier: e.cap, Open: func(dbType string, c engine.Conn) (engine.Engine, error) {
		if dbType == "mssql" {
			st := be.hosts[c.Host]
			if st == nil {
				return nil, fmt.Errorf("没有这台假实例 %s", c.Host)
			}
			base := &msEngine{host: c.Host, state: st}
			if st.parseNo {
				return base, nil
			}
			return syntaxEngine{base}, nil
		}
		return &fakeEngine{state: e.eng}, nil
	}})
	s2.SetAsync(func(f func()) { f() })
	s2.SetSpawn(func(f func()) { f() })
	s2.SetClock(func() time.Time { return e.now })
	s2.BaseURL = "http://dba.example.com"
	e.svc = s2
	return be
}

func (be *batchEnv) batchReq(sqlText string, ids ...uint) sqlflow.SubmitReq {
	return sqlflow.SubmitReq{Name: "批量改表", DBName: "ShopDB", GroupName: "研发组", SQL: sqlText, BatchInstanceIDs: ids}
}

// approveAs 走完两级审批。
func (be *batchEnv) approveAll(t *testing.T, id uint) { be.approve(t, id) }

// ---- SQL Server 单实例：检查 ----

func TestMSSQLCheckRulesAndSyntax(t *testing.T) {
	be := setupBatch(t)
	ctx := context.Background()
	chk := func(sqlText string) *sqlflow.CheckResult {
		r, err := be.svc.Preview(ctx, dev, 11, "ShopDB", sqlText, false)
		if err != nil {
			t.Fatalf("%q: %v", sqlText, err)
		}
		return r
	}
	r := chk("update dbo.Orders set status = 1")
	if r.Source != sqlflow.SourceMSSQL || r.ErrorCount != 1 || !strings.Contains(r.Rows[0].ErrorMessage, "[statement.where.require.update-delete]") ||
		r.Rows[0].StageStatus != "规则检查未通过" {
		t.Errorf("缺 WHERE 的 UPDATE: %+v", r.Rows)
	}
	r = chk("update dbo.Orders set status = 1 where id = 7")
	if r.ErrorCount != 0 || r.WarningCount != 0 || r.Rows[0].StageStatus != "Audit completed" || r.Rows[0].ErrorMessage != "None" || r.SyntaxType != sqlflow.SyntaxDML {
		t.Errorf("正常的 UPDATE: %+v", r)
	}
	// DELETE 归为 DDL（本站规则）
	if r = chk("delete from dbo.Orders where id = 1"); r.SyntaxType != sqlflow.SyntaxDDL {
		t.Errorf("DELETE 应归为 DDL: %d", r.SyntaxType)
	}
	// GO 分批：每个批次一行；规则命中的行号是脚本里的真实行号
	r = chk("create table dbo.A(id int primary key)\nGO\n\nupdate dbo.A set id = 1\nGO\nselect 1")
	if len(r.Rows) != 3 || r.SyntaxType != sqlflow.SyntaxDDL || !strings.Contains(r.Rows[1].ErrorMessage, "第 4 行") {
		t.Errorf("GO 分批与行号: %+v", r.Rows)
	}
	// 建表无主键 → 错误；危险语句 → 错误
	if r = chk("create table dbo.B(a int not null)"); r.ErrorCount != 1 || !strings.Contains(r.Rows[0].ErrorMessage, "table.require-pk") {
		t.Errorf("无主键: %+v", r.Rows)
	}
	if r = chk("exec xp_cmdshell 'dir'"); r.ErrorCount != 1 || !strings.Contains(r.Rows[0].ErrorMessage, RuleDangerous) {
		t.Errorf("危险过程: %+v", r.Rows)
	}
	// 全是查询 → 驳回；变更之后附带核对用的 SELECT → 放行并附注
	if r = chk("select * from dbo.Orders where id=1"); r.ErrorCount != 1 || r.Rows[0].StageStatus != "驳回不支持语句" {
		t.Errorf("纯查询应驳回: %+v", r.Rows)
	}
	r = chk("update dbo.Orders set status = 1 where id = 1;\nselect id from dbo.Orders where id = 1")
	if r.ErrorCount != 0 || !strings.Contains(r.Rows[1].ErrorMessage, "查询语句将随工单执行") {
		t.Errorf("变更后附带的 SELECT: %+v", r.Rows)
	}
	// SELECT … INTO 不是只读
	if r = chk("select id into #t from dbo.Orders where id = 1"); strings.Contains(r.Rows[0].StageStatus, "驳回") {
		t.Errorf("SELECT INTO 不算纯查询: %+v", r.Rows)
	}
	// 站点规则：高危正则
	be.repo.Cfg[settings.KeyReviewCriticalDDLRegex] = "^drop"
	if r = chk("drop table dbo.Old"); r.Rows[0].StageStatus != "驳回高危SQL" {
		t.Errorf("critical_ddl_regex: %+v", r.Rows)
	}
	be.repo.Cfg[settings.KeyReviewCriticalDDLRegex] = ""
}

const RuleDangerous = "statement.dangerous"

func TestMSSQLServerSideSyntaxIsAuthoritative(t *testing.T) {
	be := setupBatch(t)
	ctx := context.Background()
	st := be.hosts["ms11"]
	// 服务端说语法错 → 以服务端为准
	st.syntax = func(u string) string {
		if strings.Contains(u, "oops") {
			return "Incorrect syntax near 'oops'."
		}
		return ""
	}
	r, err := be.svc.Preview(ctx, dev, 11, "ShopDB", "update dbo.T set a = 1 where id = 1;\nupdate dbo.T set oops where id = 2", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Rows[0].ErrLevel != 0 || r.Rows[1].ErrLevel != review.LevelError || !strings.Contains(r.Rows[1].ErrorMessage, "语法检测失败: Incorrect syntax near 'oops'.") || r.Rows[1].StageStatus != "语法错误" {
		t.Errorf("服务端语法错误: %+v", r.Rows)
	}
	st.syntax = nil
	// 本地解析器认不出、服务端通过 → 只提醒（文法覆盖不全不能误拦）
	r, _ = be.svc.Preview(ctx, dev, 11, "ShopDB", "selec foo bar baz qux", false)
	if r.ErrorCount != 0 || r.WarningCount != 1 || !strings.Contains(r.Rows[0].ErrorMessage, "服务器语法检测已通过") {
		t.Errorf("本地认不出、服务端通过: %+v", r.Rows)
	}
	// 连不上服务端：本地语法错误只是警告，并带提示
	st.noConn = true
	r, err = be.svc.Preview(ctx, dev, 11, "ShopDB", "update dbo.T set where", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.ErrorCount != 0 || r.WarningCount != 1 || r.Rows[0].StageStatus != "语法待确认" || !strings.Contains(r.Notice, "未能连接目标实例") {
		t.Errorf("连不上服务端: %+v notice=%q", r.Rows, r.Notice)
	}
	st.noConn = false
	// 引擎不支持服务端检查（没有 SyntaxChecker）也走本地
	st.parseNo = true
	if r, _ = be.svc.Preview(ctx, dev, 11, "ShopDB", "update dbo.T set a = 1 where id = 1", false); r.ErrorCount != 0 || r.Notice == "" {
		t.Errorf("无服务端检查: %+v", r)
	}
}

func TestMSSQLReviewRulesConfigurable(t *testing.T) {
	be := setupBatch(t)
	ctx := context.Background()
	be.repo.Cfg[settings.KeyReviewMSSQLRules] = `{"statement.where.require.update-delete":"WARNING"}`
	r, _ := be.svc.Preview(ctx, dev, 11, "ShopDB", "update dbo.Orders set status = 1", false)
	if r.ErrorCount != 0 || r.WarningCount != 1 {
		t.Errorf("规则降为 WARNING: %+v", r.Rows)
	}
	// 配置有误：回退默认规则，不影响检查
	be.repo.Cfg[settings.KeyReviewMSSQLRules] = `{"no.such.rule":"ERROR"}`
	if r, _ = be.svc.Preview(ctx, dev, 11, "ShopDB", "update dbo.Orders set status = 1", false); r.ErrorCount != 1 {
		t.Errorf("配置有误时用默认规则: %+v", r.Rows)
	}
}

// ---- SQL Server 单实例：提交与执行 ----

func TestMSSQLSingleWorkflowExecutesNatively(t *testing.T) {
	be := setupBatch(t)
	be.useInception() // 配了 goInception 也不能用于 SQL Server
	ctx := context.Background()
	r := sqlflow.SubmitReq{Name: "改单", InstanceID: 11, DBName: "ShopDB", GroupName: "研发组", SQL: "update dbo.Orders set status = 2 where id = 5;\ninsert into dbo.Log(a) values (1)", IsBackup: true}
	res, err := be.svc.Submit(ctx, dev, r)
	if err != nil {
		t.Fatal(err)
	}
	id := res.WorkflowID
	w := be.repo.Workflows[id]
	if w.IsBackup || w.SyntaxType != sqlflow.SyntaxDML || w.Status != model.WfManReviewing {
		t.Errorf("SQL Server 工单不支持备份: %+v", w)
	}
	be.approveAll(t, id)
	opts, def := be.svc.ExecutorOptions(&model.SqlWorkflow{Instance: &model.Instance{DBType: "mssql"}})
	if len(opts) != 1 || def != sqlflow.ExecNative {
		t.Errorf("SQL Server 只有原生执行: %v %q", opts, def)
	}
	if _, err := be.svc.Execute(ctx, dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto, Executor: sqlflow.ExecGoInception}); code(err) != "bad_request" {
		t.Errorf("不能选 goInception: %v", err)
	}
	if _, err := be.svc.Execute(ctx, dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if be.status(id) != model.WfFinish || len(be.hosts["ms11"].ran) != 2 {
		t.Fatalf("执行: %s ran=%v", be.status(id), be.hosts["ms11"].ran)
	}
	rows, _ := review.ParseRows(be.repo.Contents[id].ExecuteResult)
	if len(rows) != 3 || rows[0].SQL != "USE [ShopDB]" || rows[1].AffectedRows != 2 {
		t.Errorf("结果第一行是 USE [库]: %+v", rows)
	}
}

func TestMSSQLFailureMarksRemainingAsNotExecuted(t *testing.T) {
	be := setupBatch(t)
	be.hosts["ms11"].failOn = "boom"
	res, err := be.svc.Submit(context.Background(), dev, sqlflow.SubmitReq{Name: "x", InstanceID: 11, DBName: "ShopDB", GroupName: "研发组",
		SQL: "update dbo.A set x = 1 where id = 1;\nupdate dbo.boom set x = 1 where id = 1;\nupdate dbo.C set x = 1 where id = 1"})
	if err != nil {
		t.Fatal(err)
	}
	be.approveAll(t, res.WorkflowID)
	be.svc.Execute(context.Background(), dev, res.WorkflowID, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	rows, _ := review.ParseRows(be.repo.Contents[res.WorkflowID].ExecuteResult)
	if be.status(res.WorkflowID) != model.WfException || len(rows) != 4 {
		t.Fatalf("状态/行数: %s %+v", be.status(res.WorkflowID), rows)
	}
	if rows[2].ErrLevel != review.LevelError || rows[3].StageStatus != "Audit completed" || rows[3].ErrorMessage != "前序语句失败, 未执行" || rows[3].ErrLevel != 0 {
		t.Errorf("同原项目：失败行 + 其后标记“前序语句失败, 未执行”: %+v", rows)
	}
}

// ---- 批量上线：提交校验 ----

func TestBatchSubmitValidation(t *testing.T) {
	be := setupBatch(t)
	ctx := context.Background()
	submit := func(who audit.Actor, r sqlflow.SubmitReq) error { _, err := be.svc.Submit(ctx, who, r); return err }
	sqlText := "update dbo.Orders set status = 1 where id = 1"

	if err := submit(leader, be.batchReq(sqlText, 11, 12)); code(err) != "forbidden" {
		t.Errorf("没有 sqlworkflow.submit_batch: %v", err)
	}
	if err := submit(dev, be.batchReq(sqlText, 11, 1)); code(err) != "bad_request" || !strings.Contains(err.Error(), "批量上线只支持") {
		t.Errorf("批量只允许 SQL Server: %v", err)
	}
	// 实例没有 can_write / 不在资源组 / 不存在
	be.repo.Instances = append(be.repo.Instances, model.Instance{ID: 14, InstanceName: "mssql-14", DBType: "mssql", Host: "ms14", Tags: []model.InstanceTag{{TagCode: "can_write", Active: true}}})
	be.hosts["ms14"] = &msState{dbs: []string{"ShopDB"}}
	if err := submit(dev, be.batchReq(sqlText, 11, 14)); code(err) != "bad_request" || !strings.Contains(err.Error(), "未关联实例") {
		t.Errorf("实例不属于资源组: %v", err)
	}
	if err := submit(dev, be.batchReq(sqlText, 11, 99)); code(err) != "bad_request" {
		t.Errorf("实例不存在: %v", err)
	}
	r := be.batchReq(sqlText, 11, 12)
	r.SkipCheck = true
	if err := submit(root, r); code(err) != "bad_request" {
		t.Errorf("批量不支持跳过检查: %v", err)
	}
	var many []uint
	for i := 0; i < 101; i++ {
		many = append(many, uint(1000+i))
	}
	if err := submit(dev, be.batchReq(sqlText, many...)); code(err) != "bad_request" || !strings.Contains(err.Error(), "最多支持 100") {
		t.Errorf("实例数上限: %v", err)
	}
	// 混合类型：把实例 12 改成别的类型（但仍允许批量）→ 类型不一致
	be.svc2Types(t, []string{"mssql", "mysql"})
	be.repo.Instances = append(be.repo.Instances, model.Instance{ID: 15, InstanceName: "mysql-15", DBType: "mysql", Host: "my15", Tags: []model.InstanceTag{{TagCode: "can_write", Active: true}}})
	be.repo.InstRGs[15] = []uint{1}
	if err := submit(dev, be.batchReq(sqlText, 11, 15)); code(err) != "bad_request" || !strings.Contains(err.Error(), "类型不一致") {
		t.Errorf("混合类型: %v", err)
	}
	if len(be.repo.Workflows) != 0 {
		t.Errorf("校验失败不应留下工单: %d", len(be.repo.Workflows))
	}
}

// svc2Types 重建服务，允许批量上线的类型换成 types（沿用同样的打桩）。
func (be *batchEnv) svc2Types(t *testing.T, types []string) {
	t.Helper()
	old := be.svc
	_ = old
	s := sqlflow.NewService(be.repo, sqlflow.Options{Notifier: be.cap, BatchDBTypes: types, Open: func(dbType string, c engine.Conn) (engine.Engine, error) {
		if st := be.hosts[c.Host]; st != nil {
			return syntaxEngine{&msEngine{host: c.Host, state: st}}, nil
		}
		return &fakeEngine{state: be.eng}, nil
	}})
	s.SetAsync(func(f func()) { f() })
	s.SetSpawn(func(f func()) { f() })
	s.SetClock(func() time.Time { return be.now })
	be.svc = s
}

func TestBatchPrecheckBlocksUnreachableInstanceAndMissingDB(t *testing.T) {
	be := setupBatch(t)
	ctx := context.Background()
	be.hosts["ms12"].noConn = true
	be.hosts["ms13"].dbs = []string{"Other"}
	sqlText := "update dbo.Orders set status = 1 where id = 1"
	pr, err := be.svc.PreviewBatch(ctx, dev, []uint{11, 12, 13}, "ShopDB", sqlText)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Instances) != 3 || !pr.Instances[0].OK || pr.Instances[1].OK || pr.Instances[2].OK ||
		!strings.Contains(pr.Instances[1].Error, "无法连接") || !strings.Contains(pr.Instances[2].Error, "没有找到数据库 ShopDB") || !strings.Contains(pr.Notice, "有实例无法使用") {
		t.Errorf("预检: %+v notice=%q", pr.Instances, pr.Notice)
	}
	if _, err := be.svc.Submit(ctx, dev, be.batchReq(sqlText, 11, 12, 13)); code(err) != "bad_request" || !strings.Contains(err.Error(), "mssql-12") || !strings.Contains(err.Error(), "mssql-13") {
		t.Errorf("有实例不可用时不允许提交，并列出实例: %v", err)
	}
	// 库名大小写不敏感（SQL Server 默认不区分）
	be.hosts["ms12"].noConn, be.hosts["ms13"].dbs = false, []string{"shopdb"}
	if _, err := be.svc.Submit(ctx, dev, be.batchReq(sqlText, 11, 12, 13)); err != nil {
		t.Errorf("预检通过后应能提交: %v", err)
	}
}

// ---- 批量上线：执行、重试、关闭 ----

func submitBatch(t *testing.T, be *batchEnv, ids ...uint) uint {
	t.Helper()
	res, err := be.svc.Submit(context.Background(), dev, be.batchReq("update dbo.Orders set status = 1 where id = 1", ids...))
	if err != nil {
		t.Fatal(err)
	}
	be.approveAll(t, res.WorkflowID)
	return res.WorkflowID
}

func execBatch(t *testing.T, be *batchEnv, id uint) {
	t.Helper()
	if _, err := be.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
}

func TestBatchExecuteAllSucceed(t *testing.T) {
	be := setupBatch(t)
	id := submitBatch(t, be, 11, 12, 13)
	w := be.repo.Workflows[id]
	if w.BatchInstanceIDs == nil || *w.BatchInstanceIDs != "11,12,13" || w.InstanceID != 11 {
		t.Errorf("批量工单字段: %+v", w)
	}
	be.cap.reset()
	execBatch(t, be, id)
	if be.status(id) != model.WfFinish {
		t.Fatalf("状态: %s", be.status(id))
	}
	for _, h := range []string{"ms11", "ms12", "ms13"} {
		if len(be.hosts[h].ran) != 1 {
			t.Errorf("%s 应各执行一次: %v", h, be.hosts[h].ran)
		}
	}
	rows, _ := review.ParseRows(be.repo.Contents[id].ExecuteResult)
	// 每个实例 2 行（USE + 语句）+ 汇总
	if len(rows) != 7 || !rows[6].IsSummary || !strings.Contains(rows[6].ErrorMessage, "共 3 个实例，成功 3 个，失败 0 个") || rows[6].ErrLevel != 0 {
		t.Fatalf("结果行: %+v", rows)
	}
	if rows[0].InstanceID != 11 || rows[0].InstanceName != "mssql-11" || rows[2].InstanceID != 12 || rows[4].InstanceID != 13 || rows[6].ID != 7 {
		t.Errorf("结果行带实例标记并按实例顺序归并: %+v", rows)
	}
	// 通知里的实例名：首个实例名 + 总数
	last := be.cap.msgs[len(be.cap.msgs)-1]
	if !strings.Contains(last.Content, "mssql-11...等 3 个实例（批量上线）") {
		t.Errorf("通知: %s", last.Content)
	}
	d, err := be.svc.Get(dev, id)
	if err != nil || !d.IsBatch || len(d.BatchInstances) != 3 || !d.BatchInstances[0].Success || len(d.BatchTargets) != 3 || !d.CanBatchOp {
		t.Errorf("详情: %+v %v", d, err)
	}
}

func TestBatchPartialFailureRetryAndClose(t *testing.T) {
	be := setupBatch(t)
	id := submitBatch(t, be, 11, 12, 13)
	be.hosts["ms12"].failOn = "Orders"
	be.hosts["ms13"].noConn = false
	execBatch(t, be, id)
	if be.status(id) != model.WfException {
		t.Fatalf("部分失败 → 执行有异常: %s", be.status(id))
	}
	rows, _ := review.ParseRows(be.repo.Contents[id].ExecuteResult)
	sum := rows[len(rows)-1]
	if !strings.Contains(sum.ErrorMessage, "成功 2 个，失败 1 个") || !strings.Contains(sum.ErrorMessage, "mssql-12") || !strings.Contains(sum.ErrorMessage, "已有 2 个实例的变更已生效") || sum.ErrLevel != review.LevelWarning {
		t.Errorf("汇总: %+v", sum)
	}
	d, _ := be.svc.Get(dev, id)
	if len(d.BatchInstances) != 3 || !d.BatchInstances[1].Failed || !d.CanBatchOp {
		t.Errorf("详情里的实例状态: %+v", d.BatchInstances)
	}
	ctx := context.Background()

	// 权限与状态检查
	if _, _, err := be.svc.RetryBatchInstance(ctx, leader, id, 12); code(err) != "forbidden" {
		t.Errorf("没有执行权限: %v", err)
	}
	if _, _, err := be.svc.RetryBatchInstance(ctx, outsider, id, 12); code(err) != "not_found" {
		t.Errorf("其他资源组: %v", err)
	}
	if _, _, err := be.svc.RetryBatchInstance(ctx, dev, id, 11); code(err) != "bad_request" || !strings.Contains(err.Error(), "无需重试") {
		t.Errorf("成功的实例无需重试: %v", err)
	}
	if _, _, err := be.svc.RetryBatchInstance(ctx, dev, id, 99); code(err) != "bad_request" {
		t.Errorf("不属于本工单: %v", err)
	}

	// 仍然失败的重试：结果被替换，状态不变
	msg, status, err := be.svc.RetryBatchInstance(ctx, dev, id, 12)
	if err != nil || status != model.WfException || !strings.Contains(msg, "重试失败") {
		t.Fatalf("重试仍失败: %q %q %v", msg, status, err)
	}
	if len(be.hosts["ms11"].ran) != 1 || len(be.hosts["ms13"].ran) != 1 {
		t.Errorf("重试只动失败的实例: %v %v", be.hosts["ms11"].ran, be.hosts["ms13"].ran)
	}
	rows, _ = review.ParseRows(be.repo.Contents[id].ExecuteResult)
	n12 := 0
	for _, r := range rows {
		if r.InstanceID == 12 {
			n12++
		}
	}
	if n12 != 2 || !rows[len(rows)-1].IsSummary {
		t.Errorf("旧结果行应被替换而不是追加: %d 行属于实例 12", n12)
	}

	// 故障排除后重试成功 → 工单正常结束
	be.hosts["ms12"].failOn = ""
	msg, status, err = be.svc.RetryBatchInstance(ctx, dev, id, 12)
	if err != nil || status != model.WfFinish || !strings.Contains(msg, "重试成功") || be.status(id) != model.WfFinish {
		t.Fatalf("重试成功: %q %q %v", msg, status, err)
	}
	if _, _, err := be.svc.RetryBatchInstance(ctx, dev, id, 12); code(err) != "bad_request" {
		t.Errorf("成功后不能再重试: %v", err)
	}
	var descs []string
	for _, l := range be.repo.Logs {
		descs = append(descs, l.OperationInfo)
	}
	joined := strings.Join(descs, "|")
	if !strings.Contains(joined, "重试实例：mssql-12") || !strings.Contains(joined, "重试结果：实例 mssql-12 重试成功") {
		t.Errorf("日志: %v", descs)
	}
}

func TestBatchCloseDoesNotLaunderWorkflow(t *testing.T) {
	be := setupBatch(t)
	id := submitBatch(t, be, 11, 12)
	be.hosts["ms11"].failOn, be.hosts["ms12"].failOn = "Orders", "Orders"
	execBatch(t, be, id)
	// 全部失败：既然没有成功的实例，状态是异常
	if be.status(id) != model.WfException {
		t.Fatalf("状态: %s", be.status(id))
	}
	rows, _ := review.ParseRows(be.repo.Contents[id].ExecuteResult)
	if !strings.Contains(rows[len(rows)-1].ErrorMessage, "成功 0 个，失败 2 个") {
		t.Errorf("汇总: %+v", rows[len(rows)-1])
	}
	// 关闭一个：另一个仍失败 → 仍是异常
	msg, status, err := be.svc.CloseBatchInstance(dev, id, 11)
	if err != nil || status != model.WfException || !strings.Contains(msg, "已关闭") {
		t.Fatalf("关闭: %q %q %v", msg, status, err)
	}
	if _, _, err := be.svc.CloseBatchInstance(dev, id, 11); code(err) != "bad_request" {
		t.Errorf("重复关闭: %v", err)
	}
	if _, _, err := be.svc.RetryBatchInstance(context.Background(), dev, id, 11); code(err) != "bad_request" || !strings.Contains(err.Error(), "已关闭") {
		t.Errorf("已关闭的实例不能重试: %v", err)
	}
	// 全部关闭：不能把工单“洗成”已完成
	if _, status, err = be.svc.CloseBatchInstance(dev, id, 12); err != nil || status != model.WfException {
		t.Errorf("全部关闭也不算成功: %q %v", status, err)
	}
	rows, _ = review.ParseRows(be.repo.Contents[id].ExecuteResult)
	if !strings.Contains(rows[len(rows)-1].ErrorMessage, "已关闭 2 个") {
		t.Errorf("汇总: %+v", rows[len(rows)-1])
	}
}

func TestBatchCloseFailedInstanceFinishesWhenOthersSucceeded(t *testing.T) {
	be := setupBatch(t)
	id := submitBatch(t, be, 11, 12)
	be.hosts["ms12"].failOn = "Orders"
	execBatch(t, be, id)
	if _, status, err := be.svc.CloseBatchInstance(dev, id, 12); err != nil || status != model.WfFinish || be.status(id) != model.WfFinish {
		t.Errorf("关闭失败实例后，其余都成功 → 工单正常结束: %q %v", status, err)
	}
}

func TestBatchStateGuards(t *testing.T) {
	be := setupBatch(t)
	id := submitBatch(t, be, 11, 12)
	ctx := context.Background()
	if _, _, err := be.svc.CloseBatchInstance(dev, id, 11); code(err) != "conflict" {
		t.Errorf("没执行完的工单不能操作: %v", err)
	}
	single, err := be.svc.Submit(ctx, dev, sqlflow.SubmitReq{Name: "单", InstanceID: 11, DBName: "ShopDB", GroupName: "研发组", SQL: "update dbo.T set a=1 where id=1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := be.svc.CloseBatchInstance(dev, single.WorkflowID, 11); code(err) != "bad_request" {
		t.Errorf("不是批量工单: %v", err)
	}
}

func TestBatchRunsInstancesInParallel(t *testing.T) {
	be := setupBatch(t)
	id := submitBatch(t, be, 11, 12, 13)
	// 三台实例同时到达栅栏才会放行：串行执行会超时
	var arrived sync.WaitGroup
	arrived.Add(3)
	release := make(chan struct{})
	go func() { arrived.Wait(); close(release) }()
	for _, st := range be.hosts {
		st := st
		st.syntax = nil
		_ = st
	}
	be.svc.SetOpener(func(dbType string, c engine.Conn) (engine.Engine, error) {
		return &barrierEngine{msEngine: &msEngine{host: c.Host, state: be.hosts[c.Host]}, arrived: &arrived, release: release}, nil
	})
	done := make(chan struct{})
	go func() { execBatch(t, be, id); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("三个 SQL Server 实例应并发执行（串行会在栅栏处卡住）")
	}
	if be.status(id) != model.WfFinish {
		t.Errorf("状态: %s", be.status(id))
	}
	be.repo.Cfg[settings.KeyReviewBatchWorkers] = "2"
}

type barrierEngine struct {
	*msEngine
	arrived *sync.WaitGroup
	release chan struct{}
}

func (b *barrierEngine) ExecEach(ctx context.Context, db string, units []string) ([]engine.ExecStmtResult, error) {
	b.arrived.Done()
	<-b.release
	return b.msEngine.ExecEach(ctx, db, units)
}

func TestBatchMissingInstanceRecorded(t *testing.T) {
	be := setupBatch(t)
	id := submitBatch(t, be, 11, 12)
	// 审批后 12 被删除
	var kept []model.Instance
	for _, in := range be.repo.Instances {
		if in.ID != 12 {
			kept = append(kept, in)
		}
	}
	be.repo.Instances = kept
	execBatch(t, be, id)
	rows, _ := review.ParseRows(be.repo.Contents[id].ExecuteResult)
	var found bool
	for _, r := range rows {
		if r.InstanceID == 12 && strings.Contains(r.ErrorMessage, "已不存在，跳过执行") && r.ErrLevel == review.LevelError {
			found = true
		}
	}
	if !found || be.status(id) != model.WfException {
		t.Errorf("被删除的实例必须明确记录，而不是静默少执行: %s %+v", be.status(id), rows)
	}
}

func TestBatchDDLConflictUsesInstanceSets(t *testing.T) {
	be := setupBatch(t)
	res, err := be.svc.Submit(context.Background(), dev, be.batchReq("alter table dbo.Orders add c int null", 11, 12))
	if err != nil {
		t.Fatal(err)
	}
	be.approveAll(t, res.WorkflowID)
	ids := "12,13"
	be.repo.Workflows[900] = model.SqlWorkflow{ID: 900, WorkflowName: "另一个批量DDL", Status: model.WfExecuting, DBName: "ShopDB", SyntaxType: sqlflow.SyntaxDDL, InstanceID: 12, BatchInstanceIDs: &ids}
	if _, err := be.svc.Execute(context.Background(), dev, res.WorkflowID, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); code(err) != "conflict" || !strings.Contains(err.Error(), "#900") {
		t.Errorf("实例集合有交集（12）应冲突: %v", err)
	}
	ids = "13"
	w := be.repo.Workflows[900]
	w.InstanceID = 13
	be.repo.Workflows[900] = w
	if _, err := be.svc.Execute(context.Background(), dev, res.WorkflowID, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Errorf("没有交集不冲突: %v", err)
	}
}

func TestBatchListAndNotifyDisplay(t *testing.T) {
	be := setupBatch(t)
	id := submitBatch(t, be, 11, 12, 13)
	single, _ := be.svc.Submit(context.Background(), dev, sqlflow.SubmitReq{Name: "单", InstanceID: 1, DBName: "shop", GroupName: "研发组", SQL: "update t set a=1 where id=1"})
	rows, total, err := be.svc.List(dev, sqlflow.ListQuery{})
	if err != nil || total != 2 {
		t.Fatalf("%v %d", err, total)
	}
	var item sqlflow.WorkflowItem
	for _, r := range rows {
		if r.ID == id {
			item = r
		}
	}
	if item.BatchCount != 3 || !strings.Contains(item.InstanceName, "等 3 个实例") {
		t.Errorf("列表里批量工单: %+v", item)
	}
	only := func(q sqlflow.ListQuery) []uint {
		rs, _, _ := be.svc.List(dev, q)
		var out []uint
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return out
	}
	if got := only(sqlflow.ListQuery{Batch: "1"}); len(got) != 1 || got[0] != id {
		t.Errorf("只看批量: %v", got)
	}
	if got := only(sqlflow.ListQuery{Batch: "0"}); len(got) != 1 || got[0] != single.WorkflowID {
		t.Errorf("只看单实例: %v", got)
	}
	if got := only(sqlflow.ListQuery{InstanceID: 13}); len(got) != 1 || got[0] != id {
		t.Errorf("按批量里的实例筛选: %v", got)
	}
	if got := only(sqlflow.ListQuery{InstanceID: 1}); len(got) != 1 || got[0] != single.WorkflowID {
		t.Errorf("按实例筛选: %v", got)
	}
	// 通知：提交时就是“首个实例名…等 N 个实例”
	if first := be.cap.msgs[0]; first.Name != notify.EventPending || !strings.Contains(first.Content, "mssql-11...等 3 个实例（批量上线）") {
		t.Errorf("提交通知: %+v", first)
	}
}

func TestBatchOptionsAndPermissions(t *testing.T) {
	be := setupBatch(t)
	o, err := be.svc.SubmitOptions(dev)
	if err != nil || !o.Batch.Allowed || len(o.Batch.Types) != 1 || o.Batch.Types[0] != "mssql" || o.Batch.Max != 100 {
		t.Errorf("批量选项: %+v %v", o.Batch, err)
	}
	var mssqlCount int
	for _, in := range o.Instances[1] {
		if in.DBType == "mssql" {
			mssqlCount++
		}
	}
	if mssqlCount != 3 {
		t.Errorf("提交页应列出 3 个 SQL Server 实例: %+v", o.Instances[1])
	}
	if o2, _ := be.svc.SubmitOptions(leader); o2.Batch.Allowed {
		t.Error("没有 sqlworkflow.submit_batch 不应开放批量")
	}
	if !be.svc.CanSubmit(dev) {
		t.Error("CanSubmit")
	}
	be.repo.Perms[8]["sqlworkflow.submit_batch"] = true // stranger 只有批量权限，没有 sqlworkflow.submit
	delete(be.repo.Perms[8], "sqlworkflow.submit")
	if !be.svc.CanSubmit(stranger) {
		t.Error("只有批量权限也能打开提交页")
	}
	dbs, err := be.svc.Databases(context.Background(), stranger, 11)
	if err != nil || len(dbs) != 2 {
		t.Errorf("批量用户查库: %v %v", dbs, err)
	}
}
