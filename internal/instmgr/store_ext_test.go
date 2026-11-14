// @author: kevinkelly
// @license: Apache Licence
// @file: store_ext_test.go
// @time: 2026/10/28

package instmgr

import (
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/instext"
)

// 旁路表是否存在由 information_schema.tables 的计数决定；这里让假驱动按表名回答。
func tablesExist(names ...string) func(q string, args []driver.Value) ([]string, [][]driver.Value, error) {
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	return func(q string, args []driver.Value) ([]string, [][]driver.Value, error) {
		if strings.Contains(q, "information_schema.tables") && len(args) >= 2 {
			n := int64(0)
			if have[anyString(args[1])] {
				n = 1
			}
			return []string{"count(*)"}, [][]driver.Value{{n}}, nil
		}
		if strings.HasPrefix(q, "SELECT count(*)") {
			return []string{"count(*)"}, [][]driver.Value{{int64(0)}}, nil
		}
		return nil, nil, nil
	}
}

func TestStoreListInstancesStateFilter(t *testing.T) {
	instext.Reset()
	defer instext.Reset()
	st, d, _ := newRecStore(t)
	d.rows = tablesExist("dbt_instance_ext")
	if _, _, err := st.ListInstances(InstanceFilter{State: "disabled", Env: "prod", Limit: 10}, nil); err != nil {
		t.Fatal(err)
	}
	sel := d.find("FROM `dbt_sql_instance`")
	last := sel[len(sel)-1].q
	if !strings.Contains(last, "dbt_sql_instance.id IN (SELECT instance_id FROM dbt_instance_ext WHERE disabled = 1)") || !strings.Contains(last, "WHERE env = ?") {
		t.Fatalf("按已停用 / 环境筛选: %s", last)
	}
	d.log = nil
	if _, _, err := st.ListInstances(InstanceFilter{State: "enabled", Limit: 10}, nil); err != nil {
		t.Fatal(err)
	}
	sel = d.find("FROM `dbt_sql_instance`")
	if !strings.Contains(sel[len(sel)-1].q, "dbt_sql_instance.id NOT IN (SELECT instance_id FROM dbt_instance_ext WHERE disabled = 1)") {
		t.Fatalf("排除已停用: %s", sel[len(sel)-1].q)
	}
	// 没有筛选时不引入旁路表（默认行为不变）
	d.log = nil
	if _, _, err := st.ListInstances(InstanceFilter{Limit: 10}, nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range d.find("FROM `dbt_sql_instance`") {
		if strings.Contains(s.q, "dbt_instance_ext") {
			t.Fatalf("没有状态筛选时不该碰旁路表: %s", s.q)
		}
	}
}

func TestStoreListInstancesStateFilterWithoutTable(t *testing.T) {
	instext.Reset()
	defer instext.Reset()
	st, d, _ := newRecStore(t)
	d.rows = tablesExist() // 一张都没建
	list, total, err := st.ListInstances(InstanceFilter{State: "disabled", Limit: 10}, nil)
	if err != nil || list != nil || total != 0 || len(d.find("FROM `dbt_sql_instance`")) != 0 {
		t.Fatalf("表没建时“已停用”筛选无结果、也不去查实例表: %v %d %v", list, total, err)
	}
	if _, _, err := st.ListInstances(InstanceFilter{State: "enabled", Limit: 10}, nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range d.find("FROM `dbt_sql_instance`") {
		if strings.Contains(s.q, "dbt_instance_ext") {
			t.Fatalf("表没建时不能引用它: %s", s.q)
		}
	}
}

func TestStoreBriefInstancesExcludesDisabled(t *testing.T) {
	instext.Reset()
	defer instext.Reset()
	st, d, _ := newRecStore(t)
	d.rows = tablesExist("dbt_instance_ext")
	if _, err := st.BriefInstances(nil); err != nil {
		t.Fatal(err)
	}
	sel := d.find("FROM `dbt_sql_instance`")
	if len(sel) != 1 || !strings.Contains(sel[0].q, "NOT IN (SELECT instance_id FROM dbt_instance_ext WHERE disabled = 1)") {
		t.Fatalf("下拉里不该有已停用的实例: %v", d.stmts())
	}
	instext.Reset()
	d.log, d.rows = nil, tablesExist()
	if _, err := st.BriefInstances(nil); err != nil {
		t.Fatal(err)
	}
	if sel := d.find("FROM `dbt_sql_instance`"); len(sel) != 1 || strings.Contains(sel[0].q, "dbt_instance_ext") {
		t.Fatalf("表没建时语句与原来一致: %v", d.stmts())
	}
}

func TestStoreInstanceExtWrites(t *testing.T) {
	st, d, _ := newRecStore(t)
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)
	if err := st.SetInstanceDisabled(5, true, "root", at); err != nil {
		t.Fatal(err)
	}
	ins := d.find("INSERT INTO `dbt_instance_ext`")
	if len(ins) != 1 || !strings.Contains(ins[0].q, "ON DUPLICATE KEY UPDATE") || !strings.Contains(ins[0].q, "`disabled`=VALUES(`disabled`)") ||
		!strings.Contains(ins[0].q, "`disabled_by`=VALUES(`disabled_by`)") {
		t.Fatalf("停用写旁路表（upsert）: %v", d.stmts())
	}
	d.log = nil
	if err := st.SetInstanceEnv(5, "prod", at); err != nil {
		t.Fatal(err)
	}
	ins = d.find("INSERT INTO `dbt_instance_ext`")
	if len(ins) != 1 || strings.Contains(ins[0].q, "`disabled`=VALUES") || !strings.Contains(ins[0].q, "`env`=VALUES(`env`)") {
		t.Fatalf("改环境不能顺带改停用状态: %v", d.stmts())
	}
}

func TestStoreAccountRegAndTemplateExt(t *testing.T) {
	st, d, _ := newRecStore(t)
	d.rows = tablesExist("dbt_account_reg", "dbt_param_template_ext")
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)
	if err := st.SaveAccountReg(3, AccountReg{User: "app", Host: "10.%", Owner: "dev", Purpose: "p", ExpireDate: "2026-12-31"}, "root", at); err != nil {
		t.Fatal(err)
	}
	ins := d.find("INSERT INTO `dbt_account_reg`")
	if len(ins) != 1 || !strings.Contains(ins[0].q, "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("%v", d.stmts())
	}
	if err := st.SaveAccountReg(3, AccountReg{User: "app", Host: "10.%", ExpireDate: "bad"}, "root", at); err == nil {
		t.Fatal("日期格式错误应报错")
	}
	d.log = nil
	if err := st.DeleteAccountReg(3, "app", "10.%"); err != nil || len(d.find("DELETE FROM `dbt_account_reg`")) != 1 {
		t.Fatalf("%v %v", err, d.stmts())
	}
	d.log = nil
	if err := st.SaveTemplateRecommended(9, "ON", "root", at); err != nil || len(d.find("INSERT INTO `dbt_param_template_ext`")) != 1 {
		t.Fatalf("%v %v", err, d.stmts())
	}
	// 表没建：读返回空、删除不报错、Ready 为假
	st2, d2, _ := newRecStore(t)
	d2.rows = tablesExist()
	if st2.AccountRegReady() || st2.TemplateExtReady() {
		t.Fatal("表没建应不可用")
	}
	regs, err := st2.AccountRegs(1)
	if err != nil || len(regs) != 0 || len(d2.find("FROM `dbt_account_reg`")) != 0 { // 表没建时不去读它
		t.Fatalf("%v %v %v", regs, err, d2.stmts())
	}
	if err := st2.DeleteAccountReg(1, "a", "b"); err != nil || len(d2.find("DELETE")) != 0 {
		t.Fatalf("%v", d2.stmts())
	}
}

func TestStoreListAllParamHistory(t *testing.T) {
	st, d, _ := newRecStore(t)
	d.rows = func(q string, _ []driver.Value) ([]string, [][]driver.Value, error) {
		if strings.HasPrefix(q, "SELECT count(*)") {
			return []string{"c"}, [][]driver.Value{{int64(1)}}, nil
		}
		return nil, nil, nil
	}
	if _, _, err := st.ListAllParamHistory(map[uint]bool{1: true, 3: true}, "max_", 20, 10); err != nil {
		t.Fatal(err)
	}
	h := d.find("dbt_param_history ph")
	sel := h[len(h)-1].q
	for _, want := range []string{"JOIN dbt_sql_instance i ON i.id = ph.instance_id", "ph.instance_id IN (?,?)", "LIKE ? ESCAPE '!'", "ORDER BY ph.id DESC"} {
		if !strings.Contains(sel, want) {
			t.Errorf("缺少 %q: %s", want, sel)
		}
	}
	n := len(d.stmts())
	if list, total, err := st.ListAllParamHistory(map[uint]bool{}, "", 0, 10); err != nil || list != nil || total != 0 || len(d.stmts()) != n {
		t.Fatalf("没有可见实例时不查库")
	}
}

func TestStoreInstanceRefsIncludePerfIssue(t *testing.T) {
	st, d, _ := newRecStore(t)
	d.rows = func(q string, _ []driver.Value) ([]string, [][]driver.Value, error) {
		if strings.Contains(q, "FROM dbt_perf_issue") {
			return []string{"c"}, [][]driver.Value{{int64(4)}}, nil
		}
		return []string{"c"}, [][]driver.Value{{int64(0)}}, nil
	}
	refs, err := st.InstanceRefs(9)
	if err != nil || len(refs) != 1 || refs[0].Label != "性能工单" || refs[0].Count != 4 || !refs[0].Blocking {
		t.Fatalf("删除前引用检查应包含性能工单: %+v %v", refs, err)
	}
}

// S8-C13：删除实例前的引用检查要包含数据归档任务（阻断）与运行记录（只提示）；表不存在（没执行 180_archive.sql）时跳过，不报错。
func TestStoreInstanceRefsIncludeArchive(t *testing.T) {
	st, d, _ := newRecStore(t)
	d.rows = func(q string, _ []driver.Value) ([]string, [][]driver.Value, error) {
		switch {
		case strings.Contains(q, "FROM dbt_archive_task"):
			return []string{"c"}, [][]driver.Value{{int64(2)}}, nil
		case strings.Contains(q, "FROM dbt_archive_run"):
			return []string{"c"}, [][]driver.Value{{int64(7)}}, nil
		}
		return []string{"c"}, [][]driver.Value{{int64(0)}}, nil
	}
	refs, err := st.InstanceRefs(9)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Ref{}
	for _, r := range refs {
		by[r.Label] = r
	}
	if len(refs) != 2 || by["数据归档任务"].Count != 2 || !by["数据归档任务"].Blocking || by["数据归档运行记录"].Count != 7 || by["数据归档运行记录"].Blocking {
		t.Fatalf("应包含归档任务（阻断）与运行记录（只提示）: %+v", refs)
	}
	// 任务既按来源实例也按库表备份的目标实例匹配
	q := d.find("FROM dbt_archive_task")
	if len(q) != 1 || len(q[0].args) != 2 || !strings.Contains(q[0].q, "dest_table") {
		t.Fatalf("归档任务应同时匹配来源与目标实例: %+v", q)
	}
	for _, a := range q[0].args {
		if anyString(a) != "9" {
			t.Fatalf("实例 ID 绑定: %+v", q[0].args)
		}
	}
	if q := d.find("FROM dbt_archive_run"); len(q) != 1 || len(q[0].args) != 2 {
		t.Fatalf("运行记录按来源与备份目标实例匹配: %+v", q)
	}

	// 没执行 180_archive.sql：表不存在，跳过而不是报错
	d.rows = func(q string, _ []driver.Value) ([]string, [][]driver.Value, error) {
		if strings.Contains(q, "dbt_archive_") {
			return nil, nil, errors.New("Error 1146 (42S02): Table 'archery.dbt_archive_task' doesn't exist")
		}
		return []string{"c"}, [][]driver.Value{{int64(0)}}, nil
	}
	if refs, err := st.InstanceRefs(9); err != nil || len(refs) != 0 {
		t.Fatalf("归档表不存在时应跳过: %+v %v", refs, err)
	}
}
