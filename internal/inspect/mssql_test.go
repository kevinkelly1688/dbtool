// @author: kevinkelly
// @license: Apache Licence
// @file: mssql_test.go
// @time: 2026/10/28

package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func msCtx(ft *fakeTarget) *msContext {
	c := &msContext{ctx: context.Background(), t: ft, now: newClock(ts("2026-10-06 08:00:00")).Now}
	if err := c.loadServerInfo(); err != nil {
		panic(err)
	}
	if err := c.loadDatabases(nil); err != nil {
		panic(err)
	}
	return c
}

func findCheck(key string) msCheck {
	for _, c := range msChecks {
		if c.Key == key {
			return c
		}
	}
	panic("没有检查项 " + key)
}

func TestMSSQLServerInfo(t *testing.T) {
	c := msCtx(mssqlHappyTarget())
	if c.version != 15 || c.cpuCount != 16 || c.memoryMB != 131072 || len(c.databases) != 2 {
		t.Errorf("实例信息不对：%+v", c)
	}
	if toInt(c.server["maxdop"]) != 0 || toInt(c.server["cost_threshold"]) != 5 {
		t.Errorf("配置不对：%v", c.server)
	}
	// 只巡检指定的库
	c2 := &msContext{ctx: context.Background(), t: mssqlHappyTarget(), now: c.now}
	_ = c2.loadServerInfo()
	_ = c2.loadDatabases([]string{"report"})
	if len(c2.databases) != 1 || c2.databases[0] != "Report" {
		t.Errorf("库范围过滤不对（不区分大小写）：%v", c2.databases)
	}
}

func TestMSSQLChecks(t *testing.T) {
	c := msCtx(mssqlHappyTarget())
	r, err := findCheck("memory.ple").Run(c)
	if err != nil {
		t.Fatal(err)
	}
	// 缓冲池 2097152 页 × 8KB = 16GB → 阈值 300×16/4 = 1200 秒；PLE 180 → 告警
	if r.Level != LevelAlert || !strings.Contains(r.Value, "PLE 180 s") || !strings.Contains(r.Threshold, "1,200") {
		t.Errorf("PLE：%+v", r)
	}
	r, _ = findCheck("cpu.signal_wait").Run(c)
	if r.Level != LevelAlert || !strings.Contains(r.Value, "50") { // 500/1000 = 50% > 25%
		t.Errorf("信号等待：%+v", r)
	}
	r, _ = findCheck("blocking.chain").Run(c)
	if r.Level != LevelNormal || r.Value != "无阻塞" {
		t.Errorf("阻塞链：%+v", r)
	}
	r, _ = findCheck("io.tempdb").Run(c)
	if r.Level != LevelNormal { // 8 个文件，CPU 16 核 → 期望 8 个、且大小一致
		t.Errorf("tempdb：%+v", r)
	}
	r, _ = findCheck("overview.config").Run(c)
	if r.Level != LevelAttention || !strings.Contains(r.Value, "MAXDOP=0") || !strings.Contains(r.Value, "cost threshold") {
		t.Errorf("并行度配置：%+v", r)
	}
	r, _ = findCheck("io.file_latency").Run(c)
	// 日志文件写延迟 40000/5000 = 8ms > 5 → 告警
	if r.Level != LevelAlert || r.Detail == nil || len(r.Detail.Rows) != 1 || r.Raw == nil {
		t.Errorf("文件延迟：%+v", r)
	}
	r, _ = findCheck("overview.waits").Run(c)
	if r.Level != LevelAttention || !strings.Contains(r.Value, "PAGEIOLATCH_SH 90") { // 9000/10000 = 90% > 40%
		t.Errorf("等待类型：%+v", r)
	}
	c.db = "ShopDB"
	r, _ = findCheck("database.backup").Run(c)
	if r.Level != LevelAlert || !strings.Contains(r.Value, "完整备份 从未") {
		t.Errorf("从未备份应告警：%+v", r)
	}
	r, _ = findCheck("database.log").Run(c)
	if r.Level != LevelNormal || !strings.Contains(r.Value, "VLF 120") {
		t.Errorf("日志：%+v", r)
	}
	r, _ = findCheck("database.options").Run(c)
	if r.Level != LevelNormal {
		t.Errorf("数据库选项：%+v", r)
	}
}

func TestMSSQLBackupLevels(t *testing.T) {
	ft := mssqlHappyTarget()
	ft.rules = append([]rule{
		{sub: "msdb.dbo.backupset", res: res([]string{"type", "last_finish"}, []any{"D", ts("2026-10-05 02:00:00")}, []any{"L", ts("2026-10-06 04:00:00")})},
		{sub: "recovery_model_desc FROM sys.databases WHERE database_id = DB_ID()", res: res([]string{"recovery_model_desc"}, []any{"FULL"})},
	}, ft.rules...)
	c := msCtx(ft)
	c.db = "ShopDB"
	r, _ := findCheck("database.backup").Run(c)
	// 完整备份 1 天前（正常）；日志备份 4 小时前（FULL 模式 > 1h 关注）
	if r.Level != LevelAttention || !strings.Contains(r.Value, "日志备份距今 4.0 小时") {
		t.Errorf("日志备份 4 小时：%+v", r)
	}
	if ft.dbs[len(ft.dbs)-1] != "ShopDB" {
		t.Errorf("每库检查项应切到目标库：%v", ft.dbs)
	}
}

func TestMSSQLCounterDiff(t *testing.T) {
	c := msCtx(mssqlHappyTarget())
	r, _ := findCheck("blocking.deadlocks").Run(c)
	if r.Level != LevelAttention || !strings.Contains(r.Value, "自启动以来") || !strings.Contains(r.Value, "7 次") {
		t.Errorf("没有上一份报告时按启动以来：%+v", r)
	}
	raw, _ := json.Marshal(r.Raw)
	c.prevRaw = map[string]string{"blocking.deadlocks|": string(raw)}
	r, _ = findCheck("blocking.deadlocks").Run(c)
	if r.Level != LevelNormal || !strings.Contains(r.Value, "与上一份报告的差值") || !strings.Contains(r.Value, "0 次") {
		t.Errorf("与上一份相同应为 0 次：%+v", r)
	}
	c.prevRaw = map[string]string{"blocking.deadlocks|": `{"total": 5}`}
	r, _ = findCheck("blocking.deadlocks").Run(c)
	if !strings.Contains(r.Value, "2 次") {
		t.Errorf("差值应为 2：%+v", r)
	}
}

func TestMSSQLPermissionError(t *testing.T) {
	for _, s := range []string{"mssql: The SELECT permission was denied on the object", "Msg 229 (229)", "VIEW SERVER STATE (300)", "没有权限"} {
		if !isPermissionError(errors.New(s)) {
			t.Errorf("应识别为权限错误：%s", s)
		}
	}
	if isPermissionError(errors.New("connection reset")) {
		t.Error("连接错误不是权限错误")
	}
}

func TestFmtHelpers(t *testing.T) {
	if fmtNum(1234567) != "1,234,567" || fmtNum(12) != "12" || fmtNum(-4500) != "-4,500" || fmtNum("1000") != "1,000" {
		t.Errorf("fmtNum：%s %s %s", fmtNum(1234567), fmtNum(12), fmtNum(-4500))
	}
	if fmtDuration(59) != "59s" || fmtDuration(125) != "2m05s" || fmtDuration(3700) != "1h01m" || fmtDuration(90000) != "1 天 1 小时" {
		t.Errorf("fmtDuration：%s %s %s %s", fmtDuration(59), fmtDuration(125), fmtDuration(3700), fmtDuration(90000))
	}
	long := strings.Repeat("select  a ", 200)
	if got := truncSQL(long); len([]rune(got)) != msSQLTextLimit+1 || strings.Contains(got, "  ") {
		t.Errorf("truncSQL 长度 %d", len([]rune(got)))
	}
}
