// @author: kevinkelly
// @license: Apache Licence
// @file: rule.go
// @time: 2026/10/28

package firewall

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"dbtool/internal/instext"
)

// Error 是规则校验错误（面向管理员的提示）。
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

var (
	stmtKinds = map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true}
	features  = map[string]string{
		FeatNoWhere: "没有 WHERE", FeatNoLimit: "没有 LIMIT", FeatCartesian: "笛卡尔积", FeatMultiStmt: "一次提交多条语句",
		FeatSelectStar: "SELECT *", FeatLeadingWildcard: "LIKE 以 % 开头", FeatBigIn: "IN 列表过长",
	}
	fingerprintRe = regexp.MustCompile(`^[0-9a-f]{8}$`)
	patternPartRe = regexp.MustCompile(`^[^\s.]+$`)
)

// FeatureLabel 返回结构特征的显示名。
func FeatureLabel(f string) string { return features[f] }

// Normalize 校验并规整一条规则的用户输入（ID / 创建人 / 时间戳由存储层填）。返回的规则里条件的取值已转成规范形态
// （语句类型大写、关键字去空白、对象模式补全为 库.表[.列]、数字转 int64）。
func Normalize(in Rule) (Rule, error) {
	r := in
	r.Name = strings.TrimSpace(r.Name)
	r.Description = strings.TrimSpace(r.Description)
	r.Message = strings.TrimSpace(r.Message)
	switch {
	case r.Name == "":
		return r, invalid("请填写规则名称")
	case len([]rune(r.Name)) > MaxNameLen:
		return r, invalid("规则名称过长（最多 %d 个字符）", MaxNameLen)
	case len([]rune(r.Description)) > MaxTextLen:
		return r, invalid("说明过长（最多 %d 个字符）", MaxTextLen)
	case len([]rune(r.Message)) > MaxTextLen:
		return r, invalid("提示文案过长（最多 %d 个字符）", MaxTextLen)
	}
	if r.Priority < 0 || r.Priority > 100000 {
		return r, invalid("优先级需在 0–100000 之间")
	}
	if r.Mode == "" {
		r.Mode = ModeObserve
	}
	if r.Mode != ModeObserve && r.Mode != ModeEnforce {
		return r, invalid("模式只能是 observe 或 enforce")
	}

	// 范围
	seen := map[string]bool{}
	var scenes []string
	for _, s := range r.Scenes {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		if !contains(Scenes, s) {
			return r, invalid("不认识的场景 \"%s\"", s)
		}
		seen[s] = true
		scenes = append(scenes, s)
	}
	if len(scenes) == 0 {
		return r, invalid("至少选择一个适用场景")
	}
	sort.Slice(scenes, func(i, j int) bool { return indexOf(Scenes, scenes[i]) < indexOf(Scenes, scenes[j]) })
	r.Scenes = scenes
	if r.Env == "" {
		r.Env = "all"
	}
	if r.Env != "all" && !instext.ValidEnv(r.Env) {
		return r, invalid("实例环境只能是 all 或环境字典里已有的环境编码")
	}
	r.InstanceIDs = uniqUint(r.InstanceIDs)
	if len(r.InstanceIDs) > 200 {
		return r, invalid("最多限定 200 个实例")
	}
	if r.GroupMode == "" {
		r.GroupMode = "all"
	}
	switch r.GroupMode {
	case "all":
		r.GroupIDs = nil
	case "only", "except":
		r.GroupIDs = uniqUint(r.GroupIDs)
		if len(r.GroupIDs) == 0 {
			return r, invalid("请选择权限组")
		}
	default:
		return r, invalid("用户范围只能是 all / only / except")
	}
	if r.Match == "" {
		r.Match = "all"
	}
	if r.Match != "all" && r.Match != "any" {
		return r, invalid("条件组合只能是 all（全部满足）或 any（任一满足）")
	}

	// 条件
	if len(r.Conditions) == 0 {
		return r, invalid("至少需要一个命中条件")
	}
	if len(r.Conditions) > MaxConditions {
		return r, invalid("一条规则最多 %d 个条件", MaxConditions)
	}
	conds := make([]Condition, 0, len(r.Conditions))
	for i, c := range r.Conditions {
		nc, err := normalizeCondition(c)
		if err != nil {
			return r, invalid("第 %d 个条件：%v", i+1, err)
		}
		conds = append(conds, nc)
	}
	r.Conditions = conds

	// 动作
	if !contains(Actions, r.Action) {
		return r, invalid("不认识的动作 \"%s\"", r.Action)
	}
	has := func(s string) bool { return contains(r.Scenes, s) }
	switch r.Action {
	case ActLimit:
		if r.ActionArg < 1 || r.ActionArg > MaxLimitRows {
			return r, invalid("限制返回行数需在 1–%d 之间", MaxLimitRows)
		}
		if !has(SceneQuery) && !has(SceneExport) {
			return r, invalid("“限制返回行数”只适用于在线查询和数据导出")
		}
	case ActMask:
		if !has(SceneQuery) && !has(SceneExport) {
			return r, invalid("“强制脱敏”只适用于在线查询和数据导出")
		}
		if len(forceRefs(conds)) == 0 {
			return r, invalid("“强制脱敏”需要至少一个带列名的对象条件，如 *.*.id_card 或 shop.users.phone（库与表要么都写具体名字、要么都是 *）")
		}
		r.ActionArg = 0
	case ActReason:
		if !has(SceneQuery) {
			return r, invalid("“要求填写原因”只适用于在线查询")
		}
		r.ActionArg = 0
	default:
		r.ActionArg = 0
	}
	return r, nil
}

func normalizeCondition(c Condition) (Condition, error) {
	c.T = strings.TrimSpace(c.T)
	switch c.T {
	case CondStmt:
		list, err := toStrings(c.V)
		if err != nil || len(list) == 0 {
			return c, invalid("语句类型不能为空")
		}
		var out []string
		seen := map[string]bool{}
		for _, s := range list {
			s = strings.ToUpper(strings.TrimSpace(s))
			if !stmtKinds[s] {
				return c, invalid("不认识的语句类型 \"%s\"（可选 SELECT / INSERT / UPDATE / DELETE / CREATE / ALTER / DROP / TRUNCATE）", s)
			}
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
		c.V = out
	case CondFeature:
		s, ok := c.V.(string)
		if !ok || features[strings.TrimSpace(s)] == "" {
			return c, invalid("不认识的结构特征 %v", c.V)
		}
		c.V = strings.TrimSpace(s)
	case CondKeyword:
		list, err := toStrings(c.V)
		if err != nil || len(list) == 0 {
			return c, invalid("关键字不能为空")
		}
		if len(list) > MaxListItems {
			return c, invalid("关键字最多 %d 个", MaxListItems)
		}
		var out []string
		for _, s := range list {
			s = strings.Join(strings.Fields(s), " ")
			if s == "" {
				continue
			}
			if len([]rune(s)) > 100 {
				return c, invalid("关键字过长：%.20s…", s)
			}
			out = append(out, s)
		}
		if len(out) == 0 {
			return c, invalid("关键字不能为空")
		}
		c.V = out
	case CondRegex:
		s, ok := c.V.(string)
		s = strings.TrimSpace(s)
		if !ok || s == "" {
			return c, invalid("正则不能为空")
		}
		if len(s) > MaxPatternLen {
			return c, invalid("正则过长（最多 %d 个字符）", MaxPatternLen)
		}
		if _, err := regexp.Compile("(?i)" + s); err != nil {
			return c, invalid("正则无效：%v", err)
		}
		c.V = s
	case CondObject:
		list, err := toStrings(c.V)
		if err != nil || len(list) == 0 {
			return c, invalid("涉及对象不能为空")
		}
		if len(list) > MaxListItems {
			return c, invalid("对象模式最多 %d 个", MaxListItems)
		}
		var out []string
		for _, p := range list {
			n, err := normalizePattern(p)
			if err != nil {
				return c, err
			}
			out = append(out, n)
		}
		c.V = out
	case CondScanRows, CondAffectRows:
		n, ok := toInt64(c.V)
		if !ok || n < 1 || n > 1_000_000_000_000 {
			return c, invalid("行数阈值需是正整数")
		}
		c.V = n
	case CondTime:
		s, _ := c.V.(string)
		if s != "offhours" && s != "workhours" {
			return c, invalid("时间只能是 offhours（非工作时间）或 workhours（工作时间）")
		}
	case CondFingerprint:
		s, _ := c.V.(string)
		s = strings.ToLower(strings.TrimSpace(s))
		if !fingerprintRe.MatchString(s) {
			return c, invalid("语句指纹是 8 位十六进制")
		}
		c.V = s
	default:
		return c, invalid("不认识的条件类型 \"%s\"", c.T)
	}
	return c, nil
}

// normalizePattern 把对象模式规整为 库.表 或 库.表.列：只写表名视为 *.表；各段不能为空、不能含空白；* 是通配。
func normalizePattern(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || len(p) > MaxPatternLen {
		return "", invalid("对象模式为空或过长")
	}
	parts := strings.Split(strings.ReplaceAll(strings.ReplaceAll(p, "`", ""), `"`, ""), ".")
	switch len(parts) {
	case 1:
		parts = []string{"*", parts[0]}
	case 2, 3:
	default:
		return "", invalid("对象模式 \"%s\" 最多写 库.表.列 三段", p)
	}
	for _, s := range parts {
		if !patternPartRe.MatchString(s) {
			return "", invalid("对象模式 \"%s\" 里有空的段或空白字符", p)
		}
	}
	return strings.Join(parts, "."), nil
}

// splitPattern 返回 库、表、列（没有列段时为空）。模式必须是 normalizePattern 的输出。
func splitPattern(p string) (db, tb, col string) {
	parts := strings.Split(p, ".")
	db, tb = parts[0], parts[1]
	if len(parts) == 3 {
		col = parts[2]
	}
	return
}

// forceRefs 从 object 条件里取出可用于强制脱敏的 库.表.列：必须有列段，且库与表要么都具体、要么都是 *
// （脱敏配置的匹配语义只有这两种）。
func forceRefs(conds []Condition) []ObjectRef {
	var out []ObjectRef
	seen := map[ObjectRef]bool{}
	for _, c := range conds {
		if c.T != CondObject {
			continue
		}
		list, _ := toStrings(c.V)
		for _, p := range list {
			db, tb, col := splitPattern(p)
			if col == "" || col == "*" || (db == "*") != (tb == "*") {
				continue
			}
			ref := ObjectRef{Schema: db, Table: tb, Column: col}
			if !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}
	return out
}

// ParseConditions 解析库里的条件 JSON 文本。
func ParseConditions(s string) ([]Condition, error) {
	var out []Condition
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, invalid("条件不是合法的 JSON：%v", err)
	}
	return out, nil
}

// ConditionsJSON 把条件序列化成库里存的 JSON 文本。
func ConditionsJSON(c []Condition) string {
	b, _ := json.Marshal(c)
	return string(b)
}

func toStrings(v any) ([]string, error) {
	switch x := v.(type) {
	case []string:
		return x, nil
	case string:
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, invalid("列表里只能是文本")
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, invalid("取值需要是文本列表")
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		if x == float64(int64(x)) {
			return int64(x), true
		}
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	}
	return 0, false
}

func contains(list []string, s string) bool { return indexOf(list, s) >= 0 }

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

func uniqUint(in []uint) []uint {
	if len(in) == 0 {
		return nil
	}
	seen := map[uint]bool{}
	var out []uint
	for _, x := range in {
		if x != 0 && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
