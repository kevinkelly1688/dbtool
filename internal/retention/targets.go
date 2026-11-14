// @author: kevinkelly
// @license: Apache Licence
// @file: targets.go
// @time: 2026/10/28

package retention

// Kind 是目标的清理方式。
type Kind int

const (
	KindTable   Kind = iota // 按时间列分批删除表里的行
	KindExports             // 删除已结束导出工单的导出文件（保留工单记录）
	KindBackups             // 删除超期的归档文件备份（保留运行记录）
)

// Child 是随父行一起删除的子表（按外键列）。
type Child struct{ Table, FKCol string }

// Target 描述一个清理目标。表名、列名、固定条件全部是这里的常量（不接受外部输入），取值一律走参数。
type Target struct {
	Key      string
	Label    string
	Kind     Kind
	Table    string
	IDCol    string
	TimeCol  string
	Where    string // 固定附加条件（只含常量，不含参数）
	Children []Child
	Need     []string // 主表还必须有的列（缺少则跳过并记日志）
	Deps     []string // 固定条件里引用到的其它表（缺少则跳过并记日志）
	MinDays  int      // 保留天数下限，低于它视为误配置
	Risk     string   // 风险说明（文档 / 预览用）
	Suggest  string   // 建议保留天数
}

// archiveRunWhere 只清终态运行：
//   - 进行中 / 暂停中的不动；
//   - 带文件备份清单、且备份尚未“过期清理”的运行不动（运行记录是找到备份对象的唯一索引）；
//   - 任务当前显示的“最近一次运行”不动。
const archiveRunWhere = "result NOT IN ('running','paused')" +
	" AND (manifest IS NULL OR manifest = '' OR note LIKE '" + BackupPurgedPrefix + "%')" +
	" AND id NOT IN (SELECT last_run_id FROM dbt_archive_task WHERE last_run_id > 0)"

// BackupPurgedPrefix 是运行记录 note 里“备份已过期清理”标记的前缀。
const BackupPurgedPrefix = "备份已过期清理" // i18n-ignore 写进运行记录 note、并用 LIKE 匹配的库内标记

var targets = []Target{
	{Key: TargetQueryEvent, Label: "在线查询未成功记录（dbt_query_event）", Kind: KindTable, Table: "dbt_query_event", IDCol: "id", TimeCol: "created_at",
		MinDays: 7, Risk: "含 SQL 文本前 2000 字，删除后“执行历史”看不到更早的失败 / 拦截记录", Suggest: "90"},
	{Key: TargetQueryReject, Label: "在线查询被拦截记录（dbt_query_reject）", Kind: KindTable, Table: "dbt_query_reject", IDCol: "id", TimeCol: "created_at",
		MinDays: 7, Risk: "系统审计里的“被拒绝语句”读它，删除后看不到更早的拦截记录", Suggest: "90"},
	{Key: TargetQueryLog, Label: "在线查询成功记录（dbt_query_log）", Kind: KindTable, Table: "dbt_query_log", IDCol: "id", TimeCol: "create_time",
		MinDays: 7, Risk: "含完整查询语句；删除后无法追溯更早的查询审计。用户收藏的查询（favorite=1）不删", Suggest: "90–180",
		Where: "favorite = 0", Need: []string{"favorite"}},
	{Key: TargetSlowLogs, Label: "平台采集的慢日志（dbt_slow_logs）", Kind: KindTable, Table: "dbt_slow_logs", IDCol: "id", TimeCol: "start_time",
		MinDays: 7, Risk: "采集游标取实例内最大的 updated_at_micro，保留期内始终有数据，不会重复采集；“查慢日志”的统计范围随之缩短", Suggest: "30–93"},
	{Key: TargetAuditLog, Label: "操作审计日志（dbt_audit_log + dbt_audit_ext）", Kind: KindTable, Table: "dbt_audit_log", IDCol: "id", TimeCol: "action_time",
		Children: []Child{{Table: "dbt_audit_ext", FKCol: "audit_id"}},
		MinDays:  30, Risk: "合规数据：删除不可恢复，也没有防篡改保护。请先确认公司审计保留期；建议先导出 / 异步复制到只追加存储", Suggest: "按合规要求（通常 ≥ 180）"},
	{Key: TargetExportFiles, Label: "数据导出文件（磁盘文件，保留工单）", Kind: KindExports,
		MinDays: 7, Risk: "删除已结束导出工单的 zip 文件，工单记录保留，下载页提示“文件已按保留期清理”；删后需重新提交工单才能再导出", Suggest: "30"},
	{Key: TargetArchiveStore, Label: "归档文件备份（备份存储中的对象）", Kind: KindBackups,
		Risk: "按各备份存储的“保留天数”（0 = 永久）删除运行清单里记录的对象；库表备份不动；删除后无法从备份回灌", Suggest: "按数据分级，≥ 90"},
	{Key: TargetArchiveRun, Label: "归档运行记录（dbt_archive_run/event/batch_stat）", Kind: KindTable, Table: "dbt_archive_run", IDCol: "id", TimeCol: "started_at",
		Where: archiveRunWhere,
		Need:  []string{"result", "manifest", "note"}, Deps: []string{"dbt_archive_task"},
		Children: []Child{{Table: "dbt_archive_event", FKCol: "run_id"}, {Table: "dbt_archive_batch_stat", FKCol: "run_id"}},
		MinDays:  7, Risk: "只清终态运行；文件备份未过期清理的运行不删；库表备份的运行记录会删，备份表本身不动", Suggest: "180"},
}

// allTargets 返回全部目标（固定顺序，即一轮里的执行顺序）。
func allTargets() []Target { return targets }

// daysTargets 是有“retention.<key>_days”配置项的目标（归档文件备份的天数来自备份存储，不在其中）。
func daysTargets() []Target {
	out := make([]Target, 0, len(targets))
	for _, t := range targets {
		if t.Kind != KindBackups {
			out = append(out, t)
		}
	}
	return out
}

func targetByKey(key string) (Target, bool) {
	for _, t := range targets {
		if t.Key == key {
			return t, true
		}
	}
	return Target{}, false
}
