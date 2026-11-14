// @author: kevinkelly
// @license: Apache Licence
// @file: params.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---- 可修改值的格式：范围 [1-65535]（上限可省略或写 …）、枚举 [ON|OFF] ----

type validSpec struct {
	kind   string // "" 不限制 / range / enum
	min    float64
	max    float64
	hasMax bool
	enum   []string
	raw    string
}

var rangeRe = regexp.MustCompile(`^\[\s*(-?\d+(?:\.\d+)?)\s*-\s*(-?\d+(?:\.\d+)?|…|\.\.\.)?\s*\]$`)

// parseValid 解析参数模板里的“可修改值”。不以 [ 开头的视为自由文本说明，不做限制。
func parseValid(s string) (validSpec, error) {
	s = strings.TrimSpace(s)
	spec := validSpec{raw: s}
	if s == "" || !strings.HasPrefix(s, "[") {
		return spec, nil
	}
	if m := rangeRe.FindStringSubmatch(s); m != nil {
		spec.kind = "range"
		spec.min, _ = strconv.ParseFloat(m[1], 64)
		if m[2] != "" && m[2] != "…" && m[2] != "..." {
			spec.max, _ = strconv.ParseFloat(m[2], 64)
			spec.hasMax = true
			if spec.max < spec.min {
				return spec, bad(fmt.Sprintf("可修改值的范围上限小于下限：%s", s))
			}
		}
		return spec, nil
	}
	if strings.HasSuffix(s, "]") {
		for _, it := range strings.Split(s[1:len(s)-1], "|") {
			if it = strings.TrimSpace(it); it != "" {
				spec.enum = append(spec.enum, it)
			}
		}
		if len(spec.enum) > 0 {
			spec.kind = "enum"
			return spec, nil
		}
	}
	return spec, bad("可修改值格式不对：范围写成 [1-65535]，枚举写成 [ON|OFF]")
}

// check 校验新值并返回规范化后的值（枚举取模板里的写法）。
func (v validSpec) check(value string) (string, error) {
	switch v.kind {
	case "range":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return "", bad(fmt.Sprintf("新值必须是数字（允许范围 %s）", v.raw))
		}
		if f < v.min || (v.hasMax && f > v.max) {
			return "", bad(fmt.Sprintf("新值超出允许范围 %s", v.raw))
		}
	case "enum":
		for _, e := range v.enum {
			if strings.EqualFold(e, value) {
				return e, nil
			}
		}
		return "", bad(fmt.Sprintf("新值必须是 %s 之一", v.raw))
	}
	return value, nil
}

var (
	intRe = regexp.MustCompile(`^-?\d{1,18}$`)
	decRe = regexp.MustCompile(`^-?\d{1,15}\.\d{1,10}$`)
)

// paramArg 决定 SET GLOBAL 的参数类型：整数与小数按数字传（数值型参数不接受字符串），其余按字符串。
func paramArg(v string) any {
	if intRe.MatchString(v) {
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	if decRe.MatchString(v) {
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return v
}

// deniedVars 是即使模板里写了可修改也不允许在线改的参数：能执行任意语句、改文件路径或加载外部文件。
var deniedVars = map[string]bool{
	"init_connect": true, "init_slave": true, "init_replica": true, "init_file": true, "plugin_dir": true, "datadir": true,
	"basedir": true, "tmpdir": true, "secure_file_priv": true, "general_log_file": true, "slow_query_log_file": true,
	"log_error": true, "log_bin_basename": true, "lc_messages_dir": true, "local_infile": true, "skip_grant_tables": true,
}

// ---- 参数列表 ----

// ParamRow 是参数配置页的一行。
type ParamRow struct {
	Name         string `json:"variable_name"`
	RuntimeValue string `json:"runtime_value"`
	DefaultValue string `json:"default_value"`
	ValidValues  string `json:"valid_values"`
	Description  string `json:"description"`
	Editable     bool   `json:"editable"`
	TemplateID   uint   `json:"template_id"`
	// 推荐值取自模板扩展（旁路表 dbt_param_template_ext）；Deviates 表示运行值与推荐值不同。
	Recommended  string `json:"recommended_value"`
	Deviates     bool   `json:"deviates"`
	Blacklisted  bool   `json:"blacklisted"`   // 在平台黑名单里（能执行语句 / 改文件路径），模板写了可改也拒绝
	NeedsRestart bool   `json:"needs_restart"` // 一般要改配置文件并重启才能生效（经验清单），动态修改会被数据库拒绝
}

func (s *Service) variables(se *session) (map[string]string, []string, error) {
	r, err := se.query("SHOW GLOBAL VARIABLES")
	if err != nil {
		return nil, nil, wrapTarget(err)
	}
	vars := map[string]string{}
	var names []string
	for _, rec := range r.Records() {
		n := strings.ToLower(rec.Str("variable_name"))
		if _, dup := vars[n]; !dup {
			names = append(names, n)
		}
		vars[n] = rec.Str("value")
	}
	return vars, names, nil
}

// ListParams 列出运行值，并合并参数模板。editable 取 yes / no / all（默认 all）。
func (s *Service) ListParams(ctx context.Context, c Caller, id uint, editable, q string) ([]ParamRow, error) {
	return s.ListParamsEx(ctx, c, id, ParamFilter{Editable: editable, Q: q})
}

// ParamFilter 是参数列表的筛选。Deviation 为真时只看运行值偏离模板推荐值的参数。
type ParamFilter struct {
	Editable  string // yes / no / all
	Q         string
	Deviation bool
}

// ListParamsEx 同 ListParams，多一个“偏离推荐值”筛选；每行带推荐值与是否偏离。
func (s *Service) ListParamsEx(ctx context.Context, c Caller, id uint, f ParamFilter) ([]ParamRow, error) {
	editable, q := f.Editable, f.Q
	if editable != "" && editable != "yes" && editable != "no" && editable != "all" {
		return nil, bad("editable 只能是 yes / no / all")
	}
	se, err := s.open(ctx, c, id, supportsParam, "参数配置")
	if err != nil {
		return nil, err
	}
	defer se.close()
	tpls, err := s.repo.ListTemplates(se.inst.DBType)
	if err != nil {
		return nil, err
	}
	tplBy := map[string]ParamTemplate{}
	for _, t := range tpls {
		tplBy[strings.ToLower(t.VariableName)] = t
	}
	rec := s.recommendedMap()
	vars, names, err := s.variables(se)
	if err != nil {
		return nil, err
	}
	q = strings.ToLower(strings.TrimSpace(q))
	sort.Strings(names)
	out := []ParamRow{}
	for _, n := range names {
		if q != "" && !strings.Contains(n, q) {
			continue
		}
		row := ParamRow{Name: n, RuntimeValue: vars[n]}
		if t, ok := tplBy[n]; ok {
			row.DefaultValue, row.ValidValues, row.Description, row.Editable, row.TemplateID = t.DefaultValue, t.ValidValues, t.Description, t.Editable && !deniedVars[n], t.ID
			row.Recommended = rec[t.ID]
			row.Deviates = row.Recommended != "" && !sameValue(row.RuntimeValue, row.Recommended)
		}
		row.Blacklisted, row.NeedsRestart = deniedVars[n], restartVars[n]
		if (editable == "yes" && !row.Editable) || (editable == "no" && row.Editable) || (f.Deviation && !row.Deviates) {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// ParamPlan 是一次参数修改的预览/结果。
type ParamPlan struct {
	Name        string `json:"variable_name"`
	OldValue    string `json:"old_value"`
	NewValue    string `json:"new_value"`
	ValidValues string `json:"valid_values"`
	SQL         string `json:"sql"`
	Note        string `json:"note,omitempty"`
}

type paramArgs struct {
	stmt string
	arg  any
}

func (s *Service) planParam(se *session, name, value string) (*ParamPlan, paramArgs, error) {
	var pa paramArgs
	name = strings.ToLower(strings.TrimSpace(name))
	if !varNameRe.MatchString(name) {
		return nil, pa, bad("参数名不合法")
	}
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 1024 || hasControl(value) {
		return nil, pa, bad("新值不能为空，不能含换行等控制字符，最长 1024 个字符")
	}
	if deniedVars[name] {
		return nil, pa, forbidden(fmt.Sprintf("参数 %s 不允许在线修改", name))
	}
	tpl, err := s.repo.GetTemplate(se.inst.DBType, name)
	if err != nil {
		return nil, pa, err
	}
	if tpl == nil {
		return nil, pa, bad("请先在参数模板中配置该参数")
	}
	if !tpl.Editable {
		return nil, pa, forbidden(fmt.Sprintf("参数模板里 %s 没有开启“允许在平台修改”", name))
	}
	spec, err := parseValid(tpl.ValidValues)
	if err != nil {
		return nil, pa, err
	}
	if value, err = spec.check(value); err != nil {
		return nil, pa, err
	}
	vars, _, err := s.variables(se)
	if err != nil {
		return nil, pa, err
	}
	old, ok := vars[name]
	if !ok {
		return nil, pa, notFound(fmt.Sprintf("实例上没有参数 %s", name))
	}
	if old == value {
		return nil, pa, conflict("参数值与实际运行值一致，未调整")
	}
	pa = paramArgs{stmt: "SET GLOBAL " + name + " = ?", arg: paramArg(value)}
	return &ParamPlan{Name: name, OldValue: old, NewValue: value, ValidValues: tpl.ValidValues, SQL: fmtStmt(pa.stmt, pa.arg),
		Note: "只修改运行中的值，不会写入配置文件，请事后手动持久化到 my.cnf"}, pa, nil
}

// PreviewParam 校验并生成 SET GLOBAL 预览，不执行。
func (s *Service) PreviewParam(ctx context.Context, c Caller, id uint, name, value string) (*ParamPlan, error) {
	se, err := s.open(ctx, c, id, supportsParam, "参数配置")
	if err != nil {
		return nil, err
	}
	defer se.close()
	p, _, err := s.planParam(se, name, value)
	return p, err
}

// SetParam 执行 SET GLOBAL 并记录修改历史。
func (s *Service) SetParam(ctx context.Context, c Caller, id uint, name, value string, confirm bool) (*ParamPlan, error) {
	if !confirm {
		return nil, bad("请先确认修改")
	}
	se, err := s.open(ctx, c, id, supportsParam, "参数配置")
	if err != nil {
		return nil, err
	}
	defer se.close()
	plan, pa, err := s.planParam(se, name, value)
	if err != nil {
		if refusedBy(err) { // 黑名单参数 / 模板没开放修改：服务端拒绝，留痕
			s.auditRefused(c, "修改实例参数", se.inst, fmt.Sprintf("参数 %s：%s", strings.ToLower(strings.TrimSpace(name)), err.Error())) // i18n-ignore 审计，存库的文本不翻译
		}
		return nil, err
	}
	_, execErr := se.exec(pa.stmt, pa.arg)
	outcome := "成功" // i18n-ignore 审计详情，存库的文本不翻译
	if execErr != nil {
		outcome = "失败：" + execErr.Error() // i18n-ignore 审计详情，存库的文本不翻译
	}
	s.audit(c, "修改实例参数", fmt.Sprintf("实例：%s，%s，原值：%s，结果：%s", instLabel(se.inst), plan.SQL, plan.OldValue, outcome)) // i18n-ignore 审计，存库的文本不翻译
	if execErr != nil {
		return nil, wrapTarget(execErr)
	}
	h := &ParamHistory{InstanceID: id, VariableName: plan.Name, OldVar: clipRunes(plan.OldValue, 1024), NewVar: plan.NewValue,
		SetSQL: clipRunes(plan.SQL, 1024), UserName: clipRunes(c.Username, 30), UserDisplay: clipRunes(c.Display, 50), CreateTime: s.now()}
	if err := s.repo.InsertParamHistory(h); err != nil {
		plan.Note = c.tr("参数已修改成功，但写入修改历史失败：{err}。{note}", "err", err.Error(), "note", c.trp(plan.Note))
	}
	return plan, nil
}

// HistoryPage 是修改历史的一页。
type HistoryPage struct {
	Total int64             `json:"total"`
	Rows  []ParamHistoryRow `json:"rows"`
}

// ListParamChanges 查询某实例的参数修改历史。
func (s *Service) ListParamChanges(c Caller, id uint, q string, page, pageSize int) (*HistoryPage, error) {
	if _, err := s.visibleInstance(c, id); err != nil {
		return nil, err
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 20
	}
	if page <= 0 {
		page = 1
	}
	rows, total, err := s.repo.ListParamHistory(id, strings.TrimSpace(q), (page-1)*pageSize, pageSize)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []ParamHistoryRow{}
	}
	for i := range rows {
		rows[i].CreateTimeText = rows[i].CreateTime.Format("2006-01-02 15:04:05")
	}
	return &HistoryPage{Total: total, Rows: rows}, nil
}

// ---- 参数模板 ----

// TemplateInput 是添加/修改参数模板的请求。修改时不能改数据库类型与参数名。
type TemplateInput struct {
	DBType       string `json:"db_type"`
	VariableName string `json:"variable_name"`
	DefaultValue string `json:"default_value"`
	Editable     bool   `json:"editable"`
	ValidValues  string `json:"valid_values"`
	Description  string `json:"description"`
	// 推荐值（旁路表 dbt_param_template_ext）；省略（null）表示不修改，空串表示清除。
	RecommendedValue *string `json:"recommended_value"`
}

// ListTemplates 列出某类型的参数模板。
func (s *Service) ListTemplates(dbType string) ([]ParamTemplate, error) {
	if !supportsParam(dbType) {
		return nil, unsupported(unsupportedMsg(dbType, "参数配置"))
	}
	list, err := s.repo.ListTemplates(dbType)
	if list == nil {
		list = []ParamTemplate{}
	}
	return list, err
}

// SaveTemplate 新建（id=0）或修改参数模板。
func (s *Service) SaveTemplate(c Caller, id uint, in TemplateInput) (*ParamTemplate, error) {
	var t *ParamTemplate
	if id != 0 {
		var err error
		if t, err = s.repo.GetTemplateByID(id); err != nil {
			return nil, err
		}
		if t == nil {
			return nil, notFound("参数模板不存在")
		}
		in.DBType, in.VariableName = t.DBType, t.VariableName
	}
	if !supportsParam(in.DBType) {
		return nil, unsupported(unsupportedMsg(in.DBType, "参数配置"))
	}
	name := strings.ToLower(strings.TrimSpace(in.VariableName))
	if !varNameRe.MatchString(name) {
		return nil, bad("参数名只能由小写字母、数字、下划线组成，以字母开头，最长 64 位")
	}
	var err error
	if in.DefaultValue, err = cleanText("默认值", in.DefaultValue, 1024, false); err != nil {
		return nil, err
	}
	if in.Description, err = cleanText("描述", in.Description, 1024, false); err != nil {
		return nil, err
	}
	in.ValidValues = strings.TrimSpace(in.ValidValues)
	if len(in.ValidValues) > 1024 || hasControl(in.ValidValues) {
		return nil, bad("可修改值过长或含控制字符")
	}
	spec, err := parseValid(in.ValidValues)
	if err != nil {
		return nil, err
	}
	rec, setRec, err := s.cleanRecommended(in.RecommendedValue, spec, name)
	if err != nil {
		return nil, err
	}
	if in.Editable && deniedVars[name] {
		return nil, bad(fmt.Sprintf("参数 %s 不允许在线修改，不能开启“允许在平台修改”", name))
	}
	now := s.now()
	if t == nil {
		if dup, err := s.repo.GetTemplate(in.DBType, name); err != nil {
			return nil, err
		} else if dup != nil {
			return nil, conflict(fmt.Sprintf("%s 类型已有参数 %s 的模板", dbTypeLabel(in.DBType), name))
		}
		t = &ParamTemplate{DBType: in.DBType, VariableName: name, CreateTime: now}
	}
	t.DefaultValue, t.Editable, t.ValidValues, t.Description, t.SysTime = in.DefaultValue, in.Editable, in.ValidValues, in.Description, now
	if err := s.repo.SaveTemplate(t); err != nil {
		return nil, err
	}
	recNote := ""
	if setRec {
		if err := s.ext.SaveTemplateRecommended(t.ID, rec, clipRunes(c.Username, 30), s.now()); err != nil {
			s.audit(c, "保存参数模板", fmt.Sprintf("类型：%s，参数：%s，允许修改：%v，可修改值：%s，推荐值保存失败：%s", t.DBType, t.VariableName, t.Editable, t.ValidValues, err.Error())) // i18n-ignore 审计，存库的文本不翻译
			return t, newErr(500, "register_failed", fmt.Sprintf("参数模板已保存，但保存推荐值失败：%s", err.Error()))
		}
		recNote = "，推荐值：" + nonEmptyStr(rec, "（清除）") // i18n-ignore 审计详情，存库的文本不翻译
	}
	s.audit(c, "保存参数模板", fmt.Sprintf("类型：%s，参数：%s，允许修改：%v，可修改值：%s%s", t.DBType, t.VariableName, t.Editable, t.ValidValues, recNote)) // i18n-ignore 审计，存库的文本不翻译
	return t, nil
}

// cleanRecommended 校验推荐值：必须满足模板的“可修改值”（黑名单参数除外，它们本来就不会被改）。
// set 为真表示要写入旁路表（旁路表没建而又要写非空推荐值时报错）。
func (s *Service) cleanRecommended(p *string, spec validSpec, name string) (value string, set bool, err error) {
	if p == nil {
		return "", false, nil
	}
	value = strings.TrimSpace(*p)
	if len(value) > 1024 || hasControl(value) {
		return "", false, bad("推荐值过长或含控制字符")
	}
	if value != "" && !deniedVars[name] {
		if _, err := spec.check(value); err != nil {
			return "", false, bad(fmt.Sprintf("推荐值不满足“可修改值”的限制：%s", err.Error()))
		}
	}
	if s.ext == nil || !s.ext.TemplateExtReady() {
		if value != "" {
			return "", false, errExtMissing("设置推荐值", "072_param_template_ext.sql")
		}
		return "", false, nil
	}
	return value, true, nil
}

// TemplateView 是模板加上推荐值与黑名单 / 需重启标记，参数模板总览用。
type TemplateView struct {
	ParamTemplate
	Recommended  string `json:"recommended_value"`
	Blacklisted  bool   `json:"blacklisted"`
	NeedsRestart bool   `json:"needs_restart"`
}

// recommendedMap 取全部模板的推荐值（模板 ID -> 值）；旁路表不可用或出错返回空。
func (s *Service) recommendedMap() map[uint]string {
	if s.ext == nil || !s.ext.TemplateExtReady() {
		return map[uint]string{}
	}
	m, err := s.ext.TemplateRecommended()
	if err != nil {
		slog.Warn("读取参数模板推荐值失败", "err", err)
		return map[uint]string{}
	}
	return m
}

// TemplateViewOf 构造一个模板的展示结构。
func (s *Service) TemplateViewOf(t ParamTemplate) TemplateView {
	return s.templateViews([]ParamTemplate{t})[0]
}

func (s *Service) templateViews(list []ParamTemplate) []TemplateView {
	rec := s.recommendedMap()
	out := make([]TemplateView, 0, len(list))
	for _, t := range list {
		n := strings.ToLower(t.VariableName)
		out = append(out, TemplateView{ParamTemplate: t, Recommended: rec[t.ID], Blacklisted: deniedVars[n], NeedsRestart: restartVars[n]})
	}
	return out
}

// ListTemplateViews 列出某类型的参数模板（带推荐值与黑名单 / 需重启标记）。
func (s *Service) ListTemplateViews(dbType string) ([]TemplateView, error) {
	list, err := s.ListTemplates(dbType)
	if err != nil {
		return nil, err
	}
	return s.templateViews(list), nil
}

// ListParamChangesAll 查询可见范围内所有实例的参数修改历史。
func (s *Service) ListParamChangesAll(c Caller, q string, page, pageSize int) (*HistoryPage, error) {
	if s.ext == nil {
		return nil, unsupported("当前部署不支持跨实例查看修改历史")
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 20
	}
	if page <= 0 {
		page = 1
	}
	scope, err := s.scopeOf(c)
	if err != nil {
		return nil, err
	}
	rows, total, err := s.ext.ListAllParamHistory(scope, strings.TrimSpace(q), (page-1)*pageSize, pageSize)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []ParamHistoryRow{}
	}
	for i := range rows {
		rows[i].CreateTimeText = rows[i].CreateTime.Format("2006-01-02 15:04:05")
	}
	return &HistoryPage{Total: total, Rows: rows}, nil
}

// restartVars 是常见的、需要改配置文件并重启才能生效的参数（经验清单，只做提示；动态修改它们数据库会拒绝）。
var restartVars = map[string]bool{
	"innodb_buffer_pool_instances": true, "innodb_log_file_size": true, "innodb_log_files_in_group": true, "innodb_flush_method": true,
	"innodb_data_file_path": true, "innodb_page_size": true, "innodb_doublewrite": true, "innodb_read_io_threads": true,
	"innodb_write_io_threads": true, "innodb_purge_threads": true, "innodb_undo_tablespaces": true, "innodb_buffer_pool_chunk_size": true,
	"log_bin": true, "lower_case_table_names": true, "open_files_limit": true, "skip_name_resolve": true, "performance_schema": true,
	"table_open_cache_instances": true, "large_pages": true, "innodb_force_recovery": true, "back_log": true,
}

// sameValue 比较参数的运行值与推荐值：不区分大小写；数字按数值比；ON / 1 / TRUE 与 OFF / 0 / FALSE 视为同一类布尔写法。
func sameValue(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if strings.EqualFold(a, b) {
		return true
	}
	if fa, err := strconv.ParseFloat(a, 64); err == nil {
		if fb, err := strconv.ParseFloat(b, 64); err == nil {
			return fa == fb
		}
	}
	norm := func(s string) string {
		switch strings.ToUpper(s) {
		case "ON", "1", "TRUE", "YES":
			return "ON"
		case "OFF", "0", "FALSE", "NO":
			return "OFF"
		}
		return ""
	}
	na := norm(a)
	return na != "" && na == norm(b)
}
