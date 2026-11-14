// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"dbtool/internal/model"
)

// Options 是 NewService 的可选依赖。
type Options struct {
	Advisor   Advisor    // 默认 RuleAdvisor
	Collector *Collector // 页面展示采集状态用；可为空
	Redis     *RedisSlow // Redis 慢查询（SLOWLOG 采集与查询）；为空表示未启用，Redis 实例仍显示“暂不支持”
	Now       func() time.Time
}

// Service 是两个慢日志页面的业务逻辑。所有方法先校验权限与实例范围，再把校验后的查询交给仓库。
type Service struct {
	repo      Repo
	advisor   Advisor
	collector *Collector
	redis     *redisModule
	now       func() time.Time
}

func NewService(repo Repo, o Options) *Service {
	s := &Service{repo: repo, advisor: o.Advisor, collector: o.Collector, now: o.Now, redis: newRedisModule(o.Redis)}
	if s.advisor == nil {
		s.advisor = RuleAdvisor{}
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// 数据源的显示名。
var sourceLabel = map[string]string{
	SourceDigest:    "pt-query-digest 库（dbt_mysql_slow_query_review）",
	SourceCollected: "平台采集（dbt_slow_logs）",
}

// 受支持的实例类型。
var (
	collectTypes  = map[string]bool{"mysql": true, "polardb": true, "polardb-x": true, "selectdb": true, "doris": true}
	platformTypes = map[string]bool{"polardb": true, "polardb-x": true, "selectdb": true, "doris": true, "adb": true}
	// SQLAdvisor/SOAR 依赖 MySQL 的 EXPLAIN 与 InnoDB 索引。
	adviseTypes = map[string]bool{"mysql": true, "polardb": true, "polardb-x": true}
)

// ---- 权限与实例范围 ----

func (s *Service) requirePerm(p Principal, codes ...string) error {
	if p.IsSuperuser {
		return nil
	}
	for _, c := range codes {
		ok, err := s.repo.HasButton(p.ID, c)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return newErr(http.StatusForbidden, "forbidden", "没有权限执行该操作")
}

// scope 是用户可见的实例范围：全部（超级用户或 query.admin_run），或资源组关联的实例。
type scope struct {
	all bool
	ids map[uint]bool
}

func (sc scope) allows(id uint) bool { return sc.all || sc.ids[id] }

func (s *Service) scopeOf(p Principal) (scope, error) {
	if p.IsSuperuser {
		return scope{all: true}, nil
	}
	all, err := s.repo.HasButton(p.ID, permQueryAll)
	if err != nil {
		return scope{}, err
	}
	if all {
		return scope{all: true}, nil
	}
	ids, err := s.repo.UserInstanceIDs(p.ID)
	return scope{ids: ids}, err
}

// instanceFor 取实例并确认在用户可见范围内。
func (s *Service) instanceFor(p Principal, id uint) (*model.Instance, error) {
	if id == 0 {
		return nil, badReq("请选择实例")
	}
	inst, err := s.repo.GetInstance(id)
	if err != nil || inst == nil {
		if err != nil && !isNotFound(err) {
			return nil, err
		}
		return nil, newErr(http.StatusNotFound, "not_found", "实例不存在")
	}
	sc, err := s.scopeOf(p)
	if err != nil {
		return nil, err
	}
	if !sc.allows(inst.ID) {
		return nil, newErr(http.StatusForbidden, "no_privilege", "你所在组未关联该实例")
	}
	return inst, nil
}

func isNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

// hostKey 是 pt-query-digest 表里 hostname_max 的写法：host:port。
func hostKey(inst *model.Instance) string { return fmt.Sprintf("%s:%d", inst.Host, inst.Port) }

// ---- 参数校验 ----

var (
	reSQLID   = regexp.MustCompile(`^[0-9A-Za-z]{1,64}$`)
	dayLayout = "2006-01-02"
)

func validDB(db string) (string, error) {
	db = strings.TrimSpace(db)
	if len([]rune(db)) > 64 || strings.ContainsAny(db, "\x00\r\n") {
		return "", badReq("数据库名不合法")
	}
	return db, nil
}

func validSearch(q string) (string, error) {
	q = strings.TrimSpace(q)
	if len([]rune(q)) > MaxSearchLen {
		return "", badReq("搜索关键字过长（最多 %d 个字符）", MaxSearchLen)
	}
	if strings.ContainsRune(q, 0) {
		return "", badReq("搜索关键字不合法")
	}
	return q, nil
}

func validPaging(limit, offset int) (int, int, error) {
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 0 || limit > MaxLimit {
		return 0, 0, badReq("每页条数需在 1 到 %d 之间", MaxLimit)
	}
	if offset < 0 || offset > MaxOffset {
		return 0, 0, badReq("翻页过深（最多翻到第 %d 条），请缩小时间范围或增加过滤条件", MaxOffset)
	}
	return limit, offset, nil
}

func validOrder(order string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(order)) {
	case "", "desc":
		return true, nil
	case "asc":
		return false, nil
	}
	return false, badReq("排序方向只能是 asc 或 desc")
}

func validSort(sort, def string, allowed map[string]bool) (string, error) {
	sort = strings.TrimSpace(sort)
	if sort == "" {
		return def, nil
	}
	if !allowed[sort] {
		return "", badReq("不支持按 %q 排序", sort)
	}
	return sort, nil
}

// parseRange 解析 YYYY-MM-DD 的起止日期（含当天）。都为空时取最近 defDays 天。
// 返回 [from, to) 的完整时间字符串，以及实际使用的起止日期。
func (s *Service) parseRange(start, end string, defDays int) (from, to, startDay, endDay string, err error) {
	start, end = strings.TrimSpace(start), strings.TrimSpace(end)
	var st, en time.Time
	switch {
	case start == "" && end == "":
		today := s.now()
		en = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
		st = en.AddDate(0, 0, -(defDays - 1))
	case start == "" || end == "":
		return "", "", "", "", badReq("请同时提供开始日期和结束日期")
	default:
		if st, err = time.Parse(dayLayout, start); err != nil {
			return "", "", "", "", badReq("开始日期格式应为 YYYY-MM-DD")
		}
		if en, err = time.Parse(dayLayout, end); err != nil {
			return "", "", "", "", badReq("结束日期格式应为 YYYY-MM-DD")
		}
	}
	if en.Before(st) {
		return "", "", "", "", badReq("结束日期不能早于开始日期")
	}
	if days := int(en.Sub(st).Hours()/24) + 1; days > MaxSpanDays {
		return "", "", "", "", badReq("时间范围最长 %d 天（当前 %d 天），请缩小范围", MaxSpanDays, days)
	}
	const full = "2006-01-02 15:04:05"
	return st.Format(full), en.AddDate(0, 0, 1).Format(full), st.Format(dayLayout), en.Format(dayLayout), nil
}

// ---- 实例下拉 ----

// Instances 返回页面实例下拉。page 为 "logs"（查慢日志：开启了采集开关的实例）或 "query"（慢查日志：用户可见的、有慢日志数据源的类型）。
func (s *Service) Instances(p Principal, page string) ([]InstanceInfo, error) {
	if err := s.requirePerm(p, viewButtons...); err != nil {
		return nil, err
	}
	sc, err := s.scopeOf(p)
	if err != nil {
		return nil, err
	}
	list, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	out := []InstanceInfo{}
	for _, in := range list {
		if !sc.allows(in.ID) {
			continue
		}
		info := InstanceInfo{ID: in.ID, Name: in.InstanceName, DBType: in.DBType, Collect: in.IsEnable}
		switch page {
		case "logs":
			if !in.IsEnable || !collectTypes[in.DBType] {
				continue
			}
			info.Source = SourceCollected
		case "query":
			switch {
			case in.DBType == "redis" && s.redisOn():
				info.Source = SourceRedis
			case in.DBType == "redis":
				info.Hint = "Redis 慢查询暂不支持"
			case platformTypes[in.DBType]:
				info.Source = SourceCollected
			case in.DBType == "mysql":
				info.Source = "auto"
			default:
				continue
			}
		default:
			return nil, badReq("未知的页面类型")
		}
		out = append(out, info)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---- 查慢日志（dbt_slow_logs 聚合） ----

var groupSorts = map[string]bool{
	"exec_date": true, "count": true, "query_time_avg": true, "query_time_max": true,
	"rows_examined_avg": true, "rows_sent_avg": true, "total_time": true,
}

// Groups 返回“实例 × 库 × 指纹 × 天”的聚合列表。
func (s *Service) Groups(p Principal, in GroupParams) (*GroupPage, error) {
	if err := s.requirePerm(p, viewButtons...); err != nil {
		return nil, err
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
	sortBy, err := validSort(in.Sort, "exec_date", groupSorts)
	if err != nil {
		return nil, err
	}
	desc, err := validOrder(in.Order)
	if err != nil {
		return nil, err
	}
	limit, offset, err := validPaging(in.Limit, in.Offset)
	if err != nil {
		return nil, err
	}
	sc, err := s.scopeOf(p)
	if err != nil {
		return nil, err
	}
	q := GroupQuery{DB: db, Search: search, From: from, To: to, Sort: sortBy, Desc: desc, Limit: limit, Offset: offset}
	switch {
	case in.InstanceID != 0:
		if _, err := s.instanceFor(p, in.InstanceID); err != nil {
			return nil, err
		}
		q.InstanceIDs = []uint{in.InstanceID}
	case !sc.all:
		q.InstanceIDs = sc.idList()
		if len(q.InstanceIDs) == 0 {
			return &GroupPage{Rows: []SlowLogGroup{}, From: startDay, To: endDay}, nil
		}
	}
	rows, total, err := s.repo.SlowLogGroups(q)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].TableName = ExtractTable(rows[i].SQLText)
		rows[i].Fingerprint, _ = clipFingerprint(rows[i].SQLText)
		rows[i].SQLText, rows[i].TextTruncated = clipText(rows[i].SQLText)
	}
	if rows == nil {
		rows = []SlowLogGroup{}
	}
	return &GroupPage{Total: total, Rows: rows, From: startDay, To: endDay}, nil
}

func (sc scope) idList() []uint {
	out := make([]uint, 0, len(sc.ids))
	for id, ok := range sc.ids {
		if ok {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Detail 返回某聚合行当天最近 100 条慢日志。
func (s *Service) Detail(p Principal, in DetailParams) ([]SlowLogRow, error) {
	if err := s.requirePerm(p, viewButtons...); err != nil {
		return nil, err
	}
	if _, err := s.instanceFor(p, in.InstanceID); err != nil {
		return nil, err
	}
	db, err := validDB(in.DB)
	if err != nil {
		return nil, err
	}
	md5 := strings.TrimSpace(in.MD5)
	if md5 != "" && !reSQLID.MatchString(md5) {
		return nil, badReq("SQL 指纹不合法")
	}
	if strings.TrimSpace(in.Day) == "" {
		return nil, badReq("请提供执行日期")
	}
	from, to, _, _, err := s.parseRange(in.Day, in.Day, 1)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.SlowLogDetail(DetailQuery{InstanceID: in.InstanceID, DB: db, MD5: md5, From: from, To: to, Limit: DetailRows})
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].SQLText, rows[i].TextTruncated = clipText(rows[i].SQLText)
	}
	if rows == nil {
		rows = []SlowLogRow{}
	}
	return rows, nil
}

// ---- 慢查日志（统计 / 明细 / 趋势） ----

var (
	reviewSorts = map[string]bool{
		"create_time": true, "db_name": true, "query_time_avg": true, "exec_count": true, "exec_time_sum": true,
		"parse_total_rows": true, "return_total_rows": true, "parse_row_avg": true, "return_row_avg": true,
		"query_time_max": true, "first_seen": true,
	}
	historySorts = map[string]bool{
		"exec_start": true, "db_name": true, "host_address": true, "exec_count": true, "query_time_pct95": true,
		"query_time_sum": true, "lock_time_sum": true, "parse_rows": true, "return_rows": true,
	}
)

// pickSource 按实例类型选择数据源，返回 digest / collected。
func (s *Service) pickSource(inst *model.Instance, requested string) (string, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested != "" && requested != "auto" && requested != SourceDigest && requested != SourceCollected {
		return "", badReq("不支持的数据源 %q", requested)
	}
	switch {
	case inst.DBType == "redis":
		return "", newErr(http.StatusBadRequest, "unsupported", "Redis 慢查询暂不支持（需要连接集群发现主节点）")
	case platformTypes[inst.DBType]:
		if requested == SourceDigest {
			return "", badReq("%s 实例只有平台采集的慢日志", inst.DBType)
		}
		return SourceCollected, nil
	case inst.DBType != "mysql":
		return "", newErr(http.StatusBadRequest, "unsupported", "暂不支持 %s 类型实例的慢日志", inst.DBType)
	}
	switch requested {
	case SourceCollected:
		return SourceCollected, nil
	case SourceDigest:
		ok, err := s.repo.DigestAvailable()
		if err != nil {
			return "", err
		}
		if !ok {
			return "", newErr(http.StatusConflict, "table_missing", "没有找到 dbt_mysql_slow_query_review 表：pt-query-digest 的推送脚本还没有部署，可改选“平台采集”数据源")
		}
		return SourceDigest, nil
	}
	ok, err := s.repo.DigestAvailable()
	if err != nil {
		return "", err
	}
	if ok {
		// 表在不代表有数据：基础表结构安装时就会建出这两张表，没部署 pt-query-digest 推送脚本的实例它们是空的。
		// 只有历史表里确实有这个实例（host:port）的数据才用 digest，否则用平台采集——
		// 否则“慢查日志”一直是空的，而同一批数据在“查慢日志”里明明有。
		has, err := s.repo.DigestHasHost(hostKey(inst))
		if err != nil {
			return "", err
		}
		if has {
			return SourceDigest, nil
		}
	}
	return SourceCollected, nil
}

// reviewCtx 是统计/明细/趋势共用的校验结果。
type reviewCtx struct {
	inst     *model.Instance
	source   string
	q        ReviewQuery
	startDay string
	endDay   string
}

func (s *Service) prepareReview(p Principal, in ReviewParams, sorts map[string]bool, defSort string, paged bool) (*reviewCtx, error) {
	if err := s.requirePerm(p, viewButtons...); err != nil {
		return nil, err
	}
	inst, err := s.instanceFor(p, in.InstanceID)
	if err != nil {
		return nil, err
	}
	source, err := s.pickSource(inst, in.Source)
	if err != nil {
		return nil, err
	}
	from, to, startDay, endDay, err := s.parseRange(in.Start, in.End, 7)
	if err != nil {
		return nil, err
	}
	db, err := validDB(in.DB)
	if err != nil {
		return nil, err
	}
	search, err := validSearch(in.Search)
	if err != nil {
		return nil, err
	}
	sqlID := strings.TrimSpace(in.SQLID)
	if sqlID != "" && !reSQLID.MatchString(sqlID) {
		return nil, badReq("SQL 指纹不合法")
	}
	minSec, err := parseMinSeconds(in.MinSeconds)
	if err != nil {
		return nil, err
	}
	q := ReviewQuery{InstanceID: inst.ID, HostKey: hostKey(inst), DB: db, SQLID: sqlID, Search: search, From: from, To: to, Sort: defSort, Desc: true, MinSeconds: minSec}
	if paged {
		if q.Sort, err = validSort(in.Sort, defSort, sorts); err != nil {
			return nil, err
		}
		if q.Desc, err = validOrder(in.Order); err != nil {
			return nil, err
		}
		if q.Limit, q.Offset, err = validPaging(in.Limit, in.Offset); err != nil {
			return nil, err
		}
	}
	return &reviewCtx{inst: inst, source: source, q: q, startDay: startDay, endDay: endDay}, nil
}

// Review 返回慢日志统计（按 SQL 指纹聚合）。
func (s *Service) Review(ctx context.Context, p Principal, in ReviewParams) (*ReviewPage, error) {
	if in.All { // 全部实例汇总（review_ext.go）
		return s.reviewAll(ctx, p, in)
	}
	rc, err := s.prepareReview(p, in, reviewSorts, "exec_count", true)
	if err != nil {
		return nil, err
	}
	var page *ReviewPage
	switch rc.source {
	case SourceDigest:
		page, err = s.review(s.repo.DigestReview, rc.q)
	default:
		page, err = s.review(s.repo.CollectedReview, rc.q)
		if err == nil {
			for i := range page.Rows { // 平台采集的是样例 SQL，列表展示归一化后的指纹
				if fp := Fingerprint(page.Rows[i].SQLText); fp != "" {
					page.Rows[i].SQLText = fp
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	for i := range page.Rows {
		page.Rows[i].SQLText, page.Rows[i].Truncated = clipText(page.Rows[i].SQLText)
	}
	page.Source, page.Label, page.From, page.To = rc.source, sourceLabel[rc.source], rc.startDay, rc.endDay
	s.enrichReview(page, rc)
	return page, nil
}

func (s *Service) review(f func(ReviewQuery) ([]ReviewRow, int64, error), q ReviewQuery) (*ReviewPage, error) {
	rows, total, err := f(q)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []ReviewRow{}
	}
	return &ReviewPage{Total: total, Rows: rows}, nil
}

// History 返回慢日志明细。
func (s *Service) History(ctx context.Context, p Principal, in ReviewParams) (*HistoryPage, error) {
	rc, err := s.prepareReview(p, in, historySorts, "parse_rows", true)
	if err != nil {
		return nil, err
	}
	var page *HistoryPage
	switch rc.source {
	default:
		f := s.repo.CollectedHistory
		if rc.source == SourceDigest {
			f = s.repo.DigestHistory
		}
		var rows []HistoryRow
		var total int64
		if rows, total, err = f(rc.q); err == nil {
			if rows == nil {
				rows = []HistoryRow{}
			}
			page = &HistoryPage{Total: total, Rows: rows}
		}
	}
	if err != nil {
		return nil, err
	}
	for i := range page.Rows {
		page.Rows[i].SQLText, page.Rows[i].Truncated = clipText(page.Rows[i].SQLText)
	}
	page.Source, page.Label, page.From, page.To = rc.source, sourceLabel[rc.source], rc.startDay, rc.endDay
	return page, nil
}

// Trend 返回某条 SQL 指纹在时间范围内按天（或按小时）聚合的次数与 95% 耗时。
func (s *Service) Trend(ctx context.Context, p Principal, in ReviewParams) (*Trend, error) {
	rc, err := s.prepareReview(p, in, nil, "", false)
	if err != nil {
		return nil, err
	}
	if rc.q.SQLID == "" {
		return nil, badReq("请提供 SQL 指纹")
	}
	bucket := strings.ToLower(strings.TrimSpace(in.Bucket))
	if bucket == "" {
		bucket = "day"
	}
	if bucket != "day" && bucket != "hour" {
		return nil, badReq("时间粒度只能是 day 或 hour")
	}
	if bucket == "hour" {
		st, _ := time.Parse(dayLayout, rc.startDay)
		en, _ := time.Parse(dayLayout, rc.endDay)
		if en.Sub(st).Hours()/24+1 > 7 {
			return nil, badReq("按小时统计时间范围最长 7 天")
		}
	}
	tq := TrendQuery{InstanceID: rc.inst.ID, HostKey: rc.q.HostKey, SQLID: rc.q.SQLID, From: rc.q.From, To: rc.q.To, Bucket: bucket}
	var tr *Trend
	if rc.source == SourceDigest {
		tr, err = s.repo.DigestTrend(tq)
	} else {
		tr, err = s.repo.CollectedTrend(tq)
	}
	if err != nil {
		return nil, err
	}
	if tr.Points == nil {
		tr.Points = []TrendPoint{}
	}
	tr.Bucket, tr.Source, tr.Label, tr.From, tr.To = bucket, rc.source, sourceLabel[rc.source], rc.startDay, rc.endDay
	return tr, nil
}

// Databases 返回时间范围内慢日志里出现过的库名（用于库过滤下拉）。
func (s *Service) Databases(p Principal, in ReviewParams) ([]string, error) {
	rc, err := s.prepareReview(p, in, nil, "", false)
	if err != nil {
		return nil, err
	}
	var list []string
	switch rc.source {
	case SourceDigest:
		list, err = s.repo.DigestDatabases(rc.q.HostKey, rc.q.From, rc.q.To)
	default:
		list, err = s.repo.CollectedDatabases(rc.inst.ID, rc.q.From, rc.q.To)
	}
	if list == nil {
		list = []string{}
	}
	return list, err
}

// ---- 优化建议 ----

// Advise 对选中的慢 SQL 生成优化建议。
func (s *Service) Advise(ctx context.Context, p Principal, in AdviseParams) (*Advice, error) {
	if err := s.requirePerm(p, btnAdvise); err != nil {
		return nil, err
	}
	inst, err := s.instanceFor(p, in.InstanceID)
	if err != nil {
		return nil, err
	}
	if !adviseTypes[inst.DBType] {
		return nil, newErr(http.StatusBadRequest, "unsupported", "%s 类型实例暂不支持优化建议（只对 MySQL 兼容实例开放）", inst.DBType)
	}
	sqlText := strings.TrimSpace(in.SQL)
	if sqlText == "" {
		return nil, badReq("请先选择需要优化的 SQL")
	}
	if len(sqlText) > MaxAdviseSQL {
		return nil, badReq("SQL 过长（最多 %d 字节）", MaxAdviseSQL)
	}
	db, err := validDB(in.DB)
	if err != nil {
		return nil, err
	}
	if in.RowsExamined < 0 || in.RowsSent < 0 || in.QueryTime < 0 || in.LockTime < 0 {
		return nil, badReq("统计参数不能为负数")
	}
	actx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	adv, err := s.advisor.Advise(actx, AdviseRequest{InstanceID: inst.ID, InstanceName: inst.InstanceName, DBType: inst.DBType, DB: db,
		SQL: sqlText, RowsExamined: in.RowsExamined, RowsSent: in.RowsSent, QueryTime: in.QueryTime, LockTime: in.LockTime})
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return nil, err
		}
		slog.Error("生成优化建议失败", "instance", inst.InstanceName, "err", err)
		return nil, newErr(http.StatusBadGateway, "advise_failed", "生成优化建议失败：%s", err.Error())
	}
	if adv.Items == nil {
		adv.Items = []AdviceItem{}
	}
	// 操作审计：只记录实例、库和指纹，不记录 SQL 原文（可能含业务数据）。
	if err := s.repo.InsertAudit(&model.AuditEntry{UserID: p.ID, UserName: p.Username, UserDisplay: p.Display, Action: actionAdviseName,
		ExtraInfo: fmt.Sprintf("实例=%s 库=%s 指纹=%s 长度=%d 来源=%s", inst.InstanceName, db, SQLMD5(sqlText), len(sqlText), adv.Source)}); err != nil {
		slog.Warn("写入优化建议审计失败", "err", err)
	}
	return adv, nil
}

// ---- 采集状态 ----

// CollectorOverview 返回用户可见实例的采集状态。
func (s *Service) CollectorOverview(p Principal) (*CollectorOverview, error) {
	if err := s.requirePerm(p, viewButtons...); err != nil {
		return nil, err
	}
	sc, err := s.scopeOf(p)
	if err != nil {
		return nil, err
	}
	list, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	stored, tableOK, err := s.repo.LoadStates()
	if err != nil {
		slog.Warn("读取采集状态失败", "err", err)
		stored = nil
	}
	ov := &CollectorOverview{StateTable: tableOK, Items: []CollectState{}}
	var mem map[uint]CollectState
	if s.collector != nil {
		ov.Enabled = s.collector.Enabled()
		ov.IntervalSec, ov.Workers = int(s.collector.cfg.Interval/time.Second), s.collector.cfg.Workers
		mem = s.collector.States()
	}
	for _, in := range list {
		if !sc.allows(in.ID) || !collectTypes[in.DBType] || !in.IsEnable {
			continue
		}
		st := CollectState{InstanceID: in.ID}
		if v, ok := stored[in.ID]; ok {
			st = v
		}
		if v, ok := mem[in.ID]; ok && !v.LastRunAt.Before(st.LastRunAt) {
			st = v
		}
		st.InstanceID, st.InstanceName = in.ID, in.InstanceName
		if st.Status == "" {
			st.Status = "none"
		}
		if st.Error != "" && !sc.all { // 连接错误里带目标库的地址，只给能看全部实例的用户
			st.Error = "采集失败（详细原因仅管理员可见）"
		}
		ov.Items = append(ov.Items, st)
	}
	sort.SliceStable(ov.Items, func(i, j int) bool { return ov.Items[i].InstanceName < ov.Items[j].InstanceName })
	return ov, nil
}
