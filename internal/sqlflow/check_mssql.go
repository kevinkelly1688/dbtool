// @author: kevinkelly
// @license: Apache Licence
// @file: check_mssql.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"dbtool/internal/engine"
	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/settings"
	"dbtool/internal/tsql"
)

// SourceMSSQL 是 SQL Server 工单的检查来源。
const SourceMSSQL = "mssql"

const dbTypeMSSQL = "mssql"

// mssqlReviewConfig 读取 review.mssql_rules（JSON，见 internal/tsql 的规则表）；配置有误时记日志并用默认规则。
func (s *Service) mssqlReviewConfig() tsql.Config {
	cfg, err := tsql.ParseConfig(cfgStr(s.repo, settings.KeyReviewMSSQLRules))
	if err != nil {
		slog.Error("review.mssql_rules 配置有误，已改用默认规则", "err", err)
	}
	return cfg
}

// checkMSSQL 检查 SQL Server 脚本，参考 Bytebase 的 SQL 审核：
//
//  1. 按 GO 批与顶层分号切成执行单元（同执行时的切法）；
//  2. 每个单元用 Bytebase 的 T-SQL 语法解析，语法错误 → 错误；解析通过再跑规则（缺 WHERE、无主键、危险语句…，见 internal/tsql）；
//  3. 能连上目标实例时，再让服务端 SET PARSEONLY ON 做权威的语法检查：本地解析器认不出而服务端通过的语句只给提醒，不拦截；
//  4. 叠加站点规则：全是查询语句时驳回、review.critical_ddl_regex 高危语句、review.ddl_dml_separation。
func (s *Service) checkMSSQL(ctx context.Context, inst *model.Instance, db, sqlText string, manual bool) (*CheckResult, error) {
	units := tsql.Units(sqlText)
	if len(units) == 0 {
		return nil, bad("SQL 内容不能为空")
	}
	texts := make([]string, len(units))
	for i, u := range units {
		texts[i] = u.SQL
	}
	if manual {
		return manualCheck(texts), nil
	}
	res := &CheckResult{Source: SourceMSSQL}
	res.FullSQL = sqlText
	cfg := s.mssqlReviewConfig()

	// 服务端语法检查（可选）
	serverErr, serverOK := s.serverSyntaxCheck(ctx, inst, db, texts)
	if !serverOK {
		res.Notice = "未能连接目标实例做服务端语法检查，仅做了本地语法解析与规则检查（语法错误只作为警告）。"
	}

	for i, u := range units {
		row := review.Row{ID: review.Flex(i + 1), Stage: "CHECKED", StageStatus: "Audit completed", ErrorMessage: "None", SQL: u.SQL,
			Sequence: fmt.Sprintf("%d_%d_%d", u.Batch, 0, i), BackupDBName: "None"}
		parsed := tsql.Parse(u.SQL)
		var notes []string
		level := review.LevelOK
		bump := func(l int) {
			if l > level {
				level = l
			}
		}
		switch {
		case serverOK && serverErr[i] != "":
			// 服务端说语法错：以服务端为准
			row.StageStatus = "语法错误"
			bump(review.LevelError)
			notes = append(notes, fmt.Sprintf("语法检测失败: %s", serverErr[i]))
		case !parsed.OK() && serverOK:
			// 服务端通过而本地解析器认不出（文法覆盖不全）：提醒，跳过规则
			bump(review.LevelWarning)
			notes = append(notes, fmt.Sprintf("本地语法解析器未能识别该语句（服务器语法检测已通过），上线规则检查已跳过：%s", parsed.Errors[0].String()))
		case !parsed.OK():
			// 没法让服务端确认：只给警告，避免文法覆盖不全误拦合法脚本
			row.StageStatus = "语法待确认"
			bump(review.LevelWarning)
			notes = append(notes, fmt.Sprintf("语法检测失败（本地解析，未经服务端确认）: %s", parsed.Errors[0].String()))
		default:
			for _, f := range tsql.Review(u.SQL, parsed, cfg, db) {
				lv := review.LevelWarning
				if f.Level == tsql.Error {
					lv = review.LevelError
				}
				bump(lv)
				notes = append(notes, fmt.Sprintf("[%s] %s（第 %d 行）", f.Rule, f.Message, u.Line+f.Line-1))
			}
			if level == review.LevelError {
				row.StageStatus = "规则检查未通过"
			} else if level == review.LevelWarning {
				row.StageStatus = "规则检查有警告"
			}
		}
		row.ErrLevel = review.Flex(level)
		if len(notes) > 0 {
			row.ErrorMessage = strings.Join(notes, "\n")
		}
		res.Rows = append(res.Rows, row)
	}
	res.SyntaxType = mssqlSyntaxType(texts)
	s.applyMSSQLSiteRules(res)
	return res, nil
}

// serverSyntaxCheck 让目标实例做 PARSEONLY 语法检查；连不上或引擎不支持时 ok=false。
func (s *Service) serverSyntaxCheck(ctx context.Context, inst *model.Instance, db string, units []string) ([]string, bool) {
	e, err := s.open(inst.DBType, s.repo.EngineConn(inst))
	if err != nil {
		slog.Warn("SQL Server 语法检查：创建引擎失败", "instance", inst.InstanceName, "err", err)
		return nil, false
	}
	defer e.Close()
	sc, ok := e.(engine.SyntaxChecker)
	if !ok {
		return nil, false
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := sc.SyntaxCheck(cctx, db, units)
	if err != nil {
		slog.Warn("SQL Server 语法检查失败", "instance", inst.InstanceName, "err", err)
		return nil, false
	}
	return out, true
}

var queryOnlyRe = regexp.MustCompile(`(?is)^select\b`)
var selectIntoRe = regexp.MustCompile(`(?is)\binto\s+[#@\[\w]`)

// isQueryOnly 是否只读查询语句；SELECT … INTO 会建表写数据，不算只读。
func isQueryOnly(stmt string) bool {
	st := strings.TrimSpace(tsql.StripComments(stmt))
	return queryOnlyRe.MatchString(st) && !selectIntoRe.MatchString(st)
}

func mssqlSyntaxType(units []string) int {
	for _, u := range units {
		if StatementType(tsql.StripComments(u)) == SyntaxDDL {
			return SyntaxDDL
		}
	}
	return SyntaxDML
}

// applyMSSQLSiteRules 站点规则：
// 只在“所有语句都是只读查询”时驳回（运维脚本常在变更后附带核对用的 SELECT，不能一刀切）；review.critical_ddl_regex；review.ddl_dml_separation。
func (s *Service) applyMSSQLSiteRules(res *CheckResult) {
	critical := cfgStr(s.repo, settings.KeyReviewCriticalDDLRegex)
	var criticalRe *regexp.Regexp
	if critical != "" {
		var err error
		if criticalRe, err = regexp.Compile(critical); err != nil {
			slog.Error("review.critical_ddl_regex 不是合法的正则，已忽略", "err", err)
		}
	}
	separation := cfgBool(s.repo, settings.KeyReviewDDLDMLSeparation)
	allQuery := len(res.Rows) > 0
	for _, r := range res.Rows {
		if !isQueryOnly(r.SQL) {
			allQuery = false
		}
	}
	flag := 0
	for i := range res.Rows {
		r := &res.Rows[i]
		stmt := strings.TrimSpace(tsql.StripComments(r.SQL))
		low := strings.ToLower(stmt)
		typ := StatementType(stmt)
		switch {
		case allQuery:
			markError(r, "驳回不支持语句", "仅支持DML和DDL语句，查询语句请使用SQL查询功能！")
		case criticalRe != nil && criticalRe.MatchString(low):
			markError(r, "驳回高危SQL", fmt.Sprintf("禁止提交匹配%s条件的语句！", critical))
		case separation && (typ == SyntaxDDL || typ == SyntaxDML):
			if flag == 0 {
				flag = typ
			} else if flag != typ {
				markError(r, "驳回不支持语句", "DDL语句和DML语句不能同时执行！")
			}
		case r.ErrLevel == review.LevelOK && isQueryOnly(r.SQL):
			r.ErrorMessage = "查询语句将随工单执行，执行结果只显示返回行数，不返回数据"
		}
	}
	res.Count()
}
