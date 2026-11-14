// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	"dbtool/internal/goinception"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	base "dbtool/internal/privapply/memrepo"
	"dbtool/internal/query"
	"dbtool/internal/review"
	"dbtool/internal/settings"
	"dbtool/internal/sqlflow"
	"dbtool/internal/sqlflow/memrepo"
)

// ---- 假实现 ----

type capture struct {
	mu   sync.Mutex
	msgs []notify.Event
}

func (c *capture) Notify(_ context.Context, ev notify.Event) {
	c.mu.Lock()
	c.msgs = append(c.msgs, ev)
	c.mu.Unlock()
}

func (c *capture) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.msgs {
		out = append(out, m.Name)
	}
	return out
}

func (c *capture) reset() { c.mu.Lock(); c.msgs = nil; c.mu.Unlock() }

// fakeEngine 同时实现 engine.Engine（只用到 Close/AllDatabases）和 engine.Writer。
type fakeEngine struct {
	engine.Engine
	state *fakeState
}

type fakeState struct {
	mu        sync.Mutex
	readOnly  bool
	failOn    string // 语句里含这个文本时失败
	ran       []string
	dbs       []string
	onExec    func() // 执行语句时调用（测试里观察执行期间的状态）
	ignoreCtx bool   // 取消了也照常执行完（模拟“取消请求到得太晚”）
}

func (f *fakeEngine) Close() error { return nil }
func (f *fakeEngine) AllDatabases(context.Context) ([]string, error) {
	return f.state.dbs, nil
}
func (f *fakeEngine) ReadOnly(context.Context) (bool, error) { return f.state.readOnly, nil }
func (f *fakeEngine) ExecEach(ctx context.Context, db string, stmts []string) ([]engine.ExecStmtResult, error) {
	if f.state.onExec != nil {
		f.state.onExec()
	}
	if err := ctx.Err(); err != nil && !f.state.ignoreCtx { // 被取消：第一条被中断，其后的不执行
		out := make([]engine.ExecStmtResult, len(stmts))
		for i, st := range stmts {
			out[i].SQL = st
			if i == 0 {
				out[i].Err = err.Error()
			} else {
				out[i].Skipped = true
			}
		}
		return out, nil
	}
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	out := make([]engine.ExecStmtResult, len(stmts))
	failed := false
	for i, s := range stmts {
		out[i].SQL = s
		switch {
		case failed:
			out[i].Skipped = true
		case f.state.failOn != "" && strings.Contains(s, f.state.failOn):
			out[i].Err = "Error 1146: Table doesn't exist"
			failed = true
		default:
			out[i].Affected = 3
			f.state.ran = append(f.state.ran, db+": "+s)
		}
	}
	return out, nil
}

// fakeInception 记录调用并返回预设结果。
type fakeInception struct {
	mu       sync.Mutex
	checkOut []review.Row
	execOut  []review.Row
	execErr  error
	oscOut   []map[string]any
	calls    []string
	execOpts goinception.ExecOpts
}

func (f *fakeInception) Check(_ context.Context, _ goinception.Target, db, stmt string, _ goinception.CheckOpts) ([]review.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "check:"+db)
	return f.checkOut, nil
}
func (f *fakeInception) Execute(_ context.Context, _ goinception.Target, db, stmt string, o goinception.ExecOpts) ([]review.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "execute:"+db)
	f.execOpts = o
	return f.execOut, f.execErr
}
func (f *fakeInception) Osc(_ context.Context, cmd, sha string) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "osc:"+cmd+":"+sha)
	return f.oscOut, nil
}

type fakeBackup struct{ items []goinception.Rollback }

func (b fakeBackup) Rollbacks(context.Context, []review.Row) ([]goinception.Rollback, error) {
	return b.items, nil
}

// ---- 场景 ----
//
// 资源组 1「研发组」：dev(1) 提交人，leader(2) 组长，dba(3)。审批流：组长(10) -> DBA(20)。
// 实例 1 带 can_write，属于资源组 1；实例 2 没有标签；实例 3 带标签但不属于任何资源组。
// outsider(9) 在资源组 2，同样属于“组长”权限组并有 sql_review（用来测试跨资源组越权）。
// stranger(8) 在资源组 1 但没有任何工单相关权限（只能看自己的）。

var (
	dev      = audit.Actor{ID: 1, Username: "dev", Display: "开发者"}
	leader   = audit.Actor{ID: 2, Username: "leader", Display: "组长"}
	dba      = audit.Actor{ID: 3, Username: "dba", Display: "DBA"}
	outsider = audit.Actor{ID: 9, Username: "outsider", Display: "外人"}
	stranger = audit.Actor{ID: 8, Username: "stranger", Display: "路人"}
	root     = audit.Actor{ID: 100, Username: "root", Display: "管理员", IsSuperuser: true}
)

type env struct {
	svc  *sqlflow.Service
	repo *memrepo.Repo
	cap  *capture
	eng  *fakeState
	inc  *fakeInception
	now  time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	tag := []model.InstanceTag{{TagCode: "can_write", Active: true}}
	b := base.New()
	b.Users = map[uint]model.User{1: {ID: 1, Username: "dev"}, 2: {ID: 2, Username: "leader"}, 3: {ID: 3, Username: "dba"}, 9: {ID: 9, Username: "outsider"}, 8: {ID: 8, Username: "stranger"}}
	b.Groups = map[uint]*model.AuthGroup{10: {ID: 10, Name: "组长"}, 20: {ID: 20, Name: "DBA"}}
	b.Members = map[uint]map[uint]bool{2: {10: true}, 3: {20: true}, 9: {10: true}}
	b.RGs = []model.ResourceGroup{{GroupID: 1, GroupName: "研发组"}, {GroupID: 2, GroupName: "其他组"}}
	b.UserRGs = map[uint][]uint{1: {1}, 2: {1}, 3: {1}, 8: {1}, 9: {2}}
	b.Instances = []model.Instance{
		{ID: 1, InstanceName: "prod-mysql-01", DBType: "mysql", Tags: tag},
		{ID: 2, InstanceName: "no-tag", DBType: "mysql"},
		{ID: 3, InstanceName: "orphan", DBType: "mysql", Tags: tag},
	}
	b.InstRGs = map[uint][]uint{1: {1}, 2: {1}}
	b.Perms = map[uint]map[string]bool{
		1: grants(grantSubmit, grantExecute, grantViewOwn),
		2: grants(grantReview, grantSubmit),
		3: grants(grantReview, grantExecGroup, grantExecute),
		8: grants(grantSubmit),
		9: grants(grantReview, grantExecGroup),
	}
	b.Settings = map[[2]int]*model.WorkflowAuditSetting{{1, 2}: {GroupID: 1, WorkflowType: 2, AuditAuthGroups: "10,20"}}
	r := memrepo.New(b)
	c := &capture{}
	st := &fakeState{dbs: []string{"shop", "report"}}
	inc := &fakeInception{}
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.Local)

	s := sqlflow.NewService(r, sqlflow.Options{Notifier: c, Open: func(string, engine.Conn) (engine.Engine, error) {
		return &fakeEngine{state: st}, nil
	}})
	s.SetAsync(func(f func()) { f() })
	s.SetSpawn(func(f func()) { f() })
	s.SetClock(func() time.Time { return now })
	s.SetInception(func(goinception.Config) sqlflow.Inception { return inc }, func(goinception.BackupConfig) sqlflow.BackupReader { return fakeBackup{} })
	s.BaseURL = "http://dba.example.com"
	return &env{svc: s, repo: r, cap: c, eng: st, inc: inc, now: now}
}

func (e *env) useInception() {
	e.repo.Cfg[settings.KeyGoInceptionHost] = "127.0.0.1"
	e.repo.Cfg[settings.KeyGoInceptionBackupHost] = "127.0.0.1"
}

func code(err error) string {
	var qe *query.Error
	if errors.As(err, &qe) {
		return qe.Code
	}
	if err == nil {
		return ""
	}
	return "?:" + err.Error()
}

func req(sqlText string) sqlflow.SubmitReq {
	return sqlflow.SubmitReq{Name: "给 orders 加索引", InstanceID: 1, DBName: "shop", GroupName: "研发组", SQL: sqlText}
}

func (e *env) submit(t *testing.T, who audit.Actor, sqlText string) uint {
	t.Helper()
	res, err := e.svc.Submit(context.Background(), who, req(sqlText))
	if err != nil {
		t.Fatalf("提交: %v", err)
	}
	return res.WorkflowID
}

// approve 走完两级审批，工单变为“审核通过”。
func (e *env) approve(t *testing.T, id uint) {
	t.Helper()
	if _, err := e.svc.Pass(leader, id, "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Pass(dba, id, "ok"); err != nil {
		t.Fatal(err)
	}
}

func (e *env) status(id uint) string { return e.repo.Workflows[id].Status }

// ---- 语句类型 ----

func TestStatementType(t *testing.T) {
	cases := map[string]int{
		"alter table t add c int": sqlflow.SyntaxDDL, "  CREATE TABLE x(id int)": sqlflow.SyntaxDDL, "drop table t": sqlflow.SyntaxDDL,
		"truncate t": sqlflow.SyntaxDDL, "delete from t where id=1": sqlflow.SyntaxDDL, // DELETE 归为 DDL
		"/* c */ insert into t values(1)": sqlflow.SyntaxDML, "update t set a=1": sqlflow.SyntaxDML, "select 1": sqlflow.SyntaxDML,
		"use shop": sqlflow.SyntaxOther, "grant all on *.* to x": sqlflow.SyntaxOther, "deleted_thing": sqlflow.SyntaxOther,
	}
	for in, want := range cases {
		if got := sqlflow.StatementType(in); got != want {
			t.Errorf("%q: got %d want %d", in, got, want)
		}
	}
	if sqlflow.SetSyntaxType([]string{"update t set a=1", "alter table t add c int"}) != sqlflow.SyntaxDDL ||
		sqlflow.SetSyntaxType([]string{"update t set a=1", "insert into t values(1)"}) != sqlflow.SyntaxDML {
		t.Error("工单类型：有一条 DDL 就是 DDL")
	}
}

// ---- 提交 ----

func TestSubmitNativeFlow(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "alter table orders add index idx_a(a);\nupdate orders set a=1 where id=1;")
	w := e.repo.Workflows[id]
	if w.Status != model.WfManReviewing || w.SyntaxType != sqlflow.SyntaxDDL || w.GroupName != "研发组" || w.Engineer != "dev" || w.IsBackup {
		t.Errorf("工单: %+v", w)
	}
	if w.AuditAuthGroups != "10,20" {
		t.Errorf("审批流: %q", w.AuditAuthGroups)
	}
	rows, _ := review.ParseRows(e.repo.Contents[id].ReviewContent)
	if len(rows) != 2 || rows[0].SQL != "alter table orders add index idx_a(a)" {
		t.Errorf("检查结果: %+v", rows)
	}
	au, _ := e.repo.AuditByWorkflow(audit.TypeSQLReview, id, false)
	if au == nil || au.CurrentStatus != audit.StatusWaiting || au.CurrentAudit != "10" {
		t.Errorf("审批记录: %+v", au)
	}
	// 通知：待处理，@ 组长权限组
	if len(e.cap.msgs) != 1 || e.cap.msgs[0].Name != notify.EventPending || e.cap.msgs[0].Kind != notify.KindSQLWorkflow ||
		e.cap.msgs[0].ReviewerAuthGroupID != 10 || e.cap.msgs[0].Applicant != "dev" ||
		!strings.Contains(e.cap.msgs[0].Content, "http://dba.example.com/sqlworkflow/") {
		t.Errorf("通知: %+v", e.cap.msgs)
	}
	// 消息模板用的变量：提交时带全套工单信息、SQL 全文与当前审批级
	v := e.cap.msgs[0].Vars
	if v["kind"] != "SQL上线申请" || v["applicant_user"] != "dev" || v["group"] != "研发组" || v["db"] != "shop" || v["syntax"] != "DDL" ||
		v["current_step"] == "" || v["flow"] == "" || v["ticket_id"] == "" || !strings.Contains(v["url"], "/sqlworkflow/") ||
		!strings.Contains(v["sql"], "alter table orders add index idx_a(a)") || !strings.Contains(v["sql"], "update orders set a=1") {
		t.Errorf("模板变量: %+v", v)
	}
	if k := notify.MessageKey(e.cap.msgs[0]); k != "sql.submitted" {
		t.Errorf("消息键: %q", k)
	}
}

// 没套消息模板时，默认文案里的“工单详情预览”由 notify_sql_preview 控制：mask 隐藏字符串常量，none 不显示 SQL；变量里的 SQL 全文不受影响。
func TestNotifySQLPreviewOption(t *testing.T) {
	const stmt = "update orders set note='top-secret-value' where id=1"
	for _, c := range []struct{ mode, want, deny string }{
		{"", "top-secret-value", ""},
		{"head", "top-secret-value", ""},
		{"mask", "note='***'", "top-secret-value"},
		{"none", "SQL 已隐藏", "top-secret-value"},
	} {
		e := setup(t)
		e.repo.Cfg[settings.KeyNotifySQLPreview] = c.mode
		e.submit(t, dev, stmt)
		ev := e.cap.msgs[0]
		if !strings.Contains(ev.Content, c.want) || (c.deny != "" && strings.Contains(ev.Content, c.deny)) {
			t.Errorf("mode=%q 默认文案: %s", c.mode, ev.Content)
		}
		if !strings.Contains(ev.Vars["sql"], "top-secret-value") {
			t.Errorf("mode=%q：变量里的 SQL 全文由模板决定怎么显示，不应被改: %q", c.mode, ev.Vars["sql"])
		}
	}
}

func TestSubmitValidation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	mod := func(f func(*sqlflow.SubmitReq)) sqlflow.SubmitReq { r := req("update t set a=1"); f(&r); return r }
	cases := []struct {
		name string
		who  audit.Actor
		r    sqlflow.SubmitReq
		code string
	}{
		{"没有提交权限", audit.Actor{ID: 55, Username: "nobody"}, req("update t set a=1"), "forbidden"},
		{"名称为空", dev, mod(func(r *sqlflow.SubmitReq) { r.Name = " " }), "bad_request"},
		{"名称过长", dev, mod(func(r *sqlflow.SubmitReq) { r.Name = strings.Repeat("长", 51) }), "bad_request"},
		{"需求链接不是 http(s)", dev, mod(func(r *sqlflow.SubmitReq) { r.DemandURL = "javascript:alert(1)" }), "bad_request"},
		{"没选库", dev, mod(func(r *sqlflow.SubmitReq) { r.DBName = "" }), "bad_request"},
		{"SQL 为空", dev, mod(func(r *sqlflow.SubmitReq) { r.SQL = " ; " }), "bad_request"},
		{"资源组不存在", dev, mod(func(r *sqlflow.SubmitReq) { r.GroupName = "不存在" }), "bad_request"},
		{"不在资源组内", outsider, mod(func(r *sqlflow.SubmitReq) {}), "forbidden"},
		{"实例缺少 can_write", dev, mod(func(r *sqlflow.SubmitReq) { r.InstanceID = 2 }), "bad_request"},
		{"实例不属于所选资源组", dev, mod(func(r *sqlflow.SubmitReq) { r.InstanceID = 3 }), "bad_request"},
		{"实例不存在", dev, mod(func(r *sqlflow.SubmitReq) { r.InstanceID = 99 }), "bad_request"},
		{"普通用户不能跳过检查", dev, mod(func(r *sqlflow.SubmitReq) { r.SkipCheck = true }), "forbidden"},
		{"时间窗反了", dev, mod(func(r *sqlflow.SubmitReq) {
			a, b := e.now.Add(2*time.Hour), e.now.Add(time.Hour)
			r.RunStart, r.RunEnd = &a, &b
		}), "bad_request"},
	}
	for _, c := range cases {
		if _, err := e.svc.Submit(ctx, c.who, c.r); code(err) != c.code {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if len(e.repo.Workflows) != 0 {
		t.Errorf("校验失败不应留下工单: %d", len(e.repo.Workflows))
	}
}

func TestSubmitGroupKindCheck(t *testing.T) {
	e := setup(t)
	e.repo.RGs = append(e.repo.RGs, model.ResourceGroup{GroupID: 3, GroupName: "生产DML"}, model.ResourceGroup{GroupID: 4, GroupName: "生产DDL"})
	e.repo.UserRGs[1] = []uint{1, 3, 4}
	e.repo.InstRGs[1] = []uint{1, 3, 4}
	e.repo.Settings[[2]int{3, 2}] = &model.WorkflowAuditSetting{GroupID: 3, WorkflowType: 2, AuditAuthGroups: "10"}
	e.repo.Settings[[2]int{4, 2}] = &model.WorkflowAuditSetting{GroupID: 4, WorkflowType: 2, AuditAuthGroups: "10"}
	r := req("alter table t add c int")
	r.GroupName = "生产DML"
	if _, err := e.svc.Submit(context.Background(), dev, r); code(err) != "bad_request" || !strings.Contains(err.Error(), "DDL资源组") {
		t.Errorf("DDL 选 DML 组: %v", err)
	}
	r = req("delete from t where id=1") // DELETE 也算 DDL
	r.GroupName = "生产DML"
	if _, err := e.svc.Submit(context.Background(), dev, r); code(err) != "bad_request" {
		t.Errorf("DELETE 选 DML 组应拒绝: %v", err)
	}
	r = req("update t set a=1")
	r.GroupName = "生产DDL"
	if _, err := e.svc.Submit(context.Background(), dev, r); code(err) != "bad_request" || !strings.Contains(err.Error(), "DML资源组") {
		t.Errorf("DML 选 DDL 组: %v", err)
	}
	r = req("update t set a=1")
	r.GroupName = "生产DML"
	if _, err := e.svc.Submit(context.Background(), dev, r); err != nil {
		t.Errorf("DML 选 DML 组: %v", err)
	}
	// 组名不含关键字：不区分
	if _, err := e.svc.Submit(context.Background(), dev, req("alter table t add c int")); err != nil {
		t.Errorf("普通组: %v", err)
	}
}

func TestNativeCheckRejectsSelectAndUnknown(t *testing.T) {
	e := setup(t)
	res, err := e.svc.Submit(context.Background(), dev, req("select * from t"))
	if err != nil {
		t.Fatal(err)
	}
	// 检查有错误 + 默认配置 → 系统直接驳回
	if res.Status != model.WfAutoReviewWrong || !strings.Contains(res.Message, "直接审核不通过") {
		t.Errorf("select 应被自动驳回: %+v", res)
	}
	au, _ := e.repo.AuditByWorkflow(audit.TypeSQLReview, res.WorkflowID, false)
	if au.CurrentStatus != audit.StatusRejected {
		t.Errorf("审批状态: %+v", au)
	}
	if len(e.repo.Logs) == 0 || !strings.Contains(e.repo.Logs[0].OperationInfo, "系统直接审核不通过") {
		t.Errorf("日志: %+v", e.repo.Logs)
	}
	if names := e.cap.names(); len(names) != 1 || names[0] != notify.EventRejected {
		t.Errorf("通知: %v", names)
	}
	// 配置为“错误不自动驳回”(auto_review_wrong=3)：进入人工审批
	e.repo.Cfg[settings.KeyReviewAutoRejectLevel] = "3"
	res, _ = e.svc.Submit(context.Background(), dev, req("use other_db"))
	if res.Status != model.WfManReviewing {
		t.Errorf("auto_review_wrong=3 应进入人工审批: %+v", res)
	}
}

func TestCriticalRegexAndSeparation(t *testing.T) {
	e := setup(t)
	e.repo.Cfg[settings.KeyReviewCriticalDDLRegex] = "^drop|^truncate"
	r, err := e.svc.Preview(context.Background(), dev, 1, "shop", "update t set a=1; DROP table t", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.ErrorCount != 1 || r.Rows[1].StageStatus != "驳回高危SQL" || !strings.Contains(r.Rows[1].ErrorMessage, "禁止提交匹配") {
		t.Errorf("高危语句: %+v", r.Rows)
	}
	e.repo.Cfg[settings.KeyReviewCriticalDDLRegex] = ""
	e.repo.Cfg[settings.KeyReviewDDLDMLSeparation] = "true"
	r, _ = e.svc.Preview(context.Background(), dev, 1, "shop", "update t set a=1; alter table t add c int", false)
	if r.ErrorCount != 1 || !strings.Contains(r.Rows[1].ErrorMessage, "DDL语句和DML语句不能同时执行") {
		t.Errorf("DDL/DML 分离: %+v", r.Rows)
	}
}

func TestPreviewPermissions(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.svc.Preview(ctx, audit.Actor{ID: 55, Username: "nobody"}, 1, "shop", "update t set a=1", false); code(err) != "forbidden" {
		t.Errorf("无提交权限: %v", err)
	}
	if _, err := e.svc.Preview(ctx, outsider, 1, "shop", "update t set a=1", false); code(err) != "forbidden" {
		t.Errorf("实例不在自己的资源组: %v", err) // outsider 没有 sqlworkflow.submit
	}
	r, err := e.svc.Preview(ctx, root, 1, "shop", "update t set a=1", true)
	if err != nil || r.Source != sqlflow.SourceManual || len(r.Rows) != 1 {
		t.Errorf("管理员跳过检查预览: %+v %v", r, err)
	}
	if _, err := e.svc.Preview(ctx, dev, 1, "shop", "update t set a=1", true); code(err) != "forbidden" {
		t.Errorf("普通用户跳过检查: %v", err)
	}
	if r, err := e.svc.Preview(ctx, dev, 1, "shop", "update t set a=1", false); err != nil || r.Source != sqlflow.SourceNative || r.Notice == "" {
		t.Errorf("没有 goInception 的预览应是原生检查并带提示: %+v %v", r, err)
	}
}

func TestRequireInceptionBlocksNativeCheck(t *testing.T) {
	e := setup(t)
	s2 := sqlflow.NewService(e.repo, sqlflow.Options{Notifier: e.cap, RequireInception: true})
	if _, err := s2.Preview(context.Background(), dev, 1, "shop", "update t set a=1", false); code(err) != "no_inception" {
		t.Errorf("强制 goInception 且未配置: %v", err)
	}
	if opts, def := s2.ExecutorOptions(&model.SqlWorkflow{}); len(opts) != 0 || def != "" {
		t.Errorf("没有可用执行方式: %v %q", opts, def)
	}
}

// ---- goInception 检查 ----

func inceptionRows(sqls ...string) []review.Row {
	var rows []review.Row
	for i, s := range sqls {
		rows = append(rows, review.Row{ID: review.Flex(i + 1), Stage: "CHECKED", StageStatus: "Audit completed", ErrorMessage: "None", SQL: s, AffectedRows: 1, SQLSha1: fmt.Sprintf("sha%d", i+1)})
	}
	return rows
}

func TestInceptionReturningNothingIsAnError(t *testing.T) {
	e := setup(t)
	e.useInception()
	e.inc.checkOut = nil
	if _, err := e.svc.Submit(context.Background(), dev, req("update t set a=1")); code(err) != "inception_failed" {
		t.Errorf("goInception 没返回结果不应创建工单: %v", err)
	}
	if len(e.repo.Workflows) != 0 {
		t.Error("不应留下工单")
	}
}

func TestSubmitWithInceptionForcesBackup(t *testing.T) {
	e := setup(t)
	e.useInception()
	e.inc.checkOut = inceptionRows("use `shop`", "update t set a=1")
	id := e.submit(t, dev, "update t set a=1")
	w := e.repo.Workflows[id]
	if !w.IsBackup || w.SyntaxType != sqlflow.SyntaxDML {
		t.Errorf("没开备份开关时 goInception 工单应强制备份: %+v", w)
	}
	// 开了备份开关：按用户选择
	e.repo.Cfg[settings.KeyReviewBackupEnabled] = "true"
	res, _ := e.svc.Submit(context.Background(), dev, req("update t set a=1"))
	if e.repo.Workflows[res.WorkflowID].IsBackup {
		t.Error("开了备份开关且用户没勾选，不应备份")
	}
	r2 := req("update t set a=1")
	r2.IsBackup = true
	res, _ = e.svc.Submit(context.Background(), dev, r2)
	if !e.repo.Workflows[res.WorkflowID].IsBackup {
		t.Error("用户勾选备份")
	}
}

func TestInceptionErrorRowsAutoReject(t *testing.T) {
	e := setup(t)
	e.useInception()
	rows := inceptionRows("update t set a=1")
	rows[0].ErrLevel, rows[0].ErrorMessage = review.LevelError, "表 t 不存在"
	e.inc.checkOut = rows
	res, _ := e.svc.Submit(context.Background(), dev, req("update t set a=1"))
	if res.Status != model.WfAutoReviewWrong {
		t.Errorf("有错误: %+v", res)
	}
	// 只有警告：默认不驳回；review.auto_reject_level=warning 才驳回
	rows[0].ErrLevel = review.LevelWarning
	res, _ = e.svc.Submit(context.Background(), dev, req("update t set a=1"))
	if res.Status != model.WfManReviewing {
		t.Errorf("只有警告默认进入审批: %+v", res)
	}
	e.repo.Cfg[settings.KeyReviewAutoRejectLevel] = "warning"
	res, _ = e.svc.Submit(context.Background(), dev, req("update t set a=1"))
	if res.Status != model.WfAutoReviewWrong {
		t.Errorf("auto_reject_level=warning 警告也驳回: %+v", res)
	}
}

func TestAutoReviewRules(t *testing.T) {
	e := setup(t)
	e.useInception()
	e.repo.Cfg[settings.KeyReviewAutoApproveEnabled] = "true"
	e.repo.Cfg[settings.KeyReviewAutoApproveDBTypes] = "mysql,oracle"
	e.repo.Cfg[settings.KeyReviewAutoApproveTags] = "can_write"
	e.repo.Cfg[settings.KeyReviewAutoApproveMaxRows] = "10"

	e.inc.checkOut = inceptionRows("use `shop`", "update t set a=1 where id=1")
	res, _ := e.svc.Submit(context.Background(), dev, req("update t set a=1 where id=1"))
	if res.Status != model.WfReviewPass || !strings.Contains(res.Message, "无需审批") {
		t.Fatalf("命中自动审核: %+v", res)
	}
	au, _ := e.repo.AuditByWorkflow(audit.TypeSQLReview, res.WorkflowID, false)
	if au.CurrentStatus != audit.StatusPassed || au.AuditAuthGroups != "" {
		t.Errorf("审批: %+v", au)
	}
	if names := e.cap.names(); names[len(names)-1] != notify.EventPassed {
		t.Errorf("通知: %v", names)
	}

	// 影响行数超过阈值 → 人工审批
	rows := inceptionRows("update t set a=1")
	rows[0].AffectedRows = 11
	e.inc.checkOut = rows
	res, _ = e.svc.Submit(context.Background(), dev, req("update t set a=1"))
	if res.Status != model.WfManReviewing {
		t.Errorf("行数超限: %+v", res)
	}
	// 命中需要人工复核的语句（默认正则含 ^alter）
	e.inc.checkOut = inceptionRows("-- 注释\nALTER table t add c int")
	res, _ = e.svc.Submit(context.Background(), dev, req("alter table t add c int"))
	if res.Status != model.WfManReviewing {
		t.Errorf("DDL 应人工审批: %+v", res)
	}
	// 标签不匹配
	e.repo.Cfg[settings.KeyReviewAutoApproveTags] = "other"
	e.inc.checkOut = inceptionRows("update t set a=1")
	if res, _ = e.svc.Submit(context.Background(), dev, req("update t set a=1")); res.Status != model.WfManReviewing {
		t.Errorf("标签不匹配: %+v", res)
	}
	// 开关关闭
	e.repo.Cfg[settings.KeyReviewAutoApproveTags] = "can_write"
	e.repo.Cfg[settings.KeyReviewAutoApproveEnabled] = "false"
	if res, _ = e.svc.Submit(context.Background(), dev, req("update t set a=1")); res.Status != model.WfManReviewing {
		t.Errorf("开关关闭: %+v", res)
	}
}

func TestAutoReviewNeverAppliesToNativeCheck(t *testing.T) {
	e := setup(t) // 没有 goInception：拿不到影响行数
	e.repo.Cfg[settings.KeyReviewAutoApproveEnabled] = "true"
	e.repo.Cfg[settings.KeyReviewAutoApproveDBTypes] = "mysql"
	e.repo.Cfg[settings.KeyReviewAutoApproveTags] = "can_write"
	res, _ := e.svc.Submit(context.Background(), dev, req("update t set a=1 where id=1"))
	if res.Status != model.WfManReviewing {
		t.Errorf("原生检查不允许自动通过: %+v", res)
	}
}

func TestEmptyAuditFlowAutoPasses(t *testing.T) {
	e := setup(t)
	e.repo.Settings[[2]int{1, 2}].AuditAuthGroups = ""
	res, err := e.svc.Submit(context.Background(), dev, req("update t set a=1"))
	if err != nil || res.Status != model.WfReviewPass {
		t.Errorf("空审批流应自动通过: %+v %v", res, err)
	}
	delete(e.repo.Settings, [2]int{1, 2})
	if _, err := e.svc.Submit(context.Background(), dev, req("update t set a=1")); code(err) != "audit_config" {
		t.Errorf("没配置审批流应报错: %v", err)
	}
}

// ---- 审批 ----

func TestApprovalFlowAndCrossGroup(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	e.cap.reset()
	if _, err := e.svc.Pass(outsider, id, "x"); code(err) != "forbidden" {
		t.Errorf("其他资源组的同名权限组成员不能审批: %v", err)
	}
	if _, err := e.svc.Pass(dba, id, "x"); code(err) != "forbidden" {
		t.Errorf("DBA 不是当前节点: %v", err)
	}
	if _, err := e.svc.Pass(dev, id, "x"); code(err) != "forbidden" {
		t.Errorf("没有审批权限: %v", err)
	}
	w, err := e.svc.Pass(leader, id, "同意")
	if err != nil || w.Status != model.WfManReviewing {
		t.Fatalf("一级通过后仍待审核: %+v %v", w, err)
	}
	if names := e.cap.names(); len(names) != 1 || names[0] != notify.EventPending || e.cap.msgs[0].ReviewerAuthGroupID != 20 {
		t.Errorf("流转到 DBA 的通知: %+v", e.cap.msgs)
	}
	w, err = e.svc.Pass(dba, id, "通过")
	if err != nil || w.Status != model.WfReviewPass {
		t.Fatalf("终审通过: %+v %v", w, err)
	}
	if names := e.cap.names(); names[len(names)-1] != notify.EventPassed {
		t.Errorf("通过通知: %v", names)
	}
	if _, err := e.svc.Pass(dba, id, "x"); code(err) != "conflict" {
		t.Errorf("已通过不能再审: %v", err)
	}
}

func TestBanSelfAudit(t *testing.T) {
	e := setup(t)
	e.repo.Perms[2]["sqlworkflow.submit"] = true
	e.repo.UserRGs[2] = []uint{1}
	e.repo.Cfg[settings.KeyApprovalForbidSelf] = "true"
	id := e.submit(t, leader, "update t set a=1")
	if _, err := e.svc.Pass(leader, id, "自己批"); code(err) != "forbidden" {
		t.Errorf("禁止自审: %v", err)
	}
	if _, err := e.svc.Pass(root, id, "管理员可以"); err != nil {
		t.Errorf("管理员不受限: %v", err)
	}
}

func TestNotifyPhaseControl(t *testing.T) {
	e := setup(t)
	e.repo.Cfg[settings.KeyNotifyPhases] = "Execute"
	id := e.submit(t, dev, "update t set a=1") // 提交通知不受阶段控制
	e.cap.reset()
	e.svc.Pass(leader, id, "ok")
	if len(e.cap.msgs) != 0 {
		t.Errorf("Pass 阶段关闭时不应通知: %v", e.cap.names())
	}
	e.svc.Pass(dba, id, "ok")
	if _, err := e.svc.Cancel(dev, id, "不做了"); err != nil {
		t.Fatal(err)
	}
	if len(e.cap.msgs) != 0 {
		t.Errorf("Cancel 阶段关闭: %v", e.cap.names())
	}
}

func TestCancelRules(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	if _, err := e.svc.Cancel(dev, id, ""); code(err) != "bad_request" {
		t.Errorf("终止原因不能为空: %v", err)
	}
	if _, err := e.svc.Cancel(stranger, id, "x"); code(err) != "not_found" {
		t.Errorf("看不到的工单按不存在处理: %v", err)
	}
	if _, err := e.svc.Cancel(dba, id, "x"); code(err) != "forbidden" {
		t.Errorf("不是当前审批人: %v", err)
	}
	e.cap.reset()
	// 当前审批人驳回
	if _, err := e.svc.Cancel(leader, id, "方案不对"); err != nil {
		t.Fatal(err)
	}
	au, _ := e.repo.AuditByWorkflow(audit.TypeSQLReview, id, false)
	if e.status(id) != model.WfAbort || au.CurrentStatus != audit.StatusRejected || e.cap.names()[0] != notify.EventRejected {
		t.Errorf("驳回: %s %+v %v", e.status(id), au, e.cap.names())
	}
	if _, err := e.svc.Cancel(dev, id, "x"); code(err) != "conflict" {
		t.Errorf("已终止不能再终止: %v", err)
	}

	// 提交人撤回
	id2 := e.submit(t, dev, "update t set a=2")
	e.cap.reset()
	if _, err := e.svc.Cancel(dev, id2, "写错了"); err != nil {
		t.Fatal(err)
	}
	au, _ = e.repo.AuditByWorkflow(audit.TypeSQLReview, id2, false)
	if au.CurrentStatus != audit.StatusAborted || e.cap.names()[0] != notify.EventAborted {
		t.Errorf("撤回: %+v %v", au, e.cap.names())
	}

	// 审核通过后：有执行权限的 DBA 可以驳回；其他资源组的人不行
	id3 := e.submit(t, dev, "update t set a=3")
	e.approve(t, id3)
	if _, err := e.svc.Cancel(outsider, id3, "x"); code(err) != "not_found" {
		t.Errorf("其他资源组: %v", err)
	}
	if _, err := e.svc.Cancel(leader, id3, "x"); code(err) != "forbidden" {
		t.Errorf("组长没有执行权限，通过后不能终止: %v", err)
	}
	if _, err := e.svc.Cancel(dba, id3, "窗口期取消"); err != nil {
		t.Fatalf("DBA 终止已通过的工单: %v", err)
	}
	au, _ = e.repo.AuditByWorkflow(audit.TypeSQLReview, id3, false)
	if e.status(id3) != model.WfAbort || au.CurrentStatus != audit.StatusRejected {
		t.Errorf("通过后被驳回: %s %+v", e.status(id3), au)
	}
}

func TestAlterRunDate(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	st, en := e.now.Add(time.Hour), e.now.Add(3*time.Hour)
	if err := e.svc.AlterRunDate(dba, id, &st, &en); code(err) != "forbidden" {
		t.Errorf("非当前审批人: %v", err)
	}
	if err := e.svc.AlterRunDate(leader, id, &en, &st); code(err) != "bad_request" {
		t.Errorf("时间反了: %v", err)
	}
	if err := e.svc.AlterRunDate(leader, id, &st, &en); err != nil {
		t.Fatal(err)
	}
	w := e.repo.Workflows[id]
	if w.RunDateStart == nil || !w.RunDateStart.Equal(st) || !w.RunDateEnd.Equal(en) {
		t.Errorf("时间窗: %+v", w)
	}
	e.approve(t, id)
	if err := e.svc.AlterRunDate(leader, id, nil, nil); code(err) != "conflict" {
		t.Errorf("审批结束后不能改: %v", err)
	}
}

// ---- 执行 ----

func TestExecuteNativeSuccess(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update orders set a=1 where id=1;\ndelete from orders where id=2")
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); code(err) != "conflict" {
		t.Errorf("没审批完不能执行: %v", err)
	}
	e.approve(t, id)
	e.cap.reset()
	if _, err := e.svc.Execute(context.Background(), stranger, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); code(err) != "forbidden" {
		t.Errorf("路人没有执行权限: %v", err)
	}
	if _, err := e.svc.Execute(context.Background(), outsider, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); code(err) != "not_found" {
		t.Errorf("有执行权限但在别的资源组: %v", err)
	}
	if _, err := e.svc.Execute(context.Background(), leader, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); code(err) != "forbidden" {
		t.Errorf("没有执行权限: %v", err)
	}
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto, Executor: "goinception"}); code(err) != "bad_request" {
		t.Errorf("没配置 goInception 不能选它: %v", err)
	}
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: "weird"}); code(err) != "bad_request" {
		t.Errorf("未知模式: %v", err)
	}
	if e.status(id) != model.WfReviewPass {
		t.Fatalf("失败的执行请求不应改变状态: %s", e.status(id))
	}
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	w := e.repo.Workflows[id]
	if w.Status != model.WfFinish || w.FinishTime == nil {
		t.Fatalf("执行结果: %+v", w)
	}
	if len(e.eng.ran) != 2 || e.eng.ran[0] != "shop: update orders set a=1 where id=1" {
		t.Errorf("实际执行: %v", e.eng.ran)
	}
	rows, _ := review.ParseRows(e.repo.Contents[id].ExecuteResult)
	if len(rows) != 2 || rows[0].AffectedRows != 3 || rows[0].StageStatus != "Execute Successfully" || rows[0].BackupDBName != "None" {
		t.Errorf("执行结果行: %+v", rows)
	}
	var descs []string
	for _, l := range e.repo.Logs {
		descs = append(descs, l.OperationTypeDesc)
	}
	if got := strings.Join(descs[len(descs)-3:], ","); got != "执行工单,执行工单,执行结束" {
		t.Errorf("日志: %v", descs)
	}
	if names := e.cap.names(); strings.Join(names, ",") != "executing,finished" {
		t.Errorf("通知: %v", names)
	}
	last := e.cap.msgs[len(e.cap.msgs)-1]
	if !strings.Contains(last.Title, "已正常结束") {
		t.Errorf("标题: %s", last.Title)
	}
	// 执行成功的模板变量：结束时间、影响行数（3 + 3 行）、耗时；没有错误信息
	if v := last.Vars; v["finish_at"] == "" || v["affected_rows"] == "" || v["error"] != "" || !strings.Contains(v["sql"], "delete from orders") {
		t.Errorf("执行成功的模板变量: %+v", v)
	}
	if k := notify.MessageKey(last); k != "sql.finished" {
		t.Errorf("消息键: %q", k)
	}
	// 执行报表：汇总 + 逐条执行记录（非批量工单每条语句一行）
	if v := last.Vars; v["total_count"] != "2" || v["success_count"] != "2" || v["fail_count"] != "0" ||
		v["exec_summary"] != "总数: 2 | 成功: 2 | 失败: 0 | 影响行数: 6" || strings.Count(v["exec_log"], "\n") != 1 || !strings.HasPrefix(v["exec_log"], "ID: 1 | 状态: ✅ Execute Successfully | 影响行数: 3") {
		t.Errorf("执行报表变量: %+v", v)
	}
	if !strings.Contains(last.Content, "\n执行汇总：总数: 2 | 成功: 2 | 失败: 0 | 影响行数: 6\n执行记录：\nID: 1 | 状态: ✅ Execute Successfully") {
		t.Errorf("默认文案里应有执行汇总与执行记录: %q", last.Content)
	}
	// 不能重复执行
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); code(err) != "conflict" {
		t.Errorf("重复执行: %v", err)
	}
}

func TestExecuteNativeFailureStopsAndMarksException(t *testing.T) {
	e := setup(t)
	e.eng.failOn = "boom"
	id := e.submit(t, dev, "update a set x=1; update boom set x=1; update c set x=1")
	e.approve(t, id)
	e.cap.reset()
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != model.WfException {
		t.Fatalf("状态: %s", e.status(id))
	}
	rows, _ := review.ParseRows(e.repo.Contents[id].ExecuteResult)
	if len(rows) != 3 || rows[0].ErrLevel != 0 || rows[1].ErrLevel != review.LevelError || rows[1].StageStatus != "Execute failed" ||
		rows[2].Stage != "SKIPPED" || len(e.eng.ran) != 1 {
		t.Errorf("结果: %+v ran=%v", rows, e.eng.ran)
	}
	if names := e.cap.names(); names[len(names)-1] != notify.EventException {
		t.Errorf("通知: %v", names)
	}
}

func TestExecuteReadOnlyInstanceRefused(t *testing.T) {
	e := setup(t)
	e.eng.readOnly = true
	id := e.submit(t, dev, "update a set x=1")
	e.approve(t, id)
	e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	rows, _ := review.ParseRows(e.repo.Contents[id].ExecuteResult)
	if e.status(id) != model.WfException || len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "read_only=1") || len(e.eng.ran) != 0 {
		t.Errorf("只读实例: %s %+v", e.status(id), rows)
	}
}

// SelectDB / Doris 的 read_only 是兼容 MySQL 协议的固定值 1，不能据此拦截；MySQL 报 1 仍然拒绝（上一个用例）。
func TestExecuteSelectDBIgnoresReadOnlyFlag(t *testing.T) {
	for _, typ := range []string{"selectdb", "doris"} {
		e := setup(t)
		e.repo.Instances[0].DBType = typ
		e.eng.readOnly = true // 实例上 @@global.read_only 读出来是 1
		id := e.submit(t, dev, "update a set x=1")
		e.approve(t, id)
		e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto, Executor: sqlflow.ExecNative})
		rows, _ := review.ParseRows(e.repo.Contents[id].ExecuteResult)
		for _, r := range rows {
			if strings.Contains(r.ErrorMessage, "read_only=1") {
				t.Fatalf("%s 不应因为 read_only 变量被拦：%+v", typ, rows)
			}
		}
		if len(e.eng.ran) == 0 {
			t.Errorf("%s 应该执行了语句：状态 %s %+v", typ, e.status(id), rows)
		}
	}
}

func TestExecuteViaInceptionAndRollback(t *testing.T) {
	e := setup(t)
	e.useInception()
	e.inc.checkOut = inceptionRows("use `shop`", "update t set a=1")
	id := e.submit(t, dev, "update t set a=1")
	e.approve(t, id)
	opts, def := e.svc.ExecutorOptions(&model.SqlWorkflow{})
	if def != sqlflow.ExecGoInception || len(opts) != 2 {
		t.Errorf("配置了 goInception 默认推荐它，原生仍可选: %v %q", opts, def)
	}
	e.inc.execOut = []review.Row{
		{ID: 1, Stage: "EXECUTED", StageStatus: "Execute Successfully", ErrorMessage: "None", SQL: "use `shop`", BackupDBName: "None"},
		{ID: 2, Stage: "EXECUTED", StageStatus: "Execute Successfully", ErrorMessage: "None", SQL: "update t set a=1", AffectedRows: 5, BackupDBName: "bk_db", Sequence: "'1_1_1'", SQLSha1: "sha2"},
	}
	// 不指定执行方式 → 推荐默认（goInception）
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != model.WfFinish || !e.inc.execOpts.Backup || len(e.eng.ran) != 0 {
		t.Fatalf("goInception 执行: %s backup=%v nativeRan=%v", e.status(id), e.inc.execOpts.Backup, e.eng.ran)
	}
	if got := e.inc.calls[len(e.inc.calls)-1]; got != "execute:shop" {
		t.Errorf("调用: %v", e.inc.calls)
	}

	// 回滚语句
	s2 := e.svc
	s2.SetInception(nil, func(goinception.BackupConfig) sqlflow.BackupReader {
		return fakeBackup{items: []goinception.Rollback{{SQL: "update t set a=1", Rollback: "UPDATE `shop`.`t` SET a=0 WHERE id=1;"}}}
	})
	w, items, err := s2.Rollbacks(context.Background(), dev, id)
	if err != nil || len(items) != 1 || !strings.Contains(items[0].Rollback, "SET a=0") {
		t.Fatalf("回滚: %+v %v", items, err)
	}
	script := sqlflow.RollbackScript(w, items, e.now)
	if !strings.Contains(script, "-- 原语句：update t set a=1") || !strings.Contains(script, "UPDATE `shop`.`t` SET a=0") {
		t.Errorf("下载内容:\n%s", script)
	}
	if _, _, err := s2.Rollbacks(context.Background(), stranger, id); code(err) != "not_found" {
		t.Errorf("看不到的工单: %v", err)
	}
	delete(e.repo.Cfg, settings.KeyGoInceptionBackupHost)
	if _, _, err := s2.Rollbacks(context.Background(), dev, id); code(err) != "no_backup_db" {
		t.Errorf("没配置备份库: %v", err)
	}
}

func TestRollbackRequiresBackupAndFinished(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1") // 原生检查：不备份
	if _, _, err := e.svc.Rollbacks(context.Background(), dev, id); code(err) != "conflict" {
		t.Errorf("没执行/没备份: %v", err)
	}
}

func TestInceptionExecuteErrorRowMarksException(t *testing.T) {
	e := setup(t)
	e.useInception()
	e.inc.checkOut = inceptionRows("update t set a=1")
	id := e.submit(t, dev, "update t set a=1")
	e.approve(t, id)
	e.inc.execOut = []review.Row{{ID: 1, Stage: "EXECUTED", ErrLevel: review.LevelError, StageStatus: "Execute failed", ErrorMessage: "Duplicate entry", SQL: "update t set a=1"}}
	e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	if e.status(id) != model.WfException {
		t.Errorf("状态: %s", e.status(id))
	}
	// 连接中断、没有任何结果行
	id2 := e.submit(t, dev, "update t set a=2")
	e.approve(t, id2)
	e.inc.execOut, e.inc.execErr = nil, errors.New("connection reset")
	e.svc.Execute(context.Background(), dev, id2, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	rows, _ := review.ParseRows(e.repo.Contents[id2].ExecuteResult)
	if e.status(id2) != model.WfException || len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "connection reset") {
		t.Errorf("无结果的失败: %s %+v", e.status(id2), rows)
	}
}

func TestExecuteWindowAndDDLConflict(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "alter table t add c int")
	st, en := e.now.Add(time.Hour), e.now.Add(2*time.Hour)
	if err := e.svc.AlterRunDate(leader, id, &st, &en); err != nil {
		t.Fatal(err)
	}
	e.approve(t, id)
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); code(err) != "conflict" || !strings.Contains(err.Error(), "不在可执行时间范围内") {
		t.Errorf("窗口外: %v", err)
	}
	// 窗口内：改时钟
	e.svc.SetClock(func() time.Time { return e.now.Add(90 * time.Minute) })
	// 同库另一个 DDL 正在执行
	e.repo.Workflows[900] = model.SqlWorkflow{ID: 900, WorkflowName: "另一个DDL", Status: model.WfExecuting, DBName: "shop", SyntaxType: sqlflow.SyntaxDDL, InstanceID: 1}
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); code(err) != "conflict" || !strings.Contains(err.Error(), "#900") {
		t.Errorf("DDL 并发冲突: %v", err)
	}
	// 不同库不冲突；DML 不受限
	w := e.repo.Workflows[900]
	w.DBName = "report"
	e.repo.Workflows[900] = w
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Errorf("不同库: %v", err)
	}
}

func TestExecuteManualConfirm(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "alter table t add c int")
	e.approve(t, id)
	e.cap.reset()
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeManual}); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != model.WfFinish || len(e.eng.ran) != 0 {
		t.Errorf("手工确认: %s ran=%v", e.status(id), e.eng.ran)
	}
	if e.repo.Logs[len(e.repo.Logs)-1].OperationTypeDesc != "手工工单" || e.cap.names()[0] != notify.EventFinished {
		t.Errorf("日志/通知: %+v %v", e.repo.Logs[len(e.repo.Logs)-1], e.cap.names())
	}
}

func TestDDLFinishNotifiesExtraGroup(t *testing.T) {
	e := setup(t)
	e.repo.Cfg[settings.KeyNotifyDDLExtraGroups] = "DBA"
	id := e.submit(t, dev, "alter table t add c int")
	e.approve(t, id)
	e.cap.reset()
	e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	last := e.cap.msgs[len(e.cap.msgs)-1]
	if last.Name != notify.EventFinished || len(last.ExtraUsers) != 1 || last.ExtraUsers[0] != "dba" || !strings.Contains(last.Content, "有新的DDL语句执行完成") {
		t.Errorf("DDL 通知: %+v", last)
	}
	// DML 不触发
	id2 := e.submit(t, dev, "update t set a=1")
	e.approve(t, id2)
	e.cap.reset()
	e.svc.Execute(context.Background(), dev, id2, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	if last := e.cap.msgs[len(e.cap.msgs)-1]; len(last.ExtraUsers) != 0 {
		t.Errorf("DML 不应额外通知: %+v", last)
	}
}

func TestResultSaveFailureStillLeavesExecuting(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	e.approve(t, id)
	e.repo.FailSaveContent = errors.New("disk full")
	e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	if e.status(id) == model.WfExecuting {
		t.Error("结果保存失败时工单不能一直停在执行中")
	}
}

// ---- 定时执行 ----

func TestScheduleAndRunDue(t *testing.T) {
	e := setup(t)
	e.useInception()
	e.inc.checkOut = inceptionRows("update t set a=1")
	id := e.submit(t, dev, "update t set a=1")
	e.approve(t, id)
	e.inc.execOut = []review.Row{{ID: 1, Stage: "EXECUTED", StageStatus: "Execute Successfully", SQL: "update t set a=1", BackupDBName: "None"}}
	at := e.now.Add(time.Hour)
	if _, err := e.svc.Schedule(dev, id, e.now.Add(-time.Minute), ""); code(err) != "bad_request" {
		t.Errorf("过去的时间: %v", err)
	}
	if _, err := e.svc.Schedule(leader, id, at, ""); code(err) != "forbidden" {
		t.Errorf("无执行权限: %v", err)
	}
	e.cap.reset()
	// 用户选择了原生执行（goInception 已配置，但执行人另有选择）
	if _, err := e.svc.Schedule(dev, id, at, sqlflow.ExecNative); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != model.WfTimingTask || e.repo.Schedules[id].Executor != sqlflow.ExecNative || !e.repo.Schedules[id].RunAt.Equal(at) {
		t.Fatalf("定时: %s %+v", e.status(id), e.repo.Schedules[id])
	}
	if e.cap.names()[0] != notify.EventScheduled {
		t.Errorf("通知: %v", e.cap.names())
	}
	if n := e.svc.RunDue(); n != 0 {
		t.Errorf("没到点: %d", n)
	}
	e.svc.SetClock(func() time.Time { return at.Add(time.Second) })
	if n := e.svc.RunDue(); n != 1 {
		t.Fatalf("到点应触发 1 个: %d", n)
	}
	if e.status(id) != model.WfFinish || len(e.eng.ran) != 1 || len(e.inc.calls) != 1 { // inc.calls 只有提交时的 check
		t.Errorf("按选定的原生方式执行: %s ran=%v inc=%v", e.status(id), e.eng.ran, e.inc.calls)
	}
	if _, ok := e.repo.Schedules[id]; ok {
		t.Error("计划应被认领删除")
	}
	var found bool
	for _, l := range e.repo.Logs {
		if l.OperationInfo != "" && strings.HasPrefix(l.OperationInfo, "系统定时执行工单") && l.OperatorDisplay == "系统" {
			found = true
		}
	}
	if !found {
		t.Error("定时执行日志应以系统名义")
	}
	if n := e.svc.RunDue(); n != 0 {
		t.Error("不能重复触发")
	}
}

func TestScheduleCancelledBeforeRun(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	e.approve(t, id)
	at := e.now.Add(time.Hour)
	if _, err := e.svc.Schedule(dev, id, at, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Cancel(dev, id, "取消"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.repo.Schedules[id]; ok || e.status(id) != model.WfAbort {
		t.Errorf("终止应删除计划: %s %v", e.status(id), e.repo.Schedules)
	}
	e.svc.SetClock(func() time.Time { return at.Add(time.Minute) })
	if n := e.svc.RunDue(); n != 0 || len(e.eng.ran) != 0 {
		t.Errorf("已终止的工单不能被执行: %d %v", n, e.eng.ran)
	}
	// 即使计划行残留（并发），begin 也会因状态不对而拒绝执行
	e.repo.Schedules[id] = model.SqlSchedule{WorkflowID: id, RunAt: at}
	e.svc.RunDue()
	if len(e.eng.ran) != 0 {
		t.Errorf("状态不对不应执行: %v", e.eng.ran)
	}
}

func TestScheduleNeedsTableAndWindow(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	e.approve(t, id)
	e.repo.NoSchedTb = true
	if _, err := e.svc.Schedule(dev, id, e.now.Add(time.Hour), ""); code(err) != "schedule_table" {
		t.Errorf("没建表: %v", err)
	}
	e.repo.NoSchedTb = false
	w := e.repo.Workflows[id]
	end := e.now.Add(30 * time.Minute)
	w.RunDateEnd = &end
	e.repo.Workflows[id] = w
	if _, err := e.svc.Schedule(dev, id, e.now.Add(time.Hour), ""); code(err) != "conflict" {
		t.Errorf("定时时间超出窗口: %v", err)
	}
	// 可以改期：重新设置会覆盖原计划
	if _, err := e.svc.Schedule(dev, id, e.now.Add(10*time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Schedule(dev, id, e.now.Add(20*time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if len(e.repo.Schedules) != 1 || !e.repo.Schedules[id].RunAt.Equal(e.now.Add(20*time.Minute)) {
		t.Errorf("改期: %+v", e.repo.Schedules)
	}
	// 立即执行会清掉计划
	if _, err := e.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if len(e.repo.Schedules) != 0 {
		t.Errorf("立即执行应清除定时计划: %+v", e.repo.Schedules)
	}
}

func TestSkipCheckForcesNativeExecution(t *testing.T) {
	e := setup(t)
	e.useInception() // 即使配置了 goInception
	r := req("alter table t add c int")
	r.SkipCheck = true
	res, err := e.svc.Submit(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.inc.calls) != 0 {
		t.Errorf("跳过检查不应调用 goInception: %v", e.inc.calls)
	}
	id := res.WorkflowID
	e.repo.Perms[100] = nil
	if _, err := e.svc.Pass(root, id, "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Pass(root, id, "ok"); err != nil {
		t.Fatal(err)
	}
	opts, def := e.svc.ExecutorOptions(&model.SqlWorkflow{IsManual: 1})
	if len(opts) != 1 || def != sqlflow.ExecNative {
		t.Errorf("跳过检查的工单只能原生执行: %v %q", opts, def)
	}
	if _, err := e.svc.Execute(context.Background(), root, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto, Executor: sqlflow.ExecGoInception}); code(err) != "bad_request" {
		t.Errorf("不能选 goInception: %v", err)
	}
	if _, err := e.svc.Execute(context.Background(), root, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != model.WfFinish || len(e.eng.ran) != 1 || len(e.inc.calls) != 0 {
		t.Errorf("原生执行: %s %v %v", e.status(id), e.eng.ran, e.inc.calls)
	}
}

// S1-A1：提交时跳过语法检查，提交审计要写明；普通提交不带这句；普通用户仍然不能跳过（提交被拒绝、不产生工单）。
func TestSkipCheckSubmitAuditText(t *testing.T) {
	e := setup(t)
	normal := e.submit(t, dev, "update t set a=1")
	last := func() string {
		for i := len(e.repo.LifecycleAudits) - 1; i >= 0; i-- {
			if a := e.repo.LifecycleAudits[i]; a.Action == "SQL上线-提交" {
				return a.ExtraInfo
			}
		}
		return ""
	}
	if x := last(); !strings.Contains(x, fmt.Sprintf("工单id：%d", normal)) || strings.Contains(x, "跳过语法检查") {
		t.Errorf("普通提交的审计不应有跳过标记: %q", x)
	}
	r := req("alter table t add c int")
	r.SkipCheck = true
	res, err := e.svc.Submit(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	if x := last(); !strings.Contains(x, fmt.Sprintf("工单id：%d", res.WorkflowID)) || !strings.Contains(x, "跳过语法检查") {
		t.Errorf("跳过检查提交的审计应写明: %q", x)
	}
	n := len(e.repo.LifecycleAudits)
	if _, err := e.svc.Submit(context.Background(), dev, r); code(err) != "forbidden" {
		t.Errorf("普通用户不能跳过检查: %v", err)
	}
	for _, a := range e.repo.LifecycleAudits[n:] {
		if a.Action == "SQL上线-提交" {
			t.Errorf("被拒绝的提交不应记成功的提交审计: %+v", a)
		}
	}
}

// ---- 可见性、列表、详情 ----

func TestVisibilityListAndDetail(t *testing.T) {
	e := setup(t)
	id := e.submit(t, dev, "update t set a=1")
	if _, err := e.svc.Get(stranger, id); code(err) != "not_found" {
		t.Errorf("其他人看不到: %v", err)
	}
	if _, err := e.svc.Get(outsider, id); code(err) != "not_found" {
		t.Errorf("其他资源组的审批人看不到: %v", err)
	}
	for _, who := range []audit.Actor{dev, leader, dba, root} {
		if _, err := e.svc.Get(who, id); err != nil {
			t.Errorf("%s 应能看到: %v", who.Username, err)
		}
	}
	list := func(who audit.Actor) int { rows, _, _ := e.svc.List(who, sqlflow.ListQuery{}); return len(rows) }
	if list(dev) != 1 || list(leader) != 1 || list(stranger) != 0 || list(outsider) != 0 || list(root) != 1 {
		t.Errorf("列表可见范围: dev=%d leader=%d stranger=%d outsider=%d root=%d", list(dev), list(leader), list(stranger), list(outsider), list(root))
	}
	rows, total, _ := e.svc.List(dev, sqlflow.ListQuery{Status: model.WfFinish})
	if len(rows) != 0 || total != 0 {
		t.Errorf("状态筛选: %v", rows)
	}
	rows, _, _ = e.svc.List(dev, sqlflow.ListQuery{Search: "orders"})
	if len(rows) != 1 || rows[0].StatusLabel != "等待审批" || rows[0].SyntaxLabel != "DML" {
		t.Errorf("搜索: %+v", rows)
	}

	d, err := e.svc.Get(leader, id)
	if err != nil {
		t.Fatal(err)
	}
	if !d.CanReview || !d.CanCancel || d.CanExecute || d.CanRollback || !d.CanAlterDate || len(d.Nodes) != 2 || !d.Nodes[0].Current || d.Flow != "组长->DBA" {
		t.Errorf("审批人视角: %+v", d)
	}
	d, _ = e.svc.Get(dba, id)
	if d.CanReview || d.CanCancel {
		t.Errorf("DBA 还没轮到: %+v", d)
	}
	d, _ = e.svc.Get(dev, id)
	if d.CanReview || !d.CanCancel {
		t.Errorf("提交人视角: %+v", d)
	}
	e.approve(t, id)
	d, _ = e.svc.Get(dev, id)
	if !d.CanExecute || !d.CanSchedule || d.DefaultExecutor != sqlflow.ExecNative || len(d.ReviewRows) != 1 {
		t.Errorf("通过后: %+v", d)
	}
}

func TestSubmitOptionsAndDatabases(t *testing.T) {
	e := setup(t)
	o, err := e.svc.SubmitOptions(dev)
	if err != nil || len(o.Groups) != 1 || len(o.Instances[1]) != 1 || o.Instances[1][0].Name != "prod-mysql-01" || o.Inception || !o.Native {
		t.Errorf("提交页选项: %+v %v", o, err)
	}
	dbs, err := e.svc.Databases(context.Background(), dev, 1)
	if err != nil || len(dbs) != 2 {
		t.Errorf("库列表: %v %v", dbs, err)
	}
	if _, err := e.svc.Databases(context.Background(), dev, 2); code(err) != "bad_request" {
		t.Errorf("无 can_write 标签: %v", err)
	}
	if _, err := e.svc.Databases(context.Background(), outsider, 1); code(err) != "forbidden" {
		t.Errorf("无提交权限: %v", err)
	}
}

// ---- 在线改表控制 ----

func TestOscControl(t *testing.T) {
	e := setup(t)
	e.useInception()
	rows := inceptionRows("alter table t add c int")
	rows[0].SQLSha1 = "abc123"
	e.inc.checkOut = rows
	id := e.submit(t, dev, "alter table t add c int")
	e.approve(t, id)
	e.inc.oscOut = []map[string]any{{"PERCENT": 37}}
	ctx := context.Background()

	if _, err := e.svc.OscControl(ctx, dev, id, "get", "abc123"); err != nil {
		t.Errorf("提交人查进度: %v", err)
	}
	if _, err := e.svc.OscControl(ctx, stranger, id, "get", "abc123"); code(err) != "not_found" {
		t.Errorf("看不到的工单: %v", err)
	}
	if _, err := e.svc.OscControl(ctx, dev, id, "get", "zzz999"); code(err) != "bad_request" {
		t.Errorf("sha1 不属于这张工单: %v", err)
	}
	if _, err := e.svc.OscControl(ctx, dev, id, "drop", "abc123"); code(err) != "bad_request" {
		t.Errorf("未知命令: %v", err)
	}
	if _, err := e.svc.OscControl(ctx, dev, id, "kill", "abc123"); code(err) != "conflict" {
		t.Errorf("未在执行中不能 kill: %v", err)
	}
	w := e.repo.Workflows[id]
	w.Status = model.WfExecuting
	e.repo.Workflows[id] = w
	if _, err := e.svc.OscControl(ctx, leader, id, "pause", "abc123"); code(err) != "forbidden" {
		t.Errorf("没有执行权限不能暂停: %v", err)
	}
	if out, err := e.svc.OscControl(ctx, dev, id, "pause", "ABC123"); err != nil || len(out) != 1 {
		t.Errorf("暂停: %v %v", out, err)
	}
	if got := e.inc.calls[len(e.inc.calls)-1]; got != "osc:pause:ABC123" {
		t.Errorf("调用: %v", e.inc.calls)
	}
	if d, _ := e.svc.Get(dev, id); !d.CanOsc {
		t.Error("执行中且有权限应显示在线改表控制")
	}
}
