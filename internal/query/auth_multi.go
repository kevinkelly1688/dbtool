// @author: kevinkelly
// @license: Apache Licence
// @file: auth_multi.go
// @time: 2026/10/28

package query

import (
	"context"
	"fmt"
	"strings"

	"dbtool/internal/model"
	"dbtool/internal/sqlx"
)

// PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦的库表级授权。
//
// 与 MySQL 系不同：这几类的库名、表名、集合名区分大小写，所以权限比较是精确匹配（库权限的 db_name 可以是 "*"；Oracle / 达梦没加引号的名字是大写）；
// 语句涉及的对象不是由 goInception / 词法解析给出，而是各引擎自己的静态检查结果（Checked.Tables / Collections / Databases），
// 由 prepareMulti 整理成 Targets 交给授权器。

// Targets 是一条语句要访问的对象。
type Targets struct {
	DB string
	// Objects 是语句涉及的表 / 集合（Schema=库名，Name=表名；PostgreSQL 的 Name 为 schema.table）。已排除可免权限的系统对象。
	Objects []sqlx.TableRef
	// RequireDB 非空表示必须有 DB 的库级权限，其值是无权限时的提示（Mongo 的库级语句、PG 的 NeedAllPriv）。
	RequireDB string
	// Structure 为 true 表示结构类查询（SHOW / DESCRIBE / EXISTS）：不校验表权限，行数上限按管理员上限；Dbs 里的库要通过实例的库名过滤规则。
	Structure bool
	Dbs       []string
}

// TargetAuthorizer 由授权器可选实现；PrivAuthorizer 实现了它。没有实现时（测试替身）退回按语句文本的 AuthorizeQuery。
type TargetAuthorizer interface {
	AuthorizeTargets(ctx context.Context, p Principal, inst *model.Instance, t Targets, adminLimit int) (Decision, error)
}

// mysqlTableLevel 报告该类型是否按“库权限或表权限”逐表校验：MySQL 与它的协议系（PolarDB、PolarDB-X、SelectDB、Doris、ADB）。
// 申请页对它们都能申请表权限，查询时必须认，否则会出现“批了却用不上”。
func mysqlTableLevel(dbType string) bool {
	switch dbType {
	case "mysql", "polardb", "polardb-x", "polardbx", "selectdb", "doris", "adb":
		return true
	}
	return false
}

// tableLevelType 判断该类型是否支持表级查询授权。
func tableLevelType(dbType string) bool { return mysqlTableLevel(dbType) || isMulti(dbType) }

// tableExtractorFor 选取语句涉及表的解析方式：能用 goInception 语法树的类型（与脱敏一致，见 usesSyntaxTree）走授权器配置的
// 解析器（配置了 goInception 就精确解析）；其余 MySQL 协议系（PolarDB-X、SelectDB、Doris）没有在真实实例上验证过 goInception，
// 用词法解析——词法解析宁多勿漏，只会多要权限，不会少要。
func (a *PrivAuthorizer) tableExtractorFor(dbType string) TableExtractor {
	if usesSyntaxTree(dbType) {
		return a.ext
	}
	return LexicalExtractor{}
}

// sameDBName 比较权限里的库名与请求的库名：新类型区分大小写，其余类型不区分（与 MySQL 排序规则一致）。
func sameDBName(dbType, a, b string) bool {
	if isMulti(dbType) {
		return a == b
	}
	return strings.EqualFold(a, b)
}

func dbPrivExact(privs []model.QueryPrivilege, db string) (int, bool) {
	for _, p := range privs {
		if p.PrivType == 1 && (p.DBName == "*" || p.DBName == db) {
			return p.LimitNum, true
		}
	}
	return 0, false
}

// sameObjName 比较权限里的表名与语句涉及的表名。PostgreSQL 的表名是 schema.table，权限里不带点的名字视为 public 下的表。
func sameObjName(dbType, priv, obj string) bool {
	if priv == obj {
		return true
	}
	return dbType == dbTypePG && !strings.Contains(priv, ".") && "public."+priv == obj
}

func tbPrivMulti(dbType string, privs []model.QueryPrivilege, db, obj string) (int, bool) {
	for _, p := range privs {
		if p.PrivType == 2 && p.DBName == db && sameObjName(dbType, p.TbName, obj) {
			return p.LimitNum, true
		}
	}
	return 0, false
}

// AuthorizeTargets 实现 TargetAuthorizer：管理员 / 资源组管理员放行；其余用户每个对象都要有库级或表级权限，
// 生效行数取各对象 min(库 limit, 表 limit) 的最小值。
func (a *PrivAuthorizer) AuthorizeTargets(_ context.Context, p Principal, inst *model.Instance, t Targets, adminLimit int) (Decision, error) {
	if dec, done, err := a.preAuthorize(p, inst, adminLimit); done {
		return dec, err
	}
	if t.Structure {
		if len(t.Dbs) > 0 {
			kept, err := filterDBs(t.Dbs, inst.ShowDBNameRegex, inst.DeniedDBNameRegex)
			if err != nil {
				return Decision{}, err
			}
			if len(kept) != len(uniqStrings(t.Dbs)) {
				return Decision{}, noPriv("该数据库不在你可访问的范围内", t.DB, "")
			}
		}
		return Decision{MaxRows: adminLimit, Source: "结构查询", PrivCheck: true}, nil
	}
	privs, err := a.repo.ValidQueryPrivileges(p.Username, inst.ID)
	if err != nil {
		return Decision{}, err
	}
	if t.RequireDB != "" {
		lim, ok := dbPrivExact(privs, t.DB)
		if !ok {
			return Decision{}, noPriv(t.RequireDB, t.DB, "")
		}
		return Decision{MaxRows: lim, Source: fmt.Sprintf("%s 库权限", t.DB), PrivCheck: true}, nil
	}
	if len(t.Objects) == 0 {
		return Decision{MaxRows: noTableLimit, Source: "无表查询", PrivCheck: true}, nil
	}
	best, source := 0, ""
	seen := map[sqlx.TableRef]bool{}
	for _, r := range t.Objects {
		if seen[r] {
			continue
		}
		seen[r] = true
		dbLimit, hasDB := dbPrivExact(privs, r.Schema)
		tbLimit, hasTB := tbPrivMulti(inst.DBType, privs, r.Schema, r.Name)
		if !hasDB && !hasTB {
			return Decision{}, noPriv(fmt.Sprintf("你无 %s.%s 表的查询权限，请先申请。", r.Schema, r.Name), r.Schema, r.Name)
		}
		lim := minPriv(dbLimit, hasDB, tbLimit, hasTB)
		var src string
		switch {
		case hasDB && hasTB:
			src = fmt.Sprintf("%s.%s 的库/表权限", r.Schema, r.Name)
		case hasTB:
			src = fmt.Sprintf("%s.%s 表权限", r.Schema, r.Name)
		default:
			src = fmt.Sprintf("%s 库权限", r.Schema)
		}
		if best == 0 || (lim > 0 && lim < best) {
			best, source = lim, src
		}
	}
	return Decision{MaxRows: best, Source: source, PrivCheck: true}, nil
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// HasObject 判断用户在该库对 name 表（集合）是否有查询权限；新类型按精确匹配，PostgreSQL 的 public 表允许权限里省略 schema。
func (p PolicyInfo) HasObject(dbType, name string) bool {
	switch p.Mode {
	case "admin", "group":
		return true
	case "priv":
		if p.AllTables {
			return true
		}
		for _, t := range p.Tables {
			if sameObjName(dbType, t, name) {
				return true
			}
		}
	}
	return false
}
