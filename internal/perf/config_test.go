// @author: kevinkelly
// @license: Apache Licence
// @file: config_test.go
// @time: 2026/10/28

package perf

import (
	"strings"
	"testing"
	"time"

	"dbtool/internal/settings"
)

func validCollect() CollectConfigInput {
	return CollectConfigInput{InstanceID: 2, Product: ProdSelectDB, Collector: CollDorisAudit, MinAvgMs: 3000, MinTotalSec: 30,
		ExcludeRegex: `(?i)^\s*show`, ExcludeDBs: "mysql,sys", Enabled: true}
}

func TestSaveCollectConfig(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.SaveCollectConfig(f.dev(), validCollect())
	wantErr(t, err, 403, "DBA")
	c, err := f.svc.SaveCollectConfig(f.dba(), validCollect())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := f.repo.GetCollectConfig(c.ID)
	if got.InstanceID != 2 || got.Collector != CollDorisAudit || !got.Enabled || got.MinAvgMs != 3000 {
		t.Fatalf("%+v", got)
	}
	if a := f.repo.auditActions(); a[len(a)-1] != "性能工单配置-采集配置新增" || !strings.Contains(f.repo.audits[len(a)-1].ExtraInfo, "report-doris") {
		t.Fatalf("审计: %v", a)
	}
	// 同一实例只能有一条
	_, err = f.svc.SaveCollectConfig(f.dba(), validCollect())
	wantErr(t, err, 409, "已有采集配置")
	// 修改保留采集进度
	last := f.now
	f.repo.collect[1].LastCollectTime, f.repo.collect[1].LastStatus = &last, "OK"
	in := validCollect()
	in.ID, in.MinAvgMs, in.Enabled = c.ID, 5000, false
	if _, err = f.svc.SaveCollectConfig(f.dba(), in); err != nil {
		t.Fatal(err)
	}
	got, _ = f.repo.GetCollectConfig(c.ID)
	if got.MinAvgMs != 5000 || got.Enabled || got.LastCollectTime == nil || got.LastStatus != "OK" {
		t.Fatalf("修改: %+v", got)
	}
	if a := f.repo.auditActions(); a[len(a)-1] != "性能工单配置-采集配置修改" {
		t.Fatalf("审计: %v", a)
	}
	in.InstanceID = 1
	_, err = f.svc.SaveCollectConfig(f.dba(), in)
	wantErr(t, err, 400, "不能更换")
	in.ID, in.InstanceID = 99, 2
	_, err = f.svc.SaveCollectConfig(f.dba(), in)
	wantErr(t, err, 404, "不存在")

	// 校验
	bad := func(mod func(*CollectConfigInput), status int, msg string) {
		t.Helper()
		in := validCollect()
		in.InstanceID = 1
		in.Collector = CollPlatformSlow
		in.Product = ProdMySQL
		in.ID = 1
		mod(&in)
		_, err := f.svc.SaveCollectConfig(f.dba(), in)
		wantErr(t, err, status, msg)
	}
	for id, msg := range map[uint]string{99: "实例不存在", 3: "暂不支持 pgsql"} {
		_, err := f.svc.SaveCollectConfig(f.dba(), CollectConfigInput{InstanceID: id, Product: ProdMySQL, Collector: CollPlatformSlow})
		wantErr(t, err, 400, msg)
	}
	bad(func(c *CollectConfigInput) { c.Product = "x" }, 400, "产品类型")
	bad(func(c *CollectConfigInput) { c.Collector = "x" }, 400, "采集方式")
	bad(func(c *CollectConfigInput) { c.Collector = CollDorisAudit }, 400, "不适用于 mysql")
	bad(func(c *CollectConfigInput) { c.MinAvgMs = -1 }, 400, "平均耗时阈值")
	bad(func(c *CollectConfigInput) { c.MinTotalSec = 99999999 }, 400, "窗口总耗时阈值")
	bad(func(c *CollectConfigInput) { c.ExcludeRegex = "(" }, 400, "不是合法的正则")
	bad(func(c *CollectConfigInput) { c.ExcludeRegex = strings.Repeat("a", 501) }, 400, "过长")
	bad(func(c *CollectConfigInput) { c.ExcludeDBs = strings.Repeat("a", 501) }, 400, "过长")
	bad(func(c *CollectConfigInput) { c.Collector = CollCustomSQL }, 400, "不能为空")
	bad(func(c *CollectConfigInput) { c.Collector, c.CustomSQL = CollCustomSQL, "delete from t" }, 400, "只能是 SELECT")
	bad(func(c *CollectConfigInput) { c.CustomSQL = "drop table t" }, 400, "只能是 SELECT")
	bad(func(c *CollectConfigInput) { c.TeamID = 99 }, 400, "团队不存在")
	// adb 只能用自定义 SQL；有效的自定义 SQL 通过
	f.repo.insts[4] = f.repo.insts[1]
	f.repo.insts[4] = &(*f.repo.insts[1])
	f.repo.insts[4].ID, f.repo.insts[4].DBType = 4, "adb"
	ok := CollectConfigInput{InstanceID: 4, Product: ProdADB, Collector: CollCustomSQL, CustomSQL: "select db_name, sql_text from x where t > %(start)s", MinAvgMs: 1000, MinTotalSec: 10, TeamID: 11, Enabled: true}
	if _, err = f.svc.SaveCollectConfig(f.dba(), ok); err != nil {
		t.Fatal(err)
	}
	ok.InstanceID = 4
	ok.Collector = CollPlatformSlow
	ok.ID = 0
	_, err = f.svc.SaveCollectConfig(f.dba(), ok)
	wantErr(t, err, 400, "不适用于 adb")
}

func TestDeleteCollectConfig(t *testing.T) {
	f := newFx(t)
	wantErr(t, f.svc.DeleteCollectConfig(f.dev(), 1), 403, "DBA")
	wantErr(t, f.svc.DeleteCollectConfig(f.dba(), 99), 404, "不存在")
	if err := f.svc.DeleteCollectConfig(f.dba(), 1); err != nil || len(f.repo.collect) != 0 {
		t.Fatal(err)
	}
	if a := f.repo.auditActions(); a[len(a)-1] != "性能工单配置-采集配置删除" {
		t.Fatalf("%v", a)
	}
}

func TestInitCollectConfigs(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.InitCollectConfigs(f.dev())
	wantErr(t, err, 403, "DBA")
	res, err := f.svc.InitCollectConfigs(f.dba())
	if err != nil {
		t.Fatal(err)
	}
	// 实例 1 已有配置；实例 2（doris）新建；实例 3（pgsql）不支持
	if res.Created != 1 || res.Skipped != 1 {
		t.Fatalf("%+v", res)
	}
	c, _ := f.repo.GetCollectConfigByInstance(2)
	if c.Enabled || c.Product != ProdSelectDB || c.Collector != CollPlatformSlow || c.ExcludeDBs != defaultExcludeDBs {
		t.Fatalf("默认配置应停用并按类型选方式: %+v", c)
	}
	res, _ = f.svc.InitCollectConfigs(f.dba())
	if res.Created != 0 || res.Skipped != 2 {
		t.Fatalf("重复执行不重复建: %+v", res)
	}
}

func TestCollectOverview(t *testing.T) {
	f := newFx(t)
	last := f.now
	f.repo.collect[0].LastCollectTime, f.repo.collect[0].LastStatus, f.repo.collect[0].TeamID = &last, "OK", ptr(uint(10))
	f.repo.cfg[settings.KeyPerfJobs] = "collect, sla"
	o, err := f.svc.CollectOverview()
	if err != nil {
		t.Fatal(err)
	}
	r := o.Rows[0]
	if r.Instance != "order-db" || r.Team != "订单研发" || r.CollectLabel == "" || r.LastCollect != "2026-10-06 10:00:00" || len(o.Instances) != 2 || len(o.Collectors) != 8 {
		t.Fatalf("%+v", o)
	}
	if strings.Join(o.Jobs.Enabled, ",") != "collect,sla" || len(o.Jobs.All) != 4 {
		t.Fatalf("任务开关: %+v", o.Jobs)
	}
}

func TestSaveTeam(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.SaveTeam(f.dev(), TeamInput{ResourceGroupID: 11})
	wantErr(t, err, 403, "DBA")
	in := TeamInput{ResourceGroupID: 11, DefaultAssigneeID: uDev2, LeaderIDs: []uint{uTL, uTL, uDev}, DirectorIDs: []uint{uDirector}, Enabled: true, Remark: " 会员 "}
	tc, err := f.svc.SaveTeam(f.dba(), in)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := f.repo.GetTeam(tc.ID)
	if got.Remark != "会员" || len(got.LeaderIDs) != 2 || *got.DefaultAssigneeID != uDev2 || !got.Enabled {
		t.Fatalf("%+v", got)
	}
	if a := f.repo.auditActions(); a[len(a)-1] != "性能工单配置-团队配置新增" {
		t.Fatalf("%v", a)
	}
	_, err = f.svc.SaveTeam(f.dba(), in)
	wantErr(t, err, 409, "已有团队配置")
	in.ID, in.Enabled = tc.ID, false
	if _, err = f.svc.SaveTeam(f.dba(), in); err != nil {
		t.Fatal(err)
	}
	if got, _ = f.repo.GetTeam(tc.ID); got.Enabled {
		t.Fatal("修改")
	}
	bad := func(mod func(*TeamInput), status int, msg string) {
		t.Helper()
		in := TeamInput{ResourceGroupID: 11, ID: tc.ID}
		mod(&in)
		_, err := f.svc.SaveTeam(f.dba(), in)
		wantErr(t, err, status, msg)
	}
	bad(func(i *TeamInput) { i.ResourceGroupID = 99 }, 400, "团队不存在")
	bad(func(i *TeamInput) { i.DefaultAssigneeID = 8 }, 400, "已停用")
	bad(func(i *TeamInput) { i.DefaultAssigneeID = 99 }, 400, "不存在")
	bad(func(i *TeamInput) { i.LeaderIDs = []uint{8} }, 400, "团队 TL里有不存在或已停用")
	bad(func(i *TeamInput) { i.DirectorIDs = []uint{99} }, 400, "总监里有不存在")
	bad(func(i *TeamInput) { i.Remark = strings.Repeat("备", 201) }, 400, "备注过长")
	bad(func(i *TeamInput) { i.ID, i.ResourceGroupID = 999, 11 }, 404, "不存在")
	bad(func(i *TeamInput) { i.ResourceGroupID = 10 }, 409, "已有团队配置")
	var many []uint
	for k := 0; k < 51; k++ {
		many = append(many, uint(100+k))
	}
	bad(func(i *TeamInput) { i.LeaderIDs = many }, 400, "最多 50 人")
	// 删除
	wantErr(t, f.svc.DeleteTeam(f.dev(), tc.ID), 403, "DBA")
	wantErr(t, f.svc.DeleteTeam(f.dba(), 999), 404, "不存在")
	if err := f.svc.DeleteTeam(f.dba(), tc.ID); err != nil {
		t.Fatal(err)
	}
}

func TestTeamOverview(t *testing.T) {
	f := newFx(t)
	f.repo.routes = []Route{{ID: 1, InstanceID: ptr(uint(1)), DBRegex: "pay_.*", TeamID: 10, AssigneeID: ptr(uint(uDev)), Priority: 10}, {ID: 2, DBRegex: "dw", TeamID: 11, Priority: 20}}
	f.repo.cfg[settings.KeyPerfDBAUsers] = "dba1,dba2"
	o, err := f.svc.TeamOverview()
	if err != nil {
		t.Fatal(err)
	}
	tr := o.Teams[0]
	if tr.Team != "订单研发" || tr.DefaultAssignee != "张三" || strings.Join(tr.Leaders, ",") != "王TL" || strings.Join(tr.Directors, ",") != "总监" ||
		len(tr.Webhooks) != 1 || tr.Webhooks[0] != "钉钉" {
		t.Fatalf("%+v", tr)
	}
	if o.Routes[0].Instance != "order-db" || o.Routes[0].Assignee != "张三" || o.Routes[1].Instance != "" || o.Routes[1].Team != "会员研发" || o.DBAUsers != "dba1,dba2" {
		t.Fatalf("%+v", o.Routes)
	}
}

func TestSaveRoute(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.SaveRoute(f.dev(), RouteInput{TeamID: 10})
	wantErr(t, err, 403, "DBA")
	r, err := f.svc.SaveRoute(f.dba(), RouteInput{InstanceID: 1, DBRegex: " pay_.* ", TeamID: 10, AssigneeID: uDev, Priority: 10})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := f.repo.GetRoute(r.ID)
	if got.DBRegex != "pay_.*" || *got.InstanceID != 1 || *got.AssigneeID != uDev {
		t.Fatalf("%+v", got)
	}
	// 空正则默认 .*；实例 0 表示全部
	r2, err := f.svc.SaveRoute(f.dba(), RouteInput{TeamID: 11, Priority: 20})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = f.repo.GetRoute(r2.ID); got.DBRegex != ".*" || got.InstanceID != nil || got.AssigneeID != nil {
		t.Fatalf("%+v", got)
	}
	if _, err = f.svc.SaveRoute(f.dba(), RouteInput{ID: r.ID, DBRegex: "x", TeamID: 10, Priority: 5}); err != nil {
		t.Fatal(err)
	}
	if got, _ = f.repo.GetRoute(r.ID); got.DBRegex != "x" || got.Priority != 5 || got.InstanceID != nil {
		t.Fatalf("修改: %+v", got)
	}
	bad := func(in RouteInput, status int, msg string) {
		t.Helper()
		_, err := f.svc.SaveRoute(f.dba(), in)
		wantErr(t, err, status, msg)
	}
	bad(RouteInput{TeamID: 10, DBRegex: "("}, 400, "正则不合法")
	bad(RouteInput{TeamID: 10, DBRegex: strings.Repeat("a", 201)}, 400, "过长")
	bad(RouteInput{TeamID: 10, Priority: -1}, 400, "优先级")
	bad(RouteInput{TeamID: 99}, 400, "团队不存在")
	bad(RouteInput{TeamID: 10, InstanceID: 3}, 400, "暂不支持")
	bad(RouteInput{TeamID: 10, AssigneeID: 8}, 400, "已停用")
	bad(RouteInput{TeamID: 10, ID: 99}, 404, "不存在")
	wantErr(t, f.svc.DeleteRoute(f.dev(), r.ID), 403, "DBA")
	wantErr(t, f.svc.DeleteRoute(f.dba(), 99), 404, "不存在")
	if err := f.svc.DeleteRoute(f.dba(), r.ID); err != nil || len(f.repo.routes) != 1 {
		t.Fatal(err)
	}
}

func validSLA() []SLAPolicy {
	return []SLAPolicy{
		{Severity: SevP0, AcceptHours: 2, ResolveHours: 24, L3AfterHours: 12, RemindIntervalHours: 2, MaxExtendTimes: 0},
		{Severity: SevP1, AcceptHours: 24, ResolveHours: 168, L3AfterHours: 72, RemindIntervalHours: 24, MaxExtendTimes: 1},
		{Severity: SevP2, AcceptHours: 72, ResolveHours: 336, L3AfterHours: 168, RemindIntervalHours: 48, MaxExtendTimes: 1},
		{Severity: SevP3, AcceptHours: 72, ResolveHours: 720, L3AfterHours: 336, RemindIntervalHours: 72, MaxExtendTimes: 2},
	}
}

func TestSaveSLA(t *testing.T) {
	f := newFx(t)
	wantErr(t, f.svc.SaveSLA(f.dev(), validSLA()), 403, "DBA")
	if err := f.svc.SaveSLA(f.dba(), validSLA()); err != nil {
		t.Fatal(err)
	}
	if len(f.repo.sla) != 4 || f.svc.policy(SevP0).AcceptHours != 2 {
		t.Fatalf("%+v", f.repo.sla)
	}
	// 审计里只记变化的级别
	a := f.repo.audits[len(f.repo.audits)-1]
	if a.Action != "性能工单配置-SLA策略修改" || !strings.Contains(a.ExtraInfo, "P0 受理2h/解决24h") || strings.Contains(a.ExtraInfo, "P1") {
		t.Fatalf("审计: %+v", a)
	}
	if err := f.svc.SaveSLA(f.dba(), validSLA()); err != nil {
		t.Fatal(err)
	}
	if a = f.repo.audits[len(f.repo.audits)-1]; a.ExtraInfo != "无变化" {
		t.Fatalf("%+v", a)
	}
	bad := func(mod func([]SLAPolicy) []SLAPolicy, msg string) {
		t.Helper()
		wantErr(t, f.svc.SaveSLA(f.dba(), mod(validSLA())), 400, msg)
	}
	bad(func(p []SLAPolicy) []SLAPolicy { return p[:3] }, "四个级别")
	bad(func(p []SLAPolicy) []SLAPolicy { p[1].Severity = SevP0; return p }, "重复")
	bad(func(p []SLAPolicy) []SLAPolicy { p[0].Severity = "PX"; return p }, "不合法")
	bad(func(p []SLAPolicy) []SLAPolicy { p[0].AcceptHours = 0; return p }, "P0-紧急：受理时限")
	bad(func(p []SLAPolicy) []SLAPolicy { p[1].ResolveHours = 24 * 366; return p }, "解决时限")
	bad(func(p []SLAPolicy) []SLAPolicy { p[2].AcceptHours = 400; return p }, "受理时限不能大于解决时限")
	bad(func(p []SLAPolicy) []SLAPolicy { p[3].L3AfterHours = -1; return p }, "升级总监")
	bad(func(p []SLAPolicy) []SLAPolicy { p[3].RemindIntervalHours = 0; return p }, "催办间隔")
	bad(func(p []SLAPolicy) []SLAPolicy { p[3].MaxExtendTimes = 21; return p }, "延期次数")
	// 只有第一次保存会新增行；顺序无关
	p := validSLA()
	p[0], p[3] = p[3], p[0]
	if err := f.svc.SaveSLA(f.dba(), p); err != nil || len(f.repo.sla) != 4 {
		t.Fatal(err)
	}
}

func TestSLAOverview(t *testing.T) {
	f := newFx(t)
	o, err := f.svc.SLAOverview()
	if err != nil || len(o.Policies) != 4 || o.Policies[0].Severity != SevP0 || o.Policies[0].AcceptHours != 4 || o.Custom {
		t.Fatalf("没配置时给默认: %+v %v", o, err)
	}
	if r := o.Rules[SevP0]; r.DailySec != 3600 || r.Share != 30 || r.AvgMs != 10000 || r.PerDay != 1000 {
		t.Fatalf("规则: %+v", r)
	}
	f.repo.sla = []SLAPolicy{{Severity: SevP1, AcceptHours: 1, ResolveHours: 2, RemindIntervalHours: 1}}
	f.repo.cfg[settings.KeyPerfSeverityRules] = `{"P0":{"daily_sec":99}}`
	o, _ = f.svc.SLAOverview()
	if o.Policies[1].AcceptHours != 1 || o.Policies[0].AcceptHours != 4 || !o.Custom || o.Rules[SevP0].DailySec != 99 {
		t.Fatalf("%+v", o)
	}
}

func TestConfigHelpers(t *testing.T) {
	f := newFx(t)
	f.repo.cfg["a"] = " 12 "
	f.repo.cfg["b"] = "1.5"
	f.repo.cfg["c"] = "On"
	f.repo.cfg["d"] = "garbage"
	if f.svc.cfgInt("a", 0) != 12 || f.svc.cfgInt("d", 7) != 7 || f.svc.cfgInt("none", 3) != 3 {
		t.Fatal("cfgInt")
	}
	if f.svc.cfgFloat("b", 0) != 1.5 || f.svc.cfgFloat("d", 2) != 2 {
		t.Fatal("cfgFloat")
	}
	if !f.svc.cfgBool("c", false) || f.svc.cfgBool("d", false) != false || !f.svc.cfgBool("d", true) || !f.svc.cfgBool("none", true) {
		t.Fatal("cfgBool")
	}
	if f.svc.cfgStr("a", "x") != "12" || f.svc.cfgStr("none", "x") != "x" {
		t.Fatal("cfgStr")
	}
	_ = time.Second
}
