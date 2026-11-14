// @author: kevinkelly
// @license: Apache Licence
// @file: sql_test.go
// @time: 2026/10/28

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/config"
	"dbtool/internal/crypto"
	"dbtool/internal/engine"
	exportpkg "dbtool/internal/export"
	"dbtool/internal/goinception"
	"dbtool/internal/model"
	"dbtool/internal/privapply/memrepo"
	"dbtool/internal/review"
	"dbtool/internal/server"
	"dbtool/internal/settings"
	"dbtool/internal/sqlflow"
	sqlmem "dbtool/internal/sqlflow/memrepo"
)

// sqlFake 既是引擎（Writer），也是 goInception 与备份库的假实现。
type sqlFake struct {
	engine.Engine
	mu  sync.Mutex
	ran []string
}

func (f *sqlFake) Close() error { return nil }
func (f *sqlFake) AllDatabases(context.Context) ([]string, error) {
	return []string{"shop", "report"}, nil
}
func (f *sqlFake) Query(_ context.Context, sqlText string, _ engine.QueryOptions) (*engine.ResultSet, error) {
	if strings.HasPrefix(sqlText, "SELECT COUNT(*) FROM (") {
		return &engine.ResultSet{ColumnList: []string{"c"}, Rows: [][]any{{int64(2)}}}, nil
	}
	return &engine.ResultSet{ColumnList: []string{"id", "name"}, Rows: [][]any{{int64(1), "张三"}, {int64(2), "李四"}}}, nil
}
func (f *sqlFake) ReadOnly(context.Context) (bool, error) { return false, nil }
func (f *sqlFake) ExecEach(_ context.Context, _ string, stmts []string) ([]engine.ExecStmtResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]engine.ExecStmtResult, len(stmts))
	for i, s := range stmts {
		out[i] = engine.ExecStmtResult{SQL: s, Affected: 2}
		f.ran = append(f.ran, s)
	}
	return out, nil
}

// hostEngine 让某台“主机”上的执行失败，用来测试批量上线的部分失败与重试。
type hostEngine struct {
	*sqlFake
	host     string
	failHost *string
}

func (h *hostEngine) ExecEach(ctx context.Context, db string, stmts []string) ([]engine.ExecStmtResult, error) {
	if h.failHost != nil && *h.failHost != "" && *h.failHost == h.host {
		out := make([]engine.ExecStmtResult, len(stmts))
		for i, st := range stmts {
			out[i] = engine.ExecStmtResult{SQL: st, Skipped: i > 0}
			if i == 0 {
				out[i].Err = "Invalid object name 'Orders'."
			}
		}
		return out, nil
	}
	return h.sqlFake.ExecEach(ctx, db, stmts)
}

type fakeInc struct{ rows []review.Row }

func (f fakeInc) Check(context.Context, goinception.Target, string, string, goinception.CheckOpts) ([]review.Row, error) {
	return f.rows, nil
}
func (f fakeInc) Execute(context.Context, goinception.Target, string, string, goinception.ExecOpts) ([]review.Row, error) {
	out := make([]review.Row, len(f.rows))
	copy(out, f.rows)
	for i := range out {
		out[i].Stage, out[i].StageStatus, out[i].BackupDBName, out[i].Sequence = "EXECUTED", "Execute Successfully", "bk_db", "'1_1_1'"
	}
	return out, nil
}
func (fakeInc) Osc(context.Context, string, string) ([]map[string]any, error) { return nil, nil }

type fakeBk struct{}

func (fakeBk) Rollbacks(context.Context, []review.Row) ([]goinception.Rollback, error) {
	return []goinception.Rollback{{SQL: "update t set a=1", Rollback: "UPDATE t SET a=0 WHERE id=1;"}}, nil
}

type sqlEnv struct {
	*applyEnv
	svc      *sqlflow.Service
	mem      *sqlmem.Repo
	fake     *sqlFake
	failHost *string
	xstore   interface{ Delete(name string) error } // 导出文件存储（测试里删文件模拟被清理）
}

func newSQLEnv(t *testing.T) *sqlEnv { return newSQLEnvWith(t, nil) }

// newSQLEnvWith 同 newSQLEnv，但允许调整 server.Options（如注入按钮权限检查器）。
func newSQLEnvWith(t *testing.T, tune func(*server.Options)) *sqlEnv {
	t.Helper()
	hash, err := crypto.MakeDjangoPassword("pw-123", "salty", 1000)
	if err != nil {
		t.Fatal(err)
	}
	m := memrepo.New()
	cw := []model.InstanceTag{{TagCode: "can_write", Active: true}, {TagCode: "can_read", Active: true}}
	m.Instances = []model.Instance{{ID: 1, InstanceName: "prod-mysql-01", DBType: "mysql", Tags: cw},
		{ID: 11, InstanceName: "ms-11", DBType: "mssql", Host: "ms11", Tags: cw}, {ID: 12, InstanceName: "ms-12", DBType: "mssql", Host: "ms12", Tags: cw}}
	m.RGs = []model.ResourceGroup{{GroupID: 1, GroupName: "研发组"}}
	m.InstRGs = map[uint][]uint{1: {1}, 11: {1}, 12: {1}}
	m.UserRGs = map[uint][]uint{1: {1}, 2: {1}, 3: {1}, 4: {1}, 5: {1}}
	m.Groups = map[uint]*model.AuthGroup{10: {ID: 10, Name: "组长"}}
	m.Members = map[uint]map[uint]bool{2: {10: true}}
	m.Users = map[uint]model.User{1: {ID: 1, Username: "dev"}, 2: {ID: 2, Username: "leader"}, 3: {ID: 3, Username: "other"}, 4: {ID: 4, Username: "viewer"}, 5: {ID: 5, Username: "dbauser"}}
	m.Perms = map[uint]map[string]bool{
		1: grants(grantViewOwn, grantSubmit, grantExecute, []string{"query.view", "sqlworkflow.submit_batch", "export.submit", "export.submit_batch", "export.download"}),
		2: grants(grantViewOwn, grantReview, []string{"query.view"}),
		3: grants([]string{"query.view"}),
		4: grants(grantViewOwn, []string{"query.view"}),
		5: grants(grantViewOwn, grantReview, grantExecute, grantExecGroup), // DBA：资源组内可执行、可取消执行
	}
	m.Settings[[2]int{1, 2}] = &model.WorkflowAuditSetting{GroupID: 1, WorkflowType: 2, AuditAuthGroups: "10"}
	// 数据导出要有库表查询权限：dev 在实例 1 上有 shop.orders 的表权限，在 SQL Server 实例 11 / 12 上有 shop 库的库权限（SQL 上线不看这个）
	m.Privs = append(m.Privs,
		&model.QueryPrivilege{PrivilegeID: 1, UserName: "dev", InstanceID: 1, DBName: "shop", TbName: "orders", PrivType: 2, LimitNum: 1000, ValidDate: time.Now().AddDate(1, 0, 0)},
		&model.QueryPrivilege{PrivilegeID: 2, UserName: "dev", InstanceID: 11, DBName: "shop", PrivType: 1, LimitNum: 1000, ValidDate: time.Now().AddDate(1, 0, 0)},
		&model.QueryPrivilege{PrivilegeID: 3, UserName: "dev", InstanceID: 12, DBName: "shop", PrivType: 1, LimitNum: 1000, ValidDate: time.Now().AddDate(1, 0, 0)})
	repo := &appRepo{Repo: m, users: map[string]*model.User{
		"dev":     {ID: 1, Username: "dev", Display: "开发者", Password: hash, IsActive: true},
		"leader":  {ID: 2, Username: "leader", Display: "组长", Password: hash, IsActive: true},
		"other":   {ID: 3, Username: "other", Display: "路人", Password: hash, IsActive: true},
		"viewer":  {ID: 4, Username: "viewer", Display: "观察者", Password: hash, IsActive: true},
		"dbauser": {ID: 5, Username: "dbauser", Display: "DBA 用户", Password: hash, IsActive: true},
	}}
	mem := sqlmem.New(m)
	fake := &sqlFake{}
	failHost := new(string)
	store, err := exportpkg.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := sqlflow.NewService(mem, sqlflow.Options{ExportStorage: store, Open: func(_ string, c engine.Conn) (engine.Engine, error) {
		return &hostEngine{sqlFake: fake, host: c.Host, failHost: failHost}, nil
	}})
	svc.SetAsync(func(f func()) { f() })
	svc.SetSpawn(func(f func()) { f() })
	svc.BaseURL = "http://dba.example.com"
	cfg := &config.Config{SecretKey: "unit-test-secret-key-unit-test-secret-key", PublicURL: "http://dba.example.com"}
	opt := server.Options{Opener: func(string, engine.Conn) (engine.Engine, error) { return nil, nil }, SQLFlow: svc}
	if tune != nil {
		tune(&opt)
	}
	r, err := server.New(cfg, repo, opt)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	return &sqlEnv{applyEnv: &applyEnv{h: r, repo: repo}, svc: svc, mem: mem, fake: fake, failHost: failHost, xstore: store}
}

func dataOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out struct{ Data map[string]any }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %d %s", w.Code, w.Body.String())
	}
	return out.Data
}

func (s *session) get(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(s.cookie)
	w := httptest.NewRecorder()
	s.e.h.ServeHTTP(w, req)
	return w
}

func submitBody(sqlText string) map[string]any {
	return map[string]any{"name": "给 orders 加索引", "instance_id": 1, "db_name": "shop", "group_name": "研发组", "sql": sqlText}
}

func TestSQLWorkflowHTTPFlow(t *testing.T) {
	e := newSQLEnv(t)
	dev, leader, other, viewer := e.login(t, "dev"), e.login(t, "leader"), e.login(t, "other"), e.login(t, "viewer")

	// 未登录：页面跳转登录，API 401
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/sqlworkflow", nil))
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("未登录页面: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/sqlworkflow", nil))
	if rec.Code != 401 {
		t.Errorf("未登录 API: %d", rec.Code)
	}

	// 导航与列表页
	if w := dev.get("/sqlworkflow"); w.Code != 200 || !strings.Contains(w.Body.String(), `href="/sqlworkflow/new"`) || !strings.Contains(w.Body.String(), ">变更工单</a>") {
		t.Errorf("列表页: %d", w.Code)
	}
	if w := other.get("/sqlworkflow"); w.Code != 403 {
		t.Errorf("没有 menu_sqlworkflow: %d", w.Code)
	}
	if w := viewer.get("/sqlworkflow/new"); w.Code != 403 {
		t.Errorf("没有 sql_submit 打开提交页: %d", w.Code)
	}
	if w := dev.get("/sqlworkflow/new"); w.Code != 200 || !strings.Contains(w.Body.String(), "wf-editor") {
		t.Errorf("提交页: %d", w.Code)
	}

	// 选项、库、检查
	w := dev.do("GET", "/api/v1/sqlworkflow/options", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "prod-mysql-01") || !strings.Contains(w.Body.String(), `"inception":false`) {
		t.Errorf("选项: %d %s", w.Code, w.Body.String())
	}
	if w := dev.do("GET", "/api/v1/sqlworkflow/instances/1/databases", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "report") {
		t.Errorf("库: %d %s", w.Code, w.Body.String())
	}
	w = dev.do("POST", "/api/v1/sqlworkflow/check", map[string]any{"instance_id": 1, "db_name": "shop", "sql": "update orders set a=1 where id=1;"})
	if d := dataOf(t, w); w.Code != 200 || d["source"] != "native" || d["syntax_label"] != "DML" || d["notice"] == "" {
		t.Errorf("检查: %d %s", w.Code, w.Body.String())
	}
	// 缺 CSRF 头
	req := httptest.NewRequest("POST", "/api/v1/sqlworkflow", strings.NewReader(`{}`))
	req.AddCookie(dev.cookie)
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("缺少 CSRF 应 403: %d", rec.Code)
	}
	// 校验错误
	if w := dev.do("POST", "/api/v1/sqlworkflow", map[string]any{"name": "", "instance_id": 1, "db_name": "shop", "group_name": "研发组", "sql": "x"}); w.Code != 400 {
		t.Errorf("名称为空: %d %s", w.Code, w.Body.String())
	}
	if w := other.do("POST", "/api/v1/sqlworkflow", submitBody("update t set a=1")); w.Code != 403 {
		t.Errorf("无提交权限: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow", map[string]any{"name": "x", "instance_id": 1, "db_name": "shop", "group_name": "研发组", "sql": "update t set a=1", "run_date_start": "不是时间"}); w.Code != 400 {
		t.Errorf("时间格式: %d", w.Code)
	}

	// 提交：SQL 里带 HTML，页面必须转义
	xss := "update orders set note='<script>alert(1)</script>' where id=1"
	w = dev.do("POST", "/api/v1/sqlworkflow", submitBody(xss))
	d := dataOf(t, w)
	if w.Code != 200 || d["status"] != model.WfManReviewing {
		t.Fatalf("提交: %d %s", w.Code, w.Body.String())
	}
	id := itoa(d["workflow_id"].(float64))
	page := dev.get("/sqlworkflow/" + id)
	if page.Code != 200 || strings.Contains(page.Body.String(), "<script>alert(1)") || !strings.Contains(page.Body.String(), "&lt;script&gt;alert(1)") {
		t.Errorf("详情页必须转义 SQL: %d", page.Code)
	}
	if !strings.Contains(page.Body.String(), "等待审批") || strings.Contains(page.Body.String(), "act-pass") {
		t.Error("提交人看不到审批按钮")
	}

	// 可见性：路人看不到（404），列表里没有
	if w := other.do("GET", "/api/v1/sqlworkflow/"+id, nil); w.Code != 404 {
		t.Errorf("路人看详情: %d", w.Code)
	}
	if w := other.get("/sqlworkflow/" + id); w.Code != 404 {
		t.Errorf("路人看详情页: %d", w.Code)
	}
	if w := viewer.do("GET", "/api/v1/sqlworkflow", nil); w.Code != 200 || strings.Contains(w.Body.String(), "orders") {
		t.Errorf("没有审批权限的人只能看自己的: %s", w.Body.String())
	}
	if w := leader.do("GET", "/api/v1/sqlworkflow", nil); !strings.Contains(w.Body.String(), "给 orders 加索引") {
		t.Errorf("组长能看到组内工单: %s", w.Body.String())
	}

	// 审批
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "x"}); w.Code != 403 {
		t.Errorf("提交人没有审批权限: %d", w.Code)
	}
	if lp := leader.get("/sqlworkflow/" + id); !strings.Contains(lp.Body.String(), "act-pass") || !strings.Contains(lp.Body.String(), "act-rundate") {
		t.Error("审批人应看到审批按钮")
	}
	w = leader.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "同意"})
	if d := dataOf(t, w); w.Code != 200 || d["status"] != model.WfReviewPass {
		t.Fatalf("通过: %d %s", w.Code, w.Body.String())
	}
	// 执行：先试错的，再正式执行
	if w := leader.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto"}); w.Code != 403 {
		t.Errorf("没有执行权限: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto", "executor": "goinception"}); w.Code != 400 {
		t.Errorf("没配置 goInception 不能选它: %d %s", w.Code, w.Body.String())
	}
	if rp := dev.get("/sqlworkflow/" + id); !strings.Contains(rp.Body.String(), "act-execute") || !strings.Contains(rp.Body.String(), "默认推荐") {
		t.Error("执行区应显示执行方式及推荐默认值")
	}
	w = dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto"})
	if d := dataOf(t, w); w.Code != 200 || d["status"] != model.WfQueuing { // 响应时刻是“排队中”，随后后台执行（测试里同步）
		t.Fatalf("执行: %d %s", w.Code, w.Body.String())
	}
	if len(e.fake.ran) != 1 || !strings.Contains(e.fake.ran[0], "<script>") {
		t.Errorf("实际执行: %v", e.fake.ran)
	}
	done := dev.do("GET", "/api/v1/sqlworkflow/"+id, nil)
	dd := dataOf(t, done)
	if dd["status"] != model.WfFinish || len(dd["exec_rows"].([]any)) != 1 || dd["can_execute"] != false {
		t.Errorf("执行后的详情: %s", done.Body.String())
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto"}); w.Code != 409 {
		t.Errorf("重复执行: %d", w.Code)
	}
	// 没开备份：回滚接口 409
	if w := dev.do("GET", "/api/v1/sqlworkflow/"+id+"/rollback", nil); w.Code != 409 {
		t.Errorf("没备份: %d", w.Code)
	}
}

func TestSQLWorkflowCancelAndSchedule(t *testing.T) {
	e := newSQLEnv(t)
	dev, leader := e.login(t, "dev"), e.login(t, "leader")
	w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update t set a=1"))
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	// 终止原因必填
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/cancel", map[string]any{"remark": " "}); w.Code != 400 {
		t.Errorf("终止原因: %d", w.Code)
	}
	leader.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "ok"})
	// 定时：时间格式与过去的时间
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/schedule", map[string]any{"run_at": ""}); w.Code != 400 {
		t.Errorf("空时间: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/schedule", map[string]any{"run_at": "2000-01-01 10:00"}); w.Code != 400 {
		t.Errorf("过去的时间: %d %s", w.Code, w.Body.String())
	}
	at := "2099-01-01T10:00"
	w = dev.do("POST", "/api/v1/sqlworkflow/"+id+"/schedule", map[string]any{"run_at": at})
	if d := dataOf(t, w); w.Code != 200 || d["status"] != model.WfTimingTask {
		t.Fatalf("定时: %d %s", w.Code, w.Body.String())
	}
	if p := dev.get("/sqlworkflow/" + id); !strings.Contains(p.Body.String(), "2099-01-01 10:00") {
		t.Error("详情页应显示定时执行时间")
	}
	// 提交人撤回
	w = dev.do("POST", "/api/v1/sqlworkflow/"+id+"/cancel", map[string]any{"remark": "不做了"})
	if d := dataOf(t, w); w.Code != 200 || d["status"] != model.WfAbort {
		t.Fatalf("撤回: %d %s", w.Code, w.Body.String())
	}
	if len(e.mem.Schedules) != 0 || len(e.fake.ran) != 0 {
		t.Errorf("撤回应清除定时计划且不执行: %v %v", e.mem.Schedules, e.fake.ran)
	}
}

func TestSQLWorkflowRollbackDownload(t *testing.T) {
	e := newSQLEnv(t)
	e.mem.Cfg[settings.KeyGoInceptionHost] = "127.0.0.1"
	e.mem.Cfg[settings.KeyGoInceptionBackupHost] = "127.0.0.1"
	e.svc.SetInception(func(goinception.Config) sqlflow.Inception {
		return fakeInc{rows: []review.Row{{ID: 1, Stage: "CHECKED", StageStatus: "Audit completed", ErrorMessage: "None", SQL: "update t set a=1", AffectedRows: 1}}}
	}, func(goinception.BackupConfig) sqlflow.BackupReader { return fakeBk{} })
	dev, leader, other := e.login(t, "dev"), e.login(t, "leader"), e.login(t, "other")

	w := dev.do("POST", "/api/v1/sqlworkflow", submitBody("update t set a=1"))
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	leader.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "ok"})
	// 默认推荐 goInception
	if rp := dev.get("/sqlworkflow/" + id); !strings.Contains(rp.Body.String(), `value="goinception" selected`) {
		t.Error("配置了 goInception，默认应选 goInception")
	}
	w = dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto"})
	if w.Code != 200 || len(e.fake.ran) != 0 {
		t.Fatalf("goInception 执行: %d %s ran=%v", w.Code, w.Body.String(), e.fake.ran)
	}
	if rp := dev.get("/sqlworkflow/" + id); !strings.Contains(rp.Body.String(), "act-rollback") || !strings.Contains(rp.Body.String(), "/rollback/download") {
		t.Error("已结束且备份的工单应显示回滚入口")
	}
	w = dev.get("/api/v1/sqlworkflow/" + id + "/rollback/download")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") ||
		!strings.Contains(w.Header().Get("Content-Disposition"), `attachment; filename="rollback-`) || !strings.Contains(w.Body.String(), "UPDATE t SET a=0") {
		t.Errorf("下载: %d %v\n%s", w.Code, w.Header(), w.Body.String())
	}
	if w := other.get("/api/v1/sqlworkflow/" + id + "/rollback/download"); w.Code != 404 {
		t.Errorf("路人下载: %d", w.Code)
	}
}

func TestSQLBatchHTTPFlow(t *testing.T) {
	e := newSQLEnv(t)
	dev, leader, other := e.login(t, "dev"), e.login(t, "leader"), e.login(t, "other")
	sqlText := "update dbo.Orders set status = 1 where id = 5"

	// 选项：有批量权限、允许 mssql；没有权限的人看不到
	d := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/options", nil))
	b := d["batch"].(map[string]any)
	if b["allowed"] != true || b["types"].([]any)[0] != "mssql" || b["max"].(float64) != 100 {
		t.Errorf("批量选项: %v", b)
	}
	if lo := dataOf(t, leader.do("GET", "/api/v1/sqlworkflow/options", nil)); lo["batch"].(map[string]any)["allowed"] != false {
		t.Error("没有 sqlbatch_submit 不应开放批量")
	}
	// 批量检查：规则 + 实例预检
	w := dev.do("POST", "/api/v1/sqlworkflow/check", map[string]any{"instance_ids": []int{11, 12}, "db_name": "shop", "sql": sqlText})
	cd := dataOf(t, w)
	if w.Code != 200 || cd["source"] != "mssql" || len(cd["instances"].([]any)) != 2 || cd["error_count"].(float64) != 0 {
		t.Fatalf("批量检查: %d %s", w.Code, w.Body.String())
	}
	// 规则：缺 WHERE 的 UPDATE 在检查结果里是错误
	w = dev.do("POST", "/api/v1/sqlworkflow/check", map[string]any{"instance_ids": []int{11}, "db_name": "shop", "sql": "update dbo.Orders set status = 1"})
	if cd = dataOf(t, w); cd["error_count"].(float64) != 1 || !strings.Contains(w.Body.String(), "statement.where.require.update-delete") {
		t.Errorf("规则检查: %s", w.Body.String())
	}
	// 权限与类型校验
	if w := leader.do("POST", "/api/v1/sqlworkflow", map[string]any{"name": "x", "instance_ids": []int{11}, "db_name": "shop", "group_name": "研发组", "sql": sqlText}); w.Code != 403 {
		t.Errorf("没有批量权限: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow", map[string]any{"name": "x", "instance_ids": []int{11, 1}, "db_name": "shop", "group_name": "研发组", "sql": sqlText}); w.Code != 400 {
		t.Errorf("批量混入 MySQL 实例: %d %s", w.Code, w.Body.String())
	}

	// 提交 → 审批 → 执行（其中一台失败）
	w = dev.do("POST", "/api/v1/sqlworkflow", map[string]any{"name": "批量改单", "instance_ids": []int{11, 12}, "db_name": "shop", "group_name": "研发组", "sql": sqlText})
	id := itoa(dataOf(t, w)["workflow_id"].(float64))
	if w.Code != 200 {
		t.Fatalf("提交: %d %s", w.Code, w.Body.String())
	}
	leader.do("POST", "/api/v1/sqlworkflow/"+id+"/pass", map[string]any{"remark": "ok"})
	*e.failHost = "ms12"
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/execute", map[string]any{"mode": "auto"}); w.Code != 200 {
		t.Fatalf("执行: %d %s", w.Code, w.Body.String())
	}
	dd := dataOf(t, dev.do("GET", "/api/v1/sqlworkflow/"+id, nil))
	if dd["status"] != model.WfException || dd["is_batch"] != true || dd["can_batch_op"] != true || len(dd["batch_instances"].([]any)) != 2 {
		t.Fatalf("部分失败的详情: %v", dd)
	}
	page := dev.get("/sqlworkflow/" + id)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "data-batch-op=\"retry\"") || !strings.Contains(page.Body.String(), "ms-11") || !strings.Contains(page.Body.String(), "批量执行汇总") {
		t.Errorf("详情页应显示各实例状态与重试/关闭按钮")
	}
	if n := strings.Count(page.Body.String(), "data-batch-version"); n != 2 || !strings.Contains(page.Body.String(), "<th>数据库版本</th>") {
		t.Errorf("批量实例表应有“数据库版本”列（每个实例一格，由页面异步填充）: %d", n)
	}
	// 列表：批量标记与筛选
	// 列表里“工单类型”单独一列：批量工单显示“批量工单”和实例个数；单实例显示 DML / DDL
	if lw := dev.get("/sqlworkflow?batch=1"); !strings.Contains(lw.Body.String(), "批量工单") || !strings.Contains(lw.Body.String(), "2 个实例") || !strings.Contains(lw.Body.String(), ">工单类型<") {
		t.Error("列表应显示“工单类型”列，批量工单标为“批量工单”并写实例个数")
	}
	if lw := dev.do("GET", "/api/v1/sqlworkflow?batch=0", nil); strings.Contains(lw.Body.String(), "批量改单") {
		t.Error("batch=0 只看单实例")
	}
	// 重试：权限、仍失败、故障排除后成功
	if w := leader.do("POST", "/api/v1/sqlworkflow/"+id+"/batch/12/retry", nil); w.Code != 403 {
		t.Errorf("没有执行权限: %d", w.Code)
	}
	e.mem.Perms[3]["sqlworkflow.batch_op"] = true // 有“批量实例 重试 / 关闭”按钮，但看不到这张工单
	if w := other.do("POST", "/api/v1/sqlworkflow/"+id+"/batch/12/retry", nil); w.Code != 404 {
		t.Errorf("看不到的工单: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/batch/12/retry", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "重试失败") {
		t.Errorf("仍失败的重试: %d %s", w.Code, w.Body.String())
	}
	*e.failHost = ""
	w = dev.do("POST", "/api/v1/sqlworkflow/"+id+"/batch/12/retry", nil)
	if rd := dataOf(t, w); w.Code != 200 || rd["status"] != model.WfFinish {
		t.Errorf("重试成功后工单正常结束: %d %s", w.Code, w.Body.String())
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/batch/12/retry", nil); w.Code != 400 {
		t.Errorf("成功的实例无需重试: %d", w.Code)
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/batch/12/close", nil); w.Code != 200 {
		t.Errorf("关闭实例: %d %s", w.Code, w.Body.String())
	}
	if w := dev.do("POST", "/api/v1/sqlworkflow/"+id+"/batch/12/close", nil); w.Code != 400 {
		t.Errorf("重复关闭: %d", w.Code)
	}
}
