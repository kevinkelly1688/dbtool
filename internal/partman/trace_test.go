// @author: kevinkelly
// @license: Apache Licence
// @file: trace_test.go
// @time: 2026/10/28

package partman

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

func outText(lines []traceLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.L + " " + l.M + "\n")
	}
	return b.String()
}

func mustContain(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("%s 缺少 %q:\n%s", what, w, text)
		}
	}
}

// 起始行带全部参数；每个检查 / 决策、每条语句的结果与耗时、结束行带合计；时间是毫秒精度。
func TestTraceForAddAndDrop(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07")
	in := goodInput()
	in.Retain, in.PreCreate = 2, 3
	cfg := e.addConfig(in)
	pv, err := e.svc.Preview(ctx, root, cfg.ID, nil, nil)
	if err != nil || len(pv.Drops) == 0 {
		t.Fatalf("%+v %v", pv, err)
	}
	// 预览也带“检查过程”，但不落库
	pre := outText(pv.Output.Lines)
	mustContain(t, "预览输出", pre, "表已分区（RANGE COLUMNS）", "准备新增分区", "准备删除过期分区")
	if len(e.repo.logs) != 0 {
		t.Fatal("预览不写日志")
	}
	res, err := e.svc.Run(ctx, user(permMenu, btnRun, permDelete), cfg.ID, RunOptions{ConfirmDrops: pv.Drops})
	if err != nil || res.Failed {
		t.Fatalf("%+v %v", res, err)
	}
	lines := res.Output.Lines
	txt := outText(lines)
	mustContain(t, "运行输出", txt,
		"=== 分区维护开始 | 实例=prod-mysql-01(#1) 表=log.access_log col=created_at coltype=datetime period=day interval=1 retain=2 preCreate=3 tz=Asia/Shanghai(+08:00) 来源=manual 操作人=bob ===",
		"表 log.access_log 存在，是普通表", "现有分区 6 个", "准备新增分区: p20261007, p20261008, p20261009", "准备删除过期分区: p20261001, p20261002, p20261003",
		"已确认删除分区：", "执行 SQL: ALTER TABLE `log`.`access_log` ADD PARTITION", "执行 SQL: ALTER TABLE `log`.`access_log` DROP PARTITION `p20261001`",
		"执行成功（0 ms）", "没有启用的推送通道", "=== 分区维护结束 | 成功 6 / 失败 0 / 跳过 0，耗时 0 ms ===")
	ts := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}$`)
	for _, l := range lines {
		if !ts.MatchString(l.T) || (l.L != "INFO" && l.L != "WARN" && l.L != "ERROR") || l.M == "" {
			t.Fatalf("行格式不对: %+v", l)
		}
	}
	// 写进 dbt_partman_log：语句项 + 一个 trace 项；列表只给行数，明细与下载走 GET /logs/:id
	if len(e.repo.logs) != 1 {
		t.Fatalf("%d", len(e.repo.logs))
	}
	logs, _ := e.svc.Logs(user(permMenu), cfg.ID, 10)
	if len(logs) != 1 || logs[0].OutputLines != len(lines) || len(logs[0].Items) != 6 || logs[0].Status != LogSuccess || logs[0].Kind != "maintenance" {
		t.Fatalf("%+v", logs)
	}
	d, err := e.svc.LogDetail(user(permMenu), res.LogID)
	if err != nil || len(d.Output.Lines) != len(lines) || d.Table != "log.access_log" || d.Scope != "config" || d.Output.Truncated {
		t.Fatalf("%+v %v", d, err)
	}
	name, text, err := e.svc.LogOutput(user(permMenu), res.LogID)
	if err != nil || name != "partman_1_20261006_120000.log" || !strings.Contains(text, "=== 分区维护开始 ") || !strings.HasSuffix(text, "===\n") {
		t.Fatalf("%s %q %v", name, text, err)
	}
	if strings.Count(text, "\n") != len(lines) {
		t.Fatalf("导出的行数不对: %d vs %d", strings.Count(text, "\n"), len(lines))
	}
}

func TestTraceForFailureAndSkipped(t *testing.T) {
	e := newEnv()
	e.tgt.tables["log.access_log"] = dailyTable("2026-10-06", "2026-10-07")
	in := goodInput()
	in.PreCreate, in.Retain = 4, 0
	cfg := e.addConfig(in)
	e.tgt.failOn = "p20261009"
	res, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{})
	if err != nil || !res.Failed {
		t.Fatalf("%+v %v", res, err)
	}
	txt := outText(res.Output.Lines)
	mustContain(t, "失败的运行输出", txt, "ERROR 执行失败（0 ms）: ERROR 1517", "WARN 跳过（前面的语句失败）", "成功 2 / 失败 1 / 跳过 1")
	logs, _ := e.svc.Logs(root, cfg.ID, 5)
	if logs[0].Status != LogPartial {
		t.Fatalf("%+v", logs[0])
	}
}

func TestTraceForInit(t *testing.T) {
	e := newEnv()
	e.plain("t")
	cfg := e.addConfig(initInput("t"))
	pv, err := e.svc.Preview(ctx, root, cfg.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "init 预览输出", outText(pv.Output.Lines), "表 log.t 未分区，可初始化为 RANGE 分区表", "前置检查 engine：存储引擎 InnoDB 支持分区", "前置检查 unique_keys", "表未分区，准备首次建分区，共 3 个分区: p20261006 ... p20261008")
	res, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{ConfirmInit: &pv.Init.Confirm})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "init 运行输出", outText(res.Output.Lines), "已按键入的 log.t 确认初始化：3 个分区 p20261006 ~ p20261008", "执行 SQL: ALTER TABLE `log`.`t` PARTITION BY RANGE COLUMNS", "成功 1 / 失败 0 / 跳过 0")
}

// 定时轮次：跳过了谁、为什么，作为一条系统记录（source=auto）在全局日志里可见；按可见范围过滤。
func TestAutoRoundSummaryIsVisibleInGlobalLogs(t *testing.T) {
	e := newEnv()
	e.enableAuto(false)
	e.src.cfg[cfgAutoInterval] = "60"
	// 1) 不支持的类型
	s := goodInput()
	s.InstanceID, s.TbName = 5, "sdb"
	un := e.addConfig(s)
	// 2) 被别的进程锁着
	a := goodInput()
	a.TbName = "locked"
	locked := e.addConfig(a)
	e.tgt.tables["log.locked"] = dailyTable("2026-10-07")
	e.repo.lockBusy[lockName(locked.CnfID)] = true
	// 3) 保护窗口内
	b := goodInput()
	b.TbName = "recent"
	recent := e.addConfig(b)
	e.tgt.tables["log.recent"] = dailyTable("2026-10-07")
	_, _ = e.repo.InsertLog(recent.CnfID, `[{"sql_text":"ALTER TABLE x"}]`, fixedNow.Add(-5*time.Minute))
	// 4) 未分区：不初始化
	c := goodInput()
	c.TbName = "plain"
	e.addConfig(c)
	e.plain("plain")
	// 5) 有待清理但 auto_drop 关闭 → 只续建
	d := goodInput()
	d.TbName, d.Retain, d.PreCreate = "olds", 1, 1
	olds := e.addConfig(d)
	e.tgt.tables["log.olds"] = dailyTable("2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07")
	// 6) 已是最新
	f := goodInput()
	f.TbName, f.PreCreate = "fine", 1
	e.addConfig(f)
	e.tgt.tables["log.fine"] = dailyTable("2026-10-06", "2026-10-07", "2026-10-08")
	// 7) 目标表不存在 → 失败
	g := goodInput()
	g.TbName = "gone"
	gone := e.addConfig(g)
	// 8) 别的资源组的实例（bob 看不到）上的配置
	h := goodInput()
	h.InstanceID, h.TbName = 4, "hidden"
	hid := e.addConfig(h)
	e.tgt.tables["log.hidden"] = dailyTable("2026-10-07")

	sum := e.svc.RunAuto(ctx)
	if sum.Checked != 8 || sum.Executed != 2 || sum.Failed != 1 || sum.Skipped != 4 {
		t.Fatalf("%+v", sum)
	}
	_ = un
	_ = hid

	rows, total, _, err := e.svc.GlobalLogs(root, GlobalLogQuery{System: true, Limit: 20})
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("%d %+v %v", total, rows, err)
	}
	r := rows[0]
	if r.Scope != "system" || r.Kind != "auto_round" || r.Source != SourceAuto || r.Status != LogFailed || r.Round == nil || r.OutputLines == 0 {
		t.Fatalf("%+v", r)
	}
	rs := r.Round
	if rs.Checked != 8 || rs.Executed != 2 || rs.Failed != 1 || rs.Skipped != 4 || rs.Nothing != 1 || rs.GuardMinutes != 30 || rs.AutoDrop {
		t.Fatalf("%+v", rs)
	}
	reason := map[int64]RoundItem{}
	for _, it := range rs.Items {
		reason[it.CnfID] = it
	}
	check := func(cnf int64, result string, contains ...string) {
		t.Helper()
		it, ok := reason[cnf]
		if !ok || it.Result != result {
			t.Errorf("#%d: %+v", cnf, it)
			return
		}
		for _, w := range contains {
			if !strings.Contains(it.Reason, w) {
				t.Errorf("#%d: 原因 %q 不含 %q", cnf, it.Reason, w)
			}
		}
	}
	check(un.CnfID, "skipped", "不支持分区维护")
	check(locked.CnfID, "skipped", "另一个进程正在维护")
	check(recent.CnfID, "skipped", "近期日志保护窗口")
	check(olds.CnfID, "executed", "1 条语句成功")
	check(gone.CnfID, "failed", "计划失败", "不存在")
	if it := reason[4]; it.Result != "skipped" || !strings.Contains(it.Reason, "不会初始化") {
		t.Errorf("未分区: %+v", it)
	}
	// 明细：运行输出里有轮次起止与每个配置的原因；有待清理但 auto_drop 关闭也写出来
	d2, err := e.svc.LogDetail(root, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "轮次输出", outText(d2.Output.Lines), "=== 定时维护轮次开始 | 间隔=60分钟 自动删除=false 保护窗口=30m0s 推送时机=always ===",
		"跳过——实例 mssql-01 的类型 mssql 不支持分区维护", "近期日志保护窗口 30m0s", "另一个进程正在维护该配置", "表未分区：定时维护不会初始化",
		"=== 定时维护轮次结束 | 检查 8，执行 2，无需维护 1，跳过 4，失败 1")
	// 该配置自己的那条日志里记录了“auto_drop 未开启”
	own, _ := e.svc.Logs(root, olds.ID, 5)
	if len(own) != 1 {
		t.Fatalf("%+v", own)
	}
	dd, _ := e.svc.LogDetail(root, own[0].ID)
	mustContain(t, "olds 输出", outText(dd.Output.Lines), "partition.auto_drop 未开启，本轮不删除", "来源=auto 操作人=system")

	// 不在全局列表默认混入配置日志之外的东西：有 permChange 的人在“全部”里能看到系统记录；只读用户看不到
	all, _, _, _ := e.svc.GlobalLogs(root, GlobalLogQuery{Limit: 50})
	sys := 0
	for _, x := range all {
		if x.Scope == "system" {
			sys++
		}
	}
	if sys != 1 {
		t.Fatalf("全部日志里应有 1 条系统记录: %d", sys)
	}
	ro, _, _, err := e.svc.GlobalLogs(user(permMenu), GlobalLogQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ro {
		if x.Scope == "system" {
			t.Fatalf("只读用户不应看到系统记录: %+v", x)
		}
	}
	if _, _, _, err := e.svc.GlobalLogs(user(permMenu), GlobalLogQuery{System: true}); code(err) != "forbidden" {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.LogDetail(user(permMenu), r.ID); code(err) != "forbidden" {
		t.Fatalf("%v", err)
	}
	// 有 partman.edit 但只能看到部分实例：只看到自己范围内的配置项与输出行
	bob := user(permMenu, permChange)
	bd, err := e.svc.LogDetail(bob, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range bd.Round.Items {
		if it.CnfID == hid.CnfID {
			t.Fatalf("看不到的实例上的配置不应出现: %+v", it)
		}
	}
	if strings.Contains(outText(bd.Output.Lines), "hidden") || !strings.Contains(outText(root0(e, r.ID)), "hidden") {
		t.Fatalf("输出行应按可见范围过滤")
	}
	if bd.Round.Checked != 8 {
		t.Fatalf("摘要的总数不变: %+v", bd.Round)
	}
}

func root0(e *env, id uint) []traceLine {
	d, _ := e.svc.LogDetail(root, id)
	return d.Output.Lines
}

// 日志访问范围：别的资源组的日志是 404；不存在的是 404。
func TestLogDetailScope(t *testing.T) {
	e := newEnv()
	in := goodInput()
	in.InstanceID, in.TbName = 4, "hidden"
	cfg := e.addConfig(in)
	id, _ := e.repo.InsertLog(cfg.CnfID, `[{"sql_text":"X","status":"success"}]`, fixedNow)
	if _, err := e.svc.LogDetail(user(permMenu), id); code(err) != "not_found" {
		t.Fatalf("%v", err)
	}
	if _, _, err := e.svc.LogOutput(user(permMenu), id); code(err) != "not_found" {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.LogDetail(user(permMenu), 9999); code(err) != "not_found" {
		t.Fatalf("%v", err)
	}
	// 没有运行输出的旧日志：导出给一句说明，不报错
	d, err := e.svc.LogDetail(root, id)
	if err != nil || len(d.Output.Lines) != 0 || d.Output.Truncated {
		t.Fatalf("%+v %v", d, err)
	}
	if _, text, err := e.svc.LogOutput(root, id); err != nil || !strings.Contains(text, "没有运行输出") {
		t.Fatalf("%q %v", text, err)
	}
}

func TestCapTrace(t *testing.T) {
	var lines []traceLine
	lines = append(lines, traceLine{T: "t0", L: "INFO", M: "=== 开始 ==="})
	for i := 0; i < 1000; i++ {
		lines = append(lines, traceLine{T: "t", L: "INFO", M: fmt.Sprintf("info %d", i)})
	}
	lines = append(lines, traceLine{T: "t", L: "WARN", M: "关键警告"}, traceLine{T: "t", L: "ERROR", M: "关键错误"}, traceLine{T: "t9", L: "INFO", M: "=== 结束 ==="})
	out := capTrace(lines, maxTraceLines, maxTraceBytes)
	if len(out) > maxTraceLines {
		t.Fatalf("行数 %d 超过上限", len(out))
	}
	b, _ := json.Marshal(out)
	if len(b) > maxTraceBytes {
		t.Fatalf("字节数 %d 超过上限", len(b))
	}
	if out[0].M != "=== 开始 ===" {
		t.Fatalf("起始行要保留: %+v", out[0])
	}
	if !strings.HasPrefix(out[1].M, "…已截断 ") {
		t.Fatalf("第二行应是截断标记: %+v", out[1])
	}
	var n int
	if _, err := fmt.Sscanf(out[1].M, "…已截断 %d 行", &n); err != nil || n != len(lines)-len(out)+1 {
		t.Fatalf("截断数 %d 不对（原 %d 现 %d）: %s", n, len(lines), len(out), out[1].M)
	}
	txt := outText(out)
	mustContain(t, "截断后", txt, "关键警告", "关键错误", "=== 结束 ===")
	if !outputView(out).Truncated || outputView(lines).Truncated {
		t.Fatal("Truncated 标志不对")
	}
	// 较早的 INFO 先被删：保留的是靠后的
	if strings.Contains(txt, "info 0\n") || !strings.Contains(txt, "info 999\n") {
		t.Fatal("应先删较早的 INFO")
	}
	// 没超限：原样
	small := lines[:10]
	if got := capTrace(small, maxTraceLines, maxTraceBytes); len(got) != 10 {
		t.Fatalf("%d", len(got))
	}
	// 字节上限：每行很长
	var fat []traceLine
	for i := 0; i < 200; i++ {
		fat = append(fat, traceLine{T: "2026-10-06 12:00:00.000", L: "INFO", M: strings.Repeat("长", 400)})
	}
	got := capTrace(fat, maxTraceLines, maxTraceBytes)
	b, _ = json.Marshal(got)
	if len(b) > maxTraceBytes || !strings.HasPrefix(got[1].M, "…已截断") {
		t.Fatalf("字节上限: %d %s", len(b), got[1].M)
	}
}

// exec_result 是 TEXT（64KB）：再大的初始化语句 / 再多的语句 / 再长的输出，序列化后也不超过上限，且仍然是合法 JSON、parseLog 读得出来。
func TestBuildLogBodyNeverExceedsColumn(t *testing.T) {
	huge := "ALTER TABLE `d`.`t` PARTITION BY RANGE COLUMNS(`c`) (" + strings.Repeat("PARTITION `p20260101` VALUES LESS THAN ('2026-01-02 00:00:00'), ", 5000) + ")"
	var trace []traceLine
	for i := 0; i < 3000; i++ {
		trace = append(trace, traceLine{T: "2026-10-06 12:00:00.000", L: "INFO", M: strings.Repeat("x", 600)})
	}
	var many []logItem
	for i := 0; i < 2000; i++ {
		many = append(many, logItem{SQLText: "ALTER TABLE `d`.`t` DROP PARTITION `p" + fmt.Sprint(i) + "`", Kind: "drop", Status: "success", Operator: "u", Source: "manual"})
	}
	cases := map[string][]logItem{
		"超长的 init 语句": {{SQLText: huge, Kind: "init", Status: "success", Operator: "u", Source: "manual"}},
		"上千条语句":       many,
	}
	for name, items := range cases {
		body := buildLogBody(items, trace, logItem{Operator: "u", Source: "manual"})
		if len(body) > maxLogBytes {
			t.Errorf("%s: %d 字节超过 %d", name, len(body), maxLogBytes)
		}
		var arr []map[string]any
		if err := json.Unmarshal([]byte(body), &arr); err != nil {
			t.Fatalf("%s: 不是合法 JSON: %v", name, err)
		}
		v := parseLog(Log{ExecResult: body})
		if v.Source != SourceManual || len(v.Items) == 0 || v.OutputLines == 0 || v.Raw != "" {
			t.Errorf("%s: %+v", name, v)
		}
		if !outputView(extractTrace(body)).Truncated {
			t.Errorf("%s: 应有截断标记", name)
		}
	}
	// 轮次摘要很长时也不超限
	rs := &RoundSummary{}
	for i := 0; i < 100; i++ {
		rs.Items = append(rs.Items, RoundItem{CnfID: int64(i), Table: strings.Repeat("t", 130), Result: "skipped", Reason: strings.Repeat("原因", 100)})
	}
	body := buildLogBody(nil, trace, logItem{Operator: "system", Source: "auto", Round: rs})
	if len(body) > maxLogBytes {
		t.Fatalf("轮次摘要: %d", len(body))
	}
}

// 一次真实的大初始化：语句巨大也能落库、能读回。
func TestInitWithManyPartitionsIsLoggedWithinLimit(t *testing.T) {
	e := newEnv()
	e.plain("big")
	in := initInput("big")
	in.Period, in.PreCreate = "hour", 0
	cfg := e.addConfig(in)
	req := &InitRequest{StartMode: StartDate, StartDate: "2026-04-20"} // 约 4000 个小时分区
	pv, err := e.svc.Preview(ctx, root, cfg.ID, nil, req)
	if err != nil || !pv.Init.CanExecute || pv.Init.PartitionCount < 3000 {
		t.Fatalf("%+v %v", pv.Init, err)
	}
	if len(pv.Statements) != 1 || strings.Count(pv.Statements[0].SQL, "PARTITION `p") != pv.Init.PartitionCount {
		t.Fatalf("预览必须返回完整的语句")
	}
	res, err := e.svc.Run(ctx, root, cfg.ID, RunOptions{Init: req, ConfirmInit: &pv.Init.Confirm})
	if err != nil || res.Failed || res.LogID == 0 {
		t.Fatalf("%+v %v", res, err)
	}
	lg, _ := e.repo.GetLog(res.LogID)
	if len(lg.ExecResult) > maxLogBytes {
		t.Fatalf("日志 %d 字节", len(lg.ExecResult))
	}
	if v := parseLog(*lg); len(v.Items) != 1 || v.Status != LogSuccess || v.OutputLines == 0 {
		t.Fatalf("%+v", v)
	}
	// 审计里语句被截断到 3000 字（审计表限制），但仍然先于执行写入
	if len(e.repo.audits) != 1 || len([]rune(e.repo.audits[0].ExtraInfo)) > 3001 {
		t.Fatalf("审计 %d", len(e.repo.audits))
	}
}
