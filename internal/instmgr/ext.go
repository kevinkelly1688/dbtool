// @author: kevinkelly
// @license: Apache Licence
// @file: ext.go
// @time: 2026/10/28

package instmgr

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"dbtool/internal/instext"
)

// 旁路表（dbt_*_ext 等）：不给 dbt_sql_instance / dbt_instance_account / dbt_param_template 加列，
// 实例的停用状态与环境、账号的使用人 / 用途 / 到期日、参数模板的推荐值另存 dbtool 自己的表。
// 对应脚本 deploy/sql/070_instance_ext.sql、071_account_reg.sql、072_param_template_ext.sql；表不存在时相关功能优雅降级。

// ExtRepo 是旁路表的访问。Repo 的实现（store）同时实现它；没实现时（单测里的旧假仓库）这些功能整体关闭。
type ExtRepo interface {
	// ---- dbt_instance_ext ----
	InstanceExtReady() bool
	// InstanceExts 返回这些实例的扩展记录（没有记录的不在结果里）。
	InstanceExts(ids []uint) (map[uint]instext.Row, error)
	SetInstanceDisabled(id uint, disabled bool, by string, at time.Time) error
	SetInstanceEnv(id uint, env string, at time.Time) error
	// InstanceStats 统计可见范围（scope 为 nil 表示全部）内的实例数。
	InstanceStats(scope map[uint]bool) (InstanceStats, error)

	// ---- dbt_account_reg ----
	AccountRegReady() bool
	AccountRegs(instanceID uint) (map[string]AccountReg, error) // 键 user@host
	SaveAccountReg(instanceID uint, r AccountReg, by string, at time.Time) error
	DeleteAccountReg(instanceID uint, user, host string) error

	// ---- dbt_param_template_ext ----
	TemplateExtReady() bool
	TemplateRecommended() (map[uint]string, error)
	SaveTemplateRecommended(templateID uint, value, by string, at time.Time) error

	// ---- 跨实例的参数修改历史 ----
	ListAllParamHistory(scope map[uint]bool, q string, offset, limit int) ([]ParamHistoryRow, int64, error)
}

// InstanceStats 是实例列表顶部统计条里能用 SQL 一次算出来的部分（健康状态要探测，由页面异步补）。
type InstanceStats struct {
	Total      int64 `json:"total"`
	Disabled   int64 `json:"disabled"`
	Collecting int64 `json:"collecting"` // 慢日志采集中（未停用）
}

// AccountReg 是账号的使用人 / 用途 / 到期日登记。ExpireDate 为 YYYY-MM-DD，空表示不限期。
type AccountReg struct {
	User         string `json:"user"`
	Host         string `json:"host"`
	Owner        string `json:"owner"`
	OwnerDisplay string `json:"owner_display"`
	Purpose      string `json:"purpose"`
	ExpireDate   string `json:"expire_date"`
}

func (a AccountReg) empty() bool {
	return a.Owner == "" && a.Purpose == "" && a.ExpireDate == ""
}

func regKey(user, host string) string { return user + "@" + host }

// errExtMissing 是旁路表没建时的业务错误（409：脚本执行后即可使用）。
func errExtMissing(what, script string) *Error {
	return conflict(fmt.Sprintf("%s需要先在元数据库执行 deploy/sql/%s（建表后无需重启，30 秒内生效）", what, script))
}

// instanceExtReady 旁路表是否可用。
func (s *Service) instanceExtReady() bool { return s.ext != nil && s.ext.InstanceExtReady() }

// extOf 取一批实例的扩展记录；任何错误都只记日志，按“没有记录”处理（扩展信息不能让主流程失败）。
func (s *Service) extOf(ids []uint) map[uint]instext.Row {
	if !s.instanceExtReady() || len(ids) == 0 {
		return map[uint]instext.Row{}
	}
	m, err := s.ext.InstanceExts(ids)
	if err != nil {
		slog.Warn("读取实例扩展信息失败", "err", err)
		return map[uint]instext.Row{}
	}
	return m
}

// rejectDisabled 停用的实例平台不再连接。
func (s *Service) rejectDisabled(id uint) error {
	if r, ok := s.extOf([]uint{id})[id]; ok && r.Disabled {
		return conflict("该实例已停用，平台不再连接它；如需使用请先在“实例列表”里启用")
	}
	return nil
}

// ---- 停用 / 启用 ----

// SetInstanceDisabled 停用或启用实例（实例级，区别于慢日志采集开关）。
func (s *Service) SetInstanceDisabled(c Caller, id uint, disabled bool) error {
	inst, err := s.visibleInstance(c, id)
	if err != nil {
		return err
	}
	if !s.instanceExtReady() {
		return errExtMissing("停用 / 启用实例", "070_instance_ext.sql")
	}
	cur := s.extOf([]uint{id})[id]
	if cur.Disabled == disabled {
		if disabled {
			return conflict("该实例已经是停用状态")
		}
		return conflict("该实例已经是启用状态")
	}
	if err := s.ext.SetInstanceDisabled(id, disabled, clipRunes(c.Username, 30), s.now()); err != nil {
		return err
	}
	action := "启用实例" // i18n-ignore 审计动作名，存库的枚举
	if disabled {
		action = "停用实例" // i18n-ignore 审计动作名，存库的枚举
	}
	s.audit(c, action, fmt.Sprintf("实例：%s，地址：%s:%d", instLabel(inst), inst.Host, inst.Port)) // i18n-ignore 审计详情，存库的文本不翻译
	return nil
}

// ---- 环境 ----

// EnvOption 是环境下拉的一项：标签是字典里的显示名（内置的“生产 / 测试”前端用 tp 翻译）；Prod 表示按生产对待。
type EnvOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Prod  bool   `json:"is_prod"`
}

// EnvOptions 返回环境字典（平台设置 › 环境设置）；字典不可用时是内置的生产 / 测试。
func EnvOptions() []EnvOption {
	defs := instext.EnvDefs()
	out := make([]EnvOption, 0, len(defs))
	for _, d := range defs {
		out = append(out, EnvOption{Value: d.Code, Label: d.Name, Prod: d.Prod})
	}
	return out
}

func validEnv(env string) error {
	if !instext.ValidEnv(env) {
		return bad("环境不在环境字典里（平台设置 › 环境设置）")
	}
	return nil
}

// ---- 账号登记扩展的校验 ----

// cleanReg 校验使用人 / 用途 / 到期日，返回规整后的登记（Owner 取库里的显示名）。
func (s *Service) cleanReg(user, host, owner, purpose, expire string) (AccountReg, error) {
	r := AccountReg{User: user, Host: host}
	var err error
	if r.Purpose, err = cleanText("用途", purpose, 255, false); err != nil {
		return r, err
	}
	owner = strings.TrimSpace(owner)
	if owner != "" {
		u, err := s.checkOwner(owner)
		if err != nil {
			return r, err
		}
		r.Owner, r.OwnerDisplay = u.Username, u.Display
	}
	expire = strings.TrimSpace(expire)
	if expire != "" {
		if _, err := time.Parse("2006-01-02", expire); err != nil {
			return r, bad("到期日格式应为 YYYY-MM-DD")
		}
		r.ExpireDate = expire
	}
	return r, nil
}

// expireState 把到期日换算成状态：expired 已过期 / soon 7 天内到期 / 空 正常。days 为剩余天数（已过期为负，不限期为 0）。
func expireState(expire string, now time.Time) (state string, days int) {
	if expire == "" {
		return "", 0
	}
	d, err := time.ParseInLocation("2006-01-02", expire, now.Location())
	if err != nil {
		return "", 0
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	days = int(d.Sub(today).Hours() / 24)
	switch {
	case days < 0:
		return "expired", days
	case days <= 7:
		return "soon", days
	}
	return "", days
}
