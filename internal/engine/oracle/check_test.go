// @author: kevinkelly
// @license: Apache Licence
// @file: check_test.go
// @time: 2026/10/28

package oracle

import (
	"reflect"
	"strings"
	"testing"

	"dbtool/internal/sqlx"
)

func mustCheck(t *testing.T, sql string) *Checked {
	t.Helper()
	ck, err := CheckUserQuery(sql, CheckOptions{DB: "HR"})
	if err != nil {
		t.Fatalf("应放行 %q：%v", sql, err)
	}
	return ck
}

func wantDeny(t *testing.T, sql, contains string) {
	t.Helper()
	ck, err := CheckUserQuery(sql, CheckOptions{DB: "HR"})
	if err == nil {
		t.Fatalf("应拒绝 %q，但放行了：%+v", sql, ck)
	}
	var ue *UserError
	if !asUserError(err, &ue) {
		t.Fatalf("%q：错误不是 *UserError：%T %v", sql, err, err)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("%q：错误 %q 不含 %q", sql, err.Error(), contains)
	}
}

func asUserError(err error, target **UserError) bool {
	if ue, ok := err.(*UserError); ok {
		*target = ue
		return true
	}
	return false
}

func refs(pairs ...string) []sqlx.TableRef {
	var out []sqlx.TableRef
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, sqlx.TableRef{Schema: pairs[i], Name: pairs[i+1]})
	}
	return out
}

func TestCheckAcceptsAndExtractsTables(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		kind   string
		tables []sqlx.TableRef
		dicts  []string
	}{
		{"最简单", "select * from employees", KindSelect, refs("HR", "EMPLOYEES"), nil},
		{"末尾分号和注释", "-- 查员工\nselect * from employees; -- 完", KindSelect, refs("HR", "EMPLOYEES"), nil},
		{"小写 + 跨 schema JOIN", "select e.name, d.name from hr.employees e join sales.depts d on e.d = d.id", KindSelect, refs("HR", "EMPLOYEES", "SALES", "DEPTS"), nil},
		{"逗号连接", "select * from employees e, departments d where e.d = d.id", KindSelect, refs("HR", "EMPLOYEES", "HR", "DEPARTMENTS"), nil},
		{"CTE 不算表", "with x as (select * from t1) select * from x join t2 on 1 = 1", KindSelect, refs("HR", "T1", "HR", "T2"), nil},
		{"CTE 带列名列表 + 递归", "with r(n) as (select 1 from dual union all select n + 1 from r where n < 5) select * from r", KindSelect, nil, []string{"DUAL"}},
		{"多个 CTE", "with a as (select 1 x from dual), b as (select * from a) select * from b", KindSelect, nil, []string{"DUAL"}},
		{"子查询里的表", "select * from t where a in (1, 2, 3) and exists (select 1 from u where u.x = t.x)", KindSelect, refs("HR", "T", "HR", "U"), nil},
		{"FROM 子查询", "select * from (select a from t) x where x.a > 1", KindSelect, refs("HR", "T"), nil},
		{"标量子查询", "select (select max(x) from u) from t", KindSelect, refs("HR", "U", "HR", "T"), nil},
		{"UNION", "select a from t1 union all select a from t2", KindSelect, refs("HR", "T1", "HR", "T2"), nil},
		{"MINUS 与括号开头", "(select a from t1) minus (select a from t2)", KindSelect, refs("HR", "T1", "HR", "T2"), nil},
		{"括号连接", "select * from (t1 a join t2 b on a.x = b.x)", KindSelect, refs("HR", "T1", "HR", "T2"), nil},
		{"ON 后面再逗号一张表", "select * from a join b on a.x = b.x, c", KindSelect, refs("HR", "A", "HR", "B", "HR", "C"), nil},
		{"LEFT JOIN USING", "select * from t1 left join t2 using (id)", KindSelect, refs("HR", "T1", "HR", "T2"), nil},
		{"CROSS APPLY 子查询", "select * from t1 cross apply (select * from t2 where t2.x = t1.x) q", KindSelect, refs("HR", "T1", "HR", "T2"), nil},
		{"加引号的名字", `select "Name" from "Hr"."Emp"`, KindSelect, refs("Hr", "Emp"), nil},
		{"数据字典视图免授权", "select * from all_tables where owner = 'HR'", KindSelect, nil, []string{"ALL_TABLES"}},
		{"SYS 限定的数据字典视图", "select * from sys.all_tab_columns", KindSelect, nil, nil},
		{"DUAL", "select sysdate from dual", KindSelect, nil, []string{"DUAL"}},
		{"分区 / 采样 / 闪回", "select * from t partition (p1) x", KindSelect, refs("HR", "T"), nil},
		{"FETCH FIRST", "select * from t order by a fetch first 10 rows only", KindSelect, refs("HR", "T"), nil},
		{"层次查询", "select * from t start with a is null connect by prior a = b", KindSelect, refs("HR", "T"), nil},
		{"EXPLAIN PLAN FOR", "explain plan for select * from t", KindExplain, refs("HR", "T"), nil},
		{"EXPLAIN FOR", "explain for select * from t", KindExplain, refs("HR", "T"), nil},
		{"JSON_TABLE", "select jt.* from t, json_table(t.doc, '$' columns (a number path '$.a')) jt", KindSelect, refs("HR", "T"), nil},
		{"EXTRACT / TRIM 里的 FROM 不是表", "select extract(year from d), trim(both ' ' from name) from t", KindSelect, refs("HR", "T"), nil},
		{"老式外连接", "select * from a, b where a.x = b.x (+)", KindSelect, refs("HR", "A", "HR", "B"), nil},
		{"分析函数", "select row_number() over (partition by dept order by sal desc) rn from emp", KindSelect, refs("HR", "EMP"), nil},
		{"CAST 类型带精度", "select cast(a as number(10, 2)), cast(b as varchar2(20 char)), cast(c as timestamp(6)) from t", KindSelect, refs("HR", "T"), nil},
		{"INTERVAL 字面量", "select interval '1' day(3) to second(2) from dual", KindSelect, nil, []string{"DUAL"}},
	}
	for _, c := range cases {
		ck := mustCheck(t, c.sql)
		if ck.Kind != c.kind {
			t.Errorf("%s：Kind = %s，期望 %s", c.name, ck.Kind, c.kind)
		}
		if !reflect.DeepEqual(ck.Tables, c.tables) {
			t.Errorf("%s：Tables = %+v，期望 %+v", c.name, ck.Tables, c.tables)
		}
		if !reflect.DeepEqual(ck.DictRefs, c.dicts) {
			t.Errorf("%s：DictRefs = %v，期望 %v", c.name, ck.DictRefs, c.dicts)
		}
	}
}

func TestCheckStmtText(t *testing.T) {
	ck := mustCheck(t, "  /* 头 */ select a  from t -- 尾\n ;")
	if ck.Stmt != "select a  from t" {
		t.Errorf("Stmt = %q", ck.Stmt)
	}
	ck = mustCheck(t, "explain plan for select a from t")
	if ck.Stmt != "select a from t" {
		t.Errorf("EXPLAIN 的内层语句: %q", ck.Stmt)
	}
}

func TestCheckCallNamesAndBareNames(t *testing.T) {
	ck := mustCheck(t, "select nvl(a, 0), count(*), to_char(sysdate, 'YYYY'), rownum, b from t where c = 1")
	if want := []string{"COUNT", "NVL", "TO_CHAR"}; !reflect.DeepEqual(ck.CallNames, want) {
		t.Errorf("CallNames = %v，期望 %v", ck.CallNames, want)
	}
	// 裸名字：列和不带括号的无参函数 / 伪列；表名、别名、关键字、带点的名字不算
	if want := []string{"A", "B", "C", "ROWNUM", "SYSDATE"}; !reflect.DeepEqual(ck.BareNames, want) {
		t.Errorf("BareNames = %v，期望 %v", ck.BareNames, want)
	}
	ck = mustCheck(t, "select e.name, d.id from employees e join depts d on e.d = d.id")
	if len(ck.BareNames) != 0 {
		t.Errorf("限定名不算裸名字: %v", ck.BareNames)
	}
	ck = mustCheck(t, "select row_number() over (partition by a order by b) from t")
	if want := []string{"OVER", "ROW_NUMBER"}; !reflect.DeepEqual(ck.CallNames, want) {
		t.Errorf("CallNames = %v，期望 %v", ck.CallNames, want)
	}
}

func TestCheckDenyStatementKinds(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{"insert into t values (1)", "INSERT"},
		{"update t set a = 1", "UPDATE"},
		{"delete from t", "DELETE"},
		{"merge into t using u on (1=1) when matched then update set a = 1", "MERGE"},
		{"drop table t", "DROP"},
		{"create table x (a int)", "CREATE"},
		{"alter session set nls_date_format = 'x'", "ALTER"},
		{"truncate table t", "TRUNCATE"},
		{"grant select on t to u", "GRANT"},
		{"commit", "COMMIT"},
		{"begin null; end;", "PL/SQL"},
		{"declare x number; begin null; end;", "PL/SQL"},
		{"call p()", "CALL"},
		{"exec p", "EXEC"},
		{"lock table t in exclusive mode", "LOCK"},
		{"set role all", "SET"},
		{"desc t", "SQL*Plus"},
		{"show parameters", "SQL*Plus"},
		{"flashback table t to before drop", "FLASHBACK"},
		{"values (1)", "白名单"},
		{"1 + 1", "SELECT"},
		{"(delete from t)", "SELECT"},
	} {
		wantDeny(t, c.sql, c.want)
	}
}

func TestCheckDenyDangerousConstructs(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{"select * from t for update", "FOR UPDATE"},
		{"select * from t for update nowait", "FOR UPDATE"},
		{"select a into x from t", "INTO"},
		{"select s.nextval from dual", "NEXTVAL"},
		{"select s.currval from dual", "CURRVAL"},
		{"select * from t@lnk", "数据库链接"},
		{"select * from t@lnk.example.com where 1 = 1", "数据库链接"},
		{"select :1 from dual", "绑定变量"},
		{"select * from t where a = :name", "绑定变量"},
		{"select 1 from dual; select 2 from dual", "多条语句"},
		{"select 1 from dual /", "不要加 /"},
		{"select {fn now()} from dual", "不支持的符号"},
		{"with function f return number is begin return 1; end; select f from dual", "WITH FUNCTION"},
		{"select 1 from dual where 1 = 1 and 'x' = 'y'; drop table t", "多条语句"},
		{"select * from t where a = 1 union select * from u for update", "FOR UPDATE"},
		{"", "SQL 为空"},
		{"-- 只有注释", "只有注释"},
		{"select (1 from dual", "括号不匹配"},
		{"select 1) from dual", "括号不匹配"},
		{"select * from a.b.c", "多于两段"},
		{"select * from only (t)", "ONLY"},
	} {
		wantDeny(t, c.sql, c.want)
	}
}

func TestCheckDenyFunctions(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{"select my_func(a) from t", "MY_FUNC"},
		{"select hr.my_func(a) from t", "hr.my_func"},
		{"select dbms_random.value(1, 10) from dual", "dbms_random.value"},
		{"select sys.dbms_lock.sleep(5) from dual", "sys.dbms_lock.sleep"},
		{`select "MyFunc"(a) from t`, "加双引号命名的函数"},
		{"select * from table(my_pipe(1))", "TABLE"},
		{"select cursor(select 1 from dual) from dual", "CURSOR"},
		{"select multiset(select 1 from dual) from dual", "MULTISET"},
		{"select xmlquery('x' passing a returning content) from t", "XQuery"},
		{"select * from xmltable('/a' passing x columns b varchar2(10) path 'b')", "XQuery"},
		{"select deref(r) from t", "DEREF"},
		{"select utl_http.request('http://x') from dual", "utl_http.request"},
		{"select t.col.method() from t", "t.col.method"},
	} {
		wantDeny(t, c.sql, c.want)
	}
}

// 不带括号的无参函数 / 包函数：限定名第一段必须是语句里的表 / 别名 / WITH 名。
func TestCheckDenyQualifiedWithoutParens(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{"select dbms_random.value from dual", "dbms_random.value"},
		{"select hr.purge_everything from dual", "hr.purge_everything"},
		{"select pkg.fn from t", "pkg.fn"},
		{"select sys.dual.dummy from dual", "sys.dual.dummy"},
		{"select * from t where pkg.fn = 1", "pkg.fn"},
		{"select * from t order by pkg.fn", "pkg.fn"},
		{"select u.x from t where exists (select 1 from u2 u3 where u3.a = 1)", "u.x"},
	} {
		wantDeny(t, c.sql, c.want)
	}
	// 合法的限定名
	for _, sql := range []string{
		"select t.a, t.* from t",
		"select e.a from employees e",
		"select employees.a from hr.employees",
		"select hr.employees.a from hr.employees",
		"select x.a from (select a from t) x",
		"select w.a from w where w.a > 1",
		"with w as (select 1 a from dual) select w.a from w",
		"select a.x from (t1 a join t2 b on a.id = b.id)",
		"select e.addr.city from employees e",
		"select * from t1 a where exists (select 1 from t2 b where b.x = a.x)",
	} {
		mustCheck(t, sql)
	}
}

func TestCheckDenySystemObjects(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{"select * from sys.user$", "系统 schema"},
		{"select * from system.help", "系统 schema"},
		{"select * from dba_users", "内部视图"},
		{"select * from v$session", "内部视图"},
		{"select * from gv$session", "内部视图"},
		{"select * from sys.v_$session", "系统 schema"},
		{"select * from hr.dba_x", "内部视图"},
		{"select * from all_source", "ALL_SOURCE"},
		{"select * from public.t", "系统 schema"},
		{"select * from xdb.x", "系统 schema"},
		{"select * from apex_030200.wwv_flows", "系统 schema"},
	} {
		ck, err := CheckUserQuery(c.sql, CheckOptions{DB: "HR"})
		if c.want == "ALL_SOURCE" {
			// ALL_SOURCE 不在免授权的数据字典里：按 HR.ALL_SOURCE 走库表授权（执行前核对不到这个对象，会被拒绝）
			if err != nil || len(ck.Tables) != 1 || ck.Tables[0].Name != "ALL_SOURCE" {
				t.Errorf("%q: %+v %v", c.sql, ck, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q：期望拒绝并含 %q，得到 %+v %v", c.sql, c.want, ck, err)
		}
	}
}

func TestCheckSchemaName(t *testing.T) {
	for _, db := range []string{"", "  ", `A"B`, "A\x00B", "A\nB"} {
		if _, err := CheckUserQuery("select 1 from dual", CheckOptions{DB: db}); err == nil {
			t.Errorf("schema %q 应被拒绝", db)
		}
	}
	if _, err := CheckUserQuery("select 1 from dual", CheckOptions{DB: "HR"}); err != nil {
		t.Error(err)
	}
	long := "select " + strings.Repeat("1, ", 60000) + "1 from dual"
	if _, err := CheckUserQuery(long, CheckOptions{DB: "HR"}); err == nil || !strings.Contains(err.Error(), "过长") {
		t.Errorf("超长语句: %v", err)
	}
}

func TestCheckStringsAndCommentsAreInert(t *testing.T) {
	// 字符串 / 注释 / 引号标识符里的写操作词、函数名不影响判断
	for _, sql := range []string{
		"select 'drop table t; delete from u' from dual",
		"select 1 from dual /* insert into t values (1) */",
		`select "update", "INTO" from t`,
		"select 'dbms_lock.sleep(5)' x from dual",
		"select q'[select * from t where a = :1 for update]' from dual",
	} {
		mustCheck(t, sql)
	}
}
