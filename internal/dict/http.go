// @author: kevinkelly
// @license: Apache Licence
// @file: http.go
// @time: 2026/10/28

package dict

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
	"dbtool/internal/query"
	"dbtool/internal/server"
)

func init() {
	server.AddRoutes(func(s *server.Server, rt server.Routes) {
		if _, _, ok := s.DB(); !ok {
			return
		}
		// 敏感列规则：与在线查询共用 dbt_masking_policy（s.Repo() 即 query.Repo，已实现 ActiveMaskingColumns），本包只读。
		register(s, rt, NewService(s.Repo(), query.NewPrivAuthorizer(s.Repo(), nil), DefaultOpener).WithMasking(s.Repo()))
	})
}

// btnObjInfo 是对象信息 / 可编程对象的按钮码（internal/btnperm/registry.go）。
const btnObjInfo = "query.objinfo"

type handlers struct {
	s   *server.Server
	svc *Service
}

// register 登记路由（测试里直接传入假依赖的 Service）。
//
// 原来的独立“数据字典”页面（/data_dictionary、/api/v1/dict/*、HTML / ZIP 导出）已经去掉；本包只剩在线查询工作台用的两个接口。
// 不要求菜单权限；要有按钮 query.objinfo，
// 范围与库表树元数据一致（能访问该实例 + 库名过滤规则 + 非系统库）。
func register(s *server.Server, rt server.Routes, svc *Service) {
	h := &handlers{s: s, svc: svc}
	q := rt.API.Group("/query")
	q.GET("/instances/:id/databases/:db/object-info", h.objectInfo)
	q.GET("/instances/:id/databases/:db/programmables", h.programmables)
}

func fail(c *gin.Context, err error) {
	var de *Error
	if errors.As(err, &de) {
		server.APIFail(c, de.Status, de.Code, de.Message)
		return
	}
	slog.Error("对象信息请求失败", "path", c.Request.URL.Path, "err", err)
	server.APIFail(c, http.StatusInternalServerError, "internal", "服务内部错误，请稍后重试或联系管理员")
}

// objectInfo 返回一个对象（表 / 视图 / 函数 / 存储过程 / 触发器 / 定时任务）的详情：基本信息、列信息、索引信息、创建 / 定义语句，
// 内容与数据字典的对象详情一致（有哪些小节随对象类别和引擎而定）。
func (h *handlers) objectInfo(c *gin.Context) {
	if !h.s.ButtonOrFail(c, btnObjInfo) {
		return
	}
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请选择实例")
		return
	}
	kind := Kind(c.DefaultQuery("kind", string(KindTable)))
	if !validKind(kind) {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "未知的对象类别")
		return
	}
	det, err := h.svc.Detail(c.Request.Context(), server.PrincipalOf(c), DetailRequest{InstanceID: uint(n), DB: c.Param("db"), Kind: kind,
		Schema: c.Query("schema"), Name: c.Query("name")})
	if err != nil {
		fail(c, err)
		return
	}
	det.localize(i18n.FromContext(c.Request.Context())) // 成功响应里的小节标题 / 属性名 / 说明是服务端生成的中文，按请求语言翻译
	server.APIOK(c, det)
}

// programmables 列出库里的视图 / 函数 / 存储过程 / 触发器 / 定时任务（在线查询左侧“可编程对象”页签）。
func (h *handlers) programmables(c *gin.Context) {
	if !h.s.ButtonOrFail(c, btnObjInfo) {
		return
	}
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		server.APIFail(c, http.StatusBadRequest, "bad_request", "请选择实例")
		return
	}
	groups, err := h.svc.Programmables(c.Request.Context(), server.PrincipalOf(c), uint(n), c.Param("db"))
	if err != nil {
		fail(c, err)
		return
	}
	lang := i18n.FromContext(c.Request.Context())
	for i := range groups {
		groups[i].localize(lang)
	}
	server.APIOK(c, gin.H{"groups": groups})
}
