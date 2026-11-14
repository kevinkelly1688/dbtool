// @author: kevinkelly
// @license: Apache Licence
// @file: names.go
// @time: 2026/10/28

package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var placeholderRe = regexp.MustCompile(`\{[^{}]*\}`)

var tableTplVars = map[string]bool{"table": true, "yyyy": true, "mm": true, "dd": true, "yyyymm": true, "yyyymmdd": true}
var pathTplVars = map[string]bool{"inst": true, "db": true, "table": true, "yyyy": true, "mm": true, "dd": true,
	"yyyymm": true, "yyyymmdd": true, "run_id": true, "task_id": true}

// ValidTableTemplate 校验库表备份的表名模板：只含字母、数字、下划线和 {table} {yyyy} {mm} {dd} {yyyymm} {yyyymmdd}，展开后不超过 64 位。
func ValidTableTemplate(tpl string) error {
	if strings.TrimSpace(tpl) == "" {
		return fmt.Errorf("目标表名模板不能为空")
	}
	for _, r := range tpl {
		if !(r == '_' || r == '{' || r == '}' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("目标表名模板只能含字母、数字、下划线与 {table} {yyyy} {mm} {dd} {yyyymm}")
		}
	}
	if err := checkPlaceholders(tpl, tableTplVars); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(expandTableNameRaw(tpl, "", refTime)); n > 64 {
		return fmt.Errorf("目标表名模板展开后超过 64 位")
	}
	return nil
}

func checkPlaceholders(tpl string, vars map[string]bool) error {
	for _, m := range placeholderRe.FindAllString(tpl, -1) {
		if !vars[m[1:len(m)-1]] {
			return fmt.Errorf("模板里有不认识的占位符 %s", m)
		}
	}
	if strings.ContainsAny(placeholderRe.ReplaceAllString(tpl, ""), "{}") {
		return fmt.Errorf("模板里的花括号不成对")
	}
	return nil
}

func timeVars(at time.Time) map[string]string {
	return map[string]string{
		"yyyy": fmt.Sprintf("%04d", at.Year()), "mm": fmt.Sprintf("%02d", int(at.Month())), "dd": fmt.Sprintf("%02d", at.Day()),
		"yyyymm":   fmt.Sprintf("%04d%02d", at.Year(), int(at.Month())),
		"yyyymmdd": fmt.Sprintf("%04d%02d%02d", at.Year(), int(at.Month()), at.Day()),
	}
}

func expand(tpl string, vars map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(tpl, func(m string) string {
		if v, ok := vars[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

// sanitizeName 把不适合作为表名的字符换成下划线，并截到 64 个字符。
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if rs := []rune(out); len(rs) > 64 {
		out = string(rs[:64])
	}
	return out
}

// expandTableNameRaw 展开模板但不清理、不截断（用来判断长度是否超限）。
func expandTableNameRaw(tpl, table string, at time.Time) string {
	if strings.TrimSpace(tpl) == "" {
		tpl = "{table}_archive"
	}
	v := timeVars(at)
	v["table"] = table
	return expand(tpl, v)
}

func expandTableName(tpl, table string, at time.Time) string {
	return sanitizeName(expandTableNameRaw(tpl, table, at))
}

// sanitizeSegment 清理路径片段里的值：只保留字母数字和 _ - .，且不允许出现 ".."。
func sanitizeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.ReplaceAll(b.String(), "..", "__")
	return out
}

// cleanKey 把模板展开后的路径规整成相对路径：去掉空段和 "."，".." 段换成 "_"，统一用 "/"。
func cleanKey(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	var segs []string
	for _, s := range strings.Split(p, "/") {
		switch s {
		case "", ".":
			continue
		case "..":
			s = "_"
		}
		segs = append(segs, s)
	}
	return strings.Join(segs, "/")
}

func expandPath(tpl string, spec Spec, instName string, at time.Time) string {
	if strings.TrimSpace(tpl) == "" {
		tpl = "{inst}/{db}/{table}/{yyyy}/{mm}/{dd}/{run_id}"
	}
	v := timeVars(at)
	v["inst"] = sanitizeSegment(instName)
	v["db"] = sanitizeSegment(spec.DB)
	v["table"] = sanitizeSegment(spec.Table)
	v["run_id"] = strconv.FormatUint(uint64(spec.RunID), 10)
	v["task_id"] = strconv.FormatUint(uint64(spec.TaskID), 10)
	// 模板本身的字面部分也要清理，只有占位符展开后的值是可信的
	return cleanKey(sanitizeTemplateLiterals(tpl, v))
}

// sanitizeTemplateLiterals 先展开占位符（值已清理），字面部分中的非法字符换成下划线（保留 / 作为分隔）。
func sanitizeTemplateLiterals(tpl string, vars map[string]string) string {
	var out strings.Builder
	last := 0
	for _, loc := range placeholderRe.FindAllStringIndex(tpl, -1) {
		out.WriteString(sanitizeLiteral(tpl[last:loc[0]]))
		m := tpl[loc[0]:loc[1]]
		if v, ok := vars[m[1:len(m)-1]]; ok {
			out.WriteString(v)
		} else {
			out.WriteString(sanitizeLiteral(m))
		}
		last = loc[1]
	}
	out.WriteString(sanitizeLiteral(tpl[last:]))
	return out.String()
}

func sanitizeLiteral(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == '/' {
			b.WriteRune(r)
		} else if r == '\\' {
			b.WriteByte('/')
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ValidPathTemplate 校验文件路径模板：只含字母数字 _ - . / 和 {inst} {db} {table} {yyyy} {mm} {dd} {yyyymm} {yyyymmdd} {run_id} {task_id}，
// 不能以 / 开头，不能含 ".."，不超过 128 位。
func ValidPathTemplate(tpl string) error {
	if strings.TrimSpace(tpl) == "" {
		return fmt.Errorf("路径模板不能为空")
	}
	if utf8.RuneCountInString(tpl) > 128 {
		return fmt.Errorf("路径模板最长 128 位")
	}
	for _, r := range tpl {
		if !(r == '_' || r == '-' || r == '.' || r == '/' || r == '{' || r == '}' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("路径模板只能含字母、数字、_ - . / 与 {inst} {db} {table} {yyyy} {mm} {dd} {run_id}")
		}
	}
	if strings.HasPrefix(tpl, "/") {
		return fmt.Errorf("路径模板不能以 / 开头")
	}
	if strings.Contains(tpl, "..") {
		return fmt.Errorf("路径模板不能含 ..")
	}
	return checkPlaceholders(tpl, pathTplVars)
}

// refTime 是校验时展开模板用的参考时间。
var refTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
