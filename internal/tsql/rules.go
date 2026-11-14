// @author: kevinkelly
// @license: Apache Licence
// @file: rules.go
// @time: 2026/10/28

package tsql

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/antlr4-go/antlr/v4"
	parser "github.com/bytebase/parser/tsql"
)

// Level 是规则命中后的级别，对应检查结果的 errlevel：1 警告、2 错误。
type Level int

const (
	Off     Level = 0
	Warning Level = 1
	Error   Level = 2
)

func (l Level) String() string {
	switch l {
	case Warning:
		return "WARNING"
	case Error:
		return "ERROR"
	}
	return "OFF"
}

// Finding 是一条规则命中。
type Finding struct {
	Rule    string
	Level   Level
	Message string
	Line    int // 相对于被检查文本，从 1 开始
}

// 规则 ID（命名参考 Bytebase SQL 审核）。
const (
	RuleDisallowUse          = "statement.disallow-use"
	RuleDangerous            = "statement.dangerous"
	RuleSelectNoSelectAll    = "statement.select.no-select-all"
	RuleWhereRequireSelect   = "statement.where.require.select"
	RuleWhereRequireUpdDel   = "statement.where.require.update-delete"
	RuleNoLeadingWildcard    = "statement.where.no-leading-wildcard-like"
	RuleInsertMustSpecifyCol = "statement.insert.must-specify-column"
	RuleInsertRowLimit       = "statement.insert.row-limit"
	RuleCrossDB              = "statement.disallow-cross-db-queries"
	RuleTableRequirePK       = "table.require-pk"
	RuleTableNoFK            = "table.no-foreign-key"
	RuleTableDropNaming      = "table.drop-naming-convention"
	RuleNamingTable          = "naming.table"
	RuleNamingColumn         = "naming.column"
	RuleNamingIndexPK        = "naming.index.pk"
	RuleNamingIndexUK        = "naming.index.uk"
	RuleNamingIndexIDX       = "naming.index.idx"
	RuleColumnNoNull         = "column.no-null"
	RuleColumnRequired       = "column.required"
	RuleColumnChangeType     = "column.disallow-change-type"
	RuleColumnMaxCharLen     = "column.maximum-character-length"
	RuleColumnTypeDisallow   = "column.type-disallow-list"
	RuleBackwardCompat       = "schema.backward-compatibility"
)

// RuleSetting 是一条规则的配置。JSON 里既可以只写级别字符串（"ERROR"），也可以写对象 {"level":"WARNING","regex":"…","max":20,"list":["a","b"]}。
type RuleSetting struct {
	Level Level
	Regex string   // 命名类规则的正则；索引命名可用 {{table}}、{{column_list}}
	Max   int      // 数值阈值（字符长度、插入行数…）
	List  []string // 列表参数（必需列、禁用类型）
}

func (r *RuleSetting) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		l, err := parseLevel(s)
		r.Level = l
		return err
	}
	var o struct {
		Level string   `json:"level"`
		Regex string   `json:"regex"`
		Max   int      `json:"max"`
		List  []string `json:"list"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	l, err := parseLevel(o.Level)
	r.Level, r.Regex, r.Max, r.List = l, o.Regex, o.Max, o.List
	return err
}

func parseLevel(s string) (Level, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "ERROR":
		return Error, nil
	case "WARNING", "WARN", "":
		return Warning, nil
	case "OFF", "DISABLED", "DISABLE":
		return Off, nil
	}
	return Warning, fmt.Errorf("不认识的规则级别 %q（可用 ERROR / WARNING / OFF）", s)
}

// Config 是规则集配置。
type Config map[string]RuleSetting

// DefaultConfig 是默认规则集：危险语句与缺少 WHERE 的 UPDATE/DELETE、没有主键的建表直接判错，其余是提醒。
// 命名与索引命名、必需列、类型黑名单等偏团队规范的规则默认关闭，用 dbt_setting 里的 review.mssql_rules 打开。
func DefaultConfig() Config {
	const ident = `^[A-Za-z_][A-Za-z0-9_]*$`
	return Config{
		RuleDisallowUse:          {Level: Warning},
		RuleDangerous:            {Level: Error},
		RuleSelectNoSelectAll:    {Level: Warning},
		RuleWhereRequireSelect:   {Level: Warning},
		RuleWhereRequireUpdDel:   {Level: Error},
		RuleNoLeadingWildcard:    {Level: Warning},
		RuleInsertMustSpecifyCol: {Level: Warning},
		RuleInsertRowLimit:       {Level: Warning, Max: 1000},
		RuleCrossDB:              {Level: Warning},
		RuleTableRequirePK:       {Level: Error},
		RuleTableNoFK:            {Level: Warning},
		RuleTableDropNaming:      {Level: Off, Regex: `_del$`},
		RuleNamingTable:          {Level: Off, Regex: ident, Max: 128},
		RuleNamingColumn:         {Level: Off, Regex: ident, Max: 128},
		RuleNamingIndexPK:        {Level: Off, Regex: `^PK_{{table}}$`},
		RuleNamingIndexUK:        {Level: Off, Regex: `^UQ_{{table}}_{{column_list}}$`},
		RuleNamingIndexIDX:       {Level: Off, Regex: `^IX_{{table}}_{{column_list}}$`},
		RuleColumnNoNull:         {Level: Warning},
		RuleColumnRequired:       {Level: Off, List: []string{"id"}},
		RuleColumnChangeType:     {Level: Warning},
		RuleColumnMaxCharLen:     {Level: Warning, Max: 20},
		RuleColumnTypeDisallow:   {Level: Off, List: []string{"text", "ntext", "image"}},
		RuleBackwardCompat:       {Level: Warning},
	}
}

// ParseConfig 在默认规则集上叠加 JSON 配置。空串返回默认配置；不认识的规则名、非法级别、非法正则都返回错误，调用方应记日志并回退到默认。
func ParseConfig(js string) (Config, error) {
	cfg := DefaultConfig()
	if strings.TrimSpace(js) == "" {
		return cfg, nil
	}
	var over map[string]RuleSetting
	if err := json.Unmarshal([]byte(js), &over); err != nil {
		return DefaultConfig(), fmt.Errorf("review.mssql_rules 不是合法的 JSON：%w", err)
	}
	for name, o := range over {
		base, ok := cfg[name]
		if !ok {
			return DefaultConfig(), fmt.Errorf("review.mssql_rules 里有不认识的规则 %q", name)
		}
		// 对象写法没给的参数沿用默认值
		if o.Regex == "" {
			o.Regex = base.Regex
		}
		if o.Max == 0 {
			o.Max = base.Max
		}
		if o.List == nil {
			o.List = base.List
		}
		if o.Regex != "" && (strings.Contains(name, "naming") || name == RuleTableDropNaming) {
			if _, err := regexp.Compile(expandTemplate(o.Regex, "t", "c")); err != nil {
				return DefaultConfig(), fmt.Errorf("规则 %s 的正则不合法：%w", name, err)
			}
		}
		cfg[name] = o
	}
	return cfg, nil
}

// RuleNames 返回全部规则名（排序），用于文档与校验。
func RuleNames() []string {
	var out []string
	for k := range DefaultConfig() {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func expandTemplate(re, table, cols string) string {
	re = strings.ReplaceAll(re, "{{table}}", regexp.QuoteMeta(table))
	return strings.ReplaceAll(re, "{{column_list}}", regexp.QuoteMeta(cols))
}

// Review 对解析成功的一段 T-SQL 运行规则。currentDB 是工单选择的数据库（用于跨库检查），可为空。
// 解析失败（p 不 OK）时不运行规则。
func Review(sqlText string, p *Parsed, cfg Config, currentDB string) []Finding {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	var out []Finding
	if p.OK() {
		r := &reviewer{cfg: cfg, db: currentDB}
		antlr.ParseTreeWalkerDefault.Walk(r, p.Tree)
		out = append(out, r.out...)
	}
	out = append(out, tokenRules(sqlText, cfg)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// ---- 基于词法的规则（不依赖语法树）：危险的系统过程/语句 ----

var dangerousProcs = map[string]string{
	"xp_cmdshell": "可执行操作系统命令", "sp_configure": "修改服务器配置", "sp_oacreate": "创建 OLE 对象（可执行任意代码）",
	"sp_addlinkedserver": "创建链接服务器", "sp_addlinkedsrvlogin": "配置链接服务器登录", "sp_addsrvrolemember": "修改服务器角色成员",
	"sp_addrolemember": "修改数据库角色成员", "sp_password": "修改登录密码", "sp_addlogin": "创建登录", "sp_dropserver": "删除服务器",
	"sp_makewebtask": "写出文件", "xp_regwrite": "写注册表", "xp_regdeletekey": "删注册表", "xp_dirtree": "读取服务器目录",
	"xp_fileexist": "探测服务器文件", "xp_servicecontrol": "控制系统服务",
}

func tokenRules(sqlText string, cfg Config) []Finding {
	set, ok := cfg[RuleDangerous]
	if !ok || set.Level == Off {
		return nil
	}
	var out []Finding
	line := 1
	toks := Tokenize(sqlText)
	var sig []Token
	var sigLine []int
	for _, t := range toks {
		if t.Kind != Space && t.Kind != Comment {
			sig = append(sig, t)
			sigLine = append(sigLine, line)
		}
		line += strings.Count(t.Text, "\n")
	}
	add := func(i int, msg string) {
		out = append(out, Finding{Rule: RuleDangerous, Level: set.Level, Line: sigLine[i], Message: msg})
	}
	for i, t := range sig {
		if t.Kind != Word {
			continue
		}
		low := strings.ToLower(t.Text)
		if why, hit := dangerousProcs[low]; hit {
			add(i, fmt.Sprintf("禁止调用 %s（%s）", low, why))
			continue
		}
		next := ""
		if i+1 < len(sig) && sig[i+1].Kind == Word {
			next = strings.ToUpper(sig[i+1].Text)
		}
		switch up := strings.ToUpper(t.Text); up {
		case "OPENROWSET", "OPENDATASOURCE", "OPENQUERY":
			add(i, fmt.Sprintf("禁止使用 %s（访问外部数据源）", up))
		case "BULK":
			if next == "INSERT" {
				add(i, "禁止 BULK INSERT（读取服务器文件）")
			}
		case "RESTORE":
			if next != "" {
				add(i, "禁止在工单里执行 RESTORE")
			}
		case "CREATE", "ALTER", "DROP":
			switch next {
			case "LOGIN":
				add(i, fmt.Sprintf("禁止在工单里 %s LOGIN（登录属于服务器级账号）", up))
			case "SERVER":
				add(i, fmt.Sprintf("禁止在工单里 %s SERVER 级对象", up))
			}
		}
	}
	return out
}

// ---- 基于语法树的规则 ----

type reviewer struct {
	parser.BaseTSqlParserListener
	cfg Config
	db  string
	out []Finding
}

func (r *reviewer) add(rule string, ctx antlr.ParserRuleContext, format string, a ...any) {
	set := r.cfg[rule]
	if set.Level == Off {
		return
	}
	line := 1
	if ctx != nil && ctx.GetStart() != nil {
		line = ctx.GetStart().GetLine()
	}
	r.out = append(r.out, Finding{Rule: rule, Level: set.Level, Line: line, Message: fmt.Sprintf(format, a...)})
}

func (r *reviewer) on(rule string) bool { return r.cfg[rule].Level != Off }

// nameParts 把 [db].[schema].[tbl] 这样的名字拆成各段（去掉括号）。
func nameParts(text string) []string {
	var parts []string
	var cur strings.Builder
	inBr, inQ := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case inBr:
			cur.WriteByte(c)
			if c == ']' {
				inBr = false
			}
		case inQ:
			cur.WriteByte(c)
			if c == '"' {
				inQ = false
			}
		case c == '[':
			inBr = true
			cur.WriteByte(c)
		case c == '"':
			inQ = true
			cur.WriteByte(c)
		case c == '.':
			parts = append(parts, Unquote(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, Unquote(cur.String()))
	return parts
}

func lastPart(text string) string {
	p := nameParts(text)
	return p[len(p)-1]
}

func isTemp(name string) bool { return strings.HasPrefix(name, "#") || strings.HasPrefix(name, "@") }

func (r *reviewer) EnterUse_statement(ctx *parser.Use_statementContext) {
	r.add(RuleDisallowUse, ctx, "不要在工单里用 USE 切换数据库：目标库已在提交页选定，脚本内切库会让后续语句落到别的库")
}

func (r *reviewer) EnterDrop_database(ctx *parser.Drop_databaseContext) {
	r.add(RuleDangerous, ctx, "禁止 DROP DATABASE")
}
func (r *reviewer) EnterAlter_database(ctx *parser.Alter_databaseContext) {
	r.add(RuleDangerous, ctx, "禁止 ALTER DATABASE（修改库级设置、文件）")
}
func (r *reviewer) EnterCreate_database(ctx *parser.Create_databaseContext) {
	r.add(RuleDangerous, ctx, "禁止 CREATE DATABASE")
}
func (r *reviewer) EnterBackup_statement(ctx *parser.Backup_statementContext) {
	r.add(RuleDangerous, ctx, "禁止在工单里执行 BACKUP / RESTORE")
}
func (r *reviewer) EnterDbcc_clause(ctx *parser.Dbcc_clauseContext) {
	r.add(RuleDangerous, ctx, "禁止在工单里执行 DBCC")
}
func (r *reviewer) EnterShutdown_statement(ctx *parser.Shutdown_statementContext) {
	r.add(RuleDangerous, ctx, "禁止 SHUTDOWN")
}
func (r *reviewer) EnterReconfigure_statement(ctx *parser.Reconfigure_statementContext) {
	r.add(RuleDangerous, ctx, "禁止 RECONFIGURE")
}
func (r *reviewer) EnterKill_statement(ctx *parser.Kill_statementContext) {
	r.add(RuleDangerous, ctx, "禁止 KILL 会话")
}

// 跨库：三段或四段名字里的库名与工单选择的库不同。
func (r *reviewer) EnterFull_table_name(ctx *parser.Full_table_nameContext) {
	r.crossDB(ctx, ctx.GetText())
}
func (r *reviewer) EnterTable_name(ctx *parser.Table_nameContext) {
	r.crossDB(ctx, ctx.GetText())
}

func (r *reviewer) crossDB(ctx antlr.ParserRuleContext, text string) {
	if !r.on(RuleCrossDB) || r.db == "" {
		return
	}
	parts := nameParts(text)
	if len(parts) >= 3 {
		db := parts[len(parts)-3]
		if db != "" && !strings.EqualFold(db, r.db) && !strings.HasPrefix(db, "#") {
			r.add(RuleCrossDB, ctx, "跨库访问 %s（工单目标库是 %s）", db, r.db)
		}
	}
}

func (r *reviewer) EnterUpdate_statement(ctx *parser.Update_statementContext) {
	if ctx.WHERE() == nil {
		r.add(RuleWhereRequireUpdDel, ctx, "UPDATE 没有 WHERE 条件，会修改整张表")
	}
}

func (r *reviewer) EnterDelete_statement(ctx *parser.Delete_statementContext) {
	if ctx.WHERE() == nil {
		r.add(RuleWhereRequireUpdDel, ctx, "DELETE 没有 WHERE 条件，会删除整张表的数据")
	}
}

func (r *reviewer) EnterQuery_specification(ctx *parser.Query_specificationContext) {
	if sl, ok := ctx.Select_list().(*parser.Select_listContext); ok && sl != nil {
		for _, e := range sl.AllSelect_list_elem() {
			if el, ok := e.(*parser.Select_list_elemContext); ok && el.Asterisk() != nil {
				r.add(RuleSelectNoSelectAll, ctx, "不要使用 SELECT *，请明确列出需要的列")
				break
			}
		}
	}
	if ctx.From_table_sources() != nil && ctx.WHERE() == nil && !inSubquery(ctx) {
		r.add(RuleWhereRequireSelect, ctx, "SELECT 没有 WHERE 条件，可能全表扫描")
	}
}

// inSubquery 查询是否嵌在子查询里（只对最外层查询要求 WHERE，避免对 EXISTS 子查询等噪声过多）。
func inSubquery(ctx antlr.Tree) bool {
	for p := ctx.GetParent(); p != nil; p = p.GetParent() {
		switch p.(type) {
		case *parser.SubqueryContext, *parser.Derived_tableContext:
			return true
		}
	}
	return false
}

func (r *reviewer) EnterPredicate(ctx *parser.PredicateContext) {
	if ctx.LIKE() == nil {
		return
	}
	exprs := ctx.AllExpression()
	if len(exprs) < 2 {
		return
	}
	pat := strings.ToUpper(exprs[1].GetText())
	if strings.HasPrefix(pat, "'%") || strings.HasPrefix(pat, "N'%") {
		r.add(RuleNoLeadingWildcard, ctx, "LIKE 以 %% 开头无法使用索引，会全表/全索引扫描")
	}
}

func (r *reviewer) EnterInsert_statement(ctx *parser.Insert_statementContext) {
	val := ctx.Insert_statement_value()
	if ctx.Insert_column_name_list() == nil && val != nil && val.DEFAULT() == nil && val.Execute_statement() == nil {
		r.add(RuleInsertMustSpecifyCol, ctx, "INSERT 要显式指定列名，避免表结构变化后写错列")
	}
	if set := r.cfg[RuleInsertRowLimit]; set.Level != Off && set.Max > 0 && val != nil {
		if n := countValueRows(val); n > set.Max {
			r.add(RuleInsertRowLimit, ctx, "一条 INSERT 写入 %d 行，超过上限 %d，请拆分", n, set.Max)
		}
	}
}

// countValueRows 数 VALUES 里的行数。
func countValueRows(t antlr.Tree) int {
	if _, ok := t.(*parser.Table_value_constructorContext); ok {
		n := 0
		for _, c := range t.GetChildren() {
			if _, ok := c.(*parser.Expression_list_Context); ok {
				n++
			}
		}
		return n
	}
	for _, c := range t.GetChildren() {
		if n := countValueRows(c); n > 0 {
			return n
		}
	}
	return 0
}

// ---- DDL ----

var charRe = regexp.MustCompile(`(?i)^(n?char)\((\d+)\)$`)

func (r *reviewer) EnterForeign_key_options(ctx *parser.Foreign_key_optionsContext) {
	r.add(RuleTableNoFK, ctx, "不建议使用外键约束（影响写入性能与分库分表）")
}

func (r *reviewer) EnterCreate_table(ctx *parser.Create_tableContext) {
	name := lastPart(ctx.Table_name().GetText())
	temp := isTemp(name)
	if !temp {
		r.checkNaming(RuleNamingTable, ctx, "表", name)
	}
	hasPK := false
	pkCols := map[string]bool{}
	var cols []*parser.Column_definitionContext
	var uniques, indexes []string
	if defs := ctx.Column_def_table_constraints(); defs != nil {
		for _, d := range defs.(*parser.Column_def_table_constraintsContext).AllColumn_def_table_constraint() {
			dc := d.(*parser.Column_def_table_constraintContext)
			if cd, ok := dc.Column_definition().(*parser.Column_definitionContext); ok && cd != nil {
				cols = append(cols, cd)
				for _, el := range cd.AllColumn_definition_element() {
					if cc, ok := el.(*parser.Column_definition_elementContext); ok {
						if c, ok := cc.Column_constraint().(*parser.Column_constraintContext); ok && c != nil && c.PRIMARY() != nil {
							hasPK = true
							pkCols[strings.ToLower(Unquote(cd.Id_().GetText()))] = true
						}
					}
				}
			}
			if tc, ok := dc.Table_constraint().(*parser.Table_constraintContext); ok && tc != nil {
				if tc.PRIMARY() != nil {
					hasPK = true
					for _, c := range splitList(tc.Column_name_list_with_order()) {
						pkCols[strings.ToLower(c)] = true
					}
					r.checkIndexName(RuleNamingIndexPK, ctx, constraintName(tc), name, splitList(tc.Column_name_list_with_order()))
				} else if tc.UNIQUE() != nil {
					uniques = append(uniques, constraintName(tc))
					r.checkIndexName(RuleNamingIndexUK, ctx, constraintName(tc), name, splitList(tc.Column_name_list_with_order()))
				}
			}
		}
	}
	_ = uniques
	_ = indexes
	if !hasPK && !temp {
		r.add(RuleTableRequirePK, ctx, "表 %s 没有主键，请定义 PRIMARY KEY", name)
	}
	for _, cd := range cols {
		r.checkColumn(cd, name, pkCols)
	}
	r.checkRequiredColumns(ctx, name, cols)
}

func constraintName(tc *parser.Table_constraintContext) string {
	if tc.GetConstraint() != nil {
		return Unquote(tc.GetConstraint().GetText())
	}
	return ""
}

func splitList(ctx antlr.ParserRuleContext) []string {
	if ctx == nil {
		return nil
	}
	var out []string
	for _, c := range ctx.GetChildren() {
		if rc, ok := c.(antlr.ParserRuleContext); ok {
			t := rc.GetText()
			// 去掉排序后缀
			t = strings.TrimSuffix(strings.TrimSuffix(t, "ASC"), "DESC")
			t = strings.TrimSuffix(strings.TrimSuffix(t, "asc"), "desc")
			out = append(out, Unquote(t))
		}
	}
	return out
}

func (r *reviewer) checkColumn(cd *parser.Column_definitionContext, table string, pkCols map[string]bool) {
	cname := Unquote(cd.Id_().GetText())
	r.checkNaming(RuleNamingColumn, cd, "列", cname)
	computed := cd.AS() != nil
	nullable, identity, pkHere := true, false, false
	for _, el := range cd.AllColumn_definition_element() {
		e, ok := el.(*parser.Column_definition_elementContext)
		if !ok {
			continue
		}
		if e.IDENTITY() != nil {
			identity = true
		}
		if c, ok := e.Column_constraint().(*parser.Column_constraintContext); ok && c != nil {
			if c.PRIMARY() != nil {
				pkHere = true
			}
			if nn, ok := c.Null_notnull().(*parser.Null_notnullContext); ok && nn != nil && nn.NOT() != nil {
				nullable = false
			}
		}
	}
	if !computed && nullable && !identity && !pkHere && !pkCols[strings.ToLower(cname)] {
		r.add(RuleColumnNoNull, cd, "列 %s 允许 NULL：建议显式 NOT NULL 并给默认值", cname)
	}
	if dt := cd.Data_type(); dt != nil {
		typ := dt.GetText()
		if m := charRe.FindStringSubmatch(typ); m != nil {
			if n, _ := strconv.Atoi(m[2]); n > r.cfg[RuleColumnMaxCharLen].Max {
				r.add(RuleColumnMaxCharLen, cd, "列 %s 的 %s 长度 %d 超过 %d，定长字符类型请改用 VARCHAR 或缩短", cname, strings.ToUpper(m[1]), n, r.cfg[RuleColumnMaxCharLen].Max)
			}
		}
		base := strings.ToLower(strings.SplitN(typ, "(", 2)[0])
		for _, bad := range r.cfg[RuleColumnTypeDisallow].List {
			if strings.EqualFold(bad, base) {
				r.add(RuleColumnTypeDisallow, cd, "列 %s 使用了被禁用的类型 %s", cname, base)
			}
		}
	}
}

func (r *reviewer) checkRequiredColumns(ctx antlr.ParserRuleContext, table string, cols []*parser.Column_definitionContext) {
	set := r.cfg[RuleColumnRequired]
	if set.Level == Off || isTemp(table) {
		return
	}
	have := map[string]bool{}
	for _, cd := range cols {
		have[strings.ToLower(Unquote(cd.Id_().GetText()))] = true
	}
	var missing []string
	for _, need := range set.List {
		if !have[strings.ToLower(need)] {
			missing = append(missing, need)
		}
	}
	if len(missing) > 0 {
		r.add(RuleColumnRequired, ctx, "表 %s 缺少必需的列：%s", table, strings.Join(missing, "、"))
	}
}

func (r *reviewer) checkNaming(rule string, ctx antlr.ParserRuleContext, what, name string) {
	set := r.cfg[rule]
	if set.Level == Off {
		return
	}
	if set.Regex != "" {
		if re, err := regexp.Compile(set.Regex); err == nil && !re.MatchString(name) {
			r.add(rule, ctx, "%s名 %s 不符合命名规范 %s", what, name, set.Regex)
			return
		}
	}
	if set.Max > 0 && len([]rune(name)) > set.Max {
		r.add(rule, ctx, "%s名 %s 超过最大长度 %d", what, name, set.Max)
	}
}

func (r *reviewer) checkIndexName(rule string, ctx antlr.ParserRuleContext, idx, table string, cols []string) {
	set := r.cfg[rule]
	if set.Level == Off || idx == "" || set.Regex == "" {
		return
	}
	re, err := regexp.Compile(expandTemplate(set.Regex, table, strings.Join(cols, "_")))
	if err == nil && !re.MatchString(idx) {
		r.add(rule, ctx, "约束/索引名 %s 不符合命名规范 %s", idx, expandTemplate(set.Regex, table, strings.Join(cols, "_")))
	}
}

func (r *reviewer) EnterCreate_index(ctx *parser.Create_indexContext) {
	if !r.on(RuleNamingIndexIDX) && !r.on(RuleNamingIndexUK) {
		return
	}
	idx := Unquote(ctx.Id_(0).GetText())
	table := lastPart(ctx.Table_name().GetText())
	cols := splitList(ctx.Column_name_list_with_order())
	rule := RuleNamingIndexIDX
	if ctx.UNIQUE() != nil {
		rule = RuleNamingIndexUK
	}
	r.checkIndexName(rule, ctx, idx, table, cols)
}

func (r *reviewer) EnterAlter_table(ctx *parser.Alter_tableContext) {
	table := lastPart(ctx.Table_name(0).GetText())
	switch {
	case ctx.ADD() != nil && ctx.Column_def_table_constraints() != nil:
		defs := ctx.Column_def_table_constraints().(*parser.Column_def_table_constraintsContext)
		for _, d := range defs.AllColumn_def_table_constraint() {
			dc := d.(*parser.Column_def_table_constraintContext)
			if cd, ok := dc.Column_definition().(*parser.Column_definitionContext); ok && cd != nil {
				r.checkColumn(cd, table, nil)
				// 给已有的表加 NOT NULL 又没有默认值：旧数据会让变更失败
				if hasNotNullNoDefault(cd) {
					r.add(RuleBackwardCompat, cd, "给已有表 %s 添加 NOT NULL 列 %s 却没有 DEFAULT，会因旧数据失败/锁表", table, Unquote(cd.Id_().GetText()))
				}
			}
			if tc, ok := dc.Table_constraint().(*parser.Table_constraintContext); ok && tc != nil {
				kind := ""
				switch {
				case tc.PRIMARY() != nil:
					kind = "主键"
					r.checkIndexName(RuleNamingIndexPK, ctx, constraintName(tc), table, splitList(tc.Column_name_list_with_order()))
				case tc.UNIQUE() != nil:
					kind = "唯一约束"
					r.checkIndexName(RuleNamingIndexUK, ctx, constraintName(tc), table, splitList(tc.Column_name_list_with_order()))
				case tc.FOREIGN() != nil:
					kind = "外键"
				case tc.Check_constraint() != nil:
					kind = "CHECK 约束"
				}
				if kind != "" {
					r.add(RuleBackwardCompat, ctx, "给已有表 %s 添加%s，旧数据可能不满足而失败", table, kind)
				}
			}
		}
	case len(ctx.AllALTER()) > 1 && ctx.COLUMN() != nil && ctx.Column_definition() != nil:
		cd := ctx.Column_definition().(*parser.Column_definitionContext)
		r.add(RuleColumnChangeType, ctx, "修改表 %s 的列 %s 的定义（类型/长度/可空性）", table, Unquote(cd.Id_().GetText()))
		r.add(RuleBackwardCompat, ctx, "修改列 %s 的定义可能不向后兼容（截断、锁表）", Unquote(cd.Id_().GetText()))
	case ctx.DROP() != nil && ctx.COLUMN() != nil:
		r.add(RuleBackwardCompat, ctx, "删除表 %s 的列：依赖这些列的应用会出错，且不可恢复", table)
	}
}

func hasNotNullNoDefault(cd *parser.Column_definitionContext) bool {
	notNull, hasDefault, identity := false, false, false
	for _, el := range cd.AllColumn_definition_element() {
		e, ok := el.(*parser.Column_definition_elementContext)
		if !ok {
			continue
		}
		if e.DEFAULT() != nil {
			hasDefault = true
		}
		if e.IDENTITY() != nil {
			identity = true
		}
		if c, ok := e.Column_constraint().(*parser.Column_constraintContext); ok && c != nil {
			if nn, ok := c.Null_notnull().(*parser.Null_notnullContext); ok && nn != nil && nn.NOT() != nil {
				notNull = true
			}
		}
	}
	return notNull && !hasDefault && !identity && cd.AS() == nil
}

func (r *reviewer) EnterDrop_table(ctx *parser.Drop_tableContext) {
	r.add(RuleBackwardCompat, ctx, "DROP TABLE 不可恢复，依赖它的应用会出错")
	if set := r.cfg[RuleTableDropNaming]; set.Level != Off && set.Regex != "" {
		re, err := regexp.Compile(set.Regex)
		if err != nil {
			return
		}
		for _, n := range ctx.AllTable_name() {
			if name := lastPart(n.GetText()); !re.MatchString(name) {
				r.add(RuleTableDropNaming, ctx, "只允许删除命名符合 %s 的表（待删除标记），%s 不符合", set.Regex, name)
			}
		}
	}
}

func (r *reviewer) EnterExecute_statement(ctx *parser.Execute_statementContext) {
	if strings.HasPrefix(strings.ToLower(ctx.GetText()), "execsp_rename") || strings.HasPrefix(strings.ToLower(ctx.GetText()), "executesp_rename") {
		r.add(RuleBackwardCompat, ctx, "sp_rename 重命名对象不向后兼容，依赖旧名字的应用会出错")
	}
}
