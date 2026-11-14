// @author: kevinkelly
// @license: Apache Licence
// @file: rules_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// scriptEngine 按语句前缀返回预设结果集，并记录收到的全部语句。
type scriptEngine struct {
	fakeEngine
	script map[string]*engine.ResultSet
	got    []string
	err    error
}

func (e *scriptEngine) Query(ctx context.Context, sql string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	e.got = append(e.got, sql)
	e.gotSQL, e.gotOpt = sql, opt
	if e.err != nil {
		return nil, e.err
	}
	for prefix, rs := range e.script {
		if strings.HasPrefix(sql, prefix) {
			return rs, nil
		}
	}
	return e.fakeEngine.Query(ctx, sql, opt)
}

func svcWith(r Repo, e engine.Engine) *Service {
	return NewService(r, func(string, engine.Conn) (engine.Engine, error) { return e, nil }, nil)
}

func TestCheckWithDescExplainMySQLSchema(t *testing.T) {
	s := newSvc(newRepo(), &fakeEngine{})
	pol := Policy{DisableStar: true}
	cases := []struct{ sql, db, want string }{
		{"with a as (select id from t) select id from a", "shop", ""},
		{"WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<5) SELECT x FROM n", "shop", ""},
		{"with a as (select id from t) delete from t where id in (select id from a)", "shop", "bad_query"},
		{"with a as (select id from t) update t set c=1", "shop", "bad_query"},
		{"with a as (select id from t) insert into t2 select id from a", "shop", "bad_query"},
		{"with a as (select * from t) select id from a", "shop", "star_forbidden"},
		{"with a as (select id from t) select id from a for update", "shop", "bad_query"},
		{"desc orders", "shop", ""},
		{"describe shop.orders", "shop", ""},
		{"desc select id from t", "shop", ""},
		{"explain analyze select id from t", "shop", "bad_query"},
		{"desc analyze select id from t", "shop", "bad_query"},
		{"explain format=tree analyze select id from t", "shop", "bad_query"},
		{"explain select id from t", "shop", ""},
		{"select id from mysql.user", "shop", "bad_query"},
		{"select id from mysql.db", "shop", "bad_query"},
		{"show tables from mysql", "shop", "bad_query"},
		{"desc mysql.user", "shop", "bad_query"},
		{"select id from user", "mysql", "bad_query"},
		{"select id from information_schema.tables", "shop", ""},
		{"select 1", "mysql", ""},
	}
	for _, c := range cases {
		_, _, err := s.Check(c.sql, c.db, pol)
		got := ""
		if err != nil {
			got = code(err)
		}
		if got != c.want {
			t.Errorf("Check(%q,%q): code=%q, want %q (err=%v)", c.sql, c.db, got, c.want, err)
		}
	}
}

func TestExecuteWithAndDesc(t *testing.T) {
	repo, eng := newRepo(), &fakeEngine{rows: 2}
	repo.cfg[settings.KeyQueryAdminRowLimit] = "1000"
	s := svcWith(repo, eng)
	res, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "with a as (select id from t) select id from a", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if eng.gotSQL != "with a as (select id from t) select id from a LIMIT 50" || res.Kind != "select" {
		t.Errorf("WITH 应改写 LIMIT: %q kind=%q", eng.gotSQL, res.Kind)
	}
	res, err = s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "desc orders"})
	if err != nil || eng.gotSQL != "desc orders" || res.Kind != "show" {
		t.Errorf("DESC 表: err=%v sql=%q kind=%q", err, eng.gotSQL, res.Kind)
	}
	res, err = s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "describe select id from t"})
	if err != nil || res.Kind != "explain" || eng.gotOpt.Limit != 0 {
		t.Errorf("DESC SELECT 等价 EXPLAIN: err=%v kind=%q limit=%d", err, res.Kind, eng.gotOpt.Limit)
	}
}

func TestNonAdminDescAndWith(t *testing.T) {
	r := devRepo()
	// 普通用户：DESC 表属于结构查询，不需要表权限
	if _, res, err := run(t, r, dev, "shop", "desc users"); err != nil || res.LimitSource != "结构查询" {
		t.Errorf("普通用户 DESC: %v %+v", err, res)
	}
	// WITH 里引用的真实表仍要校验权限
	if _, _, err := run(t, r, dev, "shop", "with a as (select id from users) select id from a"); code(err) != "no_privilege" {
		t.Errorf("WITH 引用无权限表应拒绝: %v", err)
	}
}

func TestExecuteCanceled(t *testing.T) {
	repo := newRepo()
	eng := &fakeEngine{sleep: 3 * time.Second}
	s := svcWith(repo, eng)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := s.Execute(ctx, admin, Request{InstanceID: 1, SQL: "select 1"})
	if code(err) != "canceled" {
		t.Fatalf("应返回 canceled: %v", err)
	}
}

// ---- 未成功记录 ----

type evRepo struct {
	*histRepo
	ready  bool
	events []*QueryEvent
}

func (r *evRepo) EventsReady() bool { return r.ready }
func (r *evRepo) InsertQueryEvent(e *QueryEvent) error {
	e.ID = uint(len(r.events) + 1)
	r.events = append(r.events, e)
	return nil
}
func (r *evRepo) ListHistory(f QueryLogFilter, status string) ([]HistoryRow, int64, error) {
	var out []HistoryRow
	if status == "" || status == StatusOK {
		rows, _, _ := r.histRepo.ListQueryLogs(QueryLogFilter{Username: f.Username, Limit: 100})
		for _, l := range rows {
			out = append(out, HistoryRow{ID: l.ID, Time: l.CreateTime, Username: l.Username, Instance: l.InstanceName, SQL: l.SQLLog, Status: StatusOK})
		}
	}
	if status != StatusOK {
		for _, e := range r.events {
			if (f.Username == "" || e.UserName == f.Username) && (status == "" || status == e.Status) {
				out = append(out, HistoryRow{ID: e.ID, Time: e.CreatedAt, Username: e.UserName, Instance: e.InstanceName, SQL: e.SQL, Status: e.Status, Code: e.Code, Reason: e.Reason})
			}
		}
	}
	return out, int64(len(out)), nil
}

func TestFailuresAreRecorded(t *testing.T) {
	repo := &evRepo{histRepo: &histRepo{fakeRepo: devRepo()}, ready: true}
	repo.cfg[settings.KeyQueryForbidSelectStar] = "true"
	eng := &fakeEngine{rows: 1}
	s := svcWith(repo, eng)
	ctx := context.Background()

	_, _ = s.Execute(ctx, admin, Request{InstanceID: 1, DB: "shop", SQL: "update t set a=1"})
	_, _ = s.Execute(ctx, dev, Request{InstanceID: 1, DB: "shop", SQL: "select id from users"})
	_, _ = s.Execute(ctx, admin, Request{InstanceID: 1, DB: "shop", SQL: "select * from t"})
	eng.err = errors.New("Table 'shop.x' doesn't exist")
	_, _ = s.Execute(ctx, admin, Request{InstanceID: 1, DB: "shop", SQL: "select a from x"})
	// 不需要记录的：请求不完整、实例不存在
	_, _ = s.Execute(ctx, admin, Request{InstanceID: 1, SQL: ""})
	_, _ = s.Execute(ctx, admin, Request{InstanceID: 9, SQL: "select 1"})

	var got []string
	for _, e := range repo.events {
		got = append(got, e.Status+":"+e.Code)
	}
	want := []string{"blocked:bad_query", "blocked:no_privilege", "blocked:star_forbidden", "failed:engine_error"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("记录=%v, want %v", got, want)
	}
	if e := repo.events[1]; e.UserName != "dev" || e.InstanceID != 1 || e.InstanceName != "prod-mysql-01" || e.DB != "shop" || !strings.Contains(e.Reason, "shop.users") {
		t.Errorf("拦截记录内容: %+v", e)
	}
	if len(repo.logs) != 0 {
		t.Error("失败 / 被拦截的查询不应写 dbt_query_log")
	}

	// 超时与取消
	eng2 := &fakeEngine{sleep: 3 * time.Second}
	repo.cfg[settings.KeyQueryMaxExecSeconds] = "1"
	s2 := svcWith(repo, eng2)
	_, _ = s2.Execute(ctx, admin, Request{InstanceID: 1, SQL: "select 1"})
	cctx, cancel := context.WithCancel(ctx)
	repo.cfg[settings.KeyQueryMaxExecSeconds] = "60"
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, _ = s2.Execute(cctx, admin, Request{InstanceID: 1, SQL: "select 1"})
	n := len(repo.events)
	if repo.events[n-2].Status != StatusTimeout || repo.events[n-1].Status != StatusCanceled {
		t.Errorf("超时 / 取消记录: %+v %+v", repo.events[n-2], repo.events[n-1])
	}
}

// 通过静态检查却被数据库只读会话拒绝的语句：记为“被拦截”，而不是普通的引擎失败；返回给用户的是友好说明，不带驱动的原始报错。
func TestReadOnlyViolationIsBlocked(t *testing.T) {
	repo := &evRepo{histRepo: &histRepo{fakeRepo: newRepo()}, ready: true}
	eng := &fakeEngine{err: fmt.Errorf("%w: Error 1792 (25006): Cannot execute statement in a READ ONLY transaction.", engine.ErrReadOnlyViolation)}
	s := svcWith(repo, eng)

	_, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select sneaky_write(1)"})
	var qe *Error
	if !errors.As(err, &qe) || qe.Code != "readonly_violation" || qe.HTTP != 403 {
		t.Fatalf("应返回 403 readonly_violation: %v", err)
	}
	if strings.Contains(qe.Message, "1792") || !strings.Contains(qe.Message, "SQL 工单") {
		t.Errorf("提示应是面向用户的说明: %q", qe.Message)
	}
	if len(repo.events) != 1 || repo.events[0].Status != StatusBlocked || repo.events[0].Code != "readonly_violation" {
		t.Fatalf("应记为 blocked:readonly_violation: %+v", repo.events)
	}
	if len(repo.logs) != 0 {
		t.Error("被拦截的查询不应写 dbt_query_log")
	}
}

func TestFailureRecordTruncatesAndDegrades(t *testing.T) {
	repo := &evRepo{histRepo: &histRepo{fakeRepo: newRepo()}, ready: false}
	s := svcWith(repo, &fakeEngine{})
	_, _ = s.Execute(context.Background(), admin, Request{InstanceID: 1, SQL: "delete from t"})
	if len(repo.events) != 0 {
		t.Error("表不存在（EventsReady=false）时不应写记录")
	}
	repo.ready = true
	long := "delete from t where x = '" + strings.Repeat("长", 3000) + "'"
	_, _ = s.Execute(context.Background(), admin, Request{InstanceID: 1, SQL: long})
	if len(repo.events) != 1 || len([]rune(repo.events[0].SQL)) != MaxEventSQL {
		t.Errorf("sql_text 应截断到 %d 字符: %d", MaxEventSQL, len(repo.events))
	}
}

func TestHistoryWithEvents(t *testing.T) {
	repo := &evRepo{histRepo: &histRepo{fakeRepo: newRepo()}, ready: true}
	s := svcWith(repo, &fakeEngine{rows: 1})
	ctx := context.Background()
	_, _ = s.Execute(ctx, admin, Request{InstanceID: 1, SQL: "select 1"})
	_, _ = s.Execute(ctx, admin, Request{InstanceID: 1, SQL: "drop table t"})
	pg, err := s.History(admin, HistoryQuery{})
	if err != nil || !pg.Events || len(pg.Items) != 2 {
		t.Fatalf("历史: %+v %v", pg, err)
	}
	seen := map[string]bool{}
	for _, it := range pg.Items {
		seen[it.Status] = true
		if it.Status == StatusBlocked && it.Reason == "" {
			t.Error("被拦截记录应带原因")
		}
	}
	if !seen[StatusOK] || !seen[StatusBlocked] {
		t.Errorf("状态: %v", seen)
	}
	pg, _ = s.History(admin, HistoryQuery{Status: StatusBlocked})
	if len(pg.Items) != 1 || pg.Items[0].Status != StatusBlocked {
		t.Errorf("状态筛选: %+v", pg.Items)
	}
	if _, err := s.History(admin, HistoryQuery{Status: "bogus"}); code(err) != "bad_request" {
		t.Errorf("非法状态应 400: %v", err)
	}
	// 没有旁路表：只有成功记录，非成功状态为空
	repo.ready = false
	pg, _ = s.History(admin, HistoryQuery{Status: StatusBlocked})
	if len(pg.Items) != 0 || pg.Events {
		t.Errorf("无旁路表: %+v", pg)
	}
}

// ---- SQL Server ----

func mssqlRepo() *fakeRepo {
	r := newRepo()
	r.insts[2] = &model.Instance{ID: 2, InstanceName: "mssql-01", DBType: "mssql", Type: "master"}
	r.perms[7] = map[string]bool{"query.run": true, "query.view": true}
	r.userInst[7] = map[uint]bool{2: true}
	r.privs = []model.QueryPrivilege{{UserName: "dev", InstanceID: 2, PrivType: 1, DBName: "sales", LimitNum: 20, ValidDate: time.Now().AddDate(0, 0, 10)}}
	return r
}

func TestMSSQLQuery(t *testing.T) {
	if !SupportsQuery("mssql") {
		t.Fatal("mssql 应支持在线查询")
	}
	repo := mssqlRepo()
	repo.cfg[settings.KeyQueryForbidSelectStar] = "true"
	eng := &fakeEngine{rows: 3}
	s := svcWith(repo, eng)
	ctx := context.Background()
	exec := func(p Principal, db, sql string) (*Result, error) {
		return s.Execute(ctx, p, Request{InstanceID: 2, DB: db, SQL: sql, Limit: 100})
	}

	res, err := exec(admin, "sales", "select id, name from dbo.orders")
	if err != nil {
		t.Fatal(err)
	}
	if eng.gotSQL != "select TOP 100 id, name from dbo.orders" || eng.gotOpt.Limit != 100 || res.Kind != "select" {
		t.Errorf("TOP 改写: %q %+v", eng.gotSQL, eng.gotOpt)
	}
	// 已有 TOP 超过上限时压到上限
	repo.cfg[settings.KeyQueryAdminRowLimit] = "50"
	if _, err = exec(admin, "sales", "select top 1000 id from dbo.orders"); err != nil || eng.gotSQL != "select top 50 id from dbo.orders" {
		t.Errorf("TOP 压上限: %v %q", err, eng.gotSQL)
	}
	// 写语句、EXEC、SELECT INTO、多余的写语句都拦
	for _, sql := range []string{"update dbo.t set a=1", "exec sp_who", "select id into #x from dbo.t", "select 1; delete from dbo.t",
		"with a as (select 1 as x) delete from dbo.t", "select * from dbo.t", "select id from srv.db.dbo.t"} {
		if _, err := exec(admin, "sales", sql); err == nil {
			// 多语句只取第一条，"select 1; delete" 的第一条是合法的 select 1
			if sql != "select 1; delete from dbo.t" {
				t.Errorf("应拦截: %q", sql)
			}
		}
	}
	// 普通用户：按库授权，行数取授权的 20
	if res, err = exec(dev, "sales", "select id from dbo.orders"); err != nil || res.EffectiveLimit != 20 || eng.gotSQL != "select TOP 20 id from dbo.orders" {
		t.Errorf("库权限: %v %+v %q", err, res, eng.gotSQL)
	}
	if _, err = exec(dev, "sales", "select id from [other].dbo.t"); code(err) != "no_privilege" {
		t.Errorf("三部分名称跨库应校验库权限: %v", err)
	}
	if _, err = exec(dev, "sales", "select a.id from dbo.t a join fin.dbo.settlement b on a.id=b.id"); code(err) != "no_privilege" {
		t.Errorf("JOIN 跨库无权限应拒绝: %v", err)
	}
	if _, err = exec(dev, "other", "select id from dbo.t"); code(err) != "no_privilege" {
		t.Errorf("当前库无权限应拒绝: %v", err)
	}
	if _, err = exec(dev, "sales", "select id from sales.dbo.orders"); err != nil {
		t.Errorf("三部分名称指向有权限的库应放行: %v", err)
	}
}

func TestMSSQLHelpers(t *testing.T) {
	top := []struct {
		in   string
		max  int
		want string
	}{
		{"select id from t", 100, "select TOP 100 id from t"},
		{"SELECT DISTINCT id FROM t", 100, "SELECT DISTINCT TOP 100 id FROM t"},
		{"select top 10 id from t", 100, "select top 10 id from t"},
		{"select top (500) id from t", 100, "select top (100) id from t"},
		{"select top 10 percent id from t", 100, "select top 10 percent id from t"},
		{"select id from t union all select id from u", 100, "select id from t union all select id from u"},
		{"select id from t order by id offset 10 rows fetch next 5 rows only", 100, "select id from t order by id offset 10 rows fetch next 5 rows only"},
		{"with a as (select id from t) select id from a;", 100, "with a as (select id from t) select TOP 100 id from a"},
		{"select id from (select top 5 id from t) x", 100, "select TOP 100 id from (select top 5 id from t) x"},
		{"select id from t", 0, "select id from t"},
	}
	for _, c := range top {
		if got := mssqlRewriteTop(c.in, c.max); got != c.want {
			t.Errorf("mssqlRewriteTop(%q)=%q, want %q", c.in, got, c.want)
		}
	}
	refs, linked := mssqlTables("select a.id from [dbo].[Orders] a join sales.dbo.items i on i.oid = a.id, fin.dbo.p where a.id in (select id from #tmp)", "shop")
	var names []string
	for _, r := range refs {
		names = append(names, r.Schema+"."+r.Name)
	}
	if linked || strings.Join(names, ",") != "shop.Orders,sales.items,fin.p" {
		t.Errorf("mssqlTables: %v linked=%v", names, linked)
	}
	if _, linked = mssqlTables("select 1 from srv.db.dbo.t", "x"); !linked {
		t.Error("四部分名称应识别为链接服务器")
	}
}

// ---- 元数据 / 限制信息 / 估算 / 执行计划提示 ----

func TestDBMetaAndPolicy(t *testing.T) {
	repo := devRepo()
	repo.maskCols = []model.DataMaskingColumn{{InstanceID: 1, TableSchema: "shop", TableName_: "orders", ColumnName: "phone", RuleType: 1, Active: true}}
	eng := &scriptEngine{script: map[string]*engine.ResultSet{
		"SHOW TABLE STATUS FROM `other`": {ColumnList: []string{"Name", "Comment"}, Rows: [][]any{{"a", ""}, {"b", ""}, {"c", ""}}},
		"SHOW TABLE STATUS FROM `shop`": {ColumnList: []string{"Name", "Engine", "Rows", "Comment"}, Rows: [][]any{
			{"orders", "InnoDB", int64(1200), "订单"}, {"users", "InnoDB", "88", ""}, {"v_orders", nil, nil, "VIEW"}}},
		"SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS": {ColumnList: []string{"TABLE_NAME", "COLUMN_NAME"}, Rows: [][]any{{"orders", "phone"}, {"users", "phone"}}},
	}}
	s := svcWith(repo, eng)
	m, err := s.DBMeta(context.Background(), dev, 1, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tables) != 3 || m.Policy.Mode != "priv" || m.Policy.Expire == "" || m.Policy.MaxRows != 3 {
		t.Fatalf("meta: %+v", m)
	}
	byName := map[string]TableMeta{}
	for _, tb := range m.Tables {
		byName[tb.Name] = tb
	}
	if o := byName["orders"]; o.Locked || o.Rows != 1200 || o.Comment != "订单" || len(o.Sensitive) != 1 || o.Sensitive[0] != "phone" {
		t.Errorf("orders: %+v", o)
	}
	if u := byName["users"]; !u.Locked || u.Rows != 88 || len(u.Sensitive) != 0 {
		t.Errorf("users 应上锁且 phone 未配置脱敏: %+v", u)
	}
	if v := byName["v_orders"]; !v.View || v.Rows != -1 || !v.Locked {
		t.Errorf("v_orders: %+v", v)
	}
	if m.LockedTotal != 2 || m.SensitiveTotal != 1 {
		t.Errorf("汇总: locked=%d sensitive=%d", m.LockedTotal, m.SensitiveTotal)
	}
	// 管理员不上锁
	if m, _ = s.DBMeta(context.Background(), admin, 1, "shop"); m.Policy.Mode != "admin" || m.LockedTotal != 0 || m.Policy.MaxRows != 5000 {
		t.Errorf("管理员: %+v", m.Policy)
	}
	// 没有任何权限的库
	if m, _ = s.DBMeta(context.Background(), dev, 1, "other"); m.Policy.Mode != "none" || m.LockedTotal != 3 {
		t.Errorf("无权限库: %+v", m.Policy)
	}
	// mysql 系统库、被实例过滤规则排除的库
	if _, err = s.DBMeta(context.Background(), admin, 1, "mysql"); code(err) != "no_privilege" {
		t.Errorf("mysql 库应拒绝: %v", err)
	}
	repo.insts[1].DeniedDBNameRegex = "^secret"
	if _, err = s.DBMeta(context.Background(), admin, 1, "secret_db"); code(err) != "no_privilege" {
		t.Errorf("被过滤的库应拒绝: %v", err)
	}
}

func TestTableColumnsAndSearch(t *testing.T) {
	repo := devRepo()
	repo.maskCols = []model.DataMaskingColumn{{InstanceID: 1, TableSchema: "*", TableName_: "*", ColumnName: "Phone", Active: true}}
	eng := &scriptEngine{script: map[string]*engine.ResultSet{
		"SHOW FULL COLUMNS FROM `shop`.`orders`": {ColumnList: []string{"Field", "Type", "Collation", "Null", "Key", "Default", "Extra", "Privileges", "Comment"}, Rows: [][]any{
			{"id", "bigint", nil, "NO", "PRI", nil, "", "", "主键"}, {"phone", "varchar(20)", nil, "YES", "", nil, "", "", "手机号"}}},
		"SELECT TABLE_NAME, COLUMN_NAME, COLUMN_COMMENT": {ColumnList: []string{"a", "b", "c"}, Rows: [][]any{{"orders", "phone", "手机号"}}},
	}}
	s := svcWith(repo, eng)
	cols, err := s.TableColumns(context.Background(), dev, 1, "shop", "orders")
	if err != nil || len(cols) != 2 || cols[0].Key != "PRI" || cols[0].Nullable || !cols[1].Nullable || !cols[1].Sensitive || cols[0].Sensitive || cols[1].Comment != "手机号" {
		t.Fatalf("列: %+v %v", cols, err)
	}
	// 注入：库表名里的反引号必须被转义
	_, _ = s.TableColumns(context.Background(), dev, 1, "shop", "a`b")
	if last := eng.got[len(eng.got)-1]; last != "SHOW FULL COLUMNS FROM `shop`.`a``b`" {
		t.Errorf("标识符转义: %q", last)
	}
	hits, err := s.SearchColumns(context.Background(), dev, 1, "shop", "手机' or 1=1 -- %")
	if err != nil || len(hits) != 1 {
		t.Fatalf("搜索: %+v %v", hits, err)
	}
	last := eng.got[len(eng.got)-1]
	if !strings.Contains(last, `LIKE '%手机'' or 1=1 -- \\%%'`) || !strings.Contains(last, "TABLE_SCHEMA = 'shop'") {
		t.Errorf("字面量转义: %q", last)
	}
	if h, _ := s.SearchColumns(context.Background(), dev, 1, "shop", "  "); len(h) != 0 {
		t.Error("空关键字应返回空")
	}
}

func TestEstimateRows(t *testing.T) {
	repo := devRepo()
	eng := &scriptEngine{script: map[string]*engine.ResultSet{
		"EXPLAIN ": {ColumnList: []string{"id", "select_type", "table", "type", "rows", "filtered"}, Rows: [][]any{
			{int64(1), "SIMPLE", "o", "ALL", int64(1000), "50.00"}, {int64(1), "SIMPLE", "i", "ref", int64(4), "100.00"}, {int64(2), "SUBQUERY", "x", "ALL", int64(99999), "100.00"}}},
	}}
	s := svcWith(repo, eng)
	est, err := s.EstimateRows(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select a from o join i on 1=1 limit 10"})
	if err != nil || est.Rows != 2000 {
		t.Fatalf("估算: %+v %v", est, err)
	}
	if eng.got[0] != "EXPLAIN select a from o join i on 1=1" {
		t.Errorf("应去掉 LIMIT 后 EXPLAIN: %q", eng.got[0])
	}
	// 无权限的表：与 Execute 一样被拒绝，拿不到行数
	if _, err = s.EstimateRows(context.Background(), dev, Request{InstanceID: 1, DB: "shop", SQL: "select id from users"}); code(err) != "no_privilege" {
		t.Errorf("估算也要校验权限: %v", err)
	}
	// 引擎报错：降级为未知
	eng.err = errors.New("boom")
	if est, err = s.EstimateRows(context.Background(), admin, Request{InstanceID: 1, SQL: "select a from o"}); err != nil || est.Rows != -1 {
		t.Errorf("引擎报错应降级: %+v %v", est, err)
	}
	// 非 SELECT 不估算
	if est, _ = s.EstimateRows(context.Background(), admin, Request{InstanceID: 1, SQL: "show tables"}); est.Rows != -1 {
		t.Errorf("SHOW 不估算: %+v", est)
	}
}

func TestPlanTips(t *testing.T) {
	cols := []string{"id", "select_type", "table", "type", "possible_keys", "key", "rows", "filtered", "Extra"}
	rows := [][]any{
		{int64(1), "SIMPLE", "orders", "ALL", nil, nil, int64(2500000), "10.00", "Using where; Using filesort"},
		{int64(1), "SIMPLE", "items", "ALL", nil, nil, "20000", "100.00", "Using join buffer (hash join)"},
		{int64(1), "SIMPLE", "small", "ALL", nil, nil, int64(50), "100.00", nil},
	}
	tips := analyzePlan("explain select id from orders where date(created_at) = '2026-10-01' and name like '%abc' order by id limit 20000, 10", cols, rows)
	var all []string
	lv := map[string]string{}
	for _, tp := range tips {
		all = append(all, tp.Text)
		lv[tp.Text[:6]] = tp.Level
	}
	text := strings.Join(all, "\n")
	for _, want := range []string{"orders 做了全表扫描，预计扫描约 2500000", "items 做了全表扫描", "Using filesort", "Using join buffer", "函数 date()", "通配符开头", "深分页"} {
		if !strings.Contains(text, want) {
			t.Errorf("缺少提示 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "small") {
		t.Error("小表全表扫描不应提示")
	}
	if tips[0].Level != "bad" || tips[1].Level != "warn" {
		t.Errorf("等级: %+v", tips[:2])
	}
	// 聚合函数、IN 不算“对列用函数”
	if tips := analyzePlan("explain select 1 from t where id in (1,2) and count(x) > 1", []string{"id"}, nil); len(tips) != 0 {
		t.Errorf("不应有提示: %+v", tips)
	}
	// 经由 Execute 返回
	repo := newRepo()
	eng := &scriptEngine{script: map[string]*engine.ResultSet{"explain ": {ColumnList: cols, Rows: rows}}}
	res, err := svcWith(repo, eng).Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "explain select id from orders"})
	if err != nil || res.Kind != "explain" || len(res.PlanTips) < 3 {
		t.Errorf("Execute 应带执行计划提示: %v %+v", err, res)
	}
}
