// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package slowlog

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	drv "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"dbtool/internal/crypto"
	"dbtool/internal/instext"
	"dbtool/internal/model"
	dbstore "dbtool/internal/store"
)

// store 是 Repo 的 GORM 实现。实例、权限相关的方法复用 internal/store 的实现（嵌入），
// 其余是慢日志自己的表：dbt_slow_logs（平台采集）、dbt_mysql_slow_query_review(_history)（pt-query-digest 推送）、
// dbt_slowlog_collect_state（采集状态，本模块新表）。
type store struct {
	*dbstore.Store
	db *gorm.DB

	mu          sync.Mutex
	digestOK    bool
	digestCheck time.Time
	digestHost  map[string]hostCache // host:port -> 历史表里是否有该实例的数据（缓存 1 分钟）
	redisOK     bool                 // dbt_redis_slowlog 与状态表已创建（只缓存“存在”）
}

func newStore(db *gorm.DB, m *crypto.Mirage) *store {
	return &store{Store: &dbstore.Store{DB: db, Mirage: m}, db: db}
}

const tsLayout = "2006-01-02 15:04:05"

// isNoTable 判断是否“表不存在”（MySQL 错误 1146）。
func isNoTable(err error) bool {
	var me *drv.MySQLError
	return errors.As(err, &me) && me.Number == 1146
}

// ListInstances 只取慢日志页面需要的列（不读账号密码）。
func (s *store) ListInstances() ([]model.Instance, error) {
	var out []model.Instance
	err := instext.ExcludeDisabled(s.db.Select("id, instance_name, type, db_type, host, port, is_enable, tunnel_id"), "dbt_sql_instance.id").Order("instance_name").Find(&out).Error
	return out, err
}

func (s *store) InsertAudit(e *model.AuditEntry) error { return s.db.Create(e).Error }

// ---- dbt_slow_logs ----

type groupRow struct {
	InstanceID   uint    `gorm:"column:instance_id"`
	InstanceName string  `gorm:"column:instance_name"`
	DBName       string  `gorm:"column:db_name"`
	SQLMD5       string  `gorm:"column:sql_md5"`
	ExecDate     string  `gorm:"column:exec_date"`
	Cnt          int64   `gorm:"column:cnt"`
	UserHost     string  `gorm:"column:user_host"`
	SQLText      string  `gorm:"column:sql_text"`
	QtAvg        float64 `gorm:"column:qt_avg"`
	QtMax        float64 `gorm:"column:qt_max"`
	QtMin        float64 `gorm:"column:qt_min"`
	QtSum        float64 `gorm:"column:qt_sum"`
	RsAvg        float64 `gorm:"column:rs_avg"`
	RsMax        float64 `gorm:"column:rs_max"`
	RsMin        float64 `gorm:"column:rs_min"`
	ReAvg        float64 `gorm:"column:re_avg"`
	ReMax        float64 `gorm:"column:re_max"`
	ReMin        float64 `gorm:"column:re_min"`
}

func (s *store) SlowLogGroups(q GroupQuery) ([]SlowLogGroup, int64, error) {
	listSQL, listArgs, countSQL, countArgs := buildGroups(q)
	var total int64
	if err := s.db.Raw(countSQL, countArgs...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	if total == 0 || int64(q.Offset) >= total {
		return nil, total, nil
	}
	var rows []groupRow
	if err := s.db.Raw(listSQL, listArgs...).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]SlowLogGroup, 0, len(rows))
	for _, r := range rows {
		out = append(out, SlowLogGroup{InstanceID: r.InstanceID, InstanceName: r.InstanceName, DBName: r.DBName, SQLMD5: r.SQLMD5,
			ExecDate: r.ExecDate, Count: r.Cnt, UserHost: r.UserHost, SQLText: r.SQLText,
			QueryTimeAvg: round(r.QtAvg, 3), QueryTimeMax: round(r.QtMax, 3), QueryTimeMin: round(r.QtMin, 3), QueryTimeSum: round(r.QtSum, 3),
			RowsSentAvg: int64(r.RsAvg), RowsSentMax: int64(r.RsMax), RowsSentMin: int64(r.RsMin),
			RowsExaminedAvg: int64(r.ReAvg), RowsExaminedMax: int64(r.ReMax), RowsExaminedMin: int64(r.ReMin)})
	}
	return out, total, nil
}

func (s *store) SlowLogDetail(q DetailQuery) ([]SlowLogRow, error) {
	query, args := buildDetail(q)
	var rows []SlowLogRow
	err := s.db.Raw(query, args...).Scan(&rows).Error
	return rows, err
}

type reviewRow struct {
	SQLID          string  `gorm:"column:sql_id"`
	LastID         uint    `gorm:"column:last_id"`
	CreateTime     string  `gorm:"column:create_time"`
	DBName         string  `gorm:"column:db_name"`
	ExecCount      float64 `gorm:"column:exec_count"`
	ExecTimeSum    float64 `gorm:"column:exec_time_sum"`
	QueryTimeAvg   float64 `gorm:"column:query_time_avg"`
	ParseTotalRows float64 `gorm:"column:parse_total_rows"`
	ReturnTotal    float64 `gorm:"column:return_total_rows"`
	ParseRowAvg    float64 `gorm:"column:parse_row_avg"`
	ReturnRowAvg   float64 `gorm:"column:return_row_avg"`
	InstanceID     uint    `gorm:"column:instance_id"` // 平台采集来源
	HostKey        string  `gorm:"column:host_key"`    // pt-query-digest 来源
	FirstSeen      string  `gorm:"column:first_seen"`
	QueryTimeMax   float64 `gorm:"column:query_time_max"`
	LockTimeAvg    float64 `gorm:"column:lock_time_avg"`
}

func (r reviewRow) toRow() ReviewRow {
	return ReviewRow{SQLID: r.SQLID, CreateTime: r.CreateTime, DBName: r.DBName, ExecCount: int64(r.ExecCount),
		ExecTimeSum: round(r.ExecTimeSum, 6), QueryTimeAvg: round(r.QueryTimeAvg, 6), ParseTotalRows: int64(r.ParseTotalRows),
		ReturnTotal: int64(r.ReturnTotal), ParseRowAvg: int64(r.ParseRowAvg), ReturnRowAvg: int64(r.ReturnRowAvg),
		InstanceID: r.InstanceID, HostKey: r.HostKey, FirstSeen: r.FirstSeen, LastSeen: r.CreateTime,
		QueryTimeMax: round(r.QueryTimeMax, 6), LockTimeAvg: round(r.LockTimeAvg, 6)}
}

func (s *store) CollectedReview(q ReviewQuery) ([]ReviewRow, int64, error) {
	listSQL, listArgs, countSQL, countArgs := buildCollectedReview(q)
	var total int64
	if err := s.db.Raw(countSQL, countArgs...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	if total == 0 || int64(q.Offset) >= total {
		return nil, total, nil
	}
	var rows []reviewRow
	if err := s.db.Raw(listSQL, listArgs...).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	ids := make([]uint, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.LastID)
	}
	samples := map[uint]string{}
	if len(ids) > 0 {
		var ss []struct {
			ID      uint   `gorm:"column:id"`
			SQLText string `gorm:"column:sql_text"`
		}
		if err := s.db.Raw("SELECT id, COALESCE(sql_text,'') AS sql_text FROM "+tblSlowLogs+" WHERE id IN ("+placeholders(len(ids))+")", idArgs(ids)...).Scan(&ss).Error; err != nil {
			return nil, 0, err
		}
		for _, x := range ss {
			samples[x.ID] = x.SQLText
		}
	}
	out := make([]ReviewRow, 0, len(rows))
	for _, r := range rows {
		row := r.toRow()
		row.SQLText = samples[r.LastID] // 样例 SQL，指纹由服务层计算
		row.SampleSQL = row.SQLText
		out = append(out, row)
	}
	return out, total, nil
}

type historyRow struct {
	ID           uint    `gorm:"column:id"`
	SQLID        string  `gorm:"column:sql_id"`
	ExecStart    string  `gorm:"column:exec_start"`
	DBName       string  `gorm:"column:db_name"`
	HostAddress  string  `gorm:"column:host_address"`
	UserMax      string  `gorm:"column:user_max"`
	ClientMax    string  `gorm:"column:client_max"`
	SQLText      string  `gorm:"column:sql_text"`
	ExecCount    float64 `gorm:"column:exec_count"`
	QueryPct95   float64 `gorm:"column:query_time_pct95"`
	QueryTimeSum float64 `gorm:"column:query_time_sum"`
	LockTimeSum  float64 `gorm:"column:lock_time_sum"`
	ParseRows    float64 `gorm:"column:parse_rows"`
	ReturnRows   float64 `gorm:"column:return_rows"`
}

func (r historyRow) toRow(digest bool) HistoryRow {
	host := r.HostAddress
	if digest { // '用户'@'客户端'
		host = fmt.Sprintf("'%s'@'%s'", r.UserMax, r.ClientMax)
	}
	return HistoryRow{ID: r.ID, SQLID: r.SQLID, ExecStart: r.ExecStart, DBName: r.DBName, HostAddress: host, SQLText: r.SQLText,
		ExecCount: int64(r.ExecCount), QueryPct95: round(r.QueryPct95, 6), QueryTimeSum: round(r.QueryTimeSum, 6),
		LockTimeSum: round(r.LockTimeSum, 6), ParseRows: int64(r.ParseRows), ReturnRows: int64(r.ReturnRows)}
}

func (s *store) history(listSQL string, listArgs []any, countSQL string, countArgs []any, offset int, digest bool) ([]HistoryRow, int64, error) {
	var total int64
	if err := s.db.Raw(countSQL, countArgs...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	if total == 0 || int64(offset) >= total {
		return nil, total, nil
	}
	var rows []historyRow
	if err := s.db.Raw(listSQL, listArgs...).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]HistoryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toRow(digest))
	}
	return out, total, nil
}

func (s *store) CollectedHistory(q ReviewQuery) ([]HistoryRow, int64, error) {
	a, b, c, d := buildCollectedHistory(q)
	return s.history(a, b, c, d, q.Offset, false)
}

type trendAgg struct {
	T     string  `gorm:"column:t"`
	Cnt   float64 `gorm:"column:cnt"`
	Pct95 float64 `gorm:"column:pct95"`
	AvgT  float64 `gorm:"column:avg_t"`
	MaxT  float64 `gorm:"column:max_t"`
}

func (s *store) CollectedTrend(q TrendQuery) (*Trend, error) {
	aggSQL, aggArgs, rowsSQL, rowsArgs := buildCollectedTrend(q)
	var aggs []trendAgg
	if err := s.db.Raw(aggSQL, aggArgs...).Scan(&aggs).Error; err != nil {
		return nil, err
	}
	var samples []struct {
		T  string  `gorm:"column:t"`
		Qt float64 `gorm:"column:qt"`
	}
	if err := s.db.Raw(rowsSQL, rowsArgs...).Scan(&samples).Error; err != nil {
		return nil, err
	}
	tr := &Trend{Approx: len(samples) > maxTrendRows}
	if tr.Approx {
		samples = samples[:maxTrendRows]
	}
	byBucket := map[string][]float64{}
	for _, x := range samples {
		byBucket[x.T] = append(byBucket[x.T], x.Qt)
	}
	for _, a := range aggs {
		tr.Points = append(tr.Points, TrendPoint{T: a.T, Count: int64(a.Cnt), Pct95: percentile95(byBucket[a.T]),
			Avg: round(a.AvgT, 6), Max: round(a.MaxT, 6)})
	}
	return tr, nil
}

func (s *store) CollectedDatabases(instanceID uint, from, to string) ([]string, error) {
	q, args := buildCollectedDatabases(instanceID, from, to)
	var out []string
	err := s.db.Raw(q, args...).Scan(&out).Error
	return out, err
}

// ---- pt-query-digest 表 ----

// DigestAvailable 判断 pt-query-digest 的两张表是否存在（结果缓存 1 分钟）。
func (s *store) DigestAvailable() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.digestCheck.IsZero() && time.Since(s.digestCheck) < time.Minute {
		return s.digestOK, nil
	}
	m := s.db.Migrator()
	ok := m.HasTable(tblReview) && m.HasTable(tblReviewHist)
	s.digestOK, s.digestCheck = ok, time.Now()
	return ok, nil
}

// hostCache 是 DigestHasHost 的缓存项。
type hostCache struct {
	has bool
	at  time.Time
}

// DigestHasHost 判断 pt-query-digest 历史表里有没有该实例（hostname_max = host:port）的数据；结果缓存 1 分钟。
// 查询是 LIMIT 1 的存在性判断，走 (hostname_max, ts_min) 索引。
func (s *store) DigestHasHost(hostKey string) (bool, error) {
	s.mu.Lock()
	if c, ok := s.digestHost[hostKey]; ok && time.Since(c.at) < time.Minute {
		s.mu.Unlock()
		return c.has, nil
	}
	s.mu.Unlock()
	var n int
	row := s.db.Raw("SELECT 1 FROM "+tblReviewHist+" WHERE hostname_max = ? LIMIT 1", hostKey).Row()
	if err := row.Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	has := n == 1
	s.mu.Lock()
	if s.digestHost == nil {
		s.digestHost = map[string]hostCache{}
	}
	s.digestHost[hostKey] = hostCache{has: has, at: time.Now()}
	s.mu.Unlock()
	return has, nil
}

func (s *store) DigestReview(q ReviewQuery) ([]ReviewRow, int64, error) {
	listSQL, listArgs, countSQL, countArgs := buildDigestReview(q)
	var total int64
	if err := s.db.Raw(countSQL, countArgs...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	if total == 0 || int64(q.Offset) >= total {
		return nil, total, nil
	}
	var rows []reviewRow
	if err := s.db.Raw(listSQL, listArgs...).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	ids := make([]any, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.SQLID)
	}
	fps := map[string]string{}
	if len(ids) > 0 {
		var ss []struct {
			Checksum    string `gorm:"column:checksum"`
			Fingerprint string `gorm:"column:fingerprint"`
		}
		if err := s.db.Raw("SELECT checksum, fingerprint FROM "+tblReview+" WHERE checksum IN ("+placeholders(len(ids))+")", ids...).Scan(&ss).Error; err != nil {
			return nil, 0, err
		}
		for _, x := range ss {
			fps[x.Checksum] = x.Fingerprint
		}
	}
	out := make([]ReviewRow, 0, len(rows))
	for _, r := range rows {
		row := r.toRow()
		row.SQLText = fps[r.SQLID]
		out = append(out, row)
	}
	return out, total, nil
}

func (s *store) DigestHistory(q ReviewQuery) ([]HistoryRow, int64, error) {
	a, b, c, d := buildDigestHistory(q)
	return s.history(a, b, c, d, q.Offset, true)
}

func (s *store) DigestTrend(q TrendQuery) (*Trend, error) {
	query, args := buildDigestTrend(q)
	var aggs []trendAgg
	if err := s.db.Raw(query, args...).Scan(&aggs).Error; err != nil {
		return nil, err
	}
	tr := &Trend{}
	for _, a := range aggs {
		tr.Points = append(tr.Points, TrendPoint{T: a.T, Count: int64(a.Cnt), Pct95: round(a.Pct95, 6), Avg: round(a.AvgT, 6), Max: round(a.MaxT, 6)})
	}
	return tr, nil
}

func (s *store) DigestDatabases(hostKey, from, to string) ([]string, error) {
	q, args := buildDigestDatabases(hostKey, from, to)
	var out []string
	err := s.db.Raw(q, args...).Scan(&out).Error
	return out, err
}

// ---- 采集器 ----

// collectDBTypes 是采集器按实例类型过滤的类型清单。
var collectDBTypes = []string{"mysql", "selectdb", "doris", "polardb", "polardb-x"}

func (s *store) CollectInstances() ([]model.Instance, error) {
	var out []model.Instance
	// 实例管理里被停用的实例（旁路表 dbt_instance_ext）平台不再连接；表不存在时不过滤。
	err := instext.ExcludeDisabled(s.db.Where("is_enable = 1 AND db_type IN ?", collectDBTypes), "dbt_sql_instance.id").Order("id").Find(&out).Error
	return out, err
}

func (s *store) Watermark(instanceID uint) (int64, error) {
	var m int64
	err := s.db.Raw("SELECT COALESCE(MAX(updated_at_micro),0) FROM "+tblSlowLogs+" WHERE instance_id = ?", instanceID).Scan(&m).Error
	return m, err
}

// maxExistingKeys 是去重时最多读取的已有记录数；一批最多 BatchLimit 条，远小于它。
const maxExistingKeys = 50000

func (s *store) ExistingKeys(instanceID uint, minMicro, maxMicro int64) (map[string]bool, error) {
	var rows []struct {
		Micro    int64  `gorm:"column:updated_at_micro"`
		MD5      string `gorm:"column:md5"`
		UserHost string `gorm:"column:user_host"`
	}
	err := s.db.Raw("SELECT updated_at_micro, COALESCE(sql_md5,'') AS md5, COALESCE(user_host,'') AS user_host FROM "+tblSlowLogs+
		" WHERE instance_id = ? AND updated_at_micro BETWEEN ? AND ? LIMIT ?", instanceID, minMicro, maxMicro, maxExistingKeys).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[entryKey(r.Micro, r.MD5, r.UserHost)] = true
	}
	return out, nil
}

const insertChunk = 200

func (s *store) InsertSlowLogs(instanceID uint, rows []Entry) (int, error) {
	now := time.Now().Format(tsLayout)
	n := 0
	err := s.db.Transaction(func(tx *gorm.DB) error {
		for i := 0; i < len(rows); i += insertChunk {
			end := i + insertChunk
			if end > len(rows) {
				end = len(rows)
			}
			chunk := rows[i:end]
			query, args := buildInsertSlowLogs(instanceID, chunk, now)
			if err := tx.Exec(query, args...).Error; err != nil {
				return err
			}
			n += len(chunk)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *store) SaveState(st CollectState) error {
	var okAt any
	if !st.LastOKAt.IsZero() {
		okAt = st.LastOKAt.Format(tsLayout)
	}
	return s.db.Exec("INSERT INTO "+tblCollectState+" (instance_id, last_run_at, last_ok_at, status, last_error, last_rows, total_rows, duration_ms, updated_at) "+
		"VALUES (?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE last_run_at = VALUES(last_run_at), last_ok_at = VALUES(last_ok_at), status = VALUES(status), "+
		"last_error = VALUES(last_error), last_rows = VALUES(last_rows), total_rows = VALUES(total_rows), duration_ms = VALUES(duration_ms), updated_at = VALUES(updated_at)",
		st.InstanceID, st.LastRunAt.Format(tsLayout), okAt, st.Status, st.Error, st.LastRows, st.TotalRows, st.DurationMs, time.Now().Format(tsLayout)).Error
}

func (s *store) LoadStates() (map[uint]CollectState, bool, error) {
	var rows []struct {
		InstanceID uint   `gorm:"column:instance_id"`
		LastRunAt  string `gorm:"column:last_run_at"`
		LastOKAt   string `gorm:"column:last_ok_at"`
		Status     string `gorm:"column:status"`
		LastError  string `gorm:"column:last_error"`
		LastRows   int    `gorm:"column:last_rows"`
		TotalRows  int64  `gorm:"column:total_rows"`
		DurationMs int64  `gorm:"column:duration_ms"`
	}
	err := s.db.Raw("SELECT instance_id, DATE_FORMAT(last_run_at,'%Y-%m-%d %H:%i:%s') AS last_run_at, " +
		"COALESCE(DATE_FORMAT(last_ok_at,'%Y-%m-%d %H:%i:%s'),'') AS last_ok_at, status, COALESCE(last_error,'') AS last_error, " +
		"last_rows, total_rows, duration_ms FROM " + tblCollectState + " LIMIT 5000").Scan(&rows).Error
	if isNoTable(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := make(map[uint]CollectState, len(rows))
	for _, r := range rows {
		st := CollectState{InstanceID: r.InstanceID, Status: r.Status, Error: r.LastError, LastRows: r.LastRows, TotalRows: r.TotalRows, DurationMs: r.DurationMs}
		st.LastRunAt, _ = time.ParseInLocation(tsLayout, r.LastRunAt, time.Local)
		if r.LastOKAt != "" {
			st.LastOKAt, _ = time.ParseInLocation(tsLayout, r.LastOKAt, time.Local)
		}
		out[r.InstanceID] = st
	}
	return out, true, nil
}
