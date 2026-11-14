// @author: kevinkelly
// @license: Apache Licence
// @file: firewall_test.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/firewall"
	"dbtool/internal/model"
	"dbtool/internal/settings"
	"dbtool/internal/sqlx"
)

// fwStore 是内存里的防火墙存储：记录写入的命中，并模拟“窗口内合并”。
type fwStore struct {
	rules    []firewall.Rule
	hits     []firewall.HitRow
	attached map[uint]uint
	loadErr  error
}

func (f *fwStore) LoadRules() ([]firewall.Rule, error) { return f.rules, f.loadErr }
func (f *fwStore) LoadExceptions(time.Time) ([]firewall.Exception, error) {
	return nil, f.loadErr
}
func (f *fwStore) UserGroupIDs(uint) ([]uint, error) { return nil, nil }
func (f *fwStore) RecordHit(h firewall.HitRow, _ time.Duration) (uint, error) {
	f.hits = append(f.hits, h)
	return uint(len(f.hits)), nil
}
func (f *fwStore) AttachQueryLog(ids []uint, logID uint) error {
	if f.attached == nil {
		f.attached = map[uint]uint{}
	}
	for _, id := range ids {
		f.attached[id] = logID
	}
	return nil
}

func fwRule(id uint, name, action string, conds ...firewall.Condition) firewall.Rule {
	return firewall.Rule{ID: id, Name: name, Priority: int(id), Enabled: true, Mode: firewall.ModeEnforce,
		Scenes: []string{firewall.SceneQuery}, Env: "all", GroupMode: "all", Match: "all", Conditions: conds, Action: action}
}

func kw(words ...string) firewall.Condition {
	return firewall.Condition{T: firewall.CondKeyword, V: words}
}

func fwService(repo Repo, eng engine.Engine, rules ...firewall.Rule) (*Service, *fwStore) {
	st := &fwStore{rules: rules}
	s := svcWith(repo, eng)
	s.SetFirewall(firewall.New(st, repo))
	return s, st
}

var fwDev = Principal{ID: 7, Username: "dev"}

func TestFirewallDenyBlocksBeforeAuthorization(t *testing.T) {
	repo := &evRepo{histRepo: &histRepo{fakeRepo: devRepo()}, ready: true}
	eng := &fakeEngine{rows: 3}
	rule := fwRule(1, "禁止查余额", firewall.ActDeny, kw("balance"))
	rule.Message = "钱包表只允许财务查询"
	s, st := fwService(repo, eng, rule)

	// dev 对 secret 库表没有任何权限：如果先做授权，会得到 no_privilege；防火墙在授权之前，所以先被它拒绝。
	_, err := s.Execute(context.Background(), fwDev, Request{InstanceID: 1, DB: "secret", SQL: "select balance from merchant_wallet"})
	var qe *Error
	if !errors.As(err, &qe) || qe.Code != "firewall_denied" || qe.HTTP != 403 || qe.Message != "钱包表只允许财务查询" {
		t.Fatalf("应在授权之前被防火墙拒绝: %v", err)
	}
	if qe.Data["rule"] != "禁止查余额" {
		t.Errorf("应带上规则名: %v", qe.Data)
	}
	if eng.gotSQL != "" {
		t.Errorf("被拒绝的语句不应到达目标库: %q", eng.gotSQL)
	}
	if len(st.hits) != 1 || st.hits[0].Effective != "deny" || st.hits[0].UserName != "dev" || st.hits[0].InstanceName != "prod-mysql-01" {
		t.Errorf("应记一条拒绝命中: %+v", st.hits)
	}
	if strings.Contains(st.hits[0].SQLText, "merchant_wallet") == false {
		t.Errorf("命中记录里仍应能看到语句形态: %q", st.hits[0].SQLText)
	}
	// 被拒绝的语句进查询事件表，记为“被拦截”
	if len(repo.events) != 1 || repo.events[0].Status != StatusBlocked || repo.events[0].Code != "firewall_denied" {
		t.Errorf("应记为 blocked:firewall_denied: %+v", repo.events)
	}
	// 不命中的语句不受影响
	if _, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id from orders"}); err != nil {
		t.Errorf("无关语句应正常执行: %v", err)
	}
}

func TestFirewallReasonRequiredThenRecordedOnce(t *testing.T) {
	repo := &evRepo{histRepo: &histRepo{fakeRepo: newRepo()}, ready: true}
	eng := &fakeEngine{rows: 2}
	rule := fwRule(1, "敏感表要原因", firewall.ActReason, kw("sys_users"))
	rule.Message = "请说明查询用途"
	s, st := fwService(repo, eng, rule)
	req := Request{InstanceID: 1, DB: "shop", SQL: "select id from sys_users"}

	_, err := s.Execute(context.Background(), admin, req)
	var qe *Error
	if !errors.As(err, &qe) || qe.Code != "reason_required" || qe.HTTP != 428 || !strings.Contains(qe.Message, "请说明查询用途") {
		t.Fatalf("应要求填写原因: %v", err)
	}
	if len(st.hits) != 0 || len(repo.events) != 0 || eng.gotSQL != "" {
		t.Errorf("要原因的那一次不记命中、不记事件、不执行: hits=%d events=%d sql=%q", len(st.hits), len(repo.events), eng.gotSQL)
	}

	req.Reason = "  月度对账  "
	res, err := s.Execute(context.Background(), admin, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.hits) != 1 || st.hits[0].Effective != "reason" || st.hits[0].ReasonText != "月度对账" {
		t.Fatalf("填写原因后记一次命中并带上原因: %+v", st.hits)
	}
	if got := st.attached[1]; got == 0 || got != res.QueryLogID {
		t.Errorf("命中应回填查询日志 ID: attached=%v log=%d", st.attached, res.QueryLogID)
	}
}

func TestFirewallLimitTightensRowCap(t *testing.T) {
	repo, eng := newRepo(), &fakeEngine{rows: 50}
	rule := fwRule(1, "明细最多 10 行", firewall.ActLimit, kw("orders"))
	rule.ActionArg = 10
	s, _ := fwService(repo, eng, rule)
	res, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id from orders"})
	if err != nil {
		t.Fatal(err)
	}
	if eng.gotOpt.Limit != 10 || !strings.HasSuffix(eng.gotSQL, "LIMIT 10") || res.EffectiveLimit != 10 {
		t.Errorf("行数上限应被收紧到 10: sql=%q limit=%d eff=%d", eng.gotSQL, eng.gotOpt.Limit, res.EffectiveLimit)
	}
	if !strings.Contains(res.LimitSource, "明细最多 10 行") || res.Firewall == nil || res.Firewall.LimitedBy != "明细最多 10 行" {
		t.Errorf("应说明限制来自防火墙规则: %q %+v", res.LimitSource, res.Firewall)
	}
	// 用户自己要的更小时，以用户为准，也不算防火墙收紧
	res, _ = s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id from orders", Limit: 5})
	if res.EffectiveLimit != 5 || res.Firewall != nil {
		t.Errorf("用户要得更少时不应标为防火墙收紧: %d %+v", res.EffectiveLimit, res.Firewall)
	}
}

func TestFirewallForcedMaskWorksWithMasterSwitchOff(t *testing.T) {
	repo := newRepo() // data_masking 没开，也没有配置任何脱敏字段
	eng := &scriptEngine{script: map[string]*engine.ResultSet{
		"select": {ColumnList: []string{"id", "phone"}, Rows: [][]any{{1, "13812345678"}, {2, "13999990000"}}},
	}}
	rule := fwRule(1, "手机号强制脱敏", firewall.ActMask, firewall.Condition{T: firewall.CondObject, V: []string{"*.*.phone"}})
	s, _ := fwService(repo, eng, rule)
	res, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id, phone from users"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][1] == "13812345678" || res.Rows[1][1] == "13999990000" {
		t.Fatalf("手机号应被强制脱敏: %v", res.Rows)
	}
	if res.Rows[0][0] != 1 {
		t.Errorf("其它列不受影响: %v", res.Rows[0])
	}
	if len(res.MaskedColumns) != 1 || res.MaskedColumns[0] != "phone" || res.Firewall == nil || len(res.Firewall.MaskedBy) != 1 {
		t.Errorf("应说明哪一列因哪条规则被脱敏: %v %+v", res.MaskedColumns, res.Firewall)
	}
	// 语句里没有这个列：规则不命中，结果原样
	eng.script["select"] = &engine.ResultSet{ColumnList: []string{"id"}, Rows: [][]any{{1}}}
	res, err = s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id from users"})
	if err != nil || res.Firewall != nil || len(res.MaskedColumns) != 0 {
		t.Errorf("没有命中不应脱敏: %v %+v", err, res)
	}
}

func TestFirewallWarnAndObserveDoNotBlock(t *testing.T) {
	repo, eng := newRepo(), &fakeEngine{rows: 1}
	warn := fwRule(1, "大表提示", firewall.ActWarn, kw("big_table"))
	warn.Message = "这是一张大表，注意加条件"
	obs := fwRule(2, "观察中的拦截", firewall.ActDeny, kw("big_table"))
	obs.Mode = firewall.ModeObserve
	s, st := fwService(repo, eng, warn, obs)
	res, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id from big_table"})
	if err != nil {
		t.Fatalf("告警与观察都不应拦截: %v", err)
	}
	if res.Firewall == nil || len(res.Firewall.Warnings) != 1 || !strings.Contains(res.Notice, "这是一张大表，注意加条件") {
		t.Errorf("应提示告警: %+v notice=%q", res.Firewall, res.Notice)
	}
	if len(st.hits) != 2 {
		t.Fatalf("告警与观察命中都要记录: %+v", st.hits)
	}
	var observed *firewall.HitRow
	for i := range st.hits {
		if st.hits[i].Effective == "observe" {
			observed = &st.hits[i]
		}
	}
	if observed == nil || observed.WouldAction != "deny" {
		t.Errorf("观察命中应记录“若生效会拒绝”: %+v", st.hits)
	}
}

func TestFirewallEstimateIsDry(t *testing.T) {
	repo, eng := newRepo(), &fakeEngine{rows: 1}
	deny := fwRule(1, "禁 a", firewall.ActDeny, kw("forbidden_a"))
	why := fwRule(2, "要原因", firewall.ActReason, kw("needs_reason"))
	s, st := fwService(repo, eng, deny, why)
	// 要原因的语句：估算不应要求原因，也不记命中
	if _, err := s.EstimateRows(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id from needs_reason"}); err != nil {
		t.Errorf("估算不应要求填写原因: %v", err)
	}
	// 被拒绝的语句：估算同样被拒绝，但不记命中
	_, err := s.EstimateRows(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id from forbidden_a"})
	if code(err) != "firewall_denied" {
		t.Errorf("被拒绝的语句估算也应被拒绝: %v", err)
	}
	if len(st.hits) != 0 {
		t.Errorf("估算不应写命中记录: %+v", st.hits)
	}
}

func TestFirewallFailurePolicy(t *testing.T) {
	repo, eng := newRepo(), &fakeEngine{rows: 1}
	st := &fwStore{loadErr: errors.New("db down")}
	s := svcWith(repo, eng)
	s.SetFirewall(firewall.New(st, repo))
	if _, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id from t"}); err != nil {
		t.Errorf("缺省 fail-open：防火墙自身故障不应影响查询: %v", err)
	}
	repo.cfg[settings.KeyFirewallOnError] = "deny"
	s2 := svcWith(repo, eng)
	s2.SetFirewall(firewall.New(st, repo))
	_, err := s2.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id from t"})
	if code(err) != "firewall_unavailable" {
		t.Errorf("firewall_on_error=deny 应拒绝: %v", err)
	}
}

func TestFirewallDisabledLeavesPipelineUntouched(t *testing.T) {
	repo, eng := newRepo(), &fakeEngine{rows: 1}
	s := svcWith(repo, eng) // 没有 SetFirewall
	res, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id from t"})
	if err != nil || res.Firewall != nil {
		t.Errorf("未启用防火墙时行为不变: %v %+v", err, res)
	}
}

// ---- 库表解析缓存 ----

type countingExtractor struct {
	n   int
	err error
}

func (c *countingExtractor) Tables(_ context.Context, _ *model.Instance, db, _ string) ([]sqlx.TableRef, error) {
	c.n++
	if c.err != nil {
		return nil, c.err
	}
	return []sqlx.TableRef{{Schema: db, Name: "t"}}, nil
}

func TestMemoExtractorCachesSuccessfulResultsOnly(t *testing.T) {
	inner := &countingExtractor{}
	m := newMemoExtractor(inner)
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	inst := &model.Instance{ID: 1}
	for i := 0; i < 3; i++ {
		if refs, err := m.Tables(context.Background(), inst, "shop", "select 1 from t"); err != nil || len(refs) != 1 {
			t.Fatal(refs, err)
		}
	}
	if inner.n != 1 {
		t.Errorf("同一语句应只解析一次: %d", inner.n)
	}
	m.Tables(context.Background(), inst, "shop", "select 2 from t")
	m.Tables(context.Background(), &model.Instance{ID: 2}, "shop", "select 1 from t")
	m.Tables(context.Background(), inst, "crm", "select 1 from t")
	if inner.n != 4 {
		t.Errorf("语句 / 实例 / 库不同应分别解析: %d", inner.n)
	}
	now = now.Add(11 * time.Second)
	m.Tables(context.Background(), inst, "shop", "select 1 from t")
	if inner.n != 5 {
		t.Errorf("过期后应重新解析: %d", inner.n)
	}
	// 缓存里的切片不会被调用方改坏
	refs, _ := m.Tables(context.Background(), inst, "shop", "select 1 from t")
	refs[0].Name = "tampered"
	if again, _ := m.Tables(context.Background(), inst, "shop", "select 1 from t"); again[0].Name != "t" {
		t.Error("返回的切片应是副本")
	}
	// 失败不缓存
	failing := &countingExtractor{err: errors.New("inception down")}
	fm := newMemoExtractor(failing)
	fm.Tables(context.Background(), inst, "shop", "x")
	fm.Tables(context.Background(), inst, "shop", "x")
	if failing.n != 2 {
		t.Errorf("失败不应缓存: %d", failing.n)
	}
}

// 总开关没开时，只有防火墙要求强制脱敏的列被隐藏；已配置的脱敏规则仍不生效（总开关就是这个意思）。
func TestFirewallForcedMaskDoesNotActivateConfiguredRulesWhenMasterOff(t *testing.T) {
	repo := newRepo()
	repo.maskCols = []model.DataMaskingColumn{keepCfg("shop", "users", "email", 2, 0)} // 已配置，但 data_masking 没开
	eng := &scriptEngine{script: map[string]*engine.ResultSet{
		"select": {ColumnList: []string{"id", "phone", "email"}, Rows: [][]any{{1, "13812345678", "zhangsan@corp.com"}}},
	}}
	rule := fwRule(1, "手机号强制脱敏", firewall.ActMask, firewall.Condition{T: firewall.CondObject, V: []string{"*.*.phone"}})
	s, _ := fwService(repo, eng, rule)
	res, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id, phone, email from users"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][1] == "13812345678" {
		t.Errorf("强制脱敏的列应被隐藏: %v", res.Rows[0])
	}
	if res.Rows[0][2] != "zhangsan@corp.com" {
		t.Errorf("总开关没开：已配置的 email 规则不应生效: %v", res.Rows[0])
	}
	// 总开关开着：两者都生效
	repo.cfg[settings.KeyQueryMaskEnabled] = "true"
	eng.script["select"] = &engine.ResultSet{ColumnList: []string{"id", "phone", "email"}, Rows: [][]any{{1, "13812345678", "zhangsan@corp.com"}}}
	res, err = s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select id, phone, email from users"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][1] == "13812345678" || res.Rows[0][2] == "zhangsan@corp.com" {
		t.Errorf("总开关开着：强制脱敏与已配置规则都应生效: %v", res.Rows[0])
	}
}

// ---- scan_rows：EXPLAIN 事实 ----

func planResult(rows int64) *engine.ResultSet {
	return &engine.ResultSet{
		ColumnList: []string{"id", "select_type", "table", "type", "key", "rows", "filtered", "Extra"},
		Rows:       [][]any{{1, "SIMPLE", "orders", "ALL", nil, rows, 100.0, nil}},
	}
}

func scanRule(id uint, name, action string, n int64) firewall.Rule {
	return fwRule(id, name, action, firewall.Condition{T: firewall.CondScanRows, V: n})
}

func (e *scriptEngine) explains() []string {
	var out []string
	for _, q := range e.got {
		if strings.HasPrefix(q, "EXPLAIN ") {
			out = append(out, q)
		}
	}
	return out
}

func TestFirewallScanRowsUsesExplain(t *testing.T) {
	repo := newRepo()
	eng := &scriptEngine{script: map[string]*engine.ResultSet{"EXPLAIN": planResult(5_000_000)}, fakeEngine: fakeEngine{rows: 3}}
	rule := scanRule(1, "大范围扫描", firewall.ActDeny, 1_000_000)
	rule.Message = "预计扫描行数过多，请加条件"
	s, st := fwService(repo, eng, rule)

	_, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select * from orders"})
	if code(err) != "firewall_denied" {
		t.Fatalf("预估扫描 500 万行应被拒绝: %v", err)
	}
	if ex := eng.explains(); len(ex) != 1 || ex[0] != "EXPLAIN select * from orders" {
		t.Errorf("应对用户语句（不是改写 LIMIT 之后的）EXPLAIN 一次: %v", eng.explains())
	}
	if len(st.hits) != 1 || !strings.Contains(st.hits[0].Matched, "预估扫描 5000000 行") {
		t.Errorf("命中说明应带预估扫描行数: %+v", st.hits)
	}
	if len(eng.got) != 1 {
		t.Errorf("被拒绝后不应再执行语句本身: %v", eng.got)
	}

	// 小表：不命中，语句照常执行；同一语句 10 秒内不再 EXPLAIN
	eng.script["EXPLAIN"] = planResult(20)
	s, _ = fwService(repo, eng, rule)
	res, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select * from orders where id = 9"})
	if err != nil || res.Firewall != nil {
		t.Errorf("扫描行数小于阈值应放行: %v %+v", err, res)
	}
}

func TestFirewallScanRowsDoesNotProbeUnauthorizedTables(t *testing.T) {
	repo := &evRepo{histRepo: &histRepo{fakeRepo: devRepo()}, ready: true}
	eng := &scriptEngine{script: map[string]*engine.ResultSet{"EXPLAIN": planResult(9_000_000)}, fakeEngine: fakeEngine{rows: 1}}
	rule := scanRule(1, "大表要原因", firewall.ActReason, 1_000_000)
	s, st := fwService(repo, eng, rule)

	// dev 对 secret 库没有权限：不能从“要求填写原因”里探测到这张表存在 / 很大，也不应向目标库下发 EXPLAIN
	_, err := s.Execute(context.Background(), fwDev, Request{InstanceID: 1, DB: "secret", SQL: "select * from merchant_wallet"})
	var qe *Error
	if !errors.As(err, &qe) || qe.Code == "reason_required" || qe.Code == "firewall_denied" {
		t.Fatalf("没有权限的语句应得到权限错误，而不是防火墙的提示: %v", err)
	}
	if len(eng.explains()) != 0 || len(st.hits) != 0 {
		t.Errorf("没有权限不应 EXPLAIN 也不应记命中: explains=%v hits=%+v", eng.explains(), st.hits)
	}
}

type explainFailEngine struct {
	scriptEngine
	failExplain error
}

func (e *explainFailEngine) Query(ctx context.Context, sql string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	if strings.HasPrefix(sql, "EXPLAIN ") && e.failExplain != nil {
		e.got = append(e.got, sql)
		return nil, e.failExplain
	}
	return e.scriptEngine.Query(ctx, sql, opt)
}

func TestFirewallScanRowsUnknownWhenExplainFails(t *testing.T) {
	repo := newRepo()
	eng := &explainFailEngine{scriptEngine: scriptEngine{fakeEngine: fakeEngine{rows: 2}}, failExplain: errors.New("Error 1146: Table 'shop.orders' doesn't exist")}
	rule := scanRule(1, "大范围扫描", firewall.ActDeny, 1000)
	s, st := fwService(repo, eng, rule)
	res, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "select * from orders"})
	if err != nil || res == nil {
		t.Fatalf("EXPLAIN 失败不能误拦：条件按未评估处理，语句照常执行: %v", err)
	}
	if len(st.hits) != 0 {
		t.Errorf("未评估不命中也不记录: %+v", st.hits)
	}
}

func TestFirewallScanRowsOnlyForSelectOnMySQLFamily(t *testing.T) {
	repo := newRepo()
	eng := &scriptEngine{script: map[string]*engine.ResultSet{"EXPLAIN": planResult(9_000_000), "SHOW": {ColumnList: []string{"Tables_in_shop"}, Rows: [][]any{{"orders"}}}}}
	rule := scanRule(1, "大范围扫描", firewall.ActDeny, 1000)
	s, _ := fwService(repo, eng, rule)
	// SHOW / DESC：没有扫描行数这个概念，不下发 EXPLAIN
	if _, err := s.Execute(context.Background(), admin, Request{InstanceID: 1, DB: "shop", SQL: "show tables"}); err != nil {
		t.Fatalf("show tables: %v", err)
	}
	if len(eng.explains()) != 0 {
		t.Errorf("SHOW 不应 EXPLAIN: %v", eng.explains())
	}
}
