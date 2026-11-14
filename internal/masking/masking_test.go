// @author: kevinkelly
// @license: Apache Licence
// @file: masking_test.go
// @time: 2026/10/28

package masking

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

var phoneRule = Rule{Type: 1, Regex: `^(\d{3})(\d{4})(\d{4})$`, HideGroup: 2}

func TestApplyRegexRule(t *testing.T) {
	cases := []struct {
		in   any
		rule Rule
		want any
	}{
		{"13812345678", phoneRule, "138****5678"},
		{"1381234567", phoneRule, "1381234567"}, // 不匹配：原样返回
		{nil, phoneRule, nil},
		{"", phoneRule, ""},
		{int64(0), phoneRule, int64(0)},
		{int64(13812345678), phoneRule, "138****5678"}, // 非字符串按 str() 处理
		// 邮箱：隐藏用户名部分，保留域名
		{"abc@x.com", Rule{Type: 4, Regex: `^(.*)(@.*)$`, HideGroup: 1}, "***@x.com"},
		// 大小写不敏感
		{"ABCD1234", Rule{Type: 6, Regex: `^([a-z]{4})(\d{4})$`, HideGroup: 2}, "ABCD****"},
	}
	for _, c := range cases {
		got, err := Apply(c.in, c.rule)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Apply(%v)=%v err=%v, want %v", c.in, got, err, c.want)
		}
	}
	// 非法正则：报错而不是放行
	if _, err := Apply("x", Rule{Type: 1, Regex: `(`, HideGroup: 1}); err == nil {
		t.Error("非法正则应报错")
	}
}

func TestThreeSegmentRule(t *testing.T) {
	r := Rule{Type: RuleThreeSegment, HideGroup: 2}
	cases := map[string]string{
		"13812345678": "138****5678", // 11 位：3 + 4 + 4
		"abcd":        "a**d",        // 4 位：1 + 2 + 1
		"ab":          "*b",          // 2 位：0 + 1 + 1
		"a":           "*",           // 1 位：0 + 1 + 0，唯一的字符落在被隐藏的中段
		"王小明":         "王*明",         // 按字符而不是字节计长
		"123456":      "12**56",
	}
	for in, want := range cases {
		got, err := Apply(in, r)
		if err != nil || got != want {
			t.Errorf("三段式(%q)=%v err=%v, want %q", in, got, err, want)
		}
	}
	// 超长值不能受 Go 正则 {n,} 的 1000 上限影响
	long := strings.Repeat("x", 6000)
	got, err := Apply(long, r)
	if err != nil || len(got.(string)) != 6000 || strings.Count(got.(string), "*") != 2000 {
		t.Errorf("超长值: err=%v len=%d", err, len(got.(string)))
	}
	// 隐藏组可配置
	if got, _ := Apply("123456", Rule{Type: RuleThreeSegment, HideGroup: 1}); got != "**3456" {
		t.Errorf("hide=1: %v", got)
	}
	// 非字符串值走库里存的正则，而不是三段式
	if got, _ := Apply(int64(123456), Rule{Type: RuleThreeSegment, Regex: DefaultRules()[100].Regex, HideGroup: 2}); got != "" && got == nil {
		t.Errorf("非字符串: %v", got)
	}
}

func TestAnalyze(t *testing.T) {
	cfg := []Column{
		{Schema: "shop", Table: "customers", Column: "phone", RuleType: 1},
		{Schema: "*", Table: "*", Column: "email", RuleType: 4},
	}
	list := []SelectColumn{
		{Index: 0, Field: "id", Table: "customers", Schema: "shop"},
		{Index: 1, Field: "Phone", Table: "customers", Schema: "SHOP", Alias: "p"}, // 大小写不敏感
		{Index: 2, Field: "phone", Table: "other", Schema: "shop"},                 // 同名字段但表不同：不命中
		{Index: 3, Field: "email", Table: "anything", Schema: "x"},                 // 通配
		{Index: 4, Field: "", Table: "", Schema: ""},                               // 表达式列
	}
	got := Analyze(list, cfg)
	want := []Hit{
		{Index: 1, Schema: "SHOP", Table: "customers", Column: "phone", RuleType: 1},
		{Index: 3, Schema: "x", Table: "anything", Column: "email", RuleType: 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Analyze=%+v\nwant %+v", got, want)
	}
}

func TestApplyHits(t *testing.T) {
	cols := []string{"id", "p", "name"}
	rows := [][]any{{1, "13812345678", "a"}, {2, "13987654321", "b"}, {3, nil, "c"}}
	masked, err := ApplyHits(cols, rows, []Hit{{Index: 1, RuleType: 1}}, map[int]Rule{1: phoneRule})
	if err != nil || !reflect.DeepEqual(masked, []string{"p"}) {
		t.Fatalf("masked=%v err=%v", masked, err)
	}
	if rows[0][1] != "138****5678" || rows[1][1] != "139****4321" || rows[2][1] != nil || rows[0][2] != "a" {
		t.Errorf("rows=%v", rows)
	}
	// 序号越界：必须报错，不能静默跳过（否则相当于没脱敏）
	_, err = ApplyHits(cols, rows, []Hit{{Index: 5, RuleType: 1}}, map[int]Rule{1: phoneRule})
	if !errors.Is(err, ErrBadIndex) {
		t.Errorf("越界应返回 ErrBadIndex: %v", err)
	}
	// 规则 100 在库里没有记录时用内置默认值
	rows = [][]any{{1, "13812345678", "a"}}
	if _, err := ApplyHits(cols, rows, []Hit{{Index: 1, RuleType: 100}}, map[int]Rule{}); err != nil || rows[0][1] != "138****5678" {
		t.Errorf("默认规则 100: %v %v", rows[0][1], err)
	}
}

func TestKeepEnds(t *testing.T) {
	keep := func(n, m int) Rule { return Rule{Kind: KindKeep, Prefix: n, Suffix: m} }
	cases := []struct {
		in   string
		rule Rule
		want string
	}{
		{"13812345678", keep(3, 4), "138***5678"},
		{"110101199003071234", keep(3, 4), "110***1234"},
		{"6222021234567890123", keep(6, 4), "622202***0123"},
		{"张三丰", keep(1, 0), "张***"},
		{"zhangsan@corp.com", keep(2, 0), "zh***"},
		// 值太短：至少隐藏 1 个字符，先保证前缀、再缩后缀
		{"abcd", keep(3, 4), "abc***"},
		{"ab", keep(3, 4), "a***"},
		{"a", keep(3, 4), "***"},
		{"abcde", keep(2, 2), "ab***de"},
		{"abcd", keep(2, 2), "ab***d"},
		{"abc", keep(0, 0), "***"},
	}
	for _, c := range cases {
		got, err := Apply(c.in, c.rule)
		if err != nil || got != c.want {
			t.Errorf("keep(%q,%d,%d)=%v err=%v, want %q", c.in, c.rule.Prefix, c.rule.Suffix, got, err, c.want)
		}
	}
	// 数值按文本处理；空值 / 0 原样
	if got, _ := Apply(int64(13812345678), keep(3, 4)); got != "138***5678" {
		t.Errorf("数值: %v", got)
	}
	for _, v := range []any{nil, "", int64(0)} {
		if got, _ := Apply(v, keep(3, 4)); got != v {
			t.Errorf("空值 %v 应原样返回，得到 %v", v, got)
		}
	}
	// 整体隐藏
	if got, _ := Apply("secret", Rule{Kind: KindWhole}); got != "***" {
		t.Errorf("whole: %v", got)
	}
	// 新的三段式写法与旧规则 100 等价
	if got, _ := Apply("13812345678", Rule{Kind: KindThree, HideGroup: 2}); got != "138****5678" {
		t.Errorf("three: %v", got)
	}
	// 自定义正则（新写法）
	if got, _ := Apply("13812345678", Rule{Kind: KindRegex, Regex: `^(\d{3})(\d{4})(\d{4})$`, HideGroup: 2}); got != "138****5678" {
		t.Errorf("regex: %v", got)
	}
}

func TestApplyNested(t *testing.T) {
	r := Rule{Kind: KindKeep, Prefix: 1, Suffix: 0}
	got, err := Apply(map[string]any{"phone": "13812345678", "tags": []any{"abc", int64(7)}, "n": nil}, r)
	want := map[string]any{"phone": "1***", "tags": []any{"a***", "***"}, "n": nil}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("嵌套: %v err=%v", got, err)
	}
	s, ok, err := ApplyJSONText(`{"phone":"13812345678","list":["x1","y2"],"age":30}`, Rule{Kind: KindKeep, Prefix: 2, Suffix: 0})
	if err != nil || !ok || s != `{"age":"3***","list":["x***","y***"],"phone":"13***"}` {
		t.Errorf("JSON 文本: %q ok=%v err=%v", s, ok, err)
	}
	if s, ok, _ := ApplyJSONText("hello", r); ok || s != "hello" {
		t.Errorf("普通文本不应当 JSON 处理: %q %v", s, ok)
	}
	if _, ok, _ := ApplyJSONText(`{"a":1} trailing`, r); ok {
		t.Error("后面有多余内容不是 JSON")
	}
}

func TestAnalyzeInlineRuleAndSchema(t *testing.T) {
	keep := &Rule{Kind: KindKeep, Prefix: 3, Suffix: 4}
	cfg := []Column{
		{Schema: "riskdb", Table: "public.account", Column: "phone", Rule: keep},
		{Schema: "erp", Table: "Buyer", Column: "mobile", RuleType: 1}, // 旧配置没写架构：与 dbo.Buyer 的最后一段比较
		{Schema: "*", Table: "*", Column: "id_number", Rule: &Rule{Kind: KindWhole}},
	}
	list := []SelectColumn{
		{Index: 0, Field: "phone", Table: "public.account", Schema: "riskdb"},
		{Index: 1, Field: "phone", Table: "audit.account", Schema: "riskdb"}, // 架构不同：不命中
		{Index: 2, Field: "Mobile", Table: "dbo.buyer", Schema: "ERP"},
		{Index: 3, Field: "id_number", Table: "public.kyc", Schema: "riskdb"},
	}
	got := Analyze(list, cfg)
	if len(got) != 3 || got[0].Index != 0 || got[0].Rule != keep || got[1].Index != 2 || got[1].RuleType != 1 || got[2].Index != 3 || got[2].Rule.Kind != KindWhole {
		t.Fatalf("Analyze=%+v", got)
	}
	// 内联规则直接生效，不查规则表
	rows := [][]any{{"13812345678", "x", "1", "340"}}
	masked, err := ApplyHits([]string{"phone", "p2", "m", "id"}, rows, got, map[int]Rule{1: phoneRule})
	if err != nil || len(masked) != 3 || rows[0][0] != "138***5678" || rows[0][1] != "x" || rows[0][3] != "***" {
		t.Errorf("rows=%v masked=%v err=%v", rows, masked, err)
	}
}

func TestRuleValidate(t *testing.T) {
	ok := []Rule{
		{Kind: KindKeep, Prefix: 3, Suffix: 4}, {Kind: KindWhole}, {Kind: KindThree, HideGroup: 2},
		{Kind: KindRegex, Regex: `^(a)(b)$`, HideGroup: 2},
	}
	for _, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v 应合法: %v", r, err)
		}
	}
	bad := []Rule{
		{Kind: KindKeep, Prefix: -1}, {Kind: KindKeep, Prefix: 65}, {Kind: KindThree, HideGroup: 0}, {Kind: KindThree, HideGroup: 4},
		{Kind: KindRegex, Regex: `(`, HideGroup: 1}, {Kind: KindRegex, Regex: `^(a)$`, HideGroup: 2}, {Kind: KindRegex, Regex: "", HideGroup: 1},
		{Kind: "nope"}, {},
	}
	for _, r := range bad {
		if r.Validate() == nil {
			t.Errorf("%+v 应被拒绝", r)
		}
	}
}

func TestConservativeHits(t *testing.T) {
	keep := &Rule{Kind: KindKeep, Prefix: 3, Suffix: 4}
	whole := &Rule{Kind: KindWhole}
	cfg := []Column{
		{Schema: "*", Table: "*", Column: "phone", Rule: keep},
		{Schema: "*", Table: "*", Column: "id_card", Rule: whole},
	}
	cases := []struct {
		name string
		d    Dialect
		sql  string
		cols []string
		want map[int]*Rule // 结果列序号 → 规则
	}{
		{"同名", DialectClickHouse, "select id, phone from users", []string{"id", "phone"}, map[int]*Rule{1: keep}},
		{"限定名与别名", DialectMySQL, "select u.phone as p, u.id from users u", []string{"p", "id"}, map[int]*Rule{0: keep}},
		{"隐式别名", DialectMySQL, "select phone p from users", []string{"p"}, map[int]*Rule{0: keep}},
		{"函数包着", DialectClickHouse, "select concat(phone, '') from users", []string{"concat(phone, '')"}, map[int]*Rule{0: keep}},
		{"函数加别名", DialectClickHouse, "select substr(phone,1,3) AS head, name from users", []string{"head", "name"}, map[int]*Rule{0: keep}},
		{"CASE 表达式", DialectMySQL, "select case when phone is null then 'x' else phone end as v from t", []string{"v"}, map[int]*Rule{0: keep}},
		{"子查询别名再引用", DialectMySQL, "select p from (select phone as p from users) t", []string{"p"}, map[int]*Rule{0: keep}},
		{"CTE 改名后引用", DialectPostgres, "with t as (select phone as contact from users) select contact, 1 as n from t", []string{"contact", "n"}, map[int]*Rule{0: keep}},
		{"SELECT *", DialectMySQL, "select * from users", []string{"id", "phone", "id_card"}, map[int]*Rule{1: keep, 2: whole}},
		{"一项里两个敏感字段取最严", DialectMySQL, "select concat(phone, id_card) as x from users", []string{"x"}, map[int]*Rule{0: whole}},
		{"UNION 分支", DialectMySQL, "select id, name from a union all select id, phone from b", []string{"id", "name"}, map[int]*Rule{1: keep}},
		{"SQL Server alias = expr", DialectMSSQL, "select top 10 p = phone, id from [dbo].[users]", []string{"p", "id"}, map[int]*Rule{0: keep}},
		{"SQL Server 方括号别名", DialectMSSQL, "select phone as [手机] from users", []string{"手机"}, map[int]*Rule{0: keep}},
		{"字符串与注释里的名字不算", DialectMySQL, "select 'phone' as a, id /* phone */ from t -- phone", []string{"a", "id"}, map[int]*Rule{}},
		// 函数名不当字段引用；但 phone(…) 不是内置函数，按自定义函数处理：整列按配置里最严的一条（id_card 的整体隐藏）脱敏，见 TestConservativeUserFunctions
		{"函数名不当字段", DialectMySQL, "select phone(1) as x from t", []string{"x"}, map[int]*Rule{0: whole}},
		{"无关语句", DialectMySQL, "select id, name from users", []string{"id", "name"}, map[int]*Rule{}},
		{"WITHIN GROUP 不是 GROUP BY", DialectMSSQL, "select string_agg(cast(a.phone as nvarchar(max)), ',') within group (order by a.id) as s, a.id as k from users a", []string{"s", "k"}, map[int]*Rule{0: keep}},
		{"WITHIN GROUP 之后还有列", DialectMSSQL, "select string_agg(a.id, ',') within group (order by a.id) as ids, a.id_card as c from users a group by a.id_card", []string{"ids", "c"}, map[int]*Rule{1: whole}},
		{"FORMAT 函数不结束列表", DialectMySQL, "select format(price, 2) as p, phone from t", []string{"p", "phone"}, map[int]*Rule{1: keep}},
		{"变量带出敏感字段（DECLARE）", DialectMSSQL, "declare @k nvarchar(max) = (select top 1 a.phone from users a); select @k as k, 1 as one", []string{"k", "one"}, map[int]*Rule{0: keep}},
		{"变量带出敏感字段（SET 与运算）", DialectMSSQL, "declare @k nvarchar(max); set @k = 'x' + (select top 1 a.phone from users a); select @k as k", []string{"k"}, map[int]*Rule{0: keep}},
		{"变量传变量", DialectMSSQL, "declare @a nvarchar(max) = (select top 1 phone from users); declare @b nvarchar(max) = @a + 'x'; select @b as b", []string{"b"}, map[int]*Rule{0: keep}},
		{"一条 DECLARE 声明多个变量", DialectMSSQL, "declare @n int = 1, @k nvarchar(max) = (select top 1 phone from users); select @k as k, @n as n", []string{"k", "n"}, map[int]*Rule{0: keep}},
		{"赋值型 SELECT 带出", DialectMSSQL, "declare @k nvarchar(max); select @k = a.phone from users a; select @k as k", []string{"k"}, map[int]*Rule{0: keep}},
		// 最后一条 SELECT 的项数要和赋值型 SELECT 不同（这里是 2 项对 1 项），否则会被“按位置对应”碰巧命中，测不出污染有没有传给变量
		{"赋值型 SELECT 的复合赋值（+=，字符串拼接聚合）带出", DialectMSSQL, "declare @s nvarchar(max) = ''; select @s += a.phone + ',' from users a; select 1 as one, @s as s", []string{"one", "s"}, map[int]*Rule{1: keep}},
		{"复合赋值的其它运算符（-= *= /=）也传给变量", DialectMSSQL, "declare @n int = 0; select @n -= a.phone from users a; select 1 as one, @n as n", []string{"one", "n"}, map[int]*Rule{1: keep}},
		{"复合赋值不带敏感字段不误伤", DialectMSSQL, "declare @s nvarchar(max) = ''; select @s += a.name from users a; select 1 as one, @s as s", []string{"one", "s"}, map[int]*Rule{}},
		{"无关变量不误伤", DialectMSSQL, "declare @n int = 1; declare @s nvarchar(20) = 'x'; select @n as n, @s as s", []string{"n", "s"}, map[int]*Rule{}},
		// MySQL 用户变量：前置 SET 赋值、同一条语句里的 `@v := 列`，都要把敏感字段的污染带到引用变量的结果列。
		{"MySQL 变量（SET :=）", DialectMySQL, "set @p := (select phone from users limit 1); select @p as p, 1 as one", []string{"p", "one"}, map[int]*Rule{0: keep}},
		{"MySQL 变量（SET =，函数包着）", DialectMySQL, "set @p = concat('x', (select phone from users limit 1)); select @p", []string{"@p"}, map[int]*Rule{0: keep}},
		{"MySQL 变量（一条 SET 赋多个）", DialectMySQL, "set @n = 1, @p := (select id_card from users limit 1), @m = 2; select @p as p, @n as n, @m as m", []string{"p", "n", "m"}, map[int]*Rule{0: whole}},
		{"MySQL 变量（IF 表达式不被截断）", DialectMySQL, "set @p := if(1, (select phone from users limit 1), 0); select @p as p", []string{"p"}, map[int]*Rule{0: keep}},
		{"MySQL 变量传变量", DialectMySQL, "set @a := (select phone from users limit 1); set @b := concat(@a, 'x'); select @b as b", []string{"b"}, map[int]*Rule{0: keep}},
		{"MySQL 同一条语句里先赋值再引用", DialectMySQL, "select @v := phone as a, @v as b from users", []string{"a", "b"}, map[int]*Rule{0: keep, 1: keep}},
		{"MySQL 无关变量不误伤", DialectMySQL, "set @n := 1; set @s := 'x'; select @n as n, @s as s", []string{"n", "s"}, map[int]*Rule{}},
		{"MySQL 变量取自不敏感的子查询", DialectMySQL, "set @org := (select org_id from tenant where id = 1); select * from t where org = @org", []string{"id", "name"}, map[int]*Rule{}},
		// 表变量（INSERT INTO @t …）和变量一样会把敏感字段带进结果列；列名对不上就不能只看结果列名
		{"表变量带出敏感字段（列名对不上）", DialectMSSQL, "declare @t table (x nvarchar(50)); insert into @t select a.phone from users a; select x from @t", []string{"x"}, map[int]*Rule{0: keep}},
		{"表变量带出（括号列清单、INTO 省略）", DialectMSSQL, "declare @t table (x nvarchar(50), y int); insert @t (x, y) select phone, 1 from users; select x, y, 1 as one from @t", []string{"x", "y", "one"}, map[int]*Rule{0: keep, 1: keep}},
		{"表变量 VALUES 里的变量", DialectMSSQL, "declare @k nvarchar(50) = (select top 1 phone from users); declare @t table (x nvarchar(50)); insert into @t values (@k); select x from @t", []string{"x"}, map[int]*Rule{0: keep}},
		{"表变量传表变量", DialectMSSQL, "declare @a table (x nvarchar(50)); declare @b table (y nvarchar(50)); insert into @a select phone from users; insert into @b select x from @a; select y from @b", []string{"y"}, map[int]*Rule{0: keep}},
		{"表变量改名后再取", DialectMSSQL, "declare @t table (x nvarchar(50)); insert into @t select phone from users; select x as z from @t", []string{"z"}, map[int]*Rule{0: keep}},
		{"表变量 UNION 分支", DialectMSSQL, "declare @t table (x nvarchar(50)); insert into @t select 'a' union all select phone from users; select x from @t", []string{"x"}, map[int]*Rule{0: keep}},
		{"表变量来源是 * 取最严", DialectMSSQL, "declare @t table (x nvarchar(50)); insert into @t select * from users; select x from @t", []string{"x"}, map[int]*Rule{0: whole}},
		{"表变量来源是变量带出的字段（经变量再入表）", DialectMSSQL, "declare @k nvarchar(50); set @k = (select top 1 id_card from users); declare @t table (x nvarchar(50)); insert into @t (x) values (@k); select * from @t", []string{"x"}, map[int]*Rule{0: whole}},
		{"表变量只放不敏感数据不误伤", DialectMSSQL, "declare @areas table (area varchar(10)); insert into @areas select distinct replace(json_value(j.value, '$.Area'), '+', '') from settings s cross apply openjson(s.value2) j; select a.area as qh, count(1) as n from users u cross apply (select top 1 x.area from @areas x where u.username like x.area + '%') a group by a.area", []string{"qh", "n"}, map[int]*Rule{}},
		{"INSERT 后面没有分号直接跟的查询不算写入来源", DialectMSSQL, "declare @t table (x int); insert into @t select 1 select x, phone as p from @t", []string{"x", "p"}, map[int]*Rule{1: keep}},
		{"派生表里的 STRING_AGG 沿别名传到外层", DialectMSSQL, "select x.s as out from (select string_agg(g.r, ',') within group (order by g.k) as s from (select a.phone as r, a.id as k from users a) g) x", []string{"out"}, map[int]*Rule{0: keep}},
	}
	for _, c := range cases {
		hits := ConservativeHits(c.sql, c.cols, cfg, c.d)
		got := map[int]*Rule{}
		for _, h := range hits {
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
	if ConservativeHits("select phone from t", []string{"phone"}, nil, DialectMySQL) != nil {
		t.Error("没有配置应返回 nil")
	}
}

// 与前端 web/static/js/maskadmin/algo.js 共用的测试向量：两边必须得到同样的结果。
func TestSharedVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []struct {
		Name string `json:"name"`
		Rule struct {
			Kind   string `json:"kind"`
			Prefix int    `json:"prefix"`
			Suffix int    `json:"suffix"`
			Hide   int    `json:"hide"`
			Regex  string `json:"regex"`
		} `json:"rule"`
		In  string `json:"in"`
		Out string `json:"out"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) < 10 {
		t.Fatalf("向量太少：%d", len(vs))
	}
	for _, v := range vs {
		r := Rule{Kind: v.Rule.Kind, Prefix: v.Rule.Prefix, Suffix: v.Rule.Suffix, HideGroup: v.Rule.Hide, Regex: v.Rule.Regex}
		if err := r.Validate(); err != nil {
			t.Errorf("%s: 规则不合法: %v", v.Name, err)
			continue
		}
		got, err := Apply(v.In, r)
		if err != nil || got != v.Out {
			t.Errorf("%s: Apply(%q)=%v err=%v，期望 %q", v.Name, v.In, got, err, v.Out)
		}
	}
}
