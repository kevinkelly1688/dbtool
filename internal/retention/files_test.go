// @author: kevinkelly
// @license: Apache Licence
// @file: files_test.go
// @time: 2026/10/28

package retention

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/settings"
)

// ---- 导出文件 ----

func exportEnv(t *testing.T) (*fakeRepo, *Service, string) {
	t.Helper()
	f := newFake()
	enable(f)
	f.markerReady = true
	f.set(settings.RetentionDaysKey("export_files"), "30")
	s, _, _ := newTestService(t, f)
	dir := filepath.Join(s.exportBase, "downloads", "DataExportFile")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	return f, s, dir
}

func writeFile(t *testing.T, dir, name string, age time.Duration, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	mt := baseNow.Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestExportFilesPurgedRecordKept(t *testing.T) {
	f, s, dir := exportEnv(t)
	writeFile(t, dir, "old_1.zip", 60*24*time.Hour, "12345")
	writeFile(t, dir, "old_2.zip", 45*24*time.Hour, "1234567")
	writeFile(t, dir, "recent.zip", 5*24*time.Hour, "x")
	writeFile(t, dir, "notmine.txt", 400*24*time.Hour, "不是任何工单记录的文件，不能动")
	f.exports = []ExportFile{
		{WorkflowID: 1, FileName: "old_1.zip", RefTime: daysAgo(60)},
		{WorkflowID: 2, FileName: "old_2.zip", RefTime: daysAgo(45)},
		{WorkflowID: 3, FileName: "recent.zip", RefTime: daysAgo(5)},
		{WorkflowID: 4, FileName: "gone.zip", RefTime: daysAgo(90)}, // 文件早已被人工删除
	}
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Failed() {
		t.Fatalf("%+v", rr.Targets)
	}
	if exists(filepath.Join(dir, "old_1.zip")) || exists(filepath.Join(dir, "old_2.zip")) {
		t.Fatal("过期导出文件应被删除")
	}
	if !exists(filepath.Join(dir, "recent.zip")) || !exists(filepath.Join(dir, "notmine.txt")) {
		t.Fatal("未过期文件和不属于任何工单的文件不能删")
	}
	if len(f.purged) != 3 || f.purged[1].SizeBytes != 5 || !f.purged[1].Existed || f.purged[4].Existed {
		t.Fatalf("应为已清理的工单写标记（含早已不存在的）: %+v", f.purged)
	}
	if _, ok := f.purged[3]; ok {
		t.Fatal("未过期工单不能标记")
	}
	tr := rr.Targets[0]
	if tr.Key != TargetExportFiles || tr.Deleted != 2 || tr.Bytes != 12 {
		t.Fatalf("统计: %+v", tr)
	}
	if len(f.audits) != 1 {
		t.Fatalf("审计: %+v", f.audits)
	}
}

func TestExportFilesNewerMtimeKept(t *testing.T) {
	f, s, dir := exportEnv(t)
	// 工单早已结束，但文件被批量导出重试重写过（修改时间较新）→ 不删、不标记
	writeFile(t, dir, "batch.zip", 2*24*time.Hour, "data")
	f.exports = []ExportFile{{WorkflowID: 7, FileName: "batch.zip", RefTime: daysAgo(90)}}
	rr := s.RunOnce(context.Background(), "schedule")
	if !exists(filepath.Join(dir, "batch.zip")) || len(f.purged) != 0 {
		t.Fatalf("文件较新不能删: %+v", rr.Targets)
	}
	if !strings.Contains(rr.Targets[0].Note, "修改时间晚于截止时间") {
		t.Fatalf("应提示: %+v", rr.Targets[0])
	}
}

func TestExportFilesNeedsMarkerTable(t *testing.T) {
	f, s, dir := exportEnv(t)
	f.markerReady = false
	writeFile(t, dir, "old.zip", 60*24*time.Hour, "x")
	f.exports = []ExportFile{{WorkflowID: 1, FileName: "old.zip", RefTime: daysAgo(60)}}
	rr := s.RunOnce(context.Background(), "schedule")
	if !exists(filepath.Join(dir, "old.zip")) {
		t.Fatal("没有标记表时不能删除文件")
	}
	if len(rr.Targets) != 1 || rr.Targets[0].Status != StatusSkipped || !strings.Contains(rr.Targets[0].Note, "205_retention.sql") {
		t.Fatalf("%+v", rr.Targets)
	}
}

func TestExportFilesMarkFailureStops(t *testing.T) {
	f, s, dir := exportEnv(t)
	writeFile(t, dir, "a.zip", 60*24*time.Hour, "x")
	f.exports = []ExportFile{{WorkflowID: 1, FileName: "a.zip", RefTime: daysAgo(60)}}
	f.markErr = os.ErrPermission
	rr := s.RunOnce(context.Background(), "schedule")
	if !rr.Failed() || !strings.Contains(rr.Targets[0].Err, "写清理标记失败") {
		t.Fatalf("%+v", rr.Targets)
	}
}

func TestExportFilesRejectsPathTraversalAndNonRegular(t *testing.T) {
	f, s, dir := exportEnv(t)
	outside := filepath.Join(s.exportBase, "secret.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../../secret.txt", `..\secret.txt`, "sub/x.zip", ".hidden", ""} {
		f.exports = []ExportFile{{WorkflowID: 1, FileName: bad, RefTime: daysAgo(90)}}
		f.purged = map[uint]ExportPurge{}
		rr := s.RunOnce(context.Background(), "schedule")
		if bad == "" {
			continue // 空文件名在 SQL 里已被过滤；假实现不过滤，这里只要不 panic
		}
		if !rr.Failed() || !exists(outside) || len(f.purged) != 0 {
			t.Fatalf("被篡改的文件名 %q 必须被拒绝: %+v", bad, rr.Targets)
		}
	}
	// 目录同名：不是普通文件，拒绝
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o750); err != nil {
		t.Fatal(err)
	}
	f.exports = []ExportFile{{WorkflowID: 9, FileName: "adir", RefTime: daysAgo(90)}}
	rr := s.RunOnce(context.Background(), "schedule")
	if !rr.Failed() || !exists(filepath.Join(dir, "adir")) {
		t.Fatalf("目录不能被删: %+v", rr.Targets)
	}
}

func TestExportDirMissingOrRemoteStorageSkipped(t *testing.T) {
	f, s, dir := exportEnv(t)
	f.exports = []ExportFile{{WorkflowID: 1, FileName: "a.zip", RefTime: daysAgo(90)}}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Targets[0].Status != StatusSkipped || !strings.Contains(rr.Targets[0].Note, "不存在") {
		t.Fatalf("目录不存在应跳过（且不创建）: %+v", rr.Targets[0])
	}
	if exists(dir) {
		t.Fatal("清理不能凭空创建导出目录")
	}
	f.set(settings.KeyExportStorageType, "sftp")
	rr = s.RunOnce(context.Background(), "schedule")
	if rr.Targets[0].Status != StatusSkipped || !strings.Contains(rr.Targets[0].Note, "sftp") {
		t.Fatalf("远程存储应跳过: %+v", rr.Targets[0])
	}
}

func TestExportDirFollowsLocalPathConfig(t *testing.T) {
	f, s, _ := exportEnv(t)
	abs := t.TempDir()
	f.set(settings.KeyExportLocalPath, abs)
	if d, err := s.exportDir(); err != nil || d != abs {
		t.Fatalf("绝对路径原样使用: %q %v", d, err)
	}
	f.set(settings.KeyExportLocalPath, "custom/dir/")
	d, _ := s.exportDir()
	if d != filepath.Join(s.exportBase, "custom/dir/") {
		t.Fatalf("相对路径相对于 EXPORT_BASE_DIR: %q", d)
	}
	f.set(settings.KeyExportLocalPath, "")
	d, _ = s.exportDir()
	if d != filepath.Join(s.exportBase, "downloads/DataExportFile/") {
		t.Fatalf("缺省目录: %q", d)
	}
}

func TestStaleUploadTempFilesCleaned(t *testing.T) {
	_, s, dir := exportEnv(t)
	writeFile(t, dir, ".upload-old", 48*time.Hour, "partial")
	writeFile(t, dir, ".upload-fresh", time.Hour, "in-flight")
	writeFile(t, dir, "keep.zip", 48*time.Hour, "ok")
	rr := s.RunOnce(context.Background(), "schedule")
	if exists(filepath.Join(dir, ".upload-old")) {
		t.Fatal("超过 24 小时的 .upload-* 应清理")
	}
	if !exists(filepath.Join(dir, ".upload-fresh")) || !exists(filepath.Join(dir, "keep.zip")) {
		t.Fatal("正在写入的临时文件和普通文件不能动")
	}
	if rr.Targets[0].Deleted != 1 || !strings.Contains(rr.Targets[0].Note, ".upload-") {
		t.Fatalf("%+v", rr.Targets[0])
	}
}

func TestExportPreviewIsReadOnly(t *testing.T) {
	f, s, dir := exportEnv(t)
	writeFile(t, dir, "old.zip", 60*24*time.Hour, "abc")
	f.exports = []ExportFile{{WorkflowID: 1, FileName: "old.zip", RefTime: daysAgo(60)}}
	p := s.Preview(context.Background())
	var it PreviewItem
	for _, x := range p.Items {
		if x.Key == TargetExportFiles {
			it = x
		}
	}
	if it.Candidates != 1 || it.Bytes != 3 {
		t.Fatalf("预览: %+v", it)
	}
	if !exists(filepath.Join(dir, "old.zip")) || len(f.purged) != 0 {
		t.Fatal("预览不能删文件也不能写标记")
	}
}

// ---- 归档文件备份 ----

func manifestRun(id uint, store uint, age int, keys ...string) BackupRun {
	return BackupRun{ID: id, StoreID: store, RefTime: daysAgo(age), Keys: keys, Bytes: int64(len(keys)) * 100, TaskName: "t"}
}

func backupEnv(t *testing.T, retention int) (*fakeRepo, *Service, *fakeFS) {
	t.Helper()
	f := newFake()
	enable(f)
	f.set(settings.KeyRetentionArchiveBackupEnabled, "true")
	fs := newFakeFS("a/part-0001.csv.gz", "a/part-0002.csv.gz", "a/manifest.json", "b/part-0001.csv.gz", "other/keep.txt")
	f.stores = []BackupStore{{ID: 1, Name: "nas", RetentionDays: retention, Config: engine.StoreConfig{ID: 1, Type: "local", Path: "/x"}}}
	s, _, _ := newTestService(t, f)
	s.openStore = func(engine.StoreConfig) (engine.FileStore, error) { return fs, nil }
	return f, s, fs
}

func TestArchiveBackupExpiredObjectsDeletedByManifestOnly(t *testing.T) {
	f, s, fs := backupEnv(t, 90)
	f.runs = []BackupRun{
		manifestRun(10, 1, 120, "a/part-0001.csv.gz", "a/part-0002.csv.gz", "a/manifest.json"), // 过期
		manifestRun(11, 1, 30, "b/part-0001.csv.gz"),                                           // 未过期
	}
	rr := s.RunOnce(context.Background(), "schedule")
	if rr.Failed() {
		t.Fatalf("%+v", rr.Targets)
	}
	want := map[string]bool{"a/part-0001.csv.gz": true, "a/part-0002.csv.gz": true, "a/manifest.json": true}
	if len(fs.deleted) != 3 {
		t.Fatalf("只删清单里的 3 个对象: %v", fs.deleted)
	}
	for _, k := range fs.deleted {
		if !want[k] {
			t.Fatalf("删了清单之外的对象 %s", k)
		}
	}
	if !fs.objects["b/part-0001.csv.gz"] || !fs.objects["other/keep.txt"] {
		t.Fatal("未过期运行的备份与存储里其它文件必须保留")
	}
	if note, ok := f.marked[10]; !ok || !strings.HasPrefix(note, BackupPurgedPrefix) || !strings.Contains(note, "nas") {
		t.Fatalf("应在运行记录上标记“备份已过期清理”: %v", f.marked)
	}
	if _, ok := f.marked[11]; ok {
		t.Fatal("未过期运行不能标记")
	}
	if tr := rr.Targets[0]; tr.Key != TargetArchiveStore || tr.Deleted != 3 || tr.Bytes != 300 {
		t.Fatalf("%+v", tr)
	}
	// 再跑一轮：已标记的不再处理
	fs.deleted = nil
	s.RunOnce(context.Background(), "schedule")
	if len(fs.deleted) != 0 {
		t.Fatalf("已清理的运行不应重复处理: %v", fs.deleted)
	}
}

func TestArchiveBackupRetentionZeroNeverPurges(t *testing.T) {
	f, s, fs := backupEnv(t, 0) // RetentionDays=0：永久
	f.stores = nil              // 真实实现的 BackupStores 只返回 retention_days > 0 的存储
	f.runs = []BackupRun{manifestRun(10, 1, 4000, "a/part-0001.csv.gz")}
	rr := s.RunOnce(context.Background(), "schedule")
	if len(fs.deleted) != 0 || len(f.marked) != 0 {
		t.Fatalf("RetentionDays=0 永不清理: %v", fs.deleted)
	}
	if len(rr.Targets) != 1 || rr.Targets[0].Status != StatusOK {
		t.Fatalf("%+v", rr.Targets)
	}
	// 即便假实现把 0 天的存储也返回了，也要因为运行不属于任何参与存储而不删
	f.stores = []BackupStore{{ID: 2, Name: "other", RetentionDays: 30}}
	s.RunOnce(context.Background(), "schedule")
	if len(fs.deleted) != 0 {
		t.Fatalf("运行所属存储不参与时不能删: %v", fs.deleted)
	}
}

func TestArchiveBackupNeedsExplicitSwitch(t *testing.T) {
	f, s, fs := backupEnv(t, 30)
	f.set(settings.KeyRetentionArchiveBackupEnabled, "")
	f.runs = []BackupRun{manifestRun(10, 1, 500, "a/part-0001.csv.gz")}
	s.RunOnce(context.Background(), "schedule")
	if len(fs.deleted) != 0 {
		t.Fatal("retention_archive_backup_enable 未打开时不能删备份")
	}
}

func TestArchiveBackupSkipsUnresolvedStoreAndIllegalKeys(t *testing.T) {
	f, s, fs := backupEnv(t, 30)
	f.runs = []BackupRun{
		manifestRun(10, 0, 500, "a/part-0001.csv.gz"),                     // 旧记录，不知道是哪个存储
		manifestRun(11, 1, 500, "a/part-0002.csv.gz", "../../etc/passwd"), // 清单被篡改
		manifestRun(12, 1, 500, "/etc/passwd"),
		manifestRun(13, 1, 500), // 空清单
	}
	rr := s.RunOnce(context.Background(), "schedule")
	if len(fs.deleted) != 0 || len(f.marked) != 0 {
		t.Fatalf("无法确定存储 / 含非法路径的运行一个对象都不能删: %v %v", fs.deleted, f.marked)
	}
	note := rr.Targets[0].Note
	if !strings.Contains(note, "没有备份存储信息") || !strings.Contains(note, "非法路径") {
		t.Fatalf("应在汇总里说明: %q", note)
	}
}

func TestArchiveBackupDeleteErrorStopsAndDoesNotMark(t *testing.T) {
	f, s, fs := backupEnv(t, 30)
	fs.failKey = "a/part-0002.csv.gz"
	f.runs = []BackupRun{manifestRun(10, 1, 500, "a/part-0001.csv.gz", "a/part-0002.csv.gz"), manifestRun(11, 1, 500, "b/part-0001.csv.gz")}
	rr := s.RunOnce(context.Background(), "schedule")
	if !rr.Failed() || len(f.marked) != 0 {
		t.Fatalf("删除失败不能标记已清理: %+v %v", rr.Targets, f.marked)
	}
	if !fs.objects["b/part-0001.csv.gz"] {
		t.Fatal("出错后停止该目标，不继续处理后面的运行")
	}
	// 修好之后重跑：幂等
	fs.failKey = ""
	rr = s.RunOnce(context.Background(), "schedule")
	if rr.Failed() || len(f.marked) != 2 {
		t.Fatalf("修复后应完成: %+v %v", rr.Targets, f.marked)
	}
}

func TestArchiveBackupMarkFailureReported(t *testing.T) {
	f, s, _ := backupEnv(t, 30)
	f.markBkErr = os.ErrClosed
	f.runs = []BackupRun{manifestRun(10, 1, 500, "a/part-0001.csv.gz")}
	rr := s.RunOnce(context.Background(), "schedule")
	if !rr.Failed() || !strings.Contains(rr.Targets[0].Err, "写“备份已过期清理”标记失败") {
		t.Fatalf("%+v", rr.Targets)
	}
}

func TestArchiveBackupStoreOpenError(t *testing.T) {
	f, s, _ := backupEnv(t, 30)
	s.openStore = func(engine.StoreConfig) (engine.FileStore, error) { return nil, os.ErrNotExist }
	f.runs = []BackupRun{manifestRun(10, 1, 500, "a/part-0001.csv.gz")}
	rr := s.RunOnce(context.Background(), "schedule")
	if !rr.Failed() || len(f.marked) != 0 {
		t.Fatalf("%+v", rr.Targets)
	}
}

func TestArchiveBackupPerStoreRetention(t *testing.T) {
	f, s, fs := backupEnv(t, 30)
	f.stores = append(f.stores, BackupStore{ID: 2, Name: "cold", RetentionDays: 365, Config: engine.StoreConfig{ID: 2, Type: "local", Path: "/y"}})
	f.runs = []BackupRun{
		manifestRun(10, 1, 100, "a/part-0001.csv.gz"), // nas 保留 30 天 → 过期
		manifestRun(11, 2, 100, "b/part-0001.csv.gz"), // cold 保留 365 天 → 还没到
	}
	s.RunOnce(context.Background(), "schedule")
	if len(fs.deleted) != 1 || fs.deleted[0] != "a/part-0001.csv.gz" {
		t.Fatalf("应按各存储自己的保留天数判断: %v", fs.deleted)
	}
}

func TestArchiveBackupPreviewIsReadOnly(t *testing.T) {
	f, s, fs := backupEnv(t, 30)
	f.set(settings.KeyRetentionArchiveBackupEnabled, "") // 预览不要求打开开关
	opened := false
	s.openStore = func(engine.StoreConfig) (engine.FileStore, error) { opened = true; return fs, nil }
	f.runs = []BackupRun{manifestRun(10, 1, 500, "a/part-0001.csv.gz", "a/manifest.json")}
	p := s.Preview(context.Background())
	var it PreviewItem
	for _, x := range p.Items {
		if x.Key == TargetArchiveStore {
			it = x
		}
	}
	if it.Candidates != 2 || it.Bytes != 200 || it.Configured || !strings.Contains(it.Note, "未打开") {
		t.Fatalf("%+v", it)
	}
	if opened || len(fs.deleted) != 0 || len(f.marked) != 0 {
		t.Fatal("预览不能连存储、不能删、不能标记")
	}
}

func TestValidKey(t *testing.T) {
	good := []string{"a/b/part-0001.csv.gz", "x.json", "inst/db/tbl/2026/10/09/12/manifest.json"}
	bad := []string{"", "../x", "a/../../x", "/abs", `\abs`, `C:\x`, "a\x00b", "a/..\\b"}
	for _, k := range good {
		if !validKey(k) {
			t.Errorf("%q 应合法", k)
		}
	}
	for _, k := range bad {
		if validKey(k) {
			t.Errorf("%q 应非法", k)
		}
	}
}

func TestManifestKeysAndFileStoreOf(t *testing.T) {
	keys, size := manifestKeys(`{"files":[{"key":"a/1","size":10},{"key":"a/2","size":5}],"manifest_key":"a/manifest.json"}`)
	if len(keys) != 3 || keys[2] != "a/manifest.json" || size != 15 {
		t.Fatalf("%v %d", keys, size)
	}
	if k, _ := manifestKeys("not json"); k != nil {
		t.Fatal("解析失败不能产生待删对象")
	}
	if id := fileStoreOf(`[{"kind":"table","location":"d.t"},{"kind":"file","store_id":7}]`); id != 7 {
		t.Fatalf("store id: %d", id)
	}
	if fileStoreOf("") != 0 || fileStoreOf("garbage") != 0 || fileStoreOf(`[{"kind":"table"}]`) != 0 {
		t.Fatal("取不到存储时返回 0")
	}
}
