// @author: kevinkelly
// @license: Apache Licence
// @file: lifecycle.go
// @time: 2026/10/28

package audit

import (
	"log/slog"
	"time"

	"dbtool/internal/model"
)

// 审批类动作（SQL 上线、查询权限申请的提交 / 审批 / 驳回 / 撤销 / 执行 / 定时 / 终止等）的系统审计。
//
// 工单自己的日志（dbt_workflow_log）不变；这里额外往 dbt_audit_log 写一条，供“系统审计”页按类别 / 权限码 / 高危筛选。
// 写入是尽力而为：仓库没有实现 LifecycleWriter、写库失败甚至 panic 都只记日志，绝不影响业务。
// 之所以不复用各包已有的“写审计”方法（如 sqlflow.Repo.InsertAuditEntry），是为了让既有的下载 / 回滚语句审计行为与测试保持原样。

// 动作名前缀。动作名形如 “SQL上线-审批通过”，Classify 按前缀 + 后缀归到对应的按钮权限码（见 ext.go）。
const (
	ActionPrefixSQLFlow = "SQL上线-"  // i18n-ignore
	ActionPrefixPriv    = "查询权限申请-" // i18n-ignore
)

// SQL 防火墙管理动作（internal/firewalladmin 写入）：动作名以 ActionPrefixFirewall 开头，审计页归到“系统管理 › SQL 防火墙”；
// ExtraInfo 以 FirewallRiskyMark 开头的标记为高危（规则由观察切到生效、全局切到观察、总开关关闭——改变拦截范围的操作）。
const (
	ActionPrefixFirewall = "SQL 防火墙" // i18n-ignore
	FirewallRiskyMark    = "【高危】"    // i18n-ignore
)

// LifecycleWriter 是仓库可选实现的接口：写一条 dbt_audit_log。
type LifecycleWriter interface {
	WriteLifecycleAudit(e *model.AuditEntry) error
}

// Lifecycle 写一条审批类动作的系统审计。repo 不支持就静默返回。
func Lifecycle(repo any, p Actor, action, extra string, at time.Time) {
	w, ok := repo.(LifecycleWriter)
	if !ok || w == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("写审批审计异常", "action", action, "panic", r)
		}
	}()
	if at.IsZero() {
		at = time.Now()
	}
	e := &model.AuditEntry{UserID: p.ID, UserName: trunc(p.Username, 30), UserDisplay: trunc(p.Display, 50),
		Action: trunc(action, 128), ExtraInfo: trunc(extra, 900), ActionTime: at}
	if err := w.WriteLifecycleAudit(e); err != nil {
		slog.Error("写审批审计失败", "action", action, "user", p.Username, "err", err)
	}
}
