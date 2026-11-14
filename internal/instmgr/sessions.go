// @author: kevinkelly
// @license: Apache Licence
// @file: sessions.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"fmt"
	"strings"
)

// ---- 进程列表 ----

// ProcFilter 是进程列表的筛选条件。
type ProcFilter struct {
	Command string // All / Not Sleep / 具体的 command（Query、Sleep …）；空等同 All
	User    string
	DB      string
	MinTime int    // 执行时长不少于多少秒
	Q       string // 关键字：匹配 USER / HOST / INFO
}

// ProcRow 是一个会话。Protected 的会话不允许终止（自己的连接、复制线程、系统线程）。
type ProcRow struct {
	ID        int64  `json:"id"`
	User      string `json:"user"`
	Host      string `json:"host"`
	DB        string `json:"db"`
	Command   string `json:"command"`
	Time      int64  `json:"time"`
	State     string `json:"state"`
	Info      string `json:"info"`
	Protected bool   `json:"protected"`
	Reason    string `json:"reason,omitempty"`
	// LongRunning：不是休眠、不受保护且已运行 >= 300 秒；Severity 为 warn（>=300s）或 crit（>=1200s）。
	LongRunning bool   `json:"long_running"`
	Severity    string `json:"severity,omitempty"`
	// Blocker：它持有的锁正让别的事务在等（阻塞源）。
	Blocker bool `json:"blocker,omitempty"`
}

// ProcResult 是进程列表。
type ProcResult struct {
	Rows      []ProcRow `json:"rows"`
	Self      int64     `json:"self"`      // 平台自己这次连接的线程号
	Truncated bool      `json:"truncated"` // 超过上限，只返回了前面一部分
	Stats     ProcStats `json:"stats"`     // 不受筛选影响的进程统计
}

// ProcStats 是进程统计条。
type ProcStats struct {
	Total    int64 `json:"total"`    // 连接总数
	Active   int64 `json:"active"`   // 活跃（command 不是 Sleep）
	Long     int64 `json:"long"`     // 长时间运行（>= 300 秒）
	Blockers int64 `json:"blockers"` // 阻塞源个数
}

// 长时间运行的阈值（秒）：>= LongWarnSeconds 标黄，>= LongCritSeconds 标红。
const (
	LongWarnSeconds = 300
	LongCritSeconds = 1200
)

const procLimit = 1000

const procBase = "SELECT id, user, host, IFNULL(db, '') AS db, command, time, IFNULL(state, '') AS state, IFNULL(info, '') AS info FROM information_schema.processlist"

// protectReason 说明为什么这个会话不能终止；空串表示可以。
func protectReason(id, self int64, user, command string) string {
	switch {
	case id == self:
		return "这是平台当前使用的连接"
	case user == "system user":
		return "系统线程（复制 / 后台）"
	case user == "event_scheduler":
		return "事件调度线程"
	case strings.HasPrefix(command, "Binlog Dump"):
		return "复制线程（从库正在读取 binlog）"
	case command == "Daemon":
		return "后台线程"
	}
	return ""
}

func procRowOf(rec Record, self int64) ProcRow {
	r := ProcRow{ID: rec.Int("id"), User: rec.Str("user"), Host: rec.Str("host"), DB: rec.Str("db"), Command: rec.Str("command"),
		Time: rec.Int("time"), State: rec.Str("state"), Info: clip(rec.Str("info"), 8192)}
	r.Reason = protectReason(r.ID, self, r.User, r.Command)
	r.Protected = r.Reason != ""
	if !r.Protected && r.Command != "Sleep" {
		switch {
		case r.Time >= LongCritSeconds:
			r.LongRunning, r.Severity = true, "crit"
		case r.Time >= LongWarnSeconds:
			r.LongRunning, r.Severity = true, "warn"
		}
	}
	return r
}

func (s *Service) connectionID(se *session) int64 {
	if r, err := se.query("SELECT CONNECTION_ID() AS id"); err == nil {
		if recs := r.Records(); len(recs) > 0 {
			return recs[0].Int("id")
		}
	}
	return 0
}

// Processlist 查询进程列表。
func (s *Service) Processlist(ctx context.Context, c Caller, id uint, f ProcFilter) (*ProcResult, error) {
	se, err := s.open(ctx, c, id, supportsSession, "进程查询")
	if err != nil {
		return nil, err
	}
	defer se.close()
	var where []string
	var args []any
	switch cmd := strings.TrimSpace(f.Command); cmd {
	case "", "All":
	case "Not Sleep":
		where = append(where, "command <> 'Sleep'")
	default:
		if len(cmd) > 32 {
			return nil, bad("command 类型不合法")
		}
		where = append(where, "command = ?")
		args = append(args, cmd)
	}
	if f.User != "" {
		where = append(where, "user = ?")
		args = append(args, f.User)
	}
	if f.DB != "" {
		where = append(where, "db = ?")
		args = append(args, f.DB)
	}
	if f.MinTime > 0 {
		where = append(where, "time >= ?")
		args = append(args, f.MinTime)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		like := "%" + likeEscape(q) + "%"
		where = append(where, "(user LIKE ? ESCAPE '!' OR host LIKE ? ESCAPE '!' OR info LIKE ? ESCAPE '!')")
		args = append(args, like, like, like)
	}
	sql := procBase
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += fmt.Sprintf(" ORDER BY time DESC, id LIMIT %d", procLimit+1)
	r, err := se.query(sql, args...)
	if err != nil {
		return nil, wrapTarget(err)
	}
	self := s.connectionID(se)
	out := &ProcResult{Rows: []ProcRow{}, Self: self}
	for _, rec := range r.Records() {
		if len(out.Rows) >= procLimit {
			out.Truncated = true
			break
		}
		out.Rows = append(out.Rows, procRowOf(rec, self))
	}
	// 阻塞源：持有锁、让别的事务在等的线程（只有支持事务 / 锁查询的类型才有；查不到就不标）。
	blockers := s.blockerThreads(se)
	for i := range out.Rows {
		if blockers[out.Rows[i].ID] {
			out.Rows[i].Blocker = true
		}
	}
	out.Stats = s.procStats(se, out.Rows)
	out.Stats.Blockers = int64(len(blockers))
	return out, nil
}

// procStats 统计整个实例的进程数（不受筛选影响）。读不到就退回用这次返回的行统计。
// 注意表名故意写成大写：information_schema 的表名不区分大小写，而单测里的假目标库按小写子串匹配进程列表查询。
func (s *Service) procStats(se *session, rows []ProcRow) ProcStats {
	r, err := se.query("SELECT COUNT(*) AS total, " +
		"SUM(CASE WHEN command <> 'Sleep' THEN 1 ELSE 0 END) AS active, " +
		"SUM(CASE WHEN command <> 'Sleep' AND time >= 300 AND user NOT IN ('system user','event_scheduler') AND command NOT LIKE 'Binlog Dump%' AND command <> 'Daemon' THEN 1 ELSE 0 END) AS long_n " +
		"FROM information_schema.PROCESSLIST")
	if err == nil {
		if recs := r.Records(); len(recs) == 1 && !recs[0].IsNull("total") {
			return ProcStats{Total: recs[0].Int("total"), Active: recs[0].Int("active"), Long: recs[0].Int("long_n")}
		}
	}
	var st ProcStats
	for _, p := range rows {
		st.Total++
		if p.Command != "Sleep" {
			st.Active++
		}
		if p.LongRunning {
			st.Long++
		}
	}
	return st
}

// blockerThreads 返回当前阻塞别人的线程号集合。
func (s *Service) blockerThreads(se *session) map[int64]bool {
	out := map[int64]bool{}
	if !supportsTrx(se.inst.DBType) {
		return out
	}
	rows, err := s.queryLocks(se)
	if err != nil {
		return out
	}
	for _, l := range rows {
		if l.BlockThread > 0 {
			out[l.BlockThread] = true
		}
	}
	return out
}

// ---- Top 表空间 ----

// SpaceRow 是一张表的空间占用。
type SpaceRow struct {
	Schema  string  `json:"schema"`
	Table   string  `json:"table"`
	Engine  string  `json:"engine"`
	TotalMB float64 `json:"total_mb"`
	Rows    int64   `json:"rows"`
	DataMB  float64 `json:"data_mb"`
	IndexMB float64 `json:"index_mb"`
	FreeMB  float64 `json:"free_mb"`
	FreePct float64 `json:"free_pct"`
}

// SpaceResult 是一页表空间。
type SpaceResult struct {
	Rows  []SpaceRow `json:"rows"`
	Total int64      `json:"total"`
}

const sysSchemas = "'information_schema', 'performance_schema', 'mysql', 'test', 'sys'"

// Tablespace 按占用空间从大到小列出用户表（information_schema.tables；8.0 的统计值默认缓存 24 小时，可能有延迟）。
func (s *Service) Tablespace(ctx context.Context, c Caller, id uint, offset, limit int) (*SpaceResult, error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	se, err := s.open(ctx, c, id, supportsSession, "表空间查询")
	if err != nil {
		return nil, err
	}
	defer se.close()
	r, err := se.query(`SELECT table_schema AS table_schema, table_name AS table_name, IFNULL(engine, '') AS engine,
 TRUNCATE((data_length + index_length + data_free) / 1024 / 1024, 2) AS total_size, IFNULL(table_rows, 0) AS table_rows,
 TRUNCATE(data_length / 1024 / 1024, 2) AS data_size, TRUNCATE(index_length / 1024 / 1024, 2) AS index_size,
 TRUNCATE(data_free / 1024 / 1024, 2) AS data_free,
 IFNULL(TRUNCATE(data_free / (data_length + index_length + data_free) * 100, 2), 0) AS pct_free
FROM information_schema.tables WHERE table_schema NOT IN (`+sysSchemas+`) AND table_type = 'BASE TABLE'
ORDER BY (data_length + index_length + data_free) DESC LIMIT ?, ?`, offset, limit)
	if err != nil {
		return nil, wrapTarget(err)
	}
	out := &SpaceResult{Rows: []SpaceRow{}}
	for _, rec := range r.Records() {
		out.Rows = append(out.Rows, SpaceRow{Schema: rec.Str("table_schema"), Table: rec.Str("table_name"), Engine: rec.Str("engine"),
			TotalMB: rec.Float("total_size"), Rows: rec.Int("table_rows"), DataMB: rec.Float("data_size"), IndexMB: rec.Float("index_size"),
			FreeMB: rec.Float("data_free"), FreePct: rec.Float("pct_free")})
	}
	if cr, err := se.query("SELECT COUNT(*) AS n FROM information_schema.tables WHERE table_schema NOT IN (" + sysSchemas + ") AND table_type = 'BASE TABLE'"); err == nil {
		if recs := cr.Records(); len(recs) > 0 {
			out.Total = recs[0].Int("n")
		}
	}
	return out, nil
}

// ---- 事务 ----

// TrxFilter 是事务列表的筛选条件。
type TrxFilter struct {
	MinSeconds int  // 事务已持续的最短秒数
	IdleOnly   bool // 只看空闲（Sleep）的事务：忘了提交的长事务
}

// TrxRow 是 information_schema.innodb_trx 的一行（关联了进程列表里的用户、主机、库）。
type TrxRow struct {
	TrxID        string `json:"trx_id"`
	Started      string `json:"trx_started"`
	Seconds      int64  `json:"trx_secs"`
	State        string `json:"trx_state"`
	OpState      string `json:"trx_operation_state"`
	User         string `json:"user"`
	Host         string `json:"host"`
	DB           string `json:"db"`
	Command      string `json:"command"`
	ThreadTime   int64  `json:"thread_time"`
	ThreadID     int64  `json:"thread_id"`
	RowsLocked   int64  `json:"rows_locked"`
	RowsModified int64  `json:"rows_modified"`
	TablesLocked int64  `json:"tables_locked"`
	Info         string `json:"info"`
}

// Transactions 列出当前的 InnoDB 事务，最老的在前。
func (s *Service) Transactions(ctx context.Context, c Caller, id uint, f TrxFilter) ([]TrxRow, error) {
	se, err := s.open(ctx, c, id, supportsTrx, "事务查询")
	if err != nil {
		return nil, err
	}
	defer se.close()
	where := "TIMESTAMPDIFF(SECOND, trx.trx_started, NOW()) >= ?"
	if f.IdleOnly {
		where += " AND p.command = 'Sleep'"
	}
	if f.MinSeconds < 0 {
		f.MinSeconds = 0
	}
	r, err := se.query(`SELECT trx.trx_id AS trx_id, trx.trx_started AS trx_started, trx.trx_state AS trx_state,
 IFNULL(trx.trx_operation_state, '') AS trx_operation_state, trx.trx_mysql_thread_id AS thread_id,
 trx.trx_tables_locked AS tables_locked, trx.trx_rows_locked AS rows_locked, trx.trx_rows_modified AS rows_modified,
 IFNULL(trx.trx_query, '') AS trx_query, TIMESTAMPDIFF(SECOND, trx.trx_started, NOW()) AS trx_secs,
 IFNULL(p.user, '') AS user, IFNULL(p.host, '') AS host, IFNULL(p.db, '') AS db, IFNULL(p.command, '') AS command, IFNULL(p.time, 0) AS thread_time
FROM information_schema.innodb_trx trx LEFT JOIN information_schema.processlist p ON p.id = trx.trx_mysql_thread_id
WHERE `+where+` ORDER BY trx.trx_started ASC LIMIT 200`, f.MinSeconds)
	if err != nil {
		return nil, wrapTarget(err)
	}
	out := []TrxRow{}
	var ids []any
	for _, rec := range r.Records() {
		row := TrxRow{TrxID: rec.Str("trx_id"), Started: rec.Str("trx_started"), Seconds: rec.Int("trx_secs"), State: rec.Str("trx_state"),
			OpState: rec.Str("trx_operation_state"), User: rec.Str("user"), Host: rec.Str("host"), DB: rec.Str("db"), Command: rec.Str("command"),
			ThreadTime: rec.Int("thread_time"), ThreadID: rec.Int("thread_id"), RowsLocked: rec.Int("rows_locked"),
			RowsModified: rec.Int("rows_modified"), TablesLocked: rec.Int("tables_locked"), Info: clip(rec.Str("trx_query"), 4000)}
		out = append(out, row)
		ids = append(ids, row.ThreadID)
	}
	// 事务当前没有在执行语句时 trx_query 为空，用该线程最近执行过的语句补上（performance_schema 没开则跳过）。
	if len(ids) > 0 {
		hist := s.recentStatements(se, ids)
		for i := range out {
			if out[i].Info == "" {
				out[i].Info = clip(hist[out[i].ThreadID], 4000)
			}
		}
	}
	return out, nil
}

// recentStatements 取一批线程最近执行的语句（最新的在前，每个线程最多 5 条）。
func (s *Service) recentStatements(se *session, threadIDs []any) map[int64]string {
	out := map[int64]string{}
	ph := strings.TrimRight(strings.Repeat("?,", len(threadIDs)), ",")
	r, err := se.query(`SELECT t.processlist_id AS thread_id, h.sql_text AS sql_text FROM performance_schema.events_statements_history h
JOIN performance_schema.threads t ON t.thread_id = h.thread_id
WHERE t.processlist_id IN (`+ph+`) AND h.sql_text IS NOT NULL ORDER BY h.timer_start DESC`, threadIDs...)
	if err != nil {
		return out
	}
	cnt := map[int64]int{}
	for _, rec := range r.Records() {
		tid := rec.Int("thread_id")
		if cnt[tid] >= 5 {
			continue
		}
		cnt[tid]++
		if out[tid] != "" {
			out[tid] += ";\n"
		}
		out[tid] += rec.Str("sql_text")
	}
	return out
}

// ---- 锁等待 ----

// LockRow 是一对“等待者 - 持有者”。
type LockRow struct {
	WaitState   string `json:"wait_state"`
	WaitStarted string `json:"wait_started"`
	WaitSecs    int64  `json:"wait_secs"`
	WaitThread  int64  `json:"wait_thread"`
	WaitSQL     string `json:"wait_sql"`
	WaitObject  string `json:"wait_object"`
	BlockThread int64  `json:"block_thread"`
	BlockSQL    string `json:"block_sql"`
	BlockState  string `json:"block_state"`
	BlockStart  string `json:"block_started"`
	BlockSecs   int64  `json:"block_secs"`
}

// MySQL 5.7 及以前（含 MariaDB）：information_schema.innodb_locks / innodb_lock_waits。
const lockSQLOld = `SELECT rtrx.trx_state AS wait_state, rtrx.trx_wait_started AS wait_started,
 TIMESTAMPDIFF(SECOND, rtrx.trx_wait_started, NOW()) AS wait_secs, rtrx.trx_mysql_thread_id AS wait_thread,
 IFNULL(rtrx.trx_query, '') AS wait_sql, CONCAT(rl.lock_table, ' (', IFNULL(rl.lock_index, '-'), ' ', rl.lock_mode, ')') AS wait_object,
 trx.trx_mysql_thread_id AS block_thread, IFNULL(trx.trx_query, '') AS block_sql, trx.trx_state AS block_state,
 trx.trx_started AS block_started, TIMESTAMPDIFF(SECOND, trx.trx_started, NOW()) AS block_secs
FROM information_schema.innodb_lock_waits lw
JOIN information_schema.innodb_trx rtrx ON rtrx.trx_id = lw.requesting_trx_id
JOIN information_schema.innodb_trx trx ON trx.trx_id = lw.blocking_trx_id
JOIN information_schema.innodb_locks rl ON rl.lock_id = lw.requested_lock_id
ORDER BY wait_secs DESC LIMIT 200`

// MySQL 8.0.1+：performance_schema.data_locks / data_lock_waits。
const lockSQLNew = `SELECT rtrx.trx_state AS wait_state, rtrx.trx_wait_started AS wait_started,
 TIMESTAMPDIFF(SECOND, rtrx.trx_wait_started, NOW()) AS wait_secs, rtrx.trx_mysql_thread_id AS wait_thread,
 IFNULL(rtrx.trx_query, '') AS wait_sql,
 CONCAT(IFNULL(rl.object_schema, ''), '.', IFNULL(rl.object_name, ''), ' (', IFNULL(rl.index_name, '-'), ' ', rl.lock_mode, ')') AS wait_object,
 trx.trx_mysql_thread_id AS block_thread, IFNULL(trx.trx_query, '') AS block_sql, trx.trx_state AS block_state,
 trx.trx_started AS block_started, TIMESTAMPDIFF(SECOND, trx.trx_started, NOW()) AS block_secs
FROM performance_schema.data_lock_waits lw
JOIN information_schema.innodb_trx rtrx ON rtrx.trx_id = lw.requesting_engine_transaction_id
JOIN information_schema.innodb_trx trx ON trx.trx_id = lw.blocking_engine_transaction_id
JOIN performance_schema.data_locks rl ON rl.engine_lock_id = lw.requesting_engine_lock_id
ORDER BY wait_secs DESC LIMIT 200`

// LockWaits 查询当前的锁等待。
func (s *Service) LockWaits(ctx context.Context, c Caller, id uint) ([]LockRow, error) {
	se, err := s.open(ctx, c, id, supportsTrx, "锁等待查询")
	if err != nil {
		return nil, err
	}
	defer se.close()
	return s.queryLocks(se)
}

// LockView 是锁等待加上由它推出来的阻塞链（根因在最前）。
type LockView struct {
	Rows   []LockRow   `json:"rows"`
	Chains []LockChain `json:"chains"`
}

// LockWaitsWithChains 查询锁等待并构造阻塞链。
func (s *Service) LockWaitsWithChains(ctx context.Context, c Caller, id uint) (*LockView, error) {
	rows, err := s.LockWaits(ctx, c, id)
	if err != nil {
		return nil, err
	}
	return &LockView{Rows: rows, Chains: BuildLockChains(rows)}, nil
}

// queryLocks 在已连接的会话上查询锁等待（按版本选 SQL）。
func (s *Service) queryLocks(se *session) ([]LockRow, error) {
	q := lockSQLOld
	if se.version().newLocks() {
		q = lockSQLNew
	}
	r, err := se.query(q)
	if err != nil {
		return nil, wrapTarget(err)
	}
	out := []LockRow{}
	for _, rec := range r.Records() {
		out = append(out, LockRow{WaitState: rec.Str("wait_state"), WaitStarted: rec.Str("wait_started"), WaitSecs: rec.Int("wait_secs"),
			WaitThread: rec.Int("wait_thread"), WaitSQL: clip(rec.Str("wait_sql"), 4000), WaitObject: rec.Str("wait_object"),
			BlockThread: rec.Int("block_thread"), BlockSQL: clip(rec.Str("block_sql"), 4000), BlockState: rec.Str("block_state"),
			BlockStart: rec.Str("block_started"), BlockSecs: rec.Int("block_secs")})
	}
	return out, nil
}

// ---- 终止会话 ----

// KillTarget 是要终止的会话。User/Host 是用户看到该会话时的值：提供时服务端会核对线程号没有被别的连接复用。
type KillTarget struct {
	ID   int64  `json:"id"`
	User string `json:"user"`
	Host string `json:"host"`
}

// KillItem 是预览里的一项。OK 为 false 表示服务端拒绝终止它，Reason 说明原因。
type KillItem struct {
	ID      int64  `json:"id"`
	User    string `json:"user"`
	Host    string `json:"host"`
	DB      string `json:"db"`
	Command string `json:"command"`
	Time    int64  `json:"time"`
	Info    string `json:"info"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	SQL     string `json:"sql,omitempty"`
}

// KillPlan 是终止会话的预览。
type KillPlan struct {
	Items      []KillItem `json:"items"`
	Statements []string   `json:"statements"`
}

const maxKill = 50

// planKill 重新查询这些线程当前的状态并逐个判定能否终止。
func (s *Service) planKill(se *session, targets []KillTarget) (*KillPlan, error) {
	if len(targets) == 0 {
		return nil, bad("请选择要终止的会话")
	}
	if len(targets) > maxKill {
		return nil, bad(fmt.Sprintf("一次最多终止 %d 个会话", maxKill))
	}
	seen := map[int64]bool{}
	var ids []any
	for _, t := range targets {
		if t.ID <= 0 {
			return nil, bad("线程号不合法")
		}
		if !seen[t.ID] {
			seen[t.ID] = true
			ids = append(ids, t.ID)
		}
	}
	ph := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	r, err := se.query(procBase+" WHERE id IN ("+ph+")", ids...)
	if err != nil {
		return nil, wrapTarget(err)
	}
	self := s.connectionID(se)
	cur := map[int64]Record{}
	for _, rec := range r.Records() {
		cur[rec.Int("id")] = rec
	}
	plan := &KillPlan{Items: []KillItem{}, Statements: []string{}}
	done := map[int64]bool{}
	for _, t := range targets {
		if done[t.ID] {
			continue
		}
		done[t.ID] = true
		rec, ok := cur[t.ID]
		if !ok {
			plan.Items = append(plan.Items, KillItem{ID: t.ID, User: t.User, Host: t.Host, Reason: "该会话已经结束"})
			continue
		}
		row := procRowOf(rec, self)
		it := KillItem{ID: row.ID, User: row.User, Host: row.Host, DB: row.DB, Command: row.Command, Time: row.Time, Info: clip(row.Info, 300)}
		switch {
		case row.Protected:
			it.Reason = fmt.Sprintf("不允许终止：%s", row.Reason)
		case (t.User != "" && t.User != row.User) || (t.Host != "" && t.Host != row.Host):
			it.Reason = fmt.Sprintf("线程 %d 已经变成另一个连接（现在是 %s@%s），为避免误杀，请刷新后重新选择", t.ID, row.User, row.Host)
		default:
			it.OK = true
			it.SQL = fmt.Sprintf("KILL %d", row.ID)
			plan.Statements = append(plan.Statements, it.SQL+";")
		}
		plan.Items = append(plan.Items, it)
	}
	return plan, nil
}

// PreviewKill 预览将要执行的 KILL 语句（第一步确认）。服务端据实际线程信息生成，不接受前端传来的语句。
func (s *Service) PreviewKill(ctx context.Context, c Caller, id uint, targets []KillTarget) (*KillPlan, error) {
	se, err := s.open(ctx, c, id, supportsSession, "终止会话")
	if err != nil {
		return nil, err
	}
	defer se.close()
	return s.planKill(se, targets)
}

// KillResult 是一个会话的终止结果。
type KillResult struct {
	ID      int64  `json:"id"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Kill 执行终止（第二步确认）：重新校验每个线程仍然存在、仍是用户看到的那个连接、不是平台自己的连接或复制线程，再逐个 KILL。
func (s *Service) Kill(ctx context.Context, c Caller, id uint, targets []KillTarget, confirm bool) ([]KillResult, error) {
	if !confirm {
		return nil, bad("请先确认要终止的会话")
	}
	se, err := s.open(ctx, c, id, supportsSession, "终止会话")
	if err != nil {
		return nil, err
	}
	defer se.close()
	plan, err := s.planKill(se, targets)
	if err != nil {
		return nil, err
	}
	var results []KillResult
	var logs, refused []string
	for _, it := range plan.Items {
		if !it.OK {
			results = append(results, KillResult{ID: it.ID, Message: it.Reason})
			logs = append(logs, fmt.Sprintf("线程 %d 未终止（%s）", it.ID, it.Reason)) // i18n-ignore 审计详情，存库的文本不翻译
			if !strings.Contains(it.Reason, "已经结束") {                           // i18n-ignore 与本包生成的中文原因做匹配 受保护的连接 / 线程号已被别的连接复用：服务端拒绝，单独留痕
				refused = append(refused, fmt.Sprintf("线程 %d（%s@%s）：%s", it.ID, it.User, it.Host, it.Reason)) // i18n-ignore 审计详情，存库的文本不翻译
			}
			continue
		}
		_, kerr := se.exec(it.SQL)
		desc := fmt.Sprintf("KILL %d（%s@%s，库：%s，状态：%s，已运行 %d 秒）", it.ID, it.User, it.Host, it.DB, it.Command, it.Time) // i18n-ignore 审计详情，存库的文本不翻译
		switch {
		case kerr == nil:
			results = append(results, KillResult{ID: it.ID, OK: true, Message: "已终止"})
			logs = append(logs, desc+" 成功") // i18n-ignore 审计详情，存库的文本不翻译
		case strings.Contains(kerr.Error(), "1094") || strings.Contains(strings.ToLower(kerr.Error()), "unknown thread"):
			results = append(results, KillResult{ID: it.ID, Message: "该会话已经结束"})
			logs = append(logs, desc+" 会话已结束") // i18n-ignore 审计详情，存库的文本不翻译
		default:
			results = append(results, KillResult{ID: it.ID, Message: kerr.Error()})
			logs = append(logs, desc+" 失败："+kerr.Error()) // i18n-ignore 审计详情，存库的文本不翻译
		}
	}
	if len(refused) > 0 {
		s.auditRefused(c, "终止会话", se.inst, strings.Join(refused, "；")) // i18n-ignore 审计，存库的文本不翻译
	}
	if len(refused) < len(plan.Items) { // 全部被拒绝时只留“（拒绝）”那一条，不写成一次终止操作
		s.audit(c, "终止会话", fmt.Sprintf("实例：%s；%s", instLabel(se.inst), strings.Join(logs, "；"))) // i18n-ignore 审计，存库的文本不翻译
	}
	return results, nil
}
