// @author: kevinkelly
// @license: Apache Licence
// @file: split.go
// @time: 2026/10/28

package tsql

import "strings"

// Unit 是一个“执行单元”：一次下发给 SQL Server 的 SQL 文本。
type Unit struct {
	SQL   string // 单元原文（保留注释与换行，已去掉首尾空白）
	Batch int    // 所在的 GO 批次序号，从 1 开始
	Line  int    // 单元第一个字符在原脚本里的行号，从 1 开始
}

// Units 把脚本切成执行单元，规则如下：
//
//  1. 先按独占一行的 GO 切批（与 SSMS/sqlcmd 一致）；
//  2. 批内只有一条语句 → 整批原文作为一个单元（保留注释与换行）；
//  3. 批内含“批作用域”构造（DECLARE @变量、游标、TRY、事务、GOTO）→ 整批作为一个单元，
//     否则拆开后后面的语句取不到前面声明的变量；
//  4. 否则按顶层分号逐条拆分；若拆分会让 ELSE 单独成句（IF…;ELSE…），整批作为一个单元；BEGIN…END、CASE…END 块内的分号不拆。
//
// 切分是词法级的：字符串、注释、[标识符] 里的分号和 GO 不会被误切。
func Units(sql string) []Unit {
	var units []Unit
	for bi, b := range splitBatches(sql) {
		for _, u := range splitBatch(b.text) {
			units = append(units, Unit{SQL: u.text, Batch: bi + 1, Line: b.line + u.line - 1})
		}
	}
	return units
}

type piece struct {
	text string
	line int // 从 1 开始，相对 text 所在的上层文本
}

// lineAt 返回偏移 off 处的行号（从 1 开始）。
func lineAt(s string, off int) int { return 1 + strings.Count(s[:off], "\n") }

// splitBatches 按独占一行的 GO 切批，丢弃只有空白/注释的批。
func splitBatches(sql string) []piece {
	toks := Tokenize(sql)
	var out []piece
	startOff := 0
	atLineStart := true
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.Kind == Word && atLineStart && strings.EqualFold(t.Text, "GO") && restOfLineBlank(toks, i+1) {
			out = append(out, piece{sql[startOff:t.Start], lineAt(sql, startOff)})
			// 跳到 GO 所在行的行尾
			j := i + 1
			for j < len(toks) && !(toks[j].Kind == Space && strings.Contains(toks[j].Text, "\n")) {
				j++
			}
			if j >= len(toks) {
				startOff = len(sql)
				i = len(toks)
				break
			}
			startOff = toks[j].Start + strings.Index(toks[j].Text, "\n") + 1
			i = j
			atLineStart = true
			continue
		}
		switch {
		case t.Kind == Space && strings.Contains(t.Text, "\n"):
			atLineStart = true
		case t.Kind == Space:
		case strings.Contains(t.Text, "\n"):
			atLineStart = false
		default:
			atLineStart = false
		}
	}
	out = append(out, piece{sql[startOff:], lineAt(sql, startOff)})
	res := out[:0]
	for _, p := range out {
		if strings.TrimSpace(StripComments(p.text)) != "" {
			res = append(res, p)
		}
	}
	return res
}

// restOfLineBlank GO 之后到行尾只有空白（或行注释）。
func restOfLineBlank(toks []Token, from int) bool {
	for j := from; j < len(toks); j++ {
		t := toks[j]
		switch {
		case t.Kind == Space:
			if strings.Contains(t.Text, "\n") {
				return true
			}
		case t.Kind == Comment && strings.HasPrefix(t.Text, "--"):
		default:
			return false
		}
	}
	return true
}

// batchScoped 批里是否含“批作用域”构造：DECLARE @变量、DECLARE x CURSOR、BEGIN TRY/TRAN、GOTO 标签。
func batchScoped(sig []Token) bool {
	for i, t := range sig {
		if t.Kind != Word {
			continue
		}
		switch strings.ToUpper(t.Text) {
		case "DECLARE":
			if i+1 < len(sig) && sig[i+1].Kind == Variable {
				return true
			}
			if i+2 < len(sig) && strings.EqualFold(sig[i+2].Text, "CURSOR") {
				return true
			}
		case "BEGIN":
			if i+1 < len(sig) {
				switch strings.ToUpper(sig[i+1].Text) {
				case "TRY", "TRAN", "TRANSACTION":
					return true
				}
			}
		case "GOTO":
			if i+1 < len(sig) && sig[i+1].Kind == Word {
				return true
			}
		}
	}
	return false
}

// splitBatch 把一个 GO 批切成单元。
func splitBatch(batch string) []piece {
	if strings.TrimSpace(StripComments(batch)) == "" {
		return nil
	}
	lead := len(batch) - len(strings.TrimLeft(batch, " \t\r\n"))
	whole := []piece{{strings.TrimSpace(batch), lineAt(batch, lead)}}
	toks := Tokenize(batch)
	if batchScoped(Significant(toks)) {
		return whole
	}
	// 按顶层分号切；BEGIN/CASE 与 END 配平
	type frag struct{ from, to int }
	var frags []frag
	depth, segStart := 0, 0
	for i, t := range toks {
		if t.Kind == Space || t.Kind == Comment {
			continue
		}
		switch up := t.Upper(); {
		case t.Kind == Word && (up == "BEGIN" || up == "CASE"):
			depth++
		case t.Kind == Word && up == "END":
			if depth > 0 {
				depth--
			}
		case t.Kind == Punct && t.Text == ";" && depth == 0:
			frags = append(frags, frag{segStart, t.Start + 1})
			segStart = t.Start + 1
		}
		_ = i
	}
	frags = append(frags, frag{segStart, len(batch)})
	var out []piece
	for _, f := range frags {
		txt := batch[f.from:f.to]
		body := strings.TrimSpace(StripComments(strings.TrimSpace(txt)))
		if strings.TrimSpace(strings.TrimSuffix(body, ";")) == "" {
			continue // 只有分号/注释的空语句
		}
		l := len(txt) - len(strings.TrimLeft(txt, " \t\r\n"))
		out = append(out, piece{strings.TrimSpace(txt), lineAt(batch, f.from+l)})
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out // 只有一条语句：用这一段（去掉多余的空语句）
	}
	for _, f := range out {
		if firstWord(f.text) == "ELSE" { // IF…;ELSE… 被切断
			return whole
		}
	}
	return out
}

func firstWord(s string) string {
	for _, t := range Tokenize(s) {
		if t.Kind == Space || t.Kind == Comment {
			continue
		}
		return strings.ToUpper(t.Text)
	}
	return ""
}
