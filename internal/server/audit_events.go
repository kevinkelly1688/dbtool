// @author: kevinkelly
// @license: Apache Licence
// @file: audit_events.go
// @time: 2026/10/28

package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/btnperm"
	"dbtool/internal/model"
)

// 本文件是 server 自己写审计的小工具：被拒绝的无权限操作（S9-A4）。
// 退出登录的审计由登录扩展（internal/authx）通过 LogoutObserver 写（S7a-A2）。

// SetAuditSink 指定审计写入函数（测试里注入以观察审计；生产环境不设，直接写元数据库的 dbt_audit_log）。
func (s *Server) SetAuditSink(f func(model.AuditEntry)) { s.auditSink = f }

// writeAudit 写一条 dbt_audit_log。仓库不是 store.Store（测试 / 演示）且没有注入 sink 时静默丢弃；写失败只记日志。
func (s *Server) writeAudit(e model.AuditEntry) {
	if s.auditSink != nil {
		s.auditSink(e)
		return
	}
	db, _, ok := s.DB()
	if !ok {
		return
	}
	if err := db.Create(&e).Error; err != nil {
		slog.Error("写审计失败", "action", e.Action, "err", err)
	}
}

// 无权限拒绝审计的去重：同一用户、同一按钮码、同一路径，1 分钟内只记一次，避免脚本 / 反复点击把审计表刷满。
const (
	denyWindow = time.Minute
	denyMaxKey = 4096 // 去重表上限；超过先清过期的，仍然超过就整体清空（宁可多记几条，也不让内存无限增长）
)

type denyDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// first 返回 key 在窗口内是不是第一次出现；是则登记。
func (d *denyDedup) first(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = map[string]time.Time{}
	}
	if t, ok := d.seen[key]; ok && now.Sub(t) < denyWindow {
		return false
	}
	if len(d.seen) >= denyMaxKey {
		for k, t := range d.seen {
			if now.Sub(t) >= denyWindow {
				delete(d.seen, k)
			}
		}
		if len(d.seen) >= denyMaxKey {
			d.seen = map[string]time.Time{}
		}
	}
	d.seen[key] = now
	return true
}

// auditNoPerm 在按钮校验返回 403 时写一条“拒绝（无权限）”审计：按钮名（码）、方法、路径。
// 只记写操作与高危按钮：普通的 GET 查看按钮（列表、详情）不记，免得刷屏；同一用户同一按钮同一路由 1 分钟内只记一次。
// codes 是这次校验接受的按钮码（任一即可），审计里的权限码取第一个，其余写在“可选”里。
func (s *Server) auditNoPerm(c *gin.Context, codes ...string) {
	if len(codes) == 0 {
		return
	}
	first := codes[0]
	b, known := btnperm.Lookup(first)
	if m := c.Request.Method; (m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions) && !(known && b.Danger()) {
		return
	}
	p := principalOf(c)
	route := c.FullPath()
	if route == "" {
		route = c.Request.URL.Path
	}
	if !s.deny.first(strings.Join([]string{strconv.FormatUint(uint64(p.ID), 10), strings.Join(codes, ","), c.Request.Method, route}, "|"), time.Now()) {
		return
	}
	label := first
	if known {
		label = b.Label
	}
	extra := label + "（" + first + "）· " + c.Request.Method + " " + c.Request.URL.Path
	if len(codes) > 1 {
		extra += "；可选按钮：" + strings.Join(codes[1:], "、") // i18n-ignore
	}
	s.writeAudit(model.AuditEntry{UserID: p.ID, UserName: cutRunes(p.Username, 30), UserDisplay: cutRunes(p.Display, 50), Action: audit.ActionNoPerm, ExtraInfo: cutRunes(extra, 1000)})
}
