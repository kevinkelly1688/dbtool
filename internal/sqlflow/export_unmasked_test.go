// @author: kevinkelly
// @license: Apache Licence
// @file: export_unmasked_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"strings"
	"testing"

	"dbtool/internal/firewall"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/review"
	"dbtool/internal/settings"
	"dbtool/internal/sqlflow"
)

// 数据导出“导出原值（不脱敏）”：仅超级用户在提交时选择、必须写理由；执行时复核提交人仍是超级用户；SQL 防火墙强制脱敏不放开。

const unmaskReason = "法务取证需要完整的原始数据"

// unmaskEnv 在导出场景上打开脱敏总开关，并让 root（超级用户）成为一个启用的用户。
func unmaskEnv(t *testing.T) *exportEnv {
	t.Helper()
	xe := setupExport(t)
	xe.repo.Cfg[settings.KeyQueryMaskEnabled] = "true"
	xe.repo.Users[100] = model.User{ID: 100, Username: "root", IsActive: true, IsSuperuser: true}
	xe.svc.SetResultMasker(&fakeMasker{}) // 有脱敏组件才谈得上“本来会脱敏”
	return xe
}

func (xe *exportEnv) unmaskReq(reason string, ids ...uint) sqlflow.SubmitReq {
	r := xe.req(exportSQL, "csv", ids...)
	r.Export.Unmasked, r.Export.UnmaskReason = true, reason
	return r
}

func (xe *exportEnv) execute(t *testing.T, id uint) {
	t.Helper()
	xe.approve(t, id)
	if _, err := xe.svc.Execute(context.Background(), root, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
}

func contentOf(t *testing.T, xe *exportEnv, id uint) string {
	t.Helper()
	var content string
	for _, c := range xe.zipEntries(t, fileOf(xe, id)) {
		content = c
	}
	return content
}

func TestExportUnmaskedSuperSkipsConfiguredMasking(t *testing.T) {
	xe := unmaskEnv(t)
	m := &fakeMasker{}
	xe.svc.SetResultMasker(m)
	res, err := xe.svc.Submit(context.Background(), root, xe.unmaskReq(unmaskReason, 1))
	if err != nil {
		t.Fatal(err)
	}
	id := res.WorkflowID
	if e := xe.repo.ExportExts[id]; !e.Unmasked || e.Reason != unmaskReason || e.SetBy != "root" {
		t.Fatalf("应记下选择、理由和提交人：%+v", e)
	}
	xe.execute(t, id)
	if xe.status(id) != model.WfFinish {
		t.Fatalf("状态：%s", xe.status(id))
	}
	if content := contentOf(t, xe, id); m.calls != 0 || !strings.Contains(content, "张三") || strings.Contains(content, "张***") {
		t.Fatalf("导出原值不应脱敏：calls=%d %q", m.calls, content)
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "（导出原值，未脱敏）") {
		t.Errorf("结果行应说明导出了原值：%+v", rows)
	}
	// 详情：审批人看得到选择、理由和提交人
	d, err := xe.svc.Get(root, id)
	if err != nil || d.ExportUnmasked == nil || d.ExportUnmasked.Reason != unmaskReason || d.ExportUnmasked.SetBy != "root" || !d.ExportUnmasked.Active {
		t.Fatalf("详情应带导出原值说明：%v %+v", err, d.ExportUnmasked)
	}
	// 系统审计：提交记录写明理由
	var audited bool
	for _, a := range xe.repo.LifecycleAudits {
		if strings.Contains(a.ExtraInfo, "选择导出原值（不脱敏），理由："+unmaskReason) {
			audited = true
		}
	}
	if !audited {
		t.Errorf("系统审计应留痕：%+v", xe.repo.LifecycleAudits)
	}
	// 通知标题里带上“导出原值”，审批人一眼能看到
	var titled bool
	xe.cap.mu.Lock()
	evs := append([]notify.Event(nil), xe.cap.msgs...)
	xe.cap.mu.Unlock()
	for _, ev := range evs {
		if strings.Contains(ev.Title, "数据导出申请（导出原值，不脱敏）") {
			titled = true
		}
	}
	if !titled {
		t.Errorf("通知标题应标明导出原值")
	}
	// 没选的导出工单照旧脱敏
	m2 := &fakeMasker{}
	xe.svc.SetResultMasker(m2)
	id2 := xe.submit(t, xe.req(exportSQL, "csv", 1))
	xe.run(t, id2)
	if content := contentOf(t, xe, id2); m2.calls != 1 || !strings.Contains(content, "张***") {
		t.Errorf("没选导出原值的工单应照常脱敏：calls=%d %q", m2.calls, content)
	}
}

func TestExportUnmaskedOnlySuperCanChoose(t *testing.T) {
	xe := unmaskEnv(t)
	before := len(xe.repo.Workflows)
	_, err := xe.svc.Submit(context.Background(), dev, xe.unmaskReq(unmaskReason, 1))
	if code(err) != "forbidden" || !strings.Contains(err.Error(), "仅超级用户") {
		t.Fatalf("非超级用户选导出原值应 403：%v", err)
	}
	if len(xe.repo.Workflows) != before || len(xe.repo.ExportExts) != 0 {
		t.Errorf("被拒绝时不应留下工单 / 记录")
	}
	// 提交选项里：只有超级用户看到这个选项
	o, err := xe.svc.SubmitOptions(root)
	if err != nil || !o.Export.CanUnmask {
		t.Errorf("超级用户应能选：%v %+v", err, o.Export)
	}
	if o, err := xe.svc.SubmitOptions(dev); err != nil || o.Export.CanUnmask {
		t.Errorf("普通用户不应有这个选项：%v %+v", err, o.Export)
	}
}

func TestExportUnmaskedValidation(t *testing.T) {
	xe := unmaskEnv(t)
	for name, reason := range map[string]string{"没写理由": "", "只有空白": "   ", "理由太短": "要用", "理由太长": strings.Repeat("长", 201)} {
		if _, err := xe.svc.Submit(context.Background(), root, xe.unmaskReq(reason, 1)); code(err) != "bad_request" {
			t.Errorf("%s：应 400，得到 %v", name, err)
		}
	}
	// 不选导出原值时，多余的理由被丢弃，不入库
	r := xe.req(exportSQL, "csv", 1)
	r.Export.UnmaskReason = "没勾选却带了理由"
	res, err := xe.svc.Submit(context.Background(), root, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := xe.repo.ExportExts[res.WorkflowID]; ok {
		t.Error("没选导出原值不应写扩展表")
	}
	// 本来就不脱敏（总开关没开）：选了没有意义，拒绝
	xe2 := unmaskEnv(t)
	xe2.repo.Cfg[settings.KeyQueryMaskEnabled] = "false"
	if _, err := xe2.svc.Submit(context.Background(), root, xe2.unmaskReq(unmaskReason, 1)); code(err) != "bad_request" || !strings.Contains(err.Error(), "不需要选择导出原值") {
		t.Errorf("本来不脱敏时应拒绝：%v", err)
	}
	if o, _ := xe2.svc.SubmitOptions(root); o.Export.CanUnmask {
		t.Error("本来不脱敏时不应出现这个选项")
	}
	// 扩展表没创建：拒绝并提示执行 SQL 脚本，选项也不出现
	xe3 := unmaskEnv(t)
	xe3.repo.NoExportExtTb = true
	if _, err := xe3.svc.Submit(context.Background(), root, xe3.unmaskReq(unmaskReason, 1)); err == nil || !strings.Contains(err.Error(), "290_export_unmasked.sql") {
		t.Errorf("表没建应提示执行脚本：%v", err)
	}
	if o, _ := xe3.svc.SubmitOptions(root); o.Export.CanUnmask {
		t.Error("表没建时不应出现这个选项")
	}
}

// 提交时是超级用户、执行时已不是（被降级 / 停用）：不导出原值，按脱敏设置导出。
func TestExportUnmaskedRechecksSubmitterAtExecution(t *testing.T) {
	for name, mutate := range map[string]func(*exportEnv){
		"降级": func(xe *exportEnv) {
			xe.repo.Users[100] = model.User{ID: 100, Username: "root", IsActive: true, IsSuperuser: false}
		},
		"停用": func(xe *exportEnv) {
			xe.repo.Users[100] = model.User{ID: 100, Username: "root", IsActive: false, IsSuperuser: true}
		},
		"删除": func(xe *exportEnv) { delete(xe.repo.Users, 100) },
	} {
		xe := unmaskEnv(t)
		m := &fakeMasker{}
		xe.svc.SetResultMasker(m)
		res, err := xe.svc.Submit(context.Background(), root, xe.unmaskReq(unmaskReason, 1))
		if err != nil {
			t.Fatal(err)
		}
		id := res.WorkflowID
		mutate(xe)
		d, err := xe.svc.Get(root, id)
		if err != nil || d == nil || d.ExportUnmasked == nil || d.ExportUnmasked.Active {
			t.Fatalf("%s：详情应显示选择已不生效：%v %+v", name, err, d)
		}
		xe.execute(t, id)
		if content := contentOf(t, xe, id); m.calls != 1 || !strings.Contains(content, "张***") || strings.Contains(content, "张三") {
			t.Errorf("%s：应按脱敏设置导出：calls=%d %q", name, m.calls, content)
		}
		rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
		if len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "（已脱敏：name）") {
			t.Errorf("%s：结果行应说明已脱敏：%+v", name, rows)
		}
	}
}

// 读不出提交人身份（查询出错）时宁可多脱敏，不能误给原值。
func TestExportUnmaskedFailsClosed(t *testing.T) {
	xe := unmaskEnv(t)
	m := &fakeMasker{}
	xe.svc.SetResultMasker(m)
	res, err := xe.svc.Submit(context.Background(), root, xe.unmaskReq(unmaskReason, 1))
	if err != nil {
		t.Fatal(err)
	}
	xe.repo.NoExportExtTb = true // 执行时扩展表读不到（模拟故障 / 表被删）
	xe.execute(t, res.WorkflowID)
	if content := contentOf(t, xe, res.WorkflowID); m.calls != 1 || strings.Contains(content, "张三") {
		t.Errorf("读不到记录应按脱敏导出：calls=%d %q", m.calls, content)
	}
}

// SQL 防火墙要求强制脱敏的列，导出原值也照样脱敏；用户配置的脱敏不做。
func TestExportUnmaskedStillMasksFirewallForcedColumns(t *testing.T) {
	xe := unmaskEnv(t)
	mask := fwRule("姓名强制脱敏", firewall.ActMask, expScene, firewall.Condition{T: firewall.CondObject, V: []string{"*.*.name"}})
	fw := useFirewall(xe.svc, xe.repo)
	m := &forcedMasker{}
	xe.svc.SetResultMasker(m)
	res, err := xe.svc.Submit(context.Background(), root, xe.unmaskReq(unmaskReason, 1))
	if err != nil {
		t.Fatal(err)
	}
	id := res.WorkflowID
	fw.setRules(mask)
	xe.execute(t, id)
	if xe.status(id) != model.WfFinish {
		t.Fatalf("状态：%s", xe.status(id))
	}
	if m.forced != 1 || m.configured || m.calls != 0 || len(m.force) != 1 || m.force[0].Column != "name" {
		t.Errorf("只做防火墙强制脱敏，不做已配置的：%+v", m)
	}
	if content := contentOf(t, xe, id); !strings.Contains(content, "张***") || strings.Contains(content, "张三") {
		t.Errorf("强制脱敏的列必须脱敏：%q", content)
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "（导出原值；SQL 防火墙强制脱敏：name）") {
		t.Errorf("结果行应说明：%+v", rows)
	}
}

// 批量导出（多实例）和失败实例重试，同样遵守选择。
func TestExportUnmaskedBatchAndRetry(t *testing.T) {
	xe := unmaskEnv(t)
	m := &fakeMasker{}
	xe.svc.SetResultMasker(m)
	res, err := xe.svc.Submit(context.Background(), root, xe.unmaskReq(unmaskReason, 11, 12))
	if err != nil {
		t.Fatal(err)
	}
	id := res.WorkflowID
	xe.hosts["ms12"].noConn = true // 12 号实例连不上：这一个失败，11 号成功
	xe.execute(t, id)
	if xe.status(id) != model.WfException {
		t.Fatalf("部分实例失败应为执行有异常：%s", xe.status(id))
	}
	if m.calls != 0 {
		t.Fatalf("批量导出原值不应脱敏：calls=%d", m.calls)
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	var noted bool
	for _, r := range rows {
		if strings.Contains(r.ErrorMessage, "（导出原值，未脱敏）") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("批量结果行应说明导出了原值：%+v", rows)
	}
	// 重试失败的实例：仍是原值
	xe.hosts["ms12"].noConn = false
	if _, _, err := xe.svc.RetryBatchInstance(context.Background(), root, id, 12); err != nil {
		t.Fatalf("重试：%v", err)
	}
	if m.calls != 0 {
		t.Errorf("重试也不应脱敏：calls=%d", m.calls)
	}
	for _, c := range xe.zipEntries(t, fileOf(xe, id)) {
		if strings.Contains(c, "张***") || !strings.Contains(c, "张三") {
			t.Errorf("压缩包里的文件应是原值：%q", c)
		}
	}
}

// 列表：选了导出原值的导出工单带标记，其它没有。
func TestExportUnmaskedListFlag(t *testing.T) {
	xe := unmaskEnv(t)
	xe.svc.SetResultMasker(&fakeMasker{})
	res, err := xe.svc.Submit(context.Background(), root, xe.unmaskReq(unmaskReason, 1))
	if err != nil {
		t.Fatal(err)
	}
	other := xe.submit(t, xe.req(exportSQL, "csv", 1))
	items, _, err := xe.svc.List(root, sqlflow.ListQuery{Kind: "export", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint]bool{}
	for _, it := range items {
		got[it.ID] = it.ExportUnmasked
	}
	if !got[res.WorkflowID] || got[other] || len(got) != 2 {
		t.Errorf("列表标记：%v（选了的 %d，没选的 %d）", got, res.WorkflowID, other)
	}
}
