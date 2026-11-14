// @author: kevinkelly
// @license: Apache Licence
// @file: grant.go
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
	"dbtool/internal/query"
)

// GrantReq 是管理员“直接授予”查询权限的请求：不走审批，必须填写原因，写审计。
type GrantReq struct {
	UserName   string
	InstanceID uint
	PrivType   int // 1 库权限；2 表权限
	DBName     string
	DBList     []string // 库权限可一次授予多个库
	TableList  []string
	ValidDate  string // YYYY-MM-DD
	LimitNum   int
	Reason     string
}

// GrantUser 是直接授予时可选的用户。
type GrantUser struct {
	UserName string
	Display  string
}

// GrantResult 是授予的结果。
type GrantResult struct {
	PrivilegeIDs []uint
}

// CanGrant 判断用户能否直接授予（需要 qpriv.manage；超级用户恒可）。
func (s *Service) CanGrant(p audit.Actor) bool {
	ok, err := s.has(p, permMgtPriv)
	return err == nil && ok
}

// GrantUsers 返回可直接授予的用户：超级用户为全部启用用户，其他管理者为与自己有共同资源组的启用用户。
func (s *Service) GrantUsers(p audit.Actor) ([]GrantUser, error) {
	if !s.CanGrant(p) {
		return nil, forbid("你没有管理查询权限的权限（缺少 qpriv.manage）")
	}
	e := ext(s.repo)
	if e == nil {
		return []GrantUser{}, nil
	}
	var gids []uint
	if !p.IsSuperuser {
		groups, err := s.repo.UserResourceGroups(p.ID, false)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			gids = append(gids, g.GroupID)
		}
	}
	users, err := e.GrantableUsers(gids, p.IsSuperuser)
	if err != nil {
		return nil, err
	}
	out := make([]GrantUser, 0, len(users))
	for _, u := range users {
		out = append(out, GrantUser{UserName: u.Username, Display: u.Display})
	}
	return out, nil
}

// GrantInstances 返回管理者可以授予权限的实例：自己资源组关联的、支持在线查询的实例（超级用户为全部支持查询的实例）。
func (s *Service) GrantInstances(p audit.Actor) ([]model.Instance, error) {
	if !s.CanGrant(p) {
		return nil, forbid("你没有管理查询权限的权限（缺少 qpriv.manage）")
	}
	list, err := s.repo.ListInstances()
	if err != nil {
		return nil, err
	}
	var ids map[uint]bool
	if !p.IsSuperuser {
		if ids, err = s.repo.UserInstanceIDs(p.ID); err != nil {
			return nil, err
		}
	}
	var out []model.Instance
	for i := range list {
		if (p.IsSuperuser || ids[list[i].ID]) && query.SupportsQuery(list[i].DBType) {
			out = append(out, list[i])
		}
	}
	return out, nil
}

// GrantPrivilege 管理员直接授予查询权限：复用申请通过时的写库逻辑（dbt_query_privileges），另在旁路表记录来源，并写系统审计。
//   - 需要 qpriv.manage；非超级用户只能在自己资源组关联的实例上授予；
//   - 目标用户必须是启用用户，且能访问该实例（所在资源组关联了它，或拥有 query.admin_run）；
//   - 原因必填（至少 5 个字）、有效期不能早于今天、行数上限 1 ~ 100 万；用户已拥有同样的权限时拒绝重复授予。
func (s *Service) GrantPrivilege(ctx context.Context, p audit.Actor, req GrantReq) (*GrantResult, error) {
	if !s.CanGrant(p) {
		return nil, forbid("你没有管理查询权限的权限（缺少 qpriv.manage）")
	}
	e := ext(s.repo)
	if e == nil {
		return nil, query.NewError(501, "unsupported", "当前存储不支持直接授予")
	}
	now := s.now()
	if req.PrivType != 1 && req.PrivType != 2 {
		return nil, bad("权限类型错误")
	}
	if req.InstanceID == 0 {
		return nil, bad("请选择实例")
	}
	if strings.TrimSpace(req.UserName) == "" {
		return nil, bad("请选择要授予的用户")
	}
	if req.LimitNum <= 0 || req.LimitNum > maxLimitNum {
		return nil, bad(fmt.Sprintf("返回行数上限需在 1 到 %d 之间", maxLimitNum))
	}
	valid, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(req.ValidDate), now.Location())
	if err != nil {
		return nil, bad("有效期格式应为 YYYY-MM-DD")
	}
	if valid.Before(today(now)) {
		return nil, bad("有效期不能早于今天")
	}
	reason, err := checkReason(req.Reason, "请填写授予原因（至少 5 个字）")
	if err != nil {
		return nil, err
	}

	insts, err := s.GrantInstances(p)
	if err != nil {
		return nil, err
	}
	var inst *model.Instance
	for i := range insts {
		if insts[i].ID == req.InstanceID {
			inst = &insts[i]
		}
	}
	if inst == nil {
		return nil, forbid("你所在组未关联该实例，或该实例不支持在线查询")
	}
	if req.PrivType == 2 && inst.DBType == "redis" {
		return nil, bad("Redis 没有表，只能授予库（db 编号）级的查询权限")
	}

	target, err := e.UserByName(strings.TrimSpace(req.UserName))
	if err != nil {
		return nil, err
	}
	if target == nil || !target.IsActive {
		return nil, bad("用户不存在或已停用")
	}
	if !target.IsSuperuser { // 超级用户本来就能查询全部，无需授予
		ta := audit.Actor{ID: target.ID, Username: target.Username, Display: target.Display}
		all, err := s.has(ta, permQueryAllInst)
		if err != nil {
			return nil, err
		}
		if !all {
			ids, err := s.repo.UserInstanceIDs(target.ID)
			if err != nil {
				return nil, err
			}
			if !ids[inst.ID] {
				return nil, bad(fmt.Sprintf("用户 %s 无权访问实例 %s（其资源组未关联该实例），授予后也无法查询", target.Username, inst.InstanceName))
			}
		}
	} else {
		return nil, bad("超级用户可查询全部库表，无需授予")
	}

	privs, err := s.repo.ValidQueryPrivileges(target.Username, inst.ID)
	if err != nil {
		return nil, err
	}
	var rows []model.QueryPrivilege
	mk := func(db, table string) model.QueryPrivilege {
		return model.QueryPrivilege{UserName: target.Username, UserDisplay: target.Display, InstanceID: inst.ID,
			DBName: db, TbName: table, ValidDate: valid, LimitNum: req.LimitNum, PrivType: req.PrivType}
	}
	var what string
	switch req.PrivType {
	case 1:
		dbs, err := cleanNames(req.DBList)
		if err != nil {
			return nil, err
		}
		if len(dbs) == 0 {
			if d := strings.TrimSpace(req.DBName); d != "" {
				dbs = []string{d}
			}
		}
		if len(dbs) == 0 {
			return nil, bad("请选择要授予的数据库")
		}
		for _, db := range dbs {
			if query.HasDBPriv(privs, db) {
				return nil, bad(fmt.Sprintf("用户已拥有%s实例%s库的有效权限，不需要重复授予", inst.InstanceName, db))
			}
			rows = append(rows, mk(db, ""))
		}
		what = strings.Join(dbs, ",") + "（库权限）" // i18n-ignore 审计 / 通知原文入库
	case 2:
		db := strings.TrimSpace(req.DBName)
		tbs, err := cleanNames(req.TableList)
		if err != nil {
			return nil, err
		}
		if db == "" || len(tbs) == 0 {
			return nil, bad("请选择数据库和要授予的表")
		}
		if len([]rune(db)) > maxNameLen || strings.ContainsAny(db, ",\r\n") {
			return nil, bad("数据库名不合法")
		}
		if query.HasDBPriv(privs, db) {
			return nil, bad(fmt.Sprintf("用户已拥有%s实例%s库的全部权限，不需要重复授予", inst.InstanceName, db))
		}
		for _, tb := range tbs {
			if query.HasTablePriv(privs, db, tb) {
				return nil, bad(fmt.Sprintf("用户已拥有%s实例%s.%s表的有效权限，不需要重复授予", inst.InstanceName, db, tb))
			}
			rows = append(rows, mk(db, tb))
		}
		what = db + "." + strings.Join(tbs, ",")
	}

	err = s.repo.InTx(func(tx Repo) error {
		if err := tx.InsertPrivileges(rows); err != nil {
			return err
		}
		if te := ext(tx); te != nil && te.PrivExtReady() {
			exts := make([]PrivExt, 0, len(rows))
			for _, r := range rows {
				if r.PrivilegeID != 0 {
					exts = append(exts, PrivExt{PrivilegeID: r.PrivilegeID, Source: SourceGrant, GrantedBy: p.Username, Reason: reason, CreatedAt: now})
				}
			}
			if len(exts) > 0 {
				if err := te.SavePrivExts(exts); err != nil {
					slog.Warn("记录权限授予来源失败", "user", target.Username, "err", err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res := &GrantResult{}
	for _, r := range rows {
		res.PrivilegeIDs = append(res.PrivilegeIDs, r.PrivilegeID)
	}
	extra := fmt.Sprintf("用户:%s 实例:%s 对象:%s 行数上限:%d 有效期至:%s 原因:%s", target.Username, inst.InstanceName, what, req.LimitNum, valid.Format("2006-01-02"), reason) // i18n-ignore 系统审计原文入库
	if err := e.WriteAuditLog(&model.AuditEntry{UserID: p.ID, UserName: truncRunes(p.Username, 30), UserDisplay: truncRunes(p.Display, 50),
		Action: "直接授予查询权限", ExtraInfo: truncRunes(extra, 900), ActionTime: now}); err != nil { // i18n-ignore 审计动作名入库
		slog.Error("写直接授予审计失败", "operator", p.Username, "user", target.Username, "err", err)
	}
	// 通知被授予人。直接授予没有来源申请单，所以不回写申请单日志；审计上面已经写了。
	if len(rows) > 0 {
		first := rows[0]
		s.afterPrivChange(p, &first, privChange{verb: "授予", evName: notify.EventPassed, object: what, // i18n-ignore 通知原文
			changes: fmt.Sprintf("授予对象 %s，行数上限 %d，有效期至 %s，原因：%s", what, req.LimitNum, valid.Format("2006-01-02"), reason)}) // i18n-ignore 通知原文
	}
	slog.Info("查询权限已直接授予", "operator", p.Username, "user", target.Username, "instance_id", inst.ID, "object", what)
	return res, nil
}
