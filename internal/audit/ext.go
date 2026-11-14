// @author: kevinkelly
// @license: Apache Licence
// @file: ext.go
// @time: 2026/10/28

package audit

// 审计日志（dbt_audit_log）的扩展信息：结果、来源 IP、类别 / 模块、权限码、高危标记。
//
// 不在 dbt_audit_log 上加列；这些信息放在旁路表 dbt_audit_ext（deploy/sql/090_audit_ext.sql，audit_id 一对一）。
// 各模块写审计的方式是直接 db.Create(&model.AuditEntry{...})，没有统一的写入函数，
// 所以这里在 GORM 层注册一个“创建 dbt_audit_log 之后”的回调（RegisterExtHook，由 store.Open 调用一次），
// 对所有调用方统一生效、调用方一行代码都不用改：
//   - 结果 / 类别 / 模块 / 权限码 / 高危：由 Classify 按动作名与额外信息推断（动作名是各模块写死的中文，见 ext_test.go 的清单）；
//   - 来源 IP：登录类审计从额外信息里取；其余由 HTTP 中间件（BindIP）把本次请求的客户端 IP 绑定到处理它的 goroutine，
//     回调在同一个 goroutine 里读取。后台任务（定时巡检等）没有请求，IP 为空。
//
// 表不存在或写入失败只记一次日志并暂停 1 分钟，绝不影响审计本身的写入。

import (
	"bytes"
	"log/slog"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"dbtool/internal/btnperm"
	"dbtool/internal/model"
)

// loginCode 是登录审计的权限码（与原型一致；不是按钮登记表里的按钮，登录无需授权）。
const loginCode = "auth.login"

// ExtTable 是审计扩展表名。
const ExtTable = "dbt_audit_ext"

// 结果取值。
const (
	ResultOK     = "成功" // i18n-ignore
	ResultFailed = "失败" // i18n-ignore
	ResultDenied = "拒绝" // i18n-ignore
)

// 类别取值（与原型“动作类别”一致）。
const (
	CatLogin   = "login"
	CatSQLFlow = "sqlflow"
	CatQuery   = "query"
	CatInst    = "inst"
	CatPerf    = "perf"
	CatOpt     = "opt"
	CatSys     = "sys"
	CatOther   = "other"
)

// Categories 是类别及显示名，按展示顺序。
var Categories = []struct{ Key, Label string }{
	{CatLogin, "登录认证"}, {CatSQLFlow, "SQL 上线"}, {CatQuery, "查询与导出"}, {CatInst, "实例管理"},
	{CatPerf, "性能工单"}, {CatOpt, "SQL 优化"}, {CatSys, "系统管理"}, {CatOther, "其他"},
}

// CategoryLabel 返回类别的显示名；空串（历史行没有扩展信息）返回空。
func CategoryLabel(key string) string {
	for _, c := range Categories {
		if c.Key == key {
			return c.Label
		}
	}
	return ""
}

// ValidCategory 判断类别取值是否合法。
func ValidCategory(key string) bool { return CategoryLabel(key) != "" }

// Ext 是一条审计的扩展信息。
type Ext struct {
	Result   string
	IP       string
	Category string
	Module   string
	Code     string
	Risky    bool
}

// catOfCode 按权限码前缀推断类别（同原型 catOf）。
func catOfCode(code string) string {
	switch strings.SplitN(code, ".", 2)[0] {
	case "auth":
		return CatLogin
	case "sqlworkflow", "sqlanalyze":
		return CatSQLFlow
	case "query", "qpriv", "export", "partman", "slowlogs", "archive":
		return CatQuery
	case "instance", "session", "database", "account", "param", "inspect", "binlog":
		return CatInst
	case "perf", "perfcfg":
		return CatPerf
	case "optimize", "slowquery":
		return CatOpt
	}
	return CatSys
}

type rule struct {
	cat    string // 为空则按 code 推断
	module string // 为空则取 code 所在页面名
	code   string
}

func r(code string) rule                { return rule{code: code} }
func rm(cat, module, code string) rule  { return rule{cat: cat, module: module, code: code} }
func login(code string) rule            { return rm(CatLogin, "登录认证", code) }
func perfRule(module, code string) rule { return rm(CatPerf, module, code) }
func sysRule(module, code string) rule  { return rm(CatSys, module, code) }

// exactRules 按动作名精确匹配。动作名是各模块写 dbt_audit_log 时用的字面值。
var exactRules = map[string]rule{
	// 登录认证
	"登入": login(loginCode), "登入失败": login(loginCode), "账号锁定": login(""), "注册用户": login(""), "自动创建用户": login(""),
	// 退出 / 页面水印 / 无权限拒绝（S7a-A2、S9-A3、S9-A4）
	"退出登录": login(""), "水印篡改": sysRule("页面水印", ""),
	// 登录方式 / 安全设置
	"登录方式-新增": r("authp.edit"), "登录方式-修改": r("authp.edit"), "登录方式-通用规则": r("authp.edit"),
	"登录方式-启用": r("authp.toggle"), "登录方式-停用": r("authp.toggle"), "登录方式-删除": r("authp.toggle"), "登录方式-排序": r("authp.toggle"),
	"登录方式-测试": r("authp.test"), "安全设置-修改": r("sec.edit"), "安全设置-短信测试": r("sec.edit"),
	// 系统管理
	"新建资源组": r("rg.edit"), "修改资源组": r("rg.edit"), "停用资源组": r("rg.delete"), "恢复资源组": r("rg.delete"),
	"彻底删除资源组": r("rg.delete"), "调整资源组关联用户": r("rg.relations"), "调整资源组关联实例": r("rg.relations"),
	"启用用户": r("user.disable"), "停用用户": r("user.disable"),
	"新建用户": r("user.edit"), "修改用户": r("user.edit"), "解锁账号": r("user.unlock"), "重置密码": r("user.reset"), "重置 2FA": r("user.reset"),
	"调整用户单独授权": r("user.edit"),
	"新建权限组":    r("ag.edit"), "修改权限组": r("ag.edit"), "删除权限组": r("ag.delete"),
	"修改按钮权限": r("ag.btnperm"), "修改按钮权限设置": r("ag.btnperm"), "迁移原权限到按钮权限": r("ag.btnperm"),
	"撤销按钮单独授权/禁用": r("ag.btnperm"), "单独授权按钮": r("ag.btnperm"), "单独禁用按钮": r("ag.btnperm"),
	"修改审批流": r("flow.edit"), "删除审批流": r("flow.edit"),
	"导出审计日志": r("audit.export"),
	// 数据清理（internal/retention）：定时清理任务以系统身份写“数据清理”，超级用户查看 dry-run 写“数据清理预览”；没有对应按钮码（仅超级用户接口），归系统管理
	"数据清理": sysRule("数据清理", ""), "数据清理预览": sysRule("数据清理", ""),
	// 系统配置页（internal/sysconfig）里修改配置项；dbt-admin reencrypt 在库里原地换密钥。都没有按钮码（仅超级用户 / 命令行），归系统管理
	"修改系统配置项": sysRule("系统配置", ""), "密文重加密": sysRule("系统配置", ""),
	// 性能工单里需要 perf.* / perfcfg.* 的审批 / 验证 / 误报关闭（动作名取自 perf.actionName；其余性能工单动作仍按“性能工单-”前缀归类、不带权限码）
	"性能工单-验证通过": perfRule("性能工单", "perf.verify"), "性能工单-验证不通过": perfRule("性能工单", "perf.verify"),
	"性能工单-同意豁免": perfRule("性能工单", "perf.approve"), "性能工单-驳回豁免": perfRule("性能工单", "perf.approve"),
	"性能工单-同意延期": perfRule("性能工单", "perf.approve"), "性能工单-驳回延期": perfRule("性能工单", "perf.approve"),
	"性能工单-误报关闭": perfRule("性能工单", "perf.close_dup"),
	// goInception
	"修改goInception连接信息": r("goinception.conn"), "修改goInception部署设置": r("goinception.restart"), "保存goInception配置": r("goinception.file"),
	"导入goInception配置文件": r("goinception.file"), "重启goInception": r("goinception.restart"), "回滚goInception配置": r("goinception.rollback"),
	// 查询与导出
	"离线下载": r("export.download"),
	// 已去掉的“数据字典”导出：历史审计记录里还有这个动作名，继续归在“查询与导出”，没有按钮权限码
	"导出数据字典":   rm(CatQuery, "数据字典", ""),
	"直接授予查询权限": r("qpriv.manage"), "修改查询权限": r("qpriv.manage"), "回收查询权限": r("qpriv.manage"), "查看回滚语句": r("sqlworkflow.rollback"), "下载回滚语句": r("sqlworkflow.rollback"),
	// SQL 优化
	"慢日志获取优化建议": r("slowquery.advise"), "SQL分析": r("sqlanalyze.run"), "SQL分析-取消": r("sqlanalyze.run"),
	// 实例管理
	"创建数据库账号": r("account.create"), "登记账号信息": r("account.edit"), "账号授权": r("account.grant"), "账号回收权限": r("account.grant"),
	"修改数据库账号密码": r("account.pwd"), "解锁数据库账号": r("account.lock"), "锁定数据库账号": r("account.lock"), "删除数据库账号": r("account.delete"),
	"创建数据库": r("database.create"), "登记数据库信息": r("database.edit"),
	"测试连接": r("instance.test"), "新增实例": r("instance.add"), "修改实例": r("instance.edit"), "切换慢日志采集": r("instance.edit"), "删除实例": r("instance.delete"),
	"清理Binlog": r("instance.purge_binlog"), "修改实例参数": r("param.edit"), "保存参数模板": r("param.template"), "终止会话": r("session.kill"),
}

// 性能巡检的动作写成“性能巡检·<动作>”。
var inspectRules = map[string]rule{
	"立即巡检": r("inspect.run"), "取消巡检": r("inspect.cancel"), "删除报告": r("inspect.delete"),
	"新建计划": r("inspect.plan_edit"), "修改计划": r("inspect.plan_edit"), "转为正式计划": r("inspect.plan_edit"),
	"启用计划": r("inspect.plan_toggle"), "停用计划": r("inspect.plan_toggle"), "删除计划": r("inspect.plan_toggle"),
	"立即运行计划": r("inspect.plan_run"), "定时运行计划": r("inspect.plan_run"),
}

var (
	codeInExtra = regexp.MustCompile(`（([a-z]+\.[a-z_]+)）`)
	leadingIP   = regexp.MustCompile(`^((?:\d{1,3}\.){3}\d{1,3}|(?:[0-9A-Fa-f]{0,4}:){2,7}[0-9A-Fa-f]{0,4})(?:（|$)`)
	failedRe    = regexp.MustCompile(`结果[:：]\s*(失败|错误|异常|出错)`) // i18n-ignore
)

// DeniedSuffix 是“被拒绝的操作”审计动作名的后缀（如 “删除实例（拒绝）”）：归类同原动作，结果为“拒绝”。
const DeniedSuffix = "（拒绝）" // i18n-ignore

// ActionNoPerm 是点了无权限的按钮（接口返回 403）时写的审计动作名；额外信息形如 “终止会话（session.kill）· POST /api/v1/…”。
const ActionNoPerm = "拒绝（无权限）"

func ruleOf(action, extra string) rule {
	if base, ok := strings.CutSuffix(strings.TrimSpace(action), DeniedSuffix); ok && base != "" {
		return ruleOf(base, extra)
	}
	if a := strings.TrimSpace(action); a != "" {
		if x, ok := exactRules[a]; ok {
			return x
		}
	}
	switch {
	case action == "高危按钮操作", action == ActionNoPerm: // i18n-ignore
		// 额外信息形如 “终止会话（session.kill）· POST /api/v1/…”
		if m := codeInExtra.FindStringSubmatch(extra); m != nil {
			return r(m[1])
		}
		return sysRule("", "")
	case strings.HasPrefix(action, "2FA-"):
		return login("")
	case strings.HasPrefix(action, "性能巡检·"): // i18n-ignore
		if x, ok := inspectRules[strings.TrimPrefix(action, "性能巡检·")]; ok { // i18n-ignore
			return x
		}
		return rm(CatInst, "性能巡检", "")
	case strings.HasPrefix(action, "SQL优化-"): // i18n-ignore
		switch rest := strings.TrimPrefix(action, "SQL优化-"); { // i18n-ignore
		case strings.Contains(rest, "Tuning"):
			return r("optimize.tuning")
		case strings.Contains(rest, "SOAR"):
			return r("optimize.soar")
		}
		return r("optimize.explain")
	case strings.HasPrefix(action, "分区维护-"): // i18n-ignore
		switch rest := strings.TrimPrefix(action, "分区维护-"); { // i18n-ignore
		case strings.Contains(rest, "删除"): // i18n-ignore
			return r("partman.delete")
		case strings.Contains(rest, "新增") || strings.Contains(rest, "新建") || strings.Contains(rest, "添加"): // i18n-ignore
			return r("partman.add")
		case strings.Contains(rest, "执行"): // i18n-ignore
			return r("partman.run")
		}
		return r("partman.edit")
	case strings.HasPrefix(action, "数据归档-"): // i18n-ignore internal/archive：任务 / 审批 / 运行控制 / 备份存储 / 安全策略
		return archiveRule(strings.TrimPrefix(action, "数据归档-")) // i18n-ignore
	case strings.HasPrefix(action, ActionPrefixFirewall): // internal/firewalladmin：规则 / 例外 / 全局模式 / 总开关
		return sysRule("SQL 防火墙", "")
	case strings.HasPrefix(action, "性能工单配置-"): // i18n-ignore
		switch rest := strings.TrimPrefix(action, "性能工单配置-"); { // i18n-ignore
		case strings.HasPrefix(rest, "采集配置") || strings.HasPrefix(rest, "批量生成采集配置"): // i18n-ignore
			return r("perfcfg.collect")
		case strings.HasPrefix(rest, "SLA"):
			return r("perfcfg.sla")
		}
		return r("perfcfg.team")
	case strings.HasPrefix(action, "性能工单-"): // i18n-ignore
		return perfRule("性能工单", "")
	case strings.HasPrefix(action, "Binlog解析-"): // i18n-ignore internal/binlog：解析 / 回滚 / 下载 / 取消
		switch strings.TrimPrefix(action, "Binlog解析-") { // i18n-ignore
		case "回滚": // i18n-ignore
			return r("binlog.rollback")
		case "下载": // i18n-ignore
			return r("binlog.download")
		}
		return r("binlog.parse")
	case strings.HasPrefix(action, "Redis上线规则-"): // i18n-ignore internal/redisrule：管理员增删改 / 启停 Redis 上线规则
		return r("redisrule.edit")
	case strings.HasPrefix(action, "Redis上线-"): // i18n-ignore internal/sqlflow：Redis 工单的检查摘要 / 执行
		switch strings.TrimPrefix(action, "Redis上线-") { // i18n-ignore
		case "执行": // i18n-ignore
			return r("sqlworkflow.execute_group")
		}
		return r("sqlworkflow.submit")
	case strings.HasPrefix(action, "Redis慢查询-"): // i18n-ignore internal/slowlog：重置 Redis 慢日志（SLOWLOG RESET）
		return r("redis.slowlog_reset")
	case action == "通知渠道-测试发送": // i18n-ignore
		return r("notify.test")
	case strings.HasPrefix(action, "通知渠道-"): // i18n-ignore
		return r("notify.edit")
	case strings.HasPrefix(action, ActionPrefixSQLFlow):
		return sqlFlowRule(strings.TrimPrefix(action, ActionPrefixSQLFlow))
	case strings.HasPrefix(action, ActionPrefixPriv):
		return privApplyRule(strings.TrimPrefix(action, ActionPrefixPriv))
	}
	return rule{}
}

// archiveRule 是数据归档动作（动作名 “数据归档-<动作>”，由 internal/archive 写入）的归类。
func archiveRule(rest string) rule {
	switch {
	case rest == "提交申请" || rest == "保存草稿" || rest == "修改任务": // i18n-ignore
		return r("archive.apply")
	case rest == "试运行": // i18n-ignore
		return r("archive.dryrun")
	case rest == "暂停任务" || rest == "恢复任务" || rest == "暂停运行" || rest == "恢复运行": // i18n-ignore
		return r("archive.pause")
	case rest == "立即执行": // i18n-ignore
		return r("archive.run")
	case rest == "终止运行": // i18n-ignore
		return r("archive.stop")
	case rest == "恢复数据": // i18n-ignore
		return r("archive.restore")
	case rest == "停用任务" || rest == "删除任务": // i18n-ignore
		return r("archive.delete")
	case strings.HasPrefix(rest, "存储-"): // i18n-ignore
		return r("archive.store_edit")
	case strings.HasPrefix(rest, "策略"): // i18n-ignore
		return r("archive.policy_edit")
	}
	return r("archive.view") // 审批通过 / 驳回 / 运行结束 / 运行中断：归到数据归档，不带高危
}

// sqlFlowRule 是 SQL 上线审批类动作（audit.Lifecycle 写入，动作名 “SQL上线-<动作>”）的归类。
func sqlFlowRule(rest string) rule {
	switch {
	case rest == "提交": // i18n-ignore
		return r("sqlworkflow.submit")
	case rest == "审批通过": // i18n-ignore
		return r("sqlworkflow.approve")
	case rest == "驳回": // i18n-ignore
		return r("sqlworkflow.reject")
	case rest == "撤销": // i18n-ignore
		return r("sqlworkflow.cancel_own")
	case rest == "立即执行": // i18n-ignore
		return r("sqlworkflow.execute_group")
	case rest == "手工执行确认": // i18n-ignore
		return r("sqlworkflow.manual_finish")
	case rest == "定时执行" || rest == "取消定时": // i18n-ignore
		return r("sqlworkflow.schedule")
	case rest == "修改可执行时间": // i18n-ignore
		return r("sqlworkflow.alter_window")
	case strings.HasPrefix(rest, "在线改表"): // i18n-ignore
		return r("sqlworkflow.osc")
	case strings.HasPrefix(rest, "批量实例"): // i18n-ignore
		return r("sqlworkflow.batch_op")
	case rest == "执行状态恢复": // i18n-ignore 启动恢复：系统把停在“执行中”且无进程执行的工单置为执行有异常，不对应任何用户按钮
		return rm(CatSQLFlow, "SQL 上线", "")
	}
	return r("sqlworkflow.view_own") // 不认识的新动作：归到 SQL 上线类别，不带高危
}

// privApplyRule 是查询权限申请审批类动作（动作名 “查询权限申请-<动作>”）的归类。
func privApplyRule(rest string) rule {
	switch rest {
	case "审批通过", "驳回": // i18n-ignore
		return r("qpriv.review")
	}
	return r("qpriv.apply") // 提交、撤销
}

// Classify 按动作名与额外信息推断审计的结果 / 类别 / 模块 / 权限码 / 高危标记（不含来源 IP）。无法归类的动作类别为 other。
func Classify(action, extra string) Ext {
	ru := ruleOf(action, extra)
	e := Ext{Code: ru.code, Category: ru.cat, Module: ru.module, Result: resultOf(action, extra)}
	if e.Category == "" {
		if e.Code != "" {
			e.Category = catOfCode(e.Code)
		} else {
			e.Category = CatOther
		}
	}
	if e.Code != "" {
		if p, _, ok := btnperm.PageOf(e.Code); ok && e.Module == "" {
			e.Module = p.Name
		}
		if b, ok := btnperm.Lookup(e.Code); ok && b.Danger() {
			e.Risky = true
		}
	}
	if action == "高危按钮操作" { // i18n-ignore
		e.Risky = true
	}
	if strings.HasPrefix(action, ActionPrefixFirewall) && strings.HasPrefix(extra, FirewallRiskyMark) {
		e.Risky = true
	}
	if e.Module == "" {
		e.Module = "—"
	}
	return e
}

func resultOf(action, extra string) string {
	switch {
	case strings.HasSuffix(strings.TrimSpace(action), DeniedSuffix), action == ActionNoPerm:
		return ResultDenied
	case action == "登入失败": // i18n-ignore
		if strings.Contains(extra, "锁定") { // i18n-ignore
			return ResultDenied
		}
		return ResultFailed
	case action == "账号锁定", action == "2FA-验证失败过多": // i18n-ignore
		return ResultDenied
	case strings.Contains(action, "失败"): // i18n-ignore
		return ResultFailed
	case action == "登录方式-测试" && strings.HasSuffix(strings.TrimSpace(extra), "false"): // i18n-ignore
		return ResultFailed
	case failedRe.MatchString(extra):
		return ResultFailed
	}
	return ResultOK
}

// ipFromEntry 取登录类审计额外信息里的 IP：登入 “1.2.3.4（本地）”，注册用户 “1.2.3.4”。
func ipFromEntry(action, extra string) string {
	if action != "登入" && action != "注册用户" { // i18n-ignore
		return ""
	}
	if m := leadingIP.FindStringSubmatch(strings.TrimSpace(extra)); m != nil {
		return m[1]
	}
	return ""
}

// ---- 请求 IP 与 goroutine 的绑定 ----

var reqIPs sync.Map // goroutine id -> ip

func goid() uint64 {
	var buf [64]byte
	b := buf[:runtime.Stack(buf[:], false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	if i := bytes.IndexByte(b, ' '); i > 0 {
		if n, err := strconv.ParseUint(string(b[:i]), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// BindIP 把客户端 IP 绑定到当前 goroutine（HTTP 中间件在处理请求前调用），返回的函数必须在请求结束时调用（defer）。
// 同一请求里同步执行的审计写入会带上这个 IP；请求处理中另起的 goroutine 看不到它（IP 为空）。
func BindIP(ip string) (release func()) {
	id := goid()
	if id == 0 || ip == "" {
		return func() {}
	}
	reqIPs.Store(id, ip)
	return func() { reqIPs.Delete(id) }
}

func boundIP() string {
	if id := goid(); id != 0 {
		if v, ok := reqIPs.Load(id); ok {
			return v.(string)
		}
	}
	return ""
}

// ---- GORM 回调 ----

var extDisabledUntil atomic.Int64 // unix 秒；写扩展表失败（多半是表还没建）后暂停到这个时间

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// RegisterExtHook 在 db 上登记“创建 dbt_audit_log 之后写 dbt_audit_ext”的回调。重复登记同名回调会被忽略（返回的 error 已丢弃）。
func RegisterExtHook(db *gorm.DB) {
	_ = db.Callback().Create().After("gorm:create").Register("dbt:audit_ext", afterCreateAudit)
}

func afterCreateAudit(tx *gorm.DB) {
	if tx.Error != nil || tx.DryRun || tx.Statement == nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "dbt_audit_log" {
		return
	}
	if time.Now().Unix() < extDisabledUntil.Load() {
		return
	}
	rv := tx.Statement.ReflectValue
	var entries []model.AuditEntry
	switch rv.Kind() {
	case reflect.Struct:
		if e, ok := rv.Interface().(model.AuditEntry); ok {
			entries = append(entries, e)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			item := rv.Index(i)
			for item.Kind() == reflect.Pointer && !item.IsNil() {
				item = item.Elem()
			}
			if e, ok := item.Interface().(model.AuditEntry); ok {
				entries = append(entries, e)
			}
		}
	}
	if len(entries) == 0 {
		return
	}
	ip := boundIP()
	db := tx.Session(&gorm.Session{NewDB: true, Logger: tx.Logger.LogMode(logger.Silent)})
	for _, e := range entries {
		if e.ID == 0 {
			continue
		}
		x := Classify(e.Action, e.ExtraInfo)
		x.IP = ipFromEntry(e.Action, e.ExtraInfo)
		if x.IP == "" {
			x.IP = ip
		}
		err := db.Exec("INSERT INTO "+ExtTable+" (audit_id, result, ip, category, module, code, risky) VALUES (?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE result = VALUES(result), ip = VALUES(ip), category = VALUES(category), module = VALUES(module), code = VALUES(code), risky = VALUES(risky)",
			e.ID, x.Result, trunc(x.IP, 64), x.Category, trunc(x.Module, 32), trunc(x.Code, 64), x.Risky).Error
		if err != nil {
			extDisabledUntil.Store(time.Now().Add(time.Minute).Unix())
			slog.Warn("写审计扩展信息失败（dbt_audit_ext 未创建？请执行 deploy/sql/090_audit_ext.sql），1 分钟后重试", "err", err)
			return
		}
	}
}
