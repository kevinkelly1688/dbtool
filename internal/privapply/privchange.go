// @author: kevinkelly
// @license: Apache Licence
// @file: privchange.go
// @time: 2026/10/28

package privapply

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/notify"
)

// 已授予权限被直接授予 / 修改 / 回收之后的善后：写带目标用户与新旧值的审计、通知受影响用户、回写来源申请单的日志。
// 都是尽力而为：失败只记日志，不影响已经完成的权限变更。

// LogPrivChange 是写到来源申请单 dbt_workflow_log 里的“权限变更”记录类型：不属于审批动作（0~3），审批时间线不会把它当成审批节点。
const LogPrivChange = 9

// 权限变更的动作名（系统审计）。
const (
	auditActModify = "修改查询权限" // i18n-ignore 审计动作名入库
	auditActRevoke = "回收查询权限" // i18n-ignore 审计动作名入库
)

type privChange struct {
	action  string // 系统审计动作名
	verb    string // 授予 / 修改 / 回收
	evName  string // 通知事件名
	object  string // 非空时替代按权限算出的对象描述（一次授予多个库表时）
	changes string // 变更内容，如 “有效期 2026-10-10→2026-10-19，行数上限 500→77”；授予时是授予的内容
}

func privObject(p *model.QueryPrivilege) string {
	if p.PrivType == 2 {
		return p.DBName + "." + p.TbName
	}
	return p.DBName + "（库权限）" // i18n-ignore 审计 / 通知原文入库
}

func (s *Service) instanceName(id uint) string {
	if list, err := s.repo.ListInstances(); err == nil {
		for i := range list {
			if list[i].ID == id {
				return list[i].InstanceName
			}
		}
	}
	return fmt.Sprintf("#%d", id)
}

// noticeGroup 选通知用的资源组：目标用户所在、且关联该实例的资源组；取不到返回 0（只走全局渠道，并 @ 目标用户）。
func (s *Service) noticeGroup(userID, instID uint) uint {
	if userID == 0 {
		return 0
	}
	groups, err := s.repo.UserResourceGroups(userID, false)
	if err != nil || len(groups) == 0 {
		return 0
	}
	if gi := groupInstances(s.repo); gi != nil {
		for _, g := range groups {
			if ids, err := gi.GroupInstanceIDs(g.GroupID); err == nil && ids[instID] {
				return g.GroupID
			}
		}
	}
	return groups[0].GroupID
}

// afterPrivChange 在权限已经写库之后调用。
func (s *Service) afterPrivChange(p audit.Actor, priv *model.QueryPrivilege, ch privChange) {
	now := s.now()
	inst := s.instanceName(priv.InstanceID)
	obj := privObject(priv)
	if ch.object != "" {
		obj = ch.object
	}
	e := ext(s.repo)

	// 1. 系统审计：目标用户、实例、对象、新旧值、权限编号。
	extra := fmt.Sprintf("用户:%s 实例:%s 对象:%s 权限#%d %s", priv.UserName, inst, obj, priv.PrivilegeID, ch.changes) // i18n-ignore 系统审计原文入库
	var src PrivExt
	var hasSrc bool
	if e != nil && e.PrivExtReady() {
		if m, err := e.PrivExts([]uint{priv.PrivilegeID}); err == nil {
			if x, ok := m[priv.PrivilegeID]; ok && x.Source == SourceApply && x.ApplyID != 0 {
				src, hasSrc = x, true
				extra += fmt.Sprintf(" 来源申请单#%d", x.ApplyID) // i18n-ignore 系统审计原文入库
			}
		}
	}
	if ch.action != "" && e != nil {
		if err := e.WriteAuditLog(&model.AuditEntry{UserID: p.ID, UserName: truncRunes(p.Username, 30), UserDisplay: truncRunes(p.Display, 50),
			Action: ch.action, ExtraInfo: truncRunes(extra, 900), ActionTime: now}); err != nil {
			slog.Error("写查询权限变更审计失败", "action", ch.action, "operator", p.Username, "user", priv.UserName, "err", err)
		}
	}

	// 2. 回写来源申请单的日志（直接授予的权限没有来源申请单）。
	if hasSrc {
		s.logToApply(p, src.ApplyID, fmt.Sprintf("%s查询权限#%d：%s，%s", ch.verb, priv.PrivilegeID, obj, ch.changes), now) // i18n-ignore 申请单日志原文入库
	}

	// 3. 通知受影响用户（机器人）。
	var uid uint
	if e != nil {
		if u, err := e.UserByName(priv.UserName); err == nil && u != nil {
			uid = u.ID
		}
	}
	ev := notify.Event{
		Kind: notify.KindQueryPrivilege, Name: ch.evName, GroupID: s.noticeGroup(uid, priv.InstanceID),
		Applicant: priv.UserName, ExtraUsers: []string{priv.UserName},
		TplKey: "priv.changed", // 与申请审批的 passed / aborted 区分开，用“权限被修改 / 回收”这条消息的模板
		Vars: map[string]string{
			"applicant_user": priv.UserName, "instance": inst, "db_list": priv.DBName, "table_list": priv.TbName,
			"change": ch.changes, "operator": displayOr(p.Display, p.Username), "created_at": now.Format("2006-01-02 15:04:05"),
			"url": strings.TrimRight(s.BaseURL, "/") + "/query/privileges",
		},
		Title: fmt.Sprintf("[查询权限]已%s：%s %s", ch.verb, inst, obj), // i18n-ignore 通知正文（语言属于接收方，不翻译）
		Content: fmt.Sprintf("用户：%s\n目标实例：%s\n对象：%s\n变更：%s\n操作人：%s\n时间：%s\n查询权限页：%s", // i18n-ignore 通知正文
			priv.UserName, inst, obj, ch.changes, displayOr(p.Display, p.Username), now.Format("2006-01-02 15:04:05"),
			strings.TrimRight(s.BaseURL, "/")+"/query/privileges"),
	}
	n := s.notifier
	s.async(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		n.Notify(ctx, ev)
	})
}

// logToApply 往来源申请单的 dbt_workflow_log 追加一条权限变更记录。
func (s *Service) logToApply(p audit.Actor, applyID uint, info string, at time.Time) {
	au, err := s.repo.AuditByWorkflow(audit.TypeQueryPrivilege, applyID, false)
	if err != nil || au == nil {
		if err != nil {
			slog.Warn("回写申请单日志：读取审批记录失败", "apply_id", applyID, "err", err)
		}
		return
	}
	if err := s.repo.InsertLog(&model.WorkflowLog{
		AuditID: au.AuditID, OperationType: LogPrivChange, OperationTypeDesc: "权限变更",
		OperationInfo: truncRunes(info, 1000), Operator: p.Username, OperatorDisplay: p.Display, OperationTime: at,
	}); err != nil {
		slog.Warn("回写申请单日志失败", "apply_id", applyID, "err", err)
	}
}
