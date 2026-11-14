// @author: kevinkelly
// @license: Apache Licence
// @file: firewalladmin_test.go
// @time: 2026/10/28

package firewalladmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/firewall"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
	"dbtool/internal/settings"
)

// ---- 假依赖 ----

type fakeStore struct {
	missing []string
	rules   []model.FirewallRule
	excs    []model.FirewallException
	hits    []model.FirewallHit
	groups  []model.AuthGroup
	ugroups map[string][]uint
	logs    []model.QueryLog
	wfs     []WorkflowSQL
	days    []DayHits
	summary *HitSummary
	audits  []model.AuditEntry
	cfg     map[string]string // 与 servertest.Repo.Config 共用：模拟“写库后立刻读得到”
	nextID  uint
	failSet error
}

func (f *fakeStore) Missing() []string { return f.missing }
func (f *fakeStore) ListRules() ([]model.FirewallRule, error) {
	out := append([]model.FirewallRule(nil), f.rules...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}
func (f *fakeStore) GetRule(id uint) (*model.FirewallRule, error) {
	for i := range f.rules {
		if f.rules[i].ID == id {
			c := f.rules[i]
			return &c, nil
		}
	}
	return nil, nil
}
func (f *fakeStore) audit(a *model.AuditEntry) {
	if a != nil {
		f.audits = append(f.audits, *a)
	}
}
func (f *fakeStore) CreateRule(m *model.FirewallRule, a *model.AuditEntry) error {
	for _, r := range f.rules {
		if strings.EqualFold(r.Name, m.Name) {
			return ErrDuplicateName
		}
	}
	f.nextID++
	m.ID = 100 + f.nextID
	f.rules = append(f.rules, *m)
	f.audit(a)
	return nil
}
func (f *fakeStore) UpdateRule(m *model.FirewallRule, a *model.AuditEntry) error {
	for i := range f.rules {
		if f.rules[i].ID == m.ID {
			f.rules[i] = *m
			f.audit(a)
			return nil
		}
	}
	return errors.New("no such rule")
}
func (f *fakeStore) PatchRule(id uint, fields map[string]any, a *model.AuditEntry) error {
	for i := range f.rules {
		if f.rules[i].ID != id {
			continue
		}
		for k, v := range fields {
			switch k {
			case "enabled":
				f.rules[i].Enabled = v.(bool)
			case "mode":
				f.rules[i].Mode = v.(string)
			case "observe_since":
				t := v.(time.Time)
				f.rules[i].ObserveSince = &t
			case "updated_by":
				f.rules[i].UpdatedBy = v.(string)
			}
		}
		f.audit(a)
		return nil
	}
	return errors.New("no such rule")
}
func (f *fakeStore) DeleteRule(id uint, a *model.AuditEntry) (int64, error) {
	var rules []model.FirewallRule
	for _, r := range f.rules {
		if r.ID != id {
			rules = append(rules, r)
		}
	}
	f.rules = rules
	var n int64
	var excs []model.FirewallException
	for _, e := range f.excs {
		if e.RuleID == id {
			n++
		} else {
			excs = append(excs, e)
		}
	}
	f.excs = excs
	f.audit(a)
	return n, nil
}
func (f *fakeStore) DayHits(time.Time) ([]DayHits, error) { return f.days, nil }
func (f *fakeStore) HitSummary(time.Time) (*HitSummary, error) {
	if f.summary == nil {
		return &HitSummary{}, nil
	}
	return f.summary, nil
}
func (f *fakeStore) ListHits(q HitFilter) ([]model.FirewallHit, int64, error) {
	var all []model.FirewallHit
	for _, h := range f.hits {
		switch {
		case q.RuleID != 0 && h.RuleID != q.RuleID, q.User != "" && h.UserName != q.User, q.Result != "" && h.Effective != q.Result,
			q.Scene != "" && h.Scene != q.Scene, q.Q != "" && !strings.Contains(h.SQLText, q.Q):
			continue
		}
		all = append(all, h)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].LastAt.After(all[j].LastAt) })
	total := int64(len(all))
	lo := min(q.Offset, len(all))
	hi := min(lo+q.Limit, len(all))
	return all[lo:hi], total, nil
}
func (f *fakeStore) GetHit(id uint) (*model.FirewallHit, error) {
	for i := range f.hits {
		if f.hits[i].ID == id {
			c := f.hits[i]
			return &c, nil
		}
	}
	return nil, nil
}
func (f *fakeStore) ListExceptions(since time.Time) ([]model.FirewallException, error) {
	var out []model.FirewallException
	for _, e := range f.excs {
		if e.ExpiresAt.After(since) {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeStore) GetException(id uint) (*model.FirewallException, error) {
	for i := range f.excs {
		if f.excs[i].ID == id {
			c := f.excs[i]
			return &c, nil
		}
	}
	return nil, nil
}
func (f *fakeStore) CreateException(m *model.FirewallException, a *model.AuditEntry) error {
	f.nextID++
	m.ID = 500 + f.nextID
	f.excs = append(f.excs, *m)
	f.audit(a)
	return nil
}
func (f *fakeStore) DeleteException(id uint, a *model.AuditEntry) error {
	var out []model.FirewallException
	for _, e := range f.excs {
		if e.ID != id {
			out = append(out, e)
		}
	}
	f.excs = out
	f.audit(a)
	return nil
}
func (f *fakeStore) SetConfig(item, value, _ string, a *model.AuditEntry) error {
	if f.failSet != nil {
		return f.failSet
	}
	f.cfg[item] = value
	f.audit(a)
	return nil
}
func (f *fakeStore) Groups() ([]model.AuthGroup, error) { return f.groups, nil }
func (f *fakeStore) UserGroups(names []string) (map[string][]uint, error) {
	out := map[string][]uint{}
	for _, n := range names {
		if g, ok := f.ugroups[n]; ok {
			out[n] = g
		}
	}
	return out, nil
}
func (f *fakeStore) QueryLogs(_ time.Time, limit int) ([]model.QueryLog, error) {
	return f.logs[:min(limit, len(f.logs))], nil
}
func (f *fakeStore) Workflows(_ time.Time, limit int) ([]WorkflowSQL, error) {
	return f.wfs[:min(limit, len(f.wfs))], nil
}

type inv struct{ n int }

func (i *inv) Invalidate() { i.n++ }

type env struct {
	st  *fakeStore
	svc *Service
	inv *inv
	te  *servertest.Env
	now time.Time
}

var actor = audit.Actor{ID: 1, Username: "root", Display: "管理员"}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{st: &fakeStore{groups: []model.AuthGroup{{ID: 3, Name: "DBA"}, {ID: 5, Name: "研发"}}, ugroups: map[string][]uint{}}, inv: &inv{},
		now: time.Date(2026, 10, 12, 10, 0, 0, 0, time.Local)}
	repo := &servertest.Repo{Instances: []model.Instance{
		{ID: 1, InstanceName: "prod-mysql-01", DBType: "mysql", Tags: []model.InstanceTag{{TagCode: "prod", TagName: "生产", Active: true}}},
		{ID: 2, InstanceName: "test-mysql-01", DBType: "mysql", Tags: []model.InstanceTag{{TagCode: "test", TagName: "测试", Active: true}}},
	}, Config: map[string]string{}}
	e.st.cfg = repo.Config
	def := servertest.New(t, servertest.Options{})
	repo.Users, repo.Perms = def.Repo.Users, def.Repo.Perms
	e.te = servertest.New(t, servertest.Options{Repo: repo, Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) {
			e.svc = NewService(s.Repo(), e.st, e.inv)
			register(s, rt, e.svc)
		}}})
	e.svc.now = func() time.Time { return e.now }
	return e
}

func ptr[T any](v T) *T { return &v }

func denyNoWhere() RuleInput {
	return RuleInput{Name: "禁止不带 WHERE 的 UPDATE / DELETE", Description: "误操作", Priority: 10, Mode: "enforce", Scenes: []string{"workflow"}, Env: "prod",
		Match: "all", Conditions: []firewall.Condition{{T: "stmt", V: []string{"update", "delete"}}, {T: "feature", V: "no_where"}},
		Action: "deny", Message: "请补充 WHERE"}
}

func (e *env) mustCreate(t *testing.T, in RuleInput) *RuleView {
	t.Helper()
	v, err := e.svc.CreateRule(actor, in)
	if err != nil {
		t.Fatalf("创建规则失败: %v", err)
	}
	return v
}

func lastAudit(t *testing.T, st *fakeStore) model.AuditEntry {
	t.Helper()
	if len(st.audits) == 0 {
		t.Fatal("应当写了审计")
	}
	return st.audits[len(st.audits)-1]
}

func errCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ---- 规则 ----

func TestCreateRuleNormalizesAndAudits(t *testing.T) {
	e := newEnv(t)
	v := e.mustCreate(t, denyNoWhere())
	if v.ID == 0 || v.Mode != "enforce" || !v.Enabled || v.Invalid != "" {
		t.Fatalf("视图 = %+v", v)
	}
	stmts := strList(v.Conditions[0].V)
	if stmts[0] != "UPDATE" || stmts[1] != "DELETE" {
		t.Errorf("语句类型应被规整为大写: %v", stmts)
	}
	if e.inv.n != 1 {
		t.Errorf("保存后快照应失效: %d", e.inv.n)
	}
	a := lastAudit(t, e.st)
	if a.Action != "SQL 防火墙规则变更" || a.UserName != "root" || !strings.Contains(a.ExtraInfo, "新建规则") || !strings.HasPrefix(a.ExtraInfo, audit.FirewallRiskyMark) {
		t.Errorf("审计 = %+v（直接以生效模式新建应标高危）", a)
	}
	if got := audit.Classify(a.Action, a.ExtraInfo); got.Category != audit.CatSys || got.Module != "SQL 防火墙" || !got.Risky {
		t.Errorf("审计分类 = %+v", got)
	}
}

func TestCreateRuleDefaultsToObserve(t *testing.T) {
	e := newEnv(t)
	in := denyNoWhere()
	in.Mode = ""
	v := e.mustCreate(t, in)
	if v.Mode != "observe" || v.ObserveSince == "" || v.ObserveDays != 1 {
		t.Errorf("缺省应为观察模式: %+v", v)
	}
	if a := lastAudit(t, e.st); strings.HasPrefix(a.ExtraInfo, audit.FirewallRiskyMark) {
		t.Errorf("观察模式新建不应标高危: %s", a.ExtraInfo)
	}
}

func TestCreateRuleRejectsInvalid(t *testing.T) {
	e := newEnv(t)
	bad := map[string]func(*RuleInput){
		"空名称":      func(r *RuleInput) { r.Name = " " },
		"没有条件":     func(r *RuleInput) { r.Conditions = nil },
		"无效正则":     func(r *RuleInput) { r.Conditions = []firewall.Condition{{T: "regex", V: "(unclosed"}} },
		"不认识的动作":   func(r *RuleInput) { r.Action = "escalate" },
		"原因只适用查询":  func(r *RuleInput) { r.Action = "reason" },
		"限行缺行数":    func(r *RuleInput) { r.Action, r.Scenes = "limit", []string{"query"} },
		"脱敏缺列":     func(r *RuleInput) { r.Action, r.Scenes = "mask", []string{"query"} },
		"权限组不存在":   func(r *RuleInput) { r.GroupMode, r.GroupIDs = "only", []uint{99} },
		"实例不存在":    func(r *RuleInput) { r.InstanceIDs = []uint{99} },
		"only 没选组": func(r *RuleInput) { r.GroupMode = "only" },
	}
	for name, mut := range bad {
		in := denyNoWhere()
		mut(&in)
		_, err := e.svc.CreateRule(actor, in)
		if errCode(err) != "bad_request" {
			t.Errorf("%s: 应 400，得到 %v", name, err)
		}
	}
	if len(e.st.rules) != 0 || len(e.st.audits) != 0 || e.inv.n != 0 {
		t.Errorf("被拒绝的请求不应写任何东西: rules=%d audits=%d inv=%d", len(e.st.rules), len(e.st.audits), e.inv.n)
	}
}

func TestCreateRuleDuplicateNameAndCap(t *testing.T) {
	e := newEnv(t)
	e.mustCreate(t, denyNoWhere())
	dup := denyNoWhere()
	dup.Name = "  禁止不带 where 的 update / delete  "
	if _, err := e.svc.CreateRule(actor, dup); errCode(err) != "duplicate_name" {
		t.Errorf("同名（忽略大小写与空白）应冲突: %v", err)
	}
	for i := len(e.st.rules); i < firewall.MaxRules; i++ {
		e.st.rules = append(e.st.rules, model.FirewallRule{ID: uint(1000 + i), Name: fmt.Sprintf("r%d", i)})
	}
	in := denyNoWhere()
	in.Name = "再来一条"
	if _, err := e.svc.CreateRule(actor, in); errCode(err) != "bad_request" || !strings.Contains(err.Error(), "上限") {
		t.Errorf("超过规则数上限: %v", err)
	}
}

func TestWritesNeedTables(t *testing.T) {
	e := newEnv(t)
	e.st.missing = []string{"dbt_firewall_rule"}
	if _, err := e.svc.CreateRule(actor, denyNoWhere()); errCode(err) != "not_ready" || !strings.Contains(err.Error(), "240_firewall.sql") {
		t.Errorf("没建表: %v", err)
	}
	ov, err := e.svc.Overview()
	if err != nil || ov.Ready || len(ov.Missing) != 1 {
		t.Errorf("概览应提示没建表: %+v %v", ov, err)
	}
	l, err := e.svc.Rules()
	if err != nil || l.Ready || len(l.Rules) != 0 {
		t.Errorf("规则列表: %+v %v", l, err)
	}
}

func TestUpdateRuleDiffAuditAndObserveSince(t *testing.T) {
	e := newEnv(t)
	v := e.mustCreate(t, denyNoWhere())
	n := len(e.st.audits)
	// 没有变化：不写库、不记审计
	same := denyNoWhere()
	if _, err := e.svc.UpdateRule(actor, v.ID, same); err != nil {
		t.Fatal(err)
	}
	if len(e.st.audits) != n {
		t.Error("没有变化不应记审计")
	}
	// 改优先级与动作文案，观察 ↔ 生效
	upd := denyNoWhere()
	upd.Priority, upd.Mode, upd.Message = 5, "observe", "新文案"
	got, err := e.svc.UpdateRule(actor, v.ID, upd)
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != 5 || got.Mode != "observe" || got.ObserveSince == "" {
		t.Errorf("更新后 = %+v", got)
	}
	a := lastAudit(t, e.st)
	if !strings.Contains(a.ExtraInfo, "优先级 10 → 5") || !strings.Contains(a.ExtraInfo, "模式 生效 → 观察") || strings.HasPrefix(a.ExtraInfo, audit.FirewallRiskyMark) {
		t.Errorf("差异审计 = %s", a.ExtraInfo)
	}
	// 改回生效：高危
	upd.Mode = "enforce"
	if _, err := e.svc.UpdateRule(actor, v.ID, upd); err != nil {
		t.Fatal(err)
	}
	if a := lastAudit(t, e.st); !strings.HasPrefix(a.ExtraInfo, audit.FirewallRiskyMark) {
		t.Errorf("由观察切到生效应标高危: %s", a.ExtraInfo)
	}
	if _, err := e.svc.UpdateRule(actor, 9999, upd); errCode(err) != "not_found" {
		t.Errorf("不存在的规则: %v", err)
	}
	// 与另一条同名
	other := denyNoWhere()
	other.Name = "另一条"
	e.mustCreate(t, other)
	upd.Name = "另一条"
	if _, err := e.svc.UpdateRule(actor, v.ID, upd); errCode(err) != "duplicate_name" {
		t.Errorf("改成别人的名字: %v", err)
	}
}

func TestDeleteRuleRemovesExceptions(t *testing.T) {
	e := newEnv(t)
	v := e.mustCreate(t, denyNoWhere())
	e.st.excs = []model.FirewallException{{ID: 1, RuleID: v.ID}, {ID: 2, RuleID: v.ID}, {ID: 3, RuleID: 777}}
	n, err := e.svc.DeleteRule(actor, v.ID)
	if err != nil || n != 2 || len(e.st.rules) != 0 || len(e.st.excs) != 1 {
		t.Fatalf("n=%d err=%v rules=%d excs=%d", n, err, len(e.st.rules), len(e.st.excs))
	}
	if a := lastAudit(t, e.st); !strings.Contains(a.ExtraInfo, "删除规则") {
		t.Errorf("审计 = %s", a.ExtraInfo)
	}
	if _, err := e.svc.DeleteRule(actor, v.ID); errCode(err) != "not_found" {
		t.Errorf("重复删除: %v", err)
	}
}

func TestToggleRule(t *testing.T) {
	e := newEnv(t)
	v := e.mustCreate(t, denyNoWhere())
	got, err := e.svc.ToggleRule(actor, v.ID, false)
	if err != nil || got.Enabled {
		t.Fatalf("停用: %+v %v", got, err)
	}
	n := len(e.st.audits)
	if _, err := e.svc.ToggleRule(actor, v.ID, false); err != nil || len(e.st.audits) != n {
		t.Error("重复停用应幂等且不记审计")
	}
	// 手工改坏的规则（条件为空）：不允许启用，也不允许切到生效
	e.st.rules[0].Conditions = "[]"
	if _, err := e.svc.ToggleRule(actor, v.ID, true); errCode(err) != "bad_request" {
		t.Errorf("无效规则不能启用: %v", err)
	}
	if _, err := e.svc.SetRuleMode(actor, v.ID, "enforce"); errCode(err) != "bad_request" {
		t.Errorf("无效规则不能切到生效: %v", err)
	}
	list, _ := e.svc.Rules()
	if list.Rules[0].Invalid == "" {
		t.Error("列表里应标出无效规则")
	}
}

func TestSetRuleModeWarnsWithoutObservation(t *testing.T) {
	e := newEnv(t)
	in := denyNoWhere()
	in.Mode = "observe"
	v := e.mustCreate(t, in)
	res, err := e.svc.SetRuleMode(actor, v.ID, "enforce")
	if err != nil || res.Rule.Mode != "enforce" || res.Observed != 0 || res.Warning == "" {
		t.Fatalf("没有观察数据: %+v %v", res, err)
	}
	if a := lastAudit(t, e.st); !strings.HasPrefix(a.ExtraInfo, audit.FirewallRiskyMark) || !strings.Contains(a.ExtraInfo, "观察切到生效") {
		t.Errorf("审计 = %s", a.ExtraInfo)
	}
	// 回到观察，有命中数据后再切：没有提示
	if _, err := e.svc.SetRuleMode(actor, v.ID, "observe"); err != nil {
		t.Fatal(err)
	}
	e.st.days = []DayHits{{RuleID: v.ID, Day: e.now.Format("2006-01-02"), N: 12}}
	res, err = e.svc.SetRuleMode(actor, v.ID, "enforce")
	if err != nil || res.Observed != 12 || res.Warning != "" {
		t.Errorf("有观察数据: %+v %v", res, err)
	}
	if _, err := e.svc.SetRuleMode(actor, v.ID, "bogus"); errCode(err) != "bad_request" {
		t.Errorf("非法模式: %v", err)
	}
}

func TestRulesListFoldsHitsIntoSpark(t *testing.T) {
	e := newEnv(t)
	v := e.mustCreate(t, denyNoWhere())
	e.st.days = []DayHits{
		{RuleID: v.ID, Day: "2026-10-12", N: 4}, {RuleID: v.ID, Day: "2026-10-10", N: 2},
		{RuleID: v.ID, Day: "2026-10-01", N: 99}, // 超出 7 天窗口
	}
	list, err := e.svc.Rules()
	if err != nil || len(list.Rules) != 1 {
		t.Fatal(list, err)
	}
	r := list.Rules[0]
	if r.Hits7 != 6 || len(r.Spark) != 7 || r.Spark[6] != 4 || r.Spark[4] != 2 {
		t.Errorf("近 7 天 = %d %v", r.Hits7, r.Spark)
	}
}

func TestOverviewCounts(t *testing.T) {
	e := newEnv(t)
	a := e.mustCreate(t, denyNoWhere())
	obs := denyNoWhere()
	obs.Name, obs.Mode = "观察中的", "observe"
	b := e.mustCreate(t, obs)
	old := denyNoWhere()
	old.Name = "很久没命中的"
	c := e.mustCreate(t, old)
	for i := range e.st.rules {
		if e.st.rules[i].ID == c.ID {
			e.st.rules[i].CreatedAt = e.now.AddDate(0, -2, 0)
		}
	}
	e.st.days = []DayHits{{RuleID: a.ID, Day: "2026-10-12", N: 3}}
	e.st.summary = &HitSummary{Users: 2, Rules: 2, Counts: []HitCount{
		{Effective: "deny", N: 3}, {Effective: "reason", N: 1}, {Effective: "limit", N: 2}, {Effective: "warn", N: 4},
		{Effective: "observe", WouldAction: "deny", N: 5}, {Effective: "observe", WouldAction: "warn", N: 1},
	}}
	ov, err := e.svc.Overview()
	if err != nil {
		t.Fatal(err)
	}
	if !ov.Ready || !ov.Enabled || ov.Mode != "enforce" || ov.RulesTotal != 3 || ov.RulesEnabled != 3 || ov.RulesObserving != 1 || ov.RulesIdle != 1 {
		t.Errorf("规则统计 = %+v", ov)
	}
	if ov.HitsToday != 16 || ov.BlockedToday != 3 || ov.ReasonToday != 1 || ov.TightenToday != 2 || ov.WarnToday != 4 || ov.ObserveToday != 6 || ov.WouldDenyToday != 5 || ov.UsersToday != 2 {
		t.Errorf("命中统计 = %+v", ov)
	}
	_ = b
}

// ---- 全局模式与总开关 ----

func TestSetModeAndSwitch(t *testing.T) {
	e := newEnv(t)
	res, err := e.svc.SetMode(actor, "observe")
	if err != nil || !res.Changed || res.Mode != "observe" || e.st.cfg[settings.KeyFirewallMode] != "observe" {
		t.Fatalf("切到观察: %+v %v cfg=%v", res, err, e.st.cfg)
	}
	if a := lastAudit(t, e.st); a.Action != "SQL 防火墙模式" || !strings.HasPrefix(a.ExtraInfo, audit.FirewallRiskyMark) {
		t.Errorf("全局切到观察应标高危: %+v", a)
	}
	n := len(e.st.audits)
	if r, _ := e.svc.SetMode(actor, "observe"); r.Changed || len(e.st.audits) != n {
		t.Error("重复设置应幂等")
	}
	if _, err := e.svc.SetMode(actor, "x"); errCode(err) != "bad_request" {
		t.Errorf("非法模式: %v", err)
	}
	if r, _ := e.svc.SetMode(actor, "enforce"); !r.Changed || e.st.cfg[settings.KeyFirewallMode] != "enforce" {
		t.Errorf("切回拦截: %+v", r)
	}
	if a := lastAudit(t, e.st); strings.HasPrefix(a.ExtraInfo, audit.FirewallRiskyMark) {
		t.Error("切回拦截不是高危")
	}

	// 总开关：关闭必须写原因
	if _, err := e.svc.SetSwitch(actor, false, "短"); errCode(err) != "bad_request" {
		t.Errorf("关闭缺原因: %v", err)
	}
	if e.st.cfg[settings.KeyFirewallEnabled] != "" {
		t.Error("被拒绝不应写配置")
	}
	r, err := e.svc.SetSwitch(actor, false, "排查误拦截，临时关闭")
	if err != nil || !r.Changed || r.Enabled || e.st.cfg[settings.KeyFirewallEnabled] != "false" {
		t.Fatalf("关闭: %+v %v", r, err)
	}
	a := lastAudit(t, e.st)
	if a.Action != "SQL 防火墙总开关" || !strings.Contains(a.ExtraInfo, "排查误拦截") || !audit.Classify(a.Action, a.ExtraInfo).Risky {
		t.Errorf("审计 = %+v", a)
	}
	if r, _ := e.svc.SetSwitch(actor, true, ""); !r.Changed || !r.Enabled || e.st.cfg[settings.KeyFirewallEnabled] != "true" {
		t.Errorf("重新开启: %+v", r)
	}
	if a := lastAudit(t, e.st); audit.Classify(a.Action, a.ExtraInfo).Risky {
		t.Error("开启不是高危")
	}
	e.st.failSet = errors.New("db down")
	if _, err := e.svc.SetSwitch(actor, false, "再关一次试试"); err == nil {
		t.Error("写库失败应返回错误")
	}
}

// ---- 命中记录 ----

func TestHitsFilterAndParse(t *testing.T) {
	e := newEnv(t)
	v := e.mustCreate(t, denyNoWhere())
	base := e.now
	for i := 0; i < 25; i++ {
		h := model.FirewallHit{ID: uint(i + 1), CreatedAt: base, LastAt: base.Add(time.Duration(i) * time.Minute), RuleID: v.ID, RuleName: "旧名字",
			Scene: "workflow", UserName: "rd_zhao", Effective: "deny", SQLText: "DELETE FROM t", RepeatCnt: 1,
			Matched: `{"matched":["语句类型是 DELETE"],"unknown":["预估扫描行数：未评估"],"downgraded":""}`}
		if i%5 == 0 {
			h.Effective, h.WouldAction, h.UserName = "observe", "deny", "qa_sun"
		}
		e.st.hits = append(e.st.hits, h)
	}
	e.st.hits = append(e.st.hits, model.FirewallHit{ID: 99, CreatedAt: base, LastAt: base.Add(time.Hour), RuleID: 4040, RuleName: "已删除规则",
		Scene: "query", UserName: "rd_li", Effective: "warn", SQLText: "select 1"})
	l, err := e.svc.Hits(HitQuery{})
	if err != nil || l.Total != 26 || len(l.Items) != 20 || l.Page != 1 || l.Size != 20 {
		t.Fatalf("默认分页: %+v %v", l, err)
	}
	if l.Items[0].ID != 99 || l.Items[0].RuleExists || l.Items[0].RuleName != "已删除规则" {
		t.Errorf("新的在前，已删除的规则仍显示冗余名: %+v", l.Items[0])
	}
	it := l.Items[1]
	if !it.RuleExists || it.RuleName != v.Name || len(it.Matched) != 1 || len(it.Unknown) != 1 || it.Message != "请补充 WHERE" {
		t.Errorf("解析与规则关联: %+v", it)
	}
	if p2, _ := e.svc.Hits(HitQuery{Page: 2}); len(p2.Items) != 6 {
		t.Errorf("第二页 = %d", len(p2.Items))
	}
	if r, _ := e.svc.Hits(HitQuery{Result: "observe"}); r.Total != 5 {
		t.Errorf("按结果筛选 = %d", r.Total)
	}
	if r, _ := e.svc.Hits(HitQuery{User: "rd_li"}); r.Total != 1 {
		t.Errorf("按用户筛选 = %d", r.Total)
	}
	if r, _ := e.svc.Hits(HitQuery{Size: 5000}); r.Size != 100 {
		t.Errorf("每页上限 = %d", r.Size)
	}
	if _, err := e.svc.Hits(HitQuery{Result: "boom"}); errCode(err) != "bad_request" {
		t.Errorf("非法结果类型: %v", err)
	}
	if _, err := e.svc.Hits(HitQuery{Scene: "boom"}); errCode(err) != "bad_request" {
		t.Errorf("非法场景: %v", err)
	}
	if d, err := e.svc.Hit(3); err != nil || d.ID != 3 {
		t.Errorf("详情: %+v %v", d, err)
	}
	if _, err := e.svc.Hit(12345); errCode(err) != "not_found" {
		t.Errorf("不存在: %v", err)
	}
}

// ---- 例外 ----

func TestExceptionValidationAndLifecycle(t *testing.T) {
	e := newEnv(t)
	v := e.mustCreate(t, denyNoWhere())
	ok := ExceptionInput{RuleID: v.ID, SubjectKind: "user", SubjectValue: " rd_li ", Reason: "月度对账报表，已评估过", Days: 30}
	bad := map[string]func(*ExceptionInput){
		"原因太短":    func(x *ExceptionInput) { x.Reason = "ab" },
		"没有原因":    func(x *ExceptionInput) { x.Reason = "  " },
		"没有期限":    func(x *ExceptionInput) { x.Days = 0 },
		"期限过长":    func(x *ExceptionInput) { x.Days = 366 },
		"规则不存在":   func(x *ExceptionInput) { x.RuleID = 9999 },
		"对象类型未知":  func(x *ExceptionInput) { x.SubjectKind = "role" },
		"用户名为空":   func(x *ExceptionInput) { x.SubjectValue = "" },
		"指纹格式错":   func(x *ExceptionInput) { x.SubjectKind, x.SubjectValue = "fingerprint", "xyz" },
		"权限组不存在":  func(x *ExceptionInput) { x.SubjectKind, x.SubjectValue = "group", "99" },
		"实例不存在":   func(x *ExceptionInput) { x.SubjectKind, x.SubjectValue = "instance", "99" },
		"权限组不是数字": func(x *ExceptionInput) { x.SubjectKind, x.SubjectValue = "group", "DBA" },
	}
	for name, mut := range bad {
		in := ok
		mut(&in)
		if _, err := e.svc.CreateException(actor, in); errCode(err) != "bad_request" {
			t.Errorf("%s: 应 400，得到 %v", name, err)
		}
	}
	if len(e.st.excs) != 0 {
		t.Fatal("被拒绝的请求不应写数据")
	}
	x, err := e.svc.CreateException(actor, ok)
	if err != nil || x.SubjectText != "用户 rd_li" || x.Expired || x.RuleName != v.Name {
		t.Fatalf("创建: %+v %v", x, err)
	}
	if want := e.now.AddDate(0, 0, 30).Format("2006-01-02"); x.ExpiresAt != want {
		t.Errorf("到期日 = %s，应为 %s", x.ExpiresAt, want)
	}
	if a := lastAudit(t, e.st); a.Action != "SQL 防火墙例外" || !strings.Contains(a.ExtraInfo, "月度对账") {
		t.Errorf("审计 = %+v", a)
	}
	g, _ := e.svc.CreateException(actor, ExceptionInput{RuleID: v.ID, SubjectKind: "group", SubjectValue: "3", Reason: "审计取证需要，已备案", Days: 90})
	i, _ := e.svc.CreateException(actor, ExceptionInput{RuleID: v.ID, SubjectKind: "instance", SubjectValue: "2", Reason: "预发布环境不限制", Days: 7})
	f, _ := e.svc.CreateException(actor, ExceptionInput{RuleID: v.ID, SubjectKind: "fingerprint", SubjectValue: "F5A0B6C2", Reason: "历史清理任务，已有备份", Days: 7})
	if g.SubjectText != "权限组 DBA" || i.SubjectText != "实例 test-mysql-01" || f.SubjectValue != "f5a0b6c2" {
		t.Errorf("主体显示: %q %q %q", g.SubjectText, i.SubjectText, f.SubjectValue)
	}
	// 刚过期的仍显示并标记；过期超过 30 天的不显示
	e.st.excs = append(e.st.excs,
		model.FirewallException{ID: 801, RuleID: v.ID, SubjectKind: "user", SubjectValue: "old1", Reason: "x", ExpiresAt: e.now.Add(-2 * 24 * time.Hour)},
		model.FirewallException{ID: 802, RuleID: v.ID, SubjectKind: "user", SubjectValue: "old2", Reason: "x", ExpiresAt: e.now.Add(-60 * 24 * time.Hour)})
	list, err := e.svc.Exceptions()
	if err != nil || len(list) != 5 {
		t.Fatalf("名单 = %d %v", len(list), err)
	}
	expired := 0
	for _, it := range list {
		if it.Expired {
			expired++
		}
	}
	if expired != 1 {
		t.Errorf("过期标记 = %d", expired)
	}
	if err := e.svc.DeleteException(actor, x.ID); err != nil {
		t.Fatal(err)
	}
	if a := lastAudit(t, e.st); !strings.Contains(a.ExtraInfo, "撤销") {
		t.Errorf("撤销审计 = %s", a.ExtraInfo)
	}
	if err := e.svc.DeleteException(actor, x.ID); errCode(err) != "not_found" {
		t.Errorf("重复撤销: %v", err)
	}
}

// ---- 模板 ----

func TestTemplatesAreValidAndApplyAsObserve(t *testing.T) {
	e := newEnv(t)
	e.st.groups = append(e.st.groups, model.AuthGroup{ID: 9, Name: "x"})
	list, err := e.svc.Templates()
	if err != nil || len(list) != len(templates) {
		t.Fatal(len(list), err)
	}
	keys := map[string]bool{}
	for _, tp := range templates {
		if keys[tp.Key] {
			t.Errorf("模板 key 重复: %s", tp.Key)
		}
		keys[tp.Key] = true
		if _, err := firewall.Normalize(tp.rule()); err != nil {
			t.Errorf("模板 %s 不能通过校验: %v", tp.Key, err)
		}
	}
	for _, tp := range list {
		if tp.Applied {
			t.Errorf("开始时不应有已应用的模板: %s", tp.Key)
		}
		if tp.Pending {
			t.Errorf("在线查询 / 工单 / 导出都已接入，模板不应再标“尚未接入”: %s", tp.Key)
		}
	}
	// 某个场景不在已接入列表里时，只含这个场景的模板标 pending
	saved := activeScenes
	activeScenes = map[string]bool{"query": true}
	if l2, _ := e.svc.Templates(); !func() bool {
		for _, tp := range l2 {
			if tp.Key == "no-where-dml" {
				return tp.Pending
			}
		}
		return false
	}() {
		t.Error("只含未接入场景的模板应标 pending")
	}
	activeScenes = saved
	v, err := e.svc.ApplyTemplate(actor, "dangerous-func")
	if err != nil || v.Mode != "observe" || !v.Enabled || v.Action != "deny" {
		t.Fatalf("应用模板: %+v %v", v, err)
	}
	if a := lastAudit(t, e.st); !strings.Contains(a.ExtraInfo, "从模板添加规则") || strings.HasPrefix(a.ExtraInfo, audit.FirewallRiskyMark) {
		t.Errorf("审计 = %+v", a)
	}
	if _, err := e.svc.ApplyTemplate(actor, "dangerous-func"); errCode(err) != "duplicate_name" {
		t.Errorf("重复应用: %v", err)
	}
	if _, err := e.svc.ApplyTemplate(actor, "nope"); errCode(err) != "not_found" {
		t.Errorf("不存在的模板: %v", err)
	}
	after, _ := e.svc.Templates()
	applied := 0
	for _, tp := range after {
		if tp.Applied {
			applied++
		}
	}
	if applied != 1 {
		t.Errorf("已应用标记 = %d", applied)
	}
}

// ---- 测试台 ----

func TestEvaluateDenyHighlightAndSkips(t *testing.T) {
	e := newEnv(t)
	e.mustCreate(t, denyNoWhere())
	dis := denyNoWhere()
	dis.Name, dis.Enabled = "已停用的", ptr(false)
	e.mustCreate(t, dis)
	res, err := e.svc.Evaluate(EvaluateRequest{SQL: "DELETE FROM orders", Scene: "workflow", Env: "prod", When: "workday_day"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != "deny" || !res.Deny || res.Rule == nil || res.Message != "请补充 WHERE" || len(res.Hits) != 1 {
		t.Fatalf("结果 = %+v", res)
	}
	if res.Facts.Kind != "DELETE" || len(res.Facts.Objects) != 1 || res.Facts.Objects[0] != "orders" {
		t.Errorf("事实 = %+v", res.Facts)
	}
	skippedDisabled := false
	for _, sk := range res.Skipped {
		if strings.Contains(sk.Reason, "已停用") {
			skippedDisabled = true
		}
	}
	if !skippedDisabled {
		t.Errorf("停用的规则应在跳过里说明: %+v", res.Skipped)
	}
	joined := strings.Join(res.Notes, "；")
	if !strings.Contains(joined, "一条语句一次求值") || strings.Contains(joined, "还没有接入执行") {
		t.Errorf("工单场景应说明求值方式，且不再标尚未接入: %v", res.Notes)
	}
	// 带 WHERE：放行
	ok, _ := e.svc.Evaluate(EvaluateRequest{SQL: "DELETE FROM orders WHERE id = 1", Scene: "workflow", Env: "prod"})
	if ok.Verdict != "pass" || len(ok.Hits) != 0 {
		t.Errorf("带 WHERE 应放行: %+v", ok)
	}
	// 环境不符：规则范围外
	test, _ := e.svc.Evaluate(EvaluateRequest{SQL: "DELETE FROM orders", Scene: "workflow", Env: "test"})
	if test.Verdict != "pass" {
		t.Errorf("测试环境不在范围: %+v", test)
	}
	// 选了实例：按实例标签推断环境
	inst, _ := e.svc.Evaluate(EvaluateRequest{SQL: "DELETE FROM orders", Scene: "workflow", InstanceID: 1})
	if inst.Verdict != "deny" {
		t.Errorf("生产实例应命中: %+v", inst)
	}
	if _, err := e.svc.Evaluate(EvaluateRequest{SQL: "select 1", InstanceID: 99}); errCode(err) != "bad_request" {
		t.Errorf("实例不存在: %v", err)
	}
	if _, err := e.svc.Evaluate(EvaluateRequest{SQL: "  "}); errCode(err) != "bad_request" {
		t.Errorf("空语句: %v", err)
	}
	if _, err := e.svc.Evaluate(EvaluateRequest{SQL: "select 1", Scene: "x"}); errCode(err) != "bad_request" {
		t.Errorf("非法场景: %v", err)
	}
}

func TestEvaluateObserveGlobalModeAndExceptions(t *testing.T) {
	e := newEnv(t)
	in := RuleInput{Name: "危险函数", Priority: 20, Mode: "enforce", Scenes: []string{"query"}, Env: "all", Match: "any",
		Conditions: []firewall.Condition{{T: "keyword", V: []string{"SLEEP"}}}, Action: "deny", Message: "不允许"}
	v := e.mustCreate(t, in)
	run := func() *EvalResult {
		r, err := e.svc.Evaluate(EvaluateRequest{SQL: "select id, sleep(30) from t", Scene: "query", User: "rd_li"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := run()
	if r.Verdict != "deny" || len(r.Highlight) != 1 || !strings.EqualFold(r.Highlight[0], "sleep") {
		t.Fatalf("命中并高亮: %+v", r)
	}
	// 全局观察：只记录，决定放行，命中标 observe
	e.st.cfg[settings.KeyFirewallMode] = "observe"
	r = run()
	if r.Verdict != "pass" || len(r.Hits) != 1 || r.Hits[0].Effective != "observe" || r.Hits[0].WouldAction != "deny" || r.GlobalMode != "observe" {
		t.Errorf("全局观察: %+v", r)
	}
	delete(e.st.cfg, settings.KeyFirewallMode)
	// 例外：用户 rd_li 被豁免，放行并在跳过里说明
	if _, err := e.svc.CreateException(actor, ExceptionInput{RuleID: v.ID, SubjectKind: "user", SubjectValue: "rd_li", Reason: "月度报表需要", Days: 7}); err != nil {
		t.Fatal(err)
	}
	r = run()
	reasoned := false
	for _, sk := range r.Skipped {
		reasoned = reasoned || strings.Contains(sk.Reason, "例外")
	}
	if r.Verdict != "pass" || !reasoned {
		t.Errorf("例外应放行并说明: %+v", r)
	}
}

func TestEvaluateTightenVerdicts(t *testing.T) {
	e := newEnv(t)
	e.mustCreate(t, RuleInput{Name: "敏感表要原因", Priority: 1, Mode: "enforce", Scenes: []string{"query"}, Match: "any",
		Conditions: []firewall.Condition{{T: "object", V: []string{"*.sys_users"}}}, Action: "reason", Message: "请说明用途"})
	e.mustCreate(t, RuleInput{Name: "敏感列脱敏", Priority: 2, Mode: "enforce", Scenes: []string{"query"}, Match: "any",
		Conditions: []firewall.Condition{{T: "object", V: []string{"*.*.password"}}}, Action: "mask", Message: "已隐藏"})
	e.mustCreate(t, RuleInput{Name: "限行", Priority: 3, Mode: "enforce", Scenes: []string{"query"}, Match: "all",
		Conditions: []firewall.Condition{{T: "feature", V: "select_star"}}, Action: "limit", ActionArg: 500, Message: "最多 500 行"})
	r, err := e.svc.Evaluate(EvaluateRequest{SQL: "select * from shop.sys_users", Scene: "query", Env: "prod", DB: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != "reason" || !r.NeedReason || r.MaxRows != 500 || r.LimitedBy != "限行" || len(r.ReasonMsgs) != 1 {
		t.Errorf("reason + limit: %+v", r)
	}
	r, _ = e.svc.Evaluate(EvaluateRequest{SQL: "select password from shop.accounts", Scene: "query"})
	if r.Verdict != "mask" || len(r.ForceMask) != 1 || r.ForceMask[0] != "*.*.password" {
		t.Errorf("强制脱敏: %+v", r)
	}
	// 工单场景：reason / limit / mask 不适用，降级为告警并写明原因
	in := e.mustCreate(t, RuleInput{Name: "跨场景限行", Priority: 4, Mode: "enforce", Scenes: []string{"query", "workflow"}, Match: "all",
		Conditions: []firewall.Condition{{T: "stmt", V: []string{"SELECT"}}}, Action: "limit", ActionArg: 100})
	_ = in
	r, _ = e.svc.Evaluate(EvaluateRequest{SQL: "select 1 from t", Scene: "workflow"})
	if r.Verdict != "warn" || len(r.Hits) != 1 || r.Hits[0].Downgraded == "" {
		t.Errorf("工单里限行应降级为告警: %+v", r)
	}
	// 手填预估行数：用来试 scan_rows 条件；不填则“未评估”
	e.mustCreate(t, RuleInput{Name: "大扫描", Priority: 5, Mode: "enforce", Scenes: []string{"query"}, Match: "all",
		Conditions: []firewall.Condition{{T: "scan_rows", V: 1000000}}, Action: "deny", Message: "扫描过大"})
	r, _ = e.svc.Evaluate(EvaluateRequest{SQL: "select a from big", Scene: "query"})
	if r.Deny {
		t.Errorf("没有预估行数应未评估: %+v", r)
	}
	r, _ = e.svc.Evaluate(EvaluateRequest{SQL: "select a from big", Scene: "query", ScanRows: ptr(int64(5000000))})
	if !r.Deny || r.Rule == nil || r.Rule.Name != "大扫描" {
		t.Errorf("手填 500 万行应命中: %+v", r)
	}
}

func TestEvaluateNotesInvalidStoredRule(t *testing.T) {
	e := newEnv(t)
	e.st.rules = append(e.st.rules, model.FirewallRule{ID: 7, Name: "手工插入的坏规则", Enabled: true, Mode: "enforce", Scenes: "query", Env: "all",
		GroupMode: "all", MatchMode: "all", Conditions: `[{"t":"regex","v":"(oops"}]`, Action: "deny"})
	r, err := e.svc.Evaluate(EvaluateRequest{SQL: "select 1", Scene: "query"})
	if err != nil || r.Verdict != "pass" || len(r.Notes) == 0 || !strings.Contains(r.Notes[0], "手工插入的坏规则") {
		t.Fatalf("坏规则应被说明而不是报错: %+v %v", r, err)
	}
}

func TestBenchTablesCoversDML(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{"SELECT a FROM shop.orders o JOIN users u ON o.uid = u.id", []string{"shop.orders", ".users"}},
		{"UPDATE orders SET status = 1 WHERE id = 2", []string{".orders"}},
		{"DELETE FROM `shop`.`orders_tmp`", []string{"shop.orders_tmp"}},
		{"INSERT INTO t_log (a) VALUES (1)", []string{".t_log"}},
		{"ALTER TABLE IF EXISTS orders ADD INDEX i (a)", []string{".orders"}},
		{"TRUNCATE TABLE t_tmp", []string{".t_tmp"}},
		{"SELECT 1", nil},
	}
	for _, c := range cases {
		var got []string
		for _, o := range benchTables(c.sql) {
			got = append(got, o.Schema+"."+o.Table)
		}
		sort.Strings(got)
		want := append([]string(nil), c.want...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: 得到 %v，应为 %v", c.sql, got, want)
		}
	}
}

func TestWhenTime(t *testing.T) {
	sunday := time.Date(2026, 10, 11, 12, 0, 0, 0, time.Local)
	if got := whenTime("workday_day", sunday); got.Weekday() != time.Friday || got.Hour() != 14 {
		t.Errorf("周日的“工作日白天”应取上一个周五 14 点: %v", got)
	}
	if got := whenTime("weekend", time.Date(2026, 10, 14, 9, 0, 0, 0, time.Local)); got.Weekday() != time.Saturday || got.Hour() != 11 {
		t.Errorf("周末: %v", got)
	}
	if got := whenTime("", sunday); !got.Equal(sunday) {
		t.Errorf("缺省取现在: %v", got)
	}
}

// ---- 回放 ----

func logAt(id uint, user, inst, sql string, at time.Time) model.QueryLog {
	return model.QueryLog{ID: id, Username: user, InstanceName: inst, DBName: "shop", SQLLog: sql, CreateTime: at}
}

func TestReplayCountsAndAdvice(t *testing.T) {
	e := newEnv(t)
	e.st.ugroups = map[string][]uint{"dba_li": {3}, "rd_zhao": {5}}
	for i := 0; i < 90; i++ {
		e.st.logs = append(e.st.logs, logAt(uint(i+1), "rd_zhao", "prod-mysql-01", "select id from orders where id = 1 limit 100", e.now))
	}
	for i := 0; i < 8; i++ {
		e.st.logs = append(e.st.logs, logAt(uint(100+i), "rd_zhao", "prod-mysql-01", "select id, sleep(1) from orders limit 10", e.now.Add(-time.Hour)))
	}
	e.st.logs = append(e.st.logs, logAt(200, "dba_li", "prod-mysql-01", "select benchmark(100, 1)", e.now), logAt(201, "qa_sun", "test-mysql-01", "select sleep(2)", e.now))
	draft := RuleInput{Name: "草稿：危险函数", Priority: 1, Mode: "observe", Scenes: []string{"query"}, Env: "prod", Match: "any",
		Conditions: []firewall.Condition{{T: "keyword", V: []string{"SLEEP", "BENCHMARK"}}}, Action: "deny"}
	res, err := e.svc.Replay(context.Background(), ReplayRequest{Rule: &draft})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applicable || res.Scanned != 100 {
		t.Fatalf("扫描 = %+v", res)
	}
	// 9 条命中：8 条 rd_zhao + 1 条 dba_li；qa_sun 在测试实例上，不在“仅生产”范围内
	if res.Hits != 9 || res.Users != 2 || !res.DBAKnown || res.DBAHits != 1 {
		t.Errorf("命中 = %+v", res)
	}
	if len(res.Samples) != replaySamples || res.Samples[0].User == "" || len(res.Samples[0].Why) == 0 {
		t.Errorf("样例 = %+v", res.Samples)
	}
	if res.Rate < 0.08 || res.Rate > 0.1 || !strings.Contains(res.Advice, "观察") {
		t.Errorf("命中占比 %.3f 建议 %q", res.Rate, res.Advice)
	}
}

func TestReplayStoredRuleAndEdgeCases(t *testing.T) {
	e := newEnv(t)
	wf := denyNoWhere() // 只适用于工单
	v := e.mustCreate(t, wf)
	res, err := e.svc.Replay(context.Background(), ReplayRequest{RuleID: v.ID})
	if err != nil || !res.Applicable || res.Scanned != 0 || !strings.Contains(res.Advice, "没有") {
		t.Fatalf("工单场景有数据来源，只是最近没有工单: %+v %v", res, err)
	}
	// 没有日志
	q := e.mustCreate(t, RuleInput{Name: "q", Priority: 2, Mode: "observe", Scenes: []string{"query"}, Match: "all",
		Conditions: []firewall.Condition{{T: "feature", V: "select_star"}}, Action: "warn"})
	res, err = e.svc.Replay(context.Background(), ReplayRequest{RuleID: q.ID, Days: 99})
	if err != nil || res.Days != replayMaxDays || res.Scanned != 0 || !strings.Contains(res.Advice, "没有") {
		t.Errorf("无日志: %+v %v", res, err)
	}
	// EXPLAIN 类条件：回放不评估，统计“未评估”
	big := e.mustCreate(t, RuleInput{Name: "big", Priority: 3, Mode: "observe", Scenes: []string{"query"}, Match: "all",
		Conditions: []firewall.Condition{{T: "scan_rows", V: 1000000}}, Action: "warn"})
	e.st.logs = []model.QueryLog{logAt(1, "u", "prod-mysql-01", "select 1 from t", e.now), logAt(2, "u", "prod-mysql-01", "select 2 from t", e.now)}
	res, _ = e.svc.Replay(context.Background(), ReplayRequest{RuleID: big.ID})
	if res.Hits != 0 || res.Unevaluated != 2 {
		t.Errorf("未评估 = %+v", res)
	}
	// 命中很少：不建议“超过 1%”
	if _, err := e.svc.Replay(context.Background(), ReplayRequest{}); errCode(err) != "bad_request" {
		t.Errorf("没指定规则: %v", err)
	}
	if _, err := e.svc.Replay(context.Background(), ReplayRequest{RuleID: 4040}); errCode(err) != "not_found" {
		t.Errorf("规则不存在: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	e.st.logs = nil
	for i := 0; i < 500; i++ {
		e.st.logs = append(e.st.logs, logAt(uint(i+1), "u", "prod-mysql-01", "select * from t", e.now))
	}
	res, err = e.svc.Replay(cancelled, ReplayRequest{RuleID: q.ID})
	if err != nil || !res.Truncated {
		t.Errorf("取消后应标记截断: %+v %v", res, err)
	}
}

func TestReplayWorkflowStatements(t *testing.T) {
	e := newEnv(t)
	e.st.ugroups = map[string][]uint{"rd_zhao": {5}, "dba_li": {3}}
	review := func(rows string) string { return rows }
	e.st.wfs = []WorkflowSQL{
		// 提交时的检查结果里有预估影响行数
		{ID: 3, Engineer: "rd_zhao", InstanceID: 1, DBName: "shop", SQL: "update orders set a = 1;\ndelete from orders", At: e.now,
			Review: review(`[{"id":1,"sql":"update orders set a = 1","affected_rows":500000},{"id":2,"sql":"delete from orders","affected_rows":0}]`)},
		{ID: 2, Engineer: "dba_li", InstanceID: 1, DBName: "shop", SQL: "delete from tmp", At: e.now},
		{ID: 1, Engineer: "rd_zhao", InstanceID: 2, DBName: "shop", SQL: "select id from orders", Export: true, At: e.now},
	}
	// 规则一：整表 UPDATE / DELETE（工单）
	res, err := e.svc.Replay(context.Background(), ReplayRequest{Rule: ptr(denyNoWhere())})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applicable || res.Scanned != 3 || res.ByScene["workflow"] != 3 || res.Hits != 3 || res.Users != 2 || res.DBAHits != 1 {
		t.Fatalf("工单语句回放: %+v", res)
	}
	if len(res.Samples) == 0 || res.Samples[0].Scene != "workflow" || res.Samples[0].User == "" {
		t.Errorf("样例应带场景: %+v", res.Samples)
	}
	// 规则二：影响行数 ≥ 10 万（工单）：只有第一条有可靠的影响行数
	big := RuleInput{Name: "影响行数过多", Priority: 1, Mode: "observe", Scenes: []string{"workflow"}, Match: "all",
		Conditions: []firewall.Condition{{T: "affect_rows", V: 100000}}, Action: "deny"}
	res, err = e.svc.Replay(context.Background(), ReplayRequest{Rule: &big})
	if err != nil || res.Hits != 1 || res.Unevaluated != 2 {
		t.Errorf("预估影响行数取自提交时的检查结果，没有值的算未评估: %+v %v", res, err)
	}
	// 规则三：只适用导出场景：只回放导出工单
	exp := RuleInput{Name: "导出订单要原因", Priority: 1, Mode: "observe", Scenes: []string{"export"}, Match: "all",
		Conditions: []firewall.Condition{{T: "keyword", V: []string{"orders"}}}, Action: "warn"}
	res, err = e.svc.Replay(context.Background(), ReplayRequest{Rule: &exp})
	if err != nil || res.Scanned != 1 || res.ByScene["export"] != 1 || res.Hits != 1 {
		t.Errorf("导出场景只回放导出工单: %+v %v", res, err)
	}
	// 规则四：在线查询 + 工单：两个来源一起回放
	e.st.logs = []model.QueryLog{logAt(1, "rd_zhao", "prod-mysql-01", "select * from orders", e.now)}
	both := RuleInput{Name: "两个场景", Priority: 1, Mode: "observe", Scenes: []string{"query", "workflow"}, Match: "all",
		Conditions: []firewall.Condition{{T: "keyword", V: []string{"orders"}}}, Action: "warn"}
	res, err = e.svc.Replay(context.Background(), ReplayRequest{Rule: &both})
	if err != nil || res.ByScene["query"] != 1 || res.ByScene["workflow"] != 3 {
		t.Errorf("两个来源: %+v %v", res, err)
	}
}

func TestHighlights(t *testing.T) {
	got := highlights([]firewall.Condition{
		{T: "keyword", V: []string{"SLEEP", "BENCHMARK"}},
		{T: "regex", V: `into\s+outfile`},
		{T: "object", V: []string{"*.sys_users", "shop.users.phone"}},
	}, "select phone, sleep(3) from sys_users into  OUTFILE '/tmp/x'")
	want := []string{"sleep", "into  OUTFILE", "sys_users", "phone"}
	if strings.ToLower(strings.Join(got, "|")) != strings.ToLower(strings.Join(want, "|")) {
		t.Errorf("高亮 = %v，应为 %v", got, want)
	}
}

// ---- HTTP ----

func TestHTTPSuperuserOnlyAndFlow(t *testing.T) {
	e := newEnv(t)
	routes := []struct{ method, path string }{
		{"GET", "/api/v1/settings/firewall/overview"}, {"GET", "/api/v1/settings/firewall/meta"}, {"GET", "/api/v1/settings/firewall/rules"},
		{"POST", "/api/v1/settings/firewall/rules"}, {"GET", "/api/v1/settings/firewall/rules/1"}, {"PUT", "/api/v1/settings/firewall/rules/1"},
		{"DELETE", "/api/v1/settings/firewall/rules/1"}, {"POST", "/api/v1/settings/firewall/rules/1/toggle"},
		{"POST", "/api/v1/settings/firewall/rules/1/mode"}, {"POST", "/api/v1/settings/firewall/rules/1/replay"},
		{"POST", "/api/v1/settings/firewall/replay"}, {"POST", "/api/v1/settings/firewall/evaluate"},
		{"GET", "/api/v1/settings/firewall/hits"}, {"GET", "/api/v1/settings/firewall/hits/1"},
		{"GET", "/api/v1/settings/firewall/exceptions"}, {"POST", "/api/v1/settings/firewall/exceptions"},
		{"DELETE", "/api/v1/settings/firewall/exceptions/1"}, {"GET", "/api/v1/settings/firewall/templates"},
		{"POST", "/api/v1/settings/firewall/templates/dangerous-func/apply"}, {"PUT", "/api/v1/settings/firewall/mode"},
		{"PUT", "/api/v1/settings/firewall/switch"},
	}
	bob := e.te.Login("bob")
	for _, r := range routes {
		if w := e.te.Anonymous(r.method, r.path); w.Code != http.StatusUnauthorized {
			t.Errorf("未登录 %s %s = %d", r.method, r.path, w.Code)
		}
		if w := bob.Do(r.method, r.path, map[string]any{}); w.Code != http.StatusForbidden {
			t.Errorf("普通用户 %s %s = %d", r.method, r.path, w.Code)
		}
	}
	if len(e.st.rules) != 0 || len(e.st.audits) != 0 || len(e.st.cfg) != 0 {
		t.Fatal("被拒绝的请求不应写任何东西")
	}
	root := e.te.Login("root")
	type envelope struct {
		Data json.RawMessage `json:"data"`
	}
	call := func(method, path string, body any) (int, json.RawMessage) {
		w := root.Do(method, path, body)
		var env envelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		return w.Code, env.Data
	}
	code, data := call("POST", "/api/v1/settings/firewall/rules", denyNoWhere())
	if code != http.StatusOK {
		t.Fatalf("创建 = %d %s", code, data)
	}
	var rv RuleView
	_ = json.Unmarshal(data, &rv)
	if rv.ID == 0 || len(e.st.rules) != 1 {
		t.Fatalf("创建结果: %+v", rv)
	}
	if code, data := call("POST", "/api/v1/settings/firewall/rules", RuleInput{Name: "x"}); code != http.StatusBadRequest {
		t.Errorf("非法规则 = %d %s", code, data)
	}
	if code, _ := call("POST", "/api/v1/settings/firewall/rules", denyNoWhere()); code != http.StatusConflict {
		t.Errorf("重名应 409，得到 %d", code)
	}
	if code, _ := call("GET", "/api/v1/settings/firewall/rules/abc", nil); code != http.StatusBadRequest {
		t.Errorf("坏编号 = %d", code)
	}
	if code, _ := call("GET", "/api/v1/settings/firewall/rules/9999", nil); code != http.StatusNotFound {
		t.Errorf("不存在 = %d", code)
	}
	code, data = call("POST", fmt.Sprintf("/api/v1/settings/firewall/rules/%d/mode", rv.ID), map[string]any{"mode": "observe"})
	var mr ModeResult
	_ = json.Unmarshal(data, &mr)
	if code != http.StatusOK || mr.Rule.Mode != "observe" {
		t.Errorf("切模式 = %d %s", code, data)
	}
	code, data = call("POST", "/api/v1/settings/firewall/evaluate", map[string]any{"sql": "delete from t", "scene": "workflow", "env": "prod", "scan_rows": 10})
	var er EvalResult
	_ = json.Unmarshal(data, &er)
	if code != http.StatusOK || er.Verdict != "pass" || len(er.Hits) != 1 || er.Hits[0].Effective != "observe" {
		t.Errorf("测试台 = %d %+v", code, er)
	}
	code, data = call("GET", "/api/v1/settings/firewall/rules", nil)
	var rl RuleList
	_ = json.Unmarshal(data, &rl)
	if code != http.StatusOK || !rl.Ready || len(rl.Rules) != 1 || rl.MaxRules != firewall.MaxRules {
		t.Errorf("列表 = %d %+v", code, rl)
	}
	code, data = call("GET", "/api/v1/settings/firewall/meta", nil)
	var meta Meta
	_ = json.Unmarshal(data, &meta)
	if code != http.StatusOK || len(meta.Groups) != 2 || len(meta.Instances) != 2 || len(meta.Scenes) != 3 || !meta.Scenes[0].Active || !meta.Scenes[1].Active || !meta.Scenes[2].Active || len(meta.Pending) != 0 {
		t.Errorf("选项 = %d %+v", code, meta)
	}
	if code, _ := call("PUT", "/api/v1/settings/firewall/switch", map[string]any{"enabled": false, "reason": ""}); code != http.StatusBadRequest {
		t.Errorf("关闭缺原因 = %d", code)
	}
	if code, _ := call("DELETE", fmt.Sprintf("/api/v1/settings/firewall/rules/%d", rv.ID), nil); code != http.StatusOK || len(e.st.rules) != 0 {
		t.Errorf("删除 = %d rules=%d", code, len(e.st.rules))
	}
}

func TestHTTPPageSuperuserOnly(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.te.Login("bob").Page("/settings/firewall"); code != http.StatusForbidden {
		t.Errorf("普通用户访问页面 = %d", code)
	}
	code, body := e.te.Login("root").Page("/settings/firewall")
	if code != http.StatusOK {
		t.Fatalf("超级用户页面 = %d", code)
	}
	for _, want := range []string{`id="fw"`, `/static/js/firewall/main.js`, `/static/css/firewall.css`, `id="fw-rules"`, `id="fw-b-sql"`} {
		if !strings.Contains(body, want) {
			t.Errorf("页面缺少 %s", want)
		}
	}
	if strings.Contains(body, " style=") || strings.Contains(body, "<script>") {
		t.Error("页面不应带内联样式 / 内联脚本（CSP）")
	}
}

type fakeStats struct {
	ex firewall.ExplainStats
	rs map[uint]firewall.RuleStat
}

func (f *fakeStats) ExplainStats() firewall.ExplainStats   { return f.ex }
func (f *fakeStats) RuleStats() map[uint]firewall.RuleStat { return f.rs }

func TestOverviewAndRulesCarryRuntimeStats(t *testing.T) {
	e := newEnv(t)
	a := e.mustCreate(t, denyNoWhere())
	other := denyNoWhere()
	other.Name = "另一条"
	b := e.mustCreate(t, other)
	e.st.cfg[settings.KeyFirewallExplainTimeoutMs] = "5000"
	ms := int64(time.Millisecond)
	e.svc.stats = &fakeStats{
		ex: firewall.ExplainStats{Calls: 12, Failures: 2, Timeouts: 1, CacheHits: 30, TotalMs: 600, MaxMs: 120, Open: []uint{3}},
		rs: map[uint]firewall.RuleStat{
			a.ID: {Evals: 10, Unknown: 4, TotalNs: 10 * 300 * ms, MaxNs: 900 * ms},
			b.ID: {Evals: 3, Unknown: 3, TotalNs: 3 * 500 * ms}, // 次数不足：不下结论
		},
	}
	ov, err := e.svc.Overview()
	if err != nil {
		t.Fatal(err)
	}
	if ov.Explain == nil || ov.Explain.Calls != 12 || len(ov.Explain.Open) != 1 || ov.ExplainTimeoutMs != 5000 || ov.SlowRules != 1 || ov.BlindRules != 1 {
		t.Errorf("概览: %+v %+v", ov, ov.Explain)
	}
	list, _ := e.svc.Rules()
	var ra, rb RuleView
	for _, r := range list.Rules {
		if r.ID == a.ID {
			ra = r
		} else {
			rb = r
		}
	}
	if !ra.Slow || !ra.Blind || ra.UnknownPct != 40 || ra.AvgMs < 299 || ra.AvgMs > 301 || ra.MaxMs < 899 {
		t.Errorf("慢规则 / 未评估占比: %+v", ra)
	}
	if rb.Slow || rb.Blind || rb.Evals != 3 {
		t.Errorf("求值次数不足时不应标慢 / 标未评估: %+v", rb)
	}
	// 没有统计来源（测试 / 演示）：字段为零值，不影响页面
	e.svc.stats = nil
	if ov2, _ := e.svc.Overview(); ov2.Explain != nil || ov2.SlowRules != 0 {
		t.Errorf("无统计来源: %+v", ov2)
	}
}
