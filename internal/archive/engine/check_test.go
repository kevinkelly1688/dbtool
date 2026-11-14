// @author: kevinkelly
// @license: Apache Licence
// @file: check_test.go
// @time: 2026/10/28

package engine_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/archive/engine/enginetest"
)

func item(rep *engine.CheckReport, key string) *engine.CheckItem {
	for i := range rep.Items {
		if rep.Items[i].Key == key {
			return &rep.Items[i]
		}
	}
	return nil
}

func items(rep *engine.CheckReport, key string) []engine.CheckItem {
	var out []engine.CheckItem
	for _, it := range rep.Items {
		if it.Key == key {
			out = append(out, it)
		}
	}
	return out
}

func wantLevel(t *testing.T, rep *engine.CheckReport, key, level, detailHas string) {
	t.Helper()
	for _, it := range items(rep, key) {
		if it.Level == level && strings.Contains(it.Detail, detailHas) {
			return
		}
	}
	t.Errorf("应有 %s/%s 项且详情含 %q，实际：%+v", key, level, detailHas, items(rep, key))
}

func runCheck(t *testing.T, h *enginetest.Harness, spec engine.Spec) *engine.CheckReport {
	t.Helper()
	rep, err := newEng().Check(t.Context(), spec, h.Policy(), h.EnvFor(spec, t.TempDir()))
	if err != nil {
		t.Fatalf("Check：%v", err)
	}
	return rep
}

func tableOfMany(rows int) *enginetest.Harness {
	h := enginetest.New(enginetest.Options{Rows: 100})
	h.Src.RowsOverride = int64(rows)
	h.Src.CountN = int64(rows) / 10
	h.Src.Info.Indexes["idx_created"] = []string{"created_at"}
	h.Src.ExplainRes = engine.ExplainResult{UsedIndex: "idx_created", ScanRows: 1000, Type: "range"}
	return h
}

func TestCheckHappyPathAndReadOnly(t *testing.T) {
	h := tableOfMany(1_000_000)
	h.Src.Info.DataMB = 200
	spec := h.Spec()
	spec.Params = engine.Params{BatchSize: 1000, SleepMs: 200}
	rep := runCheck(t, h, spec)
	if rep.HasError() {
		t.Fatalf("不应有红色项：%+v", rep.Items)
	}
	for _, k := range []string{"protected", "retention", "extra", "pk", "time_column", "index", "explain", "fk", "trigger", "partition", "blob", "large_batch", "max_rows", "dest_table", "load", "replica"} {
		if item(rep, k) == nil {
			t.Errorf("缺少检查项 %s", k)
		}
	}
	if rep.EstRows != 100_000 || rep.Capped || rep.TableRows != 1_000_000 || rep.Percent < 9.99 || rep.Percent > 10.01 {
		t.Errorf("命中 %d capped=%v 表 %d 占比 %.2f", rep.EstRows, rep.Capped, rep.TableRows, rep.Percent)
	}
	if rep.EstBatches != 100 || rep.EstSeconds < 40 || rep.EstSeconds > 80 || rep.EstBinlogMB <= 0 {
		t.Errorf("估算：批数 %d 耗时 %ds binlog %dMB", rep.EstBatches, rep.EstSeconds, rep.EstBinlogMB)
	}
	if rep.Fingerprint != h.Src.Info.Fingerprint || len(rep.PKCols) != 1 || rep.PKCols[0] != "id" {
		t.Errorf("指纹 / 主键：%s %v", rep.Fingerprint, rep.PKCols)
	}
	if len(rep.Sample) != 10 || len(rep.SampleCols) != 4 {
		t.Errorf("样例：%d 行 %v", len(rep.Sample), rep.SampleCols)
	}
	if !strings.Contains(rep.SQL.Select, "INTERVAL 90 DAY") || !strings.Contains(rep.SQL.Insert, "`shop_arch`.`orders_bak`") || !strings.Contains(rep.SQL.Delete, "DELETE FROM `shop`.`orders`") {
		t.Errorf("SQL 预览：%+v", rep.SQL)
	}
	// 只读：不写不删（只会真读一次首批来计时，且只取少量行）
	if h.Src.Reads != 1 || !h.Src.ReadReqs[0].Unordered || h.Src.ReadReqs[0].Limit > 200 {
		t.Errorf("试运行应只实测一次首批读取：%d %+v", h.Src.Reads, h.Src.ReadReqs)
	}
	wantLevel(t, rep, "first_read", "ok", "首批读取")
	if h.Src.Deletes != 0 || h.Src.Inserts != 0 || h.Dst.Inserts != 0 || h.Dst.Ensures != 0 || h.Files.Puts != 0 || h.Dst.Exists("shop_arch", "orders_bak") {
		t.Errorf("试运行必须只读")
	}
	if h.Src.Len() != 100 {
		t.Errorf("源表被动了")
	}
	wantLevel(t, rep, "dest_table", "ok", "不存在，运行时自动按源表结构创建")
	wantLevel(t, rep, "large_batch", "ok", "占表约 10.0%")
}

func TestCheckPKItem(t *testing.T) {
	h := tableOfMany(1000)
	h.Src.Info.PrimaryKey = nil
	delete(h.Src.Info.Indexes, "PRIMARY")
	rep := runCheck(t, h, h.Spec())
	wantLevel(t, rep, "pk", "error", "没有主键")
	if !rep.HasError() {
		t.Errorf("HasError")
	}
	// 用唯一键代替主键
	h2 := tableOfMany(1000)
	delete(h2.Src.Info.Indexes, "PRIMARY")
	rep = runCheck(t, h2, h2.Spec())
	wantLevel(t, rep, "pk", "ok", "唯一非空键")
}

func TestCheckTableProblems(t *testing.T) {
	h := tableOfMany(1000)
	h.Src.Info.Exists = false
	rep := runCheck(t, h, h.Spec())
	wantLevel(t, rep, "table", "error", "不存在")
	if rep.EstRows != 0 || rep.Fingerprint != "" {
		t.Errorf("表不存在时不应继续检查")
	}
	h = tableOfMany(1000)
	h.Src.Info.IsView = true
	wantLevel(t, runCheck(t, h, h.Spec()), "table", "error", "视图")

	h = tableOfMany(1000)
	h.Src.Info.FKParents = []string{"shop.refunds", "shop.items"}
	wantLevel(t, runCheck(t, h, h.Spec()), "fk", "error", "shop.refunds")

	h = tableOfMany(1000)
	h.Src.Info.HasTriggers, h.Src.Info.Partitioned, h.Src.Info.Engine = true, true, "MyISAM"
	rep = runCheck(t, h, h.Spec())
	wantLevel(t, rep, "trigger", "warn", "触发器")
	wantLevel(t, rep, "partition", "warn", "分区维护")
	wantLevel(t, rep, "engine", "warn", "MyISAM")
	if rep.HasError() {
		t.Errorf("这些只是警告：%+v", rep.Items)
	}

	h = tableOfMany(1000)
	h.Src.Info.Columns = append(h.Src.Info.Columns, engine.ColumnInfo{Name: "body", Type: "longtext"})
	spec := h.Spec()
	spec.Params.BatchSize = 1000
	wantLevel(t, runCheck(t, h, spec), "blob", "warn", "body")
	spec.Params.BatchSize = 300
	wantLevel(t, runCheck(t, h, spec), "blob", "ok", "body")
}

func TestCheckTimeColumnAndIndex(t *testing.T) {
	h := tableOfMany(1000)
	spec := h.Spec()
	spec.TimeColumn = "nope"
	wantLevel(t, runCheck(t, h, spec), "time_column", "error", "没有列 nope")
	spec.TimeColumn = "status"
	rep := runCheck(t, h, spec)
	wantLevel(t, rep, "time_column", "error", "必须是 date")
	if rep.EstRows != 0 {
		t.Errorf("时间列非法时不应统计行数")
	}
	// 没有索引：大表红色，小表黄色
	big := tableOfMany(5_000_000)
	delete(big.Src.Info.Indexes, "idx_created")
	wantLevel(t, runCheck(t, big, big.Spec()), "index", "error", "没有索引")
	small := tableOfMany(5000)
	delete(small.Src.Info.Indexes, "idx_created")
	wantLevel(t, runCheck(t, small, small.Spec()), "index", "warn", "表较小")
	// 复合索引的最左列也算
	h2 := tableOfMany(1_000_000)
	delete(h2.Src.Info.Indexes, "idx_created")
	h2.Src.Info.Indexes["idx_c_s"] = []string{"created_at", "status"}
	wantLevel(t, runCheck(t, h2, h2.Spec()), "index", "ok", "idx_c_s")
	h3 := tableOfMany(1_000_000)
	delete(h3.Src.Info.Indexes, "idx_created")
	h3.Src.Info.Indexes["idx_s_c"] = []string{"status", "created_at"}
	wantLevel(t, runCheck(t, h3, h3.Spec()), "index", "error", "没有索引")
}

func TestCheckExplain(t *testing.T) {
	h := tableOfMany(1_000_000)
	h.Src.ExplainRes = engine.ExplainResult{Type: "ALL", ScanRows: 900_000}
	wantLevel(t, runCheck(t, h, h.Spec()), "explain", "error", "未使用索引")
	h.Src.ExplainRes = engine.ExplainResult{Type: "ALL", ScanRows: 500}
	wantLevel(t, runCheck(t, h, h.Spec()), "explain", "warn", "可以接受")
	h.Src.ExplainRes = engine.ExplainResult{UsedIndex: "idx_created", Type: "range", ScanRows: 800_000, Extra: "Using index condition; Using filesort"}
	wantLevel(t, runCheck(t, h, h.Spec()), "explain", "warn", "filesort")
	h.Src.ExplainRes = engine.ExplainResult{UsedIndex: "PRIMARY", Type: "index", ScanRows: 800_000}
	// 预计扫描很多行，但实测首批读取很快（LIMIT 提前结束）：只是优化器的上界估计，不再标黄
	wantLevel(t, runCheck(t, h, h.Spec()), "explain", "ok", "上界估计")
	// 实测首批读取慢：估计与实测都不好，保持黄色
	h.Src.ReadCost = func(int) time.Duration { return 5 * time.Second }
	wantLevel(t, runCheck(t, h, h.Spec()), "explain", "warn", "超过阈值")
	h.Src.ReadCost = nil
	h.Src.ExplainRes = engine.ExplainResult{UsedIndex: "idx_created", Type: "range", ScanRows: 100}
	wantLevel(t, runCheck(t, h, h.Spec()), "explain", "ok", "未超过阈值")
	h.Src.ExplainErr = errors.New("syntax")
	wantLevel(t, runCheck(t, h, h.Spec()), "explain", "warn", "EXPLAIN 失败")
}

func TestCheckLargeBatchAndLimits(t *testing.T) {
	h := tableOfMany(1_000_000)
	h.Src.CountN = 400_000
	rep := runCheck(t, h, h.Spec())
	wantLevel(t, rep, "large_batch", "warn", "已确认大批量")
	if rep.HasError() {
		t.Errorf("大批量是需要确认的警告，不是阻塞项")
	}
	if rep.Percent != 40 {
		t.Errorf("Percent=%v", rep.Percent)
	}
	// 命中数超过单次上限
	spec := h.Spec()
	spec.Params.MaxRows = 150_000
	rep = runCheck(t, h, spec)
	wantLevel(t, rep, "max_rows", "warn", "3 次运行")
	if rep.EstBatches != 1500 {
		t.Errorf("估算批数应按单次上限截断：%d", rep.EstBatches)
	}
	// 统计到上限
	h.Src.CountN = 50_000_000
	h.Src.CountCapped = true
	rep = runCheck(t, h, h.Spec())
	if !rep.Capped || rep.EstRows != engine.CountCap {
		t.Errorf("capped：%v %d", rep.Capped, rep.EstRows)
	}
	wantLevel(t, rep, "large_batch", "warn", "统计到上限")
	// 没有命中
	h.Src.CountN = 0
	h.Src.CountCapped = false
	rep = runCheck(t, h, h.Spec())
	wantLevel(t, rep, "est_rows", "warn", "没有符合条件")
	if rep.EstBatches != 0 {
		t.Errorf("批数 %d", rep.EstBatches)
	}
	// 统计失败
	h.Src.CountErr = errors.New("timeout")
	rep = runCheck(t, h, h.Spec())
	wantLevel(t, rep, "est_rows", "error", "timeout")
	// 表行数是估算值，比命中数还小时，占比封顶 100%
	h2 := tableOfMany(10)
	h2.Src.CountN = 500
	rep = runCheck(t, h2, h2.Spec())
	if rep.Percent != 100 || rep.TableRows != 500 {
		t.Errorf("占比封顶：%v %d", rep.Percent, rep.TableRows)
	}
}

func TestCheckSpecIssuesBecomeItems(t *testing.T) {
	h := tableOfMany(1000)
	spec := h.Spec()
	spec.RetentionDays = 7
	spec.ExtraWhere = "x = 1; drop table t"
	spec.DB = "orders_db"
	rep := runCheck(t, h, spec)
	wantLevel(t, rep, "retention", "error", "最小保留天数 30")
	wantLevel(t, rep, "extra", "error", "分号")
	if !rep.HasError() {
		t.Errorf("HasError")
	}
	// 受保护库表
	h2 := tableOfMany(1000)
	spec = h2.Spec()
	spec.DB, spec.Table = "mysql", "user"
	rep = runCheck(t, h2, spec)
	wantLevel(t, rep, "protected", "error", "受保护")
	// 库名不合法：不连库
	spec = h2.Spec()
	spec.Table = "a`b"
	before := h2.Src.Reads
	rep = runCheck(t, h2, spec)
	if !rep.HasError() || before != h2.Src.Reads || rep.Fingerprint != "" {
		t.Errorf("表名非法应直接返回")
	}
	// 附加条件里的列不存在
	h3 := tableOfMany(1000)
	spec = h3.Spec()
	spec.ExtraWhere = "no_such_col = 1 AND status = 3"
	rep = runCheck(t, h3, spec)
	wantLevel(t, rep, "extra", "error", "no_such_col")
	if rep.EstRows != 0 {
		t.Errorf("条件有误时不应统计")
	}
	spec.ExtraWhere = "status IN (3,4) AND `Payload` LIKE 'p%'"
	rep = runCheck(t, h3, spec)
	if rep.HasError() {
		t.Errorf("列名大小写不敏感：%+v", rep.Items)
	}
	if !strings.Contains(rep.SQL.Select, "(`status` IN (3, 4) AND `Payload` LIKE 'p%')") {
		t.Errorf("预览里应是规范化后的条件：%s", rep.SQL.Select)
	}
}

func TestCheckDestination(t *testing.T) {
	// 目标库不存在
	h := tableOfMany(1000)
	h.Dst.Databases = map[string]bool{}
	wantLevel(t, runCheck(t, h, h.Spec()), "dest_table", "error", "目标库 shop_arch 不存在")
	// 目标表存在且兼容
	h = tableOfMany(1000)
	h.Dst.PreCreate("shop_arch", "orders_bak", h.Src.Info)
	wantLevel(t, runCheck(t, h, h.Spec()), "dest_table", "ok", "已存在且结构兼容")
	// 目标表存在但缺列
	h = tableOfMany(1000)
	like := h.Src.Info
	like.Columns = like.Columns[:2]
	h.Dst.PreCreate("shop_arch", "orders_bak", like)
	wantLevel(t, runCheck(t, h, h.Spec()), "dest_table", "error", "缺少列")
	// 目标表主键不同
	h = tableOfMany(1000)
	like = h.Src.Info
	like.PrimaryKey = []string{"status"}
	h.Dst.PreCreate("shop_arch", "orders_bak", like)
	wantLevel(t, runCheck(t, h, h.Spec()), "dest_table", "error", "主键")
	// 磁盘
	h = tableOfMany(1000)
	h.Dst.DiskPct = 2
	wantLevel(t, runCheck(t, h, h.Spec()), "dest_disk", "error", "低于阈值")
	h.Dst.DiskOK = false
	wantLevel(t, runCheck(t, h, h.Spec()), "dest_disk", "warn", "未检测")
	// 没有目标连接
	h = tableOfMany(1000)
	spec := h.Spec()
	env := h.EnvFor(spec, t.TempDir())
	env.Dst = nil
	rep, _ := newEng().Check(t.Context(), spec, h.Policy(), env)
	wantLevel(t, rep, "dest_table", "error", "没有目标库连接")
	// 模板在试运行时按当前时间展开
	h = tableOfMany(1000)
	spec = h.Spec()
	spec.Dest = &engine.TableDest{InstanceID: 1, DB: "shop_arch", Template: "{table}_{yyyy}"}
	wantLevel(t, runCheck(t, h, spec), "dest_table", "ok", "orders_2026")
}

func TestCheckFileDestination(t *testing.T) {
	h := tableOfMany(1_000_000)
	h.Src.Info.DataMB = 500
	spec := h.FileSpec("csv", true, true)
	rep := runCheck(t, h, spec)
	wantLevel(t, rep, "dest_store", "ok", "可用空间")
	if h.Files.Puts != 0 {
		t.Errorf("试运行不写业务对象")
	}
	h.Files.TestErr = errors.New("dial tcp: i/o timeout")
	wantLevel(t, runCheck(t, h, spec), "dest_store", "error", "i/o timeout")
	h.Files.TestErr = nil
	h.Files.FreeBytes = 1 << 20
	wantLevel(t, runCheck(t, h, spec), "dest_store", "error", "小于预计备份体积")
	h.Files.FreeOK = false
	wantLevel(t, runCheck(t, h, spec), "dest_store", "ok", "对象存储")
	// 要求加密但没有密钥
	env := h.EnvFor(spec, t.TempDir())
	env.FileKey = nil
	rep, _ = newEng().Check(t.Context(), spec, h.Policy(), env)
	wantLevel(t, rep, "dest_store", "error", "密钥")
	// 没有存储
	env = h.EnvFor(spec, t.TempDir())
	env.Files = nil
	rep, _ = newEng().Check(t.Context(), spec, h.Policy(), env)
	wantLevel(t, rep, "dest_store", "error", "不可用")
}

func TestCheckLoadAndReplica(t *testing.T) {
	h := tableOfMany(1000)
	h.Probe.Threads = func(int) (int, error) { return 80, nil }
	h.Probe.Lag = func(int) (int, bool, error) { return 2, true, nil }
	rep := runCheck(t, h, h.Spec())
	wantLevel(t, rep, "load", "warn", "Threads_running=80")
	wantLevel(t, rep, "replica", "ok", "检测到从库")
	// 关闭延迟检查但有从库 → 红色
	spec := h.Spec()
	spec.Params.MaxLagSeconds = -1
	wantLevel(t, runCheck(t, h, spec), "replica", "error", "不允许关闭")
	// 探针取不到
	h2 := tableOfMany(1000)
	h2.Probe.Threads = func(int) (int, error) { return 0, errors.New("x") }
	h2.Probe.Disk = func(int) (float64, bool, error) { return 0, false, nil }
	wantLevel(t, runCheck(t, h2, h2.Spec()), "load", "warn", "未检测")
	// 没有探针
	h3 := tableOfMany(1000)
	spec = h3.Spec()
	env := h3.EnvFor(spec, t.TempDir())
	env.Probe = nil
	rep, _ = newEng().Check(t.Context(), spec, h3.Policy(), env)
	wantLevel(t, rep, "load", "warn", "没有负载探针")
}

func TestCheckStructureAndPurge(t *testing.T) {
	h := tableOfMany(1000)
	spec := h.Spec()
	spec.Fingerprint = "old"
	wantLevel(t, runCheck(t, h, spec), "structure", "warn", "不一致")
	spec.Fingerprint = h.Src.Info.Fingerprint
	if item(runCheck(t, h, spec), "structure") != nil {
		t.Errorf("指纹一致时不应有 structure 项")
	}
	// purge
	h2 := tableOfMany(1000)
	spec = h2.Spec()
	spec.Mode, spec.Dest = engine.ModePurge, nil
	pol := h2.Policy()
	pol.AllowPurgeNoBackup = true
	rep, err := newEng().Check(t.Context(), spec, pol, h2.EnvFor(spec, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	wantLevel(t, rep, "purge", "warn", "无法恢复")
	if rep.SQL.Insert != "" || rep.SQL.Delete == "" {
		t.Errorf("purge 预览：%+v", rep.SQL)
	}
	// 策略不允许 → 红色
	spec2 := spec
	rep = runCheck(t, h2, spec2)
	wantLevel(t, rep, "mode", "error", "不允许")
	// copy：没有 binlog、没有删除语句
	h3 := tableOfMany(100000)
	spec = h3.Spec()
	spec.Mode = engine.ModeCopy
	rep = runCheck(t, h3, spec)
	if rep.EstBinlogMB != 0 || rep.SQL.Delete != "" {
		t.Errorf("仅备份：binlog=%d delete=%q", rep.EstBinlogMB, rep.SQL.Delete)
	}
}

func TestCheckErrors(t *testing.T) {
	if _, err := newEng().Check(t.Context(), enginetest.New(enginetest.Options{Rows: 1}).Spec(), engine.Policy{}, nil); err == nil {
		t.Errorf("nil Env")
	}
}
