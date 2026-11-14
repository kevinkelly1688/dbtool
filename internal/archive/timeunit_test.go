// @author: kevinkelly
// @license: Apache Licence
// @file: timeunit_test.go
// @time: 2026/10/28

package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"dbtool/internal/archive/engine"
	"dbtool/internal/archive/engine/enginetest"
)

// swapToEpochSource 把环境里的源表换成“时间列是 bigint 毫秒时间戳”的 shop.orders（created_at 列改成 bigint(20)，有索引）。
func swapToEpochSource(e *env, rows int, base int64) {
	cols := enginetest.DefaultColumns()
	cols[1].Type = "bigint(20)"
	e.h.Src = enginetest.NewMemSource(e.h.Log, e.h.Clock, "shop", "orders", cols, []string{"id"})
	var seed []engine.Row
	for i := 1; i <= rows; i++ {
		seed = append(seed, engine.Row{int64(i), base + int64(i), int64(3 + i%2), "p"})
	}
	e.h.Src.Seed(seed...)
	e.h.Src.Info.Indexes["idx_created"] = []string{"created_at"}
	e.h.Src.ExplainRes = engine.ExplainResult{UsedIndex: "idx_created", ScanRows: 100, Type: "range"}
	e.h.Src.ProbeRes = engine.TimeProbe{Sampled: rows, InMs: rows, Sample: base + 1, HasRange: true, Min: base + 1, Max: base + int64(rows),
		OffsetSec: 8 * 3600, GuessUnit: engine.UnitEpochMs}
}

// 整型毫秒时间戳的任务：试运行、提交、审批、运行、删除，全流程。
func TestEpochMsTaskLifecycle(t *testing.T) {
	e := newEnv(t)
	swapToEpochSource(e, 300, 1_600_000_000_000)
	in := e.baseInput("毫秒时间戳表")
	in.TimeUnit = "epoch_ms"
	in = e.checked(e.rd, in)
	r, err := e.svc.CreateTask(e.rd, in)
	if err != nil {
		t.Fatalf("创建：%v", err)
	}
	id := r.Task.ID
	if r.Task.TimeUnit != engine.UnitEpochMs || !strings.Contains(r.Task.Where, "UNIX_TIMESTAMP()") || !strings.Contains(r.Task.Where, "* 1000") {
		t.Errorf("视图应带时间单位和毫秒条件：unit=%q where=%q", r.Task.TimeUnit, r.Task.Where)
	}
	if _, err := e.svc.Approve(e.tl, id, "同意"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Approve(e.dba, id, "同意"); err != nil {
		t.Fatal(err)
	}
	if saved, _ := e.repo.GetTask(id, false); saved.TimeUnit != engine.UnitEpochMs || saved.Status != StEnabled {
		t.Fatalf("入库的任务：unit=%q status=%s", saved.TimeUnit, saved.Status)
	}
	if _, err := e.svc.RunNow(e.dba, id, true, true); err != nil {
		t.Fatal(err)
	}
	e.wait()
	var res *Run
	for _, run := range e.repo.allRuns() {
		if run.TaskID == id && run.Trigger == "manual" {
			r := run
			res = &r
		}
	}
	if res == nil || res.Result != engine.ResultSuccess || res.Deleted != 300 {
		t.Fatalf("运行结果：%+v", res)
	}
	want := "`created_at` > 0 AND `created_at` < (UNIX_TIMESTAMP() - 7776000) * 1000 AND (`status` IN (3, 4))"
	if len(e.h.Src.Wheres) == 0 || e.h.Src.Wheres[0] != want {
		t.Errorf("读取条件：%v", e.h.Src.Wheres)
	}
}

// 单位选错（10 位秒的数据选了毫秒）：试运行就是红色，不能提交。
func TestEpochUnitMismatchBlocksCheck(t *testing.T) {
	e := newEnv(t)
	swapToEpochSource(e, 100, 1_700_000_000_000)
	e.h.Src.ProbeRes = engine.TimeProbe{Sampled: 100, InSec: 100, Sample: 1700000001, GuessUnit: engine.UnitEpochSec}
	in := e.baseInput("单位选错")
	in.TimeUnit = "epoch_ms"
	resp, err := e.svc.Check(context.Background(), e.rd, in)
	if err != nil || !resp.Blocked {
		t.Fatalf("应被阻断：%v %+v", err, resp)
	}
	var got bool
	for _, it := range resp.Report.Items {
		if it.Key == "time_unit" && it.Level == "error" {
			got = true
		}
	}
	if !got {
		t.Errorf("应有时间单位红色项：%+v", resp.Report.Items)
	}
}

func TestTimeUnitValidationAndNormalize(t *testing.T) {
	e := newEnv(t)
	in := e.baseInput("单位校验")
	in.TimeUnit = "ms"
	_, err := e.svc.CreateTask(e.rd, in)
	pe := mustErrCode(t, err, 400, "validation")
	if !strings.Contains(pe.Message, "时间单位无效") {
		t.Errorf("提示：%s", pe.Message)
	}
	// 大小写、显式 datetime 都规整成标准形式
	in2 := e.baseInput("单位规整")
	in2.TimeUnit = " DateTime "
	in2.normalize(defaultPolicy())
	if in2.TimeUnit != "" {
		t.Errorf("datetime 应规整成空串：%q", in2.TimeUnit)
	}
	in2.TimeUnit = " EPOCH_MS "
	in2.normalize(defaultPolicy())
	if in2.TimeUnit != "epoch_ms" {
		t.Errorf("应规整成 epoch_ms：%q", in2.TimeUnit)
	}
	// 不认识的值原样保留，让校验报错而不是悄悄变成 datetime
	in2.TimeUnit = "ms"
	in2.normalize(defaultPolicy())
	if in2.TimeUnit != "ms" {
		t.Errorf("不认识的单位不应被改写：%q", in2.TimeUnit)
	}
}

// 升级前已批准的任务（没有时间单位）的关键字段哈希必须保持不变，否则升级后它们会被误判为“改过关键字段”。
func TestKeyHashBackwardCompatible(t *testing.T) {
	e := newEnv(t)
	in := e.baseInput("哈希兼容")
	in.normalize(defaultPolicy())
	type oldKey struct { // 升级前 hashKey 里的结构（没有 Unit）
		Inst    uint
		RG      uint
		DB, T   string
		TimeCol string
		Days    int
		Extra   string
		Mode    string
		Reason  string
		Dest    *engine.TableDest
		File    *engine.FileDest
		MaxRows int64
	}
	k := oldKey{Inst: in.InstanceID, RG: in.ResourceGroupID, DB: in.DB, T: in.Table, TimeCol: in.TimeColumn, Days: in.RetentionDays, Extra: in.ExtraWhere,
		Mode: in.Mode, Reason: in.PurgeReason, Dest: in.DestTable, File: in.DestFile, MaxRows: in.Params.MaxRows}
	sum := sha256.Sum256([]byte(mustJSON(k)))
	if want := hex.EncodeToString(sum[:16]); in.keyHash() != want {
		t.Errorf("没有时间单位时哈希应与升级前一致：%s ≠ %s", in.keyHash(), want)
	}
	withUnit := in
	withUnit.TimeUnit = "epoch_ms"
	sec := in
	sec.TimeUnit = "epoch_s"
	if withUnit.keyHash() == in.keyHash() || sec.keyHash() == withUnit.keyHash() || sec.keyHash() == in.keyHash() {
		t.Error("不同时间单位的哈希应不同")
	}
}

// 时间单位是关键字段：已启用的任务改它必须重新审批、列入变更说明、清掉旧断点。
func TestTimeUnitIsKeyField(t *testing.T) {
	e := newEnv(t)
	task := e.enabled("单位是关键字段")
	cur, _ := e.repo.GetTask(task.ID, false)
	old := inputOf(cur)
	n := old
	n.TimeUnit = "epoch_ms"
	if need, reason := needsApproval(cur, old, n); !need || !strings.Contains(reason, "关键字段") {
		t.Errorf("改时间单位应需要审批：%v %q", need, reason)
	}
	if !keyChanged(old, n) {
		t.Error("改时间单位应使旧断点失效")
	}
	if d := describeChanges(old, n); len(d) == 0 || !strings.Contains(strings.Join(d, ";"), "关键字段") {
		t.Errorf("变更说明：%v", d)
	}
	if need, _ := needsApproval(cur, old, old); need {
		t.Error("没改不需要审批")
	}
}

func TestSourceTimeProbe(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, err := e.svc.SourceTimeProbe(ctx, e.rd, 1, "shop", "orders", "created_ms")
	if err != nil || p == nil || p.GuessUnit != engine.UnitEpochMs || !p.HasRange || p.OffsetSec != 8*3600 {
		t.Fatalf("探测：%v %+v", err, p)
	}
	// 只允许整型列
	for col, want := range map[string]string{"note": "不是 int / bigint", "created_at": "不是 int / bigint", "nope": "没有列", "bad`col": "不合法", "": "不合法"} {
		_, err := e.svc.SourceTimeProbe(ctx, e.rd, 1, "shop", "orders", col)
		pe := mustErrCode(t, err, 400, "bad_request")
		if !strings.Contains(pe.Message, want) {
			t.Errorf("列 %q：%s", col, pe.Message)
		}
	}
	// 受保护库、表不存在、无权限
	_, err = e.svc.SourceTimeProbe(ctx, e.rd, 1, "mysql", "user", "id")
	mustErrCode(t, err, 403, "protected")
	_, err = e.svc.SourceTimeProbe(ctx, e.rd, 1, "shop", "nope", "id")
	mustErrCode(t, err, 404, "not_found")
	_, err = e.svc.SourceTimeProbe(ctx, caller(20, "nobody", false), 1, "shop", "orders", "created_ms")
	mustErrCode(t, err, 403, "forbidden")
}

func TestCatalogColumnsMarkIntegerCandidates(t *testing.T) {
	for typ, want := range map[string]bool{"bigint(20)": true, "bigint(20) unsigned": true, "int(11)": true, "int unsigned": true, "tinyint(1)": false, "varchar(20)": false, "datetime": false, "decimal(13,0)": false} {
		if got := engine.IsIntegerTimeType(typ); got != want {
			t.Errorf("%s: %v", typ, got)
		}
	}
}
