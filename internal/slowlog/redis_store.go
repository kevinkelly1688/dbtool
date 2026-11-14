// @author: kevinkelly
// @license: Apache Licence
// @file: redis_store.go
// @time: 2026/10/28

package slowlog

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"dbtool/internal/instext"
	"dbtool/internal/model"
)

const (
	tblRedisSlow  = "dbt_redis_slowlog"
	tblRedisState = "dbt_redis_slowlog_state"
)

// RedisTablesReady 在两张表都存在时为 true；只缓存“存在”（表可能在服务运行期间被手工创建）。
func (s *store) RedisTablesReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.redisOK {
		return true
	}
	m := s.db.Migrator()
	if m.HasTable(tblRedisSlow) && m.HasTable(tblRedisState) {
		s.redisOK = true
	}
	return s.redisOK
}

func (s *store) RedisInstances() ([]model.Instance, error) {
	var out []model.Instance
	err := instext.ExcludeDisabled(s.db.Where("is_enable = 1 AND db_type = ?", "redis"), "dbt_sql_instance.id").Order("id").Find(&out).Error
	return out, err
}

type redisStateRow struct {
	InstanceID   uint       `gorm:"column:instance_id"`
	LastSlowID   int64      `gorm:"column:last_slow_id"`
	LastSlowTime *time.Time `gorm:"column:last_slow_time"`
	LastRunAt    *time.Time `gorm:"column:last_run_at"`
	LastOKAt     *time.Time `gorm:"column:last_ok_at"`
	Status       string     `gorm:"column:status"`
	LastError    string     `gorm:"column:last_error"`
	LastRows     int        `gorm:"column:last_rows"`
	TotalRows    int64      `gorm:"column:total_rows"`
	DurationMs   int64      `gorm:"column:duration_ms"`
}

func (r redisStateRow) state() RedisState {
	st := RedisState{InstanceID: r.InstanceID, LastSlowID: r.LastSlowID, Status: r.Status, Error: r.LastError, LastRows: r.LastRows, TotalRows: r.TotalRows, DurationMs: r.DurationMs}
	if r.LastSlowTime != nil {
		st.LastSlowTime = *r.LastSlowTime
	}
	if r.LastRunAt != nil {
		st.LastRunAt = *r.LastRunAt
	}
	if r.LastOKAt != nil {
		st.LastOKAt = *r.LastOKAt
	}
	return st
}

const redisStateCols = "instance_id, last_slow_id, last_slow_time, last_run_at, last_ok_at, status, last_error, last_rows, total_rows, duration_ms"

func (s *store) RedisState(instanceID uint) (*RedisState, error) {
	var rows []redisStateRow
	if err := s.db.Raw("SELECT "+redisStateCols+" FROM "+tblRedisState+" WHERE instance_id = ?", instanceID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	st := rows[0].state()
	return &st, nil
}

func (s *store) RedisStates() ([]RedisState, error) {
	var rows []redisStateRow
	if err := s.db.Raw("SELECT " + redisStateCols + " FROM " + tblRedisState + " ORDER BY instance_id LIMIT 5000").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RedisState, len(rows))
	for i, r := range rows {
		out[i] = r.state()
	}
	return out, nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(tsLayout)
}

func (s *store) SaveRedisState(st RedisState) error {
	return s.db.Exec("INSERT INTO "+tblRedisState+" (instance_id, last_slow_id, last_slow_time, last_run_at, last_ok_at, status, last_error, last_rows, total_rows, duration_ms, updated_at) "+
		"VALUES (?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE last_slow_id = VALUES(last_slow_id), last_slow_time = VALUES(last_slow_time), last_run_at = VALUES(last_run_at), "+
		"last_ok_at = VALUES(last_ok_at), status = VALUES(status), last_error = VALUES(last_error), last_rows = VALUES(last_rows), total_rows = VALUES(total_rows), "+
		"duration_ms = VALUES(duration_ms), updated_at = VALUES(updated_at)",
		st.InstanceID, st.LastSlowID, nullTime(st.LastSlowTime), nullTime(st.LastRunAt), nullTime(st.LastOKAt), st.Status, clipRunes(st.Error, 500), st.LastRows, st.TotalRows, st.DurationMs,
		time.Now().Format(tsLayout)).Error
}

func (s *store) InsertRedisSlowlogs(instanceID uint, rows []RedisSlowRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	now := time.Now().Format(tsLayout)
	var n int64
	for i := 0; i < len(rows); i += insertChunk {
		chunk := rows[i:min(i+insertChunk, len(rows))]
		var sb strings.Builder
		sb.WriteString("INSERT IGNORE INTO " + tblRedisSlow + " (instance_id, slow_id, start_time, duration_us, command_name, fingerprint, fp_md5, command_text, client_addr, client_name, collected_at) VALUES ")
		args := make([]any, 0, len(chunk)*11)
		for j, r := range chunk {
			if j > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString("(?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args, instanceID, r.SlowID, r.StartTime.Format(tsLayout), max(r.DurationUs, 0), r.CommandName, r.Fingerprint, r.FpID, r.CommandText, r.ClientAddr, r.ClientName, now)
		}
		res := s.db.Exec(sb.String(), args...)
		if res.Error != nil {
			return int(n), res.Error
		}
		n += res.RowsAffected
	}
	return int(n), nil
}

// redisWhere 生成公共的 WHERE 条件（instance_id 必给）。
func redisWhere(q RedisQuery) (string, []any) {
	w := []string{"instance_id = ?", "start_time >= ?", "start_time < ?"}
	args := []any{q.InstanceID, q.From, q.To}
	if q.Command != "" {
		w = append(w, "command_name = ?")
		args = append(args, q.Command)
	}
	if q.FpID != "" {
		w = append(w, "fp_md5 = ?")
		args = append(args, q.FpID)
	}
	if q.MinMicros > 0 {
		w = append(w, "duration_us >= ?")
		args = append(args, q.MinMicros)
	}
	if q.Search != "" {
		like := "%" + likeEscape(q.Search) + "%"
		w = append(w, "(command_text LIKE ? ESCAPE '!' OR fingerprint LIKE ? ESCAPE '!' OR client_addr LIKE ? ESCAPE '!')")
		args = append(args, like, like, like)
	}
	return " WHERE " + strings.Join(w, " AND "), args
}

func likeEscape(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

var redisReviewOrder = map[string]string{
	"exec_count": "cnt", "total_ms": "total_us", "avg_ms": "avg_us", "max_ms": "max_us", "first_seen": "first_seen", "last_seen": "last_seen", "command_name": "command_name",
}

func (s *store) RedisReview(q RedisQuery) ([]RedisReviewRow, int64, *RedisSummary, error) {
	where, args := redisWhere(q)
	var sum struct {
		Fps   int64   `gorm:"column:fps"`
		Cnt   int64   `gorm:"column:cnt"`
		Total float64 `gorm:"column:total_us"`
		MaxUs float64 `gorm:"column:max_us"`
	}
	if err := s.db.Raw("SELECT COUNT(DISTINCT fp_md5) AS fps, COUNT(*) AS cnt, COALESCE(SUM(duration_us),0) AS total_us, COALESCE(MAX(duration_us),0) AS max_us FROM "+tblRedisSlow+where, args...).Scan(&sum).Error; err != nil {
		return nil, 0, nil, err
	}
	summary := &RedisSummary{Fingerprints: sum.Fps, ExecCount: sum.Cnt, TotalMs: round(sum.Total/1000, 3), MaxMs: round(sum.MaxUs/1000, 3)}
	if sum.Fps == 0 || int64(q.Offset) >= sum.Fps {
		return []RedisReviewRow{}, sum.Fps, summary, nil
	}
	order := redisReviewOrder[q.Sort]
	if order == "" {
		order = "cnt"
	}
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	var rows []struct {
		FpID        string  `gorm:"column:fp_md5"`
		Fingerprint string  `gorm:"column:fingerprint"`
		CommandName string  `gorm:"column:command_name"`
		Cnt         int64   `gorm:"column:cnt"`
		TotalUs     float64 `gorm:"column:total_us"`
		AvgUs       float64 `gorm:"column:avg_us"`
		MaxUs       float64 `gorm:"column:max_us"`
		FirstSeen   string  `gorm:"column:first_seen"`
		LastSeen    string  `gorm:"column:last_seen"`
	}
	query := "SELECT fp_md5, MAX(fingerprint) AS fingerprint, MAX(command_name) AS command_name, COUNT(*) AS cnt, SUM(duration_us) AS total_us, AVG(duration_us) AS avg_us, MAX(duration_us) AS max_us, " +
		"DATE_FORMAT(MIN(start_time), '%Y-%m-%d %H:%i:%s') AS first_seen, DATE_FORMAT(MAX(start_time), '%Y-%m-%d %H:%i:%s') AS last_seen FROM " + tblRedisSlow + where +
		" GROUP BY fp_md5 ORDER BY " + order + " " + dir + ", fp_md5 LIMIT ? OFFSET ?"
	if err := s.db.Raw(query, append(append([]any{}, args...), q.Limit, q.Offset)...).Scan(&rows).Error; err != nil {
		return nil, 0, nil, err
	}
	out := make([]RedisReviewRow, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, RedisReviewRow{FpID: r.FpID, Fingerprint: r.Fingerprint, CommandName: r.CommandName, ExecCount: r.Cnt, TotalMs: round(r.TotalUs/1000, 3),
			AvgMs: round(r.AvgUs/1000, 3), MaxMs: round(r.MaxUs/1000, 3), FirstSeen: r.FirstSeen, LastSeen: r.LastSeen})
		ids = append(ids, r.FpID)
	}
	// 最近一条的完整命令作为样例；行内趋势（每天次数）
	if len(ids) > 0 {
		var samples []struct {
			FpID string `gorm:"column:fp_md5"`
			Text string `gorm:"column:command_text"`
		}
		sw, sargs := redisWhere(RedisQuery{InstanceID: q.InstanceID, From: q.From, To: q.To})
		if err := s.db.Raw("SELECT t.fp_md5, t.command_text FROM "+tblRedisSlow+" t JOIN (SELECT fp_md5, MAX(id) AS mid FROM "+tblRedisSlow+sw+" AND fp_md5 IN ? GROUP BY fp_md5) m ON m.mid = t.id",
			append(append([]any{}, sargs...), ids)...).Scan(&samples).Error; err == nil {
			byID := map[string]string{}
			for _, x := range samples {
				byID[x.FpID] = x.Text
			}
			for i := range out {
				out[i].SampleText = byID[out[i].FpID]
			}
		}
		var days []struct {
			FpID string `gorm:"column:fp_md5"`
			Day  string `gorm:"column:d"`
			Cnt  int64  `gorm:"column:c"`
		}
		if err := s.db.Raw("SELECT fp_md5, DATE_FORMAT(start_time, '%Y-%m-%d') AS d, COUNT(*) AS c FROM "+tblRedisSlow+sw+" AND fp_md5 IN ? GROUP BY fp_md5, d",
			append(append([]any{}, sargs...), ids)...).Scan(&days).Error; err == nil {
			start, _ := time.ParseInLocation(tsLayout, q.From, time.Local)
			end, _ := time.ParseInLocation(tsLayout, q.To, time.Local)
			n := int(end.Sub(start).Hours()/24 + 0.5)
			if n < 1 {
				n = 1
			}
			sparks := map[string][]int64{}
			for _, d := range days {
				t, err := time.ParseInLocation("2006-01-02", d.Day, time.Local)
				if err != nil {
					continue
				}
				idx := int(t.Sub(start).Hours()/24 + 0.5)
				if idx < 0 || idx >= n {
					continue
				}
				if sparks[d.FpID] == nil {
					sparks[d.FpID] = make([]int64, n)
				}
				sparks[d.FpID][idx] = d.Cnt
			}
			for i := range out {
				out[i].Spark = sparks[out[i].FpID]
			}
		}
	}
	return out, sum.Fps, summary, nil
}

var redisHistoryOrder = map[string]string{"start_time": "start_time", "duration_ms": "duration_us", "command_name": "command_name"}

func (s *store) RedisHistory(q RedisQuery) ([]RedisHistoryRow, int64, error) {
	where, args := redisWhere(q)
	var total int64
	if err := s.db.Raw("SELECT COUNT(*) FROM "+tblRedisSlow+where, args...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	if total == 0 || int64(q.Offset) >= total {
		return []RedisHistoryRow{}, total, nil
	}
	order := redisHistoryOrder[q.Sort]
	if order == "" {
		order = "start_time"
	}
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	var rows []struct {
		ID          uint   `gorm:"column:id"`
		SlowID      int64  `gorm:"column:slow_id"`
		StartTime   string `gorm:"column:st"`
		DurationUs  int64  `gorm:"column:duration_us"`
		CommandName string `gorm:"column:command_name"`
		Fingerprint string `gorm:"column:fingerprint"`
		FpID        string `gorm:"column:fp_md5"`
		CommandText string `gorm:"column:command_text"`
		ClientAddr  string `gorm:"column:client_addr"`
		ClientName  string `gorm:"column:client_name"`
	}
	query := "SELECT id, slow_id, DATE_FORMAT(start_time, '%Y-%m-%d %H:%i:%s') AS st, duration_us, command_name, fingerprint, fp_md5, COALESCE(command_text,'') AS command_text, client_addr, client_name FROM " +
		tblRedisSlow + where + " ORDER BY " + order + " " + dir + ", id DESC LIMIT ? OFFSET ?"
	if err := s.db.Raw(query, append(append([]any{}, args...), q.Limit, q.Offset)...).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]RedisHistoryRow, len(rows))
	for i, r := range rows {
		out[i] = RedisHistoryRow{ID: r.ID, SlowID: r.SlowID, StartTime: r.StartTime, DurationMs: round(float64(r.DurationUs)/1000, 3), CommandName: r.CommandName,
			Fingerprint: r.Fingerprint, FpID: r.FpID, CommandText: r.CommandText, ClientAddr: r.ClientAddr, ClientName: r.ClientName}
	}
	return out, total, nil
}

func (s *store) RedisTrend(q RedisQuery) ([]RedisTrendPoint, error) {
	where, args := redisWhere(q)
	format := "%Y-%m-%d"
	if q.Bucket == "hour" {
		format = "%Y-%m-%d %H:00"
	}
	var rows []struct {
		T       string  `gorm:"column:t"`
		Cnt     int64   `gorm:"column:cnt"`
		AvgUs   float64 `gorm:"column:avg_us"`
		MaxUs   float64 `gorm:"column:max_us"`
		TotalUs float64 `gorm:"column:total_us"`
	}
	if err := s.db.Raw("SELECT DATE_FORMAT(start_time, '"+format+"') AS t, COUNT(*) AS cnt, AVG(duration_us) AS avg_us, MAX(duration_us) AS max_us, SUM(duration_us) AS total_us FROM "+
		tblRedisSlow+where+" GROUP BY t ORDER BY t LIMIT ?", append(append([]any{}, args...), maxTrendPoint)...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RedisTrendPoint, len(rows))
	for i, r := range rows {
		out[i] = RedisTrendPoint{T: r.T, Count: r.Cnt, AvgMs: round(r.AvgUs/1000, 3), MaxMs: round(r.MaxUs/1000, 3), TotalMs: round(r.TotalUs/1000, 3)}
	}
	return out, nil
}

func (s *store) RedisCommands(q RedisQuery) ([]string, error) {
	q.Command, q.FpID, q.Search, q.MinMicros = "", "", "", 0
	where, args := redisWhere(q)
	var out []string
	if err := s.db.Raw("SELECT DISTINCT command_name FROM "+tblRedisSlow+where+" ORDER BY command_name LIMIT 200", args...).Scan(&out).Error; err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func (s *store) PurgeRedisSlowlogs(before time.Time, limit int) (int64, error) {
	res := s.db.Exec(fmt.Sprintf("DELETE FROM "+tblRedisSlow+" WHERE start_time < ? ORDER BY id LIMIT %d", max(limit, 1)), before.Format(tsLayout))
	return res.RowsAffected, res.Error
}
