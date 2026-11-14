// @author: kevinkelly
// @license: Apache Licence
// @file: dialect.go
// @time: 2026/10/28

package dict

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rows 是一次查询的结果（值是驱动返回的原始类型，展示前用 str 转成字符串）。
type Rows struct {
	Columns []string
	Data    [][]any
}

// Querier 是一条只读的元数据连接：只执行本包里写死的 information_schema / 系统目录查询，参数一律走占位符。
// 测试里用假实现；真实实现见 opener.go。
type Querier interface {
	Query(ctx context.Context, q string, args ...any) (*Rows, error)
	Close() error
}

var (
	columnHeads = []string{"列名", "类型", "可空", "默认值", "键", "额外", "注释"}
	indexHeads  = []string{"索引名", "列", "唯一", "类型"}
)

// dialect 是某一类实例的元数据查询实现。
type dialect interface {
	// Kinds 返回这类实例支持的对象类别。
	Kinds() []Kind
	// Databases 返回实例上的库（已排除系统库）。
	Databases(ctx context.Context, q Querier) ([]string, error)
	// IsSystemDB 判断是不是系统库（永远不展示）。
	IsSystemDB(name string) bool
	// List 列出某库下某类对象。
	List(ctx context.Context, q Querier, db string, kind Kind) ([]Item, error)
	// Detail 返回对象详情；对象不存在返回 *Error{404}。
	Detail(ctx context.Context, q Querier, db string, kind Kind, schema, name string) (*Detail, error)
}

// mysqlFamily 里的类型走 MySQL 协议与 information_schema；其中只有 mysql / polardb / polardb-x 的视图、触发器、
// 存储过程、函数、事件开放，其余（SelectDB / Doris / ADB）只开放表。
var mysqlFamily = map[string]bool{"mysql": true, "polardb": true, "polardb-x": true, "selectdb": true, "doris": true, "adb": true}

func dialectFor(dbType string) dialect {
	switch {
	case dbType == typePG:
		return newPGDialect()
	case dbType == typeMongo:
		return newMongoDialect()
	case dbType == typeCH:
		return newCHDialect()
	case dbType == typeOracle:
		return newOracleDialect()
	case dbType == typeDM:
		return newDMDialect()
	case dbType == "mssql":
		return mssqlDialect{}
	case mysqlFamily[dbType]:
		return mysqlDialect{full: dbType == "mysql" || dbType == "polardb" || dbType == "polardb-x"}
	}
	return nil
}

// ---- 值转换 ----

// str 把驱动返回的值转成展示用的字符串（NULL 为空串）。
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case string:
		return x
	case time.Time:
		return x.Format("2006-01-02 15:04:05")
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// strNull 与 str 相同，但 NULL 显示为 nullText（区分“默认值是 NULL”和“默认值是空串”）。
func strNull(v any, nullText string) string {
	if v == nil {
		return nullText
	}
	return str(v)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case int:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	case []byte:
		f, err := strconv.ParseFloat(string(x), 64)
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

// kb 把字节数换成 KB 字符串（保留两位小数，去掉多余的 0）；取不到返回空串。
func kb(v any) string {
	f, ok := toFloat(v)
	if !ok {
		return ""
	}
	return strconv.FormatFloat(f/1024, 'f', 2, 64)
}

// yesNo 把 YES/NO、1/0 之类转成中文。
func yesNo(v any) string {
	switch strings.ToUpper(strings.TrimSpace(str(v))) {
	case "YES", "1", "TRUE", "Y":
		return "是"
	case "NO", "0", "FALSE", "N":
		return "否"
	}
	return str(v)
}

// aggregateIndexes 把“每行一个索引列”的结果合并成“每个索引一行”。rows 的列：表名（可为空串）、索引名、是否唯一、类型、列名，
// 必须已按 表名、索引名、列顺序 排好序。返回 map[表名][]行（索引名、列、唯一、类型）。
type indexRow struct{ table, name, unique, typ, col string }

func aggregateIndexes(rows []indexRow) map[string][][]string {
	out := map[string][][]string{}
	var cur *[]string
	var curKey string
	for _, r := range rows {
		key := r.table + "\x00" + r.name
		if cur == nil || key != curKey {
			out[r.table] = append(out[r.table], []string{r.name, r.col, r.unique, r.typ})
			cur = &out[r.table][len(out[r.table])-1]
			curKey = key
			continue
		}
		(*cur)[1] += ", " + r.col
	}
	return out
}
