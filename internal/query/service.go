// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

// Package query 实现在线查询的业务流水线。
//
// 流水线：语句检查、禁用 SELECT *、库表权限与行数上限（auth.go）、LIMIT 改写、超时、审计日志。
// 脱敏在 M3 接入。
package query

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/firewall"
	"dbtool/internal/instext"
	"dbtool/internal/masking"
	"dbtool/internal/model"
	"dbtool/internal/settings"
	"dbtool/internal/sqlx"
)

// Principal 是当前登录用户。
type Principal struct {
	ID          uint
	Username    string
	Display     string
	IsSuperuser bool
}

// Repo 是查询服务依赖的元数据访问接口（由 store 实现，测试里用假实现）。
type Repo interface {
	ListInstances() ([]model.Instance, error)
	GetInstance(id uint) (*model.Instance, error)
	EngineConn(inst *model.Instance) engine.Conn
	ConfigValue(item string) (string, bool)
	InsertQueryLog(l *model.QueryLog) error
	ActiveMaskingColumns(instanceID uint) ([]model.DataMaskingColumn, error)
	PrivRepo
}

// Opener 按类型创建引擎，默认 engine.New。
type Opener func(dbType string, c engine.Conn) (engine.Engine, error)

// Decision 是权限校验的结果。
type Decision struct {
	MaxRows   int    // 该用户本次查询允许的最大返回行数
	Source    string // 限制来源，展示给用户
	PrivCheck bool   // 是否经过了正常的库表权限校验（写入 dbt_query_log.priv_check）
}

// Authorizer 判定用户能否访问实例、能否执行某条语句。M2 将实现库表级权限。
type Authorizer interface {
	CanAccessPage(p Principal) bool
	FilterInstances(p Principal, list []model.Instance) ([]model.Instance, error)
	CanUseInstance(p Principal, inst *model.Instance) error
	AuthorizeQuery(ctx context.Context, p Principal, inst *model.Instance, db, stmt string, adminLimit int) (Decision, error)
}

// Error 是对外可见的业务错误，Code 供前端区分展示。
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	HTTP    int            `json:"-"`
	Data    map[string]any `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// statusTargetError 是“目标库 / 引擎返回了错误”的 HTTP 状态。不用 502：反向代理 / 网关常常拦截 5xx 并替换响应体，
// 浏览器就只剩“请求失败（HTTP 502）”，看不到目标库返回的真实错误信息。
const statusTargetError = 422 // Unprocessable Entity

func newErr(status int, code, msg string) *Error {
	return &Error{Code: code, Message: msg, HTTP: status}
}

type Service struct {
	repo Repo
	open Opener
	auth Authorizer
	sel  SelectListProvider
	fw   *firewall.Engine // SQL 防火墙（可选，见 firewall.go）
}

func NewService(repo Repo, open Opener, auth Authorizer) *Service {
	if open == nil {
		open = engine.New
	}
	if auth == nil {
		auth = NewPrivAuthorizer(repo, nil)
	}
	return &Service{repo: repo, open: open, auth: auth, sel: NewAutoExtractor(repo)}
}

// SetSelectListProvider 替换结果列来源的解析器（server 用它与授权器共享同一个 goInception 配置与严格模式）。
func (s *Service) SetSelectListProvider(p SelectListProvider) { s.sel = p }

// Policy 是系统配置里与查询相关的开关，对应 dbt_setting。
type Policy struct {
	DisableStar    bool `json:"disable_star"`
	MaxExecSeconds int  `json:"max_exec_seconds"`
	AdminLimit     int  `json:"admin_limit"`
	DataMasking    bool `json:"data_masking"`
	QueryCheck     bool `json:"query_check"`
	// 结果的字节上限（目前只有 SQL Server 在线查询遵守）：单个单元格 / 整个结果集。系统配置 query.max_cell_bytes / query.max_result_bytes。
	MaxCellBytes   int `json:"max_cell_bytes"`
	MaxResultBytes int `json:"max_result_bytes"`
}

func (s *Service) Policy() Policy {
	return Policy{
		DisableStar:    s.cfgBool(settings.KeyQueryForbidSelectStar),
		MaxExecSeconds: s.cfgInt(settings.KeyQueryMaxExecSeconds, 60),
		AdminLimit:     s.cfgInt(settings.KeyQueryAdminRowLimit, 5000),
		DataMasking:    s.cfgBool(settings.KeyQueryMaskEnabled),
		QueryCheck:     s.cfgBool(settings.KeyQueryBlockOnMaskError),
		MaxCellBytes:   s.cfgInt(settings.KeyQueryMaxCellBytes, defaultMaxCellBytes),
		MaxResultBytes: s.cfgInt(settings.KeyQueryMaxResultBytes, defaultMaxResultBytes),
	}
}

// 在线查询结果的默认字节上限：单元格 4 MiB（生成脚本类的查询一个格子就是几百 KB），整个结果 32 MiB。
const (
	defaultMaxCellBytes   = 4 << 20
	defaultMaxResultBytes = 32 << 20
)

func (s *Service) cfgBool(item string) bool {
	v, ok := s.repo.ConfigValue(item)
	return ok && strings.EqualFold(strings.TrimSpace(v), "true")
}

func (s *Service) cfgInt(item string, def int) int {
	v, ok := s.repo.ConfigValue(item)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// ---- 实例与元数据 ----

type InstanceView struct {
	ID     uint     `json:"id"`
	Name   string   `json:"name"`
	DBType string   `json:"db_type"`
	Role   string   `json:"role"` // master / slave
	Env    string   `json:"env"`  // 环境字典里的编码；实例管理里设置的优先，没设置时由标签推断（prod / test / ""）
	Tags   []string `json:"tags"`
	// 环境的显示名与“按生产对待”（环境字典里的定义），页面徽标与顶部色条用
	EnvLabel string `json:"env_label"`
	EnvProd  bool   `json:"env_prod"`
}

// envFromTags 由实例标签推断环境：含 prod/生产 视为生产，含 test/测试 视为测试。
// 这里只用于界面提示，不参与任何权限判断。
func envFromTags(tags []model.InstanceTag) string { return firewall.EnvOf(tags) }

func toView(i model.Instance) InstanceView {
	v := InstanceView{ID: i.ID, Name: i.InstanceName, DBType: i.DBType, Role: i.Type, Env: firewall.EnvOfInstance(i.ID, i.Tags), Tags: []string{}}
	v.EnvLabel, v.EnvProd = instext.EnvLabel(v.Env), instext.EnvIsProd(v.Env)
	for _, t := range i.Tags {
		if t.Active {
			v.Tags = append(v.Tags, t.TagName)
		}
	}
	return v
}

// queryDBTypes 是在线查询已支持的实例类型：MySQL 协议系，SQL Server（语句检查 / 库表解析 / TOP 改写见 mssql.go，
// 引擎只读查询见 engine/mssql），Redis（用命令代替 SQL，见 redis.go），以及 PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦（见 multi.go、
// pgsql.go、mongo.go、clickhouse.go、oracle.go、dm.go）。其余类型即使引擎已注册也不能进入查询流水线。
var queryDBTypes = map[string]bool{"mysql": true, "polardb": true, "polardb-x": true, "polardbx": true, "selectdb": true, "doris": true, "adb": true,
	"mssql": true, "redis": true, "pgsql": true, "mongo": true, "clickhouse": true, "oracle": true, "dm": true}

// SupportsQuery 判断某类型的实例能否用在线查询。
func SupportsQuery(dbType string) bool { return queryDBTypes[dbType] }

func (s *Service) Instances(p Principal) ([]InstanceView, error) {
	list, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	kept := list[:0:0]
	for _, in := range list {
		if SupportsQuery(in.DBType) {
			kept = append(kept, in)
		}
	}
	list = kept
	list, err = s.auth.FilterInstances(p, list)
	if err != nil {
		return nil, err
	}
	out := make([]InstanceView, 0, len(list))
	for i := range list {
		out = append(out, toView(list[i]))
	}
	return out, nil
}

// CanAccessPage 判断用户能否打开在线查询页面（query.view 权限）。
func (s *Service) CanAccessPage(p Principal) bool { return s.auth.CanAccessPage(p) }

func (s *Service) instance(p Principal, id uint) (*model.Instance, error) {
	inst, err := s.repo.GetInstance(id)
	if err != nil || inst == nil {
		return nil, newErr(404, "not_found", "实例不存在")
	}
	if !SupportsQuery(inst.DBType) {
		return nil, newErr(501, "unsupported_engine", "该类型的实例暂不支持在线查询")
	}
	if err := s.auth.CanUseInstance(p, inst); err != nil {
		return nil, err
	}
	return inst, nil
}

func (s *Service) withEngine(ctx context.Context, p Principal, id uint, fn func(engine.Engine, *model.Instance) error) error {
	inst, err := s.instance(p, id)
	if err != nil {
		return err
	}
	e, err := s.open(inst.DBType, s.repo.EngineConn(inst))
	if err != nil {
		return newErr(501, "unsupported_engine", err.Error())
	}
	defer e.Close()
	if err := fn(e, inst); err != nil {
		var be *Error
		if errors.As(err, &be) {
			return err
		}
		return newErr(statusTargetError, "engine_error", err.Error())
	}
	return nil
}

func (s *Service) Databases(ctx context.Context, p Principal, id uint) (out []string, err error) {
	err = s.withEngine(ctx, p, id, func(e engine.Engine, inst *model.Instance) error {
		if isRedis(inst.DBType) { // Redis 的“库”是 db 编号 "0".."N-1"
			out, err = s.redisDBNames(ctx, e, inst)
			return err
		}
		all, err := e.AllDatabases(ctx)
		if err != nil {
			return err
		}
		out, err = filterDBs(all, inst.ShowDBNameRegex, inst.DeniedDBNameRegex)
		return err
	})
	return
}

func (s *Service) Tables(ctx context.Context, p Principal, id uint, db string) (out []string, err error) {
	err = s.withEngine(ctx, p, id, func(e engine.Engine, inst *model.Instance) error {
		out, err = e.AllTables(ctx, db)
		return err
	})
	return
}

func (s *Service) Columns(ctx context.Context, p Principal, id uint, db, table string) (out []string, err error) {
	err = s.withEngine(ctx, p, id, func(e engine.Engine, inst *model.Instance) error {
		out, err = e.AllColumns(ctx, db, table)
		return err
	})
	return
}

// ---- 执行 ----

type Request struct {
	InstanceID uint   `json:"instance_id"`
	DB         string `json:"db"`
	SQL        string `json:"sql"`
	Limit      int    `json:"limit"`
	// Reason 是用户为 SQL 防火墙的“要求填写原因”规则填写的原因；没有这类规则命中时忽略。
	Reason string `json:"reason,omitempty"`
}

type Result struct {
	Columns        []string  `json:"columns"`
	Rows           [][]any   `json:"rows"`
	RowCount       int       `json:"row_count"`
	EffectiveLimit int       `json:"effective_limit"`
	LimitSource    string    `json:"limit_source"`
	Truncated      bool      `json:"truncated"`
	CostMs         int64     `json:"cost_ms"`
	MaskedColumns  []string  `json:"masked_columns"`
	MaskMode       string    `json:"mask_mode,omitempty"` // syntax-tree（goInception 语法树）或 column-name（按列名，较弱）
	ExecutedSQL    string    `json:"executed_sql"`
	Notice         string    `json:"notice,omitempty"`
	QueryLogID     uint      `json:"query_log_id,omitempty"`
	Kind           string    `json:"kind,omitempty"`      // select / show / explain：语句归类（WITH 属 select，DESC 属 show 或 explain）
	PlanTips       []PlanTip `json:"plan_tips,omitempty"` // 仅执行计划：风险提示（全表扫描、函数使索引失效、filesort 等）
	Cursor         string    `json:"cursor,omitempty"`    // 仅 Redis 的 SCAN 系列：下一页游标（"0" 表示已扫完）
	NextCmd        string    `json:"next_cmd,omitempty"`  // 仅 Redis 的 SCAN 系列：下一页的完整命令（游标已换成新的）

	// 以下仅 PostgreSQL / MongoDB / ClickHouse（Kind 为 pgsql / mongo / clickhouse）返回。
	StmtKind    string   `json:"stmt_kind,omitempty"`    // 引擎判定的语句种类（select / explain / find / aggregate …）
	ColumnTypes []string `json:"column_types,omitempty"` // 与 Columns 等长的类型名（MongoDB 没有）
	Warnings    []string `json:"warnings,omitempty"`     // 引擎的提示：自动补 LIMIT、截断、二进制 / 超长单元格 …
	Notices     []string `json:"notices,omitempty"`      // 静态改写提示（ClickHouse 的 FORMAT 被忽略等）
	TruncatedBy string   `json:"truncated_by,omitempty"` // rows / bytes / response
	Unmasked    bool     `json:"unmasked,omitempty"`     // 系统脱敏总开关关闭，PostgreSQL / MongoDB / ClickHouse 的结果不做脱敏

	// Firewall 是 SQL 防火墙对本次查询的提示 / 收紧结果；没有就不返回。内容同时折进了 Notice。
	Firewall *FirewallInfo `json:"firewall,omitempty"`
}

// allowedStmt：select / with（CTE，主语句必须是 select）/ show / explain / desc / describe。
var allowedStmt = map[string]bool{"select": true, "with": true, "show": true, "explain": true, "desc": true, "describe": true}

// Check 完成 MySQL 系语句的语句级检查，返回要执行的单条语句及提示。不访问数据库。
func (s *Service) Check(sql, db string, pol Policy) (stmt, notice string, err error) {
	return s.checkMySQL(sql, db, pol)
}

// checkFor 按实例类型选择语句检查：SQL Server 走 T-SQL 版本，其余走 MySQL 版本。
func (s *Service) checkFor(dbType, sql, db string, pol Policy) (stmt, notice string, err error) {
	if isMSSQL(dbType) {
		return s.checkMSSQL(sql, pol)
	}
	if isRedis(dbType) {
		return s.checkRedis(sql)
	}
	return s.checkMySQL(sql, db, pol)
}

func (s *Service) checkMySQL(sql, db string, pol Policy) (stmt, notice string, err error) {
	return s.checkMySQLStmts(sqlx.Split(sql), db, pol)
}

// checkMySQLStmts 检查已切分好的语句：只执行第 1 条，它必须是纯查询。
func (s *Service) checkMySQLStmts(stmts []string, db string, pol Policy) (stmt, notice string, err error) {
	if len(stmts) == 0 {
		return "", "", newErr(400, "bad_query", "没有有效的SQL语句")
	}
	stmt = stmts[0]
	if len(stmts) > 1 {
		notice = fmt.Sprintf("检测到 %d 条语句，本次只执行第 1 条；可选中某条语句后再运行。", len(stmts))
	}
	kw := sqlx.FirstKeyword(stmt)
	if !allowedStmt[kw] {
		return "", "", newErr(400, "bad_query", "不支持的查询语法类型！在线查询仅支持 SELECT / WITH / SHOW / DESC / EXPLAIN；数据变更请提交 SQL 工单。")
	}
	if kw == "with" {
		// CTE 之后的主语句也可以是 UPDATE / DELETE（MySQL 8），必须是纯查询。
		if sqlx.WithMainKeyword(stmt) != "select" || sqlx.HasWriteVerb(stmt) {
			return "", "", newErr(400, "bad_query", "WITH 语句的主语句必须是 SELECT，且不能含有写操作，只允许纯查询；数据变更请提交 SQL 工单。")
		}
	}
	if sqlx.ExplainAnalyze(stmt) {
		return "", "", newErr(400, "bad_query", "EXPLAIN ANALYZE 会真正执行语句，已禁止；请使用普通 EXPLAIN。")
	}
	if sqlx.TouchesMySQLSchema(stmt, db) {
		return "", "", newErr(400, "bad_query", "不允许查询 mysql 系统库（如 mysql.user）")
	}
	if sqlx.ContainsSequence(stmt, "for", "update") || sqlx.ContainsSequence(stmt, "lock", "in", "share", "mode") ||
		sqlx.ContainsSequence(stmt, "into", "outfile") || sqlx.ContainsSequence(stmt, "into", "dumpfile") {
		return "", "", newErr(400, "bad_query", "在线查询不允许加锁读取或写出文件")
	}
	if pol.DisableStar && sqlx.QueryKind(stmt) == "select" && sqlx.HasSelectStar(stmt) {
		return "", "", &Error{Code: "star_forbidden", HTTP: 400,
			Message: "系统已禁用 SELECT *，请列出需要的字段。在左侧点击表名可自动展开字段。"}
	}
	return stmt, notice, nil
}

// execTrace 记录一次执行走到哪一步，失败时用来写“被拦截 / 失败 / 超时 / 已取消”记录（见 events.go）。
type execTrace struct {
	inst *model.Instance
	cost time.Duration
	dry  bool // 只看不执行（估算行数）：不写防火墙命中记录，也不要求填写原因
}

// prepared 是通过语句检查与权限校验、可以交给引擎执行的查询。
type prepared struct {
	inst *model.Instance
	pol  Policy
	stmt string      // 检查后的单条语句
	run  string      // 实际下发的语句（已按行数上限改写）
	pre  preludeInfo // 仅 MySQL 系：SET 打头的脚本里，最后一条查询之前的语句（见 prelude.go）
	// extraForce 是脚本中间的查询被防火墙要求强制脱敏的列（变量污染分析要一并考虑）。
	extraForce []masking.Column
	kind       string // select / show / explain
	notice     string
	limit      int // 本次生效的行数上限
	dec        Decision
	redis      *redisPrep // 仅 Redis 实例：已通过检查的命令
	multi      *multiPrep // 仅 PostgreSQL / MongoDB / ClickHouse 实例：已通过检查与授权的语句

	fw        *firewall.Decision // SQL 防火墙的决定（防火墙未启用 / 该引擎不进防火墙时为 nil）
	fwLimited bool               // limit 是防火墙收紧的
}

// prepare 完成实例可用性、语句检查、库表权限与行数上限改写（Execute 与 EstimateRows 共用）。
func (s *Service) prepare(ctx context.Context, p Principal, req Request, tr *execTrace) (*prepared, error) {
	if req.InstanceID == 0 || strings.TrimSpace(req.SQL) == "" {
		return nil, newErr(400, "bad_request", "请选择实例并输入 SQL")
	}
	inst, err := s.instance(p, req.InstanceID)
	if err != nil {
		return nil, err
	}
	tr.inst = inst
	pol := s.Policy()
	if isRedis(inst.DBType) {
		return s.prepareRedis(ctx, p, req, inst, pol)
	}
	if isMulti(inst.DBType) {
		return s.prepareMulti(ctx, p, req, inst, pol, tr)
	}
	var (
		pre          preludeInfo
		stmt, notice string
	)
	if mysqlFamily(inst.DBType) {
		pre, stmt, notice, err = s.checkMySQLScript(req.SQL, req.DB, pol)
	} else {
		stmt, notice, err = s.checkFor(inst.DBType, req.SQL, req.DB, pol)
	}
	if err != nil {
		return nil, err
	}
	// SQL 防火墙：在库表权限校验之前，被拒绝的语句不连目标库、也不暴露权限信息
	// 前置 SET 里的子查询按独立查询对待：它们涉及的表同样参与防火墙的对象条件，扫描行数的估算也带上前置语句（变量要有值）。
	tables := s.fwTables(ctx, inst, req.DB, stmt)
	if len(pre.probes) > 0 {
		mainTables := tables
		tables = func() ([]sqlx.TableRef, error) {
			refs, err := mainTables()
			if err != nil {
				return nil, err
			}
			for _, pb := range pre.probes {
				more, err := s.fwTables(ctx, inst, req.DB, pb)()
				if err != nil {
					return nil, err
				}
				refs = append(refs, more...)
			}
			return refs, nil
		}
	}
	fd, err := s.firewallGate(ctx, p, inst, req, stmt, firewall.FullCaps, tr, tables, s.fwScanFunc(ctx, p, inst, req.DB, stmt, pol, pre.setStmts()))
	if err != nil {
		return nil, err
	}
	dec, err := s.auth.AuthorizeQuery(ctx, p, inst, req.DB, stmt, pol.AdminLimit)
	if err != nil {
		return nil, err
	}
	for _, pb := range pre.probes { // 前置 SET 里的子查询：库表权限与主查询同一套规则
		if _, err := s.auth.AuthorizeQuery(ctx, p, inst, req.DB, pb, pol.AdminLimit); err != nil {
			return nil, err
		}
	}
	// 脚本中间的查询：结果丢弃，但每一条都要像单独运行时一样通过防火墙与库表权限，并按行数上限改写（防止拿它们空扫大表）。
	var extraForce []masking.Column
	for _, qi := range pre.queryIdx {
		q := pre.stmts[qi]
		qreq := req
		qreq.SQL = q
		qfd, err := s.firewallGate(ctx, p, inst, qreq, q, firewall.FullCaps, tr, s.fwTables(ctx, inst, req.DB, q), s.fwScanFunc(ctx, p, inst, req.DB, q, pol, pre.setsBefore(qi)))
		if err != nil {
			return nil, err
		}
		qdec, err := s.auth.AuthorizeQuery(ctx, p, inst, req.DB, q, pol.AdminLimit)
		if err != nil {
			return nil, err
		}
		ql := qdec.MaxRows
		if req.Limit > 0 && (ql <= 0 || req.Limit < ql) {
			ql = req.Limit
		}
		ql, _, _ = applyFirewallLimit(qfd, ql, qdec)
		if sqlx.QueryKind(q) == "select" {
			pre.stmts[qi] = sqlx.RewriteLimit(q, ql)
		}
		extraForce = append(extraForce, forcedColumns(qfd)...)
	}
	limit := dec.MaxRows
	if req.Limit > 0 && (limit <= 0 || req.Limit < limit) {
		limit = req.Limit
	}
	limit, dec, fwLimited := applyFirewallLimit(fd, limit, dec)
	kind := "select"
	if !isMSSQL(inst.DBType) {
		kind = sqlx.QueryKind(stmt)
	}
	run := stmt
	if kind == "select" {
		if isMSSQL(inst.DBType) {
			run = mssqlRewriteTop(stmt, limit)
		} else {
			run = sqlx.RewriteLimit(stmt, limit)
		}
	}
	return &prepared{inst: inst, pol: pol, stmt: stmt, run: run, pre: pre, extraForce: extraForce, kind: kind, notice: notice, limit: limit, dec: dec, fw: fd, fwLimited: fwLimited}, nil
}

// capNotice 说明结果因字节上限被裁剪的情况（没有裁剪返回空串）。
func capNotice(rs *engine.ResultSet, pol Policy) string {
	var parts []string
	if rs.CellsCut > 0 {
		parts = append(parts, fmt.Sprintf("有 %d 个单元格超过 %s，已截断显示（上限见系统设置 query.max_cell_bytes）。", rs.CellsCut, byteText(pol.MaxCellBytes)))
	}
	if rs.ByteLimited {
		parts = append(parts, fmt.Sprintf("结果总量超过 %s，只返回了前 %d 行（上限见系统设置 query.max_result_bytes）。", byteText(pol.MaxResultBytes), len(rs.Rows)))
	}
	return strings.Join(parts, "")
}

func byteText(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.4g MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.4g KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d 字节", n)
}

func (s *Service) Execute(ctx context.Context, p Principal, req Request) (*Result, error) {
	tr := &execTrace{}
	res, err := s.execute(ctx, p, req, tr)
	if err != nil {
		s.recordFailure(p, req, tr, err)
	}
	return res, err
}

func (s *Service) execute(ctx context.Context, p Principal, req Request, tr *execTrace) (*Result, error) {
	pr, err := s.prepare(ctx, p, req, tr)
	if err != nil {
		return nil, err
	}
	if pr.redis != nil {
		return s.runRedis(ctx, p, req, pr, tr)
	}
	if pr.multi != nil {
		return s.runMulti(ctx, p, req, pr, tr)
	}
	inst, pol, run, kind, limit, dec, notice := pr.inst, pr.pol, pr.run, pr.kind, pr.limit, pr.dec, pr.notice
	engLimit := limit
	if kind == "explain" {
		engLimit = 0 // EXPLAIN 不限制行数
	}

	e, err := s.open(inst.DBType, s.repo.EngineConn(inst))
	if err != nil {
		return nil, newErr(501, "unsupported_engine", err.Error())
	}
	defer e.Close()

	timeout := time.Duration(pol.MaxExecSeconds) * time.Second
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	rs, err := e.Query(qctx, run, engine.QueryOptions{DBName: req.DB, Limit: engLimit, MaxExecMs: timeout.Milliseconds(),
		MaxCellBytes: pol.MaxCellBytes, MaxBytes: pol.MaxResultBytes, Prelude: pr.pre.stmts})
	cost := time.Since(start)
	tr.cost = cost
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, newErr(499, "canceled", "查询已取消")
		}
		if errors.Is(qctx.Err(), context.DeadlineExceeded) {
			return nil, newErr(504, "timeout", fmt.Sprintf("查询超过 %d 秒已终止，请缩小范围或增加过滤条件。", pol.MaxExecSeconds))
		}
		if errors.Is(err, engine.ErrReadOnlyViolation) {
			// 语句通过了上面的静态检查，却被数据库的只读会话挡下：检查有漏洞或有人在试探，留痕为“被拦截”。
			slog.Warn("在线查询的语句被数据库只读会话拒绝", "user", p.Username, "instance", inst.InstanceName, "db", req.DB)
			return nil, newErr(403, "readonly_violation", "该语句会修改数据，已被数据库的只读会话拒绝。在线查询只允许查询；数据变更请提交 SQL 工单。")
		}
		return nil, newErr(statusTargetError, "engine_error", err.Error())
	}

	// 数据脱敏：仅对 SELECT 的成功结果执行（先于写日志）。
	var mo maskOutcome
	kw := kind
	force := append(forcedColumns(pr.fw), pr.extraForce...) // SQL 防火墙要求强制脱敏的列：即使系统脱敏总开关没开也执行
	if (pol.DataMasking || len(force) > 0) && kw == "select" {
		var merr error
		mctx := withPrelude(withForcedMask(ctx, force), pr.pre.text())
		if !pol.DataMasking {
			mctx = withOnlyForced(mctx) // 总开关没开：只做防火墙要求的强制脱敏，已配置的规则仍不生效
		}
		mo, merr = s.maskResult(mctx, inst, req.DB, run, rs, e)
		if merr != nil {
			slog.Error("数据脱敏失败", "err", merr, "user", p.Username, "instance", inst.InstanceName)
			var be *Error
			// 严格模式下的业务错误、开启 query.block_on_mask_error 时的任何脱敏错误、以及有防火墙强制脱敏时的任何脱敏错误，都直接阻断，不返回数据。
			if errors.As(merr, &be) || pol.QueryCheck || len(force) > 0 {
				return nil, newErr(500, "masking_error", "数据脱敏异常，结果未返回，请联系管理员。")
			}
			// query.block_on_mask_error 关闭：放行未（完整）脱敏的数据，并留下告警。
			var ve *viewDefError
			if !errors.As(merr, &ve) { // 视图定义没读全时 mo 里是已经做完的部分脱敏与说明（其余的列已按配置脱敏）；其它错误没有做任何脱敏
				mo = maskOutcome{Notice: "数据脱敏出错，本次结果未脱敏（系统设置 query.block_on_mask_error 为关闭状态）。"}
			}
		}
	}

	fwi := fwInfo(pr.fw, pr.fwLimited)
	res := &Result{
		Columns: rs.ColumnList, Rows: rs.Rows, RowCount: len(rs.Rows),
		EffectiveLimit: limit, LimitSource: dec.Source,
		Truncated: kw == "select" && (limit > 0 && len(rs.Rows) >= limit || rs.ByteLimited),
		CostMs:    cost.Milliseconds(), MaskedColumns: []string{}, ExecutedSQL: pr.pre.withQuery(run),
		Notice: joinNotice(notice, mo.Notice, fwNotice(fwi), capNotice(rs, pol)), MaskMode: mo.Mode, Kind: kind, Firewall: fwi,
	}
	if kind == "explain" {
		res.PlanTips = analyzePlan(pr.stmt, rs.ColumnList, rs.Rows)
	}
	if mo.Columns != nil {
		res.MaskedColumns = mo.Columns
	}
	if res.Rows == nil {
		res.Rows = [][]any{}
	}

	log := &model.QueryLog{
		InstanceName: inst.InstanceName, DBName: req.DB, SQLLog: pr.pre.withQuery(run), EffectRow: int64(len(rs.Rows)),
		CostTime: strconv.FormatFloat(cost.Seconds(), 'f', 3, 64), Username: p.Username, UserDisplay: p.Display,
		PrivCheck: dec.PrivCheck, HitRule: mo.Hit, Masking: mo.Ran,
	}
	// 审计日志写不进去就不返回结果：宁可失败，也不留下没有审计记录的查询。
	if err := s.repo.InsertQueryLog(log); err != nil {
		slog.Error("写入 dbt_query_log 失败，已拒绝返回查询结果", "err", err, "user", p.Username, "instance", inst.InstanceName)
		return nil, newErr(500, "audit_failed", "查询已执行，但审计日志写入失败，结果未返回，请联系管理员。")
	}
	res.QueryLogID = log.ID
	if s.fw != nil {
		s.fw.AttachQueryLog(pr.fw, log.ID) // 把命中记录与成功执行的查询日志关联起来
	}
	return res, nil
}
