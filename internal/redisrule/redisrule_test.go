// @author: kevinkelly
// @license: Apache Licence
// @file: redisrule_test.go
// @time: 2026/10/28

package redisrule

import (
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"cache:*", "cache:a", true}, {"cache:*", "cache:", true}, {"cache:*", "cache", false}, {"cache:*", "xcache:a", false},
		{"cache:*", "cache:*", true}, // 键里的 * 只是普通字符
		{"a*b*c", "aXXbYYc", true}, {"a*b*c", "abc", true}, {"a*b*c", "aXXbYY", false}, {"*:end", "x:y:end", true}, {"*:end", "x:end:no", false},
		{"user:?", "user:1", true}, {"user:?", "user:12", false}, {"user:?", "user:", false},
		{"k[abc]", "kb", true}, {"k[abc]", "kd", false}, {"k[^abc]", "kd", true}, {"k[^abc]", "ka", false}, {"k[a-c]x", "kbx", true}, {"k[c-a]x", "kbx", true},
		{`a\*b`, "a*b", true}, {`a\*b`, "aXb", false}, {`a\?`, "a?", true}, {"k[0-9]*", "k5-anything", true}, {"k[0-9]*", "kz", false},
		{"cache:*:v?", "cache:a:b:v1", true}, {"cache:*:v?", "cache:a:b:v12", false},
		{"a[", "a[", false}, // 非法模式不匹配任何键
		{"**a", "xxa", true}, {"a**", "a", true},
	}
	for _, c := range cases {
		if got := GlobMatch(c.pat, c.s); got != c.want {
			t.Errorf("GlobMatch(%q, %q) = %v，期望 %v", c.pat, c.s, got, c.want)
		}
	}
	// 不会指数回溯
	start := time.Now()
	GlobMatch(strings.Repeat("a*", 30)+"b", strings.Repeat("a", 200))
	if time.Since(start) > time.Second {
		t.Error("glob 回溯过慢")
	}
}

func TestValidatePattern(t *testing.T) {
	for _, p := range []string{"*", "**", "?", "?*", "[ab]*", "[^a]", ""} {
		if err := ValidatePattern(p); err == nil {
			t.Errorf("%q 只含通配符或为空，应报错", p)
		}
	}
	for _, p := range []string{"cache:*", "a", "*:a", "[a]b", `\*`} {
		if err := ValidatePattern(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	if err := ValidatePattern("a[bc"); err == nil {
		t.Error("方括号未闭合应报错")
	}
	if err := ValidatePattern(strings.Repeat("a", MaxPatternLen+1)); err == nil {
		t.Error("过长应报错")
	}
}

func TestNormalize(t *testing.T) {
	r, err := Normalize(Rule{Name: " 清理 ", Commands: []string{"del, Unlink", "del"}, KeyPatterns: []string{" cache:* ", "cache:*", "tmp:*"}, InstanceIDs: []uint{3, 1, 3}, DBs: []int{2, 0, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "清理" || strings.Join(r.Commands, ",") != "DEL,UNLINK" || len(r.KeyPatterns) != 2 || len(r.InstanceIDs) != 2 || r.InstanceIDs[0] != 1 ||
		len(r.DBs) != 2 || r.DBs[0] != 0 || r.MaxKeys != DefaultMaxKey {
		t.Errorf("规整结果: %+v", r)
	}
	bad := []Rule{
		{Commands: []string{"DEL"}, KeyPatterns: []string{"a*"}},                                    // 无名称
		{Name: "x", KeyPatterns: []string{"a*"}},                                                    // 无命令
		{Name: "x", Commands: []string{"DEL"}},                                                      // 无键模式
		{Name: "x", Commands: []string{"FLUSHALL"}, KeyPatterns: []string{"a*"}},                    // 硬禁止
		{Name: "x", Commands: []string{"flushdb"}, KeyPatterns: []string{"a*"}},                     // 硬禁止（小写）
		{Name: "x", Commands: []string{"DEL", "CONFIG"}, KeyPatterns: []string{"a*"}},               // 其中一个硬禁止
		{Name: "x", Commands: []string{"PUBLISH"}, KeyPatterns: []string{"a*"}},                     // 无键命令
		{Name: "x", Commands: []string{"GET"}, KeyPatterns: []string{"a*"}},                         // 读命令
		{Name: "x", Commands: []string{"NOSUCHCMD"}, KeyPatterns: []string{"a*"}},                   // 未知
		{Name: "x", Commands: []string{"DEL;X"}, KeyPatterns: []string{"a*"}},                       // 非法字符
		{Name: "x", Commands: []string{"DEL"}, KeyPatterns: []string{"*"}},                          // 纯通配符
		{Name: "x", Commands: []string{"DEL"}, KeyPatterns: []string{"a*"}, MaxKeys: -1},            // 键数
		{Name: "x", Commands: []string{"DEL"}, KeyPatterns: []string{"a*"}, MaxKeys: 10001},         // 键数
		{Name: "x", Commands: []string{"DEL"}, KeyPatterns: []string{"a*"}, DBs: []int{-1}},         // db
		{Name: "x", Commands: []string{"DEL"}, KeyPatterns: []string{"a*"}, DBs: []int{1024}},       // db
		{Name: "x", Commands: []string{"DEL"}, KeyPatterns: []string{"a*"}, InstanceIDs: []uint{0}}, // 实例
		{Name: strings.Repeat("名", MaxNameLen+1), Commands: []string{"DEL"}, KeyPatterns: []string{"a*"}},
	}
	for i, b := range bad {
		if _, err := Normalize(b); err == nil {
			t.Errorf("第 %d 个非法规则应报错: %+v", i, b)
		} else {
			var e *Error
			if !errors.As(err, &e) {
				t.Errorf("错误类型: %T", err)
			}
		}
	}
	if _, err := Normalize(Rule{Name: "x", Commands: []string{"FLUSHALL"}, KeyPatterns: []string{"a*"}}); err == nil || !strings.Contains(err.Error(), "硬禁止") {
		t.Errorf("硬禁止的提示: %v", err)
	}
}

func rule(name string, cmds, pats string, mods ...func(*Rule)) Rule {
	r := Rule{Name: name, Enabled: true, Commands: strings.Split(cmds, ","), KeyPatterns: strings.Split(pats, ","), MaxKeys: 100}
	for _, m := range mods {
		m(&r)
	}
	r.ID = uint(len(name))
	return r
}

func eval(rules []Rule, inst uint, db int, line string) Verdict {
	return EvaluateLine(rules, Target{InstanceID: inst, DB: db}, line)
}

func TestEvaluateWhitelistSemantics(t *testing.T) {
	rules := []Rule{rule("清理", "DEL,UNLINK", "cache:*"), rule("过期", "EXPIRE", "session:*,cache:*")}
	// 命令名、键同时命中才放行；命令名大小写不敏感
	for _, line := range []string{`DEL cache:a`, `del cache:a`, `UNLINK cache:a cache:b`, `EXPIRE session:1 60`, `expire cache:x 5`} {
		if v := eval(rules, 1, 0, line); !v.Allowed() {
			t.Errorf("%s 应放行: %+v", line, v)
		}
	}
	rej := map[string]string{
		`DEL other:a`:                "不匹配规则",   // 键不匹配
		`DEL cache:a other:b`:        "other:b", // 多键里有一个不匹配
		`SET cache:a v`:              "没有任何启用的规则允许命令 SET",
		`EXPIRE other:a 60`:          "不匹配规则",
		`DEL cache:*`:                "", // 字面量 * 视为普通字符，仍按键 cache:* 匹配 -> 放行（下面单独断言）
		`FLUSHALL`:                   "硬禁止",
		`PUBLISH c m`:                "不在受支持",
		`GET cache:a`:                "不在受支持",
		`DEL`:                        "语法错误",
		`DEL "cache:a`:               "语法错误",
		`SET cache:a`:                "语法错误",
		`SORT cache:l STORE cache:d`: "硬禁止",
	}
	for line, want := range rej {
		v := eval(rules, 1, 0, line)
		if line == `DEL cache:*` {
			if !v.Allowed() {
				t.Errorf("DEL cache:* 的键按字面量 cache:* 匹配，应放行: %+v", v)
			}
			continue
		}
		if v.Allowed() || !strings.Contains(v.Message, want) {
			t.Errorf("%s 应拒绝且含 %q: %+v", line, want, v)
		}
	}
	// 停用的规则不生效；没有规则全部拒绝
	rules[0].Enabled = false
	if v := eval(rules, 1, 0, `DEL cache:a`); v.Allowed() {
		t.Error("停用的规则不应放行")
	}
	if v := eval(nil, 1, 0, `DEL cache:a`); v.Allowed() || !strings.Contains(v.Message, "没有任何启用的规则") {
		t.Errorf("没有规则应全部拒绝: %+v", v)
	}
	if v := eval([]Rule{{Name: "x", Enabled: false, Commands: []string{"DEL"}, KeyPatterns: []string{"a*"}, MaxKeys: 5}}, 1, 0, `DEL a`); v.Allowed() ||
		!strings.Contains(v.Message, "没有任何启用的规则") {
		t.Errorf("只有停用规则 = 没有规则: %+v", v)
	}
}

func TestEvaluateKeyPositions(t *testing.T) {
	rules := []Rule{rule("缓存", "MSET,MSETNX,RENAME,COPY,SMOVE,BITOP,DEL,SET,HSET", "cache:*")}
	allow := []string{`MSET cache:a 1 cache:b 2`, `RENAME cache:a cache:b`, `COPY cache:a cache:b`, `BITOP OR cache:d cache:s1 cache:s2`, `HSET cache:h f v`}
	for _, l := range allow {
		if v := eval(rules, 1, 0, l); !v.Allowed() {
			t.Errorf("%s 应放行: %+v", l, v)
		}
	}
	deny := map[string]string{
		`MSET cache:a 1 other:b 2`:  "other:b", // MSET 的值 cache:a 不是键，但第二个键不匹配
		`MSET cache:a 1 cache:b`:    "语法错误",    // 参数个数
		`RENAME cache:a other:b`:    "other:b", // RENAME 两个键都要匹配
		`RENAME other:a cache:b`:    "other:a",
		`COPY cache:a other:b`:      "other:b",
		`COPY cache:a cache:b DB 2`: "DB",
		`BITOP OR other:d cache:s1`: "other:d",
		`SET other:a cache:x`:       "other:a", // 值长得像键也不是键，第一个参数才是
	}
	for l, want := range deny {
		v := eval(rules, 1, 0, l)
		if v.Allowed() || !strings.Contains(v.Message, want) {
			t.Errorf("%s 应拒绝且含 %q: %+v", l, want, v)
		}
	}
	// MSET 的“值”不参与匹配
	if v := eval(rules, 1, 0, `MSET cache:a other:value cache:b other:value2`); !v.Allowed() {
		t.Errorf("MSET 的值不是键: %+v", v)
	}
	if v := eval(rules, 1, 0, `SET cache:a other:value`); !v.Allowed() {
		t.Errorf("SET 的值不是键: %+v", v)
	}
	if v := eval(rules, 1, 0, `MSET cache:a 1 cache:b 2`); len(v.Keys) != 2 || v.Keys[0] != "cache:a" || v.Keys[1] != "cache:b" {
		t.Errorf("MSET 的键: %v", v.Keys)
	}
}

func TestEvaluateMaxKeysAndScope(t *testing.T) {
	rules := []Rule{rule("小批量", "DEL", "cache:*", func(r *Rule) { r.MaxKeys = 2 }),
		rule("仅实例2库3", "SET", "cache:*", func(r *Rule) { r.InstanceIDs = []uint{2}; r.DBs = []int{3} })}
	if v := eval(rules, 1, 0, `DEL cache:a cache:b`); !v.Allowed() {
		t.Errorf("2 个键在上限内: %+v", v)
	}
	if v := eval(rules, 1, 0, `DEL cache:a cache:b cache:c`); v.Allowed() || !strings.Contains(v.Message, "超过规则") || !strings.Contains(v.Message, "上限 2") {
		t.Errorf("超出键数上限: %+v", v)
	}
	if v := eval(rules, 2, 3, `SET cache:a v EX 5`); !v.Allowed() {
		t.Errorf("范围内应放行: %+v", v)
	}
	for _, c := range []struct {
		inst uint
		db   int
	}{{1, 3}, {2, 0}, {2, 4}} {
		v := eval(rules, c.inst, c.db, `SET cache:a v EX 5`)
		if v.Allowed() || !strings.Contains(v.Message, "不适用于当前实例") {
			t.Errorf("实例 %d 库 %d 超出范围应拒绝: %+v", c.inst, c.db, v)
		}
	}
	// 规则 A 范围不符、规则 B 命令不含：说明里报范围问题（走得最远）
	if v := eval(rules, 9, 0, `SET cache:a v`); !strings.Contains(v.Message, "不适用于当前实例") {
		t.Errorf("原因应指向范围: %+v", v)
	}
	// 多条规则，后一条放行即可
	rules2 := []Rule{rule("严", "DEL", "cache:*", func(r *Rule) { r.MaxKeys = 1 }), rule("宽", "DEL", "cache:*", func(r *Rule) { r.MaxKeys = 10 })}
	v := eval(rules2, 1, 0, `DEL cache:a cache:b`)
	if !v.Allowed() || v.RuleName != "宽" {
		t.Errorf("应命中宽规则: %+v", v)
	}
}

func TestEvaluateWarnings(t *testing.T) {
	rules := []Rule{rule("写", "SET,SETNX,EXPIRE,PERSIST,RENAME,DEL,SETEX", "cache:*")}
	warn := map[string]string{
		`SET cache:a v`:          "没有设置过期时间",
		`SETNX cache:a v`:        "不能设置过期时间",
		`EXPIRE cache:a 0`:       "立即删除",
		`EXPIRE cache:a -1`:      "立即删除",
		`PERSIST cache:a`:        "永不过期",
		`RENAME cache:a cache:b`: "覆盖",
	}
	for l, want := range warn {
		v := eval(rules, 1, 0, l)
		if v.Level != LevelWarn || v.Status != "警告" || !strings.Contains(v.Message, want) {
			t.Errorf("%s 应警告且含 %q: %+v", l, want, v)
		}
		if !v.Allowed() {
			t.Errorf("警告不拦截: %s", l)
		}
	}
	for _, l := range []string{`SET cache:a v EX 60`, `SET cache:a v PX 100`, `SET cache:a v KEEPTTL`, `SETEX cache:a 60 v`, `EXPIRE cache:a 60`} {
		if v := eval(rules, 1, 0, l); v.Level != LevelOK {
			t.Errorf("%s 不应警告: %+v", l, v)
		}
	}
	many := "DEL"
	for i := 0; i < 25; i++ {
		many += " cache:k" + string(rune('a'+i))
	}
	if v := eval(rules, 1, 0, many); v.Level != LevelWarn {
		t.Errorf("一次删 25 个键应警告: %+v", v)
	}
}

func TestSplitLines(t *testing.T) {
	ls, err := SplitLines("\r\n  SET a 1  \r\n\n\nDEL a\n")
	if err != nil || len(ls) != 2 || ls[0].No != 2 || ls[0].Text != "SET a 1" || ls[1].No != 5 {
		t.Errorf("%+v %v", ls, err)
	}
	if _, err := SplitLines(" \n \n"); err == nil {
		t.Error("空文本应报错")
	}
	var sb strings.Builder
	for i := 0; i < MaxLines+1; i++ {
		sb.WriteString("DEL a\n")
	}
	if _, err := SplitLines(sb.String()); err == nil || !strings.Contains(err.Error(), "200") {
		t.Errorf("超过上限: %v", err)
	}
	sb.Reset()
	for i := 0; i < MaxLines; i++ {
		sb.WriteString("DEL a\n")
	}
	if ls, err := SplitLines(sb.String()); err != nil || len(ls) != MaxLines {
		t.Errorf("正好 200 条应通过: %d %v", len(ls), err)
	}
}

// ---- 管理服务 ----

type sink struct{ got []*model.AuditEntry }

func (s *sink) WriteLifecycleAudit(e *model.AuditEntry) error { s.got = append(s.got, e); return nil }

var admin = audit.Actor{ID: 1, Username: "root", Display: "管理员", IsSuperuser: true}

func apiCode(err error) string {
	var e *APIError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestServiceLifecycleAndAudit(t *testing.T) {
	repo := NewMemRepo()
	au := &sink{}
	s := NewService(repo, au)
	in := Rule{Name: "清理缓存", Enabled: true, Commands: []string{"del"}, KeyPatterns: []string{"cache:*"}, Description: "d"}

	if _, err := s.Create(admin, in, false); apiCode(err) != "confirm_required" {
		t.Errorf("未确认: %v", err)
	}
	if len(au.got) != 0 {
		t.Error("未确认不应写审计")
	}
	r, err := s.Create(admin, in, true)
	if err != nil || r.ID == 0 || r.CreatedBy != "root" || r.MaxKeys != DefaultMaxKey || r.Commands[0] != "DEL" {
		t.Fatalf("创建: %+v %v", r, err)
	}
	if _, err := s.Create(admin, in, true); apiCode(err) != "name_taken" {
		t.Errorf("同名: %v", err)
	}
	bad := in
	bad.Name, bad.Commands = "x", []string{"FLUSHALL"}
	if _, err := s.Create(admin, bad, true); apiCode(err) != "bad_rule" || !strings.Contains(err.Error(), "硬禁止") {
		t.Errorf("硬禁止: %v", err)
	}

	up := in
	up.KeyPatterns, up.MaxKeys = []string{"cache:*", "tmp:*"}, 50
	if _, err := s.Update(admin, r.ID, up, false); apiCode(err) != "confirm_required" {
		t.Errorf("更新未确认: %v", err)
	}
	r2, err := s.Update(admin, r.ID, up, true)
	if err != nil || r2.MaxKeys != 50 || r2.CreatedBy != "root" {
		t.Fatalf("更新: %+v %v", r2, err)
	}
	if _, err := s.Update(admin, 999, up, true); apiCode(err) != "not_found" {
		t.Errorf("更新不存在的: %v", err)
	}
	if _, err := s.SetEnabled(admin, r.ID, false, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Get(r.ID); got.Enabled {
		t.Error("应已停用")
	}
	if err := s.Delete(admin, r.ID, false); apiCode(err) != "confirm_required" {
		t.Errorf("删除未确认: %v", err)
	}
	if err := s.Delete(admin, r.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(admin, r.ID, true); apiCode(err) != "not_found" {
		t.Errorf("重复删除: %v", err)
	}

	// 审计：新增 / 修改 / 停用 / 删除各一条，修改带前后差异
	var actions []string
	for _, e := range au.got {
		actions = append(actions, e.Action)
		if e.UserName != "root" {
			t.Errorf("审计用户: %s", e.UserName)
		}
	}
	if strings.Join(actions, ",") != "Redis上线规则-新增,Redis上线规则-修改,Redis上线规则-停用,Redis上线规则-删除" {
		t.Fatalf("审计动作: %v", actions)
	}
	if x := au.got[1].ExtraInfo; !strings.Contains(x, "键模式：cache:* → cache:* | tmp:*") || !strings.Contains(x, "键数上限：100 → 50") || strings.Contains(x, "名称：") {
		t.Errorf("修改审计应只含变化的字段: %s", x)
	}
	if x := au.got[2].ExtraInfo; !strings.Contains(x, "启用 是 → 否") {
		t.Errorf("停用审计: %s", x)
	}
	if x := au.got[3].ExtraInfo; !strings.Contains(x, "删除前") || !strings.Contains(x, "命令=DEL") {
		t.Errorf("删除审计应带删除前内容: %s", x)
	}
	// 审计动作能被分类到 redisrule.edit（见 internal/audit/ext.go）
	if c := audit.Classify("Redis上线规则-新增", "x"); c.Code != "redisrule.edit" {
		t.Errorf("审计分类: %+v", c)
	}
}

func TestServiceTableMissing(t *testing.T) {
	repo := NewMemRepo()
	repo.NotReady = true
	s := NewService(repo, nil)
	if s.Ready() {
		t.Error("表不存在 Ready 应为 false")
	}
	if l, err := s.List(); err != nil || len(l) != 0 {
		t.Errorf("表不存在 List 应为空: %v %v", l, err)
	}
	in := Rule{Name: "x", Commands: []string{"DEL"}, KeyPatterns: []string{"a*"}}
	if _, err := s.Create(admin, in, true); apiCode(err) != "table_missing" {
		t.Errorf("建规则: %v", err)
	}
	if err := s.Delete(admin, 1, true); apiCode(err) != "table_missing" {
		t.Errorf("删规则: %v", err)
	}
	// SQL 上线读取：表不存在 = ready=false，没有规则
	rules, ready, err := SourceOf(repo).Rules(nil)
	if err != nil || ready || len(rules) != 0 {
		t.Errorf("Source: %v %v %v", rules, ready, err)
	}
}

func TestServiceRuleLimitAndTest(t *testing.T) {
	s := NewService(NewMemRepo(), nil)
	for i := 0; i < MaxRules; i++ {
		if _, err := s.Create(admin, Rule{Name: "r" + string(rune(0x4e00+i)), Commands: []string{"DEL"}, KeyPatterns: []string{"a*"}}, true); err != nil {
			t.Fatalf("第 %d 条: %v", i, err)
		}
	}
	if _, err := s.Create(admin, Rule{Name: "多余", Commands: []string{"DEL"}, KeyPatterns: []string{"a*"}}, true); apiCode(err) != "too_many" {
		t.Errorf("超过规则总数: %v", err)
	}
	s2 := NewService(NewMemRepo(rule("清理", "DEL", "cache:*")), nil)
	out, err := s2.TestLines(1, 0, "DEL cache:a\nFLUSHALL\n\nDEL other:b")
	if err != nil || len(out) != 3 || !out[0].Allowed() || out[1].Allowed() || out[2].Allowed() || out[2].Line != 4 {
		t.Errorf("TestLines: %+v %v", out, err)
	}
}

func TestTemplatesAreValid(t *testing.T) {
	ts := Templates()
	if len(ts) < 3 {
		t.Fatal("至少要有 3 个模板")
	}
	keys := map[string]bool{}
	for _, tp := range ts {
		if keys[tp.Key] {
			t.Errorf("模板键重复 %s", tp.Key)
		}
		keys[tp.Key] = true
		if _, err := Normalize(tp.Rule); err != nil {
			t.Errorf("模板 %s 不合法: %v", tp.Key, err)
		}
	}
}

func TestRowRoundTrip(t *testing.T) {
	r := Rule{ID: 3, Name: "n", Enabled: true, Commands: []string{"DEL", "SET"}, KeyPatterns: []string{"a:*", "b|c:*"}, InstanceIDs: []uint{4, 9}, DBs: []int{0, 5},
		MaxKeys: 7, RecordBefore: true, Description: "d", Remark: "r"}
	got := fromRow(toRow(r))
	if got.ID != 3 || strings.Join(got.Commands, ",") != "DEL,SET" || strings.Join(got.KeyPatterns, "#") != "a:*#b|c:*" || len(got.InstanceIDs) != 2 || got.InstanceIDs[1] != 9 ||
		len(got.DBs) != 2 || got.DBs[1] != 5 || got.MaxKeys != 7 || !got.RecordBefore || !got.Enabled {
		t.Errorf("往返: %+v", got)
	}
	if fromRow(row{Commands: "DEL", KeyPatterns: "[]"}).MaxKeys != DefaultMaxKey {
		t.Error("库里键数上限为 0 应取默认值")
	}
}
