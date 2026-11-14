// @author: kevinkelly
// @license: Apache Licence
// @file: redis_test.go
// @time: 2026/10/28

package sqlflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine/redis/redistest"
	"dbtool/internal/model"
	"dbtool/internal/redisrule"
	"dbtool/internal/review"
	"dbtool/internal/sqlflow"
)

// ---- Redis 工单上线 ----
//
// 在 setup 的基础上加一个 Redis 实例（id 31，带 can_write，属于资源组 1），指向进程内的假 Redis（redistest.KV）；
// 服务用默认的引擎创建函数，所以执行走的是真实的写客户端（RESP 协议）。规则放在内存里，测试里可以随时改。

type redisEnv struct {
	*env
	kv    *redistest.KV
	srv   *redistest.Server
	rules *redisrule.MemRepo
}

func rule(name, cmds, pats string, mods ...func(*redisrule.Rule)) redisrule.Rule {
	r := redisrule.Rule{Name: name, Enabled: true, Commands: strings.Split(cmds, ","), KeyPatterns: strings.Split(pats, ","), MaxKeys: 100}
	for _, m := range mods {
		m(&r)
	}
	return r
}

func setupRedis(t *testing.T, rules ...redisrule.Rule) *redisEnv {
	t.Helper()
	e := setup(t)
	kv := redistest.NewKV()
	srv, err := kv.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	tag := []model.InstanceTag{{TagCode: "can_write", Active: true}}
	e.repo.Instances = append(e.repo.Instances, model.Instance{ID: 31, InstanceName: "redis-prod", DBType: "redis", Host: srv.Host(), Port: srv.Port(), Tags: tag})
	e.repo.InstRGs[31] = []uint{1}
	s := sqlflow.NewService(e.repo, sqlflow.Options{Notifier: e.cap}) // 默认引擎创建函数：redis 引擎已由 sqlflow 导入注册
	s.SetAsync(func(f func()) { f() })
	s.SetSpawn(func(f func()) { f() })
	s.SetClock(func() time.Time { return e.now })
	mem := redisrule.NewMemRepo(rules...)
	s.SetRedisRules(redisrule.SourceOf(mem))
	e.svc = s
	return &redisEnv{env: e, kv: kv, srv: srv, rules: mem}
}

func rreq(cmds string) sqlflow.SubmitReq {
	return sqlflow.SubmitReq{Name: "清理缓存", InstanceID: 31, DBName: "3", GroupName: "研发组", SQL: cmds}
}

func (r *redisEnv) submit(t *testing.T, cmds string) uint {
	t.Helper()
	res, err := r.svc.Submit(context.Background(), dev, rreq(cmds))
	if err != nil {
		t.Fatalf("提交: %v", err)
	}
	return res.WorkflowID
}

func (r *redisEnv) run(t *testing.T, id uint) {
	t.Helper()
	if _, err := r.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto}); err != nil {
		t.Fatalf("执行: %v", err)
	}
	r.svc.Wait()
}

func (r *redisEnv) execRows(t *testing.T, id uint) []review.Row {
	t.Helper()
	rows, err := review.ParseRows(r.repo.Contents[id].ExecuteResult)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// 写命令（不含 AUTH / SELECT）发到过假服务端的列表。
func (r *redisEnv) writes() []string {
	var out []string
	for _, c := range r.srv.Commands() {
		switch strings.ToUpper(c[0]) {
		case "AUTH", "SELECT", "PING", "CONFIG", "INFO":
		default:
			out = append(out, strings.Join(c, " "))
		}
	}
	return out
}

func (r *redisEnv) audits() []string {
	var out []string
	for _, a := range r.repo.LifecycleAudits {
		out = append(out, a.Action)
	}
	return out
}

func TestRedisPreviewNoRulesRejectsAll(t *testing.T) {
	r := setupRedis(t)
	res, err := r.svc.Preview(context.Background(), dev, 31, "3", "DEL cache:a\nSET cache:b v EX 5", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != sqlflow.SourceRedis || len(res.Rows) != 2 || res.ErrorCount != 2 || !strings.Contains(res.Rows[0].ErrorMessage, "没有任何启用的规则") {
		t.Errorf("没有规则应全部拒绝: %+v", res)
	}
	if !strings.Contains(res.Notice, "没有自动回滚") && !strings.Contains(res.Notice, "没有事务回滚") {
		t.Errorf("提示: %s", res.Notice)
	}
}

func TestRedisPreviewMixedLines(t *testing.T) {
	r := setupRedis(t, rule("清理", "DEL,UNLINK", "cache:*"), rule("写缓存", "SET,SETEX", "cache:*"))
	cmds := "DEL cache:a cache:b\n\nSET cache:x 1\nDEL other:a\nFLUSHALL\nSET \"cache:bad\n"
	res, err := r.svc.Preview(context.Background(), dev, 31, "db3", cmds, false) // db3 与 3 都接受
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 5 {
		t.Fatalf("空行忽略: %+v", res.Rows)
	}
	want := []struct {
		id    review.Flex
		level review.Flex
		sub   string
	}{{1, 0, "命中规则"}, {3, 1, "没有设置过期时间"}, {4, 2, "不匹配规则"}, {5, 2, "硬禁止"}, {6, 2, "语法错误"}}
	for i, w := range want {
		row := res.Rows[i]
		if row.ID != w.id || row.ErrLevel != w.level || !strings.Contains(row.ErrorMessage, w.sub) {
			t.Errorf("第 %d 行: %+v，期望 id=%d level=%d 含 %q", i, row, w.id, w.level, w.sub)
		}
	}
	if res.WarningCount != 1 || res.ErrorCount != 3 || res.SyntaxType != sqlflow.SyntaxDML {
		t.Errorf("统计: %+v", res.Set)
	}
	if res.Rows[0].AffectedRows != 2 {
		t.Errorf("涉及键数: %d", res.Rows[0].AffectedRows)
	}
}

func TestRedisPreviewLimitsAndModes(t *testing.T) {
	r := setupRedis(t, rule("清理", "DEL", "cache:*"))
	ctx := context.Background()
	var sb strings.Builder
	for i := 0; i < 201; i++ {
		sb.WriteString("DEL cache:a\n")
	}
	if _, err := r.svc.Preview(ctx, dev, 31, "3", sb.String(), false); code(err) != "bad_request" {
		t.Errorf("超过 200 条: %v", err)
	}
	if _, err := r.svc.Preview(ctx, dev, 31, "x", "DEL cache:a", false); code(err) != "bad_request" {
		t.Errorf("库不是编号: %v", err)
	}
	if _, err := r.svc.Preview(ctx, dev, 31, "3", "  \n ", false); code(err) != "bad_request" {
		t.Errorf("空命令: %v", err)
	}
	if _, err := r.svc.Preview(ctx, root, 31, "3", "DEL cache:a", true); code(err) != "bad_request" {
		t.Errorf("Redis 不支持跳过检查: %v", err)
	}
	if _, err := r.svc.PreviewBatch(ctx, dev, []uint{31}, "3", "DEL cache:a"); err == nil {
		t.Error("Redis 不支持批量上线")
	}
	if _, err := r.svc.PreviewExport(ctx, dev, []uint{31}, false, "3", "select 1"); err == nil {
		t.Error("Redis 不支持数据导出")
	}
}

func TestRedisPreviewTableMissingAndRuleError(t *testing.T) {
	r := setupRedis(t)
	r.rules.NotReady = true // 规则表不存在：按没有规则处理，不报 500
	res, err := r.svc.Preview(context.Background(), dev, 31, "3", "DEL cache:a", false)
	if err != nil || res.ErrorCount != 1 || !strings.Contains(res.Notice, "170_redis_rules.sql") {
		t.Errorf("表不存在: %+v %v", res, err)
	}
	r.rules.NotReady = false
	r.rules.FailList = errors.New("db down")
	if _, err := r.svc.Preview(context.Background(), dev, 31, "3", "DEL cache:a", false); code(err) != "redis_rules" {
		t.Errorf("读规则失败应是明确的错误: %v", err)
	}
	// 没有装配规则来源（如 demo）：同样全部拒绝
	s := sqlflow.NewService(r.repo, sqlflow.Options{Notifier: r.cap})
	res, err = s.Preview(context.Background(), dev, 31, "3", "DEL cache:a", false)
	if err != nil || res.ErrorCount != 1 {
		t.Errorf("没有规则来源: %+v %v", res, err)
	}
}

func TestRedisSubmitRejectsAnyBadLine(t *testing.T) {
	r := setupRedis(t, rule("清理", "DEL", "cache:*"))
	_, err := r.svc.Submit(context.Background(), dev, rreq("DEL cache:a\nDEL other:b"))
	if code(err) != "bad_request" || !strings.Contains(err.Error(), "不匹配") {
		t.Errorf("有一行被拒绝就不能提交: %v", err)
	}
	if len(r.repo.Workflows) != 0 {
		t.Error("被拒绝的提交不应产生工单（也不是“自动审核不通过”的工单）")
	}
	// 前端传来的检查结果不可信：服务端提交时自己再检查（这里直接绕过 Preview 提交违规命令即是）
	if _, err := r.svc.Submit(context.Background(), dev, rreq("FLUSHALL")); code(err) != "bad_request" {
		t.Errorf("硬禁止: %v", err)
	}
	// 范围外的实例 / 库：规则限定 db5，提交到 db3 被拒
	r.rules = redisrule.NewMemRepo(rule("只允许db5", "DEL", "cache:*", func(x *redisrule.Rule) { x.DBs = []int{5} }))
	r.svc.SetRedisRules(redisrule.SourceOf(r.rules))
	if _, err := r.svc.Submit(context.Background(), dev, rreq("DEL cache:a")); code(err) != "bad_request" || !strings.Contains(err.Error(), "不适用于当前实例") {
		t.Errorf("范围外: %v", err)
	}
	rq := rreq("DEL cache:a")
	rq.DBName = "db5"
	if _, err := r.svc.Submit(context.Background(), dev, rq); err != nil {
		t.Errorf("db5 在范围内: %v", err)
	}
	// 跳过检查、导出、批量
	rq = rreq("DEL cache:a")
	rq.SkipCheck = true
	if _, err := r.svc.Submit(context.Background(), root, rq); err == nil {
		t.Error("Redis 工单不允许跳过检查")
	}
	rq = rreq("DEL cache:a")
	rq.BatchInstanceIDs = []uint{31}
	if _, err := r.svc.Submit(context.Background(), dev, rq); err == nil {
		t.Error("Redis 工单不允许批量")
	}
}

func TestRedisFullFlow(t *testing.T) {
	r := setupRedis(t, rule("清理", "DEL", "cache:*"), rule("写缓存", "SET", "cache:*"))
	r.kv.SetString(3, "cache:a", "old")
	r.kv.SetString(3, "cache:b", "old")
	r.kv.SetString(3, "keep:me", "x")
	longVal := strings.Repeat("v", 300)
	id := r.submit(t, "DEL cache:a cache:b\nSET cache:new "+longVal+" EX 60\nSET cache:token:1 hunter2 EX 60")
	w := r.repo.Workflows[id]
	if w.Status != model.WfManReviewing || w.DBName != "3" || w.IsBackup || w.SyntaxType != sqlflow.SyntaxDML {
		t.Errorf("工单: %+v", w)
	}
	if rows, _ := review.ParseRows(r.repo.Contents[id].ReviewContent); len(rows) != 3 || rows[0].ErrLevel != 0 {
		t.Errorf("检查结果应随工单保存: %+v", rows)
	}
	if len(r.writes()) != 0 {
		t.Fatalf("提交前不应执行任何写命令: %v", r.writes())
	}
	r.approve(t, id)
	r.run(t, id)

	if r.status(id) != model.WfFinish {
		t.Fatalf("状态: %s %s", r.status(id), r.repo.Contents[id].ExecuteResult)
	}
	rows := r.execRows(t, id)
	if len(rows) != 3 || rows[0].ErrorMessage != "(integer) 2" || rows[0].AffectedRows != 2 || rows[1].ErrorMessage != "OK" || rows[0].StageStatus != "Execute Successfully" {
		t.Errorf("执行结果: %+v", rows)
	}
	for _, row := range rows {
		if row.ID == 0 || row.ExecuteTime == "" {
			t.Errorf("每条记录要有行号与耗时: %+v", row)
		}
	}
	// 真的写进了第 3 库，且没碰别的键
	if r.kv.Exists(3, "cache:a") || r.kv.Exists(3, "cache:b") || !r.kv.Exists(3, "keep:me") {
		t.Errorf("第 3 库的键: %v", r.kv.Keys(3))
	}
	if got, ok := r.kv.Get(3, "cache:new"); !ok || got != longVal || r.kv.TTL(3, "cache:new") != 60000 {
		t.Errorf("SET 结果: %v %v", ok, r.kv.TTL(3, "cache:new"))
	}
	selected := false
	for _, c := range r.srv.Commands() {
		if len(c) == 2 && strings.EqualFold(c[0], "SELECT") && c[1] == "3" {
			selected = true
		}
	}
	if !selected {
		t.Error("应在连接时 SELECT 3")
	}
	// 审计：检查摘要 + 提交 + 执行；执行审计里值参数被截断、疑似密码的键不记录值
	acts := r.audits()
	for _, want := range []string{"Redis上线-提交检查", "SQL上线-提交", "Redis上线-执行"} {
		found := false
		for _, a := range acts {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("缺少审计 %s：%v", want, acts)
		}
	}
	for _, a := range r.repo.LifecycleAudits {
		if a.Action != "Redis上线-执行" {
			continue
		}
		if strings.Contains(a.ExtraInfo, longVal) || strings.Contains(a.ExtraInfo, "hunter2") || !strings.Contains(a.ExtraInfo, "DEL cache:a cache:b") || !strings.Contains(a.ExtraInfo, "***") {
			t.Errorf("执行审计的命令摘要: %s", a.ExtraInfo)
		}
		if c := audit.Classify(a.Action, a.ExtraInfo); c.Code != "sqlworkflow.execute_group" {
			t.Errorf("审计分类: %+v", c)
		}
	}
}

func TestRedisExecStopsOnError(t *testing.T) {
	r := setupRedis(t, rule("写", "SET,INCR,DEL", "cache:*"))
	r.kv.Fail["INCR"] = "ERR value is not an integer or out of range"
	id := r.submit(t, "SET cache:1 a EX 5\nINCR cache:2\nDEL cache:1\nSET cache:3 c EX 5")
	r.approve(t, id)
	r.run(t, id)
	if r.status(id) != model.WfException {
		t.Fatalf("遇错应是“执行有异常”: %s", r.status(id))
	}
	rows := r.execRows(t, id)
	if len(rows) != 4 || rows[0].ErrLevel != 0 || rows[1].ErrLevel != review.LevelError || !strings.Contains(rows[1].ErrorMessage, "not an integer") {
		t.Fatalf("执行结果: %+v", rows)
	}
	for _, row := range rows[2:] {
		if row.StageStatus != "未执行" || row.Stage != "SKIPPED" {
			t.Errorf("出错之后的命令应标“未执行”: %+v", row)
		}
	}
	if _, ok := r.kv.Get(3, "cache:1"); !ok || r.kv.Exists(3, "cache:3") {
		t.Error("出错前的命令已生效且不回滚；出错后的命令不应执行")
	}
	for _, w := range r.writes() {
		if strings.HasPrefix(w, "DEL") || strings.Contains(w, "cache:3") {
			t.Errorf("出错后不应再发命令: %s", w)
		}
	}
}

func TestRedisRulesChangedAfterApproval(t *testing.T) {
	r := setupRedis(t, rule("清理", "DEL", "cache:*"))
	r.kv.SetString(3, "cache:a", "old")
	id := r.submit(t, "DEL cache:a")
	r.approve(t, id)
	// 审批期间管理员停用了规则：执行时以当前规则为准，拒绝执行，工单显示原因
	list, _ := r.rules.List()
	list[0].Enabled = false
	if err := r.rules.Update(&list[0]); err != nil {
		t.Fatal(err)
	}
	r.run(t, id)
	if r.status(id) != model.WfException {
		t.Fatalf("状态: %s", r.status(id))
	}
	rows := r.execRows(t, id)
	if len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "复检未通过") || !strings.Contains(rows[0].ErrorMessage, "没有任何启用的规则") {
		t.Errorf("应显示规则复检的原因: %+v", rows)
	}
	if !r.kv.Exists(3, "cache:a") || len(r.writes()) != 0 {
		t.Errorf("复检不通过不能执行任何命令: %v", r.writes())
	}
}

func TestRedisRecheckStopsWholeBatch(t *testing.T) {
	r := setupRedis(t, rule("清理", "DEL", "cache:*"), rule("写", "SET", "tmp:*"))
	r.kv.SetString(3, "cache:a", "old")
	id := r.submit(t, "DEL cache:a\nSET tmp:b v EX 5")
	r.approve(t, id)
	// 规则改了：第 2 行不再通过。整单复检，第 1 行也不执行
	list, _ := r.rules.List()
	list[1].KeyPatterns = []string{"other:*"}
	_ = r.rules.Update(&list[1])
	r.run(t, id)
	rows := r.execRows(t, id)
	if len(rows) != 2 || rows[0].StageStatus != "未执行" || rows[1].ErrLevel != review.LevelError || !strings.Contains(rows[1].ErrorMessage, "不匹配") {
		t.Errorf("复检结果: %+v", rows)
	}
	if !r.kv.Exists(3, "cache:a") || len(r.writes()) != 0 {
		t.Errorf("一条都不应执行: %v", r.writes())
	}
}

func TestRedisRecordBefore(t *testing.T) {
	r := setupRedis(t, rule("改缓存", "SET,DEL", "cache:*", func(x *redisrule.Rule) { x.RecordBefore = true }), rule("改哈希", "HSET", "user:*"))
	r.kv.SetString(3, "cache:a", "old-value")
	r.kv.SetHash(3, "user:1", map[string]string{"name": "n"})
	id := r.submit(t, "SET cache:a new EX 5\nDEL cache:gone\nHSET user:1 name x")
	r.approve(t, id)
	r.run(t, id)
	rows := r.execRows(t, id)
	if len(rows) != 3 || !strings.Contains(rows[0].Before, `值 "old-value"`) || !strings.Contains(rows[1].Before, "执行前不存在") {
		t.Fatalf("执行前值: %+v", rows)
	}
	if rows[2].Before != "" {
		t.Errorf("规则没勾选“记录执行前值”就不记录: %q", rows[2].Before)
	}
	if got, _ := r.kv.Get(3, "cache:a"); got != "new" {
		t.Errorf("命令仍应执行: %q", got)
	}
}

func TestRedisDetailAndOptions(t *testing.T) {
	r := setupRedis(t, rule("清理", "DEL", "cache:*"))
	id := r.submit(t, "DEL cache:a")
	d, err := r.svc.Get(dev, id)
	if err != nil || !d.IsRedis || d.DBType != "redis" || len(d.ReviewRows) != 1 {
		t.Fatalf("详情: %+v %v", d, err)
	}
	r.approve(t, id)
	d, _ = r.svc.Get(dev, id)
	if len(d.Executors) != 1 || d.Executors[0].Value != sqlflow.ExecNative || d.DefaultExecutor != sqlflow.ExecNative {
		t.Errorf("Redis 只有原生执行: %+v", d.Executors)
	}
	// Redis 不经过 goInception
	if _, err := r.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeAuto, Executor: sqlflow.ExecGoInception}); err == nil {
		t.Error("Redis 不能选 goInception 执行")
	}
	// 提交页的实例选项：Redis 出现在上线实例里，不出现在导出实例里
	o, err := r.svc.SubmitOptions(dev)
	if err != nil {
		t.Fatal(err)
	}
	has := func(list []sqlflow.InstanceOption) bool {
		for _, i := range list {
			if i.ID == 31 {
				return true
			}
		}
		return false
	}
	if !has(o.Instances[1]) || has(o.ExportInstances[1]) {
		t.Errorf("实例选项: %+v / %+v", o.Instances[1], o.ExportInstances[1])
	}
	// 库列表：纯数字编号
	dbs, err := r.svc.Databases(context.Background(), dev, 31)
	if err != nil || len(dbs) != 16 || dbs[0] != "0" || dbs[15] != "15" {
		t.Errorf("库列表: %v %v", dbs, err)
	}
}

func TestRedisManualConfirmStillWorks(t *testing.T) {
	// “确认手工执行结束”是通用生命周期，不依赖规则
	r := setupRedis(t, rule("清理", "DEL", "cache:*"))
	id := r.submit(t, "DEL cache:a")
	r.approve(t, id)
	if _, err := r.svc.Execute(context.Background(), dev, id, sqlflow.ExecuteReq{Mode: sqlflow.ModeManual}); err != nil {
		t.Fatal(err)
	}
	if r.status(id) != model.WfFinish {
		t.Errorf("状态: %s", r.status(id))
	}
	if len(r.writes()) != 0 {
		t.Error("手工确认不应发命令")
	}
}

func TestRedisScheduledRun(t *testing.T) {
	r := setupRedis(t, rule("清理", "DEL", "cache:*"))
	r.kv.SetString(3, "cache:a", "old")
	id := r.submit(t, "DEL cache:a")
	r.approve(t, id)
	at := r.now.Add(time.Hour)
	if _, err := r.svc.Schedule(dev, id, at, ""); err != nil {
		t.Fatal(err)
	}
	r.svc.SetClock(func() time.Time { return at.Add(time.Minute) })
	if n := r.svc.RunDue(); n != 1 {
		t.Fatalf("到点应执行 1 张: %d", n)
	}
	r.svc.Wait()
	if r.status(id) != model.WfFinish || r.kv.Exists(3, "cache:a") {
		t.Errorf("定时执行: %s", r.status(id))
	}
	// 定时任务触发的执行审计，操作人是“system”
	found := false
	for _, a := range r.repo.LifecycleAudits {
		if a.Action == "Redis上线-执行" && a.UserName == "system" {
			found = true
		}
	}
	if !found {
		t.Errorf("定时执行审计: %v", r.audits())
	}
}

func TestRedisDoesNotAffectMySQL(t *testing.T) {
	r := setupRedis(t)
	// MySQL 实例的检查走原有路径（未配 goInception 降级为原生检查），与 Redis 规则无关
	res, err := r.svc.Preview(context.Background(), dev, 1, "shop", "update t set a=1", false)
	if err != nil || res.Source != sqlflow.SourceNative || res.ErrorCount != 0 {
		t.Errorf("MySQL 检查: %+v %v", res, err)
	}
	if got := fmt.Sprint(r.svc.BatchTypes()); got != "[mssql]" {
		t.Errorf("批量类型默认值不能变: %s", got)
	}
}
