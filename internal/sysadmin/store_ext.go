// @author: kevinkelly
// @license: Apache Licence
// @file: store_ext.go
// @time: 2026/10/28

package sysadmin

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"

	"dbtool/internal/model"
)

// ---- 旁路表是否存在（进程内缓存，所有 store 共用） ----

type readyEntry struct {
	ok bool
	at time.Time
}

var readyCache = struct {
	mu sync.Mutex
	m  map[string]readyEntry
}{m: map[string]readyEntry{}}

const (
	readyTTL    = 5 * time.Minute // 存在：很少会被删掉
	notReadyTTL = 15 * time.Second
)

func resetReadyCache() {
	readyCache.mu.Lock()
	readyCache.m = map[string]readyEntry{}
	readyCache.mu.Unlock()
}

func isNoSuchTable(err error) bool {
	var me *mysqldrv.MySQLError
	return errors.As(err, &me) && me.Number == 1146
}

// TableReady 用一条探测查询判断表是否存在；结果缓存，页面加载不会每次都多一次查询。
func (s *store) TableReady(table string) bool {
	now := time.Now()
	readyCache.mu.Lock()
	if e, ok := readyCache.m[table]; ok {
		ttl := notReadyTTL
		if e.ok {
			ttl = readyTTL
		}
		if now.Sub(e.at) < ttl {
			readyCache.mu.Unlock()
			return e.ok
		}
	}
	readyCache.mu.Unlock()
	var one []int
	// table 只来自本文件里的常量，不含用户输入。
	err := s.quiet().Raw("SELECT 1 FROM `" + table + "` LIMIT 1").Scan(&one).Error
	ok := err == nil
	readyCache.mu.Lock()
	readyCache.m[table] = readyEntry{ok: ok, at: now}
	readyCache.mu.Unlock()
	return ok
}

var _ ExtRepo = (*store)(nil)

// ---- 资源组扩展 ----

func (s *store) ResGroupExts(ids []uint) (map[uint]ResGroupExt, error) {
	out := map[uint]ResGroupExt{}
	if !s.TableReady(tblResGroupExt) {
		return out, nil
	}
	var rows []struct {
		GroupID uint
		Leader  string
		Remark  string
	}
	q := s.db.Table(tblResGroupExt).Select("group_id, leader, remark")
	if len(ids) > 0 {
		q = q.Where("group_id IN ?", ids)
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.GroupID] = ResGroupExt{Leader: r.Leader, Remark: r.Remark}
	}
	return out, nil
}

func (s *store) UpsertResGroupExt(id uint, e ResGroupExt, by string, at time.Time) error {
	if !s.TableReady(tblResGroupExt) {
		return errExtMissing
	}
	return s.db.Exec("INSERT INTO `"+tblResGroupExt+"` (group_id, leader, remark, updated_by, updated_at) VALUES (?, ?, ?, ?, ?) "+
		"ON DUPLICATE KEY UPDATE leader = VALUES(leader), remark = VALUES(remark), updated_by = VALUES(updated_by), updated_at = VALUES(updated_at)",
		id, e.Leader, e.Remark, by, at).Error
}

// ---- 用户扩展 ----

func (s *store) UserProfiles(ids []uint) (map[uint]UserProfile, error) {
	out := map[uint]UserProfile{}
	if len(ids) == 0 || !s.TableReady(tblUserProfile) {
		return out, nil
	}
	var rows []struct {
		UserID             uint
		Dept               string
		AuthType           string
		MustChangePassword bool
	}
	if err := s.db.Table(tblUserProfile).Select("user_id, dept, auth_type, must_change_password").Where("user_id IN ?", ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.UserID] = UserProfile{Dept: r.Dept, AuthType: r.AuthType, MustChangePassword: r.MustChangePassword}
	}
	return out, nil
}

func (s *store) UpsertUserProfile(id uint, p UserProfile, by string, at time.Time) error {
	if !s.TableReady(tblUserProfile) {
		return errExtMissing
	}
	err := s.db.Exec("INSERT INTO `"+tblUserProfile+"` (user_id, dept, auth_type, must_change_password, updated_by, updated_at) VALUES (?, ?, ?, ?, ?, ?) "+
		"ON DUPLICATE KEY UPDATE dept = VALUES(dept), auth_type = VALUES(auth_type), must_change_password = VALUES(must_change_password), "+
		"updated_by = VALUES(updated_by), updated_at = VALUES(updated_at)", id, p.Dept, p.AuthType, p.MustChangePassword, by, at).Error
	if err == nil {
		forgetMustChange(id)
	}
	return err
}

func (s *store) SetMustChangePassword(id uint, v bool, by string, at time.Time) error {
	if !s.TableReady(tblUserProfile) {
		return errExtMissing
	}
	err := s.db.Exec("INSERT INTO `"+tblUserProfile+"` (user_id, must_change_password, updated_by, updated_at) VALUES (?, ?, ?, ?) "+
		"ON DUPLICATE KEY UPDATE must_change_password = VALUES(must_change_password), updated_by = VALUES(updated_by), updated_at = VALUES(updated_at)",
		id, v, by, at).Error
	if err == nil {
		forgetMustChange(id)
	}
	return err
}

func (s *store) UserIDsByDept(q string, limit int) ([]uint, error) {
	if q == "" || !s.TableReady(tblUserProfile) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	var ids []uint
	err := s.db.Raw("SELECT user_id FROM `"+tblUserProfile+"` WHERE dept LIKE ? LIMIT ?", likePattern(q), limit).Scan(&ids).Error
	return ids, err
}

// ---- 权限组扩展 ----

func (s *store) AuthGroupDescs(ids []uint) (map[uint]string, error) {
	out := map[uint]string{}
	if !s.TableReady(tblAuthGroupExt) {
		return out, nil
	}
	var rows []struct {
		GroupID     uint
		Description string
	}
	q := s.db.Table(tblAuthGroupExt).Select("group_id, description")
	if len(ids) > 0 {
		q = q.Where("group_id IN ?", ids)
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.GroupID] = r.Description
	}
	return out, nil
}

func (s *store) UpsertAuthGroupDesc(id uint, desc, by string, at time.Time) error {
	if !s.TableReady(tblAuthGroupExt) {
		return errExtMissing
	}
	return s.db.Exec("INSERT INTO `"+tblAuthGroupExt+"` (group_id, description, updated_by, updated_at) VALUES (?, ?, ?, ?) "+
		"ON DUPLICATE KEY UPDATE description = VALUES(description), updated_by = VALUES(updated_by), updated_at = VALUES(updated_at)",
		id, desc, by, at).Error
}

func (s *store) DeleteAuthGroupExt(id uint) error {
	if !s.TableReady(tblAuthGroupExt) {
		return nil
	}
	return s.db.Exec("DELETE FROM `"+tblAuthGroupExt+"` WHERE group_id = ?", id).Error
}

// columnReady 判断旁路表里的某一列是否存在（150_builtin_groups.sql 给 dbt_auth_group_ext 加 builtin 列）；结果缓存，规则同 TableReady。
// table、col 只来自调用处的常量，不含用户输入。
func (s *store) columnReady(table, col string) bool {
	key := table + "." + col
	now := time.Now()
	readyCache.mu.Lock()
	if e, ok := readyCache.m[key]; ok {
		ttl := notReadyTTL
		if e.ok {
			ttl = readyTTL
		}
		if now.Sub(e.at) < ttl {
			readyCache.mu.Unlock()
			return e.ok
		}
	}
	readyCache.mu.Unlock()
	var one []int
	ok := s.quiet().Raw("SELECT `"+col+"` FROM `"+table+"` LIMIT 1").Scan(&one).Error == nil
	readyCache.mu.Lock()
	readyCache.m[key] = readyEntry{ok: ok, at: now}
	readyCache.mu.Unlock()
	return ok
}

// AuthGroupBuiltins：表或 builtin 列不存在时返回空集合，权限组都按普通组处理。
func (s *store) AuthGroupBuiltins(ids []uint) (map[uint]bool, error) {
	out := map[uint]bool{}
	if !s.TableReady(tblAuthGroupExt) || !s.columnReady(tblAuthGroupExt, "builtin") {
		return out, nil
	}
	q := s.db.Table(tblAuthGroupExt).Select("group_id").Where("builtin = 1")
	if len(ids) > 0 {
		q = q.Where("group_id IN ?", ids)
	}
	var got []uint
	if err := q.Scan(&got).Error; err != nil {
		return nil, err
	}
	for _, id := range got {
		out[id] = true
	}
	return out, nil
}

// ---- 只读：登录方式与最近登录 IP ----

func (s *store) UserAuthKinds(ids []uint) (map[uint][]string, error) {
	out := map[uint][]string{}
	if len(ids) == 0 || !s.TableReady("dbt_auth_identity") || !s.TableReady("dbt_auth_provider") {
		return out, nil
	}
	var rows []struct {
		UserID uint
		Kind   string
	}
	err := s.db.Raw("SELECT i.user_id AS user_id, p.kind AS kind FROM dbt_auth_identity i JOIN dbt_auth_provider p ON p.id = i.provider_id "+
		"WHERE i.user_id IN ? ORDER BY p.kind", ids).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.UserID] = append(out[r.UserID], r.Kind)
	}
	return out, nil
}

// UserLastLoginIP：登录成功时 authx 写审计 action='登入'，extra_info 形如 “10.1.2.3（本地）”。
func (s *store) UserLastLoginIP(username string) (string, error) {
	var extra []string
	if err := s.db.Raw("SELECT extra_info FROM dbt_audit_log WHERE user_name = ? AND action = ? ORDER BY id DESC LIMIT 1", username, "登入").Scan(&extra).Error; err != nil { // i18n-ignore SQL 里按存库的动作名查询
		return "", err
	}
	if len(extra) == 0 {
		return "", nil
	}
	return ipFromLoginExtra(extra[0]), nil
}

// ipFromLoginExtra 取“登入”审计额外信息里的来源 IP（不是 IP 的返回空）。
func ipFromLoginExtra(extra string) string {
	head := strings.TrimSpace(extra)
	if i := strings.Index(head, "（"); i >= 0 {
		head = strings.TrimSpace(head[:i])
	}
	if net.ParseIP(head) == nil {
		return ""
	}
	return head
}

// ---- 资源组：进行中的工单、引用、彻底删除 ----

var wfStatusLabel = map[string]string{
	model.WfReviewPass: "审核通过", model.WfTimingTask: "定时执行", model.WfQueuing: "排队中", model.WfExecuting: "执行中",
}

func (s *store) OpenWorkflows(groupID uint, limit int) ([]OpenWorkflow, error) {
	if limit <= 0 {
		limit = 50
	}
	var audits []struct {
		WorkflowType  int
		WorkflowID    uint
		WorkflowTitle string
	}
	if err := s.db.Raw("SELECT workflow_type, workflow_id, workflow_title FROM dbt_workflow_audit WHERE group_id = ? AND current_status = 0 ORDER BY audit_id DESC LIMIT ?",
		groupID, limit).Scan(&audits).Error; err != nil {
		return nil, err
	}
	out := make([]OpenWorkflow, 0, len(audits))
	for _, a := range audits {
		w := OpenWorkflow{ID: a.WorkflowID, Name: a.WorkflowTitle, Status: "待审批"}
		switch a.WorkflowType {
		case WorkflowQuery:
			w.Kind, w.Label, w.Href = "query_priv", "查询权限", "/query/applies/"+utoa(a.WorkflowID)
		case WorkflowSQL:
			w.Kind, w.Label, w.Href = "sql", "SQL 上线", "/sqlworkflow/"+utoa(a.WorkflowID)
		default:
			w.Kind, w.Label = "archive", "数据归档"
		}
		out = append(out, w)
	}
	if left := limit - len(out); left > 0 {
		var wfs []struct {
			ID           uint
			WorkflowName string
			Status       string
		}
		if err := s.db.Raw("SELECT id, workflow_name, status FROM dbt_sql_workflow WHERE group_id = ? AND status IN ? ORDER BY id DESC LIMIT ?",
			groupID, []string{model.WfReviewPass, model.WfTimingTask, model.WfQueuing, model.WfExecuting}, left).Scan(&wfs).Error; err != nil {
			return nil, err
		}
		for _, w := range wfs {
			label := wfStatusLabel[w.Status]
			if label == "" {
				label = w.Status
			}
			out = append(out, OpenWorkflow{Kind: "sql", Label: "SQL 上线", ID: w.ID, Name: w.WorkflowName, Status: label, Href: "/sqlworkflow/" + utoa(w.ID)})
		}
	}
	return out, nil
}

// count 统计行数；表不存在（如性能工单模块未建表）按 0 处理。
func (s *store) count(query string, args ...any) (int64, error) {
	var n int64
	if err := s.quiet().Raw(query, args...).Scan(&n).Error; err != nil {
		if isNoSuchTable(err) {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}

func (s *store) EnabledChannelTypes() (map[uint]map[string]int, error) {
	out := map[uint]map[string]int{}
	var rows []struct {
		ID          uint   `gorm:"column:id"`
		GroupID     uint   `gorm:"column:group_id"`
		ChannelType string `gorm:"column:channel_type"`
	}
	if err := s.quiet().Raw("SELECT id, group_id, channel_type FROM dbt_notify_channel WHERE enabled = 1").Scan(&rows).Error; err != nil {
		if isNoSuchTable(err) {
			return out, nil
		}
		return nil, err
	}
	exts := map[uint]string{}
	if s.TableReady("dbt_notify_channel_ext") {
		var xs []struct {
			ChannelID uint   `gorm:"column:channel_id"`
			GroupIDs  string `gorm:"column:group_ids"`
		}
		if err := s.quiet().Raw("SELECT channel_id, group_ids FROM dbt_notify_channel_ext").Scan(&xs).Error; err == nil {
			for _, x := range xs {
				exts[x.ChannelID] = x.GroupIDs
			}
		}
	}
	for _, r := range rows {
		gids := parseChannelGroupIDs(exts[r.ID])
		if len(gids) == 0 && r.GroupID != 0 {
			gids = []uint{r.GroupID}
		}
		if len(gids) == 0 {
			gids = []uint{0} // 全局渠道
		}
		for _, g := range gids {
			if out[g] == nil {
				out[g] = map[string]int{}
			}
			out[g][r.ChannelType]++
		}
	}
	return out, nil
}

// parseChannelGroupIDs 解析扩展表里逗号分隔的资源组 ID（忽略空项与非法项）。
func parseChannelGroupIDs(s string) []uint {
	var out []uint
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 32); err == nil && n > 0 {
			out = append(out, uint(n))
		}
	}
	return out
}

func (s *store) ResGroupRefs(groupID uint) (ResGroupRefs, error) {
	var r ResGroupRefs
	for _, it := range []struct {
		dst *int64
		sql string
	}{
		{&r.Users, "SELECT COUNT(*) FROM dbt_sql_users_resource_group WHERE resourcegroup_id = ?"},
		{&r.Instances, "SELECT COUNT(*) FROM dbt_sql_instance_resource_group WHERE resourcegroup_id = ?"},
		{&r.SQLWorkflows, "SELECT COUNT(*) FROM dbt_sql_workflow WHERE group_id = ?"},
		{&r.AuditFlows, "SELECT COUNT(*) FROM dbt_workflow_audit WHERE group_id = ?"},
		{&r.PrivApplies, "SELECT COUNT(*) FROM dbt_query_privileges_apply WHERE group_id = ?"},
		{&r.PerfIssues, "SELECT COUNT(*) FROM dbt_perf_issue WHERE team_id = ?"},
		{&r.NotifyChannels, "SELECT COUNT(*) FROM dbt_notify_channel WHERE group_id = ?"},
		{&r.FlowSettings, "SELECT COUNT(*) FROM dbt_workflow_audit_setting WHERE group_id = ?"},
	} {
		n, err := s.count(it.sql, groupID)
		if err != nil {
			return r, err
		}
		*it.dst = n
	}
	return r, nil
}

func (s *store) PurgeResourceGroup(groupID uint) error {
	if err := s.db.Exec("DELETE FROM dbt_workflow_audit_setting WHERE group_id = ?", groupID).Error; err != nil {
		return err
	}
	if s.TableReady(tblResGroupExt) {
		if err := s.db.Exec("DELETE FROM `"+tblResGroupExt+"` WHERE group_id = ?", groupID).Error; err != nil {
			return err
		}
	}
	res := s.db.Exec("DELETE FROM dbt_resource_group WHERE group_id = ? AND is_deleted = 1", groupID)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 { // 并发下已被删除或被恢复
		return conflict("资源组已不存在或已被恢复，请刷新后重试")
	}
	return nil
}

// ---- 强制改密标记的进程内读取（供登录流程使用） ----

var mustChange = struct {
	mu    sync.Mutex
	store *store
	cache map[string]mustEntry
}{cache: map[string]mustEntry{}}

type mustEntry struct {
	v  bool
	at time.Time
}

const mustChangeTTL = 5 * time.Second

func setMustChangeStore(s *store) {
	mustChange.mu.Lock()
	mustChange.store = s
	mustChange.cache = map[string]mustEntry{}
	mustChange.mu.Unlock()
}

// forgetMustChange 丢弃缓存（本进程内写入标记后立即生效）。
func forgetMustChange(uint) {
	mustChange.mu.Lock()
	mustChange.cache = map[string]mustEntry{}
	mustChange.mu.Unlock()
}

// MustChangePassword 返回用户是否被要求“下次登录先修改密码”（管理员用系统生成的临时密码重置后置位）。
// 登录流程在密码校验通过后调用它决定是否强制跳转到修改密码页。表不存在、用户不存在、数据库出错都返回 false（宁可放行，不能锁死登录）。
func MustChangePassword(username string) bool {
	username = strings.TrimSpace(username)
	if username == "" {
		return false
	}
	mustChange.mu.Lock()
	st := mustChange.store
	if e, ok := mustChange.cache[username]; ok && time.Since(e.at) < mustChangeTTL {
		mustChange.mu.Unlock()
		return e.v
	}
	mustChange.mu.Unlock()
	if st == nil || !st.TableReady(tblUserProfile) {
		return false
	}
	var flags []bool
	err := st.quiet().Raw("SELECT p.must_change_password FROM `"+tblUserProfile+"` p JOIN dbt_sql_users u ON u.id = p.user_id WHERE u.username = ? LIMIT 1", username).Scan(&flags).Error
	v := err == nil && len(flags) > 0 && flags[0]
	mustChange.mu.Lock()
	if len(mustChange.cache) > 4096 {
		mustChange.cache = map[string]mustEntry{}
	}
	mustChange.cache[username] = mustEntry{v: v, at: time.Now()}
	mustChange.mu.Unlock()
	return v
}

// ClearMustChangePassword 清除“下次登录必须修改密码”标记（用户自己改密成功后由账号模块调用）。没有标记或表不存在时什么也不做。
func ClearMustChangePassword(username string) error {
	username = strings.TrimSpace(username)
	mustChange.mu.Lock()
	st := mustChange.store
	mustChange.mu.Unlock()
	if username == "" || st == nil || !st.TableReady(tblUserProfile) {
		return nil
	}
	err := st.quiet().Exec("UPDATE `"+tblUserProfile+"` p JOIN dbt_sql_users u ON u.id = p.user_id SET p.must_change_password = 0, p.updated_at = ? WHERE u.username = ?",
		time.Now(), username).Error
	forgetMustChange(0)
	return err
}

func utoa(n uint) string { return strconv.FormatUint(uint64(n), 10) }
