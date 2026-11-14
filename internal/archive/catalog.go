// @author: kevinkelly
// @license: Apache Licence
// @file: catalog.go
// @time: 2026/10/28

package archive

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	drv "github.com/go-sql-driver/mysql"

	"dbtool/internal/archive/engine"
	"dbtool/internal/audit"
	"dbtool/internal/instext"
	"dbtool/internal/model"
)

// 向导里“实例 → 库 → 表 → 列”的级联数据：现读源实例的 information_schema，不落库。
// 只用于选择和早期报错；试运行 / 运行时引擎会重新读取表结构（以引擎的结果为准）。

type CatTable struct {
	Name      string `json:"name"`
	Rows      int64  `json:"rows"`
	Protected bool   `json:"protected"`
}

type CatColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Indexed  bool   `json:"indexed"`
	IsTime   bool   `json:"is_time"`
	IsInt    bool   `json:"is_int"` // int / bigint：可能存着 10 位秒 / 13 位毫秒的 Unix 时间戳，要配合“时间单位”
}

type CatTableInfo struct {
	Columns     []CatColumn `json:"columns"`
	PKCols      []string    `json:"pk_cols"`
	HasPK       bool        `json:"has_pk"`
	Partitioned bool        `json:"partitioned"`
	HasTriggers bool        `json:"has_triggers"`
	FKParents   []string    `json:"fk_parents"`
	Rows        int64       `json:"rows"`
}

// Catalog 读取实例的库、表、列。
type Catalog interface {
	Databases(ctx context.Context) ([]string, error)
	Tables(ctx context.Context, db string) ([]CatTable, error)
	Describe(ctx context.Context, db, table string) (*CatTableInfo, error)
	// ProbeTime 探测整型列像不像 Unix 时间戳（只发 SELECT）；rangeOK 表示允许用 MIN / MAX 取最早 / 最晚值。
	ProbeTime(ctx context.Context, db, table, col string, rangeOK bool) (engine.TimeProbe, error)
	Close() error
}

// CatalogFunc 按实例创建 Catalog。
type CatalogFunc func(ctx context.Context, inst *model.Instance) (Catalog, error)

const catalogTimeout = 15 * time.Second
const maxCatalogTables = 3000

var identRe = regexp.MustCompile(`^[0-9A-Za-z_$]{1,64}$`)

func validIdent(s string) bool { return identRe.MatchString(s) }

type sqlCatalog struct{ db *sql.DB }

func (s *Service) defaultCatalog(ctx context.Context, inst *model.Instance) (Catalog, error) {
	conn := s.meta.EngineConn(inst)
	cfg := drv.NewConfig()
	cfg.User, cfg.Passwd = conn.User, conn.Password
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("%s:%d", conn.Host, conn.Port)
	cfg.Timeout = 10 * time.Second
	charset := conn.Charset
	if charset == "" {
		charset = "utf8mb4"
	}
	cfg.Params = map[string]string{"charset": charset}
	if conn.SSL {
		cfg.TLSConfig = "skip-verify"
		if conn.VerifySSL {
			cfg.TLSConfig = "true"
		}
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(2 * time.Minute)
	return &sqlCatalog{db: db}, nil
}

func (c *sqlCatalog) Close() error { return c.db.Close() }

func (c *sqlCatalog) ProbeTime(ctx context.Context, db, table, col string, rangeOK bool) (engine.TimeProbe, error) {
	return engine.ProbeEpochColumn(ctx, c.db, db, table, col, rangeOK)
}

func (c *sqlCatalog) Databases(ctx context.Context) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA ORDER BY SCHEMA_NAME")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (c *sqlCatalog) Tables(ctx context.Context, db string) ([]CatTable, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT TABLE_NAME, IFNULL(TABLE_ROWS, 0) FROM information_schema.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME LIMIT ?`, db, maxCatalogTables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CatTable
	for rows.Next() {
		var t CatTable
		if err := rows.Scan(&t.Name, &t.Rows); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type catColRow struct {
	name, typ string
	nullable  bool
}

// columns 读表的列。information_schema.COLUMNS 不可用（PolarDB-X 等兼容实现）时退到 SHOW COLUMNS。
func (c *sqlCatalog) columns(ctx context.Context, db, table string) ([]catColRow, error) {
	var out []catColRow
	rows, err := c.db.QueryContext(ctx, `SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, db, table)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var r catColRow
			var nl string
			if err := rows.Scan(&r.name, &r.typ, &nl); err != nil {
				return nil, err
			}
			r.nullable = nl == "YES"
			out = append(out, r)
		}
		return out, rows.Err()
	}
	rs, err2 := c.db.QueryContext(ctx, "SHOW COLUMNS FROM `"+strings.ReplaceAll(db, "`", "``")+"`.`"+strings.ReplaceAll(table, "`", "``")+"`")
	if err2 != nil {
		return nil, err
	}
	defer rs.Close()
	names, data, err2 := scanRaw(rs)
	if err2 != nil {
		return nil, err
	}
	fi, ti, ni := rawCol(names, "Field"), rawCol(names, "Type"), rawCol(names, "Null")
	if fi < 0 || ti < 0 || ni < 0 {
		return nil, err
	}
	for _, r := range data {
		out = append(out, catColRow{name: r[fi], typ: r[ti], nullable: strings.EqualFold(r[ni], "YES")})
	}
	return out, nil
}

type catIdxRow struct {
	name      string
	nonUnique int
	seq       int
	col       string
}

// indexes 读表的索引列（不含函数索引）。information_schema.STATISTICS 不可用时退到 SHOW INDEX。
func (c *sqlCatalog) indexes(ctx context.Context, db, table string) ([]catIdxRow, error) {
	var out []catIdxRow
	rs, err := c.db.QueryContext(ctx, `SELECT INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME IS NOT NULL ORDER BY INDEX_NAME, SEQ_IN_INDEX`, db, table)
	if err == nil {
		defer rs.Close()
		for rs.Next() {
			var r catIdxRow
			if err := rs.Scan(&r.name, &r.nonUnique, &r.seq, &r.col); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, rs.Err()
	}
	rs2, err2 := c.db.QueryContext(ctx, "SHOW INDEX FROM `"+strings.ReplaceAll(db, "`", "``")+"`.`"+strings.ReplaceAll(table, "`", "``")+"`")
	if err2 != nil {
		return nil, err
	}
	defer rs2.Close()
	names, data, err2 := scanRaw(rs2)
	if err2 != nil {
		return nil, err
	}
	ki, ui, si, ci := rawCol(names, "Key_name"), rawCol(names, "Non_unique"), rawCol(names, "Seq_in_index"), rawCol(names, "Column_name")
	if ki < 0 || ui < 0 || si < 0 || ci < 0 {
		return nil, err
	}
	for _, r := range data {
		if r[ci] == "" {
			continue
		}
		nu, _ := strconv.Atoi(r[ui])
		sq, _ := strconv.Atoi(r[si])
		out = append(out, catIdxRow{name: r[ki], nonUnique: nu, seq: sq, col: r[ci]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].seq < out[j].seq
	})
	return out, nil
}

// scanRaw 把结果集读成文本（NULL 变空串），用于 SHOW 语句的结果。
func scanRaw(rs *sql.Rows) ([]string, [][]string, error) {
	names, err := rs.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out [][]string
	for rs.Next() {
		vals := make([]sql.NullString, len(names))
		ptrs := make([]any, len(names))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := make([]string, len(names))
		for i, v := range vals {
			row[i] = v.String
		}
		out = append(out, row)
	}
	return names, out, rs.Err()
}

func rawCol(names []string, want string) int {
	for i, n := range names {
		if strings.EqualFold(n, want) {
			return i
		}
	}
	return -1
}

func (c *sqlCatalog) Describe(ctx context.Context, db, table string) (*CatTableInfo, error) {
	info := &CatTableInfo{PKCols: []string{}, FKParents: []string{}}
	nullable := map[string]bool{}
	raw, err := c.columns(ctx, db, table)
	if err != nil {
		return nil, err
	}
	for _, rc := range raw {
		col := CatColumn{Name: rc.name, Type: rc.typ, Nullable: rc.nullable}
		nullable[col.Name] = col.Nullable
		lt := strings.ToLower(col.Type)
		col.IsTime = strings.HasPrefix(lt, "datetime") || strings.HasPrefix(lt, "timestamp") || strings.HasPrefix(lt, "date")
		col.IsInt = engine.IsIntegerTimeType(col.Type)
		info.Columns = append(info.Columns, col)
	}
	if len(info.Columns) == 0 {
		return nil, sql.ErrNoRows
	}
	type idxCol struct {
		unique bool
		seq    int
		col    string
	}
	idx := map[string][]idxCol{}
	ixs, err := c.indexes(ctx, db, table)
	if err != nil {
		return nil, err
	}
	first := map[string]bool{}
	for _, x := range ixs {
		idx[x.name] = append(idx[x.name], idxCol{x.nonUnique == 0, x.seq, x.col})
		if x.seq == 1 {
			first[x.col] = true
		}
	}
	for i := range info.Columns {
		info.Columns[i].Indexed = first[info.Columns[i].Name]
	}
	if p := idx["PRIMARY"]; len(p) > 0 {
		for _, x := range p {
			info.PKCols = append(info.PKCols, x.col)
		}
	} else {
		names := make([]string, 0, len(idx))
		for n := range idx {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			cols := idx[n]
			ok := cols[0].unique
			for _, x := range cols {
				ok = ok && !nullable[x.col]
			}
			if ok {
				for _, x := range cols {
					info.PKCols = append(info.PKCols, x.col)
				}
				break
			}
		}
	}
	info.HasPK = len(info.PKCols) > 0
	var n int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND PARTITION_NAME IS NOT NULL", db, table).Scan(&n); err == nil {
		info.Partitioned = n > 0
	}
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE EVENT_OBJECT_SCHEMA = ? AND EVENT_OBJECT_TABLE = ?", db, table).Scan(&n); err == nil {
		info.HasTriggers = n > 0
	}
	fk, err := c.db.QueryContext(ctx, `SELECT DISTINCT CONCAT(TABLE_SCHEMA, '.', TABLE_NAME) FROM information_schema.KEY_COLUMN_USAGE
WHERE REFERENCED_TABLE_SCHEMA = ? AND REFERENCED_TABLE_NAME = ?`, db, table)
	if err == nil {
		for fk.Next() {
			var p string
			if fk.Scan(&p) == nil {
				info.FKParents = append(info.FKParents, p)
			}
		}
		fk.Close()
	}
	_ = c.db.QueryRowContext(ctx, "SELECT IFNULL(TABLE_ROWS, 0) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", db, table).Scan(&info.Rows)
	return info, nil
}

// ---- 服务层 ----

// dbAllowed 沿用实例的库名过滤：必须匹配 show_db_name_regex（若配置）、且不匹配 denied_db_name_regex（从开头匹配）。
func dbAllowed(inst *model.Instance, db string) error {
	check := func(expr string) (bool, error) {
		if strings.TrimSpace(expr) == "" {
			return false, nil
		}
		re, err := regexp.Compile(`\A(?:` + expr + `)`)
		if err != nil {
			return false, newErr(500, "bad_config", "实例的库名过滤正则不合法：%s", expr)
		}
		return re.MatchString(db), nil
	}
	if strings.TrimSpace(inst.ShowDBNameRegex) != "" {
		ok, err := check(inst.ShowDBNameRegex)
		if err != nil {
			return err
		}
		if !ok {
			return newErr(403, "db_not_allowed", "库 %s 不在该实例允许展示的库范围内", db)
		}
	}
	if denied, err := check(inst.DeniedDBNameRegex); err != nil {
		return err
	} else if denied {
		return newErr(403, "db_not_allowed", "库 %s 被该实例禁止访问", db)
	}
	return nil
}

func (s *Service) openCatalog(ctx context.Context, c Caller, instID uint) (context.Context, Catalog, *model.Instance, func(), error) {
	if err := c.need("archive.view", "选择归档来源"); err != nil {
		return nil, nil, nil, nil, err
	}
	if !s.repo.Ready() {
		return nil, nil, nil, nil, notReady()
	}
	inst, err := s.instanceFor(c, instID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := eligibleInstance(inst); err != nil {
		return nil, nil, nil, nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	cat, err := s.catalog(cctx, inst)
	if err != nil {
		cancel()
		return nil, nil, nil, nil, newErr(502, "source_error", "连接实例失败：%s", sanitizeErr(err))
	}
	return cctx, cat, inst, func() { cat.Close(); cancel() }, nil
}

// SourceInstance 是向导里可选的实例。
type SourceInstance struct {
	ID             uint           `json:"id"`
	Name           string         `json:"name"`
	DBType         string         `json:"db_type"`
	Env            string         `json:"env"`
	Status         string         `json:"status"`
	ResourceGroups []SourceRGroup `json:"resource_groups"`
}

type SourceRGroup struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	HasFlow bool   `json:"has_flow"`
}

func (s *Service) hasFlow(rg uint) bool {
	st, err := s.repo.AuditSetting(rg, audit.TypeArchive)
	return err == nil && st != nil && len(splitGroupIDs(st.AuditAuthGroups)) > 0
}

// SourceInstances 返回可作为来源 / 库表备份目标的实例（MySQL / MariaDB 主库、调用者可访问），以及调用者能选的资源组。
func (s *Service) SourceInstances(c Caller) ([]SourceInstance, error) {
	if err := c.need("archive.view", "选择归档来源"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	list, err := s.instances()
	if err != nil {
		return nil, err
	}
	list, err = s.access.FilterInstances(c.Principal, list)
	if err != nil {
		return nil, err
	}
	mine, err := s.callerGroups(c)
	if err != nil {
		return nil, err
	}
	v := s.newViewer(c)
	out := []SourceInstance{}
	for i := range list {
		in := &list[i]
		if eligibleInstance(in) != nil {
			continue
		}
		ids, err := s.repo.InstanceResourceGroupIDs(in.ID)
		if err != nil {
			return nil, err
		}
		si := SourceInstance{ID: in.ID, Name: in.InstanceName, DBType: in.DBType, Env: envOf(in), Status: "ok", ResourceGroups: []SourceRGroup{}}
		for _, id := range ids {
			if c.viewAll() || mine[id] {
				si.ResourceGroups = append(si.ResourceGroups, SourceRGroup{ID: id, Name: v.rgName(id), HasFlow: s.hasFlow(id)})
			}
		}
		out = append(out, si)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func envOf(in *model.Instance) string {
	env := ""
	for _, t := range in.Tags {
		s := strings.ToLower(t.TagCode + " " + t.TagName)
		switch {
		case strings.Contains(s, "prod") || strings.Contains(s, "生产"): // i18n-ignore：按实例标签里的中文关键字判断环境
			return "prod"
		case strings.Contains(s, "test") || strings.Contains(s, "测试"): // i18n-ignore：按实例标签里的中文关键字判断环境
			env = "test"
		}
	}
	return instext.InstanceEnvOf(in.ID, env)
}

// SourceDB 是向导里可选的库。
type SourceDB struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
}

func (s *Service) SourceDatabases(ctx context.Context, c Caller, instID uint) ([]SourceDB, error) {
	cctx, cat, inst, release, err := s.openCatalog(ctx, c, instID)
	if err != nil {
		return nil, err
	}
	defer release()
	all, err := cat.Databases(cctx)
	if err != nil {
		return nil, newErr(502, "source_error", "读取库列表失败：%s", sanitizeErr(err))
	}
	pol := s.loadPolicy()
	out := []SourceDB{}
	for _, d := range all {
		if !validIdent(d) || dbAllowed(inst, d) != nil {
			continue
		}
		out = append(out, SourceDB{Name: d, Protected: engine.IsProtected(pol.ProtectedPatterns, d, "")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Service) SourceTables(ctx context.Context, c Caller, instID uint, db string) ([]CatTable, error) {
	db = strings.TrimSpace(db)
	if !validIdent(db) {
		return nil, badReq("库名 %s 不合法", quote(db))
	}
	cctx, cat, inst, release, err := s.openCatalog(ctx, c, instID)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := dbAllowed(inst, db); err != nil {
		return nil, err
	}
	all, err := cat.Tables(cctx, db)
	if err != nil {
		return nil, newErr(502, "source_error", "读取表列表失败：%s", sanitizeErr(err))
	}
	pol := s.loadPolicy()
	out := []CatTable{}
	for _, t := range all {
		if validIdent(t.Name) {
			t.Protected = engine.IsProtected(pol.ProtectedPatterns, db, t.Name)
			out = append(out, t)
		}
	}
	return out, nil
}

// SourceColumnsResult 是选表之后的列信息与早期报错依据。
type SourceColumnsResult struct {
	CatTableInfo
	Protected  bool        `json:"protected"`
	ActiveTask *activeTask `json:"active_task"`
}

type activeTask struct {
	ID     uint   `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (s *Service) SourceColumns(ctx context.Context, c Caller, instID uint, db, table string) (*SourceColumnsResult, error) {
	db, table = strings.TrimSpace(db), strings.TrimSpace(table)
	if !validIdent(db) || !validIdent(table) {
		return nil, badReq("库名或表名不合法")
	}
	cctx, cat, inst, release, err := s.openCatalog(ctx, c, instID)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := dbAllowed(inst, db); err != nil {
		return nil, err
	}
	info, err := cat.Describe(cctx, db, table)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, notFound("表 %s.%s 不存在", db, table)
		}
		return nil, newErr(502, "source_error", "读取表结构失败：%s", sanitizeErr(err))
	}
	if info.Columns == nil {
		info.Columns = []CatColumn{}
	}
	res := &SourceColumnsResult{CatTableInfo: *info, Protected: engine.IsProtected(s.loadPolicy().ProtectedPatterns, db, table)}
	if t, err := s.repo.ActiveTaskOnTable(instID, db, table, 0); err == nil && t != nil {
		res.ActiveTask = &activeTask{ID: t.ID, Name: t.Name, Status: t.Status}
	}
	return res, nil
}

// SourceTimeProbe 探测一个整型列像不像 10 位秒 / 13 位毫秒的 Unix 时间戳（向导里选了整型时间列之后调用，用来预填“时间单位”并让人核对）：
// 抽样判断位数；列上有索引（或表很小）时再取最早 / 最晚值。只读，只返回统计结果，不返回行数据。
func (s *Service) SourceTimeProbe(ctx context.Context, c Caller, instID uint, db, table, col string) (*engine.TimeProbe, error) {
	db, table, col = strings.TrimSpace(db), strings.TrimSpace(table), strings.TrimSpace(col)
	if !validIdent(db) || !validIdent(table) || !validIdent(col) {
		return nil, badReq("库名、表名或列名不合法")
	}
	cctx, cat, inst, release, err := s.openCatalog(ctx, c, instID)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := dbAllowed(inst, db); err != nil {
		return nil, err
	}
	if engine.IsProtected(s.loadPolicy().ProtectedPatterns, db, table) {
		return nil, newErr(403, "protected", "%s.%s 属于受保护库表，不能归档", db, table)
	}
	info, err := cat.Describe(cctx, db, table)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, notFound("表 %s.%s 不存在", db, table)
		}
		return nil, newErr(502, "source_error", "读取表结构失败：%s", sanitizeErr(err))
	}
	var cc *CatColumn
	for i := range info.Columns {
		if strings.EqualFold(info.Columns[i].Name, col) {
			cc = &info.Columns[i]
		}
	}
	switch {
	case cc == nil:
		return nil, badReq("表 %s.%s 里没有列 %s", db, table, col)
	case !cc.IsInt:
		return nil, badReq("列 %s 的类型是 %s，不是 int / bigint，不能作为时间戳列", cc.Name, cc.Type)
	}
	pb, err := cat.ProbeTime(cctx, db, table, cc.Name, cc.Indexed || info.Rows <= engine.ExplainRowsLimit)
	if err != nil {
		return nil, newErr(502, "source_error", "探测时间列失败：%s", sanitizeErr(err))
	}
	return &pb, nil
}

// FlowLevel / FlowView 是资源组的“数据归档申请”审批流预览。
type FlowLevel struct {
	Level     int      `json:"level"`
	GroupID   uint     `json:"group_id"`
	GroupName string   `json:"group_name"`
	Users     []string `json:"users"`
}

type FlowView struct {
	Exists bool        `json:"exists"`
	Levels []FlowLevel `json:"levels"`
}

func (s *Service) Flow(c Caller, rgID uint) (*FlowView, error) {
	if err := c.need("archive.view", "查看审批流"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	if ok, err := s.canSeeGroup(c, rgID); err != nil {
		return nil, err
	} else if !ok {
		return nil, notFound("资源组不存在")
	}
	out := &FlowView{Levels: []FlowLevel{}}
	st, err := s.repo.AuditSetting(rgID, audit.TypeArchive)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return out, nil
	}
	groups := splitGroupIDs(st.AuditAuthGroups)
	out.Exists = len(groups) > 0
	for i := range groups {
		fl := FlowLevel{Level: i, GroupName: s.groupNameByID(groups, i), Users: []string{}}
		for _, ch := range groups[i] {
			if ch >= '0' && ch <= '9' {
				fl.GroupID = fl.GroupID*10 + uint(ch-'0')
			}
		}
		if us, err := s.repo.AuthGroupUsers([]string{fl.GroupName}, rgID); err == nil {
			for _, u := range us {
				fl.Users = append(fl.Users, u.Username)
			}
		}
		out.Levels = append(out.Levels, fl)
	}
	return out, nil
}
