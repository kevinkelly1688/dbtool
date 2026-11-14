// @author: kevinkelly
// @license: Apache Licence
// @file: tpl_default.go
// @time: 2026/10/28

package notify

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件：消息模板页用的“系统默认模板”（定制时的起点）与模拟事件。
//
// 运行时没有模板就发事件自带的 Title / Content（原样，不经过这里）；DefaultTemplate 是把同样的信息排成一份模板，
// 管理员点“定制”时从它开始改，所以字段、顺序与现有文案一致。SQL 代码块默认用“隐藏字符串常量”。

// MaxSQLVar 是事件里带的 SQL 全文的字数上限，产生方按它截断。
const MaxSQLVar = maxSQLVar

func fRow(l, v string) Row { return Row{L: l, V: v} }

// DefaultTemplate 返回一条消息的系统默认模板（只读副本，Scope 为空）。
func DefaultTemplate(def *MessageDef) Template {
	t := Template{EventKey: def.Key, Scope: "", Enabled: true, Tone: def.Tone, Icon: def.Icon}
	sqlPrev := Block{T: BlockCode, L: "工单详情预览", V: "{{sql}}", Mode: ModeMask, N: DefaultCodeN} // i18n-ignore 通知正文 / 模板内容
	// 执行记录：逐条语句的结果（批量工单没有这一块，只在“执行汇总”里写成功 / 失败的实例个数）。和 SQL 一样放在代码块里。
	execLog := Block{T: BlockCode, L: "执行记录", V: "{{exec_log}}", Mode: ModeHead, N: 1000}                                                                             // i18n-ignore 通知正文 / 模板内容
	head := []Row{fRow("发起时间", "{{created_at}}"), fRow("发起人", "{{applicant}}"), fRow("组", "{{group}}"), fRow("目标实例", "{{instance}}"), fRow("数据库", "{{db}}")}          // i18n-ignore 通知正文 / 模板内容
	privHead := []Row{fRow("发起时间", "{{created_at}}"), fRow("发起人", "{{applicant}}"), fRow("组", "{{group}}"), fRow("目标实例", "{{instance}}"), fRow("数据库", "{{db_list}}")} // i18n-ignore 通知正文 / 模板内容
	privTail := []Row{fRow("表清单", "{{table_list}}"), fRow("授权截止时间", "{{valid_date}}"), fRow("结果集", "{{limit_num}}")}                                                  // i18n-ignore 通知正文 / 模板内容
	with := func(base []Row, more ...Row) []Row { return append(append([]Row(nil), base...), more...) }
	fields := func(rows []Row, extra ...Block) []Block {
		return append([]Block{{T: BlockFields, Rows: rows}}, extra...)
	}
	remind := Block{T: BlockText, V: "提醒：此工单被审核不通过，请按照驳回原因进行修改！"} // i18n-ignore 通知正文 / 模板内容

	switch def.Key {
	case "sql.submitted", "sql.pending":
		t.Title = "[{{kind}}]新的工单申请#{{ticket_id}}"                                                                                                             // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields(with(head, fRow("审批流程", "{{flow}}"), fRow("当前审批", "{{current_step}}"), fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}")), sqlPrev) // i18n-ignore 通知正文 / 模板内容
	case "sql.passed":
		t.Title = "[{{kind}}]工单审核通过#{{ticket_id}}"                                                                           // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields(with(head, fRow("审批流程", "{{flow}}"), fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}")), sqlPrev) // i18n-ignore 通知正文 / 模板内容
	case "sql.rejected":
		t.Title = "[{{kind}}]工单被驳回#{{ticket_id}}"                                                                                                                                                             // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields([]Row{fRow("发起时间", "{{created_at}}"), fRow("目标实例", "{{instance}}"), fRow("数据库", "{{db}}"), fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}"), fRow("驳回原因", "{{remark}}")}, remind) // i18n-ignore 通知正文 / 模板内容
	case "sql.aborted":
		t.Title = "[{{kind}}]提交人主动终止工单#{{ticket_id}}"                                                                 // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields(with(head, fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}"), fRow("终止原因", "{{remark}}"))) // i18n-ignore 通知正文 / 模板内容
	case "sql.scheduled":
		t.Title = "[{{kind}}]工单已设置定时执行#{{ticket_id}}"                                                                                                      // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields(with(head, fRow("审批流程", "{{flow}}"), fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}"), fRow("计划执行时间", "{{run_at}}")), sqlPrev) // i18n-ignore 通知正文 / 模板内容
	case "sql.executing":
		t.Title = "[{{kind}}]工单开始执行#{{ticket_id}}"                                                                                                         // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields(with(head, fRow("审批流程", "{{flow}}"), fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}"), fRow("执行方式", "{{executor}}")), sqlPrev) // i18n-ignore 通知正文 / 模板内容
	case "sql.finished":
		t.Title = "[{{kind}}]工单已正常结束#{{ticket_id}}"                                                                // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields(with(head, fRow("审批流程", "{{flow}}"), fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}"), // i18n-ignore 通知正文 / 模板内容
			fRow("结束时间", "{{finish_at}}"), fRow("耗时", "{{elapsed}}"), fRow("执行汇总", "{{exec_summary}}")), execLog, sqlPrev) // i18n-ignore 通知正文 / 模板内容
	case "sql.exception":
		t.Title = "[{{kind}}]工单执行异常#{{ticket_id}}"                                                                 // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields(with(head, fRow("审批流程", "{{flow}}"), fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}"), // i18n-ignore 通知正文 / 模板内容
			fRow("结束时间", "{{finish_at}}"), fRow("执行汇总", "{{exec_summary}}"), fRow("错误信息", "{{error}}")), execLog, sqlPrev) // i18n-ignore 通知正文 / 模板内容

	case "priv.submitted", "priv.pending":
		t.Title = "[查询权限申请]新的工单申请#{{ticket_id}}"                                                                                                          // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields(with(privHead, fRow("审批流程", "{{flow}}"), fRow("当前审批", "{{current_step}}"), fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}"))) // i18n-ignore 通知正文 / 模板内容
		t.Blocks[0].Rows = append(t.Blocks[0].Rows, privTail...)
	case "priv.passed":
		t.Title = "[查询权限申请]工单审核通过#{{ticket_id}}"                                                                        // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields(with(privHead, fRow("审批流程", "{{flow}}"), fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}"))) // i18n-ignore 通知正文 / 模板内容
		t.Blocks[0].Rows = append(t.Blocks[0].Rows, privTail...)
	case "priv.rejected":
		t.Title = "[查询权限申请]工单被驳回#{{ticket_id}}"                                                                                                                                                                    // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields([]Row{fRow("发起时间", "{{created_at}}"), fRow("目标实例", "{{instance}}"), fRow("数据库", "{{db_list}}"), fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}"), fRow("驳回原因", "{{remark}}")}, remind) // i18n-ignore 通知正文 / 模板内容
	case "priv.aborted":
		t.Title = "[查询权限申请]提交人主动终止工单#{{ticket_id}}"                                                                       // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields(with(privHead, fRow("工单名称", "{{title}}"), fRow("工单地址", "{{url}}"), fRow("终止原因", "{{remark}}"))) // i18n-ignore 通知正文 / 模板内容
	case "priv.changed":
		t.Title = "[查询权限]已变更：{{instance}}"                                                                                                               // i18n-ignore 通知正文 / 模板内容
		t.Blocks = fields([]Row{fRow("用户", "{{applicant_user}}"), fRow("目标实例", "{{instance}}"), fRow("数据库", "{{db_list}}"), fRow("表", "{{table_list}}"), // i18n-ignore 通知正文 / 模板内容
			fRow("变更", "{{change}}"), fRow("操作人", "{{operator}}"), fRow("时间", "{{created_at}}"), fRow("查询权限页", "{{url}}")}) // i18n-ignore 通知正文 / 模板内容

	default: // 归档 / 性能 / 巡检：只有系统默认的标题和正文
		t.Title = "{{msg_title}}"
		t.Blocks = []Block{{T: BlockText, V: "{{msg_content}}"}}
	}
	t.Normalize()
	return t
}

// sampleSQL 是模拟事件里的 SQL：带几个字符串常量，方便在预览里直接看到“隐藏字符串常量”的效果（常量都是虚构值）。
const sampleSQL = "UPDATE …\nSET merchant_code = 'demo-merchant' ,\n    secret_key = 'demo-secret-0123456789' ,\n    state = 1\nWHERE channel_code = 'DEMO_PAY' ;\n" +
	"UPDATE …\nSET merchant_code = 'demo-merchant' ,\n    secret_key = 'demo-secret-0123456789'\nWHERE channel_code = 'DEMO_PAY' ;"

// SampleMessageEvent 返回消息 key 对应的模拟事件（样例数据，不对应真实工单）：变量取各变量的样例值，SQL 带几个字符串常量。
// applicant 是当前操作者，会作为提交人；reviewerGroup 非 0 时作为当前审批级的权限组（让“审批人”规则有对象可解析）。
func SampleMessageEvent(key, applicant string, groupID, reviewerGroup uint) (Event, error) {
	def := MessageByKey(key)
	if def == nil {
		return Event{}, fmt.Errorf("未知的消息 %s", strconv.Quote(key))
	}
	if applicant == "" {
		applicant = "demo_user"
	}
	vars := map[string]string{}
	for _, k := range def.Vars {
		if v, ok := varByKey[k]; ok {
			vars[k] = v.Example
		}
	}
	vars["applicant"], vars["applicant_user"] = "（样例）"+applicant, applicant // i18n-ignore 通知正文 / 模板内容
	if def.Has("sql") {
		vars["sql"] = sampleSQL
	}
	vars["event_label"] = def.Name

	ev := Event{Name: def.Code, GroupID: groupID, Applicant: applicant, Test: true, TplKey: key, Vars: vars}
	switch def.Family {
	case FamSQL:
		ev.Kind = KindSQLWorkflow
		ev.Title = "[SQL上线申请]" + def.Name + "#1042"                                                                                                                                        // i18n-ignore 通知正文 / 模板内容
		ev.Content = "发起时间：2026-10-07 10:08:21\n发起人：（样例）" + applicant + "\n目标实例：mysql-order-prod-m\n数据库：order_db\n工单名称：订单表增加字段 refund_flag\n工单地址：https://dba.example.com/sqlworkflow/1042" // i18n-ignore 通知正文 / 模板内容
	case FamPriv:
		ev.Kind = KindQueryPrivilege
		ev.Title = "[查询权限申请]" + def.Name + "#1042"                                                            // i18n-ignore 通知正文 / 模板内容
		ev.Content = "发起时间：2026-10-07 10:08:21\n发起人：（样例）" + applicant + "\n目标实例：mysql-order-prod-m\n数据库：shop" // i18n-ignore 通知正文 / 模板内容
	case FamArchive:
		ev.Kind = "archive"
		ev.Title = "[数据归档]订单表 90 天前数据归档：" + def.Name                                                                                   // i18n-ignore 通知正文 / 模板内容
		ev.Content = "任务：订单表 90 天前数据归档\n目标实例：mysql-order-prod-m\n运行：#12（定时）\n读取 1,284,300 行 · 备份 1,284,300 行 · 删除 1,284,300 行 · 129 批" // i18n-ignore 通知正文 / 模板内容
		if sfx := strings.TrimPrefix(key, "archive."); strings.HasPrefix(sfx, "archive_") {
			ev.Specific = sfx
		}
	case FamPerf:
		ev.Kind = "perf_issue"
		ev.Title = "【SQL性能工单 #2031 P1】" + def.Name                                                                // i18n-ignore 通知正文 / 模板内容
		ev.Content = "实例：mysql-pay-prod-m  库：pay_db\n解决截止：2026-10-06 18:00\n详情：https://dba.example.com/perf/2031" // i18n-ignore 通知正文 / 模板内容
	case FamInspect:
		ev.Kind = "inspect"
		ev.Title = "[dbtool] 定时巡检「生产库日巡检」" + def.Name                                               // i18n-ignore 通知正文 / 模板内容
		ev.Content = "实例 12 个：成功 11 / 失败 1\n告警合计 3 · 关注合计 7\n健康评分低于 70 的实例：mysql-user-prod-m（62 分）" // i18n-ignore 通知正文 / 模板内容
	}
	// 事件名与细分事件：与产生方一致，@ 规则 / 订阅的事件过滤才能按真实逻辑生效
	switch def.Code {
	case EventSubmitted:
		ev.Name, ev.Specific = EventPending, EventSubmitted
	case EventPerfOverdue, EventInspectAlert:
		ev.Name, ev.Specific = EventException, def.Code
	case "":
		ev.Name = EventPending
	}
	if ev.Name == EventPending {
		ev.ReviewerAuthGroupID = reviewerGroup
	}
	return ev, nil
}

// TemplateHist 是消息模板的一条修改历史。
type TemplateHist struct {
	ID       uint      `json:"id"`
	Action   string    `json:"action"` // save / delete
	Note     string    `json:"note"`
	Operator string    `json:"operator"`
	At       string    `json:"at"`
	Snapshot *Template `json:"snapshot,omitempty"`
}
