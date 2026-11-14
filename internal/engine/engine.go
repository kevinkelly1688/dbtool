// @author: kevinkelly
// @license: Apache Licence
// @file: engine.go
// @time: 2026/10/28

// Package engine 定义各数据库引擎的统一接口。
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrReadOnlyViolation 表示数据库的只读会话拒绝了一条会修改数据的语句。引擎用 %w 包装它：
// 语句通过了静态检查却被数据库挡下，说明检查有漏洞或有人在试探，在线查询据此把它记为“被拦截”而不是普通失败。
var ErrReadOnlyViolation = errors.New("只读会话拒绝了会修改数据的语句")

// ErrViewDefsUnsupported 表示目标库没有可查询的视图定义目录（目录表不存在、不兼容等）。
// 数据脱敏遇到它就不展开视图（照常按配置脱敏），不当作失败；权限不足、超时等其它错误才是失败。
var ErrViewDefsUnsupported = errors.New("目标库没有可查询的视图定义目录") // i18n-ignore 只进日志

// ViewRef 是要查的对象名。Catalog 只有 SQL Server 用（三段名里的库名）；
// Schema 为空表示当前库（MySQL 系）/ 搜索路径（PostgreSQL）/ 任意架构（SQL Server）。
type ViewRef struct{ Catalog, Schema, Name string }

// ViewDef 是一个视图的定义。Definition 是库里存的文本（SQL Server 带 CREATE VIEW … AS 头，其余只有 SELECT）；
// 为空表示视图存在但定义读不到（没有权限、已加密）。Catalog 只有视图不在当前库时才有（SQL Server），用来解析它定义里没写库名的名字。
type ViewDef struct {
	Catalog, Schema, Name string
	Definition            string
	// ColumnNames 是目录里视图的实际列名（按列序）；只有 Oracle / 达梦提供：它们的 TEXT 里没有 CREATE VIEW v (列, …) 的列清单，
	// 视图体里的别名和视图的真实列名可能不同，脱敏要靠这份名单按位置对上。
	ColumnNames []string
}

// ViewDefiner 由能读视图定义的引擎实现；数据脱敏据此顺着视图追到基表字段（见 internal/query/viewexp.go）。
// refs 里不是视图的对象不返回；系统库 / 系统架构里的视图不返回。db 是当前库（没写库名的名字按它解析）。
// 目录本身不可用时返回 ErrViewDefsUnsupported，其它错误（权限、超时、连接）原样返回。
type ViewDefiner interface {
	ViewDefs(ctx context.Context, db string, refs []ViewRef) ([]ViewDef, error)
}

// MaxViewDefBytes 是单个视图定义允许的最大字节数，超过的当作读不到（防止异常大的定义拖垮分析）。
const MaxViewDefBytes = 1 << 20

// Conn 是创建引擎所需的连接信息（密码已解密，隧道已建立后的最终地址）。
type Conn struct {
	Host      string
	Port      int
	User      string
	Password  string
	DBName    string
	Charset   string
	SSL       bool
	VerifySSL bool
	// ServiceName / SID 只有 Oracle 用：连接的服务名（优先）或实例 SID。
	ServiceName string
	SID         string
}

// ResultSet 是查询/元数据结果。
type ResultSet struct {
	Rows         [][]any  `json:"rows"`
	ColumnList   []string `json:"column_list"`
	AffectedRows int64    `json:"affected_rows"`
	CostTimeMs   int64    `json:"cost_time_ms"`
	Error        string   `json:"error,omitempty"`
	// CellsCut 是因超过 QueryOptions.MaxCellBytes 被截断的单元格个数；ByteLimited 表示总量超过 MaxBytes，后面的行没有读。
	CellsCut    int  `json:"cells_cut,omitempty"`
	ByteLimited bool `json:"byte_limited,omitempty"`
}

// QueryOptions 查询选项。
type QueryOptions struct {
	DBName    string
	Limit     int // 0 表示不限制，由上层按权限/系统配置决定
	MaxExecMs int64
	// MaxCellBytes / MaxBytes 大于 0 时限制单个单元格、整个结果集的字节数（只有 SQL Server 引擎遵守；数据导出不设）。
	MaxCellBytes int
	MaxBytes     int
	// Prelude 是要在同一个会话里、先于查询执行的前置语句（只有 MySQL 系引擎遵守，用来给用户变量赋值等）。
	// 调用方负责保证它们是只读的、已经过检查；它们和查询一起在只读会话里执行，任何一条失败整个查询失败。
	Prelude []string
}

// ReviewResult 是单条 SQL 审核结果。
type ReviewResult struct {
	ID           int    `json:"id"`
	Stage        string `json:"stage"`
	ErrLevel     int    `json:"errlevel"` // 0 通过 1 警告 2 错误
	StageStatus  string `json:"stagestatus"`
	ErrorMessage string `json:"errormessage"`
	SQL          string `json:"sql"`
	AffectedRows int64  `json:"affected_rows"`
	Sequence     string `json:"sequence"`
	BackupDBName string `json:"backup_dbname"`
	ExecuteTime  string `json:"execute_time"`
	SQLSha1      string `json:"sqlsha1"`
}

// ReviewSet 是一批 SQL 的审核结果集合。
type ReviewSet struct {
	FullSQL      string         `json:"full_sql"`
	Rows         []ReviewResult `json:"rows"`
	WarningCount int            `json:"warning_count"`
	ErrorCount   int            `json:"error_count"`
	Error        string         `json:"error,omitempty"`
}

// Engine 统一引擎接口。
type Engine interface {
	// Name 返回引擎类型，对应 sql_instance.db_type（mysql、pgsql ...）。
	Name() string
	TestConnection(ctx context.Context) error
	ServerVersion(ctx context.Context) (string, error)
	AllDatabases(ctx context.Context) ([]string, error)
	AllTables(ctx context.Context, db string) ([]string, error)
	AllColumns(ctx context.Context, db, table string) ([]string, error)
	Query(ctx context.Context, sql string, opt QueryOptions) (*ResultSet, error)
	Close() error
}

// Factory 创建某类型的引擎。
type Factory func(Conn) (Engine, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register 注册引擎；各引擎包在 init() 中调用。
func Register(dbType string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	factories[dbType] = f
}

// New 按 db_type 创建引擎。
func New(dbType string, c Conn) (Engine, error) {
	mu.RLock()
	f, ok := factories[dbType]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("暂不支持的数据库类型: %s", dbType)
	}
	return f(c)
}

// Supported 返回已注册的引擎类型。
func Supported() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	return out
}

// ExecStmtResult 是原生执行时一条语句的结果。
type ExecStmtResult struct {
	SQL       string
	Affected  int64
	ElapsedMs float64
	Err       string // 非空表示该语句执行失败
	Skipped   bool   // 前面的语句失败，这条没有执行
	Note      string // 给用户看的附加说明（如 SQL Server 的服务端消息、核对查询返回的行数）；执行成功时也可能有
}

// Writer 是能够执行变更语句（DDL/DML）的引擎，用于不经过 goInception 的“原生执行”。
type Writer interface {
	// ReadOnly 判断实例是否处于只读状态（read_only / super_read_only）。
	ReadOnly(ctx context.Context) (bool, error)
	// ExecEach 在指定库里按顺序逐条执行，遇到第一条失败即停止，其后的语句标记为 Skipped。
	// 返回的切片与 stmts 等长。
	ExecEach(ctx context.Context, db string, stmts []string) ([]ExecStmtResult, error)
}

// SyntaxChecker 是能让服务端做语法检查（不执行）的引擎，如 SQL Server 的 SET PARSEONLY ON。
type SyntaxChecker interface {
	// SyntaxCheck 返回与 units 等长的错误信息，空串表示该单元语法通过。
	SyntaxCheck(ctx context.Context, db string, units []string) ([]string, error)
}
