// @author: kevinkelly
// @license: Apache Licence
// @file: localize.go
// @time: 2026/10/28

package partman

import (
	"bytes"
	"encoding/json"
	"strings"

	"dbtool/internal/i18n"
)

// 界面多语言：
//   - 4xx / 5xx 的 message 由 HTTP 边缘统一翻译；
//   - 成功响应里“系统生成”的文案（状态说明、计划警告、运行输出、轮次摘要……）在这里按请求语言翻译，
//     用户数据（库表名、说明、操作人……）不在白名单字段里，原样保留；
//   - 中文请求（含没有语言信息的后台调用）直接返回原值，不做任何处理，输出与迁移前逐字一致。

// textKeys 是响应 JSON 里携带“系统生成文案”的字段名（取值为字符串或字符串数组）。
var textKeys = map[string]bool{
	"message": true, "msg": true, "error": true, "notice": true, "notes": true, "warnings": true, "reason": true,
	"table":    true, // 系统记录的“表”列是一句说明
	"timezone": true, // “Asia/Shanghai（跟随目标库）”
	"bound":    true, // “未知”
	"sql_text": true, // 被省略语句的说明行
	"empty":    true, "formats": true, "dst_warning": true,
}

// localize 把 v（响应载荷）里白名单字段的文案翻译成 lang；lang 为中文时原样返回 v。
func localize(lang string, v any) any {
	if lang == "" || lang == i18n.ZH || v == nil {
		return v
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 大整数不能经 float64 往返
	var out any
	if err := dec.Decode(&out); err != nil {
		return v
	}
	return walkText(lang, out, false)
}

func walkText(lang string, v any, text bool) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = walkText(lang, e, textKeys[k])
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = walkText(lang, e, text)
		}
		return x
	case string:
		if text {
			return tpText(lang, x)
		}
	}
	return v
}

// tpText 翻译一条系统文案：整句（含带格式动词的键）能匹配就用整句；否则它可能是用“；”拼起来的几条原因，
// 逐条翻译后用 "; " 连接。都没有匹配时原样返回。
func tpText(lang, s string) string {
	if out := i18n.TP(lang, s); out != s || !strings.Contains(s, "；") {
		return out
	}
	parts := strings.Split(s, "；")
	changed := false
	for i, p := range parts {
		if q := i18n.TP(lang, p); q != p {
			parts[i], changed = q, true
		}
	}
	if !changed {
		return s
	}
	return strings.Join(parts, "; ")
}
