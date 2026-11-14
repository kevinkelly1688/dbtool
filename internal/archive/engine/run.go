// @author: kevinkelly
// @license: Apache Licence
// @file: run.go
// @time: 2026/10/28

package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 批循环（设计 §3 / §4）：
//
//	每批前：停止 / 暂停 → 执行窗口 → 行数上限 → 负载自检 → 表结构指纹
//	每批：读 → 写库表备份并校验 → 写文件备份并校验 → 删源（带原条件）→ 记账
//	  读取有两种：归档并删除 / 仅清理（主键能在本地比大小时）不排序、没有断点，只取当前符合条件的前 N 行，删掉后下一批自然取到剩下的；
//	  仅备份（不删数据）按“主键 > 断点”升序翻页，断点记在每批之后
//	每批后：耗时自适应 → 进度 / 事件 → 批间休眠
//
// 任何路径下，DeleteBatch 只会在“备份已写入且校验通过”之后被调用（purge 模式本来就不备份）。

func (e *Engine) run(ctx context.Context, spec Spec, pol Policy, env *Env, ctl *Control, sink Sink) (*RunResult, error) {
	if env == nil || env.Src == nil {
		return nil, errors.New("Env.Src 未设置")
	}
	if err := IssuesError(validateSpec(spec, pol, false)); err != nil {
		return nil, err
	}
	if spec.Dest != nil && env.Dst == nil {
		return nil, errors.New("任务配置了库表备份，但 Env.Dst 未设置")
	}
	if spec.File != nil {
		if env.Files == nil {
			return nil, errors.New("任务配置了文件备份，但 Env.Files 未设置")
		}
		if spec.File.Encrypt && len(env.FileKey) != 32 {
			return nil, errors.New("文件加密需要 32 字节密钥（Env.FileKey）")
		}
	}
	where, err := buildWhere(spec)
	if err != nil {
		return nil, err
	}
	r, cancel := newRunner(ctx, spec, pol, env, ctl, sink)
	defer cancel()
	ar := &archiveRun{runner: r, where: where}
	ar.execute()
	return &ar.res, nil
}

type archiveRun struct {
	*runner
	where string

	info  TableInfo
	pk    []string
	baseF string // 结构指纹基线

	bw        *backupWriter
	fileKinds []colKind
	// 已写进备份文件、但源行还没删掉的主键（删除失败后重试同一批时，不重复写文件）
	fileDone map[string]struct{}
	minPK    []string

	// 不排序读取（归档并删除 / 仅清理，且主键能在本地比大小）：没有断点，靠“删掉的行不再命中”推进；逐批记最小 / 最大主键供恢复用
	unordered bool
	rng       *pkRange
	slowReads int // 连续过慢的读取次数

	maxRows int64
}

func (a *archiveRun) execute() {
	r := a.runner
	a.res.Checkpoint = copyStrings(r.spec.Checkpoint)
	a.maxRows = EffectiveMaxRows(r.spec.Params, r.pol)
	a.fileDone = map[string]struct{}{}
	r.emit("log", fmt.Sprintf("运行开始（%s），条件：%s", modeName(r.spec.Mode), a.where),
		"mode", string(r.spec.Mode), "where", a.where)

	if a.prepare() {
		a.loop()
	}
	a.finish()
}

func modeName(m Mode) string {
	switch m {
	case ModeMove:
		return "归档并删除"
	case ModeCopy:
		return "仅备份"
	case ModePurge:
		return "仅清理"
	}
	return string(m)
}

// prepare 做开始前的检查；返回 false 表示已经设置了结果，不必进入批循环。
func (a *archiveRun) prepare() bool {
	r := a.runner
	spec := r.spec
	if r.stopped() {
		a.fail(ResultStopped, "运行开始前已被终止", false)
		return false
	}
	info, err := r.env.Src.Describe(r.ctx, spec.DB, spec.Table)
	if err != nil {
		if r.ctx.Err() != nil {
			a.fail(ResultStopped, "", false)
			return false
		}
		a.fail(ResultFailed, fmt.Sprintf("读取源表结构失败：%s", err.Error()), false)
		return false
	}
	switch {
	case !info.Exists:
		a.fail(ResultFailed, fmt.Sprintf("源表 %s.%s 不存在", spec.DB, spec.Table), true)
		return false
	case info.IsView:
		a.fail(ResultFailed, fmt.Sprintf("%s.%s 是视图，不能归档", spec.DB, spec.Table), true)
		return false
	case len(info.PrimaryKey) == 0:
		a.fail(ResultFailed, "源表没有主键（含唯一非空键），不能归档", true)
		return false
	}
	a.info, a.pk = info, info.PrimaryKey
	a.baseF = spec.Fingerprint
	if a.baseF == "" {
		a.baseF = info.Fingerprint
	}
	if info.Fingerprint != a.baseF {
		r.emit("structure", fmt.Sprintf("源表结构与审批 / 试运行时不一致，已停止：%s → %s", a.baseF[:min(8, len(a.baseF))], info.Fingerprint[:min(8, len(info.Fingerprint))]))
		a.fail(ResultStructure, "源表结构指纹与审批 / 试运行时不一致（列名、类型或主键有变化），需要重新试运行", true)
		return false
	}
	if cmps, ok := pkComparators(info); ok && isDeleting(spec.Mode) {
		a.unordered, a.rng = true, newPKRange(cmps)
		if len(spec.Checkpoint) > 0 {
			r.emit("log", "归档并删除 / 仅清理现在不排序读取、不再使用断点，已忽略旧断点（已删除的行不会再被读到）")
		}
		a.res.Checkpoint = nil
	} else if n := len(spec.Checkpoint); n != 0 && n != len(a.pk) {
		a.fail(ResultFailed, fmt.Sprintf("断点有 %d 个值，与主键列数 %d 不一致，请清除断点", n, len(a.pk)), true)
		return false
	}
	if len(info.FKParents) > 0 {
		a.fail(ResultFailed, fmt.Sprintf("源表被外键子表引用（%s），本功能不处理", strings.Join(info.FKParents, "、")), true)
		return false
	}
	// 复制延迟检查被关闭时，有从库就拒绝
	if r.p.MaxLagSeconds < 0 && r.env.Probe != nil {
		_, has, err := r.env.Probe.ReplicaLag(r.ctx)
		if err == nil && has {
			a.fail(ResultFailed, "源实例有从库，不允许关闭复制延迟检查", true)
			return false
		}
		if err != nil {
			r.markUndetected("replica_lag", err)
		}
	}
	if r.p.MaxLagSeconds < 0 {
		r.emit("warn", "复制延迟检查已关闭", "item", "replica_lag", "status", "disabled")
	}
	// 库表备份：建表 / 校验兼容
	if spec.Dest != nil {
		tbl := spec.Dest.Table
		if tbl == "" {
			tbl = expandTableName(spec.Dest.Template, spec.Table, r.start)
		}
		if !validIdent(tbl) {
			a.fail(ResultFailed, fmt.Sprintf("目标表名无效：%s", tbl), true)
			return false
		}
		a.res.DestTable = tbl
		if err := r.env.Dst.EnsureTable(r.ctx, spec.Dest.DB, tbl, info); err != nil {
			if r.ctx.Err() != nil {
				a.fail(ResultStopped, "", false)
				return false
			}
			a.fail(ResultFailed, fmt.Sprintf("准备归档库表失败：%s", err.Error()), true)
			return false
		}
		r.emit("log", fmt.Sprintf("归档库表 %s.%s 就绪", spec.Dest.DB, tbl), "dest", spec.Dest.DB+"."+tbl)
	}
	return true
}

func (a *archiveRun) newBackupWriter(cols []string) error {
	r := a.runner
	f := r.spec.File
	kinds := make([]colKind, len(cols))
	for i, c := range cols {
		for _, ic := range a.info.Columns {
			if strings.EqualFold(ic.Name, c) {
				kinds[i] = kindOf(ic.Type)
			}
		}
	}
	inst := r.env.InstName
	if inst == "" {
		inst = "inst" + strconv.FormatUint(uint64(r.spec.InstanceID), 10)
	}
	dir := expandPath(f.PathTemplate, r.spec, inst, r.start)
	tmp := r.env.TempDir
	if tmp == "" {
		tmp = filepath.Join(os.TempDir(), "dba-archive")
	}
	tag := fmt.Sprintf("t%d-r%d-%d", r.spec.TaskID, r.spec.RunID, r.start.UnixNano())
	bw, err := newBackupWriter(r.env.Files, *f, r.env.FileKey, dir, tmp, tag, r.spec.Table, cols, kinds, r.env.Sleep)
	if err != nil {
		return err
	}
	a.bw = bw
	r.emit("file", fmt.Sprintf("文件备份目录：%s", dir), "dir", dir)
	return nil
}

// batchStage 是一批里出错的阶段，用于事件说明。
func verifyErr(format string, a ...any) error {
	return fmt.Errorf("%w："+format, append([]any{ErrMismatch}, a...)...)
}

// process 处理一批已读到的行：写备份、校验、删源。返回本批删除的行数。
func (a *archiveRun) process(b Batch) (deleted int64, err error) {
	r := a.runner
	spec := r.spec
	n := len(b.Rows)
	if len(b.PKs) != n {
		return 0, fmt.Errorf("读取结果的主键个数 %d 与行数 %d 不一致", len(b.PKs), n)
	}
	// ---- 写备份并校验（purge 不备份）----
	if spec.Mode != ModePurge {
		if spec.Dest != nil {
			dbn, tbl := spec.Dest.DB, a.res.DestTable
			var inserted, existed int64
			if err := r.retry("写归档库表", func() error {
				var e error
				inserted, existed, e = r.env.Dst.Insert(r.ctx, dbn, tbl, b.Cols, b.Rows, a.pk)
				return e
			}); err != nil {
				return 0, err
			}
			if inserted+existed != int64(n) {
				return 0, verifyErr("写入 %d 行 + 已存在 %d 行 ≠ 读取 %d 行", inserted, existed, n)
			}
			var found int64
			if err := r.retry("校验归档库表", func() error {
				var e error
				found, e = r.env.Dst.VerifyPKs(r.ctx, dbn, tbl, a.pk, b.PKs)
				return e
			}); err != nil {
				return 0, err
			}
			if found != int64(n) {
				return 0, verifyErr("目标端按主键只查到 %d 行，读取 %d 行", found, n)
			}
		}
		if spec.File != nil {
			if a.bw == nil {
				if err := a.newBackupWriter(b.Cols); err != nil {
					return 0, err
				}
			}
			var rows []Row
			for i, row := range b.Rows {
				if _, done := a.fileDone[pkKeyOf(b.PKs[i])]; !done {
					rows = append(rows, row)
				}
			}
			if len(rows) > 0 {
				wrote, err := a.bw.Append(r.ctx, rows)
				if err != nil {
					return 0, err
				}
				if wrote != len(rows) {
					return 0, verifyErr("写入备份文件 %d 行 ≠ 读取 %d 行", wrote, len(rows))
				}
				for i := range b.Rows {
					a.fileDone[pkKeyOf(b.PKs[i])] = struct{}{}
				}
			}
		}
	}
	// ---- 删源：走到这里，备份一定已写入并校验通过 ----
	if isDeleting(spec.Mode) {
		var affected int64
		if err := r.retry("删除源表数据", func() error {
			var e error
			affected, e = r.env.Src.DeleteBatch(r.ctx, DeleteReq{DB: spec.DB, Table: spec.Table, Where: a.where, PKCols: a.pk, PKs: b.PKs, LockWaitSe: r.p.LockWaitSeconds})
			return e
		}); err != nil {
			return 0, err
		}
		deleted = affected
		if affected != int64(n) {
			r.emit("warn", fmt.Sprintf("删除行数 %d 与读取行数 %d 不一致（期间有行被修改 / 已被删除？）", affected, n),
				"deleted", strconv.FormatInt(affected, 10), "read", strconv.Itoa(n))
		}
		a.fileDone = map[string]struct{}{}
	}
	return deleted, nil
}

func (a *archiveRun) loop() {
	r := a.runner
	spec := r.spec
	cur := newAdaptor(r.p.BatchSize, r.p.SleepMs, time.Duration(r.p.TargetBatchMs)*time.Millisecond)
	var after []string // 仅按主键排序读取（仅备份，或主键不能在本地比大小）时使用
	if !a.unordered {
		after = copyStrings(spec.Checkpoint)
	}
	fails := 0
	r.prog.BatchSize = cur.batch
	for {
		// ---- 每批前 ----
		if r.waitWhilePaused() || r.stopped() {
			a.fail(ResultStopped, "", false)
			return
		}
		if !InWindow(r.now(), r.p.Window) {
			if a.unordered {
				r.emit("log", fmt.Sprintf("已超出执行窗口 %s–%s，优雅停止，下次接着取剩下的行", r.p.Window.Start, r.p.Window.End))
			} else {
				r.emit("log", fmt.Sprintf("已超出执行窗口 %s–%s，优雅停止，下次从断点继续", r.p.Window.Start, r.p.Window.End))
			}
			a.res.Result = ResultWindowEnd
			return
		}
		remaining := a.maxRows - a.res.Scanned
		if remaining <= 0 {
			r.emit("log", fmt.Sprintf("已达单次运行行数上限 %d", a.maxRows))
			a.res.Result = ResultLimitReached
			return
		}
		switch r.waitLoad() {
		case loadAbort:
			r.emit("log", fmt.Sprintf("负载持续超限超过 %d 秒，本次运行中断：%s", r.p.MaxWaitSeconds, r.prog.ThrottleReason))
			a.res.Result = ResultLoadAbort
			return
		case loadStopped:
			a.fail(ResultStopped, "", false)
			return
		}
		if fp, err := r.env.Src.Fingerprint(r.ctx, spec.DB, spec.Table); err != nil {
			if r.stopped() {
				a.fail(ResultStopped, "", false)
				return
			}
			if a.batchFailed(&fails, fmt.Errorf("核对表结构指纹失败：%w", err)) {
				return
			}
			continue
		} else if fp != a.baseF {
			r.emit("structure", "运行中源表结构发生变化，已停止")
			a.fail(ResultStructure, "运行中源表结构指纹发生变化（列名、类型或主键有变化），需要重新试运行", true)
			return
		}

		// ---- 一批 ----
		t0 := r.now()
		limit := cur.batch
		if int64(limit) > remaining {
			limit = int(remaining)
		}
		var b Batch
		err := r.retry("读取源表", func() error {
			var e error
			b, e = r.env.Src.ReadBatch(r.ctx, ReadReq{DB: spec.DB, Table: spec.Table, Where: a.where, PKCols: a.pk, After: after, Unordered: a.unordered, Limit: limit, LockWaitSe: r.p.LockWaitSeconds})
			return e
		})
		readDur := r.now().Sub(t0)
		var deleted int64
		if err == nil {
			if len(b.Rows) == 0 {
				r.emit("log", "没有更多符合条件的行")
				a.res.Result = ResultSuccess
				return
			}
			if a.slowRead(readDur) {
				return
			}
			if msg := a.unitGuard(b); msg != "" {
				// 整批时间值都小于该单位的合理下限：十有八九是把 10 位秒选成了 13 位毫秒（条件会让整张表都命中）。
				// 此时还没有写任何备份、也没有删任何行，直接停止并暂停任务。
				r.emit("warn", msg)
				a.fail(ResultFailed, msg, true)
				return
			}
			deleted, err = a.process(b)
		}
		if err != nil {
			if r.stopped() {
				a.fail(ResultStopped, "", false)
				return
			}
			if errors.Is(err, ErrMismatch) {
				r.emit("verify", fmt.Sprintf("备份校验不一致，该批未删除源数据，运行已停止：%s", err.Error()), "ok", "false")
				a.fail(ResultVerifyFailed, err.Error(), true)
				return
			}
			if a.batchFailed(&fails, err) {
				return
			}
			continue
		}
		fails = 0

		// ---- 批成功：记账、断点 ----
		n := len(b.Rows)
		if !a.unordered && pkKeyOf(b.PKs[n-1]) == pkKeyOf(after) {
			// 断点没有前进：说明数据源没有遵守“主键 > 断点”，继续下去会死循环
			r.fail(ResultFailed, "读取结果没有越过断点，疑似数据源返回了重复数据，已停止", true)
			return
		}
		a.res.Batches++
		a.res.Scanned += int64(n)
		if spec.Mode != ModePurge {
			a.res.Backed += int64(n)
		}
		a.res.Deleted += deleted
		if a.unordered {
			a.rng.add(b.PKs)
			a.res.MinPK, a.res.MaxPK = copyStrings(a.rng.min), copyStrings(a.rng.max)
		} else {
			after = copyStrings(b.PKs[n-1])
			a.res.Checkpoint = copyStrings(after)
			if a.minPK == nil {
				a.minPK = copyStrings(b.PKs[0])
			}
			a.res.MinPK, a.res.MaxPK = copyStrings(a.minPK), copyStrings(after)
		}
		elapsed := r.now().Sub(t0)

		changed, slow, msg := cur.observe(elapsed)
		if changed {
			if slow {
				a.res.ThrottleCount++
			}
			r.emit("throttle", msg, "kind", "adaptive", "batch_size", strconv.Itoa(cur.batch), "sleep_ms", strconv.Itoa(cur.sleep))
		}
		r.prog.BatchSize = cur.batch
		if !r.emittedAny || r.now().Sub(r.lastEmit) >= progressEvery {
			if a.unordered {
				r.emit("batch", fmt.Sprintf("第 %d 批：读 %d 行，删 %d 行，耗时 %d ms（读取 %d ms）", a.res.Batches, n, deleted, elapsed.Milliseconds(), readDur.Milliseconds()),
					"batch", strconv.Itoa(a.res.Batches), "rows", strconv.Itoa(n), "deleted", strconv.FormatInt(deleted, 10), "ms", strconv.FormatInt(elapsed.Milliseconds(), 10),
					"read_ms", strconv.FormatInt(readDur.Milliseconds(), 10), "scanned", strconv.FormatInt(a.res.Scanned, 10))
			} else {
				r.emit("batch", fmt.Sprintf("第 %d 批：%d 行，耗时 %d ms，断点 %s", a.res.Batches, n, elapsed.Milliseconds(), strings.Join(after, ",")),
					"batch", strconv.Itoa(a.res.Batches), "rows", strconv.Itoa(n), "ms", strconv.FormatInt(elapsed.Milliseconds(), 10),
					"checkpoint", strings.Join(after, ","), "scanned", strconv.FormatInt(a.res.Scanned, 10))
			}
		}
		r.emitProgress(false)

		// 空转保护：没有断点可以“越过”卡住的行，读到行却一行都删不掉就会原地打转，必须停下来
		if a.unordered && deleted == 0 {
			a.fail(ResultFailed, fmt.Sprintf("本批读到 %d 行，但一行也没有删掉（%s 的删除没有生效），已停止以免原地空转；请先排查（触发器、权限、行被并发修改），再恢复任务", n, qualified(spec.DB, spec.Table)), true)
			return
		}

		if n < limit { // 读到的比要求的少：没有更多了
			r.emit("log", "没有更多符合条件的行")
			a.res.Result = ResultSuccess
			return
		}
		if cur.sleep > 0 {
			if err := r.wsleep(time.Duration(cur.sleep) * time.Millisecond); err != nil && r.stopped() {
				a.fail(ResultStopped, "", false)
				return
			}
		}
	}
}

// slowRead 登记一次读取的耗时：连续 slowReadLimit 次超过 slowReadWarn 就停止并自动暂停。返回 true 表示已停止。
// 不排序读取每批都从索引头开始，清理落后（有长事务）或执行计划变了会越读越慢；这里不让它一直拖着线上。
func (a *archiveRun) slowRead(d time.Duration) bool {
	r := a.runner
	if d < slowReadWarn {
		a.slowReads = 0
		return false
	}
	a.slowReads++
	r.emit("warn", fmt.Sprintf("读取一批耗时 %d 秒（超过 %d 秒，连续第 %d/%d 次）：可能是执行计划变了，或已删除的记录还没被清理", int(d.Seconds()), int(slowReadWarn.Seconds()), a.slowReads, slowReadLimit),
		"read_ms", strconv.FormatInt(d.Milliseconds(), 10), "slow", strconv.Itoa(a.slowReads))
	if a.slowReads < slowReadLimit {
		return false
	}
	a.fail(ResultFailed, fmt.Sprintf("连续 %d 次读取一批都超过 %d 秒（最近一次 %d 秒），已停止并暂停任务；请检查时间列索引与执行计划、是否有长事务拖慢清理，处理后再恢复任务", slowReadLimit, int(slowReadWarn.Seconds()), int(d.Seconds())), true)
	return true
}

// unitGuard 是整型时间戳列的运行期兜底：读到的这一批里时间列的最大值仍小于所选单位的合理下限（1980 年），
// 说明单位选错了（例如列里是 10 位秒、任务却按 13 位毫秒算）。返回非空表示应当停止，内容是给人看的原因。
func (a *archiveRun) unitGuard(b Batch) string {
	spec := a.runner.spec
	unit, _ := NormalizeTimeUnit(spec.TimeUnit)
	if !IsEpochUnit(unit) {
		return ""
	}
	mx, ok := batchTimeMax(b, spec.TimeColumn)
	if !ok {
		return ""
	}
	if lo, _ := EpochRange(unit); mx < lo {
		return fmt.Sprintf("时间单位疑似选错：本批读到的时间列 %s 最大值是 %d，小于 %d 位 Unix 时间戳的合理下限 %d（1980 年）。已停止，没有备份也没有删除任何数据；请确认列里存的是秒还是毫秒，修改后重新试运行",
			spec.TimeColumn, mx, EpochDigits(unit), lo)
	}
	return ""
}

// finish 收尾：上传文件备份、生成清单、填结果。即使运行被终止 / 失败也要把已删源行对应的备份文件上传。
func (a *archiveRun) finish() {
	r := a.runner
	if a.bw != nil {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 30*time.Minute)
		m, err := a.bw.Finish(fctx, a.info, a.res.MinPK, a.res.MaxPK, *r.spec.File)
		cancel()
		if err != nil {
			msg := fmt.Sprintf("文件备份收尾失败：%s", err.Error())
			if lo := a.bw.LocalLeftovers(); len(lo) > 0 {
				msg = fmt.Sprintf("文件备份收尾失败：%s（本地文件：%s）", err.Error(), strings.Join(lo, "、"))
			}
			r.emit("file", msg, "ok", "false")
			a.res.Result = ResultFailed // 源行可能已经删了而备份没落地：无论之前是什么结果，都按失败处理
			if a.res.Err == "" {
				a.res.Err = msg
			} else {
				a.res.Err += "；" + msg
			}
			a.res.AutoPause = true
		} else if m != nil {
			a.res.Manifest = m
			r.emit("file", fmt.Sprintf("文件备份完成：%d 个文件，%d 行，清单 %s", len(m.Files), m.Rows, m.ManifestKey),
				"ok", "true", "manifest", m.ManifestKey, "files", strconv.Itoa(len(m.Files)))
		}
	}
	if a.res.Result == "" {
		a.res.Result = ResultFailed
	}
	if a.res.Result == ResultSuccess && isDeleting(r.spec.Mode) {
		a.res.Checkpoint = nil // 已处理完：下次从头开始，才能捡到后来才过期的小主键行
	}
	a.res.Duration = r.now().Sub(r.start)
	a.res.PeakThreads = r.maxThreads
	r.emit("checkpoint", fmt.Sprintf("运行结束：%s，扫描 %d 行，备份 %d 行，删除 %d 行，%d 批", a.res.Result, a.res.Scanned, a.res.Backed, a.res.Deleted, a.res.Batches),
		"result", a.res.Result, "checkpoint", strings.Join(a.res.Checkpoint, ","))
	r.prog.Throttled, r.prog.ThrottleReason = false, ""
	r.emitProgress(true)
}
