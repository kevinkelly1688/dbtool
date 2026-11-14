// @author: kevinkelly
// @license: Apache Licence
// @file: validate.go
// @time: 2026/10/28

package perf

import (
	"errors"
	"strings"
	"unicode"
)

// 手工建单 / 重新诊断粘贴的 SQL 会被拿去做 EXPLAIN 和诊断，所以只接受单条查询类语句。

var errSampleSQL = errors.New("SQL 只支持单条 SELECT / UPDATE / DELETE / WITH 语句（不要带分号，也不要写多条）")

// sqlSkeleton 去掉注释，并把字符串 / 反引号标识符的内容抹掉，只留下能判断“有几条语句、是什么语句”的骨架。
// 遇到 MySQL 会真正执行的 /*! ... */ 注释时返回 ok=false。
func sqlSkeleton(sql string) (skel string, ok bool) {
	rs := []rune(sql)
	var b strings.Builder
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			q := c
			i++
			for i < len(rs) {
				if rs[i] == '\\' && q != '`' {
					i += 2
					continue
				}
				if rs[i] == q {
					if i+1 < len(rs) && rs[i+1] == q { // 连写两个引号 = 转义
						i += 2
						continue
					}
					break
				}
				i++
			}
			i++
			b.WriteString("''")
		case c == '#':
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
		case c == '-' && i+1 < len(rs) && rs[i+1] == '-' && (i+2 >= len(rs) || unicode.IsSpace(rs[i+2]) || unicode.IsControl(rs[i+2])):
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(rs) && rs[i+1] == '*':
			if i+2 < len(rs) && rs[i+2] == '!' {
				return "", false
			}
			i += 2
			for i+1 < len(rs) && !(rs[i] == '*' && rs[i+1] == '/') {
				i++
			}
			i += 2
			b.WriteByte(' ')
		default:
			b.WriteRune(c)
			i++
		}
	}
	return b.String(), true
}

// validateSampleSQL 校验手工建单 / 重新诊断提交的 SQL：单条、以 SELECT / UPDATE / DELETE / WITH 开头、没有分号。
func validateSampleSQL(sql string) error {
	skel, ok := sqlSkeleton(sql)
	if !ok {
		return errors.New("SQL 里不能包含 /*! ... */ 这类会被数据库执行的注释")
	}
	skel = strings.TrimSpace(skel)
	if skel == "" {
		return errors.New("请填写SQL")
	}
	if strings.ContainsRune(skel, ';') {
		return errSampleSQL
	}
	words := strings.FieldsFunc(skel, func(r rune) bool { return !(unicode.IsLetter(r) || r == '_') })
	if len(words) == 0 {
		return errSampleSQL
	}
	switch strings.ToLower(words[0]) {
	case "select", "update", "delete", "with":
		return nil
	}
	return errSampleSQL
}

// filterActionsByButton 按细粒度按钮码裁剪详情里的操作按钮：服务端接口按 actionButtons 校验按钮，页面显示要与之一致。
// 没有登记按钮的操作保持原样（由服务层自己拒绝）。
func filterActionsByButton(actions []ActionDTO, has func(code string) bool) []ActionDTO {
	out := make([]ActionDTO, 0, len(actions))
	for _, a := range actions {
		if code, ok := actionButtons[a.Name]; ok && !has(code) {
			continue
		}
		out = append(out, a)
	}
	return out
}
