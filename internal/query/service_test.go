// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/btnperm"
	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

type fakeRepo struct {
	logErr   error // 非空时 InsertQueryLog 返回该错误
	insts    map[uint]*model.Instance
	cfg      map[string]string
	logs     []*model.QueryLog
	perms    map[uint]map[string]bool // userID -> codename
	userInst map[uint]map[uint]bool   // userID -> instanceID
	privs    []model.QueryPrivilege
	maskCols []model.DataMaskingColumn
}

func (r *fakeRepo) ActiveMaskingColumns(uint) ([]model.DataMaskingColumn, error) {
	return r.maskCols, nil
}

func newRepo() *fakeRepo {
	return &fakeRepo{
		insts: map[uint]*model.Instance{
			1: {ID: 1, InstanceName: "prod-mysql-01", DBType: "mysql", Type: "master", Tags: []model.InstanceTag{{TagCode: "prod", TagName: "生产", Active: true}}},
		},
		cfg:      map[string]string{},
		perms:    map[uint]map[string]bool{},
		userInst: map[uint]map[uint]bool{},
	}
}

func (r *fakeRepo) HasButton(uid uint, code string) (bool, error) {
	return r.perms[uid][code] || btnperm.IsOpen(code), nil
}
func (r *fakeRepo) UserInstanceIDs(uid uint) (map[uint]bool, error) {
	return r.userInst[uid], nil
}
func (r *fakeRepo) ValidQueryPrivileges(user string, inst uint) ([]model.QueryPrivilege, error) {
	var out []model.QueryPrivilege
	for _, p := range r.privs {
		if p.UserName == user && p.InstanceID == inst {
			out = append(out, p)
		}
	}
	return out, nil
}
func (r *fakeRepo) ListInstances() ([]model.Instance, error) {
	var out []model.Instance
	for _, i := range r.insts {
		out = append(out, *i)
	}
	return out, nil
}
func (r *fakeRepo) GetInstance(id uint) (*model.Instance, error) {
	if i, ok := r.insts[id]; ok {
		return i, nil
	}
	return nil, errors.New("not found")
}
func (r *fakeRepo) EngineConn(i *model.Instance) engine.Conn {
	return engine.Conn{Host: i.Host, Port: i.Port}
}
func (r *fakeRepo) ConfigValue(k string) (string, bool) { v, ok := r.cfg[k]; return v, ok }
func (r *fakeRepo) InsertQueryLog(l *model.QueryLog) error {
	if r.logErr != nil {
		return r.logErr
	}
	l.ID = uint(len(r.logs) + 1)
	r.logs = append(r.logs, l)
	return nil
}

type fakeEngine struct {
	gotSQL string
	gotOpt engine.QueryOptions
	rows   int
	err    error
	sleep  time.Duration
}

func (e *fakeEngine) Name() string                                  { return "mysql" }
func (e *fakeEngine) TestConnection(context.Context) error          { return nil }
func (e *fakeEngine) ServerVersion(context.Context) (string, error) { return "8.0", nil }
func (e *fakeEngine) AllDatabases(context.Context) ([]string, error) {
	return []string{"shop"}, nil
}
func (e *fakeEngine) AllTables(context.Context, string) ([]string, error) {
	return []string{"orders"}, nil
}
func (e *fakeEngine) AllColumns(context.Context, string, string) ([]string, error) {
	return []string{"id"}, nil
}
func (e *fakeEngine) Close() error { return nil }
func (e *fakeEngine) Query(ctx context.Context, sql string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	e.gotSQL, e.gotOpt = sql, opt
	if e.sleep > 0 {
		select {
		case <-time.After(e.sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	rs := &engine.ResultSet{ColumnList: []string{"id"}}
	for i := 0; i < e.rows; i++ {
		rs.Rows = append(rs.Rows, []any{i})
	}
	return rs, nil
}

func newSvc(r *fakeRepo, e *fakeEngine) *Service {
	return NewService(r, func(string, engine.Conn) (engine.Engine, error) { return e, nil }, nil)
}

var admin = Principal{ID: 1, Username: "root", IsSuperuser: true}

func code(err error) string {
	var qe *Error
	if errors.As(err, &qe) {
		return qe.Code
	}
	return "?"
}

func TestCheck(t *testing.T) {
	s := newSvc(newRepo(), &fakeEngine{})
	pol := Policy{DisableStar: true}
	cases := []struct {
		sql, wantCode string
	}{
		{"select id from t", ""},
		{"select count(*) from t", ""},
		{"select * from t", "star_forbidden"},
		{"select t.* from t", "star_forbidden"},
		{"delete from t", "bad_query"},
		{"update t set a=1", "bad_query"},
		{"   ", "bad_query"},
		{"select id from t for update", "bad_query"},
		{"select id from t into outfile '/tmp/x'", "bad_query"},
		{"select * from mysql.user", "bad_query"},
		{"show databases", ""},
		{"explain select * from t", ""}, // explain 不检查通配
	}
	for _, c := range cases {
		_, _, err := s.Check(c.sql, "shop", pol)
		got := ""
		if err != nil {
			got = code(err)
		}
		if got != c.wantCode {
			t.Errorf("Check(%q): code=%q, want %q (err=%v)", c.sql, got, c.wantCode, err)
		}
	}
	// 关闭 disable_star 时放行
	if _, _, err := s.Check("select * from t", "shop", Policy{}); err != nil {
		t.Errorf("未开启 disable_star 时不应拦截: %v", err)
	}
	// 多语句只取第一条并提示
	stmt, notice, err := s.Check("select 1; select 2", "shop", Policy{})
	if err != nil || stmt != "select 1" || !strings.Contains(notice, "2 条") {
		t.Errorf("多语句: stmt=%q notice=%q err=%v", stmt, notice, err)
	}
}

func TestExecuteRewritesLimitAndLogs(t *testing.T) {
	repo, eng := newRepo(), &fakeEngine{rows: 5}
	repo.cfg[settings.KeyQueryAdminRowLimit] = "1000"
	s := newSvc(repo, eng)

	res, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id from orders", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if eng.gotSQL != "select id from orders LIMIT 5" {
		t.Errorf("实际执行的 SQL=%q", eng.gotSQL)
	}
	if res.EffectiveLimit != 5 || !res.Truncated || res.RowCount != 5 {
		t.Errorf("结果元信息异常: %+v", res)
	}
	if len(repo.logs) != 1 || repo.logs[0].Username != "root" || repo.logs[0].EffectRow != 5 || res.QueryLogID != 1 {
		t.Errorf("审计日志异常: %+v", repo.logs)
	}
	// 请求值大于管理员上限时取上限
	_, _ = s.Execute(context.Background(), admin, Request{InstanceID: 1, SQL: "select id from orders limit 99999", Limit: 99999})
	if eng.gotSQL != "select id from orders LIMIT 1000" {
		t.Errorf("上限改写失败: %q", eng.gotSQL)
	}
	// EXPLAIN 不限制行数、不改写
	_, _ = s.Execute(context.Background(), admin, Request{InstanceID: 1, SQL: "explain select id from orders"})
	if eng.gotSQL != "explain select id from orders" || eng.gotOpt.Limit != 0 {
		t.Errorf("explain 不应改写: %q %+v", eng.gotSQL, eng.gotOpt)
	}
}

func TestExecuteErrors(t *testing.T) {
	repo, eng := newRepo(), &fakeEngine{}
	s := newSvc(repo, eng)
	ctx := context.Background()

	if _, err := s.Execute(ctx, Principal{Username: "bob"}, Request{InstanceID: 1, SQL: "select 1"}); code(err) != "no_privilege" {
		t.Errorf("非管理员应被拒绝: %v", err)
	}
	if _, err := s.Execute(ctx, admin, Request{InstanceID: 9, SQL: "select 1"}); code(err) != "not_found" {
		t.Errorf("实例不存在: %v", err)
	}
	if _, err := s.Execute(ctx, admin, Request{InstanceID: 1, SQL: ""}); code(err) != "bad_request" {
		t.Errorf("空 SQL: %v", err)
	}
	eng.err = errors.New("Table 'shop.x' doesn't exist")
	if _, err := s.Execute(ctx, admin, Request{InstanceID: 1, SQL: "select a from x"}); code(err) != "engine_error" {
		t.Errorf("引擎错误: %v", err)
	}
	if len(repo.logs) != 0 {
		t.Error("失败的查询不应写入 dbt_query_log")
	}
}

func TestExecuteTimeout(t *testing.T) {
	repo, eng := newRepo(), &fakeEngine{sleep: 3 * time.Second}
	repo.cfg[settings.KeyQueryMaxExecSeconds] = "1"
	s := newSvc(repo, eng)
	start := time.Now()
	_, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, SQL: "select 1"})
	if code(err) != "timeout" {
		t.Fatalf("应返回 timeout: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("超时没有及时生效")
	}
}

func TestInstancesEnv(t *testing.T) {
	s := newSvc(newRepo(), &fakeEngine{})
	list, _ := s.Instances(admin)
	if len(list) != 1 || list[0].Env != "prod" || list[0].Role != "master" {
		t.Errorf("实例视图: %+v", list)
	}
	if l, _ := s.Instances(Principal{Username: "bob"}); len(l) != 0 {
		t.Error("非管理员不应看到实例")
	}
}

// 回归：审计日志写入失败时不能返回查询结果（真库冒烟测试发现 dbt_query_log 曾因零值时间被拒，
// 而旧代码只记警告、照常返回数据，导致查询没有审计记录）。
func TestAuditFailureBlocksResult(t *testing.T) {
	repo := newRepo()
	repo.logErr = errors.New("Error 1292: Incorrect datetime value")
	res, err := newSvc(repo, &fakeEngine{rows: 3}).Execute(context.Background(), admin, Request{InstanceID: 1, SQL: "select id from t"})
	if res != nil || code(err) != "audit_failed" {
		t.Fatalf("审计失败应拒绝返回结果: res=%v err=%v", res, err)
	}
}

// PolarDB-X 走 MySQL 协议：在线查询可用；数据脱敏不用 goInception 语法树，与 SelectDB / Doris 一样按列名保守匹配。
func TestPolarDBXSupported(t *testing.T) {
	for _, tp := range []string{"polardb-x", "polardbx"} {
		if !SupportsQuery(tp) {
			t.Errorf("%s 应支持在线查询", tp)
		}
		if usesSyntaxTree(tp) {
			t.Errorf("%s 不应使用语法树脱敏（保守匹配）", tp)
		}
	}
	for _, tp := range []string{"mysql", "polardb", "adb"} {
		if !usesSyntaxTree(tp) {
			t.Errorf("%s 应继续使用语法树脱敏", tp)
		}
	}
}
