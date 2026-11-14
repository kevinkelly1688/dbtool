// @author: kevinkelly
// @license: Apache Licence
// @file: plan.go
// @time: 2026/10/28

package partman

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// 本文件是分区维护的核心：纯函数，不访问数据库，给定“配置 + 表当前的分区状态 + 当前时间”，算出要执行的语句。
//
// 命名与边界约定（与原型、外部程序的示例一致）：分区名 = 该分区起始时刻按周期格式化后加 p 前缀
// （day → p20261009，month → p202610，hour → p2026100915，minute → p202610091530，year → p2026，
// week → 窗口最后一天 p20261011，与原外部程序一致），VALUES LESS THAN 取该分区的结束时刻（下一个分区的起始时刻）。
// SelectDB / Doris 方言：ADD PARTITION p VALUES [("下界"), ("上界"))，每个分区一条语句；没有 REORGANIZE / MAXVALUE，也不初始化未分区表。
// 续建一律从“现有最后一个带边界的分区”的边界开始按“周期 × 跨度”向后接，保证边界连续、永不与既有分区冲突，
// 不依赖外部程序当初用什么对齐方式建的分区。

// maxAddPerRun 是单次最多创建的分区数。超过说明配置明显不合理（比如按分钟分区却很久没维护），拒绝而不是一口气建几千个。
const maxAddPerRun = 100

// RawPartition 是 information_schema.PARTITIONS 里的一行（按 PARTITION_ORDINAL_POSITION 排序）。
type RawPartition struct {
	Name string
	Desc string // PARTITION_DESCRIPTION：RANGE 是数字，RANGE COLUMNS 是带引号的字面量，最后一个可能是 MAXVALUE
}

// TableState 是目标表当前的状态。
type TableState struct {
	Exists         bool
	IsBase         bool   // information_schema.TABLES.TABLE_TYPE = 'BASE TABLE'
	ColumnType     string // 分区字段的 DATA_TYPE（int / bigint / datetime / date ...）；字段不存在时为空
	ColumnTypeFull string // COLUMN_TYPE，用来识别 unsigned
	Method         string // RANGE / RANGE COLUMNS / LIST ...；不是分区表时为空
	Expression     string // PARTITION_EXPRESSION
	Subpartitioned bool
	Partitions     []RawPartition // Doris：Desc 是 SHOW PARTITIONS 的 Range 列原文

	// Dialect 为空按 MySQL；服务层按实例类型设置（DialectOf）。
	Dialect string
	// SessionOffset 是目标库会话相对 UTC 的偏移秒数（配置时区为空时用）；HasSessionOffset=false 表示没读到。
	SessionOffset    int
	HasSessionOffset bool
	// PartitionKey 是 Doris SHOW PARTITIONS 的 PartitionKey（只用于提示，不做硬校验）。
	PartitionKey string

	// 以下只在 MySQL 方言且表未分区时由 Inspect 读取，供“初始化为分区表”的前置检查使用。
	Engine      string      // information_schema.TABLES.ENGINE
	UniqueKeys  []UniqueKey // 含主键
	ForeignKeys []string    // 与该表有关的外键（作为子表或被引用）的描述，非空即不能分区
}

// UniqueKey 是一个唯一索引（含主键 PRIMARY）及其列。
type UniqueKey struct {
	Name    string
	Columns []string
}

// 方言。
const (
	DialectMySQL = "mysql"
	DialectDoris = "doris"
)

// DialectOf 由实例类型得到分区维护方言；不支持的类型返回 ""。
func DialectOf(dbType string) string {
	switch strings.ToLower(strings.TrimSpace(dbType)) {
	case "mysql", "polardb":
		return DialectMySQL
	case "selectdb", "doris":
		return DialectDoris
	}
	return ""
}

// 计划类型。
const (
	KindMaintain = "maintain"
	KindInit     = "init"
)

// 初始化起点。
const (
	StartCurrent  = "current"
	StartEarliest = "earliest"
	StartDate     = "date"
)

// 初始化的上限（沿用原程序 5000；超过 200 给 warning）。
const (
	maxInitPartitions  = 5000
	warnInitPartitions = 200
)

// InitOptions 是“初始化为分区表”的选项。EarliestRead / HasEarliest / Earliest 由服务层在 StartMode=earliest 时读目标库后填入（BuildPlan 保持纯函数）。
type InitOptions struct {
	StartMode    string // 空 = current
	StartDate    string // StartMode=date：YYYY-MM-DD 或 YYYY-MM-DD HH:MM:SS（配置时区）
	EarliestRead bool   // 已经尝试读取最早数据
	HasEarliest  bool   // 表里有数据
	Earliest     time.Time
}

// InitCheck 是一项前置检查的结果。Level：ok / warn / error。
type InitCheck struct {
	Code    string `json:"code"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// InitInfo 是初始化计划的说明。
type InitInfo struct {
	StartMode      string
	StartDate      string
	Start          time.Time // 第一个窗口的起点（已按网格对齐）
	PartitionCount int
	First, Last    string // 第一个 / 最后一个分区名
	LastBound      time.Time
	Earliest       time.Time // StartMode=earliest 且有数据时
	CanExecute     bool
	Checks         []InitCheck
	Notes          []string
}

// Partition 是解析后的现有分区。
type Partition struct {
	Name     string
	Desc     string
	MaxValue bool
	Unknown  bool      // 只有 Doris：Range 文本里读不到上界。不参与续建与清理（永不删除）
	Bound    time.Time // 上边界（不含），MaxValue / Unknown 时为零值
}

// Statement 是一条要执行的 ALTER 语句。
type Statement struct {
	Kind       string   `json:"kind"` // add / drop / reorganize / init
	Partitions []string `json:"partitions"`
	Bound      string   `json:"bound,omitempty"` // add：新分区的上边界；reorganize：最后一个新分区的上边界
	SQL        string   `json:"sql"`
	Danger     bool     `json:"danger"` // 会永久删除数据
	Heavy      bool     `json:"heavy"`  // 会重写 MAXVALUE 分区里的数据，耗时可能很长
}

// Plan 是一次维护要做的事。
type Plan struct {
	Config     Config
	Now        time.Time // 配置时区下的当前时间
	Method     string
	Expression string
	ColumnType string
	Existing   []Partition
	Adds       []Statement
	Drops      []Statement
	Reorganize *Statement // 表里有 MAXVALUE 分区时，续建改用 REORGANIZE PARTITION（Adds 为空）
	Warnings   []string

	Kind     string     // KindMaintain / KindInit
	Dialect  string     // DialectMySQL / DialectDoris
	Timezone string     // 实际使用的时区（展示用）
	InitStmt *Statement // Kind=init 且前置检查通过时的那一条 ALTER
	Init     *InitInfo
	Log      []planLog // 检查 / 决策过程（运行输出用）
}

// Statements 按执行顺序返回全部语句：先续建（或重组），再清理。初始化计划只有一条语句。
func (p *Plan) Statements() []Statement {
	var out []Statement
	if p.InitStmt != nil {
		return []Statement{*p.InitStmt}
	}
	if p.Reorganize != nil {
		out = append(out, *p.Reorganize)
	}
	out = append(out, p.Adds...)
	out = append(out, p.Drops...)
	return out
}

// DropNames 返回计划删除的分区名。
func (p *Plan) DropNames() []string {
	var out []string
	for _, d := range p.Drops {
		out = append(out, d.Partitions...)
	}
	return out
}

func conflict(code, format string, a ...any) *Error { return newErr(409, code, format, a...) }

func normExpr(s string) string {
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, " ", "")
	return strings.ToLower(s)
}

const dtLayout = "2006-01-02 15:04:05"

// parseBound 把 PARTITION_DESCRIPTION 解析成时间点。
func parseBound(desc, colType string, loc *time.Location) (time.Time, error) {
	s := strings.TrimSpace(desc)
	s = strings.Trim(s, "'")
	switch colType {
	case ColDatetime, ColDate:
		for _, layout := range []string{dtLayout, "2006-01-02 15:04:05.999999", "2006-01-02"} {
			if t, err := time.ParseInLocation(layout, s, loc); err == nil {
				return t, nil
			}
		}
	case ColInt, ColBigint:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			if colType == ColInt {
				return time.Unix(n, 0).In(loc), nil
			}
			return time.UnixMilli(n).In(loc), nil
		}
	}
	return time.Time{}, fmt.Errorf("无法按 %s 解析分区边界 %s", colType, qstr(desc))
}

// step 返回 t 往后推 k 个“周期 × 跨度”。month / year 用日历加法，要求 t 已对齐到月初 / 年初（由调用方检查）。
func step(t time.Time, period string, n, k int) time.Time {
	m := n * k
	switch period {
	case "minute":
		return t.Add(time.Duration(m) * time.Minute)
	case "hour":
		return t.Add(time.Duration(m) * time.Hour)
	case "day":
		return t.AddDate(0, 0, m)
	case "week":
		return t.AddDate(0, 0, 7*m)
	case "month":
		return t.AddDate(0, m, 0)
	default: // year
		return t.AddDate(m, 0, 0)
	}
}

// partName 按周期格式化分区名：起始时刻；week 取窗口最后一天（end 的前一天），与原外部程序一致。
func partName(start, end time.Time, period string) string {
	switch period {
	case "minute":
		return "p" + start.Format("200601021504")
	case "hour":
		return "p" + start.Format("2006010215")
	case "month":
		return "p" + start.Format("200601")
	case "year":
		return "p" + start.Format("2006")
	case "week":
		return "p" + end.AddDate(0, 0, -1).Format("20060102")
	default: // day
		return "p" + start.Format("20060102")
	}
}

// literal 是 VALUES LESS THAN 里的边界字面量。
func literal(t time.Time, cfg *Config, st *TableState) (string, error) {
	switch cfg.ColType {
	case ColDatetime:
		return "'" + t.Format(dtLayout) + "'", nil
	case ColDate:
		return "'" + t.Format("2006-01-02") + "'", nil
	case ColBigint:
		return strconv.FormatInt(t.UnixMilli(), 10), nil
	default: // int
		v := t.Unix()
		limit := int64(math.MaxInt32)
		if strings.Contains(strings.ToLower(st.ColumnTypeFull), "unsigned") {
			limit = math.MaxUint32
		}
		if v > limit {
			return "", conflict("int_overflow", "边界 %s 对应的秒级时间戳 %d 超出 int 的范围，请把分区字段改为 bigint 或 datetime", t.Format(dtLayout), v)
		}
		return strconv.FormatInt(v, 10), nil
	}
}

func tableLabel(cfg *Config) string { return cfg.DBName + "." + cfg.TbName }
