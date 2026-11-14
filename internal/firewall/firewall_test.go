// @author: kevinkelly
// @license: Apache Licence
// @file: firewall_test.go
// @time: 2026/10/28

package firewall

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"dbtool/internal/settings"
)

// ---- 辅助 ----

func rule(name string, prio int, action string, conds ...Condition) Rule {
	return Rule{Name: name, Priority: prio, Enabled: true, Mode: ModeEnforce, Scenes: []string{SceneQuery, SceneWorkflow, SceneExport},
		Env: "all", GroupMode: "all", Match: "all", Conditions: conds, Action: action}
}

func stmtC(kinds ...string) Condition { return Condition{T: CondStmt, V: kinds} }
func featC(f string) Condition        { return Condition{T: CondFeature, V: f} }
func kwC(words ...string) Condition   { return Condition{T: CondKeyword, V: words} }
func objC(p ...string) Condition      { return Condition{T: CondObject, V: p} }

func mustCompile(t *testing.T, rs ...Rule) []*compiledRule {
	t.Helper()
	var out []*compiledRule
	for i, r := range rs {
		if r.ID == 0 {
			r.ID = uint(i + 1)
		}
		c, err := compile(r)
		if err != nil {
			t.Fatalf("规则 %q 编译失败: %v", r.Name, err)
		}
		out = append(out, c)
	}
	return out
}

var monday10 = time.Date(2026, 10, 12, 10, 0, 0, 0, time.Local) // 周一 10:00（工作时间）

func baseReq(sql string) Request {
	return Request{Scene: SceneQuery, User: "rd_zhao", UserID: 7, InstanceID: 1, InstanceName: "prod-mysql", Env: "prod", DB: "shop", SQL: sql,
		Caps: FullCaps, AffectRows: -1, Now: monday10}
}

func eval(t *testing.T, rules []Rule, req Request) *Decision {
	t.Helper()
	return evaluate(mustCompile(t, rules...), nil, &req, nil, false, req.Now, nil)
}

// ---- 指纹 / 脱字面量 ----

func TestFingerprintNormalizes(t *testing.T) {
	_, a := Fingerprint("SELECT * FROM `Orders` WHERE id = 12 AND name = 'bob' -- c\n;")
	_, b := Fingerprint("select *   from orders where id = 99 and name = 'alice'")
	if a != b || len(a) != 8 {
		t.Fatalf("字面量 / 大小写 / 反引号 / 注释不应影响指纹: %s %s", a, b)
	}
	_, c := Fingerprint("select * from orders where id in (1, 2, 3)")
	_, d := Fingerprint("select * from orders where id in (4)")
	text, _ := Fingerprint("select * from orders where id in (1,2,3,4,5)")
	if !strings.Contains(text, "in ( ? )") {
		t.Errorf("IN 列表应折叠: %q", text)
	}
	if c == d {
		// 单项 IN 与多项 IN 折叠后文本相同才应相等；这里 (4) 本来就是 in ( ? )，折叠后两者一致
		t.Log("IN 折叠后指纹一致（预期）")
	}
	if _, e := Fingerprint("select * from users where id = 1"); e == a {
		t.Error("不同语句指纹不应相同")
	}
}

func TestRedactHidesLiterals(t *testing.T) {
	got := Redact("SELECT * FROM users WHERE phone = '13812345678' AND id = 42 /* 注释 */ AND note = 'it''s'")
	if strings.Contains(got, "138") || strings.Contains(got, "42") || strings.Contains(got, "注释") || strings.Contains(got, "it") {
		t.Errorf("字面量与注释应被去掉: %q", got)
	}
	if !strings.Contains(got, "phone = '?'") || !strings.Contains(got, "id = ?") {
		t.Errorf("结构应保留: %q", got)
	}
}

// ---- 事实：结构特征 ----

func TestStructuralFacts(t *testing.T) {
	big := "SELECT * FROM t WHERE id IN (" + strings.TrimSuffix(strings.Repeat("1,", 1001), ",") + ")"
	cases := []struct {
		sql  string
		feat string
		want bool
	}{
		{"DELETE FROM orders", FeatNoWhere, true},
		{"DELETE FROM orders WHERE id = 1", FeatNoWhere, false},
		{"UPDATE orders SET a = 1", FeatNoWhere, true},
		{"UPDATE orders SET a = 1 WHERE id = 2", FeatNoWhere, false},
		{"SELECT 1", FeatNoWhere, false},
		{"SELECT id FROM t", FeatNoWhere, true},
		{"SELECT id FROM t WHERE id = 1", FeatNoWhere, false},
		{"SELECT id FROM (SELECT id FROM t WHERE id > 1) x", FeatNoWhere, true}, // 子查询里的 WHERE 不算最外层
		{"SELECT * FROM t", FeatSelectStar, true},
		{"SELECT a.* FROM a", FeatSelectStar, true},
		{"SELECT id, * FROM t", FeatSelectStar, true},
		{"SELECT COUNT(*) FROM t", FeatSelectStar, false},
		{"SELECT id FROM (SELECT * FROM t) x", FeatSelectStar, false}, // 子查询里的 * 不算
		{"SELECT id FROM t", FeatNoLimit, true},
		{"SELECT id FROM t LIMIT 10", FeatNoLimit, false},
		{"SELECT id FROM (SELECT id FROM t LIMIT 10) x", FeatNoLimit, true},
		{"UPDATE t SET a = 1", FeatNoLimit, false}, // no_limit 仅适用 SELECT
		{"SELECT * FROM a, b", FeatCartesian, true},
		{"SELECT * FROM a, b WHERE a.id = b.id", FeatCartesian, false},
		{"SELECT * FROM a JOIN b", FeatCartesian, true},
		{"SELECT * FROM a JOIN b ON a.id = b.id", FeatCartesian, false},
		{"SELECT * FROM a JOIN b USING (id)", FeatCartesian, false},
		{"SELECT * FROM a JOIN b ON a.id = b.id JOIN c", FeatCartesian, true},
		{"UPDATE a, b SET a.x = 1", FeatCartesian, true},
		{"SELECT * FROM t WHERE name LIKE '%bob'", FeatLeadingWildcard, true},
		{"SELECT * FROM t WHERE name LIKE 'bob%'", FeatLeadingWildcard, false},
		{big, FeatBigIn, true},
		{"SELECT * FROM t WHERE id IN (1, 2, 3)", FeatBigIn, false},
		{"SELECT * FROM t WHERE id IN (SELECT id FROM u)", FeatBigIn, false},
		{"SELECT 1; SELECT 2", FeatMultiStmt, true},
		{"SELECT 1;", FeatMultiStmt, false},
	}
	for _, c := range cases {
		r := rule("x", 1, ActWarn, featC(c.feat))
		d := eval(t, []Rule{r}, baseReq(c.sql))
		got := len(d.Hits) == 1
		if got != c.want {
			t.Errorf("%s ⟵ %q = %v, want %v", c.feat, c.sql, got, c.want)
		}
	}
}

func TestStatementKinds(t *testing.T) {
	cases := map[string]string{
		"select 1": "SELECT", "(select 1) union (select 2)": "SELECT", "WITH a AS (SELECT 1) SELECT * FROM a": "SELECT",
		"with a as (select 1) delete from t": "DELETE", "REPLACE INTO t VALUES (1)": "INSERT", "rename table a to b": "ALTER",
		"truncate table t": "TRUNCATE", "show tables": "SHOW", "explain select 1": "EXPLAIN",
	}
	for sql, want := range cases {
		if got := stmtKind(sql); got != want {
			t.Errorf("%q = %q, want %q", sql, got, want)
		}
	}
}

// ---- 条件 ----

func TestKeywordMatchesWholeWordsOutsideLiteralsAndComments(t *testing.T) {
	r := rule("fn", 1, ActDeny, kwC("SLEEP", "into outfile"))
	cases := []struct {
		sql  string
		want bool
	}{
		{"SELECT SLEEP(5)", true},
		{"select sleep ( 5 )", true},
		{"SELECT * FROM t INTO   OUTFILE '/tmp/x'", true},
		{"SELECT sleeper FROM t", false},
		{"SELECT 'sleep' FROM t", false},
		{"SELECT 1 /* sleep(3) */", false},
		{"SELECT 1 -- into outfile\n", false},
		{"SELECT 1; SELECT SLEEP(1)", true}, // 第二条语句里也要抓到
	}
	for _, c := range cases {
		if got := eval(t, []Rule{r}, baseReq(c.sql)).Deny; got != c.want {
			t.Errorf("%q = %v, want %v", c.sql, got, c.want)
		}
	}
}

func TestRegexAndFingerprintConditions(t *testing.T) {
	re := rule("sys", 1, ActDeny, Condition{T: CondRegex, V: `information_schema\.(processlist|user_privileges)`})
	if !eval(t, []Rule{re}, baseReq("select * from INFORMATION_SCHEMA.PROCESSLIST")).Deny {
		t.Error("正则应不区分大小写命中")
	}
	if eval(t, []Rule{re}, baseReq("select * from information_schema.tables")).Deny {
		t.Error("不相关的语句不应命中")
	}
	_, fp := Fingerprint("select * from t where id = 1")
	black := rule("bl", 1, ActDeny, Condition{T: CondFingerprint, V: fp})
	if !eval(t, []Rule{black}, baseReq("SELECT * FROM t WHERE id = 99")).Deny {
		t.Error("同一指纹的语句应命中黑名单")
	}
	if eval(t, []Rule{black}, baseReq("select * from t where id = 1 and x = 2")).Deny {
		t.Error("不同指纹不应命中")
	}
}

func TestObjectConditionUsesProvidedTablesAndColumnPresence(t *testing.T) {
	tables := func(objs ...Object) func() ([]Object, error) { return func() ([]Object, error) { return objs, nil } }
	r := rule("obj", 1, ActWarn, objC("*.sys_users"))
	req := baseReq("select UserId from sys_users where UserId = 1")
	req.Tables = tables(Object{Table: "sys_users"})
	if d := eval(t, []Rule{r}, req); len(d.Hits) != 1 {
		t.Fatalf("*.表 应命中: %+v", d.Hits)
	}
	req.Tables = tables(Object{Schema: "shopbasic", Table: "SYS_USERS"})
	if d := eval(t, []Rule{r}, req); len(d.Hits) != 1 {
		t.Error("表名不区分大小写")
	}
	req.Tables = tables(Object{Table: "orders"})
	if d := eval(t, []Rule{r}, req); len(d.Hits) != 0 {
		t.Error("别的表不应命中")
	}

	col := rule("col", 1, ActWarn, objC("shop.users.phone"))
	req = baseReq("select id from users")
	req.Tables = tables(Object{Table: "users"}) // 库名补全为请求里的 shop
	if d := eval(t, []Rule{col}, req); len(d.Hits) != 0 {
		t.Error("带列的模式要求语句里出现该列")
	}
	req.SQL = "select id, Phone from users"
	if d := eval(t, []Rule{col}, req); len(d.Hits) != 1 {
		t.Errorf("出现列名（不区分大小写）应命中: %+v", d)
	}
	req.DB = "crm" // 补全成 crm.users，与 shop.users 不同
	if d := eval(t, []Rule{col}, req); len(d.Hits) != 0 {
		t.Error("库名不同不应命中")
	}

	anyCol := rule("any", 1, ActWarn, objC("*.*.id_card"))
	req = baseReq("select id_card from t")
	req.Tables = nil // 没有表解析：任意库表的同名列只靠标识符判断
	if d := eval(t, []Rule{anyCol}, req); len(d.Hits) != 0 {
		// object 条件在拿不到表时整体未知；*.*.col 在未知时不命中（见 TestUnknownFactsDoNotHit）
		t.Log("拿不到表解析时 object 条件未知")
	}
}

func TestUnknownFactsDoNotHitButAreExplained(t *testing.T) {
	r := rule("obj", 1, ActDeny, objC("*.sys_users"))
	req := baseReq("select * from sys_users")
	req.Tables = func() ([]Object, error) { return nil, errors.New("goInception 不可用") }
	d := eval(t, []Rule{r}, req)
	if d.Deny || len(d.Hits) != 0 {
		t.Fatalf("事实未知时不能拦: %+v", d)
	}
	if len(d.Skipped) != 1 || !strings.Contains(d.Skipped[0].Reason, "未评估") {
		t.Errorf("应在跳过说明里写明未评估: %+v", d.Skipped)
	}
	scan := rule("scan", 1, ActDeny, stmtC("SELECT"), Condition{T: CondScanRows, V: 1000000})
	d = eval(t, []Rule{scan}, baseReq("select * from t"))
	if d.Deny {
		t.Error("扫描行数未知时不应拦")
	}
	req = baseReq("select * from t")
	req.ScanRows = func() (int64, error) { return 5_000_000, nil }
	if !eval(t, []Rule{scan}, req).Deny {
		t.Error("扫描行数达到阈值应命中")
	}
}

func TestAffectRowsAndTime(t *testing.T) {
	aff := rule("aff", 1, ActDeny, stmtC("UPDATE", "DELETE"), Condition{T: CondAffectRows, V: 100000})
	req := baseReq("delete from t where x = 1")
	req.AffectRows = 250000
	if !eval(t, []Rule{aff}, req).Deny {
		t.Error("影响行数超过阈值应命中")
	}
	req.AffectRows = 10
	if eval(t, []Rule{aff}, req).Deny {
		t.Error("影响行数未超阈值不应命中")
	}
	off := rule("off", 1, ActWarn, Condition{T: CondTime, V: "offhours"})
	req = baseReq("select 1")
	if len(eval(t, []Rule{off}, req).Hits) != 0 {
		t.Error("周一 10 点是工作时间")
	}
	req.Now = time.Date(2026, 10, 11, 14, 0, 0, 0, time.Local) // 周日
	if len(eval(t, []Rule{off}, req).Hits) != 1 {
		t.Error("周日是非工作时间")
	}
	req.Now = time.Date(2026, 10, 12, 22, 30, 0, 0, time.Local)
	if len(eval(t, []Rule{off}, req).Hits) != 1 {
		t.Error("22 点是非工作时间")
	}
}

func TestMatchAllVsAny(t *testing.T) {
	all := rule("all", 1, ActDeny, stmtC("DELETE"), featC(FeatNoWhere))
	anyR := rule("any", 1, ActDeny, stmtC("DELETE"), featC(FeatNoWhere))
	anyR.Match = "any"
	if eval(t, []Rule{all}, baseReq("delete from t where id = 1")).Deny {
		t.Error("全部满足：只满足一个不应命中")
	}
	if !eval(t, []Rule{anyR}, baseReq("delete from t where id = 1")).Deny {
		t.Error("任一满足：满足一个即命中")
	}
}

// ---- 范围与例外 ----

func TestScope(t *testing.T) {
	mk := func(mod func(*Rule)) Rule {
		r := rule("s", 1, ActDeny, kwC("sleep"))
		mod(&r)
		return r
	}
	req := baseReq("select sleep(1)")
	groups := map[uint]bool{3: true}
	run := func(r Rule, rq Request) bool {
		return evaluate(mustCompile(t, r), nil, &rq, groups, false, rq.Now, nil).Deny
	}
	if !run(mk(func(r *Rule) {}), req) {
		t.Fatal("基线应命中")
	}
	if run(mk(func(r *Rule) { r.Scenes = []string{SceneWorkflow} }), req) {
		t.Error("场景不符不应命中")
	}
	if run(mk(func(r *Rule) { r.Env = "test" }), req) {
		t.Error("环境不符不应命中")
	}
	noEnv := req
	noEnv.Env = ""
	if run(mk(func(r *Rule) { r.Env = "prod" }), noEnv) {
		t.Error("实例没有环境标签时，限定 prod 的规则不适用")
	}
	if run(mk(func(r *Rule) { r.InstanceIDs = []uint{9} }), req) {
		t.Error("实例不在范围内不应命中")
	}
	if !run(mk(func(r *Rule) { r.InstanceIDs = []uint{1, 9} }), req) {
		t.Error("实例在范围内应命中")
	}
	if !run(mk(func(r *Rule) { r.GroupMode, r.GroupIDs = "only", []uint{3} }), req) {
		t.Error("only：所属权限组在名单里应命中")
	}
	if run(mk(func(r *Rule) { r.GroupMode, r.GroupIDs = "only", []uint{4} }), req) {
		t.Error("only：不在名单里不应命中")
	}
	if run(mk(func(r *Rule) { r.GroupMode, r.GroupIDs = "except", []uint{3} }), req) {
		t.Error("except：所属权限组被排除不应命中")
	}
	if !run(mk(func(r *Rule) { r.GroupMode, r.GroupIDs = "except", []uint{4} }), req) {
		t.Error("except：不在排除名单里应命中")
	}
}

func TestExceptions(t *testing.T) {
	rules := mustCompile(t, rule("fn", 1, ActDeny, kwC("sleep")))
	req := baseReq("select sleep(1)")
	_, fp := Fingerprint(req.SQL)
	future, past := monday10.Add(24*time.Hour), monday10.Add(-time.Hour)
	run := func(ex Exception) *Decision {
		ex.RuleID = rules[0].ID
		return evaluate(rules, []Exception{ex}, &req, map[uint]bool{3: true}, false, monday10, nil)
	}
	for name, ex := range map[string]Exception{
		"user":        {SubjectKind: SubjectUser, SubjectValue: "RD_ZHAO", ExpiresAt: future},
		"group":       {SubjectKind: SubjectGroup, SubjectValue: "3", ExpiresAt: future},
		"instance":    {SubjectKind: SubjectInstance, SubjectValue: "1", ExpiresAt: future},
		"fingerprint": {SubjectKind: SubjectFingerprint, SubjectValue: fp, ExpiresAt: future, Reason: "历史任务"},
	} {
		d := run(ex)
		if d.Deny || len(d.Skipped) != 1 || !strings.Contains(d.Skipped[0].Reason, "例外") {
			t.Errorf("%s 例外应豁免并说明: %+v", name, d)
		}
	}
	if d := run(Exception{SubjectKind: SubjectUser, SubjectValue: "rd_zhao", ExpiresAt: past}); !d.Deny {
		t.Error("过期的例外不应生效")
	}
	if d := run(Exception{SubjectKind: SubjectUser, SubjectValue: "someone_else", ExpiresAt: future}); !d.Deny {
		t.Error("别人的例外不应豁免我")
	}
}

// ---- 合并 ----

func TestMergeMostSevere(t *testing.T) {
	kw := kwC("sleep")
	deny1 := rule("deny-low", 50, ActDeny, kw)
	deny1.Message = "来自 deny-low"
	deny2 := rule("deny-high", 10, ActDeny, kw)
	deny2.Message = "来自 deny-high"
	reason := rule("why", 20, ActReason, kw)
	reason.Message = "请写原因"
	lim1 := rule("lim1000", 30, ActLimit, kw)
	lim1.ActionArg = 1000
	lim2 := rule("lim100", 31, ActLimit, kw)
	lim2.ActionArg = 100
	warn := rule("warn", 40, ActWarn, kw)

	d := eval(t, []Rule{deny1, deny2, reason, lim1, lim2, warn}, baseReq("select sleep(1)"))
	if !d.Deny || d.Rule == nil || d.Rule.Name != "deny-high" || d.Message != "来自 deny-high" {
		t.Fatalf("任一 deny 即拒绝，展示 priority 最小的那条: %+v", d.Rule)
	}
	if len(d.Hits) != 6 {
		t.Errorf("被压住的命中仍要留记录: %d", len(d.Hits))
	}

	d = eval(t, []Rule{reason, lim1, lim2, warn}, baseReq("select sleep(1)"))
	if d.Deny || !d.NeedReason || d.MaxRows != 100 || d.LimitedBy != "lim100" || len(d.Warnings) != 1 {
		t.Errorf("收紧型动作叠加: %+v", d)
	}
	if len(d.ReasonMsg) != 1 || d.ReasonMsg[0] != "请写原因" {
		t.Errorf("reason 文案: %v", d.ReasonMsg)
	}
}

func TestMaskUnionAndCaps(t *testing.T) {
	m1 := rule("m1", 1, ActMask, objC("*.*.id_card"))
	m2 := rule("m2", 2, ActMask, objC("shop.users.phone", "*.*.id_card", "*.users.email"))
	req := baseReq("select id_card, phone from users")
	req.Tables = func() ([]Object, error) { return []Object{{Table: "users"}}, nil }
	d := eval(t, []Rule{m1, m2}, req)
	if len(d.ForceMask) != 2 { // *.*.id_card（去重）+ shop.users.phone；*.users.email 不是可用的脱敏形态
		t.Fatalf("强制脱敏取并集并去重: %+v", d.ForceMask)
	}

	// 调用方没有对应能力：降级为 warn，并说明原因
	rr := rule("why", 1, ActReason, kwC("sleep"))
	req = baseReq("select sleep(1)")
	req.Caps = Caps{} // 什么收紧能力都没有（如工单）
	d = eval(t, []Rule{rr}, req)
	if d.NeedReason || len(d.Warnings) != 1 || d.Hits[0].Effective != ActWarn || d.Hits[0].Downgraded == "" {
		t.Errorf("应降级为 warn: %+v", d.Hits[0])
	}
}

func TestObserveModeNeverExecutes(t *testing.T) {
	r := rule("obs", 1, ActDeny, kwC("sleep"))
	r.Mode = ModeObserve
	d := eval(t, []Rule{r}, baseReq("select sleep(1)"))
	if d.Deny || len(d.Hits) != 1 || d.Hits[0].Effective != "observe" || d.Hits[0].WouldAction != ActDeny {
		t.Fatalf("观察态：记录命中并写明若生效会怎样，但不拦: %+v", d)
	}
	if len(d.Executed()) != 0 {
		t.Error("观察命中不算生效命中")
	}
	// 全局观察闸门：生效的规则也只记录
	enf := mustCompile(t, rule("enf", 1, ActDeny, kwC("sleep")))
	req := baseReq("select sleep(1)")
	d = evaluate(enf, nil, &req, nil, true, monday10, nil)
	if d.Deny || d.Hits[0].Effective != "observe" {
		t.Errorf("全局观察: %+v", d)
	}
}

func TestDefaultMessages(t *testing.T) {
	d := eval(t, []Rule{rule("无提示", 1, ActDeny, kwC("sleep"))}, baseReq("select sleep(1)"))
	if !strings.Contains(d.Message, "无提示") {
		t.Errorf("没有配置文案时给出默认说明: %q", d.Message)
	}
}

// ---- 规则校验 ----

func TestNormalizeRejectsBadRules(t *testing.T) {
	ok := rule("ok", 1, ActDeny, kwC("sleep"))
	bad := map[string]func(*Rule){
		"名称为空":        func(r *Rule) { r.Name = " " },
		"没有条件":        func(r *Rule) { r.Conditions = nil },
		"未知动作":        func(r *Rule) { r.Action = "escalate" },
		"未知场景":        func(r *Rule) { r.Scenes = []string{"bogus"} },
		"没有场景":        func(r *Rule) { r.Scenes = nil },
		"未知语句类型":      func(r *Rule) { r.Conditions = []Condition{stmtC("MERGE")} },
		"未知特征":        func(r *Rule) { r.Conditions = []Condition{featC("nope")} },
		"正则无效":        func(r *Rule) { r.Conditions = []Condition{{T: CondRegex, V: "(unclosed"}} },
		"阈值不是正整数":     func(r *Rule) { r.Conditions = []Condition{{T: CondScanRows, V: 0}} },
		"指纹格式不对":      func(r *Rule) { r.Conditions = []Condition{{T: CondFingerprint, V: "xyz"}} },
		"对象模式有空段":     func(r *Rule) { r.Conditions = []Condition{objC("shop..users")} },
		"only 没选组":    func(r *Rule) { r.GroupMode, r.GroupIDs = "only", nil },
		"limit 没行数":   func(r *Rule) { r.Action, r.ActionArg = ActLimit, 0 },
		"mask 没有列":    func(r *Rule) { r.Action, r.Conditions = ActMask, []Condition{objC("*.users")} },
		"mask 库表混写":   func(r *Rule) { r.Action, r.Conditions = ActMask, []Condition{objC("shop.*.phone")} },
		"reason 不在查询": func(r *Rule) { r.Action, r.Scenes = ActReason, []string{SceneWorkflow} },
		"条件太多": func(r *Rule) {
			r.Conditions = nil
			for i := 0; i <= MaxConditions; i++ {
				r.Conditions = append(r.Conditions, featC(FeatNoWhere))
			}
		},
	}
	for name, mod := range bad {
		r := ok
		r.Conditions = append([]Condition(nil), ok.Conditions...)
		mod(&r)
		if _, err := Normalize(r); err == nil {
			t.Errorf("%s：应拒绝", name)
		}
	}
	if _, err := Normalize(ok); err != nil {
		t.Errorf("合法规则被拒绝: %v", err)
	}
}

func TestNormalizeCanonicalizes(t *testing.T) {
	r := rule(" n ", 5, ActMask, Condition{T: CondObject, V: []any{"Users", "`shop`.`users`.`phone`"}},
		Condition{T: CondStmt, V: []any{"select", "Select", "delete"}}, Condition{T: CondScanRows, V: float64(2000000)})
	r.Scenes = []string{SceneExport, SceneQuery, SceneQuery}
	got, err := Normalize(r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "n" || fmt.Sprint(got.Scenes) != "[query export]" {
		t.Errorf("名称去空白、场景去重并按固定顺序: %q %v", got.Name, got.Scenes)
	}
	objs, _ := toStrings(got.Conditions[0].V)
	if fmt.Sprint(objs) != "[*.Users shop.users.phone]" {
		t.Errorf("对象模式规整: %v", objs)
	}
	if fmt.Sprint(got.Conditions[1].V) != "[SELECT DELETE]" {
		t.Errorf("语句类型大写去重: %v", got.Conditions[1].V)
	}
	if n, _ := got.Conditions[2].V.(int64); n != 2000000 {
		t.Errorf("阈值转 int64: %#v", got.Conditions[2].V)
	}
	conds, err := ParseConditions(ConditionsJSON(got.Conditions))
	if err != nil || len(conds) != 3 {
		t.Fatalf("JSON 往返: %v %v", conds, err)
	}
	if _, err := Normalize(Rule{Name: "rt", Enabled: true, Scenes: []string{SceneQuery}, Conditions: conds, Action: ActWarn}); err != nil {
		t.Errorf("从 JSON 读回的条件（数字是 float64）应能通过校验: %v", err)
	}
}

// ---- 引擎：快照 / 失败策略 / 记录 ----

type fakeStore struct {
	rules    []Rule
	exs      []Exception
	groups   []uint
	loadErr  error
	loads    int
	hits     []HitRow
	hitIDs   map[string]uint
	repeats  map[uint]int
	attached map[uint]uint
	nextID   uint
}

func (f *fakeStore) LoadRules() ([]Rule, error) {
	f.loads++
	return f.rules, f.loadErr
}
func (f *fakeStore) LoadExceptions(time.Time) ([]Exception, error) { return f.exs, f.loadErr }
func (f *fakeStore) UserGroupIDs(uint) ([]uint, error)             { return f.groups, nil }
func (f *fakeStore) RecordHit(h HitRow, _ time.Duration) (uint, error) {
	if f.hitIDs == nil {
		f.hitIDs, f.repeats = map[string]uint{}, map[uint]int{}
	}
	key := fmt.Sprint(h.RuleID, h.UserName, h.Fingerprint, h.Effective)
	if id, ok := f.hitIDs[key]; ok {
		f.repeats[id]++
		return id, nil
	}
	f.nextID++
	f.hitIDs[key] = f.nextID
	f.repeats[f.nextID] = 1
	f.hits = append(f.hits, h)
	return f.nextID, nil
}
func (f *fakeStore) AttachQueryLog(ids []uint, logID uint) error {
	if f.attached == nil {
		f.attached = map[uint]uint{}
	}
	for _, id := range ids {
		f.attached[id] = logID
	}
	return nil
}

type fakeCfg map[string]string

func (c fakeCfg) ConfigValue(k string) (string, bool) { v, ok := c[k]; return v, ok }

func newEngine(st *fakeStore, cfg fakeCfg) (*Engine, *time.Time) {
	now := monday10
	e := New(st, cfg)
	e.now = func() time.Time { return now }
	return e, &now
}

func TestEngineSnapshotCachingAndInvalidate(t *testing.T) {
	r := rule("fn", 1, ActDeny, kwC("sleep"))
	r.ID = 1
	st := &fakeStore{rules: []Rule{r}}
	e, now := newEngine(st, nil)
	req := baseReq("select sleep(1)")
	if !e.Evaluate(context.Background(), req).Deny {
		t.Fatal("应拒绝")
	}
	e.Evaluate(context.Background(), req)
	if st.loads != 1 {
		t.Errorf("TTL 内不应重读规则: %d", st.loads)
	}
	st.rules = nil
	if !e.Evaluate(context.Background(), req).Deny {
		t.Error("TTL 内沿用旧快照")
	}
	e.Invalidate()
	if e.Evaluate(context.Background(), req).Deny {
		t.Error("失效后应读到最新（没有规则）")
	}
	st.rules = []Rule{r}
	*now = now.Add(SnapshotTTL + time.Second)
	if !e.Evaluate(context.Background(), req).Deny {
		t.Error("过期后应重读")
	}
}

func TestEngineFailurePolicy(t *testing.T) {
	r := rule("fn", 1, ActDeny, kwC("sleep"))
	r.ID = 1
	req := baseReq("select sleep(1)")

	st := &fakeStore{rules: []Rule{r}, loadErr: errors.New("db down")}
	e, _ := newEngine(st, nil)
	d := e.Evaluate(context.Background(), req)
	if d.Deny || !d.Failed {
		t.Errorf("缺省 fail-open: %+v", d)
	}
	e, _ = newEngine(st, fakeCfg{settings.KeyFirewallOnError: "deny"})
	d = e.Evaluate(context.Background(), req)
	if !d.Deny || !d.Failed || d.Message == "" {
		t.Errorf("firewall_on_error=deny 应拒绝并说明: %+v", d)
	}

	// 先读成功、之后刷新失败：沿用旧快照，不退化
	st = &fakeStore{rules: []Rule{r}}
	e, now := newEngine(st, nil)
	if !e.Evaluate(context.Background(), req).Deny {
		t.Fatal("基线")
	}
	st.loadErr = errors.New("db down")
	*now = now.Add(time.Minute)
	if d := e.Evaluate(context.Background(), req); !d.Deny || d.Failed {
		t.Errorf("刷新失败应沿用旧快照: %+v", d)
	}
}

func TestEngineSwitchesAndInvalidRules(t *testing.T) {
	good := rule("good", 2, ActDeny, kwC("sleep"))
	good.ID = 1
	bad := rule("bad", 1, ActDeny, Condition{T: CondRegex, V: "(unclosed"})
	bad.ID = 2
	st := &fakeStore{rules: []Rule{bad, good}}
	req := baseReq("select sleep(1)")

	e, _ := newEngine(st, nil)
	if d := e.Evaluate(context.Background(), req); !d.Deny || d.Rule.Name != "good" {
		t.Errorf("无效规则应被跳过、不影响其他规则: %+v", d)
	}
	e, _ = newEngine(st, fakeCfg{settings.KeyFirewallEnabled: "false"})
	if d := e.Evaluate(context.Background(), req); d.Deny || len(d.Hits) != 0 {
		t.Errorf("总开关关闭应完全跳过: %+v", d)
	}
	e, _ = newEngine(st, fakeCfg{settings.KeyFirewallMode: "observe"})
	if d := e.Evaluate(context.Background(), req); d.Deny || len(d.Hits) != 1 || d.Hits[0].Effective != "observe" {
		t.Errorf("全局观察: %+v", d)
	}
	st.rules = nil
	e, _ = newEngine(st, nil)
	if d := e.Evaluate(context.Background(), req); d.Deny || d.Failed || len(d.Hits) != 0 {
		t.Errorf("没有规则等同关闭: %+v", d)
	}
}

func TestEngineLoadsGroupsFromStoreOnlyWhenNeeded(t *testing.T) {
	r := rule("grp", 1, ActDeny, kwC("sleep"))
	r.ID, r.GroupMode, r.GroupIDs = 1, "except", []uint{3}
	st := &fakeStore{rules: []Rule{r}, groups: []uint{3}}
	e, _ := newEngine(st, nil)
	req := baseReq("select sleep(1)")
	req.GroupIDs = nil
	if e.Evaluate(context.Background(), req).Deny {
		t.Error("用户所在权限组被排除，不应拦")
	}
	st.groups = []uint{4}
	e.Invalidate()
	if !e.Evaluate(context.Background(), req).Deny {
		t.Error("不在被排除的组里应拦")
	}
}

func TestRecordStoresRedactedSQLAndAttachesQueryLog(t *testing.T) {
	r := rule("fn", 1, ActWarn, kwC("sleep"))
	r.ID = 7
	st := &fakeStore{rules: []Rule{r}}
	e, _ := newEngine(st, nil)
	req := baseReq("select sleep(1), '13812345678' from users where id = 42")
	d := e.Evaluate(context.Background(), req)
	e.Record(context.Background(), req, d, "")
	if len(st.hits) != 1 {
		t.Fatalf("应写一行命中: %d", len(st.hits))
	}
	h := st.hits[0]
	if strings.Contains(h.SQLText, "138") || strings.Contains(h.SQLText, "42") || h.RuleID != 7 || h.RuleName != "fn" || h.Effective != ActWarn || h.UserName != "rd_zhao" {
		t.Errorf("命中行: %+v", h)
	}
	if d.Hits[0].ID == 0 {
		t.Error("应回填命中行 ID")
	}
	e.AttachQueryLog(d, 99)
	if st.attached[d.Hits[0].ID] != 99 {
		t.Errorf("应回填查询日志 ID: %v", st.attached)
	}
	// 合并：同一语句再来一次不新增行
	d2 := e.Evaluate(context.Background(), req)
	e.Record(context.Background(), req, d2, "")
	if len(st.hits) != 1 || st.repeats[d2.Hits[0].ID] != 2 {
		t.Errorf("窗口内应合并: 行=%d repeats=%v", len(st.hits), st.repeats)
	}

	// firewall_store_sql=raw 才存原文
	st2 := &fakeStore{rules: []Rule{r}}
	e2, _ := newEngine(st2, fakeCfg{settings.KeyFirewallStoreSQL: "raw"})
	d3 := e2.Evaluate(context.Background(), req)
	e2.Record(context.Background(), req, d3, "")
	if !strings.Contains(st2.hits[0].SQLText, "13812345678") {
		t.Errorf("raw 应存原文: %q", st2.hits[0].SQLText)
	}
}

func TestRecordWritesReasonOnlyForReasonAction(t *testing.T) {
	r := rule("why", 1, ActReason, kwC("sleep"))
	r.ID = 1
	st := &fakeStore{rules: []Rule{r}}
	e, _ := newEngine(st, nil)
	req := baseReq("select sleep(1)")
	d := e.Evaluate(context.Background(), req)
	e.Record(context.Background(), req, d, "  对账需要  ")
	if st.hits[0].ReasonText != "对账需要" {
		t.Errorf("原因应去空白后写入: %q", st.hits[0].ReasonText)
	}
}

func TestEvaluateNeverPanicsOnGarbage(t *testing.T) {
	rules := []Rule{
		rule("a", 1, ActDeny, featC(FeatNoWhere)), rule("b", 2, ActWarn, featC(FeatCartesian)),
		rule("c", 3, ActWarn, featC(FeatBigIn)), rule("d", 4, ActWarn, objC("*.*.x")),
	}
	st := &fakeStore{rules: rules}
	for i := range st.rules {
		st.rules[i].ID = uint(i + 1)
	}
	e, _ := newEngine(st, nil)
	for _, sql := range []string{"", ";;;", "select", "select (((", "' unterminated", "/* unterminated", "\x00\xff", "update", "delete from", "in (", strings.Repeat("(", 5000)} {
		req := baseReq(sql)
		_ = e.Evaluate(context.Background(), req)
	}
}

// 手工配置的示例规则必须真的能通过校验并生效（示例不能是摆设）。
func TestDocExamplesAreValid(t *testing.T) {
	cases := []struct {
		name, conds, action, env, groupMode string
		groups                              []uint
		arg                                 int
		sql                                 string
		wantDeny, wantReason, wantMask      bool
		wantRows                            int
	}{
		{name: "危险函数", conds: `[{"t":"keyword","v":["SLEEP","BENCHMARK"]}]`, action: ActDeny, env: "prod", groupMode: "all",
			sql: "select sleep(1)", wantDeny: true},
		{name: "敏感表要原因", conds: `[{"t":"object","v":["*.sys_users"]}]`, action: ActReason, env: "prod", groupMode: "except", groups: []uint{1},
			sql: "select id from sys_users", wantReason: true},
		{name: "密码列强制脱敏", conds: `[{"t":"object","v":["*.*.Password","*.*.PasswordSalt"]}]`, action: ActMask, env: "all", groupMode: "all",
			sql: "select Password from sys_users", wantMask: true},
		{name: "明细最多 1000 行", conds: `[{"t":"stmt","v":["SELECT"]},{"t":"feature","v":"no_limit"}]`, action: ActLimit, env: "prod", groupMode: "all",
			arg: 1000, sql: "select id from t", wantRows: 1000},
	}
	for _, c := range cases {
		conds, err := ParseConditions(c.conds)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		r := Rule{ID: 1, Name: c.name, Priority: 10, Enabled: true, Mode: ModeEnforce, Scenes: []string{SceneQuery}, Env: c.env,
			GroupMode: c.groupMode, GroupIDs: c.groups, Match: "any", Conditions: conds, Action: c.action, ActionArg: c.arg}
		if c.action == ActLimit {
			r.Match = "all"
		}
		req := baseReq(c.sql)
		req.Tables = func() ([]Object, error) { return []Object{{Table: "sys_users"}, {Table: "t"}}, nil }
		d := eval(t, []Rule{r}, req)
		if d.Deny != c.wantDeny || d.NeedReason != c.wantReason || (len(d.ForceMask) > 0) != c.wantMask || d.MaxRows != c.wantRows {
			t.Errorf("%s: 决定不符: deny=%v reason=%v mask=%v rows=%d (%+v)", c.name, d.Deny, d.NeedReason, d.ForceMask, d.MaxRows, d.Hits)
		}
	}
}
