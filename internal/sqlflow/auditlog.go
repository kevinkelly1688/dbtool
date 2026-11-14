// @author: kevinkelly
// @license: Apache Licence
// @file: auditlog.go
// @time: 2026/10/28

package sqlflow

import (
	"fmt"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// 审批类动作写系统审计（dbt_audit_log）：提交、审批通过、驳回、撤销、执行、定时、取消定时等。
// 工单自己的日志（dbt_workflow_log）不变；这里是额外一条，供“系统审计”页按类别 / 权限码 / 高危筛选。
// 仓库不支持或写失败只记日志，不影响业务（见 audit.Lifecycle）。动作名 “SQL上线-<动作>” 由 audit.Classify 归类。

func (s *Service) sysAudit(p audit.Actor, act, extra string) {
	audit.Lifecycle(s.repo, p, audit.ActionPrefixSQLFlow+act, extra, s.now())
}

// wfAuditInfo 是审计额外信息的公共部分：工单号、名称、类型。
func wfAuditInfo(w *model.SqlWorkflow) string {
	// 写进审计库的存档文本（语言属于“事发当时”，不随界面语言变化），整段不翻译。
	kind := "SQL上线" // i18n-ignore
	switch {
	case w.IsOfflineExport != 0 && w.IsBatch():
		kind = "批量导出" // i18n-ignore
	case w.IsOfflineExport != 0:
		kind = "数据导出" // i18n-ignore
	case w.IsBatch():
		kind = "批量上线" // i18n-ignore
	}
	name := truncate(w.WorkflowName, 60)
	return fmt.Sprintf("工单id：%d，名称：%s，类型：%s，库：%s", w.ID, name, kind, w.DBName) // i18n-ignore
}

func withRemark(info, remark string) string {
	if remark == "" {
		return info
	}
	return info + "，备注：" + truncate(remark, 200) // i18n-ignore 写进审计库的存档文本
}
