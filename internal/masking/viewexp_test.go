// @author: kevinkelly
// @license: Apache Licence
// @file: viewexp_test.go
// @time: 2026/10/28

package masking

import (
	"reflect"
	"testing"
)

func refs(rs ...NameRef) []NameRef { return rs }

func TestTableNames(t *testing.T) {
	cases := []struct {
		name string
		d    Dialect
		sql  string
		want []NameRef
	}{
		{"单表", DialectMySQL, "select a from t", refs(NameRef{Name: "t"})},
		{"库名限定、反引号、JOIN、逗号连接的表", DialectMySQL,
			"select * from `shop`.`v_customers` c join other.v2 x on c.id = x.id, t3 where 1",
			refs(NameRef{Schema: "shop", Name: "v_customers"}, NameRef{Schema: "other", Name: "v2"}, NameRef{Name: "t3"})},
		{"别名与索引提示不影响后面的逗号", DialectMySQL, "select 1 from a x, b y use index (i1), c",
			refs(NameRef{Name: "a"}, NameRef{Name: "b"}, NameRef{Name: "c"})},
		{"ON 条件之后的逗号", DialectMySQL, "select 1 from a join b on a.x = b.x, c",
			refs(NameRef{Name: "a"}, NameRef{Name: "b"}, NameRef{Name: "c"})},
		{"select 列表里的子查询", DialectMySQL, "select (select max(x) from t2), c from t1",
			refs(NameRef{Name: "t2"}, NameRef{Name: "t1"})},
		{"派生表之后的逗号", DialectMySQL, "select 1 from (select 1 from d1) x, t2",
			refs(NameRef{Name: "d1"}, NameRef{Name: "t2"})},
		{"括起来的连接组", DialectMySQL, "select 1 from (a join b on a.x = b.x) join c on 1 = 1",
			refs(NameRef{Name: "a"}, NameRef{Name: "b"}, NameRef{Name: "c"})},
		{"STRAIGHT_JOIN", DialectMySQL, "select 1 from a straight_join b on a.x = b.x", refs(NameRef{Name: "a"}, NameRef{Name: "b"})},
		{"UNION 的两边", DialectMySQL, "select 1 from a union all select 2 from b", refs(NameRef{Name: "a"}, NameRef{Name: "b"})},
		{"limit / order by / group by 里的逗号不是连接", DialectMySQL, "select 1 from a group by x, y order by x, y limit 1, 2", refs(NameRef{Name: "a"})},
		{"CTE 里的表也收进来", DialectMySQL, "with c as (select 1 from base) select * from c", refs(NameRef{Name: "base"}, NameRef{Name: "c"})},
		{"IS DISTINCT FROM 不是表位置", DialectPostgres, "select a is distinct from b from t", refs(NameRef{Name: "t"})},
		{"PostgreSQL：没加引号折成小写，加了引号保持原样；ONLY / LATERAL / 函数", DialectPostgres,
			`select * from "Reporting"."V_Account" a, only Public.T, lateral fn(a.x) f, unnest(arr) u`,
			refs(NameRef{Schema: "Reporting", Name: "V_Account"}, NameRef{Schema: "public", Name: "t"})},
		{"函数调用和 #临时表、@表变量不算，但不影响后面的表", DialectMSSQL, "select * from #tmp, @tv, string_split('a,b', ',') s, dbo.t2",
			refs(NameRef{Schema: "dbo", Name: "t2"})},
		{"SQL Server：三段名、方括号、WITH (NOLOCK)、APPLY", DialectMSSQL,
			"select * from [ERP].[dbo].[v_buyer] b with (nolock), dbo.t2 cross apply dbo.f(b.id) x",
			refs(NameRef{Catalog: "ERP", Schema: "dbo", Name: "v_buyer"}, NameRef{Schema: "dbo", Name: "t2"})},
		{"SQL Server：省略架构 a..b", DialectMSSQL, "select * from ERP..v_buyer", refs(NameRef{Catalog: "ERP", Name: "v_buyer"})},
		{"四段名（链接服务器）不收", DialectMSSQL, "select * from s.db.dbo.t", nil},
		{"去重", DialectMySQL, "select 1 from t a join t b on 1 = 1", refs(NameRef{Name: "t"})},
	}
	for _, c := range cases {
		got := TableNames(c.sql, c.d)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n  得到 %+v\n  期望 %+v", c.name, got, c.want)
		}
	}
	// EXTRACT(… FROM x) 这类函数里的 FROM 会把 x 当成表位置：多查一次库，查不到忽略，不会漏掉真正的表。
	got := TableNames("select extract(year from d) from t", DialectMySQL)
	found := false
	for _, r := range got {
		found = found || r.Name == "t"
	}
	if !found {
		t.Errorf("EXTRACT 之外的表要收进来: %+v", got)
	}
}

func TestCheckViewBodyAndClean(t *testing.T) {
	ok := []struct {
		d    Dialect
		body string
	}{
		{DialectMySQL, "select `a`.`phone` AS `mobile` from `shop`.`a`"},
		{DialectPostgres, " SELECT account.email AS contact\n   FROM account"},
		{DialectMSSQL, "SELECT Head = LEFT([Phone], 3) FROM [dbo].[Buyer] -- 尾注释\n"},
		{DialectMySQL, "select 'it''s' as q /* 注释 */ from t"},
	}
	for _, c := range ok {
		if err := CheckViewBody(c.body, c.d); err != nil {
			t.Errorf("%q 应通过: %v", c.body, err)
		}
	}
	bad := []struct {
		name string
		d    Dialect
		body string
	}{
		{"字符串没结束", DialectMySQL, "select 'x from t"},
		{"反引号标识符没结束", DialectMySQL, "select `a from t"},
		{"方括号标识符没结束", DialectMSSQL, "select [a from t"},
		{"块注释没结束", DialectMySQL, "select a /* from t"},
		{"括号没配平", DialectMySQL, "select (a from t"},
		{"没有 SELECT", DialectMySQL, "values (1)"},
	}
	for _, c := range bad {
		if err := CheckViewBody(c.body, c.d); err == nil {
			t.Errorf("%s：%q 应不通过", c.name, c.body)
		}
	}
	// 行注释结尾没有换行是正常的
	if _, clean := tokenizeEx("select 1 -- end", DialectMySQL); !clean {
		t.Error("行注释到末尾不算不干净")
	}
}

// 视图查询体交给保守匹配：别名、表达式、嵌套视图、自定义函数都把污染传给视图的输出列。
func TestConservativeHitsViews(t *testing.T) {
	keep := &Rule{Kind: KindKeep, Prefix: 3, Suffix: 4}
	loose := &Rule{Kind: KindKeep, Prefix: 1, Suffix: 0}
	whole := &Rule{Kind: KindWhole}
	cfg := []Column{
		{Schema: "*", Table: "*", Column: "phone", Rule: keep},
		{Schema: "*", Table: "*", Column: "name", Rule: loose},
		{Schema: "*", Table: "*", Column: "id_card", Rule: whole},
	}
	cases := []struct {
		name  string
		d     Dialect
		views []ViewBody
		sql   string
		cols  []string
		want  map[int]*Rule
	}{
		{"视图把 phone 改名", DialectMySQL,
			[]ViewBody{{Body: "select `shop`.`customers`.`phone` AS `mobile`,`shop`.`customers`.`id` AS `id` from `shop`.`customers`"}},
			"select mobile, id from v_customers", []string{"mobile", "id"}, map[int]*Rule{0: keep}},
		{"视图里的表达式列", DialectMySQL,
			[]ViewBody{{Body: "select left(`c`.`phone`,3) AS `head`,`c`.`id` AS `id` from `shop`.`customers` `c`"}},
			"select head, id from v", []string{"head", "id"}, map[int]*Rule{0: keep}},
		{"视图套视图", DialectMySQL,
			[]ViewBody{
				{Body: "select `shop`.`customers`.`phone` AS `mobile` from `shop`.`customers`"},
				{Body: "select `shop`.`v1`.`mobile` AS `m2` from `shop`.`v1`"},
			},
			"select m2 from v2", []string{"m2"}, map[int]*Rule{0: keep}},
		{"视图里调用自定义函数：整列按最严规则", DialectMySQL,
			[]ViewBody{{Body: "select `shop`.`fn_get_phone`(`c`.`id`) AS `p` from `shop`.`customers` `c`"}},
			"select p from v", []string{"p"}, map[int]*Rule{0: whole}},
		{"视图不含敏感字段不误伤", DialectMySQL,
			[]ViewBody{{Body: "select `c`.`id` AS `id`,`c`.`age` AS `age` from `shop`.`customers` `c`"}},
			"select id, age from v", []string{"id", "age"}, map[int]*Rule{}},
		{"select * 取视图", DialectMySQL,
			[]ViewBody{{Body: "select `c`.`phone` AS `mobile`,`c`.`id` AS `id` from `shop`.`customers` `c`"}},
			"select * from v_customers", []string{"mobile", "id"}, map[int]*Rule{0: keep}},
		{"用视图名限定列、语句自带 WITH", DialectMySQL,
			[]ViewBody{{Body: "select `c`.`phone` AS `mobile` from `shop`.`customers` `c`"}},
			"with t as (select 1 as one) select v.mobile, one from v join t on 1 = 1", []string{"mobile", "one"}, map[int]*Rule{0: keep}},
		{"视图的列在外层再改名", DialectMySQL,
			[]ViewBody{{Body: "select `c`.`phone` AS `mobile` from `shop`.`customers` `c`"}},
			"select mobile as x from v", []string{"x"}, map[int]*Rule{0: keep}},
		{"同一视图里两个敏感来源取最严", DialectMySQL,
			[]ViewBody{{Body: "select concat(`c`.`phone`, `c`.`id_card`) AS `both_` from `shop`.`customers` `c`"}},
			"select both_ from v", []string{"both_"}, map[int]*Rule{0: whole}},
		{"PostgreSQL 视图定义", DialectPostgres,
			[]ViewBody{{Body: " SELECT account.phone AS contact,\n    account.id\n   FROM account"}},
			"select contact, id from reporting.v_account", []string{"contact", "id"}, map[int]*Rule{0: keep}},
		{"SQL Server 视图：别名 = 表达式", DialectMSSQL,
			[]ViewBody{{Body: "SELECT Head = LEFT(Phone, 3), BuyerId FROM dbo.Buyer"}},
			"select Head, BuyerId from crm.v_buyer", []string{"Head", "BuyerId"}, map[int]*Rule{0: keep}},
		{"SQL Server 视图带显式列清单：按位置改名", DialectMSSQL,
			[]ViewBody{{Body: "SELECT Phone, BuyerId FROM dbo.Buyer", Columns: []string{"Mobile", "Id2"}}},
			"select Mobile, Id2 from crm.v_buyer", []string{"Mobile", "Id2"}, map[int]*Rule{0: keep}},
		{"带列清单的视图查询体是 *：清单里的名字都按最严处理", DialectMSSQL,
			[]ViewBody{{Body: "SELECT * FROM dbo.Buyer", Columns: []string{"A1", "B1"}}},
			"select A1, B1 from crm.v_buyer", []string{"A1", "B1"}, map[int]*Rule{0: whole, 1: whole}},
		{"视图查询体里 select * from 自定义表值函数：整个结果按最严规则", DialectMSSQL,
			[]ViewBody{{Body: "SELECT * FROM dbo.tvf_buyer(1)"}},
			"select x, y from crm.v_buyer", []string{"x", "y"}, map[int]*Rule{0: whole, 1: whole}},
		{"视图定义本身写坏了（字符串没结束）不影响语句自己的分析", DialectMySQL,
			[]ViewBody{{Body: "select 'x from users"}},
			"select phone, id from customers", []string{"phone", "id"}, map[int]*Rule{0: keep}},
		{"没有视图时与 ConservativeHits 一致", DialectMySQL, nil,
			"select phone, id from customers", []string{"phone", "id"}, map[int]*Rule{0: keep}},
	}
	for _, c := range cases {
		got := map[int]*Rule{}
		for _, h := range ConservativeHitsViews(c.sql, c.cols, cfg, c.d, c.views) {
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
	// 不带视图时，视图改名后的列确实漏（这正是要展开的原因）
	if hs := ConservativeHits("select mobile from v_customers", []string{"mobile"}, cfg, DialectMySQL); len(hs) != 0 {
		t.Errorf("没有视图定义时无从知道 mobile 来自 phone: %+v", hs)
	}
}
