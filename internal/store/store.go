// @author: kevinkelly
// @license: Apache Licence
// @file: store.go
// @time: 2026/10/28

// Package store 封装对元数据库（MySQL）的访问。
package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"dbtool/internal/audit"
	"dbtool/internal/btnperm"
	"dbtool/internal/config"
	"dbtool/internal/crypto"
	"dbtool/internal/engine"
	"dbtool/internal/instext"
	"dbtool/internal/model"
	"dbtool/internal/query"
	"dbtool/internal/settings"
	"dbtool/internal/sshtunnel"
)

type Store struct {
	DB     *gorm.DB
	Mirage *crypto.Mirage
}

func Open(c *config.Config) (*Store, error) {
	dsn, err := c.MySQLDSN()
	if err != nil {
		return nil, err
	}
	lvl := logger.Warn
	if c.Debug {
		lvl = logger.Info
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger:         logger.Default.LogMode(lvl),
		NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	if err != nil {
		return nil, fmt.Errorf("连接元数据库: %w", err) // i18n-ignore
	}
	m, err := crypto.NewMirage(c.MirageSecretKey, c.MirageCipherMode, c.MirageCipherIV)
	if err != nil {
		return nil, err
	}
	// 按钮级权限：进程内共用一个带缓存的解析器；DDL（deploy/sql/019_btnperm.sql）没执行时按“没有按钮授予”运行。
	btnperm.SetDefault(btnperm.New(btnperm.NewGormRepo(db)))
	// 审计扩展信息（结果 / 来源 IP / 类别 / 权限码 / 高危）：在 GORM 层统一旁路写入 dbt_audit_ext，各模块写 dbt_audit_log 的代码不用改。
	audit.RegisterExtHook(db)
	st := &Store{DB: db, Mirage: m}
	return st, nil
}

// Settings 返回系统设置服务（进程内共享，带缓存）。
func (s *Store) Settings() *settings.Service { return settings.Open(s.DB, s.Mirage) }

// ListInstances 返回实例列表（敏感字段不会被序列化，见 model 的 json:"-"）。
func (s *Store) ListInstances() ([]model.Instance, error) {
	var out []model.Instance
	// 已停用的实例（旁路表 dbt_instance_ext；表不存在时不过滤）不再出现在各页面的实例列表里。
	err := instext.ExcludeDisabled(s.DB.Preload("Tags"), "dbt_sql_instance.id").Order("instance_name").Find(&out).Error
	return out, err
}

// ConfigValue 读取一项系统设置的生效值（显式设置的值，否则登记的默认值；敏感项已解密）。没有或为空白返回 ok=false。
// key 是 settings.KeyXxx 常量。
func (s *Store) ConfigValue(key string) (string, bool) { return s.Settings().Value(key) }

// InsertQueryLog 写入在线查询审计日志。
func (s *Store) InsertQueryLog(l *model.QueryLog) error {
	return s.DB.Create(l).Error
}

// likeEscape 转义 LIKE 里的通配符，让用户输入的 % 和 _ 按字面匹配。
func likeEscape(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

// ListQueryLogs 按条件分页查询执行历史（新的在前）。关键词之间是“且”，对 SQL 文本做不区分大小写的模糊匹配。
func (s *Store) ListQueryLogs(f query.QueryLogFilter) ([]model.QueryLog, int64, error) {
	q := s.DB.Model(&model.QueryLog{})
	if f.Username != "" {
		q = q.Where("username = ?", f.Username)
	}
	if f.UserLike != "" {
		p := "%" + likeEscape(f.UserLike) + "%"
		q = q.Where("(username LIKE ? ESCAPE '!' OR user_display LIKE ? ESCAPE '!')", p, p)
	}
	if f.Instance != "" {
		q = q.Where("instance_name = ?", f.Instance)
	}
	if f.Start != nil {
		q = q.Where("create_time >= ?", *f.Start)
	}
	if f.End != nil {
		q = q.Where("create_time < ?", *f.End)
	}
	for _, k := range f.Keywords {
		q = q.Where("sqllog LIKE ? ESCAPE '!'", "%"+likeEscape(k)+"%")
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.QueryLog
	err := q.Order("id DESC").Offset(f.Offset).Limit(f.Limit).Find(&rows).Error
	return rows, total, err
}

// QueryLogUsers 按用户汇总执行过查询的次数，次数多的在前。
func (s *Store) QueryLogUsers() ([]model.QueryLogUser, error) {
	var out []model.QueryLogUser
	err := s.DB.Model(&model.QueryLog{}).Select("username, MAX(user_display) AS user_display, COUNT(*) AS count").
		Group("username").Order("count DESC").Limit(1000).Scan(&out).Error
	return out, err
}

func (s *Store) GetInstance(id uint) (*model.Instance, error) {
	var inst model.Instance
	if err := s.DB.Preload("Tunnel").Preload("Tags").First(&inst, id).Error; err != nil {
		return nil, err
	}
	return &inst, nil
}

// EngineConn 解密凭据，构造连接信息。
// 实例配置了 SSH 隧道（tunnel_id）时，Host/Port 改写为本机的隧道转发器地址（见 internal/sshtunnel），
// 所以所有走 EngineConn 的模块（查询、工单、导出、归档、分区、数据字典、性能、巡检、慢日志、Binlog、Redis 采集…）自动经隧道连接；
// 没有配置隧道的实例原样返回。
func (s *Store) EngineConn(inst *model.Instance) engine.Conn {
	c := engine.Conn{
		Host:      inst.Host,
		Port:      inst.Port,
		User:      s.Mirage.Decrypt(inst.User),
		Password:  s.Mirage.Decrypt(inst.Password),
		DBName:    inst.DBName,
		Charset:   inst.Charset,
		SSL:       inst.IsSSL,
		VerifySSL: inst.VerifySSL,
		// 只有 Oracle 用：连接的服务名 / SID
		ServiceName: inst.ServiceNameValue(),
		SID:         inst.SIDValue(),
	}
	if sshtunnel.Enabled(inst) {
		c = sshtunnel.NewGormResolver(s.DB, s.Mirage.Decrypt).Apply(inst, c)
	}
	return c
}

// GetUserByName 按用户名取用户（登录用）。
func (s *Store) GetUserByName(name string) (*model.User, error) {
	var u model.User
	if err := s.DB.Where("username = ?", name).First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// HasButton 判断用户是否拥有某个按钮（按钮权限解析器：权限组授予 ∪ 单独授权，再扣掉单独禁用；基础按钮对启用的用户开放）。
// 超级用户由调用方先行判断。Open 时已装配解析器（internal/btnperm）；没有解析器（测试 / 演示的内存仓库不走这里）时一律没有权限。
func (s *Store) HasButton(userID uint, code string) (bool, error) {
	if r := btnperm.Default(); r != nil {
		return r.HasButton(userID, code)
	}
	return false, nil
}

// Buttons 返回按钮权限检查器（server.HasButton 用）；没有装配时为 nil。
func (s *Store) Buttons() btnperm.Checker {
	if r := btnperm.Default(); r != nil {
		return r
	}
	return nil
}

// AllowedButtons 一次取出用户拥有的全部按钮码（规则同 HasButton）。超级用户由调用方先行判断。
// 页面需要同时判断多个按钮时用它，避免逐个查询。
func (s *Store) AllowedButtons(userID uint) (map[string]bool, error) {
	if r := btnperm.Default(); r != nil {
		return r.AllowedButtons(userID)
	}
	return map[string]bool{}, nil
}

// UserInstanceIDs 返回用户通过资源组可访问的实例 ID（对应 user_instances 的非管理员分支）。
func (s *Store) UserInstanceIDs(userID uint) (map[uint]bool, error) {
	var ids []uint
	err := s.DB.Raw(`
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

// ValidQueryPrivileges 返回用户在某实例上当前有效的查询权限（未删除、未过期），按 privilege_id 升序。
func (s *Store) ValidQueryPrivileges(username string, instanceID uint) ([]model.QueryPrivilege, error) {
	var out []model.QueryPrivilege
	err := s.DB.Where("user_name = ? AND instance_id = ? AND is_deleted = 0 AND valid_date >= ?",
		username, instanceID, time.Now().Format("2006-01-02")).Order("privilege_id").Find(&out).Error
	return out, err
}

// ActiveMaskingColumns 返回某实例下所有激活的脱敏字段配置（dbt_masking_policy）。表还没建（没执行
// deploy/sql/230_masking_policy.sql）时返回空。
func (s *Store) ActiveMaskingColumns(instanceID uint) ([]model.DataMaskingColumn, error) {
	var pol []model.MaskingPolicy
	if err := s.DB.Where("instance_id = ? AND active = 1", instanceID).Order("id").Find(&pol).Error; err != nil {
		var me *gomysql.MySQLError
		if errors.As(err, &me) && me.Number == 1146 {
			return nil, nil
		}
		return nil, err
	}
	out := make([]model.DataMaskingColumn, 0, len(pol))
	for _, p := range pol {
		out = append(out, model.DataMaskingColumn{
			PolicyID: p.ID, InstanceID: p.InstanceID, Active: true,
			TableSchema: p.ScopeDB, TableName_: p.ScopeTable, ColumnName: p.FieldPath,
			Kind: p.RuleKind, KeepPrefix: p.KeepPrefix, KeepSuffix: p.KeepSuffix, HideGroup: p.HideGroup, RuleRegex: p.RuleRegex,
		})
	}
	return out, nil
}

// UndecryptableConfigItems 返回 dbt_setting 里“密文用当前密钥解不开”的设置键（只返回键名，不返回值）。
// 这样的项读出来按“未设置”处理：开关类的项回落到默认值，连接类的项缺失。
func (s *Store) UndecryptableConfigItems() ([]string, error) {
	return s.Settings().UndecryptableKeys(), nil
}

// UndecryptableInstances 返回账号或密码“看起来是密文、但用当前密钥解不开”的实例名（只返回名字，不返回值）。
// 这样的实例连不上目标库：服务拿去登录的“账号 / 密码”是密文原文（Decrypt 解不开时原样返回，为了兼容明文）。
// 典型原因：库里的账号 / 密码是用另一把密钥加密的，与当前部署使用的密钥不一致。
func (s *Store) UndecryptableInstances() ([]string, error) {
	var rows []model.Instance
	if err := s.DB.Find(&rows).Error; err != nil {
		return nil, err
	}
	return undecryptableInstanceNames(s.Mirage, rows), nil
}

// undecryptableInstanceNames 是 UndecryptableInstances 的筛选逻辑（拆出来便于不连库测试）：名字去重、升序。
func undecryptableInstanceNames(m *crypto.Mirage, rows []model.Instance) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if (m.Undecryptable(r.User) || m.Undecryptable(r.Password)) && !seen[r.InstanceName] {
			seen[r.InstanceName] = true
			out = append(out, r.InstanceName)
		}
	}
	sort.Strings(out)
	return out
}

// GormDB / MirageCrypto 让各功能模块（internal/<模块>）直接使用元数据库句柄与字段加解密器。
func (s *Store) GormDB() *gorm.DB             { return s.DB }
func (s *Store) MirageCrypto() *crypto.Mirage { return s.Mirage }
