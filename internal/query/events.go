// @author: kevinkelly
// @license: Apache Licence
// @file: events.go
// @time: 2026/10/28

package query

import (
	"errors"
	"log/slog"
	"strings"
	"time"
)

// 未成功的查询也要留痕：被拦截（语句 / 权限规则）、引擎报错、超时、用户取消。
// dbt_query_log 不加列、成功记录也保持现状，所以这些记录写进旁路表（deploy/sql/050_query_reject.sql）：
//   - dbt_query_event：全部未成功的记录（状态 blocked / failed / timeout / canceled + 错误码 + 原因），执行历史读它；
//   - dbt_query_reject：只含被拦截（blocked）的语句，字段固定 id,user_name,instance_id,db_name,sql_text,reason,created_at，
//     供系统审计的“查询审计 / 仅被拒绝”只读使用。
// 表不存在时（DDL 未执行）EventRepo 的实现返回 ErrEventsUnavailable 或 Ready()=false，查询本身不受影响。

// 状态取值。
const (
	StatusOK       = "ok"
	StatusBlocked  = "blocked"
	StatusFailed   = "failed"
	StatusTimeout  = "timeout"
	StatusCanceled = "canceled"
)

// MaxEventSQL 是 sql_text 的最大长度（字符数）。
const MaxEventSQL = 2000

// QueryEvent 是一条未成功的查询记录。
type QueryEvent struct {
	ID           uint
	UserName     string
	UserDisplay  string
	InstanceID   uint
	InstanceName string
	DB           string
	SQL          string
	Status       string
	Code         string
	Reason       string
	CostMs       int64
	CreatedAt    time.Time
}

// HistoryRow 是执行历史里的一行：成功记录来自 dbt_query_log，其余来自 dbt_query_event。
type HistoryRow struct {
	ID          uint
	Time        time.Time
	Username    string
	Display     string
	Instance    string
	DB          string
	SQL         string
	Rows        int64
	CostSec     string
	Status      string
	Code        string
	Reason      string
	FromEvent   bool
	PrivChecked bool
}

// EventRepo 由 store 实现（可选）。仓库不实现或表不存在时，历史只显示成功记录。
type EventRepo interface {
	// EventsReady 报告旁路表是否可用。
	EventsReady() bool
	InsertQueryEvent(e *QueryEvent) error
	// ListHistory 返回 dbt_query_log 与 dbt_query_event 合并后的一页（按时间倒序）以及总数。status 为空表示全部，
	// "ok" 只看成功，其余只看对应状态的未成功记录。
	ListHistory(f QueryLogFilter, status string) ([]HistoryRow, int64, error)
}

// statusOfError 把业务错误归为记录状态；返回空串表示不需要记录（请求本身不完整、实例不存在等）。
func statusOfError(err error) (status, code, reason string) {
	var qe *Error
	if !errors.As(err, &qe) {
		return StatusFailed, "internal", "服务异常"
	}
	switch qe.Code {
	case "bad_request", "not_found", "unsupported_engine", "reason_required":
		return "", "", "" // reason_required 只是“请补充原因再提交”，补充之后的那次才是真正的一次查询
	case "bad_query", "star_forbidden", "no_privilege", "readonly_violation", "firewall_denied":
		return StatusBlocked, qe.Code, qe.Message
	case "timeout":
		return StatusTimeout, qe.Code, qe.Message
	case "canceled":
		return StatusCanceled, qe.Code, "用户取消"
	}
	return StatusFailed, qe.Code, qe.Message
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// recordFailure 记录一次未成功的执行；写不进去只记日志，不影响返回给用户的错误。
func (s *Service) recordFailure(p Principal, req Request, tr *execTrace, err error) {
	er, ok := s.repo.(EventRepo)
	if !ok || !er.EventsReady() {
		return
	}
	if tr.inst == nil && req.InstanceID != 0 { // 例如“未关联该实例”被拒绝时还没取到实例
		if inst, err := s.repo.GetInstance(req.InstanceID); err == nil && inst != nil {
			tr.inst = inst
		}
	}
	if tr.inst == nil {
		return
	}
	status, code, reason := statusOfError(err)
	if status == "" {
		return
	}
	ev := &QueryEvent{
		UserName: p.Username, UserDisplay: p.Display, InstanceID: tr.inst.ID, InstanceName: tr.inst.InstanceName, DB: req.DB,
		SQL: truncateRunes(strings.TrimSpace(req.SQL), MaxEventSQL), Status: status, Code: code,
		Reason: truncateRunes(reason, 500), CostMs: tr.cost.Milliseconds(), CreatedAt: time.Now(),
	}
	if e := er.InsertQueryEvent(ev); e != nil {
		slog.Warn("写入查询失败记录失败", "err", e, "user", p.Username)
	}
}
