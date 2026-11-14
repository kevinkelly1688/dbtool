// @author: kevinkelly
// @license: Apache Licence
// @file: databases.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// DBItem 是数据库管理页的一行：库本身（来自目标实例）加平台登记的负责人与备注。
type DBItem struct {
	Name         string   `json:"db_name"`
	Charset      string   `json:"charset"`
	Collation    string   `json:"collation"`
	Grantees     []string `json:"grantees"`
	Saved        bool     `json:"saved"`
	Owner        string   `json:"owner"`
	OwnerDisplay string   `json:"owner_display"`
	Remark       string   `json:"remark"`
	Tables       int64    `json:"tables"`   // 表数（含视图）；读不到为 0
	SizeMB       float64  `json:"size_mb"`  // 数据 + 索引大小（MB）；读不到为 0
	StatsOK      bool     `json:"stats_ok"` // 表数 / 大小读到了
}

// ListDatabases 列出实例的库（不含系统库），并合并平台登记的负责人与备注。saved 为 true 时只返回已录入的。
func (s *Service) ListDatabases(ctx context.Context, c Caller, id uint, saved bool, q string) ([]DBItem, error) {
	scope := ""
	if saved {
		scope = "1"
	}
	return s.ListDatabasesScoped(ctx, c, id, scope, q)
}

// ListDatabasesScoped 同 ListDatabases；scope 取 "1" 只看已录入 / "0" 只看未录入 / 空 全部。
func (s *Service) ListDatabasesScoped(ctx context.Context, c Caller, id uint, scope, q string) ([]DBItem, error) {
	if scope != "" && scope != "0" && scope != "1" {
		return nil, bad("范围只能是 1（已录入）或 0（未录入）")
	}
	se, err := s.open(ctx, c, id, supportsDatabase, "数据库管理")
	if err != nil {
		return nil, err
	}
	defer se.close()
	r, err := se.query("SELECT schema_name AS schema_name, default_character_set_name AS db_charset, default_collation_name AS db_collation FROM information_schema.schemata WHERE schema_name NOT IN (" + sysSchemas + ") ORDER BY schema_name")
	if err != nil {
		return nil, wrapTarget(err)
	}
	// 授权账号：一次取全，失败（如 Doris 没有这张表）就留空。
	grantees := map[string][]string{}
	if gr, err := se.query("SELECT table_schema AS db, grantee AS grantee FROM information_schema.schema_privileges GROUP BY table_schema, grantee ORDER BY table_schema, grantee"); err == nil {
		for _, rec := range gr.Records() {
			grantees[rec.Str("db")] = append(grantees[rec.Str("db")], rec.Str("grantee"))
		}
	}
	sizes := s.dbSizes(se)
	savedList, err := s.repo.SavedDatabases(id)
	if err != nil {
		return nil, err
	}
	savedBy := map[string]DatabaseRecord{}
	for _, d := range savedList {
		savedBy[d.DBName] = d
	}
	q = strings.ToLower(strings.TrimSpace(q))
	out := []DBItem{}
	for _, rec := range r.Records() {
		it := DBItem{Name: rec.Str("schema_name"), Charset: rec.Str("db_charset"), Collation: rec.Str("db_collation"), Grantees: grantees[rec.Str("schema_name")]}
		if it.Grantees == nil {
			it.Grantees = []string{}
		}
		if d, ok := savedBy[it.Name]; ok {
			it.Saved, it.Owner, it.OwnerDisplay, it.Remark = true, d.Owner, d.OwnerDisplay, d.Remark
		}
		if z, ok := sizes[it.Name]; ok {
			it.Tables, it.SizeMB, it.StatsOK = z.tables, z.mb, true
		}
		if (scope == "1" && !it.Saved) || (scope == "0" && it.Saved) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(it.Name), q) {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

type dbSize struct {
	tables int64
	mb     float64
}

// dbSizes 一次取全各库的表数与大小（数据 + 索引）。读不到（权限、引擎不支持）就返回空，页面显示“—”。
func (s *Service) dbSizes(se *session) map[string]dbSize {
	out := map[string]dbSize{}
	r, err := se.query("SELECT table_schema AS db, COUNT(*) AS n, IFNULL(SUM(data_length + index_length), 0) AS bytes FROM information_schema.tables WHERE table_schema NOT IN (" + sysSchemas + ") GROUP BY table_schema")
	if err != nil {
		return out
	}
	for _, rec := range r.Records() {
		out[rec.Str("db")] = dbSize{tables: rec.Int("n"), mb: round1(rec.Float("bytes") / 1024 / 1024)}
	}
	return out
}

// CharsetOption 是字符集及其排序规则。
type CharsetOption struct {
	Name       string   `json:"name"`
	Default    string   `json:"default_collation"`
	Collations []string `json:"collations"`
}

// UserOption 是负责人下拉里的一项。
type UserOption struct {
	Username string `json:"username"`
	Display  string `json:"display"`
}

// DBOptions 是创建数据库对话框的可选项。
type DBOptions struct {
	Charsets        []CharsetOption `json:"charsets"`
	Owners          []UserOption    `json:"owners"`
	CharsetEditable bool            `json:"charset_editable"` // 该类型实例是否支持指定字符集（Doris 等不支持）
	DefaultCharset  string          `json:"default_charset"`
}

func supportsCharset(dbType string) bool {
	return dbType == "mysql" || dbType == "polardb" || dbType == "polardb-x"
}

func (s *Service) loadCharsets(se *session) ([]CharsetOption, error) {
	cs, err := se.query("SELECT character_set_name AS name, default_collate_name AS def FROM information_schema.character_sets ORDER BY character_set_name")
	if err != nil {
		return nil, wrapTarget(err)
	}
	var out []CharsetOption
	idx := map[string]int{}
	for _, rec := range cs.Records() {
		idx[rec.Str("name")] = len(out)
		out = append(out, CharsetOption{Name: rec.Str("name"), Default: rec.Str("def"), Collations: []string{}})
	}
	co, err := se.query("SELECT collation_name AS name, character_set_name AS cs_name FROM information_schema.collations ORDER BY collation_name")
	if err != nil {
		return nil, wrapTarget(err)
	}
	for _, rec := range co.Records() {
		if i, ok := idx[rec.Str("cs_name")]; ok {
			out[i].Collations = append(out[i].Collations, rec.Str("name"))
		}
	}
	return out, nil
}

func (s *Service) owners() ([]UserOption, error) {
	users, err := s.repo.ActiveUsers()
	if err != nil {
		return nil, err
	}
	out := make([]UserOption, 0, len(users))
	for _, u := range users {
		out = append(out, UserOption{Username: u.Username, Display: u.Display})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

// DatabaseOptions 返回创建数据库需要的字符集、排序规则与负责人列表。ownersOnly 为 true 时只取负责人，不连目标库（编辑登记信息用）。
func (s *Service) DatabaseOptions(ctx context.Context, c Caller, id uint, ownersOnly bool) (*DBOptions, error) {
	out := &DBOptions{Charsets: []CharsetOption{}, DefaultCharset: "utf8mb4"}
	var err error
	if ownersOnly {
		inst, err := s.visibleInstance(c, id)
		if err != nil {
			return nil, err
		}
		if !supportsDatabase(inst.DBType) {
			return nil, unsupported(unsupportedMsg(inst.DBType, "数据库管理"))
		}
		out.CharsetEditable = supportsCharset(inst.DBType)
		out.Owners, err = s.owners()
		return out, err
	}
	se, err := s.open(ctx, c, id, supportsDatabase, "数据库管理")
	if err != nil {
		return nil, err
	}
	defer se.close()
	out.CharsetEditable = supportsCharset(se.inst.DBType)
	if out.CharsetEditable {
		if out.Charsets, err = s.loadCharsets(se); err != nil {
			return nil, err
		}
	}
	out.Owners, err = s.owners()
	return out, err
}

// DBCreate 是创建数据库的请求。Confirm 必须为 true（界面上的二次确认）。
type DBCreate struct {
	Name      string `json:"db_name"`
	Charset   string `json:"charset"`
	Collation string `json:"collation"`
	Owner     string `json:"owner"`
	Remark    string `json:"remark"`
	Confirm   bool   `json:"confirm"`
}

// CreateDatabase 在目标实例创建数据库并登记负责人与备注。
func (s *Service) CreateDatabase(ctx context.Context, c Caller, id uint, in DBCreate) error {
	if err := validNewDBName(in.Name); err != nil {
		return err
	}
	owner, err := s.checkOwner(in.Owner)
	if err != nil {
		return err
	}
	remark, err := cleanText("备注", in.Remark, 255, false)
	if err != nil {
		return err
	}
	if !in.Confirm {
		return bad("请先确认创建")
	}
	se, err := s.open(ctx, c, id, supportsDatabase, "创建数据库")
	if err != nil {
		return err
	}
	defer se.close()
	in.Charset, in.Collation = strings.ToLower(strings.TrimSpace(in.Charset)), strings.ToLower(strings.TrimSpace(in.Collation))
	clause := ""
	if in.Charset != "" || in.Collation != "" {
		if !supportsCharset(se.inst.DBType) {
			return unsupported(unsupportedMsg(se.inst.DBType, "指定字符集和排序规则"))
		}
		list, err := s.loadCharsets(se)
		if err != nil {
			return err
		}
		clause, err = charsetClause(list, in.Charset, in.Collation)
		if err != nil {
			return err
		}
	}
	// 同名库已存在？information_schema 里的比较遵循服务器的大小写设置。
	if r, err := se.query("SELECT 1 AS n FROM information_schema.schemata WHERE schema_name = ?", in.Name); err == nil && len(r.Rows) > 0 {
		return conflict(fmt.Sprintf("数据库已存在：%s", in.Name))
	}
	stmt := "CREATE DATABASE `" + in.Name + "`" + clause // 库名已通过白名单，字符集与排序规则来自目标库自己的列表
	_, execErr := se.exec(stmt)
	outcome := "成功" // i18n-ignore 审计详情，存库的文本不翻译
	if execErr != nil {
		outcome = "失败：" + execErr.Error() // i18n-ignore 审计详情，存库的文本不翻译
	}
	s.audit(c, "创建数据库", fmt.Sprintf("实例：%s，%s，负责人：%s，备注：%s，结果：%s", instLabel(se.inst), stmt, owner.Username, remark, outcome)) // i18n-ignore 审计，存库的文本不翻译
	if execErr != nil {
		return wrapTarget(execErr)
	}
	if err := s.repo.SaveDatabaseRecord(id, DatabaseRecord{DBName: in.Name, Owner: owner.Username, OwnerDisplay: owner.Display, Remark: remark}); err != nil {
		return newErr(500, "register_failed", fmt.Sprintf("数据库已创建，但在平台登记负责人失败，请到列表里点“录入”补充：%s", err.Error()))
	}
	return nil
}

// charsetClause 校验字符集与排序规则（必须是目标库自己列出来的，且互相匹配），返回 DEFAULT CHARACTER SET … COLLATE … 子句。
func charsetClause(list []CharsetOption, charset, collation string) (string, error) {
	if charset == "" && collation != "" {
		for _, cs := range list {
			for _, co := range cs.Collations {
				if co == collation {
					charset = cs.Name
				}
			}
		}
		if charset == "" {
			return "", bad(fmt.Sprintf("不支持的排序规则：%s", collation))
		}
	}
	var found *CharsetOption
	for i := range list {
		if list[i].Name == charset {
			found = &list[i]
		}
	}
	if found == nil || !identWordRe.MatchString(charset) {
		return "", bad(fmt.Sprintf("不支持的字符集：%s", charset))
	}
	clause := " DEFAULT CHARACTER SET " + charset
	if collation != "" {
		ok := false
		for _, co := range found.Collations {
			ok = ok || co == collation
		}
		if !ok || !identWordRe.MatchString(collation) {
			return "", bad(fmt.Sprintf("排序规则 %s 不属于字符集 %s", collation, charset))
		}
		clause += " COLLATE " + collation
	}
	return clause, nil
}

func (s *Service) checkOwner(username string) (*UserOption, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, bad("请选择负责人")
	}
	u, err := s.repo.ActiveUser(username)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, bad("负责人不存在")
	}
	return &UserOption{Username: u.Username, Display: u.Display}, nil
}

// DBInfo 是登记（录入/编辑）库信息的请求。
type DBInfo struct {
	Name   string `json:"db_name"`
	Owner  string `json:"owner"`
	Remark string `json:"remark"`
}

// SaveDatabaseInfo 登记或更新库的负责人与备注（只改平台信息，不碰目标库）。
func (s *Service) SaveDatabaseInfo(c Caller, id uint, in DBInfo) error {
	inst, err := s.visibleInstance(c, id)
	if err != nil {
		return err
	}
	if !supportsDatabase(inst.DBType) {
		return unsupported(unsupportedMsg(inst.DBType, "数据库管理"))
	}
	name, err := cleanText("数据库名", in.Name, 128, true)
	if err != nil {
		return err
	}
	owner, err := s.checkOwner(in.Owner)
	if err != nil {
		return err
	}
	remark, err := cleanText("备注", in.Remark, 255, false)
	if err != nil {
		return err
	}
	if err := s.repo.SaveDatabaseRecord(id, DatabaseRecord{DBName: name, Owner: owner.Username, OwnerDisplay: owner.Display, Remark: remark}); err != nil {
		return err
	}
	s.audit(c, "登记数据库信息", fmt.Sprintf("实例：%s，库：%s，负责人：%s，备注：%s", instLabel(inst), name, owner.Username, remark)) // i18n-ignore 审计，存库的文本不翻译
	return nil
}
