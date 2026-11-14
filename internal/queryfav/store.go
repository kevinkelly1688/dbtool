// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package queryfav

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// errNoTable 表示还没执行 250_query_favorite.sql。
var errNoTable = newErr(503, "table_missing", "收藏功能还没有启用：请先在元数据库执行 deploy/sql/250_query_favorite.sql（升级时 install.sh 会自动执行），然后刷新页面")

type store struct{ db *gorm.DB }

func newStore(db *gorm.DB) *store { return &store{db: db} }

// wrap 把“表不存在”转成可读的业务错误。
func wrap(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "1146") || strings.Contains(strings.ToLower(msg), "doesn't exist") || strings.Contains(strings.ToLower(msg), "no such table") {
		return errNoTable
	}
	return err
}

// likeEscape 转义 LIKE 通配符（配合 ESCAPE '!'）。
func likeEscape(s string) string {
	r := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return r.Replace(s)
}

func (s *store) List(owner string, keywords []string, sort string, asc bool, offset, limit int) ([]Row, int64, error) {
	q := s.db.Model(&Row{}).Where("owner = ?", owner)
	for _, k := range keywords {
		p := "%" + likeEscape(k) + "%"
		q = q.Where("(remark LIKE ? ESCAPE '!' OR sql_text LIKE ? ESCAPE '!')", p, p)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, wrap(err)
	}
	col := "COALESCE(last_used_at, created_at)"
	if sort == "created" {
		col = "created_at"
	}
	dir := "DESC"
	if asc {
		dir = "ASC"
	}
	var rows []Row
	err := q.Order(col + " " + dir + ", id " + dir).Offset(offset).Limit(limit).Find(&rows).Error
	return rows, total, wrap(err)
}

func (s *store) Get(owner string, id uint) (*Row, error) {
	var r Row
	err := s.db.Where("owner = ? AND id = ?", owner, id).Limit(1).Find(&r).Error
	if err != nil {
		return nil, wrap(err)
	}
	if r.ID == 0 {
		return nil, nil
	}
	return &r, nil
}

func (s *store) FindBySQL(owner, md5 string) (*Row, error) {
	var r Row
	err := s.db.Where("owner = ? AND sql_md5 = ?", owner, md5).Limit(1).Find(&r).Error
	if err != nil {
		return nil, wrap(err)
	}
	if r.ID == 0 {
		return nil, nil
	}
	return &r, nil
}

func (s *store) Count(owner string) (int64, error) {
	var n int64
	err := s.db.Model(&Row{}).Where("owner = ?", owner).Count(&n).Error
	return n, wrap(err)
}

func (s *store) Insert(r *Row) error {
	err := s.db.Create(r).Error
	if err != nil && (strings.Contains(err.Error(), "1062") || strings.Contains(strings.ToLower(err.Error()), "unique")) {
		return newErr(409, "duplicate", "已经收藏过这条 SQL")
	}
	return wrap(err)
}

func (s *store) Update(r *Row) error {
	err := s.db.Model(&Row{}).Where("owner = ? AND id = ?", r.Owner, r.ID).Updates(map[string]any{
		"remark": r.Remark, "sql_text": r.SQLText, "sql_md5": r.SQLMD5, "instance_name": r.Instance, "db_name": r.DBName, "updated_at": r.UpdatedAt,
	}).Error
	if err != nil && (strings.Contains(err.Error(), "1062") || strings.Contains(strings.ToLower(err.Error()), "unique")) {
		return newErr(409, "duplicate", "已经收藏过这条 SQL")
	}
	return wrap(err)
}

func (s *store) Touch(owner string, id uint, at time.Time) error {
	err := s.db.Model(&Row{}).Where("owner = ? AND id = ?", owner, id).
		Updates(map[string]any{"last_used_at": at, "use_count": gorm.Expr("use_count + 1")}).Error
	return wrap(err)
}

func (s *store) Delete(owner string, id uint) (bool, error) {
	res := s.db.Where("owner = ? AND id = ?", owner, id).Delete(&Row{})
	if res.Error != nil {
		return false, wrap(res.Error)
	}
	return res.RowsAffected > 0, nil
}

var _ Repo = (*store)(nil)

// IsTableMissing 判断 err 是否是“收藏表还没建”。
func IsTableMissing(err error) bool { return errors.Is(err, errNoTable) }
