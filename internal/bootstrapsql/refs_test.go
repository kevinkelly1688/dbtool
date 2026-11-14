// @author: kevinkelly
// @license: Apache Licence
// @file: refs_test.go
// @time: 2026/10/28

package bootstrapsql

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"dbtool/internal/btnperm"
)

// 这个文件做“引用校验”：扫描 internal/**、cmd/** 的非测试 Go 源码里引用的表名与权限码，
// 断言它们都能在 001 / deploy/sql / 002 里找到，差集写进日志。
//
// 本系统的表一律带 dbt_ 前缀（001 里的基础表 = dbt_ + 未加前缀的表名，例外见 names_test.go）。因此：
//   - 代码 SQL 里出现未加前缀的旧表名（sql_users、audit_log …）= 改名漏网，在这里被点名；
//   - 只有 internal/dbtadmin（检测库里是否已有旧表）可以合法地写旧名，见 legacyNameDirs。
//
// 扫描是静态的、尽力而为：
//   - 表名来源：GORM 模型的 TableName() 返回值；形如 SELECT / INSERT / UPDATE / DELETE / ALTER / CREATE / DROP / TRUNCATE
//     开头的字符串里 FROM / JOIN / INTO / UPDATE / TABLE 之后的名字；.Table("x y") 与 .Joins("JOIN x …") 的参数；
//   - 动态拼接（"… FROM " + name、fmt.Sprintf("… %s")）取不到名字：这类表靠“字符串字面量恰好等于已知表名”补充证据；
//   - 权限码来源：名字以 perm / btn 开头（不区分大小写）的常量 / 变量的字符串值、HasButton / RequireButtonAPI / RequireButtonPage /
//     ButtonOrFail 等调用的字符串实参、Perm / PermCode 键的字符串值、以及所有 menu_* 字面量。
//     带点的（sqlworkflow.approve）是按钮码，必须在登记表里；不带点的是原权限码（兼容模式和迁移还在用），必须在 002 里。

// externalNames 是 SQL 里出现、但不是元数据库表的名字：目标库（被管理的实例）里的系统表 / 虚拟表，或 SQL 关键字。
// 每项写明原因；新增引用要么属于元数据库（应出现在 001 / deploy/sql 里），要么在这里补一条。
var externalNames = map[string]string{
	// 关键字 / 语法片段被正则误当成表名（出现在报错文案、注释式字符串里）
	"dual": "MySQL 伪表", "select": "关键字", "set": "关键字", "values": "关键字", "where": "关键字",
	"not": "关键字", "if": "关键字", "exists": "关键字", "the": "英文文案", "this": "英文文案", "a": "英文文案",
	// 目标库 / 模板
	"status":          "SHOW TABLE STATUS 的关键字（internal/query/meta.go）",
	"your_table_name": "导出 INSERT 脚本的占位表名，由使用者替换（internal/export/format.go）",
	"orders":          "cmd/demo 演示用的假目标库表",
	"users":           "cmd/demo 演示用的假目标库表",
	"daily_sales":     "cmd/demo 演示用的假目标库表",
	// Oracle / 达梦目标库里的静态数据字典视图（internal/engine/oracle、internal/engine/dm）
	"all_users": "Oracle / 达梦的数据字典视图", "all_objects": "Oracle / 达梦的数据字典视图", "all_tables": "Oracle / 达梦的数据字典视图",
	"all_views": "Oracle / 达梦的数据字典视图", "all_tab_columns": "Oracle / 达梦的数据字典视图", "all_tab_comments": "Oracle / 达梦的数据字典视图",
	"all_col_comments": "Oracle / 达梦的数据字典视图", "all_indexes": "Oracle / 达梦的数据字典视图", "all_ind_columns": "Oracle / 达梦的数据字典视图",
	"all_constraints": "Oracle / 达梦的数据字典视图", "all_cons_columns": "Oracle / 达梦的数据字典视图", "all_synonyms": "Oracle / 达梦的数据字典视图",
	"product_component_version": "Oracle 的版本视图（internal/engine/oracle）",
	"v":                         "V$VERSION 被正则从 $ 处截断（Oracle / 达梦的版本视图）",
	"table":                     "关键字（SQL 文本里的 TABLE 字样，Oracle 引擎的对象类型过滤）",
}

// optionalTables 是代码引用、但不在 001 与 deploy/sql 里、且允许不存在的“可选 / 外部程序负责”的元数据库表。
// 代码对这些表缺失有容错（HasTable 守卫或 1146 错误处理）。
var optionalTables = map[string]string{
	"dbt_bootstrap_applied": "由 dbt-admin 自己在首次运行时创建（internal/dbtadmin），记录已应用的脚本摘要",
}

// legacyNameDirs 是允许在 SQL 里写旧表名（未加前缀）的目录，以及原因。
// 其它目录里出现旧表名一律算残留。这处的“旧名”是对旧版库说话，不是本系统的表；
// 它们有没有把旧名误当目标表用，由各自包里的测试（内存假库区分源名与目标名）负责。
var legacyNameDirs = map[string]string{
	"internal/dbtadmin/": "bootstrap / status 检测库里是否已有 Archery 的旧表（R7）",
}

func legacyNameAllowed(file string) bool {
	for d := range legacyNameDirs {
		if strings.HasPrefix(file, d) {
			return true
		}
	}
	return false
}

type codeRefs struct {
	sqlTables map[string]map[string]bool // 表名 -> 引用它的文件（相对仓库根）
	literals  map[string]map[string]bool // 字符串字面量 -> 文件（只收录等于某个已知表名的）
	perms     map[string]map[string]bool // 权限码 -> 文件
	files     int
}

var (
	sqlVerbRe  = regexp.MustCompile(`(?is)^\s*(SELECT|INSERT|UPDATE|DELETE|REPLACE|ALTER|CREATE|DROP|TRUNCATE|WITH|LOCK|SHOW)\b`)
	joinHeadRe = regexp.MustCompile(`(?is)^\s*((LEFT|RIGHT|INNER|OUTER|CROSS)\s+)*JOIN\s`)
	tableRefRe = regexp.MustCompile("(?is)\\b(?:FROM|JOIN|INTO|UPDATE|TRUNCATE)\\s+`?([A-Za-z_][A-Za-z0-9_]*)`?(\\.)?|\\bTABLE\\s+(?:IF\\s+(?:NOT\\s+)?EXISTS\\s+)?`?([A-Za-z_][A-Za-z0-9_]*)`?(\\.)?")
	onDupRe    = regexp.MustCompile(`(?i)ON\s+DUPLICATE\s+KEY\s+UPDATE`)
	permFnRe   = regexp.MustCompile(`^(HasButton|ActorHasButton|RequireButtonAPI|RequireButtonAPIAudited|RequireButtonPage|ButtonOrFail|hasButton|PermCodes?)$`)
	permKeyRe  = regexp.MustCompile(`^(?i)(Perm|PermCode)$`)
	permNameRe = regexp.MustCompile(`^(?i)(perm|btn)`)
	// buttonsNameRe：名字以 Button / Buttons 结尾的变量（viewButtons、managerButtons、actionButtons …），整个字面量里的字符串都当按钮码。
	buttonsNameRe = regexp.MustCompile(`(?i)buttons?$`)
	dottedCodeRe  = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
	menuLitRe     = regexp.MustCompile(`^menu_[a-z0-9_]+$`)
	identRe       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
)

func unq(lit *ast.BasicLit) (string, bool) {
	if lit == nil || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func scanCode(t testing.TB, known map[string]bool) *codeRefs {
	t.Helper()
	root := repoRoot(t)
	r := &codeRefs{sqlTables: map[string]map[string]bool{}, literals: map[string]map[string]bool{}, perms: map[string]map[string]bool{}}
	add := func(m map[string]map[string]bool, k, f string) {
		if m[k] == nil {
			m[k] = map[string]bool{}
		}
		m[k][f] = true
	}
	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if de.IsDir() {
				if de.Name() == "testdata" || rel == "internal/bootstrapsql" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, src, 0)
			if err != nil {
				t.Fatalf("解析 %s：%v", rel, err)
			}
			r.files++
			addSQL := func(s string) {
				s = onDupRe.ReplaceAllString(s, "ON DUPLICATE KEY ASSIGN")
				for _, m := range tableRefRe.FindAllStringSubmatch(s, -1) {
					name, dotted := m[1], m[2] != ""
					if name == "" {
						name, dotted = m[3], m[4] != ""
					}
					if name == "" || dotted {
						continue // schema.table：目标库对象
					}
					add(r.sqlTables, strings.ToLower(name), rel)
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncDecl:
					if x.Name.Name == "TableName" && x.Body != nil {
						ast.Inspect(x.Body, func(n ast.Node) bool {
							if rs, ok := n.(*ast.ReturnStmt); ok && len(rs.Results) == 1 {
								if s, ok := unq(asLit(rs.Results[0])); ok {
									add(r.sqlTables, strings.ToLower(s), rel)
								}
							}
							return true
						})
					}
				case *ast.BasicLit:
					s, ok := unq(x)
					if !ok {
						return true
					}
					if menuLitRe.MatchString(s) {
						add(r.perms, s, rel)
					}
					if known[s] {
						add(r.literals, s, rel)
					}
					if sqlVerbRe.MatchString(s) || joinHeadRe.MatchString(s) {
						addSQL(s)
					}
				case *ast.CallExpr:
					name := ""
					switch fn := x.Fun.(type) {
					case *ast.SelectorExpr:
						name = fn.Sel.Name
					case *ast.Ident:
						name = fn.Name
					}
					if (name == "Table") && len(x.Args) >= 1 {
						if s, ok := unq(asLit(x.Args[0])); ok {
							if m := identRe.FindString(strings.TrimSpace(strings.Trim(s, "`"))); m != "" && !strings.Contains(s, ".") {
								add(r.sqlTables, strings.ToLower(m), rel)
							}
						}
					}
					if permFnRe.MatchString(name) {
						for _, a := range x.Args {
							if s, ok := unq(asLit(a)); ok {
								add(r.perms, s, rel)
							}
						}
					}
				case *ast.KeyValueExpr:
					if k, ok := x.Key.(*ast.Ident); ok && permKeyRe.MatchString(k.Name) {
						if s, ok := unq(asLit(x.Value)); ok && s != "" {
							add(r.perms, s, rel)
						}
					}
				case *ast.ValueSpec:
					for i, n := range x.Names {
						if buttonsNameRe.MatchString(n.Name) && i < len(x.Values) {
							ast.Inspect(x.Values[i], func(n ast.Node) bool {
								if lit, ok := n.(*ast.BasicLit); ok {
									if s, ok := unq(lit); ok && dottedCodeRe.MatchString(s) {
										add(r.perms, s, rel)
									}
								}
								return true
							})
						}
						if permNameRe.MatchString(n.Name) && i < len(x.Values) {
							if s, ok := unq(asLit(x.Values[i])); ok && regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$`).MatchString(s) {
								add(r.perms, s, rel)
							}
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func asLit(e ast.Expr) *ast.BasicLit {
	if l, ok := e.(*ast.BasicLit); ok {
		return l
	}
	return nil
}

var createTableRe = regexp.MustCompile("(?i)CREATE\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?`?([A-Za-z0-9_]+)`?")

// deploySQLTables 返回 deploy/sql/*.sql 创建的表。
func deploySQLTables(t testing.TB) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot(t), "deploy", "sql", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到 deploy/sql/*.sql：%v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var code []string
		for _, l := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "--") { // 注释里会写“CREATE TABLE IF NOT EXISTS”字样
				code = append(code, l)
			}
		}
		for _, m := range createTableRe.FindAllStringSubmatch(strings.Join(code, "\n"), -1) {
			out[strings.ToLower(m[1])] = filepath.Base(f)
		}
	}
	return out
}

func relevant(m map[string]map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstFiles(m map[string]bool, n int) string {
	fs := keys(m)
	if len(fs) > n {
		fs = append(fs[:n], "…")
	}
	return strings.Join(fs, ", ")
}

func TestReferencedTablesAreCreatedByBootstrap(t *testing.T) {
	schema := tableNames(loadSchema(t))
	inSchema := setOf(schema)
	dbt := deploySQLTables(t)
	excluded := setOf(expectedExcluded)

	known := map[string]bool{}
	for _, n := range schema {
		known[n] = true
	}
	for n := range dbt {
		known[n] = true
	}
	for n := range excluded {
		known[n] = true
	}
	refs := scanCode(t, known)
	if refs.files < 100 {
		t.Fatalf("只扫描到 %d 个源文件，扫描可能失效", refs.files)
	}

	legacy := legacySourceNames(schema) // 旧表名：001 里每张表去掉 dbt_ 前缀（含例外还原）+ 被排除的表
	var unknown []string
	var legacyInAllowed []string
	for _, n := range relevant(refs.sqlTables) {
		switch {
		case inSchema[n], dbt[n] != "":
		case optionalTables[n] != "", externalNames[n] != "":
		case excluded[n]:
			// 被排除的表：任何包的 SQL 都不应引用它；要引用就要重新评估取舍
			for f := range refs.sqlTables[n] {
				t.Errorf("被排除的表 %s 被 %s 的 SQL 引用：请把它加回 001（同时改脚本 EXCLUDED 与设计文档）", n, f)
			}
		case legacy[n]:
			// 旧表名：只有 legacyNameDirs 里的目录可以出现；其它地方是改名漏网
			for f := range refs.sqlTables[n] {
				if legacyNameAllowed(f) {
					legacyInAllowed = append(legacyInAllowed, n+" <- "+f)
					continue
				}
				t.Errorf("旧表名 %s 残留在 %s 的 SQL 里：本系统的表一律带 %s 前缀，应为 %s", n, f, tablePrefix, targetName(n))
			}
		default:
			unknown = append(unknown, n)
		}
	}
	sort.Strings(legacyInAllowed)
	if len(legacyInAllowed) > 0 {
		t.Logf("legacyNameDirs 里按设计出现的旧表名（%d 处，仅供核对，不算错误）：%v", len(legacyInAllowed), legacyInAllowed)
	}
	if len(unknown) > 0 {
		var lines []string
		for _, n := range unknown {
			lines = append(lines, n+"  <- "+firstFiles(refs.sqlTables[n], 3))
		}
		t.Errorf("代码的 SQL 里引用了既不在 001、也不在 deploy/sql、也不在白名单里的名字（%d 个）：\n  %s", len(unknown), strings.Join(lines, "\n  "))
	}

	// 字面量证据：被排除的表不得以字面量形式出现在代码里
	for n := range excluded {
		for f := range refs.literals[n] {
			t.Errorf("被排除的表 %s 作为字符串常量出现在 %s", n, f)
		}
	}

	// 信息：001 里代码从未引用过的表（保留的理由见设计文档：结构兼容 / 外键依赖 / 外部程序使用）
	var unreferenced []string
	for _, n := range schema {
		ref := len(refs.sqlTables[n]) > 0 || len(refs.literals[n]) > 0
		if !ref {
			unreferenced = append(unreferenced, n)
		}
	}
	t.Logf("扫描 %d 个源文件：SQL 引用 %d 个名字；001 里 %d 张表中 %d 张没有引用：%v",
		refs.files, len(refs.sqlTables), len(schema), len(unreferenced), unreferenced)
	t.Logf("deploy/sql 的扩展表脚本建了 %d 张表；被引用而不在 001 / deploy/sql 的名字：%v", len(dbt), unknown)
}

func TestPermissionCodesUsedByCodeExistInPermissionData(t *testing.T) {
	d := loadDataset(t)
	known := map[string]bool{}
	refs := scanCode(t, known)
	codes := relevant(refs.perms)
	if len(codes) < 40 {
		t.Fatalf("只扫描到 %d 个权限码，扫描可能失效", len(codes))
	}
	var unregistered, missing, legacyUsed []string
	buttons := 0
	for _, c := range codes {
		switch {
		case strings.Contains(c, "."): // 按钮码：必须在登记表里（防止写错码，授予了也不起作用）
			buttons++
			if !btnperm.Valid(c) {
				unregistered = append(unregistered, c+"  <- "+firstFiles(refs.perms[c], 3))
			}
		case d.has(btnperm.LegacyApp(c), c): // 原权限码：只有登记表 / 迁移（internal/btnperm）会引用；判断点上用原权限码授予了也不起作用
			for f := range refs.perms[c] {
				if !strings.HasPrefix(f, "internal/btnperm/") {
					legacyUsed = append(legacyUsed, c+"  <- "+f)
				}
			}
		default:
			missing = append(missing, c+"  <- "+firstFiles(refs.perms[c], 3))
		}
	}
	if buttons < 40 {
		t.Fatalf("只扫描到 %d 个按钮码，扫描可能失效", buttons)
	}
	if len(unregistered) > 0 {
		t.Errorf("代码里校验的按钮码没有登记在 internal/btnperm/registry.go（%d 个）：\n  %s", len(unregistered), strings.Join(unregistered, "\n  "))
	}
	if len(legacyUsed) > 0 {
		t.Errorf("判断点上用的是原权限码（已不再有效，请改用 internal/btnperm/registry.go 里的按钮码）：\n  %s", strings.Join(legacyUsed, "\n  "))
	}
	// 登记表里引用的每个原权限码（迁移用）在 002 里也要有。
	for _, c := range btnperm.LegacyCodes() {
		if !d.has(btnperm.LegacyApp(c), c) {
			missing = append(missing, c+"  <- 登记表 Legacy")
		}
	}
	if len(missing) > 0 {
		t.Errorf("引用的原权限码在 002 里找不到（%d 个）：\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
	t.Logf("代码引用了 %d 个按钮码（全部已登记），登记表用到 %d 个原权限码（全部在 002 里）", buttons, len(btnperm.LegacyCodes()))
}

// 反方向：002 里的每个权限码都得被迁移用上——按钮登记表的 Legacy 里有，或 internal/** 的代码里引用了它。
// 迁移用不上的权限码留在种子里只会误导（精简前 291 个里有 233 个是这种）。
func TestEveryPermissionRowHasAJudgmentPoint(t *testing.T) {
	d := loadDataset(t)
	refs := scanCode(t, map[string]bool{})
	inRegistry := map[string]bool{}
	for _, c := range btnperm.LegacyCodes() {
		inRegistry[c] = true
	}
	var dead []string
	for id, p := range d.Perms {
		if inRegistry[p.Codename] || len(refs.perms[p.Codename]) > 0 {
			continue
		}
		dead = append(dead, d.permKey(id))
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("002 里这些权限码既不在登记表的 Legacy 里、代码里也没有引用，迁移用不上（%d 个）：%v\n  要么让登记表的某个按钮声明它，要么从 002 里去掉", len(dead), dead)
	}
	t.Logf("002 的 %d 个权限码迁移时都用得上", len(d.Perms))
}
