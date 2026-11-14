// @author: kevinkelly
// @license: Apache Licence
// @file: oracle_test.go
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
	"dbtool/internal/engine/oracle"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// Oracle 的在线查询接线：语句检查用真实的 oracle.CheckUserQuery，执行 / 元数据用假引擎（引擎自己的执行逻辑在 internal/engine/oracle 里测）。
// 覆盖：放行 / 拒绝 / 库表授权 / 数据字典免授权 / 错误分类 / 限量 / 脱敏（列名、视图展开）/ 库表树元数据 / 查询日志。

const oraID = uint(14)

type oraFake struct {
	engine.Engine // 只实现用到的方法；其余调用会 panic（说明接线多调用了东西）
	shaped        *oracle.Shaped
	err           error
	checked       []*oracle.Checked
	lims          []oracle.QueryLimits
	tables        []oracle.TableInfo
	cols          map[string][]oracle.ColumnInfo
	hits          []oracle.ColumnHit
	views         []engine.ViewDef
	viewErr       error
	viewAsked     [][]engine.ViewRef
}

func (f *oraFake) Close() error { return nil }
func (f *oraFake) RunUser(_ context.Context, ck *oracle.Checked, lim oracle.QueryLimits) (*oracle.Shaped, error) {
	f.checked = append(f.checked, ck)
	f.lims = append(f.lims, lim)
	if f.err != nil {
		return nil, f.err
	}
	return f.shaped, nil
}
func (f *oraFake) TableInfos(context.Context, string) ([]oracle.TableInfo, error) {
	return f.tables, nil
}
func (f *oraFake) ColumnInfos(_ context.Context, _, table string) ([]oracle.ColumnInfo, error) {
	return f.cols[table], nil
}
func (f *oraFake) SearchColumns(context.Context, string, string) ([]oracle.ColumnHit, error) {
	return f.hits, nil
}
func (f *oraFake) ViewDefs(_ context.Context, _ string, refs []engine.ViewRef) ([]engine.ViewDef, error) {
	f.viewAsked = append(f.viewAsked, refs)
	return f.views, f.viewErr
}

type oraEnv struct {
	repo *evRepo
	fake *oraFake
	svc  *Service
}

func newOraEnv(t *testing.T, cols ...model.DataMaskingColumn) *oraEnv {
	t.Helper()
	e := &oraEnv{fake: &oraFake{
		shaped: &oracle.Shaped{ColumnList: []string{"ID", "NAME"}, ColumnTypes: []string{"NUMBER", "VARCHAR2"}, Rows: [][]any{{int64(1), "a"}, {int64(2), "b"}}},
		tables: []oracle.TableInfo{{Name: "EMPLOYEES", Kind: "table", Comment: "员工", Rows: ptr64(107)}, {Name: "EMP_V", Kind: "view", IsView: true}, {Name: "MV_S", Kind: "matview", IsView: true}},
		cols: map[string][]oracle.ColumnInfo{"EMPLOYEES": {
			{Name: "ID", Type: "NUMBER(10)", IsPrimaryKey: true, Position: 1}, {Name: "PHONE", Type: "VARCHAR2(20 CHAR)", Nullable: true, Comment: "手机号", Position: 2}}},
		hits: []oracle.ColumnHit{{Table: "EMPLOYEES", Column: "PHONE", Comment: "手机号"}},
	}}
	r := newRepo()
	r.cfg[settings.KeyQueryMaskEnabled] = "true"
	r.maskCols = cols
	r.insts = map[uint]*model.Instance{oraID: {ID: oraID, InstanceName: "ora-01", DBType: "oracle", Type: "master", Tags: canRead()}}
	r.perms[7] = map[string]bool{"query.run": true, "query.view": true}
	r.userInst[7] = map[uint]bool{oraID: true}
	e.repo = &evRepo{histRepo: &histRepo{fakeRepo: r}, ready: true}
	e.svc = NewService(e.repo, func(dbType string, _ engine.Conn) (engine.Engine, error) {
		if dbType == "oracle" {
			return e.fake, nil
		}
		return nil, fmt.Errorf("暂不支持的数据库类型: %s", dbType)
	}, nil)
	return e
}

func ptr64(n int64) *int64 { return &n }

func (e *oraEnv) grant(typ int, db, tb string, limit int) {
	e.repo.privs = append(e.repo.privs, model.QueryPrivilege{UserName: "dev", InstanceID: oraID, PrivType: typ, DBName: db, TbName: tb, LimitNum: limit,
		ValidDate: time.Now().AddDate(0, 0, 30)})
}

func (e *oraEnv) exec(p Principal, db, sql string) (*Result, error) {
	return e.svc.Execute(context.Background(), p, Request{InstanceID: oraID, DB: db, SQL: sql})
}

func TestOracleSupportsQuery(t *testing.T) {
	if !SupportsQuery("oracle") || !isMulti("oracle") || !tableLevelType("oracle") {
		t.Fatal("oracle 应支持在线查询，并按库 / 表授权")
	}
	e := newOraEnv(t)
	list, err := e.svc.Instances(dev)
	if err != nil || len(list) != 1 || list[0].DBType != "oracle" {
		t.Fatalf("实例列表: %v %+v", err, list)
	}
}

func TestOracleExecuteAdminShapesResult(t *testing.T) {
	e := newOraEnv(t)
	e.fake.shaped.Truncated, e.fake.shaped.TruncatedBy = true, "rows"
	e.fake.shaped.Warnings = []string{"有 1 个单元格超过 64 KB，已截断显示"}
	e.fake.shaped.Notices = []string{"提示一条"}
	res, err := e.exec(admin, "HR", "-- 查员工\nselect id, name from hr.employees where rownum <= 5; ")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "oracle" || res.StmtKind != "select" || res.RowCount != 2 || strings.Join(res.Columns, ",") != "ID,NAME" || strings.Join(res.ColumnTypes, ",") != "NUMBER,VARCHAR2" {
		t.Fatalf("结果: %+v", res)
	}
	if cellRows(res) != "1,a|2,b" || !res.Truncated || res.TruncatedBy != "rows" || len(res.Warnings) == 0 || len(res.Notices) != 1 {
		t.Errorf("行 / 截断 / 提示: %+v", res)
	}
	if res.ExecutedSQL != "select id, name from hr.employees where rownum <= 5" {
		t.Errorf("执行的语句是去掉注释和分号后的原文: %q", res.ExecutedSQL)
	}
	ck := e.fake.checked[0]
	if ck.DB != "HR" || len(ck.Tables) != 1 || ck.Tables[0].Schema != "HR" || ck.Tables[0].Name != "EMPLOYEES" {
		t.Errorf("引擎收到的检查结果: %+v", ck)
	}
	if lim := e.fake.lims[0]; lim.Timeout <= 0 || lim.MaxRows != res.EffectiveLimit {
		t.Errorf("限额: %+v effective=%d", lim, res.EffectiveLimit)
	}
	if len(e.repo.logs) != 1 || e.repo.logs[0].DBName != "HR" || e.repo.logs[0].EffectRow != 2 || e.repo.logs[0].Username != "root" || res.QueryLogID == 0 {
		t.Errorf("查询日志: %+v", e.repo.logs)
	}
}

func TestOracleAuthorization(t *testing.T) {
	e := newOraEnv(t)
	// 没有权限
	if _, err := e.exec(dev, "HR", "select * from employees"); code(err) != "no_privilege" {
		t.Fatalf("无权限: %v", err)
	}
	if len(e.fake.checked) != 0 {
		t.Fatal("没有权限不能连目标库")
	}
	// 表级权限：没加引号的名字折成大写；limit 取权限里的行数
	e.grant(2, "HR", "EMPLOYEES", 100)
	res, err := e.exec(dev, "HR", "select * from employees")
	if err != nil || res.EffectiveLimit != 100 {
		t.Fatalf("表权限: %v %+v", err, res)
	}
	if _, err := e.exec(dev, "HR", "select * from hr.employees e where exists (select 1 from hr.employees x where x.id = e.id)"); err != nil {
		t.Errorf("同一张表写多次也只要一个权限: %v", err)
	}
	// 加引号的小写名字是另一张表
	if _, err := e.exec(dev, "HR", `select * from "hr"."employees"`); code(err) != "no_privilege" {
		t.Errorf("加引号的小写名字不是 HR.EMPLOYEES: %v", err)
	}
	// 别的表 / 别的 schema
	if _, err := e.exec(dev, "HR", "select * from departments"); code(err) != "no_privilege" {
		t.Errorf("没有 DEPARTMENTS 的权限: %v", err)
	}
	if _, err := e.exec(dev, "HR", "select * from sales.orders"); code(err) != "no_privilege" {
		t.Errorf("没有 SALES.ORDERS 的权限: %v", err)
	}
	e.grant(1, "SALES", "", 50) // SALES 库级权限
	if _, err := e.exec(dev, "HR", "select e.id from employees e join sales.orders o on o.eid = e.id"); err != nil {
		t.Errorf("HR.EMPLOYEES 表权限 + SALES 库权限: %v", err)
	}
	// 数据字典视图与 DUAL 免授权
	for _, sql := range []string{"select sysdate from dual", "select table_name from all_tables where owner = 'HR'", "select column_name from user_tab_columns"} {
		if _, err := e.exec(dev, "HR", sql); err != nil {
			t.Errorf("%s 应免授权: %v", sql, err)
		}
	}
	// 管理员不受库表授权限制
	if _, err := e.exec(admin, "HR", "select * from departments"); err != nil {
		t.Errorf("管理员: %v", err)
	}
}

func TestOracleRejectsBeforeConnecting(t *testing.T) {
	e := newOraEnv(t)
	for _, c := range []struct{ sql, want string }{
		{"delete from employees", "DELETE"},
		{"insert into t values (1)", "INSERT"},
		{"select * from dba_users", "内部视图"},
		{"select * from sys.user$", "系统 schema"},
		{"select my_func(1) from dual", "MY_FUNC"},
		{"select dbms_lock.sleep(5) from dual", "dbms_lock.sleep"},
		{"select * from employees for update", "FOR UPDATE"},
		{"select * from employees@lnk", "数据库链接"},
		{"select seq.nextval from dual", "NEXTVAL"},
		{"select :1 from dual", "绑定变量"},
		{"select 1 from dual; select 2 from dual", "多条语句"},
		{"begin null; end;", "BEGIN"},
	} {
		_, err := e.exec(admin, "HR", c.sql)
		if code(err) != "bad_query" && code(err) != "bad_request" || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q：期望 bad_query 且含 %q，得到 %v", c.sql, c.want, err)
		}
	}
	if len(e.fake.checked) != 0 {
		t.Errorf("被拒绝的语句不能到引擎: %d", len(e.fake.checked))
	}
	if _, err := e.svc.Execute(context.Background(), admin, Request{InstanceID: oraID, SQL: "select 1 from dual"}); code(err) != "bad_request" {
		t.Errorf("没选库: %v", err)
	}
}

func TestOracleEngineErrorsAreClassified(t *testing.T) {
	e := newOraEnv(t)
	for _, c := range []struct {
		err  error
		code string
	}{
		{&oracle.UserError{Msg: "对象 HR.NOPE 不存在，或账号没有访问它的权限"}, "bad_query"},
		{context.DeadlineExceeded, "timeout"},
		{fmt.Errorf("%w: %w", engine.ErrReadOnlyViolation, errors.New("ORA-01456")), "readonly_violation"},
		{errors.New("ORA-00942: table or view does not exist"), "engine_error"},
	} {
		e.fake.err = c.err
		_, err := e.exec(admin, "HR", "select * from employees")
		if code(err) != c.code {
			t.Errorf("%v → %v，期望 %s", c.err, err, c.code)
		}
	}
	e.fake.err = errors.New("ORA-00942: table or view does not exist")
	if _, err := e.exec(admin, "HR", "select * from employees"); err == nil || !strings.Contains(err.Error(), "ORA-00942") {
		t.Errorf("引擎的错误原文要给用户: %v", err)
	}
}

func TestOracleMaskByColumnName(t *testing.T) {
	e := newOraEnv(t, keepCfg("HR", "EMPLOYEES", "PHONE", 3, 4))
	e.fake.shaped = &oracle.Shaped{ColumnList: []string{"ID", "PHONE", "HEAD"}, ColumnTypes: []string{"NUMBER", "VARCHAR2", "VARCHAR2"},
		Rows: [][]any{{int64(1), "13812345678", "13812345678"}}}
	res, err := e.exec(admin, "HR", "select id, phone, substr(phone, 1, 11) head from hr.employees")
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][1] != "138***5678" || res.Rows[0][2] != "138***5678" || res.Rows[0][0] != int64(1) {
		t.Errorf("脱敏结果: %v", res.Rows)
	}
	if strings.Join(res.MaskedColumns, ",") != "PHONE,HEAD" || res.MaskMode != "column-name" || res.Unmasked {
		t.Errorf("脱敏信息: masked=%v mode=%q unmasked=%v", res.MaskedColumns, res.MaskMode, res.Unmasked)
	}
	// 内置函数包着的敏感列也脱敏；不相关的列不误伤
	e.fake.shaped = &oracle.Shaped{ColumnList: []string{"P", "N"}, Rows: [][]any{{"13812345678", "x"}}}
	res, err = e.exec(admin, "HR", "select nvl(phone, '-') p, nvl(name, '-') n from hr.employees")
	if err != nil || res.Rows[0][0] != "138***5678" || res.Rows[0][1] != "x" {
		t.Errorf("函数包着的列: %v %v", res.Rows, err)
	}
	// EXPLAIN 不脱敏（不返回业务数据）
	e.fake.shaped = &oracle.Shaped{ColumnList: []string{"PLAN_TABLE_OUTPUT"}, Rows: [][]any{{"| Id | Operation |"}}}
	res, err = e.exec(admin, "HR", "explain plan for select phone from hr.employees")
	if err != nil || res.StmtKind != "explain" || len(res.MaskedColumns) != 0 {
		t.Errorf("EXPLAIN: %v %+v", err, res)
	}
	// 总开关关闭：不脱敏，并告诉页面
	e.repo.cfg[settings.KeyQueryMaskEnabled] = "false"
	e.fake.shaped = &oracle.Shaped{ColumnList: []string{"PHONE"}, Rows: [][]any{{"13812345678"}}}
	res, err = e.exec(admin, "HR", "select phone from hr.employees")
	if err != nil || res.Rows[0][0] != "13812345678" || !res.Unmasked {
		t.Errorf("总开关关闭: %v %+v", err, res)
	}
}

func TestOracleMaskThroughViews(t *testing.T) {
	e := newOraEnv(t, keepCfg("HR", "EMPLOYEES", "PHONE", 3, 4))
	e.fake.views = []engine.ViewDef{
		{Schema: "HR", Name: "EMP_V", Definition: "select phone, id from employees", ColumnNames: []string{"CONTACT", "ID"}}, // CREATE VIEW emp_v (contact, id) AS …
		{Schema: "HR", Name: "UDF_V", Definition: "select get_phone(id) c, id from employees", ColumnNames: []string{"C", "ID"}},
	}
	e.fake.shaped = &oracle.Shaped{ColumnList: []string{"CONTACT", "ID"}, Rows: [][]any{{"13812345678", int64(1)}}}
	res, err := e.exec(admin, "HR", "select contact, id from emp_v")
	if err != nil || res.Rows[0][0] != "138***5678" || res.Rows[0][1] != int64(1) {
		t.Fatalf("视图改名后的列也要脱敏: %v %+v", err, res)
	}
	// 向引擎要视图定义时：没写 schema 的名字原样交给引擎（引擎按当前 schema 解析）
	if len(e.fake.viewAsked) == 0 || e.fake.viewAsked[0][0].Name != "EMP_V" || e.fake.viewAsked[0][0].Schema != "" {
		t.Errorf("视图定义请求: %+v", e.fake.viewAsked)
	}
	// 视图体里调用了自定义函数：整列按最严规则脱敏
	e.fake.shaped = &oracle.Shaped{ColumnList: []string{"C", "ID"}, Rows: [][]any{{"13812345678", int64(1)}}}
	res, err = e.exec(admin, "HR", "select c, id from udf_v")
	if err != nil || res.Rows[0][0] != "138***5678" {
		t.Errorf("视图里的自定义函数: %v %+v", err, res)
	}
	// 读视图定义失败：默认放行但提示；开启 query.block_on_mask_error 则阻断
	e.fake.views, e.fake.viewErr = nil, errors.New("ORA-01031: insufficient privileges")
	e.fake.shaped = &oracle.Shaped{ColumnList: []string{"ID"}, Rows: [][]any{{int64(1)}}}
	res, err = e.exec(admin, "HR", "select id from emp_v")
	if err != nil || !strings.Contains(strings.Join(res.Warnings, "|"), "没有脱敏") {
		t.Errorf("读不到视图定义应放行并提示: %v %+v", err, res)
	}
	e.repo.cfg[settings.KeyQueryBlockOnMaskError] = "true"
	if _, err := e.exec(admin, "HR", "select id from emp_v"); code(err) != "masking_error" {
		t.Errorf("开启阻断后应拒绝返回: %v", err)
	}
}

func TestOracleMeta(t *testing.T) {
	e := newOraEnv(t)
	e.grant(2, "HR", "EMPLOYEES", 100)
	meta, err := e.svc.DBMeta(context.Background(), dev, oraID, "HR")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]TableMeta{}
	for _, tb := range meta.Tables {
		by[tb.Name] = tb
	}
	if len(meta.Tables) != 3 || by["EMPLOYEES"].Rows != 107 || by["EMPLOYEES"].Comment != "员工" || by["EMPLOYEES"].Kind != "table" || by["EMPLOYEES"].View {
		t.Errorf("表: %+v", meta.Tables)
	}
	if !by["EMP_V"].View || by["EMP_V"].Kind != "view" || by["MV_S"].Kind != "matview" || by["EMP_V"].Rows != -1 {
		t.Errorf("视图: %+v", meta.Tables)
	}
	if by["EMPLOYEES"].Locked || !by["EMP_V"].Locked || meta.LockedTotal != 2 {
		t.Errorf("没有权限的表要标 locked: %+v total=%d", meta.Tables, meta.LockedTotal)
	}
	cols, err := e.svc.TableColumns(context.Background(), dev, oraID, "HR", "EMPLOYEES")
	if err != nil || len(cols) != 2 || cols[0].Key != "PRI" || cols[1].Comment != "手机号" || !cols[1].Nullable {
		t.Errorf("列: %+v %v", cols, err)
	}
	hits, err := e.svc.SearchColumns(context.Background(), dev, oraID, "HR", "phone")
	if err != nil || len(hits) != 1 || hits[0].Column != "PHONE" || hits[0].Comment != "手机号" {
		t.Errorf("搜索: %+v %v", hits, err)
	}
	// 系统 schema 不能浏览
	for _, db := range []string{"SYS", "sys", "SYSTEM", "XDB", "APEX_040200"} {
		if _, err := e.svc.DBMeta(context.Background(), dev, oraID, db); code(err) != "no_privilege" {
			t.Errorf("%s 应拒绝: %v", db, err)
		}
		if _, err := e.svc.TableColumns(context.Background(), dev, oraID, db, "T"); code(err) != "no_privilege" {
			t.Errorf("%s 的列应拒绝: %v", db, err)
		}
	}
	if _, err := e.svc.DBMeta(context.Background(), dev, oraID, ""); code(err) != "bad_request" {
		t.Errorf("没选库: %v", err)
	}
}

func TestOracleSensitiveColumnsAreMarked(t *testing.T) {
	e := newOraEnv(t, keepCfg("HR", "EMPLOYEES", "PHONE", 3, 4))
	cols, err := e.svc.TableColumns(context.Background(), admin, oraID, "HR", "EMPLOYEES")
	if err != nil || len(cols) != 2 || cols[0].Sensitive || !cols[1].Sensitive {
		t.Errorf("敏感列标记: %+v %v", cols, err)
	}
}
