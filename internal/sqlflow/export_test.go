// @author: kevinkelly
// @license: Apache Licence
// @file: export_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	exportpkg "dbtool/internal/export"
	"dbtool/internal/model"
	"dbtool/internal/notify"
	"dbtool/internal/review"
	"dbtool/internal/settings"
	"dbtool/internal/sqlflow"
)

// ---- 数据导出的测试场景 ----
//
// 在 setup 的基础上：dev 拥有 export.submit / export.submit_batch / export.download；
// 实例 1（MySQL）、11、12、13（SQL Server）都带 can_read 标签；每个实例有自己的假引擎状态（行、行数、故障）。

type qState struct {
	mu      sync.Mutex
	cols    []string
	rows    [][]any
	count   int64
	failQ   string            // 查询里含这个文本时失败
	plan    *engine.ResultSet // 非空时 EXPLAIN 开头的查询返回它（防火墙的 scan_rows 条件）
	noConn  bool
	queries []string
	opts    []engine.QueryOptions
}

type qEngine struct {
	engine.Engine
	host string
	st   *qState
}

func (q *qEngine) Close() error { return nil }
func (q *qEngine) AllDatabases(context.Context) ([]string, error) {
	if q.st.noConn {
		return nil, fmt.Errorf("dial tcp %s: connection refused", q.host)
	}
	return []string{"shop", "ShopDB"}, nil
}
func (q *qEngine) Query(_ context.Context, sqlText string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	q.st.mu.Lock()
	defer q.st.mu.Unlock()
	if q.st.noConn {
		return nil, fmt.Errorf("dial tcp %s: connection refused", q.host)
	}
	q.st.queries = append(q.st.queries, sqlText)
	q.st.opts = append(q.st.opts, opt)
	if q.st.failQ != "" && strings.Contains(sqlText, q.st.failQ) {
		return nil, fmt.Errorf("Invalid object name 'orders'.")
	}
	if q.st.plan != nil && strings.HasPrefix(sqlText, "EXPLAIN ") {
		return q.st.plan, nil
	}
	if strings.HasPrefix(sqlText, "SELECT COUNT(*) FROM (") {
		return &engine.ResultSet{ColumnList: []string{"c"}, Rows: [][]any{{q.st.count}}}, nil
	}
	if strings.HasSuffix(sqlText, ") t LIMIT 0") || strings.HasPrefix(sqlText, "SELECT TOP 0 * FROM (") { // 合并导出提交前取列名
		return &engine.ResultSet{ColumnList: q.st.cols, Rows: [][]any{}}, nil
	}
	rows := q.st.rows
	if opt.Limit > 0 && len(rows) > opt.Limit {
		rows = rows[:opt.Limit]
	}
	return &engine.ResultSet{ColumnList: q.st.cols, Rows: rows}, nil
}

type exportEnv struct {
	*env
	dir   string
	store *exportpkg.LocalStorage
	hosts map[string]*qState
}

func setupExport(t *testing.T) *exportEnv {
	t.Helper()
	e := setup(t)
	rd := []model.InstanceTag{{TagCode: "can_read", Active: true}}
	for i := range e.repo.Instances { // 实例 1：MySQL，同时带 can_write 与 can_read
		if e.repo.Instances[i].ID == 1 {
			e.repo.Instances[i].Tags = append(e.repo.Instances[i].Tags, rd...)
			e.repo.Instances[i].Host = "my1"
		}
	}
	xe := &exportEnv{env: e, dir: filepath.Join(t.TempDir(), "exports"), hosts: map[string]*qState{}}
	xe.hosts["my1"] = &qState{}
	for _, id := range []uint{11, 12, 13} {
		host := fmt.Sprintf("ms%d", id)
		e.repo.Instances = append(e.repo.Instances, model.Instance{ID: id, InstanceName: fmt.Sprintf("mssql-%d", id), DBType: "mssql", Host: host, Port: 1433, Tags: rd})
		e.repo.InstRGs[id] = []uint{1}
		xe.hosts[host] = &qState{}
	}
	for _, st := range xe.hosts {
		st.cols = []string{"id", "name"}
		st.rows = [][]any{{int64(1), "张三"}, {int64(2), "李四"}}
		st.count = 2
	}
	var err error
	if xe.store, err = exportpkg.NewLocalStorage(xe.dir); err != nil {
		t.Fatal(err)
	}
	e.repo.Perms[1]["export.submit"] = true
	e.repo.Perms[1]["export.submit_batch"] = true
	e.repo.Perms[1]["export.download"] = true
	xe.rebuild()
	return xe
}

func (xe *exportEnv) rebuild() {
	s := sqlflow.NewService(xe.repo, sqlflow.Options{Notifier: xe.cap, ExportStorage: xe.store, Open: func(_ string, c engine.Conn) (engine.Engine, error) {
		st := xe.hosts[c.Host]
		if st == nil {
			return nil, fmt.Errorf("没有这台假实例 %q", c.Host)
		}
		return &qEngine{host: c.Host, st: st}, nil
	}})
	s.SetAsync(func(f func()) { f() })
	s.SetSpawn(func(f func()) { f() })
	s.SetClock(func() time.Time { return xe.now })
	s.BaseURL = "http://dba.example.com"
	xe.svc = s
}

func (xe *exportEnv) req(sqlText, format string, ids ...uint) sqlflow.SubmitReq {
	r := sqlflow.SubmitReq{Name: "导出订单", DBName: "shop", GroupName: "研发组", SQL: sqlText, Export: &sqlflow.ExportSpec{Format: format}}
	if len(ids) == 1 {
		r.InstanceID = ids[0]
	} else {
		r.BatchInstanceIDs = ids
	}
	return r
}

func (xe *exportEnv) submit(t *testing.T, r sqlflow.SubmitReq) uint {
	t.Helper()
	res, err := xe.svc.Submit(context.Background(), dev, r)
	if err != nil {
		t.Fatal(err)
	}
	return res.WorkflowID
}

func (xe *exportEnv) run(t *testing.T, id uint) {
	t.Helper()
	xe.approve(t, id)
	if _, err := xe.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
}

func (xe *exportEnv) zipEntries(t *testing.T, name string) map[string]string {
	t.Helper()
	f, size, err := xe.store.Open(name)
	if err != nil {
		t.Fatalf("打开 %s: %v", name, err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if int64(len(data)) != size {
		t.Errorf("大小不一致")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), size)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, zf := range zr.File {
		rc, _ := zf.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[zf.Name] = string(b)
	}
	return out
}

func fileOf(xe *exportEnv, id uint) string {
	if w := xe.repo.Workflows[id]; w.FileName != nil {
		return *w.FileName
	}
	return ""
}

const exportSQL = "select id, name from orders where id > 0 order by id"

// ---- 提交与检查 ----

func TestExportSubmitAndCheck(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	r, err := xe.svc.PreviewExport(ctx, dev, []uint{1}, false, "shop", exportSQL)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != sqlflow.SourceExport || r.ErrorCount != 0 || r.SyntaxType != sqlflow.SyntaxExport || len(r.Rows) != 1 ||
		r.Rows[0].StageStatus != "行数统计完成" || r.Rows[0].AffectedRows != 2 || r.Rows[0].ErrorMessage != "None" {
		t.Fatalf("检查结果: %+v", r)
	}
	if q := xe.hosts["my1"].queries; len(q) != 1 || q[0] != "SELECT COUNT(*) FROM ("+exportSQL+") t" || xe.hosts["my1"].opts[0].DBName != "shop" || xe.hosts["my1"].opts[0].MaxExecMs != 60000 {
		t.Errorf("统计语句与选项: %v %+v", q, xe.hosts["my1"].opts)
	}

	id := xe.submit(t, xe.req(exportSQL, "csv", 1))
	w := xe.repo.Workflows[id]
	if w.IsOfflineExport != 1 || w.ExportFormat == nil || *w.ExportFormat != "csv" || w.SyntaxType != 3 || w.IsBackup || w.Status != model.WfManReviewing || w.Instance != nil {
		t.Errorf("工单字段: %+v", w)
	}
	if first := xe.cap.msgs[0]; first.Name != notify.EventPending || !strings.Contains(first.Title, "[数据导出申请]") {
		t.Errorf("通知标题应区分导出: %+v", first)
	}
	d, err := xe.svc.Get(dev, id)
	if err != nil || !d.IsExport || d.ExportFormat != "csv" || d.CanDownload || d.SyntaxLabel != "离线导出" {
		t.Errorf("详情: %+v %v", d, err)
	}
}

func TestExportValidation(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	submit := func(who audit.Actor, r sqlflow.SubmitReq) error { _, err := xe.svc.Submit(ctx, who, r); return err }

	if err := submit(leader, xe.req(exportSQL, "csv", 1)); code(err) != "forbidden" || !strings.Contains(err.Error(), "export.submit") {
		t.Errorf("没有导出权限: %v", err)
	}
	if err := submit(leader, xe.req(exportSQL, "csv", 11, 12)); code(err) != "forbidden" || !strings.Contains(err.Error(), "export.submit_batch") {
		t.Errorf("没有批量导出权限: %v", err)
	}
	if err := submit(dev, xe.req(exportSQL, "pdf", 1)); code(err) != "bad_request" || !strings.Contains(err.Error(), "导出格式") {
		t.Errorf("格式: %v", err)
	}
	if err := submit(dev, xe.req(exportSQL, "", 1)); code(err) != "bad_request" {
		t.Errorf("格式为空: %v", err)
	}
	r := xe.req(exportSQL, "csv", 1)
	r.SkipCheck = true
	if err := submit(root, r); code(err) != "bad_request" {
		t.Errorf("导出不支持跳过检查: %v", err)
	}
	// 实例只有 can_write（没有读标签）→ 不能导出
	xe.repo.Instances = append(xe.repo.Instances, model.Instance{ID: 20, InstanceName: "write-only", DBType: "mysql", Host: "w20", Tags: []model.InstanceTag{{TagCode: "can_write", Active: true}}})
	xe.repo.InstRGs[20] = []uint{1}
	xe.hosts["w20"] = &qState{}
	if err := submit(dev, xe.req(exportSQL, "csv", 20)); code(err) != "bad_request" || !strings.Contains(err.Error(), "can_read") {
		t.Errorf("缺读标签: %v", err)
	}
	// 反过来：只读实例不能做上线变更
	xe.repo.Instances = append(xe.repo.Instances, model.Instance{ID: 21, InstanceName: "read-only", DBType: "mysql", Host: "r21", Tags: []model.InstanceTag{{TagCode: "can_read", Active: true}}})
	xe.repo.InstRGs[21] = []uint{1}
	xe.hosts["r21"] = &qState{}
	if _, err := xe.svc.Submit(ctx, dev, sqlflow.SubmitReq{Name: "x", InstanceID: 21, DBName: "shop", GroupName: "研发组", SQL: "update t set a=1 where id=1"}); code(err) != "bad_request" {
		t.Errorf("只读实例不能上线: %v", err)
	}
	// 批量导出的实例类型限制同批量上线
	if err := submit(dev, xe.req(exportSQL, "csv", 1, 11)); code(err) != "bad_request" || !strings.Contains(err.Error(), "批量导出只支持") {
		t.Errorf("批量只允许 SQL Server: %v", err)
	}
	if err := submit(outsider, xe.req(exportSQL, "csv", 1)); code(err) != "forbidden" {
		t.Errorf("其他资源组: %v", err)
	}
	if len(xe.repo.Workflows) != 0 {
		t.Errorf("校验失败不应留下工单: %d", len(xe.repo.Workflows))
	}
}

func TestExportCheckRejectsNonQueryAndTooManyRows(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	for sqlText, want := range map[string]string{
		"update orders set a=1 where id=1":  "违规语句",
		"select * from t into outfile '/x'": "写出文件",
		"select 1; select 2":                "只支持一条",
		"select load_file('/etc/passwd')":   "load_file",
		"delete from orders":                "违规语句",
	} {
		r, err := xe.svc.PreviewExport(ctx, dev, []uint{1}, false, "shop", sqlText)
		if err != nil || r.ErrorCount != 1 || !strings.Contains(r.Rows[0].ErrorMessage, want) || r.Rows[0].StageStatus != "检查未通过！" {
			t.Errorf("%q: %+v %v", sqlText, r, err)
		}
	}
	// 行数超过阈值（默认 10000；可用 max_export_rows 调）
	xe.hosts["my1"].count = 10001
	r, _ := xe.svc.PreviewExport(ctx, dev, []uint{1}, false, "shop", exportSQL)
	if r.ErrorCount != 1 || !strings.Contains(r.Rows[0].ErrorMessage, "导出数据行数(10001)超过阈值(10000)") {
		t.Errorf("超过阈值: %+v", r.Rows)
	}
	xe.repo.Cfg[settings.KeyExportMaxRows] = "20000"
	if r, _ = xe.svc.PreviewExport(ctx, dev, []uint{1}, false, "shop", exportSQL); r.ErrorCount != 0 {
		t.Errorf("阈值调大后应通过: %+v", r.Rows)
	}
	// 检查有错误 → 系统自动驳回（同上线工单）
	xe.hosts["my1"].count = 99999
	res, err := xe.svc.Submit(ctx, dev, xe.req(exportSQL, "csv", 1))
	if err != nil || res.Status != model.WfAutoReviewWrong {
		t.Errorf("行数超限应被自动驳回: %+v %v", res, err)
	}
	// 统计失败
	xe.hosts["my1"].count = 1
	xe.hosts["my1"].failQ = "COUNT"
	if r, _ = xe.svc.PreviewExport(ctx, dev, []uint{1}, false, "shop", exportSQL); r.ErrorCount != 1 || !strings.Contains(r.Rows[0].ErrorMessage, "检查失败: Invalid object name") {
		t.Errorf("统计失败: %+v", r.Rows)
	}
}

func TestExportMSSQLCheck(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	// 最外层 ORDER BY 要去掉才能包成派生表（SQL Server 报 1033）
	r, err := xe.svc.PreviewExport(ctx, dev, []uint{11}, false, "ShopDB", "select id from dbo.Orders where id > 0 order by id")
	if err != nil || r.ErrorCount != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	if q := xe.hosts["ms11"].queries[0]; q != "SELECT COUNT(*) FROM (select id from dbo.Orders where id > 0) t" {
		t.Errorf("T-SQL 统计语句: %s", q)
	}
	// 只读校验：夹带写语句、SELECT INTO
	for sqlText, want := range map[string]string{
		"select 1\nupdate dbo.T set c=1": "update",
		"select * into #t from dbo.T":    "into",
		"exec sp_who":                    "不允许 exec",
		"select 1; select 2":             "一条查询",
	} {
		r, _ = xe.svc.PreviewExport(ctx, dev, []uint{11}, false, "ShopDB", sqlText)
		if r.ErrorCount != 1 || !strings.Contains(r.Rows[0].ErrorMessage, want) {
			t.Errorf("%q: %+v", sqlText, r.Rows)
		}
	}
	// DECLARE/SET @ 批处理：跳过行数预统计，给警告
	r, _ = xe.svc.PreviewExport(ctx, dev, []uint{11}, false, "ShopDB", "declare @d date = '2026-01-01';\nselect * from dbo.T where d = @d")
	if r.ErrorCount != 0 || r.WarningCount != 1 || r.Rows[0].StageStatus != "批处理语句，跳过行数预统计" || len(xe.hosts["ms11"].queries) != 1 {
		t.Errorf("批处理: %+v", r.Rows)
	}
}

// ---- 执行与文件 ----

func TestExportSingleExecutesAndWritesZip(t *testing.T) {
	xe := setupExport(t)
	id := xe.submit(t, xe.req(exportSQL, "csv", 1))
	if _, err := xe.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeManual}); code(err) != "conflict" {
		t.Errorf("没审批完不能执行: %v", err)
	}
	xe.approve(t, id)
	xe.cap.reset()
	if _, err := xe.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeManual}); code(err) != "bad_request" || !strings.Contains(err.Error(), "没有“手工执行”") {
		t.Errorf("导出没有手工执行: %v", err)
	}
	if _, err := xe.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto, Executor: sqlflow.ExecGoInception}); code(err) != "bad_request" {
		t.Errorf("导出不能选 goInception: %v", err)
	}
	if _, err := xe.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if xe.status(id) != model.WfFinish {
		t.Fatalf("状态: %s", xe.status(id))
	}
	name := fileOf(xe, id)
	if !strings.HasPrefix(name, "shop_20261005100000_") || !strings.HasSuffix(name, ".zip") {
		t.Fatalf("压缩包名: %q", name)
	}
	entries := xe.zipEntries(t, name)
	if len(entries) != 1 {
		t.Fatalf("包内文件: %v", entries)
	}
	for fn, content := range entries {
		if !strings.HasSuffix(fn, ".csv") || !strings.HasPrefix(content, `"id","name"`+"\r\n"+`"1","张三"`+"\r\n") {
			t.Errorf("包内内容: %s\n%q", fn, content)
		}
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if len(rows) != 1 || rows[0].StageStatus != "执行正常" || rows[0].AffectedRows != 2 || !strings.Contains(rows[0].ErrorMessage, "保存文件: "+name) {
		t.Errorf("执行结果: %+v", rows)
	}
	// 查询语句：带库、行数上限+1（用来判断是否超限）、超时
	st := xe.hosts["my1"]
	last := st.opts[len(st.opts)-1]
	if last.DBName != "shop" || last.Limit != 10001 || last.MaxExecMs != 60000 || st.queries[len(st.queries)-1] != exportSQL {
		t.Errorf("导出查询: %q %+v", st.queries[len(st.queries)-1], last)
	}
	if names := xe.cap.names(); strings.Join(names, ",") != "executing,finished" || !strings.Contains(xe.cap.msgs[1].Title, "[数据导出申请]工单已正常结束") {
		t.Errorf("通知: %v", xe.cap.msgs)
	}
	d, _ := xe.svc.Get(dev, id)
	if !d.CanDownload || d.FileName != name {
		t.Errorf("详情应可下载: %+v", d)
	}
}

func TestExportAllFormats(t *testing.T) {
	xe := setupExport(t)
	for _, f := range exportpkg.Formats {
		id := xe.submit(t, xe.req(exportSQL, f, 1))
		xe.run(t, id)
		if xe.status(id) != model.WfFinish {
			t.Fatalf("%s: %s %s", f, xe.status(id), xe.repo.Contents[id].ExecuteResult)
		}
		entries := xe.zipEntries(t, fileOf(xe, id))
		for fn, content := range entries {
			if !strings.HasSuffix(fn, "."+f) || len(content) < 20 {
				t.Errorf("%s: %s %q", f, fn, content)
			}
		}
	}
}

func TestExportRowCapAtExecution(t *testing.T) {
	xe := setupExport(t)
	id := xe.submit(t, xe.req(exportSQL, "csv", 1))
	xe.approve(t, id)
	// 提交后数据增长：执行时 Query 返回 cap+1 行，必须中止，不能把超限的数据写出去
	xe.repo.Cfg[settings.KeyExportMaxRows] = "2"
	xe.hosts["my1"].rows = [][]any{{int64(1), "a"}, {int64(2), "b"}, {int64(3), "c"}}
	xe.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if xe.status(id) != model.WfException || len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "超过阈值(2)") || fileOf(xe, id) != "" {
		t.Errorf("超限应中止且不留文件: %s %+v", xe.status(id), rows)
	}
	ents, _ := os.ReadDir(xe.dir)
	if len(ents) != 0 {
		t.Errorf("不应写出文件: %v", ents)
	}
	// 查询失败
	id2 := xe.submit(t, xe.req(exportSQL, "csv", 1))
	xe.approve(t, id2)
	xe.repo.Cfg[settings.KeyExportMaxRows] = ""
	xe.hosts["my1"].failQ = "order by"
	xe.svc.Execute(context.Background(), dev, id2, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	if xe.status(id2) != model.WfException || !strings.Contains(xe.repo.Contents[id2].ExecuteResult, "Invalid object name") {
		t.Errorf("查询失败: %s", xe.status(id2))
	}
}

func TestExportNonLocalStorageIsRefused(t *testing.T) {
	xe := setupExport(t)
	id := xe.submit(t, xe.req(exportSQL, "csv", 1))
	xe.approve(t, id)
	xe.rebuild()
	// 去掉注入的本地存储，模拟按 dbt_sql_config 取存储；配置成 sftp
	s := sqlflow.NewService(xe.repo, sqlflow.Options{Notifier: xe.cap, ExportBaseDir: t.TempDir(), Open: func(_ string, c engine.Conn) (engine.Engine, error) {
		return &qEngine{host: c.Host, st: xe.hosts[c.Host]}, nil
	}})
	s.SetAsync(func(f func()) { f() })
	s.SetSpawn(func(f func()) { f() })
	s.SetClock(func() time.Time { return xe.now })
	xe.svc = s
	xe.repo.Cfg[settings.KeyExportStorageType] = "sftp"
	s.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	if xe.status(id) != model.WfException || !strings.Contains(xe.repo.Contents[id].ExecuteResult, "暂只支持本地存储") {
		t.Errorf("sftp 应明确拒绝: %s %s", xe.status(id), xe.repo.Contents[id].ExecuteResult)
	}
	// 本地存储：目录取 local_path，相对路径相对 ExportBaseDir
	base := t.TempDir()
	xe.repo.Cfg[settings.KeyExportStorageType], xe.repo.Cfg[settings.KeyExportLocalPath] = "local", "out/files/"
	s2 := sqlflow.NewService(xe.repo, sqlflow.Options{Notifier: xe.cap, ExportBaseDir: base, Open: func(_ string, c engine.Conn) (engine.Engine, error) {
		return &qEngine{host: c.Host, st: xe.hosts[c.Host]}, nil
	}})
	s2.SetAsync(func(f func()) { f() })
	s2.SetSpawn(func(f func()) { f() })
	s2.SetClock(func() time.Time { return xe.now })
	xe.svc = s2
	id2 := xe.submit(t, xe.req(exportSQL, "json", 1))
	xe.run(t, id2)
	ents, err := os.ReadDir(filepath.Join(base, "out", "files"))
	if err != nil || len(ents) != 1 || xe.status(id2) != model.WfFinish {
		t.Errorf("相对路径应落在 ExportBaseDir 下: %v %v %s", ents, err, xe.status(id2))
	}
}

// ---- 下载 ----

func TestExportDownload(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	id := xe.submit(t, xe.req(exportSQL, "csv", 1))
	if _, err := xe.svc.Download(dev, id); code(err) != "conflict" {
		t.Errorf("没导出完不能下载: %v", err)
	}
	xe.run(t, id)
	name := fileOf(xe, id)

	f, err := xe.svc.Download(dev, id)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f.Reader)
	f.Reader.Close()
	if f.Name != name || f.Size != int64(len(data)) || len(data) < 50 {
		t.Errorf("下载内容: %s %d", f.Name, f.Size)
	}
	if n := len(xe.repo.AuditEntries); n != 1 || xe.repo.AuditEntries[0].Action != "离线下载" || xe.repo.AuditEntries[0].UserName != "dev" ||
		!strings.Contains(xe.repo.AuditEntries[0].ExtraInfo, fmt.Sprintf("工单id：%d，文件：%s", id, name)) {
		t.Errorf("审计日志: %+v", xe.repo.AuditEntries)
	}
	// 权限：没有 export.download 的人（组长能看到工单，但不能下载）；看不到的人 404
	if _, err := xe.svc.Download(leader, id); code(err) != "forbidden" || !strings.Contains(err.Error(), "export.download") {
		t.Errorf("没有下载权限: %v", err)
	}
	xe.repo.Perms[8]["export.download"] = true
	if _, err := xe.svc.Download(stranger, id); code(err) != "not_found" {
		t.Errorf("看不到的工单: %v", err)
	}
	if f, err := xe.svc.Download(root, id); err != nil {
		t.Errorf("管理员可下载: %v", err)
	} else {
		f.Reader.Close()
	}
	// 不是导出工单
	plain, _ := xe.svc.Submit(ctx, dev, sqlflow.SubmitReq{Name: "x", InstanceID: 1, DBName: "shop", GroupName: "研发组", SQL: "update t set a=1 where id=1"})
	if _, err := xe.svc.Download(dev, plain.WorkflowID); code(err) != "bad_request" {
		t.Errorf("非导出工单: %v", err)
	}

	// 文件被清理：404，且仍记审计日志（带错误）
	if err := xe.store.Delete(name); err != nil {
		t.Fatal(err)
	}
	before := len(xe.repo.AuditEntries)
	if _, err := xe.svc.Download(dev, id); code(err) != "not_found" {
		t.Errorf("文件不存在: %v", err)
	}
	if len(xe.repo.AuditEntries) != before+1 || !strings.Contains(xe.repo.AuditEntries[before].ExtraInfo, "error:") {
		t.Errorf("失败也要记审计: %+v", xe.repo.AuditEntries[before:])
	}
}

func TestExportDownloadRefusesTamperedFileName(t *testing.T) {
	xe := setupExport(t)
	id := xe.submit(t, xe.req(exportSQL, "csv", 1))
	xe.run(t, id)
	secret := filepath.Join(filepath.Dir(xe.dir), "secret.txt")
	os.WriteFile(secret, []byte("top secret"), 0o600)
	for _, bad := range []string{"../secret.txt", `..\secret.txt`, secret, "a/b.zip", ".hidden"} {
		w := xe.repo.Workflows[id]
		w.FileName = &bad
		xe.repo.Workflows[id] = w
		f, err := xe.svc.Download(dev, id)
		if err == nil {
			data, _ := io.ReadAll(f.Reader)
			f.Reader.Close()
			t.Errorf("被篡改的文件名 %q 不应读到内容: %q", bad, data)
		}
	}
}

// ---- 批量导出 ----

func TestExportBatchZipPerInstance(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	r, err := xe.svc.PreviewExport(ctx, dev, []uint{11, 12, 13}, true, "ShopDB", "select id from dbo.Orders where id > 0 order by id")
	if err != nil || r.ErrorCount != 0 || len(r.Rows) != 3 || len(r.Instances) != 3 || r.Rows[1].InstanceName != "mssql-12" || !strings.Contains(r.Rows[1].ErrorMessage, "实例[mssql-12]待导出 2 行") {
		t.Fatalf("批量检查: %+v %v", r, err)
	}
	xe.hosts["ms13"].count = 20000
	r, _ = xe.svc.PreviewExport(ctx, dev, []uint{11, 12, 13}, true, "ShopDB", "select id from dbo.Orders where id > 0")
	if r.ErrorCount != 1 || r.Rows[2].ErrLevel != review.LevelError || !strings.Contains(r.Rows[2].ErrorMessage, "实例[mssql-13]导出数据行数(20000)超过阈值(10000)") || r.Instances[2].OK {
		t.Errorf("任一实例超限都不通过: %+v", r.Rows)
	}
	xe.hosts["ms13"].count = 2
	xe.hosts["ms12"].noConn = true
	r, _ = xe.svc.PreviewExport(ctx, dev, []uint{11, 12, 13}, true, "ShopDB", "select id from dbo.Orders where id > 0")
	if r.ErrorCount != 1 || !strings.Contains(r.Rows[1].ErrorMessage, "实例[mssql-12]检查失败") {
		t.Errorf("连不上: %+v", r.Rows)
	}
	xe.hosts["ms12"].noConn = false

	id := xe.submit(t, xe.req("select id, name from dbo.Orders where id > 0", "csv", 11, 12, 13))
	if w := xe.repo.Workflows[id]; w.BatchInstanceIDs == nil || *w.BatchInstanceIDs != "11,12,13" || w.IsOfflineExport != 1 {
		t.Errorf("批量导出工单: %+v", w)
	}
	xe.run(t, id)
	if xe.status(id) != model.WfFinish {
		t.Fatalf("状态: %s %s", xe.status(id), xe.repo.Contents[id].ExecuteResult)
	}
	name := fileOf(xe, id)
	if !strings.HasPrefix(name, fmt.Sprintf("%d_20261005100000_", id)) {
		t.Errorf("压缩包名: %s", name)
	}
	entries := xe.zipEntries(t, name)
	var names []string
	for fn := range entries {
		names = append(names, fn)
	}
	sort.Strings(names)
	want := []string{fmt.Sprintf("%d/mssql-11.csv", id), fmt.Sprintf("%d/mssql-12.csv", id), fmt.Sprintf("%d/mssql-13.csv", id)}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("包内应是 <工单号>/<实例名>.csv: %v", names)
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if len(rows) != 4 || !rows[3].IsSummary || !strings.Contains(rows[3].ErrorMessage, "批量导出汇总：共 3 个实例，成功 3 个，失败 0 个，已关闭 0 个。保存文件: "+name) || rows[0].InstanceID != 11 || rows[0].AffectedRows != 2 {
		t.Errorf("批量结果: %+v", rows)
	}
	last := xe.cap.msgs[len(xe.cap.msgs)-1]
	if !strings.Contains(last.Content, "mssql-11...等 3 个实例（批量上线）") {
		t.Logf("通知实例名: %s", last.Content) // 导出同样按“首个实例名…等 N 个实例”显示
	}
}

func TestExportBatchPartialFailureRetryMergesIntoZip(t *testing.T) {
	xe := setupExport(t)
	id := xe.submit(t, xe.req("select id, name from dbo.Orders where id > 0", "csv", 11, 12, 13))
	xe.hosts["ms12"].failQ = "dbo.Orders where id > 0"
	xe.hosts["ms12"].count = 2
	xe.hosts["ms11"].rows = [][]any{{int64(11), "ms11-data"}}
	xe.hosts["ms13"].rows = [][]any{{int64(13), "ms13-data"}}
	xe.approve(t, id)
	// 提交时统计通过，执行时 ms12 的查询才失败（failQ 只对非统计语句：统计语句也含该文本，所以先让统计通过）
	xe.hosts["ms12"].failQ = ""
	xe.hosts["ms12"].mu.Lock()
	xe.hosts["ms12"].failQ = "id, name"
	xe.hosts["ms12"].mu.Unlock()
	if _, err := xe.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatal(err)
	}
	if xe.status(id) != model.WfException {
		t.Fatalf("部分失败 → 执行有异常: %s", xe.status(id))
	}
	oldName := fileOf(xe, id)
	entries := xe.zipEntries(t, oldName)
	if len(entries) != 2 {
		t.Fatalf("已成功实例的文件仍在包里: %v", entries)
	}
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	sum := rows[len(rows)-1]
	if !strings.Contains(sum.ErrorMessage, "共 3 个实例，成功 2 个，失败 1 个") || !strings.Contains(sum.ErrorMessage, "失败实例未包含在压缩包中") || sum.ErrLevel != review.LevelWarning {
		t.Errorf("汇总: %+v", sum)
	}
	d, _ := xe.svc.Get(dev, id)
	if !d.CanDownload || len(d.BatchInstances) != 3 || !d.BatchInstances[1].Failed || !d.CanBatchOp {
		t.Errorf("详情: %+v", d)
	}

	// 故障排除后重试：新文件合并进压缩包，旧包被删除，其余实例的文件还在
	xe.hosts["ms12"].failQ = ""
	xe.hosts["ms12"].rows = [][]any{{int64(12), "ms12-data"}}
	msg, status, err := xe.svc.RetryBatchInstance(context.Background(), dev, id, 12)
	if err != nil || status != model.WfFinish || !strings.Contains(msg, "重试成功") {
		t.Fatalf("重试: %q %q %v", msg, status, err)
	}
	newName := fileOf(xe, id)
	if newName == "" || newName == oldName || xe.store.Exists(oldName) || !xe.store.Exists(newName) {
		t.Errorf("新包替换旧包: old=%s new=%s", oldName, newName)
	}
	merged := xe.zipEntries(t, newName)
	if len(merged) != 3 || !strings.Contains(merged[fmt.Sprintf("%d/mssql-11.csv", id)], "ms11-data") || !strings.Contains(merged[fmt.Sprintf("%d/mssql-12.csv", id)], "ms12-data") ||
		!strings.Contains(merged[fmt.Sprintf("%d/mssql-13.csv", id)], "ms13-data") {
		t.Errorf("合并后的包: %v", merged)
	}
	rows, _ = review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	if n := 0; true {
		for _, r := range rows {
			if r.InstanceID == 12 {
				n++
			}
		}
		if n != 1 {
			t.Errorf("实例 12 的结果行应被替换: %d", n)
		}
	}
	if !strings.Contains(xe.repo.Logs[len(xe.repo.Logs)-1].OperationInfo, "重试结果") {
		t.Errorf("日志: %+v", xe.repo.Logs[len(xe.repo.Logs)-1])
	}
}

func TestExportBatchAllFailNoFile(t *testing.T) {
	xe := setupExport(t)
	id := xe.submit(t, xe.req("select id, name from dbo.Orders where id > 0", "csv", 11, 12))
	xe.approve(t, id)
	for _, h := range []string{"ms11", "ms12"} {
		xe.hosts[h].failQ = "id, name"
	}
	xe.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	if xe.status(id) != model.WfException || fileOf(xe, id) != "" {
		t.Errorf("全部失败不应生成文件: %s %q", xe.status(id), fileOf(xe, id))
	}
	if _, err := xe.svc.Download(dev, id); code(err) != "not_found" {
		t.Errorf("没有文件可下载: %v", err)
	}
	d, _ := xe.svc.Get(dev, id)
	if d.CanDownload {
		t.Error("没有文件不应显示下载")
	}
}

func TestExportBatchInstanceDeleted(t *testing.T) {
	xe := setupExport(t)
	id := xe.submit(t, xe.req("select id, name from dbo.Orders where id > 0", "csv", 11, 12))
	xe.approve(t, id)
	var kept []model.Instance
	for _, in := range xe.repo.Instances {
		if in.ID != 12 {
			kept = append(kept, in)
		}
	}
	xe.repo.Instances = kept
	xe.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto})
	rows, _ := review.ParseRows(xe.repo.Contents[id].ExecuteResult)
	var found bool
	for _, r := range rows {
		found = found || (r.InstanceID == 12 && strings.Contains(r.ErrorMessage, "实例已不存在，跳过导出"))
	}
	if !found || xe.status(id) != model.WfException || len(xe.zipEntries(t, fileOf(xe, id))) != 1 {
		t.Errorf("被删除的实例必须明确记录，其余实例照常导出: %s %+v", xe.status(id), rows)
	}
}

func TestExportListFilterAndOptions(t *testing.T) {
	xe := setupExport(t)
	exp := xe.submit(t, xe.req(exportSQL, "xlsx", 1))
	chg, _ := xe.svc.Submit(context.Background(), dev, sqlflow.SubmitReq{Name: "改单", InstanceID: 1, DBName: "shop", GroupName: "研发组", SQL: "update t set a=1 where id=1"})
	ids := func(q sqlflow.ListQuery) []uint {
		rs, _, _ := xe.svc.List(dev, q)
		var out []uint
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return out
	}
	if got := ids(sqlflow.ListQuery{Kind: "export"}); len(got) != 1 || got[0] != exp {
		t.Errorf("只看导出: %v", got)
	}
	if got := ids(sqlflow.ListQuery{Kind: "change"}); len(got) != 1 || got[0] != chg.WorkflowID {
		t.Errorf("只看上线: %v", got)
	}
	rs, _, _ := xe.svc.List(dev, sqlflow.ListQuery{Kind: "export"})
	if !rs[0].IsExport || rs[0].ExportFormat != "xlsx" || rs[0].SyntaxLabel != "离线导出" {
		t.Errorf("列表行: %+v", rs[0])
	}

	o, err := xe.svc.SubmitOptions(dev)
	if err != nil || !o.Export.Allowed || !o.Export.BatchAllowed || len(o.Export.Formats) != 5 || o.Export.MaxRows != 10000 || o.Export.Seconds != 60 {
		t.Fatalf("导出选项: %+v %v", o.Export, err)
	}
	var exIDs []uint
	for _, in := range o.ExportInstances[1] {
		exIDs = append(exIDs, in.ID)
	}
	sort.Slice(exIDs, func(i, j int) bool { return exIDs[i] < exIDs[j] })
	if fmt.Sprint(exIDs) != "[1 11 12 13]" {
		t.Errorf("可导出的实例（can_read）: %v", exIDs)
	}
	if lo, _ := xe.svc.SubmitOptions(leader); lo.Export.Allowed || lo.Export.BatchAllowed {
		t.Error("没有权限不应开放导出")
	}
	// 页面权限：菜单权限任意一个即可
	xe.repo.Perms[8]["export.view"] = true
	if !xe.svc.CanAccessPage(stranger) {
		t.Error("export.view 也能打开列表")
	}
	if dbs, err := xe.svc.Databases(context.Background(), dev, 11); err != nil || len(dbs) != 2 {
		t.Errorf("导出用户（只有读标签的实例）也能查库: %v %v", dbs, err)
	}
}

// 带旧标签 read_view 的实例：导出与 can_read 实例一致，不再限制只能导出 v_ 前缀的视图。
func TestExportLegacyReadViewTagNotRestricted(t *testing.T) {
	xe := setupExport(t)
	ctx := context.Background()
	rv := model.InstanceTag{TagCode: "read_view", Active: true}
	for i := range xe.repo.Instances {
		if id := xe.repo.Instances[i].ID; id == 1 || id == 11 || id == 12 {
			xe.repo.Instances[i].Tags = append(xe.repo.Instances[i].Tags, rv)
		}
	}
	xe.rebuild()

	// 普通表也能导出：统计行数、提交进入审批、审批后执行成功
	r, err := xe.svc.PreviewExport(ctx, dev, []uint{1}, false, "shop", "select id, name from orders where id > 0")
	if err != nil || r.ErrorCount != 0 || r.Rows[0].StageStatus != "行数统计完成" {
		t.Fatalf("普通表应通过检查: %+v %v", r, err)
	}
	if n := len(xe.hosts["my1"].queries); n != 1 {
		t.Errorf("应去实例上统计行数: %d", n)
	}
	id := xe.submit(t, xe.req("select id, name from orders where id > 0", "csv", 1))
	if w := xe.repo.Workflows[id]; w.Status != model.WfManReviewing {
		t.Errorf("应进入人工审批: %s", w.Status)
	}
	xe.run(t, id)
	if xe.status(id) != model.WfFinish || fileOf(xe, id) == "" {
		t.Errorf("审批通过后应导出成功: %s %s", xe.status(id), xe.repo.Contents[id].ExecuteResult)
	}

	// 批量：带旧标签的 SQL Server 实例引用非视图也能通过
	r, _ = xe.svc.PreviewExport(ctx, dev, []uint{11, 12, 13}, true, "ShopDB", "select id from dbo.Orders where id > 0")
	if r.ErrorCount != 0 {
		t.Errorf("批量检查: %+v", r.Rows)
	}
	// T-SQL 批处理照常放行（不再被禁用）
	r, _ = xe.svc.PreviewExport(ctx, dev, []uint{11}, false, "ShopDB", "declare @d date = '2026-01-01';\nselect * from dbo.v_Orders where d = @d")
	if r.ErrorCount != 0 {
		t.Errorf("批处理应放行: %+v", r.Rows)
	}
}
