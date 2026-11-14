// @author: kevinkelly
// @license: Apache Licence
// @file: apply_grant_handlers.go
// @time: 2026/10/28

package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"dbtool/internal/privapply"
)

// 管理员“直接授予”查询权限的接口（按钮码 qpriv.manage）。
func init() {
	addRoutes(func(s *Server, rt Routes) {
		if s.apply == nil {
			return
		}
		g := rt.API.Group("/query/privileges")
		g.GET("/grant-options", s.RequireButtonAPI("qpriv.manage"), s.apiGrantOptions)
		g.POST("", s.RequireButtonAPI("qpriv.manage"), s.apiGrantPrivilege)
	})
}

func (s *Server) apiGrantOptions(c *gin.Context) {
	a := actorOf(c)
	users, err := s.apply.GrantUsers(a)
	if err != nil {
		writeErr(c, err)
		return
	}
	insts, err := s.apply.GrantInstances(a)
	if err != nil {
		writeErr(c, err)
		return
	}
	us := make([]gin.H, 0, len(users))
	for _, u := range users {
		us = append(us, gin.H{"username": u.UserName, "display": u.Display})
	}
	is := make([]optInstance, 0, len(insts))
	for _, i := range insts {
		is = append(is, optInstance{ID: i.ID, Name: i.InstanceName, DBType: i.DBType})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"users": us, "instances": is}})
}

type grantReq struct {
	UserName   string   `json:"username"`
	InstanceID uint     `json:"instance_id"`
	PrivType   int      `json:"priv_type"`
	DBName     string   `json:"db_name"`
	DBList     []string `json:"db_list"`
	TableList  []string `json:"table_list"`
	ValidDate  string   `json:"valid_date"`
	LimitNum   int      `json:"limit_num"`
	Reason     string   `json:"reason"`
}

func (s *Server) apiGrantPrivilege(c *gin.Context) {
	var req grantReq
	if err := c.ShouldBindJSON(&req); err != nil {
		apiFail(c, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	res, err := s.apply.GrantPrivilege(c.Request.Context(), actorOf(c), privapply.GrantReq(req))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"privilege_ids": res.PrivilegeIDs}})
}
