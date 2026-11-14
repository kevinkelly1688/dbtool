// @author: kevinkelly
// @license: Apache Licence
// @file: auditlog.go
// @time: 2026/10/28

package privapply

import (
	"fmt"

	"dbtool/internal/audit"
	"dbtool/internal/model"
)

// 查询权限申请的审批类动作写系统审计（dbt_audit_log）：提交、审批通过、驳回、撤销。
// 申请单自己的日志（dbt_workflow_log）不变；这里是额外一条，供“系统审计”页按类别 / 权限码筛选。
// 仓库不支持或写失败只记日志，不影响业务（见 audit.Lifecycle）。动作名 “查询权限申请-<动作>” 由 audit.Classify 归类。

func (s *Service) sysAudit(p audit.Actor, act, extra string) {
	audit.Lifecycle(s.repo, p, audit.ActionPrefixPriv+act, extra, s.now())
}

func tableAuditText(tables string) string {
	if tables == "" {
		return ""
	}
	return "." + tables
}

// applyAuditInfo 是审计额外信息的公共部分：申请单号、标题、申请人、实例、有效期。
func applyAuditInfo(a *model.QueryPrivilegesApply, remark string) string {
	inst := fmt.Sprintf("%d", a.InstanceID)
	if a.Instance != nil {
		inst = a.Instance.InstanceName
	}
	info := fmt.Sprintf("申请单id：%d，标题：%s，申请人：%s，实例：%s，有效期至：%s", a.ApplyID, truncRunes(a.Title, 60), a.UserName, inst, a.ValidDate.Format("2006-01-02")) // i18n-ignore 系统审计原文入库
	if remark != "" {
		info += "，备注：" + truncRunes(remark, 200) // i18n-ignore 系统审计原文入库
	}
	return info
}
