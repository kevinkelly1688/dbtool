// @author: kevinkelly
// @license: Apache Licence
// @file: queryfav_test.go
// @time: 2026/10/28

package queryfav

import (
	"sort"
	"strings"
	"testing"
	"time"

	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// memRepo 是内存仓库：语义与 store 一致（owner 隔离、同一个人同一条 SQL 唯一、关键词都包含且不区分大小写）。
type memRepo struct {
	rows []*Row
	next uint
}

func (m *memRepo) List(owner string, kws []string, sortBy string, asc bool, offset, limit int) ([]Row, int64, error) {
	var all []Row
	for _, r := range m.rows {
		if r.Owner != owner {
			continue
		}
		ok := true
		for _, k := range kws {
			k = strings.ToLower(k)
			ok = ok && (strings.Contains(strings.ToLower(r.Remark), k) || strings.Contains(strings.ToLower(r.SQLText), k))
		}
		if ok {
			all = append(all, *r)
		}
	}
	key := func(r Row) time.Time {
		if sortBy == "created" || r.LastUsedAt == nil {
			return r.CreatedAt
		}
		return *r.LastUsedAt
	}
	sort.SliceStable(all, func(i, j int) bool {
		if asc {
			return key(all[i]).Before(key(all[j]))
		}
		return key(all[i]).After(key(all[j]))
	})
	total := int64(len(all))
	if offset > len(all) {
		offset = len(all)
	}
	all = all[offset:]
	if limit < len(all) {
		all = all[:limit]
	}
	return all, total, nil
}

func (m *memRepo) find(owner string, pred func(*Row) bool) *Row {
	for _, r := range m.rows {
		if r.Owner == owner && pred(r) {
			return r
		}
	}
	return nil
}

func (m *memRepo) Get(owner string, id uint) (*Row, error) {
	if r := m.find(owner, func(r *Row) bool { return r.ID == id }); r != nil {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}

func (m *memRepo) FindBySQL(owner, md5 string) (*Row, error) {
	if r := m.find(owner, func(r *Row) bool { return r.SQLMD5 == md5 }); r != nil {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}

func (m *memRepo) Count(owner string) (int64, error) {
	var n int64
	for _, r := range m.rows {
		if r.Owner == owner {
			n++
		}
	}
	return n, nil
}

func (m *memRepo) Insert(r *Row) error {
	m.next++
	r.ID = m.next
	cp := *r
	m.rows = append(m.rows, &cp)
	return nil
}

func (m *memRepo) Update(r *Row) error {
	if cur := m.find(r.Owner, func(x *Row) bool { return x.ID == r.ID }); cur != nil {
		cur.Remark, cur.SQLText, cur.SQLMD5, cur.Instance, cur.DBName, cur.UpdatedAt = r.Remark, r.SQLText, r.SQLMD5, r.Instance, r.DBName, r.UpdatedAt
	}
	return nil
}

func (m *memRepo) Touch(owner string, id uint, at time.Time) error {
	if cur := m.find(owner, func(x *Row) bool { return x.ID == id }); cur != nil {
		cur.LastUsedAt = &at
		cur.UseCount++
	}
	return nil
}

func (m *memRepo) Delete(owner string, id uint) (bool, error) {
	for i, r := range m.rows {
		if r.Owner == owner && r.ID == id {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Minute); return c.t }

func newSvc() (*Service, *memRepo) {
	r := &memRepo{}
	c := &clock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.Local)}
	return NewService(r, c.now), r
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	e, ok := err.(*Error)
	if !ok || e.Code != code {
		t.Fatalf("期望业务错误 %q，实际 %v", code, err)
	}
}

func TestAddListOwnerOnly(t *testing.T) {
	s, _ := newSvc()
	if _, _, err := s.Add("dev", SaveReq{Comment: "订单", SQL: "select * from orders limit 10", Instance: "prod", DB: "shop"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Add("dev", SaveReq{Comment: "用户", SQL: "select id from users"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Add("wang", SaveReq{Comment: "王工的", SQL: "select 1"}); err != nil {
		t.Fatal(err)
	}
	pg, err := s.List("dev", ListQuery{})
	if err != nil || pg.Total != 2 || len(pg.Items) != 2 || pg.Max != MaxPerUser {
		t.Fatalf("dev 只能看到自己的 2 条: %+v %v", pg, err)
	}
	for _, it := range pg.Items {
		if strings.Contains(it.Comment, "王工") {
			t.Errorf("不应看到别人的收藏: %+v", it)
		}
	}
	// 关键词：注释和 SQL 都能搜，多个词都要包含，不区分大小写
	if pg, _ = s.List("dev", ListQuery{Q: "ORDERS"}); pg.Total != 1 || pg.Items[0].Comment != "订单" {
		t.Errorf("按 SQL 搜: %+v", pg)
	}
	if pg, _ = s.List("dev", ListQuery{Q: "用户"}); pg.Total != 1 {
		t.Errorf("按注释搜: %+v", pg)
	}
	if pg, _ = s.List("dev", ListQuery{Q: "select users"}); pg.Total != 1 {
		t.Errorf("多个关键词都包含: %+v", pg)
	}
	if pg, _ = s.List("dev", ListQuery{Q: "select 王工"}); pg.Total != 0 {
		t.Errorf("没有同时包含: %+v", pg)
	}
}

func TestAddSameSQLUpdatesInsteadOfDuplicating(t *testing.T) {
	s, r := newSvc()
	a, existed, err := s.Add("dev", SaveReq{Comment: "第一次", SQL: "select 1"})
	if err != nil || existed {
		t.Fatal(a, existed, err)
	}
	b, existed, err := s.Add("dev", SaveReq{Comment: "改了注释", SQL: "  select 1  \n", Instance: "prod", DB: "shop"})
	if err != nil || !existed || b.ID != a.ID || b.Comment != "改了注释" || b.Instance != "prod" {
		t.Fatalf("同一条 SQL（忽略首尾空白）应更新原记录: %+v existed=%v %v", b, existed, err)
	}
	// 注释留空：保留原注释
	c, _, _ := s.Add("dev", SaveReq{SQL: "select 1"})
	if c.Comment != "改了注释" || len(r.rows) != 1 {
		t.Errorf("留空注释保留原值: %+v rows=%d", c, len(r.rows))
	}
	// 别人收藏同一条 SQL 是各自的一份
	if _, existed, _ := s.Add("wang", SaveReq{SQL: "select 1"}); existed || len(r.rows) != 2 {
		t.Errorf("不同的人各存一份: rows=%d", len(r.rows))
	}
}

func TestValidation(t *testing.T) {
	s, _ := newSvc()
	_, _, err := s.Add("dev", SaveReq{SQL: "   "})
	wantCode(t, err, "bad_request")
	_, _, err = s.Add("dev", SaveReq{SQL: strings.Repeat("a", 60001)})
	wantCode(t, err, "bad_request")
	_, _, err = s.Add("dev", SaveReq{SQL: "select 1", Comment: strings.Repeat("注", 101)})
	wantCode(t, err, "bad_request")
	it, _, err := s.Add("dev", SaveReq{SQL: "select 1", Comment: "多行\n注释  带空格"})
	if err != nil || it.Comment != "多行 注释 带空格" {
		t.Errorf("注释折成一行: %q %v", it.Comment, err)
	}
}

func TestLimitPerUser(t *testing.T) {
	s, r := newSvc()
	for i := 0; i < MaxPerUser; i++ {
		r.next++
		r.rows = append(r.rows, &Row{ID: r.next, Owner: "dev", SQLText: "s", SQLMD5: sqlKey(strings.Repeat("x", i+1))})
	}
	_, _, err := s.Add("dev", SaveReq{SQL: "select new"})
	wantCode(t, err, "limit")
	if _, _, err := s.Add("wang", SaveReq{SQL: "select new"}); err != nil {
		t.Errorf("别人不受影响: %v", err)
	}
}

func TestUpdateUseDeleteOwnerOnly(t *testing.T) {
	s, _ := newSvc()
	a, _, _ := s.Add("dev", SaveReq{Comment: "a", SQL: "select a"})
	b, _, _ := s.Add("dev", SaveReq{Comment: "b", SQL: "select b"})

	// 别人不能改 / 用 / 删
	_, err := s.Update("wang", a.ID, SaveReq{Comment: "x", SQL: "select x"})
	wantCode(t, err, "not_found")
	wantCode(t, s.Use("wang", a.ID), "not_found")
	wantCode(t, s.Delete("wang", a.ID), "not_found")

	// 自己改注释和 SQL
	u, err := s.Update("dev", a.ID, SaveReq{Comment: "新注释", SQL: "select a2"})
	if err != nil || u.Comment != "新注释" || u.SQL != "select a2" {
		t.Fatalf("修改: %+v %v", u, err)
	}
	// 改成已有的另一条 SQL：冲突
	_, err = s.Update("dev", a.ID, SaveReq{Comment: "撞了", SQL: "select b"})
	wantCode(t, err, "duplicate")
	// 不改 SQL 只改注释不算冲突
	if _, err := s.Update("dev", b.ID, SaveReq{Comment: "只改注释", SQL: "select b"}); err != nil {
		t.Errorf("同一条 SQL 只改注释: %v", err)
	}

	// 使用：最后使用时间、次数
	if err := s.Use("dev", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Use("dev", a.ID); err != nil {
		t.Fatal(err)
	}
	pg, _ := s.List("dev", ListQuery{})
	var got Item
	for _, it := range pg.Items {
		if it.ID == a.ID {
			got = it
		}
	}
	if got.UseCount != 2 || got.LastUsedAt == "" {
		t.Errorf("使用次数与最后使用时间: %+v", got)
	}
	// 默认按最后使用时间倒序：刚用过的在前
	if pg.Items[0].ID != a.ID {
		t.Errorf("最近使用的排在前面: %+v", pg.Items)
	}
	// created 排序 + 升序
	pg, _ = s.List("dev", ListQuery{Sort: "created", Asc: true})
	if pg.Items[0].ID != a.ID || pg.Items[1].ID != b.ID {
		t.Errorf("按添加时间升序: %+v", pg.Items)
	}

	if err := s.Delete("dev", a.ID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, s.Delete("dev", a.ID), "not_found")
	if pg, _ = s.List("dev", ListQuery{}); pg.Total != 1 {
		t.Errorf("删除后: %+v", pg)
	}
}

func TestListPaging(t *testing.T) {
	s, _ := newSvc()
	for i := 0; i < 25; i++ {
		if _, _, err := s.Add("dev", SaveReq{SQL: "select " + strings.Repeat("1", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	pg, _ := s.List("dev", ListQuery{})
	if pg.Total != 25 || len(pg.Items) != 20 {
		t.Errorf("默认每页 20: %d/%d", len(pg.Items), pg.Total)
	}
	pg, _ = s.List("dev", ListQuery{Offset: 20, Limit: 20})
	if len(pg.Items) != 5 {
		t.Errorf("第二页: %d", len(pg.Items))
	}
	pg, _ = s.List("dev", ListQuery{Limit: 100000})
	if len(pg.Items) != 25 {
		t.Errorf("每页上限 100: %d", len(pg.Items))
	}
}

// ---- HTTP：登录、权限、owner 隔离 ----

func newHTTP(t *testing.T) (*servertest.Env, *memRepo) {
	t.Helper()
	r := &memRepo{}
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) { register(s, rt, NewService(r, nil)) },
	}})
	// bob 是普通用户：授予在线查询菜单权限（query.view）
	for _, u := range env.Repo.Users {
		if u.Username == "bob" {
			env.Repo.Perms[u.ID] = map[string]bool{"query.view": true}
		}
	}
	return env, r
}

func TestHTTPFavorites(t *testing.T) {
	env, _ := newHTTP(t)
	bob, root := env.Login("bob"), env.Login("root")

	var added struct {
		Item    Item `json:"item"`
		Existed bool `json:"existed"`
	}
	bob.PostJSON("/api/v1/query/favorites", SaveReq{Comment: "鲍勃的", SQL: "select 1 from t", Instance: "prod", DB: "shop"}, &added)
	if added.Item.ID == 0 || added.Existed {
		t.Fatalf("新增: %+v", added)
	}
	var pg Page
	bob.GetJSON("/api/v1/query/favorites", &pg)
	if pg.Total != 1 || pg.Items[0].Comment != "鲍勃的" {
		t.Fatalf("自己的列表: %+v", pg)
	}
	// 超级用户也只能看到自己的收藏，看不到 bob 的
	root.GetJSON("/api/v1/query/favorites", &pg)
	if pg.Total != 0 {
		t.Errorf("超级用户看不到别人的收藏: %+v", pg)
	}
	// 超级用户不能改 / 删 / 用 bob 的
	for _, c := range []struct{ method, path string }{
		{"PUT", "/api/v1/query/favorites/1"}, {"POST", "/api/v1/query/favorites/1/use"}, {"DELETE", "/api/v1/query/favorites/1"},
	} {
		if code, _ := root.JSON(c.method, c.path, SaveReq{SQL: "select 2"}, nil); code != 404 {
			t.Errorf("%s %s 对别人的收藏应 404: %d", c.method, c.path, code)
		}
	}
	// 自己：用、改、删
	if code, msg := bob.JSON("POST", "/api/v1/query/favorites/1/use", nil, nil); code != 200 {
		t.Errorf("use: %d %s", code, msg)
	}
	var upd Item
	if code, msg := bob.JSON("PUT", "/api/v1/query/favorites/1", SaveReq{Comment: "改", SQL: "select 2"}, &upd); code != 200 || upd.Comment != "改" {
		t.Errorf("update: %d %s %+v", code, msg, upd)
	}
	if code, _ := bob.JSON("DELETE", "/api/v1/query/favorites/1", nil, nil); code != 200 {
		t.Errorf("delete: %d", code)
	}
	// 参数校验
	if code, _ := bob.JSON("POST", "/api/v1/query/favorites", SaveReq{SQL: " "}, nil); code != 400 {
		t.Errorf("空 SQL: %d", code)
	}
	if code, _ := bob.JSON("DELETE", "/api/v1/query/favorites/abc", nil, nil); code != 400 {
		t.Errorf("非法 id: %d", code)
	}
	// 未登录
	if w := env.Anonymous("GET", "/api/v1/query/favorites"); w.Code != 401 && w.Code != 302 {
		t.Errorf("未登录: %d", w.Code)
	}
}

func TestHTTPNeedsMenuQuery(t *testing.T) {
	r := &memRepo{}
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) { register(s, rt, NewService(r, nil)) },
	}})
	_ = model.User{}
	// bob 没有 query.view：不能用收藏接口
	bob := env.Login("bob")
	if code, _ := bob.JSON("GET", "/api/v1/query/favorites", nil, nil); code != 403 {
		t.Errorf("没有在线查询权限不能用收藏: %d", code)
	}
}
