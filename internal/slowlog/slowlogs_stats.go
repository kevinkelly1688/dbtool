// @author: kevinkelly
// @license: Apache Licence
// @file: slowlogs_stats.go
// @time: 2026/10/28

package slowlog

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/server"
)

// 本文件是“查慢日志”页的统计能力：顶部 KPI（总次数 / 指纹数 / 累计耗时 / 最慢单条）、
// 每日次数与每日累计耗时折线、库下拉。统计与列表用同一组过滤条件（实例、库、搜索、日期范围），
// 但与分页、排序无关；与列表分开请求，统计失败不影响列表。

// maxFingerprintRunes 是返回给前端的 SQL 指纹的最大长度。
const maxFingerprintRunes = 500

// maxStatDatabases 是库下拉最多返回的库数。
const maxStatDatabases = 300

// DayStat 是某一天的慢日志次数与累计耗时。
type DayStat struct {
	Day       string  `json:"day"` // YYYY-MM-DD
	Count     int64   `json:"count"`
	TotalTime float64 `json:"total_time"` // 秒
}

// SlowestEntry 是范围内耗时最长的一条慢日志。
type SlowestEntry struct {
	InstanceID   uint    `json:"instance_id"`
	InstanceName string  `json:"instance_name"`
	DBName       string  `json:"db_name"`
	SQLMD5       string  `json:"sql_md5"`
	QueryTimeSec float64 `json:"query_time_sec"`
	StartTime    string  `json:"start_time"`
	Fingerprint  string  `json:"fingerprint"`
}

// StatsData 是仓库返回的原始统计（天数据是稀疏的，没有慢日志的日子不出现）。
type StatsData struct {
	Total        int64
	Fingerprints int64
	TotalTime    float64
	MaxTime      float64
	Days         []DayStat
	Slowest      *SlowestEntry // SQLText 在 Fingerprint 字段里暂存，服务层转成指纹
	Databases    []string
}

// StatsRepo 是“查慢日志”统计需要的仓库能力。单独成接口：不实现它的仓库，统计接口返回 501，列表不受影响。
type StatsRepo interface {
	SlowLogStats(q GroupQuery) (*StatsData, error)
}

// SlowLogStats 是统计接口的返回。
type SlowLogStats struct {
	Total        int64         `json:"total"`        // 慢日志总次数
	Fingerprints int64         `json:"fingerprints"` // 实例 × 指纹 的个数
	TotalTime    float64       `json:"total_time"`   // 累计耗时（秒）
	MaxTime      float64       `json:"max_time"`     // 最慢单条（秒）
	Slowest      *SlowestEntry `json:"slowest,omitempty"`
	Days         []DayStat     `json:"days"` // 范围内每一天（没有数据的日子补 0）
	Databases    []string      `json:"databases"`
	From         string        `json:"from"`
	To           string        `json:"to"`
}

// Stats 返回当前过滤条件下的 KPI 与每日曲线。权限、实例范围、参数校验与 Groups 一致。
func (s *Service) Stats(p Principal, in GroupParams) (*SlowLogStats, error) {
	if err := s.requirePerm(p, viewButtons...); err != nil {
		return nil, err
	}
	sr, ok := s.repo.(StatsRepo)
	if !ok {
		return nil, newErr(http.StatusNotImplemented, "not_supported", "当前仓库不支持慢日志统计")
	}
	from, to, startDay, endDay, err := s.parseRange(in.Start, in.End, 31)
	if err != nil {
		return nil, err
	}
	search, err := validSearch(in.Search)
	if err != nil {
		return nil, err
	}
	db, err := validDB(in.DB)
	if err != nil {
		return nil, err
	}
	sc, err := s.scopeOf(p)
	if err != nil {
		return nil, err
	}
	q := GroupQuery{DB: db, Search: search, From: from, To: to}
	switch {
	case in.InstanceID != 0:
		if _, err := s.instanceFor(p, in.InstanceID); err != nil {
			return nil, err
		}
		q.InstanceIDs = []uint{in.InstanceID}
	case !sc.all:
		q.InstanceIDs = sc.idList()
		if len(q.InstanceIDs) == 0 {
			return fillStats(&StatsData{}, startDay, endDay), nil
		}
	}
	data, err := sr.SlowLogStats(q)
	if err != nil {
		return nil, err
	}
	if data == nil {
		data = &StatsData{}
	}
	if data.Slowest != nil {
		sl := *data.Slowest
		sl.Fingerprint, _ = clipFingerprint(sl.Fingerprint)
		data.Slowest = &sl
	}
	return fillStats(data, startDay, endDay), nil
}

// fillStats 把稀疏的天数据补成 [startDay, endDay] 的连续序列。
func fillStats(d *StatsData, startDay, endDay string) *SlowLogStats {
	out := &SlowLogStats{Total: d.Total, Fingerprints: d.Fingerprints, TotalTime: round(d.TotalTime, 3), MaxTime: round(d.MaxTime, 3),
		Slowest: d.Slowest, Days: []DayStat{}, Databases: d.Databases, From: startDay, To: endDay}
	if out.Databases == nil {
		out.Databases = []string{}
	}
	byDay := make(map[string]DayStat, len(d.Days))
	for _, x := range d.Days {
		byDay[x.Day] = x
	}
	st, err1 := time.Parse(dayLayout, startDay)
	en, err2 := time.Parse(dayLayout, endDay)
	if err1 != nil || err2 != nil {
		return out
	}
	for t := st; !t.After(en) && len(out.Days) <= MaxSpanDays; t = t.AddDate(0, 0, 1) {
		day := t.Format(dayLayout)
		x, ok := byDay[day]
		if !ok {
			x = DayStat{Day: day}
		}
		x.TotalTime = round(x.TotalTime, 3)
		out.Days = append(out.Days, x)
	}
	return out
}

// clipFingerprint 计算 SQL 指纹并截断到 maxFingerprintRunes 个字符。
func clipFingerprint(sql string) (string, bool) {
	fp := Fingerprint(sql)
	if len(fp) <= maxFingerprintRunes {
		return fp, false
	}
	r := []rune(fp)
	if len(r) <= maxFingerprintRunes {
		return fp, false
	}
	return string(r[:maxFingerprintRunes]), true
}

// ---- SQL 拼装（纯函数，便于单测） ----

// statsBaseWhere 是统计共用的过滤条件（与列表一致）。
func statsBaseWhere(q GroupQuery) (string, []any) { return groupWhere(q) }

func buildStatsSummary(q GroupQuery) (string, []any) {
	where, args := statsBaseWhere(q)
	return "SELECT COUNT(*) AS total, COALESCE(SUM(l.query_time_sec),0) AS total_time, COALESCE(MAX(l.query_time_sec),0) AS max_time, " +
		"COUNT(DISTINCT l.instance_id, COALESCE(l.sql_md5,'')) AS fps FROM " + tblSlowLogs + " l WHERE " + where, args
}

func buildStatsDays(q GroupQuery) (string, []any) {
	where, args := statsBaseWhere(q)
	return "SELECT DATE_FORMAT(l.start_time,'%Y-%m-%d') AS d, COUNT(*) AS cnt, COALESCE(SUM(l.query_time_sec),0) AS total_time FROM " +
		tblSlowLogs + " l WHERE " + where + " GROUP BY d ORDER BY d LIMIT ?", append(append([]any{}, args...), MaxSpanDays+1)
}

func buildStatsSlowest(q GroupQuery) (string, []any) {
	where, args := statsBaseWhere(q)
	return "SELECT l.instance_id AS instance_id, COALESCE(i.instance_name,'') AS instance_name, COALESCE(l.db_name,'') AS db_name, " +
		"COALESCE(l.sql_md5,'') AS sql_md5, COALESCE(l.query_time_sec,0) AS query_time_sec, DATE_FORMAT(l.start_time,'%Y-%m-%d %H:%i:%s') AS start_time, " +
		"COALESCE(l.sql_text,'') AS sql_text FROM " + tblSlowLogs + " l LEFT JOIN " + tblInstance + " i ON i.id = l.instance_id WHERE " + where +
		" ORDER BY l.query_time_sec DESC, l.id DESC LIMIT 1", args
}

// buildStatsDatabases 取范围内出现过的库（库下拉用）：只按实例与日期过滤，不带库与搜索条件，这样选了某个库之后下拉里仍有其他库。
func buildStatsDatabases(q GroupQuery) (string, []any) {
	base := GroupQuery{InstanceIDs: q.InstanceIDs, From: q.From, To: q.To}
	where, args := statsBaseWhere(base)
	return "SELECT DISTINCT l.db_name FROM " + tblSlowLogs + " l WHERE " + where +
		" AND l.db_name IS NOT NULL AND l.db_name <> '' ORDER BY l.db_name LIMIT ?", append(append([]any{}, args...), maxStatDatabases)
}

// SlowLogStats 是 StatsRepo 的 GORM 实现。
func (s *store) SlowLogStats(q GroupQuery) (*StatsData, error) {
	out := &StatsData{}
	sumSQL, sumArgs := buildStatsSummary(q)
	var sum struct {
		Total     int64   `gorm:"column:total"`
		TotalTime float64 `gorm:"column:total_time"`
		MaxTime   float64 `gorm:"column:max_time"`
		Fps       int64   `gorm:"column:fps"`
	}
	if err := s.db.Raw(sumSQL, sumArgs...).Scan(&sum).Error; err != nil {
		return nil, err
	}
	out.Total, out.TotalTime, out.MaxTime, out.Fingerprints = sum.Total, sum.TotalTime, sum.MaxTime, sum.Fps

	daySQL, dayArgs := buildStatsDays(q)
	var days []struct {
		D         string  `gorm:"column:d"`
		Cnt       int64   `gorm:"column:cnt"`
		TotalTime float64 `gorm:"column:total_time"`
	}
	if err := s.db.Raw(daySQL, dayArgs...).Scan(&days).Error; err != nil {
		return nil, err
	}
	for _, d := range days {
		out.Days = append(out.Days, DayStat{Day: d.D, Count: d.Cnt, TotalTime: d.TotalTime})
	}

	if sum.Total > 0 {
		slowSQL, slowArgs := buildStatsSlowest(q)
		var rows []struct {
			InstanceID   uint    `gorm:"column:instance_id"`
			InstanceName string  `gorm:"column:instance_name"`
			DBName       string  `gorm:"column:db_name"`
			SQLMD5       string  `gorm:"column:sql_md5"`
			QueryTimeSec float64 `gorm:"column:query_time_sec"`
			StartTime    string  `gorm:"column:start_time"`
			SQLText      string  `gorm:"column:sql_text"`
		}
		if err := s.db.Raw(slowSQL, slowArgs...).Scan(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			r := rows[0]
			// 指纹在服务层计算，这里先把 SQL 文本放在 Fingerprint 字段里。
			out.Slowest = &SlowestEntry{InstanceID: r.InstanceID, InstanceName: r.InstanceName, DBName: r.DBName, SQLMD5: r.SQLMD5,
				QueryTimeSec: round(r.QueryTimeSec, 3), StartTime: r.StartTime, Fingerprint: r.SQLText}
		}
	}

	dbSQL, dbArgs := buildStatsDatabases(q)
	if err := s.db.Raw(dbSQL, dbArgs...).Scan(&out.Databases).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 页面与接口 ----

// logsPageInfo 是“查慢日志”页的模板数据。
type logsPageInfo struct {
	// CanOptimize 为 true 时详情里显示“去优化工具”链接（需要 optimize.explain 按钮，否则链接过去是 403）。
	CanOptimize bool
}

func (h *handlers) canOptimize(c *gin.Context) bool {
	return h.s.HasButton(c, btnOpenAdvisor)
}

func (h *handlers) apiGroupStats(c *gin.Context) {
	inst, ok := intQuery(c, "instance_id")
	if !ok {
		return
	}
	st, err := h.svc.Stats(server.PrincipalOf(c), GroupParams{InstanceID: uint(inst), DB: c.Query("db_name"), Search: c.Query("q"),
		Start: c.Query("start"), End: c.Query("end")})
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, st)
}
