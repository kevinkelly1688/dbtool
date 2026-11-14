// @author: kevinkelly
// @license: Apache Licence
// @file: http_ext.go
// @time: 2026/10/28

package sysadmin

import (
	"github.com/gin-gonic/gin"

	"dbtool/internal/i18n"
	"dbtool/internal/server"
)

// 系统管理的新增接口（扩展字段、KPI、彻底删除、关联预览）；路由挂在 /api/v1/settings 下，仅超级用户。
func (h *handlers) registerExt(st *gin.RouterGroup) {
	st.GET("/resource-groups/summary", h.rgSummary)
	st.DELETE("/resource-groups/:id/purge", h.rgPurge)
	st.POST("/resource-groups/:id/relations/preview", h.rgRelationPreview)
	st.GET("/users/summary", h.userSummary)
}

func (h *handlers) rgSummary(c *gin.Context) {
	v, err := h.svc.ResGroupSummary()
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, v)
}

func (h *handlers) rgPurge(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req struct {
		ConfirmName string `json:"confirm_name"`
	}
	if !bind(c, &req) {
		return
	}
	done(c, h.svc.PurgeResourceGroup(actorOf(c), id, req.ConfirmName))
}

func (h *handlers) rgRelationPreview(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req struct {
		Kind   string `json:"kind"`
		Add    []uint `json:"add"`
		Remove []uint `json:"remove"`
	}
	if !bind(c, &req) {
		return
	}
	v, err := h.svc.PreviewRelationChange(id, req.Kind, RelationChange{Add: req.Add, Remove: req.Remove})
	if err != nil {
		writeErr(c, err)
		return
	}
	if lang := reqLang(c); lang != i18n.ZH {
		for i, nb := range v.NewBreaks {
			v.NewBreaks[i] = i18n.TP(lang, nb)
		}
	}
	server.APIOK(c, v)
}

func (h *handlers) userSummary(c *gin.Context) {
	v, err := h.svc.UserSummary()
	if err != nil {
		writeErr(c, err)
		return
	}
	server.APIOK(c, v)
}
