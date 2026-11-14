// @author: kevinkelly
// @license: Apache Licence
// @file: charset.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// DBCharset 是目标库的默认字符集与校验规则（information_schema.SCHEMATA）。
// dbtool 的所有表、所有字段都不指定字符集与校验规则，建出来的表完全取决于它（docs/charset-policy.md）。
type DBCharset struct {
	Charset   string // DEFAULT_CHARACTER_SET_NAME
	Collation string // DEFAULT_COLLATION_NAME
}

func (c DBCharset) String() string { return c.Charset + " / " + c.Collation }

// charsetLevel 是默认字符集预检的结论。
type charsetLevel int

const (
	charsetOK     charsetLevel = iota // utf8mb4 + 常见的不区分大小写校验规则
	charsetWarn                       // utf8mb4，但校验规则不在常见清单里
	charsetRefuse                     // 不是 utf8mb4
)

// 常见的、行为可预期的校验规则：都不区分大小写（比较 / 唯一键 / LIKE 行为一致）。
var commonCollations = map[string]bool{
	"utf8mb4_general_ci": true,
	"utf8mb4_0900_ai_ci": true,
	"utf8mb4_unicode_ci": true,
}

// suggestCollation 按服务器版本给出建库 / 改库时建议的校验规则：MySQL 8.0.1+ 用 utf8mb4_0900_ai_ci，其余（5.7、MariaDB）用 utf8mb4_general_ci。
func suggestCollation(version string) string {
	v := strings.ToLower(version)
	if strings.Contains(v, "mariadb") {
		return "utf8mb4_general_ci"
	}
	if i := strings.IndexByte(v, '.'); i > 0 {
		if major, err := strconv.Atoi(v[:i]); err == nil && major >= 8 {
			return "utf8mb4_0900_ai_ci"
		}
	}
	return "utf8mb4_general_ci"
}

// describeCollation 说明一个“不在常见清单里”的 utf8mb4 校验规则会带来什么差异。
func describeCollation(co string) string {
	l := strings.ToLower(co)
	var parts []string
	switch {
	case strings.HasSuffix(l, "_bin"):
		parts = append(parts, "二进制比较：大小写、重音、末尾空格都敏感（'A' 与 'a' 是两个不同的值，唯一键允许两者并存，用户名 / 实例名 / 表名的匹配也区分大小写）")
	case strings.HasSuffix(l, "_cs") || strings.Contains(l, "_cs_"):
		parts = append(parts, "区分大小写（且通常区分重音）：唯一键、等值比较、LIKE 都按大小写区分，与常见的不区分大小写默认行为不同")
	case strings.Contains(l, "_as_") || strings.HasSuffix(l, "_as"):
		parts = append(parts, "区分重音：'e' 与 'é' 视为不同的值")
	default:
		parts = append(parts, "语言 / 版本相关的排序规则：排序顺序、等值比较（例如重音、特殊字符）与 general_ci / 0900_ai_ci 可能不同")
	}
	return strings.Join(parts, "；")
}

// EvaluateCharset 按版本与库默认值给出结论与展示文字。db 是库名，用于生成修复命令。
func EvaluateCharset(cs DBCharset, version, db string) (charsetLevel, []string) {
	fix := fmt.Sprintf("ALTER DATABASE `%s` CHARACTER SET utf8mb4 COLLATE %s;", db, suggestCollation(version))
	if !strings.EqualFold(cs.Charset, "utf8mb4") {
		return charsetRefuse, []string{
			fmt.Sprintf("错误：目标库 %s 的默认字符集是 %s（排序规则 %s），不是 utf8mb4。", db, orUnknown(cs.Charset), orUnknown(cs.Collation)),
			"dbtool 的表和字段都不指定字符集，建出来的表会沿用库默认值，这会得到非 utf8mb4 的表（emoji / 4 字节字符写入失败或被截断）。",
			"修复（只改库的默认值，只影响之后新建的表，不修改任何已有表；用有 ALTER 权限的账号执行）：",
			"  " + fix,
			"  排序规则的选择见 docs/charset-policy.md。",
			"确需忽略（不建议）：加 --skip-charset-check 与 --yes。",
		}
	}
	if !commonCollations[strings.ToLower(cs.Collation)] {
		return charsetWarn, []string{
			fmt.Sprintf("警告：目标库 %s 的默认校验规则是 %s，不在常见清单（utf8mb4_general_ci / utf8mb4_0900_ai_ci / utf8mb4_unicode_ci）里。", db, orUnknown(cs.Collation)),
			"  影响：" + describeCollation(cs.Collation) + "。",
			"  dbtool 的代码按“不区分大小写”的默认行为编写；若这不是你有意的选择，改库默认值再建表：",
			"  " + fix,
		}
	}
	return charsetOK, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "未知"
	}
	return s
}

// charsetGate 读取库默认字符集并打印；返回 false 表示应当拒绝继续（调用方返回 ExitFailed）。
// skip=true（--skip-charset-check，调用方已确认带了 --yes）时只打印风险，不拒绝。
func (e *Env) charsetGate(ctx context.Context, st Store, version, db string, skip bool) (DBCharset, charsetLevel, bool) {
	cs, err := st.DatabaseCharset(ctx)
	if err != nil {
		if skip {
			e.out("警告：读取库默认字符集失败（%s）；已按 --skip-charset-check 跳过。风险：无法确认新建的表是 utf8mb4\n", RedactMsg(err.Error()))
			return DBCharset{}, charsetOK, true
		}
		e.err("错误：读取库默认字符集失败：%s\n  需要能读 information_schema.SCHEMATA；确需忽略（不建议）：加 --skip-charset-check 与 --yes\n", RedactMsg(err.Error()))
		return DBCharset{}, charsetRefuse, false
	}
	e.out("库默认字符集 / 校验规则：%s（所有表、所有字段都不指定，新建的表全部跟随它）\n", cs)
	level, lines := EvaluateCharset(cs, version, db)
	switch level {
	case charsetOK:
		return cs, level, true
	case charsetWarn:
		for _, l := range lines {
			e.out("%s\n", l)
		}
		return cs, level, true
	}
	if skip {
		e.out("警告：库默认字符集是 %s，不是 utf8mb4；已按 --skip-charset-check 跳过检查。\n"+
			"  风险：表不指定字符集，将按库默认值建出非 utf8mb4 的表；4 字节字符（emoji 等）会报错或被截断，之后统一需要重建表。\n", cs)
		return cs, level, true
	}
	for _, l := range lines {
		e.err("%s\n", l)
	}
	return cs, level, false
}
