// @author: kevinkelly
// @license: Apache Licence
// @file: prelude.go
// @time: 2026/10/28

package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dbtool/internal/sqlx"
)

// MySQL 系在线查询的“SET 打头的脚本”。
//
// 脚本以若干条 SET 开头时，按“会话脚本”处理：所有语句在同一个（只读）会话里按顺序执行，返回最后一条查询的结果。
//   - SET 用来给用户变量赋值：SET @SiteId := 1008;  SET @OrgId := (SELECT OrgId FROM tenant WHERE Id = @SiteId);
//   - 或调整少数几个无害的会话参数：SET block_encryption_mode = 'aes-256-cbc';
//   - 其余语句只能是查询（SELECT / WITH / SHOW / DESC / EXPLAIN）；中间的查询通常是“校验”（子查询返回多行就报错中断），
//     结果丢弃，只返回最后一条的结果。脚本里有任何写语句 / 其它 SET 就整份拒绝，一条都不执行。
// 其它 SET（全局变量、事务属性、sql_mode、字符集、密码、角色……）一律拒绝。
// 没有 SET 打头的多语句仍然只执行第 1 条（与以前一致）。
//
// 安全边界（数据与库优先）：
//  1. 只认“SET @变量 {:=|=} 表达式”和白名单里的会话参数；白名单参数的值必须是符合格式的字面量；
//  2. 赋值表达式不允许 INTO / 加锁读取 / 写操作 / sleep、benchmark、load_file 等危险函数 / mysql 系统库，
//     与普通查询的限制一样，且不区分管理员（query.max_execution_seconds 管不到 SET，所以更严）；
//  3. 表达式里的子查询当成一条独立查询对待：库表权限、SQL 防火墙（表对象与扫描行数）都要校验，
//     否则可以把没有权限的表读进变量再 SELECT 出来；
//  4. 变量会把敏感字段的值带进结果列：脱敏时把前置 SET 一起纳入变量污染分析（见 masking.userVarAssigns）；
//  5. 前置语句与查询在同一个只读会话里执行，写操作被数据库自己拒绝；用过的连接丢弃，变量不会留给别人；
//  6. 前置语句写进查询日志，和查询一起留痕；
//  7. 中间的查询逐条做库表权限与防火墙（含扫描行数）校验，行数同样按上限改写；脚本里禁止 INTO（SELECT … INTO @变量）。

const (
	maxPreludeSets  = 20   // SET 语句条数上限
	maxPreludeChars = 8000 // SET 语句总长度上限
	maxScriptStmts  = 40   // 脚本语句条数上限（SET + 查询）
	maxScriptQuery  = 10   // 脚本里的查询条数上限（含最后一条）
)

// preludeInfo 是通过检查的前置语句：最后一条查询之前的全部语句（SET 与中间的查询），按脚本里的顺序。
type preludeInfo struct {
	stmts    []string // 要先执行的语句（已去注释；中间查询的行数上限由 prepare 改写进来）
	queryIdx []int    // stmts 里哪些是查询（其余是 SET）
	probes   []string // 含子查询的 SET 赋值，用 “SELECT <表达式>” 做库表授权 / 防火墙对象解析
}

// setStmts 是前置语句里的 SET（扫描行数估算用：只需要变量有值，不重复执行中间的查询）。
func (p preludeInfo) setStmts() []string {
	if len(p.queryIdx) == 0 {
		return p.stmts
	}
	isQuery := map[int]bool{}
	for _, i := range p.queryIdx {
		isQuery[i] = true
	}
	var out []string
	for i, st := range p.stmts {
		if !isQuery[i] {
			out = append(out, st)
		}
	}
	return out
}

// setsBefore 是第 i 条前置语句之前的 SET。
func (p preludeInfo) setsBefore(i int) []string {
	sub := preludeInfo{stmts: p.stmts[:i]}
	for _, q := range p.queryIdx {
		if q < i {
			sub.queryIdx = append(sub.queryIdx, q)
		}
	}
	return sub.setStmts()
}

// text 是前置语句拼成的文本（写日志、展示、脱敏变量分析用）。
func (p preludeInfo) text() string { return strings.Join(p.stmts, ";\n") }

// withQuery 把前置语句与查询拼成一份完整脚本文本。
func (p preludeInfo) withQuery(q string) string {
	if len(p.stmts) == 0 {
		return q
	}
	return p.text() + ";\n" + q
}

// 允许设置的会话参数；值必须是下面这些格式的字面量。
var setSysVars = map[string]func(sqlx.Token) error{
	"block_encryption_mode": func(t sqlx.Token) error {
		if v, ok := strLit(t); ok && aesModeRe.MatchString(v) {
			return nil
		}
		return fmt.Errorf("值必须是 'aes-128-ecb' / 'aes-256-cbc' 这样的加密模式字符串")
	},
	"group_concat_max_len": func(t sqlx.Token) error { return intRange(t, 4, 1<<20) },
	"div_precision_increment": func(t sqlx.Token) error {
		return intRange(t, 0, 30)
	},
	"time_zone": func(t sqlx.Token) error {
		if v, ok := strLit(t); ok && tzRe.MatchString(v) {
			return nil
		}
		return fmt.Errorf("值必须是 '+08:00' 或 'SYSTEM' 或 'Asia/Shanghai' 这样的时区字符串")
	},
	"lc_time_names": func(t sqlx.Token) error {
		if v, ok := strLit(t); ok && lcRe.MatchString(v) {
			return nil
		}
		return fmt.Errorf("值必须是 'zh_CN' / 'en_US' 这样的区域字符串")
	},
}

var (
	aesModeRe = regexp.MustCompile(`(?i)^aes-(128|192|256)-(ecb|cbc|cfb1|cfb8|cfb128|ofb)$`)
	tzRe      = regexp.MustCompile(`^([+-](0?[0-9]|1[0-4]):[0-5][0-9]|SYSTEM|[A-Za-z]+(/[A-Za-z0-9_+-]+)*)$`)
	lcRe      = regexp.MustCompile(`^[a-z]{2}_[A-Z]{2}$`)
	userVarRe = regexp.MustCompile(`^@[\p{L}\p{N}_.$]+$`)
)

const setSysVarNames = "block_encryption_mode / group_concat_max_len / div_precision_increment / time_zone / lc_time_names"

// strLit 取单引号字符串字面量的内容（不处理转义：含反斜杠或引号的一律不认）。
func strLit(t sqlx.Token) (string, bool) {
	if t.Kind != sqlx.String || len(t.Text) < 2 {
		return "", false
	}
	v := t.Text[1 : len(t.Text)-1]
	if strings.ContainsAny(v, `\'`) {
		return "", false
	}
	return v, true
}

func intRange(t sqlx.Token, lo, hi int) error {
	n, err := strconv.Atoi(t.Text)
	if t.Kind != sqlx.Number || err != nil || n < lo || n > hi {
		return fmt.Errorf("值必须是 %d 到 %d 之间的整数", lo, hi)
	}
	return nil
}

func badSet(format string, a ...any) error {
	return newErr(400, "bad_query", fmt.Sprintf(format, a...))
}

// checkMySQLScript 检查 MySQL 系在线查询的脚本。
//   - 不以 SET 打头：与 checkMySQL 完全一致（只执行第 1 条）；
//   - 以 SET 打头：整份脚本都要通过检查才执行——SET 只认 checkSet 放行的写法，其余语句只能是查询，最后一条必须是查询；
//     返回最后一条之前的全部语句（pre）与最后一条查询（stmt）。
func (s *Service) checkMySQLScript(sql, db string, pol Policy) (pre preludeInfo, stmt, notice string, err error) {
	stmts := sqlx.Split(sql)
	if len(stmts) == 0 || sqlx.FirstKeyword(stmts[0]) != "set" {
		stmt, notice, err = s.checkMySQLStmts(stmts, db, pol)
		return preludeInfo{}, stmt, notice, err
	}
	last := len(stmts) - 1
	switch {
	case sqlx.FirstKeyword(stmts[last]) == "set":
		return pre, "", "", badSet("脚本的最后一条必须是查询（SELECT / WITH / SHOW / DESC / EXPLAIN），现在最后一条是 SET：SET 只能放在查询前面。")
	case len(stmts) > maxScriptStmts:
		return pre, "", "", badSet("脚本最多 %d 条语句（现在有 %d 条）。", maxScriptStmts, len(stmts))
	}
	var sets, queries, setChars int
	for i, st := range stmts[:last] {
		if sqlx.FirstKeyword(st) == "set" {
			sets++
			setChars += len(st)
			probes, err := checkSet(st, db)
			if err != nil {
				return preludeInfo{}, "", "", badSet("第 %d 条语句（SET）不能执行：%s", i+1, errText(err))
			}
			pre.probes = append(pre.probes, probes...)
			pre.stmts = append(pre.stmts, st)
			continue
		}
		queries++
		q, _, err := s.checkMySQLStmts([]string{st}, db, pol)
		if err != nil {
			return preludeInfo{}, "", "", badSet("第 %d 条语句不能执行：%s", i+1, errText(err))
		}
		if hasInto(q) {
			return preludeInfo{}, "", "", badSet("第 %d 条语句不能执行：脚本里的查询不能使用 INTO（SELECT … INTO @变量 会绕过变量的脱敏分析）。", i+1)
		}
		pre.queryIdx = append(pre.queryIdx, len(pre.stmts))
		pre.stmts = append(pre.stmts, q)
	}
	switch {
	case sets > maxPreludeSets:
		return preludeInfo{}, "", "", badSet("脚本里的 SET 语句最多 %d 条（现在有 %d 条）。", maxPreludeSets, sets)
	case setChars > maxPreludeChars:
		return preludeInfo{}, "", "", badSet("脚本里 SET 语句的总长度超过 %d 个字符。", maxPreludeChars)
	case queries+1 > maxScriptQuery:
		return preludeInfo{}, "", "", badSet("脚本里的查询最多 %d 条（现在有 %d 条）。", maxScriptQuery, queries+1)
	}
	stmt, _, err = s.checkMySQLStmts([]string{stmts[last]}, db, pol)
	if err != nil {
		return preludeInfo{}, "", "", badSet("第 %d 条语句不能执行：%s", last+1, errText(err))
	}
	if hasInto(stmt) {
		return preludeInfo{}, "", "", badSet("第 %d 条语句不能执行：脚本里的查询不能使用 INTO。", last+1)
	}
	notice = fmt.Sprintf("已按顺序在同一个会话里执行脚本的 %d 条语句（SET %d 条、查询 %d 条），下面是最后一条查询的结果。", len(stmts), sets, queries+1)
	return pre, stmt, notice, nil
}

// hasInto 判断语句里有没有 INTO 关键字（词法级，字符串与注释里的不算）。
func hasInto(stmt string) bool {
	for _, t := range sqlx.Significant(sqlx.Tokenize(stmt)) {
		if t.Kind == sqlx.Word && t.Lower() == "into" {
			return true
		}
	}
	return false
}

func errText(err error) string {
	if qe, ok := err.(*Error); ok {
		return qe.Message
	}
	return err.Error()
}

// checkSet 检查一条 SET 语句，返回其中含子查询的赋值表达式对应的 “SELECT <表达式>”。
func checkSet(stmt, db string) ([]string, error) {
	sig := sqlx.Significant(sqlx.Tokenize(stmt))
	if len(sig) < 4 || sig[0].Lower() != "set" {
		return nil, fmt.Errorf("写法不完整")
	}
	var probes []string
	i := 1
	for i < len(sig) {
		// 赋值目标
		var sysName string
		t := sig[i]
		switch {
		case t.Kind == sqlx.Word && strings.HasPrefix(t.Text, "@@"):
			scope := strings.ToLower(strings.TrimPrefix(t.Text, "@@"))
			if i+2 < len(sig) && sig[i+1].Text == "." {
				if scope != "session" && scope != "local" {
					return nil, fmt.Errorf("不允许修改 %s 级别的系统变量", scope)
				}
				sysName = strings.ToLower(sig[i+2].Text)
				i += 3
			} else {
				sysName = scope
				i++
			}
		case t.Kind == sqlx.Word && strings.HasPrefix(t.Text, "@"):
			if !userVarRe.MatchString(t.Text) {
				return nil, fmt.Errorf("用户变量名只支持字母、数字、下划线、点和 $")
			}
			i++
		case t.Kind == sqlx.Word:
			switch lw := strings.ToLower(t.Text); lw {
			case "session", "local":
				if i+1 >= len(sig) || sig[i+1].Kind != sqlx.Word {
					return nil, fmt.Errorf("写法不完整")
				}
				sysName = strings.ToLower(sig[i+1].Text)
				i += 2
			case "global", "persist", "persist_only":
				return nil, fmt.Errorf("不允许修改全局变量")
			default:
				sysName = lw
				i++
			}
		default:
			return nil, fmt.Errorf("不认识的写法")
		}
		// 赋值符号 = 或 :=
		switch {
		case i < len(sig) && sig[i].Kind == sqlx.Punct && sig[i].Text == "=":
			i++
		case i+1 < len(sig) && sig[i].Text == ":" && sig[i+1].Text == "=":
			i += 2
		default:
			return nil, fmt.Errorf("只支持 SET @变量 := 值 或 SET @变量 = 值 的写法")
		}
		// 值：到同层的逗号为止
		from, depth := i, 0
		for i < len(sig) {
			if sig[i].Kind == sqlx.Punct {
				switch sig[i].Text {
				case "(":
					depth++
				case ")":
					depth--
				}
				if sig[i].Text == "," && depth == 0 {
					break
				}
			}
			i++
		}
		if i == from || depth != 0 {
			return nil, fmt.Errorf("赋值表达式为空或括号不匹配")
		}
		if sysName != "" {
			check, ok := setSysVars[sysName]
			if !ok {
				return nil, fmt.Errorf("不允许修改系统变量 %s。在线查询的 SET 只能给用户变量（@名称）赋值，或设置 %s", sysName, setSysVarNames)
			}
			if i-from != 1 {
				return nil, fmt.Errorf("系统变量 %s 的值必须是一个字面量", sysName)
			}
			if err := check(sig[from]); err != nil {
				return nil, fmt.Errorf("%s：%v", sysName, err)
			}
		} else {
			probe, err := checkSetExpr(stmt[sig[from].Start:sig[i-1].End], db)
			if err != nil {
				return nil, err
			}
			if probe != "" {
				probes = append(probes, probe)
			}
		}
		if i < len(sig) { // 逗号：后面还要有一个赋值
			i++
			if i >= len(sig) {
				return nil, fmt.Errorf("末尾多了一个逗号")
			}
		}
	}
	return probes, nil
}

// checkSetExpr 检查用户变量的赋值表达式；含子查询时返回 “SELECT <表达式>”（用于授权与防火墙）。
func checkSetExpr(expr, db string) (string, error) {
	sig := sqlx.Significant(sqlx.Tokenize(expr))
	hasSub := false
	for _, t := range sig {
		if t.Kind != sqlx.Word {
			continue
		}
		switch t.Lower() {
		case "into":
			return "", fmt.Errorf("赋值表达式里不能使用 INTO")
		case "select":
			hasSub = true
		}
	}
	probe := "SELECT " + expr
	switch {
	case sqlx.HasWriteVerb(expr):
		return "", fmt.Errorf("赋值表达式里不能包含写操作")
	case sqlx.ContainsSequence(expr, "for", "update"), sqlx.ContainsSequence(expr, "lock", "in", "share", "mode"):
		return "", fmt.Errorf("赋值表达式里不允许加锁读取")
	case sqlx.TouchesMySQLSchema(probe, db):
		return "", fmt.Errorf("不允许查询 mysql 系统库")
	}
	if fns := sqlx.DangerousFunctions(expr); len(fns) > 0 {
		return "", fmt.Errorf("不允许使用函数 %s", strings.Join(fns, "、"))
	}
	if !hasSub {
		return "", nil
	}
	return probe, nil
}

// mysqlFamily 判断实例类型是否走 MySQL 版的语句检查（MySQL / PolarDB / ADB / SelectDB / Doris 等 MySQL 协议的库）。
func mysqlFamily(dbType string) bool {
	return !isMSSQL(dbType) && !isRedis(dbType) && !isMulti(dbType)
}
