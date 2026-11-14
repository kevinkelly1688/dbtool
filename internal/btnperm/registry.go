// @author: kevinkelly
// @license: Apache Licence
// @file: registry.go
// @time: 2026/10/28

package btnperm

import (
	"fmt"
	"sort"
	"strings"
)

// Kind 是按钮类型。
type Kind string

const (
	KindView   Kind = "v" // 查看：页面访问的前提
	KindOp     Kind = "o" // 操作
	KindDanger Kind = "d" // 高危：删除 / 终止 / 执行 / 授权 / 导出下载等；前端二次确认，服务端对写操作审计
)

// Label 返回类型的中文名。
func (k Kind) Label() string {
	switch k {
	case KindView:
		return "查看"
	case KindOp:
		return "操作"
	case KindDanger:
		return "高危"
	}
	return string(k)
}

// Button 是登记表里的一个按钮。登记表是按钮权限的唯一事实来源：
// 权限树、服务端校验、前端的 can 判断、迁移预览都从这里生成，新模块新增按钮只改这里。
type Button struct {
	Code  string `json:"code"`  // 权限码，如 sqlworkflow.execute_group
	Label string `json:"label"` // 显示名
	Kind  Kind   `json:"kind"`

	// Legacy 是这个按钮对应的原权限码（早期版本 dbt_auth_permission 里的码）。只给一次性迁移用（dbt-admin perm migrate 按它把权限组 /
	// 用户持有的原权限码展开成按钮授权，新装库内置权限组的初始授权也走这条路）；运行时的权限判断完全不看它，所以不输出给前端。
	Legacy []string `json:"-"`

	// Open 表示“登录即可”的基础按钮：对所有启用的登录用户固定开放，不进权限树、不写库；对个别用户的“单独禁用”仍然优先。
	Open bool `json:"open,omitempty"`

	// Super 表示只有超级用户能用；Locked 表示服务端至今仍只允许超级用户（SuperuserAPI），不能授予。
	// Super 而非 Locked 的按钮（如 inspect.run）服务端已改为按钮校验，可以单独授予。
	Super  bool   `json:"super,omitempty"`
	Locked bool   `json:"locked,omitempty"`
	Note   string `json:"note,omitempty"` // 补充说明，如“原：is_staff”
}

// Grantable 表示该按钮能被授予给权限组 / 用户。
func (b Button) Grantable() bool { return !b.Locked }

// Danger 表示高危。
func (b Button) Danger() bool { return b.Kind == KindDanger }

// Page 是一个页面（对应一组菜单 / 路由）。
type Page struct {
	Key     string   `json:"key"`
	Name    string   `json:"name"`
	Route   string   `json:"route"`
	Menu    []string `json:"-"` // 页面访问用的原权限码（如 menu_sqlworkflow），只给一次性迁移的校验用；空表示仅超级用户
	Buttons []Button `json:"buttons"`
}

// Module 是一组页面（对应顶栏的一个菜单分组）。
type Module struct {
	Name  string `json:"name"`
	Pages []Page `json:"pages"`
}

// ---- 登记表构造辅助 ----

func btn(kind Kind, code, label string, legacy ...string) Button {
	return Button{Code: code, Label: label, Kind: kind, Legacy: legacy}
}
func v(code, label string, legacy ...string) Button { return btn(KindView, code, label, legacy...) }
func o(code, label string, legacy ...string) Button { return btn(KindOp, code, label, legacy...) }
func d(code, label string, legacy ...string) Button { return btn(KindDanger, code, label, legacy...) }

func (b Button) open() Button         { b.Open = true; return b }
func (b Button) super() Button        { b.Super = true; return b }
func (b Button) locked() Button       { b.Super, b.Locked = true, true; return b }
func (b Button) note(s string) Button { b.Note = s; return b }

// 系统管理里仅超级用户的按钮：服务端是 SuperuserAPI，登记出来是为了让权限树完整并说明原因。
func lv(code, label string) Button { return v(code, label).locked() }
func lo(code, label string) Button { return o(code, label).locked() }
func ld(code, label string) Button { return d(code, label).locked() }

// 这些原权限码的 app_label 是 sql_perf（其余都是 sql）。
var legacyApps = map[string]string{"menu_perf": "sql_perf", "perf_manage": "sql_perf"}

// LegacyApp 返回原权限码所属应用（app_label）。
func LegacyApp(legacy string) string {
	if a, ok := legacyApps[legacy]; ok {
		return a
	}
	return "sql"
}

// Tree 是按钮登记表：模块 → 页面 → 按钮。
//
// 登记原则：只登记真实存在、服务端有校验点的页面与按钮（没有校验点的按钮授予了也不起作用）。
// 每个页面至少有一个查看类按钮（页面访问的前提）。
var Tree = []Module{
	{"变更发布", []Page{
		{"sqlworkflow", "变更工单", "/sqlworkflow", []string{"menu_sqlworkflow"}, []Button{
			v("sqlworkflow.view_own", "查看自己的工单", "menu_sqlworkflow"),
			v("sqlworkflow.view_group", "查看资源组内所有工单", "sql_review"),
			o("sqlworkflow.submit", "提交工单", "sql_submit"),
			o("sqlworkflow.submit_batch", "提交批量上线", "sqlbatch_submit"),
			d("sqlworkflow.skip_check", "跳过语法检查").locked().note("服务端强制仅超级用户"),
			o("sqlworkflow.cancel_own", "终止自己的工单").open(),
			o("sqlworkflow.approve", "审批通过", "sql_review"),
			o("sqlworkflow.reject", "驳回 / 终止他人工单", "sql_review"),
			o("sqlworkflow.alter_window", "修改可执行时间", "sql_review"),
			d("sqlworkflow.execute_group", "立即执行（资源组内）", "sql_execute_for_resource_group"),
			d("sqlworkflow.execute_own", "立即执行（仅自己的）", "sql_execute"),
			d("sqlworkflow.schedule", "定时执行", "sql_execute"),
			d("sqlworkflow.manual_finish", "确认手工执行结束", "sql_execute"),
			o("sqlworkflow.rollback", "查看 / 下载回滚语句").open().note("能看到工单即可"),
			d("sqlworkflow.osc", "在线改表控制（暂停 / 恢复 / 终止）", "sql_execute"),
			d("sqlworkflow.cancel_run", "取消执行（执行中的工单）", "sql_execute_for_resource_group"),
			d("sqlworkflow.batch_op", "批量实例 重试 / 关闭", "sql_execute"),
		}},
		{"sqlanalyze", "SQL 体检", "/sqlanalyze", []string{"menu_sqlanalyze"}, []Button{
			v("sqlanalyze.view", "查看分析报告", "menu_sqlanalyze"),
			o("sqlanalyze.run", "上传并开始分析", "sql_analyze"),
		}},
	}},
	{"数据服务", []Page{
		{"query", "在线查询", "/query", []string{"menu_query"}, []Button{
			v("query.view", "打开在线查询", "menu_query"),
			o("query.run", "执行查询（含执行计划）", "query_submit"),
			o("query.objinfo", "查看对象信息 / 可编程对象（表结构、建表语句、定义）", "query_submit"),
			o("query.group_run", "资源组内实例按管理员行数上限查询", "query_resource_group_instance"),
			d("query.admin_run", "查看 / 查询全部实例，不受库表授权限制", "query_all_instances").note("同时决定各页面的实例可见范围"),
		}},
		{"export", "数据导出", "/sqlworkflow/export/new", []string{"menu_sqlexportworkflow", "menu_sqlexportbatchworkflow"}, []Button{
			v("export.view", "查看导出工单", "menu_sqlexportworkflow"),
			v("export.view_batch", "查看批量导出工单", "menu_sqlexportbatchworkflow"),
			o("export.submit", "提交导出", "sqlexport_submit"),
			o("export.submit_batch", "提交批量导出", "sqlexportbatch_submit"),
			d("export.download", "下载导出文件", "offline_download"),
			d("export.unmasked", "导出原值（不脱敏）").locked().note("服务端强制仅超级用户"),
		}},
		{"archive", "数据归档", "/archive", []string{"menu_archive"}, []Button{
			v("archive.view", "查看归档任务与运行记录", "menu_archive"),
			o("archive.apply", "新建 / 修改归档任务（提交申请）", "archive_apply"),
			o("archive.dryrun", "试运行", "archive_apply"),
			o("archive.pause", "暂停 / 恢复", "archive_mgt"),
			d("archive.run", "立即执行", "archive_mgt"),
			d("archive.stop", "终止运行", "archive_mgt"),
			d("archive.restore", "恢复数据（回灌）", "archive_mgt"),
			d("archive.delete", "停用 / 删除任务", "archive_mgt"),
			v("archive.cfg_view", "查看备份存储与安全策略", "menu_archive"),
			ld("archive.store_edit", "编辑备份存储"),
			ld("archive.policy_edit", "编辑安全策略"),
		}},
		{"partman", "分区维护", "/partman", []string{"menu_partman_list"}, []Button{
			v("partman.view", "查看配置与维护日志", "menu_partman_list"),
			o("partman.add", "新增配置", "add_partmanconfig"),
			o("partman.edit", "编辑配置", "change_partmanconfig"),
			d("partman.run", "立即执行维护", "change_partmanconfig"),
			d("partman.delete", "删除配置", "delete_partmanconfig"),
		}},
		{"qapply", "权限申请", "/query/applies", []string{"menu_queryapplylist"}, []Button{
			v("qpriv.view", "查看申请与审批进度", "menu_queryapplylist"),
			o("qpriv.apply", "申请 / 撤回查询权限").open(),
			o("qpriv.review", "审批查询权限申请", "query_review"),
		}},
		{"qprivs", "授权清单", "/query/privileges", []string{"query_applypriv"}, []Button{
			v("qpriv.list", "查看已授予的查询权限", "query_applypriv"),
			d("qpriv.manage", "修改 / 回收他人权限", "query_mgtpriv"),
		}},
	}},
	{"性能优化", []Page{
		{"sqladvisor", "智能调优", "/sqladvisor", []string{"menu_sqladvisor"}, []Button{
			v("optimize.explain", "执行计划 / 索引建议 / 格式化", "menu_sqladvisor"),
			o("optimize.tuning", "SQLTuning", "optimize_sqltuning"),
			o("optimize.soar", "SOAR", "optimize_soar"),
		}},
		{"slowlogs", "慢日志检索", "/slowlogs", []string{"menu_slowquery"}, []Button{
			v("slowlogs.view", "查看慢日志与明细", "menu_slowquery"),
		}},
		{"slowquery", "慢 SQL 洞察", "/slowquery", []string{"menu_slowquery"}, []Button{
			v("slowquery.view", "查看统计与明细", "menu_slowquery"),
			o("slowquery.advise", "获取优化建议", "menu_sqladvisor", "optimize_soar", "optimize_sqltuning"),
			d("redis.slowlog_reset", "重置 Redis 慢日志（SLOWLOG RESET）").super().note("重置前会先采集入库，默认仅超级用户"),
		}},
	}},
	{"性能治理", []Page{
		{"perf", "问题工单 / 详情 / 团队榜单", "/perf", []string{"menu_perf", "perf_manage"}, []Button{
			v("perf.view", "查看工单与排行榜", "menu_perf"),
			o("perf.handle", "受理 / 申请验证 / 申请延期 / 申请豁免", "menu_perf").note("另需是工单负责人或团队成员"),
			o("perf.comment", "评论", "menu_perf"),
			o("perf.create", "手工建单", "perf_manage"),
			o("perf.assign", "指派", "perf_manage"),
			o("perf.verify", "验证通过 / 不通过", "perf_manage"),
			o("perf.approve", "审批延期 / 豁免", "perf_manage"),
			o("perf.adjust", "调整级别 / 切换处理方", "perf_manage"),
			o("perf.reopen", "重开 / 重新诊断", "perf_manage"),
			d("perf.close_dup", "误报关闭", "perf_manage"),
		}},
		{"perfcfg", "采集 / 团队 / SLA 配置", "/perf/config", []string{"perf_manage"}, []Button{
			v("perfcfg.view", "查看配置", "perf_manage"),
			o("perfcfg.collect", "修改采集配置 / 试采集", "perf_manage"),
			o("perfcfg.team", "修改团队配置与路由规则", "perf_manage"),
			d("perfcfg.sla", "修改 SLA 策略", "perf_manage"),
		}},
	}},
	{"实例运维", []Page{
		{"instlist", "实例列表", "/instances", []string{"menu_instance_list"}, []Button{
			v("instance.view", "查看实例", "menu_instance_list"),
			o("instance.test", "测试连接", "add_instance", "change_instance"),
			d("instance.add", "添加实例", "add_instance"),
			d("instance.edit", "编辑 / 启停实例", "change_instance"),
			d("instance.delete", "删除实例", "delete_instance"),
			d("instance.purge_binlog", "Binlog 清理", "binlog_del").super().note("默认仅超级用户"),
		}},
		{"sessions", "会话诊断", "/instances/sessions", []string{"menu_dbdiagnostic"}, []Button{
			v("session.open", "打开会话诊断", "menu_dbdiagnostic"),
			v("session.process", "查看进程", "process_view"),
			v("session.tablespace", "查看 Top 表空间", "tablespace_view"),
			v("session.trx", "查看事务", "trx_view"),
			v("session.locks", "查看锁信息", "trxandlocks_view"),
			d("session.kill", "终止会话", "process_kill"),
		}},
		{"databases", "数据库", "/instances/databases", []string{"menu_database"}, []Button{
			v("database.view", "查看数据库", "menu_database"),
			o("database.edit", "编辑 / 录入负责人与备注", "menu_database"),
			d("database.create", "创建数据库", "menu_database"),
		}},
		{"accounts", "帐号维护", "/instances/accounts", []string{"menu_instance_account"}, []Button{
			v("account.view", "查看账号与权限", "menu_instance_account"),
			d("account.create", "创建账号", "instance_account_manage"),
			o("account.edit", "编辑 / 录入登记信息", "instance_account_manage"),
			d("account.grant", "授权 / 回收", "instance_account_manage"),
			d("account.pwd", "修改密码", "instance_account_manage"),
			d("account.lock", "锁定 / 解锁", "instance_account_manage"),
			d("account.delete", "删除账号", "instance_account_manage"),
		}},
		{"params", "参数管理", "/instances/params", []string{"menu_param"}, []Button{
			v("param.open", "打开参数管理", "menu_param"),
			v("param.view", "查看参数与修改历史", "param_view"),
			d("param.edit", "修改参数值", "param_edit"),
			o("param.template", "添加 / 编辑参数模板", "add_paramtemplate", "change_paramtemplate"),
		}},
		{"inspect", "性能巡检", "/inspection", []string{"menu_mssqlinspection"}, []Button{
			v("inspect.view", "查看报告与定时计划", "menu_mssqlinspection"),
			o("inspect.run", "立即 / 重新巡检").super().note("默认仅超级用户，可显式授予"),
			o("inspect.cancel", "取消进行中的巡检").super(),
			d("inspect.delete", "删除报告").super().note("默认仅超级用户，可显式授予"),
			o("inspect.plan_edit", "新建 / 编辑定时计划", "inspection_plan_manage"),
			d("inspect.plan_toggle", "暂停 / 启用 / 删除定时计划", "inspection_plan_manage"),
			d("inspect.plan_run", "立即运行定时计划", "inspection_plan_manage"),
		}},
		{"binlog", "日志解析", "/binlog", []string{"menu_my2sql"}, []Button{
			v("binlog.view", "打开日志解析、查看 binlog 文件列表", "menu_my2sql"),
			o("binlog.parse", "解析 binlog 还原 SQL", "menu_my2sql").note("结果含业务数据，仅限有权限者"),
			d("binlog.rollback", "生成回滚 SQL").super().note("默认仅超级用户，需要时在权限组里显式授予"),
			d("binlog.download", "下载解析 / 回滚结果文件").super().note("默认仅超级用户；每次下载写审计"),
		}},
	}},
	{"访问控制", []Page{
		{"user", "用户", "/settings/users", nil, []Button{
			lv("user.view", "查看用户"), lo("user.edit", "新建 / 编辑用户"), lo("user.unlock", "解锁账号"), ld("user.reset", "重置密码 / 重置 2FA"),
			ld("user.disable", "停用 / 启用"), ld("user.grant_super", "授予超级用户"),
		}},
		{"ag", "权限组", "/settings/auth-groups", nil, []Button{
			lv("ag.view", "查看权限组"), ld("ag.edit", "新建 / 编辑权限与成员"), ld("ag.delete", "删除权限组"), ld("ag.btnperm", "按钮权限与显示方式"),
		}},
		{"rg", "资源组", "/settings/resource-groups", nil, []Button{
			lv("rg.view", "查看资源组"), lo("rg.edit", "新建 / 编辑资源组与通知群"), lo("rg.relations", "调整成员与实例"), ld("rg.delete", "删除 / 恢复资源组"),
		}},
		{"authp", "登录认证", "/settings/auth-providers", nil, []Button{
			lv("authp.view", "查看登录方式"), lo("authp.test", "测试连接 / 测试登录"), ld("authp.edit", "编辑配置 / 入组规则"), ld("authp.toggle", "启用 / 停用 / 排序 / 删除"),
		}},
		{"sec", "安全策略", "/settings/security", nil, []Button{
			lv("sec.view", "查看"), ld("sec.edit", "修改"),
		}},
	}},
	{"安全合规", []Page{
		{"flow", "审批流程", "/settings/audit-flow", nil, []Button{
			lv("flow.view", "查看审批流"), ld("flow.edit", "变更审批流"),
		}},
		{"redisrule", "Redis 规则", "/settings/redis-rules", nil, []Button{
			lv("redisrule.view", "查看 Redis 规则"), ld("redisrule.edit", "新增 / 修改 / 启停 / 删除规则"),
		}},
		{"audit", "审计日志", "/audit", []string{"audit_user"}, []Button{
			v("audit.view", "查看审计日志", "audit_user"),
			d("audit.export", "导出审计日志", "audit_user"),
		}},
	}},
	{"平台设置", []Page{
		{"env", "环境设置", "/settings/environments", nil, []Button{
			lv("env.view", "查看环境字典"), ld("env.edit", "新增 / 修改 / 删除环境"),
		}},
		{"notify", "通知渠道", "/settings/notify", nil, []Button{
			lv("notify.view", "查看渠道"), lo("notify.edit", "新建 / 编辑渠道与 @ 规则"), lo("notify.test", "测试发送"),
		}},
		{"goinception", "审核引擎", "/settings/goinception", nil, []Button{
			lv("goinception.view", "查看连接信息与配置"), ld("goinception.conn", "修改连接信息 / 备份库"), ld("goinception.file", "修改配置文件（仅保存）"),
			ld("goinception.restart", "保存并重启 / 仅重启"), ld("goinception.rollback", "回滚历史版本"),
		}},
		// 值班表：顶栏的值班信息所有登录用户都能看；下面三个按钮只管“值班表”管理页，没有原权限码，默认只有超级用户，要在权限组里显式授予。
		{"duty", "值班表", "/settings/duty", nil, []Button{
			v("duty.view", "查看值班表管理页").super().note("默认仅超级用户，可显式授予"),
			d("duty.edit", "新增 / 修改 / 停用 / 删除班次").super().note("默认仅超级用户，可显式授予"),
			o("duty.swap", "新增 / 撤销临时调班").super().note("默认仅超级用户，可显式授予"),
		}},
	}},
}

// ---- 索引与查询 ----

type index struct {
	byCode    map[string]*Button
	pageOf    map[string]*Page
	moduleOf  map[string]string
	codes     []string
	byLegacy  map[string][]string // 原权限码 → 声明了该 Legacy 的按钮（只给一次性迁移用）
	legacyAll []string
}

var idx = buildIndex()

func buildIndex() *index {
	x := &index{byCode: map[string]*Button{}, pageOf: map[string]*Page{}, moduleOf: map[string]string{}, byLegacy: map[string][]string{}}
	seenLegacy := map[string]bool{}
	for mi := range Tree {
		for pi := range Tree[mi].Pages {
			p := &Tree[mi].Pages[pi]
			for bi := range p.Buttons {
				b := &p.Buttons[bi]
				x.byCode[b.Code] = b
				x.pageOf[b.Code] = p
				x.moduleOf[b.Code] = Tree[mi].Name
				x.codes = append(x.codes, b.Code)
				for _, l := range b.Legacy {
					x.byLegacy[l] = append(x.byLegacy[l], b.Code)
					seenLegacy[l] = true
				}
			}
		}
	}
	for l := range seenLegacy {
		x.legacyAll = append(x.legacyAll, l)
	}
	sort.Strings(x.legacyAll)
	return x
}

// Modules 返回完整登记表（只读使用，不要修改）。
func Modules() []Module { return Tree }

// Codes 返回全部按钮码（按登记顺序）。
func Codes() []string { return append([]string(nil), idx.codes...) }

// GrantableCodes 返回可授予的按钮码。
func GrantableCodes() []string {
	var out []string
	for _, c := range idx.codes {
		if idx.byCode[c].Grantable() {
			out = append(out, c)
		}
	}
	return out
}

// Lookup 按码查按钮。
func Lookup(code string) (Button, bool) {
	b, ok := idx.byCode[code]
	if !ok {
		return Button{}, false
	}
	return *b, true
}

// Valid 判断按钮码是否已登记。
func Valid(code string) bool { _, ok := idx.byCode[code]; return ok }

// IsOpen 报告 code 是不是“登录即可”的基础按钮（登记表里标了 Open）。测试 / 演示用的内存仓库用它给基础按钮放行，
// 生产环境由 EvalButton 判定。
func IsOpen(code string) bool { b, ok := idx.byCode[code]; return ok && b.Open }

// PageOf 返回按钮所在页面与模块名。
func PageOf(code string) (Page, string, bool) {
	p, ok := idx.pageOf[code]
	if !ok {
		return Page{}, "", false
	}
	return *p, idx.moduleOf[code], true
}

// ButtonsForLegacy 返回声明了该原权限码的全部按钮：迁移时“每个原权限码 → 它对应的全部按钮”。
func ButtonsForLegacy(legacy string) []Button {
	var out []Button
	for _, c := range idx.byLegacy[legacy] {
		out = append(out, *idx.byCode[c])
	}
	return out
}

// LegacyCodes 返回登记表里出现过的全部原权限码（已排序）。
func LegacyCodes() []string { return append([]string(nil), idx.legacyAll...) }

// Counts 返回按钮总数、可授予数、高危数（可授予里的高危）。
func Counts() (total, grantable, danger int) {
	for _, c := range idx.codes {
		b := idx.byCode[c]
		total++
		if b.Grantable() {
			grantable++
			if b.Danger() {
				danger++
			}
		}
	}
	return
}

// Validate 检查登记表自身的一致性（init 时不调用；测试与启动自检用）。
func Validate() error {
	seen := map[string]bool{}
	pages := map[string]bool{}
	for _, m := range Tree {
		if m.Name == "" || len(m.Pages) == 0 {
			return fmt.Errorf("模块 %q 没有页面", m.Name) // i18n-ignore
		}
		for _, p := range m.Pages {
			if p.Key == "" || pages[p.Key] {
				return fmt.Errorf("页面键 %q 为空或重复", p.Key) // i18n-ignore
			}
			pages[p.Key] = true
			if len(p.Buttons) == 0 {
				return fmt.Errorf("页面 %s 没有按钮", p.Key) // i18n-ignore
			}
			hasMenuView := len(p.Menu) == 0
			for _, b := range p.Buttons {
				if b.Code == "" || seen[b.Code] {
					return fmt.Errorf("按钮码 %q 为空或重复", b.Code) // i18n-ignore
				}
				seen[b.Code] = true
				if i := strings.Index(b.Code, "."); i <= 0 || i == len(b.Code)-1 || len(b.Code) > 64 {
					return fmt.Errorf("按钮码 %q 格式应为 前缀.动作", b.Code) // i18n-ignore
				}
				if b.Label == "" {
					return fmt.Errorf("按钮 %s 没有显示名", b.Code) // i18n-ignore
				}
				if b.Kind != KindView && b.Kind != KindOp && b.Kind != KindDanger {
					return fmt.Errorf("按钮 %s 的类型 %q 无效", b.Code, b.Kind) // i18n-ignore
				}
				for _, l := range b.Legacy {
					if l == "" || strings.ContainsAny(l, " ,+") {
						return fmt.Errorf("按钮 %s 的原权限码 %q 非法", b.Code, l) // i18n-ignore
					}
				}
				if b.Locked && !b.Super {
					return fmt.Errorf("按钮 %s：Locked 必须同时 Super", b.Code) // i18n-ignore
				}
				if b.Locked && len(b.Legacy) > 0 {
					return fmt.Errorf("按钮 %s：仅超管按钮不应有原权限码", b.Code) // i18n-ignore
				}
				if b.Open && (b.Super || len(b.Legacy) > 0) {
					return fmt.Errorf("按钮 %s：基础按钮（Open）不能是仅超管、也不能有原权限码", b.Code) // i18n-ignore
				}
				if b.Kind == KindView {
					for _, mn := range p.Menu {
						if contains(b.Legacy, mn) {
							hasMenuView = true
						}
					}
				}
			}
			if !hasMenuView {
				return fmt.Errorf("页面 %s 缺少能满足页面访问码 %v 的查看类按钮", p.Key, p.Menu) // i18n-ignore
			}
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
