// @author: kevinkelly
// @license: Apache Licence
// @file: localize.go
// @time: 2026/10/28

package dict

import (
	"strings"

	"dbtool/internal/i18n"
)

// 对象详情 / 可编程对象列表里的文案是服务端生成的中文：小节标题、属性名、表头、说明，以及少量标签型的单元格（是 / 否、（自增）…）。
// HTTP 层按请求语言翻译它们（见 http.go）。标题 / 属性名 / 表头 / 说明按目录整句翻译（含带 %s 的格式串）；
// 单元格只翻译已知的“标签”，不碰库里的名字、注释与默认值——它们是用户数据，哪怕恰好和某条文案同形也不能改。

// 标签型单元格所在的列（按翻译前的表头判断）：列信息的“可空”“额外”“脱敏”，索引信息的“唯一”“类型”。
var labelColumns = map[string][]string{
	"列信息":  {"可空", "额外", sensitiveHead},
	"索引信息": {"唯一", "类型"},
}

// 值是标签的属性名（kv 小节）：分区表 / 可更新 是 “是 / 否”，状态是 “启用 / 已禁用”，SQL Server 的“主键”里带“（自增）”。
var labelPairKeys = map[string]bool{"分区表": true, "可更新": true, "状态": true, "主键": true}

// labelFrags 是内嵌在单元格文字里的标签片段（如 “id（自增）”“BTREE（主键）”）。
var labelFrags = []string{"（自增）", "（主键）", "（无效）", " · 主键"}

// trCell 翻译标签型单元格：整串能按目录（含带格式动词的键，如 “TTL %d 秒”）翻译就整串翻译；
// 否则按“，”拆开逐段翻译（索引的多个标记是用“，”拼起来的），再替换段内的标签片段，段之间改用 “, ” 连接。
func trCell(lang, s string) string {
	if s == "" {
		return s
	}
	if out := i18n.TP(lang, s); out != s {
		return out
	}
	parts := strings.Split(s, "，")
	for i, p := range parts {
		if out := i18n.TP(lang, p); out != p {
			parts[i] = out
			continue
		}
		for _, f := range labelFrags {
			if strings.Contains(p, f) {
				p = strings.ReplaceAll(p, f, i18n.T(lang, f))
			}
		}
		parts[i] = p
	}
	return strings.Join(parts, ", ")
}

// genCommentPrefixes 是引擎自己生成的“注释”文字的开头（MongoDB 的“采样 N 个文档…”“视图，源集合 x”）。注释列里多数是库里的
// 用户注释，不能翻译；只有这两种开头的才是引擎生成的，按目录（含格式串）翻译。
var genCommentPrefixes = []string{"采样 ", "视图，源集合 "} // i18n-ignore: 只用来识别引擎输出的开头，不是展示文案

func trGenComment(lang, s string) string {
	for _, p := range genCommentPrefixes {
		if strings.HasPrefix(s, p) {
			return i18n.TP(lang, s)
		}
	}
	return s
}

// trCodeComments 翻译还原出的 DDL 文本里引擎生成的说明行（如 PostgreSQL 开头的“近似 DDL”声明）：
// 只处理以 "-- " 开头且整行能按目录翻译的行，其余原样。
func trCodeComments(lang, text string) string {
	if !strings.Contains(text, "-- ") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "-- ") {
			if out := i18n.TP(lang, l); out != l {
				lines[i] = out
			}
		}
	}
	return strings.Join(lines, "\n")
}

func trPairValue(lang, key, v string) string {
	if key == "注释" {
		return trGenComment(lang, v)
	}
	if !labelPairKeys[key] {
		return v
	}
	if key == "状态" && v == "启用" { // “启用”在通用词表里是动作按钮（Enable），状态标签要 Enabled
		return i18n.T(lang, "启用|status")
	}
	return trCell(lang, v)
}

// localize 把详情里的服务端文案翻译成 lang（中文原样返回）。
func (d *Detail) localize(lang string) {
	if d == nil || lang == i18n.ZH {
		return
	}
	for i, n := range d.Notes {
		d.Notes[i] = i18n.TP(lang, n)
	}
	for i := range d.Sections {
		s := &d.Sections[i]
		var cellCols []int // 标签型单元格所在列的下标
		commentCol := -1   // 注释列（只翻译引擎生成的注释）
		for j, h := range s.Columns {
			for _, want := range labelColumns[s.Title] {
				if h == want {
					cellCols = append(cellCols, j)
				}
			}
			if h == "注释" {
				commentCol = j
			}
		}
		s.Title = i18n.TP(lang, s.Title)
		s.Note = i18n.TP(lang, s.Note)
		switch s.Type {
		case "note":
			s.Text = i18n.TP(lang, s.Text)
		case "code":
			s.Text = trCodeComments(lang, s.Text)
		case "kv":
			pairs := make([]Pair, len(s.Pairs))
			for j, p := range s.Pairs {
				pairs[j] = Pair{Key: i18n.TP(lang, p.Key), Value: trPairValue(lang, p.Key, p.Value)}
			}
			s.Pairs = pairs
		case "table":
			heads := make([]string, len(s.Columns)) // 表头是包级共享切片，不能原地改
			for j, h := range s.Columns {
				heads[j] = i18n.TP(lang, h)
			}
			s.Columns = heads
			rows := make([][]string, len(s.Rows))
			for j, r := range s.Rows {
				row := append([]string(nil), r...)
				for _, c := range cellCols {
					if c < len(row) {
						row[c] = trCell(lang, row[c])
					}
				}
				if commentCol >= 0 && commentCol < len(row) {
					row[commentCol] = trGenComment(lang, row[commentCol])
				}
				rows[j] = row
			}
			s.Rows = rows
		}
	}
}

// localize 翻译可编程对象分组的类别名与取失败原因。
func (g *ProgrammableGroup) localize(lang string) {
	if lang == i18n.ZH {
		return
	}
	g.Label = i18n.TP(lang, g.Label)
	g.Error = i18n.TP(lang, g.Error)
}
