// @author: kevinkelly
// @license: Apache Licence
// @file: presets.go
// @time: 2026/10/28

package btnperm

// PresetNames 是预设角色，顺序即页面上的展示顺序。
// 预设只是“套用模板”：套用后仍要保存才写库，不会自动改任何权限组。
var PresetNames = []string{"DBA", "TL", "RD", "QA", "Default", "审计"} // i18n-ignore

// 预设的白名单，与原型 prototype/js/perm.js 的 GROUPS 逐项对齐（TL / RD / QA / Default / 审计）。
// 与原型一致：TL 含“分析 / 优化 / 慢日志 / 查询权限列表”页面里除 qpriv.manage、optimize.soar、optimize.tuning 外的全部可授予按钮。
var presetKeep = map[string][]string{
	"TL": {"sqlanalyze.view", "sqlanalyze.run", "optimize.explain", "slowquery.view", "slowquery.advise", "slowlogs.view", "qpriv.list",
		"sqlworkflow.view_own", "sqlworkflow.view_group", "sqlworkflow.submit", "sqlworkflow.cancel_own", "sqlworkflow.approve", "sqlworkflow.reject", "sqlworkflow.alter_window", "sqlworkflow.rollback",
		"query.view", "query.run", "query.objinfo", "query.group_run", "qpriv.view", "qpriv.apply", "qpriv.review", "export.view", "export.submit",
		"perf.view", "perf.handle", "perf.comment", "instance.view", "partman.view",
		"archive.view", "archive.apply", "archive.dryrun", "archive.pause"},
	"RD": {"sqlworkflow.view_own", "sqlworkflow.submit", "sqlworkflow.cancel_own", "sqlworkflow.execute_own", "sqlworkflow.rollback",
		"query.view", "query.run", "query.objinfo", "qpriv.view", "qpriv.apply", "qpriv.list", "export.view", "export.submit",
		"slowlogs.view", "partman.view", "archive.view", "archive.apply", "archive.dryrun",
		"sqlanalyze.view", "sqlanalyze.run", "optimize.explain", "slowquery.view", "slowquery.advise",
		"perf.view", "perf.handle", "perf.comment"},
	"QA": {"sqlworkflow.view_own", "sqlworkflow.submit", "sqlworkflow.cancel_own", "sqlworkflow.rollback",
		"query.view", "query.run", "query.objinfo", "qpriv.view", "qpriv.apply", "qpriv.list", "export.view", "export.submit", "export.download",
		"slowlogs.view", "optimize.explain", "archive.view"},
	"Default": {"query.view", "query.run", "query.objinfo", "qpriv.view", "qpriv.apply", "qpriv.list"},
	"审计":      {"audit.view", "audit.export", "sqlworkflow.view_own", "sqlworkflow.view_group", "qpriv.view", "qpriv.list", "export.view", "archive.view"}, // i18n-ignore
}

// DBA 预设 = 全部可授予的按钮，再减去 dbaExclude；Super 类按钮里服务端已改为按钮校验的几个运维按钮在 dbaSuperAllowed 里显式纳入
// （新增的 Super 类按钮默认不进 DBA，需要时在这里明确加）。
var (
	// 与原型一致：DBA 有“立即执行（资源组内）”，所以不含“仅自己的”；Binlog 回滚 / 下载按设计只给超级用户（需要时在权限组里显式授予）。
	// audit.view / audit.export 有意不给 DBA（沿用既有设计；原型里 DBA 含这两个码，属已知差异）：需要时授予“审计”预设。
	// redis.slowlog_reset 的锁定标记待产品确认，仍只给超级用户。
	dbaExclude = map[string]bool{
		"sqlworkflow.execute_own": true, "binlog.rollback": true, "binlog.download": true,
		"audit.view": true, "audit.export": true, "redis.slowlog_reset": true,
	}
	dbaSuperAllowed = map[string]bool{
		"instance.purge_binlog": true, "inspect.run": true, "inspect.cancel": true, "inspect.delete": true,
	}
)

// Preset 返回预设角色的按钮码（登记顺序）。未知预设返回 nil。
func Preset(name string) []string {
	var out []string
	switch name {
	case "DBA":
		for _, c := range idx.codes {
			b := idx.byCode[c]
			if !b.Grantable() || dbaExclude[c] {
				continue
			}
			if b.Super && !dbaSuperAllowed[c] {
				continue
			}
			out = append(out, c)
		}
		return out
	case "TL", "RD", "QA", "Default", "审计": // i18n-ignore
		keep := map[string]bool{}
		for _, c := range presetKeep[name] {
			keep[c] = true
		}
		for _, c := range idx.codes {
			if keep[c] {
				out = append(out, c)
			}
		}
		return out
	}
	return nil
}
