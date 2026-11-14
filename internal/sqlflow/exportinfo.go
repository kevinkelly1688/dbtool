// @author: kevinkelly
// @license: Apache Licence
// @file: exportinfo.go
// @time: 2026/10/28

package sqlflow

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"dbtool/internal/audit"
	exportpkg "dbtool/internal/export"
	"dbtool/internal/model"
)

// ---- 导出结果文件卡片 ----

// ExportFileInfo 是详情页“结果文件”卡片：大小、生成时间、下载次数、是否已被清理。
type ExportFileInfo struct {
	Name        string
	Size        int64
	SizeText    string
	GeneratedAt *time.Time
	Downloads   int        // 成功下载的次数；仓库不支持统计时为 -1
	Cleaned     bool       // 文件已不在存储里（被清理或手工删除），无法再下载
	PurgedAt    *time.Time // 文件是被数据保留策略清理的，及清理时间（Cleaned 时才可能有；手工删除的没有）
	Unknown     bool       // 存储暂时读不到（配置错误等），无法确认文件状态
}

// HumanSize 把字节数写成 1.4 MB 这样的文本。
func HumanSize(n int64) string {
	const k = 1024.0
	f := float64(n)
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case f < k*k:
		return fmt.Sprintf("%.1f KB", f/k)
	case f < k*k*k:
		return fmt.Sprintf("%.1f MB", f/(k*k))
	}
	return fmt.Sprintf("%.2f GB", f/(k*k*k))
}

// ExportPurgedLookup 查询某导出工单的文件是否已被数据保留策略清理，以及清理时间。
// retention 包依赖 sqlflow（经 server），所以这里只定义钩子，由 retention 在拿到元数据库后通过 SetExportPurgedLookup 注入；没注入时一律视为“不是被清理的”。
type ExportPurgedLookup func(workflowID uint) (at time.Time, ok bool)

var (
	purgedMu     sync.RWMutex
	purgedLookup ExportPurgedLookup
)

// SetExportPurgedLookup 注入“导出文件是否已按保留策略清理”的查询（传 nil 取消）。
func SetExportPurgedLookup(fn ExportPurgedLookup) {
	purgedMu.Lock()
	purgedLookup = fn
	purgedMu.Unlock()
}

// exportPurgedAt 返回导出文件被保留策略清理的时间；查询出错（panic）当作没有。
func exportPurgedAt(workflowID uint) (at time.Time, ok bool) {
	purgedMu.RLock()
	fn := purgedLookup
	purgedMu.RUnlock()
	if fn == nil {
		return time.Time{}, false
	}
	defer func() {
		if r := recover(); r != nil {
			at, ok = time.Time{}, false
		}
	}()
	return fn(workflowID)
}

// purgedMessage 是文件已被清理时给用户的说明（下载接口与详情页共用同一句话）。
func purgedMessage(at time.Time) string {
	return fmt.Sprintf("文件已于 %s 按保留策略清理，请重新提交导出", at.Format("2006-01-02 15:04:05"))
}

func (s *Service) exportFileInfo(w *model.SqlWorkflow, name string) *ExportFileInfo {
	info := &ExportFileInfo{Name: name, GeneratedAt: w.FinishTime, Downloads: -1}
	if dc, ok := s.repo.(DownloadCounter); ok {
		if n, err := dc.DownloadCount(w.ID); err == nil {
			info.Downloads = n
		}
	}
	st, err := s.storage()
	if err != nil {
		info.Unknown = true
		return info
	}
	f, size, err := st.Open(name)
	switch {
	case errors.Is(err, exportpkg.ErrNotFound):
		info.Cleaned = true
		if at, ok := exportPurgedAt(w.ID); ok {
			info.PurgedAt = &at
		}
	case err != nil:
		info.Unknown = true
	default:
		f.Close()
		info.Size, info.SizeText = size, HumanSize(size)
	}
	return info
}

// canSubmitKind 能否提交这一类工单（复制为新工单时决定按钮是否出现）。
func (s *Service) canSubmitKind(pm perms, export, batch bool) bool {
	if export {
		return pm.exportSubmit || pm.exportBatch
	}
	return pm.submit || pm.batchSubmit
}

// ---- 执行 / 导出进度 ----

// runProgress 记录一张工单后台执行的进度：批量工单按实例计数。进度只存在内存里（本进程），
// 多进程部署时其他进程看不到，页面会退化成“已执行多久”。
type runProgress struct {
	start time.Time
	total atomic.Int32
	done  atomic.Int32
}

func (s *Service) progBegin(id uint) *runProgress {
	p := &runProgress{start: s.now()}
	s.prog.Store(id, p)
	return p
}

func (s *Service) progEnd(id uint) { s.prog.Delete(id) }

func (s *Service) progTotal(id uint, n int) {
	if v, ok := s.prog.Load(id); ok {
		v.(*runProgress).total.Store(int32(n))
	}
}

func (s *Service) progDone(id uint) {
	if v, ok := s.prog.Load(id); ok {
		v.(*runProgress).done.Add(1)
	}
}

// Progress 是执行 / 导出进度，供详情页的进度条轮询。
type Progress struct {
	Status     string     `json:"status"`
	Label      string     `json:"status_label"`
	Active     bool       `json:"active"` // 还在排队或执行
	StartedAt  *time.Time `json:"started_at,omitempty"`
	ElapsedSec int        `json:"elapsed_sec"`           // 已执行多久
	Total      int        `json:"total"`                 // 批量工单的实例数；单实例为 1
	Done       int        `json:"done"`                  // 已处理完的实例数
	Percent    int        `json:"percent"`               // 0-100；-1 表示无法估算（单条语句 / 单个导出只能显示已用时间）
	TimeoutSec int        `json:"timeout_sec,omitempty"` // 导出：单次查询最长秒数
	File       string     `json:"file_name,omitempty"`
}

// Progress 返回工单当前的执行进度。看不到的工单按不存在处理。
func (s *Service) Progress(p audit.Actor, id uint) (*Progress, error) {
	w, err := s.load(id, false)
	if err != nil {
		return nil, err
	}
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	if !pm.canView(p, w) {
		return nil, notFound("工单不存在")
	}
	out := &Progress{Status: w.Status, Label: StatusLabel(w.Status), Total: 1, Percent: -1,
		Active: w.Status == model.WfQueuing || w.Status == model.WfExecuting}
	if ids := parseBatchIDs(w.BatchInstanceIDs); len(ids) > 0 {
		out.Total = len(ids)
	}
	if w.IsOfflineExport != 0 {
		_, out.TimeoutSec = s.exportCaps()
	}
	switch {
	case out.Active:
		if v, ok := s.prog.Load(id); ok {
			rp := v.(*runProgress)
			t := rp.start
			out.StartedAt = &t
			if n := int(rp.total.Load()); n > 0 {
				out.Total = n
			}
			out.Done = int(rp.done.Load())
			if out.Total > 1 || w.IsBatch() {
				out.Percent = out.Done * 100 / out.Total
				if out.Percent > 99 {
					out.Percent = 99 // 汇总、打包还没结束
				}
			}
		} else if t := s.startedAtFromLog(id); t != nil {
			out.StartedAt = t // 别的进程在执行：只知道开始时间，没有逐实例进度
		}
	case w.Status == model.WfFinish || w.Status == model.WfException:
		out.Done, out.Percent = out.Total, 100
		if w.FileName != nil {
			out.File = *w.FileName
		}
	}
	if out.StartedAt != nil && out.Active {
		if d := s.now().Sub(*out.StartedAt); d > 0 {
			out.ElapsedSec = int(d.Seconds())
		}
	}
	return out, nil
}

// startedAtFromLog 没有内存进度时（别的进程在执行），用最近一条“执行工单”日志的时间当开始时间。
func (s *Service) startedAtFromLog(id uint) *time.Time {
	au, err := s.repo.AuditByWorkflow(audit.TypeSQLReview, id, false)
	if err != nil || au == nil {
		return nil
	}
	logs, err := s.repo.AuditLogs(au.AuditID)
	if err != nil {
		return nil
	}
	var at *time.Time
	for i := range logs {
		if logs[i].OperationType == opExecute {
			t := logs[i].OperationTime
			at = &t
		}
	}
	return at
}
