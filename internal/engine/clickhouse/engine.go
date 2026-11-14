// @author: kevinkelly
// @license: Apache Licence
// @file: engine.go
// @time: 2026/10/28

// Package clickhouse 是 ClickHouse 引擎的 Go 实现：只用标准库的 HTTP 接口，覆盖在线查询（只读）与库表元数据。
//
// 在线查询的流程与 Redis / SQL Server 一致：先 CheckUserQuery（纯静态检查，不连服务端），
// 上层据 Checked.Tables / Databases 做库表权限校验，再 Engine.RunUser 真正执行并整形结果。
// 防护分层：静态检查 → 每个请求 readonly + 各类限额（见 client.go）→ 执行前核对所涉表的引擎 → 结果行数 / 字节 / 单元格截断。
package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"dbtool/internal/engine"
)

// DBType 是实例类型，与 internal/instmgr/types.go 一致。
const DBType = "clickhouse"

func init() {
	engine.Register(DBType, func(c engine.Conn) (engine.Engine, error) { return New(c) })
}

// Engine 实现 engine.Engine。每个请求都是独立的 HTTP 调用（连接由 http.Transport 复用），没有会话状态。
type Engine struct {
	cfg Config
	cli *Client
}

// New 按约定创建引擎（不立即连接）。需要 RunUser / 元数据方法时，对返回值做 *Engine 类型断言，或直接用 NewEngine。
func New(c engine.Conn) (engine.Engine, error) { return NewEngine(c) }

// NewEngine 同 New，但返回具体类型。
func NewEngine(c engine.Conn) (*Engine, error) { return NewWithConfig(ConfigFromConn(c)) }

// NewWithConfig 用完整配置创建引擎（可指定 Readonly 级别等）。
func NewWithConfig(cfg Config) (*Engine, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("ClickHouse 实例地址为空")
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return nil, errors.New("ClickHouse 端口不合法")
	}
	return &Engine{cfg: cfg, cli: NewClient(cfg)}, nil
}

func (e *Engine) Name() string { return DBType }

func (e *Engine) Close() error {
	e.cli.Close()
	return nil
}

// ---- 在线查询 ----

// Query 实现 engine.Engine：自己再做一遍 CheckUserQuery（默认拒绝），然后执行并返回结果集。
// opt.DBName 为当前库，opt.Limit 为行数上限（0 用默认值），opt.MaxExecMs 为超时（0 用默认值）。
// 上层的在线查询流水线应使用 CheckUserQuery + RunUser，以便拿到截断标记、警告与涉及的库表。
func (e *Engine) Query(ctx context.Context, sql string, opt engine.QueryOptions) (*engine.ResultSet, error) {
	db := opt.DBName
	if db == "" {
		db = e.cfg.Database
	}
	ck, err := CheckUserQuery(sql, CheckOptions{DB: db})
	if err != nil {
		return nil, err
	}
	sh, err := e.RunUser(ctx, ck, QueryLimits{MaxRows: opt.Limit, Timeout: time.Duration(opt.MaxExecMs) * time.Millisecond})
	if err != nil {
		return nil, err
	}
	return sh.ToResultSet(), nil
}

// RunUser 执行通过 CheckUserQuery 的语句并整形结果。
//
//   - 执行前用 system.tables 核对语句涉及的表：引擎为 File / URL / S3 / MySQL / Kafka 等会读服务器文件或访问外部系统的，拒绝；
//     查不到引擎信息时按“无法确认”拒绝（默认拒绝）；
//   - 每个请求带 readonly、query.max_execution_seconds、max_result_rows / bytes、max_rows_to_read / bytes、max_memory_usage、max_threads；
//   - 超过行数 / 字节上限的结果被截断并标记 Truncated；单元格超过 lim.MaxCellBytes 被截断；
//   - 并发由调用方控制。
func (e *Engine) RunUser(ctx context.Context, ck *Checked, lim QueryLimits) (*Shaped, error) {
	if ck == nil || strings.TrimSpace(ck.Stmt) == "" {
		return nil, errors.New("语句为空")
	}
	lim = lim.Normalize()
	start := time.Now()
	db := ck.DB
	if db == "" {
		db = e.cfg.Database
	}
	if err := e.checkEngines(ctx, ck, lim); err != nil {
		return nil, err
	}
	// 引擎核对占用的时间也算在超时预算里
	if left := lim.Timeout - time.Since(start); left < time.Second {
		lim.Timeout = time.Second
	} else {
		lim.Timeout = left
	}
	sh, err := e.run(ctx, ck.Stmt, reqOpts{db: db, QueryLimits: lim})
	if err != nil {
		return nil, err
	}
	sh.Notices = append(sh.Notices, ck.Notices...)
	sh.ElapsedMs = time.Since(start).Milliseconds()
	if sh.Truncated {
		switch sh.TruncatedBy {
		case "rows":
			sh.Warnings = append(sh.Warnings, fmt.Sprintf("结果超过 %d 行上限，已截断；请加 WHERE / LIMIT 缩小范围", lim.MaxRows))
		case "bytes":
			sh.Warnings = append(sh.Warnings, fmt.Sprintf("结果超过 %d MB 上限，已截断；请只查询需要的列", lim.MaxBytes>>20))
		default:
			sh.Warnings = append(sh.Warnings, "服务端返回的数据量超过读取上限，已截断")
		}
	}
	return sh, nil
}

// run 发送请求并解析整形。
func (e *Engine) run(ctx context.Context, stmt string, o reqOpts) (*Shaped, error) {
	resp, cancel, err := e.cli.do(ctx, stmt, o)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()
	hard := o.MaxBytes*2 + (1 << 20)
	if hard > 256<<20 {
		hard = 256 << 20
	}
	cr := &capReader{r: resp.Body, left: hard}
	p, err := parseJSONCompact(cr, o.MaxRows, o.MaxBytes)
	if err != nil {
		var se *ServerError
		if errors.As(err, &se) || errors.Is(err, ErrTimeout) {
			return nil, err
		}
		// 读取过程中被取消 / 超时：以调用方上下文为准
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, ErrCanceled
			}
			return nil, ErrTimeout
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, fmt.Errorf("%s", scrub(err.Error(), e.cfg.Password))
	}
	if t := resp.Trailer.Get("X-ClickHouse-Exception-Code"); t != "" {
		return nil, e.cli.serverError(200, resp.Trailer, nil)
	}
	return shape(p, o.MaxCellBytes), nil
}

// checkEngines 查询语句涉及的表的引擎，命中 denyEngines 则拒绝。system / information_schema 里的表不查（已在静态检查里限定）。
func (e *Engine) checkEngines(ctx context.Context, ck *Checked, lim QueryLimits) error {
	var pairs []string
	for _, t := range ck.Tables {
		if t.Name == "" || isSystemDB(t.Schema) || isInfoSchemaDB(t.Schema) {
			continue
		}
		pairs = append(pairs, "("+sqlString(t.Schema)+", "+sqlString(t.Name)+")")
	}
	if len(pairs) == 0 {
		return nil
	}
	sql := "SELECT database, name, engine FROM system.tables WHERE (database, name) IN (" + strings.Join(pairs, ", ") + ")"
	o := reqOpts{QueryLimits: QueryLimits{MaxRows: maxTablesPerQuery + 1, MaxBytes: 1 << 20, Timeout: minDur(lim.Timeout, 15*time.Second),
		MaxRowsToRead: 10_000_000, MaxBytesToRead: 1 << 30, MaxMemory: 512 << 20, MaxThreads: 1}.Normalize()}
	sh, err := e.run(ctx, sql, o)
	if err != nil {
		if IsCanceled(err) || IsTimeout(err) {
			return err
		}
		return fmt.Errorf("无法确认所查询表的引擎，已按默认拒绝处理：%w", err)
	}
	for _, r := range sh.Rows {
		if len(r) < 3 {
			return errors.New("无法确认所查询表的引擎：服务端返回的列数不对，已按默认拒绝处理")
		}
		eng, _ := r[2].(string)
		if why, bad := denyEngines[strings.ToLower(eng)]; bad {
			return uerr("不允许查询 %v.%v：该表使用 %s 引擎（%s）", r[0], r[1], eng, why)
		}
	}
	return nil
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// sqlString 生成 ClickHouse 字符串字面量（转义反斜杠与单引号）。
func sqlString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	s = strings.ReplaceAll(s, "\x00", `\0`)
	return "'" + s + "'"
}

// ---- 平台内部的元数据查询 ----

// metaLimits 是内部元数据查询的限额：行数多一些，其余同样受限。
var metaLimits = QueryLimits{MaxRows: 200_000, MaxBytes: 32 << 20, Timeout: 30 * time.Second, MaxRowsToRead: 200_000_000,
	MaxBytesToRead: 20 << 30, MaxMemory: 1 << 30, MaxThreads: 2}

// rowset 是内部查询的结果，按列名取值。
type rowset struct {
	cols []string
	rows [][]any
}

func (r *rowset) idx(name string) int {
	for i, c := range r.cols {
		if c == name {
			return i
		}
	}
	return -1
}

// str 取字符串列；NULL 或非字符串返回空串。
func (r *rowset) str(row []any, name string) string {
	i := r.idx(name)
	if i < 0 || i >= len(row) {
		return ""
	}
	switch v := row[i].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (r *rowset) i64(row []any, name string) (int64, bool) {
	i := r.idx(name)
	if i < 0 || i >= len(row) || row[i] == nil {
		return 0, false
	}
	switch v := row[i].(type) {
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case string:
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func (e *Engine) meta(ctx context.Context, sql string) (*rowset, error) {
	sh, err := e.run(ctx, sql, reqOpts{QueryLimits: metaLimits.Normalize()})
	if err != nil {
		return nil, err
	}
	if sh.Truncated {
		return nil, errors.New("元数据查询结果过大，已中止")
	}
	return &rowset{cols: sh.ColumnList, rows: sh.Rows}, nil
}

func checkName(kind, s string) error {
	if s == "" {
		return fmt.Errorf("%s名为空", kind)
	}
	if len(s) > 512 || strings.ContainsAny(s, "\x00\r\n") {
		return fmt.Errorf("%s名不合法", kind)
	}
	return nil
}

// ---- engine.Engine 的元数据方法 ----

func (e *Engine) TestConnection(ctx context.Context) error {
	rs, err := e.meta(ctx, "SELECT 1")
	if err != nil {
		return err
	}
	if len(rs.rows) != 1 {
		return errors.New("ClickHouse 返回了意料之外的结果")
	}
	return nil
}

func (e *Engine) ServerVersion(ctx context.Context) (string, error) {
	rs, err := e.meta(ctx, "SELECT version() AS v")
	if err != nil {
		return "", err
	}
	if len(rs.rows) == 0 {
		return "", errors.New("取不到版本")
	}
	return rs.str(rs.rows[0], "v"), nil
}

// hiddenDatabases 是不当作业务库列出的内部库。
const hiddenDatabases = "('system', 'INFORMATION_SCHEMA', 'information_schema')"

// AllDatabases 返回账号可见的业务库（排除 system / INFORMATION_SCHEMA / information_schema）。
func (e *Engine) AllDatabases(ctx context.Context) ([]string, error) {
	rs, err := e.meta(ctx, "SELECT name FROM system.databases WHERE name NOT IN "+hiddenDatabases+" ORDER BY name")
	if err != nil {
		return nil, err
	}
	return rs.column("name"), nil
}

// AllTables 返回库里的表 / 视图 / 字典（排除物化视图的内部表 .inner*）。
func (e *Engine) AllTables(ctx context.Context, db string) ([]string, error) {
	if err := checkName("库", db); err != nil {
		return nil, err
	}
	rs, err := e.meta(ctx, "SELECT name FROM system.tables WHERE database = "+sqlString(db)+" AND NOT startsWith(name, '.inner') ORDER BY name")
	if err != nil {
		return nil, err
	}
	return rs.column("name"), nil
}

// AllColumns 返回表的列名（按列序）。
func (e *Engine) AllColumns(ctx context.Context, db, table string) ([]string, error) {
	if err := checkName("库", db); err != nil {
		return nil, err
	}
	if err := checkName("表", table); err != nil {
		return nil, err
	}
	rs, err := e.meta(ctx, "SELECT name FROM system.columns WHERE database = "+sqlString(db)+" AND table = "+sqlString(table)+" ORDER BY position")
	if err != nil {
		return nil, err
	}
	return rs.column("name"), nil
}

func (r *rowset) column(name string) []string {
	out := make([]string, 0, len(r.rows))
	for _, row := range r.rows {
		out = append(out, r.str(row, name))
	}
	return out
}

var _ engine.Engine = (*Engine)(nil)
