// @author: kevinkelly
// @license: Apache Licence
// @file: localize.go
// @time: 2026/10/28

// Package localize 在 HTTP 边缘把“成功响应里服务端生成的中文文案”翻译成请求语言。
//
// 错误响应的 message 已由 server 的语言中间件统一翻译；成功响应里的标签、提示、
// 建议等（报告的检查项名称、优化建议、状态说明……）由各业务包拼出来，这里按目录整体翻译：
// 每个字符串先按原文精确查目录，查不到再按带 %s %d 的格式串匹配（i18n.TP）。
//
// 中文请求原样返回（恒等，零开销）；其它语言先把响应转成通用 JSON 树再翻译，不会改动业务对象本身
// （任务结果等对象常驻内存，不能原地改写）。
package localize

import (
	"bytes"
	"encoding/json"
	"strings"

	"dbtool/internal/i18n"
)

// Value 返回翻译后的响应体。skipKeys 里列出的键对应的整棵子树不翻译（前端要拿原文做比较 / 展示的数据，
// 如检查项的明细表）。无法转换时原样返回 v。
func Value(lang string, v any, skipKeys ...string) any {
	if lang == "" || lang == i18n.ZH {
		return v
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 大整数 / 小数保持原样，不经过 float64
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return v
	}
	skip := make(map[string]bool, len(skipKeys))
	for _, k := range skipKeys {
		skip[k] = true
	}
	return walk(lang, tree, skip)
}

// translate 翻译一个字符串；多行文案（如任务提示按行拼接）整体匹配不上时逐行翻译。
func translate(lang, s string) string {
	out := i18n.TP(lang, s)
	if out != s || !strings.Contains(s, "\n") {
		return out
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = i18n.TP(lang, l)
	}
	return strings.Join(lines, "\n")
}

func walk(lang string, x any, skip map[string]bool) any {
	switch v := x.(type) {
	case string:
		return translate(lang, v)
	case []any:
		for i := range v {
			v[i] = walk(lang, v[i], skip)
		}
		return v
	case map[string]any:
		for k, e := range v {
			if skip[k] {
				continue
			}
			v[k] = walk(lang, e, skip)
		}
		return v
	}
	return x
}
