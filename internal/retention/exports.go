// @author: kevinkelly
// @license: Apache Licence
// @file: exports.go
// @time: 2026/10/28

package retention

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	exportpkg "dbtool/internal/export"
	"dbtool/internal/settings"
)

// exportDir 解析导出文件目录，规则与 internal/sqlflow/export.go 的 storage() 一致：
// export.storage_type 只能是空或 local；目录取 export.local_path（缺省 downloads/DataExportFile/），相对路径相对于 EXPORT_BASE_DIR。
// 与 sqlflow 不同的是这里不创建目录（清理不应凭空建目录）。
func (s *Service) exportDir() (string, error) {
	if t := cfgTrim(s.repo, settings.KeyExportStorageType); t != "" && t != "local" {
		return "", fmt.Errorf("导出文件存储类型是 %s，只支持清理本地存储", t)
	}
	dir := cfgTrim(s.repo, settings.KeyExportLocalPath)
	if dir == "" {
		dir = "downloads/DataExportFile/"
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(s.exportBase, dir)
	}
	return dir, nil
}

// purgeExportFile 删除导出目录里工单记录的那个文件。名字必须是纯文件名（不含路径），只删普通文件；
// 文件的修改时间晚于 cutoff（例如批量导出重试后又写过）就不删，返回 skipped=true。
func purgeExportFile(dir, name string, cutoff time.Time, dryRun bool) (size int64, existed, skipped bool, err error) {
	if err := exportpkg.CheckName(name); err != nil {
		return 0, false, false, fmt.Errorf("文件名不合法：%q", name)
	}
	p := filepath.Join(dir, name)
	fi, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, err
	}
	if !fi.Mode().IsRegular() {
		return 0, true, false, fmt.Errorf("%q 不是普通文件（目录或符号链接），拒绝删除", name)
	}
	if fi.ModTime().After(cutoff) {
		return fi.Size(), true, true, nil
	}
	if dryRun {
		return fi.Size(), true, false, nil
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return 0, true, false, err
	}
	return fi.Size(), true, false, nil
}

// purgeStaleUploads 清理导出目录里被杀进程遗留的 .upload-* 临时文件（超过 24 小时）。返回个数与字节数。
func purgeStaleUploads(dir string, now time.Time, dryRun bool) (n int, bytes int64, err error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".upload-") || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) < stalePartAge {
			continue
		}
		if !dryRun {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
				return n, bytes, err
			}
		}
		n++
		bytes += fi.Size()
	}
	return n, bytes, nil
}

// runExports 清理已结束且过了保留期的导出工单的文件。工单记录保留；文件删除后在 dbt_export_purged 里写“已清理”标记
// （页面据此提示“文件已按保留期清理”）。没有标记表就不删：宁可不清理，也不留下说不清的“文件不存在”。
func (s *Service) runExports(ctx context.Context, t Target, set Settings, halt func() string) TargetResult {
	return s.exports(ctx, t, set, halt, false)
}

// exports 是导出文件清理的实现；dry=true 只统计（不删文件、不写标记、不休眠），最多看 previewCap 个。
func (s *Service) exports(ctx context.Context, t Target, set Settings, halt func() string, dry bool) TargetResult {
	res := TargetResult{Key: t.Key, Label: t.Label, Status: StatusSkipped}
	days := set.Days[t.Key]
	if days <= 0 {
		return res
	}
	res.Days = days
	ready, err := s.repo.ExportMarkerReady()
	if err != nil {
		res.Status, res.Err = StatusError, fmt.Sprintf("检查 dbt_export_purged 失败：%s", err.Error())
		return res
	}
	if !ready {
		res.Note = "dbt_export_purged 不存在，已跳过（先执行 deploy/sql/205_retention.sql）"
		return res
	}
	dir, err := s.exportDir()
	if err != nil {
		res.Note = fmt.Sprintf("%s，已跳过", err.Error())
		return res
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		res.Note = fmt.Sprintf("导出目录 %s 不存在，已跳过", dir)
		return res
	}
	cut := s.cutoff(days)
	res.Cutoff = cut.Format(timeLayout)
	res.Status = StatusOK
	var after uint
	var skippedNewer int
	for {
		if reason := halt(); reason != "" {
			res.Status, res.Note = StatusPartial, reason
			return res
		}
		if res.Deleted >= set.MaxRows {
			res.Status, res.Note = StatusPartial, fmt.Sprintf("已达到每轮上限 %d 个文件，剩余部分下一轮继续", set.MaxRows)
			return res
		}
		page, err := s.repo.ExpiredExports(ctx, cut, after, pageSize)
		if err != nil {
			res.Status, res.Err = StatusError, err.Error()
			return res
		}
		for _, f := range page {
			after = f.WorkflowID
			size, existed, skipped, err := purgeExportFile(dir, f.FileName, cut, dry)
			if err != nil {
				// 单个文件出错（名字非法、不是普通文件、权限）：停止该目标，其余留到修好之后。
				res.Status, res.Err = StatusError, fmt.Sprintf("工单 #%d：%v", f.WorkflowID, err)
				return res
			}
			if skipped {
				skippedNewer++
				continue
			}
			if !dry {
				p := ExportPurge{WorkflowID: f.WorkflowID, FileName: f.FileName, SizeBytes: size, Existed: existed, PurgedAt: s.now()}
				if err := s.repo.MarkExportPurged(ctx, p); err != nil {
					res.Status, res.Err = StatusError, fmt.Sprintf("工单 #%d 的文件已删除但写清理标记失败：%v", f.WorkflowID, err)
					return res
				}
			}
			if existed {
				res.Deleted++
				res.Bytes += size
			}
		}
		if len(page) < pageSize {
			break
		}
		if dry {
			if res.Deleted >= previewCap {
				res.Status, res.Note = StatusPartial, fmt.Sprintf("预览最多统计 %d 个文件，实际更多", previewCap)
				return res
			}
			continue
		}
		s.sleep(ctx, set.BatchSleep)
	}
	if n, b, err := purgeStaleUploads(dir, s.now(), dry); err != nil {
		slog.Warn("数据清理：清理导出目录里的 .upload-* 临时文件失败", "dir", dir, "err", err)
	} else if n > 0 {
		res.Deleted += int64(n)
		res.Bytes += b
		res.Note = fmt.Sprintf("含 %d 个超过 24 小时的 .upload-* 残留临时文件", n)
	}
	if skippedNewer > 0 {
		res.Note = strings.TrimSpace(res.Note + fmt.Sprintf(" 有 %d 个文件修改时间晚于截止时间（重试写过），本轮保留", skippedNewer))
	}
	return res
}
