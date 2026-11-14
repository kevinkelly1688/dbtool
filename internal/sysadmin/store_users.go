// @author: kevinkelly
// @license: Apache Licence
// @file: store_users.go
// @time: 2026/10/28

package sysadmin

import (
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"dbtool/internal/model"
)

// ---- 用户 ----

func (s *store) GetUser(id uint) (*model.User, error) {
	var u model.User
	if err := s.db.Omit("password").First(&u, id).Error; err != nil {
		return nil, nilIfMissing(err)
	}
	return &u, nil
}

func (s *store) GetUserByName(name string) (*model.User, error) {
	var u model.User
	if err := s.db.Omit("password").Where("username = ?", name).First(&u).Error; err != nil {
		return nil, nilIfMissing(err)
	}
	return &u, nil
}

func (s *store) ListUsers(f UserFilter) ([]model.User, int64, error) {
	q := s.db.Model(&model.User{}).Omit("password") // 列表不需要密码哈希，不读出来
	if f.Search != "" {
		p := likePattern(f.Search)
		if len(f.DeptUserIDs) > 0 {
			q = q.Where("(dbt_sql_users.username LIKE ? OR dbt_sql_users.display LIKE ? OR dbt_sql_users.email LIKE ? OR dbt_sql_users.id IN ?)", p, p, p, f.DeptUserIDs)
		} else {
			q = q.Where("(dbt_sql_users.username LIKE ? OR dbt_sql_users.display LIKE ? OR dbt_sql_users.email LIKE ?)", p, p, p)
		}
	}
	if f.AuthGroupID != 0 {
		q = q.Where("EXISTS (SELECT 1 FROM dbt_sql_users_groups g WHERE g.users_id = dbt_sql_users.id AND g.group_id = ?)", f.AuthGroupID)
	}
	if f.ResourceGroupID != 0 {
		q = q.Where("EXISTS (SELECT 1 FROM dbt_sql_users_resource_group r WHERE r.users_id = dbt_sql_users.id AND r.resourcegroup_id = ?)", f.ResourceGroupID)
	}
	switch f.Status {
	case StatusActive:
		q = q.Where("dbt_sql_users.is_active = 1")
	case StatusInactive:
		q = q.Where("dbt_sql_users.is_active = 0")
	case StatusLocked:
		q = q.Where("dbt_sql_users.failed_login_count >= ? AND dbt_sql_users.last_login_failed_at > ?", f.LockCount, f.Now.Add(-f.LockWindow))
	case StatusNo2FA:
		q = q.Where("NOT EXISTS (SELECT 1 FROM `dbt_2fa_config` t WHERE t.user_id = dbt_sql_users.id)")
	case StatusSuper:
		q = q.Where("dbt_sql_users.is_superuser = 1")
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.User
	err := q.Order(userOrder(f.Sort)).Offset(f.Offset).Limit(f.Limit).Find(&rows).Error
	return rows, total, err
}

// userOrder 把排序参数（白名单）翻译成 ORDER BY；不认识的值回到用户名升序。
func userOrder(sort string) string {
	switch sort {
	case SortLastLogin:
		return "(dbt_sql_users.last_login IS NULL), dbt_sql_users.last_login DESC, dbt_sql_users.username"
	case SortStatus:
		return "dbt_sql_users.is_active ASC, dbt_sql_users.failed_login_count DESC, dbt_sql_users.username"
	}
	return "dbt_sql_users.username"
}

func (s *store) CreateUser(u *model.User) error {
	return s.quiet().Omit(clause.Associations).Create(u).Error
}

func (s *store) UpdateUser(id uint, fields map[string]any) error {
	return s.quiet().Model(&model.User{}).Where("id = ?", id).Updates(fields).Error
}

func (s *store) CountOtherSuperusers(excludeID uint) (int64, error) {
	var ids []uint
	err := s.db.Model(&model.User{}).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("is_superuser = 1 AND is_active = 1 AND id <> ?", excludeID).Pluck("id", &ids).Error
	return int64(len(ids)), err
}

type userGroupScan struct {
	UserID uint
	ID     uint
	Name   string
}

func groupMap(rows []userGroupScan) map[uint][]IDName {
	out := map[uint][]IDName{}
	for _, r := range rows {
		out[r.UserID] = append(out[r.UserID], IDName{r.ID, r.Name})
	}
	return out
}

func (s *store) UserAuthGroups(userIDs []uint) (map[uint][]IDName, error) {
	if len(userIDs) == 0 {
		return map[uint][]IDName{}, nil
	}
	var rows []userGroupScan
	err := s.db.Raw("SELECT ug.users_id AS user_id, g.id AS id, g.name AS name FROM dbt_sql_users_groups ug "+
		"JOIN dbt_auth_group g ON g.id = ug.group_id WHERE ug.users_id IN ? ORDER BY g.name", userIDs).Scan(&rows).Error
	return groupMap(rows), err
}

// UserResourceGroups 只返回未停用的资源组。
func (s *store) UserResourceGroups(userIDs []uint) (map[uint][]IDName, error) {
	if len(userIDs) == 0 {
		return map[uint][]IDName{}, nil
	}
	var rows []userGroupScan
	err := s.db.Raw("SELECT ur.users_id AS user_id, rg.group_id AS id, rg.group_name AS name FROM dbt_sql_users_resource_group ur "+
		"JOIN dbt_resource_group rg ON rg.group_id = ur.resourcegroup_id AND rg.is_deleted = 0 WHERE ur.users_id IN ? ORDER BY rg.group_name", userIDs).Scan(&rows).Error
	return groupMap(rows), err
}

// UserTwoFATypes 返回用户已绑定的 2FA 类型。dbt_2fa_config.auth_type 是 mirage 加密字段。
func (s *store) UserTwoFATypes(userIDs []uint) (map[uint][]string, error) {
	out := map[uint][]string{}
	if len(userIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		UserID   uint
		AuthType string
	}
	if err := s.db.Raw("SELECT user_id, auth_type FROM `dbt_2fa_config` WHERE user_id IN ?", userIDs).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		t := strings.TrimSpace(s.m.Decrypt(r.AuthType))
		if t == "" {
			t = "unknown"
		}
		out[r.UserID] = append(out[r.UserID], t)
	}
	return out, nil
}

// replaceLinks 让多对多关联表里 owner 的关联集合变成 ids：先按 current 查询出现有集合，只增删差异。
// table/ownerCol/otherCol 都是调用方传入的常量；current 是读取现有集合的 SQL（只含本次要管理的范围）。
func (s *store) replaceLinks(table, ownerCol, otherCol string, owner uint, ids []uint, current string, args ...any) error {
	var cur []uint
	if err := s.db.Raw(current, args...).Scan(&cur).Error; err != nil {
		return err
	}
	have, want := map[uint]bool{}, map[uint]bool{}
	for _, id := range cur {
		have[id] = true
	}
	var add, del []uint
	for _, id := range ids {
		want[id] = true
		if !have[id] {
			add = append(add, id)
		}
	}
	for _, id := range cur {
		if !want[id] {
			del = append(del, id)
		}
	}
	if len(del) > 0 {
		if err := s.db.Exec("DELETE FROM `"+table+"` WHERE `"+ownerCol+"` = ? AND `"+otherCol+"` IN ?", owner, del).Error; err != nil {
			return err
		}
	}
	return s.insertLinks(table, ownerCol, otherCol, owner, add)
}

// insertLinks 批量插入关联（INSERT IGNORE：多对多关联表对两列有唯一约束，重复插入被忽略）。
func (s *store) insertLinks(table, ownerCol, otherCol string, owner uint, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)*2)
	for i, id := range ids {
		ph[i] = "(?, ?)"
		args = append(args, owner, id)
	}
	return s.db.Exec("INSERT IGNORE INTO `"+table+"` (`"+ownerCol+"`, `"+otherCol+"`) VALUES "+strings.Join(ph, ","), args...).Error
}

func (s *store) deleteLinks(table, ownerCol, otherCol string, owner uint, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.Exec("DELETE FROM `"+table+"` WHERE `"+ownerCol+"` = ? AND `"+otherCol+"` IN ?", owner, ids).Error
}

func (s *store) SetUserAuthGroups(userID uint, ids []uint) error {
	return s.replaceLinks("dbt_sql_users_groups", "users_id", "group_id", userID, ids,
		"SELECT group_id FROM dbt_sql_users_groups WHERE users_id = ?", userID)
}

// SetUserResourceGroups 只管理未停用的资源组，用户在已停用资源组里的关联保持不变。
func (s *store) SetUserResourceGroups(userID uint, ids []uint) error {
	return s.replaceLinks("dbt_sql_users_resource_group", "users_id", "resourcegroup_id", userID, ids,
		"SELECT ur.resourcegroup_id FROM dbt_sql_users_resource_group ur JOIN dbt_resource_group rg ON rg.group_id = ur.resourcegroup_id AND rg.is_deleted = 0 WHERE ur.users_id = ?", userID)
}

func (s *store) DeleteUserTwoFA(userID uint) (int64, error) {
	res := s.db.Exec("DELETE FROM `dbt_2fa_config` WHERE user_id = ?", userID)
	return res.RowsAffected, res.Error
}

// ---- 权限组 ----

func (s *store) ListAuthGroups() ([]AuthGroupRow, error) {
	var rows []AuthGroupRow
	err := s.db.Raw("SELECT g.id AS id, g.name AS name, " +
		"(SELECT COUNT(*) FROM dbt_sql_users_groups ug WHERE ug.group_id = g.id) AS members " +
		"FROM dbt_auth_group g ORDER BY g.name").Scan(&rows).Error
	return rows, err
}

func (s *store) GetAuthGroup(id uint) (*model.AuthGroup, error) {
	var g model.AuthGroup
	if err := s.db.First(&g, id).Error; err != nil {
		return nil, nilIfMissing(err)
	}
	return &g, nil
}

func (s *store) AuthGroupByName(name string) (*model.AuthGroup, error) {
	var g model.AuthGroup
	if err := s.db.Where("name = ?", name).First(&g).Error; err != nil {
		return nil, nilIfMissing(err)
	}
	return &g, nil
}

func (s *store) CreateAuthGroup(g *model.AuthGroup) error { return s.db.Create(g).Error }

func (s *store) RenameAuthGroup(id uint, name string) error {
	return s.db.Model(&model.AuthGroup{}).Where("id = ?", id).Update("name", name).Error
}

func (s *store) DeleteAuthGroup(id uint) error {
	var last *gorm.DB
	for _, q := range []string{
		"DELETE FROM dbt_sql_users_groups WHERE group_id = ?",
		"DELETE FROM dbt_auth_group_permissions WHERE group_id = ?",
		"DELETE FROM dbt_auth_group WHERE id = ?",
	} {
		last = s.db.Exec(q, id)
		if last.Error != nil {
			return last.Error
		}
	}
	// 最后一条删的是权限组本体：影响 0 行说明并发的另一个请求已经删掉了它（本事务的快照里还看得到），
	// 返回“不存在”让整个事务回滚，避免重复写审计。
	if last.RowsAffected == 0 {
		return notFound("权限组不存在")
	}
	return nil
}

func (s *store) AuthGroupMembers(id uint) ([]UserRef, error) {
	var rows []UserRef
	err := s.db.Raw("SELECT u.id AS id, u.username AS username, u.display AS display, u.is_active AS is_active FROM dbt_sql_users u "+
		"JOIN dbt_sql_users_groups ug ON ug.users_id = u.id WHERE ug.group_id = ? ORDER BY u.username", id).Scan(&rows).Error
	return rows, err
}

// PendingAuditsUsingGroup 统计审批流里含有该权限组、且仍待审批的工单数。
func (s *store) PendingAuditsUsingGroup(authGroupID uint) (int64, error) {
	var n int64
	err := s.db.Raw("SELECT COUNT(*) FROM dbt_workflow_audit WHERE current_status = 0 AND FIND_IN_SET(?, REPLACE(audit_auth_groups, ' ', '')) > 0",
		authGroupID).Scan(&n).Error
	return n, err
}
