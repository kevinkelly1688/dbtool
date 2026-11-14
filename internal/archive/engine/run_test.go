// @author: kevinkelly
// @license: Apache Licence
// @file: run_test.go
// @time: 2026/10/28

package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/archive/engine/enginetest"
)

func newEng() *engine.Engine { return engine.New() }

func mustRun(t *testing.T, h *enginetest.Harness, spec engine.Spec, pol engine.Policy, ctl *engine.Control) *engine.RunResult {
	t.Helper()
	res, err := newEng().Run(t.Context(), spec, pol, h.EnvFor(spec, t.TempDir()), ctl, h.Sink)
	if err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	return res
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func checkOrder(t *testing.T, h *enginetest.Harness) {
	t.Helper()
	if err := h.Log.CheckBackupBeforeDelete(); err != nil {
		t.Errorf("违反“先备份校验、后删除”：%v", err)
	}
}

// ---- 基本流程 ----

func TestRunMoveToTable(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 1000})
	spec := h.Spec()
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Scanned != 1000 || res.Backed != 1000 || res.Deleted != 1000 || res.Batches != 10 || res.Err != "" || res.AutoPause {
		t.Fatalf("结果：%+v", res)
	}
	if h.Src.Len() != 0 {
		t.Errorf("源表应被清空，剩 %d", h.Src.Len())
	}
	if rows := h.Dst.Table("shop_arch", "orders_bak"); len(rows) != 1000 || rows[0][0] != int64(1) || rows[999][0] != int64(1000) {
		t.Errorf("备份表行数 %d", len(rows))
	}
	checkOrder(t, h)
	if res.Checkpoint != nil {
		t.Errorf("完整跑完后断点应清空，让下次从头开始：%v", res.Checkpoint)
	}
	if strings.Join(res.MinPK, ",") != "1" || strings.Join(res.MaxPK, ",") != "1000" || res.DestTable != "orders_bak" {
		t.Errorf("主键范围 %v~%v 备份表 %q", res.MinPK, res.MaxPK, res.DestTable)
	}
	// 读和删都带着同一个归档条件
	wantWhere := "`created_at` < NOW() - INTERVAL 90 DAY"
	for _, w := range h.Src.Wheres {
		if w != wantWhere {
			t.Errorf("读取条件 %q", w)
		}
	}
	for _, d := range h.Src.DeleteReqs {
		if d.Where != wantWhere || len(d.PKCols) != 1 || d.PKCols[0] != "id" {
			t.Errorf("删除请求必须重复归档条件：%+v", d)
		}
	}
	// 每批读 100，批间休眠 50ms（10 批，最后一批后不再读到数据；读到的行数不足时不再休眠）
	for _, r := range h.Src.ReadReqs {
		if r.Limit != 100 {
			t.Errorf("批大小 %d", r.Limit)
		}
	}
	if h.Dst.Ensures != 1 {
		t.Errorf("EnsureTable 次数 %d", h.Dst.Ensures)
	}
	if res.PeakThreads != 3 {
		t.Errorf("峰值 Threads_running %d", res.PeakThreads)
	}
	if len(h.Sink.Find("log", "运行开始")) != 1 || len(h.Sink.Find("checkpoint", "运行结束")) != 1 {
		t.Errorf("应有开始 / 结束事件")
	}
	if p := h.Sink.LastProgress(); p.Scanned != 1000 || p.Deleted != 1000 || p.Batches != 10 {
		t.Errorf("最终进度：%+v", p)
	}
}

func TestRunDestTemplateExpandedAtRunTime(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 100})
	spec := h.Spec()
	spec.Dest = &engine.TableDest{InstanceID: 1, DB: "shop_arch", Template: "{table}_{yyyy}"}
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.DestTable != "orders_2026" || !h.Dst.Exists("shop_arch", "orders_2026") {
		t.Fatalf("%+v exists=%v", res, h.Dst.Exists("shop_arch", "orders_2026"))
	}
}

func TestRunCopyKeepsSourceAndCheckpoint(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 250})
	spec := h.Spec()
	spec.Mode = engine.ModeCopy
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Scanned != 250 || res.Backed != 250 || res.Deleted != 0 {
		t.Fatalf("%+v", res)
	}
	if h.Src.Len() != 250 || h.Src.Deletes != 0 {
		t.Errorf("仅备份不能删源")
	}
	if len(res.Checkpoint) != 1 || res.Checkpoint[0] != "250" {
		t.Errorf("仅备份应保留断点：%v", res.Checkpoint)
	}
	// 再来一次从断点继续：没有新数据
	spec.Checkpoint = res.Checkpoint
	res2 := mustRun(t, h, spec, h.Policy(), nil)
	if res2.Result != engine.ResultSuccess || res2.Scanned != 0 {
		t.Errorf("%+v", res2)
	}
}

func TestRunPurge(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	spec := h.Spec()
	spec.Mode, spec.Dest = engine.ModePurge, nil
	pol := h.Policy()
	// 策略不允许 → 直接拒绝运行，什么都不碰
	if _, err := newEng().Run(t.Context(), spec, pol, h.EnvFor(spec, t.TempDir()), nil, h.Sink); err == nil {
		t.Fatalf("不允许不备份清理时应拒绝")
	}
	if h.Src.Reads != 0 || h.Src.Len() != 300 {
		t.Fatalf("被拒绝的运行不应碰数据")
	}
	pol.AllowPurgeNoBackup = true
	res := mustRun(t, h, spec, pol, nil)
	if res.Result != engine.ResultSuccess || res.Scanned != 300 || res.Backed != 0 || res.Deleted != 300 || h.Src.Len() != 0 {
		t.Fatalf("%+v", res)
	}
	if h.Dst.Inserts != 0 || h.Files.Puts != 0 {
		t.Errorf("purge 不写任何备份")
	}
	// purge 仍然限流：每批后都有休眠
	if n := len(h.Clock.Slept()); n < 2 {
		t.Errorf("purge 也应批间休眠：%d", n)
	}
}

func TestRunRejectsInvalidSpec(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 10})
	cases := map[string]func(*engine.Spec){
		"retention below policy": func(s *engine.Spec) { s.RetentionDays = 5 },
		"protected":              func(s *engine.Spec) { s.DB = "mysql" },
		"bad extra":              func(s *engine.Spec) { s.ExtraWhere = "a=1;drop table orders" },
		"sleep too low":          func(s *engine.Spec) { s.Params.SleepMs = 10 },
		"bad window":             func(s *engine.Spec) { s.Params.Window = engine.Window{Start: "01:00", End: "01:10"} },
		"no backup":              func(s *engine.Spec) { s.Dest = nil },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			spec := h.Spec()
			mut(&spec)
			res, err := newEng().Run(t.Context(), spec, h.Policy(), h.EnvFor(spec, t.TempDir()), nil, nil)
			var ve *engine.ValidationError
			if err == nil || res != nil || !errors.As(err, &ve) {
				t.Fatalf("应返回 ValidationError：%v %v", res, err)
			}
			if h.Src.Reads != 0 || h.Src.Deletes != 0 || h.Src.Len() != 10 {
				t.Fatalf("非法规格不应触碰数据")
			}
		})
	}
	spec := h.Spec()
	if _, err := newEng().Run(t.Context(), spec, h.Policy(), nil, nil, nil); err == nil {
		t.Errorf("nil Env")
	}
	env := h.Env()
	env.Dst = nil
	if _, err := newEng().Run(t.Context(), spec, h.Policy(), env, nil, nil); err == nil {
		t.Errorf("缺 Dst")
	}
	fs := h.FileSpec("csv", false, true)
	env = h.Env()
	env.FileKey = []byte("short")
	if _, err := newEng().Run(t.Context(), fs, h.Policy(), env, nil, nil); err == nil {
		t.Errorf("加密缺密钥")
	}
	env = h.Env()
	env.Files = nil
	if _, err := newEng().Run(t.Context(), fs, h.Policy(), env, nil, nil); err == nil {
		t.Errorf("缺 Files")
	}
}

// ---- 先备份后删除：失败路径下源行不能被删 ----

func TestRunDestWriteFailureNeverDeletes(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 1000})
	h.Dst.OnInsert = func(call int, rows []engine.Row) error { return errors.New("disk full") }
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultFailed || !res.AutoPause || res.Batches != 0 || res.Deleted != 0 || !strings.Contains(res.Err, "disk full") {
		t.Fatalf("%+v", res)
	}
	if h.Src.Deletes != 0 || h.Src.Len() != 1000 {
		t.Fatalf("目标写失败时源表一行都不能少")
	}
	if h.Src.Reads != 3 { // 连续失败 3 次后停止
		t.Errorf("读取次数 %d，应在连续 3 次失败后停止", h.Src.Reads)
	}
	if len(h.Sink.Find("warn", "批失败")) != 3 {
		t.Errorf("应有 3 条批失败事件")
	}
	if !strings.Contains(res.Err, "连续失败 3 批") {
		t.Errorf("Err：%s", res.Err)
	}
}

func TestRunTransientFailureRecovers(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 500})
	h.Dst.OnInsert = func(call int, rows []engine.Row) error {
		if call <= 2 {
			return errors.New("connection reset")
		}
		return nil
	}
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Deleted != 500 || h.Src.Len() != 0 {
		t.Fatalf("%+v", res)
	}
	if len(h.Sink.Find("warn", "批失败")) != 2 {
		t.Errorf("应记录 2 次失败")
	}
	// 失败的批只计一次：Backed / Scanned 不重复
	if res.Scanned != 500 || res.Backed != 500 || res.Batches != 5 {
		t.Errorf("失败重试不应重复计数：%+v", res)
	}
	checkOrder(t, h)
}

func TestRunConsecutiveFailuresResetBySuccess(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 600})
	// 每隔一次失败一次：永远不会连续 3 次失败
	h.Dst.OnInsert = func(call int, rows []engine.Row) error {
		if call%2 == 1 {
			return errors.New("flaky")
		}
		return nil
	}
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultSuccess || h.Src.Len() != 0 {
		t.Fatalf("成功会清零连续失败计数：%+v", res)
	}
}

func TestRunVerifyMismatchStopsWithoutDelete(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 500})
	h.Dst.VerifyAdjust = func(call int, found int64) int64 {
		if call == 2 {
			return found - 1
		}
		return found
	}
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultVerifyFailed || !res.AutoPause || res.Batches != 1 || res.Deleted != 100 {
		t.Fatalf("%+v", res)
	}
	if h.Src.Len() != 400 || h.Src.Deletes != 1 {
		t.Fatalf("第 2 批校验不一致，第 2 批不能被删：剩 %d，删除调用 %d", h.Src.Len(), h.Src.Deletes)
	}
	if !h.Src.Has("101") || !h.Src.Has("200") {
		t.Errorf("第 2 批的行必须还在")
	}
	checkOrder(t, h)
	if len(h.Sink.Find("verify", "")) == 0 {
		t.Errorf("应有校验失败事件")
	}
	if res.Checkpoint != nil || strings.Join(res.MinPK, ",") != "1" || strings.Join(res.MaxPK, ",") != "100" {
		t.Errorf("不排序读取没有断点；处理过的主键范围只含成功的那一批：断点 %v 范围 %v~%v", res.Checkpoint, res.MinPK, res.MaxPK)
	}
}

func TestRunExistingRowContentMismatch(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 200})
	spec := h.Spec()
	h.Dst.PreCreate("shop_arch", "orders_bak", h.Src.Info)
	// 备份表里已有 id=150 但内容不同
	rows := h.Src.Rows()
	bad := append(engine.Row(nil), rows[149]...)
	bad[3] = "TAMPERED"
	if _, _, err := h.Dst.Insert(t.Context(), "shop_arch", "orders_bak", []string{"id", "created_at", "status", "payload"}, []engine.Row{bad}, []string{"id"}); err != nil {
		t.Fatal(err)
	}
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultVerifyFailed || !res.AutoPause {
		t.Fatalf("同主键内容不一致 = 校验失败：%+v", res)
	}
	if h.Src.Len() != 100 || h.Src.Has("150") == false {
		t.Errorf("第 2 批（101-200）不能被删")
	}
	checkOrder(t, h)
}

func TestRunIdempotentRetryAfterDeleteFailure(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	h.Src.OnDelete = func(call int, req engine.DeleteReq) error {
		if call == 2 {
			return errors.New("lost connection")
		}
		return nil
	}
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultSuccess || h.Src.Len() != 0 {
		t.Fatalf("%+v", res)
	}
	// 第 2 批删除失败后重读同一批：备份端已存在且一致 → existed，备份表里每行只有一份
	if rows := h.Dst.Table("shop_arch", "orders_bak"); len(rows) != 300 {
		t.Errorf("备份行数 %d", len(rows))
	}
	if res.Backed != 300 || res.Deleted != 300 || res.Scanned != 300 {
		t.Errorf("计数不应重复：%+v", res)
	}
	checkOrder(t, h)
}

func TestRunDeleteAffectedMismatchWarns(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 200})
	h.Src.DeleteAffected = func(call int, actual int64) int64 {
		if call == 1 {
			return actual - 3
		}
		return actual
	}
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Deleted != 197 || res.Scanned != 200 {
		t.Fatalf("%+v", res)
	}
	if len(h.Sink.Find("warn", "删除行数 97")) != 1 {
		t.Errorf("应记警告事件：%+v", h.Sink.Find("warn", ""))
	}
}

func TestRunRetriesDeadlocks(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 200})
	h.Src.OnDelete = func(call int, req engine.DeleteReq) error {
		if call <= 2 {
			return &engine.RetryableError{Err: errors.New("Deadlock found when trying to get lock")}
		}
		return nil
	}
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultSuccess || h.Src.Len() != 0 || res.Batches != 2 {
		t.Fatalf("%+v", res)
	}
	if len(h.Sink.Find("warn", "死锁")) != 2 || len(h.Sink.Find("warn", "批失败")) != 0 {
		t.Errorf("死锁应在批内重试，不计批失败：%+v", h.Sink.Events())
	}
	// 指数退避：200ms、400ms
	slept := h.Clock.Slept()
	if len(slept) < 2 || slept[0] != ms(200) || slept[1] != ms(400) {
		t.Errorf("退避：%v", slept)
	}
}

func TestRunDeadlockRetriesExhausted(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	h.Src.OnDelete = func(call int, req engine.DeleteReq) error {
		return &engine.RetryableError{Err: errors.New("Lock wait timeout exceeded")}
	}
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultFailed || !res.AutoPause || h.Src.Len() != 300 {
		t.Fatalf("%+v", res)
	}
	if h.Src.Deletes != 12 { // 3 批 × （1 次 + 3 次重试）
		t.Errorf("删除尝试次数 %d，应为 12", h.Src.Deletes)
	}
}

// ---- 断点 / 窗口 / 上限 ----

// 仅备份不删数据，必须靠“主键 > 断点”翻页，保持原样。
func copyHarness(rows int) (*enginetest.Harness, engine.Spec) {
	h := enginetest.New(enginetest.Options{Rows: rows})
	spec := h.Spec()
	spec.Mode = engine.ModeCopy
	return h, spec
}

func TestRunCopyResumesFromCheckpoint(t *testing.T) {
	h, spec := copyHarness(1000)
	spec.Checkpoint = []string{"500"}
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Scanned != 500 || h.Src.Len() != 1000 {
		t.Fatalf("%+v 剩 %d", res, h.Src.Len())
	}
	if first := h.Src.ReadReqs[0]; len(first.After) != 1 || first.After[0] != "500" || first.Unordered {
		t.Errorf("仅备份首批应从断点读取、按主键排序：%+v", first)
	}
	if strings.Join(res.MinPK, ",") != "501" || strings.Join(res.MaxPK, ",") != "1000" || len(res.Checkpoint) != 1 || res.Checkpoint[0] != "1000" {
		t.Errorf("范围 %v~%v 断点 %v", res.MinPK, res.MaxPK, res.Checkpoint)
	}
}

// 归档并删除 / 仅清理不再使用断点：旧断点被忽略，已删除的行不会再被读到。
func TestRunMoveIgnoresOldCheckpointAndReadsUnordered(t *testing.T) {
	for _, mode := range []engine.Mode{engine.ModeMove, engine.ModePurge} {
		h := enginetest.New(enginetest.Options{Rows: 1000})
		spec := h.Spec()
		spec.Mode = mode
		if mode == engine.ModePurge {
			spec.Dest = nil
		}
		spec.Checkpoint = []string{"500"}
		pol := h.Policy()
		pol.AllowPurgeNoBackup = true
		res := mustRun(t, h, spec, pol, nil)
		if res.Result != engine.ResultSuccess || res.Scanned != 1000 || res.Deleted != 1000 || h.Src.Len() != 0 {
			t.Fatalf("%s：%+v 剩 %d", mode, res, h.Src.Len())
		}
		for _, rq := range h.Src.ReadReqs {
			if !rq.Unordered || len(rq.After) != 0 {
				t.Errorf("%s：读取应不排序、没有断点：%+v", mode, rq)
			}
		}
		if res.Checkpoint != nil || strings.Join(res.MinPK, ",") != "1" || strings.Join(res.MaxPK, ",") != "1000" {
			t.Errorf("%s：断点 %v 范围 %v~%v", mode, res.Checkpoint, res.MinPK, res.MaxPK)
		}
		if len(h.Sink.Find("log", "已忽略旧断点")) == 0 {
			t.Errorf("%s：应说明忽略了旧断点", mode)
		}
	}
}

func TestRunBadCheckpoint(t *testing.T) {
	h, spec := copyHarness(100)
	spec.Checkpoint = []string{"1", "2"}
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultFailed || !strings.Contains(res.Err, "断点") || h.Src.Reads != 0 {
		t.Fatalf("%+v", res)
	}
	// 归档并删除不使用断点：旧断点个数不对也不影响运行
	h2 := enginetest.New(enginetest.Options{Rows: 100})
	spec2 := h2.Spec()
	spec2.Checkpoint = []string{"1", "2"}
	if res := mustRun(t, h2, spec2, h2.Policy(), nil); res.Result != engine.ResultSuccess || res.Deleted != 100 {
		t.Fatalf("归档并删除应忽略旧断点：%+v", res)
	}
}

func TestRunWindowEndAndResume(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 1000})
	spec := h.Spec()
	spec.Params.Window = engine.Window{Start: "02:00", End: "02:30"}
	h.Src.DeleteCost = func(int) time.Duration { return 6 * time.Minute }
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultWindowEnd || res.Batches != 5 || res.Scanned != 500 || res.AutoPause || res.Err != "" {
		t.Fatalf("%+v", res)
	}
	if res.Checkpoint != nil || h.Src.Len() != 500 {
		t.Errorf("窗口结束后没有断点，剩下的行留在表里等下次：断点 %v 剩 %d", res.Checkpoint, h.Src.Len())
	}
	// 下一个窗口接着跑：不需要任何断点，已删除的行不会再被读到
	h.Clock.Advance(23*time.Hour + 30*time.Minute)
	h.Src.DeleteCost = nil
	res2 := mustRun(t, h, spec, h.Policy(), nil)
	if res2.Result != engine.ResultSuccess || res2.Scanned != 500 || h.Src.Len() != 0 {
		t.Fatalf("%+v", res2)
	}
}

func TestRunWindowAcrossMidnight(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 1000, Start: time.Date(2026, 10, 8, 23, 50, 0, 0, time.Local)})
	spec := h.Spec()
	spec.Params.Window = engine.Window{Start: "23:00", End: "00:30"}
	h.Src.DeleteCost = func(int) time.Duration { return 10 * time.Minute }
	res := mustRun(t, h, spec, h.Policy(), nil)
	// 批开始时刻：23:50 00:00 00:10 00:20 在窗口内，00:30 不在
	if res.Result != engine.ResultWindowEnd || res.Batches != 4 {
		t.Fatalf("跨零点窗口：%+v", res)
	}
	// 窗口外启动：一批都不跑
	h2 := enginetest.New(enginetest.Options{Rows: 100, Start: time.Date(2026, 10, 8, 12, 0, 0, 0, time.Local)})
	res = mustRun(t, h2, spec, h2.Policy(), nil)
	if res.Result != engine.ResultWindowEnd || res.Batches != 0 || h2.Src.Reads != 0 {
		t.Fatalf("窗口外：%+v", res)
	}
}

func TestRunRowLimit(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 1000})
	spec := h.Spec()
	spec.Params.MaxRows = 250
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultLimitReached || res.Scanned != 250 || res.Deleted != 250 || h.Src.Len() != 750 {
		t.Fatalf("%+v", res)
	}
	var limits []int
	for _, r := range h.Src.ReadReqs {
		limits = append(limits, r.Limit)
	}
	if fmt.Sprint(limits) != "[100 100 50]" {
		t.Errorf("最后一批应只取剩余行数：%v", limits)
	}
	if res.Checkpoint != nil || strings.Join(res.MinPK, ",") != "1" || strings.Join(res.MaxPK, ",") != "250" {
		t.Errorf("断点 %v 范围 %v~%v", res.Checkpoint, res.MinPK, res.MaxPK)
	}
	// 策略硬上限更小时取小者
	h2 := enginetest.New(enginetest.Options{Rows: 1000})
	pol := h2.Policy()
	pol.MaxRowsPerRun = 150
	res = mustRun(t, h2, h2.Spec(), pol, nil)
	if res.Result != engine.ResultLimitReached || res.Scanned != 150 {
		t.Fatalf("%+v", res)
	}
	// 恰好等于表行数：读完即成功，不是 limit_reached
	h3 := enginetest.New(enginetest.Options{Rows: 250})
	spec = h3.Spec()
	spec.Params.MaxRows = 300
	if res = mustRun(t, h3, spec, h3.Policy(), nil); res.Result != engine.ResultSuccess || res.Scanned != 250 {
		t.Fatalf("%+v", res)
	}
}

// ---- 负载自检 ----

func TestRunLoadWaitThenResume(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	h.Probe.Threads = func(call int) (int, error) {
		if call >= 2 && call <= 4 { // 第 2 批前开始超限，复查两次后恢复
			return 99, nil
		}
		return 5, nil
	}
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultSuccess || h.Src.Len() != 0 || res.ThrottleCount != 1 || res.PeakThreads != 99 {
		t.Fatalf("%+v", res)
	}
	waits := 0
	for _, d := range h.Clock.Slept() {
		if d == 30*time.Second {
			waits++
		}
	}
	if waits != 3 {
		t.Errorf("应每 30 秒复查一次，共等待 3 次，得到 %d：%v", waits, h.Clock.Slept())
	}
	if len(h.Sink.Find("throttle", "Threads_running=99")) != 1 || len(h.Sink.Find("resume", "负载恢复")) != 1 {
		t.Errorf("事件：%+v", h.Sink.Events())
	}
	// 进度里能看到被限流的状态
	sawThrottled := false
	for _, p := range h.Sink.Progresses() {
		if p.Throttled && strings.Contains(p.ThrottleReason, "Threads_running") {
			sawThrottled = true
		}
	}
	if !sawThrottled {
		t.Errorf("进度应反映限流状态")
	}
	if p := h.Sink.LastProgress(); p.Throttled {
		t.Errorf("结束时不应仍是限流状态")
	}
}

func TestRunLoadAbort(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	h.Probe.Threads = func(call int) (int, error) { return 99, nil }
	spec := h.Spec()
	spec.Params.MaxWaitSeconds = 100
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultLoadAbort || res.AutoPause || res.Err != "" || res.Batches != 0 || h.Src.Reads != 0 {
		t.Fatalf("负载中断不算失败：%+v", res)
	}
	var total time.Duration
	for _, d := range h.Clock.Slept() {
		total += d
	}
	if total != 100*time.Second {
		t.Errorf("总等待 %v，应恰好是最长等待 100s（30+30+30+10）", total)
	}
	if res.ThrottleCount != 1 {
		t.Errorf("ThrottleCount=%d", res.ThrottleCount)
	}
}

func TestRunLoadAbortMidRun(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	h.Probe.Threads = func(call int) (int, error) {
		if call > 2 {
			return 99, nil
		}
		return 1, nil
	}
	spec := h.Spec()
	spec.Params.MaxWaitSeconds = 60
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultLoadAbort || res.Batches != 2 || res.Checkpoint != nil || res.Deleted != 200 || strings.Join(res.MaxPK, ",") != "200" {
		t.Fatalf("%+v", res)
	}
}

func TestRunLoadChecksNotDetected(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	h.Probe.Threads = func(int) (int, error) { return 0, errors.New("access denied") }
	h.Probe.Lag = func(int) (int, bool, error) { return 0, true, errors.New("replica unreachable") }
	h.Probe.Disk = func(int) (float64, bool, error) { return 0, false, nil }
	h.Dst.DiskOK = false
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultSuccess {
		t.Fatalf("取不到的项不阻塞运行：%+v", res)
	}
	for _, item := range []string{"threads_running", "replica_lag", "src_disk", "dest_disk"} {
		evs := h.Sink.Find("warn", "未检测："+item)
		if len(evs) != 1 {
			t.Errorf("%s：每个运行应只记一条“未检测”事件，得到 %d", item, len(evs))
		} else if evs[0].Fields["status"] != "undetected" {
			t.Errorf("%s 的事件应带 status=undetected", item)
		}
	}
}

func TestRunLagAndDiskThresholds(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 300})
	h.Probe.Lag = func(call int) (int, bool, error) {
		if call == 2 {
			return 30, true, nil
		}
		return 1, true, nil
	}
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.ThrottleCount != 1 || len(h.Sink.Find("throttle", "复制延迟 30 秒超过阈值 5 秒")) != 1 {
		t.Fatalf("复制延迟：%+v %+v", res, h.Sink.Find("throttle", ""))
	}
	// 磁盘：源库磁盘低于阈值
	h2 := enginetest.New(enginetest.Options{Rows: 300})
	h2.Probe.Disk = func(call int) (float64, bool, error) { return 8, true, nil }
	spec := h2.Spec()
	spec.Params.MaxWaitSeconds = 60
	res = mustRun(t, h2, spec, h2.Policy(), nil)
	if res.Result != engine.ResultLoadAbort || len(h2.Sink.Find("throttle", "源库磁盘剩余 8.0% 低于阈值 15%")) != 1 {
		t.Fatalf("磁盘：%+v", res)
	}
	// 目标库磁盘
	h3 := enginetest.New(enginetest.Options{Rows: 300})
	h3.Dst.DiskPct = 3
	spec = h3.Spec()
	spec.Params.MaxWaitSeconds = 60
	if res = mustRun(t, h3, spec, h3.Policy(), nil); res.Result != engine.ResultLoadAbort || len(h3.Sink.Find("throttle", "目标库磁盘")) != 1 {
		t.Fatalf("目标磁盘：%+v", res)
	}
	// 文件存储空间不足
	h4 := enginetest.New(enginetest.Options{Rows: 300})
	h4.Files.FreeBytes = 10 << 20
	fs := h4.FileSpec("csv", false, false)
	fs.Params.MaxWaitSeconds = 60
	if res = mustRun(t, h4, fs, h4.Policy(), nil); res.Result != engine.ResultLoadAbort || len(h4.Sink.Find("throttle", "文件存储可用空间")) != 1 {
		t.Fatalf("文件空间：%+v", res)
	}
}

func TestRunDiskCheckIsCached(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 1000})
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	_, lag, disk := h.Probe.Calls()
	if res.Batches != 10 || disk >= 10 || lag < 10 {
		t.Errorf("磁盘检查应缓存（30 秒内不重复查），复制延迟每批查：disk=%d lag=%d", disk, lag)
	}
}

func TestRunLagCheckDisabledWithReplicaRefuses(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 100})
	h.Probe.Lag = func(int) (int, bool, error) { return 0, true, nil }
	spec := h.Spec()
	spec.Params.MaxLagSeconds = -1
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultFailed || !strings.Contains(res.Err, "从库") || h.Src.Reads != 0 {
		t.Fatalf("%+v", res)
	}
	// 没有从库时允许关闭
	h2 := enginetest.New(enginetest.Options{Rows: 100})
	spec = h2.Spec()
	spec.Params.MaxLagSeconds = -1
	if res = mustRun(t, h2, spec, h2.Policy(), nil); res.Result != engine.ResultSuccess {
		t.Fatalf("%+v", res)
	}
}

// ---- 批耗时自适应 ----

func TestRunAdaptiveBatchSize(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 4000})
	spec := h.Spec()
	spec.Params.BatchSize = 1000
	spec.Params.SleepMs = 100
	h.Src.ReadCost = func(call int) time.Duration {
		if call <= 3 {
			return 800 * time.Millisecond // > 目标 500ms
		}
		return 10 * time.Millisecond
	}
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultSuccess || h.Src.Len() != 0 {
		t.Fatalf("%+v", res)
	}
	var limits []int
	for _, r := range h.Src.ReadReqs {
		limits = append(limits, r.Limit)
	}
	// 前 3 批慢：1000 → 500 → 250 → 125；之后连续 5 批正常恢复一步（×1.5 向上取整 188）
	if len(limits) < 10 || fmt.Sprint(limits[:4]) != "[1000 500 250 125]" {
		t.Fatalf("批大小序列：%v", limits)
	}
	if limits[4] != 125 || limits[7] != 125 || limits[8] != 188 {
		t.Errorf("连续 5 批正常后才恢复：%v", limits)
	}
	slept := h.Clock.Slept()
	if len(slept) < 4 || slept[0] != ms(200) || slept[1] != ms(400) || slept[2] != ms(800) {
		t.Errorf("休眠应加倍：%v", slept)
	}
	if res.ThrottleCount != 3 {
		t.Errorf("每次因慢降速计一次限流：%d", res.ThrottleCount)
	}
	if len(h.Sink.Find("throttle", "批大小 1000 → 500")) != 1 || len(h.Sink.Find("throttle", "逐步恢复")) == 0 {
		t.Errorf("事件：%+v", h.Sink.Find("throttle", ""))
	}
	// 不超过设定值
	for _, l := range limits {
		if l > 1000 {
			t.Errorf("批大小不得超过设定值：%d", l)
		}
	}
}

// ---- 暂停 / 终止 / 取消 ----

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func TestRunPauseResume(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 500})
	ctl := engine.NewControl()
	h.Src.OnRead = func(call int, req engine.ReadReq) error {
		if call == 2 {
			ctl.Pause() // 在第 2 批读取过程中请求暂停：第 2 批做完后停在批边界
		}
		return nil
	}
	spec := h.Spec()
	done := make(chan *engine.RunResult, 1)
	go func() {
		res, err := newEng().Run(t.Context(), spec, h.Policy(), h.EnvFor(spec, t.TempDir()), ctl, h.Sink)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	waitUntil(t, "进入暂停", func() bool { return len(h.Sink.Find("log", "已暂停")) == 1 })
	if !ctl.Paused() {
		t.Errorf("Paused() 应为 true")
	}
	time.Sleep(30 * time.Millisecond)
	if h.Src.Reads != 2 || h.Src.Len() != 300 {
		t.Fatalf("暂停时应已完成第 2 批、尚未读第 3 批：读 %d 次，剩 %d 行", h.Src.Reads, h.Src.Len())
	}
	select {
	case <-done:
		t.Fatal("暂停期间运行不应结束")
	default:
	}
	ctl.Resume()
	res := <-done
	if res.Result != engine.ResultSuccess || h.Src.Len() != 0 || res.Scanned != 500 {
		t.Fatalf("%+v", res)
	}
	if len(h.Sink.Find("resume", "已恢复")) != 1 {
		t.Errorf("应有恢复事件")
	}
	if ctl.Paused() {
		t.Errorf("恢复后 Paused() 应为 false")
	}
}

func TestRunStopFinishesCurrentBatch(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 500})
	ctl := engine.NewControl()
	h.Src.OnRead = func(call int, req engine.ReadReq) error {
		if call == 2 {
			ctl.Stop()
		}
		return nil
	}
	res := mustRun(t, h, h.Spec(), h.Policy(), ctl)
	if res.Result != engine.ResultStopped || res.Batches != 2 || res.Deleted != 200 || h.Src.Len() != 300 {
		t.Fatalf("终止应在当前批做完后结束：%+v 剩 %d", res, h.Src.Len())
	}
	if res.Checkpoint != nil || strings.Join(res.MaxPK, ",") != "200" || res.AutoPause {
		t.Errorf("%+v", res)
	}
	checkOrder(t, h)
	if !ctl.Stopped() {
		t.Errorf("Stopped()")
	}
}

func TestRunStopWhilePausedAndDuringLoadWait(t *testing.T) {
	// 暂停中被终止
	h := enginetest.New(enginetest.Options{Rows: 500})
	ctl := engine.NewControl()
	h.Src.OnRead = func(call int, req engine.ReadReq) error {
		if call == 1 {
			ctl.Pause()
		}
		return nil
	}
	spec := h.Spec()
	done := make(chan *engine.RunResult, 1)
	go func() {
		res, _ := newEng().Run(t.Context(), spec, h.Policy(), h.EnvFor(spec, t.TempDir()), ctl, h.Sink)
		done <- res
	}()
	waitUntil(t, "进入暂停", func() bool { return ctl.Paused() && len(h.Sink.Find("log", "已暂停")) == 1 })
	ctl.Stop()
	if res := <-done; res.Result != engine.ResultStopped || res.Batches != 1 {
		t.Fatalf("%+v", res)
	}
	if ctl.Paused() {
		t.Errorf("Stop 之后不再是暂停状态")
	}

	// 负载等待中被终止（真实睡眠，验证 Stop 能打断等待）
	h2 := enginetest.New(enginetest.Options{Rows: 500})
	h2.Probe.Threads = func(int) (int, error) { return 99, nil }
	ctl2 := engine.NewControl()
	env := h2.EnvFor(spec, t.TempDir())
	env.Sleep = nil // 用真实睡眠：30 秒
	done2 := make(chan *engine.RunResult, 1)
	start := time.Now()
	go func() {
		res, _ := newEng().Run(t.Context(), spec, h2.Policy(), env, ctl2, h2.Sink)
		done2 <- res
	}()
	waitUntil(t, "进入负载等待", func() bool { return len(h2.Sink.Find("throttle", "负载超限")) == 1 })
	ctl2.Stop()
	select {
	case res := <-done2:
		if res.Result != engine.ResultStopped || res.Batches != 0 {
			t.Fatalf("%+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 没能打断负载等待")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("等待没有被及时打断")
	}
}

func TestRunContextCancelIsStopped(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 500})
	ctx, cancel := context.WithCancel(t.Context())
	h.Src.OnRead = func(call int, req engine.ReadReq) error {
		if call == 2 {
			cancel()
		}
		return nil
	}
	spec := h.Spec()
	res, err := newEng().Run(ctx, spec, h.Policy(), h.EnvFor(spec, t.TempDir()), nil, h.Sink)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != engine.ResultStopped || res.AutoPause || res.Err != "" {
		t.Fatalf("ctx 取消 = stopped：%+v", res)
	}
	if h.Src.Len() != 400 { // 只有第 1 批被删；第 2 批在写备份时发现取消，未删
		t.Errorf("剩 %d", h.Src.Len())
	}
	checkOrder(t, h)
	// 已取消的 ctx 开始前就停
	cctx, cancel2 := context.WithCancel(t.Context())
	cancel2()
	h2 := enginetest.New(enginetest.Options{Rows: 10})
	res, _ = newEng().Run(cctx, h2.Spec(), h2.Policy(), h2.Env(), nil, nil)
	if res.Result != engine.ResultStopped || h2.Src.Reads != 0 {
		t.Fatalf("%+v", res)
	}
}

// ---- 结构变化 ----

func TestRunStructureChanged(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 500})
	spec := h.Spec()
	spec.Fingerprint = "00000000-审批时的指纹"
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultStructure || !res.AutoPause || h.Src.Reads != 0 || h.Src.Len() != 500 {
		t.Fatalf("开始前指纹不一致：%+v", res)
	}
	if len(h.Sink.Find("structure", "")) != 1 {
		t.Errorf("应有结构变更事件")
	}
	// 运行中途表结构被改
	h2 := enginetest.New(enginetest.Options{Rows: 500})
	h2.Src.OnRead = func(call int, req engine.ReadReq) error {
		if call == 3 {
			h2.Src.SetFingerprint("changed-by-ddl")
		}
		return nil
	}
	res = mustRun(t, h2, h2.Spec(), h2.Policy(), nil)
	if res.Result != engine.ResultStructure || !res.AutoPause || res.Batches != 3 || h2.Src.Len() != 200 {
		t.Fatalf("运行中结构变化：%+v 剩 %d", res, h2.Src.Len())
	}
	// 没有传审批指纹时，以运行开始时的结构为基线，同样能发现中途变化
	h3 := enginetest.New(enginetest.Options{Rows: 500})
	h3.Src.OnRead = func(call int, req engine.ReadReq) error {
		if call == 2 {
			h3.Src.SetFingerprint("changed")
		}
		return nil
	}
	spec = h3.Spec()
	spec.Fingerprint = ""
	if res = mustRun(t, h3, spec, h3.Policy(), nil); res.Result != engine.ResultStructure || res.Batches != 2 {
		t.Fatalf("%+v", res)
	}
}

func TestRunSourceProblems(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 10})
	h.Src.Info.Exists = false
	if res := mustRun(t, h, h.Spec(), h.Policy(), nil); res.Result != engine.ResultFailed || !strings.Contains(res.Err, "不存在") {
		t.Errorf("%+v", res)
	}
	h = enginetest.New(enginetest.Options{Rows: 10})
	h.Src.Info.PrimaryKey = nil
	if res := mustRun(t, h, h.Spec(), h.Policy(), nil); res.Result != engine.ResultFailed || !strings.Contains(res.Err, "主键") {
		t.Errorf("%+v", res)
	}
	h = enginetest.New(enginetest.Options{Rows: 10})
	h.Src.Info.FKParents = []string{"shop.refunds"}
	if res := mustRun(t, h, h.Spec(), h.Policy(), nil); res.Result != engine.ResultFailed || !strings.Contains(res.Err, "外键") || h.Src.Reads != 0 {
		t.Errorf("%+v", res)
	}
	h = enginetest.New(enginetest.Options{Rows: 10})
	h.Dst.Databases = map[string]bool{} // 目标库不存在
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultFailed || !res.AutoPause || h.Src.Reads != 0 || !strings.Contains(res.Err, "归档库表") {
		t.Errorf("目标库不存在：%+v", res)
	}
}

// ---- 复合主键 ----

func TestRunCompoundPK(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 400, CompoundPK: true})
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Deleted != 400 || h.Src.Len() != 0 {
		t.Fatalf("%+v", res)
	}
	if r := h.Src.DeleteReqs[0]; len(r.PKCols) != 2 || r.PKCols[0] != "tenant" || len(r.PKs[0]) != 2 {
		t.Errorf("复合主键删除请求：%+v", r.PKCols)
	}
	if strings.Join(res.MinPK, ",") != "a,1" || strings.Join(res.MaxPK, ",") != "b,400" {
		t.Errorf("范围 %v ~ %v", res.MinPK, res.MaxPK)
	}
	checkOrder(t, h)
	// 断点是元组：(a,150) 之后
	h2 := enginetest.New(enginetest.Options{Rows: 400, CompoundPK: true})
	spec := h2.Spec()
	spec.Checkpoint = []string{"a", "150"}
	res = mustRun(t, h2, spec, h2.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Scanned != 250 || h2.Src.Len() != 150 || !h2.Src.Has("a,150") || h2.Src.Has("a,151") || h2.Src.Has("b,400") {
		t.Fatalf("%+v 剩 %d", res, h2.Src.Len())
	}
	if first := h2.Src.ReadReqs[0]; len(first.After) != 2 || first.After[0] != "a" {
		t.Errorf("首批断点 %v", first.After)
	}
	// 断点列数不对
	spec.Checkpoint = []string{"150"}
	if res = mustRun(t, h2, spec, h2.Policy(), nil); res.Result != engine.ResultFailed {
		t.Errorf("%+v", res)
	}
}

// ---- 事件 / 进度不阻塞 ----

type slowSink struct {
	n     atomic.Int64
	delay time.Duration
}

func (s *slowSink) Event(engine.Event)       { s.n.Add(1); time.Sleep(s.delay) }
func (s *slowSink) Progress(engine.Progress) { time.Sleep(s.delay) }

func TestRunProgressIsThrottled(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 5000})
	spec := h.Spec()
	spec.Params.SleepMs = 50
	// 假时钟每批前进 ~50ms（休眠），5000 行 / 100 = 50 批 ≈ 2.5 秒 → 进度最多 ~4 条而不是 50 条
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Batches != 50 {
		t.Fatalf("%+v", res)
	}
	if n := len(h.Sink.Progresses()); n > 8 {
		t.Errorf("进度应节流到约每秒一次：%d 条", n)
	}
	if n := len(h.Sink.Find("batch", "")); n > 8 || n < 1 {
		t.Errorf("批事件应节流：%d 条", n)
	}
	// 慢 Sink 不会导致引擎出错
	h2 := enginetest.New(enginetest.Options{Rows: 300})
	ss := &slowSink{delay: time.Millisecond}
	spec = h2.Spec()
	res2, err := newEng().Run(t.Context(), spec, h2.Policy(), h2.Env(), nil, ss)
	if err != nil || res2.Result != engine.ResultSuccess {
		t.Fatalf("%v %+v", err, res2)
	}
}
