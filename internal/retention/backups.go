// @author: kevinkelly
// @license: Apache Licence
// @file: backups.go
// @time: 2026/10/28

package retention

import (
	"context"
	"fmt"
	"strings"

	"dbtool/internal/archive/engine"
)

// validKey 校验备份对象 Key：非空、相对路径、不含 .. 段、不含 NUL 与盘符。清单里的 Key 来自运行记录（数据库），
// 属于不可信输入，删除前必须确认它不会指向备份根目录之外。
func validKey(key string) bool {
	if key == "" || len(key) > 1024 || strings.ContainsRune(key, 0) {
		return false
	}
	if strings.HasPrefix(key, "/") || strings.HasPrefix(key, `\`) || (len(key) >= 2 && key[1] == ':') {
		return false
	}
	for _, seg := range strings.FieldsFunc(key, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return false
		}
	}
	return true
}

// runBackups 按备份存储的“保留天数”删除超期的归档文件备份：
//   - 只删运行记录里 manifest 记录的对象（各数据文件 + 清单自身），不扫描存储目录；
//   - 存储的 RetentionDays=0 永不清理（这类存储不参与）；库表备份不动；
//   - 对象全部删除后，在运行记录的 note 里标记“备份已过期清理”，运行记录保留（之后“归档运行记录”目标才允许删它）；
//   - 需要 retention.archive_backup_enabled=true（总开关之外的第二道开关）。
func (s *Service) runBackups(ctx context.Context, t Target, set Settings, halt func() string) TargetResult {
	return s.backups(ctx, t, set, halt, false)
}

// backups 是归档文件备份清理的实现；dry=true 只统计（不连存储、不删对象、不写标记），最多看 previewCap 个运行，且不要求 retention.archive_backup_enabled。
func (s *Service) backups(ctx context.Context, t Target, set Settings, halt func() string, dry bool) TargetResult {
	res := TargetResult{Key: t.Key, Label: t.Label, Status: StatusSkipped}
	if !dry && !set.BackupEnabled {
		return res
	}
	ok, err := s.tablesOK(map[string][]string{
		"dbt_archive_run":   {"id", "manifest", "backups", "note", "started_at", "finished_at", "result", "trigger_kind"},
		"dbt_archive_store": {"id", "retention_days"},
	})
	if err != nil {
		res.Status, res.Err = StatusError, fmt.Sprintf("检查归档表失败：%s", err.Error())
		return res
	}
	if !ok {
		res.Note = "归档运行 / 备份存储表不存在或缺列，已跳过"
		return res
	}
	stores, err := s.repo.BackupStores(ctx)
	if err != nil {
		res.Status, res.Err = StatusError, fmt.Sprintf("读取备份存储失败：%s", err.Error())
		return res
	}
	if len(stores) == 0 {
		res.Status, res.Note = StatusOK, "没有设置了保留天数（> 0）的备份存储"
		return res
	}
	byID := make(map[uint]BackupStore, len(stores))
	minDays := stores[0].RetentionDays
	for _, st := range stores {
		byID[st.ID] = st
		if st.RetentionDays < minDays {
			minDays = st.RetentionDays
		}
	}
	res.Status = StatusOK
	res.Days = minDays
	res.Cutoff = s.cutoff(minDays).Format(timeLayout)
	fss := map[uint]engine.FileStore{}
	var after uint
	var runs, unresolved, badKeys int
	for {
		if reason := halt(); reason != "" {
			res.Status, res.Note = StatusPartial, reason
			return res
		}
		if res.Deleted >= set.MaxRows {
			res.Status, res.Note = StatusPartial, fmt.Sprintf("已达到每轮上限 %d 个对象，剩余部分下一轮继续", set.MaxRows)
			return res
		}
		page, err := s.repo.BackupRuns(ctx, s.cutoff(minDays), after, pageSize)
		if err != nil {
			res.Status, res.Err = StatusError, err.Error()
			return res
		}
		for _, r := range page {
			after = r.ID
			st, ok := byID[r.StoreID]
			if r.StoreID == 0 {
				unresolved++ // 运行记录里没有存储 ID（旧记录）：无法确定该删哪个存储，保守不删
				continue
			}
			if !ok || len(r.Keys) == 0 || r.RefTime.After(s.cutoff(st.RetentionDays)) {
				continue // 存储不参与（保留天数 0 / 已删除）、没有清单、或还没到期
			}
			if !keysValid(r.Keys) {
				badKeys++
				continue
			}
			if dry {
				res.Deleted += int64(len(r.Keys))
				res.Bytes += r.Bytes
				runs++
				continue
			}
			fs, ok := fss[st.ID]
			if !ok {
				fs, err = s.openStore(st.Config)
				if err != nil {
					res.Status, res.Err = StatusError, fmt.Sprintf("打开备份存储「%s」失败：%v", st.Name, err)
					return res
				}
				fss[st.ID] = fs
			}
			for _, k := range r.Keys {
				if err := fs.Delete(ctx, k); err != nil {
					res.Status, res.Err = StatusError, fmt.Sprintf("运行 #%d 删除备份对象失败（存储「%s」）：%v", r.ID, st.Name, err)
					return res
				}
				res.Deleted++
			}
			res.Bytes += r.Bytes
			note := fmt.Sprintf("%s（%s，存储「%s」保留 %d 天，删除 %d 个对象）", BackupPurgedPrefix, s.now().Format("2006-01-02"), st.Name, st.RetentionDays, len(r.Keys))
			if err := s.repo.MarkBackupPurged(ctx, r.ID, note); err != nil {
				res.Status, res.Err = StatusError, fmt.Sprintf("运行 #%d 的备份对象已删除但写“备份已过期清理”标记失败：%v", r.ID, err)
				return res
			}
			runs++
		}
		if len(page) < pageSize {
			break
		}
		if dry {
			if runs >= previewCap {
				res.Status, res.Note = StatusPartial, fmt.Sprintf("预览最多统计 %d 个运行，实际更多", previewCap)
				return res
			}
			continue
		}
		s.sleep(ctx, set.BatchSleep)
	}
	var notes []string
	verb := "清理"
	if dry {
		verb = "将清理"
	}
	notes = append(notes, fmt.Sprintf("%s %d 个运行的备份", verb, runs))
	if unresolved > 0 {
		notes = append(notes, fmt.Sprintf("%d 个运行记录里没有备份存储信息，未处理", unresolved))
	}
	if badKeys > 0 {
		notes = append(notes, fmt.Sprintf("%d 个运行的清单含非法路径，已拒绝处理", badKeys))
	}
	res.Note = strings.Join(notes, "；")
	return res
}

func keysValid(keys []string) bool {
	for _, k := range keys {
		if !validKey(k) {
			return false
		}
	}
	return true
}

// tablesOK 确认所有表及其所需列都存在。
func (s *Service) tablesOK(need map[string][]string) (bool, error) {
	for table, cols := range need {
		st, err := s.repo.TableState(table, cols...)
		if err != nil {
			return false, err
		}
		if !st.OK() {
			return false, nil
		}
	}
	return true, nil
}
