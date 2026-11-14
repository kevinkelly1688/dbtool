// @author: kevinkelly
// @license: Apache Licence
// @file: redis_test.go
// @time: 2026/10/28

package slowlog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine"
	"dbtool/internal/engine/redis"
	"dbtool/internal/engine/redis/redistest"
	"dbtool/internal/model"
	"dbtool/internal/server"
	"dbtool/internal/server/servertest"
)

// ---- 假仓库 ----

type memRedisRepo struct {
	mu        sync.Mutex
	ready     bool
	instances []model.Instance
	rows      map[uint][]RedisSlowRow
	states    map[uint]RedisState
	saveErr   error

	lastReview, lastHistory, lastTrend, lastCommands *RedisQuery
	reviewRows                                       []RedisReviewRow
	purged                                           int
}

func newMemRedisRepo() *memRedisRepo {
	return &memRedisRepo{ready: true, rows: map[uint][]RedisSlowRow{}, states: map[uint]RedisState{}}
}

func (r *memRedisRepo) RedisTablesReady() bool { return r.ready }
func (r *memRedisRepo) RedisInstances() ([]model.Instance, error) {
	var out []model.Instance
	for _, in := range r.instances {
		if in.DBType == "redis" && in.IsEnable {
			out = append(out, in)
		}
	}
	return out, nil
}
func (r *memRedisRepo) RedisState(id uint) (*RedisState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.states[id]; ok {
		return &st, nil
	}
	return nil, nil
}
func (r *memRedisRepo) RedisStates() ([]RedisState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []RedisState
	for _, st := range r.states {
		out = append(out, st)
	}
	return out, nil
}
func (r *memRedisRepo) SaveRedisState(st RedisState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveErr != nil {
		return r.saveErr
	}
	r.states[st.InstanceID] = st
	return nil
}
func (r *memRedisRepo) InsertRedisSlowlogs(id uint, rows []RedisSlowRow) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range rows {
		dup := false
		for _, y := range r.rows[id] {
			if y.SlowID == x.SlowID && y.StartTime.Equal(x.StartTime) { // 对应唯一键 (instance_id, slow_id, start_time)
				dup = true
			}
		}
		if !dup {
			r.rows[id] = append(r.rows[id], x)
			n++
		}
	}
	return n, nil
}
func (r *memRedisRepo) RedisReview(q RedisQuery) ([]RedisReviewRow, int64, *RedisSummary, error) {
	r.lastReview = &q
	return r.reviewRows, int64(len(r.reviewRows)), &RedisSummary{Fingerprints: int64(len(r.reviewRows))}, nil
}
func (r *memRedisRepo) RedisHistory(q RedisQuery) ([]RedisHistoryRow, int64, error) {
	r.lastHistory = &q
	return nil, 0, nil
}
func (r *memRedisRepo) RedisTrend(q RedisQuery) ([]RedisTrendPoint, error) {
	r.lastTrend = &q
	return nil, nil
}
func (r *memRedisRepo) RedisCommands(q RedisQuery) ([]string, error) {
	r.lastCommands = &q
	return []string{"HGETALL"}, nil
}
func (r *memRedisRepo) PurgeRedisSlowlogs(time.Time, int) (int64, error) { r.purged++; return 0, nil }

// ---- 假 Redis（RESP） ----

type fakeSlow struct {
	mu      sync.Mutex
	entries []redis.SlowEntry // 最新在前
	resets  int
	failGet bool
}

func (f *fakeSlow) handler() redistest.Handler {
	return func(a []string) any {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch strings.ToUpper(a[0]) {
		case "PING":
			return redistest.Simple("PONG")
		case "SLOWLOG":
			switch strings.ToUpper(a[1]) {
			case "LEN":
				return int64(len(f.entries))
			case "GET":
				if f.failGet {
					return redistest.Err("ERR boom")
				}
				var out []any
				for _, e := range f.entries {
					args := make([]any, len(e.Args))
					for i, x := range e.Args {
						args[i] = x
					}
					out = append(out, []any{e.ID, e.Time.Unix(), e.DurationMicros, args, e.Client, e.ClientName})
				}
				return out
			case "RESET":
				f.resets++
				f.entries = nil
				return redistest.Simple("OK")
			}
		}
		return redistest.Err("ERR unknown command")
	}
}

func rslow(id int64, sec int64, us int64, client string, args ...string) redis.SlowEntry {
	return redis.SlowEntry{ID: id, Time: time.Unix(1700000000+sec, 0), DurationMicros: us, Args: args, Client: client, ClientName: "app"}
}

type redisEnv struct {
	repo *memRedisRepo
	fake *fakeSlow
	srv  *redistest.Server
	coll *RedisCollector
	inst model.Instance
	now  time.Time
}

func newRedisEnv(t *testing.T) *redisEnv {
	t.Helper()
	f := &fakeSlow{}
	srv, err := redistest.New(f.handler())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	repo := newMemRedisRepo()
	inst := model.Instance{ID: 4, InstanceName: "redis-01", DBType: "redis", Host: srv.Host(), Port: srv.Port(), IsEnable: true}
	repo.instances = []model.Instance{inst}
	e := &redisEnv{repo: repo, fake: f, srv: srv, inst: inst, now: time.Date(2026, 10, 6, 15, 0, 0, 0, time.Local)}
	e.coll = NewRedisCollector(repo, OpenRedisTarget, func(in *model.Instance) engine.Conn { return engine.Conn{Host: in.Host, Port: in.Port} }, func() time.Time { return e.now })
	return e
}

// ---- 游标与去重 ----

func TestSelectNewEntries(t *testing.T) {
	es := []redis.SlowEntry{rslow(12, 30, 100, "a"), rslow(11, 20, 100, "a"), rslow(10, 10, 100, "a")}
	// 第一次：全部
	fresh, id, tm := SelectNewEntries(es, -1, time.Time{})
	if len(fresh) != 3 || id != 12 || fresh[0].ID != 10 || fresh[2].ID != 12 || !tm.Equal(es[0].Time) {
		t.Fatalf("首次采集: %+v id=%d", fresh, id)
	}
	// 增量：只要 id 大于游标的
	fresh, id, _ = SelectNewEntries(es, 11, es[1].Time)
	if len(fresh) != 1 || fresh[0].ID != 12 || id != 12 {
		t.Fatalf("增量: %+v id=%d", fresh, id)
	}
	// 没有新的：游标不变
	fresh, id, tm = SelectNewEntries(es, 12, es[0].Time)
	if len(fresh) != 0 || id != 12 || !tm.Equal(es[0].Time) {
		t.Fatalf("无新增: %+v id=%d", fresh, id)
	}
	// Redis 重启：id 从 0 重新计数，最新 id(2) 比游标(12) 小 → 按时间过滤
	restarted := []redis.SlowEntry{rslow(2, 100, 5000, "b"), rslow(1, 90, 5000, "b"), rslow(0, 5, 5000, "b")}
	fresh, id, _ = SelectNewEntries(restarted, 12, es[0].Time)
	if len(fresh) != 2 || fresh[0].ID != 1 || fresh[1].ID != 2 || id != 2 {
		t.Fatalf("重启后应只取开始时间不早于游标时间的条目: %+v id=%d", fresh, id)
	}
	if f, _, _ := SelectNewEntries(nil, 5, tm); f != nil {
		t.Error("空输入")
	}
}

func TestMaskArgsAndCommandText(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"AUTH", "hunter2"}, []string{"AUTH", "***"}},
		{[]string{"auth", "user", "hunter2"}, []string{"auth", "***", "***"}},
		{[]string{"HELLO", "3", "AUTH", "u", "hunter2"}, []string{"HELLO", "3", "AUTH", "***", "***"}},
		{[]string{"CONFIG", "SET", "requirepass", "hunter2"}, []string{"CONFIG", "SET", "requirepass", "***"}},
		{[]string{"CONFIG", "SET", "maxmemory", "1gb"}, []string{"CONFIG", "SET", "maxmemory", "1gb"}},
		{[]string{"ACL", "SETUSER", "bob", ">hunter2", "on"}, []string{"ACL", "SETUSER", "bob", ">***", "on"}},
		{[]string{"HGETALL", "k"}, []string{"HGETALL", "k"}},
	}
	for _, c := range cases {
		got := MaskArgs(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("MaskArgs(%v)=%v，期望 %v", c.in, got, c.want)
		}
		if strings.Contains(strings.Join(got, " "), "hunter2") {
			t.Errorf("密码未抹掉: %v", got)
		}
	}
	if got := CommandText([]string{"SET", "a b", "", `x"y`}); got != `SET "a b" "" "x\"y"` {
		t.Errorf("CommandText=%s", got)
	}
	row := ToSlowRow(rslow(1, 0, 25000, "10.0.0.5:5000", "AUTH", "hunter2"))
	if strings.Contains(row.CommandText, "hunter2") || strings.Contains(row.Fingerprint, "hunter2") || row.CommandName != "AUTH" {
		t.Errorf("入库行不应含密码: %+v", row)
	}
	row = ToSlowRow(rslow(2, 0, 1000, "c", "HGETALL", "user:42"))
	if row.Fingerprint != "HGETALL ?" || row.CommandText != "HGETALL user:42" || len(row.FpID) != 32 || row.DurationUs != 1000 {
		t.Errorf("row=%+v", row)
	}
}

// ---- 采集器 ----

func TestCollectorIncremental(t *testing.T) {
	e := newRedisEnv(t)
	e.fake.entries = []redis.SlowEntry{rslow(2, 20, 30000, "10.0.0.5:1", "HGETALL", "u:1"), rslow(1, 10, 15000, "10.0.0.6:2", "KEYS", "*"), rslow(0, 5, 11000, "", "GET", "k")}
	ctx := context.Background()

	e.coll.RunOnce(ctx)
	if got := len(e.repo.rows[4]); got != 3 {
		t.Fatalf("第一次应入库 3 条: %d", got)
	}
	st := e.repo.states[4]
	if st.Status != "ok" || st.LastSlowID != 2 || st.TotalRows != 3 || st.LastRows != 3 {
		t.Fatalf("状态: %+v", st)
	}
	var kinds []string
	for _, r := range e.repo.rows[4] {
		kinds = append(kinds, r.Fingerprint)
	}
	if strings.Join(kinds, "|") != "GET ?|KEYS ?|HGETALL ?" {
		t.Errorf("应指纹化并按旧→新入库: %v", kinds)
	}

	// 间隔内再跑一轮：不会重复采集
	e.coll.RunOnce(ctx)
	if len(e.repo.rows[4]) != 3 {
		t.Fatal("间隔内不应重复采集")
	}
	// 间隔后：没有新条目 → 不新增；有新条目 → 只新增新的
	e.now = e.now.Add(2 * time.Minute)
	e.coll.RunOnce(ctx)
	if len(e.repo.rows[4]) != 3 {
		t.Fatal("没有新条目不应新增")
	}
	e.fake.entries = append([]redis.SlowEntry{rslow(4, 50, 90000, "x", "SMEMBERS", "big"), rslow(3, 40, 20000, "x", "LRANGE", "q", "0", "-1")}, e.fake.entries...)
	e.now = e.now.Add(2 * time.Minute)
	e.coll.RunOnce(ctx)
	if len(e.repo.rows[4]) != 5 || e.repo.states[4].LastSlowID != 4 || e.repo.states[4].TotalRows != 5 {
		t.Fatalf("应只新增 2 条: %d %+v", len(e.repo.rows[4]), e.repo.states[4])
	}

	// 实例重启：条目 id 从 0 重新计数
	e.fake.entries = []redis.SlowEntry{rslow(1, 200, 8000, "y", "ZRANGE", "z", "0", "-1"), rslow(0, 190, 8000, "y", "LRANGE", "q", "0", "-1")}
	e.now = e.now.Add(2 * time.Minute)
	e.coll.RunOnce(ctx)
	if len(e.repo.rows[4]) != 7 || e.repo.states[4].LastSlowID != 1 {
		t.Fatalf("重启后新条目应入库: %d %+v", len(e.repo.rows[4]), e.repo.states[4])
	}
}

func TestCollectorFailureBackoffAndMissingTable(t *testing.T) {
	e := newRedisEnv(t)
	e.fake.entries = []redis.SlowEntry{rslow(1, 1, 1000, "", "GET", "k")}
	e.fake.failGet = true
	ctx := context.Background()
	e.coll.RunOnce(ctx)
	st := e.repo.states[4]
	if st.Status != "error" || !strings.Contains(st.Error, "SLOWLOG GET") || len(e.repo.rows[4]) != 0 {
		t.Fatalf("失败应记状态且不入库: %+v", st)
	}
	// 退避：立刻再跑不会重试
	calls := len(e.srv.Commands())
	e.coll.RunOnce(ctx)
	if len(e.srv.Commands()) != calls {
		t.Error("失败后应退避，不立刻重试")
	}
	// 恢复
	e.fake.failGet = false
	e.now = e.now.Add(5 * time.Minute)
	e.coll.RunOnce(ctx)
	if e.repo.states[4].Status != "ok" || len(e.repo.rows[4]) != 1 {
		t.Fatalf("恢复后应成功: %+v", e.repo.states[4])
	}

	// 表没建：不连实例、不报错
	e2 := newRedisEnv(t)
	e2.repo.ready = false
	e2.coll.RunOnce(ctx)
	if len(e2.srv.Commands()) != 0 {
		t.Error("表不存在时不应连接实例")
	}
	// 只有 is_enable 的 redis 实例才采集
	e3 := newRedisEnv(t)
	e3.repo.instances[0].IsEnable = false
	e3.coll.RunOnce(ctx)
	if len(e3.srv.Commands()) != 0 {
		t.Error("未启用的实例不应采集")
	}
	// 只读：采集器只发 SLOWLOG LEN / GET
	for _, c := range e.srv.Commands() {
		if strings.ToUpper(c[0]) != "SLOWLOG" {
			t.Errorf("采集器只应发 SLOWLOG 命令: %v", c)
		}
		if strings.ToUpper(c[1]) == "RESET" {
			t.Error("采集器不应重置慢日志")
		}
	}
}

func TestCollectorTunnelInstanceCollected(t *testing.T) {
	e := newRedisEnv(t)
	tid := uint(1)
	e.repo.instances[0].TunnelID = &tid
	e.coll.RunOnce(context.Background())
	if len(e.srv.Commands()) == 0 || e.repo.states[4].Status == "error" {
		t.Errorf("配置了隧道的实例不应再被跳过（连接经 conn 函数自动走隧道）: %+v", e.repo.states[4])
	}
}

// ---- 服务 ----

func redisService(t *testing.T) (*Service, *redisEnv, *fakeRepo) {
	t.Helper()
	e := newRedisEnv(t)
	svc, repo := fixture()
	repo.instances[3] = e.inst // id 4 就是 redis-01，指向假服务端
	repo.userInsts[2][4] = true
	repo.grant(3, "slowlogs.view", "slowquery.view", permQueryAll)
	rs := &RedisSlow{Repo: e.repo, Open: OpenRedisTarget, Conn: e.coll.conn, Collector: e.coll}
	svc = NewService(repo, Options{Redis: rs, Now: func() time.Time { return time.Date(2026, 10, 6, 15, 4, 5, 0, time.Local) }})
	return svc, e, repo
}

func TestRedisInstancesAndLegacyBehaviour(t *testing.T) {
	// 没启用 Redis 模块：和以前一样，带“暂不支持”提示
	s, _ := fixture()
	list, err := s.Instances(root, "query")
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range list {
		if i.DBType == "redis" && (i.Hint == "" || i.Source != "") {
			t.Errorf("未启用 Redis 模块时应保持“暂不支持”: %+v", i)
		}
	}
	// 启用后：Redis 实例带 source=redis、没有提示；查慢日志（logs）页仍不含 Redis
	svc, _, _ := redisService(t)
	list, _ = svc.Instances(root, "query")
	found := false
	for _, i := range list {
		if i.DBType == "redis" {
			found = true
			if i.Source != SourceRedis || i.Hint != "" {
				t.Errorf("redis 实例: %+v", i)
			}
		}
	}
	if !found {
		t.Fatal("慢查日志页应列出 redis 实例")
	}
	logs, _ := svc.Instances(root, "logs")
	for _, i := range logs {
		if i.DBType == "redis" {
			t.Error("“查慢日志”页不含 redis")
		}
	}
	// MySQL 系统一的 review 接口对 Redis 仍然拒绝（走专用接口）
	_, err = svc.Review(context.Background(), root, ReviewParams{InstanceID: 4})
	wantErr(t, err, 400, "Redis")
	// 汇总（全部实例）不含 redis
	if _, err := svc.Review(context.Background(), root, ReviewParams{All: true}); err != nil {
		t.Errorf("全部实例汇总: %v", err)
	}
}

func TestRedisReviewValidationAndScope(t *testing.T) {
	svc, e, repo := redisService(t)
	repo.userInsts[2] = map[uint]bool{1: true} // bob 看不到 redis-01
	repo.grant(2, "slowlogs.view", "slowquery.view")

	if _, err := svc.RedisReview(nobo, RedisParams{InstanceID: 4}); err == nil {
		t.Fatal("没有 slowlogs.view/slowquery.view 应被拒绝")
	} else {
		wantErr(t, err, 403, "没有权限")
	}
	_, err := svc.RedisReview(bob, RedisParams{InstanceID: 4})
	wantErr(t, err, 403, "未关联")
	_, err = svc.RedisReview(root, RedisParams{})
	wantErr(t, err, 400, "选择实例")
	_, err = svc.RedisReview(root, RedisParams{InstanceID: 1}) // mysql 实例
	wantErr(t, err, 400, "不是 Redis")
	_, err = svc.RedisReview(root, RedisParams{InstanceID: 99})
	wantErr(t, err, 404, "不存在")

	bad := []RedisParams{
		{InstanceID: 4, FpID: "a b"}, {InstanceID: 4, Command: "1; DROP"}, {InstanceID: 4, MinSeconds: "abc"}, {InstanceID: 4, MinSeconds: "-1"},
		{InstanceID: 4, MinSeconds: "99999"}, {InstanceID: 4, Sort: "bogus"}, {InstanceID: 4, Order: "sideways"}, {InstanceID: 4, Limit: 9999},
		{InstanceID: 4, Offset: 999999}, {InstanceID: 4, Start: "2026-01-01", End: "2026-12-31"}, {InstanceID: 4, Start: "2026-10-01"},
		{InstanceID: 4, Search: strings.Repeat("x", 300)},
	}
	for _, p := range bad {
		if _, err := svc.RedisReview(root, p); err == nil {
			t.Errorf("%+v 应被拒绝", p)
		}
	}

	e.repo.reviewRows = []RedisReviewRow{{FpID: "abc", Fingerprint: "HGETALL ?", CommandName: "HGETALL", ExecCount: 3, SampleText: "HGETALL u:1"}}
	page, err := svc.RedisReview(root, RedisParams{InstanceID: 4, Command: " hgetall ", MinSeconds: "0.05", Search: "u:1", Sort: "max_ms", Order: "asc", Limit: 10})
	if err != nil || len(page.Rows) != 1 || page.From != "2026-09-30" || page.To != "2026-10-06" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	q := e.repo.lastReview
	if q.InstanceID != 4 || q.Command != "HGETALL" || q.MinMicros != 50000 || q.Search != "u:1" || q.Sort != "max_ms" || q.Desc || q.Limit != 10 || q.From != "2026-09-30 00:00:00" || q.To != "2026-10-07 00:00:00" {
		t.Errorf("传给仓库的查询: %+v", q)
	}

	// 表没建：409 并提示执行哪个脚本
	e.repo.ready = false
	_, err = svc.RedisReview(root, RedisParams{InstanceID: 4})
	wantErr(t, err, 409, "130_redis_slowlog.sql")
	if ov, err := svc.RedisCollectorOverview(root); err != nil || ov.TableReady {
		t.Errorf("overview=%+v err=%v", ov, err)
	}
}

func TestRedisHistoryTrendCommands(t *testing.T) {
	svc, e, _ := redisService(t)
	if _, err := svc.RedisHistory(root, RedisParams{InstanceID: 4, FpID: "0123456789abcdef0123456789abcdef", Sort: "duration_ms"}); err != nil {
		t.Fatal(err)
	}
	if q := e.repo.lastHistory; q.FpID != "0123456789abcdef0123456789abcdef" || q.Sort != "duration_ms" || !q.Desc {
		t.Errorf("history 查询: %+v", q)
	}
	if _, err := svc.RedisHistory(root, RedisParams{InstanceID: 4, Sort: "id; drop"}); err == nil {
		t.Error("非法排序列应拒绝")
	}
	tr, err := svc.RedisTrend(root, RedisParams{InstanceID: 4, Bucket: "hour"})
	if err != nil || tr.Bucket != "hour" || e.repo.lastTrend.Bucket != "hour" {
		t.Errorf("trend=%+v err=%v", tr, err)
	}
	if _, err := svc.RedisTrend(root, RedisParams{InstanceID: 4, Bucket: "minute"}); err == nil {
		t.Error("非法粒度应拒绝")
	}
	cmds, err := svc.RedisCommands(root, RedisParams{InstanceID: 4, Command: "x", MinSeconds: "1"})
	if err != nil || len(cmds) != 1 || e.repo.lastCommands.Command != "X" {
		t.Errorf("cmds=%v %v", cmds, err)
	}
}

func TestRedisCollectNowAndReset(t *testing.T) {
	svc, e, repo := redisService(t)
	ctx := context.Background()
	e.fake.entries = []redis.SlowEntry{rslow(1, 10, 20000, "c", "HGETALL", "u")}

	n, err := svc.RedisCollectNow(ctx, root, 4)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	// 限频
	_, err = svc.RedisCollectNow(ctx, root, 4)
	wantErr(t, err, 429, "过于频繁")

	// 重置：先采集入库，再 SLOWLOG RESET，写审计
	e.fake.entries = append([]redis.SlowEntry{rslow(2, 20, 40000, "c", "KEYS", "*")}, e.fake.entries...)
	res, err := svc.RedisReset(ctx, root, 4)
	if err != nil || res.Cleared != 2 || res.Collected != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if e.fake.resets != 1 || len(e.repo.rows[4]) != 2 {
		t.Errorf("resets=%d rows=%d", e.fake.resets, len(e.repo.rows[4]))
	}
	if len(repo.audits) != 1 || repo.audits[0].Action != actionRedisReset || !strings.Contains(repo.audits[0].ExtraInfo, "redis-01") || !strings.Contains(repo.audits[0].ExtraInfo, "成功") {
		t.Errorf("审计: %+v", repo.audits)
	}

	// 保底采集失败 → 取消重置
	e.fake.entries = []redis.SlowEntry{rslow(3, 30, 1000, "c", "GET", "k")}
	e.fake.failGet = true
	_, err = svc.RedisReset(ctx, root, 4)
	wantErr(t, err, 502, "已取消重置")
	if e.fake.resets != 1 {
		t.Error("采集失败时不应重置")
	}
	// S4-C1：保底采集失败（服务端取消重置）也要留痕：结果为失败，不含成功字样
	if len(repo.audits) != 2 || repo.audits[1].Action != actionRedisReset || !strings.Contains(repo.audits[1].ExtraInfo, "结果：失败") || !strings.Contains(repo.audits[1].ExtraInfo, "保底采集失败") {
		t.Errorf("保底采集失败应写审计: %+v", repo.audits)
	}
	if audit.Classify(repo.audits[1].Action, repo.audits[1].ExtraInfo).Result != audit.ResultFailed {
		t.Errorf("失败审计的结果应归为失败: %+v", audit.Classify(repo.audits[1].Action, repo.audits[1].ExtraInfo))
	}
	// 范围外的实例不能重置
	repo.userInsts[2] = map[uint]bool{1: true}
	repo.grant(2, "slowlogs.view", "slowquery.view")
	_, err = svc.RedisReset(ctx, bob, 4)
	wantErr(t, err, 403, "未关联")
}

// S4-C1：连接失败（保底采集成功之后重置前连不上）也写审计，且没有执行 SLOWLOG RESET。
func TestRedisResetConnectFailureAudited(t *testing.T) {
	e := newRedisEnv(t)
	svc, repo := fixture()
	repo.instances[3] = e.inst
	repo.userInsts[2][4] = true
	rs := &RedisSlow{Repo: e.repo, Conn: e.coll.conn, Collector: e.coll,
		Open: func(context.Context, engine.Conn) (RedisTarget, error) { return nil, errors.New("dial tcp: refused") }}
	svc = NewService(repo, Options{Redis: rs, Now: func() time.Time { return time.Date(2026, 10, 6, 15, 4, 5, 0, time.Local) }})
	_, err := svc.RedisReset(context.Background(), root, 4)
	wantErr(t, err, 502, "无法连接实例")
	if e.fake.resets != 0 {
		t.Error("连接失败不应重置")
	}
	if len(repo.audits) != 1 || !strings.Contains(repo.audits[0].ExtraInfo, "结果：失败") || !strings.Contains(repo.audits[0].ExtraInfo, "无法连接实例") {
		t.Errorf("连接失败应写审计: %+v", repo.audits)
	}
}

func TestRedisModuleOff(t *testing.T) {
	s, _ := fixture()
	if _, err := s.RedisReview(root, RedisParams{InstanceID: 4}); err == nil {
		t.Error("未启用 Redis 模块应拒绝")
	}
	if _, err := s.RedisReset(context.Background(), root, 4); err == nil {
		t.Error("未启用 Redis 模块应拒绝重置")
	}
	if got := redisGapFromEnv(func(string) string { return "" }); got != time.Minute {
		t.Errorf("默认间隔: %v", got)
	}
	if got := redisGapFromEnv(func(string) string { return "1" }); got != 10*time.Second {
		t.Errorf("下限: %v", got)
	}
	if got := redisGapFromEnv(func(string) string { return "999999" }); got != time.Hour {
		t.Errorf("上限: %v", got)
	}
}

// ---- HTTP ----

func redisHTTPEnv(t *testing.T) (*servertest.Env, *redisEnv, *fakeRepo) {
	t.Helper()
	svc, e, repo := redisService(t)
	env := servertest.New(t, servertest.Options{Hooks: []func(*server.Server, server.Routes){
		func(s *server.Server, rt server.Routes) { register(s, rt, svc) },
	}})
	return env, e, repo
}

func TestRedisHTTP(t *testing.T) {
	env, e, _ := redisHTTPEnv(t)
	grantServer(env, 2, "slowlogs.view", "slowquery.view")
	root, bob := env.Login("root"), env.Login("bob")
	paths := []string{"/api/v1/slowlog/redis/review?instance_id=4", "/api/v1/slowlog/redis/history?instance_id=4", "/api/v1/slowlog/redis/trend?instance_id=4",
		"/api/v1/slowlog/redis/commands?instance_id=4", "/api/v1/slowlog/redis/collector"}
	for _, p := range paths {
		if w := env.Anonymous("GET", p); w.Code != 401 {
			t.Errorf("未登录 GET %s 应 401: %d", p, w.Code)
		}
	}
	for _, p := range []string{"/api/v1/slowlog/redis/collect", "/api/v1/slowlog/redis/reset"} {
		if w := env.Anonymous("POST", p); w.Code != 401 {
			t.Errorf("未登录 POST %s 应 401: %d", p, w.Code)
		}
	}
	for _, p := range paths {
		if code, env2 := call(t, root, "GET", p, nil); code != 200 {
			t.Errorf("root GET %s: %d %s", p, code, env2.Msg)
		}
	}
	if code, _ := call(t, root, "GET", "/api/v1/slowlog/redis/review?instance_id=abc", nil); code != 400 {
		t.Errorf("非法实例 id: %d", code)
	}

	// 普通用户：有菜单权限但没有 redis.slowlog_reset 按钮 → 重置 403，且不会碰实例
	e.fake.entries = []redis.SlowEntry{rslow(1, 1, 1000, "c", "GET", "k")}
	before := len(e.srv.Commands())
	if code, _ := call(t, bob, "POST", "/api/v1/slowlog/redis/reset", map[string]any{"instance_id": 4}); code != 403 {
		t.Errorf("没有按钮不能重置: %d", code)
	}
	if len(e.srv.Commands()) != before {
		t.Error("被拒绝的重置不应连接实例")
	}
	// 超级用户可以
	code, env2 := call(t, root, "POST", "/api/v1/slowlog/redis/reset", map[string]any{"instance_id": 4})
	if code != 200 {
		t.Fatalf("root 重置: %d %s", code, env2.Msg)
	}
	if e.fake.resets != 1 {
		t.Error("应已重置")
	}
	// 立即采集：普通用户（有查看权限、实例可见）可以
	e.fake.entries = []redis.SlowEntry{rslow(2, 2, 1000, "c", "GET", "k")}
	if code, env2 := call(t, bob, "POST", "/api/v1/slowlog/redis/collect", map[string]any{"instance_id": 4}); code != 200 {
		t.Errorf("bob 立即采集: %d %s", code, env2.Msg)
	}
	// 实例不可见
	if code, _ := call(t, bob, "POST", "/api/v1/slowlog/redis/collect", map[string]any{"instance_id": 3}); code != 400 && code != 403 {
		t.Errorf("非 Redis / 不可见实例: %d", code)
	}

	// 页面：Redis 面板与重置按钮权限
	code, body := root.Page("/slowquery")
	for _, want := range []string{`id="rq"`, `id="rq-srows"`, `id="rq-drows"`, `id="rq-reset"`, `data-can-redis-reset="1"`, `id="rq-modal"`} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("/slowquery 缺少 %q (%d)", want, code)
		}
	}
	_, body = bob.Page("/slowquery")
	if !strings.Contains(body, `data-can-redis-reset=""`) {
		t.Error("没有重置按钮权限时 data-can-redis-reset 应为空")
	}
	if strings.Contains(body, "onclick=") || strings.Contains(strings.ReplaceAll(body, `<script type="module" src=`, ""), "<script>") {
		t.Error("页面不能有内联脚本")
	}
	// 实例下拉的 source
	code, env2 = call(t, root, "GET", "/api/v1/slowlog/instances?page=query", nil)
	if code != 200 || !strings.Contains(string(env2.Data), `"source":"redis"`) {
		t.Errorf("instances: %d %s", code, env2.Data)
	}
	_ = fmt.Sprint
	_ = errors.New
}

func TestRedisAuditActionClassified(t *testing.T) {
	ext := audit.Classify(actionRedisReset, "实例：redis-01（#4）；结果：成功")
	if ext.Code != redisResetButton || !ext.Risky {
		t.Errorf("重置慢日志的审计应归到高危按钮 %s: %+v", redisResetButton, ext)
	}
}
