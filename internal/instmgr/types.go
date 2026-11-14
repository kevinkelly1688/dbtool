// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/i18n"
)

// ---- 引擎类型 ----

// mysqlLike 是走 MySQL 协议的类型。
var mysqlLike = map[string]bool{"mysql": true, "polardb": true, "polardb-x": true, "selectdb": true, "doris": true, "adb": true}

// 各功能支持的类型：只对其中一部分类型实现，其余明确提示“暂不支持”。
func supportsSession(t string) bool     { return mysqlLike[t] } // 进程列表 / 表空间 / 终止会话
func supportsTrx(t string) bool         { return t == "mysql" || t == "polardb" || t == "polardb-x" }
func supportsAccount(t string) bool     { return t == "mysql" || t == "polardb" || t == "polardb-x" }
func supportsDatabase(t string) bool    { return mysqlLike[t] }
func supportsParam(t string) bool       { return mysqlLike[t] }
func supportsBinlogPurge(t string) bool { return t == "mysql" }

// queryOnly 是只接入“在线查询”的类型（PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦）：实例管理里的会话、账号、参数、
// 数据库管理、Binlog 等功能对它们一律“暂不支持”（上面的 supportsXxx 都是显式白名单，不含这些类型）。
func queryOnly(t string) bool {
	return t == "pgsql" || t == "mongo" || t == "clickhouse" || t == "oracle" || t == "dm"
}

// defaultPorts 是这几类实例新建时的缺省端口（ClickHouse 为 HTTP 接口端口，TLS 常用 8443，需手填）。
var defaultPorts = map[string]int{"pgsql": 5432, "mongo": 27017, "clickhouse": 8123, "oracle": 1521, "dm": 5236}

// featuresOf 返回实例列表里“支持”一栏：只对 queryOnly 的类型返回。
func featuresOf(t string) (codes []string, label string) {
	if !queryOnly(t) {
		return nil, ""
	}
	return []string{"query"}, "在线查询"
}

// dbTypeLabels 是实例表单里可选的数据库类型（goInception 是内部引擎，不作为业务实例）。
var dbTypeLabels = []Option{
	{"mysql", "MySQL"}, {"mssql", "SQL Server"}, {"redis", "Redis"}, {"pgsql", "PgSQL"}, {"oracle", "Oracle"}, {"dm", "达梦 DM"},
	{"mongo", "Mongo"}, {"polardb", "PolarDB"}, {"polardb-x", "PolarDB-X"}, {"clickhouse", "ClickHouse"},
	{"cassandra", "Cassandra"}, {"selectdb", "SelectDB"}, {"doris", "Doris"}, {"adb", "ADB"},
	{"elasticsearch", "Elasticsearch"}, {"opensearch", "OpenSearch"}, {"memcached", "Memcached"}, {"tdengine", "TDengine"},
}

// Option 是下拉选项。
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func validDBType(t string) bool {
	for _, o := range dbTypeLabels {
		if o.Value == t {
			return true
		}
	}
	return false
}

func dbTypeLabel(t string) string {
	for _, o := range dbTypeLabels {
		if o.Value == t {
			return o.Label
		}
	}
	return t
}

func unsupportedMsg(dbType, feature string) string {
	return fmt.Sprintf("暂不支持 %s 类型实例的%s", dbTypeLabel(dbType), feature)
}

// ---- 调用者 ----

// Caller 是当前登录用户及其可见范围。AllInstances 为 true（超级用户或 query.admin_run）时可见全部实例，
// 否则只有通过资源组关联的实例。
type Caller struct {
	ID           uint
	Username     string
	Display      string
	IsSuperuser  bool
	AllInstances bool
	// Lang 是本次请求的界面语言（空 = 中文）：只用来生成落进成功响应的提示文字；审计与错误信息仍是中文，错误在 HTTP 边缘统一翻译。
	Lang string
}

// tr 按调用者的界面语言翻译一条中文原文（带 {名字} 占位时成对传参）；Lang 为空时输出中文。
func (c Caller) tr(key string, kv ...any) string { return i18n.T(c.Lang, key, kv...) }

// trp 翻译运行时生成的中文文案（变量），精确匹配目录，没有就原样返回。
func (c Caller) trp(s string) string { return i18n.TP(c.Lang, s) }

// ---- 错误 ----

// Error 是对外可见的业务错误，Status 映射为 HTTP 状态码。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}
func bad(msg string) *Error         { return newErr(400, "bad_request", msg) }
func forbidden(msg string) *Error   { return newErr(403, "forbidden", msg) }
func notFound(msg string) *Error    { return newErr(404, "not_found", msg) }
func conflict(msg string) *Error    { return newErr(409, "conflict", msg) }
func unsupported(msg string) *Error { return newErr(400, "unsupported", msg) }
func targetErr(err error) *Error {
	return newErr(502, "target_error", fmt.Sprintf("目标库返回错误：%s", err.Error()))
}

// ---- 目标库访问 ----

// TunnelInfo 是 SSH 隧道的连接信息（已解密）。
type TunnelInfo struct {
	Host         string
	Port         int
	User         string
	Password     string
	Pkey         string
	PkeyPassword string
}

// ConnInfo 是连接目标实例所需的全部信息（密码已解密）。
type ConnInfo struct {
	DBType    string
	Host      string
	Port      int
	User      string
	Password  string
	DBName    string
	Charset   string
	SSL       bool
	VerifySSL bool
	// ServiceName / SID 只有 Oracle 用（连接的服务名优先，其次 SID）。
	ServiceName string
	SID         string
	Tunnel      *TunnelInfo
}

// Target 是一个已连接的目标库。同一个 Target 的所有调用都在同一条连接上（CONNECTION_ID() 前后一致），
// 不能并发使用；用完必须 Close。
type Target interface {
	// Query 执行查询。args 里的 ? 占位符由驱动在客户端安全转义（interpolateParams）。
	// 返回结果的列名统一为小写。
	Query(ctx context.Context, sql string, args ...any) (*Result, error)
	// Exec 执行语句，返回影响行数。
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
	Close() error
}

// Dialer 连接目标实例。所有对目标库的访问都经过它，单测用假实现。
type Dialer interface {
	// Dial 连接 MySQL 协议系实例。
	Dial(ctx context.Context, c ConnInfo) (Target, error)
	// Probe 测试其他类型实例的连通性并返回版本号（走 internal/engine 里已注册的引擎）。
	Probe(ctx context.Context, c ConnInfo) (version string, err error)
}

// Result 是查询结果：列名小写，值已规整为 nil / string / int64 / uint64 / float64。
type Result struct {
	Columns []string
	Rows    [][]any
}

// Record 是按列名索引的一行（列名小写）。
type Record map[string]any

// Records 把结果转成按列名索引的行。
func (r *Result) Records() []Record {
	if r == nil {
		return nil
	}
	out := make([]Record, 0, len(r.Rows))
	for _, row := range r.Rows {
		rec := make(Record, len(r.Columns))
		for i, c := range r.Columns {
			if i < len(row) {
				rec[c] = row[i]
			}
		}
		out = append(out, rec)
	}
	return out
}

// Str 返回列的字符串形式，NULL 为空串。
func (r Record) Str(k string) string {
	switch v := r[k].(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	case time.Time:
		return v.Format("2006-01-02 15:04:05")
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// Int 返回列的整数值，NULL 或无法解析为 0。
func (r Record) Int(k string) int64 {
	switch v := r[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case uint64:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	case []byte:
		n, _ := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
		return n
	}
	return 0
}

// Float 返回列的浮点数值，NULL 或无法解析为 0。
func (r Record) Float(k string) float64 {
	switch v := r[k].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f
	case []byte:
		f, _ := strconv.ParseFloat(strings.TrimSpace(string(v)), 64)
		return f
	}
	return 0
}

// IsNull 判断列是否为 NULL（或不存在）。
func (r Record) IsNull(k string) bool { return r[k] == nil }

// ---- 版本 ----

// Version 是目标库版本。
type Version struct {
	Major, Minor, Patch int
	MariaDB             bool
	Raw                 string
}

// parseVersion 解析 SELECT VERSION() 的结果，如 "8.0.36"、"5.7.44-log"、"10.6.12-MariaDB-1:10.6.12+maria~ubu2004"。
func parseVersion(raw string) Version {
	v := Version{Raw: raw, MariaDB: strings.Contains(strings.ToLower(raw), "mariadb")}
	nums := [3]int{}
	rest := raw
	for i := 0; i < 3; i++ {
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j == 0 {
			break
		}
		nums[i], _ = strconv.Atoi(rest[:j])
		rest = rest[j:]
		if len(rest) > 0 && rest[0] == '.' {
			rest = rest[1:]
		} else {
			break
		}
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v
}

// atLeast 判断版本不低于 major.minor.patch。
func (v Version) atLeast(major, minor, patch int) bool {
	if v.Major != major {
		return v.Major > major
	}
	if v.Minor != minor {
		return v.Minor > minor
	}
	return v.Patch >= patch
}

// supportsAccountLock：MySQL 5.7.6+、MariaDB 10.4.2+。
func (v Version) supportsAccountLock() bool {
	if v.MariaDB {
		return v.atLeast(10, 4, 2)
	}
	return v.atLeast(5, 7, 6)
}

// newLocks：MySQL 8.0.1+ 用 performance_schema.data_locks（MariaDB 仍用 information_schema.INNODB_LOCKS）。
func (v Version) newLocks() bool { return !v.MariaDB && v.atLeast(8, 0, 1) }
