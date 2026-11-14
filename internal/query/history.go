// @author: kevinkelly
// @license: Apache Licence
// @file: history.go
// @time: 2026/10/28

package query

import (
	"fmt"
	"strings"
	"time"

	"dbtool/internal/model"
)

// 在线查询的 SQL 执行历史：数据就是已有的审计表 dbt_query_log（每次成功执行都会写一行），不新增表。
// 普通用户只能看自己执行的；管理员（超级用户）可以按用户过滤，看到所有人的“记录 + SQL 语句”。

// QueryLogFilter 是 dbt_query_log 的查询条件。
type QueryLogFilter struct {
	Username string // 精确匹配；普通用户固定为自己
	UserLike string // 管理员的模糊过滤：用户名或姓名包含该文本（不区分大小写）；与 Username 同时给出时都要满足
	Keywords []string
	Instance string // 实例名，空表示不限
	Start    *time.Time
	End      *time.Time // 不含
	Offset   int
	Limit    int
}

// HistoryRepo 是历史查询需要的数据访问；由 store 实现。没实现时历史功能返回 501。
type HistoryRepo interface {
	ListQueryLogs(f QueryLogFilter) ([]model.QueryLog, int64, error)
	QueryLogUsers() ([]model.QueryLogUser, error)
}

// HistoryQuery 是页面传来的筛选条件。
type HistoryQuery struct {
	Search   string // 模糊匹配 SQL；空白分隔多个关键词，全部包含才算命中
	User     string // 仅管理员有效：模糊匹配用户名或姓名
	Instance string
	Start    string // YYYY-MM-DD
	End      string // YYYY-MM-DD，含当天
	Offset   int
	Limit    int
	Status   string // 空=全部；ok / blocked / failed / timeout / canceled（旁路表可用时才有非成功记录）
}

// HistoryItem 是一条历史记录。
type HistoryItem struct {
	ID       uint   `json:"id"`
	Time     string `json:"time"`
	Username string `json:"username"`
	Display  string `json:"display"`
	Instance string `json:"instance"`
	DB       string `json:"db"`
	SQL      string `json:"sql"`
	Rows     int64  `json:"rows"`
	CostSec  string `json:"cost_sec"`
	Status   string `json:"status"`           // ok / blocked / failed / timeout / canceled
	Reason   string `json:"reason,omitempty"` // 非成功记录的原因
	Code     string `json:"code,omitempty"`
}

// HistoryPage 是一页历史。
type HistoryPage struct {
	Items  []HistoryItem `json:"items"`
	Total  int64         `json:"total"`
	Admin  bool          `json:"admin"`  // 当前用户能否看所有人的历史、按用户过滤
	Scope  string        `json:"scope"`  // 展示给用户的范围说明
	Events bool          `json:"events"` // 是否同时记录并展示被拦截 / 失败 / 超时 / 已取消的语句
}

// HistoryUser 是管理员过滤下拉里的一项。
type HistoryUser struct {
	Username string `json:"username"`
	Display  string `json:"display"`
	Count    int64  `json:"count"`
}

const (
	defaultHistoryLimit = 20
	maxHistoryLimit     = 100
	maxKeywords         = 8
)

// Keywords 把搜索框的内容拆成关键词（空白分隔，最多 8 个）。
func Keywords(s string) []string {
	f := strings.Fields(s)
	if len(f) > maxKeywords {
		f = f[:maxKeywords]
	}
	return f
}

func parseDay(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return nil, newErr(400, "bad_request", "日期格式应为 YYYY-MM-DD")
	}
	return &t, nil
}

func (s *Service) historyRepo() (HistoryRepo, error) {
	h, ok := s.repo.(HistoryRepo)
	if !ok {
		return nil, newErr(501, "unsupported", "当前部署不支持查看执行历史")
	}
	return h, nil
}

// History 返回一页执行历史。普通用户永远只看自己的（忽略 User 参数）；管理员可按用户过滤，不填则看所有人的。
func (s *Service) History(p Principal, q HistoryQuery) (*HistoryPage, error) {
	h, err := s.historyRepo()
	if err != nil {
		return nil, err
	}
	f := QueryLogFilter{Keywords: Keywords(q.Search), Instance: strings.TrimSpace(q.Instance), Offset: q.Offset, Limit: q.Limit}
	if f.Offset < 0 {
		f.Offset = 0
	}
	if f.Limit <= 0 {
		f.Limit = defaultHistoryLimit
	}
	if f.Limit > maxHistoryLimit {
		f.Limit = maxHistoryLimit
	}
	scope := "我执行过的 SQL"
	if p.IsSuperuser {
		f.UserLike = strings.TrimSpace(q.User)
		scope = "所有人执行过的 SQL"
		if f.UserLike != "" {
			scope = fmt.Sprintf("用户名或姓名含“%s”的人执行过的 SQL", f.UserLike)
		}
	} else {
		f.Username = p.Username
	}
	if f.Start, err = parseDay(q.Start); err != nil {
		return nil, err
	}
	end, err := parseDay(q.End)
	if err != nil {
		return nil, err
	}
	if end != nil {
		e := end.AddDate(0, 0, 1)
		f.End = &e
	}
	status := strings.TrimSpace(q.Status)
	switch status {
	case "", StatusOK, StatusBlocked, StatusFailed, StatusTimeout, StatusCanceled:
	default:
		return nil, newErr(400, "bad_request", "状态筛选取值不合法")
	}
	if er, ok := s.repo.(EventRepo); ok && er.EventsReady() {
		rows, total, err := er.ListHistory(f, status)
		if err != nil {
			return nil, err
		}
		pg := &HistoryPage{Items: make([]HistoryItem, 0, len(rows)), Total: total, Admin: p.IsSuperuser, Scope: scope, Events: true}
		for _, r := range rows {
			pg.Items = append(pg.Items, HistoryItem{ID: r.ID, Time: r.Time.Format("2006-01-02 15:04:05"), Username: r.Username, Display: r.Display,
				Instance: r.Instance, DB: r.DB, SQL: r.SQL, Rows: r.Rows, CostSec: r.CostSec, Status: r.Status, Reason: r.Reason, Code: r.Code})
		}
		return pg, nil
	}
	if status != "" && status != StatusOK {
		// 没有旁路表：只有成功记录，其余状态一定为空
		return &HistoryPage{Items: []HistoryItem{}, Admin: p.IsSuperuser, Scope: scope}, nil
	}
	rows, total, err := h.ListQueryLogs(f)
	if err != nil {
		return nil, err
	}
	pg := &HistoryPage{Items: make([]HistoryItem, 0, len(rows)), Total: total, Admin: p.IsSuperuser, Scope: scope}
	for _, r := range rows {
		pg.Items = append(pg.Items, HistoryItem{ID: r.ID, Time: r.CreateTime.Format("2006-01-02 15:04:05"), Username: r.Username, Display: r.UserDisplay,
			Instance: r.InstanceName, DB: r.DBName, SQL: r.SQLLog, Rows: r.EffectRow, CostSec: r.CostTime, Status: StatusOK})
	}
	return pg, nil
}

// HistoryUsers 返回管理员过滤下拉的用户列表（执行过查询的用户及次数）。非管理员报 403。
func (s *Service) HistoryUsers(p Principal) ([]HistoryUser, error) {
	if !p.IsSuperuser {
		return nil, newErr(403, "forbidden", "只有管理员可以查看其他用户的执行历史")
	}
	h, err := s.historyRepo()
	if err != nil {
		return nil, err
	}
	us, err := h.QueryLogUsers()
	if err != nil {
		return nil, err
	}
	out := make([]HistoryUser, 0, len(us))
	for _, u := range us {
		out = append(out, HistoryUser{Username: u.Username, Display: u.UserDisplay, Count: u.Count})
	}
	return out, nil
}
