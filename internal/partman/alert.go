// @author: kevinkelly
// @license: Apache Licence
// @file: alert.go
// @time: 2026/10/28

package partman

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 可用分区数量告警：
//
//   - 可用分区数 = 目标表里“上界晚于当前时间”的分区个数：含正在写入的当前分区，不含已经过去的历史分区，不含 MAXVALUE 分区。
//     维护正常时它稳定在 预创建数 + 1；定时维护跑不动（没开、报错、连不上实例）时会一天天变少，直到写入报错。
//   - 告警条件：可用分区数 < 阈值。阈值 = 这张表单独设置的值（dbt_partman_alert.threshold），没设置就用系统配置
//     partition.alert_min_available（默认 3）；0 = 这张表不告警。
//   - 计算不依赖维护计划能不能生成：边界不连续、字段类型对不上这类“计划失败”的表，恰恰最需要告警，所以这里自己读分区边界。
//     读不准的（读不到、边界解析失败、不是 RANGE 分区、配置与表对不上）一律“无法判断”，不告警。
//   - 检查：状态检查（页面）算出可用分区数并在列表里标出；后台每隔“自动维护间隔”检查一轮（RunAlerts，自动维护没开也检查），
//     排在定时维护之后，续建成功就不会推。
//   - 去重：首次不足推一次；之后每隔 partition.alert_repeat_hours 小时提醒一次（0 = 不重复）；回到阈值以上时可推一条“已恢复”。
//   - 推送沿用“推送设置”里启用的通道；推送失败只记告警记录与服务日志，不影响别的功能。

const (
	cfgAlertEnabled = settings.KeyPartitionAlertEnabled
	cfgAlertMin     = settings.KeyPartitionAlertMinAvailable
	cfgAlertRepeat  = settings.KeyPartitionAlertRepeatHours
	cfgAlertRecover = settings.KeyPartitionAlertNotifyRecover

	defaultAlertMin    = 3
	defaultAlertRepeat = 24
	maxAlertThreshold  = 100
	maxSilenceHours    = 24 * 90 // 最长静默 90 天
	maxAlertMsg        = 500
	alertLockName      = "dbt_partman_alert" // 跨进程互斥：同一时刻只有一个 dbtool 进程在检查并推送
)

// 告警记录的类型。
const (
	AlertFirst   = "alert"   // 首次不足
	AlertRepeat  = "repeat"  // 持续不足时的重复提醒
	AlertRecover = "recover" // 回到阈值以上
)

// 告警级别（页面与后台共用同一套判定）。
const (
	levelOff    = "off"    // 全局未启用，或这张表阈值为 0
	levelNA     = "na"     // 无法判断（读不到、不是 RANGE 分区、配置与表对不上……）
	levelOK     = "ok"     // 可用分区数不小于阈值
	levelAlert  = "alert"  // 小于阈值
	levelSilent = "silent" // 小于阈值，但在静默期内
)

// ErrAlertNotReady 表示扩展表 dbt_partman_alert / dbt_partman_alert_log 还没建（没执行 280_partman_alert.sql）。
var ErrAlertNotReady = errors.New("可用分区告警的扩展表还没有建") // i18n-ignore：内部哨兵错误，对外由 errAlertNotReady 给出提示

func errAlertNotReady() error {
	return newErr(503, "alert_table_missing", "可用分区告警还没有启用：请先在元数据库执行 deploy/sql/280_partman_alert.sql（升级时 install.sh 会自动执行），然后刷新页面")
}

// AlertSetting -> dbt_partman_alert：每张表自己的阈值、静默与告警状态。
type AlertSetting struct {
	CnfID        int64      `gorm:"column:cnf_id;primaryKey"`
	Threshold    *int       `gorm:"column:threshold"`     // nil 跟随全局，0 不告警
	SilenceUntil *time.Time `gorm:"column:silence_until"` // 静默到何时
	Alerting     bool       `gorm:"column:alerting"`      // 上一次检查时是否处于告警
	LastAlertAt  *time.Time `gorm:"column:last_alert_at"` // 上一次成功推送告警 / 提醒的时间
	UpdatedAt    time.Time  `gorm:"column:updated_at;->"`
}

func (AlertSetting) TableName() string { return "dbt_partman_alert" }

// AlertLog -> dbt_partman_alert_log：每次推送一条。
type AlertLog struct {
	ID         uint      `gorm:"column:id;primaryKey"`
	CnfID      int64     `gorm:"column:cnf_id"`
	Kind       string    `gorm:"column:kind"`
	Available  int       `gorm:"column:available"`
	Threshold  int       `gorm:"column:threshold"`
	Channels   string    `gorm:"column:channels"`
	OK         bool      `gorm:"column:ok"`
	Message    string    `gorm:"column:message"`
	CreateTime time.Time `gorm:"column:created_at"`
}

func (AlertLog) TableName() string { return "dbt_partman_alert_log" }

// AlertRepo 是可用分区告警需要的元数据库访问（Repo 的可选能力，用类型断言取得；扩展表没建时各方法返回 ErrAlertNotReady）。
type AlertRepo interface {
	AlertReady() bool
	ListAlertSettings(cnfIDs []int64) (map[int64]AlertSetting, error)
	SetAlertThreshold(cnfID int64, threshold *int) error
	SetAlertSilence(cnfID int64, until *time.Time) error
	// SaveAlertState 记下“是否处于告警”；lastAlertAt 非 nil 时同时更新上一次推送时间。
	SaveAlertState(cnfID int64, alerting bool, lastAlertAt *time.Time) error
	InsertAlertLog(l *AlertLog) error
	ListAlertLogs(cnfIDs []int64, offset, limit int) ([]AlertLog, int64, error)
	DeleteAlertData(cnfID int64) error
}

// ---- 全局设置（系统配置里的 partition.alert_*） ----

// AlertSettings 是全局的告警设置。
type AlertSettings struct {
	Enabled       bool `json:"enabled"`
	MinAvailable  int  `json:"min_available"`
	RepeatHours   int  `json:"repeat_hours"`
	NotifyRecover bool `json:"notify_recover"`
}

func (s *Service) alertSettings() AlertSettings {
	flag := func(item string, def bool) bool {
		v, ok := s.meta.ConfigValue(item)
		if !ok || strings.TrimSpace(v) == "" {
			return def
		}
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	num := func(item string, def, lo, hi int) int {
		if v, ok := s.meta.ConfigValue(item); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= lo && n <= hi {
				return n
			}
		}
		return def
	}
	return AlertSettings{Enabled: flag(cfgAlertEnabled, true), MinAvailable: num(cfgAlertMin, defaultAlertMin, 1, maxAlertThreshold),
		RepeatHours: num(cfgAlertRepeat, defaultAlertRepeat, 0, 720), NotifyRecover: flag(cfgAlertRecover, true)}
}

// AlertOverview 是 overview 里的 alert 段：全局设置 + 扩展表是否已建 + 输入范围。
type AlertOverview struct {
	Ready           bool `json:"ready"`
	MaxThreshold    int  `json:"max_threshold"`
	MaxSilenceHours int  `json:"max_silence_hours"`
	AlertSettings
}

func (s *Service) alertOverview() AlertOverview {
	return AlertOverview{Ready: s.alerts != nil && s.alerts.AlertReady(), MaxThreshold: maxAlertThreshold, MaxSilenceHours: maxSilenceHours, AlertSettings: s.alertSettings()}
}

// ---- 可用分区数 ----

// availInfo 是一张表的可用分区情况。Known=false 表示读不准，不做任何判断。
type availInfo struct {
	Known    bool
	Count    int            // 可用分区数
	End      time.Time      // 最后一个可用分区的上界（预计用尽的时刻）；Count=0 时为零值
	MaxValue bool           // 表里有 MAXVALUE 兜底分区
	Loc      *time.Location // 分区边界所在的时区
}

// availability 数出 st 里“上界晚于 now”的分区个数（MAXVALUE 与读不到上界的分区不计）。
// 刻意不走 BuildPlan：计划因边界不连续、字段类型不符等原因失败的表，更需要知道还剩几个分区。
func availability(cfg Config, st TableState, now time.Time) availInfo {
	doris := st.Dialect == DialectDoris
	switch {
	case !st.Exists || !st.IsBase || len(st.Partitions) == 0:
		return availInfo{}
	case !doris && st.Method != "RANGE" && st.Method != "RANGE COLUMNS", !doris && st.Subpartitioned:
		return availInfo{}
	case st.ColumnType != "" && !strings.EqualFold(st.ColumnType, cfg.ColType): // 配置与表对不上，边界按配置的类型解析会不准
		return availInfo{}
	}
	tz, err := resolveTimezone(cfg.Timezone, &st)
	if err != nil {
		return availInfo{}
	}
	loc := tz.Loc
	now = now.In(loc)
	ai := availInfo{Known: true, Loc: loc}
	for _, rp := range st.Partitions {
		var bound time.Time
		switch {
		case doris:
			t, max, ok := dorisUpper(rp.Desc, cfg.ColType, loc)
			if !ok {
				return availInfo{} // 读不到上界：数出来的只是下限，不判断
			}
			if max {
				ai.MaxValue = true
				continue
			}
			bound = t
		case strings.EqualFold(strings.TrimSpace(rp.Desc), "MAXVALUE"):
			ai.MaxValue = true
			continue
		default:
			b, err := parseBound(rp.Desc, cfg.ColType, loc)
			if err != nil {
				return availInfo{}
			}
			bound = b
		}
		if bound.After(now) {
			ai.Count++
			if bound.After(ai.End) {
				ai.End = bound
			}
		}
	}
	return ai
}

// alertLevelOf 判定一张表的告警级别，并返回生效的阈值。页面（前端有同一套判定）与后台检查共用这套规则。
func alertLevelOf(set AlertSettings, per AlertSetting, avail *int, now time.Time) (string, int) {
	thr := set.MinAvailable
	if per.Threshold != nil {
		thr = *per.Threshold
	}
	switch {
	case !set.Enabled:
		return levelOff, thr
	case avail == nil:
		return levelNA, thr
	case thr <= 0:
		return levelOff, thr
	case *avail >= thr:
		return levelOK, thr
	case per.SilenceUntil != nil && per.SilenceUntil.After(now):
		return levelSilent, thr
	}
	return levelAlert, thr
}

// ---- 每张表的设置（列表 / 编辑） ----

// AlertView 是一张表的告警设置。
type AlertView struct {
	Threshold    *int   `json:"alert_threshold"`               // nil = 跟随全局，0 = 不告警
	SilenceUntil string `json:"alert_silence_until,omitempty"` // 仍在静默期内才有值
}

func (s *Service) alertView(a AlertSetting) AlertView {
	v := AlertView{Threshold: a.Threshold}
	if a.SilenceUntil != nil && a.SilenceUntil.After(s.now()) {
		v.SilenceUntil = fmtTime(*a.SilenceUntil)
	}
	return v
}

// alertSettingsOf 读这些配置各自的告警设置（按 cnf_id）；扩展表没建或读取失败时返回空表（降级为“都跟随全局”）。
func (s *Service) alertSettingsOf(rows []Config) map[int64]AlertSetting {
	if s.alerts == nil || len(rows) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.CnfID)
	}
	m, err := s.alerts.ListAlertSettings(ids)
	if err != nil {
		if !errors.Is(err, ErrAlertNotReady) {
			slog.Warn("读取分区告警设置失败（已忽略）", "err", err)
		}
		return nil
	}
	return m
}

// attachAlerts 把每张表的阈值与静默期并进列表行；扩展表没建时原样返回。
func (s *Service) attachAlerts(rows []Config, out []ConfigView) {
	m := s.alertSettingsOf(rows)
	if m == nil {
		return
	}
	for i, r := range rows {
		if a, ok := m[r.CnfID]; ok {
			v := s.alertView(a)
			out[i].AlertThreshold, out[i].AlertSilenceUntil = v.Threshold, v.SilenceUntil
		}
	}
}

func (s *Service) alertTable() (AlertRepo, error) {
	if s.alerts == nil || !s.alerts.AlertReady() {
		return nil, errAlertNotReady()
	}
	return s.alerts, nil
}

// SetAlertThreshold 设置这张表自己的阈值：nil = 跟随全局默认，0 = 这张表不告警。与编辑配置同权限，写审计。
func (s *Service) SetAlertThreshold(c Caller, id uint, threshold *int) (*AlertView, error) {
	if err := c.need(permChange, "修改告警阈值"); err != nil {
		return nil, err
	}
	if threshold != nil && (*threshold < 0 || *threshold > maxAlertThreshold) {
		return nil, badReq("告警阈值需要在 0 到 %d 之间（0 = 这张表不告警，留空 = 跟随全局默认）", maxAlertThreshold)
	}
	cfg, inst, err := s.load(c, id)
	if err != nil {
		return nil, err
	}
	repo, err := s.alertTable()
	if err != nil {
		return nil, err
	}
	if err := repo.SetAlertThreshold(cfg.CnfID, threshold); err != nil {
		return nil, s.alertRepoErr(err)
	}
	what := "跟随全局默认" // i18n-ignore：审计日志正文，不翻译
	if threshold != nil {
		what = fmt.Sprintf("%d", *threshold)
	}
	s.auditBestEffort(c, "分区维护-告警阈值", fmt.Sprintf("配置 #%d（实例 %s）：%s.%s 的可用分区告警阈值设为 %s", cfg.CnfID, inst.InstanceName, cfg.DBName, cfg.TbName, what)) // i18n-ignore：审计日志正文，不翻译
	return s.currentAlertView(repo, cfg.CnfID)
}

// SilenceAlert 静默这张表的告警 hours 小时（期间只在列表里标记，不推送）；0 = 取消静默。
func (s *Service) SilenceAlert(c Caller, id uint, hours int) (*AlertView, error) {
	if err := c.need(permChange, "静默告警"); err != nil {
		return nil, err
	}
	if hours < 0 || hours > maxSilenceHours {
		return nil, badReq("静默时长需要在 0 到 %d 小时之间（0 = 取消静默）", maxSilenceHours)
	}
	cfg, inst, err := s.load(c, id)
	if err != nil {
		return nil, err
	}
	repo, err := s.alertTable()
	if err != nil {
		return nil, err
	}
	var until *time.Time
	if hours > 0 {
		t := s.now().Add(time.Duration(hours) * time.Hour)
		until = &t
	}
	if err := repo.SetAlertSilence(cfg.CnfID, until); err != nil {
		return nil, s.alertRepoErr(err)
	}
	what := "取消静默" // i18n-ignore：审计日志正文，不翻译
	if hours > 0 {
		what = fmt.Sprintf("静默 %d 小时", hours) // i18n-ignore：审计日志正文，不翻译
	}
	s.auditBestEffort(c, "分区维护-告警静默", fmt.Sprintf("配置 #%d（实例 %s）：%s.%s %s", cfg.CnfID, inst.InstanceName, cfg.DBName, cfg.TbName, what)) // i18n-ignore：审计日志正文，不翻译
	return s.currentAlertView(repo, cfg.CnfID)
}

func (s *Service) currentAlertView(repo AlertRepo, cnfID int64) (*AlertView, error) {
	m, err := repo.ListAlertSettings([]int64{cnfID})
	if err != nil {
		return nil, s.alertRepoErr(err)
	}
	v := s.alertView(m[cnfID])
	return &v, nil
}

// alertRepoErr 把“扩展表没建”换成给用户看的提示，其它错误原样返回（由 HTTP 层统一成 500）。
func (s *Service) alertRepoErr(err error) error {
	if errors.Is(err, ErrAlertNotReady) {
		return errAlertNotReady()
	}
	return err
}

// ---- 告警记录 ----

// AlertLogView 是告警记录页的一行。
type AlertLogView struct {
	ID         uint   `json:"id"`
	CreateTime string `json:"create_time"`
	CnfID      int64  `json:"cnf_id"`
	Instance   string `json:"instance"`
	DBName     string `json:"dbname"`
	TbName     string `json:"tbname"`
	Kind       string `json:"kind"` // alert / repeat / recover
	Available  int    `json:"available"`
	Threshold  int    `json:"threshold"`
	Channels   string `json:"channels"`
	OK         bool   `json:"ok"`
	Message    string `json:"message,omitempty"`
}

// AlertLogs 列出调用者能看到的实例上的告警记录（新的在前）。
func (s *Service) AlertLogs(c Caller, offset, limit int) ([]AlertLogView, int64, error) {
	insts, err := s.visible(c)
	if err != nil {
		return nil, 0, wrap(err)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	repo, err := s.alertTable()
	if err != nil {
		return nil, 0, err
	}
	byInst := make(map[uint]*model.Instance, len(insts))
	ids := make([]uint, 0, len(insts))
	for i := range insts {
		byInst[insts[i].ID] = &insts[i]
		ids = append(ids, insts[i].ID)
	}
	if len(ids) == 0 {
		return []AlertLogView{}, 0, nil
	}
	cfgs, _, err := s.repo.ListConfigs(ConfigFilter{InstanceIDs: ids})
	if err != nil {
		return nil, 0, err
	}
	byCnf := make(map[int64]Config, len(cfgs))
	cnfIDs := make([]int64, 0, len(cfgs))
	for _, cf := range cfgs {
		byCnf[cf.CnfID] = cf
		cnfIDs = append(cnfIDs, cf.CnfID)
	}
	rows, total, err := repo.ListAlertLogs(cnfIDs, offset, limit)
	if err != nil {
		return nil, 0, s.alertRepoErr(err)
	}
	out := make([]AlertLogView, 0, len(rows))
	for _, r := range rows {
		v := AlertLogView{ID: r.ID, CreateTime: fmtTime(r.CreateTime), CnfID: r.CnfID, Kind: r.Kind, Available: r.Available,
			Threshold: r.Threshold, Channels: r.Channels, OK: r.OK, Message: r.Message}
		if cf, ok := byCnf[r.CnfID]; ok {
			v.DBName, v.TbName = cf.DBName, cf.TbName
			if in := byInst[cf.InstanceID]; in != nil {
				v.Instance = in.InstanceName
			}
		}
		out = append(out, v)
	}
	return out, total, nil
}

// ---- 后台检查与推送 ----

// AlertSummary 是一轮检查的统计。
type AlertSummary struct {
	Enabled   bool
	Checked   int
	Alerted   int // 首次告警
	Repeated  int // 重复提醒
	Recovered int // 已恢复
	Failed    int // 推送失败
	Notes     []string
}

// RunAlerts 检查所有分区配置的可用分区数，按“去重规则”推送告警 / 提醒 / 已恢复。
// 同一时刻只有一个进程在跑（GET_LOCK）；“是否在告警、上一次推送时间”存在 dbt_partman_alert，所以多进程、重启之后也不会重复推。
func (s *Service) RunAlerts(ctx context.Context) AlertSummary {
	set := s.alertSettings()
	sum := AlertSummary{Enabled: set.Enabled}
	if !set.Enabled || s.alerts == nil {
		return sum
	}
	if !s.alerts.AlertReady() {
		slog.Warn("分区可用数量告警：扩展表还没有建，本轮不检查（请在元数据库执行 deploy/sql/280_partman_alert.sql）")
		return sum
	}
	release, ok, err := s.repo.TryLock(ctx, alertLockName)
	if err != nil {
		slog.Warn("分区可用数量告警：取锁失败，本轮不检查", "err", err)
		return sum
	}
	if !ok {
		return sum // 另一个进程正在检查
	}
	defer release()
	insts, err := s.meta.ListInstances()
	if err != nil {
		slog.Warn("分区可用数量告警：读取实例列表失败", "err", err)
		return sum
	}
	byID := make(map[uint]*model.Instance, len(insts))
	for i := range insts {
		byID[insts[i].ID] = &insts[i]
	}
	cfgs, _, err := s.repo.ListConfigs(ConfigFilter{AllInstances: true})
	if err != nil {
		slog.Warn("分区可用数量告警：读取分区配置失败", "err", err)
		return sum
	}
	ids := make([]int64, 0, len(cfgs))
	for _, cf := range cfgs {
		ids = append(ids, cf.CnfID)
	}
	states, err := s.alerts.ListAlertSettings(ids)
	if err != nil {
		slog.Warn("分区可用数量告警：读取告警状态失败", "err", err)
		return sum
	}
	var chs []pushChannel
	chsLoaded, noChannel := false, 0
	for _, cfg := range cfgs {
		if ctx.Err() != nil {
			return sum
		}
		inst := byID[cfg.InstanceID]
		if inst == nil || !SupportedDBType(inst.DBType) {
			continue
		}
		cfg := cfg
		v := s.probe(ctx, inst, &cfg)
		s.storeStatus(&cfg, v)
		sum.Checked++
		now := s.now()
		per := states[cfg.CnfID]
		per.CnfID = cfg.CnfID
		level, thr := alertLevelOf(set, per, v.Available, now)
		var kind string
		switch level {
		case levelAlert:
			switch {
			case !per.Alerting:
				kind = AlertFirst
			case set.RepeatHours > 0 && per.LastAlertAt != nil && now.Sub(*per.LastAlertAt) >= time.Duration(set.RepeatHours)*time.Hour:
				kind = AlertRepeat
			}
		case levelOK:
			if per.Alerting {
				if set.NotifyRecover {
					kind = AlertRecover
				} else if err := s.alerts.SaveAlertState(cfg.CnfID, false, nil); err != nil {
					slog.Warn("分区可用数量告警：保存状态失败", "cnf_id", cfg.CnfID, "err", err)
				}
			}
		case levelOff:
			if per.Alerting { // 阈值改成了 0 / 总开关关了：不再算在告警里，也不推“已恢复”
				if err := s.alerts.SaveAlertState(cfg.CnfID, false, nil); err != nil {
					slog.Warn("分区可用数量告警：保存状态失败", "cnf_id", cfg.CnfID, "err", err)
				}
			}
		}
		if kind == "" {
			continue
		}
		if !chsLoaded {
			chs, chsLoaded = s.alertChannels(), true
		}
		if len(chs) == 0 { // 没有启用的通道：不记告警、不改状态，等有了通道再推
			noChannel++
			continue
		}
		names, okAny, msg := s.pushAlert(ctx, chs, kind, &cfg, inst, v, *v.Available, thr, per.Threshold != nil, now)
		if err := s.alerts.InsertAlertLog(&AlertLog{CnfID: cfg.CnfID, Kind: kind, Available: *v.Available, Threshold: thr, Channels: names, OK: okAny,
			Message: clipMsg(msg, maxAlertMsg), CreateTime: now}); err != nil {
			slog.Warn("分区可用数量告警：写入告警记录失败", "cnf_id", cfg.CnfID, "err", err)
		}
		switch {
		case kind == AlertRecover:
			sum.Recovered++
			// 推送失败也不再重试“已恢复”：状态照样复位，失败原因在告警记录里
			if err := s.alerts.SaveAlertState(cfg.CnfID, false, nil); err != nil {
				slog.Warn("分区可用数量告警：保存状态失败", "cnf_id", cfg.CnfID, "err", err)
			}
		case okAny:
			if kind == AlertFirst {
				sum.Alerted++
			} else {
				sum.Repeated++
			}
			if err := s.alerts.SaveAlertState(cfg.CnfID, true, &now); err != nil {
				slog.Warn("分区可用数量告警：保存状态失败", "cnf_id", cfg.CnfID, "err", err)
			}
		default: // 所有通道都失败：状态不动，下一轮重试
			sum.Failed++
		}
	}
	if noChannel > 0 {
		sum.Notes = append(sum.Notes, fmt.Sprintf("%d 张表需要推送，但没有启用的推送通道", noChannel)) // i18n-ignore：只写服务日志
	}
	return sum
}

func (s *Service) alertChannels() []pushChannel {
	hooks, err := s.repo.ListWebhooks(true)
	if err != nil {
		slog.Warn("分区可用数量告警：读取推送通道失败", "err", err)
		return nil
	}
	chs := make([]pushChannel, 0, len(hooks))
	for _, h := range hooks {
		chs = append(chs, channelOf(h))
	}
	return chs
}

// pushAlert 向所有通道推送一条告警 / 提醒 / 已恢复，返回通道类型清单、是否至少一个成功、失败原因。
func (s *Service) pushAlert(ctx context.Context, chs []pushChannel, kind string, cfg *Config, inst *model.Instance, v StatusView, avail, thr int, custom bool, now time.Time) (string, bool, string) {
	header, body := alertMessage(kind, cfg, inst, v, avail, thr, custom, now)
	res := s.sendAll(ctx, chs, header, body, kind != AlertRecover) // 告警按“失败”处理 @（仅失败时 @ 的通道也会 @）
	names := make([]string, 0, len(chs))
	var fails []string
	okAny := false
	for i, r := range res {
		names = append(names, chs[i].Type)
		if r.OK {
			okAny = true
			continue
		}
		fails = append(fails, fmt.Sprintf("%s %s：%s", chs[i].Type, r.WebhookURL, r.Message))
		slog.Warn("分区可用数量告警推送失败", "channel", chs[i].Type, "webhook", r.WebhookURL, "status", r.Status, "msg", r.Message)
	}
	return strings.Join(names, ","), okAny, strings.Join(fails, "；") // i18n-ignore：分隔符
}

// alertMessage 生成推送的标题与正文（语言属于接收方，固定中文）。
func alertMessage(kind string, cfg *Config, inst *model.Instance, v StatusView, avail, thr int, custom bool, now time.Time) (header, body string) {
	title := "【分区告警】可用分区数不足" // i18n-ignore：推送消息正文，语言属于接收方
	switch kind {
	case AlertRepeat:
		title = "【分区告警·持续】可用分区数仍然不足" // i18n-ignore：推送消息正文，语言属于接收方
	case AlertRecover:
		title = "【分区告警·已恢复】可用分区数已恢复" // i18n-ignore：推送消息正文，语言属于接收方
	}
	header = title + "  " + now.Format(timeLayout)
	var b strings.Builder
	env := ""
	if cfg.Env != "" {
		env = "（" + cfg.Env + "）"
	}
	fmt.Fprintf(&b, "实例：%s%s\n", inst.InstanceName, env)  // i18n-ignore：推送消息正文，语言属于接收方
	fmt.Fprintf(&b, "库表：%s.%s\n", cfg.DBName, cfg.TbName) // i18n-ignore：推送消息正文，语言属于接收方
	own := ""
	if custom {
		own = "，本表单独设置" // i18n-ignore：推送消息正文，语言属于接收方
	}
	fmt.Fprintf(&b, "可用分区：%d 个（阈值 %d%s）\n", avail, thr, own) // i18n-ignore：推送消息正文，语言属于接收方
	if kind == AlertRecover {
		return header, b.String()
	}
	switch {
	case avail == 0 && v.HasMaxValue:
		b.WriteString("预计用尽：已用尽，新数据正在落入 MAXVALUE 分区（写入不会失败，但需要人工 REORGANIZE）\n") // i18n-ignore：推送消息正文，语言属于接收方
	case avail == 0:
		b.WriteString("预计用尽：已用尽，新数据写入会报错（没有可用分区）\n") // i18n-ignore：推送消息正文，语言属于接收方
	default:
		fmt.Fprintf(&b, "预计用尽：%s（约 %s后）\n", v.AvailableEnd, humanSecs(v.AvailableSecs)) // i18n-ignore：推送消息正文，语言属于接收方
	}
	if cfg.IsEnable {
		b.WriteString("维护开关：已开启\n") // i18n-ignore：推送消息正文，语言属于接收方
	} else {
		b.WriteString("维护开关：已关闭（不会自动续建）\n") // i18n-ignore：推送消息正文，语言属于接收方
	}
	switch v.State {
	case StateError:
		fmt.Fprintf(&b, "当前状态：异常：%s\n", clipMsg(v.Message, 200)) // i18n-ignore：推送消息正文，语言属于接收方
	case StatePending:
		fmt.Fprintf(&b, "当前状态：待续建 %d\n", v.Adds) // i18n-ignore：推送消息正文，语言属于接收方
	}
	b.WriteString("建议：到“分区维护”页预览并执行续建") // i18n-ignore：推送消息正文，语言属于接收方
	if v.HasMaxValue {
		b.WriteString("（该表含 MAXVALUE 分区，续建需要 REORGANIZE）") // i18n-ignore：推送消息正文，语言属于接收方
	}
	b.WriteString("\n")
	return header, b.String()
}

// humanSecs 把秒数写成“1 天 14 小时”“3 小时 20 分钟”。
func humanSecs(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	m := sec / 60
	d, h := m/1440, m%1440/60
	switch {
	case d >= 1 && d < 10 && h > 0:
		return fmt.Sprintf("%d 天 %d 小时", d, h) // i18n-ignore：推送消息正文，语言属于接收方
	case d >= 1:
		return fmt.Sprintf("%d 天", d) // i18n-ignore：推送消息正文，语言属于接收方
	case h >= 1 && m%60 > 0:
		return fmt.Sprintf("%d 小时 %d 分钟", h, m%60) // i18n-ignore：推送消息正文，语言属于接收方
	case h >= 1:
		return fmt.Sprintf("%d 小时", h) // i18n-ignore：推送消息正文，语言属于接收方
	}
	return fmt.Sprintf("%d 分钟", m) // i18n-ignore：推送消息正文，语言属于接收方
}
