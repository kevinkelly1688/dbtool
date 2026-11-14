// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package duty

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"dbtool/internal/audit"
	"dbtool/internal/model"
	"dbtool/internal/server"
)

// 按钮码（登记在 internal/btnperm/registry.go）：默认只有超级用户，可以在权限组里授予。
const (
	BtnView = "duty.view" // 打开值班表管理页、查看班次与调班
	BtnEdit = "duty.edit" // 新增 / 修改 / 停用 / 删除班次
	BtnSwap = "duty.swap" // 新增 / 撤销临时调班
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		db, _, ok := s.DB() // 仓库不是 store.Store（测试 / 演示）时没有元数据库，由调用方用 Mount 自己装配
		if !ok {
			return
		}
		Mount(s, rt, New(NewGormStore(db), NewGormDirectory(db)), func(e *model.AuditEntry) error { return db.Create(e).Error })
	})
	server.AddMenu(server.MenuItem{Group: "plat", Key: "sys-duty", Href: "/settings/duty", Label: "值班表", Order: 25,
		Show: func(s *server.Server, a audit.Actor) bool { return s.HasDuty() && s.ActorHasButton(a, BtnView) }})
}

type auditSink struct{ write func(*model.AuditEntry) error }

func (a auditSink) WriteLifecycleAudit(e *model.AuditEntry) error {
	if a.write == nil {
		return nil
	}
	return a.write(e)
}

type handlers struct {
	s    *server.Server
	svc  *Service
	sink auditSink
}

// Mount 登记页面与接口，并把值班芯片装配到顶栏。writeAudit 为空时不写审计（测试 / 演示）。
func Mount(s *server.Server, rt server.Routes, svc *Service, writeAudit func(*model.AuditEntry) error) {
	h := &handlers{s: s, svc: svc, sink: auditSink{write: writeAudit}}
	s.SetDutyProvider(chipProvider{svc})
	rt.Pages.GET("/settings/duty", s.RequireButtonPage(BtnView), h.page)
	g := rt.API.Group("/duty")
	g.GET("/now", h.now) // 登录即可：顶栏芯片和浮层的数据
	g.GET("/overview", s.RequireButtonAPI(BtnView), h.overview)
	g.POST("/shifts", s.RequireButtonAPI(BtnEdit), h.createShift)
	g.PUT("/shifts/:id", s.RequireButtonAPI(BtnEdit), h.updateShift)
	g.POST("/shifts/:id/enable", s.RequireButtonAPI(BtnEdit), h.enableShift)
	g.POST("/shifts/:id/delete", s.RequireButtonAPI(BtnEdit), h.deleteShift)
	g.POST("/overrides", s.RequireButtonAPI(BtnSwap), h.createOverride)
	g.POST("/overrides/:id/cancel", s.RequireButtonAPI(BtnSwap), h.cancelOverride)
}

// chipProvider 把值班快照交给 server 的顶栏。
type chipProvider struct{ svc *Service }

func (p chipProvider) Chip(now time.Time) *server.DutyChip {
	v := p.svc.Now(now)
	if !v.Enabled {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	chip := &server.DutyChip{State: v.State, JSON: string(b)}
	if v.Cur != nil {
		chip.Name = v.Cur.Display
		chip.Range = v.Cur.Start + "–" + v.Cur.End
		chip.Override = v.Cur.Kind == KindOverride
	}
	return chip
}

func (h *handlers) page(c *gin.Context) {
	h.s.Render(c, http.StatusOK, "duty/index.html", server.PageData{Title: "值班表", Nav: "sys-duty"})
}

func (h *handlers) now(c *gin.Context) {
	server.APIOK(c, h.svc.Now(time.Now()))
}

func (h *handlers) overview(c *gin.Context) {
	// 值班人名单（全部账号的登录名和显示名）只给能新增 / 修改班次或调班的人；只读的人不需要，也不该拿到
	a := server.ActorOf(c)
	ov, err := h.svc.Overview(h.s.ActorHasButton(a, BtnEdit) || h.s.ActorHasButton(a, BtnSwap))
	if err != nil {
		fail(c, err)
		return
	}
	server.APIOK(c, ov)
}

func bind(c *gin.Context, v any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.ShouldBindJSON(v); err != nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return false
	}
	return true
}

func needConfirm(c *gin.Context, ok bool) bool {
	if !ok {
		server.APIFail(c, http.StatusBadRequest, "need_confirm", "需要二次确认")
	}
	return ok
}

func (h *handlers) audit(c *gin.Context, action, extra string) {
	audit.Lifecycle(h.sink, server.ActorOf(c), action, extra, time.Now())
}

// idParam 取路径里的 :id；不是正整数就按“不存在”处理（notFound 是对应的提示：班次不存在 / 调班不存在）。
func idParam(c *gin.Context, notFound string) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusNotFound, "not_found", notFound)
		return 0, false
	}
	return uint(n), true
}

// weekdaysText 是适用星期的中文写法（审计用）：每天 / 周一至周五 / 周一、周三。
func weekdaysText(mask uint8) string {
	switch mask {
	case AllWeek:
		return "每天" // i18n-ignore 审计文本，存库后不翻译
	case 0x3e:
		return "周一至周五" // i18n-ignore 审计文本，存库后不翻译
	}
	var parts []string
	for _, d := range []int{1, 2, 3, 4, 5, 6, 0} {
		if mask&(1<<uint(d)) != 0 {
			parts = append(parts, weekdayNames[d])
		}
	}
	return strings.Join(parts, "、") // i18n-ignore 审计文本，存库后不翻译
}

func shiftText(sh *Shift) string {
	var b strings.Builder
	fmt.Fprintf(&b, "班次「%s」，值班人 %s，%s，%s", sh.Name, sh.Username, RangeText(sh.StartMin, sh.EndMin), weekdaysText(sh.Weekdays)) // i18n-ignore 审计文本，存库后不翻译
	if sh.Contact != "" {
		b.WriteString("，联系方式 " + sh.Contact) // i18n-ignore 审计文本，存库后不翻译
	}
	if sh.Remark != "" {
		b.WriteString("，备注：" + sh.Remark) // i18n-ignore 审计文本，存库后不翻译
	}
	if !sh.Enabled {
		b.WriteString("，已停用") // i18n-ignore 审计文本，存库后不翻译
	}
	return b.String()
}

func (h *handlers) createShift(c *gin.Context) {
	var in ShiftInput
	if !bind(c, &in) {
		return
	}
	sh, err := h.svc.CreateShift(server.ActorOf(c).Username, in)
	if err != nil {
		fail(c, err)
		return
	}
	h.audit(c, "值班-新增班次", shiftText(sh)) // i18n-ignore 审计文本，存库后不翻译
	server.APIOK(c, gin.H{"id": sh.ID})
}

func (h *handlers) updateShift(c *gin.Context) {
	id, ok := idParam(c, "班次不存在")
	if !ok {
		return
	}
	var in ShiftInput
	if !bind(c, &in) {
		return
	}
	old, cur, err := h.svc.UpdateShift(server.ActorOf(c).Username, id, in)
	if err != nil {
		fail(c, err)
		return
	}
	if o, n := shiftText(old), shiftText(cur); o != n {
		h.audit(c, "值班-修改班次", fmt.Sprintf("%s -> %s", o, n)) // i18n-ignore 审计文本，存库后不翻译
	}
	server.APIOK(c, gin.H{"id": id})
}

// enableShift 只改班次的启用状态（“停用 / 启用”按钮）。单独一个接口，是为了不用页面上可能已过期的整行去覆盖别人刚做的修改。
func (h *handlers) enableShift(c *gin.Context) {
	id, ok := idParam(c, "班次不存在")
	if !ok {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if !bind(c, &in) {
		return
	}
	if in.Enabled == nil {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	old, cur, err := h.svc.SetShiftEnabled(server.ActorOf(c).Username, id, *in.Enabled)
	if err != nil {
		fail(c, err)
		return
	}
	if old.Enabled != cur.Enabled { // 状态没变（重复点击）不写审计
		action := "值班-停用班次" // i18n-ignore 审计文本，存库后不翻译
		if cur.Enabled {
			action = "值班-启用班次" // i18n-ignore 审计文本，存库后不翻译
		}
		h.audit(c, action, shiftText(cur))
	}
	server.APIOK(c, gin.H{"id": id, "enabled": cur.Enabled})
}

func (h *handlers) deleteShift(c *gin.Context) {
	id, ok := idParam(c, "班次不存在")
	if !ok {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !bind(c, &in) || !needConfirm(c, in.Confirm) {
		return
	}
	sh, err := h.svc.DeleteShift(id)
	if err != nil {
		fail(c, err)
		return
	}
	h.audit(c, "值班-删除班次", shiftText(sh)) // i18n-ignore 审计文本，存库后不翻译
	server.APIOK(c, gin.H{"ok": true})
}

func overrideText(o *Override, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "调班 #%d：%s 值班，%s 至 %s", o.ID, o.Username, o.Start.In(loc).Format(timeLayout), o.End.In(loc).Format(timeLayout)) // i18n-ignore 审计文本，存库后不翻译
	if o.Reason != "" {
		b.WriteString("，原因：" + o.Reason) // i18n-ignore 审计文本，存库后不翻译
	}
	if o.Contact != "" {
		b.WriteString("，联系方式 " + o.Contact) // i18n-ignore 审计文本，存库后不翻译
	}
	return b.String()
}

func (h *handlers) createOverride(c *gin.Context) {
	var in OverrideInput
	if !bind(c, &in) {
		return
	}
	o, err := h.svc.CreateOverride(server.ActorOf(c).Username, in)
	if err != nil {
		fail(c, err)
		return
	}
	h.audit(c, "值班-新增调班", overrideText(o, h.svc.loc)) // i18n-ignore 审计文本，存库后不翻译
	server.APIOK(c, gin.H{"id": o.ID})
}

func (h *handlers) cancelOverride(c *gin.Context) {
	id, ok := idParam(c, "调班不存在")
	if !ok {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !bind(c, &in) || !needConfirm(c, in.Confirm) {
		return
	}
	o, err := h.svc.CancelOverride(server.ActorOf(c).Username, id)
	if err != nil {
		fail(c, err)
		return
	}
	h.audit(c, "值班-撤销调班", overrideText(o, h.svc.loc)) // i18n-ignore 审计文本，存库后不翻译
	server.APIOK(c, gin.H{"ok": true})
}

func fail(c *gin.Context, err error) {
	var ie *InputError
	switch {
	case errors.As(err, &ie):
		status := http.StatusBadRequest
		switch ie.Kind {
		case "conflict":
			status = http.StatusConflict
		case "not_found":
			status = http.StatusNotFound
		}
		server.APIFail(c, status, ie.Kind, ie.Msg)
	case errors.Is(err, ErrTableMissing):
		server.APIFail(c, http.StatusServiceUnavailable, "table_missing", "值班表未创建，请先执行 deploy/sql/300_duty.sql")
	default:
		slog.Error("值班表接口出错", "path", c.Request.URL.Path, "err", err)
		server.APIFail(c, http.StatusInternalServerError, "internal", "服务内部错误")
	}
}
