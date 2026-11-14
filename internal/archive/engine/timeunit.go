// @author: kevinkelly
// @license: Apache Licence
// @file: timeunit.go
// @time: 2026/10/28

package engine

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// 时间列的单位。
//
// date / datetime / timestamp 列直接和 NOW() 比较；int / bigint 里存的是 Unix 时间戳（10 位秒或 13 位毫秒），
// 必须先把“N 天前”换算成同一个单位再比较。不能把整型列直接拿去和 NOW() - INTERVAL 比：
// MySQL 会把日期转成 20260424120000 这样的数字，比任何 10 / 13 位时间戳都大，结果是整张表都命中。
const (
	UnitDatetime = ""         // date / datetime / timestamp 列（默认；旧任务没有这个字段，等同于它）
	UnitEpochSec = "epoch_s"  // int / bigint，Unix 秒（10 位）
	UnitEpochMs  = "epoch_ms" // int / bigint，Unix 毫秒（13 位）
)

// 各单位下“像时间戳”的取值区间：1980-01-01 00:00:00 UTC 至 2100-01-01 00:00:00 UTC。
// 它用来识别选错单位 / 选错列（例如把 10 位秒当成毫秒，或者选了自增主键），不是对数据的硬性要求。
const (
	epochSecMin int64 = 315532800
	epochSecMax int64 = 4102444800
)

// NormalizeTimeUnit 规整时间单位：去空白、转小写；datetime 记为 UnitDatetime（空串）。ok=false 表示不认识。
func NormalizeTimeUnit(u string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(u)) {
	case "", "datetime":
		return UnitDatetime, true
	case UnitEpochSec:
		return UnitEpochSec, true
	case UnitEpochMs:
		return UnitEpochMs, true
	}
	return "", false
}

// IsEpochUnit 判断是不是整型时间戳单位（秒 / 毫秒）。
func IsEpochUnit(u string) bool { return u == UnitEpochSec || u == UnitEpochMs }

// EpochDigits 返回该单位的时间戳位数（秒 10、毫秒 13）；非时间戳单位返回 0。
func EpochDigits(u string) int {
	switch u {
	case UnitEpochSec:
		return 10
	case UnitEpochMs:
		return 13
	}
	return 0
}

// EpochRange 返回该单位下“像时间戳”的取值区间 [lo, hi]；非时间戳单位返回 0, 0。
func EpochRange(u string) (lo, hi int64) {
	switch u {
	case UnitEpochSec:
		return epochSecMin, epochSecMax
	case UnitEpochMs:
		return epochSecMin * 1000, epochSecMax * 1000
	}
	return 0, 0
}

// IsIntegerTimeType 判断列类型能不能存 10 / 13 位的 Unix 时间戳：int / bigint（含 unsigned、显示宽度）。
// tinyint / smallint / mediumint 装不下，decimal / float / varchar 不支持。
func IsIntegerTimeType(colType string) bool {
	t := strings.ToLower(strings.TrimSpace(colType))
	if i := strings.IndexAny(t, "( "); i >= 0 {
		t = t[:i]
	}
	return t == "int" || t == "integer" || t == "bigint"
}

// epochCutoff 返回 SQL 里“N 天前”的时间戳表达式。UNIX_TIMESTAMP() 与会话时区无关；它在一条语句内是常量，列上不套函数，索引照常可用。
func epochCutoff(unit string, days int) string {
	sec := int64(days) * 86400
	if unit == UnitEpochMs {
		return fmt.Sprintf("(UNIX_TIMESTAMP() - %d) * 1000", sec)
	}
	return fmt.Sprintf("UNIX_TIMESTAMP() - %d", sec)
}

// FormatEpoch 把时间戳按单位换成 "2006-01-02 15:04:05"；offsetSec 是显示用的时区偏移（秒）。超出可显示范围时原样返回数字。
func FormatEpoch(v int64, unit string, offsetSec int) string {
	sec := v
	if unit == UnitEpochMs {
		sec = v / 1000
	}
	if sec < -62135596800 || sec > 253402300799 {
		return strconv.FormatInt(v, 10)
	}
	return time.Unix(sec, 0).In(time.FixedZone("", offsetSec)).Format("2006-01-02 15:04:05")
}

// ---- 探测 ----

// TimeProbe 是对一个整型时间戳列的探测结果：抽样判断像不像秒 / 毫秒，必要时取最早 / 最晚值。
type TimeProbe struct {
	Sampled   int    `json:"sampled"`    // 抽样读到的行数（只取值大于 0 的行）
	InSec     int    `json:"in_sec"`     // 其中落在 10 位秒级区间的行数
	InMs      int    `json:"in_ms"`      // 其中落在 13 位毫秒级区间的行数
	Sample    int64  `json:"sample"`     // 抽到的第一个值；没有抽到为 0
	HasRange  bool   `json:"has_range"`  // 是否取到了最早 / 最晚值
	Min       int64  `json:"min"`        // 值大于 0 的最小值（HasRange 时有效）
	Max       int64  `json:"max"`        // 值大于 0 的最大值（HasRange 时有效）
	OffsetSec int    `json:"offset_sec"` // 服务器时区相对 UTC 的偏移（秒），换算成可读时间用
	GuessUnit string `json:"guess_unit"` // 抽样里至少 90% 落在某一单位的区间时，给出该单位；否则为空
}

// TimeProber 是源库的可选能力：探测整型时间戳列。rangeOK 表示允许用 MIN / MAX 取最早 / 最晚值
// （列上有索引，或表很小；否则会全表扫描，不取）。不实现它的源库，试运行里这一项标“无法核对”。
type TimeProber interface {
	ProbeTime(ctx context.Context, db, table, col string, rangeOK bool) (TimeProbe, error)
}

// Querier 是 *sql.DB / *sql.Conn / *sql.Tx 共有的只读查询能力。
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// probeSampleRows 是抽样行数。不加 ORDER BY：按存储顺序（通常是主键序）取前若干行，开销与表大小无关。
const probeSampleRows = 1000

// ProbeEpochColumn 探测整型时间戳列。只发 SELECT；列名必须是合法标识符。
func ProbeEpochColumn(ctx context.Context, q Querier, db, table, col string, rangeOK bool) (TimeProbe, error) {
	var p TimeProbe
	if !validIdent(db) || !validIdent(table) || !validIdent(col) {
		return p, fmt.Errorf("库名 / 表名 / 列名无效")
	}
	tbl, c := qualified(db, table), quoteIdent(col)

	// 抽样：只看值大于 0 的行（0 / NULL 表示“未设置”，归档条件也排除它们）
	rs, err := q.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s WHERE %s > 0 LIMIT %d", c, tbl, c, probeSampleRows))
	if err != nil {
		return p, err
	}
	for rs.Next() {
		var s sql.NullString
		if err := rs.Scan(&s); err != nil {
			rs.Close()
			return p, err
		}
		v, ok := parseEpochInt(s.String)
		if !s.Valid || !ok {
			continue
		}
		p.Sampled++
		if p.Sample == 0 {
			p.Sample = v
		}
		switch {
		case v >= epochSecMin && v <= epochSecMax:
			p.InSec++
		case v >= epochSecMin*1000 && v <= epochSecMax*1000:
			p.InMs++
		}
	}
	err = rs.Err()
	rs.Close()
	if err != nil {
		return p, err
	}
	switch {
	case p.Sampled == 0:
	case p.InSec*10 >= p.Sampled*9:
		p.GuessUnit = UnitEpochSec
	case p.InMs*10 >= p.Sampled*9:
		p.GuessUnit = UnitEpochMs
	}

	// 最早 / 最晚：失败不影响抽样结论（可能是超时）
	if rangeOK {
		var mn, mx sql.NullString
		if err := q.QueryRowContext(ctx, fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s WHERE %s > 0", c, c, tbl, c)).Scan(&mn, &mx); err == nil && mn.Valid && mx.Valid {
			lo, ok1 := parseEpochInt(mn.String)
			hi, ok2 := parseEpochInt(mx.String)
			if ok1 && ok2 {
				p.HasRange, p.Min, p.Max = true, lo, hi
			}
		}
	}

	// 显示用的时区：会话时区就是服务器时区（Opener 把会话固定成服务器偏移；向导的连接用服务器默认时区）
	var diff string
	if err := q.QueryRowContext(ctx, "SELECT TIMEDIFF(NOW(), UTC_TIMESTAMP())").Scan(&diff); err == nil {
		p.OffsetSec = offsetSeconds(diff)
	}
	return p, nil
}

// parseEpochInt 解析十进制整数文本；大到装不下 int64 的正数按 MaxInt64 处理（无符号 bigint 的极端值）。
func parseEpochInt(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err == nil {
		return v, true
	}
	if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange && !strings.HasPrefix(s, "-") {
		return math.MaxInt64, true
	}
	return 0, false
}

// offsetSeconds 把 TIMEDIFF 的结果（如 "08:00:00"、"-05:30:00"）转成秒；解析不了按 0。
func offsetSeconds(s string) int {
	f, err := fmtOffset(s) // "+08:00"
	if err != nil || len(f) != 6 {
		return 0
	}
	h, _ := strconv.Atoi(f[1:3])
	m, _ := strconv.Atoi(f[4:6])
	sec := h*3600 + m*60
	if f[0] == '-' {
		sec = -sec
	}
	return sec
}

// ---- 运行期兜底 ----

// batchTimeMax 取一批行里时间列的最大值；找不到列或一个值都解析不了时 ok=false。
func batchTimeMax(b Batch, col string) (max int64, ok bool) {
	idx := -1
	for i, c := range b.Cols {
		if strings.EqualFold(c, col) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return 0, false
	}
	for _, r := range b.Rows {
		if idx >= len(r) {
			continue
		}
		v, vok := anyToInt64(r[idx])
		if !vok {
			continue
		}
		if !ok || v > max {
			max, ok = v, true
		}
	}
	return max, ok
}

func anyToInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		if x > math.MaxInt64 {
			return math.MaxInt64, true
		}
		return int64(x), true
	case []byte:
		return parseEpochInt(string(x))
	case string:
		return parseEpochInt(x)
	}
	return 0, false
}
