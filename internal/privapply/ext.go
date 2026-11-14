// @author: kevinkelly
// @license: Apache Licence
// @file: ext.go
// @time: 2026/10/28

package privapply

import (
	"fmt"
	"strings"
	"time"

	"dbtool/internal/model"
)

// 本文件定义“旁路扩展”：不给 dbt_query_privileges_apply / dbt_query_privileges 加列，
// 申请理由、授予来源等信息存在 dbt_query_apply_ext / dbt_query_priv_ext 两张旁路表里
// （deploy/sql/040_query_apply_ext.sql、041_query_priv_ext.sql）。
// 这些能力通过可选接口 ExtRepo 提供：仓库不实现、或表还没创建时，功能优雅降级（不要求填理由、授予来源显示“—”），页面不会 500。

// 授予来源。
const (
	SourceApply = "apply" // 申请单审批通过后自动授予
	SourceGrant = "grant" // 管理员直接授予
)

const (
	minReasonRunes = 5   // 申请理由最少字数
	maxReasonRunes = 500 // 申请理由 / 授予原因最多字数（与旁路表字段一致）
)

// ApplyExt 是申请单的扩展信息。
type ApplyExt struct {
	ApplyID   uint
	Reason    string
	CreatedAt time.Time
}

// PrivExt 是已授予权限的扩展信息（授予来源）。
type PrivExt struct {
	PrivilegeID uint
	Source      string // SourceApply / SourceGrant
	ApplyID     uint   // 来源为申请单时的单号
	GrantedBy   string // 直接授予时的操作人账号
	Reason      string // 直接授予时的原因
	CreatedAt   time.Time
}

// PrivStats 是“查询权限”页顶部 KPI 与筛选项所需的聚合（范围 = 当前用户可见的全部未回收权限，不受筛选条件影响）。
type PrivStats struct {
	Valid    int // 有效（含即将到期）
	Soon     int // 7 天内到期
	Expired  int // 已过期（未回收）
	Users    int // 涉及的用户数
	Insts    int // 涉及的实例数
	UserOpts []UserOpt
	InstOpts []InstOpt
	DBOpts   []string // 当前实例筛选下涉及的库
}

type UserOpt struct{ UserName, Display string }
type InstOpt struct {
	ID   uint
	Name string
}

// ExtRepo 是权限申请的可选扩展数据访问。store.Store 与 memrepo 都实现它；没实现的仓库（例如其他模块的假仓库）照常工作。
type ExtRepo interface {
	// ApplyExtReady / PrivExtReady 表示对应旁路表已创建（deploy/sql/040、041）。
	ApplyExtReady() bool
	PrivExtReady() bool

	SaveApplyExt(e ApplyExt) error
	ApplyExts(ids []uint) (map[uint]ApplyExt, error)
	SavePrivExts(es []PrivExt) error
	PrivExts(ids []uint) (map[uint]PrivExt, error)
	PrivExtsByApply(applyID uint) ([]PrivExt, error)

	// PrivStats 统计 f 范围内（只看 Scope / UserName / InstanceIDs）的未回收权限；KPI 与用户 / 实例选项忽略其他筛选条件，
	// 库选项（DBOpts）在 f.FilterInst 不为 0 时只列该实例涉及的库。
	PrivStats(f PrivFilter, today time.Time) (PrivStats, error)

	// UserByName 按账号取用户（不存在返回 nil, nil）。
	UserByName(username string) (*model.User, error)
	// GrantableUsers 返回直接授予时可选的用户：全部启用用户（all=true）或与给定资源组有交集的启用用户。
	GrantableUsers(groupIDs []uint, all bool) ([]model.User, error)
	// UserDept 取用户部门（来自系统管理的用户扩展表；没有则返回空串）。
	UserDept(username string) string
	// WriteAuditLog 写系统审计。
	WriteAuditLog(e *model.AuditEntry) error
}

// ext 返回仓库的扩展接口；仓库不支持时返回 nil。
func ext(r Repo) ExtRepo {
	e, _ := r.(ExtRepo)
	return e
}

// reasonEnabled 表示可以保存申请理由（旁路表就绪）；此时提交申请必须填写理由。
func reasonEnabled(r Repo) bool {
	e := ext(r)
	return e != nil && e.ApplyExtReady()
}

// ReasonRequired 告诉前端是否要求填写申请理由。
func (s *Service) ReasonRequired() bool { return reasonEnabled(s.repo) }

// checkReason 校验申请理由 / 授予原因；tooShort 是长度不足时的完整提示（整句，便于翻译）。
func checkReason(reason string, tooShort string) (string, error) {
	reason = strings.TrimSpace(reason)
	n := len([]rune(reason))
	if n < minReasonRunes {
		return "", bad(tooShort)
	}
	if n > maxReasonRunes {
		return "", bad("文字过长（最多 500 个字）")
	}
	return reason, nil
}

// ---- 状态推导 ----

// StatusExpired 是“已过期”的显示状态（数据库里没有这个值）：审批通过的申请，授予的权限已过有效期。
const StatusExpired = 4

// EffStatus 返回申请单的显示状态：审批通过且有效期早于今天 → 已过期，其余同数据库状态。
func EffStatus(status int, valid, now time.Time) int {
	if status == 1 && valid.Before(today(now)) {
		return StatusExpired
	}
	return status
}

// EffStatusLabel 是显示状态的文字。
func EffStatusLabel(eff int) string {
	switch eff {
	case 0:
		return "待审核"
	case 1:
		return "审核通过"
	case 2:
		return "审核不通过"
	case 3:
		return "审核取消"
	case StatusExpired:
		return "已过期"
	}
	return ""
}

// 权限的有效状态。
const (
	PrivValid   = "valid"
	PrivSoon    = "soon" // 7 天内到期
	PrivExpired = "expired"
)

// soonDays 是“即将到期”的天数阈值。
const soonDays = 7

// DaysLeft 是有效期距今天的天数（今天到期为 0，已过期为负数）。
func DaysLeft(valid, now time.Time) int {
	a := time.Date(valid.Year(), valid.Month(), valid.Day(), 0, 0, 0, 0, time.UTC)
	t := today(now)
	b := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return int(a.Sub(b) / (24 * time.Hour))
}

// PrivState 按有效期推导权限状态。
func PrivState(valid, now time.Time) string {
	d := DaysLeft(valid, now)
	switch {
	case d < 0:
		return PrivExpired
	case d <= soonDays:
		return PrivSoon
	}
	return PrivValid
}

// StateLabel 是权限状态的文字。
func StateLabel(state string) string {
	switch state {
	case PrivSoon:
		return "即将到期"
	case PrivExpired:
		return "已过期"
	}
	return "有效"
}

// DaysText 是剩余天数的文字描述。
func DaysText(d int) string {
	switch {
	case d < 0:
		return fmt.Sprintf("已过期 %d 天", -d)
	case d == 0:
		return "今天到期"
	}
	return fmt.Sprintf("剩余 %d 天", d)
}

// GroupInstanceRepo 是可选接口：查询某个资源组关联的实例。仓库实现它时，提交申请会校验“实例属于所选资源组”
// （审批流跟着资源组走，不允许拿别的组的宽松审批流去申请这个实例的权限）；不实现时跳过该校验。
type GroupInstanceRepo interface {
	GroupInstanceIDs(groupID uint) (map[uint]bool, error)
}

func groupInstances(r Repo) GroupInstanceRepo {
	g, _ := r.(GroupInstanceRepo)
	return g
}
