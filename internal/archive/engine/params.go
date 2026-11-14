// @author: kevinkelly
// @license: Apache Licence
// @file: params.go
// @time: 2026/10/28

package engine

import "time"

// 参数范围与内置默认值（设计 §3）。
const (
	minBatch, maxBatch = 100, 10000
	maxSleepMs         = 5000
	minDeleteSleepMs   = 50 // move / purge 的批间休眠下限
	minRows            = 100
	minThreads         = 4
	maxThreads         = 1000
	maxLagLimit        = 3600
	minDisk, maxDisk   = 5, 50
	minTargetMs        = 100
	maxTargetMs        = 5000
	minWaitSec         = 1
	maxWaitSec         = 3600
	maxFailLimit       = 10
	maxLockWait        = 60
	maxRetentionDays   = 3650
	minWindowMinutes   = 30
	maxExtraLen        = 500
	minPartMB          = 1
	maxPartMB          = 4096
	nameMin, nameMax   = 2, 50

	defBatch          = 1000
	defSleepMs        = 200
	defMaxRows  int64 = 5_000_000
	defThreads        = 32
	defLag            = 5
	defDisk           = 15
	defTargetMs       = 500
	defWaitSec        = 600
	defFails          = 3
	defLockWait       = 3
	defPartMB         = 256

	loadRecheck    = 30 * time.Second // 负载超限后的复查间隔
	diskCacheTTL   = 30 * time.Second // 磁盘剩余的缓存时间（变化慢，不必每批都查）
	okStreakToGrow = 5                // 连续多少批正常才恢复批量
	retryAttempts  = 3                // 死锁 / 锁等待的重试次数（不含首次）
	retryBase      = 200 * time.Millisecond
	failBackoff    = time.Second // 一批失败后到下一次尝试的基础等待
	progressEvery  = time.Second // 进度 / 批事件的最小间隔

	// 读取一批的耗时保护：正常读一批（默认 1000 行）是毫秒到秒级。不排序读取没有断点可以跳过“刚删掉、InnoDB 还没清理”的记录，
	// 清理落后（有长事务）或执行计划变了，读取会越来越慢；连续几次读取都过慢就停下来、自动暂停任务，别再拖着线上。
	slowReadWarn  = 15 * time.Second // 一次读取超过它算“过慢”（试运行里实测首批读取超过它判红）
	slowReadLimit = 3                // 连续这么多次过慢就停止并自动暂停
	firstReadWarn = 3 * time.Second  // 试运行实测首批读取超过它给黄色提示
	probeReadRows = 200              // 试运行实测首批读取最多取多少行（只为看计划和耗时，不必取满一批，也避免大字段表占内存）
)

// EffectiveParams 合并参数：Spec 里非零的字段优先，其次 def（Policy.Defaults），最后是内置默认值。
// MaxLagSeconds 为负数表示“关闭复制延迟检查”（有从库时引擎会拒绝运行）。
func EffectiveParams(p, def Params) Params {
	pi := func(v, d, b int) int {
		if v != 0 {
			return v
		}
		if d != 0 {
			return d
		}
		return b
	}
	out := Params{
		BatchSize:              pi(p.BatchSize, def.BatchSize, defBatch),
		SleepMs:                p.SleepMs,
		MaxThreadsRunning:      pi(p.MaxThreadsRunning, def.MaxThreadsRunning, defThreads),
		MaxLagSeconds:          pi(p.MaxLagSeconds, def.MaxLagSeconds, defLag),
		MinDiskFreePct:         pi(p.MinDiskFreePct, def.MinDiskFreePct, defDisk),
		TargetBatchMs:          pi(p.TargetBatchMs, def.TargetBatchMs, defTargetMs),
		MaxWaitSeconds:         pi(p.MaxWaitSeconds, def.MaxWaitSeconds, defWaitSec),
		MaxConsecutiveFailures: pi(p.MaxConsecutiveFailures, def.MaxConsecutiveFailures, defFails),
		LockWaitSeconds:        pi(p.LockWaitSeconds, def.LockWaitSeconds, defLockWait),
		Window:                 p.Window,
	}
	// 零值表示“用默认”，所以休眠 0 ms 无法表达：Spec 与 Defaults 都是 0 时取内置默认 200 ms。
	if out.SleepMs == 0 {
		out.SleepMs = def.SleepMs
	}
	if out.SleepMs == 0 {
		out.SleepMs = defSleepMs
	}
	out.MaxRows = p.MaxRows
	if out.MaxRows == 0 {
		out.MaxRows = def.MaxRows
	}
	if out.MaxRows == 0 {
		out.MaxRows = defMaxRows
	}
	if out.Window.Start == "" && out.Window.End == "" {
		out.Window = def.Window
	}
	return out
}

// EffectiveMaxRows 是单次运行实际的行数上限：min(Params.MaxRows, Policy.MaxRowsPerRun)（后者 <=0 表示不限）。
func EffectiveMaxRows(p Params, pol Policy) int64 {
	m := EffectiveParams(p, pol.Defaults).MaxRows
	if pol.MaxRowsPerRun > 0 && pol.MaxRowsPerRun < m {
		m = pol.MaxRowsPerRun
	}
	return m
}

func effPartMB(f *FileDest) int {
	if f == nil || f.PartMB <= 0 {
		return defPartMB
	}
	return f.PartMB
}
