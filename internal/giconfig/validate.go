// @author: kevinkelly
// @license: Apache Licence
// @file: validate.go
// @time: 2026/10/28

package giconfig

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// MaxContentBytes 是配置文件大小上限；goInception 的 config.toml 通常只有几 KB。
const MaxContentBytes = 1 << 20

// Issue 是校验发现的一个问题。Level 为 error（必须修正才能保存）或 warn（需要用户确认）。
type Issue struct {
	Level   string `json:"level"`
	Key     string `json:"key,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

const (
	LevelError = "error"
	LevelWarn  = "warn"
)

// Report 是一次校验的结果。
type Report struct {
	Issues   []Issue `json:"issues"`
	Errors   int     `json:"errors"`
	Warnings int     `json:"warnings"`
}

func (r *Report) add(level, key string, line int, format string, a ...any) {
	r.Issues = append(r.Issues, Issue{Level: level, Key: key, Line: line, Message: fmt.Sprintf(format, a...)})
	if level == LevelError {
		r.Errors++
	} else {
		r.Warnings++
	}
}

// OK 表示没有错误（可以有警告）。
func (r Report) OK() bool { return r.Errors == 0 }

func flatten(prefix string, m map[string]any, out map[string]any) {
	for k, v := range m {
		id := k
		if prefix != "" {
			id = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			flatten(id, sub, out)
			continue
		}
		out[id] = v
	}
}

// ParseValues 把 TOML 解析成 “段.参数名 → 值” 的平面表。
func ParseValues(src string) (map[string]any, error) {
	var m map[string]any
	if err := toml.Unmarshal([]byte(normalizeNewlines(src)), &m); err != nil {
		return nil, err
	}
	out := map[string]any{}
	flatten("", m, out)
	return out, nil
}

func typeName(v any) string {
	switch v.(type) {
	case bool:
		return "布尔值"
	case int64:
		return "整数"
	case float64:
		return "小数"
	case string:
		return "字符串"
	case []any:
		return "数组"
	}
	return fmt.Sprintf("%T", v)
}

// Validate 检查配置文件：先看 TOML 语法，再按参数目录检查类型与取值范围；
// 未知的段/参数和有风险的取值给警告而不是报错。
func Validate(src string) Report {
	r := Report{Issues: []Issue{}}
	if len(src) > MaxContentBytes {
		r.add(LevelError, "", 0, "配置文件过大（超过 %d KB）", MaxContentBytes/1024)
		return r
	}
	if strings.ContainsRune(src, 0) {
		r.add(LevelError, "", 0, "配置文件含有不可见的 NUL 字符，请检查是不是粘贴了二进制内容")
		return r
	}
	norm := normalizeNewlines(src)
	if strings.TrimSpace(norm) == "" {
		r.add(LevelError, "", 0, "配置文件不能为空")
		return r
	}
	var m map[string]any
	if err := toml.Unmarshal([]byte(norm), &m); err != nil {
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			r.add(LevelError, "", row, "TOML 语法错误（第 %d 行第 %d 列）：%s", row, col, firstLine(de.Error()))
		} else {
			r.add(LevelError, "", 0, "TOML 语法错误：%s", firstLine(err.Error()))
		}
		return r
	}
	vals := map[string]any{}
	flatten("", m, vals)
	lines := lineIndex(norm)

	ids := make([]string, 0, len(vals))
	for id := range vals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	warnedSection := map[string]bool{}
	for _, id := range ids {
		v := vals[id]
		line := lines[id]
		section, key := "", id
		if i := strings.LastIndex(id, "."); i >= 0 {
			section, key = id[:i], id[i+1:]
		}
		if IsSensitiveKey(key) {
			if s, ok := v.(string); ok && s == MaskPlaceholder {
				r.add(LevelError, id, line, "%s 的值还是页面上的占位符 %s，请填写真实密码或保持页面原样提交", id, MaskPlaceholder)
				continue
			}
		}
		if section == "inc_level" {
			// 审核级别段：键是错误码名，值是整数级别（上游默认配置里只出现 1 和 2）
			n, isInt := v.(int64)
			switch {
			case !isInt:
				r.add(LevelError, id, line, "%s 应为整数（审核级别），实际是%s；类型不对会导致 goInception 无法启动", id, typeName(v))
			case n < 0 || n > 2:
				r.add(LevelWarn, id, line, "%s 的审核级别 %d 不在 0–2 之内，请对照 goInception 文档确认", id, n)
			}
			continue
		}
		p := params[id]
		if p == nil {
			if knownGeneric[id] {
				continue
			}
			switch {
			case section != "" && !knownSections[section]:
				if !warnedSection[section] {
					warnedSection[section] = true
					r.add(LevelWarn, section, line, "未知的配置段 [%s]：不是 goInception 已知的配置段，请确认没有写错", section)
				}
			case section == "":
				r.add(LevelWarn, id, line, "顶层参数 %s 不在已知参数列表里，请对照所用 goInception 版本的文档确认", id)
			default:
				r.add(LevelWarn, id, line, "未知参数 %s：不在 [%s] 的已知参数列表里，可能是拼写错误，或是当前版本新增的参数", key, section)
			}
			continue
		}
		checkParam(&r, p, id, line, v)
	}
	sort.SliceStable(r.Issues, func(i, j int) bool { return r.Issues[i].Line < r.Issues[j].Line })
	return r
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func checkParam(r *Report, p *param, id string, line int, v any) {
	bad := func(want string) {
		r.add(LevelError, id, line, "%s 应为%s，实际是%s；类型不对会导致 goInception 无法启动", id, want, typeName(v))
	}
	switch p.Kind {
	case kBool:
		if _, ok := v.(bool); !ok {
			bad("布尔值（true / false，不加引号）")
			return
		}
	case kInt:
		n, ok := v.(int64)
		if !ok {
			bad("整数")
			return
		}
		checkRange(r, p, id, line, float64(n))
	case kFloat:
		switch n := v.(type) {
		case float64:
			checkRange(r, p, id, line, n)
		case int64:
			checkRange(r, p, id, line, float64(n))
		default:
			bad("数字")
			return
		}
	case kString:
		if _, ok := v.(string); !ok {
			bad("字符串（加引号）")
			return
		}
	case kEnum:
		s, ok := v.(string)
		if !ok {
			bad("字符串（加引号）")
			return
		}
		valid := false
		for _, e := range p.Enum {
			valid = valid || e == s
		}
		if !valid {
			r.add(LevelError, id, line, "%s 的值 \"%s\" 不合法，可选：%s", id, s, strings.Join(p.Enum, " / "))
			return
		}
	case kList:
		arr, ok := v.([]any)
		if ok {
			for _, e := range arr {
				if _, isStr := e.(string); !isStr {
					ok = false
					break
				}
			}
		}
		if !ok {
			bad("字符串数组（如 [\"a\", \"b\"]）")
			return
		}
	}
	if p.Warn != nil {
		if w := p.Warn(v); w != "" {
			r.add(LevelWarn, id, line, "%s", w)
		}
	}
}

func checkRange(r *Report, p *param, id string, line int, n float64) {
	if (p.Min != nil && n < *p.Min) || (p.Max != nil && n > *p.Max) {
		r.add(LevelError, id, line, "%s 的值 %g 超出范围（%s）", id, n, describeRange(p))
	}
}

func displayValue(id string, v any) string {
	if IsSensitiveKey(id) {
		return "******"
	}
	if s, ok := v.(string); ok {
		r := []rune(s)
		if len(r) > 40 {
			s = string(r[:40]) + "…"
		}
		return `"` + s + `"`
	}
	return fmt.Sprint(v)
}

// Summarize 生成“变更摘要”，如 `check_dml_where: false → true；max_update_rows: 5000 → 10000`。
func Summarize(oldSrc, newSrc string) string {
	ov, err1 := ParseValues(oldSrc)
	nv, err2 := ParseValues(newSrc)
	if err1 != nil || err2 != nil {
		return "配置文件内容变更" // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	}
	var parts []string
	for id, n := range nv {
		o, had := ov[id]
		short := id
		if i := strings.LastIndex(id, "."); i >= 0 {
			short = id[i+1:]
		}
		switch {
		case !had:
			parts = append(parts, fmt.Sprintf("%s: 新增 %s", short, displayValue(id, n))) // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
		case fmt.Sprint(o) != fmt.Sprint(n):
			if IsSensitiveKey(id) {
				parts = append(parts, short+": 已修改") // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
			} else {
				parts = append(parts, fmt.Sprintf("%s: %s → %s", short, displayValue(id, o), displayValue(id, n)))
			}
		}
	}
	for id := range ov {
		if _, ok := nv[id]; !ok {
			short := id
			if i := strings.LastIndex(id, "."); i >= 0 {
				short = id[i+1:]
			}
			parts = append(parts, short+": 已删除") // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
		}
	}
	if len(parts) == 0 {
		return "仅注释或排版变更" // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	}
	sort.Strings(parts)
	out := strings.Join(parts, "；")
	if r := []rune(out); len(r) > 200 {
		out = string(r[:200]) + fmt.Sprintf("…（共 %d 项）", len(parts)) // i18n-ignore 审计文本 / 存库的版本摘要，不翻译
	}
	return out
}
