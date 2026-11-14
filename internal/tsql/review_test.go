// @author: kevinkelly
// @license: Apache Licence
// @file: review_test.go
// @time: 2026/10/28

package tsql

import (
	"strings"
	"testing"
)

// run 解析并运行规则，返回命中的规则 ID（去重、按出现顺序）。
func run(t *testing.T, sql string, cfg Config) []string {
	t.Helper()
	p := Parse(sql)
	if !p.OK() {
		t.Fatalf("应能解析: %q\n%v", sql, p.Errors)
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range Review(sql, p, cfg, "shop") {
		if !seen[f.Rule] {
			seen[f.Rule] = true
			out = append(out, f.Rule)
		}
	}
	return out
}

func has(rules []string, want ...string) bool {
	set := map[string]bool{}
	for _, r := range rules {
		set[r] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func TestParseSyntaxErrors(t *testing.T) {
	for _, sql := range []string{"selec * from t", "update t set where id=1", "create table t(", "insert into t values (1,"} {
		p := Parse(sql)
		if p.OK() || len(p.Errors) == 0 {
			t.Errorf("%q 应报语法错误", sql)
			continue
		}
		if p.Errors[0].Line != 1 || p.Errors[0].Message == "" {
			t.Errorf("错误位置/信息: %+v", p.Errors[0])
		}
	}
	// 合法脚本（大小写、方括号、GO 分批、变量、事务、存储过程）
	for _, sql := range []string{
		"SELECT [a].[b] FROM dbo.[Orders] AS a WHERE a.id = 1",
		"declare @i int = 1; update dbo.t set x=@i where id=@i;",
		"begin try begin tran; update t set a=1 where id=1; commit; end try begin catch rollback; throw; end catch",
		"create table dbo.t(id int identity(1,1) primary key, name nvarchar(50) not null)\nGO\ncreate index ix on dbo.t(name)",
		"CREATE PROCEDURE dbo.p @a int AS BEGIN SELECT @a END",
		"merge into t using s on t.id=s.id when matched then update set t.a=s.a;",
	} {
		if p := Parse(sql); !p.OK() {
			t.Errorf("合法 T-SQL 被拒绝: %q\n%v", sql, p.Errors)
		}
	}
	// 位置：多行
	p := Parse("select 1;\nselec 2;")
	if p.OK() || p.Errors[0].Line != 2 {
		t.Errorf("错误应在第 2 行: %+v", p.Errors)
	}
}

func TestRulesDML(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string // 必须命中
		not  []string // 必须不命中
	}{
		{"UPDATE 无 WHERE", "update t set a=1", []string{RuleWhereRequireUpdDel}, nil},
		{"DELETE 无 WHERE", "delete from t", []string{RuleWhereRequireUpdDel}, nil},
		{"UPDATE 有 WHERE", "update t set a=1 where id=1", nil, []string{RuleWhereRequireUpdDel}},
		{"DELETE 有 WHERE", "delete from t where id=1", nil, []string{RuleWhereRequireUpdDel}},
		{"UPDATE 带 FROM 无 WHERE", "update a set a.x=b.x from t a join s b on a.id=b.id", []string{RuleWhereRequireUpdDel}, nil},
		{"SELECT *", "select * from t where id=1", []string{RuleSelectNoSelectAll}, []string{RuleWhereRequireSelect}},
		{"SELECT 无 WHERE", "select id from t", []string{RuleWhereRequireSelect}, []string{RuleSelectNoSelectAll}},
		{"COUNT(*) 不算 select *", "select count(*) from t where id>1", nil, []string{RuleSelectNoSelectAll}},
		{"子查询无 WHERE 不报", "select id from t where exists (select 1 from s) and id>1", nil, []string{RuleWhereRequireSelect}},
		{"无 FROM 的 SELECT", "select 1", nil, []string{RuleWhereRequireSelect}},
		{"LIKE 前导通配", "select id from t where name like '%abc'", []string{RuleNoLeadingWildcard}, nil},
		{"LIKE Unicode 前导通配", "select id from t where name like N'%abc'", []string{RuleNoLeadingWildcard}, nil},
		{"LIKE 后缀通配可用", "select id from t where name like 'abc%'", nil, []string{RuleNoLeadingWildcard}},
		{"INSERT 无列名", "insert into t values (1,2)", []string{RuleInsertMustSpecifyCol}, nil},
		{"INSERT 有列名", "insert into t(a,b) values (1,2)", nil, []string{RuleInsertMustSpecifyCol}},
		{"INSERT DEFAULT VALUES", "insert into t default values", nil, []string{RuleInsertMustSpecifyCol}},
		{"USE", "use other_db", []string{RuleDisallowUse}, nil},
		{"跨库", "update other.dbo.t set a=1 where id=1", []string{RuleCrossDB}, nil},
		{"本库三段名不算跨库", "update shop.dbo.t set a=1 where id=1", nil, []string{RuleCrossDB}},
	}
	for _, c := range cases {
		got := run(t, c.sql, nil)
		if !has(got, c.want...) {
			t.Errorf("%s: 期望命中 %v，实际 %v", c.name, c.want, got)
		}
		for _, n := range c.not {
			if has(got, n) {
				t.Errorf("%s: 不应命中 %s（实际 %v）", c.name, n, got)
			}
		}
	}
}

func TestInsertRowLimit(t *testing.T) {
	var rows []string
	for i := 0; i < 1001; i++ {
		rows = append(rows, "(1)")
	}
	if got := run(t, "insert into t(a) values "+strings.Join(rows, ","), nil); !has(got, RuleInsertRowLimit) {
		t.Errorf("超过 1000 行应命中: %v", got)
	}
	if got := run(t, "insert into t(a) values "+strings.Join(rows[:1000], ","), nil); has(got, RuleInsertRowLimit) {
		t.Errorf("1000 行不应命中: %v", got)
	}
}

func TestRulesDDL(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
		not  []string
	}{
		{"建表无主键", "create table dbo.t(a int not null)", []string{RuleTableRequirePK}, nil},
		{"列级主键", "create table dbo.t(id int primary key)", nil, []string{RuleTableRequirePK, RuleColumnNoNull}},
		{"表级主键", "create table dbo.t(id int not null, constraint pk_t primary key (id))", nil, []string{RuleTableRequirePK}},
		{"临时表不要求主键", "create table #tmp(a int)", nil, []string{RuleTableRequirePK}},
		{"外键", "create table dbo.t(id int primary key, pid int not null references dbo.p(id))", []string{RuleTableNoFK}, nil},
		{"加外键", "alter table dbo.t add constraint fk foreign key (pid) references dbo.p(id)", []string{RuleTableNoFK, RuleBackwardCompat}, nil},
		{"列允许 NULL", "create table dbo.t(id int primary key, name nvarchar(20))", []string{RuleColumnNoNull}, nil},
		{"identity 与计算列不报 NULL", "create table dbo.t(id int identity(1,1) primary key, a int not null, b as (a+1))", nil, []string{RuleColumnNoNull}},
		{"CHAR 过长", "create table dbo.t(id int primary key, code char(30) not null)", []string{RuleColumnMaxCharLen}, nil},
		{"VARCHAR 不受 CHAR 规则影响", "create table dbo.t(id int primary key, code varchar(300) not null)", nil, []string{RuleColumnMaxCharLen}},
		{"改列类型", "alter table dbo.t alter column name nvarchar(100) not null", []string{RuleColumnChangeType, RuleBackwardCompat}, nil},
		{"删列", "alter table dbo.t drop column name", []string{RuleBackwardCompat}, nil},
		{"加 NOT NULL 列无默认值", "alter table dbo.t add c int not null", []string{RuleBackwardCompat}, nil},
		{"加 NOT NULL 列有默认值", "alter table dbo.t add c int not null default 0", nil, []string{RuleBackwardCompat}},
		{"加可空列", "alter table dbo.t add c int null", nil, []string{RuleBackwardCompat}},
		{"加主键", "alter table dbo.t add constraint pk primary key (id)", []string{RuleBackwardCompat}, nil},
		{"DROP TABLE", "drop table dbo.t", []string{RuleBackwardCompat}, nil},
		{"sp_rename", "exec sp_rename 'dbo.t', 'u'", []string{RuleBackwardCompat}, nil},
	}
	for _, c := range cases {
		got := run(t, c.sql, nil)
		if !has(got, c.want...) {
			t.Errorf("%s: 期望命中 %v，实际 %v", c.name, c.want, got)
		}
		for _, n := range c.not {
			if has(got, n) {
				t.Errorf("%s: 不应命中 %s（实际 %v）", c.name, n, got)
			}
		}
	}
}

func TestDangerousStatements(t *testing.T) {
	for _, sql := range []string{
		"drop database x", "alter database x set recovery simple", "exec xp_cmdshell 'dir'", "EXEC master..xp_cmdshell 'dir'",
		"exec sp_configure 'show advanced options', 1", "reconfigure", "shutdown", "dbcc checkdb", "kill 55",
		"select * from openrowset('SQLNCLI','x','select 1')", "bulk insert t from 'c:\\x.csv'", "backup database x to disk='c:\\x.bak'",
		"create login a with password='x'", "restore database x from disk='c:\\x.bak'",
	} {
		p := Parse(sql)
		var lv Level
		for _, f := range Review(sql, p, nil, "shop") {
			if f.Rule == RuleDangerous && f.Level > lv {
				lv = f.Level
			}
		}
		if lv != Error {
			t.Errorf("%q 应被判为危险（ERROR），实际级别 %v，解析错误 %v", sql, lv, p.Errors)
		}
	}
	// 普通语句里出现类似的单词（字符串、注释、标识符）不误报
	for _, sql := range []string{"update t set note='xp_cmdshell' where id=1", "select id from t where id=1 -- openrowset", "update [xp_cmdshell] set a=1 where id=1"} {
		for _, f := range Review(sql, Parse(sql), nil, "shop") {
			if f.Rule == RuleDangerous {
				t.Errorf("%q 不应命中危险规则: %v", sql, f)
			}
		}
	}
}

func TestConfigOverride(t *testing.T) {
	cfg, err := ParseConfig(`{
		"statement.where.require.update-delete": "WARNING",
		"statement.select.no-select-all": "OFF",
		"naming.table": {"level": "ERROR", "regex": "^t_[a-z]+$"},
		"column.required": {"level": "WARNING", "list": ["id", "created_at"]}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg[RuleWhereRequireUpdDel].Level != Warning || cfg[RuleSelectNoSelectAll].Level != Off || cfg[RuleNamingTable].Level != Error {
		t.Errorf("覆盖: %+v", cfg)
	}
	if cfg[RuleNamingTable].Max != 128 {
		t.Error("对象写法没给的参数沿用默认值")
	}
	sql := "create table dbo.BadName(id int primary key)"
	p := Parse(sql)
	var gotNaming, gotRequired bool
	for _, f := range Review(sql, p, cfg, "shop") {
		gotNaming = gotNaming || (f.Rule == RuleNamingTable && f.Level == Error)
		gotRequired = gotRequired || f.Rule == RuleColumnRequired
	}
	if !gotNaming || !gotRequired {
		t.Errorf("命名/必需列规则应命中: naming=%v required=%v", gotNaming, gotRequired)
	}
	if got := run(t, "select * from t where id=1", cfg); has(got, RuleSelectNoSelectAll) {
		t.Error("关闭的规则不应命中")
	}

	for _, bad := range []string{`{`, `{"no.such.rule":"ERROR"}`, `{"naming.table":"FATAL"}`, `{"naming.table":{"regex":"("}}`} {
		if _, err := ParseConfig(bad); err == nil {
			t.Errorf("%s 应报错", bad)
		}
	}
	if c, err := ParseConfig(""); err != nil || len(c) != len(RuleNames()) {
		t.Errorf("空配置: %v %v", c, err)
	}
}

func TestIndexNamingTemplates(t *testing.T) {
	cfg, _ := ParseConfig(`{"naming.index.pk":"ERROR","naming.index.idx":"WARNING","naming.index.uk":"WARNING"}`)
	if got := run(t, "create table dbo.orders(id int not null, constraint PK_orders primary key (id))", cfg); has(got, RuleNamingIndexPK) {
		t.Errorf("符合模板不应命中: %v", got)
	}
	if got := run(t, "create table dbo.orders(id int not null, constraint bad primary key (id))", cfg); !has(got, RuleNamingIndexPK) {
		t.Errorf("不符合模板应命中: %v", got)
	}
	if got := run(t, "create index IX_orders_a_b on dbo.orders(a, b)", cfg); has(got, RuleNamingIndexIDX) {
		t.Errorf("索引名符合 IX_{{table}}_{{column_list}}: %v", got)
	}
	if got := run(t, "create index wrong on dbo.orders(a, b)", cfg); !has(got, RuleNamingIndexIDX) {
		t.Errorf("索引名不符合应命中: %v", got)
	}
}

func TestFindingLines(t *testing.T) {
	sql := "select 1;\nupdate t set a=1;\n"
	var line int
	for _, f := range Review(sql, Parse(sql), nil, "shop") {
		if f.Rule == RuleWhereRequireUpdDel {
			line = f.Line
		}
	}
	if line != 2 {
		t.Errorf("命中行号应为 2: %d", line)
	}
}
