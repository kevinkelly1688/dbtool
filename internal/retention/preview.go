// @author: kevinkelly
// @license: Apache Licence
// @file: preview.go
// @time: 2026/10/28

package retention

import (
	"context"
	"fmt"
	"time"
)

// PreviewItem 是预览里一个目标的统计。
type PreviewItem struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	ConfigKey  string `json:"config_key,omitempty"`
	Configured bool   `json:"configured"` // 是否设置了保留天数（归档文件备份：是否打开了 retention.archive_backup_enabled）
	Days       int    `json:"days"`
	Cutoff     string `json:"cutoff,omitempty"`
	Candidates int64  `json:"candidates"` // 将被删除的行数 / 文件数 / 备份对象数
	Bytes      int64  `json:"bytes,omitempty"`
	Status     string `json:"status"` // ok / partial（统计到上限） / skipped / error / off（未配置）
	Note       string `json:"note,omitempty"`
	Err        string `json:"error,omitempty"`
	Risk       string `json:"risk"`
	Suggest    string `json:"suggest"`
}

// Preview 是 dry-run 的结果：按当前配置“如果现在执行会删多少”，不删除任何东西。
type Preview struct {
	GeneratedAt     string        `json:"generated_at"`
	Enabled         bool          `json:"enabled"` // 总开关；false 时清理不会执行，下面的数字是“打开后将清理的量”
	BackupEnabled   bool          `json:"archive_backup_enabled"`
	Window          string        `json:"window"`
	WindowOpen      bool          `json:"window_open"` // 此刻是否在窗口内
	IntervalMinutes int           `json:"interval_minutes"`
	BatchSize       int           `json:"batch_size"`
	BatchSleepMs    int           `json:"batch_sleep_ms"`
	MaxRows         int64         `json:"max_rows"`
	MaxMinutes      int           `json:"max_minutes"`
	Problems        []string      `json:"problems,omitempty"`
	Items           []PreviewItem `json:"items"`
}

// Preview 统计每个目标现在有多少数据会被清理。只读：表上只发 COUNT，文件只 stat，备份存储不连接。
// 大表上的 COUNT 最多算 20 秒（超时该目标报错，其余照常）。
func (s *Service) Preview(ctx context.Context) Preview {
	set := loadSettings(s.repo)
	now := s.now()
	p := Preview{GeneratedAt: now.Format(timeLayout), Enabled: set.Enabled, BackupEnabled: set.BackupEnabled, Window: set.windowText(),
		WindowOpen: set.windowOpen(now), IntervalMinutes: int(set.Interval / time.Minute), BatchSize: set.BatchSize,
		BatchSleepMs: int(set.BatchSleep / time.Millisecond), MaxRows: set.MaxRows, MaxMinutes: int(set.MaxDuration / time.Minute),
		Problems: set.Problems, Items: []PreviewItem{}}
	noHalt := func() string {
		if ctx.Err() != nil {
			return "已取消"
		}
		return ""
	}
	// 预览不受每轮上限约束。
	pset := set
	pset.MaxRows = 1 << 40
	for _, t := range allTargets() {
		it := PreviewItem{Key: t.Key, Label: t.Label, Risk: t.Risk, Suggest: t.Suggest, Status: "off"}
		if t.Kind != KindBackups {
			it.ConfigKey = daysKey(t.Key)
		} else {
			it.ConfigKey = cfgBackupEnable
		}
		var res TargetResult
		switch t.Kind {
		case KindTable:
			it.Days = set.Days[t.Key]
			it.Configured = it.Days > 0
			if !it.Configured {
				p.Items = append(p.Items, it)
				continue
			}
			res = s.previewTable(ctx, t, it.Days)
		case KindExports:
			it.Days = set.Days[t.Key]
			it.Configured = it.Days > 0
			if !it.Configured {
				p.Items = append(p.Items, it)
				continue
			}
			res = s.exports(ctx, t, pset, noHalt, true)
		case KindBackups:
			it.Configured = set.BackupEnabled
			res = s.backups(ctx, t, pset, noHalt, true)
			if res.Status == StatusSkipped && res.Note == "" {
				it.Status = "off"
				p.Items = append(p.Items, it)
				continue
			}
		}
		it.Days, it.Cutoff = max(res.Days, it.Days), res.Cutoff
		it.Candidates, it.Bytes, it.Note, it.Err = res.Deleted, res.Bytes, res.Note, res.Err
		it.Status = res.Status
		if t.Kind == KindBackups && !set.BackupEnabled {
			it.Note = fmt.Sprintf("（retention.archive_backup_enabled 未打开，不会执行）%s", it.Note)
		}
		p.Items = append(p.Items, it)
	}
	return p
}

func (s *Service) previewTable(ctx context.Context, t Target, days int) TargetResult {
	res := TargetResult{Key: t.Key, Label: t.Label, Days: days, Status: StatusSkipped}
	t, note, err := s.checkTable(t)
	if err != nil {
		res.Status, res.Err = StatusError, fmt.Sprintf("检查表结构失败：%s", err.Error())
		return res
	}
	if note != "" {
		res.Note = note
		return res
	}
	cut := s.cutoff(days)
	res.Cutoff = cut.Format(timeLayout)
	n, err := s.repo.CountOlder(ctx, t, cut)
	if err != nil {
		res.Status, res.Err = StatusError, fmt.Sprintf("统计失败：%v", err)
		return res
	}
	res.Status, res.Deleted = StatusOK, n
	return res
}
