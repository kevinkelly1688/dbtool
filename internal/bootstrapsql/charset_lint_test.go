// @author: kevinkelly
// @license: Apache Licence
// @file: charset_lint_test.go
// @time: 2026/10/28

package bootstrapsql

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"
	mysql "github.com/bytebase/parser/mysql"
)

// 字符集策略（docs/charset-policy.md）：dbtool 创建的所有表、所有字段都不指定字符集与校验规则，全部跟随库默认。
//
// 本文件用 MySQL 语法解析器（不是正则）检查每条 CREATE TABLE / ALTER TABLE：语法树里不能出现
//   - 列上的 CHARACTER SET / CHARSET / COLLATE，以及 ASCII / UNICODE / BYTE / BINARY（隐含字符集或 _bin）属性；
//   - NCHAR / NVARCHAR / NATIONAL 类型（隐含 utf8mb3）；
//   - 表选项里的 [DEFAULT] CHARSET=… / CHARACTER SET … / COLLATE=…；
//   - 表达式里的 COLLATE。
// 因为是按语法树判断：注释、COMMENT '…' / DEFAULT '…' 字符串、反引号标识符里的同名文字既不会误报，也不会因为
// 换行 / 注释穿插（CHARACTER /* x */ SET）而漏报。字符串里的 ALTER TABLE（预处理语句，见 150_builtin_groups.sql）也会被解出来检查。

// lintCharset 返回 text（一条语句，不含分号）里所有字符集 / 校验规则相关的子句；解析失败也算问题（无法确认）。
func lintCharset(text string) (hits []string) {
	defer func() {
		if r := recover(); r != nil {
			hits = append(hits, fmt.Sprintf("解析器内部错误：%v", r))
		}
	}()
	el := &syntaxErrs{DefaultErrorListener: antlr.NewDefaultErrorListener()}
	lex := mysql.NewMySQLLexer(antlr.NewInputStream(text + ";"))
	lex.RemoveErrorListeners()
	lex.AddErrorListener(el)
	p := mysql.NewMySQLParser(antlr.NewCommonTokenStream(lex, antlr.TokenDefaultChannel))
	p.RemoveErrorListeners()
	p.AddErrorListener(el)
	tree := p.Script()
	for _, m := range el.msgs {
		hits = append(hits, "语法错误（无法确认是否含字符集子句）："+m)
	}
	walkCharset(tree, p, &hits)
	return hits
}

var forbiddenTokens = map[int]string{
	mysql.MySQLParserNCHAR_SYMBOL:    "NCHAR（隐含 utf8mb3）",
	mysql.MySQLParserNVARCHAR_SYMBOL: "NVARCHAR（隐含 utf8mb3）",
	mysql.MySQLParserNATIONAL_SYMBOL: "NATIONAL 类型（隐含 utf8mb3）",
}

func walkCharset(t antlr.Tree, p antlr.Parser, hits *[]string) {
	switch c := t.(type) {
	case *mysql.CharsetContext, *mysql.CharsetClauseContext, *mysql.DefaultCharsetContext:
		*hits = append(*hits, "CHARACTER SET / CHARSET："+ctxText(c, p))
		return
	case *mysql.CollateContext, *mysql.DefaultCollationContext, *mysql.SimpleExprCollateContext:
		*hits = append(*hits, "COLLATE："+ctxText(c, p))
		return
	case *mysql.CharsetWithOptBinaryContext:
		*hits = append(*hits, "列字符集 / BINARY / ASCII / UNICODE 属性："+ctxText(c, p))
		return
	case antlr.TerminalNode:
		if what, bad := forbiddenTokens[c.GetSymbol().GetTokenType()]; bad {
			*hits = append(*hits, what+"："+c.GetText())
		}
		return
	}
	for i := 0; i < t.GetChildCount(); i++ {
		walkCharset(t.GetChild(i), p, hits)
	}
}

func ctxText(t antlr.Tree, p antlr.Parser) string {
	if rc, ok := t.(antlr.RuleContext); ok {
		return strings.Join(strings.Fields(p.GetTokenStream().GetTextFromRuleContext(rc)), " ")
	}
	return fmt.Sprint(t)
}

var (
	ddlHeadRe   = regexp.MustCompile(`(?is)^\s*(CREATE|ALTER)\s+(TEMPORARY\s+)?TABLE\b`)
	quotedStrRe = regexp.MustCompile(`'(?:[^'\\]|''|\\.)*'`)
)

func unquoteSQL(lit string) string {
	s := lit[1 : len(lit)-1]
	s = strings.ReplaceAll(s, "''", "'")
	s = strings.ReplaceAll(s, `\'`, "'")
	return s
}

// lintStatement 检查一条语句：自身是 CREATE / ALTER TABLE 则解析检查；其中的字符串字面量若本身是 CREATE / ALTER TABLE
// （预处理语句）也递归检查。isDDL 表示检查了多少条 DDL。
func lintStatement(text string) (hits []string, ddl int) {
	if ddlHeadRe.MatchString(text) {
		ddl++
		hits = append(hits, lintCharset(text)...)
	}
	for _, lit := range quotedStrRe.FindAllString(text, -1) {
		inner := unquoteSQL(lit)
		if ddlHeadRe.MatchString(inner) {
			ddl++
			for _, h := range lintCharset(inner) {
				hits = append(hits, "（字符串里的预处理 DDL）"+h)
			}
		}
	}
	return hits, ddl
}

// ---- 自检：检查器自己必须能抓到各种写法，也不能误报 ----

func TestCharsetLintDetectsEveryForm(t *testing.T) {
	bad := map[string]string{
		"列 CHARACTER SET":         "CREATE TABLE t (a varchar(10) CHARACTER SET utf8mb4 NOT NULL)",
		"列 CHARSET":               "CREATE TABLE t (a varchar(10) CHARSET utf8mb4)",
		"列 COLLATE":               "CREATE TABLE t (a varchar(10) COLLATE utf8mb4_general_ci NOT NULL DEFAULT '')",
		"列 字符集 + 校验规则":            "CREATE TABLE t (a text CHARACTER SET utf8mb4 COLLATE utf8mb4_bin)",
		"小写与换行":                   "create table t (a varchar(10)\n  character\n  set utf8mb4\n  collate utf8mb4_general_ci)",
		"关键字之间夹注释":                "CREATE TABLE t (a varchar(10) CHARACTER /* x */ SET utf8mb4)",
		"表 DEFAULT CHARSET=":      "CREATE TABLE t (a int) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"表 CHARSET=":              "CREATE TABLE t (a int) ENGINE=InnoDB CHARSET=utf8mb4",
		"表 DEFAULT CHARACTER SET": "CREATE TABLE t (a int) DEFAULT CHARACTER SET utf8mb4",
		"表 COLLATE=":              "CREATE TABLE t (a int) ENGINE=InnoDB COLLATE=utf8mb4_general_ci",
		"表 DEFAULT COLLATE":       "CREATE TABLE t (a int) DEFAULT COLLATE utf8mb4_0900_ai_ci",
		"表选项逗号分隔":                 "CREATE TABLE t (a int) ENGINE=InnoDB, DEFAULT CHARSET=utf8mb4, COLLATE=utf8mb4_general_ci",
		"版本注释里的表选项":               "CREATE TABLE t (a int) ENGINE=InnoDB /*!50100 DEFAULT CHARSET=utf8mb4 */",
		"ENUM 带字符集":               "CREATE TABLE t (a enum('x','y') CHARACTER SET utf8mb4)",
		"BINARY 属性":               "CREATE TABLE t (a varchar(10) BINARY)",
		"ASCII 属性":                "CREATE TABLE t (a char(2) ASCII)",
		"NVARCHAR":                "CREATE TABLE t (a nvarchar(10))",
		"NCHAR":                   "CREATE TABLE t (a nchar(10))",
		"生成列表达式里的 COLLATE":        "CREATE TABLE t (a varchar(10), b varchar(10) AS (a COLLATE utf8mb4_bin) VIRTUAL)",
		"ALTER 加列":                "ALTER TABLE t ADD COLUMN b varchar(10) COLLATE utf8mb4_bin NOT NULL",
		"字符串里的预处理 ALTER":          "SET @ddl := IF(1=1, 'ALTER TABLE t ADD COLUMN b varchar(10) CHARACTER SET utf8mb4', 'SELECT 1')",
	}
	for name, sql := range bad {
		hits, ddl := lintStatement(sql)
		if ddl == 0 {
			t.Errorf("%s：没有被识别为 DDL：%s", name, sql)
			continue
		}
		if len(hits) == 0 {
			t.Errorf("%s：应被检出但没有：%s", name, sql)
		}
	}
}

func TestCharsetLintNoFalsePositives(t *testing.T) {
	good := map[string]string{
		"COMMENT 里的字样":     "CREATE TABLE t (a varchar(10) NOT NULL COMMENT 'CHARACTER SET utf8mb4 COLLATE utf8mb4_bin') ENGINE=InnoDB COMMENT='DEFAULT CHARSET=latin1 COLLATE=x'",
		"DEFAULT 字符串":      "CREATE TABLE t (a varchar(40) NOT NULL DEFAULT 'COLLATE utf8mb4_bin')",
		"行注释":              "CREATE TABLE t ( -- DEFAULT CHARSET=utf8mb4 COLLATE x\n a int)",
		"块注释":              "CREATE TABLE t (/* CHARACTER SET utf8mb4 */ a int) /* COLLATE=utf8mb4_bin */ ENGINE=InnoDB",
		"反引号列名":            "CREATE TABLE t (`charset` varchar(20) NOT NULL DEFAULT '', `collate` int, `character` int)",
		"不带引号的 charset 列名": "CREATE TABLE t (charset varchar(20) NOT NULL DEFAULT '', id int)",
		"字符串里的预处理 ALTER 无字符集": "SET @ddl := IF(1=1, 'ALTER TABLE t ADD COLUMN b varchar(10) NOT NULL DEFAULT '''' COMMENT ''COLLATE x''', 'SELECT 1')",
		"普通表":  "CREATE TABLE IF NOT EXISTS t (id int NOT NULL AUTO_INCREMENT, n varchar(30) NOT NULL DEFAULT '', PRIMARY KEY (id), UNIQUE KEY uk (n)) ENGINE=InnoDB COMMENT='x'",
		"字节类型": "CREATE TABLE t (a varbinary(10), b blob, c binary(4))",
	}
	for name, sql := range good {
		hits, ddl := lintStatement(sql)
		if ddl == 0 {
			t.Errorf("%s：没有被识别为 DDL", name)
		}
		if len(hits) > 0 {
			t.Errorf("%s：不应误报：%v\n  %s", name, hits, sql)
		}
	}
}

// ---- 对仓库里全部建表脚本的检查 ----

func TestNoCharsetOrCollationInAnyTableDefinition(t *testing.T) {
	root := repoRoot(t)
	var files []string
	for _, g := range []string{"deploy/bootstrap/*.sql", "deploy/sql/*.sql"} {
		m, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(g)))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	if len(files) < 20 {
		t.Fatalf("只找到 %d 个 SQL 文件：路径不对？", len(files))
	}
	total, creates := 0, map[string]int{}
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		stmts, err := splitSQL(string(b))
		if err != nil {
			t.Errorf("%s：%v", rel, err)
			continue
		}
		for _, s := range stmts {
			hits, n := lintStatement(s.Text)
			total += n
			if strings.HasPrefix(strings.ToUpper(s.Text), "CREATE TABLE") {
				creates[filepath.ToSlash(filepath.Dir(rel))]++
			}
			for _, h := range hits {
				t.Errorf("%s 第 %d 行：%s\n  语句：%s", rel, s.Line, h, preview(s.Text))
			}
		}
	}
	// 防止“什么都没检查就通过”：001 有 48 张表，deploy/sql 里有 54 张
	if creates["deploy/bootstrap"] < 48 || creates["deploy/sql"] < 54 {
		t.Errorf("检查到的 CREATE TABLE 太少：%v（期望 deploy/bootstrap ≥ 48、deploy/sql ≥ 54）", creates)
	}
	t.Logf("检查了 %d 个文件、CREATE TABLE %v（含字符串里的预处理 DDL，共 %d 条 DDL）", len(files), creates, total)
}
