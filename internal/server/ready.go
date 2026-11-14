// @author: kevinkelly
// @license: Apache Licence
// @file: ready.go
// @time: 2026/10/28

package server

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 就绪探针 GET /readyz（不要求登录；/healthz 仍只表示进程存活）。
//
//	{"status":"ok|degraded|fail","checks":[{"name":…,"ok":…,"detail":…}]}
//
// 检查项：metadb（元数据库 Ping，2 秒超时）、required_tables（核心表是否都在）、optional_tables（dbt_* 扩展表，
// 缺失只列为 degraded，不算失败）。status=fail 时 HTTP 503，ok / degraded 为 200。
// detail 只给固定描述与表名，不回显驱动错误文本（可能带主机 / 账号），完整错误写服务日志。
// 结果缓存 readyCacheTTL，避免未登录请求被用来给元数据库加压。

// ReadyChecker 由仓库实现（可选，主要给测试 / 演示）；没有实现时，提供了元数据库句柄（DBProvider）就直接探测，都没有则跳过检查。
type ReadyChecker interface {
	PingMetaDB(ctx context.Context) error
	// ExistingTables 返回 names 中当前库里实际存在的表。
	ExistingTables(ctx context.Context, names []string) (map[string]bool, error)
}

// ReadyCheck 是响应里的一项检查。
type ReadyCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// ReadyReport 是 /readyz 的响应体。
type ReadyReport struct {
	Status   string       `json:"status"`
	Checks   []ReadyCheck `json:"checks"`
	Degraded []string     `json:"degraded,omitempty"` // 缺失的可选表
}

const (
	readyTimeout  = 2 * time.Second
	readyCacheTTL = 2 * time.Second
)

// requiredTables 是没有就无法正常服务的核心表。
var requiredTables = []string{
	"dbt_sql_users", "dbt_setting", "dbt_sql_instance", "dbt_resource_group", "dbt_auth_group",
	"dbt_workflow_audit", "dbt_workflow_audit_setting", "dbt_workflow_audit_detail", "dbt_workflow_log",
	"dbt_sql_workflow", "dbt_sql_workflow_content", "dbt_query_privileges_apply", "dbt_query_privileges", "dbt_query_log", "dbt_audit_log",
}

// optionalTables 是本系统新增的扩展表（deploy/sql）：缺失时对应功能降级，服务本身可用。
var optionalTables = []string{
	"dbt_audit_ext", "dbt_auth_group_ext", "dbt_auth_identity", "dbt_auth_provider", "dbt_setting_history",
	"dbt_group_perm", "dbt_user_perm", "dbt_perm_setting", "dbt_user_profile",
	"dbt_instance_ext", "dbt_resource_group_ext", "dbt_notify_channel", "dbt_notify_log",
	"dbt_query_event", "dbt_query_reject", "dbt_query_priv_ext", "dbt_query_apply_ext", "dbt_sql_schedule",
	"dbt_archive_task", "dbt_archive_run", "dbt_inspect_run", "dbt_perf_issue_ext", "dbt_redis_rule", "dbt_gi_setting",
}

type readyCache struct {
	mu  sync.Mutex
	at  time.Time
	rep ReadyReport
}

func (s *Server) readyChecker() ReadyChecker {
	if c, ok := s.repo.(ReadyChecker); ok {
		return c
	}
	if db, _, ok := s.DB(); ok && db != nil {
		return gormReady{db: db}
	}
	return nil
}

func (s *Server) readyz(c *gin.Context) {
	rep := s.readyReport(c.Request.Context())
	status := http.StatusOK
	if rep.Status == "fail" {
		status = http.StatusServiceUnavailable
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(status, rep)
}

func (s *Server) readyReport(ctx context.Context) ReadyReport {
	s.ready.mu.Lock()
	defer s.ready.mu.Unlock()
	if now := statusClock(); !s.ready.at.IsZero() && now.Sub(s.ready.at) < readyCacheTTL && !now.Before(s.ready.at) {
		return s.ready.rep
	}
	rep := runReadyChecks(ctx, s.readyChecker())
	s.ready.at, s.ready.rep = statusClock(), rep
	return rep
}

func runReadyChecks(parent context.Context, rc ReadyChecker) ReadyReport {
	if rc == nil {
		return ReadyReport{Status: "ok", Checks: []ReadyCheck{{Name: "metadb", OK: true, Detail: "未接入元数据库探测（测试 / 演示模式）"}}} // i18n-ignore
	}
	ctx, cancel := context.WithTimeout(parent, readyTimeout)
	defer cancel()
	rep := ReadyReport{Status: "ok"}

	if err := rc.PingMetaDB(ctx); err != nil {
		slog.Warn("就绪探针：元数据库不可达", "err", err)
		rep.Status = "fail"
		rep.Checks = append(rep.Checks, ReadyCheck{Name: "metadb", OK: false, Detail: "元数据库不可达或响应超过 2 秒"}) // i18n-ignore
		return rep
	}
	rep.Checks = append(rep.Checks, ReadyCheck{Name: "metadb", OK: true})

	all := append(append([]string{}, requiredTables...), optionalTables...)
	have, err := rc.ExistingTables(ctx, all)
	if err != nil {
		slog.Warn("就绪探针：查询表清单失败", "err", err)
		rep.Status = "fail"
		rep.Checks = append(rep.Checks, ReadyCheck{Name: "required_tables", OK: false, Detail: "无法确认核心表是否存在"}) // i18n-ignore
		return rep
	}
	var missReq, missOpt []string
	for _, t := range requiredTables {
		if !have[t] {
			missReq = append(missReq, t)
		}
	}
	for _, t := range optionalTables {
		if !have[t] {
			missOpt = append(missOpt, t)
		}
	}
	sort.Strings(missReq)
	sort.Strings(missOpt)
	if len(missReq) > 0 {
		rep.Status = "fail"
		rep.Checks = append(rep.Checks, ReadyCheck{Name: "required_tables", OK: false, Detail: "缺少核心表: " + join(missReq)}) // i18n-ignore
	} else {
		rep.Checks = append(rep.Checks, ReadyCheck{Name: "required_tables", OK: true})
	}
	if len(missOpt) > 0 {
		if rep.Status == "ok" {
			rep.Status = "degraded"
		}
		rep.Degraded = missOpt
		rep.Checks = append(rep.Checks, ReadyCheck{Name: "optional_tables", OK: true, Detail: "缺少可选扩展表（对应功能降级，请执行 deploy/sql）: " + join(missOpt)}) // i18n-ignore
	} else {
		rep.Checks = append(rep.Checks, ReadyCheck{Name: "optional_tables", OK: true})
	}
	return rep
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
