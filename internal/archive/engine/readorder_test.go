// @author: kevinkelly
// @license: Apache Licence
// @file: readorder_test.go
// @time: 2026/10/28

package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/archive/engine/enginetest"
)

// ---- 空转保护 ----

// 不排序读取没有断点可以“越过”卡住的行：读到行却一行都删不掉，必须停下来，不能原地打转。
func TestRunUnorderedIdleGuard(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 500})
	h.Src.DeleteAffected = func(call int, actual int64) int64 {
		if call >= 2 {
			return 0 // 第 2 批起删除没有生效（比如触发器、权限、行被并发改走）
		}
		return actual
	}
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultFailed || !res.AutoPause || !strings.Contains(res.Err, "一行也没有删掉") {
		t.Fatalf("应停止并自动暂停：%+v", res)
	}
	if h.Src.Reads != 2 || res.Batches != 2 {
		t.Errorf("第 2 批删不掉就该停，不能继续读：reads=%d batches=%d", h.Src.Reads, res.Batches)
	}
	checkOrder(t, h)
	// 部分行删不掉不算空转（并发改动导致少删几行是正常的），照常往下跑
	h2 := enginetest.New(enginetest.Options{Rows: 300})
	h2.Src.DeleteAffected = func(call int, actual int64) int64 { return actual - 1 }
	if res := mustRun(t, h2, h2.Spec(), h2.Policy(), nil); res.Result != engine.ResultSuccess || len(h2.Sink.Find("warn", "不一致")) == 0 {
		t.Fatalf("少删几行只告警：%+v", res)
	}
}

// ---- 读取过慢保护 ----

func TestRunSlowReadGuard(t *testing.T) {
	// 连续 3 次读取都超过 15 秒：停止并自动暂停，第 3 批的行没有被处理
	h := enginetest.New(enginetest.Options{Rows: 1000})
	h.Src.ReadCost = func(int) time.Duration { return 20 * time.Second }
	res := mustRun(t, h, h.Spec(), h.Policy(), nil)
	if res.Result != engine.ResultFailed || !res.AutoPause || !strings.Contains(res.Err, "连续 3 次读取一批都超过 15 秒") {
		t.Fatalf("%+v", res)
	}
	if res.Batches != 2 || h.Src.Len() != 800 {
		t.Errorf("第 3 次过慢就停，该批不处理：batches=%d 剩 %d", res.Batches, h.Src.Len())
	}
	if len(h.Sink.Find("warn", "读取一批耗时 20 秒")) != 3 {
		t.Errorf("每次过慢都应有告警事件：%d", len(h.Sink.Find("warn", "读取一批耗时 20 秒")))
	}
	checkOrder(t, h)
	// 中间有一次正常读取就清零：慢、慢、快、慢、慢 不会触发
	h2 := enginetest.New(enginetest.Options{Rows: 700})
	h2.Src.ReadCost = func(call int) time.Duration {
		if call == 3 || call > 5 {
			return 10 * time.Millisecond
		}
		return 20 * time.Second
	}
	if res := mustRun(t, h2, h2.Spec(), h2.Policy(), nil); res.Result != engine.ResultSuccess || h2.Src.Len() != 0 {
		t.Fatalf("不连续的过慢不应停止：%+v", res)
	}
	// 仅备份同样有保护
	h3, spec3 := copyHarness(1000)
	h3.Src.ReadCost = func(int) time.Duration { return 20 * time.Second }
	if res := mustRun(t, h3, spec3, h3.Policy(), nil); res.Result != engine.ResultFailed || !res.AutoPause {
		t.Fatalf("仅备份也应有读取过慢保护：%+v", res)
	}
}

// ---- 主键不能在本地比大小：仍按主键排序读取 ----

func TestRunStringPKStillReadsOrderedWithCheckpoint(t *testing.T) {
	h := enginetest.New(enginetest.Options{Rows: 400, CompoundPK: true}) // 主键 (tenant varchar, id)
	spec := h.Spec()
	res := mustRun(t, h, spec, h.Policy(), nil)
	if res.Result != engine.ResultSuccess || res.Deleted != 400 || h.Src.Len() != 0 {
		t.Fatalf("%+v", res)
	}
	for _, rq := range h.Src.ReadReqs {
		if rq.Unordered {
			t.Fatalf("含字符串列的主键不能不排序读取（本地算不准恢复范围）：%+v", rq)
		}
	}
	if len(h.Src.ReadReqs) < 2 || len(h.Src.ReadReqs[1].After) != 2 {
		t.Errorf("应带着断点翻页：%+v", h.Src.ReadReqs)
	}
	if res.Checkpoint != nil { // 跑完清空，下次从头
		t.Errorf("完整跑完后断点应清空：%v", res.Checkpoint)
	}
	// 试运行里同样按排序读取评估，预览带 ORDER BY
	h2 := enginetest.New(enginetest.Options{Rows: 400, CompoundPK: true})
	h2.Src.Info.Indexes["idx_created"] = []string{"created_at"}
	rep := runCheck(t, h2, h2.Spec())
	if !strings.Contains(rep.SQL.Select, "ORDER BY") || h2.Src.ReadReqs[0].Unordered {
		t.Errorf("字符串主键应按主键排序读取：%s %+v", rep.SQL.Select, h2.Src.ReadReqs[0])
	}
	wantLevel(t, rep, "pk", "ok", "按主键升序分批")
}

// ---- 试运行：实测首批读取 ----

func TestCheckFirstRead(t *testing.T) {
	h := tableOfMany(1_000_000)
	rep := runCheck(t, h, h.Spec())
	wantLevel(t, rep, "first_read", "ok", "首批读取 100 行，耗时 0 ms")
	wantLevel(t, rep, "pk", "ok", "每批取走当前符合条件的前 N 行（不排序）")
	if !strings.Contains(rep.SQL.Select, "LIMIT 100") || strings.Contains(rep.SQL.Select, "ORDER BY") {
		t.Errorf("预览应是不排序读取：%s", rep.SQL.Select)
	}
	if req := h.Src.ReadReqs[0]; !req.Unordered || len(req.After) != 0 || req.Limit != 100 {
		t.Errorf("首批读取请求：%+v", req)
	}
	// 慢：黄色（≥ 3 秒）、红色（≥ 15 秒）；红色阻断提交
	h.Src.ReadCost = func(int) time.Duration { return 4 * time.Second }
	wantLevel(t, runCheck(t, h, h.Spec()), "first_read", "warn", "耗时 4.0 秒")
	h.Src.ReadCost = func(int) time.Duration { return 20 * time.Second }
	rep = runCheck(t, h, h.Spec())
	wantLevel(t, rep, "first_read", "error", "耗时 20.0 秒")
	if !rep.HasError() {
		t.Error("首批读取过慢应判红")
	}
	// 读取出错：黄色，不拦截；超时：红色
	h.Src.ReadCost = nil
	h.Src.OnRead = func(int, engine.ReadReq) error { return errors.New("connection reset") }
	rep = runCheck(t, h, h.Spec())
	wantLevel(t, rep, "first_read", "warn", "首批读取失败：connection reset")
	h.Src.OnRead = func(int, engine.ReadReq) error { return context.DeadlineExceeded }
	rep = runCheck(t, h, h.Spec())
	wantLevel(t, rep, "first_read", "error", "超过 60 秒仍没有返回")
	// 仅备份：按主键翻页读取，带断点
	hc := tableOfMany(1_000_000)
	spec := hc.Spec()
	spec.Mode = engine.ModeCopy
	spec.Checkpoint = []string{"500"}
	rep = runCheck(t, hc, spec)
	if req := hc.Src.ReadReqs[0]; req.Unordered || len(req.After) != 1 || req.After[0] != "500" {
		t.Errorf("仅备份首批读取应带断点、按主键排序：%+v", req)
	}
	wantLevel(t, rep, "pk", "ok", "按主键升序分批")
	if !strings.Contains(rep.SQL.Select, "ORDER BY `id`") {
		t.Errorf("仅备份预览应排序：%s", rep.SQL.Select)
	}
	// 条件没通过时不读：时间列不存在
	hb := tableOfMany(1000)
	sb := hb.Spec()
	sb.TimeColumn = "nope"
	runCheck(t, hb, sb)
	if hb.Src.Reads != 0 {
		t.Errorf("时间列非法时不应读取：%d", hb.Src.Reads)
	}
}
