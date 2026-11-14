// @author: kevinkelly
// @license: Apache Licence
// @file: extras.go
// @time: 2026/10/28

package sqlflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/goinception"
	"dbtool/internal/i18n"
	"dbtool/internal/model"
	"dbtool/internal/query"
	"dbtool/internal/review"
	"dbtool/internal/settings"
)

// ---- 回滚语句 ----

// RollbackItem 是一条原语句及其回滚语句。
type RollbackItem struct {
	SQL      string `json:"sql"`
	Rollback string `json:"rollback"`
}

func (s *Service) backupConfig() (goinception.BackupConfig, bool) {
	host := cfgStr(s.repo, settings.KeyGoInceptionBackupHost)
	if host == "" {
		return goinception.BackupConfig{}, false
	}
	cfg := goinception.BackupConfig{Host: host, User: cfgStr(s.repo, settings.KeyGoInceptionBackupUser), Password: cfgStr(s.repo, settings.KeyGoInceptionBackupPassword)}
	cfg.Port = 3306
	if n, err := strconv.Atoi(cfgStr(s.repo, settings.KeyGoInceptionBackupPort)); err == nil && n > 0 {
		cfg.Port = n
	}
	return cfg, true
}

// Rollbacks 返回已结束且开启了备份的工单的回滚语句（按执行顺序倒序）。可见性同工单详情。
func (s *Service) Rollbacks(ctx context.Context, p audit.Actor, id uint) (*model.SqlWorkflow, []RollbackItem, error) {
	w, items, _, err := s.rollbacks(ctx, p, id)
	return w, items, err
}

// rollbacks 同 Rollbacks，另返回回滚语句里是否有被脱敏的值（命中“数据脱敏”配置的字段：非超级用户看到的是脱敏后的值）。
func (s *Service) rollbacks(ctx context.Context, p audit.Actor, id uint) (*model.SqlWorkflow, []RollbackItem, bool, error) {
	w, err := s.load(id, false)
	if err != nil {
		return nil, nil, false, err
	}
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, nil, false, err
	}
	if !pm.canView(p, w) {
		return nil, nil, false, notFound("工单不存在")
	}
	if !w.IsBackup || (w.Status != model.WfFinish && w.Status != model.WfException) {
		return nil, nil, false, conflict("只有开启了备份且已执行结束的工单才能查看回滚语句")
	}
	cfg, ok := s.backupConfig()
	if !ok {
		return nil, nil, false, newErr(500, "no_backup_db", "未配置 goInception 备份库（系统设置 goinception.backup_host 等），无法读取回滚语句，请联系管理员。")
	}
	c, err := s.repo.GetContent(id)
	if err != nil {
		return nil, nil, false, err
	}
	if c == nil {
		return nil, nil, false, notFound("工单内容不存在")
	}
	rows, err := review.ParseRows(c.ExecuteResult)
	if err != nil {
		return nil, nil, false, bad("执行结果无法解析，无法获取回滚语句")
	}
	rbs, err := s.newBackup(cfg).Rollbacks(ctx, rows)
	if err != nil {
		return nil, nil, false, newErr(502, "backup_failed", fmt.Sprintf("读取回滚语句失败：%s", err.Error()))
	}
	mk, err := s.maskerFor(&p, w)
	if err != nil {
		return nil, nil, false, err
	}
	out := make([]RollbackItem, 0, len(rbs))
	for _, r := range rbs {
		out = append(out, RollbackItem{SQL: mk.SQL(r.SQL), Rollback: mk.SQL(r.Rollback)})
	}
	return w, out, mk.Changed(), nil
}

// 回滚语句的访问方式，写进审计日志的操作名。
const (
	RollbackView     = "查看回滚语句" // i18n-ignore 审计动作名
	RollbackDownload = "下载回滚语句" // i18n-ignore 审计动作名
)

// RollbackAudited 同 Rollbacks，但每次查看 / 下载（含失败）都写一条操作审计日志（dbt_audit_log）：回滚语句里是变更前的数据，属于敏感内容。
// how 取 RollbackView 或 RollbackDownload。审计写失败只记日志，不影响访问。
func (s *Service) RollbackAudited(ctx context.Context, p audit.Actor, id uint, how string) (*model.SqlWorkflow, []RollbackItem, error) {
	w, items, masked, err := s.rollbacks(ctx, p, id)
	if err == nil && masked && how == RollbackDownload {
		// 脱敏后的回滚语句不能直接执行，下载没有意义；明文只有超级用户能取
		w, items, err = nil, nil, conflict("回滚语句里含有已配置脱敏的字段值，只有超级用户可以下载；可在页面上查看脱敏后的内容")
	}
	info := fmt.Sprintf("工单id：%d", id) // i18n-ignore 写进审计库的存档文本
	if err != nil {
		info += "，error:" + truncate(err.Error(), 200) // i18n-ignore 写进审计库的存档文本
	} else {
		info += fmt.Sprintf("，库：%s，语句数：%d", w.DBName, len(items)) // i18n-ignore 写进审计库的存档文本
	}
	// 工单不存在 / 看不到时不写（没有可审计的对象，也避免被用来刷日志）
	if code := errCode(err); code != "not_found" {
		e := &model.AuditEntry{UserID: p.ID, UserName: p.Username, UserDisplay: p.Display, Action: how, ExtraInfo: info}
		if werr := s.repo.InsertAuditEntry(e); werr != nil {
			slog.Error("写回滚语句访问审计失败", "workflow", id, "err", werr)
		}
	}
	return w, items, err
}

// errCode 取业务错误的错误码；不是业务错误返回空。
func errCode(err error) string {
	var qe *query.Error
	if errors.As(err, &qe) {
		return qe.Code
	}
	return ""
}

// RollbackScript 把回滚语句整理成可下载的 .sql 文本（注释为中文）。
func RollbackScript(w *model.SqlWorkflow, items []RollbackItem, at time.Time) string {
	return RollbackScriptFor(i18n.ZH, w, items, at)
}

// RollbackScriptFor 同 RollbackScript，文件里的注释按 lang 输出（下载接口按请求语言调用）。
func RollbackScriptFor(lang string, w *model.SqlWorkflow, items []RollbackItem, at time.Time) string {
	var b strings.Builder
	oneLine := strings.NewReplacer("\r", " ", "\n", " ")
	b.WriteString(i18n.T(lang, "-- 工单 #{id} {name} 的回滚语句\n-- 库：{db}    生成时间：{time}\n-- 按执行顺序倒序排列，执行前请逐条确认。\n\n",
		"id", w.ID, "name", oneLine.Replace(w.WorkflowName), "db", w.DBName, "time", at.Format("2006-01-02 15:04:05")))
	for _, it := range items {
		b.WriteString(i18n.T(lang, "-- 原语句：{sql}\n{rollback}\n\n", "sql", oneLine.Replace(it.SQL), "rollback", strings.TrimSpace(it.Rollback)))
	}
	if len(items) == 0 {
		b.WriteString(i18n.T(lang, "-- 没有可回滚的语句（可能没有产生备份）。\n"))
	}
	return b.String()
}

// ---- 在线改表（pt-osc / gh-ost）进度控制 ----

// OscRow 是在线改表的一条进度。
type OscRow = map[string]any

// OscControl 查询或控制 goInception 里正在进行的在线改表。
// 查看进度需要能看到该工单；暂停/终止/恢复需要执行权限，且 sqlsha1 必须属于这张工单。
func (s *Service) OscControl(ctx context.Context, p audit.Actor, id uint, command, sqlsha1 string) ([]OscRow, error) {
	command = strings.ToLower(strings.TrimSpace(command))
	sqlsha1 = strings.TrimSpace(sqlsha1)
	w, err := s.load(id, false)
	if err != nil {
		return nil, err
	}
	pm, err := s.loadPerms(p)
	if err != nil {
		return nil, err
	}
	if !pm.canView(p, w) {
		return nil, notFound("工单不存在")
	}
	switch command {
	case "get":
	case "kill", "pause", "resume":
		if !s.canControlOsc(p, pm, w) {
			return nil, forbid("你无权控制该工单的在线改表")
		}
	default:
		return nil, bad("不支持的命令，可用：get / kill / pause / resume")
	}
	if w.Status != model.WfExecuting && command != "get" {
		return nil, conflict("工单当前不在执行中")
	}
	cfg, ok := s.inceptionConfig()
	if !ok {
		return nil, bad("未配置 goInception，没有在线改表可控制")
	}
	c, err := s.repo.GetContent(id)
	if err != nil {
		return nil, err
	}
	if c == nil || !s.sha1Belongs(c, sqlsha1) {
		return nil, bad("该 sqlsha1 不属于这张工单")
	}
	rows, err := s.newInception(cfg).Osc(ctx, command, sqlsha1)
	if err != nil {
		return nil, newErr(502, "inception_failed", fmt.Sprintf("goInception 请求失败：%s", err.Error()))
	}
	if label, ok := map[string]string{"kill": "终止", "pause": "暂停", "resume": "恢复"}[command]; ok { // i18n-ignore 审计动作名的后缀 // 只记会改变执行的控制命令，查询进度不记
		s.sysAudit(p, "在线改表"+label, wfAuditInfo(w)+"，sqlsha1："+truncate(sqlsha1, 40)) // i18n-ignore 审计动作名与存档文本
	}
	return rows, nil
}

// sha1Belongs sqlsha1 必须出现在这张工单的检查或执行结果里。
func (s *Service) sha1Belongs(c *model.SqlWorkflowContent, sha string) bool {
	if sha == "" {
		return false
	}
	for _, raw := range []string{c.ReviewContent, c.ExecuteResult} {
		rows, err := review.ParseRows(raw)
		if err != nil {
			continue
		}
		for _, r := range rows {
			if strings.EqualFold(r.SQLSha1, sha) {
				return true
			}
		}
	}
	return false
}
