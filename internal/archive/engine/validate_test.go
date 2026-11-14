// @author: kevinkelly
// @license: Apache Licence
// @file: validate_test.go
// @time: 2026/10/28

package engine

import (
	"strings"
	"testing"
)

func okSpec() Spec {
	return Spec{
		Name: "orders 归档", InstanceID: 1, DB: "shop", Table: "orders",
		TimeColumn: "created_at", RetentionDays: 90, Mode: ModeMove,
		Dest:   &TableDest{InstanceID: 1, DB: "shop_arch", Template: "{table}_{yyyy}"},
		Params: Params{BatchSize: 1000, SleepMs: 200},
	}
}

func okPolicy() Policy {
	return Policy{MinRetentionDays: 30, ProtectedPatterns: []string{"secret_*", "shop.vip_*"}, MaxRowsPerRun: 5_000_000}
}

func issue(issues []Issue, field string) *Issue {
	for i := range issues {
		if issues[i].Field == field && issues[i].Level == "error" {
			return &issues[i]
		}
	}
	return nil
}

func TestValidateOK(t *testing.T) {
	is := Validate(okSpec(), okPolicy())
	if HasErrors(is) {
		t.Fatalf("合法规格不应有 error：%+v", is)
	}
	if IssuesError(is) != nil {
		t.Errorf("IssuesError 应为 nil")
	}
}

func TestValidateTable(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(*Spec)
		pol    func(*Policy)
		field  string
		errHas string
	}{
		{"name too short", func(s *Spec) { s.Name = "a" }, nil, "name", "2–50"},
		{"name too long", func(s *Spec) { s.Name = strings.Repeat("长", 51) }, nil, "name", "2–50"},
		{"no instance", func(s *Spec) { s.InstanceID = 0 }, nil, "instance_id", "实例"},
		{"bad db", func(s *Spec) { s.DB = "a`b" }, nil, "db", "库名无效"},
		{"bad table", func(s *Spec) { s.Table = "a b" }, nil, "table", "表名无效"},
		{"empty table", func(s *Spec) { s.Table = "" }, nil, "table", "表名无效"},
		{"protected builtin", func(s *Spec) { s.DB = "mysql"; s.Table = "user" }, nil, "table", "受保护"},
		{"protected builtin case", func(s *Spec) { s.DB = "INFORMATION_SCHEMA"; s.Table = "t" }, nil, "table", "受保护"},
		{"protected pattern db", func(s *Spec) { s.DB = "secret_finance" }, nil, "table", "受保护"},
		{"protected pattern table", func(s *Spec) { s.Table = "vip_users" }, nil, "table", "受保护"},
		{"bad time col", func(s *Spec) { s.TimeColumn = "" }, nil, "time_column", "时间列"},
		{"retention zero", func(s *Spec) { s.RetentionDays = 0 }, nil, "retention_days", "正整数"},
		{"retention < policy", func(s *Spec) { s.RetentionDays = 29 }, nil, "retention_days", "最小保留天数 30"},
		{"retention > max", func(s *Spec) { s.RetentionDays = 4000 }, nil, "retention_days", "最大"},
		{"extra bad", func(s *Spec) { s.ExtraWhere = "a=1; drop table t" }, nil, "extra_where", "分号"},
		{"mode invalid", func(s *Spec) { s.Mode = "zap" }, nil, "mode", "模式无效"},
		{"move without backup", func(s *Spec) { s.Dest = nil }, nil, "mode", "备份去向"},
		{"copy without backup", func(s *Spec) { s.Dest = nil; s.Mode = ModeCopy }, nil, "mode", "备份去向"},
		{"purge not allowed", func(s *Spec) { s.Dest = nil; s.Mode = ModePurge }, nil, "mode", "不允许"},
		{"purge with dest", func(s *Spec) { s.Mode = ModePurge }, func(p *Policy) { p.AllowPurgeNoBackup = true }, "mode", "不备份"},
		{"dest no instance", func(s *Spec) { s.Dest.InstanceID = 0 }, nil, "dest_table.instance_id", "实例"},
		{"dest bad db", func(s *Spec) { s.Dest.DB = "" }, nil, "dest_table.db", "归档库名无效"},
		{"dest protected db", func(s *Spec) { s.Dest.DB = "mysql" }, nil, "dest_table.db", "受保护"},
		{"dest bad template", func(s *Spec) { s.Dest.Template = "{table};{x}" }, nil, "dest_table.template", "只能含"},
		{"dest unknown var", func(s *Spec) { s.Dest.Template = "{table}_{week}" }, nil, "dest_table.template", "占位符"},
		{"dest empty template", func(s *Spec) { s.Dest.Template = "" }, nil, "dest_table.template", "不能为空"},
		{"dest same as src", func(s *Spec) { s.Dest.DB = "shop"; s.Dest.Template = "{table}" }, nil, "dest_table.table", "源表"},
		{"dest fixed table same as src", func(s *Spec) { s.Dest.DB = "shop"; s.Dest.Table = "orders"; s.Dest.Template = "" }, nil, "dest_table.table", "源表"},
		{"dest too long", func(s *Spec) { s.Dest.Template = strings.Repeat("a", 60) + "_{yyyymm}_{yyyy}" }, nil, "dest_table.template", "64"},
		{"file no store", func(s *Spec) { s.Dest = nil; s.File = &FileDest{Format: "csv", PathTemplate: "{db}/{table}"} }, nil, "dest_file.store_id", "备份存储"},
		{"file bad format", func(s *Spec) { s.Dest = nil; s.File = &FileDest{StoreID: 1, Format: "xlsx", PathTemplate: "a"} }, nil, "dest_file.format", "csv"},
		{"file path traversal", func(s *Spec) { s.Dest = nil; s.File = &FileDest{StoreID: 1, Format: "csv", PathTemplate: "a/../b"} }, nil, "dest_file.path_template", ".."},
		{"file path abs", func(s *Spec) { s.Dest = nil; s.File = &FileDest{StoreID: 1, Format: "csv", PathTemplate: "/etc/x"} }, nil, "dest_file.path_template", "/"},
		{"file path chars", func(s *Spec) { s.Dest = nil; s.File = &FileDest{StoreID: 1, Format: "csv", PathTemplate: "a b"} }, nil, "dest_file.path_template", "只能含"},
		{"file path unknown var", func(s *Spec) { s.Dest = nil; s.File = &FileDest{StoreID: 1, Format: "csv", PathTemplate: "{foo}"} }, nil, "dest_file.path_template", "占位符"},
		{"file part_mb", func(s *Spec) {
			s.Dest = nil
			s.File = &FileDest{StoreID: 1, Format: "csv", PathTemplate: "a", PartMB: 5000}
		}, nil, "dest_file.part_mb", "MB"},
		{"batch low", func(s *Spec) { s.Params.BatchSize = 99 }, nil, "params.batch_size", "100–10000"},
		{"batch high", func(s *Spec) { s.Params.BatchSize = 10001 }, nil, "params.batch_size", "100–10000"},
		{"sleep high", func(s *Spec) { s.Params.SleepMs = 5001 }, nil, "params.sleep_ms", "5000"},
		{"sleep low move", func(s *Spec) { s.Params.SleepMs = 49 }, nil, "params.sleep_ms", "不得低于 50"},
		{"maxrows tiny", func(s *Spec) { s.Params.MaxRows = 10 }, nil, "params.max_rows", "不能小于"},
		{"threads low", func(s *Spec) { s.Params.MaxThreadsRunning = 2 }, nil, "params.max_threads_running", "4–1000"},
		{"lag high", func(s *Spec) { s.Params.MaxLagSeconds = 4000 }, nil, "params.max_lag_seconds", "3600"},
		{"disk low", func(s *Spec) { s.Params.MinDiskFreePct = 1 }, nil, "params.min_disk_free_pct", "5–50"},
		{"target low", func(s *Spec) { s.Params.TargetBatchMs = 5 }, nil, "params.target_batch_ms", "100–5000"},
		{"wait high", func(s *Spec) { s.Params.MaxWaitSeconds = 4000 }, nil, "params.max_wait_seconds", "3600"},
		{"fails high", func(s *Spec) { s.Params.MaxConsecutiveFailures = 11 }, nil, "params.max_consecutive_failures", "1–10"},
		{"lockwait high", func(s *Spec) { s.Params.LockWaitSeconds = 61 }, nil, "params.lock_wait_seconds", "60"},
		{"window bad", func(s *Spec) { s.Params.Window = Window{Start: "25:00", End: "03:00"} }, nil, "params.window", "HH:MM"},
		{"window half", func(s *Spec) { s.Params.Window = Window{Start: "01:00"} }, nil, "params.window", "HH:MM"},
		{"window short", func(s *Spec) { s.Params.Window = Window{Start: "01:00", End: "01:29"} }, nil, "params.window", "至少 30"},
		{"window short across midnight", func(s *Spec) { s.Params.Window = Window{Start: "23:50", End: "00:10"} }, nil, "params.window", "至少 30"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, p := okSpec(), okPolicy()
			c.mut(&s)
			if c.pol != nil {
				c.pol(&p)
			}
			is := Validate(s, p)
			i := issue(is, c.field)
			if i == nil {
				t.Fatalf("应在 %s 上报 error，实际：%+v", c.field, is)
			}
			if !strings.Contains(i.Msg, c.errHas) {
				t.Errorf("消息应包含 %q：%s", c.errHas, i.Msg)
			}
			if IssuesError(is) == nil {
				t.Errorf("IssuesError 应非 nil")
			}
		})
	}
}

func TestValidateAcceptsEdges(t *testing.T) {
	cases := map[string]func(*Spec, *Policy){
		"window exactly 30 min":    func(s *Spec, p *Policy) { s.Params.Window = Window{Start: "01:00", End: "01:30"} },
		"window across midnight":   func(s *Spec, p *Policy) { s.Params.Window = Window{Start: "23:00", End: "01:00"} },
		"window equal = unlimited": func(s *Spec, p *Policy) { s.Params.Window = Window{Start: "03:00", End: "03:00"} },
		"window empty":             func(s *Spec, p *Policy) { s.Params.Window = Window{} },
		"batch 100":                func(s *Spec, p *Policy) { s.Params.BatchSize = 100 },
		"batch 10000":              func(s *Spec, p *Policy) { s.Params.BatchSize = 10000 },
		"sleep 50 move":            func(s *Spec, p *Policy) { s.Params.SleepMs = 50 },
		"sleep 5000":               func(s *Spec, p *Policy) { s.Params.SleepMs = 5000 },
		"copy any sleep >= 1":      func(s *Spec, p *Policy) { s.Mode = ModeCopy; s.Params.SleepMs = 1 },
		"retention == policy min":  func(s *Spec, p *Policy) { s.RetentionDays = 30 },
		"lag disabled (warn only)": func(s *Spec, p *Policy) { s.Params.MaxLagSeconds = -1 },
		"purge allowed":            func(s *Spec, p *Policy) { s.Dest = nil; s.Mode = ModePurge; p.AllowPurgeNoBackup = true },
		"dest + file": func(s *Spec, p *Policy) {
			s.File = &FileDest{StoreID: 2, Format: "sql", PathTemplate: "{inst}/{table}/{run_id}", Gzip: true, Encrypt: true}
		},
		"dest other instance":       func(s *Spec, p *Policy) { s.Dest.InstanceID = 2; s.Dest.DB = "shop"; s.Dest.Template = "{table}" },
		"defaults from policy":      func(s *Spec, p *Policy) { s.Params = Params{}; p.Defaults = Params{BatchSize: 500, SleepMs: 100} },
		"fixed dest table":          func(s *Spec, p *Policy) { s.Dest.Template = ""; s.Dest.Table = "orders_bak" },
		"policy max rows caps only": func(s *Spec, p *Policy) { s.Params.MaxRows = 9_000_000 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s, p := okSpec(), okPolicy()
			mut(&s, &p)
			if is := Validate(s, p); HasErrors(is) {
				t.Fatalf("不应有 error：%+v", is)
			}
		})
	}
	// 警告而非错误
	s, p := okSpec(), okPolicy()
	s.Params.MaxRows = 9_000_000
	found := false
	for _, i := range Validate(s, p) {
		if i.Field == "params.max_rows" && i.Level == "warn" {
			found = true
		}
	}
	if !found {
		t.Errorf("超过策略硬上限应给出 warn")
	}
	s = okSpec()
	s.Dest = nil
	s.File = &FileDest{StoreID: 1, Format: "csv", PathTemplate: "{db}/{table}"}
	found = false
	for _, i := range Validate(s, p) {
		if i.Field == "dest_file.path_template" && i.Level == "warn" {
			found = true
		}
	}
	if !found {
		t.Errorf("路径模板没有 {run_id} 应给出 warn")
	}
}

func TestValidateNoNameCheckForRun(t *testing.T) {
	s := okSpec()
	s.Name = ""
	if !HasErrors(Validate(s, okPolicy())) {
		t.Errorf("保存时名称必填")
	}
	if HasErrors(validateSpec(s, okPolicy(), false)) {
		t.Errorf("运行时不检查名称")
	}
}

func TestEffectiveParams(t *testing.T) {
	got := EffectiveParams(Params{BatchSize: 500}, Params{BatchSize: 800, SleepMs: 120, MaxThreadsRunning: 20})
	if got.BatchSize != 500 || got.SleepMs != 120 || got.MaxThreadsRunning != 20 || got.MaxLagSeconds != 5 || got.MinDiskFreePct != 15 ||
		got.TargetBatchMs != 500 || got.MaxWaitSeconds != 600 || got.MaxConsecutiveFailures != 3 || got.LockWaitSeconds != 3 || got.MaxRows != 5_000_000 {
		t.Errorf("%+v", got)
	}
	if got := EffectiveParams(Params{}, Params{}); got.BatchSize != 1000 || got.SleepMs != 200 {
		t.Errorf("内置默认值：%+v", got)
	}
	if got := EffectiveParams(Params{MaxLagSeconds: -1}, Params{MaxLagSeconds: 9}); got.MaxLagSeconds != -1 {
		t.Errorf("负数保留为“关闭”：%+v", got)
	}
	w := Window{Start: "01:00", End: "05:00"}
	if got := EffectiveParams(Params{}, Params{Window: w}); got.Window != w {
		t.Errorf("窗口取默认：%+v", got)
	}
	if got := EffectiveMaxRows(Params{MaxRows: 9_000_000}, Policy{MaxRowsPerRun: 1_000_000}); got != 1_000_000 {
		t.Errorf("取 min：%d", got)
	}
	if got := EffectiveMaxRows(Params{MaxRows: 500}, Policy{MaxRowsPerRun: 1_000_000}); got != 500 {
		t.Errorf("取 min：%d", got)
	}
	if got := EffectiveMaxRows(Params{}, Policy{}); got != 5_000_000 {
		t.Errorf("默认：%d", got)
	}
}

func TestProtected(t *testing.T) {
	pats := []string{"Secret_*", "shop.tmp?", "audit.*"}
	cases := []struct {
		db, tbl string
		want    bool
	}{
		{"mysql", "x", true}, {"MYSQL", "", true}, {"sys", "t", true}, {"performance_schema", "t", true},
		{"secret_finance", "t", true}, {"shop", "orders", false}, {"shop", "tmp1", true}, {"shop", "tmp12", false},
		{"shop", "", false}, {"audit", "anything", true}, {"audit", "", false}, {"shopx", "tmp1", false},
	}
	for _, c := range cases {
		if got := IsProtected(pats, c.db, c.tbl); got != c.want {
			t.Errorf("IsProtected(%q,%q)=%v want %v", c.db, c.tbl, got, c.want)
		}
	}
}

func TestWindow(t *testing.T) {
	cases := []struct {
		w       Window
		minutes int
	}{
		{Window{"01:00", "05:00"}, 240}, {Window{"23:00", "01:00"}, 120}, {Window{"", ""}, 1440}, {Window{"03:00", "03:00"}, 1440},
		{Window{"1:00", "05:00"}, -1}, {Window{"01:00", ""}, -1}, {Window{"24:00", "01:00"}, -1}, {Window{"01:60", "02:00"}, -1},
	}
	for _, c := range cases {
		if got := WindowMinutes(c.w); got != c.minutes {
			t.Errorf("WindowMinutes(%+v)=%d want %d", c.w, got, c.minutes)
		}
	}
	at := func(h, m int) (tm struct{ h, m int }) { tm.h, tm.m = h, m; return }
	_ = at
	chk := func(w Window, h, m int, want bool) {
		t.Helper()
		tm := refTime.Add(0)
		tm = tm.Add(time0(h, m))
		if got := InWindow(tm, w); got != want {
			t.Errorf("InWindow(%+v, %02d:%02d)=%v want %v", w, h, m, got, want)
		}
	}
	day := Window{"01:00", "05:00"}
	chk(day, 0, 59, false)
	chk(day, 1, 0, true)
	chk(day, 4, 59, true)
	chk(day, 5, 0, false)
	night := Window{"23:00", "02:00"}
	chk(night, 22, 59, false)
	chk(night, 23, 0, true)
	chk(night, 0, 30, true)
	chk(night, 1, 59, true)
	chk(night, 2, 0, false)
	chk(Window{}, 12, 0, true)
	chk(Window{"09:00", "09:00"}, 3, 0, true)
	chk(Window{"bad", "05:00"}, 3, 0, false)
}
