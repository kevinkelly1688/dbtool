// @author: kevinkelly
// @license: Apache Licence
// @file: text.go
// @time: 2026/10/28

package firewalladmin

import (
	"fmt"
	"reflect"
	"strings"

	"dbtool/internal/firewall"
	"dbtool/internal/instext"
	"dbtool/internal/model"
	"dbtool/internal/query"
)

// envOf 由实例标签推断环境（与防火墙求值时用的是同一个函数）。
func envOf(in model.Instance) string { return query.EnvOfInstance(in) }

var actionLabels = map[string]string{
	firewall.ActDeny:   "拒绝",     // i18n-ignore 审计文本，存库后不翻译
	firewall.ActReason: "要求填写原因", // i18n-ignore 审计文本，存库后不翻译
	firewall.ActLimit:  "限制返回行数", // i18n-ignore 审计文本，存库后不翻译
	firewall.ActMask:   "强制脱敏",   // i18n-ignore 审计文本，存库后不翻译
	firewall.ActWarn:   "仅告警",    // i18n-ignore 审计文本，存库后不翻译
}

func actionLabel(a string) string {
	if l, ok := actionLabels[a]; ok {
		return l
	}
	return a
}

// strList 取条件取值里的字符串列表（规整后是 []string，从库里读回来是 []any）。
func strList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{x}
	}
	return nil
}

func numText(v any) string {
	switch x := v.(type) {
	case int64:
		return fmt.Sprint(x)
	case int:
		return fmt.Sprint(x)
	case float64:
		return fmt.Sprintf("%.0f", x)
	}
	return fmt.Sprint(v)
}

// condText 是一个条件的中文说明（审计与命中说明用）。
func condText(c firewall.Condition) string {
	switch c.T {
	case firewall.CondStmt:
		return "语句类型 " + strings.Join(strList(c.V), " / ") // i18n-ignore 审计文本，存库后不翻译
	case firewall.CondFeature:
		if s, ok := c.V.(string); ok {
			if l := firewall.FeatureLabel(s); l != "" {
				return "语句" + l // i18n-ignore 审计文本，存库后不翻译
			}
			return s
		}
	case firewall.CondKeyword:
		return "含 " + strings.Join(strList(c.V), " / ") // i18n-ignore 审计文本，存库后不翻译
	case firewall.CondRegex:
		return "正则 " + fmt.Sprint(c.V) // i18n-ignore 审计文本，存库后不翻译
	case firewall.CondObject:
		return "涉及 " + strings.Join(strList(c.V), "、") // i18n-ignore 审计文本，存库后不翻译
	case firewall.CondScanRows:
		return "预估扫描 ≥ " + numText(c.V) + " 行" // i18n-ignore 审计文本，存库后不翻译
	case firewall.CondAffectRows:
		return "预估影响 ≥ " + numText(c.V) + " 行" // i18n-ignore 审计文本，存库后不翻译
	case firewall.CondTime:
		if c.V == "offhours" {
			return "非工作时间" // i18n-ignore 审计文本，存库后不翻译
		}
		return "工作时间" // i18n-ignore 审计文本，存库后不翻译
	case firewall.CondFingerprint:
		return "指纹 " + fmt.Sprint(c.V) // i18n-ignore 审计文本，存库后不翻译
	}
	return c.T
}

func scenesText(scenes []string) string {
	out := make([]string, 0, len(scenes))
	for _, s := range scenes {
		if l, ok := sceneLabels[s]; ok {
			out = append(out, l)
		} else {
			out = append(out, s)
		}
	}
	return strings.Join(out, "、")
}

func envText(env string) string {
	if env == "" || env == "all" {
		return "全部实例" // i18n-ignore 审计文本，存库后不翻译
	}
	return "仅" + instext.EnvLabel(env) + "实例" // i18n-ignore 审计文本，存库后不翻译
}

func condsText(r firewall.Rule) string {
	parts := make([]string, 0, len(r.Conditions))
	for _, c := range r.Conditions {
		parts = append(parts, condText(c))
	}
	join := " 且 " // i18n-ignore 审计文本，存库后不翻译
	if r.Match == "any" {
		join = " 或 " // i18n-ignore 审计文本，存库后不翻译
	}
	return strings.Join(parts, join)
}

func actionText(r firewall.Rule) string {
	t := actionLabel(r.Action)
	if r.Action == firewall.ActLimit && r.ActionArg > 0 {
		t += fmt.Sprintf("（最多 %d 行）", r.ActionArg) // i18n-ignore 审计文本，存库后不翻译
	}
	return t
}

func modeText(m string) string {
	if m == firewall.ModeEnforce {
		return "生效" // i18n-ignore 审计文本，存库后不翻译
	}
	return "观察" // i18n-ignore 审计文本，存库后不翻译
}

// ruleSummary 是一条规则的一句话描述（审计用）。
func ruleSummary(r firewall.Rule) string {
	parts := []string{"场景 " + scenesText(r.Scenes), envText(r.Env)} // i18n-ignore 审计文本，存库后不翻译
	if n := len(r.InstanceIDs); n > 0 {
		parts = append(parts, fmt.Sprintf("限定 %d 个实例", n)) // i18n-ignore 审计文本，存库后不翻译
	}
	switch r.GroupMode {
	case "only":
		parts = append(parts, fmt.Sprintf("仅 %d 个权限组", len(r.GroupIDs))) // i18n-ignore 审计文本，存库后不翻译
	case "except":
		parts = append(parts, fmt.Sprintf("排除 %d 个权限组", len(r.GroupIDs))) // i18n-ignore 审计文本，存库后不翻译
	}
	parts = append(parts, "条件："+condsText(r), "动作 "+actionText(r), "模式 "+modeText(r.Mode)) // i18n-ignore 审计文本，存库后不翻译
	if !r.Enabled {
		parts = append(parts, "已停用") // i18n-ignore 审计文本，存库后不翻译
	}
	return strings.Join(parts, "；")
}

// diffRules 返回两条规则之间有变化的字段说明；没有变化返回空。比较前先规整，避免 float64 / int64 之类的表示差异造成假变化。
func diffRules(old, cur firewall.Rule) []string {
	if n, err := firewall.Normalize(old); err == nil {
		n.Enabled = old.Enabled
		old = n
	}
	var out []string
	chg := func(label, a, b string) {
		if a != b {
			out = append(out, fmt.Sprintf("%s %s → %s", label, truncate(a, 60), truncate(b, 60)))
		}
	}
	chg("名称", old.Name, cur.Name) // i18n-ignore 审计文本，存库后不翻译
	if old.Description != cur.Description {
		out = append(out, "说明已修改") // i18n-ignore 审计文本，存库后不翻译
	}
	chg("优先级", fmt.Sprint(old.Priority), fmt.Sprint(cur.Priority)) // i18n-ignore 审计文本，存库后不翻译
	chg("启用", fmt.Sprint(old.Enabled), fmt.Sprint(cur.Enabled))    // i18n-ignore 审计文本，存库后不翻译
	chg("模式", modeText(old.Mode), modeText(cur.Mode))              // i18n-ignore 审计文本，存库后不翻译
	chg("场景", scenesText(old.Scenes), scenesText(cur.Scenes))      // i18n-ignore 审计文本，存库后不翻译
	chg("实例环境", envText(old.Env), envText(cur.Env))                // i18n-ignore 审计文本，存库后不翻译
	if !reflect.DeepEqual(nz(old.InstanceIDs), nz(cur.InstanceIDs)) {
		out = append(out, fmt.Sprintf("限定实例 %v → %v", nz(old.InstanceIDs), nz(cur.InstanceIDs))) // i18n-ignore 审计文本，存库后不翻译
	}
	chg("用户范围", old.GroupMode, cur.GroupMode) // i18n-ignore 审计文本，存库后不翻译
	if !reflect.DeepEqual(nz(old.GroupIDs), nz(cur.GroupIDs)) {
		out = append(out, fmt.Sprintf("权限组 %v → %v", nz(old.GroupIDs), nz(cur.GroupIDs))) // i18n-ignore 审计文本，存库后不翻译
	}
	chg("条件组合", old.Match, cur.Match) // i18n-ignore 审计文本，存库后不翻译
	if firewall.ConditionsJSON(old.Conditions) != firewall.ConditionsJSON(cur.Conditions) {
		out = append(out, "条件 "+truncate(condsText(old), 100)+" → "+truncate(condsText(cur), 100)) // i18n-ignore 审计文本，存库后不翻译
	}
	chg("动作", actionText(old), actionText(cur)) // i18n-ignore 审计文本，存库后不翻译
	if old.Message != cur.Message {
		out = append(out, "提示文案已修改") // i18n-ignore 审计文本，存库后不翻译
	}
	chg("通知", fmt.Sprint(old.Notify), fmt.Sprint(cur.Notify)) // i18n-ignore 审计文本，存库后不翻译
	return out
}
