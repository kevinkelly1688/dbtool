// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

package sqlopt

import (
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/settings"
)

// 按钮权限码（登记表 internal/btnperm）。
const (
	PermMenuAnalyze   = "sqlanalyze.view"  // 打开 SQL 体检页
	PermMenuAdvisor   = "optimize.explain" // 打开智能调优页：执行计划 / 索引建议 / 格式化
	PermAnalyze       = "sqlanalyze.run"   // 上传并开始分析
	PermSOAR          = "optimize.soar"    // SOAR 优化
	PermSQLTuning     = "optimize.tuning"  // SQLTuning 优化
	permQueryAllInsts = "query.admin_run"  // 查看 / 查询全部实例
	configSoar        = settings.KeyOptimizerSoarPath
	configSoarTestDSN = settings.KeyOptimizerSoarTestDSN
	configSoarConfig  = settings.KeyOptimizerSoarConfig
)

// 工具名（对应前端“优化方式”）。
const (
	ToolSOAR   = "soar"
	ToolTuning = "sqltuning"
)

// 资源上限。上传内容是不可信数据，所有上限都在服务端强制。
const (
	MaxTextBytes      = 5 << 20 // 上传文本（SQL / Mapper XML / 慢日志）上限
	MaxRequestBytes   = 12 << 20
	MaxStatements     = 1000     // 一次分析任务最多多少条 SQL
	MaxSQLBytes       = 60 << 10 // 单条 SQL 上限（也受命令行长度限制）
	MaxItemReportSize = 64 << 10 // 单条 SQL 的外部工具报告上限
	MaxToolOutput     = 1 << 20  // 单次外部工具输出上限
	MaxJobBytes       = 8 << 20  // 单个任务保存的结果总量上限
	MaxTableRows      = 500      // 返回给页面的结果集行数上限
	MaxCellBytes      = 64 << 10 // 结果集单元格长度上限
	MaxTuningTables   = 10       // SQLTuning 最多展示几张表的统计信息

	jobTTL          = 30 * time.Minute // 已结束任务保留时长
	jobMaxRun       = 30 * time.Minute // 单个任务最长运行时间
	explainTimeout  = 30 * time.Second
	adviseTimeout   = 45 * time.Second
	tuningTimeout   = 120 * time.Second
	soarTimeout     = 90 * time.Second
	acquireWait     = 5 * time.Second
	maxActivePerUsr = 2
	maxActiveAll    = 16
	maxStoredJobs   = 60
)

// Error 是业务错误，Message 可直接展示给用户。
type Error struct {
	HTTP    int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, msg string) *Error {
	return &Error{HTTP: status, Code: code, Message: msg}
}

func bad(msg string) *Error { return newErr(http.StatusBadRequest, "bad_request", msg) }

// Table 是返回给页面的结果集。单元格已转成可展示的值（字符串、数字或 nil）。
type Table struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated,omitempty"`
}

// colIndex 返回列名（不区分大小写）的位置，没有返回 -1。
func (t *Table) colIndex(name string) int {
	if t == nil {
		return -1
	}
	for i, c := range t.Columns {
		if strings.EqualFold(c, name) {
			return i
		}
	}
	return -1
}

// str 取单元格的字符串形式，nil 返回空串。
func (t *Table) str(row []any, name string) string {
	i := t.colIndex(name)
	if i < 0 || i >= len(row) || row[i] == nil {
		return ""
	}
	return cellString(row[i])
}

// 目标库类型分类：EXPLAIN 与 SOAR / SQLTuning 的适用范围。
var (
	toolDBTypes    = map[string]bool{"mysql": true, "polardb": true, "polardb-x": true}
	explainDBTypes = map[string]bool{"mysql": true, "polardb": true, "polardb-x": true, "selectdb": true, "doris": true, "adb": true}
)

// systemDBs 默认不展示的系统库，与 engine/mysql 的过滤一致。
var systemDBs = map[string]bool{
	"information_schema": true, "performance_schema": true, "mysql": true, "test": true, "sys": true,
}

// validDBName 校验库名：用于 USE（会加反引号转义）与外部工具参数前的第一道检查。
func validDBName(db string) bool {
	if db == "" || utf8.RuneCountInString(db) > 64 || !utf8.ValidString(db) {
		return false
	}
	for _, r := range db {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// toolDBNameRe 是传给外部工具的库名白名单（工具会把它拼进自己的 DSN / 命令里，宁严勿宽）。
var toolDBNameRe = regexp.MustCompile(`^[A-Za-z0-9_$][A-Za-z0-9_$.\-]{0,63}$`)

// quoteIdent 给标识符加反引号并转义。
func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// instanceView 是页面下拉里用的实例信息（不含凭据）。
type instanceView struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	DBType  string `json:"db_type"`
	Type    string `json:"type"`
	Tools   bool   `json:"tools"`   // 支持 SOAR / SQLTuning / 内置建议
	Explain bool   `json:"explain"` // 支持执行计划
}

func viewOf(i *model.Instance) instanceView {
	return instanceView{ID: i.ID, Name: i.InstanceName, DBType: i.DBType, Type: i.Type, Tools: toolDBTypes[i.DBType], Explain: explainDBTypes[i.DBType]}
}

// connOf 取实例的连接信息；DBName 清空，库由会话里的 USE 决定。
func connOf(repo Repo, inst *model.Instance) engine.Conn {
	c := repo.EngineConn(inst)
	c.DBName = ""
	return c
}

// truncRunes 按字符数截断。
func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// redact 把输出里的敏感字符串（实例密码等）替换掉。过短的值不处理，避免误伤。
func redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= 4 {
			s = strings.ReplaceAll(s, sec, "******")
		}
	}
	return s
}
