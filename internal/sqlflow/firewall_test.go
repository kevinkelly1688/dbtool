// @author: kevinkelly
// @license: Apache Licence
// @file: firewall_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/firewall"
	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/settings"
	"dbtool/internal/sqlflow"
)

// SQL 防火墙在工单与导出里的接线。

// ---- 假存储 ----

type fwStore struct {
	mu     sync.Mutex
	rules  []firewall.Rule
	excs   []firewall.Exception
	hits   []firewall.HitRow
	groups map[string][]uint // 用户名 → 权限组 ID
}

func (f *fwStore) LoadRules() ([]firewall.Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]firewall.Rule(nil), f.rules...), nil
}
func (f *fwStore) LoadExceptions(time.Time) ([]firewall.Exception, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]firewall.Exception(nil), f.excs...), nil
}
func (f *fwStore) UserGroupIDs(uint) ([]uint, error) { return nil, nil }
func (f *fwStore) UserGroupIDsByName(name string) ([]uint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groups[name], nil
}
func (f *fwStore) RecordHit(h firewall.HitRow, _ time.Duration) (uint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, h)
	return uint(len(f.hits)), nil
}
func (f *fwStore) AttachQueryLog([]uint, uint) error { return nil }

func (f *fwStore) hitsOf(scene string) []firewall.HitRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []firewall.HitRow
	for _, h := range f.hits {
		if h.Scene == scene {
			out = append(out, h)
		}
	}
	return out
}

type fwEnv struct {
	st  *fwStore
	eng *firewall.Engine
}

var fwSeq uint

func fwRule(name, action string, scenes []string, conds ...firewall.Condition) firewall.Rule {
	fwSeq++
	return firewall.Rule{ID: fwSeq, Name: name, Priority: int(fwSeq), Enabled: true, Mode: firewall.ModeEnforce, Scenes: scenes, Env: "all",
		GroupMode: "all", Match: "all", Conditions: conds, Action: action, Message: name + "：请调整语句"}
}

func kwCond(words ...string) firewall.Condition {
	return firewall.Condition{T: firewall.CondKeyword, V: words}
}

func useFirewall(svc *sqlflow.Service, cfg firewall.ConfigSource, rules ...firewall.Rule) *fwEnv {
	st := &fwStore{rules: rules, groups: map[string][]uint{}}
	eng := firewall.New(st, cfg)
	svc.SetFirewall(eng)
	return &fwEnv{st: st, eng: eng}
}

func (f *fwEnv) setRules(rules ...firewall.Rule) {
	f.st.mu.Lock()
	f.st.rules = rules
	f.st.mu.Unlock()
	f.eng.Invalidate()
}

var wfScene = []string{firewall.SceneWorkflow}

// ---- 提交检查 ----

func TestFirewallDenyMarksCheckRowAndAutoRejects(t *testing.T) {
	e := setup(t)
	fw := useFirewall(e.svc, e.repo, fwRule("禁止整表删除", firewall.ActDeny, wfScene,
		firewall.Condition{T: firewall.CondStmt, V: []string{"DELETE"}}, firewall.Condition{T: firewall.CondFeature, V: firewall.FeatNoWhere}))
	ctx := context.Background()

	res, err := e.svc.Preview(ctx, dev, 1, "shop", "delete from orders", false)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Rows[0]
	if r.ErrLevel != review.LevelError || r.StageStatus != "驳回高危SQL（防火墙）" || !strings.Contains(r.ErrorMessage, "禁止整表删除") {
		t.Fatalf("应标为错误并写明规则: %+v", r)
	}
	if res.ErrorCount != 1 {
		t.Errorf("错误数 = %d", res.ErrorCount)
	}
	if hits := fw.st.hitsOf("workflow"); len(hits) != 1 || hits[0].Effective != "deny" || hits[0].UserName != "dev" || hits[0].InstanceName != "prod-mysql-01" || hits[0].DB != "shop" {
		t.Errorf("命中记录: %+v", fw.st.hits)
	}

	// 带 WHERE：不命中
	ok, err := e.svc.Preview(ctx, dev, 1, "shop", "delete from orders where id = 1", false)
	if err != nil || ok.Rows[0].ErrLevel != review.LevelOK || ok.ErrorCount != 0 {
		t.Errorf("带 WHERE 应放行: %+v %v", ok.Rows, err)
	}

	// 提交：走现有的“检查有错误 → 系统自动驳回”
	sub, err := e.svc.Submit(ctx, dev, req("delete from orders"))
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != model.WfAutoReviewWrong || !strings.Contains(sub.Message, "审核不通过") {
		t.Errorf("提交结果: %+v", sub)
	}
}

func TestFirewallWarnIsNotAutoRejected(t *testing.T) {
	e := setup(t)
	useFirewall(e.svc, e.repo, fwRule("大表提醒", firewall.ActWarn, wfScene, kwCond("big_table")))
	e.repo.Cfg[settings.KeyReviewAutoRejectLevel] = "1" // 站点配置：出现警告就驳回
	res, err := e.svc.Preview(context.Background(), dev, 1, "shop", "update big_table set a = 1 where id = 1", false)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Rows[0]
	if r.ErrLevel != review.LevelWarning || r.StageStatus != "SQL 防火墙提示" || !strings.Contains(r.ErrorMessage, "大表提醒") {
		t.Fatalf("应标为警告: %+v", r)
	}
	sub, err := e.svc.Submit(context.Background(), dev, req("update big_table set a = 1 where id = 1"))
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status == model.WfAutoReviewWrong {
		t.Errorf("防火墙的“仅告警”只是提示，不应让“警告就驳回”的配置把工单驳回: %+v", sub)
	}
}

func TestFirewallObserveRecordsButDoesNotMark(t *testing.T) {
	e := setup(t)
	r := fwRule("观察中的规则", firewall.ActDeny, wfScene, kwCond("orders"))
	r.Mode = firewall.ModeObserve
	fw := useFirewall(e.svc, e.repo, r)
	res, err := e.svc.Preview(context.Background(), dev, 1, "shop", "delete from orders where id = 1", false)
	if err != nil || res.ErrorCount != 0 || res.Rows[0].ErrLevel != review.LevelOK {
		t.Fatalf("观察态不改变检查结果: %+v %v", res, err)
	}
	hits := fw.st.hitsOf("workflow")
	if len(hits) != 1 || hits[0].Effective != "observe" || hits[0].WouldAction != "deny" {
		t.Errorf("观察命中应记录“若生效会拒绝”: %+v", hits)
	}
}

func TestFirewallDoesNotOverrideCriticalDDLRegex(t *testing.T) {
	e := setup(t)
	e.repo.Cfg[settings.KeyReviewCriticalDDLRegex] = "^drop"
	useFirewall(e.svc, e.repo, fwRule("禁止 DROP", firewall.ActDeny, wfScene, firewall.Condition{T: firewall.CondStmt, V: []string{"DROP"}}))
	res, err := e.svc.Preview(context.Background(), dev, 1, "shop", "drop table t", false)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Rows[0]
	if r.ErrLevel != review.LevelError || r.StageStatus != "驳回高危SQL" || !strings.Contains(r.ErrorMessage, "禁止提交匹配") {
		t.Errorf("两者都命中时以 critical_ddl_regex 的文案为准: %+v", r)
	}
}

func TestFirewallTightenActionsDowngradeToWarnInWorkflow(t *testing.T) {
	e := setup(t)
	lim := fwRule("限行", firewall.ActLimit, []string{firewall.SceneQuery, firewall.SceneWorkflow}, kwCond("orders"))
	lim.ActionArg = 100
	useFirewall(e.svc, e.repo, lim)
	res, err := e.svc.Preview(context.Background(), dev, 1, "shop", "update orders set a = 1 where id = 1", false)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Rows[0]
	if r.ErrLevel != review.LevelWarning || !strings.Contains(r.ErrorMessage, "已降级为仅告警") {
		t.Errorf("工单里没有限行能力，应降级为告警并写明: %+v", r)
	}
}

func TestFirewallSkipsManualWorkflows(t *testing.T) {
	e := setup(t)
	fw := useFirewall(e.svc, e.repo, fwRule("全拒", firewall.ActDeny, wfScene, kwCond("orders")))
	r := req("delete from orders")
	r.SkipCheck = true                                      // 线下手工执行：系统不检查也不执行
	sub, err := e.svc.Submit(context.Background(), root, r) // 只有管理员能跳过语法检查提交
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status == model.WfAutoReviewWrong || len(fw.st.hits) != 0 {
		t.Errorf("手工工单不进防火墙: %+v hits=%d", sub, len(fw.st.hits))
	}
}

func TestFirewallScopeEnvAndInstance(t *testing.T) {
	e := setup(t)
	r := fwRule("仅测试环境", firewall.ActDeny, wfScene, kwCond("orders"))
	r.Env = "test"
	useFirewall(e.svc, e.repo, r)
	// 实例 1 只有 can_write 标签，没有环境：不被“仅测试”规则覆盖
	res, err := e.svc.Preview(context.Background(), dev, 1, "shop", "update orders set a = 1 where id = 1", false)
	if err != nil || res.ErrorCount != 0 {
		t.Errorf("没有环境的实例不应命中仅测试规则: %+v %v", res, err)
	}
}

// ---- 执行前复核 ----

func TestFirewallRecheckBlocksExecutionAfterRuleChange(t *testing.T) {
	e := setup(t)
	fw := useFirewall(e.svc, e.repo) // 提交时还没有规则
	id := e.submit(t, dev, "update orders set a = 1 where id = 1")
	e.approve(t, id)
	// 审批期间新增了一条规则
	fw.setRules(fwRule("冻结 orders 表", firewall.ActDeny, wfScene, kwCond("orders")))

	if _, err := e.svc.Execute(context.Background(), dba, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if e.status(id) != model.WfException {
		t.Fatalf("被复核拒绝的工单应进入执行有异常: %s", e.status(id))
	}
	rows, _ := review.ParseRows(e.repo.Contents[id].ExecuteResult)
	if len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "执行前复核") || !strings.Contains(rows[0].ErrorMessage, "冻结 orders 表") ||
		!strings.Contains(rows[0].ErrorMessage, "没有执行任何语句") {
		t.Errorf("应写明规则与没有执行: %+v", rows)
	}
	if len(e.eng.ran) != 0 {
		t.Errorf("不应执行任何语句: %v", e.eng.ran)
	}
	if hits := fw.st.hitsOf("workflow"); len(hits) != 1 || hits[0].Effective != "deny" || hits[0].UserName != "dev" {
		t.Errorf("复核拒绝应记录命中，用户是提交人（不是点执行的人）: %+v", fw.st.hits)
	}
}

func TestFirewallRecheckOnlyHonoursDeny(t *testing.T) {
	e := setup(t)
	obs := fwRule("观察中", firewall.ActDeny, wfScene, kwCond("orders"))
	obs.Mode = firewall.ModeObserve
	warn := fwRule("提醒", firewall.ActWarn, wfScene, kwCond("orders"))
	fw := useFirewall(e.svc, e.repo)
	id := e.submit(t, dev, "update orders set a = 1 where id = 1")
	e.approve(t, id)
	fw.setRules(obs, warn)
	if _, err := e.svc.Execute(context.Background(), dba, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if e.status(id) != model.WfFinish || len(e.eng.ran) != 1 {
		t.Errorf("观察与告警不应拦截执行: %s ran=%v", e.status(id), e.eng.ran)
	}
}

func TestFirewallRecheckSubjectIsSubmitterGroupsAndExceptions(t *testing.T) {
	e := setup(t)
	fw := useFirewall(e.svc, e.repo)
	fw.st.groups["dev"] = []uint{5} // 提交人 dev 在权限组 5；执行人 dba 不在
	id := e.submit(t, dev, "update orders set a = 1 where id = 1")
	e.approve(t, id)

	only := fwRule("仅研发组受限", firewall.ActDeny, wfScene, kwCond("orders"))
	only.GroupMode, only.GroupIDs = "only", []uint{5}
	fw.setRules(only)
	_, _ = e.svc.Execute(context.Background(), dba, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	e.svc.Wait()
	if e.status(id) != model.WfException || len(e.eng.ran) != 0 {
		t.Fatalf("规则看的是提交人所在的组，换 DBA 点执行不能绕过: %s ran=%v", e.status(id), e.eng.ran)
	}

	// 为提交人添加例外后，重新提交就能执行
	fw.st.mu.Lock()
	fw.st.excs = []firewall.Exception{{ID: 1, RuleID: only.ID, SubjectKind: firewall.SubjectUser, SubjectValue: "dev", Reason: "有备案", ExpiresAt: time.Now().Add(24 * time.Hour)}} // 防火墙引擎用真实时钟
	fw.st.mu.Unlock()
	fw.eng.Invalidate()
	id2 := e.submit(t, dev, "update orders set a = 2 where id = 1")
	e.approve(t, id2)
	if _, err := e.svc.Execute(context.Background(), dba, id2, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if e.status(id2) != model.WfFinish || len(e.eng.ran) != 1 {
		t.Errorf("例外生效后应能执行: %s ran=%v", e.status(id2), e.eng.ran)
	}
}

func TestFirewallRecheckUsesStoredAffectedRows(t *testing.T) {
	e := setup(t)
	e.useInception()
	e.inc.checkOut = inceptionRows("use `shop`", "update orders set a = 1")
	e.inc.checkOut[1].AffectedRows = 500000
	big := fwRule("影响行数过多", firewall.ActDeny, wfScene, firewall.Condition{T: firewall.CondAffectRows, V: 100000})
	fw := useFirewall(e.svc, e.repo)
	id := e.submit(t, dev, "update orders set a = 1")
	e.approve(t, id)
	fw.setRules(big)
	e.inc.execOut = []review.Row{{ID: 1, Stage: "EXECUTED", StageStatus: "Execute Successfully", ErrorMessage: "None", SQL: "update orders set a = 1", AffectedRows: 500000, BackupDBName: "None"}}
	if _, err := e.svc.Execute(context.Background(), dba, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if e.status(id) != model.WfException {
		t.Errorf("提交时检查结果里的影响行数应带到复核里: %s", e.status(id))
	}
	for _, c := range e.inc.calls {
		if strings.HasPrefix(c, "execute:") {
			t.Errorf("被拒绝后不应交给 goInception 执行: %v", e.inc.calls)
		}
	}
}

// ---- 数据导出 ----

var expScene = []string{firewall.SceneExport}

func TestFirewallExportDenyAtCheck(t *testing.T) {
	xe := setupExport(t)
	fw := useFirewall(xe.svc, xe.repo, fwRule("不许导出订单", firewall.ActDeny, expScene, kwCond("orders")))
	res, err := xe.svc.PreviewExport(context.Background(), dev, []uint{1}, false, "shop", exportSQL)
	if err != nil {
		t.Fatal(err)
	}
	if res.ErrorCount != 1 || res.Rows[0].StageStatus != "驳回高危SQL（防火墙）" || !strings.Contains(res.Rows[0].ErrorMessage, "不许导出订单") {
		t.Fatalf("应检查不通过并写明规则: %+v", res.Rows)
	}
	for _, q := range xe.hosts["my1"].queries {
		if strings.HasPrefix(q, "SELECT COUNT(*)") {
			t.Errorf("被拒绝后不必再统计行数: %v", xe.hosts["my1"].queries)
		}
	}
	if hits := fw.st.hitsOf("export"); len(hits) == 0 || hits[0].Effective != "deny" {
		t.Errorf("命中记录: %+v", fw.st.hits)
	}
	sub, err := xe.svc.Submit(context.Background(), dev, xe.req(exportSQL, "csv", 1))
	if err != nil || sub.Status != model.WfAutoReviewWrong {
		t.Errorf("提交应被系统驳回: %+v %v", sub, err)
	}
}

func TestFirewallExportLimitTakesTheSmaller(t *testing.T) {
	xe := setupExport(t)
	lim := fwRule("导出最多 1 行", firewall.ActLimit, expScene, kwCond("orders"))
	lim.ActionArg = 1
	fw := useFirewall(xe.svc, xe.repo, lim)
	// 检查：行数 2 超过防火墙的 1 行上限（即使导出自己的上限是 10000）
	res, err := xe.svc.PreviewExport(context.Background(), dev, []uint{1}, false, "shop", exportSQL)
	if err != nil || res.ErrorCount != 1 || !strings.Contains(res.Rows[0].ErrorMessage, "导出最多 1 行") {
		t.Fatalf("检查应按防火墙的上限判错: %+v %v", res.Rows, err)
	}
	// 执行：提交时没有规则，审批期间新增，执行前复核按新上限中止
	fw.setRules()
	id := xe.submit(t, xe.req(exportSQL, "csv", 1))
	fw.setRules(lim)
	xe.run(t, id)
	if xe.status(id) == model.WfFinish || fileOf(xe, id) != "" {
		t.Fatalf("超过防火墙上限应中止导出: %s", xe.status(id))
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if len(rows) == 0 || !strings.Contains(rows[0].ErrorMessage, "导出最多 1 行") {
		t.Errorf("应写明规则: %+v", rows)
	}
}

type forcedMasker struct {
	fakeMasker
	forced     int
	configured bool
	force      []firewall.ObjectRef
}

func (m *forcedMasker) MaskExportForced(_ context.Context, _ *model.Instance, _, _ string, _ engine.Engine, rs *engine.ResultSet, force []firewall.ObjectRef, configured bool) ([]string, error) {
	m.forced++
	m.configured, m.force = configured, force
	for i := range rs.Rows {
		rs.Rows[i] = append([]any(nil), rs.Rows[i]...)
		rs.Rows[i][1] = "张***"
	}
	return []string{"name"}, nil
}

func TestFirewallExportForcedMaskWorksWithMasterSwitchOff(t *testing.T) {
	xe := setupExport(t) // data_masking 没开
	mask := fwRule("姓名强制脱敏", firewall.ActMask, expScene, firewall.Condition{T: firewall.CondObject, V: []string{"*.*.name"}})
	fw := useFirewall(xe.svc, xe.repo)
	m := &forcedMasker{}
	xe.svc.SetResultMasker(m)
	id := xe.submit(t, xe.req(exportSQL, "csv", 1))
	fw.setRules(mask)
	xe.run(t, id)
	if xe.status(id) != model.WfFinish {
		t.Fatalf("状态: %s", xe.status(id))
	}
	if m.forced != 1 || m.configured || m.calls != 0 || len(m.force) != 1 || m.force[0].Column != "name" {
		t.Errorf("总开关没开：只做强制脱敏，不做已配置的: %+v", m)
	}
	var content string
	for _, c := range xe.zipEntries(t, fileOf(xe, id)) {
		content = c
	}
	if !strings.Contains(content, "张***") || strings.Contains(content, "张三") {
		t.Errorf("导出内容应已脱敏: %q", content)
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "已脱敏：name") {
		t.Errorf("应说明已脱敏的列: %+v", rows)
	}
}

func TestFirewallExportForcedMaskFailsClosedWithoutCapableMasker(t *testing.T) {
	xe := setupExport(t)
	mask := fwRule("姓名强制脱敏", firewall.ActMask, expScene, firewall.Condition{T: firewall.CondObject, V: []string{"*.*.name"}})
	fw := useFirewall(xe.svc, xe.repo)
	id := xe.submit(t, xe.req(exportSQL, "csv", 1)) // 没有设置脱敏组件
	fw.setRules(mask)
	xe.run(t, id)
	if xe.status(id) == model.WfFinish || fileOf(xe, id) != "" {
		t.Fatalf("做不了强制脱敏就不能导出: %s", xe.status(id))
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if len(rows) == 0 || !strings.Contains(rows[0].ErrorMessage, "不会导出未脱敏的数据") {
		t.Errorf("应说明原因: %+v", rows)
	}
}

func TestFirewallExportBatchRecheckIsPerInstance(t *testing.T) {
	xe := setupExport(t)
	fw := useFirewall(xe.svc, xe.repo)
	id := xe.submit(t, xe.req(exportSQL, "csv", 11, 12))
	deny := fwRule("12 号实例禁止导出", firewall.ActDeny, expScene, kwCond("orders"))
	deny.InstanceIDs = []uint{12}
	fw.setRules(deny)
	xe.run(t, id)
	if xe.status(id) != model.WfException {
		t.Fatalf("部分实例失败应为执行有异常: %s", xe.status(id))
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	var failed, ok int
	for _, r := range rows {
		switch {
		case r.IsSummary:
		case r.InstanceID == 12 && r.ErrLevel == review.LevelError && strings.Contains(r.ErrorMessage, "执行前复核"):
			failed++
		case r.InstanceID == 11 && r.ErrLevel == review.LevelOK:
			ok++
		}
	}
	if failed != 1 || ok != 1 {
		t.Errorf("应只有 12 号实例被拒绝，11 号照常导出: %+v", rows)
	}
	if fileOf(xe, id) == "" || len(xe.zipEntries(t, fileOf(xe, id))) != 1 {
		t.Errorf("已成功实例的文件仍应可下载")
	}
}

func TestFirewallExportWarnIsNotAutoRejected(t *testing.T) {
	xe := setupExport(t)
	useFirewall(xe.svc, xe.repo, fwRule("导出提醒", firewall.ActWarn, expScene, kwCond("orders")))
	xe.repo.Cfg[settings.KeyReviewAutoRejectLevel] = "1"
	res, err := xe.svc.PreviewExport(context.Background(), dev, []uint{1}, false, "shop", exportSQL)
	if err != nil || res.ErrorCount != 0 || res.Rows[0].StageStatus != "SQL 防火墙提示" || res.Rows[0].ErrLevel != review.LevelWarning {
		t.Fatalf("应只是警告: %+v %v", res.Rows, err)
	}
	sub, err := xe.svc.Submit(context.Background(), dev, xe.req(exportSQL, "csv", 1))
	if err != nil || sub.Status == model.WfAutoReviewWrong {
		t.Errorf("仅告警不应让工单被驳回: %+v %v", sub, err)
	}
}

func TestFirewallDisabledLeavesWorkflowsUntouched(t *testing.T) {
	e := setup(t)
	// 没有 SetFirewall：行为与以前完全一致
	res, err := e.svc.Preview(context.Background(), dev, 1, "shop", "delete from orders", false)
	if err != nil || res.ErrorCount != 0 {
		t.Errorf("未接入防火墙不应有任何变化: %+v %v", res, err)
	}
	// 接入但总开关关闭
	fw := useFirewall(e.svc, e.repo, fwRule("全拒", firewall.ActDeny, wfScene, kwCond("orders")))
	e.repo.Cfg[settings.KeyFirewallEnabled] = "false"
	fw.eng.Invalidate()
	res, err = e.svc.Preview(context.Background(), dev, 1, "shop", "delete from orders", false)
	if err != nil || res.ErrorCount != 0 || len(fw.st.hits) != 0 {
		t.Errorf("总开关关闭应放行且不记录: %+v %v hits=%d", res, err, len(fw.st.hits))
	}
}

// 一个脚本里成百上千条同一形态的语句：命中只记一次，不把命中表写爆。
func TestFirewallBulkStatementsRecordHitsOnce(t *testing.T) {
	e := setup(t)
	fw := useFirewall(e.svc, e.repo, fwRule("改 orders 提醒", firewall.ActWarn, wfScene, kwCond("orders")))
	var b strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "update orders set status = 3 where id = %d;\n", i)
	}
	res, err := e.svc.Preview(context.Background(), dev, 1, "shop", b.String(), false)
	if err != nil {
		t.Fatal(err)
	}
	warned := 0
	for _, r := range res.Rows {
		if r.ErrLevel == review.LevelWarning {
			warned++
		}
	}
	if warned != 300 {
		t.Errorf("每条语句都应带提示: %d", warned)
	}
	if hits := fw.st.hitsOf("workflow"); len(hits) != 1 {
		t.Errorf("同形态语句只记一次命中: %d", len(hits))
	}
}

// ---- scan_rows / affect_rows：EXPLAIN 事实 ----

type planState struct {
	mu    sync.Mutex
	plan  *engine.ResultSet
	err   error
	calls []string
	dbs   []string
}

func planOf(rows int64) *engine.ResultSet {
	return &engine.ResultSet{
		ColumnList: []string{"id", "select_type", "table", "type", "key", "rows", "filtered", "Extra"},
		Rows:       [][]any{{1, "SIMPLE", "orders", "ALL", nil, rows, 100.0, nil}},
	}
}

type planEngine struct {
	*fakeEngine
	ps *planState
}

func (p *planEngine) Query(_ context.Context, q string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	p.ps.mu.Lock()
	defer p.ps.mu.Unlock()
	p.ps.calls = append(p.ps.calls, q)
	p.ps.dbs = append(p.ps.dbs, opt.DBName)
	if p.ps.err != nil {
		return nil, p.ps.err
	}
	return p.ps.plan, nil
}

// withPlans 用一个能回答 EXPLAIN 的假引擎重建工单服务（setup 里的假引擎没有 Query）。
func withPlans(e *env, ps *planState) {
	s := sqlflow.NewService(e.repo, sqlflow.Options{Notifier: e.cap, Open: func(string, engine.Conn) (engine.Engine, error) {
		return &planEngine{fakeEngine: &fakeEngine{state: e.eng}, ps: ps}, nil
	}})
	s.SetAsync(func(f func()) { f() })
	s.SetSpawn(func(f func()) { f() })
	s.SetClock(func() time.Time { return e.now })
	s.BaseURL = "http://dba.example.com"
	e.svc = s
}

func scanDeny(n int64, scenes []string, conds ...firewall.Condition) firewall.Rule {
	conds = append(conds, firewall.Condition{T: firewall.CondScanRows, V: n})
	return fwRule("预估扫描行数过多", firewall.ActDeny, scenes, conds...)
}

func TestFirewallWorkflowScanRowsUsesExplainOnTargetInstance(t *testing.T) {
	e := setup(t)
	ps := &planState{plan: planOf(500000)}
	withPlans(e, ps)
	fw := useFirewall(e.svc, e.repo, scanDeny(100000, wfScene))
	ctx := context.Background()

	res, err := e.svc.Preview(ctx, dev, 1, "shop", "update orders set status = 3", false)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Rows[0]
	if r.ErrLevel != review.LevelError || !strings.Contains(r.ErrorMessage, "预估扫描行数过多") {
		t.Fatalf("预估扫描 50 万行应拒绝: %+v", r)
	}
	if len(ps.calls) != 1 || ps.calls[0] != "EXPLAIN update orders set status = 3" || ps.dbs[0] != "shop" {
		t.Errorf("应在目标库对语句 EXPLAIN 一次: %v %v", ps.calls, ps.dbs)
	}
	if hits := fw.st.hitsOf("workflow"); len(hits) != 1 || !strings.Contains(hits[0].Matched, "预估扫描 500000 行") {
		t.Errorf("命中说明: %+v", fw.st.hits)
	}

	// 取不到事实（EXPLAIN 出错）：未评估，不误拦
	fw.eng.Invalidate()
	ps.err = errors.New("Error 1146: Table doesn't exist")
	res, err = e.svc.Preview(ctx, dev, 1, "shop", "update orders set status = 4", false)
	if err != nil || res.ErrorCount != 0 {
		t.Errorf("EXPLAIN 失败应按未评估放行: %+v %v", res, err)
	}
	// DDL 不支持 EXPLAIN：不下发
	ps.err = nil
	n := len(ps.calls)
	res, _ = e.svc.Preview(ctx, dev, 1, "shop", "alter table orders add c int", false)
	if len(ps.calls) != n || res.ErrorCount != 0 {
		t.Errorf("DDL 不应 EXPLAIN: %v", ps.calls[n:])
	}
}

func TestFirewallWorkflowExplainBudgetPerCheck(t *testing.T) {
	e := setup(t)
	ps := &planState{plan: planOf(10)}
	withPlans(e, ps)
	useFirewall(e.svc, e.repo, scanDeny(100000, wfScene, kwCond("orders")))
	var b strings.Builder
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&b, "update orders set status = %d where id = %d;\n", i, i) // 每条文本都不同，不会命中 EXPLAIN 缓存
	}
	if _, err := e.svc.Preview(context.Background(), dev, 1, "shop", b.String(), false); err != nil {
		t.Fatal(err)
	}
	if len(ps.calls) > 50 || len(ps.calls) == 0 {
		t.Errorf("一次检查最多下发 50 次 EXPLAIN: %d", len(ps.calls))
	}
}

func TestFirewallWorkflowAffectRowsFallsBackToExplainForNativeCheck(t *testing.T) {
	e := setup(t) // 没有 goInception：原生降级检查没有影响行数
	ps := &planState{plan: planOf(80000)}
	withPlans(e, ps)
	big := fwRule("影响行数过多", firewall.ActDeny, wfScene, firewall.Condition{T: firewall.CondAffectRows, V: 50000})
	useFirewall(e.svc, e.repo, big)
	res, err := e.svc.Preview(context.Background(), dev, 1, "shop", "delete from orders where created < '2020-01-01'", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0].ErrLevel != review.LevelError || len(ps.calls) != 1 {
		t.Errorf("没有检查结果的影响行数时，UPDATE / DELETE 应退回 EXPLAIN 上界: %+v calls=%v", res.Rows[0], ps.calls)
	}
	// INSERT ... VALUES 没有可估的扫描行数：影响行数未知，不命中
	ps.plan = planOf(0)
	res, _ = e.svc.Preview(context.Background(), dev, 1, "shop", "insert into orders (a) values (1)", false)
	if res.ErrorCount != 0 {
		t.Errorf("INSERT 的影响行数未知: %+v", res.Rows)
	}
}

func TestFirewallWorkflowRecheckEvaluatesScanRows(t *testing.T) {
	e := setup(t)
	ps := &planState{plan: planOf(500000)}
	withPlans(e, ps)
	fw := useFirewall(e.svc, e.repo)
	id := e.submit(t, dev, "update orders set status = 3")
	e.approve(t, id)
	fw.setRules(scanDeny(100000, wfScene))
	if _, err := e.svc.Execute(context.Background(), dba, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != model.WfException || len(e.eng.ran) != 0 {
		t.Fatalf("执行前复核应按 EXPLAIN 的扫描行数拒绝: %s ran=%v", e.status(id), e.eng.ran)
	}
	rows, _ := review.ParseRows(e.repo.Contents[id].ExecuteResult)
	if len(rows) == 0 || !strings.Contains(rows[0].ErrorMessage, "预估扫描行数过多") {
		t.Errorf("应写明规则: %+v", rows)
	}
}

func TestFirewallExportScanRows(t *testing.T) {
	xe := setupExport(t)
	xe.hosts["my1"].plan = planOf(8_000_000)
	useFirewall(xe.svc, xe.repo, scanDeny(1_000_000, expScene))
	res, err := xe.svc.PreviewExport(context.Background(), dev, []uint{1}, false, "shop", exportSQL)
	if err != nil {
		t.Fatal(err)
	}
	if res.ErrorCount != 1 || !strings.Contains(res.Rows[0].ErrorMessage, "预估扫描行数过多") {
		t.Fatalf("导出的查询预估扫描 800 万行应检查不通过: %+v", res.Rows)
	}
	// 小范围：通过
	xe.hosts["my1"].plan = planOf(300)
	useFirewall(xe.svc, xe.repo, scanDeny(1_000_000, expScene)) // 新引擎：不沿用上一次的 EXPLAIN 缓存
	res, err = xe.svc.PreviewExport(context.Background(), dev, []uint{1}, false, "shop", exportSQL)
	if err != nil || res.ErrorCount != 0 {
		t.Errorf("小范围应通过: %+v %v", res.Rows, err)
	}
}
