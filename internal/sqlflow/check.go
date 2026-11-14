// @author: kevinkelly
// @license: Apache Licence
// @file: check.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"dbtool/internal/goinception"
	"dbtool/internal/model"
	"dbtool/internal/review"
	"dbtool/internal/settings"
	"dbtool/internal/sqlx"
)

// 检查来源。
const (
	SourceGoInception = "goinception"
	SourceNative      = "native"
	SourceManual      = "manual"
)

// CheckResult 是一次 SQL 检查的结果。
type CheckResult struct {
	review.Set
	Source string // SourceGoInception / SourceNative / SourceManual
	Notice string // 给用户的提示（例如“未经 goInception 审核”）
	// 批量上线时每个实例的预检结果（能连上、库存在）；单实例工单为空
	Instances []InstanceCheck
	// Denied 是数据导出检查时第一条“没有库表查询权限”的说明（空 = 都有权限）。提交时据此直接拒绝，不产生工单。
	Denied string
}

// inceptionConfig 读取 goInception 服务的连接配置；未配置 goinception.host 返回 ok=false。
func (s *Service) inceptionConfig() (goinception.Config, bool) {
	host := cfgStr(s.repo, settings.KeyGoInceptionHost)
	if host == "" {
		return goinception.Config{}, false
	}
	cfg := goinception.Config{Host: host}
	if n, err := strconv.Atoi(cfgStr(s.repo, settings.KeyGoInceptionPort)); err == nil {
		cfg.Port = n
	}
	cfg.User = cfgStr(s.repo, settings.KeyGoInceptionUser)
	cfg.Password = cfgStr(s.repo, settings.KeyGoInceptionPassword)
	return cfg, true
}

// InceptionConfigured 是否配置了 goInception（页面据此决定默认的执行方式）。
func (s *Service) InceptionConfigured() bool {
	_, ok := s.inceptionConfig()
	return ok
}

// RequireInception 是否强制必须使用 goInception（对应环境变量 REQUIRE_GOINCEPTION）。
func (s *Service) RequireInception() bool { return s.requireInception }

func (s *Service) target(inst *model.Instance) goinception.Target {
	c := s.repo.EngineConn(inst)
	return goinception.Target{Host: c.Host, Port: c.Port, User: c.User, Password: c.Password}
}

// Check 检查一批语句：goInception 优先；没有配置时降级为原生检查（开发环境）。
// manual=true 表示线下手工执行的工单，不做检查，只切分语句。
func (s *Service) Check(ctx context.Context, inst *model.Instance, db, sqlText string, manual bool) (*CheckResult, error) {
	if inst.DBType == dbTypeMSSQL {
		return s.checkMSSQL(ctx, inst, db, sqlText, manual)
	}
	if inst.DBType == dbTypeRedis {
		return s.checkRedis(ctx, inst, db, sqlText, manual)
	}
	stmts := sqlx.Split(sqlText)
	if len(stmts) == 0 {
		return nil, bad("SQL 内容不能为空")
	}
	if manual {
		return manualCheck(stmts), nil
	}
	if nativeCreateTable(inst.DBType, sqlText) { // PolarDB-X / SelectDB / ADB / Doris 的建表：只做语法检查，不走 goInception（见 check_createtable.go）
		return s.checkCreateTable(stmts), nil
	}
	if cfg, ok := s.inceptionConfig(); ok {
		return s.checkWithInception(ctx, cfg, inst, db, sqlText, stmts)
	}
	if s.requireInception {
		return nil, newErr(500, "no_inception", "未配置 goInception，无法检查 SQL，请联系管理员。")
	}
	slog.Warn("未配置 goinception.host，SQL 工单检查降级为原生检查（仅建议在开发环境使用）")
	return s.checkNative(stmts), nil
}

func manualCheck(stmts []string) *CheckResult {
	r := &CheckResult{Source: SourceManual, Notice: "线下手工执行：系统不会检查或执行这些语句，请自行确认后在工单里点“确认手工执行结束”。"}
	for i, st := range stmts {
		r.Rows = append(r.Rows, review.Row{ID: review.Flex(i + 1), Stage: "CHECKED", StageStatus: "Audit completed", ErrorMessage: "None", SQL: st, Sequence: "0_0_0", BackupDBName: "None"})
	}
	r.SyntaxType = SetSyntaxType(stmts)
	r.FullSQL = strings.Join(stmts, ";\n")
	return r
}

func (s *Service) checkWithInception(ctx context.Context, cfg goinception.Config, inst *model.Instance, db, sqlText string, stmts []string) (*CheckResult, error) {
	rows, err := s.newInception(cfg).Check(ctx, s.target(inst), db, sqlText, goinception.CheckOpts{RealRowCount: cfgBool(s.repo, settings.KeyReviewExactRowCount)})
	if err != nil {
		slog.Error("goInception 检查失败", "instance", inst.InstanceName, "err", err)
		return nil, newErr(502, "inception_failed", fmt.Sprintf("goInception 检测语句报错，请检查系统配置中 goInception 的设置：\n%s", err.Error()))
	}
	if len(rows) == 0 {
		return nil, newErr(502, "inception_failed", "goInception 没有返回任何检查结果，请检查 goInception 的配置与连通性")
	}
	res := &CheckResult{Source: SourceGoInception}
	res.Rows, res.FullSQL = rows, sqlText
	res.SyntaxType = SyntaxDML
	for _, r := range rows {
		if StatementType(r.SQL) == SyntaxDDL {
			res.SyntaxType = SyntaxDDL
			break
		}
	}
	s.applyRules(res)
	return res, nil
}

// applyRules 叠加站点自己的限制：仅允许 DML/DDL、高危语句正则、DDL 与 DML 不能混提。
func (s *Service) applyRules(res *CheckResult) {
	critical := cfgStr(s.repo, settings.KeyReviewCriticalDDLRegex)
	var criticalRe *regexp.Regexp
	if critical != "" {
		var err error
		if criticalRe, err = regexp.Compile(critical); err != nil {
			slog.Error("review.critical_ddl_regex 不是合法的正则，已忽略", "err", err)
		}
	}
	separation := cfgBool(s.repo, settings.KeyReviewDDLDMLSeparation)
	flag := 0
	for i := range res.Rows {
		r := &res.Rows[i]
		stmt := strings.TrimSpace(sqlx.StripComments(r.SQL))
		typ := StatementType(stmt)
		low := strings.ToLower(stmt)
		switch {
		case strings.HasPrefix(low, "select"):
			markError(r, "驳回不支持语句", "仅支持DML和DDL语句，查询语句请使用SQL查询功能！")
		case criticalRe != nil && criticalRe.MatchString(low):
			markError(r, "驳回高危SQL", fmt.Sprintf("禁止提交匹配%s条件的语句！", critical))
		case separation && (typ == SyntaxDDL || typ == SyntaxDML):
			if flag == 0 {
				flag = typ
			} else if flag != typ {
				markError(r, "驳回不支持语句", "DDL语句和DML语句不能同时执行！")
			}
		}
	}
	res.Count()
}

func markError(r *review.Row, status, msg string) {
	r.StageStatus, r.ErrLevel, r.ErrorMessage = status, review.LevelError, msg
}

// checkNative 是没有 goInception 时的降级检查：只做词法级判断，不连目标实例，因此不会校验语法、表是否存在，
// 也无法估算影响行数。语句类型仅允许 DDL/DML。结果会带明显提示。
func (s *Service) checkNative(stmts []string) *CheckResult {
	res := &CheckResult{Source: SourceNative,
		Notice: "未配置 goInception：当前只做了基础检查，不校验语法、表结构与影响行数，不支持回滚备份；语句的正确性请自行保证。"}
	res.FullSQL = strings.Join(stmts, ";\n")
	for i, st := range stmts {
		r := review.Row{ID: review.Flex(i + 1), Stage: "CHECKED", StageStatus: "Audit completed", ErrorMessage: "None", SQL: st, Sequence: "0_0_" + strconv.Itoa(i), BackupDBName: "None"}
		switch StatementType(st) {
		case SyntaxOther:
			markError(&r, "驳回不支持语句", "仅支持 DDL 与 DML 语句；USE、SET、GRANT 等语句不能通过工单提交。")
		}
		res.Rows = append(res.Rows, r)
	}
	res.SyntaxType = SetSyntaxType(stmts)
	s.applyRules(res)
	return res
}

// isAutoReject 检查结果有问题时系统直接驳回，由系统设置 review.auto_reject_level 控制：
// warning = 出现警告或错误都驳回；error（默认，值为空时同它）= 只在出现错误时驳回；never = 不自动驳回。
func (s *Service) isAutoReject(res *review.Set) bool {
	res.Count()
	mode := cfgStr(s.repo, settings.KeyReviewAutoRejectLevel)
	// 防火墙“仅告警”产生的警告行只是提示，不计入“出现警告就驳回”
	warnings := res.WarningCount - firewallWarnings(res.Rows)
	return (warnings > 0 && mode == "warning") || (res.ErrorCount > 0 && (mode == "" || mode == "warning" || mode == "error"))
}

// isAutoPass 判断是否命中“自动审核通过”规则。
// 只有 goInception 的检查结果（有可靠的影响行数）才允许自动通过；原生检查拿不到影响行数，一律走人工审批。
func (s *Service) isAutoPass(w *model.SqlWorkflow, inst *model.Instance, c *CheckResult) bool {
	if c.Source != SourceGoInception || w.IsOfflineExport != 0 || w.IsManual != 0 {
		return false
	}
	if !cfgBool(s.repo, settings.KeyReviewAutoApproveEnabled) {
		return false
	}
	if !inList(cfgStr(s.repo, settings.KeyReviewAutoApproveDBTypes), inst.DBType) {
		return false
	}
	tagOK := false
	allowed := splitComma(cfgStr(s.repo, settings.KeyReviewAutoApproveTags))
	for _, t := range inst.Tags {
		if t.Active && contains(allowed, t.TagCode) {
			tagOK = true
		}
	}
	if !tagOK {
		return false
	}
	pattern := cfgStr(s.repo, settings.KeyReviewAutoApproveExclRegex)
	if pattern == "" {
		pattern = "^alter|^create|^drop|^truncate|^rename|^delete|^del|^flushdb|^flushall|^lpop|^rpop"
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		slog.Error("review.auto_approve_exclude_regex 不是合法的正则，自动审核已停用", "err", err)
		return false
	}
	maxRows := 50
	if n, err := strconv.Atoi(cfgStr(s.repo, settings.KeyReviewAutoApproveMaxRows)); err == nil {
		maxRows = n
	}
	total := int64(0)
	for _, r := range c.Rows {
		stmt := strings.TrimSpace(strings.NewReplacer("\n", "", "\r", "").Replace(sqlx.StripComments(r.SQL)))
		if re.MatchString(stmt) {
			return false // 命中需要人工复核的语句
		}
		total += int64(r.AffectedRows)
	}
	return total <= int64(maxRows)
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func inList(csv, v string) bool { return contains(splitComma(csv), v) }
