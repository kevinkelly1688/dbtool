// @author: kevinkelly
// @license: Apache Licence
// @file: history_test.go
// @time: 2026/10/28

package query

import (
	"strings"
	"testing"
	"time"

	"dbtool/internal/model"
)

// histRepo 在 fakeRepo 上加上内存版的历史查询（语义同 store：用户精确、实例精确、关键词都包含且不区分大小写、新的在前）。
type histRepo struct {
	*fakeRepo
	last QueryLogFilter
}

func (r *histRepo) ListQueryLogs(f QueryLogFilter) ([]model.QueryLog, int64, error) {
	r.last = f
	var all []model.QueryLog
	for i := len(r.logs) - 1; i >= 0; i-- {
		l := r.logs[i]
		if f.Username != "" && l.Username != f.Username || f.Instance != "" && l.InstanceName != f.Instance {
			continue
		}
		if f.UserLike != "" && !strings.Contains(strings.ToLower(l.Username), strings.ToLower(f.UserLike)) &&
			!strings.Contains(strings.ToLower(l.UserDisplay), strings.ToLower(f.UserLike)) {
			continue
		}
		if f.Start != nil && l.CreateTime.Before(*f.Start) || f.End != nil && !l.CreateTime.Before(*f.End) {
			continue
		}
		hit := true
		for _, k := range f.Keywords {
			hit = hit && strings.Contains(strings.ToLower(l.SQLLog), strings.ToLower(k))
		}
		if hit {
			all = append(all, *l)
		}
	}
	total := int64(len(all))
	if f.Offset > len(all) {
		f.Offset = len(all)
	}
	all = all[f.Offset:]
	if f.Limit < len(all) {
		all = all[:f.Limit]
	}
	return all, total, nil
}

func (r *histRepo) QueryLogUsers() ([]model.QueryLogUser, error) {
	cnt := map[string]*model.QueryLogUser{}
	var order []string
	for _, l := range r.logs {
		u := cnt[l.Username]
		if u == nil {
			u = &model.QueryLogUser{Username: l.Username, UserDisplay: l.UserDisplay}
			cnt[l.Username] = u
			order = append(order, l.Username)
		}
		u.Count++
	}
	var out []model.QueryLogUser
	for _, n := range order {
		out = append(out, *cnt[n])
	}
	return out, nil
}

func seedHistory() (*Service, *histRepo) {
	base := newRepo()
	day := func(d string) time.Time { t, _ := time.ParseInLocation("2006-01-02 15:04:05", d, time.Local); return t }
	add := func(user, display, inst, db, sql, at string) {
		base.logs = append(base.logs, &model.QueryLog{ID: uint(len(base.logs) + 1), Username: user, UserDisplay: display, InstanceName: inst, DBName: db, SQLLog: sql, EffectRow: 3, CostTime: "0.012", CreateTime: day(at)})
	}
	add("dev", "开发者", "prod-mysql-01", "shop", "select id from orders limit 10", "2026-10-01 10:00:00")
	add("dev", "开发者", "prod-mysql-01", "shop", "select * from Users where name like '50%_off' limit 10", "2026-10-02 11:00:00")
	add("wang", "王工", "prod-mysql-01", "report", "select city, count(*) from users group by city limit 100", "2026-10-03 12:00:00")
	add("dev", "开发者", "test-mysql-01", "shop", "select id, status from orders where status = 'paid' limit 5", "2026-10-04 13:00:00")
	r := &histRepo{fakeRepo: base}
	return NewService(r, nil, nil), r
}

func hist(t *testing.T, s *Service, p Principal, q HistoryQuery) *HistoryPage {
	t.Helper()
	pg, err := s.History(p, q)
	if err != nil {
		t.Fatal(err)
	}
	return pg
}

func ids(pg *HistoryPage) string {
	var out []string
	for _, it := range pg.Items {
		out = append(out, string(rune('0'+it.ID)))
	}
	return strings.Join(out, "")
}

func TestHistoryOwnOnlyAndFuzzy(t *testing.T) {
	s, _ := seedHistory()
	dev, wang := Principal{ID: 1, Username: "dev", Display: "开发者"}, Principal{ID: 2, Username: "wang"}

	pg := hist(t, s, dev, HistoryQuery{})
	if ids(pg) != "421" || pg.Total != 3 || pg.Admin || pg.Scope != "我执行过的 SQL" {
		t.Errorf("只能看到自己的，新的在前: %s %+v", ids(pg), pg)
	}
	// 普通用户传别人的用户名也没用
	if pg = hist(t, s, dev, HistoryQuery{User: "wang"}); ids(pg) != "421" {
		t.Errorf("普通用户的 user 参数应被忽略: %s", ids(pg))
	}
	if pg = hist(t, s, wang, HistoryQuery{}); ids(pg) != "3" {
		t.Errorf("wang 只看自己: %s", ids(pg))
	}
	// 模糊：不区分大小写；多个关键词都要包含
	if pg = hist(t, s, dev, HistoryQuery{Search: "ORDERS"}); ids(pg) != "41" {
		t.Errorf("模糊搜索: %s", ids(pg))
	}
	if pg = hist(t, s, dev, HistoryQuery{Search: "orders  paid"}); ids(pg) != "4" {
		t.Errorf("多关键词是“且”: %s", ids(pg))
	}
	if pg = hist(t, s, dev, HistoryQuery{Search: "nothing-here"}); len(pg.Items) != 0 || pg.Total != 0 {
		t.Errorf("无匹配: %+v", pg)
	}
	// 实例与日期（结束日含当天）
	if pg = hist(t, s, dev, HistoryQuery{Instance: "test-mysql-01"}); ids(pg) != "4" {
		t.Errorf("按实例: %s", ids(pg))
	}
	if pg = hist(t, s, dev, HistoryQuery{Start: "2026-10-02", End: "2026-10-02"}); ids(pg) != "2" {
		t.Errorf("按日期（含结束日）: %s", ids(pg))
	}
	// 分页与上限
	if pg = hist(t, s, dev, HistoryQuery{Limit: 2}); ids(pg) != "42" || pg.Total != 3 {
		t.Errorf("分页第 1 页: %s", ids(pg))
	}
	if pg = hist(t, s, dev, HistoryQuery{Limit: 2, Offset: 2}); ids(pg) != "1" {
		t.Errorf("分页第 2 页: %s", ids(pg))
	}
	if _, err := s.History(dev, HistoryQuery{Start: "10/01"}); code(err) != "bad_request" {
		t.Errorf("日期格式错误: %v", err)
	}
	it := hist(t, s, dev, HistoryQuery{Limit: 1}).Items[0]
	if it.Time != "2026-10-04 13:00:00" || it.Instance != "test-mysql-01" || it.DB != "shop" || it.Rows != 3 || it.CostSec != "0.012" || it.Display != "开发者" {
		t.Errorf("记录内容: %+v", it)
	}
}

func TestHistoryAdminFilterByUser(t *testing.T) {
	s, r := seedHistory()
	admin := Principal{ID: 100, Username: "root", IsSuperuser: true}
	pg := hist(t, s, admin, HistoryQuery{})
	if ids(pg) != "4321" || !pg.Admin || pg.Scope != "所有人执行过的 SQL" {
		t.Errorf("管理员默认看所有人: %s %+v", ids(pg), pg)
	}
	if pg = hist(t, s, admin, HistoryQuery{User: "wang"}); ids(pg) != "3" || pg.Scope != "用户名或姓名含“wang”的人执行过的 SQL" {
		t.Errorf("按用户过滤: %s %s", ids(pg), pg.Scope)
	}
	if pg = hist(t, s, admin, HistoryQuery{User: "dev", Search: "users"}); ids(pg) != "2" {
		t.Errorf("用户 + 关键词: %s", ids(pg))
	}
	// 关键词里的 % _ 不是通配符，应原样传给存储层（由存储层转义）
	hist(t, s, admin, HistoryQuery{Search: "50%_off"})
	if len(r.last.Keywords) != 1 || r.last.Keywords[0] != "50%_off" {
		t.Errorf("关键词原样传递: %v", r.last.Keywords)
	}
	us, err := s.HistoryUsers(admin)
	if err != nil || len(us) != 2 || us[0].Username != "dev" || us[0].Count != 3 || us[1].Username != "wang" || us[1].Display != "王工" {
		t.Errorf("用户下拉: %+v %v", us, err)
	}
	if _, err := s.HistoryUsers(Principal{ID: 1, Username: "dev"}); code(err) != "forbidden" {
		t.Errorf("普通用户不能取用户列表: %v", err)
	}
	// 限制：单页最多 100
	hist(t, s, admin, HistoryQuery{Limit: 100000})
	if r.last.Limit != 100 {
		t.Errorf("单页上限: %d", r.last.Limit)
	}
	// 关键词上限 8 个
	if n := len(Keywords("a b c d e f g h i j")); n != 8 {
		t.Errorf("关键词上限: %d", n)
	}
}

func TestHistoryUnsupportedRepo(t *testing.T) {
	s := NewService(newRepo(), nil, nil) // 没有实现 HistoryRepo
	if _, err := s.History(Principal{Username: "dev"}, HistoryQuery{}); code(err) != "unsupported" {
		t.Errorf("没有实现时应明确报错: %v", err)
	}
}

// 超级用户按用户过滤是模糊匹配（用户名或姓名包含，不区分大小写），可与语句关键词叠加；传给仓库的是 UserLike，不是精确用户名。
func TestHistoryAdminFuzzyUser(t *testing.T) {
	s, r := seedHistory()
	root := Principal{ID: 9, Username: "root", IsSuperuser: true}

	if pg := hist(t, s, root, HistoryQuery{}); ids(pg) != "4321" || !pg.Admin {
		t.Fatalf("管理员不过滤看到所有人: %s", ids(pg))
	}
	if pg := hist(t, s, root, HistoryQuery{User: "ang"}); ids(pg) != "3" { // wang 的一部分
		t.Errorf("用户名的一部分: %s", ids(pg))
	}
	if pg := hist(t, s, root, HistoryQuery{User: "WANG"}); ids(pg) != "3" {
		t.Errorf("不区分大小写: %s", ids(pg))
	}
	if pg := hist(t, s, root, HistoryQuery{User: "开发"}); ids(pg) != "421" { // 姓名“开发者”的一部分
		t.Errorf("姓名的一部分: %s", ids(pg))
	}
	pg := hist(t, s, root, HistoryQuery{User: "dev", Search: "orders"})
	if ids(pg) != "41" {
		t.Errorf("用户 + 语句叠加: %s", ids(pg))
	}
	if r.last.UserLike != "dev" || r.last.Username != "" {
		t.Errorf("管理员的 user 应作为模糊条件传给仓库: %+v", r.last)
	}
	if !strings.Contains(pg.Scope, "dev") {
		t.Errorf("范围说明应带上过滤的用户: %q", pg.Scope)
	}
	if pg = hist(t, s, root, HistoryQuery{User: "nobody"}); len(pg.Items) != 0 || pg.Total != 0 {
		t.Errorf("没有匹配的用户: %+v", pg)
	}
	// 普通用户的 user 参数仍然被忽略，且不会变成模糊过滤
	dev := Principal{ID: 1, Username: "dev", Display: "开发者"}
	if pg = hist(t, s, dev, HistoryQuery{User: "wang"}); ids(pg) != "421" || r.last.UserLike != "" || r.last.Username != "dev" {
		t.Errorf("普通用户只能看自己的: %s %+v", ids(pg), r.last)
	}
}
