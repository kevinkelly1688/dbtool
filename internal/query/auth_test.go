// @author: kevinkelly
// @license: Apache Licence
// @file: auth_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"dbtool/internal/goinception"
	"dbtool/internal/masking"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

var (
	dev = Principal{ID: 7, Username: "dev", Display: "开发者"} // 普通用户
)

func privRow(typ int, db, tb string, limit int) model.QueryPrivilege {
	return model.QueryPrivilege{UserName: "dev", InstanceID: 1, PrivType: typ, DBName: db, TbName: tb, LimitNum: limit,
		ValidDate: time.Now().AddDate(0, 0, 30)}
}

// devRepo 返回一个配置好的仓库：dev 在实例 1 的资源组内，有 query.run，
// 拥有 shop.orders 表权限(3 行)与 report 库权限(10 行)。
func devRepo() *fakeRepo {
	r := newRepo()
	r.perms[7] = map[string]bool{"query.run": true, "query.view": true}
	r.userInst[7] = map[uint]bool{1: true}
	r.privs = []model.QueryPrivilege{privRow(2, "shop", "orders", 3), privRow(1, "report", "", 10)}
	return r
}

func run(t *testing.T, r *fakeRepo, p Principal, db, sql string) (*fakeEngine, *Result, error) {
	t.Helper()
	eng := &fakeEngine{rows: 50}
	res, err := newSvc(r, eng).Execute(context.Background(), p, Request{InstanceID: 1, DB: db, SQL: sql})
	return eng, res, err
}

func TestTablePrivilegeAndLimit(t *testing.T) {
	r := devRepo()
	eng, res, err := run(t, r, dev, "shop", "select id from orders")
	if err != nil {
		t.Fatal(err)
	}
	if res.EffectiveLimit != 3 || eng.gotSQL != "select id from orders LIMIT 3" || res.LimitSource != "shop.orders 表权限" {
		t.Errorf("表权限限制: limit=%d sql=%q src=%q", res.EffectiveLimit, eng.gotSQL, res.LimitSource)
	}
	// 库权限
	if _, res, err = run(t, r, dev, "report", "select dt from daily_sales"); err != nil || res.EffectiveLimit != 10 {
		t.Errorf("库权限: %v %+v", err, res)
	}
	// 没有权限的表：返回结构化的库/表，方便前端直接申请
	_, _, err = run(t, r, dev, "shop", "select id from users")
	var qe *Error
	if !errors.As(err, &qe) || qe.Code != "no_privilege" || qe.HTTP != 403 || qe.Data["database"] != "shop" || qe.Data["table"] != "users" {
		t.Errorf("无权限错误: %#v", err)
	}
	// 连接查询：任意一张表没权限就拒绝
	if _, _, err = run(t, r, dev, "shop", "select a.id from orders a join users b on a.user_id=b.id"); code(err) != "no_privilege" {
		t.Errorf("JOIN 无权限表应拒绝: %v", err)
	}
	// 子查询里的表也要校验（防止用子查询绕过）
	if _, _, err = run(t, r, dev, "shop", "select id from orders where user_id in (select id from users)"); code(err) != "no_privilege" {
		t.Errorf("子查询无权限表应拒绝: %v", err)
	}
	// 跨库：显式写库名
	if _, _, err = run(t, r, dev, "shop", "select dt from report.daily_sales"); err != nil {
		t.Errorf("跨库但有库权限应放行: %v", err)
	}
	if _, _, err = run(t, r, dev, "shop", "select x from other.t"); code(err) != "no_privilege" {
		t.Errorf("跨库无权限应拒绝: %v", err)
	}
	// 库名/表名大小写不敏感（与 MySQL 排序规则一致）
	if _, _, err = run(t, r, dev, "SHOP", "select id from ORDERS"); err != nil {
		t.Errorf("大小写不敏感: %v", err)
	}
}

func TestPrivilegeExpiryAndWildcard(t *testing.T) {
	r := devRepo()
	// fake 仓库本身不过滤过期（真实仓库在 SQL 里过滤），这里验证 '*' 库权限
	r.privs = []model.QueryPrivilege{privRow(1, "*", "", 20)}
	if _, res, err := run(t, r, dev, "anydb", "select a from t"); err != nil || res.EffectiveLimit != 20 {
		t.Errorf("* 库权限: %v %+v", err, res)
	}
	// 库权限与表权限并存时取较小值
	r.privs = []model.QueryPrivilege{privRow(1, "shop", "", 100), privRow(2, "shop", "orders", 5)}
	if _, res, err := run(t, r, dev, "shop", "select id from orders"); err != nil || res.EffectiveLimit != 5 {
		t.Errorf("取较小: %v %+v", err, res)
	}
	// 多表取各表限制的最小值
	r.privs = []model.QueryPrivilege{privRow(2, "shop", "orders", 8), privRow(2, "shop", "users", 4)}
	if _, res, err := run(t, r, dev, "shop", "select a.id from orders a join users b on a.user_id=b.id"); err != nil || res.EffectiveLimit != 4 {
		t.Errorf("多表最小值: %v %+v", err, res)
	}
}

func TestPermissionGates(t *testing.T) {
	// 没有 query.run
	r := devRepo()
	r.perms[7] = map[string]bool{}
	if _, _, err := run(t, r, dev, "shop", "select id from orders"); code(err) != "no_privilege" {
		t.Errorf("缺 query.run 应拒绝: %v", err)
	}
	// 实例不在用户资源组
	r = devRepo()
	r.userInst[7] = nil
	if _, _, err := run(t, r, dev, "shop", "select id from orders"); code(err) != "no_privilege" {
		t.Errorf("实例不在资源组应拒绝: %v", err)
	}
	if l, _ := newSvc(r, &fakeEngine{}).Instances(dev); len(l) != 0 {
		t.Error("实例列表不应包含无权访问的实例")
	}
	// query.admin_run：不校验库表，上限取 admin_query_limit
	r = devRepo()
	r.userInst[7] = nil
	r.perms[7] = map[string]bool{"query.run": true, "query.admin_run": true}
	r.cfg[settings.KeyQueryAdminRowLimit] = "777"
	if _, res, err := run(t, r, dev, "shop", "select id from users"); err != nil || res.EffectiveLimit != 777 {
		t.Errorf("query.admin_run: %v %+v", err, res)
	}
	// query.group_run 需要实例开启查询（can_read，旧标签 read_view 同义）
	r = devRepo()
	r.perms[7] = map[string]bool{"query.run": true, "query.group_run": true}
	r.privs = nil
	if _, _, err := run(t, r, dev, "shop", "select id from users"); code(err) != "no_privilege" {
		t.Errorf("无标签的实例资源组管理员不应放行: %v", err)
	}
	r.insts[1].Tags = append(r.insts[1].Tags, model.InstanceTag{TagCode: "can_read", Active: true})
	if _, res, err := run(t, r, dev, "shop", "select id from users"); err != nil || res.EffectiveLimit != 5000 {
		t.Errorf("资源组管理员: %v %+v", err, res)
	}
	// 页面权限
	svc := newSvc(devRepo(), &fakeEngine{})
	if !svc.CanAccessPage(dev) || svc.CanAccessPage(Principal{ID: 99}) || !svc.CanAccessPage(admin) {
		t.Error("query.view 页面权限判断错误")
	}
}

func TestNonAdminStatementRestrictions(t *testing.T) {
	r := devRepo()
	for _, sql := range []string{"show processlist", "show grants", "show full processlist", "show master status"} {
		if _, _, err := run(t, r, dev, "shop", sql); code(err) != "no_privilege" {
			t.Errorf("%q 普通用户应拒绝: %v", sql, err)
		}
	}
	for _, sql := range []string{"show databases", "show tables", "show create table orders", "show columns from orders"} {
		if _, _, err := run(t, r, dev, "shop", sql); err != nil {
			t.Errorf("%q 应放行: %v", sql, err)
		}
	}
	for _, sql := range []string{"select sleep(100)", "select load_file('/etc/passwd')", "select id, benchmark(1000000, md5('a')) from orders"} {
		if _, _, err := run(t, r, dev, "shop", sql); code(err) != "bad_query" {
			t.Errorf("%q 普通用户应拒绝: %v", sql, err)
		}
	}
	// 管理员不受这些限制
	if _, _, err := run(t, r, admin, "shop", "show processlist"); err != nil {
		t.Errorf("管理员 show processlist: %v", err)
	}
	// 无表查询有较小的行数上限
	if _, res, err := run(t, r, dev, "shop", "select version()"); err != nil || res.EffectiveLimit != noTableLimit {
		t.Errorf("无表查询: %v %+v", err, res)
	}
	// EXPLAIN 不校验库表权限
	if _, _, err := run(t, r, dev, "shop", "explain select id from users"); err != nil {
		t.Errorf("explain: %v", err)
	}
}

type fakeTreeClient struct {
	tree   any
	err    error
	got    goinception.Target
	list   []masking.SelectColumn
	listEr error
	gotSQL string
}

func (c *fakeTreeClient) SelectList(_ context.Context, t goinception.Target, _, stmt string) ([]masking.SelectColumn, error) {
	c.got, c.gotSQL = t, stmt
	return c.list, c.listEr
}

func (c *fakeTreeClient) QueryTree(_ context.Context, t goinception.Target, _, _ string) (any, error) {
	c.got = t
	return c.tree, c.err
}

func TestGoInceptionExtractor(t *testing.T) {
	r := devRepo()
	r.cfg[settings.KeyGoInceptionHost] = "10.1.1.1"
	r.cfg[settings.KeyGoInceptionPort] = "4000"
	r.insts[1].Host, r.insts[1].Port = "db.internal", 3306

	// 语法树里只有 orders（词法解析会把 users 也找出来）：以 goInception 的结果为准
	tree := map[string]any{"From": map[string]any{"TableRefs": map[string]any{"Left": map[string]any{"Source": map[string]any{
		"Name": map[string]any{"O": "orders"}}}, "Right": nil}}}
	fc := &fakeTreeClient{tree: tree}
	ext := NewAutoExtractor(r)
	var gotCfg goinception.Config
	ext.NewClient = func(c goinception.Config) InceptionClient {
		gotCfg = c
		return fc
	}
	auth := NewPrivAuthorizer(r, ext)
	dec, err := auth.AuthorizeQuery(context.Background(), dev, r.insts[1], "shop", "select id from orders", 5000)
	if err != nil || dec.MaxRows != 3 {
		t.Fatalf("goInception 路径: %v %+v", err, dec)
	}
	if gotCfg.Host != "10.1.1.1" || gotCfg.Port != 4000 || fc.got.Host != "db.internal" {
		t.Errorf("配置/目标传递错误: %+v %+v", gotCfg, fc.got)
	}

	// goInception 出错：必须拒绝，不能悄悄降级为词法解析。
	// （库表解析结果会按 实例 + 库 + 语句 缓存 10 秒，供防火墙与授权共用，所以这里换一条没解析过的语句）
	fc.err = errors.New("connection refused")
	_, err = auth.AuthorizeQuery(context.Background(), dev, r.insts[1], "shop", "select id from orders where id = 2", 5000)
	if code(err) != "parse_failed" {
		t.Errorf("goInception 故障应 fail closed: %v", err)
	}
}

func TestFilterDBs(t *testing.T) {
	list := []string{"shop", "report", "test_db", "mysql", "za_x"}
	got, err := filterDBs(list, "", "")
	if err != nil || !reflect.DeepEqual(got, []string{"mysql", "report", "shop", "test_db", "za_x"}) {
		t.Errorf("无过滤: %v %v", got, err)
	}
	got, _ = filterDBs(list, "^(shop|report|za.*)$", "")
	if !reflect.DeepEqual(got, []string{"report", "shop", "za_x"}) {
		t.Errorf("show 正则: %v", got)
	}
	got, _ = filterDBs(list, "", "^(mysql|test_db)$")
	if !reflect.DeepEqual(got, []string{"report", "shop", "za_x"}) {
		t.Errorf("denied 正则: %v", got)
	}
	// 只从开头匹配，不要求匹配整串
	got, _ = filterDBs(list, "sh", "")
	if !reflect.DeepEqual(got, []string{"shop"}) {
		t.Errorf("前缀匹配: %v", got)
	}
	// denied 优先
	got, _ = filterDBs(list, "^(shop|report)$", "^report$")
	if !reflect.DeepEqual(got, []string{"shop"}) {
		t.Errorf("denied 优先: %v", got)
	}
	// 非法正则：报错而不是放行全部
	if _, err := filterDBs(list, "(", ""); code(err) != "bad_config" {
		t.Errorf("非法正则应报错: %v", err)
	}
}

func TestStrictModeRequiresGoInception(t *testing.T) {
	r := devRepo() // 未配置 go_inception_host
	ext := NewAutoExtractor(r)
	ext.Strict = true
	auth := NewPrivAuthorizer(r, ext)
	_, err := auth.AuthorizeQuery(context.Background(), dev, r.insts[1], "shop", "select id from orders", 5000)
	if code(err) != "parse_failed" {
		t.Errorf("严格模式下未配置 goInception 应拒绝: %v", err)
	}
	// 管理员不经过表解析，不受影响
	if _, err := auth.AuthorizeQuery(context.Background(), admin, r.insts[1], "shop", "select id from orders", 5000); err != nil {
		t.Errorf("管理员不应受影响: %v", err)
	}
}

// MySQL 协议系实例（PolarDB / PolarDB-X / SelectDB / Doris / ADB）与 MySQL 一样逐表校验：表权限要能让查询通过，
// 没有授权的表仍被拒绝；只有库权限的行为不变。
func TestTablePrivilegeOnMySQLProtocolFamily(t *testing.T) {
	for _, tp := range []string{"mysql", "polardb", "polardb-x", "polardbx", "selectdb", "doris", "adb"} {
		t.Run(tp, func(t *testing.T) {
			r := devRepo()
			r.insts[1].DBType = tp
			if _, res, err := run(t, r, dev, "shop", "select id from orders"); err != nil || res.EffectiveLimit != 3 || res.LimitSource != "shop.orders 表权限" {
				t.Errorf("表权限应让查询通过: err=%v res=%+v", err, res)
			}
			_, _, err := run(t, r, dev, "shop", "select id from users")
			var qe *Error
			if !errors.As(err, &qe) || qe.Code != "no_privilege" || qe.Data["database"] != "shop" || qe.Data["table"] != "users" {
				t.Errorf("同库未授权的表应拒绝并带出库表: %#v", err)
			}
			if _, _, err := run(t, r, dev, "shop", "select a.id from orders a join users b on a.user_id=b.id"); code(err) != "no_privilege" {
				t.Errorf("JOIN 里有未授权表应拒绝: %v", err)
			}
			if _, res, err := run(t, r, dev, "report", "select dt from daily_sales"); err != nil || res.EffectiveLimit != 10 {
				t.Errorf("库权限不应受影响: err=%v res=%+v", err, res)
			}
		})
	}
}

// SQL Server 仍然只按库校验：表权限不能让它通过（本次改动不影响它）。
func TestTablePrivilegeStillDBLevelForMSSQL(t *testing.T) {
	r := devRepo()
	r.insts[1].DBType = "mssql"
	if _, _, err := run(t, r, dev, "shop", "select id from orders"); code(err) != "no_privilege" {
		t.Errorf("SQL Server 只有表权限应仍被拒绝: %v", err)
	}
}

// 左侧对象树的锁标记来自 PolicyInfo：PolarDB 等实例的表权限要出现在“可查的表”里（库权限覆盖全部表）。
func TestPolicyInfoTablesOnPolarDB(t *testing.T) {
	r := devRepo()
	r.insts[1].DBType = "polardb"
	pa := NewPrivAuthorizer(r, LexicalExtractor{})
	info, err := pa.PolicyInfo(context.Background(), dev, r.insts[1], "shop", 500)
	if err != nil {
		t.Fatal(err)
	}
	if !info.TableLevel || info.AllTables || !reflect.DeepEqual(info.Tables, []string{"orders"}) {
		t.Errorf("shop 库：%+v", info)
	}
	info, err = pa.PolicyInfo(context.Background(), dev, r.insts[1], "report", 500)
	if err != nil || !info.AllTables {
		t.Errorf("report 库权限应覆盖全部表：%+v %v", info, err)
	}
}
