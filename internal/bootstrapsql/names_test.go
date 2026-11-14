// @author: kevinkelly
// @license: Apache Licence
// @file: names_test.go
// @time: 2026/10/28

package bootstrapsql

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
)

// 表名 / 外键约束名的改写规则（R1 / R2），测试侧的镜像。
//
// “源表 -> 新表”的映射规则：Go 的 internal/dbtadmin.TargetName 与本文件的测试镜像；
// 改规则必须同时改两处，下面的测试会让不一致立刻暴露：
//   - TestRenameRuleMatchesGeneratedHeader：001 文件头声明的例外与这里一致。

const tablePrefix = "dbt_"

// renameExceptions：不按“机械加前缀”规则改名的表（源表名 -> 本系统里的表名）。
var renameExceptions = map[string]string{
	"django_content_type": "dbt_content_type",
}

const (
	identMax = 64 // MySQL 表名 / 外键约束名的长度上限
	fkKeep   = identMax - len(tablePrefix) - 1 - 4
)

// targetName 是 R1：源表名 -> 本系统里的表名（已以 dbt_ 开头的不再加）。
func targetName(src string) string {
	if v, ok := renameExceptions[src]; ok {
		return v
	}
	if strings.HasPrefix(src, tablePrefix) {
		return src
	}
	return tablePrefix + src
}

// sourceName 是 targetName 的逆：新表名 -> 源表名；不是由规则产生的名字返回 ("", false)。
func sourceName(dst string) (string, bool) {
	for s, d := range renameExceptions {
		if d == dst {
			return s, true
		}
	}
	if !strings.HasPrefix(dst, tablePrefix) {
		return "", false
	}
	src := strings.TrimPrefix(dst, tablePrefix)
	if _, isException := renameExceptions[src]; isException {
		return "", false // 例外表的“机械名”不是合法的新名
	}
	return src, true
}

// fkTargetName 是 R2：dbt_ + 原名；超过 64 个字符则 dbt_ + 原名前 55 个字符 + "_" + sha1(原名) 的前 4 位十六进制。
func fkTargetName(old string) string {
	n := tablePrefix + old
	if len(n) <= identMax {
		return n
	}
	h := sha1.Sum([]byte(old))
	return tablePrefix + old[:fkKeep] + "_" + hex.EncodeToString(h[:])[:4]
}

var (
	fkNameRe = regexp.MustCompile("CONSTRAINT `([^`]+)` FOREIGN KEY")
)

// legacySourceNames 返回 001 里每张表对应的旧表名（未加 dbt_ 前缀的名字），再并上排除的表（它们不在 001 里，但同样是旧名）。
func legacySourceNames(newNames []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range newNames {
		if s, ok := sourceName(n); ok {
			out[s] = true
		}
	}
	for _, e := range expectedExcluded {
		out[e] = true
	}
	return out
}
