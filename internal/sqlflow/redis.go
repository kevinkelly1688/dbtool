// @author: kevinkelly
// @license: Apache Licence
// @file: redis.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/engine/redis"
	"dbtool/internal/model"
	"dbtool/internal/redisrule"
	"dbtool/internal/review"
)

// Redis 工单上线：提交的是命令（每行一条），按管理员定义的规则（internal/redisrule）检查，走和其他实例一样的审批流，审批通过后原生执行。
//   - 检查：服务端提交时再做一次，只要有一行被拒绝就不允许提交；
//   - 执行：执行时用“当前规则”整单复检（任何一行不再通过则整单不执行），再逐条执行，每条命令发出前再校验一次，遇错即停；
//   - 执行用独立的写客户端（internal/engine/redis/exec.go），只读客户端的白名单不受影响；
//   - 无自动回滚；规则勾选“记录执行前值”时，把被改键的原值记在执行结果里供人工恢复。
// MySQL / SQL Server 的路径完全不受影响：只在实例类型为 redis 时进入本文件的分支。

// SourceRedis 是 Redis 工单的检查来源。
const SourceRedis = "redis"

const dbTypeRedis = "redis"

// RedisNotice 是 Redis 工单页面上固定显示的提示。
const RedisNotice = "Redis 没有事务回滚：命令一旦执行就无法自动撤销，遇错即停、已执行的不会回退。请勿在命令里写入密码等敏感信息——命令原文会完整保存在工单里、对有权查看工单的人可见。"

// SetRedisRules 设置 Redis 规则的来源。不设置时按“没有任何规则”处理（Redis 工单全部拒绝）。
func (s *Service) SetRedisRules(src redisrule.Source) { s.redisRules = src }

// loadRedisRules 读取当前规则。规则表还没建时 ready=false，返回空规则（全部拒绝）而不是报错。
func (s *Service) loadRedisRules(ctx context.Context) (rules []redisrule.Rule, ready bool, err error) {
	if s.redisRules == nil {
		return nil, false, nil
	}
	return s.redisRules.Rules(ctx)
}

// parseRedisDB 把库名（“3” 或 “db3”）转成库编号。
func parseRedisDB(db string) (int, error) {
	t := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(db)), "db")
	n, err := strconv.Atoi(t)
	if err != nil || n < 0 || n > 1023 {
		return 0, bad("Redis 的库请选择编号（0 ~ 1023），如 0 或 db0")
	}
	return n, nil
}

// normalizeRedisDB 返回工单里保存的库名：纯数字。
func normalizeRedisDB(db string) string {
	if n, err := parseRedisDB(db); err == nil {
		return strconv.Itoa(n)
	}
	return db
}

// checkRedis 检查 Redis 命令脚本：每行一条命令，逐行给出 通过 / 警告 / 拒绝。
func (s *Service) checkRedis(ctx context.Context, inst *model.Instance, db, sqlText string, manual bool) (*CheckResult, error) {
	if manual {
		return nil, bad("Redis 工单不支持跳过检查")
	}
	dbNum, err := parseRedisDB(db)
	if err != nil {
		return nil, err
	}
	lines, err := redisrule.SplitLines(sqlText)
	if err != nil {
		return nil, bad(err.Error())
	}
	rules, ready, err := s.loadRedisRules(ctx)
	if err != nil {
		return nil, newErr(500, "redis_rules", "读取 Redis 规则失败，请稍后再试或联系管理员")
	}
	res := &CheckResult{Source: SourceRedis}
	res.FullSQL = sqlText
	notice := RedisNotice
	if !ready {
		notice = "Redis 规则表 dbt_redis_rule 尚未创建（见 deploy/sql/170_redis_rules.sql），没有任何规则时所有命令都会被拒绝，请联系管理员。" + " " + notice
	}
	res.Notice = notice
	target := redisrule.Target{InstanceID: inst.ID, DB: dbNum}
	for _, l := range lines {
		v := redisrule.EvaluateLine(rules, target, l.Text)
		row := review.Row{ID: review.Flex(l.No), Stage: "CHECKED", StageStatus: v.Status, ErrorMessage: "None", SQL: l.Text,
			Sequence: fmt.Sprintf("0_0_%d", l.No), BackupDBName: "None", ErrLevel: review.Flex(v.Level), AffectedRows: review.Flex(len(v.Keys))}
		switch {
		case v.Message != "":
			row.ErrorMessage = v.Message
		case v.RuleName != "":
			row.ErrorMessage = fmt.Sprintf("命中规则「%s」", v.RuleName)
		}
		res.Rows = append(res.Rows, row)
	}
	res.SyntaxType = SyntaxDML
	res.Count()
	return res, nil
}

// redisCheckSummary 是写进审计的检查摘要：条数、各级别数量、第一条拒绝原因。
func redisCheckSummary(inst *model.Instance, db string, res *CheckResult) string {
	ok, warn, rej := 0, 0, 0
	first := ""
	for _, r := range res.Rows {
		switch r.ErrLevel {
		case review.LevelError:
			rej++
			if first == "" {
				first = fmt.Sprintf("第 %d 行：%s", r.ID, r.ErrorMessage)
			}
		case review.LevelWarning:
			warn++
		default:
			ok++
		}
	}
	s := fmt.Sprintf("实例：%s，库：%s，共 %d 条命令：通过 %d、警告 %d、拒绝 %d", truncate(inst.InstanceName, 40), db, len(res.Rows), ok, warn, rej) // i18n-ignore 写进审计库的存档文本
	if first != "" {
		s += "，" + truncate(first, 200)
	}
	return s
}

func (s *Service) redisAudit(p audit.Actor, act, extra string) {
	audit.Lifecycle(s.repo, p, "Redis上线-"+act, extra, s.now()) // i18n-ignore 审计动作名
}

// redisExecOpener 是 redis 引擎提供的写连接入口（*redis.Engine 实现）。
type redisExecOpener interface {
	OpenExec(ctx context.Context, db int) (*redis.ExecClient, error)
}

// 执行结果里被跳过的命令的提示。
const (
	redisSkipAfterFail = "前面的命令执行失败，已停止执行"
	redisSkipAll       = "有命令按当前规则不再通过，整单未执行"
)

// execRedis 在目标实例上执行 Redis 工单：整单复检 → 逐条执行（遇错即停）。永远返回结果集（出错时带错误行）。
func (s *Service) execRedis(ctx context.Context, w *model.SqlWorkflow, inst *model.Instance, sqlText string) *review.Set {
	dbNum, err := parseRedisDB(w.DBName)
	if err != nil {
		return failedSet(sqlText, fmt.Sprintf("工单里的 Redis 库编号不合法：%s", w.DBName))
	}
	lines, err := redisrule.SplitLines(sqlText)
	if err != nil {
		return failedSet(sqlText, err.Error())
	}
	rules, _, err := s.loadRedisRules(ctx)
	if err != nil {
		return failedSet(sqlText, fmt.Sprintf("读取 Redis 规则失败，未执行任何命令：%s", err.Error()))
	}
	target := redisrule.Target{InstanceID: inst.ID, DB: dbNum}

	// 1. 用执行时的当前规则整单复检：规则在审批期间被改过，以现在的规则为准；任何一行不再通过，一条都不执行。
	verdicts := make([]redisrule.Verdict, len(lines))
	failAt := -1
	for i, l := range lines {
		verdicts[i] = redisrule.EvaluateLine(rules, target, l.Text)
		if !verdicts[i].Allowed() && failAt < 0 {
			failAt = i
		}
	}
	set := &review.Set{FullSQL: sqlText}
	if failAt >= 0 {
		for i, l := range lines {
			row := redisRow(l)
			if i == failAt {
				row.Stage, row.ErrLevel, row.StageStatus = "Execute failed", review.LevelError, "规则复检未通过，拒绝执行"
				row.ErrorMessage = fmt.Sprintf("执行前按当前规则复检未通过：%s", verdicts[i].Message)
				set.Error = fmt.Sprintf("Line %d has error/warning: %s", row.ID, row.ErrorMessage)
			} else {
				markRedisSkipped(&row, redisSkipAll)
			}
			set.Rows = append(set.Rows, row)
		}
		return set
	}

	// 2. 连接并逐条执行
	eng, err := s.open(inst.DBType, s.repo.EngineConn(inst))
	if err != nil {
		return failedSet(sqlText, fmt.Sprintf("连接实例失败：%s", err.Error()))
	}
	defer eng.Close()
	opener, ok := eng.(redisExecOpener)
	if !ok {
		return failedSet(sqlText, fmt.Sprintf("%s 类型的实例暂不支持原生执行", inst.DBType))
	}
	x, err := opener.OpenExec(ctx, dbNum)
	if err != nil {
		return failedSet(sqlText, fmt.Sprintf("连接 Redis 失败：%s", err.Error()))
	}
	defer x.Close()

	failed := false
	for i, l := range lines {
		row := redisRow(l)
		if failed {
			markRedisSkipped(&row, redisSkipAfterFail)
			set.Rows = append(set.Rows, row)
			continue
		}
		start := time.Now()
		// 每条命令发出前再校验一次键与规则（规则快照同上）
		v := redisrule.Evaluate(rules, target, verdicts[i].Args)
		var reply any
		var execErr error
		if !v.Allowed() {
			execErr = fmt.Errorf("执行前复检未通过：%s", v.Message)
		} else if a, err := redis.Approve(v.Args); err != nil {
			execErr = err
		} else {
			if v.RecordBefore {
				row.Before = x.Snapshot(ctx, a)
			}
			reply, execErr = x.Exec(ctx, a)
		}
		row.ExecuteTime = fmt.Sprintf("%.3f", time.Since(start).Seconds())
		if execErr != nil {
			failed = true
			row.Stage, row.ErrLevel, row.StageStatus, row.ErrorMessage = "Execute failed", review.LevelError, "Execute failed", execErr.Error()
			set.Error = fmt.Sprintf("Line %d has error/warning: %s", row.ID, execErr.Error())
		} else {
			row.ErrorMessage = redis.Summarize(reply)
			row.AffectedRows = review.Flex(redis.ReplyInt(reply))
			if v.Level == redisrule.LevelWarn {
				row.ErrorMessage += "（" + v.Message + "）"
			}
		}
		set.Rows = append(set.Rows, row)
	}
	return set
}

// redisRow 是一行命令的执行结果骨架。
func redisRow(l redisrule.Line) review.Row {
	return review.Row{ID: review.Flex(l.No), Stage: "EXECUTED", StageStatus: "Execute Successfully", ErrorMessage: "None", SQL: l.Text,
		Sequence: fmt.Sprintf("0_0_%d", l.No), BackupDBName: "None", ExecuteTime: "0"}
}

func markRedisSkipped(r *review.Row, why string) {
	r.Stage, r.ErrLevel, r.StageStatus, r.ErrorMessage, r.ExecuteTime = "SKIPPED", review.LevelWarning, "未执行", why, "0"
}

// auditRedisExec 写 Redis 工单执行的系统审计：每条命令的摘要（值参数截断、疑似密码不记录）与结果。actor 为 nil 表示定时任务。
func (s *Service) auditRedisExec(actor *audit.Actor, w *model.SqlWorkflow, set *review.Set) {
	p := audit.Actor{Username: "system", Display: "系统"}
	if actor != nil {
		p = *actor
	}
	var parts []string
	ok, bad := 0, 0
	for _, r := range set.Rows {
		args, err := redis.TokenizeLine(r.SQL)
		desc := truncate(r.SQL, 60)
		if err == nil && len(args) > 0 {
			desc = redis.Redact(args)
		}
		st := "成功" // i18n-ignore 写进审计库的存档文本
		switch {
		case r.Stage == "SKIPPED":
			st = "未执行" // i18n-ignore 写进审计库的存档文本
		case r.ErrLevel == review.LevelError:
			st = "失败" // i18n-ignore 写进审计库的存档文本
			bad++
		default:
			ok++
		}
		parts = append(parts, fmt.Sprintf("[%d]%s→%s", r.ID, desc, st))
	}
	inst := ""
	if w.Instance != nil {
		inst = w.Instance.InstanceName
	}
	info := fmt.Sprintf("工单id：%d，实例：%s，库：%s，共 %d 条：成功 %d、失败 %d；%s", w.ID, truncate(inst, 40), w.DBName, len(set.Rows), ok, bad, strings.Join(parts, "；")) // i18n-ignore 写进审计库的存档文本
	if set.Error != "" && len(set.Rows) == 1 && set.Rows[0].Stage == "Execute failed" && set.Rows[0].SQL == set.FullSQL {
		info = fmt.Sprintf("工单id：%d，实例：%s，库：%s，未能执行：%s", w.ID, truncate(inst, 40), w.DBName, truncate(set.Error, 200)) // i18n-ignore 写进审计库的存档文本
	}
	s.redisAudit(p, "执行", info)
}

// 编译期保证 *redis.Engine 提供写连接入口。
var _ redisExecOpener = (*redis.Engine)(nil)

// redisModeError Redis 实例只支持单实例的命令上线：不支持数据导出与批量上线。
func redisModeError(inst *model.Instance, batch, export bool) error {
	if inst == nil || inst.DBType != dbTypeRedis {
		return nil
	}
	switch {
	case export:
		return bad(fmt.Sprintf("Redis 实例（%s）不支持数据导出工单；读取数据请用「在线查询」", inst.InstanceName))
	case batch:
		return bad(fmt.Sprintf("Redis 实例（%s）不支持批量上线", inst.InstanceName))
	}
	return nil
}

// redisDBNumbers 把引擎返回的 db0、db1… 转成纯数字编号。
func redisDBNumbers(dbs []string) []string {
	out := make([]string, 0, len(dbs))
	for _, d := range dbs {
		out = append(out, strings.TrimPrefix(d, "db"))
	}
	return out
}
