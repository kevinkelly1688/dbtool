// @author: kevinkelly
// @license: Apache Licence
// @file: auth.go
// @time: 2026/10/28

package query

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbtool/internal/goinception"
	"dbtool/internal/masking"
	"dbtool/internal/model"
	"dbtool/internal/settings"
	"dbtool/internal/sqlx"
)

// PrivRepo 是权限校验需要的数据访问：权限码、用户可见实例、查询权限。
type PrivRepo interface {
	HasButton(userID uint, code string) (bool, error)
	UserInstanceIDs(userID uint) (map[uint]bool, error)
	ValidQueryPrivileges(username string, instanceID uint) ([]model.QueryPrivilege, error)
}

// 权限码。
const (
	permMenuQuery            = "query.view"      // 打开在线查询
	permQuerySubmit          = "query.run"       // 提交 SQL 查询
	permQueryAllInstances    = "query.admin_run" // 查看 / 查询全部实例，不受库表授权限制
	permQueryResourceGroupIn = "query.group_run" // 资源组内实例按管理员行数上限查询
)

// 没有任何库表的查询（SELECT 1、SELECT VERSION()）的行数上限。
const noTableLimit = 100

// ---- 语句涉及的库表解析 ----

// TableExtractor 解析一条语句引用了哪些表。缺省库名用 db 补全。
type TableExtractor interface {
	Tables(ctx context.Context, inst *model.Instance, db, stmt string) ([]sqlx.TableRef, error)
}

// LexicalExtractor 是词法级的保守实现，用于没有 goInception 的环境（见 sqlx.ExtractTables）。
type LexicalExtractor struct{}

func (LexicalExtractor) Tables(_ context.Context, _ *model.Instance, db, stmt string) ([]sqlx.TableRef, error) {
	refs := sqlx.ExtractTables(stmt)
	for i := range refs {
		if refs[i].Schema == "" {
			refs[i].Schema = db
		}
	}
	return refs, nil
}

// AutoExtractor 按系统配置选择解析器：配置了 goinception.host 就用 goInception（出错即拒绝，
// 不会悄悄降级到较弱的词法解析）；未配置时使用词法解析。
type AutoExtractor struct {
	repo Repo
	// NewClient 可替换，便于测试。
	NewClient func(goinception.Config) InceptionClient
	warned    sync.Once
	// Strict 为 true 时，未配置 goInception 直接拒绝（生产环境应开启），而不是降级为词法解析。
	Strict bool
}

// InceptionClient 是 goInception 客户端的抽象（真实实现见 internal/goinception）。
type InceptionClient interface {
	QueryTree(ctx context.Context, t goinception.Target, db, stmt string) (any, error)
	SelectList(ctx context.Context, t goinception.Target, db, stmt string) ([]masking.SelectColumn, error)
}

func NewAutoExtractor(repo Repo) *AutoExtractor {
	return &AutoExtractor{repo: repo, NewClient: func(c goinception.Config) InceptionClient { return goinception.New(c) }}
}

func (a *AutoExtractor) Tables(ctx context.Context, inst *model.Instance, db, stmt string) ([]sqlx.TableRef, error) {
	host, ok := a.repo.ConfigValue(settings.KeyGoInceptionHost)
	if !ok {
		if a.Strict {
			slog.Error("REQUIRE_GOINCEPTION 已开启，但系统设置里未配置 goinception.host")
			return nil, newErr(500, "parse_failed", "未配置 goInception，无法校验查询语句权限，请联系管理员。")
		}
		a.warned.Do(func() {
			slog.Warn("未配置 goinception.host，库表权限校验使用词法解析（仅建议在开发环境使用）")
		})
		return LexicalExtractor{}.Tables(ctx, inst, db, stmt)
	}
	cfg := goinception.Config{Host: host}
	if v, ok := a.repo.ConfigValue(settings.KeyGoInceptionPort); ok {
		cfg.Port, _ = strconv.Atoi(strings.TrimSpace(v))
	}
	cfg.User, _ = a.repo.ConfigValue(settings.KeyGoInceptionUser)
	cfg.Password, _ = a.repo.ConfigValue(settings.KeyGoInceptionPassword)

	conn := a.repo.EngineConn(inst)
	tree, err := a.NewClient(cfg).QueryTree(ctx, goinception.Target{Host: conn.Host, Port: conn.Port, User: conn.User, Password: conn.Password}, db, stmt)
	if err != nil {
		slog.Error("goInception 解析语句失败", "instance", inst.InstanceName, "err", err)
		return nil, newErr(500, "parse_failed", "无法校验查询语句权限，请联系管理员。")
	}
	return goinception.TableRefs(tree, db), nil
}

// ErrNoInception 表示没有配置 goInception（且未开启严格模式），调用方可降级为较弱的列名脱敏。
var ErrNoInception = errors.New("未配置 goInception")

// SelectList 让 goInception 给出语句每个结果列的来源，用于数据脱敏。
// 未配置 goInception：严格模式返回业务错误，否则返回 ErrNoInception。
func (a *AutoExtractor) SelectList(ctx context.Context, inst *model.Instance, db, stmt string) ([]masking.SelectColumn, error) {
	host, ok := a.repo.ConfigValue(settings.KeyGoInceptionHost)
	if !ok {
		if a.Strict {
			return nil, newErr(500, "masking_error", "未配置 goInception，无法进行数据脱敏，请联系管理员。")
		}
		return nil, ErrNoInception
	}
	cfg := goinception.Config{Host: host}
	if v, ok := a.repo.ConfigValue(settings.KeyGoInceptionPort); ok {
		cfg.Port, _ = strconv.Atoi(strings.TrimSpace(v))
	}
	cfg.User, _ = a.repo.ConfigValue(settings.KeyGoInceptionUser)
	cfg.Password, _ = a.repo.ConfigValue(settings.KeyGoInceptionPassword)
	conn := a.repo.EngineConn(inst)
	return a.NewClient(cfg).SelectList(ctx, goinception.Target{Host: conn.Host, Port: conn.Port, User: conn.User, Password: conn.Password}, db, stmt)
}

// ---- 授权器 ----

// PrivAuthorizer 实现查询权限规则：
//
//   - superuser 或 query.admin_run：不做库表校验，行数上限取 query.admin_row_limit；
//   - query.group_run：在其资源组内且开启查询（HasReadTag）的实例同上；
//   - 其他用户：MySQL 与它的协议系（PolarDB、PolarDB-X、SelectDB、Doris、ADB）逐表要求库权限或表权限，SQL Server 与 Redis 要求库权限；
//   - 生效行数 = 涉及各表（库/表权限取小）的最小值，再与请求值取小。
//
// 普通用户只允许结构类 SHOW；禁止 load_file/sleep/benchmark 等函数。
type PrivAuthorizer struct {
	repo Repo
	ext  TableExtractor
	now  func() time.Time
}

func NewPrivAuthorizer(repo Repo, ext TableExtractor) *PrivAuthorizer {
	if ext == nil {
		ext = NewAutoExtractor(repo)
	}
	// 解析结果缓存几秒：SQL 防火墙与库表权限校验解析的是同一条语句，共用这个解析器就不会调用两次 goInception。
	if _, ok := ext.(*memoExtractor); !ok {
		ext = newMemoExtractor(ext)
	}
	return &PrivAuthorizer{repo: repo, ext: ext, now: time.Now}
}

func (a *PrivAuthorizer) isAdmin(p Principal) (bool, error) {
	if p.IsSuperuser {
		return true, nil
	}
	return a.repo.HasButton(p.ID, permQueryAllInstances)
}

func (a *PrivAuthorizer) CanAccessPage(p Principal) bool {
	if p.IsSuperuser {
		return true
	}
	ok, err := a.repo.HasButton(p.ID, permMenuQuery)
	return err == nil && ok
}

func (a *PrivAuthorizer) FilterInstances(p Principal, list []model.Instance) ([]model.Instance, error) {
	if admin, err := a.isAdmin(p); err != nil || admin {
		return list, err
	}
	ids, err := a.repo.UserInstanceIDs(p.ID)
	if err != nil {
		return nil, err
	}
	var out []model.Instance
	for _, i := range list {
		if ids[i.ID] {
			out = append(out, i)
		}
	}
	return out, nil
}

func (a *PrivAuthorizer) CanUseInstance(p Principal, inst *model.Instance) error {
	list, err := a.FilterInstances(p, []model.Instance{*inst})
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return newErr(403, "no_privilege", "你所在组未关联该实例")
	}
	return nil
}

func hasActiveTag(inst *model.Instance, codes ...string) bool {
	for _, t := range inst.Tags {
		if !t.Active {
			continue
		}
		for _, c := range codes {
			if t.TagCode == c {
				return true
			}
		}
	}
	return false
}

// HasReadTag 判断实例是否开启了“查询”：带启用的 can_read 标签。
func HasReadTag(inst *model.Instance) bool { return hasActiveTag(inst, "can_read") }

func noPriv(msg string, db, table string) *Error {
	e := newErr(403, "no_privilege", msg)
	if db != "" {
		e.Data = map[string]any{"database": db, "table": table}
	}
	return e
}

// preAuthorize 是各类型共用的前置判断：能否使用实例、是否有提交查询的权限、管理员 / 资源组管理员直接放行。
// done 为 true 表示已有结论（放行或拒绝），调用方原样返回；否则继续按库表权限校验。
func (a *PrivAuthorizer) preAuthorize(p Principal, inst *model.Instance, adminLimit int) (dec Decision, done bool, err error) {
	if err := a.CanUseInstance(p, inst); err != nil {
		return Decision{}, true, err
	}
	if !p.IsSuperuser {
		ok, err := a.repo.HasButton(p.ID, permQuerySubmit)
		if err != nil {
			return Decision{}, true, err
		}
		if !ok {
			return Decision{}, true, noPriv("你没有提交 SQL 查询的权限，请联系管理员。", "", "")
		}
	}
	admin, err := a.isAdmin(p)
	if err != nil {
		return Decision{}, true, err
	}
	if admin {
		return Decision{MaxRows: adminLimit, Source: "管理员上限"}, true, nil
	}
	if ok, err := a.repo.HasButton(p.ID, permQueryResourceGroupIn); err != nil {
		return Decision{}, true, err
	} else if ok && HasReadTag(inst) {
		return Decision{MaxRows: adminLimit, Source: "资源组管理员上限"}, true, nil
	}
	return Decision{}, false, nil
}

func (a *PrivAuthorizer) AuthorizeQuery(ctx context.Context, p Principal, inst *model.Instance, db, stmt string, adminLimit int) (Decision, error) {
	if dec, done, err := a.preAuthorize(p, inst, adminLimit); done {
		return dec, err
	}

	if isRedis(inst.DBType) { // Redis 只有库级权限（db 编号），见 redis.go
		return a.authorizeRedis(p, inst, db)
	}
	if isMulti(inst.DBType) { // PostgreSQL / MongoDB / ClickHouse / Oracle / 达梦必须经 AuthorizeTargets（按语句涉及的对象校验），不走按语句文本的校验
		return Decision{}, newErr(501, "unsupported_engine", "该类型的实例暂不支持这种授权方式")
	}

	kw := sqlx.QueryKind(stmt)
	if isMSSQL(inst.DBType) {
		kw = "select"
	}
	switch kw {
	case "explain":
		// EXPLAIN 不校验库表权限（只返回执行计划，不返回数据）。
		return Decision{MaxRows: adminLimit, Source: "执行计划", PrivCheck: true}, nil
	case "show":
		// DESC / DESCRIBE <表> 等价 SHOW COLUMNS，同属结构查询。
		if sqlx.FirstKeyword(stmt) == "show" && !sqlx.IsSafeShow(stmt) {
			return Decision{}, noPriv("该 SHOW 语句仅管理员可执行。普通用户可使用 SHOW DATABASES/TABLES/COLUMNS/INDEX/CREATE TABLE。", "", "")
		}
		return Decision{MaxRows: adminLimit, Source: "结构查询", PrivCheck: true}, nil
	}
	if fns := sqlx.DangerousFunctions(stmt); len(fns) > 0 {
		return Decision{}, newErr(400, "bad_query", fmt.Sprintf("不允许使用函数 %s", strings.Join(fns, "、")))
	}

	privs, err := a.repo.ValidQueryPrivileges(p.Username, inst.ID)
	if err != nil {
		return Decision{}, err
	}

	// 解析语句涉及的库表：MySQL 协议系用精确的表（能用 goInception 的走它，其余词法解析），SQL Server 与其余引擎只看库。
	var refs []sqlx.TableRef
	if mysqlTableLevel(inst.DBType) {
		refs, err = a.tableExtractorFor(inst.DBType).Tables(ctx, inst, db, stmt)
		if err != nil {
			return Decision{}, err
		}
	} else if isMSSQL(inst.DBType) {
		refs, _ = mssqlTables(stmt, db) // 一/二部分名称属于当前库，三部分名称取其库名；按库校验
	} else {
		refs, _ = LexicalExtractor{}.Tables(ctx, inst, db, stmt)
	}
	if len(refs) == 0 {
		return Decision{MaxRows: noTableLimit, Source: "无表查询", PrivCheck: true}, nil
	}

	tableLevel := mysqlTableLevel(inst.DBType)
	best, source := 0, ""
	seen := map[string]bool{}
	for _, r := range refs {
		key := strings.ToLower(r.Schema)
		if tableLevel {
			key += "." + strings.ToLower(r.Name)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		dbLimit, hasDB := dbPriv(privs, r.Schema)
		tbLimit, hasTB := 0, false
		if tableLevel {
			tbLimit, hasTB = tbPriv(privs, r.Schema, r.Name)
		}
		if !hasDB && !hasTB {
			if tableLevel {
				return Decision{}, noPriv(fmt.Sprintf("你无 %s.%s 表的查询权限，请先申请。", r.Schema, r.Name), r.Schema, r.Name)
			}
			return Decision{}, noPriv(fmt.Sprintf("你无 %s 数据库的查询权限，请先申请。", r.Schema), r.Schema, "")
		}
		lim, src := minPriv(dbLimit, hasDB, tbLimit, hasTB), ""
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

func minPriv(dbLimit int, hasDB bool, tbLimit int, hasTB bool) int {
	switch {
	case hasDB && hasTB:
		if tbLimit < dbLimit {
			return tbLimit
		}
		return dbLimit
	case hasTB:
		return tbLimit
	default:
		return dbLimit
	}
}

// dbPriv 查库权限（库名相同或 '*'），取 privilege_id 最小的一条。
func dbPriv(privs []model.QueryPrivilege, db string) (int, bool) {
	for _, p := range privs {
		if p.PrivType == 1 && (p.DBName == "*" || strings.EqualFold(p.DBName, db)) {
			return p.LimitNum, true
		}
	}
	return 0, false
}

// tbPriv 查表权限。库表名比较不区分大小写，与 MySQL 默认排序规则一致。
func tbPriv(privs []model.QueryPrivilege, db, tb string) (int, bool) {
	for _, p := range privs {
		if p.PrivType == 2 && strings.EqualFold(p.DBName, db) && strings.EqualFold(p.TbName, tb) {
			return p.LimitNum, true
		}
	}
	return 0, false
}

// ---- 库名过滤 ----

// filterDBs 按实例配置过滤库列表：先保留匹配 show_db_name_regex 的，再去掉匹配 denied_db_name_regex 的。
// 正则从字符串开头匹配。正则非法时返回错误而不是放行。
func filterDBs(list []string, showRegex, deniedRegex string) ([]string, error) {
	compile := func(expr string) (func(string) bool, error) {
		if strings.TrimSpace(expr) == "" {
			return nil, nil
		}
		re, err := compileAnchored(expr)
		if err != nil {
			return nil, newErr(500, "bad_config", fmt.Sprintf("实例的库名过滤正则不合法：%s", expr))
		}
		return re, nil
	}
	show, err := compile(showRegex)
	if err != nil {
		return nil, err
	}
	denied, err := compile(deniedRegex)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, d := range list {
		if show != nil && !show(d) {
			continue
		}
		if denied != nil && denied(d) {
			continue
		}
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

// compileAnchored 编译正则并要求从字符串开头匹配。
func compileAnchored(expr string) (func(string) bool, error) {
	re, err := regexp.Compile(`\A(?:` + expr + `)`)
	if err != nil {
		return nil, err
	}
	return re.MatchString, nil
}
