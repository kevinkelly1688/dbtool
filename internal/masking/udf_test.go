// @author: kevinkelly
// @license: Apache Licence
// @file: udf_test.go
// @time: 2026/10/28

package masking

import "testing"

// 非内置函数（存储函数 / UDF / 自定义表值函数）的结果列整列按配置里最严的一条规则脱敏（见 udf.go）。
func TestConservativeUserFunctions(t *testing.T) {
	keep := &Rule{Kind: KindKeep, Prefix: 3, Suffix: 4}
	loose := &Rule{Kind: KindKeep, Prefix: 1, Suffix: 0}
	whole := &Rule{Kind: KindWhole}
	cfg := []Column{
		{Schema: "*", Table: "*", Column: "phone", Rule: keep},
		{Schema: "*", Table: "*", Column: "name", Rule: loose},
		{Schema: "*", Table: "*", Column: "id_card", Rule: whole},
	}
	cases := []struct {
		name string
		d    Dialect
		sql  string
		cols []string
		want map[int]*Rule // 结果列序号 → 规则
	}{
		// ---- MySQL 系：存储函数 ----
		{"存储函数，别名与字段无关", DialectMySQL, "select fn_get_phone(1) as p", []string{"p"}, map[int]*Rule{0: whole}},
		{"存储函数，没有别名", DialectMySQL, "select fn_get_phone(1)", []string{"fn_get_phone(1)"}, map[int]*Rule{0: whole}},
		{"库名限定的存储函数", DialectMySQL, "select shop.fn_get_phone(1) as p", []string{"p"}, map[int]*Rule{0: whole}},
		{"存储函数包在内置函数里", DialectMySQL, "select concat('x', fn_x(1)) as v", []string{"v"}, map[int]*Rule{0: whole}},
		{"内置函数包着存储函数的 CASE", DialectMySQL, "select case when fn_ok(id) then 'y' else 'n' end as flag from t", []string{"flag"}, map[int]*Rule{0: whole}},
		{"返回 JSON 的存储函数", DialectMySQL, "select fn_user_json(1) as j", []string{"j"}, map[int]*Rule{0: whole}},
		{"标量子查询里的存储函数", DialectMySQL, "select (select fn_x(1)) as v", []string{"v"}, map[int]*Rule{0: whole}},
		{"同一语句里只有函数列脱敏", DialectMySQL, "select id, fn_x(id) as v, upper(name) as n2 from t", []string{"id", "v", "n2"}, map[int]*Rule{1: whole, 2: loose}},
		{"函数只出现在 WHERE 里，结果列不受影响", DialectMySQL, "select id from t where id = fn_x(1)", []string{"id"}, map[int]*Rule{}},
		{"子查询别名再引用", DialectMySQL, "select p from (select fn_get_phone(1) as p) t", []string{"p"}, map[int]*Rule{0: whole}},
		{"CTE 里的存储函数", DialectMySQL, "with t as (select fn_x(1) as v) select v, 1 as one from t", []string{"v", "one"}, map[int]*Rule{0: whole}},
		{"UNION 分支里的存储函数", DialectMySQL, "select id from a union all select fn_x(1) from b", []string{"id"}, map[int]*Rule{0: whole}},
		{"用户变量带出存储函数的值", DialectMySQL, "set @p := fn_x(1); select @p as p, 1 as one", []string{"p", "one"}, map[int]*Rule{0: whole}},
		{"同一条语句里先赋值再引用", DialectMySQL, "select @v := fn_x(1) as a, @v as b, 1 as c", []string{"a", "b", "c"}, map[int]*Rule{0: whole, 1: whole}},
		{"星号之后的存储函数从尾部对位", DialectMySQL, "select *, fn_x(1) from t", []string{"id", "phone", "fn_x(1)"}, map[int]*Rule{1: keep, 2: whole}},
		{"星号之前的存储函数从头部对位", DialectMySQL, "select fn_a(1), * from t", []string{"fn_a(1)", "id", "phone"}, map[int]*Rule{0: whole, 2: keep}},
		{"两个星号之间的存储函数位置不定，整个星号区域脱敏", DialectMySQL, "select a.*, fn_x(1), b.* from a, b", []string{"id", "x", "c1", "c2"}, map[int]*Rule{0: whole, 1: whole, 2: whole, 3: whole}},
		{"星号区域之外的普通列不受影响", DialectMySQL, "select id, a.*, fn_x(1), b.*, name from a, b", []string{"id", "c0", "x", "c1", "name"}, map[int]*Rule{1: whole, 2: whole, 3: whole, 4: loose}},
		{"参数就是敏感字段：取最严而不是该字段自己的规则", DialectMySQL, "select fn_fmt(phone) as x from t", []string{"x"}, map[int]*Rule{0: whole}},
		// ---- 内置函数、类型名、关键字不受影响 ----
		{"内置函数与聚合不算", DialectMySQL, "select concat('a', 'b') as c, now() as n, count(*) as t, ifnull(x, 0) as i, date_format(d, '%Y') as y, json_extract(j, '$.a') as je from t",
			[]string{"c", "n", "t", "i", "y", "je"}, map[int]*Rule{}},
		{"CAST / CONVERT 里的类型名不算", DialectMySQL, "select cast(a as decimal(10,2)) as d, convert(b, char(10)) as c from t", []string{"d", "c"}, map[int]*Rule{}},
		{"窗口函数不算", DialectMySQL, "select row_number() over (partition by a order by b) as rn, sum(x) over (order by b rows between 1 preceding and current row) as s from t", []string{"rn", "s"}, map[int]*Rule{}},
		{"IN / EXISTS 子查询不算", DialectMySQL, "select a in (select 1) as x, exists (select 1) as y from t", []string{"x", "y"}, map[int]*Rule{}},
		{"Doris 内置函数不算", DialectMySQL, "select bitmap_union(b) as bu, array_join(a, ',') as aj, date_trunc(d, 'day') as dt from t", []string{"bu", "aj", "dt"}, map[int]*Rule{}},
		// ---- SQL Server ----
		{"标量 UDF", DialectMSSQL, "select dbo.fn_get_phone(1) as p", []string{"p"}, map[int]*Rule{0: whole}},
		{"别名 = 表达式的 UDF", DialectMSSQL, "select top 10 p = dbo.fn_x(1), id from users", []string{"p", "id"}, map[int]*Rule{0: whole}},
		{"变量带出 UDF 的值", DialectMSSQL, "declare @k nvarchar(50) = dbo.fn_get_phone(1); select @k as k, 1 as one", []string{"k", "one"}, map[int]*Rule{0: whole}},
		{"SET 变量带出 UDF 的值", DialectMSSQL, "declare @k nvarchar(50); set @k = 'x' + dbo.fn_x(1); select @k as k", []string{"k"}, map[int]*Rule{0: whole}},
		{"表变量带出 UDF 的值", DialectMSSQL, "declare @t table (x nvarchar(50)); insert into @t select dbo.fn_x(1); select x from @t", []string{"x"}, map[int]*Rule{0: whole}},
		{"表变量 VALUES 里的 UDF", DialectMSSQL, "declare @t table (x nvarchar(50)); insert into @t values (dbo.fn_x(1)); select x from @t", []string{"x"}, map[int]*Rule{0: whole}},
		{"表值函数 select *", DialectMSSQL, "select * from dbo.tvf_users(1)", []string{"mobile_no", "name"}, map[int]*Rule{0: whole, 1: whole}},
		{"表值函数：别名限定的列", DialectMSSQL, "select u.id, f.contact from users u cross apply dbo.tvf_contact(u.id) f", []string{"id", "contact"}, map[int]*Rule{1: whole}},
		{"表值函数：没有限定的列分不清来源", DialectMSSQL, "select id, contact from users cross apply dbo.tvf_contact(users.id) as f", []string{"id", "contact"}, map[int]*Rule{0: whole, 1: whole}},
		{"表值函数：别名.* 在前，别的表的列在后", DialectMSSQL, "select f.*, u.id from users u cross apply dbo.tvf(u.id) f", []string{"a", "b", "id"}, map[int]*Rule{0: whole, 1: whole}},
		{"表值函数：别的表的 别名.* 不受影响", DialectMSSQL, "select u.*, f.contact from users u outer apply dbo.tvf(u.id) f", []string{"id", "phone", "contact"}, map[int]*Rule{1: keep, 2: whole}},
		{"JOIN 里的表值函数", DialectMSSQL, "select f.contact from users u join dbo.tvf(1) f on f.id = u.id", []string{"contact"}, map[int]*Rule{0: whole}},
		{"派生表里的表值函数 select *", DialectMSSQL, "select * from (select * from dbo.tvf(1)) t", []string{"a", "b"}, map[int]*Rule{0: whole, 1: whole}},
		{"CTE 里的表值函数按别名传到外层", DialectMSSQL, "with t as (select f.contact as c from dbo.tvf(1) f) select c, 1 as one from t", []string{"c", "one"}, map[int]*Rule{0: whole}},
		{"SQL Server 内置函数不算", DialectMSSQL, "select string_agg(a, ',') within group (order by b) as s, datediff(day, a, b) as d, convert(varchar(10), c, 120) as v, isnull(x, 0) as i, json_value(j, '$.a') as jv, len(x) as l, getdate() as g from t",
			[]string{"s", "d", "v", "i", "jv", "l", "g"}, map[int]*Rule{}},
		{"系统架构的函数不算", DialectMSSQL, "select sys.fn_my_permissions(null, null) as q, cast(x as nvarchar(max)) as c from t", []string{"q", "c"}, map[int]*Rule{}},
		{"xml / hierarchyid 实例方法不算", DialectMSSQL, "select x.value('(/a)[1]', 'varchar(20)') as xv, h.GetLevel() as lv from t x", []string{"xv", "lv"}, map[int]*Rule{}},
		{"FOR XML PATH 子查询不算", DialectMSSQL, "select stuff((select ',' + a from t2 for xml path('')), 1, 1, '') as s", []string{"s"}, map[int]*Rule{}},
		{"内置表值函数不算", DialectMSSQL, "select value from string_split('a,b', ',')", []string{"value"}, map[int]*Rule{}},
		{"变量只带出内置函数的值不误伤", DialectMSSQL, "declare @n int = len('abc'); select @n as n", []string{"n"}, map[int]*Rule{}},
		// ---- PostgreSQL ----
		{"带架构的函数", DialectPostgres, "select public.get_phone(1) as p", []string{"p"}, map[int]*Rule{0: whole}},
		{"不带架构的自定义函数", DialectPostgres, "select get_phone(1) as p", []string{"p"}, map[int]*Rule{0: whole}},
		{"表值函数", DialectPostgres, "select * from public.get_users(1)", []string{"contact", "name"}, map[int]*Rule{0: whole, 1: whole}},
		{"表值函数带别名与列别名", DialectPostgres, "select u.contact from get_users(1) as u(contact, nm)", []string{"contact"}, map[int]*Rule{0: whole}},
		{"PostgreSQL 内置函数不算", DialectPostgres, "select pg_catalog.lower(a) as l, lower(b) as l2, to_char(now(), 'YYYY') as t, jsonb_build_object('a', 1) as j, count(*) filter (where x > 1) as c, a::varchar(20) as v, unnest(arr) as u, string_agg(a, ',' order by b) as sa from t",
			[]string{"l", "l2", "t", "j", "c", "v", "u", "sa"}, map[int]*Rule{}},
		{"内置表值函数不算", DialectPostgres, "select g from generate_series(1, 3) g", []string{"g"}, map[int]*Rule{}},
		// ---- ClickHouse：内置函数清单列不全，目前不判断 ----
		{"ClickHouse 不判断自定义函数", DialectClickHouse, "select myfn(1) as p", []string{"p"}, map[int]*Rule{}},
	}
	for _, c := range cases {
		got := map[int]*Rule{}
		for _, h := range ConservativeHits(c.sql, c.cols, cfg, c.d) {
			got[h.Index] = h.Rule
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: 命中 %v，期望 %v", c.name, got, c.want)
			continue
		}
		for i, r := range c.want {
			if got[i] != r {
				t.Errorf("%s: 第 %d 列规则 %v，期望 %v", c.name, i, got[i], r)
			}
		}
	}
}

// 取最严的一条：配置里没有整体隐藏时，保留位数最少的最严；多条规则里取严与字段名无关。
func TestConservativeUserFunctionStrictest(t *testing.T) {
	keep := &Rule{Kind: KindKeep, Prefix: 3, Suffix: 4}
	loose := &Rule{Kind: KindKeep, Prefix: 1, Suffix: 0}
	cfg := []Column{
		{Schema: "*", Table: "*", Column: "phone", Rule: keep},
		{Schema: "*", Table: "*", Column: "name", Rule: loose},
	}
	hits := ConservativeHits("select fn_x(1) as v", []string{"v"}, cfg, DialectMySQL)
	if len(hits) != 1 || hits[0].Rule != loose {
		t.Fatalf("应取保留位数最少的一条: %+v", hits)
	}
	// 总开关里只有防火墙强制脱敏的列（整体隐藏）时同样适用
	forced := []Column{{Schema: "s", Table: "t", Column: "secret", Rule: &Rule{Kind: KindWhole}}}
	hits = ConservativeHits("select fn_x(1) as v", []string{"v"}, forced, DialectMySQL)
	if len(hits) != 1 || hits[0].Rule.Kind != KindWhole {
		t.Fatalf("强制脱敏的列也应触发: %+v", hits)
	}
	// 没有配置任何字段：不脱敏（实例没有脱敏配置时不受这条规则影响）
	if ConservativeHits("select fn_x(1) as v", []string{"v"}, nil, DialectMySQL) != nil {
		t.Error("没有配置应返回 nil")
	}
	// 配置里的字段名全是空串：没有可参照的规则，不误伤
	if hits = ConservativeHits("select fn_x(1) as v", []string{"v"}, []Column{{Schema: "*", Table: "*", Column: " ", Rule: keep}}, DialectMySQL); len(hits) != 0 {
		t.Errorf("字段名为空的配置不应触发: %+v", hits)
	}
}

// 函数识别本身：限定名、系统架构、关键字、类型名。
func TestIsUserFunc(t *testing.T) {
	cases := []struct {
		d     Dialect
		chain []string
		want  bool
	}{
		{DialectMySQL, []string{"fn_x"}, true},
		{DialectMySQL, []string{"concat"}, false},
		{DialectMySQL, []string{"shop", "concat"}, true}, // 限定名一律是自定义函数
		{DialectMySQL, []string{"in"}, false},
		{DialectMSSQL, []string{"dbo", "fn_x"}, true},
		{DialectMSSQL, []string{"sys", "fn_x"}, false},
		{DialectMSSQL, []string{"x", "value"}, false},
		{DialectMSSQL, []string{"x", "my_method"}, true},
		{DialectMSSQL, []string{"@v"}, false},
		{DialectPostgres, []string{"pg_catalog", "lower"}, false},
		{DialectPostgres, []string{"public", "lower"}, true},
		{DialectPostgres, []string{"jsonb_build_object"}, false},
		{DialectClickHouse, []string{"anything"}, false},
		{DialectMySQL, nil, false},
	}
	for _, c := range cases {
		if got := isUserFunc(c.chain, c.d); got != c.want {
			t.Errorf("isUserFunc(%v, %v) = %v，期望 %v", c.chain, c.d, got, c.want)
		}
	}
}
