// @author: kevinkelly
// @license: Apache Licence
// @file: check_targets.go
// @time: 2026/10/28

package engine

import (
	"context"
	"fmt"
	"strings"
)

type addFn func(key, title, level, format string, a ...any)

// checkDest 检查库表备份去向：目标库是否存在、目标表是否存在 / 结构是否兼容、目标端磁盘。只读，不建库不建表。
func (e *Engine) checkDest(ctx context.Context, env *Env, spec Spec, p Params, src TableInfo, add addFn) {
	d := spec.Dest
	if env.Dst == nil {
		add("dest_table", "目标 · 归档库表", "error", "没有目标库连接（实例 #%d 不可用？）", d.InstanceID)
		return
	}
	tbl := d.Table
	if tbl == "" {
		tbl = expandTableName(d.Template, spec.Table, env.nowOr())
	}
	ok, err := env.Dst.DBExists(ctx, d.DB)
	switch {
	case err != nil:
		add("dest_table", "目标 · 归档库表", "error", "读取目标库失败：%v", err)
		return
	case !ok:
		add("dest_table", "目标 · 归档库表", "error", "目标库 %s 不存在，请先建库", d.DB)
		return
	}
	dst, err := env.Dst.Describe(ctx, d.DB, tbl)
	switch {
	case err != nil:
		add("dest_table", "目标 · 归档库表", "error", "读取目标表失败：%v", err)
	case dst.IsView:
		add("dest_table", "目标 · 归档库表", "error", "目标 %s.%s 是视图", d.DB, tbl)
	case !dst.Exists:
		add("dest_table", "目标 · 归档库表", "ok", "目标表 %s.%s 不存在，运行时自动按源表结构创建", d.DB, tbl)
	default:
		if cerr := compatErr(src, dst); cerr != nil {
			add("dest_table", "目标 · 归档库表", "error", "目标表 %s.%s 已存在，但%v", d.DB, tbl, cerr)
		} else {
			add("dest_table", "目标 · 归档库表", "ok", "目标表 %s.%s 已存在且结构兼容；主键冲突视为“已存在”，比对一致才算成功", d.DB, tbl)
		}
	}
	pct, okd, derr := env.Dst.DiskFreePct(ctx)
	switch {
	case derr != nil || !okd:
		add("dest_disk", "目标 · 磁盘", "warn", "目标端磁盘剩余未检测（运行时会在运行记录里标注“未检测”）")
	case pct < float64(p.MinDiskFreePct):
		add("dest_disk", "目标 · 磁盘", "error", "目标端磁盘剩余 %.1f%% 低于阈值", pct)
	default:
		add("dest_disk", "目标 · 磁盘", "ok", "目标端磁盘剩余 %.1f%%", pct)
	}
}

// checkFile 检查文件备份去向：存储连通性（写入 / 读取 / 删除一个探测对象，不碰业务数据）和可用空间。
func (e *Engine) checkFile(ctx context.Context, env *Env, spec Spec, rep *CheckReport, rowBytes int64, add addFn) {
	f := spec.File
	if env.Files == nil {
		add("dest_store", "目标 · 文件存储", "error", "备份存储 #%d 不可用（未配置或已停用）", f.StoreID)
		return
	}
	if f.Encrypt && len(env.FileKey) != 32 {
		add("dest_store", "目标 · 文件存储", "error", "要求加密，但系统没有可用的 32 字节密钥")
		return
	}
	dur, err := env.Files.Test(ctx)
	if err != nil {
		add("dest_store", "目标 · 文件存储", "error", "连通性测试失败：%v", err)
		return
	}
	need := rep.EstRows * rowBytes
	if f.Gzip {
		need /= 3
	}
	free, ok, ferr := env.Files.Free(ctx)
	switch {
	case ferr != nil || !ok:
		add("dest_store", "目标 · 文件存储", "ok", "连通性正常（探测耗时 %d ms），无法得知可用空间（对象存储）", dur.Milliseconds())
	case need > 0 && free < need:
		add("dest_store", "目标 · 文件存储", "error", "可用空间 %d MB 小于预计备份体积约 %d MB", free>>20, need>>20)
	default:
		add("dest_store", "目标 · 文件存储", "ok", "连通性正常（探测耗时 %d ms），可用空间 %d MB，预计备份体积约 %d MB", dur.Milliseconds(), free>>20, need>>20)
	}
}

// checkLoad 做一次负载探测（只读），并检查“关闭复制延迟检查但有从库”的非法组合。
func (e *Engine) checkLoad(ctx context.Context, env *Env, p Params, spec Spec, add addFn) {
	if env.Probe == nil {
		add("load", "负载探针", "warn", "没有负载探针，运行时的负载自检全部标“未检测”")
		return
	}
	var parts []string
	level := "ok"
	if n, err := env.Probe.ThreadsRunning(ctx); err != nil {
		parts = append(parts, "Threads_running 未检测")
		level = "warn"
	} else {
		parts = append(parts, fmt.Sprintf("Threads_running=%d（阈值 %d）", n, p.MaxThreadsRunning))
		if n > p.MaxThreadsRunning {
			level = "warn"
		}
	}
	sec, has, err := env.Probe.ReplicaLag(ctx)
	switch {
	case err != nil:
		parts = append(parts, "复制延迟未检测")
		level = "warn"
		if p.MaxLagSeconds < 0 {
			add("replica", "从库复制延迟", "warn", "复制延迟检查已关闭，且无法确认源实例是否有从库：%v", err)
		}
	case has && p.MaxLagSeconds < 0:
		add("replica", "从库复制延迟", "error", "源实例有从库，不允许关闭复制延迟检查")
	case has:
		parts = append(parts, fmt.Sprintf("复制延迟 %d 秒（阈值 %d 秒）", sec, p.MaxLagSeconds))
		if sec > p.MaxLagSeconds {
			level = "warn"
		}
		add("replica", "从库复制延迟", "ok", "检测到从库，每批前检查复制延迟 ≤ %d 秒", p.MaxLagSeconds)
	default:
		add("replica", "从库复制延迟", "ok", "没有从库，复制延迟检查自动跳过")
	}
	if pct, ok, derr := env.Probe.DiskFreePct(ctx); derr != nil || !ok {
		parts = append(parts, "源库磁盘剩余未检测")
		level = "warn"
	} else {
		parts = append(parts, fmt.Sprintf("源库磁盘剩余 %.1f%%（阈值 %d%%）", pct, p.MinDiskFreePct))
		if pct < float64(p.MinDiskFreePct) {
			level = "warn"
		}
	}
	add("load", "当前负载", level, "%s", strings.Join(parts, "；"))
}
