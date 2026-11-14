// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package inspect

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"dbtool/internal/crypto"
	"dbtool/internal/engine"
	"dbtool/internal/instext"
	"dbtool/internal/model"
	"dbtool/internal/settings"
	"dbtool/internal/sshtunnel"
)

// ---- 表映射 ----

// 这些表只读写既有列，不改结构。start_time 要自己填。

type mssqlItemRow struct {
	ID         uint    `gorm:"primaryKey"`
	ReportID   uint    `gorm:"column:report_id"`
	Section    string  `gorm:"column:section"`
	ItemKey    string  `gorm:"column:item_key"`
	ItemName   string  `gorm:"column:item_name"`
	DBName     string  `gorm:"column:db_name"`
	Level      int     `gorm:"column:level"`
	Value      string  `gorm:"column:value"`
	Threshold  string  `gorm:"column:threshold"`
	Detail     *string `gorm:"column:detail"`
	RawCounter *string `gorm:"column:raw_counter"`
	ElapsedMs  int     `gorm:"column:elapsed_ms"`
}

func (mssqlItemRow) TableName() string { return "dbt_sql_mssql_inspection_item" }

// 新表（deploy/sql/013_inspect.sql）。

type planRow struct {
	ID               uint       `gorm:"primaryKey"`
	Name             string     `gorm:"column:name"`
	Enabled          bool       `gorm:"column:enabled"`
	ScheduleKind     string     `gorm:"column:schedule_kind"`
	ScheduleTime     string     `gorm:"column:schedule_time"`
	ScheduleWeekdays string     `gorm:"column:schedule_weekdays"`
	ScheduleMonthday int        `gorm:"column:schedule_monthday"`
	CronExpr         string     `gorm:"column:cron_expr"`
	TagCodes         string     `gorm:"column:tag_codes"`
	MSSQLDatabases   string     `gorm:"column:mssql_databases"`
	Concurrency      int        `gorm:"column:concurrency"`
	StaggerSeconds   int        `gorm:"column:stagger_seconds"`
	SkipBusy         bool       `gorm:"column:skip_busy"`
	RetryOnce        bool       `gorm:"column:retry_once"`
	TimeoutMinutes   int        `gorm:"column:timeout_minutes"`
	NotifyMode       string     `gorm:"column:notify_mode"`
	NotifyChannels   string     `gorm:"column:notify_channels"`
	RetentionDays    int        `gorm:"column:retention_days"`
	NextRunAt        *time.Time `gorm:"column:next_run_at"`
	LastRunAt        *time.Time `gorm:"column:last_run_at"`
	LastRunID        uint       `gorm:"column:last_run_id"`
	CreatedBy        string     `gorm:"column:created_by"`
	CreateTime       time.Time  `gorm:"column:create_time"`
	SysTime          time.Time  `gorm:"column:sys_time"`
}

func (planRow) TableName() string { return "dbt_inspect_plan" }

type planInstanceRow struct {
	PlanID     uint `gorm:"column:plan_id;primaryKey"`
	InstanceID uint `gorm:"column:instance_id;primaryKey"`
}

func (planInstanceRow) TableName() string { return "dbt_inspect_plan_instance" }

type runRow struct {
	ID          uint       `gorm:"primaryKey"`
	PlanID      uint       `gorm:"column:plan_id"`
	PlanName    string     `gorm:"column:plan_name"`
	TriggerType string     `gorm:"column:trigger_type"`
	Operator    string     `gorm:"column:operator"`
	ScheduledAt time.Time  `gorm:"column:scheduled_at"`
	StartedAt   time.Time  `gorm:"column:started_at"`
	FinishedAt  *time.Time `gorm:"column:finished_at"`
	Status      string     `gorm:"column:status"`
	Total       int        `gorm:"column:instance_total"`
	OK          int        `gorm:"column:ok_count"`
	Failed      int        `gorm:"column:failed_count"`
	Skipped     int        `gorm:"column:skipped_count"`
	Alert       int        `gorm:"column:alert_total"`
	Attention   int        `gorm:"column:attention_total"`
	Note        string     `gorm:"column:note"`
	HeartbeatAt time.Time  `gorm:"column:heartbeat_at"`
}

func (runRow) TableName() string { return "dbt_inspect_run" }

type runItemRow struct {
	ID           uint   `gorm:"primaryKey"`
	RunID        uint   `gorm:"column:run_id"`
	InstanceID   uint   `gorm:"column:instance_id"`
	InstanceName string `gorm:"column:instance_name"`
	ReportKind   string `gorm:"column:report_kind"`
	ReportID     uint   `gorm:"column:report_id"`
	Status       string `gorm:"column:status"`
	Alert        int    `gorm:"column:alert_count"`
	Attention    int    `gorm:"column:attention_count"`
	Message      string `gorm:"column:message"`
}

func (runItemRow) TableName() string { return "dbt_inspect_run_item" }

// store 是 Repo 的 GORM 实现。
type store struct {
	db     *gorm.DB
	m      *crypto.Mirage
	connFn func(*model.Instance) engine.Conn
	// 表存在性只在“存在”时缓存（表可能在服务运行期间被手工创建）
	planTables bool
	slowTable  bool
	redisTable bool // dbt_inspect_redis_report 是否已创建（只缓存“存在”）
}

func newStore(db *gorm.DB, m *crypto.Mirage) *store { return &store{db: db, m: m} }

func reportTable(kind string) string {
	if kind == KindMSSQL {
		return "dbt_sql_mssql_inspection_report"
	}
	if kind == KindRedis {
		return "dbt_inspect_redis_report"
	}
	return "dbt_sql_mysql_inspection_report"
}

// ---- 实例 ----

func (s *store) ListInstances() ([]model.Instance, error) {
	var out []model.Instance
	err := s.db.Preload("Tags").Preload("ResourceGroups").Order("instance_name").Find(&out).Error
	return out, err
}

func (s *store) GetInstance(id uint) (*model.Instance, error) {
	var in model.Instance
	if err := s.db.Preload("Tunnel").First(&in, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &in, nil
}

func (s *store) InstanceIDsByTags(codes []string) ([]uint, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	var ids []uint
	err := s.db.Raw(`SELECT DISTINCT it.instance_id FROM dbt_sql_instance_instance_tag it JOIN dbt_sql_instance_tag t ON t.id = it.instancetag_id WHERE t.tag_code IN ? AND t.active = 1`, codes).Scan(&ids).Error
	return ids, err
}

func (s *store) Conn(inst *model.Instance) engine.Conn {
	if s.connFn != nil {
		return s.connFn(inst)
	}
	c := engine.Conn{Host: inst.Host, Port: inst.Port, User: s.m.Decrypt(inst.User), Password: s.m.Decrypt(inst.Password), DBName: inst.DBName,
		Charset: inst.Charset, SSL: inst.IsSSL, VerifySSL: inst.VerifySSL, ServiceName: inst.ServiceNameValue(), SID: inst.SIDValue()}
	if sshtunnel.Enabled(inst) { // 正式装配时 connFn 已是 Store.EngineConn；这里兜底，保证直接用 store 时也走隧道
		c = sshtunnel.NewGormResolver(s.db, s.m.Decrypt).Apply(inst, c)
	}
	return c
}

// ---- 报告 ----

func (s *store) CreateReport(kind string, instanceID uint, trigger, operator string, total int, now time.Time, staleAfter time.Duration) (uint, bool, error) {
	if kind == KindRedis && !s.hasRedisTable() {
		return 0, false, errRedisTable
	}
	tbl := reportTable(kind)
	var id uint
	busy := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		// 锁住实例行，让多个进程同时为同一实例建报告时串行，保证“同一实例同时只有一份进行中的巡检”
		var lock model.Instance
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&lock, instanceID).Error; err != nil {
			return err
		}
		// 进程被杀等情况会留下永远 running 的报告，超过 staleAfter 的按失败收尾，避免实例被永久锁住
		if err := tx.Exec("UPDATE "+tbl+" SET status = 'failed', error_message = ?, finish_time = ? WHERE instance_id = ? AND status = 'running' AND start_time < ?",
			"执行超时或任务进程中断", now, instanceID, now.Add(-staleAfter)).Error; err != nil {
			return err
		}
		var n int64
		if err := tx.Raw("SELECT COUNT(*) FROM "+tbl+" WHERE instance_id = ? AND status = 'running'", instanceID).Scan(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			busy = true
			return nil
		}
		var res *gorm.DB
		if kind == KindMSSQL {
			res = tx.Exec("INSERT INTO "+tbl+" (instance_id, status, trigger_type, operator, alert_count, attention_count, normal_count, failed_count, total_items, done_items, start_time) VALUES (?, 'running', ?, ?, 0, 0, 0, 0, ?, 0, ?)",
				instanceID, trigger, operator, total, now)
		} else {
			res = tx.Exec("INSERT INTO "+tbl+" (instance_id, status, trigger_type, operator, health_score, alert_count, attention_count, failed_count, total_items, done_items, start_time) VALUES (?, 'running', ?, ?, 100, 0, 0, 0, ?, 0, ?)",
				instanceID, trigger, operator, total, now)
		}
		if res.Error != nil {
			return res.Error
		}
		return tx.Raw("SELECT LAST_INSERT_ID()").Scan(&id).Error
	})
	return id, busy, err
}

func (s *store) ReportStatus(kind string, id uint) (string, error) {
	if kind == KindRedis && !s.hasRedisTable() {
		return "", gorm.ErrRecordNotFound
	}
	var st string
	res := s.db.Raw("SELECT status FROM "+reportTable(kind)+" WHERE id = ?", id).Scan(&st)
	if res.Error == nil && res.RowsAffected == 0 {
		return "", gorm.ErrRecordNotFound
	}
	return st, res.Error
}

func (s *store) UpdateProgress(kind string, id uint, done, total int) error {
	return s.db.Exec("UPDATE "+reportTable(kind)+" SET done_items = ?, total_items = ? WHERE id = ? AND status = 'running'", done, total, id).Error
}

func (s *store) InsertMSSQLItems(reportID uint, items []MSSQLItem) error {
	rows := make([]mssqlItemRow, len(items))
	opt := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	for i, it := range items {
		rows[i] = mssqlItemRow{ReportID: reportID, Section: it.Section, ItemKey: it.Key, ItemName: it.Name, DBName: it.DB, Level: it.Level,
			Value: it.Value, Threshold: it.Threshold, Detail: opt(it.Detail), RawCounter: opt(it.RawCounter), ElapsedMs: it.ElapsedMs}
	}
	return s.db.CreateInBatches(rows, 200).Error
}

func (s *store) FinishReport(kind string, id uint, f Finish) (bool, error) {
	tbl := reportTable(kind)
	at := f.At
	nullable := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	var res *gorm.DB
	switch {
	case f.Status != StatusFinish:
		// 失败 / 取消：只写状态与原因，进度保持最后一次更新的值
		res = s.db.Exec("UPDATE "+tbl+" SET status = ?, error_message = ?, finish_time = ? WHERE id = ? AND status = 'running'", f.Status, nullable(f.Error), at, id)
	case kind == KindMSSQL:
		res = s.db.Exec("UPDATE "+tbl+" SET status = 'finish', alert_count = ?, attention_count = ?, normal_count = ?, failed_count = ?, total_items = ?, done_items = ?, summary = ?, server_start_time = ?, error_message = NULL, finish_time = ? WHERE id = ? AND status = 'running'",
			f.Alert, f.Attention, f.Normal, f.Failed, f.Total, f.Done, nullable(f.Summary), f.ServerStart, at, id)
	default:
		res = s.db.Exec("UPDATE "+tbl+" SET status = 'finish', health_score = ?, alert_count = ?, attention_count = ?, failed_count = ?, total_items = ?, done_items = ?, result = ?, error_message = NULL, finish_time = ? WHERE id = ? AND status = 'running'",
			f.HealthScore, f.Alert, f.Attention, f.Failed, f.Total, f.Done, nullable(f.Result), at, id)
	}
	return res.RowsAffected > 0, res.Error
}

func (s *store) CancelReports(refs []ReportRef, by string, now time.Time) ([]ReportRef, error) {
	var done []ReportRef
	for _, r := range refs {
		res := s.db.Exec("UPDATE "+reportTable(r.Kind)+" SET status = 'cancelled', error_message = ?, finish_time = ? WHERE id = ? AND status = 'running'", by, now, r.ID)
		if res.Error != nil {
			return done, res.Error
		}
		if res.RowsAffected > 0 {
			done = append(done, r)
		}
	}
	return done, nil
}

func (s *store) DeleteReports(refs []ReportRef) (deleted, running []ReportRef, err error) {
	for _, kind := range s.kinds() {
		var ids []uint
		for _, r := range refs {
			if r.Kind == kind {
				ids = append(ids, r.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		tbl := reportTable(kind)
		type st struct {
			ID     uint
			Status string
		}
		var rows []st
		if err := s.db.Raw("SELECT id, status FROM "+tbl+" WHERE id IN ?", ids).Scan(&rows).Error; err != nil {
			return deleted, running, err
		}
		var del []uint
		for _, r := range rows {
			if r.Status == StatusRunning {
				running = append(running, ReportRef{kind, r.ID})
			} else {
				del = append(del, r.ID)
			}
		}
		if len(del) == 0 {
			continue
		}
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if kind == KindMSSQL { // 库里有外键级联；没有外键的环境也要删干净
				if err := tx.Exec("DELETE FROM dbt_sql_mssql_inspection_item WHERE report_id IN ?", del).Error; err != nil {
					return err
				}
			}
			if err := tx.Exec("DELETE FROM "+tbl+" WHERE id IN ? AND status <> 'running'", del).Error; err != nil {
				return err
			}
			if s.hasPlanTables() {
				return tx.Exec("DELETE FROM dbt_inspect_run_item WHERE report_kind = ? AND report_id IN ?", kind, del).Error
			}
			return nil
		})
		if err != nil {
			return deleted, running, err
		}
		for _, id := range del {
			deleted = append(deleted, ReportRef{kind, id})
		}
	}
	return deleted, running, nil
}

func (s *store) hasPlanTables() bool {
	if s.planTables {
		return true
	}
	m := s.db.Migrator()
	if m.HasTable("dbt_inspect_plan") && m.HasTable("dbt_inspect_plan_instance") && m.HasTable("dbt_inspect_run") && m.HasTable("dbt_inspect_run_item") {
		s.planTables = true
	}
	return s.planTables
}

func (s *store) PlanTablesReady() bool { return s.hasPlanTables() }

var errRedisTable = &Error{Status: 409, Code: "table_missing", Message: "Redis 巡检的表还没有创建，请先在数据库里执行 deploy/sql/131_redis_inspect.sql"}

func (s *store) hasRedisTable() bool {
	if s.redisTable {
		return true
	}
	if s.db.Migrator().HasTable("dbt_inspect_redis_report") {
		s.redisTable = true
	}
	return s.redisTable
}

// kinds 是当前可查询的报告种类：Redis 报告表没创建时不参与，保证旧库上页面照常工作。
func (s *store) kinds() []string {
	if s.hasRedisTable() {
		return []string{KindMySQL, KindMSSQL, KindRedis}
	}
	return []string{KindMySQL, KindMSSQL}
}

const summaryCols = `r.id, r.instance_id, i.instance_name, i.db_type, r.status, r.trigger_type, r.operator, r.alert_count, r.attention_count, r.failed_count, r.total_items, r.done_items,
 r.start_time, r.finish_time, LEFT(IFNULL(r.error_message, ''), 200) AS error_message`

// reportSelect 返回某种报告的列表查询（不含 WHERE）。
func (s *store) reportSelect(kind string) string {
	var sel string
	if kind == KindMSSQL {
		sel = "SELECT 'mssql' AS kind, " + summaryCols + ", NULL AS health_score, r.normal_count"
	} else {
		sel = "SELECT '" + kind + "' AS kind, " + summaryCols + ", r.health_score AS health_score, 0 AS normal_count"
	}
	if s.hasPlanTables() {
		sel += ", COALESCE(pr.plan_id, 0) AS plan_id, COALESCE(pr.plan_name, '') AS plan_name"
	} else {
		sel += ", 0 AS plan_id, '' AS plan_name"
	}
	sel += " FROM " + reportTable(kind) + " r JOIN dbt_sql_instance i ON i.id = r.instance_id"
	if s.hasPlanTables() {
		sel += " LEFT JOIN (SELECT ri.report_kind, ri.report_id, ru.plan_id, ru.plan_name FROM dbt_inspect_run_item ri JOIN dbt_inspect_run ru ON ru.id = ri.run_id) pr ON pr.report_kind = '" + kind + "' AND pr.report_id = r.id"
	}
	return sel
}

func likeEscape(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

func (s *store) reportWhere(kind string, f ReportFilter) (string, []any) {
	var w []string
	var args []any
	add := func(cond string, a ...any) { w = append(w, cond); args = append(args, a...) }
	if f.InstanceID != 0 {
		add("r.instance_id = ?", f.InstanceID)
	}
	if !f.AllInst {
		add("r.instance_id IN ?", f.InstanceIDs)
	}
	switch f.Level {
	case "alert":
		add("r.alert_count > 0")
	case "attention":
		add("r.alert_count = 0 AND r.attention_count > 0")
	case "clean":
		add("r.status = 'finish' AND r.alert_count = 0 AND r.attention_count = 0")
	case "failed":
		add("r.status = 'failed'")
	case "cancelled":
		add("r.status = 'cancelled'")
	}
	if f.Trigger != "" {
		add("r.trigger_type = ?", f.Trigger)
	}
	if f.Running {
		add("r.status = 'running'")
	}
	if f.Latest { // 每个实例最近一次结束的报告：先选出每个实例最大的报告 ID（只算成功和失败的），再去套其它条件
		add("r.id IN (SELECT MAX(l.id) FROM " + reportTable(kind) + " l WHERE l.status IN ('finish', 'failed') GROUP BY l.instance_id)")
	}
	if f.Start != nil {
		add("r.start_time >= ?", *f.Start)
	}
	if f.End != nil {
		add("r.start_time < ?", *f.End)
	}
	if f.Search != "" {
		add("i.instance_name LIKE ? ESCAPE '!'", "%"+likeEscape(f.Search)+"%")
	}
	if (f.PlanID != 0 || f.RunID != 0) && s.hasPlanTables() {
		cond := "EXISTS (SELECT 1 FROM dbt_inspect_run_item ri JOIN dbt_inspect_run ru ON ru.id = ri.run_id WHERE ri.report_kind = '" + kind + "' AND ri.report_id = r.id"
		var a []any
		if f.PlanID != 0 {
			cond += " AND ru.plan_id = ?"
			a = append(a, f.PlanID)
		}
		if f.RunID != 0 {
			cond += " AND ru.id = ?"
			a = append(a, f.RunID)
		}
		add(cond+")", a...)
	}
	if len(w) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(w, " AND "), args
}

func (s *store) ListReports(f ReportFilter) ([]ReportSummary, int64, error) {
	var total int64
	var unions []string
	var args []any
	for _, kind := range s.kinds() {
		where, a := s.reportWhere(kind, f)
		var n int64
		cnt := "SELECT COUNT(*) FROM " + reportTable(kind) + " r JOIN dbt_sql_instance i ON i.id = r.instance_id" + where
		if err := s.db.Raw(cnt, a...).Scan(&n).Error; err != nil {
			return nil, 0, err
		}
		total += n
		unions = append(unions, "("+s.reportSelect(kind)+where+")")
		args = append(args, a...)
	}
	q := strings.Join(unions, " UNION ALL ") + " ORDER BY start_time DESC, kind DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, f.Limit, f.Offset)
	var out []ReportSummary
	if err := s.db.Raw(q, args...).Scan(&out).Error; err != nil {
		return nil, 0, err
	}
	if out == nil {
		out = []ReportSummary{}
	}
	return out, total, nil
}

func (s *store) ReportBrief(kind string, id uint) (*ReportSummary, error) {
	if kind == KindRedis && !s.hasRedisTable() {
		return nil, nil
	}
	var out []ReportSummary
	if err := s.db.Raw(s.reportSelect(kind)+" WHERE r.id = ?", id).Scan(&out).Error; err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

type detailScan struct {
	ReportSummary
	Result      *string    `gorm:"column:result"`
	Summary     *string    `gorm:"column:summary"`
	ServerStart *time.Time `gorm:"column:server_start_time"`
}

func (s *store) GetReport(kind string, id uint) (*ReportDetail, error) {
	if kind == KindRedis && !s.hasRedisTable() {
		return nil, nil
	}
	extra := ", r.result AS result, NULL AS summary, NULL AS server_start_time"
	if kind == KindMSSQL {
		extra = ", NULL AS result, r.summary AS summary, r.server_start_time AS server_start_time"
	}
	q := s.reportSelect(kind)
	// 在 SELECT 列表末尾（FROM 之前）补上大字段；详情页不需要截断错误信息
	i := strings.Index(q, " FROM ")
	q = q[:i] + extra + q[i:] + " WHERE r.id = ?"
	var rows []detailScan
	if err := s.db.Raw(q, id).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	d := &ReportDetail{ReportSummary: rows[0].ReportSummary, ServerStart: rows[0].ServerStart}
	if rows[0].Result != nil {
		d.Result = *rows[0].Result
	}
	if rows[0].Summary != nil {
		d.Summary = *rows[0].Summary
	}
	var full struct{ Err *string }
	if err := s.db.Raw("SELECT error_message AS err FROM "+reportTable(kind)+" WHERE id = ?", id).Scan(&full).Error; err == nil && full.Err != nil {
		d.Error = *full.Err
	}
	if kind == KindMSSQL {
		var items []mssqlItemRow
		if err := s.db.Where("report_id = ?", id).Order("id").Find(&items).Error; err != nil {
			return nil, err
		}
		for _, it := range items {
			m := MSSQLItem{Section: it.Section, Key: it.ItemKey, Name: it.ItemName, DB: it.DBName, Level: it.Level, Value: it.Value, Threshold: it.Threshold, ElapsedMs: it.ElapsedMs}
			if it.Detail != nil && *it.Detail != "null" {
				m.Detail = *it.Detail
			}
			d.Items = append(d.Items, m)
		}
	}
	return d, nil
}

func (s *store) PrevMSSQLRaw(instanceID, excludeID uint) (*PrevRaw, error) {
	var rows []struct {
		ID          uint
		StartTime   time.Time
		ServerStart *time.Time `gorm:"column:server_start_time"`
	}
	if err := s.db.Raw("SELECT id, start_time, server_start_time FROM dbt_sql_mssql_inspection_report WHERE instance_id = ? AND status = 'finish' AND id <> ? ORDER BY id DESC LIMIT 1", instanceID, excludeID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := &PrevRaw{ReportID: rows[0].ID, StartTime: rows[0].StartTime, ServerStart: rows[0].ServerStart, Raw: map[string]string{}}
	var items []struct {
		ItemKey    string  `gorm:"column:item_key"`
		DBName     string  `gorm:"column:db_name"`
		RawCounter *string `gorm:"column:raw_counter"`
	}
	if err := s.db.Raw("SELECT item_key, db_name, raw_counter FROM dbt_sql_mssql_inspection_item WHERE report_id = ? AND raw_counter IS NOT NULL", rows[0].ID).Scan(&items).Error; err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.RawCounter != nil && *it.RawCounter != "null" {
			out.Raw[it.ItemKey+"|"+it.DBName] = *it.RawCounter
		}
	}
	return out, nil
}

func (s *store) PlanReportRefsBefore(planID uint, cutoff time.Time, limit int) ([]ReportRef, error) {
	var out []ReportRef
	for _, kind := range s.kinds() {
		var ids []uint
		if err := s.db.Raw("SELECT r.id FROM "+reportTable(kind)+" r JOIN dbt_inspect_run_item ri ON ri.report_kind = ? AND ri.report_id = r.id JOIN dbt_inspect_run ru ON ru.id = ri.run_id "+
			"WHERE ru.plan_id = ? AND r.start_time < ? AND r.status <> 'running' LIMIT ?", kind, planID, cutoff, limit).Scan(&ids).Error; err != nil {
			return out, err
		}
		for _, id := range ids {
			out = append(out, ReportRef{kind, id})
		}
	}
	return out, nil
}

func (s *store) UnlinkedReportRefsBefore(kind string, cutoff time.Time, limit int) ([]ReportRef, error) {
	if kind == KindRedis && !s.hasRedisTable() {
		return nil, nil
	}
	q := "SELECT r.id FROM " + reportTable(kind) + " r WHERE r.start_time < ? AND r.status <> 'running'"
	if s.hasPlanTables() {
		q += " AND NOT EXISTS (SELECT 1 FROM dbt_inspect_run_item ri WHERE ri.report_kind = '" + kind + "' AND ri.report_id = r.id)"
	}
	var ids []uint
	if err := s.db.Raw(q+" LIMIT ?", cutoff, limit).Scan(&ids).Error; err != nil {
		return nil, err
	}
	out := make([]ReportRef, len(ids))
	for i, id := range ids {
		out[i] = ReportRef{kind, id}
	}
	return out, nil
}

// SlowTop 汇总平台采集的慢日志（dbt_slow_logs 表）。
func (s *store) SlowTop(instanceID uint, since time.Time, limit int) (map[string]any, bool) {
	if !s.slowTable {
		if !s.db.Migrator().HasTable("dbt_slow_logs") {
			return nil, false
		}
		s.slowTable = true
	}
	var total, fps int64
	if err := s.db.Raw("SELECT COUNT(*) FROM dbt_slow_logs WHERE instance_id = ? AND start_time >= ?", instanceID, since).Scan(&total).Error; err != nil || total == 0 {
		return nil, false
	}
	if err := s.db.Raw("SELECT COUNT(DISTINCT sql_md5) FROM dbt_slow_logs WHERE instance_id = ? AND start_time >= ? AND sql_md5 IS NOT NULL", instanceID, since).Scan(&fps).Error; err != nil {
		return nil, false
	}
	var rows []struct {
		Cnt          int64
		TotalSec     float64
		AvgSec       float64
		MaxSec       float64
		RowsExamined float64
		DB           *string
		Sample       *string
		LastSeen     *time.Time
	}
	if err := s.db.Raw("SELECT COUNT(*) AS cnt, SUM(query_time_sec) AS total_sec, AVG(query_time_sec) AS avg_sec, MAX(query_time_sec) AS max_sec, AVG(rows_examined) AS rows_examined, MAX(db_name) AS db, MAX(sql_text) AS sample, MAX(start_time) AS last_seen "+
		"FROM dbt_slow_logs WHERE instance_id = ? AND start_time >= ? AND sql_md5 IS NOT NULL GROUP BY sql_md5 ORDER BY total_sec DESC LIMIT ?", instanceID, since, limit).Scan(&rows).Error; err != nil {
		return nil, false
	}
	items := []any{}
	for _, r := range rows {
		it := map[string]any{"count": r.Cnt, "total_seconds": round2(r.TotalSec), "avg_seconds": round3(r.AvgSec), "max_seconds": round3(r.MaxSec), "avg_rows_examined": int64(r.RowsExamined), "db": "", "sql": "", "last_seen": ""}
		if r.DB != nil {
			it["db"] = *r.DB
		}
		if r.Sample != nil {
			it["sql"] = truncate(fingerprint(*r.Sample), 300)
		}
		if r.LastSeen != nil {
			it["last_seen"] = r.LastSeen.Format("2006-01-02 15:04:05")
		}
		items = append(items, it)
	}
	return map[string]any{"source": "slow_log", "days": slowTopDays, "slow_count": total, "fingerprint_count": fps, "items": items}, true
}

// ---- 计划 ----

func csvUints(s string) []uint {
	var out []uint
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 32); err == nil {
			out = append(out, uint(n))
		}
	}
	return out
}

func joinUints(v []uint) string {
	p := make([]string, len(v))
	for i, n := range v {
		p[i] = strconv.FormatUint(uint64(n), 10)
	}
	return strings.Join(p, ",")
}

func rowToPlan(r planRow, ids []uint) Plan {
	p := Plan{ID: r.ID, Name: r.Name, Enabled: r.Enabled, Kind: r.ScheduleKind, Time: r.ScheduleTime, MonthDay: r.ScheduleMonthday, Cron: r.CronExpr,
		InstanceIDs: ids, Concurrency: r.Concurrency, StaggerSeconds: r.StaggerSeconds, SkipBusy: r.SkipBusy, RetryOnce: r.RetryOnce, TimeoutMinutes: r.TimeoutMinutes,
		NotifyMode: r.NotifyMode, NotifyChannels: csvUints(r.NotifyChannels), RetentionDays: r.RetentionDays, NextRunAt: r.NextRunAt, LastRunAt: r.LastRunAt,
		LastRunID: r.LastRunID, CreatedBy: r.CreatedBy, CreateTime: r.CreateTime, SysTime: r.SysTime}
	for _, d := range csvUints(r.ScheduleWeekdays) {
		p.Weekdays = append(p.Weekdays, int(d))
	}
	for _, t := range strings.Split(r.TagCodes, ",") {
		if t = strings.TrimSpace(t); t != "" {
			p.TagCodes = append(p.TagCodes, t)
		}
	}
	if r.MSSQLDatabases != "" {
		_ = json.Unmarshal([]byte(r.MSSQLDatabases), &p.Databases)
	}
	return p
}

func planToRow(p *Plan) planRow {
	days := make([]string, len(p.Weekdays))
	for i, d := range p.Weekdays {
		days[i] = strconv.Itoa(d)
	}
	dbs := ""
	if len(p.Databases) > 0 {
		b, _ := json.Marshal(p.Databases)
		dbs = string(b)
	}
	return planRow{ID: p.ID, Name: p.Name, Enabled: p.Enabled, ScheduleKind: p.Kind, ScheduleTime: p.Time, ScheduleWeekdays: strings.Join(days, ","), ScheduleMonthday: p.MonthDay,
		CronExpr: p.Cron, TagCodes: strings.Join(p.TagCodes, ","), MSSQLDatabases: dbs, Concurrency: p.Concurrency, StaggerSeconds: p.StaggerSeconds, SkipBusy: p.SkipBusy,
		RetryOnce: p.RetryOnce, TimeoutMinutes: p.TimeoutMinutes, NotifyMode: p.NotifyMode, NotifyChannels: joinUints(p.NotifyChannels), RetentionDays: p.RetentionDays,
		NextRunAt: p.NextRunAt, LastRunAt: p.LastRunAt, LastRunID: p.LastRunID, CreatedBy: p.CreatedBy, CreateTime: p.CreateTime, SysTime: p.SysTime}
}

func (s *store) planInstances(planIDs ...uint) (map[uint][]uint, error) {
	var rows []planInstanceRow
	q := s.db.Order("plan_id, instance_id")
	if len(planIDs) > 0 {
		q = q.Where("plan_id IN ?", planIDs)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[uint][]uint{}
	for _, r := range rows {
		out[r.PlanID] = append(out[r.PlanID], r.InstanceID)
	}
	return out, nil
}

func (s *store) ListPlans() ([]Plan, error) {
	var rows []planRow
	if err := s.db.Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	ids, err := s.planInstances()
	if err != nil {
		return nil, err
	}
	out := make([]Plan, len(rows))
	for i, r := range rows {
		out[i] = rowToPlan(r, ids[r.ID])
	}
	return out, nil
}

func (s *store) getPlanWhere(cond string, arg any) (*Plan, error) {
	var rows []planRow
	if err := s.db.Where(cond, arg).Limit(1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	ids, err := s.planInstances(rows[0].ID)
	if err != nil {
		return nil, err
	}
	p := rowToPlan(rows[0], ids[rows[0].ID])
	return &p, nil
}

func (s *store) GetPlan(id uint) (*Plan, error)        { return s.getPlanWhere("id = ?", id) }
func (s *store) PlanByName(name string) (*Plan, error) { return s.getPlanWhere("name = ?", name) }

func (s *store) SavePlan(p *Plan) (uint, error) {
	row := planToRow(p)
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		if row.ID == 0 {
			err = tx.Create(&row).Error
		} else {
			err = tx.Save(&row).Error
		}
		if err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM dbt_inspect_plan_instance WHERE plan_id = ?", row.ID).Error; err != nil {
			return err
		}
		if len(p.InstanceIDs) > 0 {
			links := make([]planInstanceRow, len(p.InstanceIDs))
			for i, id := range p.InstanceIDs {
				links[i] = planInstanceRow{PlanID: row.ID, InstanceID: id}
			}
			return tx.Create(&links).Error
		}
		return nil
	})
	return row.ID, err
}

func (s *store) DeletePlan(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		for _, q := range []string{
			"DELETE FROM dbt_inspect_run_item WHERE run_id IN (SELECT id FROM dbt_inspect_run WHERE plan_id = ?)",
			"DELETE FROM dbt_inspect_run WHERE plan_id = ?",
			"DELETE FROM dbt_inspect_plan_instance WHERE plan_id = ?",
			"DELETE FROM dbt_inspect_plan WHERE id = ?",
		} {
			if err := tx.Exec(q, id).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *store) SetPlanEnabled(id uint, enabled bool, next *time.Time) error {
	return s.db.Exec("UPDATE dbt_inspect_plan SET enabled = ?, next_run_at = ?, sys_time = ? WHERE id = ?", enabled, next, time.Now(), id).Error
}

func (s *store) DuePlans(now time.Time, limit int) ([]Plan, error) {
	var rows []planRow
	if err := s.db.Where("enabled = 1 AND (next_run_at IS NULL OR next_run_at <= ?)", now).Order("next_run_at").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Plan, 0, len(rows))
	for _, r := range rows {
		ids, err := s.planInstances(r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, rowToPlan(r, ids[r.ID]))
	}
	return out, nil
}

// ClaimPlan 用一条带条件的 UPDATE 做原子占位：只有 next_run_at 仍等于 expected 的那个进程能改成功。
func (s *store) ClaimPlan(id uint, expected time.Time, next *time.Time, now time.Time) (bool, error) {
	res := s.db.Exec("UPDATE dbt_inspect_plan SET next_run_at = ?, sys_time = ? WHERE id = ? AND enabled = 1 AND next_run_at = ?", next, now, id, expected)
	return res.RowsAffected > 0, res.Error
}

func (s *store) MarkPlanRun(id uint, runID uint, at time.Time) error {
	return s.db.Exec("UPDATE dbt_inspect_plan SET last_run_at = ?, last_run_id = ? WHERE id = ?", at, runID, id).Error
}

func rowToRun(r runRow) Run {
	return Run{ID: r.ID, PlanID: r.PlanID, PlanName: r.PlanName, Trigger: r.TriggerType, Operator: r.Operator, ScheduledAt: r.ScheduledAt, StartedAt: r.StartedAt,
		FinishedAt: r.FinishedAt, Status: r.Status, Total: r.Total, OK: r.OK, Failed: r.Failed, Skipped: r.Skipped, Alert: r.Alert, Attention: r.Attention, Note: r.Note, Heartbeat: r.HeartbeatAt}
}

func runToRow(r *Run) runRow {
	return runRow{ID: r.ID, PlanID: r.PlanID, PlanName: r.PlanName, TriggerType: r.Trigger, Operator: r.Operator, ScheduledAt: r.ScheduledAt, StartedAt: r.StartedAt,
		FinishedAt: r.FinishedAt, Status: r.Status, Total: r.Total, OK: r.OK, Failed: r.Failed, Skipped: r.Skipped, Alert: r.Alert, Attention: r.Attention, Note: r.Note, HeartbeatAt: r.Heartbeat}
}

func (s *store) CreateRun(r *Run) (uint, error) {
	row := runToRow(r)
	if err := s.db.Create(&row).Error; err != nil {
		return 0, err
	}
	return row.ID, nil
}

func (s *store) UpdateRun(r *Run) error {
	row := runToRow(r)
	return s.db.Save(&row).Error
}

func (s *store) TouchRun(id uint, now time.Time) error {
	return s.db.Exec("UPDATE dbt_inspect_run SET heartbeat_at = ? WHERE id = ? AND status = 'running'", now, id).Error
}

func (s *store) GetRun(id uint) (*Run, error) {
	var rows []runRow
	if err := s.db.Where("id = ?", id).Limit(1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rowToRun(rows[0])
	return &r, nil
}

func (s *store) ListRuns(planID uint, limit int) ([]Run, error) {
	var rows []runRow
	if err := s.db.Where("plan_id = ?", planID).Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Run, len(rows))
	for i, r := range rows {
		out[i] = rowToRun(r)
	}
	return out, nil
}

func (s *store) ActiveRun(planID uint, freshAfter time.Time) (*Run, error) {
	var rows []runRow
	if err := s.db.Where("plan_id = ? AND status = 'running' AND heartbeat_at >= ?", planID, freshAfter).Limit(1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rowToRun(rows[0])
	return &r, nil
}

func (s *store) FailStaleRuns(before time.Time, now time.Time) (int, error) {
	res := s.db.Exec("UPDATE dbt_inspect_run SET status = 'failed', finished_at = ?, note = ? WHERE status = 'running' AND heartbeat_at < ?", now, "运行中断（进程退出或失联）", before)
	return int(res.RowsAffected), res.Error
}

func (s *store) AddRunItem(it *RunItem) error {
	row := runItemRow{RunID: it.RunID, InstanceID: it.InstanceID, InstanceName: it.InstanceName, ReportKind: it.Kind, ReportID: it.ReportID, Status: it.Status,
		Alert: it.Alert, Attention: it.Attention, Message: it.Message}
	if err := s.db.Create(&row).Error; err != nil {
		return err
	}
	it.ID = row.ID
	return nil
}

func (s *store) RunItems(runID uint) ([]RunItem, error) {
	var rows []runItemRow
	if err := s.db.Where("run_id = ?", runID).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RunItem, len(rows))
	for i, r := range rows {
		out[i] = RunItem{ID: r.ID, RunID: r.RunID, InstanceID: r.InstanceID, InstanceName: r.InstanceName, Kind: r.ReportKind, ReportID: r.ReportID, Status: r.Status,
			Alert: r.Alert, Attention: r.Attention, Message: r.Message}
	}
	return out, nil
}

func (s *store) DeleteRunsBefore(planID uint, cutoff time.Time) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM dbt_inspect_run_item WHERE run_id IN (SELECT id FROM (SELECT id FROM dbt_inspect_run WHERE plan_id = ? AND started_at < ? AND status <> 'running') x)", planID, cutoff).Error; err != nil {
			return err
		}
		return tx.Exec("DELETE FROM dbt_inspect_run WHERE plan_id = ? AND started_at < ? AND status <> 'running'", planID, cutoff).Error
	})
}

// ---- 配置 / 审计 ----

func (s *store) ConfigValue(item string) (string, bool) {
	return settings.Open(s.db, s.m).Value(item)
}

func (s *store) InsertAudit(e *model.AuditEntry) error { return s.db.Create(e).Error }

// DisabledInstanceIDs 返回在实例管理里被停用的实例（旁路表 dbt_instance_ext；表不存在返回空集合）。
func (s *store) DisabledInstanceIDs() (map[uint]bool, error) { return instext.DisabledSet(s.db) }
