// @author: kevinkelly
// @license: Apache Licence
// @file: types.go
// @time: 2026/10/28

// Package queryfav 实现在线查询页的“收藏”：每个人收藏自己常用的 SQL，只能看到和管理自己的。
// 表见 deploy/sql/250_query_favorite.sql。执行历史不在这里（沿用 dbt_query_log，见 internal/query/history.go）。
package queryfav

import "time"

// Row 是 dbt_query_favorite 的一行。
type Row struct {
	ID         uint       `gorm:"primaryKey"`
	Owner      string     `gorm:"column:owner"`
	Remark     string     `gorm:"column:remark"`
	SQLText    string     `gorm:"column:sql_text"`
	SQLMD5     string     `gorm:"column:sql_md5"`
	Instance   string     `gorm:"column:instance_name"`
	DBName     string     `gorm:"column:db_name"`
	UseCount   int        `gorm:"column:use_count"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at"`
	LastUsedAt *time.Time `gorm:"column:last_used_at"`
}

// TableName 返回表名。
func (Row) TableName() string { return "dbt_query_favorite" }

// Item 是返回给页面的一条收藏。
type Item struct {
	ID         uint   `json:"id"`
	Comment    string `json:"comment"`
	SQL        string `json:"sql"`
	Instance   string `json:"instance"`
	DB         string `json:"db"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at"` // 从未使用为空
	UseCount   int    `json:"use_count"`
}

// Page 是一页收藏。
type Page struct {
	Items []Item `json:"items"`
	Total int64  `json:"total"`
	Max   int    `json:"max"` // 每人最多收藏条数
}

// SaveReq 是新增 / 修改的请求体。
type SaveReq struct {
	Comment  string `json:"comment"`
	SQL      string `json:"sql"`
	Instance string `json:"instance"`
	DB       string `json:"db"`
}

// ListQuery 是列表筛选条件。
type ListQuery struct {
	Q      string // 空白分隔多个关键词，全部包含才算命中（对注释和 SQL 都做不区分大小写的模糊匹配）
	Sort   string // used（默认：最后使用时间，没用过的按添加时间）/ created
	Asc    bool
	Offset int
	Limit  int
}

// Repo 是收藏的数据访问；测试里用内存实现。
type Repo interface {
	List(owner string, keywords []string, sort string, asc bool, offset, limit int) ([]Row, int64, error)
	Get(owner string, id uint) (*Row, error)
	FindBySQL(owner, md5 string) (*Row, error)
	Count(owner string) (int64, error)
	Insert(r *Row) error
	Update(r *Row) error
	Touch(owner string, id uint, at time.Time) error
	Delete(owner string, id uint) (bool, error)
}

// Error 是业务错误。
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}
