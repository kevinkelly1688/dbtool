// @author: kevinkelly
// @license: Apache Licence
// @file: tpl_catalog.go
// @time: 2026/10/28

package notify

import "sort"

// 本文件：消息模板能用的“变量”和“消息”目录。
//
//   - 变量是白名单：模板里只能写 {{key}} 这些，写别的保存时报错；
//   - 消息 = 一类通知（新工单提交、执行完成……），键为 “家族.事件”，由 MessageKey 从 Event 算出；
//   - 每条消息声明自己带哪些变量（Vars）。模板用了这条消息里没有的变量：保存时只提示，发出去是空的。

// Var 是一个模板变量。
type Var struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Group   string `json:"group"`
	Example string `json:"example"`
}

// 变量分组（显示顺序）。
const (
	gTicket = "工单"
	gPeople = "人员"
	gTarget = "目标"
	gBody   = "内容"
	gFlow   = "审批"
	gExec   = "执行"
	gPriv   = "权限"
	gGen    = "消息原文"
)

// Vars 是全部变量，按显示顺序。
var Vars = []Var{
	{"ticket_id", "工单号", gTicket, "3058"},
	{"kind", "工单类型", gTicket, "SQL上线申请"},     // i18n-ignore 预览样例值
	{"title", "工单名称", gTicket, "更新支付渠道账户联调"}, // i18n-ignore 预览样例值
	{"created_at", "发起时间", gTicket, "2026-10-13 14:44:35"},
	{"url", "工单地址", gTicket, "https://dba.example.com/sqlworkflow/3058"},
	{"applicant", "提交人", gPeople, "Alice"},
	{"applicant_user", "提交人账号", gPeople, "alice"},
	{"group", "资源组", gPeople, "支付组"},                                  // i18n-ignore 预览样例值
	{"instance", "实例名", gTarget, "(Sit) shop-2002 [SQL-Server]-(华东)"}, // i18n-ignore 预览样例值
	{"db", "数据库", gTarget, "shopdb"},
	{"syntax", "语句类型", gTarget, "DML"},
	{"sql", "SQL 内容", gBody, "UPDATE … SET secret_key = 'demo-key-123' WHERE code = 'DEMO'"},
	{"flow", "审批流程", gFlow, "组长(passed) -> DBA(current)"}, // i18n-ignore 预览样例值
	{"current_step", "当前审批", gFlow, "DBA"},
	{"remark", "备注 / 原因", gFlow, "缺少 WHERE 条件，请补充后重新提交"}, // i18n-ignore 预览样例值
	{"run_at", "计划执行时间", gExec, "2026-10-13 23:30"},
	{"executor", "执行方式", gExec, "goInception"},
	{"finish_at", "结束时间", gExec, "2026-10-13 15:02:11"},
	{"elapsed", "执行耗时", gExec, "2.3 秒"}, // i18n-ignore 预览样例值
	{"affected_rows", "影响行数", gExec, "2"},
	{"total_count", "语句总数（批量工单为实例数）", gExec, "10"},
	{"success_count", "成功个数", gExec, "10"},
	{"fail_count", "失败个数", gExec, "0"},
	{"exec_summary", "执行汇总", gExec, "总数: 10 | 成功: 10 | 失败: 0 | 影响行数: 2"}, // i18n-ignore 预览样例值
	{"exec_log", "执行记录（逐条语句；批量工单没有）", gExec, "ID: 1 | 状态: ✅ Execute Successfully | 影响行数: 0\nID: 2 | 状态: ✅ Execute Successfully | 影响行数: 2"}, // i18n-ignore 预览样例值
	{"error", "错误信息", gExec, "Lock wait timeout exceeded; try restarting transaction"},
	{"db_list", "数据库清单", gPriv, "shop, report"},
	{"table_list", "表清单", gPriv, "orders, users"},
	{"valid_date", "授权截止时间", gPriv, "2026-11-13 00:00:00"},
	{"limit_num", "结果集上限", gPriv, "1000"},
	{"change", "变更内容", gPriv, "到期时间 2026-10-20 → 2026-11-13"}, // i18n-ignore 预览样例值
	{"operator", "操作人", gPriv, "王管理员"},                        // i18n-ignore 预览样例值
	{"event_label", "事件名", gGen, "审批通过"},                      // i18n-ignore 预览样例值
	{"msg_title", "消息标题（系统默认）", gGen, "[SQL上线申请]工单审核通过#3058"}, // i18n-ignore 预览样例值
	{"msg_content", "消息正文（系统默认）", gGen, "发起时间：…\n目标实例：…"},     // i18n-ignore 预览样例值
}

var varByKey = func() map[string]Var {
	m := make(map[string]Var, len(Vars))
	for _, v := range Vars {
		m[v.Key] = v
	}
	return m
}()

// VarByKey 按键取变量定义。
func VarByKey(k string) (Var, bool) { v, ok := varByKey[k]; return v, ok }

// 消息家族。
const (
	FamSQL     = "sql"
	FamPriv    = "priv"
	FamArchive = "archive"
	FamPerf    = "perf"
	FamInspect = "inspect"
)

// MessageDef 是一类消息的定义。
type MessageDef struct {
	Key    string   `json:"key"`    // sql.submitted
	Family string   `json:"family"` // sql / priv / archive / perf / inspect
	Group  string   `json:"group"`  // 页面里的分组
	Name   string   `json:"name"`
	Hint   string   `json:"hint"`
	Tone   string   `json:"tone"` // 系统默认的状态色
	Icon   string   `json:"icon"`
	Vars   []string `json:"vars"` // 这条消息里有值的变量
	Code   string   `json:"code"` // 对应渠道订阅 / @ 规则里的事件名（Specific 优先，否则 Name）
}

// Has 判断这条消息带不带变量 k。
func (d MessageDef) Has(k string) bool {
	for _, v := range d.Vars {
		if v == k {
			return true
		}
	}
	return false
}

var (
	// 工单（SQL 上线 / 数据导出、查询权限）共有的变量。
	ticketVars = []string{"ticket_id", "kind", "title", "created_at", "url", "applicant", "applicant_user", "group", "instance", "db", "flow", "event_label", "msg_title"}
	sqlVars    = append(append([]string(nil), ticketVars...), "syntax", "sql")
	privVars   = append(append([]string(nil), ticketVars...), "db_list", "table_list", "valid_date", "limit_num")
	// 执行报表：执行结束的消息里的汇总与逐条执行记录。
	execVars = []string{"total_count", "success_count", "fail_count", "exec_summary", "exec_log"}
	// 归档 / 性能 / 巡检：只提供系统默认的标题和正文，模板可以改状态色、图标、版式、加链接按钮。
	genericVars = []string{"msg_title", "msg_content", "applicant_user", "event_label"}
)

func with(base []string, extra ...string) []string {
	return append(append([]string(nil), base...), extra...)
}

const (
	grpSQL  = "SQL 上线 / 数据导出"
	grpPriv = "查询权限"
	grpArc  = "数据归档"
	grpPerf = "性能工单"
	grpInsp = "巡检"
)

// Messages 是全部消息，按显示顺序。
var Messages = []MessageDef{
	{Key: "sql.submitted", Family: FamSQL, Group: grpSQL, Name: "新工单提交", Tone: ToneWarn, Icon: "📋", Code: EventSubmitted,
		Hint: "提交人提交工单、进入第一级审批时发送（渠道订阅里同时属于“待处理”和“新工单提交”）", Vars: with(sqlVars, "current_step")},
	{Key: "sql.pending", Family: FamSQL, Group: grpSQL, Name: "流转到下一级审批", Tone: ToneWarn, Icon: "🔔", Code: EventPending,
		Hint: "上一级审批通过后，工单流转到下一级审批人时发送（渠道订阅里属于“待处理”）", Vars: with(sqlVars, "current_step")},
	{Key: "sql.passed", Family: FamSQL, Group: grpSQL, Name: "审批通过", Tone: ToneOK, Icon: "✅", Code: EventPassed,
		Hint: "最后一级审批通过，工单可以执行时发送", Vars: sqlVars},
	{Key: "sql.rejected", Family: FamSQL, Group: grpSQL, Name: "被驳回", Tone: ToneCrit, Icon: "⛔", Code: EventRejected,
		Hint: "审批人驳回工单时发送，带驳回原因", Vars: with(sqlVars, "remark")},
	{Key: "sql.aborted", Family: FamSQL, Group: grpSQL, Name: "提交人终止", Tone: ToneInfo, Icon: "↩️", Code: EventAborted,
		Hint: "提交人主动撤回 / 终止工单时发送", Vars: with(sqlVars, "remark")},
	{Key: "sql.scheduled", Family: FamSQL, Group: grpSQL, Name: "已设置定时执行", Tone: ToneInfo, Icon: "⏰", Code: EventScheduled,
		Hint: "给工单设置了定时执行时发送", Vars: with(sqlVars, "run_at")},
	{Key: "sql.executing", Family: FamSQL, Group: grpSQL, Name: "开始执行", Tone: ToneInfo, Icon: "▶️", Code: EventExecuting,
		Hint: "工单开始执行时发送", Vars: with(sqlVars, "executor")},
	{Key: "sql.finished", Family: FamSQL, Group: grpSQL, Name: "执行完成", Tone: ToneOK, Icon: "✅", Code: EventFinished,
		Hint: "工单执行完成（含手工确认结束）时发送；DDL 完成还会 @ 配置的相关人员", Vars: with(sqlVars, append([]string{"finish_at", "elapsed", "affected_rows"}, execVars...)...)},
	{Key: "sql.exception", Family: FamSQL, Group: grpSQL, Name: "执行异常", Tone: ToneCrit, Icon: "🚨", Code: EventException,
		Hint: "工单执行出错、需要人工处理时发送", Vars: with(sqlVars, append([]string{"finish_at", "elapsed", "affected_rows", "error"}, execVars...)...)},

	{Key: "priv.submitted", Family: FamPriv, Group: grpPriv, Name: "新的权限申请", Tone: ToneWarn, Icon: "🔑", Code: EventSubmitted,
		Hint: "提交查询权限申请、进入第一级审批时发送", Vars: with(privVars, "current_step")},
	{Key: "priv.pending", Family: FamPriv, Group: grpPriv, Name: "流转到下一级审批", Tone: ToneWarn, Icon: "🔔", Code: EventPending,
		Hint: "权限申请流转到下一级审批人时发送", Vars: with(privVars, "current_step")},
	{Key: "priv.passed", Family: FamPriv, Group: grpPriv, Name: "申请通过", Tone: ToneOK, Icon: "✅", Code: EventPassed,
		Hint: "申请审批通过、权限已授予时发送", Vars: privVars},
	{Key: "priv.rejected", Family: FamPriv, Group: grpPriv, Name: "申请被驳回", Tone: ToneCrit, Icon: "⛔", Code: EventRejected,
		Hint: "申请被驳回时发送", Vars: with(privVars, "remark")},
	{Key: "priv.aborted", Family: FamPriv, Group: grpPriv, Name: "提交人终止", Tone: ToneInfo, Icon: "↩️", Code: EventAborted,
		Hint: "提交人撤回权限申请时发送", Vars: with(privVars, "remark")},
	{Key: "priv.changed", Family: FamPriv, Group: grpPriv, Name: "权限被修改 / 回收", Tone: ToneInfo, Icon: "🛠️", Code: EventPassed,
		Hint: "管理员修改或回收已授予的查询权限后，通知受影响的用户",
		Vars: []string{"applicant_user", "instance", "db_list", "table_list", "change", "operator", "created_at", "url", "event_label", "msg_title"}},

	{Key: "archive.submitted", Family: FamArchive, Group: grpArc, Name: "归档任务待审批（新提交）", Tone: ToneWarn, Icon: "🗄️", Code: EventSubmitted,
		Hint: "提交归档任务、进入第一级审批时发送", Vars: genericVars},
	{Key: "archive.pending", Family: FamArchive, Group: grpArc, Name: "归档任务流转到下一级审批", Tone: ToneWarn, Icon: "🔔", Code: EventPending,
		Hint: "归档任务流转到下一级审批人时发送", Vars: genericVars},
	{Key: "archive.passed", Family: FamArchive, Group: grpArc, Name: "归档任务审批通过", Tone: ToneOK, Icon: "✅", Code: EventPassed,
		Hint: "归档任务审批通过、任务已启用时发送", Vars: genericVars},
	{Key: "archive.rejected", Family: FamArchive, Group: grpArc, Name: "归档任务被驳回", Tone: ToneCrit, Icon: "⛔", Code: EventRejected,
		Hint: "归档任务被驳回时发送", Vars: genericVars},
	{Key: "archive.archive_start", Family: FamArchive, Group: grpArc, Name: "归档开始", Tone: ToneInfo, Icon: "▶️", Code: EventExecuting,
		Hint: "归档任务开始一次运行时发送", Vars: genericVars},
	{Key: "archive.archive_done", Family: FamArchive, Group: grpArc, Name: "归档完成", Tone: ToneOK, Icon: "✅", Code: EventFinished,
		Hint: "归档任务一次运行成功结束时发送", Vars: genericVars},
	{Key: "archive.archive_interrupt", Family: FamArchive, Group: grpArc, Name: "归档中断", Tone: ToneWarn, Icon: "⏸️", Code: EventFinished,
		Hint: "运行被执行窗口 / 单次行数上限中断（下次接着跑），或负载超限等待超时", Vars: genericVars},
	{Key: "archive.archive_failed", Family: FamArchive, Group: grpArc, Name: "归档失败", Tone: ToneCrit, Icon: "🚨", Code: EventException,
		Hint: "归档运行出错时发送", Vars: genericVars},
	{Key: "archive.archive_verify_failed", Family: FamArchive, Group: grpArc, Name: "归档备份校验失败", Tone: ToneCrit, Icon: "🚨", Code: EventException,
		Hint: "备份校验不一致、该批未删除源数据时发送", Vars: genericVars},
	{Key: "archive.archive_structure", Family: FamArchive, Group: grpArc, Name: "源表结构变更", Tone: ToneCrit, Icon: "🚨", Code: EventException,
		Hint: "源表结构发生变化、任务已暂停时发送", Vars: genericVars},
	{Key: "archive.archive_paused", Family: FamArchive, Group: grpArc, Name: "任务自动暂停", Tone: ToneCrit, Icon: "⏸️", Code: EventException,
		Hint: "连续失败等原因触发自动暂停时发送", Vars: genericVars},
	{Key: "archive.archive_lost", Family: FamArchive, Group: grpArc, Name: "运行失联", Tone: ToneCrit, Icon: "🚨", Code: EventException,
		Hint: "执行进程失联 / 被强杀，租约过期被回收时发送", Vars: genericVars},

	{Key: "perf.pending", Family: FamPerf, Group: grpPerf, Name: "状态变化 / 待处理", Tone: ToneWarn, Icon: "🐢", Code: EventPending,
		Hint: "新发现、指派、重开、影响扩大、标记修复、验证未通过、延期 / 豁免申请、评论、SLA 即将超时等", Vars: genericVars},
	{Key: "perf.finished", Family: FamPerf, Group: grpPerf, Name: "工单已关闭", Tone: ToneOK, Icon: "✅", Code: EventFinished,
		Hint: "性能工单关闭时发送", Vars: genericVars},
	{Key: "perf.passed", Family: FamPerf, Group: grpPerf, Name: "延期申请通过", Tone: ToneOK, Icon: "✅", Code: EventPassed,
		Hint: "延期申请被 DBA 通过时发送", Vars: genericVars},
	{Key: "perf.rejected", Family: FamPerf, Group: grpPerf, Name: "申请被驳回", Tone: ToneCrit, Icon: "⛔", Code: EventRejected,
		Hint: "延期 / 豁免申请被驳回时发送", Vars: genericVars},
	{Key: "perf.perf_overdue", Family: FamPerf, Group: grpPerf, Name: "SLA 超时升级", Tone: ToneCrit, Icon: "⏳", Code: EventPerfOverdue,
		Hint: "性能工单超过 SLA 未处理、催办 / 升级时发送", Vars: genericVars},

	{Key: "inspect.finished", Family: FamInspect, Group: grpInsp, Name: "巡检完成", Tone: ToneOK, Icon: "🩺", Code: EventFinished,
		Hint: "定时巡检完成且没有发现问题时发送（计划里选了“每次都通知”才会发）", Vars: genericVars},
	{Key: "inspect.inspect_alert", Family: FamInspect, Group: grpInsp, Name: "巡检告警", Tone: ToneCrit, Icon: "🩺", Code: EventInspectAlert,
		Hint: "定时巡检发现问题时发送", Vars: genericVars},
}

var msgByKey = func() map[string]*MessageDef {
	m := make(map[string]*MessageDef, len(Messages))
	for i := range Messages {
		m[Messages[i].Key] = &Messages[i]
	}
	return m
}()

// MessageByKey 按键取消息定义；不存在返回 nil。
func MessageByKey(k string) *MessageDef { return msgByKey[k] }

// Family 返回事件所属的消息家族；未知返回空（沿用系统默认，不套模板）。
func Family(kind string) string {
	switch kind {
	case KindSQLWorkflow:
		return FamSQL
	case KindQueryPrivilege:
		return FamPriv
	case "archive":
		return FamArchive
	case "perf_issue":
		return FamPerf
	case "inspect":
		return FamInspect
	}
	return ""
}

// MessageKey 算出事件对应的消息键（TplKey 优先，否则 家族.（Specific 或 Name））；算不出或不在目录里返回空。
func MessageKey(ev Event) string {
	if ev.TplKey != "" {
		if msgByKey[ev.TplKey] != nil {
			return ev.TplKey
		}
		return ""
	}
	fam := Family(ev.Kind)
	if fam == "" {
		return ""
	}
	code := ev.Specific
	if code == "" {
		code = ev.Name
	}
	if k := fam + "." + code; msgByKey[k] != nil {
		return k
	}
	// 没有细分事件的目录项时退回到 Name（如 archive 的某个 Specific 没在目录里）
	if k := fam + "." + ev.Name; msgByKey[k] != nil {
		return k
	}
	return ""
}

// GroupsOf 按显示顺序返回消息分组名。
func GroupsOf() []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range Messages {
		if !seen[m.Group] {
			seen[m.Group] = true
			out = append(out, m.Group)
		}
	}
	return out
}

// VarsFor 返回一条消息的变量定义（按全局变量表的顺序）。
func VarsFor(def *MessageDef) []Var {
	if def == nil {
		return nil
	}
	var out []Var
	for _, v := range Vars {
		if def.Has(v.Key) {
			out = append(out, v)
		}
	}
	return out
}

// SortedVarKeys 返回全部变量键（测试 / 文档用）。
func SortedVarKeys() []string {
	out := make([]string, 0, len(varByKey))
	for k := range varByKey {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sqlish 判断变量是否按“SQL”处理（在非代码块位置展开时要隐藏字符串常量并截短）。
func sqlish(k string) bool { return k == "sql" }
