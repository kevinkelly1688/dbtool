// @author: kevinkelly
// @license: Apache Licence
// @file: i18n_test.go
// @time: 2026/10/28

package inspect

import (
	"strings"
	"testing"

	"dbtool/internal/i18n"
)

// 英文页面：模板里的文案都走目录翻译，中文原文不再出现；中文页保持原样。
func TestEnglishPages(t *testing.T) {
	_, env := newHTTP(t)
	root := env.Login("root")
	for _, p := range []struct {
		path string
		keys []string
	}{
		{"/inspection", []string{"定时巡检", "新建定时巡检计划", "全部实例", "运行记录", "告警 / 关注合计"}},
		{"/inspection/mssql-12", []string{"性能巡检报告", "重新巡检", "同实例趋势", "加载中…"}},
	} {
		code, zh := root.Page(p.path)
		if code != 200 {
			t.Fatalf("%s = %d", p.path, code)
		}
		code, en := root.Page(p.path + "?lang=en")
		if code != 200 || !strings.Contains(en, `<html lang="en"`) {
			t.Fatalf("%s?lang=en = %d", p.path, code)
		}
		for _, k := range p.keys {
			if !strings.Contains(zh, k) {
				t.Errorf("中文页 %s 缺 %q", p.path, k)
			}
			if want := i18n.T(i18n.EN, k); want == k {
				t.Errorf("英文目录缺 %q", k)
			} else if !strings.Contains(en, want) || strings.Contains(en, k) {
				t.Errorf("英文页 %s 应有 %q 而不是 %q", p.path, want, k)
			}
		}
	}
}

// 计划周期说明：中文与 Describe 逐字一致（通知正文、审计日志都在用），英文按语序重组。
func TestDescribeLang(t *testing.T) {
	cases := []struct {
		p  Plan
		zh string
	}{
		{Plan{Kind: KindDaily, Time: "02:00"}, "每天 02:00"},
		{Plan{Kind: KindWeekly, Time: "03:30", Weekdays: []int{1, 3}}, "每周一、三 03:30"},
		{Plan{Kind: KindMonthly, Time: "04:00", MonthDay: 15}, "每月 15 日 04:00"},
		{Plan{Kind: KindCron, Cron: "0  2 * * *"}, "cron 0 2 * * *"},
	}
	for _, c := range cases {
		if got := c.p.Describe(); got != c.zh {
			t.Errorf("Describe = %q，期望 %q", got, c.zh)
		}
		if got := c.p.DescribeLang(i18n.ZH); got != c.zh {
			t.Errorf("DescribeLang(zh) = %q，期望 %q", got, c.zh)
		}
		if en := c.p.DescribeLang(i18n.EN); c.p.Kind != KindCron && (en == c.zh || strings.ContainsAny(en, "每周日一二三四五六月天、")) {
			t.Errorf("DescribeLang(en) = %q，不应残留中文", en)
		}
	}
	w := Plan{Kind: KindWeekly, Time: "03:30", Weekdays: []int{1, 3}}
	if en := w.DescribeLang(i18n.EN); !strings.Contains(en, "Mon") || !strings.Contains(en, "Wed") || !strings.Contains(en, "03:30") {
		t.Errorf("英文周期说明: %q", en)
	}
}
