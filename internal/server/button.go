// @author: kevinkelly
// @license: Apache Licence
// @file: button.go
// @time: 2026/10/28

package server

import (
	"html/template"
	"log/slog"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/btnperm"
	"dbtool/internal/i18n"
	"dbtool/internal/model"
	"dbtool/internal/sqlflow"
)

// 本文件把按钮权限接入 server（权限只有按钮这一套）：
//   - s.HasButton / s.ActorHasButton / s.RequireButtonAPI / s.RequireButtonPage：按登记表（internal/btnperm）里的按钮码校验；
//   - 模板里的 .Btn.Can / can 函数：按权限显示或置灰按钮。
// 模块里一律用这里的函数，不要自己查权限表；服务层没有 gin.Context 时用仓库的 HasButton(userID, code)。

// buttonProvider 由 store.Store 实现：返回按钮权限检查器（没有装配时为 nil）。
type buttonProvider interface{ Buttons() btnperm.Checker }

// SetButtonChecker 指定按钮权限检查器（测试里注入假实现；生产环境由仓库提供）。
func (s *Server) SetButtonChecker(c btnperm.Checker) { s.btnChecker = c }

func (s *Server) buttons() btnperm.Checker {
	if s.btnChecker != nil {
		return s.btnChecker
	}
	if p, ok := s.repo.(buttonProvider); ok {
		return p.Buttons()
	}
	return nil
}

var unknownCodeOnce sync.Map

// hasButton 是 HasButton 的核心：super 为真恒通过；仅超级用户的锁定码对其余用户恒假；其余问按钮权限检查器（测试里注入的，
// 或仓库提供的解析器），都没有时问仓库的 HasButton（测试 / 演示的内存仓库）。
func (s *Server) hasButton(uid uint, super bool, code string) bool {
	b, ok := btnperm.Lookup(code)
	if !ok {
		if _, dup := unknownCodeOnce.LoadOrStore(code, true); !dup {
			slog.Error("校验了未登记的按钮权限码", "code", code)
		}
		return false
	}
	if super {
		return true
	}
	if b.Locked {
		return false
	}
	if ck := s.buttons(); ck != nil {
		ok, err := ck.HasButton(uid, code)
		return err == nil && ok
	}
	ok, err := s.repo.HasButton(uid, code)
	return err == nil && ok
}

// HasButton 判断当前用户是否拥有按钮：超级用户恒真；其余看权限组授予 ∪ 单独授权，再扣掉单独禁用（单独禁用优先）；基础按钮对所有登录用户开放。
func (s *Server) HasButton(c *gin.Context, code string) bool {
	p := principalOf(c)
	return s.hasButton(p.ID, p.IsSuperuser, code)
}

// ActorHasButton 同 HasButton，用于菜单 Show 等拿不到 gin.Context 的地方。
func (s *Server) ActorHasButton(a audit.Actor, code string) bool {
	return s.hasButton(a.ID, a.IsSuperuser, code)
}

func (s *Server) hasAnyButton(c *gin.Context, codes []string) (string, bool) {
	for _, code := range codes {
		if s.HasButton(c, code) {
			return code, true
		}
	}
	return "", false
}

// RequireButtonAPI 要求拥有任一按钮（超级用户通过）。
func (s *Server) RequireButtonAPI(codes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := s.hasAnyButton(c, codes); !ok {
			s.auditNoPerm(c, codes...)
			apiFail(c, http.StatusForbidden, "forbidden", "没有权限执行该操作")
			return
		}
		c.Next()
	}
}

// RequireButtonAPIAudited 同 RequireButtonAPI；命中的是高危类按钮（登记表里类型为 d）且请求成功时，再写一条审计“高危按钮操作”。
// 只用在业务模块自己没有审计的接口上（各模块的写操作大多已经自己写 dbt_audit_log，不要重复）。
func (s *Server) RequireButtonAPIAudited(codes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		code, ok := s.hasAnyButton(c, codes)
		if !ok {
			s.auditNoPerm(c, codes...)
			apiFail(c, http.StatusForbidden, "forbidden", "没有权限执行该操作")
			return
		}
		c.Next()
		if !c.IsAborted() && c.Writer.Status() < http.StatusBadRequest {
			s.AuditButton(c, code)
		}
	}
}

// ButtonOrFail 在处理器里校验按钮（任一即可）：没有权限时写出 403 并返回 false。用于按请求内容才能确定按钮的接口（如执行工单的 mode），
// 以及核心业务接口上比服务层判断更细的补充校验。
func (s *Server) ButtonOrFail(c *gin.Context, codes ...string) bool {
	if _, ok := s.hasAnyButton(c, codes); ok {
		return true
	}
	s.auditNoPerm(c, codes...)
	apiFail(c, http.StatusForbidden, "forbidden", "没有权限执行该操作")
	return false
}

// AuditButton 为高危类按钮的一次成功调用写审计（非高危按钮不写）。
func (s *Server) AuditButton(c *gin.Context, code string) {
	if b, ok := btnperm.Lookup(code); ok && b.Danger() {
		s.auditDanger(c, b)
	}
}

// RequireButtonPage 页面版：无权限时渲染 403 页。
func (s *Server) RequireButtonPage(codes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := s.hasAnyButton(c, codes); ok {
			c.Next()
			return
		}
		s.renderForbidden(c, "没有权限访问该页面。")
		c.Abort()
	}
}

// auditDanger 记录高危按钮的一次成功调用。只记按钮、方法与路径（不含查询串与请求体），写失败只记日志。
func (s *Server) auditDanger(c *gin.Context, b btnperm.Button) {
	p := principalOf(c)
	s.writeAudit(model.AuditEntry{UserID: p.ID, UserName: cutRunes(p.Username, 30), UserDisplay: cutRunes(p.Display, 50), Action: "高危按钮操作", // i18n-ignore
		ExtraInfo: b.Label + "（" + b.Code + "）· " + c.Request.Method + " " + c.Request.URL.Path})
}

func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---- 模板 ----

// BtnCtx 是页面模板里的按钮权限判断：{{if .Btn.Can "sqlworkflow.approve"}}…{{end}}。每个请求只算一次。
type BtnCtx struct {
	s       *Server
	uid     uint
	lang    string // 请求的界面语言（Attr 的提示文案用）
	super   bool
	once    sync.Once
	allowed map[string]bool
	set     btnperm.Setting
}

func (s *Server) newBtnCtx(c *gin.Context) *BtnCtx {
	p := principalOf(c)
	return &BtnCtx{s: s, uid: p.ID, lang: langOf(c), super: p.IsSuperuser, set: btnperm.DefaultSetting()}
}

func (b *BtnCtx) load() {
	b.once.Do(func() {
		b.allowed = map[string]bool{}
		if ck := b.s.buttons(); ck != nil {
			if set, err := ck.Setting(); err == nil {
				b.set = set
			}
			if !b.super {
				if m, err := ck.AllowedButtons(b.uid); err == nil {
					b.allowed = m
				}
			}
			return
		}
		if !b.super {
			for _, code := range btnperm.Codes() {
				if b.s.hasButton(b.uid, false, code) {
					b.allowed[code] = true
				}
			}
		}
	})
}

// Can 判断当前用户能否使用按钮（超级用户恒真；Locked 按钮仅超级用户）。
func (b *BtnCtx) Can(code string) bool {
	if b == nil {
		return false
	}
	if bt, ok := btnperm.Lookup(code); !ok {
		return false
	} else if b.super {
		return true
	} else if bt.Locked {
		return false
	}
	b.load()
	return b.allowed[code]
}

// Show 判断按钮是否应该出现在页面上：有权限，或设置为“置灰”时无权限也显示（带 disabled）。
func (b *BtnCtx) Show(code string) bool {
	if b.Can(code) {
		return true
	}
	return b != nil && b.DenyMode() == btnperm.DenyDisable
}

// DenyMode 返回无权限按钮的呈现方式：hide / disable。
func (b *BtnCtx) DenyMode() string {
	if b == nil {
		return btnperm.DenyHide
	}
	b.load()
	return b.set.DenyDisplay
}

// Attr 返回放在 <button> 标签里的属性：无权限时 ` disabled title="无权限：需要「按钮名」（code）"`，有权限时为空。
// 常用写法：{{if .Btn.Show "x.y"}}<button type="button"{{.Btn.Attr "x.y"}}>…</button>{{end}}。
func (b *BtnCtx) Attr(code string) template.HTMLAttr {
	if b.Can(code) {
		return ""
	}
	label := code
	if bt, ok := btnperm.Lookup(code); ok {
		label = bt.Label
	}
	lang := i18n.ZH
	if b != nil && b.lang != "" {
		lang = b.lang
	}
	tip := i18n.T(lang, "无权限：需要「{label}」（{code}）", "label", i18n.TP(lang, label), "code", code)
	return template.HTMLAttr(` disabled title="` + template.HTMLEscapeString(tip) + `"`)
}

// ---- SQL 工单：按钮级校验 ----

// cancelAllowed 校验“终止工单”：分“终止自己的”（sqlworkflow.cancel_own）与“驳回 / 终止他人的”（sqlworkflow.reject）两个按钮。
// 只有 cancel_own 的用户，必须是工单提交人；服务层里对他人工单的驳回仍要求 sqlworkflow.reject。无权限时已写出 403。
func (s *Server) cancelAllowed(c *gin.Context, id uint) bool {
	if s.HasButton(c, "sqlworkflow.reject") {
		return true
	}
	if !s.HasButton(c, "sqlworkflow.cancel_own") {
		s.auditNoPerm(c, "sqlworkflow.cancel_own", "sqlworkflow.reject")
		apiFail(c, http.StatusForbidden, "forbidden", "没有权限执行该操作")
		return false
	}
	d, err := s.sql.Get(actorOf(c), id)
	if err != nil {
		writeErr(c, err)
		return false
	}
	if d.Workflow.Engineer != principalOf(c).Username {
		s.auditNoPerm(c, "sqlworkflow.reject")
		apiFail(c, http.StatusForbidden, "forbidden", "没有驳回他人工单的权限")
		return false
	}
	return true
}

// applyDetailButtons 让工单详情里的“能不能操作”标记同时满足更细的按钮权限（服务层的标记按资源组 / 提交人 / 状态算）。
func (s *Server) applyDetailButtons(c *gin.Context, d *sqlflow.Detail) {
	d.CanReview = d.CanReview && s.HasButton(c, "sqlworkflow.approve")
	d.CanAlterDate = d.CanAlterDate && s.HasButton(c, "sqlworkflow.alter_window")
	d.CanExecute = d.CanExecute && (s.HasButton(c, "sqlworkflow.execute_group") || s.HasButton(c, "sqlworkflow.execute_own"))
	d.CanCancel = d.CanCancel && (s.HasButton(c, "sqlworkflow.cancel_own") || s.HasButton(c, "sqlworkflow.reject"))
	d.CanRollback = d.CanRollback && s.HasButton(c, "sqlworkflow.rollback")
	d.CanOsc = d.CanOsc && s.HasButton(c, "sqlworkflow.osc")
	d.CanBatchOp = d.CanBatchOp && s.HasButton(c, "sqlworkflow.batch_op")
}
