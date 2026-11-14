// @author: kevinkelly
// @license: Apache Licence
// @file: extra_test.go
// @time: 2026/10/28

package partman

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
)

// ---- 假仓库 / 假目标库的补充能力 ----

func (r *fakeRepo) ListLogsByCnf(ids []int64, source string, offset, limit int) ([]Log, int64, error) {
	in := map[int64]bool{}
	for _, id := range ids {
		in[id] = true
	}
	var all []Log
	for i := len(r.logs) - 1; i >= 0; i-- { // 最新在前
		l := r.logs[i]
		if !in[l.CnfID] {
			continue
		}
		manual, auto := strings.Contains(l.ExecResult, `"source":"manual"`), strings.Contains(l.ExecResult, `"source":"auto"`)
		switch source {
		case "manual":
			if !manual {
				continue
			}
		case "auto":
			if !auto {
				continue
			}
		case "external":
			if manual || auto {
				continue
			}
		}
		all = append(all, l)
	}
	total := int64(len(all))
	if offset >= len(all) {
		return nil, total, nil
	}
	all = all[offset:]
	if len(all) > limit {
		all = all[:limit]
	}
	return all, total, nil
}

func (f *fakeTarget) Databases(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectEr != nil {
		return nil, f.inspectEr
	}
	seen := map[string]bool{"mysql": true, "information_schema": true, "bad name": true, "secretdb": true}
	var out []string
	for _, d := range []string{"mysql", "information_schema", "bad name", "secretdb"} {
		out = append(out, d)
	}
	for k := range f.tables {
		d := k[:strings.Index(k, ".")]
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeTarget) Tables(_ context.Context, db string) ([]TableInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectEr != nil {
		return nil, f.inspectEr
	}
	out := []TableInfo{{Name: "plain_tbl"}, {Name: "bad name"}}
	for k, st := range f.tables {
		if strings.HasPrefix(k, db+".") {
			out = append(out, TableInfo{Name: k[len(db)+1:], Partitioned: st.Method != "", Method: st.Method})
		}
	}
	return out, nil
}

func (f *fakeTarget) Columns(_ context.Context, db, table string) ([]ColumnInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectEr != nil {
		return nil, f.inspectEr
	}
	return []ColumnInfo{{Name: "id", DataType: "bigint", ColumnType: "bigint(20)"}, {Name: "created_at", DataType: "datetime", ColumnType: "datetime"},
		{Name: "d", DataType: "date", ColumnType: "date"}, {Name: "ts", DataType: "timestamp"}, {Name: "n", DataType: "int", ColumnType: "int unsigned"},
		{Name: "bad name", DataType: "datetime"}, {Name: "name", DataType: "varchar"}}, nil
}

// ---- 状态 ----

func okTable() *TableState {
	// 今天 2026-10-06 12:00：包含当前时刻的分区 p20261006（边界 10-07）+ 之后 3 个预创建，保留 30 个历史
	return dailyTable("2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10")
}

func statusOf(t *testing.T, e *env, id uint) *StatusView {
	t.Helper()
	v, err := e.svc.Status(ctx, root, id, true)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestStatusStates(t *testing.T) {
	e := newEnv()
	// 正常：已有 4 个未来分区（含当前）
	e.tgt.tables["log.access_log"] = okTable()
	c := e.addConfig(goodInput())
	if v := statusOf(t, e, c.ID); v.State != StateOK || v.Adds != 0 || v.Drops != 0 || v.Partitions != 6 {
		t.Fatalf("正常: %+v", v)
	}
	// 待续建：预创建改为 5，缺 2 个
	in := goodInput()
	in.TbName, in.PreCreate = "need_add", 5
	c2 := e.addConfig(in)
	e.tgt.tables["log.need_add"] = okTable()
	if v := statusOf(t, e, c2.ID); v.State != StatePending || v.Adds != 2 || v.Drops != 0 {
		t.Fatalf("待续建: %+v", v)
	}
	// 待清理：保留 1，历史分区 p20261005 只有 1 个 → 不清理；再加一个更早的
	in = goodInput()
	in.TbName, in.Retain = "need_drop", 1
	c3 := e.addConfig(in)
	e.tgt.tables["log.need_drop"] = dailyTable("2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10")
	if v := statusOf(t, e, c3.ID); v.State != StatePending || v.Adds != 0 || v.Drops != 3 {
		t.Fatalf("待清理: %+v", v)
	}
	// 续建 + 清理同时存在
	in = goodInput()
	in.TbName, in.Retain, in.PreCreate = "both", 1, 6
	c4 := e.addConfig(in)
	e.tgt.tables["log.both"] = dailyTable("2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08")
	if v := statusOf(t, e, c4.ID); v.State != StatePending || v.Adds != 5 || v.Drops != 3 {
		t.Fatalf("两者: %+v", v)
	}
	// 含 MAXVALUE：REORGANIZE，Adds 不计 pmax
	in = goodInput()
	in.TbName = "withmax"
	c5 := e.addConfig(in)
	st := dailyTable("2026-10-06", "2026-10-07")
	st.Partitions = append(st.Partitions, RawPartition{Name: "pmax", Desc: "MAXVALUE"})
	e.tgt.tables["log.withmax"] = st
	if v := statusOf(t, e, c5.ID); v.State != StatePending || !v.Reorganize || v.Adds != 3 {
		t.Fatalf("reorganize: %+v", v)
	}
	// 异常：不是分区表 / 表不存在
	in = goodInput()
	in.TbName = "plain"
	c6 := e.addConfig(in)
	e.tgt.tables["log.plain"] = &TableState{Exists: true, IsBase: true, ColumnType: "datetime", ColumnTypeFull: "datetime"}
	if v := statusOf(t, e, c6.ID); v.State != StateUnpartitioned || !v.CanInit {
		t.Fatalf("非分区表: %+v", v)
	}
	in = goodInput()
	in.TbName = "gone"
	c7 := e.addConfig(in)
	if v := statusOf(t, e, c7.ID); v.State != StateError || !strings.Contains(v.Message, "不存在") {
		t.Fatalf("表不存在: %+v", v)
	}
	// 库名被实例规则禁止 → 异常（不连库）
	in = goodInput()
	in.InstanceID, in.DBName, in.TbName = 2, "secretdb", "t"
	c8 := e.addConfig(in)
	dials := e.tgt.dials
	if v := statusOf(t, e, c8.ID); v.State != StateError || e.tgt.dials != dials {
		t.Fatalf("被禁止的库: %+v dials=%d", v, e.tgt.dials)
	}
}

func TestStatusUnknownUnsupportedAndScope(t *testing.T) {
	e := newEnv()
	c := e.addConfig(goodInput())
	// 连不上 / 读不到 → unknown，不是接口错误
	e.tgt.dialErr = errors.New("dial tcp 10.0.0.1:3306: i/o timeout")
	v, err := e.svc.Status(ctx, root, c.ID, true)
	if err != nil || v.State != StateUnknown || !strings.Contains(v.Message, "i/o timeout") {
		t.Fatalf("连不上: %+v %v", v, err)
	}
	e.tgt.dialErr = nil
	e.tgt.inspectEr = errors.New("Error 1142: SELECT command denied")
	if v, err = e.svc.Status(ctx, root, c.ID, true); err != nil || v.State != StateUnknown || !strings.Contains(v.Message, "denied") {
		t.Fatalf("读不到: %+v %v", v, err)
	}
	e.tgt.inspectEr = nil
	// 不支持的实例类型：不连库
	in := goodInput()
	in.InstanceID, in.TbName = 5, "sdb"
	cs := e.addConfig(in)
	dials := e.tgt.dials
	if v, err = e.svc.Status(ctx, root, cs.ID, false); err != nil || v.State != StateUnsupported || e.tgt.dials != dials {
		t.Fatalf("不支持: %+v %v", v, err)
	}
	// 范围：看不到的实例 / 不存在的配置是错误
	in = goodInput()
	in.InstanceID, in.TbName = 4, "hidden"
	ch := e.addConfig(in)
	if _, err = e.svc.Status(ctx, user(permMenu), ch.ID, false); code(err) != "no_privilege" {
		t.Fatalf("越权: %v", err)
	}
	if _, err = e.svc.Status(ctx, root, 999, false); code(err) != "not_found" {
		t.Fatalf("%v", err)
	}
}

func TestStatusCacheAndInvalidation(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = okTable()
	c := e.addConfig(goodInput())
	now := fixedNow
	e.svc.now = func() time.Time { return now }
	v1, _ := e.svc.Status(ctx, root, c.ID, false)
	d := e.tgt.dials
	v2, _ := e.svc.Status(ctx, root, c.ID, false)
	if !v2.Cached || v1.Cached || e.tgt.dials != d {
		t.Fatalf("应命中缓存: %+v %+v dials %d->%d", v1, v2, d, e.tgt.dials)
	}
	// refresh 绕过缓存
	if v, _ := e.svc.Status(ctx, root, c.ID, true); v.Cached || e.tgt.dials != d+1 {
		t.Fatalf("refresh: %+v", v)
	}
	// 编辑后失效（签名变化 + 主动失效）
	in := goodInput()
	in.PreCreate = 6
	if _, err := e.svc.Update(root, c.ID, in); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.svc.Status(ctx, root, c.ID, false); v.Cached || v.Adds != 3 {
		t.Fatalf("编辑后: %+v", v)
	}
	// 过期
	now = now.Add(statusTTL + time.Second)
	if v, _ := e.svc.Status(ctx, root, c.ID, false); v.Cached {
		t.Fatalf("过期后应重新检查: %+v", v)
	}
	// 未知结果只缓存很短时间
	e.tgt.dialErr = errors.New("down")
	e.svc.invalidateStatus(c.ID)
	e.svc.Status(ctx, root, c.ID, false)
	e.tgt.dialErr = nil
	now = now.Add(statusUnknownTTL + time.Second)
	if v, _ := e.svc.Status(ctx, root, c.ID, false); v.State == StateUnknown {
		t.Fatalf("恢复后应重新检查: %+v", v)
	}
}

// slowTarget 让 Inspect 阻塞到被放行，用来验证并发闸与超时。
type slowTarget struct {
	*fakeTarget
	cur, max int32
	gate     chan struct{}
}

func (s *slowTarget) Inspect(ctx context.Context, db, table, col string) (TableState, error) {
	n := atomic.AddInt32(&s.cur, 1)
	for {
		m := atomic.LoadInt32(&s.max)
		if n <= m || atomic.CompareAndSwapInt32(&s.max, m, n) {
			break
		}
	}
	defer atomic.AddInt32(&s.cur, -1)
	select {
	case <-s.gate:
	case <-ctx.Done():
		return TableState{}, ctx.Err()
	}
	return s.fakeTarget.Inspect(ctx, db, table, col)
}

func TestStatusPerInstanceConcurrencyAndTimeout(t *testing.T) {
	e := newEnv()
	slow := &slowTarget{fakeTarget: e.tgt, gate: make(chan struct{})}
	e.svc.dial = func(context.Context, *model.Instance, engine.Conn) (Target, error) { return slow, nil }
	var ids []uint
	for i := 0; i < 5; i++ {
		in := goodInput()
		in.TbName = "t" + string(rune('a'+i))
		e.tgt.tables["log."+in.TbName] = okTable()
		ids = append(ids, e.addConfig(in).ID)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id uint) { defer wg.Done(); e.svc.Status(ctx, root, id, true) }(id)
	}
	time.Sleep(150 * time.Millisecond)
	if m := atomic.LoadInt32(&slow.cur); m > perInstConc {
		t.Fatalf("同一实例同时检查数 %d 超过上限 %d", m, perInstConc)
	}
	close(slow.gate)
	wg.Wait()
	if slow.max > perInstConc {
		t.Fatalf("峰值 %d 超过上限 %d", slow.max, perInstConc)
	}

	// 超时：永远不放行 → unknown，而不是挂住
	old := statusTimeout
	statusTimeout = 100 * time.Millisecond
	defer func() { statusTimeout = old }()
	slow2 := &slowTarget{fakeTarget: e.tgt, gate: make(chan struct{})}
	e.svc.dial = func(context.Context, *model.Instance, engine.Conn) (Target, error) { return slow2, nil }
	start := time.Now()
	v, err := e.svc.Status(ctx, root, ids[0], true)
	if err != nil || v.State != StateUnknown || time.Since(start) > 3*time.Second {
		t.Fatalf("超时: %+v %v %v", v, err, time.Since(start))
	}
}

func TestSummary(t *testing.T) {
	e := newEnv()
	a := e.addConfig(goodInput())
	in := goodInput()
	in.TbName, in.IsEnable = "off", false
	e.addConfig(in)
	in = goodInput()
	in.InstanceID, in.TbName = 5, "sdb"
	e.addConfig(in)
	in = goodInput()
	in.InstanceID, in.TbName = 4, "hidden"
	e.addConfig(in)
	s, err := e.svc.Summary(root)
	if err != nil || s.Total != 4 || s.Enabled != 3 || s.Supported != 3 || len(s.Items) != 4 || s.Items[0].ID != a.ID {
		t.Fatalf("root: %+v %v", s, err)
	}
	s, err = e.svc.Summary(user(permMenu))
	if err != nil || s.Total != 3 {
		t.Fatalf("普通用户只统计可见实例: %+v %v", s, err)
	}
}

// ---- 一键启用 / 停用 ----

func TestSetEnabled(t *testing.T) {
	e := newEnv()
	c := e.addConfig(goodInput())
	if _, err := e.svc.SetEnabled(user(permMenu), c.ID, false); code(err) != "forbidden" {
		t.Fatalf("只读用户不能停用: %v", err)
	}
	v, err := e.svc.SetEnabled(user(permMenu, permChange), c.ID, false)
	if err != nil || v.IsEnable {
		t.Fatalf("停用: %+v %v", v, err)
	}
	got, _ := e.repo.GetConfig(c.ID)
	if got.IsEnable || got.Interval != 1 || got.TbName != "access_log" || got.LastestTime.IsZero() {
		t.Fatalf("其他字段不得变化: %+v", got)
	}
	if acts := e.repo.auditActions(); len(acts) != 1 || acts[0] != "分区维护-停用配置" {
		t.Fatalf("审计: %v", acts)
	}
	// 幂等：再停用一次不重复写审计
	if _, err := e.svc.SetEnabled(root, c.ID, false); err != nil || len(e.repo.audits) != 1 {
		t.Fatalf("幂等: %v %v", err, e.repo.auditActions())
	}
	if v, err = e.svc.SetEnabled(root, c.ID, true); err != nil || !v.IsEnable || len(e.repo.audits) != 2 || e.repo.audits[1].Action != "分区维护-启用配置" {
		t.Fatalf("启用: %+v %v %v", v, err, e.repo.auditActions())
	}
	// 范围与类型
	in := goodInput()
	in.InstanceID, in.TbName = 4, "hidden"
	ch := e.addConfig(in)
	if _, err := e.svc.SetEnabled(user(permMenu, permChange), ch.ID, true); code(err) != "no_privilege" {
		t.Fatalf("越权: %v", err)
	}
	in.InstanceID, in.TbName = 5, "sdb"
	cs := e.addConfig(in)
	if _, err := e.svc.SetEnabled(root, cs.ID, false); code(err) != "unsupported_engine" {
		t.Fatalf("不支持的实例: %v", err)
	}
	if _, err := e.svc.SetEnabled(root, 999, true); code(err) != "not_found" {
		t.Fatalf("%v", err)
	}
}

// ---- 日志：来源 / 操作人 / 状态 ----

func TestSummarizeLog(t *testing.T) {
	mk := func(s string) []map[string]any {
		var a []map[string]any
		if err := json.Unmarshal([]byte(s), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	cases := []struct{ in, src, op, st string }{
		{`[{"sql_text":"a","status":"success","source":"manual","operator":"bob"}]`, "manual", "bob", "success"},
		{`[{"sql_text":"a","status":"success","source":"auto","operator":"system"},{"sql_text":"b","status":"failed","error":"x","source":"auto","operator":"system"},{"sql_text":"c","status":"skipped","source":"auto"}]`, "auto", "system", "partial"},
		{`[{"sql_text":"a","status":"failed","error":"x","source":"manual","operator":"bob"},{"sql_text":"b","status":"skipped","source":"manual"}]`, "manual", "bob", "failed"},
		{`[{"sql_text":"ALTER TABLE x DROP PARTITION p0"}]`, "external", "", "unknown"},
		{`[{"sql_text":"a","error":"boom"}]`, "external", "", "failed"},
		{`[{"sql_text":"a","status":"ok"}]`, "external", "", "success"},
		{`[]`, "external", "", "unknown"},
	}
	for _, c := range cases {
		src, op, st := summarizeLog(mk(c.in))
		if src != c.src || op != c.op || st != c.st {
			t.Errorf("%s => %s/%s/%s，期望 %s/%s/%s", c.in, src, op, st, c.src, c.op, c.st)
		}
	}
	// parseLog 带出这些字段；非 JSON 也有默认值
	v := parseLog(Log{ID: 1, ExecResult: `[{"sql_text":"a","status":"success","source":"auto","operator":"system"}]`})
	if v.Source != "auto" || v.Operator != "system" || v.Status != "success" {
		t.Fatalf("%+v", v)
	}
	if v = parseLog(Log{ID: 2, ExecResult: "plain"}); v.Source != "external" || v.Status != "unknown" {
		t.Fatalf("%+v", v)
	}
}

func TestGlobalLogs(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10")
	a := e.addConfig(goodInput())
	in := goodInput()
	in.TbName = "other"
	b := e.addConfig(in)
	in.InstanceID, in.TbName = 4, "hidden"
	h := e.addConfig(in) // 别的资源组实例
	// 手工执行一次（带 source=manual）
	if _, err := e.svc.Run(ctx, root, a.ID, RunOptions{ConfirmDrops: []string{"p20261001"}}); err != nil && code(err) != "confirm_required" {
		t.Fatal(err)
	}
	e.repo.InsertLog(a.CnfID, `[{"sql_text":"ADD","status":"success","source":"manual","operator":"bob"}]`, fixedNow)
	e.repo.InsertLog(a.CnfID, `[{"sql_text":"ADD","status":"success","source":"auto","operator":"system"}]`, fixedNow)
	e.repo.InsertLog(b.CnfID, `[{"sql_text":"ALTER TABLE x DROP PARTITION p0"}]`, fixedNow)
	e.repo.InsertLog(h.CnfID, `[{"sql_text":"secret","source":"manual"}]`, fixedNow)

	rows, total, opts, err := e.svc.GlobalLogs(user(permMenu), GlobalLogQuery{})
	if err != nil || total != 3 || len(rows) != 3 || len(opts) != 2 {
		t.Fatalf("普通用户只看可见实例的日志: %d %d %v", total, len(opts), err)
	}
	if rows[0].Source != "external" || rows[0].Table != "log.other" || rows[0].InstanceName != "prod-mysql-01" || rows[0].ConfigID != b.ID {
		t.Fatalf("最新在前且带配置信息: %+v", rows[0])
	}
	for _, r := range rows {
		if strings.Contains(r.Items[0].SQL, "secret") {
			t.Fatal("泄露了别的资源组的日志")
		}
	}
	for src, want := range map[string]int{"manual": 1, "auto": 1, "external": 1} {
		if _, n, _, err := e.svc.GlobalLogs(user(permMenu), GlobalLogQuery{Source: src}); err != nil || n != int64(want) {
			t.Errorf("来源 %s: %d %v", src, n, err)
		}
	}
	if _, n, _, _ := e.svc.GlobalLogs(root, GlobalLogQuery{ConfigID: a.ID, Source: "manual"}); n != 1 {
		t.Errorf("按配置 × 来源: %d", n)
	}
	if _, _, _, err := e.svc.GlobalLogs(root, GlobalLogQuery{Source: "x"}); code(err) != "bad_request" {
		t.Errorf("非法来源: %v", err)
	}
	if _, _, _, err := e.svc.GlobalLogs(user(permMenu), GlobalLogQuery{ConfigID: h.ID}); code(err) != "not_found" {
		t.Errorf("不可见配置: %v", err)
	}
	// 分页
	if rows, total, _, _ = e.svc.GlobalLogs(user(permMenu), GlobalLogQuery{Limit: 2, Offset: 2}); total != 3 || len(rows) != 1 {
		t.Errorf("分页: %d %d", total, len(rows))
	}
}

// ---- 库表级联 ----

func TestCatalog(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = okTable()
	bob := user(permMenu, permAdd)
	dbs, err := e.svc.Databases(ctx, bob, 1)
	if err != nil || strings.Join(dbs, ",") != "log,secretdb" {
		t.Fatalf("库列表应去掉系统库与非法名: %v %v", dbs, err)
	}
	// 实例 2 的库名过滤规则禁止 ^secret
	e.tgt.tables["secretdb.t"] = okTable()
	dbs, _ = e.svc.Databases(ctx, root, 2)
	for _, d := range dbs {
		if d == "secretdb" {
			t.Fatal("被实例规则禁止的库不应出现")
		}
	}
	tbs, err := e.svc.Tables(ctx, bob, 1, "log")
	if err != nil || len(tbs) != 2 {
		t.Fatalf("%v %v", tbs, err)
	}
	for _, tb := range tbs {
		if tb.Name == "access_log" && (!tb.Partitioned || tb.Method != "RANGE COLUMNS") {
			t.Fatalf("分区表标注: %+v", tb)
		}
		if tb.Name == "plain_tbl" && tb.Partitioned {
			t.Fatalf("普通表: %+v", tb)
		}
	}
	cols, err := e.svc.Columns(ctx, bob, 1, "log", "access_log")
	if err != nil || len(cols) != 4 {
		t.Fatalf("只列 datetime/date/int/bigint: %+v %v", cols, err)
	}
	want := map[string]string{"id": "bigint", "created_at": "datetime", "d": "date", "n": "int"}
	for _, c := range cols {
		if want[c.Name] != c.ColType {
			t.Fatalf("类型自动带出: %+v", c)
		}
	}
	// 校验与权限
	if _, err := e.svc.Tables(ctx, bob, 1, "mysql"); code(err) != "bad_request" {
		t.Fatalf("系统库: %v", err)
	}
	if _, err := e.svc.Tables(ctx, bob, 1, "a`b"); code(err) != "bad_request" {
		t.Fatalf("非法库名: %v", err)
	}
	if _, err := e.svc.Tables(ctx, bob, 2, "secretdb"); code(err) != "db_not_allowed" {
		t.Fatalf("实例 2 禁止 ^secret 开头的库: %v", err)
	}
}

// 表列表里标出“已有分区维护配置”的表：只看同一实例、同一个库（库名 / 表名不区分大小写，同库默认校验规则下的唯一键一致）。
func TestCatalogConfiguredFlag(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = okTable()
	e.tgt.tables["log.Orders"] = okTable()
	in := goodInput() // 实例 1 · log.access_log
	in.DBName = "LOG"
	e.addConfig(in)
	other := goodInput() // 同一实例的另一个库、另一个实例上的同名库表：都不影响实例 1 的 log 库
	other.DBName, other.TbName = "other", "Orders"
	e.addConfig(other)
	other = goodInput()
	other.InstanceID, other.TbName = 4, "plain_tbl"
	e.addConfig(other)

	tbs, err := e.svc.Tables(ctx, user(permMenu, permAdd), 1, "log")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tb := range tbs {
		got[tb.Name] = tb.Configured
	}
	if len(got) != 3 || !got["access_log"] || got["Orders"] || got["plain_tbl"] {
		t.Fatalf("只有 access_log 已有配置: %+v", got)
	}
	// 前端按这个键名读取
	b, _ := json.Marshal(TableInfo{Name: "x", Configured: true})
	if !strings.Contains(string(b), `"configured":true`) {
		t.Fatalf("JSON 键名: %s", b)
	}
	if b, _ = json.Marshal(TableInfo{Name: "x"}); strings.Contains(string(b), "configured") {
		t.Fatalf("没有配置时不输出该键: %s", b)
	}
}

func TestCatalogRefusals(t *testing.T) {
	e := newEnv()
	if _, err := e.svc.Databases(ctx, user(permMenu), 1); code(err) != "forbidden" {
		t.Fatalf("只读用户不能浏览库表: %v", err)
	}
	if _, err := e.svc.Databases(ctx, user(permMenu, permChange), 4); code(err) != "no_privilege" {
		t.Fatalf("别的资源组实例: %v", err)
	}
	if _, err := e.svc.Databases(ctx, root, 5); code(err) != "unsupported_engine" {
		t.Fatalf("mssql: %v", err)
	}
	e.tgt.dialErr = errors.New("refused")
	if _, err := e.svc.Databases(ctx, root, 1); code(err) != "target_error" {
		t.Fatalf("连不上: %v", err)
	}
	e.tgt.dialErr = nil
	e.tgt.inspectEr = errors.New("denied")
	if _, err := e.svc.Tables(ctx, root, 1, "log"); code(err) != "target_error" {
		t.Fatalf("读取失败: %v", err)
	}
	// 不支持浏览的 Target：501
	e.svc.dial = func(context.Context, *model.Instance, engine.Conn) (Target, error) { return noCatalog{}, nil }
	if _, err := e.svc.Databases(ctx, root, 1); code(err) != "unsupported_engine" {
		t.Fatalf("无 Catalog: %v", err)
	}
}

type noCatalog struct{}

func (noCatalog) Inspect(context.Context, string, string, string) (TableState, error) {
	return TableState{}, nil
}
func (noCatalog) Exec(context.Context, []string) []ExecResult { return nil }
func (noCatalog) Close() error                                { return nil }
