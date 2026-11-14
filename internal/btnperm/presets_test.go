// @author: kevinkelly
// @license: Apache Licence
// @file: presets_test.go
// @time: 2026/10/28

package btnperm

import (
	"sort"
	"strings"
	"testing"
)

// 以下集合照抄原型 prototype/js/perm.js 的 GROUPS（由脚本导出后固化），与 presets.go 独立：预设改动必须同时改这里。
var protoPresets = map[string][]string{
	"TL": {
		"archive.apply", "archive.dryrun", "archive.pause", "archive.view",
		"export.submit", "export.view", "instance.view",
		"optimize.explain", "partman.view", "perf.comment",
		"perf.handle", "perf.view", "qpriv.apply", "qpriv.list",
		"qpriv.review", "qpriv.view", "query.group_run", "query.objinfo", "query.run",
		"query.view", "slowlogs.view", "slowquery.advise", "slowquery.view",
		"sqlanalyze.run", "sqlanalyze.view", "sqlworkflow.alter_window", "sqlworkflow.approve",
		"sqlworkflow.cancel_own", "sqlworkflow.reject", "sqlworkflow.rollback", "sqlworkflow.submit",
		"sqlworkflow.view_group", "sqlworkflow.view_own",
	},
	"RD": {
		"archive.apply", "archive.dryrun", "archive.view",
		"export.submit", "export.view", "optimize.explain",
		"partman.view", "perf.comment", "perf.handle", "perf.view",
		"qpriv.apply", "qpriv.list", "qpriv.view", "query.objinfo", "query.run",
		"query.view", "slowlogs.view", "slowquery.advise", "slowquery.view",
		"sqlanalyze.run", "sqlanalyze.view", "sqlworkflow.cancel_own", "sqlworkflow.execute_own",
		"sqlworkflow.rollback", "sqlworkflow.submit", "sqlworkflow.view_own",
	},
	"QA": {
		"archive.view", "export.download", "export.submit",
		"export.view", "optimize.explain", "qpriv.apply", "qpriv.list",
		"qpriv.view", "query.objinfo", "query.run", "query.view", "slowlogs.view",
		"sqlworkflow.cancel_own", "sqlworkflow.rollback", "sqlworkflow.submit", "sqlworkflow.view_own",
	},
	"Default": {
		"qpriv.apply", "qpriv.list", "qpriv.view",
		"query.objinfo", "query.run", "query.view",
	},
	"审计": {
		"archive.view", "audit.export", "audit.view", "export.view",
		"qpriv.list", "qpriv.view", "sqlworkflow.view_group", "sqlworkflow.view_own",
	},
}

func sortedPreset(name string) []string {
	l := append([]string(nil), Preset(name)...)
	sort.Strings(l)
	return l
}

// S9-C3 / C17：TL / RD / QA / Default / 审计与原型逐项一致（不多不少）。
func TestPresetsMatchPrototype(t *testing.T) {
	for name, want := range protoPresets {
		got := sortedPreset(name)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			gm, wm := map[string]bool{}, map[string]bool{}
			for _, c := range got {
				gm[c] = true
			}
			for _, c := range want {
				wm[c] = true
			}
			var extra, missing []string
			for _, c := range got {
				if !wm[c] {
					extra = append(extra, c)
				}
			}
			for _, c := range want {
				if !gm[c] {
					missing = append(missing, c)
				}
			}
			t.Errorf("预设 %s 与原型不一致：多出 %v，缺少 %v", name, extra, missing)
		}
	}
}

// DBA = 原型的 DBA（可授予全部去掉 execute_own / binlog.rollback / binlog.download）再按有意差异调整：
// 不含 audit.view / audit.export（沿用既有设计），不含 redis.slowlog_reset（锁定标记待产品确认，D3），
// 含 instance.purge_binlog 与 inspect.run|cancel|delete（服务端已按按钮校验，原型也给 DBA）。
func TestPresetDBAAlignedWithPrototype(t *testing.T) {
	got := map[string]bool{}
	for _, c := range Preset("DBA") {
		got[c] = true
	}
	for _, c := range []string{"sqlworkflow.execute_own", "binlog.rollback", "binlog.download", "audit.view", "audit.export", "redis.slowlog_reset"} {
		if got[c] {
			t.Errorf("DBA 预设不应含 %s", c)
		}
	}
	for _, c := range []string{"sqlworkflow.execute_group", "instance.purge_binlog", "inspect.run", "inspect.cancel", "inspect.delete", "binlog.view", "binlog.parse", "query.admin_run"} {
		if !got[c] {
			t.Errorf("DBA 预设应含 %s", c)
		}
	}
	// 除上述有意差异外，DBA 应等于全部可授予按钮
	// 值班表的三个按钮是新功能的 Super 类按钮（没有原权限码，只能显式授予），按约定默认不进 DBA 预设。
	skip := map[string]bool{"sqlworkflow.execute_own": true, "binlog.rollback": true, "binlog.download": true, "audit.view": true, "audit.export": true, "redis.slowlog_reset": true,
		"duty.view": true, "duty.edit": true, "duty.swap": true}
	for _, c := range GrantableCodes() {
		if !skip[c] && !got[c] {
			t.Errorf("DBA 预设缺少可授予按钮 %s", c)
		}
	}
	// 锁定码永远不进任何预设
	for _, n := range PresetNames {
		for _, c := range Preset(n) {
			if b, _ := Lookup(c); b.Locked {
				t.Errorf("预设 %s 含锁定码 %s", n, c)
			}
		}
	}
}
