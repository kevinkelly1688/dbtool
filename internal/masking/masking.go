// @author: kevinkelly
// @license: Apache Licence
// @file: masking.go
// @time: 2026/10/28

// Package masking 实现查询结果的数据脱敏，全引擎通用。
//
// 定位“哪些结果列要脱敏”有三种方式，由各引擎的查询流水线按自己能拿到的信息选择：
//   - 来源列方式（Analyze）：引擎给出每个结果列来自哪个 库.表.字段（goInception 语法树、SQL Server 的 sp_describe_first_result_set、
//     PostgreSQL 结果集描述里的表 OID + 列号），再对照脱敏配置，精确；
//   - 保守方式（ConservativeHits）：没有来源信息时，按结果列名、别名与表达式里的字段引用匹配，宁多勿漏；
//   - 路径方式：MongoDB 按文档字段路径匹配（见 DocPath）。
//
// 值的替换规则（Rule）有四种：保留首尾、三段式、整体隐藏、自定义正则；Kind 为空时按 RuleType 取引擎内置默认正则。
package masking

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 旧规则类型。
const RuleThreeSegment = 100 // 三段式通用规则：按长度三等分，隐藏其中一段

// 规则种类（Rule.Kind）。空串：按 Type 取引擎内置默认正则。
const (
	KindKeep  = "keep"  // 保留前 Prefix 位和后 Suffix 位，中间固定显示 ***
	KindThree = "three" // 三段式：按长度三等分，隐藏 HideGroup 指定的一段（1 前 2 中 3 后）
	KindWhole = "whole" // 一律显示 ***
	KindRegex = "regex" // 正则 + 要隐藏的分组
)

// Mask 是被隐藏部分的固定替身。固定三个星号，看不出原值有多长。
const Mask = "***"

// Rule 是一条值的替换规则。
type Rule struct {
	Type      int    // 内置默认规则编号（Kind 为空时使用）
	Regex     string // 旧规则 / KindRegex 的正则
	HideGroup int    // 旧规则 / KindRegex 要隐藏的分组；KindThree 为 1 前 2 中 3 后
	Kind      string // 空 = 旧规则
	Prefix    int    // KindKeep 保留前几位
	Suffix    int    // KindKeep 保留后几位
}

// Column 对应一条激活的脱敏字段配置（来自 dbt_masking_policy）。
// Schema 与 Table 同时为 "*" 表示该字段名在所有库表生效。
// PostgreSQL / SQL Server 的 Table 写成 架构.表；MongoDB 的 Table 是集合名、Column 是点号分隔的字段路径。
type Column struct {
	Schema   string
	Table    string
	Column   string
	RuleType int
	Rule     *Rule // 非空：配置自带的规则（新配置）；空：按 RuleType 取旧规则
}

// SelectColumn 是引擎给出的一个结果列：index 为它在结果集里的位置，其余说明它来自哪里。
// 表达式列（函数、运算）没有 table/field。Schema 是库名；PostgreSQL / SQL Server 的 Table 是 架构.表。
type SelectColumn struct {
	Index  int    `json:"index"`
	Field  string `json:"field"`
	Type   string `json:"type"`
	Table  string `json:"table"`
	Schema string `json:"schema"`
	Alias  string `json:"alias"`
}

// Hit 是命中脱敏规则的一个结果列。
type Hit struct {
	Index    int
	Schema   string
	Table    string
	Column   string
	RuleType int
	Rule     *Rule // 非空时直接用它，不再按 RuleType 查
}

// DefaultRules 返回内置规则：规则 100 在库里没有记录时的默认值（隐藏中间一段）。
func DefaultRules() map[int]Rule {
	return map[int]Rule{RuleThreeSegment: {Type: RuleThreeSegment, Regex: `^([\s\S]{0,}?)([\s\S]{0,}?)([\s\S]{0,}?)$`, HideGroup: 2}}
}

// Resolve 取出命中列实际使用的规则。旧规则在库里找不到时返回 false。
func (h Hit) Resolve(rules map[int]Rule) (Rule, bool) {
	return resolve(h.Rule, h.RuleType, rules)
}

// ResolveColumn 同 Hit.Resolve，用于按配置项直接取规则。
func (c Column) ResolveColumn(rules map[int]Rule) (Rule, bool) {
	return resolve(c.Rule, c.RuleType, rules)
}

func resolve(inline *Rule, ruleType int, rules map[int]Rule) (Rule, bool) {
	if inline != nil {
		return *inline, true
	}
	if r, ok := rules[ruleType]; ok {
		return r, true
	}
	if d, ok := DefaultRules()[ruleType]; ok {
		return d, true
	}
	return Rule{}, false
}

// tableMatch 判断配置里的表名是否等于来源表名。配置里没写架构（旧配置 / 手写）时，与来源表名的最后一段比较。
func tableMatch(cfg, src string) bool {
	if strings.EqualFold(cfg, src) {
		return true
	}
	if !strings.Contains(cfg, ".") {
		if i := strings.LastIndex(src, "."); i >= 0 {
			return strings.EqualFold(cfg, src[i+1:])
		}
	}
	return false
}

// Analyze 对照配置找出命中的结果列。
// 先按 库.表.字段 精确匹配，找不到再按“任意库表的同名字段”（配置里 schema 与 table 都是 *）匹配。
// 库名、表名比较不区分大小写（服务器常配 lower_case_table_names），多脱敏而不会少脱敏。
func Analyze(list []SelectColumn, configured []Column) []Hit {
	var hits []Hit
	for _, sc := range list {
		field := strings.ToLower(sc.Field)
		if field == "" {
			continue
		}
		var found *Column
		for i := range configured {
			c := &configured[i]
			if strings.ToLower(c.Column) == field && c.Schema != "*" && strings.EqualFold(c.Schema, sc.Schema) && tableMatch(c.Table, sc.Table) {
				found = c
				break
			}
		}
		if found == nil {
			for i := range configured {
				c := &configured[i]
				if strings.ToLower(c.Column) == field && c.Schema == "*" && c.Table == "*" {
					found = c
					break
				}
			}
		}
		if found != nil {
			hits = append(hits, Hit{Index: sc.Index, Schema: sc.Schema, Table: sc.Table, Column: field, RuleType: found.RuleType, Rule: found.Rule})
		}
	}
	return hits
}

// ErrBadIndex 表示引擎返回的列序号超出结果集范围，无法确定该脱敏哪一列。
var ErrBadIndex = errors.New("脱敏列序号超出结果集范围")

// ApplyHits 按命中列对结果集脱敏（原地修改 rows），返回被脱敏的结果列名。
func ApplyHits(columns []string, rows [][]any, hits []Hit, rules map[int]Rule) ([]string, error) {
	return ApplyHitsTyped(columns, nil, rows, hits, rules)
}

// IsCompositeType 判断列类型是否是 JSON / Map / Array / Tuple 一类的复合类型（引擎把它们整形成了 JSON 文本）。
func IsCompositeType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	for {
		switch {
		case strings.HasPrefix(t, "nullable("), strings.HasPrefix(t, "lowcardinality("):
			t = t[strings.Index(t, "(")+1:]
			t = strings.TrimSuffix(t, ")")
			continue
		}
		break
	}
	for _, p := range []string{"json", "map(", "array(", "tuple(", "nested(", "object("} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// applyCell 对一个单元格脱敏：复合类型列的 JSON 文本按叶子逐个处理，其余按值处理。
func applyCell(v any, typ string, r Rule) (any, error) {
	if s, ok := v.(string); ok && IsCompositeType(typ) {
		out, isJSON, err := ApplyJSONText(s, r)
		if err != nil {
			return nil, err
		}
		if isJSON {
			return out, nil
		}
	}
	return Apply(v, r)
}

// ApplyHitsTyped 同 ApplyHits；types 与 columns 对应（可为 nil），复合类型的列按 JSON 叶子逐个脱敏。
func ApplyHitsTyped(columns, types []string, rows [][]any, hits []Hit, rules map[int]Rule) ([]string, error) {
	var masked []string
	seen := map[int]bool{}
	for _, h := range hits {
		if h.Index < 0 || h.Index >= len(columns) {
			return nil, fmt.Errorf("%w（序号 %d，共 %d 列）", ErrBadIndex, h.Index, len(columns))
		}
		rule, ok := h.Resolve(rules)
		if !ok {
			continue // 规则不存在：跳过该列
		}
		for i := range rows {
			if h.Index >= len(rows[i]) {
				continue
			}
			typ := ""
			if h.Index < len(types) {
				typ = types[h.Index]
			}
			v, err := applyCell(rows[i][h.Index], typ, rule)
			if err != nil {
				return nil, err
			}
			rows[i][h.Index] = v
		}
		if !seen[h.Index] {
			seen[h.Index] = true
			masked = append(masked, columns[h.Index])
		}
	}
	return masked, nil
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []byte:
		return len(x) == 0
	case int:
		return x == 0
	case int32:
		return x == 0
	case int64:
		return x == 0
	case uint64:
		return x == 0
	case float64:
		return x == 0
	case bool:
		return !x
	}
	return false
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(v)
	}
}

// Apply 按规则对一个值脱敏。空值（None、空串、0）原样返回；
//   - 保留首尾 / 整体隐藏 / 三段式：把值转成文本后处理，结果一律是文本；
//   - 正则：命中时把“隐藏分组”换成等长的 *，其余分组保持不变，不匹配则原样返回（旧规则语义）；
//   - 嵌套的 map / 数组（JSON、Map 类型的列）：对其中的每个叶子值逐个处理，键名不变。
func Apply(v any, r Rule) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			m, err := Apply(e, r)
			if err != nil {
				return nil, err
			}
			out[k] = m
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			m, err := Apply(e, r)
			if err != nil {
				return nil, err
			}
			out[i] = m
		}
		return out, nil
	}
	if isEmpty(v) {
		return v, nil
	}
	switch r.Kind {
	case KindKeep:
		return keepEnds(str(v), r.Prefix, r.Suffix), nil
	case KindWhole:
		return Mask, nil
	case KindThree:
		return threeSegment(str(v), r.HideGroup), nil
	}
	if r.Kind == "" && r.Type == RuleThreeSegment {
		if s, ok := v.(string); ok {
			return threeSegment(s, r.HideGroup), nil
		}
	}
	return applyRegex(v, r)
}

func applyRegex(v any, r Rule) (any, error) {
	re, err := compileRegex(r.Regex, "i")
	if err != nil {
		return nil, err
	}
	s := str(v)
	m := re.FindStringSubmatchIndex(s)
	if m == nil {
		return v, nil
	}
	last := 0 // 最后一个参与匹配的分组
	for g := 1; g <= re.NumSubexp(); g++ {
		if m[2*g] >= 0 {
			last = g
		}
	}
	var b strings.Builder
	for g := 1; g <= last; g++ {
		if m[2*g] < 0 {
			return nil, fmt.Errorf("脱敏正则的第 %d 组没有参与匹配", g)
		}
		part := s[m[2*g]:m[2*g+1]]
		if g == r.HideGroup {
			b.WriteString(strings.Repeat("*", utf8.RuneCountInString(part)))
		} else {
			b.WriteString(part)
		}
	}
	return b.String(), nil
}

// ApplyJSONText 对“JSON 文本”（PostgreSQL json / jsonb、ClickHouse Map / Array / Tuple 被整形成的文本）里的
// 每个叶子字符串 / 数字逐个脱敏，再序列化回紧凑 JSON。不是合法 JSON 对象 / 数组时返回 false，由调用方按普通文本处理。
func ApplyJSONText(s string, r Rule) (string, bool, error) {
	t := strings.TrimSpace(s)
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return s, false, nil
	}
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil || dec.More() {
		return s, false, nil
	}
	masked, err := Apply(normalizeNumbers(tree), r)
	if err != nil {
		return "", false, err
	}
	b, err := json.Marshal(masked)
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// normalizeNumbers 把 json.Number 转成文本，让数字叶子也按“值”脱敏。
func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = normalizeNumbers(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = normalizeNumbers(e)
		}
		return x
	case json.Number:
		return x.String()
	}
	return v
}

// keepEnds 保留前 n 位和后 m 位（按字符计），中间固定显示 ***。
// 值太短时少保留：至少隐藏 1 个字符，先保证前缀、再缩后缀。
func keepEnds(s string, n, m int) string {
	runes := []rune(s)
	l := len(runes)
	if l == 0 {
		return s
	}
	if n < 0 {
		n = 0
	}
	if m < 0 {
		m = 0
	}
	p := min(n, l-1)
	q := min(m, l-1-p)
	if q < 0 {
		q = 0
	}
	return string(runes[:p]) + Mask + string(runes[l-q:])
}

// threeSegment 是三段式通用规则：把字符串按长度分成三段，较长的段排在后面，隐藏 hide 指定的一段。
// 语义等同于动态正则 ^([\s\S]{a,}?)([\s\S]{b,}?)([\s\S]{c,}?)$：三段长度之和恰好等于总长度，
// 懒惰量词因此每段都取最小长度；这里直接按长度计算，避开 Go 正则 {n,} 的 1000 上限。
func threeSegment(s string, hide int) string {
	runes := []rune(s)
	n := len(runes)
	avg, rem := n/3, n%3
	lens := [3]int{avg, avg, avg}
	if rem > 0 {
		lens[1]++
	}
	if rem > 1 {
		lens[2]++
	}
	var b strings.Builder
	pos := 0
	for g := 1; g <= 3; g++ {
		seg := runes[pos : pos+lens[g-1]]
		pos += lens[g-1]
		if g == hide {
			b.WriteString(strings.Repeat("*", len(seg)))
		} else {
			b.WriteString(string(seg))
		}
	}
	return b.String()
}

func compileRegex(expr, flags string) (*regexp.Regexp, error) {
	re, err := regexp.Compile("(?" + flags + ")" + expr)
	if err != nil {
		return nil, fmt.Errorf("脱敏规则的正则不合法（%s）：%v", expr, err)
	}
	return re, nil
}

// Validate 校验一条规则的参数是否合法（保存配置前调用）。
func (r Rule) Validate() error {
	switch r.Kind {
	case KindKeep:
		if r.Prefix < 0 || r.Prefix > 64 || r.Suffix < 0 || r.Suffix > 64 {
			return errors.New("保留位数需在 0～64 之间")
		}
	case KindThree:
		if r.HideGroup < 1 || r.HideGroup > 3 {
			return errors.New("三段式要隐藏的段需是 1（前）、2（中）或 3（后）")
		}
	case KindWhole:
	case KindRegex:
		if strings.TrimSpace(r.Regex) == "" || len(r.Regex) > 255 {
			return errors.New("请填写正则（不超过 255 个字符）")
		}
		re, err := compileRegex(r.Regex, "i")
		if err != nil {
			return err
		}
		if r.HideGroup < 1 || r.HideGroup > re.NumSubexp() {
			return fmt.Errorf("要隐藏的分组需在 1～%d 之间（正则里共有 %d 个分组）", max(re.NumSubexp(), 1), re.NumSubexp())
		}
	default:
		return fmt.Errorf("不认识的规则种类 %q", r.Kind)
	}
	return nil
}

// Strictness 返回规则的“严格程度”，用于一个字段名对应多条配置却分不清来源时取最严的：越大越严。
func (r Rule) Strictness() int {
	switch r.Kind {
	case KindWhole:
		return 1000
	case KindKeep:
		return 500 - min(r.Prefix+r.Suffix, 400)
	case KindThree, KindRegex:
		return 300
	}
	return 300 // 旧规则
}
