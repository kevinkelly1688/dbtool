// @author: kevinkelly
// @license: Apache Licence
// @file: advice_test.go
// @time: 2026/10/28

package inspect

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 每个检查项（MySQL 19 项、SQL Server 27 项）都要有一句建议，写在检查项定义里。
func TestEveryCheckHasAdvice(t *testing.T) {
	if len(mysqlSteps) != 19 || len(msChecks) != 27 {
		t.Fatalf("检查项数量变了（MySQL %d、SQL Server %d），请同步补建议文案", len(mysqlSteps), len(msChecks))
	}
	for _, st := range mysqlSteps {
		if n := utf8.RuneCountInString(st.Advice); n < 10 || n > 120 {
			t.Errorf("MySQL 步骤 %s 的建议应是一句话（10~120 字），实际 %d 字：%q", st.Key, n, st.Advice)
		}
	}
	seen := map[string]string{}
	for _, c := range msChecks {
		if n := utf8.RuneCountInString(c.Advice); n < 10 || n > 120 {
			t.Errorf("SQL Server 检查项 %s 的建议应是一句话（10~120 字），实际 %d 字：%q", c.Key, n, c.Advice)
		}
		if prev, dup := seen[c.Advice]; dup {
			t.Errorf("检查项 %s 与 %s 的建议完全相同，应各写各的", c.Key, prev)
		}
		seen[c.Advice] = c.Key
	}
}

func TestMySQLSectionsCarryAdvice(t *testing.T) {
	result := jmap{
		"connection":  jmap{"connection_usage_rate": 97.0, "threads_connected": 97.0, "max_connections": 100.0},
		"buffer_pool": jmap{"buffer_pool_hit_rate": 85.0, "buffer_pool_size": 1.0 * float64(gb), "buffer_pool_usage_rate": 50.0},
		"query":       jmap{"error": "denied"},
		"auto_increment": jmap{"max_usage_pct": 90.0, "column_count": 1.0, "items": []any{
			jmap{"table": "shop.orders", "column": "id", "usage_pct": 90.0, "auto_increment": 100.0, "max_value": 111.0, "column_type": "int"}}},
	}
	secs := BuildMySQLSections(result)
	adv := mysqlStepAdvice()
	find := func(code, name string) Item {
		for _, s := range secs {
			if s.Code != code {
				continue
			}
			for _, it := range s.Items {
				if it.Name == name {
					return it
				}
			}
		}
		t.Fatalf("没找到 %s/%s", code, name)
		return Item{}
	}
	if it := find("connection", "连接使用率"); it.Level != LevelAlert || it.Advice != adv["connection"] {
		t.Fatalf("告警项带所属步骤的建议: %+v", it)
	}
	if it := find("memory", "缓冲池命中率"); it.Advice != adv["buffer_pool"] {
		t.Fatalf("缓冲池的检查项用 buffer_pool 步骤的建议: %+v", it)
	}
	if it := find("memory", "tmp_table_size"); it.Advice != adv["memory"] {
		t.Fatalf("内存分区的其他项用 memory 步骤的建议: %+v", it)
	}
	if it := find("query", "查询"); it.Level != LevelFailed || it.Advice != adviceFailed {
		t.Fatalf("采集失败用通用建议: %+v", it)
	}
	for _, s := range secs {
		for _, it := range s.Items {
			if it.Detail && it.Advice != "" {
				t.Errorf("明细行不挂建议: %+v", it)
			}
			if !it.Detail && it.Advice == "" {
				t.Errorf("分区 %s 的检查项 %s 没有建议", s.Code, it.Name)
			}
		}
	}
}

func TestMSSQLReportItemsCarryAdvice(t *testing.T) {
	e := newEnv()
	ref, _ := e.run(t, KindMSSQL, 3, RunOptions{})
	v, err := e.svc.GetReport(all, ref)
	if err != nil {
		t.Fatal(err)
	}
	adv := msCheckAdvice()
	n := 0
	for _, s := range v.Sections {
		for _, it := range s.Items {
			n++
			if want := msAdviceFor(adv, it.Key, it.Level); it.Advice != want || want == "" {
				t.Errorf("%s 的建议不对: %q（期望 %q）", it.Key, it.Advice, want)
			}
		}
	}
	if n == 0 {
		t.Fatal("报告里没有检查项")
	}
	if got := msAdviceFor(adv, "memory.ple", LevelFailed); got != adviceFailed {
		t.Fatalf("未采集用通用建议: %q", got)
	}
	if !strings.Contains(adv["memory.ple"], "PLE") {
		t.Fatalf("建议应对应具体检查项: %q", adv["memory.ple"])
	}
}

// ---- 已停用的实例 ----

type disRepo struct {
	*memRepo
	dis map[uint]bool
}

func (r *disRepo) DisabledInstanceIDs() (map[uint]bool, error) { return r.dis, nil }

func TestDisabledInstancesAreSkipped(t *testing.T) {
	e := newEnv()
	repo := &disRepo{memRepo: e.repo, dis: map[uint]bool{2: true}}
	svc := NewService(repo, Options{Now: e.clk.Now})

	opts, _, err := svc.Instances(all)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		if o.ID == 2 {
			t.Fatal("已停用的实例不该出现在可巡检实例里")
		}
	}
	if len(opts) != 2 { // 1 和 3；4 是 redis（不支持巡检），2 已停用
		t.Fatalf("可巡检实例应是 1 和 3: %+v", opts)
	}
	res, err := svc.RunNow(admin, all, []uint{2})
	if err != nil || len(res.Disabled) != 1 || res.Disabled[0] != "prod-mysql-02" || len(res.Created) != 0 {
		t.Fatalf("立即巡检已停用的实例应被明确跳过: %+v %v", res, err)
	}
	if len(e.repo.reports) != 0 {
		t.Fatal("不能为已停用的实例建报告")
	}
	list, err := svc.resolveInstances(&Plan{InstanceIDs: []uint{1, 2, 3}})
	if err != nil || len(list) != 2 {
		t.Fatalf("计划也不再连接已停用的实例: %+v %v", list, err)
	}
	for _, in := range list {
		if in.ID == 2 {
			t.Fatal("计划里不该包含已停用的实例")
		}
	}
	// 没有实现该能力的仓库（旧行为）：一个都不跳过
	plain, _, _ := e.svc.Instances(all)
	found := false
	for _, o := range plain {
		found = found || o.ID == 2
	}
	if !found {
		t.Fatal("默认行为不变：没有停用记录时实例 2 照常可选")
	}
}
