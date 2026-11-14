// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

package instmgr

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"dbtool/internal/crypto"
	"dbtool/internal/instext"
	"dbtool/internal/model"
)

// store 是 Repo 的 GORM 实现。只读写既有列，不建表。
type store struct {
	db *gorm.DB
	m  *crypto.Mirage
	// 旁路表就绪缓存（账号登记扩展、参数模板扩展；实例扩展表的缓存在 instext 包里）。
	regGate, tplGate tableGate
}

func newStore(db *gorm.DB, m *crypto.Mirage) *store { return &store{db: db, m: m} }

var _ Repo = (*store)(nil)

// ---- 实例 ----

func (r *store) ListInstances(f InstanceFilter, scope map[uint]bool) ([]model.Instance, int64, error) {
	q := r.db.Model(&model.Instance{})
	if scope != nil {
		if len(scope) == 0 {
			return nil, 0, nil
		}
		ids := make([]uint, 0, len(scope))
		for id := range scope {
			ids = append(ids, id)
		}
		q = q.Where("dbt_sql_instance.id IN ?", ids)
	}
	if f.Role != "" {
		q = q.Where("dbt_sql_instance.`type` = ?", f.Role)
	}
	if f.DBType != "" {
		q = q.Where("dbt_sql_instance.db_type = ?", f.DBType)
	}
	if f.Tag != "" {
		const existsTag = "EXISTS (SELECT 1 FROM dbt_sql_instance_instance_tag it JOIN dbt_sql_instance_tag t ON t.id = it.instancetag_id " +
			"WHERE it.instance_id = dbt_sql_instance.id AND t.tag_code %s AND t.active = 1)"
		q = q.Where(fmt.Sprintf(existsTag, "= ?"), f.Tag)
	}
	if f.Enabled != nil {
		q = q.Where("dbt_sql_instance.is_enable = ?", *f.Enabled)
	}
	if f.Q != "" {
		like := "%" + likeEscape(f.Q) + "%"
		q = q.Where("(dbt_sql_instance.instance_name LIKE ? ESCAPE '!' OR dbt_sql_instance.host LIKE ? ESCAPE '!')", like, like)
	}
	var none bool
	if q, none = r.extFilter(q, f); none {
		return nil, 0, nil
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []model.Instance
	err := q.Preload("Tags").Preload("ResourceGroups", "is_deleted = 0").Preload("Tunnel").
		Order("dbt_sql_instance.id").Offset(f.Offset).Limit(f.Limit).Find(&out).Error
	return out, total, err
}

func (r *store) GetInstance(id uint) (*model.Instance, error) {
	var inst model.Instance
	err := r.db.Preload("Tags").Preload("ResourceGroups", "is_deleted = 0").Preload("Tunnel").First(&inst, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inst, nil
}

func (r *store) BriefInstances(scope map[uint]bool) ([]model.Instance, error) {
	q := r.db.Model(&model.Instance{}).Select("id, instance_name, `type`, db_type")
	if scope != nil {
		if len(scope) == 0 {
			return nil, nil
		}
		ids := make([]uint, 0, len(scope))
		for id := range scope {
			ids = append(ids, id)
		}
		q = q.Where("id IN ?", ids)
	}
	q = instext.ExcludeDisabled(q, "dbt_sql_instance.id") // 已停用的实例不出现在各页面的实例下拉里
	var out []model.Instance
	err := q.Order("instance_name").Find(&out).Error
	return out, err
}

func (r *store) InstanceOptions() (*InstanceOptions, error) {
	out := &InstanceOptions{}
	var groups []model.ResourceGroup
	if err := r.db.Where("is_deleted = 0").Order("group_name").Find(&groups).Error; err != nil {
		return nil, err
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, GroupOption{ID: g.GroupID, Name: g.GroupName})
	}
	var tags []model.InstanceTag
	if err := r.db.Order("id").Find(&tags).Error; err != nil {
		return nil, err
	}
	for _, t := range tags {
		out.Tags = append(out.Tags, TagOption{ID: t.ID, Code: t.TagCode, Name: t.TagName, Active: t.Active})
	}
	var tunnels []model.Tunnel
	if err := r.db.Select("id", "tunnel_name").Order("tunnel_name").Find(&tunnels).Error; err != nil {
		return nil, err
	}
	for _, t := range tunnels {
		out.Tunnels = append(out.Tunnels, TunnelOption{ID: t.ID, Name: t.TunnelName})
	}
	return out, nil
}

func (r *store) TunnelInfo(id uint) (*TunnelInfo, error) {
	var t model.Tunnel
	err := r.db.First(&t, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r.tunnelOf(&t), nil
}

func (r *store) tunnelOf(t *model.Tunnel) *TunnelInfo {
	dec := func(p *string) string {
		if p == nil {
			return ""
		}
		return r.m.Decrypt(*p)
	}
	return &TunnelInfo{Host: t.Host, Port: t.Port, User: dec(t.User), Password: dec(t.Password), Pkey: dec(t.Pkey), PkeyPassword: dec(t.PkeyPassword)}
}

func (r *store) InstanceNameTaken(name string, exceptID uint) (bool, error) {
	var n int64
	err := r.db.Model(&model.Instance{}).Where("instance_name = ? AND id <> ?", name, exceptID).Count(&n).Error
	return n > 0, err
}

func (r *store) SaveInstance(id uint, in InstanceWrite) (uint, error) {
	now := time.Now()
	err := r.db.Transaction(func(tx *gorm.DB) error {
		cols := map[string]any{
			"instance_name": in.Name, "type": in.Role, "db_type": in.DBType, "mode": in.Mode, "host": in.Host, "port": in.Port,
			"user": r.m.Encrypt(in.User), "is_ssl": in.IsSSL, "is_enable": in.IsEnable, "verify_ssl": in.VerifySSL,
			"db_name": in.DBName, "show_db_name_regex": in.ShowRegex, "denied_db_name_regex": in.DeniedRegex, "charset": in.Charset,
			"update_time": now,
		}
		if in.Password != nil {
			cols["password"] = r.m.Encrypt(*in.Password)
		}
		if in.TunnelID != nil {
			cols["tunnel_id"] = *in.TunnelID
		} else {
			cols["tunnel_id"] = nil
		}
		if in.ServiceName != nil {
			cols["service_name"] = *in.ServiceName
		} else {
			cols["service_name"] = nil
		}
		if in.SID != nil {
			cols["sid"] = *in.SID
		} else {
			cols["sid"] = nil
		}
		if id == 0 {
			inst := model.Instance{InstanceName: in.Name, Type: in.Role, DBType: in.DBType, Mode: in.Mode, Host: in.Host, Port: in.Port,
				User: r.m.Encrypt(in.User), Password: r.m.Encrypt(derefStr(in.Password)), IsSSL: in.IsSSL, IsEnable: in.IsEnable, VerifySSL: in.VerifySSL,
				DBName: in.DBName, ShowDBNameRegex: in.ShowRegex, DeniedDBNameRegex: in.DeniedRegex, Charset: in.Charset,
				ServiceName: in.ServiceName, SID: in.SID, TunnelID: in.TunnelID, CreateTime: now, UpdateTime: now}
			if err := tx.Omit(clause.Associations).Create(&inst).Error; err != nil {
				return err
			}
			id = inst.ID
		} else {
			res := tx.Model(&model.Instance{}).Where("id = ?", id).Updates(cols)
			if res.Error != nil {
				return res.Error
			}
		}
		// 资源组与标签是多对多关联表：整体替换。
		if err := tx.Exec("DELETE FROM dbt_sql_instance_resource_group WHERE instance_id = ?", id).Error; err != nil {
			return err
		}
		for _, g := range in.GroupIDs {
			if err := tx.Exec("INSERT INTO dbt_sql_instance_resource_group (instance_id, resourcegroup_id) VALUES (?, ?)", id, g).Error; err != nil {
				return err
			}
		}
		if err := tx.Exec("DELETE FROM dbt_sql_instance_instance_tag WHERE instance_id = ?", id).Error; err != nil {
			return err
		}
		for _, t := range in.TagIDs {
			if err := tx.Exec("INSERT INTO dbt_sql_instance_instance_tag (instance_id, instancetag_id) VALUES (?, ?)", id, t).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (r *store) SetInstanceEnable(id uint, enable bool) error {
	return r.db.Model(&model.Instance{}).Where("id = ?", id).Updates(map[string]any{"is_enable": enable, "update_time": time.Now()}).Error
}

// refCheck 描述一类引用：查询必须返回 COUNT(*)。查询里的 ? 都绑定实例 ID。
type refCheck struct {
	label    string
	table    string
	where    string
	blocking bool
	hint     string
}

const refCap = 10000

var refChecks = []refCheck{
	{"SQL 工单", "dbt_sql_workflow", "instance_id = ?", true, "工单是审计记录，不能随实例删除"},
	{"批量导出工单", "dbt_sql_workflow", "FIND_IN_SET(?, batch_instance_ids) > 0", true, ""},
	{"查询权限申请", "dbt_query_privileges_apply", "instance_id = ?", true, ""},
	{"查询权限", "dbt_query_privileges", "instance_id = ?", true, "先在“查询权限”里回收并清理"},
	{"分区维护配置", "dbt_partman_config", "instance_id = ?", true, ""},
	// 数据归档（deploy/sql/180_archive.sql）：任务按来源实例或库表备份的目标实例（dest_table JSON 里的 instance_id，它是该 JSON 的第一个字段）匹配。
	// 运行记录只提示、不阻断：任务删除后运行记录仍保留（带实例名快照），没有页面可以删它们，阻断会让实例永远删不掉。
	{"数据归档任务", "dbt_archive_task", `instance_id = ? OR dest_table LIKE CONCAT('%"instance_id":', ?, ',%')`, true, "先在“数据归档”里停用并删除这些任务"},
	{"数据归档运行记录", "dbt_archive_run", "instance_id = ? OR dest_instance_id = ?", false, "运行记录是归档审计，会保留（含实例名快照）"},
	{"脱敏字段配置", "dbt_masking_policy", "instance_id = ?", true, ""},
	{"SQL Server 巡检报告", "dbt_sql_mssql_inspection_report", "instance_id = ?", true, ""},
	{"MySQL 巡检报告", "dbt_sql_mysql_inspection_report", "instance_id = ?", true, ""},
	{"Redis 巡检报告", "dbt_inspect_redis_report", "instance_id = ?", true, "先在“性能巡检”里删除这些报告"},
	{"性能工单", "dbt_perf_issue", "instance_id = ?", true, "先在“性能工单”里关闭或转移这些工单"},
	{"慢日志采集数据", "dbt_slow_logs", "instance_id = ?", true, "先停用慢日志采集并清理 dbt_slow_logs 里该实例的数据"},
	// 以下是配置类数据，随实例一起删除，只提示数量。
	{"Redis 慢查询采集数据", "dbt_redis_slowlog", "instance_id = ?", false, ""},
	{"账号登记", "dbt_instance_account", "instance_id = ?", false, ""},
	{"数据库登记", "dbt_instance_database", "instance_id = ?", false, ""},
	{"参数修改历史", "dbt_param_history", "instance_id = ?", false, ""},
}

func isMissingTable(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "1146") || strings.Contains(m, "1054") || strings.Contains(m, "doesn't exist") || strings.Contains(m, "Unknown column")
}

func (r *store) InstanceRefs(id uint) ([]Ref, error) {
	var out []Ref
	for _, c := range refChecks {
		args := make([]any, strings.Count(c.where, "?"))
		for i := range args {
			args[i] = id
		}
		var n int64
		q := fmt.Sprintf("SELECT COUNT(*) FROM (SELECT 1 FROM %s WHERE %s LIMIT %d) t", c.table, c.where, refCap+1)
		if err := r.db.Raw(q, args...).Scan(&n).Error; err != nil {
			if isMissingTable(err) {
				continue // 该部署没有这张表/列
			}
			return nil, err
		}
		if n == 0 {
			continue
		}
		ref := Ref{Label: c.label, Count: n, Blocking: c.blocking, Hint: c.hint}
		if n > refCap {
			ref.Count, ref.Capped = refCap, true
		}
		out = append(out, ref)
	}
	return out, nil
}

func (r *store) DeleteInstance(id uint) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		for _, q := range []string{
			"DELETE FROM dbt_instance_account WHERE instance_id = ?",
			"DELETE FROM dbt_instance_database WHERE instance_id = ?",
			"DELETE FROM dbt_param_history WHERE instance_id = ?",
			"DELETE FROM dbt_sql_instance_resource_group WHERE instance_id = ?",
			"DELETE FROM dbt_sql_instance_instance_tag WHERE instance_id = ?",
			"DELETE FROM dbt_redis_slowlog WHERE instance_id = ?",
			"DELETE FROM dbt_redis_slowlog_state WHERE instance_id = ?",
		} {
			if err := tx.Exec(q, id).Error; err != nil && !isMissingTable(err) {
				return err
			}
		}
		return tx.Delete(&model.Instance{}, id).Error
	})
}

func (r *store) ConnInfo(inst *model.Instance) (ConnInfo, error) {
	ci := ConnInfo{DBType: inst.DBType, Host: inst.Host, Port: inst.Port, User: r.m.Decrypt(inst.User), Password: r.m.Decrypt(inst.Password),
		DBName: inst.DBName, Charset: inst.Charset, SSL: inst.IsSSL, VerifySSL: inst.VerifySSL, ServiceName: inst.ServiceNameValue(), SID: inst.SIDValue()}
	if inst.Tunnel != nil {
		ci.Tunnel = r.tunnelOf(inst.Tunnel)
	} else if inst.TunnelID != nil {
		t, err := r.TunnelInfo(*inst.TunnelID)
		if err != nil {
			return ci, err
		}
		ci.Tunnel = t
	}
	return ci, nil
}

func (r *store) Decrypt(s string) string { return r.m.Decrypt(s) }

// UserInstanceIDs 返回用户通过资源组可访问的实例。
func (r *store) UserInstanceIDs(userID uint) (map[uint]bool, error) {
	var ids []uint
	err := r.db.Raw(`
SELECT DISTINCT ir.instance_id FROM dbt_sql_instance_resource_group ir
JOIN dbt_resource_group rg ON rg.group_id = ir.resourcegroup_id AND rg.is_deleted = 0
JOIN dbt_sql_users_resource_group ur ON ur.resourcegroup_id = rg.group_id
WHERE ur.users_id = ?`, userID).Scan(&ids).Error
	out := make(map[uint]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

// ---- 数据库登记 ----

type dbRow struct {
	ID           uint      `gorm:"primaryKey"`
	InstanceID   uint      `gorm:"column:instance_id"`
	DBName       string    `gorm:"column:db_name"`
	Owner        string    `gorm:"column:owner"`
	OwnerDisplay string    `gorm:"column:owner_display"`
	Remark       string    `gorm:"column:remark"`
	SysTime      time.Time `gorm:"column:sys_time"`
}

func (dbRow) TableName() string { return "dbt_instance_database" }

func (r *store) SavedDatabases(instanceID uint) ([]DatabaseRecord, error) {
	var rows []dbRow
	if err := r.db.Where("instance_id = ?", instanceID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]DatabaseRecord, 0, len(rows))
	for _, x := range rows {
		out = append(out, DatabaseRecord{DBName: x.DBName, Owner: x.Owner, OwnerDisplay: x.OwnerDisplay, Remark: x.Remark})
	}
	return out, nil
}

func (r *store) SaveDatabaseRecord(instanceID uint, rec DatabaseRecord) error {
	var row dbRow
	err := r.db.Where("instance_id = ? AND db_name = ?", instanceID, rec.DBName).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return r.db.Create(&dbRow{InstanceID: instanceID, DBName: rec.DBName, Owner: rec.Owner, OwnerDisplay: rec.OwnerDisplay, Remark: rec.Remark, SysTime: time.Now()}).Error
	}
	if err != nil {
		return err
	}
	return r.db.Model(&dbRow{}).Where("id = ?", row.ID).Updates(map[string]any{"owner": rec.Owner, "owner_display": rec.OwnerDisplay,
		"remark": rec.Remark, "sys_time": time.Now()}).Error
}

func (r *store) ActiveUser(username string) (*model.User, error) {
	var u model.User
	err := r.db.Where("username = ? AND is_active = 1", username).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *store) ActiveUsers() ([]model.User, error) {
	var out []model.User
	err := r.db.Select("id", "username", "display").Where("is_active = 1").Order("username").Limit(5000).Find(&out).Error
	return out, err
}

// ---- 账号登记（user、password 是 mirage 密文，db_name 是 Mongo 用的，MySQL 系为空串）----

type accRow struct {
	ID         uint      `gorm:"primaryKey"`
	InstanceID uint      `gorm:"column:instance_id"`
	User       string    `gorm:"column:user"`
	Host       string    `gorm:"column:host"`
	DBName     string    `gorm:"column:db_name"`
	Password   string    `gorm:"column:password"`
	Remark     string    `gorm:"column:remark"`
	SysTime    time.Time `gorm:"column:sys_time"`
}

func (accRow) TableName() string { return "dbt_instance_account" }

func (r *store) accountRows(instanceID uint) ([]accRow, error) {
	var rows []accRow
	err := r.db.Where("instance_id = ? AND db_name = ''", instanceID).Find(&rows).Error
	return rows, err
}

func (r *store) SavedAccounts(instanceID uint) ([]AccountRecord, error) {
	rows, err := r.accountRows(instanceID)
	if err != nil {
		return nil, err
	}
	out := make([]AccountRecord, 0, len(rows))
	for _, x := range rows {
		out = append(out, AccountRecord{User: r.m.Decrypt(x.User), Host: x.Host, Remark: x.Remark})
	}
	return out, nil
}

func (r *store) findAccount(instanceID uint, user, host string) (*accRow, error) {
	rows, err := r.accountRows(instanceID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Host == host && r.m.Decrypt(rows[i].User) == user {
			return &rows[i], nil
		}
	}
	return nil, nil
}

func (r *store) SaveAccountRecord(instanceID uint, rec AccountRecord) error {
	row, err := r.findAccount(instanceID, rec.User, rec.Host)
	if err != nil {
		return err
	}
	if row == nil {
		return r.db.Create(&accRow{InstanceID: instanceID, User: r.m.Encrypt(rec.User), Host: rec.Host, Password: r.m.Encrypt(derefStr(rec.Password)),
			Remark: rec.Remark, SysTime: time.Now()}).Error
	}
	cols := map[string]any{"sys_time": time.Now()}
	if rec.Password != nil {
		cols["password"] = r.m.Encrypt(*rec.Password)
	}
	if !rec.KeepRemark { // 改密流程只更新密码，不动备注
		cols["remark"] = rec.Remark
	}
	return r.db.Model(&accRow{}).Where("id = ?", row.ID).Updates(cols).Error
}

func (r *store) DeleteAccountRecord(instanceID uint, user, host string) error {
	row, err := r.findAccount(instanceID, user, host)
	if err != nil || row == nil {
		return err
	}
	return r.db.Delete(&accRow{}, row.ID).Error
}

// ---- 参数 ----

func (r *store) ListTemplates(dbType string) ([]ParamTemplate, error) {
	var out []ParamTemplate
	err := r.db.Where("db_type = ?", dbType).Order("variable_name").Find(&out).Error
	return out, err
}

func (r *store) GetTemplate(dbType, name string) (*ParamTemplate, error) {
	var t ParamTemplate
	err := r.db.Where("db_type = ? AND LOWER(variable_name) = ?", dbType, strings.ToLower(name)).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *store) GetTemplateByID(id uint) (*ParamTemplate, error) {
	var t ParamTemplate
	err := r.db.First(&t, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *store) SaveTemplate(t *ParamTemplate) error {
	if t.ID == 0 {
		return r.db.Create(t).Error
	}
	return r.db.Model(&ParamTemplate{}).Where("id = ?", t.ID).Updates(map[string]any{"default_value": t.DefaultValue, "editable": t.Editable,
		"valid_values": t.ValidValues, "description": t.Description, "sys_time": t.SysTime}).Error
}

func (r *store) InsertParamHistory(h *ParamHistory) error { return r.db.Create(h).Error }

func (r *store) ListParamHistory(instanceID uint, q string, offset, limit int) ([]ParamHistoryRow, int64, error) {
	base := r.db.Table("dbt_param_history ph").Joins("JOIN dbt_sql_instance i ON i.id = ph.instance_id").Where("ph.instance_id = ?", instanceID)
	if q != "" {
		base = base.Where("ph.variable_name LIKE ? ESCAPE '!'", "%"+likeEscape(q)+"%")
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []ParamHistoryRow
	err := base.Select("ph.*, i.instance_name AS instance_name").Order("ph.id DESC").Offset(offset).Limit(limit).Scan(&out).Error
	return out, total, err
}

// ---- 审计 ----

func (r *store) InsertAudit(e *model.AuditEntry) error { return r.db.Create(e).Error }
