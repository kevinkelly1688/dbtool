// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

package dict

import "fmt"

// Kind 是数据字典里的对象类别。
type Kind string

const (
	KindTable     Kind = "table"
	KindView      Kind = "view"
	KindTrigger   Kind = "trigger"
	KindProcedure Kind = "procedure"
	KindFunction  Kind = "function"
	KindEvent     Kind = "event"
)

var allKinds = []Kind{KindTable, KindView, KindTrigger, KindProcedure, KindFunction, KindEvent}

func validKind(k Kind) bool {
	for _, x := range allKinds {
		if x == k {
			return true
		}
	}
	return false
}

// Item 是对象列表里的一项。SQL Server 的对象带架构名；MySQL 的 Schema 为空。
//
// 表的估算行数 / 大小 / 分区表标记取不到时留空（指针为 nil、Partitioned 为 false），不影响列表；
// Sensitive 是该表里命中脱敏规则的列数（没有配置脱敏规则或读取失败时为 0）。
type Item struct {
	Schema      string `json:"schema,omitempty"`
	Name        string `json:"name"`
	Comment     string `json:"comment"`
	RowsEst     *int64 `json:"rows_est,omitempty"`    // 估算行数
	SizeBytes   *int64 `json:"size_bytes,omitempty"`  // 数据+索引大小（字节）
	Partitioned bool   `json:"partitioned,omitempty"` // 分区表
	AutoIncPK   string `json:"auto_inc_pk,omitempty"` // 主键含自增列时的列名（目前只有 SQL Server 填）；前端标注“自增主键”
	Sensitive   int    `json:"sensitive,omitempty"`   // 敏感列数
	Type        string `json:"type,omitempty"`        // 仅 PostgreSQL / MongoDB / ClickHouse：对象的具体类型（BASE TABLE / VIEW、collection / view / timeseries、引擎名）
}

// Pair 是详情里的一对键值。
type Pair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Section 是详情里的一段：kv（键值）、table（表格）、code（代码块）、note（说明文字）。
// 全部是展示用的字符串，前端用 textContent 渲染。
type Section struct {
	Title   string     `json:"title"`
	Type    string     `json:"type"`
	Pairs   []Pair     `json:"pairs,omitempty"`
	Columns []string   `json:"columns,omitempty"`
	Rows    [][]string `json:"rows,omitempty"`
	Text    string     `json:"text,omitempty"`
	Note    string     `json:"note,omitempty"` // 显示在 code 小节下方的小字说明（如“由系统目录还原，仅供参考”）
}

// Detail 是对象详情。
type Detail struct {
	Kind     Kind      `json:"kind"`
	Schema   string    `json:"schema,omitempty"`
	Name     string    `json:"name"`
	Sections []Section `json:"sections"`
	// SensitiveColumns 是命中脱敏规则的列名（表、视图详情；没有则为空）。
	SensitiveColumns []string `json:"sensitive_columns,omitempty"`
	// Notes 是仅 PostgreSQL / MongoDB / ClickHouse 返回的说明（行数是估算、字段是采样、没有创建时间等），前端逐条显示。
	Notes []string `json:"notes,omitempty"`
}

// ---- 错误 ----

// Error 是对外可见的业务错误，Message 可直接展示给用户。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

func badReq(format string, a ...any) *Error { return newErr(400, "bad_request", format, a...) }
