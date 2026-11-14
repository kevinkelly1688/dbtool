// @author: kevinkelly
// @license: Apache Licence
// @file: restore.go
// @time: 2026/10/28

package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// restore 把库表备份中 [MinPK, MaxPK] 范围的行 INSERT IGNORE 回源表：只补缺、不覆盖。幂等：重复执行只会补回仍然缺的行。
// 恢复同样遵守负载自检、批间休眠、连续失败上限、暂停 / 终止；不受执行窗口限制（人工触发）。
func (e *Engine) restore(ctx context.Context, spec Spec, rng RestoreRange, env *Env, ctl *Control, sink Sink) (*RunResult, error) {
	if env == nil || env.Src == nil || env.Dst == nil {
		return nil, errors.New("恢复需要 Env.Src（写源表）和 Env.Dst（读备份表）")
	}
	if !validIdent(spec.DB) || !validIdent(spec.Table) {
		return nil, errors.New("源库 / 表名无效")
	}
	if IsProtected(nil, spec.DB, spec.Table) {
		return nil, fmt.Errorf("%s.%s 属于受保护库表", spec.DB, spec.Table)
	}
	if !validIdent(rng.DestDB) || !validIdent(rng.DestTable) {
		return nil, errors.New("备份库 / 表名无效")
	}
	if len(rng.MinPK) == 0 || len(rng.MinPK) != len(rng.MaxPK) {
		return nil, errors.New("恢复需要有效的主键范围（MinPK / MaxPK 个数相同且不为空）")
	}
	r, cancel := newRunner(ctx, spec, Policy{}, env, ctl, sink)
	defer cancel()
	rr := &restoreRun{runner: r, rng: rng}
	rr.execute()
	return &rr.res, nil
}

type restoreRun struct {
	*runner
	rng RestoreRange
}

func (x *restoreRun) execute() {
	r := x.runner
	spec := r.spec
	r.emit("log", fmt.Sprintf("恢复开始：从 %s.%s 按主键 %s ~ %s 补回 %s.%s（INSERT IGNORE，只补缺不覆盖）",
		x.rng.DestDB, x.rng.DestTable, strings.Join(x.rng.MinPK, ","), strings.Join(x.rng.MaxPK, ","), spec.DB, spec.Table))
	r.res.MinPK, r.res.MaxPK = copyStrings(x.rng.MinPK), copyStrings(x.rng.MaxPK)
	r.res.DestTable = x.rng.DestTable
	defer func() {
		if r.res.Result == "" {
			r.res.Result = ResultFailed
		}
		r.res.Duration = r.now().Sub(r.start)
		r.res.PeakThreads = r.maxThreads
		r.emit("checkpoint", fmt.Sprintf("恢复结束：%s，读取 %d 行，补回 %d 行", r.res.Result, r.res.Scanned, r.res.Backed), "result", r.res.Result)
		r.emitProgress(true)
	}()

	if r.stopped() {
		r.fail(ResultStopped, "", false)
		return
	}
	src, err := r.env.Src.Describe(r.ctx, spec.DB, spec.Table)
	if err != nil {
		r.fail(ResultFailed, fmt.Sprintf("读取源表结构失败：%s", err.Error()), false)
		return
	}
	if !src.Exists || src.IsView {
		r.fail(ResultFailed, fmt.Sprintf("源表 %s.%s 不存在", spec.DB, spec.Table), true)
		return
	}
	if len(src.PrimaryKey) == 0 || len(src.PrimaryKey) != len(x.rng.MinPK) {
		r.fail(ResultFailed, "源表主键与恢复范围的主键列数不一致", true)
		return
	}
	dst, err := r.env.Dst.Describe(r.ctx, x.rng.DestDB, x.rng.DestTable)
	if err != nil {
		r.fail(ResultFailed, fmt.Sprintf("读取备份表结构失败：%s", err.Error()), false)
		return
	}
	if !dst.Exists {
		r.fail(ResultFailed, fmt.Sprintf("备份表 %s.%s 不存在", x.rng.DestDB, x.rng.DestTable), true)
		return
	}
	if err := compatErr(src, dst); err != nil {
		r.fail(ResultFailed, fmt.Sprintf("备份表与源表结构不兼容：%s", err.Error()), true)
		return
	}
	if len(dst.Columns) != len(src.Columns) {
		r.fail(ResultFailed, "备份表的列数与源表不一致，不能直接恢复", true)
		return
	}
	pk := src.PrimaryKey
	var after []string
	fails := 0
	batch := r.p.BatchSize
	for {
		if r.waitWhilePaused() || r.stopped() {
			r.fail(ResultStopped, "", false)
			return
		}
		switch r.waitLoad() {
		case loadAbort:
			r.fail(ResultLoadAbort, "", false)
			r.res.Err = ""
			return
		case loadStopped:
			r.fail(ResultStopped, "", false)
			return
		}
		t0 := r.now()
		var b Batch
		err := r.retry("读取备份表", func() error {
			var e error
			b, e = r.env.Dst.ReadRange(r.ctx, RangeReq{DB: x.rng.DestDB, Table: x.rng.DestTable, PKCols: pk,
				Min: x.rng.MinPK, Max: x.rng.MaxPK, After: after, Limit: batch, LockWaitSe: r.p.LockWaitSeconds})
			return e
		})
		var affected int64
		if err == nil {
			if len(b.Rows) == 0 {
				r.res.Result = ResultSuccess
				return
			}
			if len(b.PKs) != len(b.Rows) {
				err = errors.New("读取结果的主键个数与行数不一致")
			} else {
				err = r.retry("写回源表", func() error {
					var e error
					affected, e = r.env.Src.InsertIgnore(r.ctx, InsertReq{DB: spec.DB, Table: spec.Table, Cols: b.Cols, Rows: b.Rows, LockWaitSe: r.p.LockWaitSeconds})
					return e
				})
			}
		}
		if err != nil {
			if r.stopped() {
				r.fail(ResultStopped, "", false)
				return
			}
			if r.batchFailed(&fails, err) {
				return
			}
			continue
		}
		fails = 0
		n := len(b.Rows)
		r.res.Batches++
		r.res.Scanned += int64(n)
		r.res.Backed += affected
		after = copyStrings(b.PKs[n-1])
		r.res.Checkpoint = copyStrings(after)
		r.prog.BatchSize = batch
		r.emit("batch", fmt.Sprintf("第 %d 批：读取 %d 行，补回 %d 行，耗时 %d ms", r.res.Batches, n, affected, r.now().Sub(t0).Milliseconds()),
			"batch", strconv.Itoa(r.res.Batches), "rows", strconv.Itoa(n), "restored", strconv.FormatInt(affected, 10))
		r.emitProgress(false)
		if n < batch {
			r.res.Result = ResultSuccess
			return
		}
		if r.p.SleepMs > 0 {
			if err := r.wsleep(time.Duration(r.p.SleepMs) * time.Millisecond); err != nil && r.stopped() {
				r.fail(ResultStopped, "", false)
				return
			}
		}
	}
}
