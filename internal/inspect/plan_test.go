// @author: kevinkelly
// @license: Apache Licence
// @file: plan_test.go
// @time: 2026/10/28

package inspect

import (
	"strings"
	"testing"
	"time"
)

func basePlan() Plan {
	return Plan{Name: "生产夜间巡检", Enabled: true, Kind: KindDaily, Time: "02:00", Concurrency: 3, StaggerSeconds: 30, SkipBusy: true, TimeoutMinutes: 15,
		NotifyMode: NotifyNone, RetentionDays: 90, InstanceIDs: []uint{1, 2}}
}

func TestPlanNormalizeOK(t *testing.T) {
	p := basePlan()
	p.Name = "  生产夜间巡检  "
	p.InstanceIDs = []uint{2, 1, 2, 0}
	p.TagCodes = []string{" mysql_inspection ", "", "mysql_inspection"}
	p.Databases = []string{" ShopDB ", ""}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if p.Name != "生产夜间巡检" || len(p.InstanceIDs) != 2 || p.InstanceIDs[0] != 1 || len(p.TagCodes) != 1 || p.TagCodes[0] != "mysql_inspection" || p.Databases[0] != "ShopDB" {
		t.Errorf("规整结果：%+v", p)
	}
	if p.Cron != "0 2 * * *" {
		t.Errorf("cron：%q", p.Cron)
	}
	// 仅按标签、不选实例也可以
	q := basePlan()
	q.InstanceIDs, q.TagCodes = nil, []string{"mssql_inspection"}
	if err := q.Normalize(); err != nil {
		t.Errorf("只选标签应允许：%v", err)
	}
}

func TestPlanNormalizeRejects(t *testing.T) {
	cases := []struct {
		name string
		mod  func(p *Plan)
		want string
	}{
		{"名称为空", func(p *Plan) { p.Name = "  " }, "计划名称"},
		{"名称过长", func(p *Plan) { p.Name = strings.Repeat("长", 61) }, "计划名称"},
		{"周期类型", func(p *Plan) { p.Kind = "yearly" }, "周期类型"},
		{"时刻格式", func(p *Plan) { p.Time = "25:00" }, "HH:MM"},
		{"时刻缺失", func(p *Plan) { p.Time = "" }, "HH:MM"},
		{"每周没选星期", func(p *Plan) { p.Kind = KindWeekly; p.Weekdays = nil }, "至少选择一个星期"},
		{"星期越界", func(p *Plan) { p.Kind = KindWeekly; p.Weekdays = []int{7} }, "星期取值"},
		{"每月 29 日", func(p *Plan) { p.Kind = KindMonthly; p.MonthDay = 29 }, "1 到 28"},
		{"每月 0 日", func(p *Plan) { p.Kind = KindMonthly; p.MonthDay = 0 }, "1 到 28"},
		{"cron 段数", func(p *Plan) { p.Kind = KindCron; p.Cron = "0 2 * *" }, "5 段"},
		{"cron 描述符", func(p *Plan) { p.Kind = KindCron; p.Cron = "@every 1m" }, "5 段"},
		{"cron 带 @ 的 5 段", func(p *Plan) { p.Kind = KindCron; p.Cron = "@daily a b c d" }, "@"},
		{"cron 时区前缀", func(p *Plan) { p.Kind = KindCron; p.Cron = "TZ=UTC 0 2 * *" }, "不支持"},
		{"cron 语法", func(p *Plan) { p.Kind = KindCron; p.Cron = "0 25 * * *" }, "cron 表达式不合法"},
		{"cron 每分钟", func(p *Plan) { p.Kind = KindCron; p.Cron = "* * * * *" }, "间隔不能小于 5 分钟"},
		{"cron 每 4 分钟", func(p *Plan) { p.Kind = KindCron; p.Cron = "*/4 * * * *" }, "间隔不能小于 5 分钟"},
		{"并发 0", func(p *Plan) { p.Concurrency = 0 }, "同时巡检"},
		{"并发 11", func(p *Plan) { p.Concurrency = 11 }, "同时巡检"},
		{"错峰为负", func(p *Plan) { p.StaggerSeconds = -1 }, "错峰"},
		{"错峰过大", func(p *Plan) { p.StaggerSeconds = 3601 }, "错峰"},
		{"超时 0", func(p *Plan) { p.TimeoutMinutes = 0 }, "超时"},
		{"超时 241", func(p *Plan) { p.TimeoutMinutes = 241 }, "超时"},
		{"通知方式", func(p *Plan) { p.NotifyMode = "email" }, "通知方式"},
		{"通知没选渠道", func(p *Plan) { p.NotifyMode = NotifyAlert }, "通知渠道"},
		{"渠道过多", func(p *Plan) {
			p.NotifyMode = NotifyAlways
			p.NotifyChannels = []uint{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
		}, "最多选 10"},
		{"保留 0", func(p *Plan) { p.RetentionDays = 0 }, "保留天数"},
		{"保留过大", func(p *Plan) { p.RetentionDays = 3651 }, "保留天数"},
		{"没有实例也没有标签", func(p *Plan) { p.InstanceIDs, p.TagCodes = nil, nil }, "至少选择"},
		{"实例过多", func(p *Plan) {
			p.InstanceIDs = nil
			for i := 1; i <= maxPlanInstances+1; i++ {
				p.InstanceIDs = append(p.InstanceIDs, uint(i))
			}
		}, "最多选择"},
		{"库名非法", func(p *Plan) { p.Databases = []string{"a]b"} }, "库名"},
		{"标签过长", func(p *Plan) { p.TagCodes = []string{strings.Repeat("x", 51)} }, "标签代码"},
	}
	for _, c := range cases {
		p := basePlan()
		c.mod(&p)
		err := p.Normalize()
		if err == nil {
			t.Errorf("%s：应被拒绝", c.name)
			continue
		}
		e := asErrT(err)
		if e == nil || e.Status != 400 || !strings.Contains(e.Message, c.want) {
			t.Errorf("%s：错误 %v，期望包含 %q", c.name, err, c.want)
		}
	}
}

func asErrT(err error) *Error {
	e, _ := err.(*Error)
	return e
}

func TestCronExprAndDescribe(t *testing.T) {
	cases := []struct {
		p    Plan
		cron string
		desc string
	}{
		{Plan{Kind: KindDaily, Time: "02:00"}, "0 2 * * *", "每天 02:00"},
		{Plan{Kind: KindDaily, Time: "23:05"}, "5 23 * * *", "每天 23:05"},
		{Plan{Kind: KindWeekly, Time: "03:30", Weekdays: []int{1, 3}}, "30 3 * * 1,3", "每周一、三 03:30"},
		{Plan{Kind: KindWeekly, Time: "03:00", Weekdays: []int{0, 6}}, "0 3 * * 0,6", "每周日、六 03:00"},
		{Plan{Kind: KindMonthly, Time: "02:00", MonthDay: 5}, "0 2 5 * *", "每月 5 日 02:00"},
		{Plan{Kind: KindCron, Cron: "  0   *  * * * "}, "0 * * * *", "cron 0 * * * *"},
	}
	for _, c := range cases {
		if got := c.p.CronExpr(); got != c.cron {
			t.Errorf("%+v：cron %q，期望 %q", c.p, got, c.cron)
		}
		if got := c.p.Describe(); got != c.desc {
			t.Errorf("describe %q，期望 %q", got, c.desc)
		}
	}
}

func TestNextRuns(t *testing.T) {
	from := ts("2026-10-06 08:00:00") // 周二
	get := func(p Plan, n int) []string {
		times, err := p.NextRuns(from, n)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range times {
			out = append(out, x.Format("2006-01-02 15:04"))
		}
		return out
	}
	eq := func(name string, got, want []string) {
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s：%v，期望 %v", name, got, want)
		}
	}
	eq("每天", get(Plan{Kind: KindDaily, Time: "02:00"}, 3), []string{"2026-10-07 02:00", "2026-10-08 02:00", "2026-10-09 02:00"})
	eq("今天还没到", get(Plan{Kind: KindDaily, Time: "23:00"}, 2), []string{"2026-10-06 23:00", "2026-10-07 23:00"})
	eq("每周一", get(Plan{Kind: KindWeekly, Time: "03:00", Weekdays: []int{1}}, 2), []string{"2026-10-12 03:00", "2026-10-19 03:00"})
	eq("每月 5 日", get(Plan{Kind: KindMonthly, Time: "02:00", MonthDay: 5}, 2), []string{"2026-11-05 02:00", "2026-12-05 02:00"})
	eq("cron 每小时", get(Plan{Kind: KindCron, Cron: "0 * * * *"}, 2), []string{"2026-10-06 09:00", "2026-10-06 10:00"})
	p := Plan{Kind: KindDaily, Time: "02:00"}
	next, err := p.NextRun(from)
	if err != nil || next.Format("2006-01-02 15:04") != "2026-10-07 02:00" {
		t.Errorf("NextRun：%v %v", next, err)
	}
	bad := Plan{Kind: KindCron, Cron: "bad"}
	if _, err := bad.NextRun(from); err == nil {
		t.Error("非法 cron 应报错")
	}
	// 夏令时无关：时刻按进程本地时区计算
	if next.Location() != time.Local && next.Location().String() != "Local" {
		t.Errorf("时区：%v", next.Location())
	}
}
