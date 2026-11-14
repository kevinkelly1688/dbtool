// @author: kevinkelly
// @license: Apache Licence
// @file: oracle_test.go
// @time: 2026/10/28

package masking

import (
	"reflect"
	"testing"
)

// Oracle / 达梦：大小写不敏感的列名匹配、反斜杠不是转义、没加引号的名字折成大写、内置函数不算自定义函数。
func TestConservativeHitsOracleFamily(t *testing.T) {
	keep := &Rule{Kind: KindKeep, Prefix: 3, Suffix: 4}
	cfg := []Column{{Schema: "*", Table: "*", Column: "phone", Rule: keep}}
	for _, d := range []Dialect{DialectOracle, DialectDM} {
		cases := []struct {
			name string
			sql  string
			cols []string
			want []int
		}{
			{"结果列名是大写", "select phone, id from hr.employees", []string{"PHONE", "ID"}, []int{0}},
			{"别名", `select phone as "Contact", id from hr.employees`, []string{"Contact", "ID"}, []int{0}},
			{"表达式里引用了敏感字段", "select substr(phone, 1, 3) head, id from employees", []string{"HEAD", "ID"}, []int{0}},
			{"内置函数不算自定义函数：不误伤", "select nvl(name, 'x') n, to_char(sysdate, 'yyyy') y, id from employees", []string{"N", "Y", "ID"}, nil},
			{"自定义函数整列脱敏", "select get_contact(id) c, id from employees", []string{"C", "ID"}, []int{0}},
			{"反斜杠不是转义：字符串到第二个引号就结束", `select 'a\' x, phone from employees`, []string{"X", "PHONE"}, []int{1}},
			{"CTE 传递", "with t as (select phone p from employees) select p from t", []string{"P"}, []int{0}},
			{"子查询别名", "select q.p from (select phone as p from employees) q", []string{"P"}, []int{0}},
			{"DUAL", "select sysdate from dual", []string{"SYSDATE"}, nil},
		}
		for _, c := range cases {
			var got []int
			for _, h := range ConservativeHits(c.sql, c.cols, cfg, d) {
				got = append(got, h.Index)
				if h.Rule != keep {
					t.Errorf("方言 %d %s: 规则 %v", d, c.name, h.Rule)
				}
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("方言 %d %s: 命中 %v，期望 %v", d, c.name, got, c.want)
			}
		}
		// 视图定义：体里的别名、自定义函数把污染传给视图的输出列
		hits := ConservativeHitsViews("select contact, id from v", []string{"CONTACT", "ID"}, cfg, d,
			[]ViewBody{{Body: "select phone as contact, id from hr.employees"}})
		if len(hits) != 1 || hits[0].Index != 0 {
			t.Errorf("方言 %d 视图别名: %+v", d, hits)
		}
		hits = ConservativeHitsViews("select c, id from v", []string{"C", "ID"}, cfg, d,
			[]ViewBody{{Body: "select get_contact(id) c, id from hr.employees"}})
		if len(hits) != 1 || hits[0].Index != 0 {
			t.Errorf("方言 %d 视图里的自定义函数: %+v", d, hits)
		}
	}
	// 达梦自己的内置函数不算自定义函数
	if hits := ConservativeHits("select getdate() a, isnull(name, 'x') b, len(name) c from employees", []string{"A", "B", "C"}, cfg, DialectDM); len(hits) != 0 {
		t.Errorf("达梦内置函数被当成自定义函数: %+v", hits)
	}
	// 达梦的内置函数在 Oracle 方言里查不到：按自定义函数处理（Oracle 没有 getdate）
	if hits := ConservativeHits("select getdate() a from employees", []string{"A"}, cfg, DialectOracle); len(hits) != 1 {
		t.Errorf("Oracle 里 getdate 不是内置函数: %+v", hits)
	}
}

func TestTableNamesOracleFamilyFoldsToUpper(t *testing.T) {
	sql := `select * from hr.emp e join "Mixed"."t1" x on 1 = 1, dept d where exists (select 1 from sales.v1)`
	for _, d := range []Dialect{DialectOracle, DialectDM} {
		got := TableNames(sql, d)
		want := []NameRef{{Schema: "HR", Name: "EMP"}, {Schema: "Mixed", Name: "t1"}, {Name: "DEPT"}, {Schema: "SALES", Name: "V1"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("方言 %d: %+v，期望 %+v", d, got, want)
		}
	}
	// PostgreSQL 仍然折成小写
	if got := TableNames("select * from HR.Emp", DialectPostgres); !reflect.DeepEqual(got, []NameRef{{Schema: "hr", Name: "emp"}}) {
		t.Errorf("PostgreSQL: %+v", got)
	}
}

func TestCheckViewBodyOracleFamily(t *testing.T) {
	for _, d := range []Dialect{DialectOracle, DialectDM} {
		if err := CheckViewBody("select a, b from hr.t where c = 'x\\'", d); err != nil {
			t.Errorf("方言 %d: 反斜杠不是转义，字符串应当正常结束: %v", d, err)
		}
		if err := CheckViewBody("select a from t where c = 'x", d); err == nil {
			t.Errorf("方言 %d: 字符串没有结束应报错", d)
		}
	}
}

// 视图的实际列名（Oracle / 达梦的目录里读到）：CREATE VIEW v (c1, c2) AS select phone, id … 把列改了名，外层引用新名字也要脱敏。
func TestViewBodyPositionalColumns(t *testing.T) {
	keep := &Rule{Kind: KindKeep, Prefix: 3, Suffix: 4}
	cfg := []Column{{Schema: "*", Table: "*", Column: "phone", Rule: keep}}
	for _, d := range []Dialect{DialectOracle, DialectDM} {
		// 视图体里的别名和视图的真实列名不同
		hits := ConservativeHitsViews("select c1, c2 from v", []string{"C1", "C2"}, cfg, d,
			[]ViewBody{{Body: "select phone, id from hr.employees", Positional: []string{"C1", "C2"}}})
		if len(hits) != 1 || hits[0].Index != 0 {
			t.Errorf("方言 %d 按位置重命名: %+v", d, hits)
		}
		// 没有列名时退回按体里的别名
		hits = ConservativeHitsViews("select contact, id from v", []string{"CONTACT", "ID"}, cfg, d,
			[]ViewBody{{Body: "select phone as contact, id from hr.employees"}})
		if len(hits) != 1 || hits[0].Index != 0 {
			t.Errorf("方言 %d 按别名: %+v", d, hits)
		}
		// 体里有 *：位置不可知，列名清单不参与（视图列名与基表列名相同，按列名匹配）；也不会把所有列都当成敏感
		hits = ConservativeHitsViews("select phone, id from v", []string{"PHONE", "ID"}, cfg, d,
			[]ViewBody{{Body: "select * from hr.employees", Positional: []string{"PHONE", "ID"}}})
		if len(hits) != 1 || hits[0].Index != 0 {
			t.Errorf("方言 %d 体里有 *: %+v", d, hits)
		}
		// 项数对不上：清单不参与
		hits = ConservativeHitsViews("select a, b from v", []string{"A", "B"}, cfg, d,
			[]ViewBody{{Body: "select phone as a from hr.employees", Positional: []string{"A", "B"}}})
		if len(hits) != 1 || hits[0].Index != 0 {
			t.Errorf("方言 %d 项数不一致: %+v", d, hits)
		}
	}
}
