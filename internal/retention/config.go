// @author: kevinkelly
// @license: Apache Licence
// @file: config.go
// @time: 2026/10/28

package retention

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/archive/engine"
	"dbtool/internal/settings"
)

// dbt_setting 里的配置项。全部缺省 = 不清理（总开关缺省关闭，各目标天数缺省 0）。
//
//	retention.enabled                 总开关，true 才会执行（缺省 false）
//	retention.interval_minutes       两轮之间的间隔（分钟），缺省 60，范围 10 ~ 1440
//	retention.window                 允许执行的时间窗口 HH:MM-HH:MM（可跨零点），空 = 不限制
//	retention.batch_size             每批删除行数，缺省 1000，范围 100 ~ 5000
//	retention.batch_sleep_ms         批间休眠毫秒，缺省 200，范围 0 ~ 5000
//	retention.max_rows               每个目标每轮最多删除的行数，缺省 2000000，范围 1000 ~ 50000000
//	retention.max_minutes            一轮最长执行分钟数，缺省 120，范围 1 ~ 1440
//	retention.<target>_days          各目标保留天数；0 / 未设 = 不清理；低于该目标的下限视为配置错误，该目标跳过
//	retention.archive_backup_enabled  true 才按备份存储的“保留天数”删除超期的备份对象（缺省 false）
const (
	cfgEnable         = settings.KeyRetentionEnabled
	cfgInterval       = settings.KeyRetentionIntervalMinutes
	cfgWindow         = settings.KeyRetentionWindow
	cfgBatchSize      = settings.KeyRetentionBatchSize
	cfgBatchSleep     = settings.KeyRetentionBatchSleepMs
	cfgMaxRows        = settings.KeyRetentionMaxRows
	cfgMaxMinutes     = settings.KeyRetentionMaxMinutes
	cfgBackupEnable   = settings.KeyRetentionArchiveBackupEnabled
	cfgDaysPrefix     = "retention."
	cfgDaysSuffix     = "_days"
	defaultInterval   = 60
	defaultBatchSize  = 1000
	maxBatchSize      = 5000 // 每批 ≤ 5000 行是硬上限
	defaultSleepMs    = 200
	defaultMaxRows    = 2_000_000
	defaultMaxMinutes = 120

	// stalePartAge 是导出目录里被杀进程遗留的 .upload-* 临时文件的清理年龄。
	stalePartAge = 24 * time.Hour
)

// 目标键（即 retention.<key>_days 里的 <key>）。
const (
	TargetQueryEvent   = "query_event"
	TargetQueryReject  = "query_reject"
	TargetQueryLog     = "query_log"
	TargetSlowLogs     = "slow_logs"
	TargetAuditLog     = "audit_log"
	TargetArchiveRun   = "archive_run"
	TargetExportFiles  = "export_files"
	TargetArchiveStore = "archive_backup"
)

// Settings 是一轮清理的有效设置（每轮重新读取，修改后无需重启）。
type Settings struct {
	Enabled       bool
	Interval      time.Duration
	Window        engine.Window
	BatchSize     int
	BatchSleep    time.Duration
	MaxRows       int64
	MaxDuration   time.Duration
	BackupEnabled bool
	Days          map[string]int // 目标 -> 保留天数（0 = 不清理）
	WindowRaw     string         // retention.window 原文（展示用）
	Problems      []string       // 配置值非法 / 低于下限的说明（对应目标已被忽略）
}

func cfgTrim(r Repo, item string) string {
	v, _ := r.ConfigValue(item)
	return strings.TrimSpace(v)
}

func cfgBool(r Repo, item string) bool { return strings.EqualFold(cfgTrim(r, item), "true") }

// cfgInt 读整数配置：未设 = def；非法或越界 = def 并记入 problems。
func cfgInt(r Repo, item string, def, lo, hi int, problems *[]string) int {
	v := cfgTrim(r, item)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		*problems = append(*problems, fmt.Sprintf("%s=%q 不合法（范围 %d ~ %d），已使用缺省值 %d", item, v, lo, hi, def))
		return def
	}
	return n
}

// daysKey 返回某目标的天数配置项名。
func daysKey(target string) string { return cfgDaysPrefix + target + cfgDaysSuffix }

// loadSettings 读取全部配置。
func loadSettings(r Repo) Settings {
	s := Settings{Enabled: cfgBool(r, cfgEnable), BackupEnabled: cfgBool(r, cfgBackupEnable), Days: map[string]int{}}
	s.Interval = time.Duration(cfgInt(r, cfgInterval, defaultInterval, 10, 1440, &s.Problems)) * time.Minute
	s.BatchSize = cfgInt(r, cfgBatchSize, defaultBatchSize, 100, maxBatchSize, &s.Problems)
	s.BatchSleep = time.Duration(cfgInt(r, cfgBatchSleep, defaultSleepMs, 0, 5000, &s.Problems)) * time.Millisecond
	s.MaxRows = int64(cfgInt(r, cfgMaxRows, defaultMaxRows, 1000, 50_000_000, &s.Problems))
	s.MaxDuration = time.Duration(cfgInt(r, cfgMaxMinutes, defaultMaxMinutes, 1, 1440, &s.Problems)) * time.Minute
	if w := cfgTrim(r, cfgWindow); w != "" {
		s.WindowRaw = w
		a, b, ok := strings.Cut(w, "-")
		_, ok1 := engine.ParseHM(a)
		_, ok2 := engine.ParseHM(b)
		if !ok || !ok1 || !ok2 {
			// 窗口写错时宁可不执行（engine.InWindow 对非法格式返回 false），并提示。
			s.Problems = append(s.Problems, fmt.Sprintf("%s=%q 不合法（格式 HH:MM-HH:MM），在改正之前不会执行清理", cfgWindow, w))
			s.Window = engine.Window{Start: "xx", End: "xx!"}
		} else {
			s.Window = engine.Window{Start: strings.TrimSpace(a), End: strings.TrimSpace(b)}
		}
	}
	for _, t := range daysTargets() {
		item := daysKey(t.Key)
		v := cfgTrim(r, item)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		switch {
		case err != nil || n < 0:
			s.Problems = append(s.Problems, fmt.Sprintf("%s=%q 不是非负整数，该目标不清理", item, v))
		case n == 0:
		case n < t.MinDays:
			s.Problems = append(s.Problems, fmt.Sprintf("%s=%d 低于下限 %d 天，该目标不清理（防止误配置）", item, n, t.MinDays))
		default:
			s.Days[t.Key] = n
		}
	}
	return s
}

// windowOpen 判断 now 是否在允许窗口内（窗口为空 = 不限制）。
func (s Settings) windowOpen(now time.Time) bool { return engine.InWindow(now, s.Window) }

// windowText 供预览展示。
func (s Settings) windowText() string { return s.WindowRaw }
