// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

// Package maskadmin 是“安全合规 › 数据脱敏”：给管理员批量选择多个库表字段、匹配同一条脱敏规则，
// 配置存在 dbt_masking_policy（见 deploy/sql/230_masking_policy.sql）。
//
// 脱敏本身在各引擎的在线查询 / 导出里执行（internal/query、internal/sqlflow），这里只管配置。仅超级用户可用。
package maskadmin

import "dbtool/internal/masking"

// Error 是业务错误，Status 为 HTTP 状态码。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func badReq(msg string) *Error { return &Error{Status: 400, Code: "bad_request", Message: msg} }

// RuleSpec 是接口和存储共用的规则描述。
type RuleSpec struct {
	Kind   string `json:"kind"`             // keep 保留首尾 / three 三段式 / whole 整体隐藏 / regex 自定义正则 / legacy 无法识别的旧规则
	Prefix int    `json:"prefix,omitempty"` // keep：保留前几位
	Suffix int    `json:"suffix,omitempty"` // keep：保留后几位
	Hide   int    `json:"hide,omitempty"`   // three：1 前段 2 中段 3 后段；regex：要隐藏的分组
	Regex  string `json:"regex,omitempty"`
}

func (r RuleSpec) toRule() masking.Rule {
	return masking.Rule{Kind: r.Kind, Prefix: r.Prefix, Suffix: r.Suffix, HideGroup: r.Hide, Regex: r.Regex}
}

// EngineInfo 说明某类数据库的字段层级与结果列定位方式（页面的说明条据此渲染）。
type EngineInfo struct {
	Label   string `json:"label"`
	Hier    string `json:"hier"`     // 库 › 架构.表 › 列
	Path    string `json:"path"`     // 库.架构.表.列
	Table   string `json:"table"`    // 表 / 集合
	TableN  string `json:"table_n"`  // 张表 / 个集合
	Field   string `json:"field"`    // 字段 / 列
	RowUnit string `json:"row_unit"` // 行 / 文档
	Source  string `json:"source"`   // 字段来源
	Locate  string `json:"locate"`   // exact 精确 / name 保守 / path 字段路径
	How     string `json:"how"`      // 结果列怎么定位
	Note    string `json:"note"`
}

// InstanceItem 是脱敏页的实例下拉项。
type InstanceItem struct {
	ID        uint        `json:"id"`
	Name      string      `json:"name"`
	DBType    string      `json:"db_type"`
	Label     string      `json:"label"`
	Supported bool        `json:"supported"`
	Why       string      `json:"why,omitempty"` // 不支持的原因
	Count     int         `json:"count"`         // 已配置（启用）的字段数
	Engine    *EngineInfo `json:"engine,omitempty"`
}

// CatalogColumn / CatalogTable / Catalog 是某个库的字段目录。
type CatalogColumn struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Comment string `json:"comment"`
}

type CatalogTable struct {
	Name    string          `json:"name"` // PostgreSQL / SQL Server 为 架构.表
	Comment string          `json:"comment"`
	View    bool            `json:"view"`
	Rows    int64           `json:"rows"` // -1 未知
	Columns []CatalogColumn `json:"columns"`
}

type Catalog struct {
	DB        string         `json:"db"`
	Tables    []CatalogTable `json:"tables"`
	Truncated bool           `json:"truncated"`
	Note      string         `json:"note,omitempty"`
}

// ConfigItem 是一条已配置的脱敏字段（新策略或旧配置）。
type ConfigItem struct {
	Source     string   `json:"source"` // policy / legacy
	ID         uint     `json:"id"`
	ScopeDB    string   `json:"scope_db"`
	ScopeTable string   `json:"scope_table"`
	Field      string   `json:"field"`
	Wildcard   bool     `json:"wildcard"` // 任意库表里的同名字段
	Rule       RuleSpec `json:"rule"`
	RuleText   string   `json:"rule_text"`
	Active     bool     `json:"active"`
	UpdatedBy  string   `json:"updated_by,omitempty"`
	UpdatedAt  string   `json:"updated_at,omitempty"`
	LegacyDesc string   `json:"legacy_desc,omitempty"`
}

type ConfigView struct {
	Items      []ConfigItem `json:"items"`
	PolicyOK   bool         `json:"policy_ok"`   // dbt_masking_policy 已建；否则只能查看旧配置
	MaskingOn  bool         `json:"masking_on"`  // 系统脱敏总开关 query.mask_enabled
	ExportOn   bool         `json:"export_on"`   // 导出工单是否脱敏
	QueryCheck bool         `json:"query_check"` // 脱敏失败是否阻断
}

// ToggleResult 是开启系统脱敏总开关的结果。
type ToggleResult struct {
	MaskingOn bool `json:"masking_on"`
	Changed   bool `json:"changed"` // false：本来就是开启的，没有写库
}

// Change 是一条待保存的变更。
type Change struct {
	Op         string    `json:"op"` // set / remove / enable / disable
	ScopeDB    string    `json:"scope_db"`
	ScopeTable string    `json:"scope_table"`
	Field      string    `json:"field"`
	Rule       *RuleSpec `json:"rule,omitempty"`
}

type SaveRequest struct {
	InstanceID uint     `json:"instance_id"`
	Changes    []Change `json:"changes"`
}

type SaveResult struct {
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Removed int `json:"removed"`
	Toggled int `json:"toggled"`
}
