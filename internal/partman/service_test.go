// @author: kevinkelly
// @license: Apache Licence
// @file: service_test.go
// @time: 2026/10/28

package partman

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func code(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	if err == nil {
		return ""
	}
	return "?" + err.Error()
}

var ctx = context.Background()

func TestCreateValidationAndPermission(t *testing.T) {
	e := newEnv()
	mod := func(f func(*ConfigInput)) ConfigInput { in := goodInput(); f(&in); return in }

	// 权限：只有菜单权限不能新增
	if _, err := e.svc.Create(user("partman.view"), goodInput()); code(err) != "forbidden" {
		t.Fatalf("无新增权限应 403: %v", err)
	}
	// 实例范围：bob 看不到实例 4
	if _, err := e.svc.Create(user(permAdd), mod(func(in *ConfigInput) { in.InstanceID = 4 })); code(err) != "no_privilege" {
		t.Fatalf("不在资源组的实例应拒绝: %v", err)
	}
	if _, err := e.svc.Create(root, mod(func(in *ConfigInput) { in.InstanceID = 99 })); code(err) != "not_found" {
		t.Fatalf("实例不存在: %v", err)
	}
	bad := map[string]ConfigInput{
		"未选实例":   mod(func(in *ConfigInput) { in.InstanceID = 0 }),
		"库名含反引号": mod(func(in *ConfigInput) { in.DBName = "log`" }),
		"表名含空格":  mod(func(in *ConfigInput) { in.TbName = "a b" }),
		"表名含分号":  mod(func(in *ConfigInput) { in.TbName = "t;drop table x" }),
		"字段为空":   mod(func(in *ConfigInput) { in.Column = " " }),
		"表名超长":   mod(func(in *ConfigInput) { in.TbName = strings.Repeat("a", 65) }),
		"系统库":    mod(func(in *ConfigInput) { in.DBName = "mysql" }),
		"系统库大小写": mod(func(in *ConfigInput) { in.DBName = "Information_Schema" }),
		"未知字段类型": mod(func(in *ConfigInput) { in.ColType = "timestamp" }),
		"未知周期":   mod(func(in *ConfigInput) { in.Period = "quarter" }),
		"跨度为 0":  mod(func(in *ConfigInput) { in.Interval = 0 }),
		"跨度过大":   mod(func(in *ConfigInput) { in.Interval = 256 }),
		"保留为负":   mod(func(in *ConfigInput) { in.Retain = -1 }),
		"预创建过多":  mod(func(in *ConfigInput) { in.PreCreate = 101 }),
		"未知时区":   mod(func(in *ConfigInput) { in.Timezone = "Europe/Nowhere" }),
		"说明过长":   mod(func(in *ConfigInput) { in.Env = strings.Repeat("长", 101) }),
		"mssql":  mod(func(in *ConfigInput) { in.InstanceID = 5 }),
		"库被实例禁止": mod(func(in *ConfigInput) { in.InstanceID = 2; in.DBName = "secret_db" }),
	}
	for name, in := range bad {
		if _, err := e.svc.Create(root, in); err == nil {
			t.Errorf("%s：应拒绝", name)
		}
	}
	if len(e.repo.configs) != 0 || len(e.repo.audits) != 0 {
		t.Fatalf("校验失败不应写入: %d 配置 %d 审计", len(e.repo.configs), len(e.repo.audits))
	}
	if _, err := e.svc.Create(root, mod(func(in *ConfigInput) { in.InstanceID = 5 })); code(err) != "unsupported_engine" {
		t.Fatalf("mssql 应提示暂不支持: %v", err)
	}

	v, err := e.svc.Create(user(permAdd), goodInput())
	if err != nil {
		t.Fatal(err)
	}
	if v.CnfID != 1 || v.InstanceName != "prod-mysql-01" || !v.Supported || v.CreateTime != "2026-10-06 12:00:00" {
		t.Fatalf("%+v", v)
	}
	if got := strings.Join(e.repo.auditActions(), ","); got != "分区维护-新增配置" {
		t.Fatalf("审计: %s", got)
	}
	if a := e.repo.audits[0]; a.UserName != "bob" || !strings.Contains(a.ExtraInfo, "log.access_log") {
		t.Fatalf("审计内容: %+v", a)
	}
	// 同一实例同一张表不能重复配置
	if _, err := e.svc.Create(root, goodInput()); code(err) != "duplicate" {
		t.Fatalf("重复配置: %v", err)
	}
	// cnf_id 递增
	in2 := goodInput()
	in2.TbName = "other"
	if v2, err := e.svc.Create(root, in2); err != nil || v2.CnfID != 2 {
		t.Fatalf("cnf_id: %v %+v", err, v2)
	}
}

func TestUpdateDeleteAndScope(t *testing.T) {
	e := newEnv()
	cfg := e.addConfig(goodInput())
	other := goodInput()
	other.InstanceID, other.TbName = 4, "t4"
	cfg4 := e.addConfig(other)

	in := goodInput()
	in.Retain, in.PreCreate = 7, 5
	if _, err := e.svc.Update(user(permMenu), cfg.ID, in); code(err) != "forbidden" {
		t.Fatalf("无编辑权限: %v", err)
	}
	if _, err := e.svc.Update(user(permChange), cfg4.ID, in); code(err) != "no_privilege" {
		t.Fatalf("不能编辑别的资源组实例上的配置: %v", err)
	}
	if _, err := e.svc.Update(user(permChange), 999, in); code(err) != "not_found" {
		t.Fatalf("配置不存在: %v", err)
	}
	v, err := e.svc.Update(user(permChange), cfg.ID, in)
	if err != nil || v.Retain != 7 || v.PreCreate != 5 {
		t.Fatalf("%v %+v", err, v)
	}
	// 改成与另一条配置相同的 (实例, 库, 表)
	e.addConfig(ConfigInput{InstanceID: 1, DBName: "log", TbName: "dup", Column: "c", ColType: ColInt, Period: "day", Interval: 1, Timezone: "UTC"})
	in.TbName = "dup"
	if _, err := e.svc.Update(root, cfg.ID, in); code(err) != "duplicate" {
		t.Fatalf("改名撞唯一键: %v", err)
	}

	// 删除：需要 delete 权限；日志一并删除
	_, _ = e.repo.InsertLog(cfg.CnfID, "[]", fixedNow)
	if err := e.svc.Delete(user(permChange), cfg.ID); code(err) != "forbidden" {
		t.Fatalf("删除需要 partman.delete: %v", err)
	}
	if err := e.svc.Delete(user(permDelete), cfg.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.repo.GetConfig(cfg.ID); c != nil || len(e.repo.logs) != 0 {
		t.Fatalf("配置与日志应删除: %v %d", c, len(e.repo.logs))
	}
	got := strings.Join(e.repo.auditActions(), ",")
	if got != "分区维护-编辑配置,分区维护-删除配置" {
		t.Fatalf("审计: %s", got)
	}
}

func TestListScopeAndFilters(t *testing.T) {
	e := newEnv()
	a := goodInput()
	e.addConfig(a)
	b := goodInput()
	b.TbName, b.Period, b.ColType, b.IsEnable, b.Env = "t_metric", "hour", ColInt, false, "sit 环境"
	e.addConfig(b)
	c := goodInput()
	c.InstanceID, c.TbName = 4, "hidden"
	e.addConfig(c)
	e.repo.InsertLog(1, "[]", fixedNow.Add(-time.Hour))

	rows, total, err := e.svc.List(user(permMenu), ListQuery{})
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("普通用户只能看到资源组内实例的配置: %v %d %v", err, total, rows)
	}
	if rows[0].LastRun != "2026-10-06 11:00:00" || rows[1].LastRun != "" {
		t.Fatalf("最近维护: %+v", rows)
	}
	if _, total, _ = e.svc.List(root, ListQuery{}); total != 3 {
		t.Fatalf("超级用户看全部: %d", total)
	}
	off := false
	if _, total, _ = e.svc.List(root, ListQuery{IsEnable: &off}); total != 1 {
		t.Fatalf("按启用过滤: %d", total)
	}
	if rows, _, _ = e.svc.List(root, ListQuery{Period: "hour"}); len(rows) != 1 || rows[0].TbName != "t_metric" {
		t.Fatalf("按周期过滤: %+v", rows)
	}
	if rows, _, _ = e.svc.List(root, ListQuery{Search: "sit"}); len(rows) != 1 {
		t.Fatalf("搜索: %+v", rows)
	}
	if rows, total, _ = e.svc.List(root, ListQuery{Limit: 2, Offset: 2}); len(rows) != 1 || total != 3 {
		t.Fatalf("分页: %d %d", len(rows), total)
	}
	// 没有任何可见实例
	e.access.allowed[2] = nil
	if rows, total, err = e.svc.List(user(permMenu), ListQuery{}); err != nil || total != 0 || rows == nil {
		t.Fatalf("无可见实例应返回空列表: %v %d %v", err, total, rows)
	}
	ov, err := e.svc.Overview(user(permMenu, permAdd))
	if err != nil || len(ov.Instances) != 0 || !ov.CanAdd || ov.CanEdit || ov.Auto.Enabled {
		t.Fatalf("%v %+v", err, ov)
	}
}

func TestPreviewSavedAndDraft(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-06", "2026-10-07", "2026-10-08")
	cfg := e.addConfig(goodInput())

	pv, err := e.svc.Preview(ctx, user(permMenu), cfg.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Statements) != 2 || pv.Table != "log.access_log" || len(pv.Existing) != 3 || pv.Method != "RANGE COLUMNS" {
		t.Fatalf("%+v", pv)
	}
	if len(e.tgt.executed) != 0 {
		t.Fatal("预览不能执行任何语句")
	}
	d := goodInput()
	d.Retain = 0
	pv, err = e.svc.Preview(ctx, user(permMenu), 0, &d, nil)
	if err != nil || len(pv.Statements) != 2 {
		t.Fatalf("草稿预览: %v %+v", err, pv)
	}
	// 草稿里的危险输入也要拒绝；没有配置也没有草稿
	d.TbName = "x;y"
	if _, err = e.svc.Preview(ctx, root, 0, &d, nil); code(err) != "bad_request" {
		t.Fatalf("%v", err)
	}
	if _, err = e.svc.Preview(ctx, root, 0, nil, nil); code(err) != "bad_request" {
		t.Fatalf("%v", err)
	}
	// 普通用户不能预览别的资源组实例
	d = goodInput()
	d.InstanceID = 4
	if _, err = e.svc.Preview(ctx, user(permMenu), 0, &d, nil); code(err) != "no_privilege" {
		t.Fatalf("%v", err)
	}
	// 目标表不是分区表 / 不存在 / 连接失败
	e.tgt.tables["log.access_log"] = &TableState{Exists: true, IsBase: true}
	if pv, err := e.svc.Preview(ctx, root, cfg.ID, nil, nil); err != nil || pv.Kind != KindInit || pv.Init == nil || pv.Init.CanExecute {
		t.Fatalf("未分区且缺字段：应返回不可执行的 init 计划: %+v %v", pv, err)
	}
	delete(e.tgt.tables, "log.access_log")
	if _, err = e.svc.Preview(ctx, root, cfg.ID, nil, nil); code(err) != "table_missing" {
		t.Fatalf("%v", err)
	}
	e.tgt.dialErr = errors.New("dial tcp: i/o timeout")
	if _, err = e.svc.Preview(ctx, root, cfg.ID, nil, nil); code(err) != "target_error" || strings.Contains(err.Error(), "密码") {
		t.Fatalf("%v", err)
	}
}

func TestRunAddsAndIsIdempotent(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-06", "2026-10-07", "2026-10-08")
	cfg := e.addConfig(goodInput())

	if _, err := e.svc.Run(ctx, user(permMenu), cfg.ID, RunOptions{}); code(err) != "forbidden" {
		t.Fatalf("只有菜单权限不能执行: %v", err)
	}
	res, err := e.svc.Run(ctx, user(btnRun), cfg.ID, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed || len(res.Statements) != 2 || res.Statements[0].Status != "success" || res.LogID == 0 {
		t.Fatalf("%+v", res)
	}
	if got := e.tgt.names("log.access_log"); got != "p20261005,p20261006,p20261007,p20261008,p20261009" {
		t.Fatalf("目标表分区: %s", got)
	}
	// 审计先于执行，内容含语句；日志是 JSON 数组，每项有 sql_text
	if len(e.repo.audits) != 1 || e.repo.audits[0].Action != "分区维护-执行" || !strings.Contains(e.repo.audits[0].ExtraInfo, "ADD PARTITION") || e.repo.audits[0].UserName != "bob" {
		t.Fatalf("审计: %+v", e.repo.audits)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(e.repo.logs[0].ExecResult), &items); err != nil || len(items) != 3 || items[2]["kind"] != "trace" || items[0]["sql_text"] == "" || items[0]["operator"] != "bob" {
		t.Fatalf("日志 JSON: %v %s", err, e.repo.logs[0].ExecResult)
	}
	// 再执行一次：什么都不需要做，不写日志不写审计
	res, err = e.svc.Run(ctx, user(btnRun), cfg.ID, RunOptions{})
	if err != nil || len(res.Statements) != 0 || res.Notice == "" || len(e.repo.logs) != 1 || len(e.repo.audits) != 1 {
		t.Fatalf("第二次应是空操作: %v %+v", err, res)
	}
}

func TestRunDropRequiresConfirmationAndPermission(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10")
	in := goodInput()
	in.Retain = 2
	cfg := e.addConfig(in)
	drops := []string{"p20261001", "p20261002", "p20261003"}

	// 没有 delete 权限：即使确认也不行
	if _, err := e.svc.Run(ctx, user(btnRun), cfg.ID, RunOptions{ConfirmDrops: drops}); code(err) != "forbidden" {
		t.Fatalf("含 DROP 需要 partman.delete: %v", err)
	}
	// 没有确认 / 确认不全 / 多确认：409，且带计划
	for _, confirm := range [][]string{nil, drops[:2], append(append([]string{}, drops...), "p20261004")} {
		_, err := e.svc.Run(ctx, user(btnRun, permDelete), cfg.ID, RunOptions{ConfirmDrops: confirm})
		var pe *Error
		if !errors.As(err, &pe) || pe.Code != "confirm_required" || pe.Status != 409 {
			t.Fatalf("确认 %v 应 409: %v", confirm, err)
		}
		if pv, ok := pe.Data.(*PlanView); !ok || strings.Join(pv.Drops, ",") != strings.Join(drops, ",") {
			t.Fatalf("应带上计划: %+v", pe.Data)
		}
	}
	if len(e.tgt.executed) != 0 || len(e.repo.audits) != 0 {
		t.Fatalf("未确认不得执行也不写审计: %v", e.tgt.executed)
	}
	// 表在预览之后发生变化（别人手工多删了一个分区）：旧的确认失效，重新计算
	e.tgt.tables["log.access_log"].Partitions = e.tgt.tables["log.access_log"].Partitions[1:]
	if _, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{ConfirmDrops: drops}); code(err) != "confirm_required" {
		t.Fatalf("计划变化后旧确认应失效: %v", err)
	}
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10")

	res, err := e.svc.Run(ctx, user(btnRun, permDelete), cfg.ID, RunOptions{ConfirmDrops: []string{"p20261003", "p20261001", "p20261002"}})
	if err != nil || res.Failed || len(res.Statements) != 3 {
		t.Fatalf("%v %+v", err, res)
	}
	if got := e.tgt.names("log.access_log"); strings.Contains(got, "p20261001") || strings.Contains(got, "p20261003") || !strings.Contains(got, "p20261004") {
		t.Fatalf("目标表分区: %s", got)
	}
	if len(e.repo.audits) != 1 || !strings.Contains(e.repo.audits[0].ExtraInfo, "DROP PARTITION `p20261001`") {
		t.Fatalf("删除必须写审计: %+v", e.repo.audits)
	}
}

func TestRunReorganizeNeedsConfirmation(t *testing.T) {
	e := newEnv()
	st := dailyTable("2026-10-07")
	st.Partitions = append(st.Partitions, RawPartition{Name: "pmax", Desc: "MAXVALUE"})
	e.tgt.tables["log.access_log"] = st
	in := goodInput()
	in.PreCreate, in.Retain = 2, 0
	cfg := e.addConfig(in)
	_, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{})
	if code(err) != "confirm_required" {
		t.Fatalf("%v", err)
	}
	res, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{ConfirmReorganize: true})
	if err != nil || res.Failed || len(res.Statements) != 1 || res.Statements[0].Kind != "reorganize" {
		t.Fatalf("%v %+v", err, res)
	}
	if got := e.tgt.names("log.access_log"); got != "p20261006,p20261007,p20261008,pmax" {
		t.Fatalf("重组后: %s", got)
	}
}

func TestRunStopsAtFirstFailureAndLogs(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-07")
	cfg := e.addConfig(goodInput())
	e.tgt.failOn = "p20261008"
	res, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{})
	if err != nil || !res.Failed {
		t.Fatalf("%v %+v", err, res)
	}
	st := []string{}
	for _, s := range res.Statements {
		st = append(st, s.Status)
	}
	if strings.Join(st, ",") != "success,failed,skipped" || !strings.Contains(res.Statements[1].Error, "Duplicate partition") {
		t.Fatalf("状态: %v %+v", st, res.Statements)
	}
	logs, err := e.svc.Logs(root, cfg.ID, 10)
	if err != nil || len(logs) != 1 || len(logs[0].Items) != 3 || logs[0].Items[1].Status != "failed" || logs[0].Items[1].Error == "" {
		t.Fatalf("%v %+v", err, logs)
	}
}

func TestRunRefusals(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-07")
	cfg := e.addConfig(goodInput())

	// 审计写不进去：不执行
	e.repo.failAudit = true
	if _, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{}); code(err) != "audit_failed" || len(e.tgt.executed) != 0 || len(e.repo.logs) != 0 {
		t.Fatalf("审计失败应取消执行: %v %v", err, e.tgt.executed)
	}
	e.repo.failAudit = false

	// 别的进程正在维护 / 本进程正在维护
	e.repo.lockBusy[lockName(cfg.CnfID)] = true
	if _, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{}); code(err) != "busy" {
		t.Fatalf("%v", err)
	}
	delete(e.repo.lockBusy, lockName(cfg.CnfID))
	if !e.svc.acquire(cfg.CnfID) {
		t.Fatal("acquire")
	}
	if _, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{}); code(err) != "busy" {
		t.Fatalf("%v", err)
	}
	e.svc.release(cfg.CnfID)

	// 配置关闭 / 别的资源组 / 实例类型不支持 / 库被禁止
	off := goodInput()
	off.IsEnable, off.TbName = false, "off"
	c2 := e.addConfig(off)
	if _, err := e.svc.Run(ctx, root, c2.ID, RunOptions{}); code(err) != "disabled" {
		t.Fatalf("%v", err)
	}
	h := goodInput()
	h.InstanceID, h.TbName = 4, "hidden"
	c4 := e.addConfig(h)
	if _, err := e.svc.Run(ctx, user(btnRun), c4.ID, RunOptions{}); code(err) != "no_privilege" {
		t.Fatalf("%v", err)
	}
	s := goodInput()
	s.InstanceID, s.TbName = 5, "sdb"
	c3 := e.addConfig(s)
	if _, err := e.svc.Run(ctx, root, c3.ID, RunOptions{}); code(err) != "unsupported_engine" {
		t.Fatalf("%v", err)
	}
	d := goodInput()
	d.InstanceID, d.DBName, d.TbName = 2, "secret_x", "t"
	cd := e.addConfig(d)
	if _, err := e.svc.Run(ctx, root, cd.ID, RunOptions{}); code(err) != "db_not_allowed" {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.Run(ctx, root, 12345, RunOptions{}); code(err) != "not_found" {
		t.Fatalf("%v", err)
	}
	if len(e.tgt.executed) != 0 {
		t.Fatalf("以上都不应执行: %v", e.tgt.executed)
	}
}

func TestParseLogFormats(t *testing.T) {
	// 外部程序写的格式：只有 sql_text 等字段
	v := parseLog(Log{ID: 1, CreateTime: fixedNow, ExecResult: `[{"sql_text":"ALTER TABLE ` + "`a`.`b`" + ` ADD PARTITION (PARTITION p1 VALUES LESS THAN (1))","status":"ok"},{"sql_text":"ALTER TABLE x DROP PARTITION p0"}]`})
	if len(v.Items) != 2 || v.Raw != "" || v.Items[1].SQL != "ALTER TABLE x DROP PARTITION p0" {
		t.Fatalf("%+v", v)
	}
	// 不是 JSON：原文展示
	v = parseLog(Log{ID: 2, ExecResult: "plain text <b>x</b>"})
	if len(v.Items) != 0 || v.Raw != "plain text <b>x</b>" {
		t.Fatalf("%+v", v)
	}
	// JSON 但没有 sql_text：原文展示，不丢信息
	v = parseLog(Log{ID: 3, ExecResult: `[{"foo":1}]`})
	if v.Raw == "" {
		t.Fatalf("%+v", v)
	}
}

func TestLogsScope(t *testing.T) {
	e := newEnv()
	h := goodInput()
	h.InstanceID, h.TbName = 4, "hidden"
	c4 := e.addConfig(h)
	if _, err := e.svc.Logs(user(permMenu), c4.ID, 10); code(err) != "no_privilege" {
		t.Fatalf("不能看别的资源组实例上配置的日志: %v", err)
	}
	if _, err := e.svc.Logs(root, 777, 10); code(err) != "not_found" {
		t.Fatalf("%v", err)
	}
}

// ---- 定时维护 ----

func (e *env) enableAuto(drop bool) {
	e.src.cfg[cfgAutoRun] = "true"
	if drop {
		e.src.cfg[cfgAutoDrop] = "true"
	}
}

func TestAutoDisabledByDefault(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-07")
	e.addConfig(goodInput())
	sum := e.svc.RunAuto(ctx)
	if sum.Enabled || sum.Checked != 0 || e.tgt.dials != 0 || len(e.tgt.executed) != 0 {
		t.Fatalf("默认必须关闭: %+v dials=%d", sum, e.tgt.dials)
	}
}

func TestAutoAddsOnlyByDefault(t *testing.T) {
	e := newEnv()
	e.enableAuto(false)
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07")
	in := goodInput()
	in.Retain = 1
	cfg := e.addConfig(in)

	sum := e.svc.RunAuto(ctx)
	if sum.Executed != 1 || sum.Failed != 0 {
		t.Fatalf("%+v", sum)
	}
	got := e.tgt.names("log.access_log")
	if !strings.Contains(got, "p20261009") || !strings.Contains(got, "p20261001") {
		t.Fatalf("应续建、且默认不删除旧分区: %s", got)
	}
	if len(e.repo.audits) != 1 || e.repo.audits[0].Action != "分区维护-定时执行" || e.repo.audits[0].UserName != "system" {
		t.Fatalf("定时执行也要写审计: %+v", e.repo.audits)
	}
	var items []logItem
	_ = json.Unmarshal([]byte(e.repo.logs[0].ExecResult), &items)
	if len(items) == 0 || items[0].Source != "auto" || e.repo.logs[0].CnfID != cfg.CnfID {
		t.Fatalf("日志: %+v", e.repo.logs)
	}
	// 同一轮之后立刻再跑：日志在保护窗口内，直接跳过
	sum = e.svc.RunAuto(ctx)
	if sum.Executed != 0 || sum.Skipped != 1 {
		t.Fatalf("刚维护过应跳过: %+v", sum)
	}
}

func TestAutoDropOnlyWhenEnabledAndNeverReorganize(t *testing.T) {
	e := newEnv()
	e.enableAuto(true)
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07", "2026-10-08", "2026-10-09", "2026-10-10")
	in := goodInput()
	in.Retain = 3
	e.addConfig(in)
	// 另一张带 MAXVALUE 的表：只提示，不重组
	st := dailyTable("2026-10-07")
	st.Partitions = append(st.Partitions, RawPartition{Name: "pmax", Desc: "MAXVALUE"})
	e.tgt.tables["log.t_max"] = st
	m := goodInput()
	m.TbName = "t_max"
	m.Retain = 0
	e.addConfig(m)

	sum := e.svc.RunAuto(ctx)
	if sum.Executed != 1 || sum.Failed != 0 {
		t.Fatalf("%+v", sum)
	}
	if got := e.tgt.names("log.access_log"); strings.Contains(got, "p20261001") || !strings.Contains(got, "p20261003") {
		t.Fatalf("开启自动删除后应保留最近 3 个历史分区: %s", got)
	}
	if got := e.tgt.names("log.t_max"); got != "p20261006,pmax" {
		t.Fatalf("REORGANIZE 不能由定时任务执行: %s", got)
	}
}

func TestAutoSkipsUnsupportedLockedAndRecentAndRecordsPlanFailure(t *testing.T) {
	e := newEnv()
	e.enableAuto(false)
	e.src.cfg[cfgAutoInterval] = "60"
	// 1) 不支持的实例类型（mssql）：跳过
	s := goodInput()
	s.InstanceID, s.TbName = 5, "sdb"
	e.addConfig(s)
	// 2) 被别的进程锁着
	a := goodInput()
	a.TbName = "locked"
	locked := e.addConfig(a)
	e.tgt.tables["log.locked"] = dailyTable("2026-10-07")
	e.repo.lockBusy[lockName(locked.CnfID)] = true
	// 3) 外部程序刚刚写过日志
	b := goodInput()
	b.TbName = "recent"
	recent := e.addConfig(b)
	e.tgt.tables["log.recent"] = dailyTable("2026-10-07")
	_, _ = e.repo.InsertLog(recent.CnfID, `[{"sql_text":"ALTER TABLE x"}]`, fixedNow.Add(-5*time.Minute))
	// 4) 目标表不存在：计划失败，记录失败原因
	c := goodInput()
	c.TbName = "gone"
	plain := e.addConfig(c)
	// 5) 配置关闭的不处理
	d := goodInput()
	d.TbName, d.IsEnable = "off", false
	e.addConfig(d)
	e.tgt.tables["log.off"] = dailyTable("2026-10-07")

	sum := e.svc.RunAuto(ctx)
	if sum.Checked != 4 || sum.Skipped != 3 || sum.Failed != 1 || sum.Executed != 0 {
		t.Fatalf("%+v", sum)
	}
	if len(e.tgt.executed) != 0 {
		t.Fatalf("不应执行任何语句: %v", e.tgt.executed)
	}
	logs, _ := e.svc.Logs(root, plain.ID, 5)
	if len(logs) != 1 || len(logs[0].Items) != 1 || !strings.Contains(logs[0].Items[0].Error, "不存在") {
		t.Fatalf("计划失败应写进日志: %+v", logs)
	}
}

func TestAutoAuditFailureAborts(t *testing.T) {
	e := newEnv()
	e.enableAuto(false)
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-07")
	e.addConfig(goodInput())
	e.repo.failAudit = true
	sum := e.svc.RunAuto(ctx)
	if sum.Failed != 1 || len(e.tgt.executed) != 0 {
		t.Fatalf("审计失败不得执行: %+v %v", sum, e.tgt.executed)
	}
}

func TestAutoSettingsParsing(t *testing.T) {
	e := newEnv()
	if a := e.svc.autoSettings(); a.Enabled || a.Drop || a.IntervalMinutes != 180 {
		t.Fatalf("%+v", a)
	}
	e.src.cfg[cfgAutoRun], e.src.cfg[cfgAutoDrop], e.src.cfg[cfgAutoInterval] = " TRUE ", "true", "30"
	if a := e.svc.autoSettings(); !a.Enabled || !a.Drop || a.IntervalMinutes != 30 {
		t.Fatalf("%+v", a)
	}
	for _, bad := range []string{"1", "abc", "99999", "-5"} {
		e.src.cfg[cfgAutoInterval] = bad
		if a := e.svc.autoSettings(); a.IntervalMinutes != 180 {
			t.Fatalf("间隔 %q 应回落到默认值: %+v", bad, a)
		}
	}
}

func TestWorkerStopsOnCancel(t *testing.T) {
	e := newEnv()
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { e.svc.Worker(c); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后 Worker 应返回")
	}
}
