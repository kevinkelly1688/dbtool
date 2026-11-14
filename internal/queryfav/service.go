// @author: kevinkelly
// @license: Apache Licence
// @file: service.go
// @time: 2026/10/28

package queryfav

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxPerUser 是每人最多收藏的条数。
	MaxPerUser     = 500
	maxCommentLen  = 100   // 字符
	maxSQLBytes    = 60000 // 字节（列是 TEXT，64KB）
	defaultLimit   = 20
	maxLimit       = 100
	maxKeywords    = 8
	maxInstanceLen = 100
	maxDBLen       = 128
)

// Service 是收藏的业务逻辑：所有操作都限定在 owner（当前登录用户）自己的记录上，没有“看别人收藏”的入口。
type Service struct {
	repo Repo
	now  func() time.Time
}

// NewService 创建服务；now 为空取系统时间。
func NewService(repo Repo, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, now: now}
}

func sqlKey(sql string) string {
	sum := md5.Sum([]byte(strings.TrimSpace(sql)))
	return hex.EncodeToString(sum[:])
}

func toItem(r Row) Item {
	it := Item{ID: r.ID, Comment: r.Remark, SQL: r.SQLText, Instance: r.Instance, DB: r.DBName, UseCount: r.UseCount,
		CreatedAt: r.CreatedAt.Format("2006-01-02 15:04:05")}
	if r.LastUsedAt != nil {
		it.LastUsedAt = r.LastUsedAt.Format("2006-01-02 15:04:05")
	}
	return it
}

// clean 校验并整理请求体。
func clean(in SaveReq) (SaveReq, *Error) {
	out := SaveReq{Comment: strings.TrimSpace(in.Comment), SQL: strings.TrimSpace(in.SQL), Instance: strings.TrimSpace(in.Instance), DB: strings.TrimSpace(in.DB)}
	if out.SQL == "" {
		return out, newErr(400, "bad_request", "SQL 不能为空")
	}
	if len(out.SQL) > maxSQLBytes {
		return out, newErr(400, "bad_request", "SQL 太长（最多 60000 字节），请只收藏需要的那一条语句")
	}
	if utf8.RuneCountInString(out.Comment) > maxCommentLen {
		return out, newErr(400, "bad_request", "注释最长 100 个字符")
	}
	if strings.ContainsAny(out.Comment, "\r\n") {
		out.Comment = strings.Join(strings.Fields(out.Comment), " ") // 注释是一行文字
	}
	if utf8.RuneCountInString(out.Instance) > maxInstanceLen || utf8.RuneCountInString(out.DB) > maxDBLen {
		return out, newErr(400, "bad_request", "实例名或库名过长")
	}
	return out, nil
}

// List 返回 owner 自己的一页收藏。
func (s *Service) List(owner string, q ListQuery) (*Page, error) {
	if q.Limit <= 0 {
		q.Limit = defaultLimit
	}
	if q.Limit > maxLimit {
		q.Limit = maxLimit
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	kw := strings.Fields(q.Q)
	if len(kw) > maxKeywords {
		kw = kw[:maxKeywords]
	}
	sort := q.Sort
	if sort != "created" {
		sort = "used"
	}
	rows, total, err := s.repo.List(owner, kw, sort, q.Asc, q.Offset, q.Limit)
	if err != nil {
		return nil, err
	}
	pg := &Page{Items: make([]Item, 0, len(rows)), Total: total, Max: MaxPerUser}
	for _, r := range rows {
		pg.Items = append(pg.Items, toItem(r))
	}
	return pg, nil
}

// Add 新增一条收藏；同一个人收藏过同一条 SQL 时更新那条的注释 / 实例 / 库（注释留空则保留原注释），existed 为 true。
func (s *Service) Add(owner string, in SaveReq) (it Item, existed bool, err error) {
	req, e := clean(in)
	if e != nil {
		return Item{}, false, e
	}
	md5 := sqlKey(req.SQL)
	if cur, err := s.repo.FindBySQL(owner, md5); err != nil {
		return Item{}, false, err
	} else if cur != nil {
		if req.Comment != "" {
			cur.Remark = req.Comment
		}
		if req.Instance != "" {
			cur.Instance, cur.DBName = req.Instance, req.DB
		}
		cur.UpdatedAt = s.now()
		if err := s.repo.Update(cur); err != nil {
			return Item{}, false, err
		}
		return toItem(*cur), true, nil
	}
	n, err := s.repo.Count(owner)
	if err != nil {
		return Item{}, false, err
	}
	if n >= MaxPerUser {
		return Item{}, false, newErr(409, "limit", "收藏已满（每人最多 500 条），请先删掉不用的")
	}
	now := s.now()
	r := &Row{Owner: owner, Remark: req.Comment, SQLText: req.SQL, SQLMD5: md5, Instance: req.Instance, DBName: req.DB, CreatedAt: now, UpdatedAt: now}
	if err := s.repo.Insert(r); err != nil {
		return Item{}, false, err
	}
	return toItem(*r), false, nil
}

// Update 修改自己的一条收藏的注释和 SQL。
func (s *Service) Update(owner string, id uint, in SaveReq) (Item, error) {
	req, e := clean(in)
	if e != nil {
		return Item{}, e
	}
	cur, err := s.repo.Get(owner, id)
	if err != nil {
		return Item{}, err
	}
	if cur == nil {
		return Item{}, newErr(404, "not_found", "收藏不存在")
	}
	md5 := sqlKey(req.SQL)
	if md5 != cur.SQLMD5 {
		if other, err := s.repo.FindBySQL(owner, md5); err != nil {
			return Item{}, err
		} else if other != nil {
			return Item{}, newErr(409, "duplicate", fmt.Sprintf("已经收藏过这条 SQL（注释：%s）", other.Remark))
		}
	}
	cur.Remark, cur.SQLText, cur.SQLMD5, cur.UpdatedAt = req.Comment, req.SQL, md5, s.now()
	if req.Instance != "" {
		cur.Instance, cur.DBName = req.Instance, req.DB
	}
	if err := s.repo.Update(cur); err != nil {
		return Item{}, err
	}
	return toItem(*cur), nil
}

// Use 记一次“使用”（粘贴到编辑器），更新最后使用时间和次数。
func (s *Service) Use(owner string, id uint) error {
	cur, err := s.repo.Get(owner, id)
	if err != nil {
		return err
	}
	if cur == nil {
		return newErr(404, "not_found", "收藏不存在")
	}
	return s.repo.Touch(owner, id, s.now())
}

// Delete 删除自己的一条收藏。
func (s *Service) Delete(owner string, id uint) error {
	ok, err := s.repo.Delete(owner, id)
	if err != nil {
		return err
	}
	if !ok {
		return newErr(404, "not_found", "收藏不存在")
	}
	return nil
}
