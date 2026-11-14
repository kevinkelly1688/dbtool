// @author: kevinkelly
// @license: Apache Licence
// @file: notify_test.go
// @time: 2026/10/28

package store

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"dbtool/internal/crypto"
	"dbtool/internal/model"
	"dbtool/internal/notify"
)

// 假驱动：按 SQL 里的关键字返回预置的结果集，记录执行过的语句。不连接任何真实数据库。
type fakeDB struct {
	mu      sync.Mutex
	logs    []string
	tables  map[string]bool           // 存在的表
	results []fakeResult              // 按顺序匹配
	args    map[string][]driver.Value // 语句 -> 最近一次参数
}

type fakeResult struct {
	match string
	cols  []string
	rows  [][]driver.Value
}

func (d *fakeDB) record(q string, args []driver.Value) {
	d.mu.Lock()
	defer d.mu.Unlock()
	q = strings.Join(strings.Fields(q), " ")
	d.logs = append(d.logs, q)
	if d.args == nil {
		d.args = map[string][]driver.Value{}
	}
	d.args[q] = args
}

func (d *fakeDB) has(sub string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func (d *fakeDB) Open(string) (driver.Conn, error) { return &fakeConn{d}, nil }

type fakeConn struct{ d *fakeDB }

func (c *fakeConn) Prepare(q string) (driver.Stmt, error) { return &fakeStmt{c.d, q}, nil }
func (c *fakeConn) Close() error                          { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)             { return fakeTx{}, nil }

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeStmt struct {
	d *fakeDB
	q string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }
func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.d.record(s.q, args)
	return fakeRes{}, nil
}

type fakeRes struct{}

func (fakeRes) LastInsertId() (int64, error) { return 9, nil }
func (fakeRes) RowsAffected() (int64, error) { return 1, nil }

func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.d.record(s.q, args)
	if strings.Contains(s.q, "information_schema.tables") {
		name := ""
		for _, a := range args {
			if str, ok := a.(string); ok && strings.HasPrefix(str, "dbt_") || ok && str == "resource_group" {
				name = str
			}
		}
		n := int64(0)
		if s.d.tables[name] {
			n = 1
		}
		return &fakeRows{cols: []string{"count(*)"}, rows: [][]driver.Value{{n}}}, nil
	}
	for _, r := range s.d.results {
		if strings.Contains(s.q, r.match) {
			return &fakeRows{cols: r.cols, rows: r.rows}, nil
		}
	}
	return &fakeRows{cols: []string{"c"}}, nil
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

var dbSeq atomic.Int32

func newNotifyStore(t *testing.T, d *fakeDB) *Store {
	t.Helper()
	tableReady.Range(func(k, _ any) bool { tableReady.Delete(k); return true })
	logCleanedAt = time.Time{}
	name := fmt.Sprintf("store-notify-%s-%d", t.Name(), dbSeq.Add(1))
	sql.Register(name, d)
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
		Logger: logger.Discard, NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := crypto.NewMirage("hfusaf2m4ot#7)fkw#di2bu6(cv0@opwmafx5n#6=3d%x^hpl6", "ECB", "")
	if err != nil {
		t.Fatal(err)
	}
	return &Store{DB: db, Mirage: m}
}

var chanCols = []string{"id", "group_id", "name", "channel_type", "webhook_url", "secret", "events", "mentions", "enabled", "created_by"}

func chanRow(id, group int64, name, typ, events string, enabled int64) []driver.Value {
	return []driver.Value{id, group, name, typ, "https://hooks.slack.com/services/" + name, "", events, `[{"target":"applicant"}]`, enabled, "root"}
}

func chanRowR(id, group int64, name, typ, events string, enabled int64, rules string) []driver.Value {
	row := chanRow(id, group, name, typ, events, enabled)
	row[7] = rules
	return row
}

var extCols = []string{"channel_id", "group_ids", "duty_users", "events_v2", "update_time"}

func TestChannelsResolveMultipleGroupsAndGlobal(t *testing.T) {
	d := &fakeDB{tables: map[string]bool{"dbt_notify_channel": true, "dbt_notify_channel_ext": true, "resource_group": true}}
	d.results = []fakeResult{
		{match: "FROM `dbt_notify_channel_ext`", cols: extCols, rows: [][]driver.Value{
			{int64(2), "1,3", "alice,bob", int64(1), time.Now()}, // 渠道 2：多个资源组 1、3，细分模型
		}},
		{match: "FROM `dbt_notify_channel`", cols: chanCols, rows: [][]driver.Value{
			chanRow(1, 1, "只有组1", "slack", "", 1),
			chanRow(2, 1, "多组", "slack", "perf_overdue", 1), // group_id 保持第一个，真正的范围在扩展表
			chanRow(3, 0, "全局", "slack", "", 1),
			chanRow(4, 2, "只有组2", "slack", "pending,exception", 1),
		}},
	}
	s := newNotifyStore(t, d)
	names := func(group uint) string {
		chans, err := s.Channels(group)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range chans {
			out = append(out, c.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(1); got != "只有组1,多组,全局" {
		t.Errorf("组 1: %s", got)
	}
	if got := names(3); got != "多组,全局" {
		t.Errorf("组 3 只被多组渠道和全局渠道覆盖: %s", got)
	}
	if got := names(2); got != "全局,只有组2" {
		t.Errorf("组 2: %s", got)
	}
	if got := names(0); got != "全局" {
		t.Errorf("资源组 0（性能工单全局事件）只发给全局渠道: %s", got)
	}
	chans, _ := s.Channels(3)
	if c := chans[0]; !c.EventsV2 || len(c.GroupIDs) != 2 || len(c.DutyUsers) != 2 || c.DutyUsers[1] != "bob" {
		t.Errorf("扩展信息应带出: %+v", c)
	}
}

func TestChannelsWithoutExtBehaveLikeBefore(t *testing.T) {
	d := &fakeDB{tables: map[string]bool{"dbt_notify_channel": true, "resource_group": true}}
	d.results = []fakeResult{{match: "FROM `dbt_notify_channel`", cols: chanCols, rows: [][]driver.Value{chanRow(1, 1, "组1", "slack", "pending", 1)}}}
	s := newNotifyStore(t, d)
	chans, err := s.Channels(1)
	if err != nil || len(chans) != 1 || chans[0].EventsV2 || len(chans[0].GroupIDs) != 0 {
		t.Fatalf("%+v %v", chans, err)
	}
	if !d.has("group_id IN") {
		t.Errorf("没有扩展信息时应直接按 group_id 过滤: %v", d.logs)
	}
	if d.has("dbt_notify_channel_ext` ") && d.has("FROM `dbt_notify_channel_ext`") {
		t.Error("扩展表不存在时不应查询它")
	}
}

func TestListChannelViewsKeepsEventsAndLastSend(t *testing.T) {
	d := &fakeDB{tables: map[string]bool{"dbt_notify_channel": true, "dbt_notify_channel_ext": true, "dbt_notify_log": true}}
	now := time.Date(2026, 10, 7, 10, 0, 0, 0, time.Local)
	d.results = []fakeResult{
		{match: "FROM `dbt_notify_channel_ext`", cols: extCols, rows: [][]driver.Value{{int64(2), "", "", int64(1), now}}},
		{match: "FROM dbt_notify_log l JOIN", cols: []string{"id", "channel_id", "channel_name", "event", "kind", "ok", "cost_ms", "error", "mentions", "is_test", "created_at"},
			rows: [][]driver.Value{{int64(5), int64(1), "旧", "pending", "sql_workflow", int64(0), int64(120), "HTTP 429", int64(2), int64(0), now}}},
		{match: "FROM `dbt_notify_channel`", cols: chanCols, rows: [][]driver.Value{
			chanRowR(1, 1, "旧", "slack", "exception", 1, `[{"target":"applicant","events":["exception"]}]`), // 旧模型：订阅与规则过滤都展开
			chanRow(2, 0, "新", "slack", "exception", 1),                                                     // 细分模型：不展开
			chanRow(3, 1, "全部", "slack", "", 1),                                                             // 订阅全部：保持为空
		}},
	}
	s := newNotifyStore(t, d)
	views, err := s.ListChannelViews()
	if err != nil || len(views) != 3 {
		t.Fatalf("%+v %v", views, err)
	}
	old, nw, all := views[0], views[1], views[2]
	if len(old.Events) != 1 || old.GroupIDs[0] != 1 || len(old.Rules[0].Events) != 1 {
		t.Errorf("订阅与规则的事件原样返回: %+v", old)
	}
	if len(nw.Events) != 1 || len(nw.GroupIDs) != 0 {
		t.Errorf("全局渠道无资源组: %+v", nw)
	}
	if len(all.Events) != 0 {
		t.Errorf("订阅全部保持为空: %+v", all)
	}
	if old.LastSend == nil || old.LastSend.OK || old.LastSend.Error != "HTTP 429" || old.LastSend.Time != "2026-10-07 10:00:00" || nw.LastSend != nil {
		t.Errorf("最近发送: %+v", old.LastSend)
	}
}

func TestSaveChannelWritesExtAndKeepsFirstGroup(t *testing.T) {
	d := &fakeDB{tables: map[string]bool{"dbt_notify_channel": true, "dbt_notify_channel_ext": true}}
	s := newNotifyStore(t, d)
	id, err := s.SaveChannel(0, notify.ChannelInput{Name: "x", Type: "slack", URL: "https://hooks.slack.com/services/a", Enabled: true,
		GroupIDs: []uint{7, 8}, DutyUsers: []string{"alice", "bob"}, Events: []string{"pending"}, Operator: "root"})
	if err != nil || id == 0 {
		t.Fatalf("%d %v", id, err)
	}
	var chanArgs, extArgs []driver.Value
	for q, a := range d.args {
		switch {
		case strings.Contains(q, "INSERT INTO `dbt_notify_channel` "):
			chanArgs = a
		case strings.Contains(q, "dbt_notify_channel_ext"):
			extArgs = a
		}
	}
	if chanArgs == nil || extArgs == nil {
		t.Fatalf("应同时写渠道表与扩展表: %v", d.logs)
	}
	has := func(args []driver.Value, want any) bool {
		for _, a := range args {
			if fmt.Sprint(a) == fmt.Sprint(want) {
				return true
			}
		}
		return false
	}
	if !has(chanArgs, int64(7)) || has(chanArgs, int64(8)) {
		t.Errorf("group_id 应保存为第一个资源组: %v", chanArgs)
	}
	if !has(extArgs, "7,8") || !has(extArgs, "alice,bob") || !has(extArgs, true) && !has(extArgs, int64(1)) {
		t.Errorf("扩展表应保存资源组列表、值班人员、细分事件模型标记: %v", extArgs)
	}
	for _, a := range chanArgs {
		if s, ok := a.(string); ok && strings.Contains(s, "hooks.slack.com") {
			t.Errorf("webhook 地址应加密后入库: %v", a)
		}
	}
	// 单个资源组 / 全局：扩展表的 group_ids 为空，范围仍由 group_id 决定
	d2 := &fakeDB{tables: map[string]bool{"dbt_notify_channel": true, "dbt_notify_channel_ext": true}}
	s2 := newNotifyStore(t, d2)
	if _, err := s2.SaveChannel(0, notify.ChannelInput{Name: "y", Type: "slack", URL: "u", GroupIDs: []uint{4}}); err != nil {
		t.Fatal(err)
	}
	for q, a := range d2.args {
		if strings.Contains(q, "dbt_notify_channel_ext") && has(a, "4") {
			t.Errorf("单个资源组不需要写 group_ids: %v", a)
		}
	}
	// 扩展表不存在：只写渠道表
	d3 := &fakeDB{tables: map[string]bool{"dbt_notify_channel": true}}
	s3 := newNotifyStore(t, d3)
	if _, err := s3.SaveChannel(0, notify.ChannelInput{Name: "z", Type: "slack", URL: "u", GroupIDs: []uint{1, 2}}); err != nil {
		t.Fatal(err)
	}
	if d3.has("dbt_notify_channel_ext") {
		t.Errorf("扩展表不存在时不应写它: %v", d3.logs)
	}
}

func TestRecordSendAndCleanup(t *testing.T) {
	d := &fakeDB{tables: map[string]bool{"dbt_notify_log": true}}
	s := newNotifyStore(t, d)
	long := strings.Repeat("错", 600)
	s.RecordSend(notify.SendLog{ChannelID: 3, ChannelName: "c", Event: "pending", OK: false, CostMs: 12, Error: long, Mentions: 2, Test: true, At: time.Now()})
	if !d.has("INSERT INTO `dbt_notify_log`") {
		t.Fatalf("应写发送记录: %v", d.logs)
	}
	if !d.has("DELETE FROM `dbt_notify_log` WHERE created_at <") {
		t.Errorf("第一次写入应顺带清理 30 天前的记录: %v", d.logs)
	}
	for q, a := range d.args {
		if strings.Contains(q, "INSERT INTO `dbt_notify_log`") {
			for _, v := range a {
				if str, ok := v.(string); ok && len([]rune(str)) > 500 {
					t.Errorf("错误摘要应截断到 500 个字符: %d", len([]rune(str)))
				}
			}
		}
		if strings.Contains(q, "DELETE FROM `dbt_notify_log`") {
			cut, ok := a[0].(time.Time)
			if !ok || time.Since(cut) < 29*24*time.Hour || time.Since(cut) > 31*24*time.Hour {
				t.Errorf("清理界线应是 30 天前: %v", a)
			}
		}
	}
	d.mu.Lock()
	d.logs = nil
	d.mu.Unlock()
	s.RecordSend(notify.SendLog{ChannelID: 3, OK: true})
	if d.has("DELETE FROM") {
		t.Error("一小时内不重复清理")
	}
	// 表不存在：什么也不做
	d2 := &fakeDB{tables: map[string]bool{}}
	s2 := newNotifyStore(t, d2)
	s2.RecordSend(notify.SendLog{ChannelID: 1, OK: true})
	if d2.has("INSERT") {
		t.Error("发送记录表不存在时不应写入")
	}
}

func TestLogQueriesAndStats(t *testing.T) {
	d := &fakeDB{tables: map[string]bool{"dbt_notify_log": true}}
	d.results = []fakeResult{{match: "COALESCE(SUM", cols: []string{"total", "failed"}, rows: [][]driver.Value{{int64(10), int64(3)}}}}
	s := newNotifyStore(t, d)
	since := time.Now().AddDate(0, 0, -1)
	if _, _, err := s.ListSendLogs(notify.LogFilter{ChannelID: 2, OnlyFailed: true, Event: "pending", Since: &since, Limit: 20}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"channel_id = ?", "ok = 0", "event = ?", "created_at >= ?", "ORDER BY id DESC"} {
		if !d.has(want) {
			t.Errorf("发送记录查询缺少 %q: %v", want, d.logs)
		}
	}
	st, err := s.SendStats(since)
	if err != nil || st.Total != 10 || st.Failed != 3 {
		t.Errorf("%+v %v", st, err)
	}
	if err := s.SetChannelEnabled(4, false); err != nil || !d.has("UPDATE `dbt_notify_channel` SET `enabled`=") {
		t.Errorf("只改启用状态: %v %v", err, d.logs)
	}
	// 表不存在：空结果
	s2 := newNotifyStore(t, &fakeDB{tables: map[string]bool{}})
	rows, total, err := s2.ListSendLogs(notify.LogFilter{Limit: 10})
	if err != nil || len(rows) != 0 || total != 0 {
		t.Errorf("%v %d %v", rows, total, err)
	}
	if st, _ := s2.SendStats(since); st.Total != 0 {
		t.Error("表不存在时统计为 0")
	}
}

func TestPureHelpers(t *testing.T) {
	if got := groupIDsOf(5, nil); len(got) != 1 || got[0] != 5 {
		t.Errorf("%v", got)
	}
	if got := groupIDsOf(0, nil); got != nil {
		t.Errorf("全局: %v", got)
	}
	if got := groupIDsOf(5, &model.NotifyChannelExt{GroupIDs: "2,9"}); len(got) != 2 || got[1] != 9 {
		t.Errorf("%v", got)
	}
	if !channelApplies(nil, 3) || !channelApplies(nil, 0) || channelApplies([]uint{1}, 2) || !channelApplies([]uint{1, 2}, 2) || channelApplies([]uint{1}, 0) {
		t.Error("channelApplies")
	}
	if joinIDs([]uint{3, 1}) != "3,1" || len(splitNames(" a, ,b ")) != 2 || hostOf("https://h.example/x?y") != "h.example" {
		t.Error("编码辅助")
	}
}
