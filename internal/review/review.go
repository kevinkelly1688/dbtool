// @author: kevinkelly
// @license: Apache Licence
// @file: review.go
// @time: 2026/10/28

// Package review 定义 SQL 检查/执行结果的数据结构，
// 存进 dbt_sql_workflow_content.review_content / execute_result 的内容采用它的 JSON 格式。
package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 检查/执行阶段的错误级别。
const (
	LevelOK      = 0
	LevelWarning = 1
	LevelError   = 2
)

// Flex 是兼容字符串与数字的整数：已存储的 affected_rows 等字段两种形态都出现过。
type Flex int64

func (f *Flex) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" || len(b) == 0 {
		*f = 0
		return nil
	}
	s := strings.Trim(string(b), `"`)
	if s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		if fl, err2 := strconv.ParseFloat(s, 64); err2 == nil {
			*f = Flex(fl)
			return nil
		}
		*f = 0
		return nil // 脏数据按 0 处理，不让整条工单无法展示
	}
	*f = Flex(n)
	return nil
}

// Row 是一条语句的检查/执行结果。
type Row struct {
	ID                 Flex   `json:"id"`
	Stage              string `json:"stage"`
	ErrLevel           Flex   `json:"errlevel"`
	StageStatus        string `json:"stagestatus"`
	ErrorMessage       string `json:"errormessage"`
	SQL                string `json:"sql"`
	AffectedRows       Flex   `json:"affected_rows"`
	Sequence           string `json:"sequence"`
	BackupDBName       string `json:"backup_dbname"`
	ExecuteTime        string `json:"execute_time"`
	SQLSha1            string `json:"sqlsha1"`
	BackupTime         string `json:"backup_time"`
	ActualAffectedRows string `json:"actual_affected_rows"`

	// 批量上线工单的扩展字段（只有批量工单才有，序列化时省略空值）。
	InstanceID   Flex   `json:"instance_id,omitempty"`
	InstanceName string `json:"instance_name,omitempty"`
	IsSummary    bool   `json:"is_summary,omitempty"`
	BatchClosed  bool   `json:"batch_closed,omitempty"`

	// Redis 工单：执行前记录的被改键的原值（规则勾选“记录执行前值”时才有；供人工恢复，序列化时省略空值）。
	Before string `json:"before,omitempty"`
}

// Set 是一次检查或执行的完整结果。
type Set struct {
	FullSQL      string
	Rows         []Row
	Warning      string
	Error        string
	WarningCount int
	ErrorCount   int
	SyntaxType   int    // 1 DDL 2 DML 3 离线导出 0 其他
	FileName     string // 数据导出工单：生成的文件名（不序列化到结果里，由服务写进 dbt_sql_workflow.file_name）
}

// JSON 序列化为 review_content / execute_result 的格式（Row 的 JSON 数组）。
func (s *Set) JSON() string {
	rows := s.Rows
	if rows == nil {
		rows = []Row{}
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

// Count 统计各级别的数量并写回 WarningCount / ErrorCount。
func (s *Set) Count() {
	s.WarningCount, s.ErrorCount = 0, 0
	for _, r := range s.Rows {
		switch r.ErrLevel {
		case LevelWarning:
			s.WarningCount++
		case LevelError:
			s.ErrorCount++
		}
	}
}

// ParseRows 解析 review_content / execute_result。兼容旧数据 `[[...]]`（按 goInception 原始列顺序的数组）。
// 空串返回空切片；无法解析返回错误。
func ParseRows(s string) ([]Row, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("执行结果 JSON 解析失败: %w", err)
	}
	out := make([]Row, 0, len(raw))
	for _, item := range raw {
		item = bytes.TrimSpace(item)
		if len(item) > 0 && item[0] == '[' { // 旧格式：列表
			var cols []any
			if err := json.Unmarshal(item, &cols); err != nil {
				return nil, err
			}
			out = append(out, RowFromColumns(cols))
			continue
		}
		var r Row
		if err := json.Unmarshal(item, &r); err != nil {
			return nil, fmt.Errorf("执行结果 JSON 解析失败: %w", err)
		}
		out = append(out, r)
	}
	return out, nil
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case []byte:
		return string(x)
	}
	return fmt.Sprint(v)
}

func num(v any) Flex {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return Flex(x)
	case int64:
		return Flex(x)
	case int:
		return Flex(x)
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(str(v)), 10, 64)
	return Flex(n)
}

// RowFromColumns 把 goInception 结果集的一行（按位置）转成 Row：
// [0]序号 [1]阶段 [2]错误级别 [3]阶段状态 [4]错误信息 [5]SQL [6]影响行数 [7]sequence [8]备份库 [9]执行耗时 [10]sha1 [11]备份耗时。
func RowFromColumns(c []any) Row {
	get := func(i int) any {
		if i < len(c) {
			return c[i]
		}
		return nil
	}
	return Row{
		ID: num(get(0)), Stage: str(get(1)), ErrLevel: num(get(2)), StageStatus: str(get(3)), ErrorMessage: str(get(4)),
		SQL: str(get(5)), AffectedRows: num(get(6)), Sequence: str(get(7)), BackupDBName: str(get(8)),
		ExecuteTime: str(get(9)), SQLSha1: str(get(10)), BackupTime: str(get(11)),
	}
}
