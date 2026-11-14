// @author: kevinkelly
// @license: Apache Licence
// @file: run_test.go
// @time: 2026/10/28

package mongo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"dbtool/internal/engine"
)

func mkDocs(n int) []bson.D {
	out := make([]bson.D, n)
	for i := range out {
		out[i] = bson.D{{Key: "_id", Value: int32(i + 1)}, {Key: "name", Value: fmt.Sprintf("n%d", i+1)}}
	}
	return out
}

func mustCheck(t *testing.T, text string, opts ...CheckOptions) *Checked {
	t.Helper()
	c, err := CheckUserQuery(text, opts...)
	if err != nil {
		t.Fatalf("check %q: %v", text, err)
	}
	return c
}

func newTestEngine(t *testing.T) (*Engine, *fakeBackend) {
	t.Helper()
	be := newFake()
	return NewWithBackend(be), be
}

func TestRunFindBasics(t *testing.T) {
	e, be := newTestEngine(t)
	be.db("app").coll("users", mkDocs(5)...)
	lim := QueryLimits{MaxRows: 100, Timeout: 7 * time.Second}

	sh, err := e.RunUser(context.Background(), "app", mustCheck(t, `db.users.find({a: 1}, {name: 1}).sort({_id: -1}).skip(1)`), lim)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sh.Columns, ","); got != "_id,name" || len(sh.Rows) != 4 || sh.Truncated {
		t.Fatalf("columns=%v rows=%d truncated=%v", sh.Columns, len(sh.Rows), sh.Truncated)
	}
	if sh.Rows[0][0] != int32(2) || sh.Rows[0][1] != "n2" {
		t.Errorf("row0=%v", sh.Rows[0])
	}
	f := be.finds[0]
	if f.DB != "app" || f.Coll != "users" || f.Spec.Limit != 101 || f.Spec.Skip != 1 || f.Spec.MaxTime != 7*time.Second || f.Spec.BatchSize != 101 {
		t.Errorf("find spec=%+v", f)
	}
	if len(f.Spec.Filter) != 1 || len(f.Spec.Projection) != 1 || len(f.Spec.Sort) != 1 {
		t.Errorf("参数未透传：%+v", f.Spec)
	}

	// 用户 limit 在上限内：原样发送，不算截断
	sh, err = e.RunUser(context.Background(), "app", mustCheck(t, `db.users.find().limit(2)`), lim)
	if err != nil || len(sh.Rows) != 2 || sh.Truncated {
		t.Fatalf("limit 2: %v rows=%d trunc=%v", err, len(sh.Rows), sh.Truncated)
	}
	if be.finds[1].Spec.Limit != 2 {
		t.Errorf("limit=%d", be.finds[1].Spec.Limit)
	}
	// 缺省 BatchSize 不超过 1000
	_, _ = e.RunUser(context.Background(), "app", mustCheck(t, `db.users.find()`), QueryLimits{MaxRows: 50000})
	if got := be.finds[2].Spec; got.BatchSize != 1000 || got.Limit != 50001 {
		t.Errorf("批大小：%+v", got)
	}
	// 默认值
	_, _ = e.RunUser(context.Background(), "app", mustCheck(t, `db.users.find()`), QueryLimits{})
	if got := be.finds[3].Spec; got.Limit != DefaultMaxRows+1 || got.MaxTime != 30*time.Second {
		t.Errorf("默认限量：%+v", got)
	}
}

func TestRunFindRowTruncation(t *testing.T) {
	e, be := newTestEngine(t)
	be.db("app").coll("users", mkDocs(10)...)
	sh, err := e.RunUser(context.Background(), "app", mustCheck(t, `db.users.find()`), QueryLimits{MaxRows: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Rows) != 3 || !sh.Truncated {
		t.Fatalf("rows=%d truncated=%v", len(sh.Rows), sh.Truncated)
	}
	if !strings.Contains(strings.Join(sh.Warnings, ";"), "截断") {
		t.Errorf("应有截断提示：%v", sh.Warnings)
	}
	// 恰好等于上限：不算截断
	sh, _ = e.RunUser(context.Background(), "app", mustCheck(t, `db.users.find()`), QueryLimits{MaxRows: 10})
	if len(sh.Rows) != 10 || sh.Truncated {
		t.Errorf("恰好 10 行不应截断：rows=%d trunc=%v", len(sh.Rows), sh.Truncated)
	}
	// 用户 limit 大于上限：收窄并标记截断
	sh, _ = e.RunUser(context.Background(), "app", mustCheck(t, `db.users.find().limit(9)`), QueryLimits{MaxRows: 4})
	if len(sh.Rows) != 4 || !sh.Truncated {
		t.Errorf("limit 超过上限：rows=%d trunc=%v", len(sh.Rows), sh.Truncated)
	}
}

func TestRunByteLimit(t *testing.T) {
	e, be := newTestEngine(t)
	pad := strings.Repeat("x", 1000)
	var docs []bson.D
	for i := 0; i < 50; i++ {
		docs = append(docs, bson.D{{Key: "_id", Value: int32(i)}, {Key: "pad", Value: pad}})
	}
	be.db("app").coll("big", docs...)
	sh, err := e.RunUser(context.Background(), "app", mustCheck(t, `db.big.find()`), QueryLimits{MaxRows: 1000, MaxBytes: 10 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.Truncated || len(sh.Rows) == 0 || len(sh.Rows) >= 50 || len(sh.Rows) > 10 {
		t.Fatalf("字节上限未生效：rows=%d trunc=%v", len(sh.Rows), sh.Truncated)
	}
	// 第一个文档超过上限也要返回（单元格另行截断）
	huge := bson.D{{Key: "_id", Value: int32(1)}, {Key: "pad", Value: strings.Repeat("y", 200<<10)}}
	be.db("app").coll("huge", huge)
	sh, err = e.RunUser(context.Background(), "app", mustCheck(t, `db.huge.find()`), QueryLimits{MaxBytes: 1 << 10})
	if err != nil || len(sh.Rows) != 1 {
		t.Fatalf("首个文档应返回：%v rows=%d", err, len(sh.Rows))
	}
	cell := sh.Rows[0][1].(string)
	if len(cell) > DefaultMaxCellBytes+200 || !strings.Contains(cell, "已截断") {
		t.Errorf("大字符串应截断：len=%d", len(cell))
	}
	if !strings.Contains(strings.Join(sh.Warnings, ";"), "超过 64 KB") {
		t.Errorf("应有单元格截断提示：%v", sh.Warnings)
	}
	// MaxBytes 不能超过硬上限
	if got := (QueryLimits{MaxBytes: 100 << 20}).norm().MaxBytes; got != HardMaxBytes {
		t.Errorf("MaxBytes 应被夹到 %d：%d", HardMaxBytes, got)
	}
}

func TestRunAggregate(t *testing.T) {
	e, be := newTestEngine(t)
	be.db("app").coll("users", mkDocs(10)...)
	be.db("app").coll("orders")
	sh, err := e.RunUser(context.Background(), "app", mustCheck(t, `db.users.aggregate([{$match: {a: 1}}, {$lookup: {from: "orders", localField: "a", foreignField: "b", as: "o"}}])`), QueryLimits{MaxRows: 4, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Rows) != 4 || !sh.Truncated {
		t.Fatalf("rows=%d trunc=%v", len(sh.Rows), sh.Truncated)
	}
	a := be.aggs[0]
	n := len(a.Spec.Pipeline)
	if n != 3 || a.Spec.Pipeline[n-1][0].Key != "$limit" || i64(a.Spec.Pipeline[n-1][0].Value) != 5 || a.Spec.MaxTime != 3*time.Second {
		t.Errorf("pipeline 末尾应补 $limit 5：%+v", a.Spec)
	}
	// 用户的 pipeline 不被改动（checked 可复用）
	c := mustCheck(t, `db.users.aggregate([{$match: {}}])`)
	_, _ = e.RunUser(context.Background(), "app", c, QueryLimits{})
	_, _ = e.RunUser(context.Background(), "app", c, QueryLimits{})
	if len(c.Pipeline) != 1 {
		t.Errorf("Checked.Pipeline 被改动：%d", len(c.Pipeline))
	}
}

func TestRunMiscKinds(t *testing.T) {
	e, be := newTestEngine(t)
	d := be.db("app")
	c := d.coll("users", mkDocs(3)...)
	c.indexes = []bson.D{{{Key: "v", Value: int32(2)}, {Key: "key", Value: bson.D{{Key: "_id", Value: int32(1)}}}, {Key: "name", Value: "_id_"}}}
	c.stats = bson.D{{Key: "ns", Value: "app.users"}, {Key: "count", Value: int32(3)}, {Key: "size", Value: int64(300)}, {Key: "wiredTiger", Value: bson.D{{Key: "x", Value: 1}}}}
	c.distinct = bson.A{"b", "a", int32(3), nil}
	d.coll("system.profile")
	d.infos = append(d.infos, CollectionInfo{Name: "v1", Type: "view", Options: bson.D{{Key: "viewOn", Value: "users"}}},
		CollectionInfo{Name: "ts", Type: "timeseries"},
		CollectionInfo{Name: "cap", Type: "collection", Options: bson.D{{Key: "capped", Value: true}}})
	d.stats = bson.D{{Key: "db", Value: "app"}, {Key: "collections", Value: int32(5)}, {Key: "dataSize", Value: float64(1234)}}
	ctx := context.Background()

	sh, _ := e.RunUser(ctx, "app", mustCheck(t, `db.users.countDocuments({a: 1})`), QueryLimits{})
	if len(sh.Rows) != 1 || sh.Rows[0][0] != int64(3) || sh.Columns[0] != "count" {
		t.Errorf("countDocuments=%v", sh.Rows)
	}
	sh, _ = e.RunUser(ctx, "app", mustCheck(t, `db.users.find().count()`), QueryLimits{})
	if sh.Rows[0][0] != int64(3) {
		t.Errorf("find().count()=%v", sh.Rows)
	}
	sh, _ = e.RunUser(ctx, "app", mustCheck(t, `db.users.estimatedDocumentCount()`), QueryLimits{})
	if sh.Rows[0][0] != int64(3) {
		t.Errorf("estimated=%v", sh.Rows)
	}
	sh, _ = e.RunUser(ctx, "app", mustCheck(t, `db.users.distinct("x")`), QueryLimits{})
	if sh.Columns[0] != "x" || len(sh.Rows) != 4 || sh.Rows[2][0] != int32(3) || sh.Rows[3][0] != nil {
		t.Errorf("distinct=%v", sh.Rows)
	}
	sh, _ = e.RunUser(ctx, "app", mustCheck(t, `db.users.distinct("x")`), QueryLimits{MaxRows: 2})
	if len(sh.Rows) != 2 || !sh.Truncated {
		t.Errorf("distinct 截断：%d %v", len(sh.Rows), sh.Truncated)
	}
	sh, _ = e.RunUser(ctx, "app", mustCheck(t, `db.getCollectionNames()`), QueryLimits{})
	var names []string
	for _, r := range sh.Rows {
		names = append(names, fmt.Sprintf("%v:%v", r[0], r[1]))
	}
	if got := strings.Join(names, ","); got != "cap:capped,ts:timeseries,users:collection,v1:view" {
		t.Errorf("getCollectionNames=%s", got)
	}
	sh, _ = e.RunUser(ctx, "app", mustCheck(t, `db.stats()`), QueryLimits{})
	if strings.Join(sh.Columns, ",") != "db,collections,dataSize" || sh.Rows[0][1] != int32(5) || sh.Rows[0][2] != float64(1234) {
		t.Errorf("db.stats=%v %v", sh.Columns, sh.Rows)
	}
	sh, _ = e.RunUser(ctx, "app", mustCheck(t, `db.users.stats()`), QueryLimits{})
	if strings.Join(sh.Columns, ",") != "ns,count,size" || sh.Rows[0][2] != "300" {
		t.Errorf("coll.stats=%v %v", sh.Columns, sh.Rows)
	}
	sh, _ = e.RunUser(ctx, "app", mustCheck(t, `db.users.getIndexes()`), QueryLimits{})
	if strings.Join(sh.Columns, ",") != "v,key,name" || sh.Rows[0][1] != `{"_id":1}` || sh.Rows[0][2] != "_id_" {
		t.Errorf("getIndexes=%v %v", sh.Columns, sh.Rows)
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	e, be := newTestEngine(t)
	be.db("app").coll("users", mkDocs(1)...)
	c := mustCheck(t, `db.users.find()`)
	for _, db := range []string{"", "admin", "local", "config", "a.b", "a b"} {
		if _, err := e.RunUser(context.Background(), db, c, QueryLimits{}); err == nil {
			t.Errorf("库 %q 应被拒绝", db)
		}
	}
	if _, err := e.RunUser(context.Background(), "app", nil, QueryLimits{}); err == nil {
		t.Error("nil Checked 应报错")
	}
	if len(be.finds) != 0 {
		t.Error("被拒绝的请求不应到达后端")
	}
}

func TestRunTimeoutAndCancel(t *testing.T) {
	e, be := newTestEngine(t)
	be.db("app").coll("slow", mkDocs(3)...).block = true
	start := time.Now()
	_, err := e.RunUser(context.Background(), "app", mustCheck(t, `db.slow.find()`), QueryLimits{Timeout: 60 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("应超时：%v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("超时没有及时生效")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err = e.RunUser(ctx, "app", mustCheck(t, `db.slow.find()`), QueryLimits{Timeout: 10 * time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回取消：%v", err)
	}
	// 聚合与 count 同样遵守 ctx
	be.db("app").coll("slow").err = context.DeadlineExceeded
	_, err = e.RunUser(context.Background(), "app", mustCheck(t, `db.slow.countDocuments()`), QueryLimits{})
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("count 超时：%v", err)
	}
}

func TestRunErrorMapping(t *testing.T) {
	e, be := newTestEngine(t)
	e.password = "s3cret!"
	c := be.db("app").coll("users", mkDocs(2)...)
	cases := []struct {
		err  error
		want string
		is   error
	}{
		{&codeErr{code: 50, msg: "operation exceeded time limit"}, "", ErrTimeout},
		{&codeErr{code: 262, msg: "x"}, "", ErrTimeout},
		{&codeErr{code: 292, msg: "Sort exceeded memory limit"}, "禁止使用磁盘", nil},
		{&codeErr{code: 16819, msg: "Sort exceeded memory limit of 33554432 bytes"}, "排序字段建索引", nil},
		{&codeErr{code: 13, msg: "not authorized on app to execute command"}, "没有执行该查询所需的权限", nil},
		{&codeErr{code: 18, msg: "Authentication failed"}, "认证失败", nil},
		{errors.New("connection refused to mongodb://root:s3cret!@db1:27017/admin: auth s3cret!"), "mongodb-uri://db1:27017", nil},
	}
	for _, tc := range cases {
		c.err = tc.err
		_, err := e.RunUser(context.Background(), "app", mustCheck(t, `db.users.find()`), QueryLimits{})
		if err == nil {
			t.Fatalf("%v 应返回错误", tc.err)
		}
		if tc.is != nil && !errors.Is(err, tc.is) {
			t.Errorf("%v → %v", tc.err, err)
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v → %q 应包含 %q", tc.err, err.Error(), tc.want)
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "root:") {
			t.Errorf("错误信息泄露了凭据：%q", err.Error())
		}
	}
	// 游标中途出错
	c.err = nil
	c.cursorEr = errors.New("cursor killed")
	if _, err := e.RunUser(context.Background(), "app", mustCheck(t, `db.users.find()`), QueryLimits{}); err == nil || !strings.Contains(err.Error(), "cursor killed") {
		t.Errorf("游标错误应上抛：%v", err)
	}
}

func TestScrub(t *testing.T) {
	e := &Engine{password: "p@ss"}
	got := e.scrub("fail p@ss mongodb://u:p%40ss@h1:1,h2:2/db?x=1 and mongodb+srv://a:b@cluster.example/x")
	for _, bad := range []string{"p@ss", "u:", "a:b@"} {
		if strings.Contains(got, bad) {
			t.Errorf("scrub 后仍含 %q：%s", bad, got)
		}
	}
	if !strings.Contains(got, "h1:1,h2:2") || !strings.Contains(got, "cluster.example") {
		t.Errorf("主机信息应保留：%s", got)
	}
	long := strings.Repeat("长", 2000)
	if r := []rune(e.scrub(long)); len(r) > 601 {
		t.Errorf("应截断：%d", len(r))
	}
}

func TestCursorsClosed(t *testing.T) {
	fakeClosedMu.Lock()
	fakeClosed = nil
	fakeClosedMu.Unlock()
	e, be := newTestEngine(t)
	d := be.db("app")
	d.coll("users", mkDocs(20)...).indexes = mkDocs(2)
	for _, q := range []string{`db.users.find()`, `db.users.aggregate([])`, `db.users.getIndexes()`} {
		if _, err := e.RunUser(context.Background(), "app", mustCheck(t, q), QueryLimits{MaxRows: 3}); err != nil {
			t.Fatal(err)
		}
	}
	fakeClosedMu.Lock()
	defer fakeClosedMu.Unlock()
	if len(fakeClosed) != 3 {
		t.Fatalf("游标数=%d", len(fakeClosed))
	}
	for i, c := range fakeClosed {
		if !*c {
			t.Errorf("第 %d 个游标没有关闭", i)
		}
	}
}

func TestRunConcurrent(t *testing.T) {
	e, be := newTestEngine(t)
	be.db("app").coll("users", mkDocs(50)...)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := fmt.Sprintf(`db.users.find({n: %d}).limit(%d)`, i, i%7+1)
			if i%3 == 0 {
				q = `db.users.aggregate([{$match: {}}])`
			}
			c, err := CheckUserQuery(q)
			if err != nil {
				errs <- err
				return
			}
			sh, err := e.RunUser(context.Background(), "app", c, QueryLimits{MaxRows: 20})
			if err != nil {
				errs <- err
				return
			}
			if i%3 != 0 && len(sh.Rows) != i%7+1 {
				errs <- fmt.Errorf("rows=%d want %d", len(sh.Rows), i%7+1)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestQueryViaEngineInterface(t *testing.T) {
	be := newFake()
	be.db("app").coll("users", mkDocs(5)...)
	e := NewWithBackend(be)
	e.defaultDB = "app"
	var eng engine.Engine = e
	rs, err := eng.Query(context.Background(), `db.users.find()`, engine.QueryOptions{Limit: 3, MaxExecMs: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Rows) != 3 || strings.Join(rs.ColumnList, ",") != "_id,name" {
		t.Errorf("rs=%+v", rs)
	}
	if be.finds[0].Spec.MaxTime != 5*time.Second {
		t.Errorf("MaxExecMs 未生效：%v", be.finds[0].Spec.MaxTime)
	}
	if _, err := eng.Query(context.Background(), `db.users.drop()`, engine.QueryOptions{DBName: "app"}); err == nil {
		t.Error("写操作应被拒绝")
	}
	if _, err := eng.Query(context.Background(), `db.users.find()`, engine.QueryOptions{DBName: "admin"}); err == nil {
		t.Error("系统库应被拒绝")
	}
	rs, err = eng.Query(context.Background(), `db.users.find()`, engine.QueryOptions{DBName: "app"})
	if err != nil || len(rs.Rows) != 5 {
		t.Errorf("DBName 指定库：%v", err)
	}
	if eng.Name() != "mongo" {
		t.Error(eng.Name())
	}
}

func TestEngineRegistered(t *testing.T) {
	found := false
	for _, n := range engine.Supported() {
		if n == "mongo" {
			found = true
		}
	}
	if !found {
		t.Fatal("mongo 引擎未注册")
	}
}

func TestShapedJSONSafe(t *testing.T) {
	e, be := newTestEngine(t)
	oid, _ := bson.ObjectIDFromHex("507f1f77bcf86cd799439011")
	dec, _ := bson.ParseDecimal128("12345678901234567890.123456789")
	be.db("app").coll("t", bson.D{
		{Key: "_id", Value: oid},
		{Key: "big", Value: int64(9007199254740993)},
		{Key: "dec", Value: dec},
		{Key: "d", Value: bson.NewDateTimeFromTime(time.Date(2024, 1, 2, 3, 4, 5, 6000000, time.UTC))},
		{Key: "bin", Value: bson.Binary{Subtype: 0, Data: []byte{1, 2, 255}}},
		{Key: "doc", Value: bson.D{{Key: "a", Value: oid}, {Key: "n", Value: bson.A{int32(1), "x"}}}},
		{Key: "nan", Value: nanValue()},
	})
	sh, err := e.RunUser(context.Background(), "app", mustCheck(t, `db.t.find()`), QueryLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(sh); err != nil {
		t.Fatalf("结果必须能序列化为 JSON：%v", err)
	}
	row := map[string]any{}
	for i, c := range sh.Columns {
		row[c] = sh.Rows[0][i]
	}
	want := map[string]any{
		"_id": `ObjectId("507f1f77bcf86cd799439011")`,
		"big": "9007199254740993",
		"dec": "12345678901234567890.123456789",
		"d":   "2024-01-02T03:04:05.006Z",
		"bin": `BinData(0, \x0102ff)`,
		"doc": `{"a":{"$oid":"507f1f77bcf86cd799439011"},"n":[1,"x"]}`,
		"nan": "NaN",
	}
	for k, w := range want {
		if row[k] != w {
			t.Errorf("%s: got %v want %v", k, row[k], w)
		}
	}
	if !strings.Contains(strings.Join(sh.Warnings, ";"), "二进制") {
		t.Errorf("应有二进制提示：%v", sh.Warnings)
	}
}
