// @author: kevinkelly
// @license: Apache Licence
// @file: sqlflow_export_ext_test.go
// @time: 2026/10/28

package store

import (
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"dbtool/internal/model"
)

func lastQuery(d *fakeDB) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.logs[len(d.logs)-1]
}

// 导出原值执行时要复核提交人：只认启用的超级用户。
func TestSQLRepoUserIsSuperuser(t *testing.T) {
	d := &fakeDB{results: []fakeResult{{match: "FROM `dbt_sql_users`", cols: []string{"count(*)"}, rows: [][]driver.Value{{int64(1)}}}}}
	r := &SQLRepo{Store: newNotifyStore(t, d)}
	ok, err := r.UserIsSuperuser("root")
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	q := lastQuery(d)
	for _, want := range []string{"`dbt_sql_users`", "username = ?", "is_active = ?", "is_superuser = ?"} {
		if !strings.Contains(q, want) {
			t.Errorf("应只数启用的超级用户，缺少 %q：%s", want, q)
		}
	}
	d0 := &fakeDB{results: []fakeResult{{match: "FROM `dbt_sql_users`", cols: []string{"count(*)"}, rows: [][]driver.Value{{int64(0)}}}}}
	if ok, err := (&SQLRepo{Store: newNotifyStore(t, d0)}).UserIsSuperuser("ghost"); err != nil || ok {
		t.Errorf("没有这个用户应是 false：%v %v", ok, err)
	}
}

func TestSQLRepoExportExt(t *testing.T) {
	// 表没建：读到 nil、批量查询为空，不报错
	none := &fakeDB{}
	r0 := &SQLRepo{Store: newNotifyStore(t, none)}
	if r0.ExportExtReady() {
		t.Error("表没建应是 false")
	}
	if e, err := r0.GetExportExt(7); e != nil || err != nil {
		t.Errorf("表没建读到 nil：%v %v", e, err)
	}
	if m, err := r0.ExportUnmaskedIDs([]uint{1, 2}); err != nil || len(m) != 0 {
		t.Errorf("表没建应是空集合：%v %v", m, err)
	}
	// 表已建：批量查询只取 unmasked = 1 的
	d := &fakeDB{tables: map[string]bool{"dbt_sql_export_ext": true}, results: []fakeResult{
		{match: "FROM `dbt_sql_export_ext`", cols: []string{"workflow_id", "unmasked", "reason", "set_by", "created_at"},
			rows: [][]driver.Value{{int64(5), int64(1), "取证", "root", time.Now()}, {int64(9), int64(1), "对账", "root", time.Now()}}},
	}}
	r := &SQLRepo{Store: newNotifyStore(t, d)}
	m, err := r.ExportUnmaskedIDs([]uint{5, 6, 9})
	if err != nil || !m[5] || !m[9] || m[6] || len(m) != 2 {
		t.Fatalf("%v %v", m, err)
	}
	if q := lastQuery(d); !strings.Contains(q, "workflow_id IN") || !strings.Contains(q, "unmasked = ?") {
		t.Errorf("只查选了导出原值的：%s", q)
	}
	if err := r.UpsertExportExt(&modelExportExt); err != nil {
		t.Fatal(err)
	}
	if q := lastQuery(d); !strings.Contains(q, "ON DUPLICATE KEY UPDATE") || !strings.Contains(q, "`dbt_sql_export_ext`") {
		t.Errorf("应是幂等的 upsert：%s", q)
	}
}

var modelExportExt = model.SqlExportExt{WorkflowID: 5, Unmasked: true, Reason: "取证", SetBy: "root", CreatedAt: time.Now()}

// 批量导出“合并成一个文件”要 merge_files 这一列：旧版 290 脚本建的表没有，要重新执行脚本补上。
func TestSQLRepoExportMergeColumn(t *testing.T) {
	ext := model.SqlExportExt{WorkflowID: 8, MergeFiles: true, CreatedAt: time.Now()}
	col := func(n int64) fakeResult {
		return fakeResult{match: "INFORMATION_SCHEMA.columns", cols: []string{"count(*)"}, rows: [][]driver.Value{{n}}}
	}
	// 新表：这一列在，写入时带上它
	d := &fakeDB{tables: map[string]bool{"dbt_sql_export_ext": true}, results: []fakeResult{col(1)}}
	r := &SQLRepo{Store: newNotifyStore(t, d)}
	if !r.ExportMergeReady() {
		t.Fatal("表和列都在应是 true")
	}
	if err := r.UpsertExportExt(&ext); err != nil {
		t.Fatal(err)
	}
	q := lastQuery(d)
	for _, want := range []string{"`merge_files`", "ON DUPLICATE KEY UPDATE", "`merge_files`=VALUES(`merge_files`)"} {
		if !strings.Contains(q, want) {
			t.Errorf("新表应写入并更新合并选项，缺少 %q：%s", want, q)
		}
	}
	if strings.Contains(q, "merge_col") {
		t.Errorf("不应再有 merge_col：%s", q)
	}
	// 旧表：没有这一列。ExportMergeReady 为 false；写入只用旧的列，不能让“导出原值”跟着失败
	old := &fakeDB{tables: map[string]bool{"dbt_sql_export_ext": true}, results: []fakeResult{col(0)}}
	ro := &SQLRepo{Store: newNotifyStore(t, old)}
	if ro.ExportMergeReady() {
		t.Error("缺列应是 false")
	}
	if err := ro.UpsertExportExt(&modelExportExt); err != nil {
		t.Fatal(err)
	}
	qo := lastQuery(old)
	if strings.Contains(qo, "merge_files") || !strings.Contains(qo, "`unmasked`") || !strings.Contains(qo, "ON DUPLICATE KEY UPDATE") {
		t.Errorf("旧表不应写合并列：%s", qo)
	}
	// 整张表没建
	if (&SQLRepo{Store: newNotifyStore(t, &fakeDB{})}).ExportMergeReady() {
		t.Error("表没建应是 false")
	}
}
