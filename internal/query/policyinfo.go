// @author: kevinkelly
// @license: Apache Licence
// @file: policyinfo.go
// @time: 2026/10/28

package query

import (
	"context"
	"strings"
	"time"

	"dbtool/internal/model"
)

// PolicyInfo 是“某用户在某实例 × 库上的查询限制”，展示在查询页的限制提示条里（不参与任何放行判断，放行仍由 AuthorizeQuery 决定）。
type PolicyInfo struct {
	Mode          string   `json:"mode"`                     // admin / group / priv / none / unknown
	Source        string   `json:"source"`                   // 限制来源说明
	MaxRows       int      `json:"max_rows"`                 // 行数上限（priv 模式为各授权里最大的一个）
	MinRows       int      `json:"min_rows"`                 // priv 模式下各授权里最小的上限
	AllTables     bool     `json:"all_tables"`               // 有该库的库级权限（全部表）
	Tables        []string `json:"tables"`                   // 仅有表级权限时的表名
	Expire        string   `json:"expire,omitempty"`         // priv：最早到期的授权日期
	ExpireDays    int      `json:"expire_days"`              // 距离到期天数
	LatestExpired string   `json:"latest_expired,omitempty"` // none：该库最近一条已过期授权的到期日（取不到为空）
	TableLevel    bool     `json:"table_level"`              // 该类型实例是否支持表级授权
}

// HasTable 判断用户对 name 表是否有查询权限。
func (p PolicyInfo) HasTable(name string) bool {
	switch p.Mode {
	case "admin", "group":
		return true
	case "priv":
		if p.AllTables {
			return true
		}
		for _, t := range p.Tables {
			if strings.EqualFold(t, name) {
				return true
			}
		}
	}
	return false
}

// PolicyProvider 由授权器可选实现。
type PolicyProvider interface {
	PolicyInfo(ctx context.Context, p Principal, inst *model.Instance, db string, adminLimit int) (PolicyInfo, error)
}

// ExpiredPrivRepo 由仓库可选实现：返回用户在实例上已过期但未删除的查询权限（用于提示“权限已于 X 过期”）。
type ExpiredPrivRepo interface {
	ExpiredQueryPrivileges(username string, instanceID uint) ([]model.QueryPrivilege, error)
}

func day(t time.Time) string { return t.Format("2006-01-02") }

// PolicyInfo 实现 PolicyProvider。规则与 AuthorizeQuery 一致：管理员 / 资源组管理员按 query.admin_row_limit，
// 其余按有效授权（库权限，或 MySQL 协议系与 PostgreSQL / MongoDB / ClickHouse 的表权限）。
func (a *PrivAuthorizer) PolicyInfo(_ context.Context, p Principal, inst *model.Instance, db string, adminLimit int) (PolicyInfo, error) {
	tableLevel := tableLevelType(inst.DBType)
	info := PolicyInfo{Tables: []string{}, TableLevel: tableLevel}
	admin, err := a.isAdmin(p)
	if err != nil {
		return info, err
	}
	if admin {
		info.Mode, info.Source, info.MaxRows = "admin", "管理员，不受库表授权限制", adminLimit
		return info, nil
	}
	if ok, err := a.repo.HasButton(p.ID, permQueryResourceGroupIn); err != nil {
		return info, err
	} else if ok && HasReadTag(inst) {
		info.Mode, info.Source, info.MaxRows = "group", "资源组内实例，免库表授权，按管理员行数上限", adminLimit
		return info, nil
	}
	privs, err := a.repo.ValidQueryPrivileges(p.Username, inst.ID)
	if err != nil {
		return info, err
	}
	var earliest time.Time
	for _, v := range privs {
		switch {
		case v.PrivType == 1 && (v.DBName == "*" || sameDBName(inst.DBType, v.DBName, db)):
			info.AllTables = true
		case v.PrivType == 2 && tableLevel && sameDBName(inst.DBType, v.DBName, db):
			info.Tables = append(info.Tables, v.TbName)
		default:
			continue
		}
		if info.MinRows == 0 || (v.LimitNum > 0 && v.LimitNum < info.MinRows) {
			info.MinRows = v.LimitNum
		}
		if v.LimitNum > info.MaxRows {
			info.MaxRows = v.LimitNum
		}
		if earliest.IsZero() || v.ValidDate.Before(earliest) {
			earliest = v.ValidDate
		}
	}
	if !info.AllTables && len(info.Tables) == 0 {
		info.Mode, info.Source = "none", "该库没有有效的查询权限"
		if er, ok := a.repo.(ExpiredPrivRepo); ok {
			if list, err := er.ExpiredQueryPrivileges(p.Username, inst.ID); err == nil {
				var latest time.Time
				for _, v := range list {
					if (v.PrivType == 1 && (v.DBName == "*" || sameDBName(inst.DBType, v.DBName, db))) || (v.PrivType == 2 && sameDBName(inst.DBType, v.DBName, db)) {
						if v.ValidDate.After(latest) {
							latest = v.ValidDate
						}
					}
				}
				if !latest.IsZero() {
					info.LatestExpired = day(latest)
				}
			}
		}
		return info, nil
	}
	info.Mode = "priv"
	info.Source = "按查询权限授予的行数上限"
	if !earliest.IsZero() {
		info.Expire = day(earliest)
		today, _ := time.ParseInLocation("2006-01-02", a.now().Format("2006-01-02"), time.Local)
		e, _ := time.ParseInLocation("2006-01-02", day(earliest), time.Local)
		info.ExpireDays = int(e.Sub(today).Hours() / 24)
	}
	return info, nil
}

// PolicyFor 返回当前用户在实例 × 库上的限制信息；授权器不支持（测试替身等）时返回 mode=unknown。
func (s *Service) PolicyFor(ctx context.Context, p Principal, instanceID uint, db string) (PolicyInfo, error) {
	inst, err := s.instance(p, instanceID)
	if err != nil {
		return PolicyInfo{}, err
	}
	pp, ok := s.auth.(PolicyProvider)
	if !ok {
		return PolicyInfo{Mode: "unknown", Tables: []string{}}, nil
	}
	return pp.PolicyInfo(ctx, p, inst, db, s.Policy().AdminLimit)
}
